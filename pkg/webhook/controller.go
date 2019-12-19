// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webhook

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"reflect"
	"time"

	kubeApiAdmission "k8s.io/api/admissionregistration/v1beta1"
	kubeApiRbac "k8s.io/api/rbac/v1"
	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	kubeApiMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubectl/pkg/scheme"

	"istio.io/pkg/filewatcher"
	"istio.io/pkg/log"
)

var scope = log.RegisterScope("webhook controller", "webhook controller", 0)

type Options struct {
	WatchedNamespace string
	ResyncPeriod     time.Duration
	CAPath           string
	ConfigPath       string
	ConfigName       string
	ServiceName      string
	Client           kubernetes.Interface
	GalleyDeployment string
}

type Controller struct {
	o                 Options
	ownerRefs         []kubeApiMeta.OwnerReference
	queue             workqueue.RateLimitingInterface
	sharedInformers   informers.SharedInformerFactory
	caFileWatcher     filewatcher.FileWatcher
	readFile          func(filename string) ([]byte, error) // test stub
	reconcileDone     func()
	endpointReadyOnce bool
}

type work struct{}

func filter(in interface{}, wantName, wantNamespace string) (skip bool) {
	obj, err := meta.Accessor(in)
	if err != nil {
		return true
	}
	if wantNamespace != "" && obj.GetNamespace() != wantNamespace {
		return true
	}
	if wantName != "" && obj.GetName() != wantName {
		return true
	}
	return false
}

func makeEventHandler(queue workqueue.Interface, name, namespace string) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if filter(obj, name, namespace) {
				return
			}
			queue.Add(&work{})
		},
		UpdateFunc: func(prev, curr interface{}) {
			if filter(curr, name, namespace) {
				return
			}
			if !reflect.DeepEqual(prev, curr) {
				queue.Add(&work{})
			}
		},
		DeleteFunc: func(obj interface{}) {
			if _, ok := obj.(kubeApiMeta.Object); !ok {
				// If the object doesn't have Metadata, assume it is a tombstone object
				// of type DeletedFinalStateUnknown
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				obj = tombstone.Obj
			}
			if filter(obj, name, namespace) {
				return
			}
			queue.Add(&work{})
		},
	}
}

func findOwnerRefs(client kubernetes.Interface, clusterRoleName string) []kubeApiMeta.OwnerReference {
	clusterRole, err := client.RbacV1().ClusterRoles().Get(clusterRoleName, kubeApiMeta.GetOptions{})
	if err != nil {
		scope.Warnf("Could not find clusterrole: %s to set ownerRef. "+
			"The webhook configuration must be deleted manually.",
			clusterRoleName)
		return nil
	}

	return []kubeApiMeta.OwnerReference{
		*kubeApiMeta.NewControllerRef(
			clusterRole,
			kubeApiRbac.SchemeGroupVersion.WithKind("ClusterRole"),
		),
	}
}

func New(o Options) *Controller {
	return newController(o, filewatcher.NewWatcher, ioutil.ReadFile, nil)
}

type readFileFunc func(filename string) ([]byte, error)

func newController(
	o Options,
	newFileWatcher filewatcher.NewFileWatcherFunc,
	readFile readFileFunc,
	reconcileDone func(),
) *Controller {
	c := &Controller{
		o:             o,
		queue:         workqueue.NewRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter()),
		caFileWatcher: newFileWatcher(),
		readFile:      readFile,
		reconcileDone: reconcileDone,
		ownerRefs:     findOwnerRefs(o.Client, "istiod-istio-system"),
	}

	c.sharedInformers = informers.NewSharedInformerFactoryWithOptions(o.Client, o.ResyncPeriod,
		informers.WithNamespace(o.WatchedNamespace))

	webhookInformer := c.sharedInformers.Admissionregistration().V1beta1().ValidatingWebhookConfigurations().Informer()
	webhookInformer.AddEventHandler(makeEventHandler(c.queue, o.ConfigName, ""))

	endpointInformer := c.sharedInformers.Core().V1().Endpoints().Informer()
	endpointInformer.AddEventHandler(makeEventHandler(c.queue, o.ServiceName, o.WatchedNamespace))

	deploymentInformer := c.sharedInformers.Apps().V1().Deployments().Informer()
	deploymentInformer.AddEventHandler(makeEventHandler(c.queue, o.GalleyDeployment, o.WatchedNamespace))

	return c
}

