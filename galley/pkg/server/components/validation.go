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

package components

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"istio.io/istio/galley/pkg/server/process"
	"istio.io/istio/mixer/pkg/validate"
	"istio.io/istio/pkg/cmd"
	"istio.io/istio/pkg/config/schemas"
	"istio.io/istio/pkg/webhook/controller"
	"istio.io/istio/pkg/webhook/server"

	"istio.io/pkg/log"
	"istio.io/pkg/probe"
)

// This is for lint fix
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

func monitorReadiness(stop <-chan struct{}, port uint, readiness *probe.Probe) {
	const httpsHandlerReadinessFreq = time.Second

	ready := false
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	for {
		if err := webhookHTTPSHandlerReady(client, port); err != nil {
			readiness.SetAvailable(errors.New("not ready"))
			scope.Infof("https handler for validation webhook is not ready: %v\n", err)
			ready = false
		} else {
			readiness.SetAvailable(nil)
			if !ready {
				scope.Info("https handler for validation webhook is ready\n")
				ready = true
			}
		}

		select {
		case <-stop:
			return
		case <-time.After(httpsHandlerReadinessFreq):
			// check again
		}
	}
}

func webhookHTTPSHandlerReady(client httpClient, port uint) error {
	readinessURL := &url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("localhost:%v", port),
		Path:   server.HTTPSHandlerReadyPath,
	}

	req := &http.Request{
		Method: http.MethodGet,
		URL:    readinessURL,
	}

	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request to %v failed: %v", readinessURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %v returned non-200 status=%v",
			readinessURL, response.StatusCode)
	}
	return nil
}

// RunValidation start running Galley validation mode
func RunValidation(
	stopCh <-chan struct{},
	vc server.Options,
	livenessProbeController,
	readinessProbeController probe.Controller,
) {
	log.Infof("Galley validation started with \n%s", vc)

	vc.MixerValidator = validate.NewDefaultValidator(false)
	vc.PilotDescriptor = schemas.Istio
	wh, err := server.New(vc)
	if err != nil {
		log.Fatalf("cannot create validation webhook service: %v", err)
	}

	validationLivenessProbe := probe.NewProbe()
	if vc.Mux == nil && livenessProbeController != nil {
		validationLivenessProbe.SetAvailable(nil)
		validationLivenessProbe.RegisterProbe(livenessProbeController, "validationLiveness")

		go func() {
			<-stopCh
			validationLivenessProbe.SetAvailable(errors.New("stopped"))
		}()
	}

	validationReadinessProbe := probe.NewProbe()
	if vc.Mux == nil && readinessProbeController != nil {
		validationReadinessProbe.SetAvailable(errors.New("init"))
		validationReadinessProbe.RegisterProbe(readinessProbeController, "validationReadiness")

		go func() {
			monitorReadiness(stopCh, vc.Port, validationReadinessProbe)

			validationReadinessProbe.SetAvailable(errors.New("stopped"))
		}()
	}

	wh.Run(stopCh)
}

func NewValidationServer(
	params server.Options,
	liveness probe.Controller,
	readiness probe.Controller,
) process.Component {
	return process.ComponentFromFns(
		// start
		func() error {
			stop := make(chan struct{})
			go RunValidation(stop, params, liveness, readiness)
			go cmd.WaitSignal(stop)
			return nil
		},
		// stop
		func() {
			// server doesn't have a stop function.
		})
}

func NewValidationController(options controller.Options) process.Component {
	return process.ComponentFromFns(
		// start
		func() error {
			controller, err := controller.New(options)
			if err != nil {
				return err
			}
			stop := make(chan struct{})
			go controller.Start(stop)
			go cmd.WaitSignal(stop)
			return nil
		},
		// stop
		func() {
			// controller doesn't have a stop function
		})
}
