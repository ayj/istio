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

	"istio.io/istio/mixer/pkg/validate"
	"istio.io/istio/pkg/config/schemas"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/webhook/controller"
	"istio.io/istio/pkg/webhook/server"

	"istio.io/pkg/env"
)

var (
	validationWebhookControllerEnabled = env.RegisterBoolVar("VALIDATION_CONTROLLER", false,
		"Whether or not to run the webhook configuration controller.")
)

// TODO - move these to a common file with other istiod constants and envvars
const (
	validationWebhookName = "istio-galley"
	webhookConfigPath     = "/etc/istio/config/validation"
)

func (s *Server) initConfigValidation(args *PilotArgs) error {
	params := server.Options{
		MixerValidator:  validate.NewDefaultValidator(false),
		PilotDescriptor: schemas.Istio,
		DomainSuffix:    args.Config.ControllerOptions.DomainSuffix,
		Port:            9443, // TOOD - move to 15000 range
		CertFile:        filepath.Join(dnsCertDir, "cert-chain.pem"),
		KeyFile:         filepath.Join(dnsCertDir, "key.pem"),
		Enabled:         true, // always run the http server.
		Mux:             s.mux,
	}
	whServer, err := server.New(params)
	if err != nil {
		return err
	}

	s.addStartFunc(func(stop <-chan struct{}) error {
		whServer.Run(stop)
		return nil
	})

	if validationWebhookControllerEnabled.Get() {
		client, err := kube.CreateClientset(args.Config.KubeConfig, "")
		if err != nil {
			return err
		}
		o := controller.Options{
			Client:               client,
			WatchedNamespace:     args.Namespace,
			CAPath:               defaultCACertPath,
			WebhookConfigName:    validationWebhookName,
			WebhookConfigPath:    webhookConfigPath,
			ServiceName:          "istio-pilot",
			GalleyDeploymentName: "istio-galley",
			ClusterRoleName:      "istio-pilot-" + args.Namespace,
		}
		controller, err := controller.New(o)
		if err != nil {
			return err
		}

		s.addStartFunc(func(stop <-chan struct{}) error {
			go controller.Start(stop)
			return nil
		})
	}
	return nil
}
