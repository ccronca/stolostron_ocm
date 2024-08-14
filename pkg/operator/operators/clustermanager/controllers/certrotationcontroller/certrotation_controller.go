package certrotationcontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	errorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	operatorinformer "open-cluster-management.io/api/client/operator/informers/externalversions/operator/v1"
	operatorlister "open-cluster-management.io/api/client/operator/listers/operator/v1"
	operatorv1 "open-cluster-management.io/api/operator/v1"
	"open-cluster-management.io/sdk-go/pkg/certrotation"

	"open-cluster-management.io/ocm/pkg/common/queue"
	"open-cluster-management.io/ocm/pkg/operator/helpers"
)

const (
	signerNamePrefix = "cluster-manager-webhook"
)

// Follow the rules below to set the value of SigningCertValidity/TargetCertValidity/ResyncInterval:
//
// 1) SigningCertValidity * 1/5 * 1/5 > ResyncInterval * 2
// 2) TargetCertValidity * 1/5 > ResyncInterval * 2
var SigningCertValidity = time.Hour * 24 * 365
var TargetCertValidity = time.Hour * 24 * 30
var ResyncInterval = time.Minute * 10

// certRotationController does:
//
//  1. continuously create a self-signed signing CA (via SigningRotation).
//     It creates the next one when a given percentage of the validity of the old CA has passed.
//  2. maintain a CA bundle with all not yet expired CA certs.
//  3. continuously create target cert/key pairs signed by the latest signing CA
//     It creates the next one when a given percentage of the validity of the previous cert has
//     passed, or when a new CA has been created.
type certRotationController struct {
	rotationMap          map[string]rotations // key is clusterManager's name, value is a rotations struct
	kubeClient           kubernetes.Interface
	secretInformers      map[string]corev1informers.SecretInformer
	configMapInformer    corev1informers.ConfigMapInformer
	recorder             events.Recorder
	clusterManagerLister operatorlister.ClusterManagerLister
}

type rotations struct {
	signingRotation  certrotation.SigningRotation
	caBundleRotation certrotation.CABundleRotation
	targetRotations  []certrotation.TargetRotation
}

func NewCertRotationController(
	kubeClient kubernetes.Interface,
	secretInformers map[string]corev1informers.SecretInformer,
	configMapInformer corev1informers.ConfigMapInformer,
	clusterManagerInformer operatorinformer.ClusterManagerInformer,
	recorder events.Recorder,
) factory.Controller {
	c := &certRotationController{
		rotationMap:          make(map[string]rotations),
		kubeClient:           kubeClient,
		secretInformers:      secretInformers,
		configMapInformer:    configMapInformer,
		recorder:             recorder,
		clusterManagerLister: clusterManagerInformer.Lister(),
	}
	return factory.New().
		ResyncEvery(ResyncInterval).
		WithSync(c.sync).
		WithInformersQueueKeysFunc(queue.QueueKeyByMetaName, clusterManagerInformer.Informer()).
		WithInformersQueueKeysFunc(helpers.ClusterManagerQueueKeyFunc(c.clusterManagerLister),
			configMapInformer.Informer(),
			secretInformers[helpers.SignerSecret].Informer(),
			secretInformers[helpers.RegistrationWebhookSecret].Informer(),
			secretInformers[helpers.WorkWebhookSecret].Informer()).
		ToController("CertRotationController", recorder)
}

func (c certRotationController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	key := syncCtx.QueueKey()
	switch {
	case key == "":
		return nil
	case key == factory.DefaultQueueKey:
		// ensure every clustermanager's certificates
		clustermanagers, err := c.clusterManagerLister.List(labels.Everything())
		if err != nil {
			return err
		}

		// do nothing if there is no cluster manager
		if len(clustermanagers) == 0 {
			klog.V(4).Infof("No ClusterManager found")
			return nil
		}

		var errs []error
		for i := range clustermanagers {
			err = c.syncOne(ctx, syncCtx, clustermanagers[i])
			if err != nil {
				errs = append(errs, err)
			}
		}
		return operatorhelpers.NewMultiLineAggregate(errs)
	default:
		clustermanagerName := key
		clustermanager, err := c.clusterManagerLister.Get(clustermanagerName)
		// ClusterManager not found, could have been deleted, do nothing.
		if errors.IsNotFound(err) {
			return fmt.Errorf("no clustermanager for %s", clustermanagerName)
		}
		err = c.syncOne(ctx, syncCtx, clustermanager)
		if err != nil {
			return err
		}
		return nil
	}
}

