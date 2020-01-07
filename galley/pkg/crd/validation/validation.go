// Copyright 2018 Istio Authors
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

package validation

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/hashicorp/go-multierror"

	"istio.io/istio/mixer/pkg/validate"
	"istio.io/istio/pkg/config/schemas"
	"istio.io/istio/pkg/webhook/server"
	"istio.io/pkg/log"
	"istio.io/pkg/probe"
)

var scope = log.RegisterScope("validation", "CRD validation debugging", 0)

const (
	dns1123LabelMaxLength int    = 63
	dns1123LabelFmt       string = "[a-zA-Z0-9]([-a-z-A-Z0-9]*[a-zA-Z0-9])?"

	httpsHandlerReadinessFreq = time.Second
)

var dns1123LabelRegexp = regexp.MustCompile("^" + dns1123LabelFmt + "$")

// This is for lint fix
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

func webhookHTTPSHandlerReady(client httpClient, vc *server.Options) error {
	readinessURL := &url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("localhost:%v", vc.Port),
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

//RunValidation start running Galley validation mode
func RunValidation(stopCh chan struct{}, vc server.Options,
	livenessProbeController, readinessProbeController probe.Controller) {
	log.Infof("Galley validation started with \n%s", vc)
	mixerValidator := validate.NewDefaultValidator(false)

	vc.MixerValidator = mixerValidator
	vc.PilotDescriptor = schemas.Istio
	wh, err := server.New(vc)
	if err != nil {
		log.Fatalf("cannot create validation webhook service: %v", err)
	}
	validationLivenessProbe := probe.NewProbe()
	if vc.Mux == nil && livenessProbeController != nil {
		validationLivenessProbe.SetAvailable(nil)
		validationLivenessProbe.RegisterProbe(livenessProbeController, "validationLiveness")
	}

	validationReadinessProbe := probe.NewProbe()
	if vc.Mux == nil && readinessProbeController != nil {
		validationReadinessProbe.SetAvailable(errors.New("init"))
		validationReadinessProbe.RegisterProbe(readinessProbeController, "validationReadiness")

		go func() {
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
				if err := webhookHTTPSHandlerReady(client, vc); err != nil {
					validationReadinessProbe.SetAvailable(errors.New("not ready"))
					scope.Infof("https handler for validation webhook is not ready: %v\n", err)
					ready = false
				} else {
					validationReadinessProbe.SetAvailable(nil)
					if !ready {
						scope.Info("https handler for validation webhook is ready\n")
						ready = true
					}
				}
				<-time.After(httpsHandlerReadinessFreq)
				// check again
			}
		}()
	}

	go func() {
		<-stopCh
		if livenessProbeController != nil {
			validationLivenessProbe.SetAvailable(errors.New("stopped"))
		}
		if readinessProbeController != nil {
			validationReadinessProbe.SetAvailable(errors.New("stopped"))
		}
	}()
	go wh.Run(stopCh)
}

// isDNS1123Label tests for a string that conforms to the definition of a label in
// DNS (RFC 1123).
func isDNS1123Label(value string) bool {
	return len(value) <= dns1123LabelMaxLength && dns1123LabelRegexp.MatchString(value)
}

// validatePort checks that the network port is in range
func validatePort(port int) error {
	if 1 <= port && port <= 65535 {
		return nil
	}
	return fmt.Errorf("port number %d must be in the range 1..65535", port)
}

// Validate tests if the WebhookParameters has valid params.
func Validate(p server.Options) error {
	var errs *multierror.Error
	if p.Enabled {
		if len(p.CertFile) == 0 {
			errs = multierror.Append(errs, errors.New("cert file not specified"))
		}
		if len(p.KeyFile) == 0 {
			errs = multierror.Append(errs, errors.New("key file not specified"))
		}
		if err := validatePort(int(p.Port)); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	return errs.ErrorOrNil()
}
