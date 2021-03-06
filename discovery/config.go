// Copyright 2019 Samaritan Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package discovery

import (
	"sync"

	"github.com/samaritan-proxy/samaritan-api/go/api"
	"github.com/samaritan-proxy/samaritan-api/go/config/service"
	"google.golang.org/grpc/peer"

	"github.com/samaritan-proxy/sash/config"
	"github.com/samaritan-proxy/sash/logger"
)

//go:generate mockgen -package $GOPACKAGE -self_package github.com/samaritan-proxy/sash/$GOPACKAGE --destination ./mock_config_test.go github.com/samaritan-proxy/samaritan-api/go/api DiscoveryService_StreamSvcConfigsServer

type (
	configSubHandler   func(svcName string, session *configDiscoverySession)
	configUnsubHandler func(svcName string, session *configDiscoverySession)
)

type configDiscoverySession struct {
	stream api.DiscoveryService_StreamSvcConfigsServer
	remote *peer.Peer

	subscribed map[string]struct{} // subscribed services.
	subHdlr    configSubHandler
	unsubHdlr  configUnsubHandler
	eventCh    chan *config.ProxyConfigEvent

	quit chan struct{}
}

func newConfigDiscoverySession(stream api.DiscoveryService_StreamSvcConfigsServer) *configDiscoverySession {
	remote, _ := peer.FromContext(stream.Context())
	return &configDiscoverySession{
		stream:     stream,
		remote:     remote,
		subscribed: make(map[string]struct{}, 8),
		eventCh:    make(chan *config.ProxyConfigEvent, 16),
		quit:       make(chan struct{}),
	}
}

func (s *configDiscoverySession) SetSubscribeHandler(hdlr configSubHandler) {
	s.subHdlr = hdlr
}

func (s *configDiscoverySession) SetUnsubscribeHandler(hdlr configUnsubHandler) {
	s.unsubHdlr = hdlr
}

func (s *configDiscoverySession) Serve() {
	recvDone := make(chan struct{})
	defer func() {
		close(s.quit)
		// wait recv goroutine done
		<-recvDone
		// unsubscribe all the services.
		s.unsubscribeAll()
		logger.Debugf("Config discovery session %s exit", s.remote.Addr)
	}()

	go func() {
		defer close(recvDone)
		for {
			req, err := s.stream.Recv()
			if err != nil {
				logger.Warnf("Read from service config stream %s failed: %v", s.remote.Addr, err)
				return
			}

			s.handleSubscribe(req.SvcNamesSubscribe...)
			s.handleUnsubscribe(req.SvcNamesUnsubscribe...)
		}
	}()

	var event *config.ProxyConfigEvent
	for {
		select {
		case event = <-s.eventCh:
		case <-recvDone:
			return
		}

		var cfg *service.Config
		switch event.Type {
		case config.EventAdd, config.EventUpdate:
			cfg = event.ProxyConfig.Config
		default:
		}
		resp := &api.SvcConfigDiscoveryResponse{
			Updated: map[string]*service.Config{
				event.ProxyConfig.ServiceName: cfg,
			},
		}
		if err := s.stream.Send(resp); err != nil {
			logger.Warnf("Send to config stream %s failed: %v", s.remote.Addr, err)
			return
		}
	}
}

func (s *configDiscoverySession) handleSubscribe(svcNames ...string) {
	for _, svcName := range svcNames {
		_, ok := s.subscribed[svcName]
		if ok {
			continue
		}

		if s.subHdlr != nil {
			s.subHdlr(svcName, s)
		}
		s.subscribed[svcName] = struct{}{}
	}
}

func (s *configDiscoverySession) handleUnsubscribe(svcNames ...string) {
	for _, svcName := range svcNames {
		_, ok := s.subscribed[svcName]
		if !ok {
			continue
		}
		if s.unsubHdlr != nil {
			s.unsubHdlr(svcName, s)
		}
		delete(s.subscribed, svcName)
	}
}

func (s *configDiscoverySession) unsubscribeAll() {
	for svcName := range s.subscribed {
		s.handleUnsubscribe(svcName)
	}
}

func (s *configDiscoverySession) SendEvent(event *config.ProxyConfigEvent) {
	select {
	case s.eventCh <- event:
	case <-s.quit:
	}
}

type configDiscoverySessions map[*configDiscoverySession]interface{}

type configDiscoveryServer struct {
	sync.RWMutex
	cfgCtl *config.ProxyConfigsController

	subscribers map[string]configDiscoverySessions
}

func newConfigDiscoveryServer(ctl *config.Controller) *configDiscoveryServer {
	s := &configDiscoveryServer{
		cfgCtl:      ctl.ProxyConfigs(),
		subscribers: make(map[string]configDiscoverySessions),
	}
	s.cfgCtl.RegisterEventHandler(s.dispatchEvent)
	return s
}

func (s *configDiscoveryServer) dispatchEvent(evt *config.ProxyConfigEvent) {
	s.RLock()
	defer s.RUnlock()
	subscribers := s.subscribers[evt.ProxyConfig.ServiceName]
	for subscriber := range subscribers {
		subscriber.SendEvent(evt)
	}
}

func (s *configDiscoveryServer) handleSubscribe(svcName string, c *configDiscoverySession) {
	s.Lock()
	defer s.Unlock()

	subscribers, ok := s.subscribers[svcName]
	if !ok {
		subscribers = configDiscoverySessions{}
		s.subscribers[svcName] = subscribers
	}

	if _, ok := subscribers[c]; ok {
		return
	}

	subscribers[c] = struct{}{}

	// send config when first subscribe
	cfg, err := s.cfgCtl.GetCache(svcName)
	if err != nil {
		return
	}
	c.SendEvent(&config.ProxyConfigEvent{
		Type:        config.EventAdd,
		ProxyConfig: cfg,
	})
}

func (s *configDiscoveryServer) handleUnsubscribe(svcName string, c *configDiscoverySession) {
	s.Lock()
	defer s.Unlock()
	// Go runtime never shrink map after elements removal, refer to: https://github.com/golang/go/issues/20135
	// FIXME: To prevent OOM after long running, we should add some memchainsm recycle the memory.
	subscribers, ok := s.subscribers[svcName]
	if !ok {
		return
	}
	delete(subscribers, c)
}

func (s *configDiscoveryServer) Subscribers() map[string]configDiscoverySessions {
	return s.subscribers
}

func (s *configDiscoveryServer) StreamSvcConfigs(stream api.DiscoveryService_StreamSvcConfigsServer) error {
	session := newConfigDiscoverySession(stream)
	session.SetSubscribeHandler(s.handleSubscribe)
	session.SetUnsubscribeHandler(s.handleUnsubscribe)
	session.Serve()
	return nil
}