func (c *Controller) Start(stop <-chan struct{}) {
	go c.startFileWatcher(stop)
	go c.sharedInformers.Start(stop)

	for _, ready := range c.sharedInformers.WaitForCacheSync(stop) {
		if !ready {
			return
		}
	}

	// inject a deployment check to force the initial reconcilation check.
	c.queue.Add(&work{})

	go c.runWorker()
}

func (c *Controller) startFileWatcher(stop <-chan struct{}) {
	for {
		select {
		case <-c.caFileWatcher.Events(c.o.CAPath):
			c.queue.Add(&work{})
		case <-c.caFileWatcher.Errors(c.o.CAPath):
			// log only
		case <-stop:
			return
		}
	}
}

func (c *Controller) processDeployments() (stop bool, err error) {
	galley, err := c.sharedInformers.Apps().V1().
		Deployments().Lister().Deployments(c.o.WatchedNamespace).Get(c.o.GalleyDeployment)

	// galley does/doesn't exist
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			return false, nil
		}
		return true, err
	}

	// galley is scaled down to zero replicas. This is useful for debugging
	// to force the istiod controller to run.
	if galley.Spec.Replicas != nil && *galley.Spec.Replicas == 0 {
		return false, nil
	}
	return true, nil
}

func (c *Controller) processEndpoints() (stop bool, err error) {
	if c.endpointReadyOnce {
		return false, nil
	}
	endpoint, err := c.sharedInformers.Core().V1().
		Endpoints().Lister().Endpoints(c.o.WatchedNamespace).Get(c.o.ServiceName)
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			return true, nil
		}
		return true, err
	}
	for _, subset := range endpoint.Subsets {
		if len(subset.Addresses) > 0 {
			c.endpointReadyOnce = true
			return false, nil
		}
	}
	return true, nil
}

func (c *Controller) buildCABundle() ([]byte, error) {
	certBytes, err := c.readFile(c.o.CAPath)
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

func (c *Controller) buildConfig() (*kubeApiAdmission.ValidatingWebhookConfiguration, error) {
	encoded, err := c.readFile(c.o.ConfigPath)
	if err != nil {
		return nil, err
	}

	var config kubeApiAdmission.ValidatingWebhookConfiguration
	if err := runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), encoded, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *Controller) buildValidatingWebhookConfig() (*kubeApiAdmission.ValidatingWebhookConfiguration, error) {
	config, err := c.buildConfig()
	if err != nil {
		return nil, err
	}

	caBundle, err := c.buildCABundle()
	if err != nil {
		return nil, err
	}

	// update runtime fields
	config.OwnerReferences = c.ownerRefs
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caBundle
	}

	return config, nil
}

func (c *Controller) reconcile() error {
	defer func() {
		if c.reconcileDone != nil {
			c.reconcileDone()
		}
	}()

	// skip reconciliation if our endpoint isn't ready ...
	if stop, err := c.processEndpoints(); stop || err != nil {
		return err
	}
	// ... or another galley deployment is already managed the webhook.
	if stop, err := c.processDeployments(); stop || err != nil {
		return err
	}

	desired, err := c.buildValidatingWebhookConfig()
	if err != nil {
		return err
	}

	current, err := c.sharedInformers.Admissionregistration().V1beta1().
		ValidatingWebhookConfigurations().Lister().Get(c.o.ConfigName)
	if kubeErrors.IsNotFound(err) {
		_, err := c.o.Client.AdmissionregistrationV1beta1().
			ValidatingWebhookConfigurations().Create(desired)
		if err != nil {
			return err
		}
		return nil
	}

	updated := current.DeepCopyObject().(*kubeApiAdmission.ValidatingWebhookConfiguration)
	updated.Webhooks = desired.Webhooks
	updated.OwnerReferences = desired.OwnerReferences

	if !reflect.DeepEqual(updated, current) {
		_, err := c.o.Client.AdmissionregistrationV1beta1().
			ValidatingWebhookConfigurations().Update(updated)
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

	if _, ok := obj.(*work); !ok {
		// don't retry an invalid work item
		c.queue.Forget(obj)
		return true
	}

	if err := c.reconcile(); err != nil {
		c.queue.AddRateLimited(obj)
		utilruntime.HandleError(err)
	} else {
		c.queue.Forget(obj)
	}
	return true
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}
