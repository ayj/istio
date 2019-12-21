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

package bootstrap

import (
	"path/filepath"

	"istio.io/pkg/env"

	"istio.io/istio/galley/pkg/crd/validation"
	"istio.io/istio/mixer/pkg/validate"
	"istio.io/istio/pkg/config/schemas"
	"istio.io/istio/pkg/webhook/controller"
)

var (
	validationWebhookServer = env.RegisterBoolVar("VALIDATION_WEBHOOK_SERVER", true,
		"Enable the configuration validation webhook server")
	validationWebhookController = env.RegisterBoolVar("VALIDATION_WEBHOOK_CONTROLLER", true,
		"Enable the configuration validation webhook controller")
	validationWebhookName = env.RegisterStringVar("VALIDATION_WEBHOOK_NAME", "istio-galley",
		"Name of the validatingwebhookconfiguration")
	validationWebhookConfigPath = env.RegisterStringVar("VALIDATION_WEBHOOK_CONFIG_PATH",
		"/var/lib/istio/validation/config.yaml",
		"Full path to the validatingwebhookconfiguration file")
	validationWebhookServiceName = env.RegisterStringVar("VALIDATION_WEBHOOK_SERVICE_NAME", "pilot",
		"istiod service name")
)

const (
	galleyDeploymentName = "istio-galley"
)

// TODO - move these to a common file with other istiod constants and envvars

func (s *Server) initConfigValidation(args *PilotArgs) error {
	if !validationWebhookServer.Get() && !validationWebhookController.Get() {
		return nil
	}

	if validationWebhookServer.Get() {
		serverParams := validation.WebhookParameters{
			MixerValidator:                validate.NewDefaultValidator(false),
			PilotDescriptor:               schemas.Istio,
			DomainSuffix:                  args.Config.ControllerOptions.DomainSuffix,
			Port:                          9443, // TOOD - move to 15000 range
			CertFile:                      filepath.Join(DNSCertDir, "cert-chain.pem"),
			KeyFile:                       filepath.Join(DNSCertDir, "key.pem"),
			WebhookConfigFile:             validationWebhookConfigPath.Get(),
			CACertFile:                    defaultCACertPath,
			DeploymentAndServiceNamespace: args.Namespace,
			WebhookName:                   validationWebhookName.Get(),
			DeploymentName:                validationWebhookServiceName.Get(),
			ServiceName:                   validationWebhookServiceName.Get(),
			Clientset:                     s.kubeClient,
			EnableValidation:              validationWebhookServer.Get(),
			Mux:                           s.mux,
		}
		c, err := validation.NewWebhook(serverParams)
		if err != nil {
			return err
		}

		s.addStartFunc(func(stop <-chan struct{}) error {
			go c.Run(stop)
			return nil
		})
	}

	if validationWebhookServer.Get() {
		opts := controller.Options{
			WatchedNamespace:  args.Namespace,
			ResyncPeriod:      0,
			CAPath:            defaultCACertPath,
			ConfigPath:        validationWebhookConfigPath.Get(),
			WebhookConfigName: validationWebhookName.Get(),
			ServiceName:       "istiod",
			Client:            s.kubeClient,
			GalleyDeployment:  galleyDeploymentName,
			ClusterRoleName:   "istiod-istio-system",
		}
		c := controller.New(opts)

		s.addStartFunc(func(stop <-chan struct{}) error {
			c.Start(stop)
			return nil
		})
	}

	return nil
}
