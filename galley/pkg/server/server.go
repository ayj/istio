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

package server

import (
	"net"

	"istio.io/pkg/ctrlz/fw"

	"istio.io/istio/galley/pkg/server/components"
	"istio.io/istio/galley/pkg/server/process"
	"istio.io/istio/galley/pkg/server/settings"
)

// Server is the main entry point into the Galley code.
type Server struct {
	host process.Host

	p *components.Processing
}

// New returns a new instance of a Server.
func New(a *settings.Args) *Server {
	s := &Server{}

	var topics []fw.Topic

	liveness := components.NewProbe(&a.Liveness)
	s.host.Add(liveness)

	readiness := components.NewProbe(&a.Readiness)
	s.host.Add(readiness)

	if a.ValidationWebhookServerArgs.Enabled {
		live, ready := liveness.Controller(), readiness.Controller()
		server := components.NewValidationServer(a.ValidationWebhookServerArgs, live, ready)
		s.host.Add(server)
	}
	if a.ValidationWebhookControllerArgs.Enabled ||
		(a.ValidationWebhookServerArgs.Enabled && a.ValidationWebhookControllerArgs.UnregisterValidationWebhook) {
		controller := components.NewValidationController(a.ValidationWebhookControllerArgs)
		s.host.Add(controller)
	}

	if a.EnableServer {
		s.p = components.NewProcessing(a)
		s.host.Add(s.p)
		t := s.p.ConfigZTopic()
		topics = append(topics, t)
	}

	mon := components.NewMonitoring(a.MonitoringPort)
	s.host.Add(mon)

	if a.EnableProfiling {
		prof := components.NewProfiling(a.PprofPort)
		s.host.Add(prof)
	}

	clz := components.NewCtrlz(a.IntrospectionOptions, topics...)
	s.host.Add(clz)

	return s
}

// Address returns the address of the config processing server.
func (s *Server) Address() net.Addr {
	return s.p.Address()

}

// Start the process.
func (s *Server) Start() error {
	return s.host.Start()
}

// Stop cleans up resources used by the server.
func (s *Server) Stop() {
	s.host.Stop()
}
