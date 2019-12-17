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
	"time"

	"istio.io/pkg/env"
	"istio.io/pkg/probe"
	"k8s.io/client-go/kubernetes"

	"istio.io/istio/galley/pkg/crd/validation"
	"istio.io/istio/galley/pkg/server/components"
	mixervalidate "istio.io/istio/mixer/pkg/validate"
	"istio.io/istio/pkg/config/schemas"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/webhook"
)

var (
	validationWebhookConfigName = env.RegisterStringVar("VALIDATION_WEBHOOK_NAME", "",
		"Name of the validatingwebhookconfiguration to reconcile, if istioctl is not used.")
)

// TODO - move these to a common file with other istiod constants and envvars
const (
	validationWebhookName = "istiod"
	webhookConfigPath     = "/var/lib/istio/validation/config.yaml"
)

func (s *Server) initConfigValidation(args *PilotArgs) error {
	params := validation.DefaultArgs()
	params = &validation.WebhookParameters{
		MixerValidator:                      mixervalidate.NewDefaultValidator(false),
		PilotDescriptor:                     schemas.Istio,
		DomainSuffix:                        args.Config.ControllerOptions.DomainSuffix,
		Port:                                9443, // TOOD - move to 15000 range
		CertFile:                            filepath.Join(DNSCertDir, "cert-chain.pem"),
		KeyFile:                             filepath.Join(DNSCertDir, "key.pem"),
		WebhookConfigFile:                   webhookConfigPath,
		CACertFile:                          defaultCACertPath,
		DeploymentAndServiceNamespace:       args.Namespace,
		WebhookName:                         validationWebhookName,
		DeploymentName:                      "istiod",
		ServiceName:                         "istiod",
		Clientset:                           s.kubeClient,
		EnableValidation:                    true, // always run the http server.
		EnableReconcileWebhookConfiguration: validationWebhookConfigName.Get() != "",
		Mux:                                 s.mux,
	}

	webhookServerReady := make(chan struct{}, 1)

	// TODO - plumb webhook readiness through to Pilot's readiness
	readinessOptions := probe.Options{
		Path:           "TODO",
		UpdateInterval: time.Second, // TODO
	}
	readiness := components.NewProbe(&readinessOptions)

	s.addStartFunc(func(stop <-chan struct{}) error {
		validation.RunValidation(webhookServerReady, stop, params, (kubernetes.Interface)(nil),
			args.Config.KubeConfig, nil, readiness.Controller())
		return nil
	})

	if validationWebhookConfigName.Get() != "" {
		client, err := kube.CreateClientset(args.Config.KubeConfig, "")
		if err != nil {
			return err
		}

		options := webhook.Options{
			WatchedNamespace: args.Namespace,
			ResyncPeriod:     time.Minute,
			CAPath:           defaultCACertPath,
			ConfigPath:       webhookConfigPath,
			ConfigName:       "istiod",
			ServiceName:      "istiod",
			Client:           client,
		}
		controller := webhook.New(options)

		s.addStartFunc(func(stop <-chan struct{}) error {
			go readiness.Start()
			go func() {
				<-stop
				readiness.Stop()
			}()

			go controller.Start(stop)
			return nil
		})
	}
	return nil
}
