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

package controller

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"reflect"
	"regexp"
	"time"

	"github.com/hashicorp/go-multierror"
	"k8s.io/api/admissionregistration/v1beta1"
	kubeApiAdmission "k8s.io/api/admissionregistration/v1beta1"
	kubeApiApp "k8s.io/api/apps/v1"
	kubeApiCore "k8s.io/api/core/v1"
	kubeApiRbac "k8s.io/api/rbac/v1"
	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	kubeApiMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	Client kubernetes.Interface

	// Istio system namespace in which galley and istiod reside.
	WatchedNamespace string

	// Periodically resync with the kube-apiserver. Set to zero to disable.
	ResyncPeriod time.Duration

	// File path to the x509 certificate bundle used by the webhook server
	// and patched into the webhook config.
	CAPath string

	// Name of the k8s validatingwebhookconfiguration resource. This should
	// match the name in the config template.
	WebhookConfigName string

	// File path to the validatingwebhookconfiguration template
	WebhookConfigPath string

	// Name of the service running the webhook server.
	ServiceName string

	// name of the galley deployment in the watched namespace.
	// When non-empty the controller will defer reconciling config
	// until the named deployment no longer exists.
	GalleyDeploymentName string

	// Name of the ClusterRole that the controller should assign
	// cluster-scoped ownership to. The webhook config will be GC'c
	// when this ClusterRole is deleted.
	ClusterRoleName string

	// Enable galley validation controller mode
	Enabled bool
}

const (
	dns1123LabelMaxLength int    = 63
	dns1123LabelFmt       string = "[a-zA-Z0-9]([-a-z-A-Z0-9]*[a-zA-Z0-9])?"
)

var dns1123LabelRegexp = regexp.MustCompile("^" + dns1123LabelFmt + "$")

// isDNS1123Label tests for a string that conforms to the definition of a label in
// DNS (RFC 1123).
func isDNS1123Label(value string) bool {
	return len(value) <= dns1123LabelMaxLength && dns1123LabelRegexp.MatchString(value)
}

func (o Options) Validate() error {
	var errs *multierror.Error
	// Validate the options that exposed to end users
	if o.WebhookConfigName == "" || !isDNS1123Label(o.WebhookConfigName) {
		errs = multierror.Append(errs, fmt.Errorf("invalid webhook name: %q", o.WebhookConfigName)) // nolint: lll
	}
	if o.WatchedNamespace == "" || !isDNS1123Label(o.WatchedNamespace) {
		errs = multierror.Append(errs, fmt.Errorf("invalid namespace: %q", o.WatchedNamespace)) // nolint: lll
	}
	if o.GalleyDeploymentName != "" && !isDNS1123Label(o.GalleyDeploymentName) {
		errs = multierror.Append(errs, fmt.Errorf("invalid deployment name: %q", o.GalleyDeploymentName))
	}
	if o.ServiceName == "" || !isDNS1123Label(o.ServiceName) {
		errs = multierror.Append(errs, fmt.Errorf("invalid service name: %q", o.ServiceName))
	}
	if len(o.CAPath) == 0 {
		errs = multierror.Append(errs, errors.New("CA cert file not specified"))
	}
	if len(o.WebhookConfigPath) == 0 {
		errs = multierror.Append(errs, errors.New("webhook config file not specified"))
	}
	return errs
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

type reconcileRequest struct {
	description string
}

func (rr reconcileRequest) String() string {
	return rr.description
}

func filter(in interface{}, wantName, wantNamespace string) (skip bool, key string) {
	obj, err := meta.Accessor(in)
	if err != nil {
		skip = true
		return
	}
	if wantNamespace != "" && obj.GetNamespace() != wantNamespace {
		skip = true
		return
	}
	if wantName != "" && obj.GetName() != wantName {
		skip = true
		return
	}

	// ignore the error because there's nothing to do if this fails.
	key, _ = cache.DeletionHandlingMetaNamespaceKeyFunc(in)
	return
}

func makeHandler(queue workqueue.Interface, gvk schema.GroupVersionKind, nameMatch, namespaceMatch string) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			skip, key := filter(obj, nameMatch, namespaceMatch)
			scope.Debugf("HandlerAdd: key=%v skip=%v", key, skip)
			if skip {
				return
			}

			req := &reconcileRequest{fmt.Sprintf("adding (%v, Kind=%v) %v", gvk.GroupVersion(), gvk.Kind, key)}
			queue.Add(req)
		},
		UpdateFunc: func(prev, curr interface{}) {
			skip, key := filter(curr, nameMatch, namespaceMatch)
			scope.Debugf("HandlerUpdate: key=%v skip=%v", key, skip)
			if skip {
				return
			}
			if !reflect.DeepEqual(prev, curr) {
				req := &reconcileRequest{fmt.Sprintf("update (%v, Kind=%v) %v", gvk.GroupVersion(), gvk.Kind, key)}
				queue.Add(req)
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
			skip, key := filter(obj, nameMatch, namespaceMatch)
			scope.Debugf("HandlerDelete: key=%v skip=%v", key, skip)
			if skip {
				return
			}
			req := &reconcileRequest{fmt.Sprintf("delete (%v, Kind=%v) %v", gvk.GroupVersion(), gvk.Kind, key)}
			queue.Add(req)
		},
	}
}

