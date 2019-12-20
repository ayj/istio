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

	"k8s.io/api/admissionregistration/v1beta1"
	kubeApiAdmission "k8s.io/api/admissionregistration/v1beta1"
	kubeApiCore "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	kubeApiRbac "k8s.io/api/rbac/v1"
	kubeApiMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	endpointsInformerV1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubectl/pkg/scheme"
)

// Return nil when the endpoint is ready. Otherwise, return an error.
func EndpointReady(endpoint *kubeApiCore.Endpoints) (ready bool, reason string) {
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

func VerifyCABundle(caBundle []byte) error {
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

func DecodeValidatingConfig(encoded []byte) (*v1beta1.ValidatingWebhookConfiguration, error) {
	var config kubeApiAdmission.ValidatingWebhookConfiguration
	if err := runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), encoded, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func BuildValidatingWebhookConfiguration(
	caBundle, webhook []byte,
	ownerRefs []kubeApiMeta.OwnerReference,
) (*v1beta1.ValidatingWebhookConfiguration, error) {
	config, err := DecodeValidatingConfig(webhook)
	if err != nil {
		return nil, err
	}
	if err := VerifyCABundle(caBundle); err != nil {
		return nil, err
	}
	// update runtime fields
	config.OwnerReferences = ownerRefs
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caBundle
	}

	// fill in missing defaults to minimize desired vs. actual diffs later.
	// TODO is this still needed if we're using 'scheme.Codecs.UniversalDecoder'?
	//for i := 0; i < len(config.Webhooks); i++ {
	//	if config.Webhooks[i].FailurePolicy == nil {
	//		failurePolicy := v1beta1.Fail
	//		config.Webhooks[i].FailurePolicy = &failurePolicy
	//	}
	//	if config.Webhooks[i].NamespaceSelector == nil {
	//		config.Webhooks[i].NamespaceSelector = &kubeApiMeta.LabelSelector{}
	//	}
	//}

	return config, nil
}

func BuildValidatingWebhookConfigurationFromFiles(
	caFile, webhookFile string,
	ownerRefs []kubeApiMeta.OwnerReference,
) (*v1beta1.ValidatingWebhookConfiguration, error) {
	caBundle, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	webhook, err := ioutil.ReadFile(webhookFile)
	if err != nil {
		return nil, err
	}
	return BuildValidatingWebhookConfiguration(caBundle, webhook, ownerRefs)
}

func FindClusterRoleOwnerRefs(client kubernetes.Interface, clusterRoleName string) []kubeApiMeta.OwnerReference {
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

func WaitForEndpointReady(stopCh <-chan struct{}, client kubernetes.Interface, service, namespace string) (shutdown bool) {
	scope.Infof("Checking if %s/%s is ready before registering webhook configuration ",
		namespace, service)

	defer func() {
		if shutdown {
			scope.Info("Endpoint readiness check stopped - controller shutting down")
		} else {
			scope.Infof("Endpoint %s/%s is ready", namespace, service)
		}
	}()

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer queue.ShutDown()

	informer := endpointsInformerV1.NewEndpointsInformer(client, namespace, 0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	informer.AddEventHandler(&cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(prev, curr interface{}) {
			prevObj := prev.(*kubeApiCore.Endpoints)
			currObj := curr.(*kubeApiCore.Endpoints)
			if prevObj.ResourceVersion != currObj.ResourceVersion {
				if key, err := cache.MetaNamespaceKeyFunc(curr); err == nil {
					queue.Add(key)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
				queue.Add(key)
			}
		},
	})

	controllerStopCh := make(chan struct{})
	defer close(controllerStopCh)
	go informer.Run(controllerStopCh)

	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		scope.Errorf("wait for cache sync failed")
		return true
	}

	for {
		select {
		case <-stopCh:
			return true
		default:
			ready, shutdown := func() (bool, bool) {
				key, quit := queue.Get()
				if quit {
					return false, true
				}
				defer queue.Done(key)

				item, exists, err := informer.GetStore().GetByKey(key.(string))
				if err != nil || !exists {
					return false, false
				}
				endpoints, ok := item.(*v1.Endpoints)
				if !ok {
					return false, false
				}
				ready, reason := EndpointReady(endpoints)
				if !ready {
					scope.Warnf("%s/%v endpoint not ready: %v", namespace, service, reason)
				}
				return ready, false
			}()

			switch {
			case shutdown:
				return true
			case ready:
				return true
			default:
				// continue waiting for endpoint to be ready
			}
		}
	}
}
