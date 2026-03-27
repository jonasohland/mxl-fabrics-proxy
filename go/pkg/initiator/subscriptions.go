package initiator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/common"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/metrics"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
)

type Subscription struct {
	mu sync.Mutex

	wwg     *sync.WaitGroup
	wctx    context.Context
	wcancel context.CancelFunc
	worker  *worker.ProxyWorker

	config        worker.Config
	metrics       *metrics.Metrics
	lastKeepAlive time.Time
}

func NewSubscription(config worker.Config, metrics *metrics.Metrics) *Subscription {
	ctx, cancel := context.WithCancel(context.Background())
	worker := worker.NewWorker(config, 0)

	sub := &Subscription{
		mu: sync.Mutex{},

		wwg:     &sync.WaitGroup{},
		wctx:    ctx,
		wcancel: cancel,
		worker:  worker,

		config:        config,
		metrics:       metrics,
		lastKeepAlive: time.Now(),
	}

	sub.worker.Start(sub.wctx, sub.wwg)
	sub.metrics.Add(sub.worker)
	return sub
}

func (s *Subscription) Terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.wcancel != nil {
		s.wcancel()
	}

	s.wwg.Wait()
	s.wcancel = nil
	s.wctx = nil
	s.metrics.Remove(s.worker)
}

func (s *Subscription) HasTargetInfoChanged(t string) bool {
	return s.config.TargetInfo != t
}

type Subscriptions struct {
	mu            sync.Mutex
	subscriptions map[string]*Subscription
	domains       *Domains
	metrics       *metrics.Metrics
}

func NewSubscriptions(ctx context.Context, wg *sync.WaitGroup, domains *Domains, metrics *metrics.Metrics) *Subscriptions {
	s := &Subscriptions{
		mu:            sync.Mutex{},
		subscriptions: make(map[string]*Subscription),
		domains:       domains,
		metrics:       metrics,
	}

	wg.Add(1)
	go s.run(ctx, wg)

	return s
}

func (s *Subscriptions) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer s.shutdown()

	for {
		select {
		case <-ctx.Done():
			return

		case <-time.After(5 * time.Second):
			s.cleanup()
		}
	}
}

func (s *Subscriptions) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, sub := range s.subscriptions {
		if time.Since(sub.lastKeepAlive) > 20*time.Second {
			slog.Warn("terminating inactive subscription",
				"id", id, "flow-id", sub.config.FlowID, "domain", sub.config.Domain)

			s.removeSubscription(id)
		}
	}
}
func (s *Subscriptions) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id := range s.subscriptions {
		s.removeSubscription(id)
	}
}

func (s *Subscriptions) Mux(mux *http.ServeMux) {
	mux.Handle("POST /v1/subscriptions", s)
	mux.Handle("PATCH /v1/subscriptions/{id}", s)
	mux.Handle("DELETE /v1/subscriptions/{id}", s)
}

func (s *Subscriptions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parseRequest := func() (*common.SubscriptionRequest, error) {
		var req common.SubscriptionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, fmt.Errorf("%w: %w", server.ErrInvalidArgument, err)
		}

		return &req, nil
	}

	switch r.Method {
	case "POST":
		req, err := parseRequest()
		if err != nil {
			server.Error(w, err)
			return
		}
		id, err := s.Create(req)
		if err != nil {
			server.Error(w, err)
			return
		}

		server.Response(w, &common.SubscriptionResponse{ID: id})
		return
	case "PATCH":
		req, err := parseRequest()
		if err != nil {
			server.Error(w, err)
			return
		}

		if err := s.KeepAlive(r.PathValue("id"), req); err != nil {
			server.Error(w, err)
			return
		}

		server.Response(w, &server.Message{Code: 200, Message: "ok"})
		return
	case "DELETE":
		if err := s.Remove(r.PathValue("id")); err != nil {
			server.Error(w, err)
			return
		}

		server.Response(w, &server.Message{Code: 200, Message: "ok"})
		return
	}

	server.Error(w, server.ErrNotFound)
}

func (s *Subscriptions) Create(req *common.SubscriptionRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.createSuscription(req)
}

func (s *Subscriptions) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.subscriptions[id]
	if !ok {
		return server.ErrNotFound
	}

	slog.Info("delete subscription", "id", id)
	s.removeSubscription(id)
	return nil
}

func (s *Subscriptions) KeepAlive(id string, req *common.SubscriptionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sub, ok := s.subscriptions[id]
	if !ok {
		return server.ErrNotFound
	}

	if sub.HasTargetInfoChanged(req.TargetInfo) {
		slog.Warn("target info has changed, terminating subscription", "id", id)
		s.removeSubscription(id)
		return fmt.Errorf("%w: target info has changed", server.ErrInvalidArgument)
	}

	sub.lastKeepAlive = time.Now()
	return nil
}

func (s *Subscriptions) createSuscription(req *common.SubscriptionRequest) (string, error) {
	domain, id, err := common.ParseFlowURLSingle(req.FlowURL)
	if err != nil {
		return "", err
	}

	info, err := s.domains.Find(domain, id)
	if err != nil {
		return "", err
	}

	cookie, err := common.NewCookie()
	if err != nil {
		return "", err
	}

	config := worker.Config{
		Target:          false,
		ProxyID:         cookie,
		Node:            info.Node,
		Service:         "",
		Provider:        req.Provider,
		Domain:          info.Path,
		TargetInfo:      req.TargetInfo,
		FlowDefinition:  "",
		FlowID:          id,
		EFAUseWait:      true,
		NoNetLatMeasure: req.NoNetLatMeasure,
		SchedPrio:       req.SchedPrio,

		Labels: req.Labels,
	}

	s.subscriptions[cookie] = NewSubscription(config, s.metrics)
	slog.Info("subscription created", "id", cookie, "domain", domain, "flow-id", id, "node", info.Node)
	return cookie, nil
}

func (s *Subscriptions) removeSubscription(id string) {
	sub, ok := s.subscriptions[id]
	if !ok {
		return
	}

	sub.Terminate()
	delete(s.subscriptions, id)
}