func New(o Options) (*Controller, error) {
	return newController(o, filewatcher.NewWatcher, ioutil.ReadFile, nil)
}

type readFileFunc func(filename string) ([]byte, error)

// precompute GVK for known types.
var (
	configGVK     = kubeApiAdmission.SchemeGroupVersion.WithKind(reflect.TypeOf(kubeApiAdmission.ValidatingWebhookConfiguration{}).Name())
	endpointGVK   = kubeApiCore.SchemeGroupVersion.WithKind(reflect.TypeOf(kubeApiCore.Endpoints{}).Name())
	deploymentGVK = kubeApiApp.SchemeGroupVersion.WithKind(reflect.TypeOf(kubeApiApp.Deployment{}).Name())
)

func findClusterRoleOwnerRefs(client kubernetes.Interface, clusterRoleName string) []kubeApiMeta.OwnerReference {
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

func newController(
	o Options,
	newFileWatcher filewatcher.NewFileWatcherFunc,
	readFile readFileFunc,
	reconcileDone func(),
) (*Controller, error) {
	caFileWatcher := newFileWatcher()
	if err := caFileWatcher.Add(o.WebhookConfigPath); err != nil {
		return nil, err
	}
	if err := caFileWatcher.Add(o.CAPath); err != nil {
		return nil, err
	}

	c := &Controller{
		o:             o,
		queue:         workqueue.NewRateLimitingQueue(workqueue.DefaultItemBasedRateLimiter()),
		caFileWatcher: caFileWatcher,
		readFile:      readFile,
		reconcileDone: reconcileDone,
		ownerRefs:     findClusterRoleOwnerRefs(o.Client, o.ClusterRoleName),
	}

	c.sharedInformers = informers.NewSharedInformerFactoryWithOptions(o.Client, o.ResyncPeriod,
		informers.WithNamespace(o.WatchedNamespace))

	webhookInformer := c.sharedInformers.Admissionregistration().V1beta1().ValidatingWebhookConfigurations().Informer()
	webhookInformer.AddEventHandler(makeHandler(c.queue, configGVK, o.WebhookConfigName, ""))

	endpointInformer := c.sharedInformers.Core().V1().Endpoints().Informer()
	endpointInformer.AddEventHandler(makeHandler(c.queue, endpointGVK, o.ServiceName, o.WatchedNamespace))

	deploymentInformer := c.sharedInformers.Apps().V1().Deployments().Informer()
	deploymentInformer.AddEventHandler(makeHandler(c.queue, deploymentGVK, o.GalleyDeploymentName, o.WatchedNamespace))

	return c, nil
}

func (c *Controller) Start(stop <-chan struct{}) {
	go c.startFileWatcher(stop)
	go c.sharedInformers.Start(stop)

	for _, ready := range c.sharedInformers.WaitForCacheSync(stop) {
		if !ready {
			return
		}
	}

	req := &reconcileRequest{"initial request to kickstart reconcilation"}
	c.queue.Add(req)

	go c.runWorker()
}

func (c *Controller) startFileWatcher(stop <-chan struct{}) {
	for {
		select {
		case ev := <-c.caFileWatcher.Events(c.o.WebhookConfigPath):
			req := &reconcileRequest{fmt.Sprintf("Webhook configuration changed: %v", ev)}
			c.queue.Add(req)
		case ev := <-c.caFileWatcher.Events(c.o.CAPath):
			req := &reconcileRequest{fmt.Sprintf("CA file changed: %v", ev)}
			c.queue.Add(req)
		case err := <-c.caFileWatcher.Errors(c.o.WebhookConfigPath):
			scope.Warnf("error watching local webhook configuration: %v", err)
		case err := <-c.caFileWatcher.Errors(c.o.CAPath):
			scope.Warnf("error watching local CA bundle: %v", err)
		case <-stop:
			return
		}
	}
}

func (c *Controller) processDeployments() (stop bool, err error) {
	if c.o.GalleyDeploymentName == "" {
		return false, nil
	}

	galley, err := c.sharedInformers.Apps().V1().
		Deployments().Lister().Deployments(c.o.WatchedNamespace).Get(c.o.GalleyDeploymentName)

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

// Return nil when the endpoint is ready. Otherwise, return an error.
func endpointReady(endpoint *kubeApiCore.Endpoints) (ready bool, reason string) {
	if len(endpoint.Subsets) == 0 {
		return false, "no subsets"
	}
	for _, subset := range endpoint.Subsets {
		if len(subset.Addresses) > 0 {
			return true, ""
		}
	}
	return false, "no subset addresses ready"
}

func (c *Controller) processEndpoints() (stop bool, err error) {
	if c.endpointReadyOnce {
		return true, nil
	}
	endpoint, err := c.sharedInformers.Core().V1().
		Endpoints().Lister().Endpoints(c.o.WatchedNamespace).Get(c.o.ServiceName)
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			return true, nil
		}
		return true, err
	}
	ready, _ := endpointReady(endpoint)
	return !ready, nil
}

