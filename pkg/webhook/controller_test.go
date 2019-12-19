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
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	kubeApiAdmission "k8s.io/api/admissionregistration/v1beta1"
	kubeApiApp "k8s.io/api/apps/v1"
	kubeApiCore "k8s.io/api/core/v1"
	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	kubeApisMeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubeTypedAdmission "k8s.io/client-go/kubernetes/typed/admissionregistration/v1beta1"
	kubeTypedApp "k8s.io/client-go/kubernetes/typed/apps/v1"
	kubeTypedCore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubectl/pkg/scheme"

	"istio.io/pkg/filewatcher"
)

var (
	istiodEndpoint = &kubeApiCore.Endpoints{
		ObjectMeta: kubeApisMeta.ObjectMeta{
			Name:      istiod,
			Namespace: namespace,
		},
		Subsets: []kubeApiCore.EndpointSubset{{
			Addresses: []kubeApiCore.EndpointAddress{{
				IP: "192.168.1.1",
			}},
		}},
	}

	galleyDeployment = &kubeApiApp.Deployment{
		ObjectMeta: kubeApisMeta.ObjectMeta{
			Name:      galley,
			Namespace: namespace,
		},
		Spec: kubeApiApp.DeploymentSpec{
			Replicas: &[]int32{1}[0],
		},
	}

	unpatchedIstiodWebhookConfig = &kubeApiAdmission.ValidatingWebhookConfiguration{
		TypeMeta: kubeApisMeta.TypeMeta{
			APIVersion: kubeApiAdmission.SchemeGroupVersion.String(),
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: kubeApisMeta.ObjectMeta{Name: galley},
		Webhooks: []kubeApiAdmission.ValidatingWebhook{{
			Name: "hook0",
			ClientConfig: kubeApiAdmission.WebhookClientConfig{Service: &kubeApiAdmission.ServiceReference{
				Namespace: namespace,
				Name:      istiod,
				Path:      &[]string{"/hook0"}[0],
			}},
			Rules: []kubeApiAdmission.RuleWithOperations{{
				Operations: []kubeApiAdmission.OperationType{kubeApiAdmission.Create, kubeApiAdmission.Update},
				Rule: kubeApiAdmission.Rule{
					APIGroups:   []string{"group0"},
					APIVersions: []string{"*"},
					Resources:   []string{"*"},
				},
			}},
		}, {
			Name: "hook1",
			ClientConfig: kubeApiAdmission.WebhookClientConfig{Service: &kubeApiAdmission.ServiceReference{
				Namespace: namespace,
				Name:      istiod,
				Path:      &[]string{"/hook1"}[0],
			}},
			Rules: []kubeApiAdmission.RuleWithOperations{{
				Operations: []kubeApiAdmission.OperationType{kubeApiAdmission.Create, kubeApiAdmission.Update},
				Rule: kubeApiAdmission.Rule{
					APIGroups:   []string{"group1"},
					APIVersions: []string{"*"},
					Resources:   []string{"*"},
				},
			}},
		}},
	}
	istiodWebhookConfigEncoded = runtime.EncodeOrDie(scheme.Codecs.LegacyCodec(kubeApiAdmission.SchemeGroupVersion), unpatchedIstiodWebhookConfig)
	finalIstiodWebhookConfig   *kubeApiAdmission.ValidatingWebhookConfiguration
	galleyWebhookConfig        *kubeApiAdmission.ValidatingWebhookConfiguration

	caBundle0 = []byte(`-----BEGIN CERTIFICATE-----
MIIC9DCCAdygAwIBAgIJAIFe3lWPaalKMA0GCSqGSIb3DQEBCwUAMA4xDDAKBgNV
BAMMA19jYTAgFw0xNzEyMjIxODA0MjRaGA8yMjkxMTAwNzE4MDQyNFowDjEMMAoG
A1UEAwwDX2NhMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAuBdxj+Hi
8h0TkId1f64TprLydwgzzLwXAs3wpmXz+BfnW1oMQPNyN7vojW6VzqJGGYLsc1OB
MgwObU/VeFNc6YUCmu6mfFJwoPfXMPnhmGuSwf/kjXomlejAYjxClU3UFVWQht54
xNLjTi2M1ZOnwNbECOhXC3Tw3G8mCtfanMAO0UXM5yObbPa8yauUpJKkpoxWA7Ed
qiuUD9qRxluFPqqw/z86V8ikmvnyjQE9960j+8StlAbRs82ArtnrhRgkDO0Smtf7
4QZsb/hA1KNMm73bOGS6+SVU+eH8FgVOzcTQYFRpRT3Mhi6dKZe9twIO8mpZK4wk
uygRxBM32Ag9QQIDAQABo1MwUTAdBgNVHQ4EFgQUc8tvoNNBHyIkoVV8XCXy63Ya
BEQwHwYDVR0jBBgwFoAUc8tvoNNBHyIkoVV8XCXy63YaBEQwDwYDVR0TAQH/BAUw
AwEB/zANBgkqhkiG9w0BAQsFAAOCAQEAVmaUkkYESfcfgnuPeZ4sTNs2nk2Y+Xpd
lxkMJhChb8YQtlCe4uiLvVe7er1sXcBLNCm/+2K9AT71gnxBSeS5mEOzWmCPErhy
RmYtSxeRyXAaUWVYLs/zMlBQ0Iz4dpY+FVVbMjIurelVwHF0NBk3VtU5U3lHyKdZ
j4C2rMjvTxmkyIcR1uBEeVvuGU8R70nZ1yfo3vDwmNGMcLwW+4QK+WcfwfjLXhLs
5550arfEYdTzYFMxY60HJT/LvbGrjxY0PQUWWDbPiRfsdRjOFduAbM0/EVRda/Oo
Fg72WnHeojDUhqEz4UyFZbnRJ4x6leQhnrIcVjWX4FFFktiO9rqqfw==
-----END CERTIFICATE-----`)

	caBundle1 = []byte(`-----BEGIN CERTIFICATE-----
MIIDCzCCAfOgAwIBAgIQbfOzhcKTldFipQ1X2WXpHDANBgkqhkiG9w0BAQsFADAv
MS0wKwYDVQQDEyRhNzU5YzcyZC1lNjcyLTQwMzYtYWMzYy1kYzAxMDBmMTVkNWUw
HhcNMTkwNTE2MjIxMTI2WhcNMjQwNTE0MjMxMTI2WjAvMS0wKwYDVQQDEyRhNzU5
YzcyZC1lNjcyLTQwMzYtYWMzYy1kYzAxMDBmMTVkNWUwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQC6sSAN80Ci0DYFpNDumGYoejMQai42g6nSKYS+ekvs
E7uT+eepO74wj8o6nFMNDu58+XgIsvPbWnn+3WtUjJfyiQXxmmTg8om4uY1C7R1H
gMsrL26pUaXZ/lTE8ZV5CnQJ9XilagY4iZKeptuZkxrWgkFBD7tr652EA3hmj+3h
4sTCQ+pBJKG8BJZDNRrCoiABYBMcFLJsaKuGZkJ6KtxhQEO9QxJVaDoSvlCRGa8R
fcVyYQyXOZ+0VHZJQgaLtqGpiQmlFttpCwDiLfMkk3UAd79ovkhN1MCq+O5N7YVt
eVQWaTUqUV2tKUFvVq21Zdl4dRaq+CF5U8uOqLY/4Kg9AgMBAAGjIzAhMA4GA1Ud
DwEB/wQEAwICBDAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3DQEBCwUAA4IBAQCg
oF71Ey2b1QY22C6BXcANF1+wPzxJovFeKYAnUqwh3rF7pIYCS/adZXOKlgDBsbcS
MxAGnCRi1s+A7hMYj3sQAbBXttc31557lRoJrx58IeN5DyshT53t7q4VwCzuCXFT
3zRHVRHQnO6LHgZx1FuKfwtkhfSXDyYU2fQYw2Hcb9krYU/alViVZdE0rENXCClq
xO7AQk5MJcGg6cfE5wWAKU1ATjpK4CN+RTn8v8ODLoI2SW3pfsnXxm93O+pp9HN4
+O+1PQtNUWhCfh+g6BN2mYo2OEZ8qGSxDlMZej4YOdVkW8PHmFZTK0w9iJKqM5o1
V6g5gZlqSoRhICK09tpc
-----END CERTIFICATE-----`)

	endpointNotFoundErr = kubeErrors.NewNotFound(kubeApiCore.Resource("endpoints"), "istiod")
	configNotFoundErr   = kubeErrors.NewNotFound(kubeApiAdmission.Resource("validatingwebhookconfigurations"), galleyWebhookName)
)

// patch the caBundle into the final istiod and galley configs.
func init() {
	finalIstiodWebhookConfig = unpatchedIstiodWebhookConfig.DeepCopy()
	finalIstiodWebhookConfig.Webhooks[0].ClientConfig.CABundle = caBundle0
	finalIstiodWebhookConfig.Webhooks[1].ClientConfig.CABundle = caBundle0

	galleyWebhookConfig = finalIstiodWebhookConfig.DeepCopy()
	galleyWebhookConfig.Webhooks[0].ClientConfig.Service.Name = galley
	galleyWebhookConfig.Webhooks[1].ClientConfig.Service.Name = galley
	galleyWebhookConfig.Webhooks[0].ClientConfig.CABundle = caBundle1
	galleyWebhookConfig.Webhooks[1].ClientConfig.CABundle = caBundle1
}

type fakeController struct {
	*Controller

	caChangedCh     chan bool
	configChangedCh chan bool
	reconcileDoneCh chan struct{}
	fakeWatcher     *filewatcher.FakeWatcher
	fakeClient      *fake.Clientset
}

func (fc *fakeController) ValidatingWebhookConfigurations() kubeTypedAdmission.ValidatingWebhookConfigurationInterface {
	return fc.o.Client.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations()
}

func (fc *fakeController) Endpoints() kubeTypedCore.EndpointsInterface {
	return fc.o.Client.CoreV1().Endpoints(fc.o.WatchedNamespace)
}

func (fc *fakeController) Deployments() kubeTypedApp.DeploymentInterface {
	return fc.o.Client.AppsV1().Deployments(fc.o.WatchedNamespace)
}

const (
	namespace            = "istio-system"
	galley               = "istio-galley"
	galleyDeploymentName = "istio-galley"
	galleyWebhookName    = "istio-galley"
	istiod               = "istiod"
	caPath               = "fakeCAPath"
	configPath           = "fakeConfigPath"
)

func createTestController() *fakeController {
	fakeClient := fake.NewSimpleClientset()
	o := Options{
		WatchedNamespace: namespace,
		ResyncPeriod:     time.Minute,
		CAPath:           caPath,
		ConfigPath:       configPath,
		ConfigName:       galleyWebhookName,
		ServiceName:      istiod,
		Client:           fakeClient,
		GalleyDeployment: galleyDeploymentName,
	}

	caChanged := make(chan bool, 10)
	configChanged := make(chan bool, 10)
	changed := func(path string, added bool) {
		switch path {
		case o.CAPath:
			caChanged <- added
		case o.ConfigPath:
			configChanged <- added
		}
	}

	readFile := func(filename string) ([]byte, error) {
		switch filename {
		case o.CAPath:
			return caBundle0, nil
		case o.ConfigPath:
			return []byte(istiodWebhookConfigEncoded), nil
		}
		return nil, os.ErrNotExist
	}

	newFileWatcher, fakeWatcher := filewatcher.NewFakeWatcher(changed)

	reconcileDoneCh := make(chan struct{}, 1000)
	reconcileDone := func() {
		reconcileDoneCh <- struct{}{}
	}

	return &fakeController{
		Controller:      newController(o, newFileWatcher, readFile, reconcileDone),
		caChangedCh:     caChanged,
		configChangedCh: configChanged,
		reconcileDoneCh: reconcileDoneCh,
		fakeWatcher:     fakeWatcher,
		fakeClient:      fakeClient,
	}
}

func TestController_Greenfield(t *testing.T) {
	g := NewGomegaWithT(t)
	c := createTestController()

	stop := make(chan struct{})
	defer func() { close(stop) }()
	c.Start(stop)

	g.Consistently(func() error {
		_, err := c.ValidatingWebhookConfigurations().Get(galleyWebhookName, kubeApisMeta.GetOptions{})
		return err
	}).Should(MatchError(configNotFoundErr))

	g.Expect(c.o.Client.CoreV1().Endpoints(c.o.WatchedNamespace).Create(istiodEndpoint)).Should(Succeed())

	g.Eventually(func() *kubeApiAdmission.ValidatingWebhookConfiguration {
		config, _ := c.ValidatingWebhookConfigurations().Get(galleyWebhookName, kubeApisMeta.GetOptions{})
		return config
	}, time.Second).Should(Equal(finalIstiodWebhookConfig))

	g.Eventually(func() *kubeApiAdmission.ValidatingWebhookConfiguration {
		config, _ := c.ValidatingWebhookConfigurations().Get(galleyWebhookName, kubeApisMeta.GetOptions{})
		return config
	}, time.Second).Should(Equal(finalIstiodWebhookConfig))
}

func TestController_Upgrade(t *testing.T) {
	g := NewGomegaWithT(t)
	c := createTestController()

	stop := make(chan struct{})
	defer func() { close(stop) }()
	c.Start(stop)

	// simulate an existing install with Galley.
	g.Expect(c.Deployments().Create(galleyDeployment)).Should(Succeed())
	g.Expect(c.ValidatingWebhookConfigurations().Create(galleyWebhookConfig)).Should(Succeed())

	g.Expect(c.Endpoints().Create(istiodEndpoint)).Should(Succeed())

	// install istiod validation webhook
	g.Consistently(func() error {
		_, err := c.ValidatingWebhookConfigurations().Get(galleyWebhookName, kubeApisMeta.GetOptions{})
		return err
	}).Should(Equal(galleyWebhookConfig))

	g.Eventually(func() *kubeApiAdmission.ValidatingWebhookConfiguration {
		config, _ := c.ValidatingWebhookConfigurations().Get(galleyWebhookName, kubeApisMeta.GetOptions{})
		return config
	}, time.Second).Should(Equal(finalIstiodWebhookConfig))
}
