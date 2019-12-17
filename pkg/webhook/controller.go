package webhook

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"time"

	kubeApiAdmission "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"istio.io/pkg/filewatcher"
	"istio.io/pkg/log"
)

var scope = log.RegisterScope("webhook controller", "webhook controller", 0)

const (
	galleyDeploymentName = "istio-galley"
)

type Options struct {
	WatchedNamespace string
	ResyncPeriod     time.Duration
	CAPath           string
	ConfigPath       string
	ConfigName       string
	ServiceName      string
	Client           clientset.Interface
}

type Controller struct {
	o         Options
	queue     workqueue.RateLimitingInterface
	ownerRefs []metav1.OwnerReference

	sharedInformers informers.SharedInformerFactory
	caFileWatcher   filewatcher.FileWatcher

	endpointReady bool // webhook endpoint has been marked as ready.
	doReconcile   bool // controller is responsible for reconciling the webhook

	codec runtime.Codec
}

type workType int

const (
	workCheckWebhook workType = iota
	workCheckDeployment
	workCheckCAFile
	workCheckEndpoint
)

func makeEventHandler(wq workqueue.Interface, w workType) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(_ interface{}) { wq.Add(w) },
		UpdateFunc: func(prev, curr interface{}) {
			if !reflect.DeepEqual(prev, curr) {
				wq.Add(w)
			}
		},
		DeleteFunc: func(_ interface{}) { wq.Add(w) },
	}
}

func createAdmissionCodec() runtime.Codec {
	scheme := runtime.NewScheme()
	utilruntime.Must(kubeApiAdmission.AddToScheme(scheme))
	opt := json.SerializerOptions{true, false, false}
	serializer := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme, opt)
	codec := versioning.NewDefaultingCodecForScheme(
		scheme,
		serializer,
		serializer,
		kubeApiAdmission.SchemeGroupVersion,
		runtime.InternalGroupVersioner,
	)
	return codec
}

func findOwnerRefs(client clientset.Interface, clusterRoleName string) []metav1.OwnerReference {
	clusterRole, err := client.RbacV1().ClusterRoles().Get(clusterRoleName, metav1.GetOptions{})
	if err != nil {
		scope.Warnf("Could not find clusterrole: %s to set ownerRef. "+
			"The webhook configuration must be deleted manually.",
			clusterRoleName)
		return nil
	}

	return []metav1.OwnerReference{
		*metav1.NewControllerRef(
			clusterRole,
			rbacv1.SchemeGroupVersion.WithKind("ClusterRole"),
		),
	}
}

func New(o Options) *Controller {
	c := &Controller{
		o:             o,
		queue:         workqueue.NewRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter()),
		codec:         createAdmissionCodec(),
		caFileWatcher: filewatcher.NewWatcher(),
		ownerRefs:     findOwnerRefs(o.Client, "istiod-istio-system"),
	}

	sharedInformers := informers.NewSharedInformerFactoryWithOptions(o.Client, o.ResyncPeriod,
		informers.WithNamespace(o.WatchedNamespace))

	webhookInformer := sharedInformers.Admissionregistration().V1().ValidatingWebhookConfigurations().Informer()
	webhookInformer.AddEventHandler(makeEventHandler(c.queue, workCheckWebhook))

	endpointInformer := sharedInformers.Core().V1().Endpoints().Informer()
	endpointInformer.AddEventHandler(makeEventHandler(c.queue, workCheckEndpoint))

	deploymentInformer := sharedInformers.Apps().V1().Deployments().Informer()
	deploymentInformer.AddEventHandler(makeEventHandler(c.queue, workCheckDeployment))

	return c
}

func (c *Controller) startFileWatcher(stop <-chan struct{}) {
	for {
		select {
		case <-c.caFileWatcher.Events(c.o.CAPath):
			c.queue.Add(workCheckCAFile)
		case <-c.caFileWatcher.Errors(c.o.CAPath):
			// log only
		case <-stop:
			return
		}
	}
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) Start(stop <-chan struct{}) {
	go c.startFileWatcher(stop)
	go c.sharedInformers.Start(stop)

	for _, ready := range c.sharedInformers.WaitForCacheSync(stop) {
		if !ready {
			return
		}
	}

	go c.runWorker()
}

// Load a PEM encoded cert from the input reader. This also verifies that the certificate is a validate x509 cert.
func loadAndValidateCert(in io.Reader) ([]byte, error) {
	certBytes, err := ioutil.ReadAll(in)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(certBytes)
	if block == nil {
		return nil, errors.New("could not decode pem")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("cert contains wrong pem type: %q", block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("cert contains invalid x509 certificate: %v", err)
	}
	return certBytes, nil
}

func (c *Controller) buildValidatingWebhookConfig() (*kubeApiAdmission.ValidatingWebhookConfiguration, error) {
	encoded, err := ioutil.ReadFile(c.o.CAPath)
	if err != nil {
		return nil, err
	}
	decoded, _, err := c.codec.Decode(encoded, nil, nil)
	if err != nil {
		return nil, err
	}
	config := decoded.(*kubeApiAdmission.ValidatingWebhookConfiguration)

	ca, err := os.Open(c.o.CAPath)
	if err != nil {
		return nil, err
	}
	caPem, err := loadAndValidateCert(ca)
	if err != nil {
		return nil, err
	}

	// update runtime fields
	config.OwnerReferences = c.ownerRefs
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caPem
	}

	return config, nil
}

func (c *Controller) buildAndUpdateValidatingConfig() error {
	desired, err := c.buildValidatingWebhookConfig()
	if err != nil {
		return err
	}

	current, err := c.sharedInformers.Admissionregistration().V1().ValidatingWebhookConfigurations().Lister().Get(c.o.ConfigName)
	if k8serrors.IsNotFound(err) {
		_, err := c.o.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(desired)
		if err != nil {
			return err
		}
		return nil
	}

	updated := current.DeepCopyObject().(*kubeApiAdmission.ValidatingWebhookConfiguration)
	updated.Webhooks = desired.Webhooks
	updated.OwnerReferences = desired.OwnerReferences

	if !reflect.DeepEqual(updated, current) {
		_, err := c.o.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(updated)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) processNextWorkItem() (cont bool) {
	obj, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(obj)

	work, ok := obj.(workType)
	if !ok {
		// don't retry an invalid work item
		c.queue.Forget(obj)
		return true
	}

	var retry bool
	switch work {
	case workCheckWebhook, workCheckCAFile:
		if c.doReconcile {
			if err := c.buildAndUpdateValidatingConfig(); err != nil {
				retry = true
			}
		}
	case workCheckDeployment:
		_, err := c.sharedInformers.Apps().V1().Deployments().Lister().Deployments(c.o.WatchedNamespace).Get(galleyDeploymentName)
		if kerrors.IsNotFound(err) {
			c.doReconcile = true
			c.queue.Add("webhook")
		} else {
			c.doReconcile = false
		}
	case workCheckEndpoint:
		if c.endpointReady {
			break
		}
		endpoint, err := c.sharedInformers.Core().V1().Endpoints().Lister().Endpoints(c.o.WatchedNamespace).Get(c.o.ServiceName)
		if err != nil {
			retry = true
			break
		}
		if len(endpoint.Subsets) == 0 {
			break
		}
		for _, subset := range endpoint.Subsets {
			if len(subset.Addresses) > 0 {
				c.endpointReady = true
				break
			}
		}
	}

	if !retry {
		c.queue.Forget(obj)
	}
	return true
}