// reconcile the desired state with the kube-apiserver.
func (c *Controller) reconcile(req *reconcileRequest) error {
	defer func() {
		if c.reconcileDone != nil {
			c.reconcileDone()
		}
	}()

	scope.Infof("Reconcile: %v", req)

	// skip reconciliation if our endpoint isn't ready ...
	if stop, err := c.processEndpoints(); stop || err != nil {
		return err
	}

	// ... or another galley deployment is already managed the webhook.
	if stop, err := c.processDeployments(); stop || err != nil {
		return err
	}

	desired, err := c.buildValidatingWebhookConfiguration()
	if err != nil {
		reportValidationConfigLoadError(err.(*configError).Reason())
		// no point in retrying unless a local config or cert file changes.
		return nil
	}

	current, err := c.sharedInformers.Admissionregistration().V1beta1().
		ValidatingWebhookConfigurations().Lister().Get(c.o.WebhookConfigName)
	if kubeErrors.IsNotFound(err) {
		_, err := c.o.Client.AdmissionregistrationV1beta1().
			ValidatingWebhookConfigurations().Create(desired)
		if err != nil {
			reportValidationConfigUpdateError(kubeErrors.ReasonForError(err)) // TODO
			return err
		}
		reportValidationConfigUpdate()
		return nil
	}

	updated := current.DeepCopyObject().(*kubeApiAdmission.ValidatingWebhookConfiguration)
	updated.Webhooks = desired.Webhooks
	updated.OwnerReferences = desired.OwnerReferences

	if !reflect.DeepEqual(updated, current) {
		_, err := c.o.Client.AdmissionregistrationV1beta1().
			ValidatingWebhookConfigurations().Update(updated)
		if err != nil {
			reportValidationConfigUpdateError(kubeErrors.ReasonForError(err)) // TODO
			return err
		}
	}
	reportValidationConfigUpdate()

	return nil
}

func (c *Controller) processNextWorkItem() (cont bool) {
	obj, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(obj)

	req, ok := obj.(*reconcileRequest)
	if !ok {
		// don't retry an invalid reconcileRequest item
		c.queue.Forget(req)
		return true
	}

	if err := c.reconcile(req); err != nil {
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

type configError struct {
	err    error
	reason string
}

func (e configError) Error() string {
	return e.err.Error()
}

func (e configError) Reason() string {
	return e.reason
}

func (c *Controller) buildValidatingWebhookConfiguration() (*kubeApiAdmission.ValidatingWebhookConfiguration, error) {
	webhook, err := c.readFile(c.o.WebhookConfigPath)
	if err != nil {
		return nil, &configError{err, "failed to load webhook webhook config"}
	}
	caBundle, err := c.readFile(c.o.CAPath)
	if err != nil {
		return nil, &configError{err, "failed to load caBundle"}
	}
	return buildValidatingWebhookConfiguration(caBundle, webhook, c.ownerRefs)
}

func buildValidatingWebhookConfiguration(
	caBundle, webhook []byte,
	ownerRefs []kubeApiMeta.OwnerReference,
) (*v1beta1.ValidatingWebhookConfiguration, error) {
	config, err := decodeValidatingConfig(webhook)
	if err != nil {
		return nil, &configError{err, "config decode failed"}
	}
	if err := verifyCABundle(caBundle); err != nil {
		return nil, &configError{err, "caBundle verification failed"}
	}
	// update runtime fields
	config.OwnerReferences = ownerRefs
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caBundle
	}

	return config, nil
}

func decodeValidatingConfig(encoded []byte) (*v1beta1.ValidatingWebhookConfiguration, error) {
	var config kubeApiAdmission.ValidatingWebhookConfiguration
	if err := runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), encoded, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func verifyCABundle(caBundle []byte) error {
	block, _ := pem.Decode(caBundle)
	if block == nil {
		return errors.New("could not decode pem")
	}
	if block.Type != "CERTIFICATE" {
		return fmt.Errorf("cert contains wrong pem type: %q", block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("cert contains invalid x509 certificate: %v", err)
	}
	return nil
}