func (c certRotationController) syncOne(ctx context.Context, syncCtx factory.SyncContext, clustermanager *operatorv1.ClusterManager) error {
	clustermanagerName := clustermanager.Name
	clustermanagerNamespace := helpers.ClusterManagerNamespace(clustermanager.Name, clustermanager.Spec.DeployOption.Mode)

	var err error

	klog.Infof("Reconciling ClusterManager %q", clustermanagerName)
	// if the cluster manager is deleting, delete the rotation in map as well.
	if !clustermanager.DeletionTimestamp.IsZero() {
		// clean up all resources related with this clustermanager
		if _, ok := c.rotationMap[clustermanagerName]; ok {
			// delete signerSecret
			err = c.kubeClient.CoreV1().Secrets(clustermanagerNamespace).Delete(ctx, helpers.SignerSecret, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("clean up deleted cluster-manager, deleting signer secret failed, err:%s", err.Error())
			}

			// delete caBundleConfig
			err = c.kubeClient.CoreV1().ConfigMaps(clustermanagerNamespace).Delete(ctx, helpers.CaBundleConfigmap, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("clean up deleted cluster-manager, deleting caBundle config failed, err:%s", err.Error())
			}

			// delete registration webhook secret
			err = c.kubeClient.CoreV1().Secrets(clustermanagerNamespace).Delete(ctx, helpers.RegistrationWebhookSecret, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("clean up deleted cluster-manager, deleting registration webhook secret failed, err:%s", err.Error())
			}

			// delete work webhook secret
			err = c.kubeClient.CoreV1().Secrets(clustermanagerNamespace).Delete(ctx, helpers.WorkWebhookSecret, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("clean up deleted cluster-manager, deleting work webhook secret failed, err:%s", err.Error())
			}

			delete(c.rotationMap, clustermanagerName)
		}
		return nil
	}

	_, err = c.kubeClient.CoreV1().Namespaces().Get(ctx, clustermanagerNamespace, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("namespace %q does not exist yet", clustermanagerNamespace)
	}
	if err != nil {
		return err
	}

	// check if rotations exist, if not exist then create one
	if _, ok := c.rotationMap[clustermanager.Name]; !ok {
		signingRotation := certrotation.SigningRotation{
			Namespace:        clustermanagerNamespace,
			Name:             helpers.SignerSecret,
			SignerNamePrefix: signerNamePrefix,
			Validity:         SigningCertValidity,
			Lister:           c.secretInformers[helpers.SignerSecret].Lister(),
			Client:           c.kubeClient.CoreV1(),
		}
		caBundleRotation := certrotation.CABundleRotation{
			Namespace: clustermanagerNamespace,
			Name:      helpers.CaBundleConfigmap,
			Lister:    c.configMapInformer.Lister(),
			Client:    c.kubeClient.CoreV1(),
		}
		targetRotations := []certrotation.TargetRotation{
			{
				Namespace: clustermanagerNamespace,
				Name:      helpers.RegistrationWebhookSecret,
				Validity:  TargetCertValidity,
				HostNames: []string{fmt.Sprintf("%s.%s.svc", helpers.RegistrationWebhookService, clustermanagerNamespace)},
				Lister:    c.secretInformers[helpers.RegistrationWebhookSecret].Lister(),
				Client:    c.kubeClient.CoreV1(),
			},
			{
				Namespace: clustermanagerNamespace,
				Name:      helpers.WorkWebhookSecret,
				Validity:  TargetCertValidity,
				HostNames: []string{fmt.Sprintf("%s.%s.svc", helpers.WorkWebhookService, clustermanagerNamespace)},
				Lister:    c.secretInformers[helpers.WorkWebhookSecret].Lister(),
				Client:    c.kubeClient.CoreV1(),
			},
		}
		c.rotationMap[clustermanagerName] = rotations{
			signingRotation:  signingRotation,
			caBundleRotation: caBundleRotation,
			targetRotations:  targetRotations,
		}
	}

	// Ensure certificates are exists
	rotations := c.rotationMap[clustermanagerName] // reconcile cert/key pair for signer
	signingCertKeyPair, err := rotations.signingRotation.EnsureSigningCertKeyPair()
	if err != nil {
		return err
	}

	// reconcile ca bundle
	cabundleCerts, err := rotations.caBundleRotation.EnsureConfigMapCABundle(signingCertKeyPair)
	if err != nil {
		return err
	}

	// reconcile target cert/key pairs
	var errs []error
	for _, targetRotation := range rotations.targetRotations {
		if err := targetRotation.EnsureTargetCertKeyPair(signingCertKeyPair, cabundleCerts); err != nil {
			errs = append(errs, err)
		}
	}

	return errorhelpers.NewMultiLineAggregate(errs)
}
