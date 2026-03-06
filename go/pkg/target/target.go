package target

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/common"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/metrics"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
)

type Config struct {
	Node     string
	Service  string
	Provider string
}

type Targets struct {
	ctx     context.Context
	wg      *sync.WaitGroup
	metrics *metrics.Metrics

	config Config

	targets []*Target
}

func NewTargets(ctx context.Context, wg *sync.WaitGroup, metrics *metrics.Metrics, config Config) *Targets {
	return &Targets{ctx: ctx, wg: wg, metrics: metrics, config: config, targets: nil}
}

func (t *Targets) Create(localDomain string, remoteFlows string) error {
	localDomain, err := common.ParseDomainURL(localDomain)
	if err != nil {
		return err
	}

	remoteFlowsURL, err := url.Parse(remoteFlows)
	if err != nil {
		return fmt.Errorf("%w: invalid remote flow url: %w", server.ErrInvalidArgument, err)
	}

	if remoteFlowsURL.Port() == "" {
		remoteFlowsURL.Host += ":2283"
	}

	for _, id := range remoteFlowsURL.Query()["id"] {
		if err := t.create(localDomain, remoteFlowsURL.Host, remoteFlowsURL.Path, id); err != nil {
			return err
		}
	}

	return nil
}

func (t *Targets) create(localDomain string, remoteAuthority string, remoteDomain string, id string) error {
	t.targets = append(t.targets,
		NewTarget(t.ctx, t.wg, t.metrics, t.config, localDomain, remoteAuthority, remoteDomain, id))
	return nil
}

type Target struct {
	client  *http.Client
	metrics *metrics.Metrics

	config       Config
	localDomain  string
	remoteDomain string
	authority    string
	flowID       string

	flowDefinition string
	proxyID        string

	activeSubscription string

	wctx    context.Context
	wcancel context.CancelFunc
	wwg     sync.WaitGroup
	worker  *worker.ProxyWorker
}

func NewTarget(ctx context.Context, wg *sync.WaitGroup, metrics *metrics.Metrics, config Config,
	localDomain, authority, remoteDomain, id string) *Target {
	t := &Target{
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		metrics: metrics,

		config:       config,
		localDomain:  localDomain,
		remoteDomain: remoteDomain,
		authority:    authority,
		flowID:       id,

		flowDefinition: "",
		proxyID:        common.NewCookieMust(),

		wctx:    nil,
		wcancel: nil,
		wwg:     sync.WaitGroup{},
		worker:  nil,
	}

	wg.Add(1)
	go t.run(ctx, wg)
	return t
}

func (t *Target) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer t.cleanup()
	slog.Info("running target", "local-domain", t.localDomain, "authority", t.authority, "remote-domain", t.remoteDomain, "remote-flow-id", t.flowID)

	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = time.Second * 3

		t.cleanup()

		if err := t.getFlowDefinition(ctx); err != nil {
			slog.Error("failed to get flow definition", "error", err)
			continue
		}

		if err := t.startWorker(); err != nil {
			slog.Error("failed to create subscription", "error", err)
			continue
		}

		subscriptionID, err := t.createSubscription(ctx)
		if err != nil {
			slog.Error("failed to create subscription", "error", err)
			continue
		}

		t.activeSubscription = subscriptionID

		slog.Info("subscription created", "subscription-id", subscriptionID)

		if err := t.keepAlive(ctx, subscriptionID); err != nil {
			slog.Error("keep alive failed", "error", err)
		}
	}
}

func (t *Target) startWorker() error {
	if t.worker != nil {
		t.wcancel()
		t.wwg.Wait()
	}

	wctx, wcancel := context.WithCancel(context.Background())
	t.wctx = wctx
	t.wcancel = wcancel
	t.worker = worker.NewWorker(
		worker.Config{
			Target:         true,
			ProxyID:        t.proxyID,
			Node:           t.config.Node,
			Service:        t.config.Service,
			Provider:       t.config.Provider,
			Domain:         t.localDomain,
			FlowDefinition: t.flowDefinition,
			FlowID:         t.flowID,
		})

	t.worker.Start(t.wctx, &t.wwg)
	t.metrics.Add(t.worker)

	return nil
}

func (t *Target) getFlowDefinition(ctx context.Context) error {
	var domainInfo common.DomainInfo
	if err := t.req(ctx, http.MethodGet, "/v1/flows"+t.remoteDomain, map[string]string{"id": t.flowID}, nil, &domainInfo); err != nil {
		return err
	}

	flow, ok := domainInfo.Flows[t.flowID]
	if !ok {
		return fmt.Errorf("%w: flow not in domain info", server.ErrNotFound)
	}

	buf := bytes.NewBuffer(nil)
	if err := json.NewEncoder(buf).Encode(&flow.FlowDefinition); err != nil {
		return err
	}

	t.flowDefinition = buf.String()
	return nil
}

func (t *Target) getTargetInfo(ctx context.Context) (string, error) {
	wait := time.Millisecond * 200
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}

		wait = time.Second * 2

		targetInfo, err := t.worker.GetTargetInfo()
		if err != nil {
			slog.Error("could not get target info", "error", err)
			continue
		}

		return targetInfo, nil
	}
}

func (t *Target) createSubscription(ctx context.Context) (string, error) {
	info, err := t.getTargetInfo(ctx)
	if err != nil {
		return "", err
	}

	req := common.SubscriptionRequest{
		FlowURL:    fmt.Sprintf("mxl://%s?id=%s", t.remoteDomain, t.flowID),
		TargetInfo: info,
		Provider:   t.config.Provider,
	}

	var res common.SubscriptionResponse
	if err := t.req(ctx, http.MethodPost, "/v1/subscriptions", nil, &req, &res); err != nil {
		return "", err
	}

	return res.ID, nil
}

func (t *Target) keepAlive(ctx context.Context, subID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}

		info, err := t.getTargetInfo(ctx)
		if err != nil {
			return err
		}

		req := &common.SubscriptionRequest{
			TargetInfo: info,
		}

		if err := t.req(ctx, http.MethodPatch, "/v1/subscriptions/"+subID, nil, req, nil); err != nil {
			return err
		}
	}
}

func (t *Target) cleanup() {
	if t.worker == nil {
		return
	}

	if t.activeSubscription != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()

		if err := t.req(ctx, http.MethodDelete, "/v1/subscriptions/"+t.activeSubscription, nil, nil, nil); err != nil {
			slog.Error("failed to delete active subscription", "error", err)
		}
	}

	t.wcancel()
	t.metrics.Remove(t.worker)
	t.wwg.Wait()
}

func (t *Target) req(ctx context.Context, method, path string, query map[string]string, in any, out any) error {
	urlString := fmt.Sprintf("http://%s%s", t.authority, path)
	if len(query) > 0 {
		var qp []string
		for k, v := range query {
			qp = append(qp, k+"="+v)
		}
		urlString += "?" + strings.Join(qp, "&")
	}

	rurl, err := url.Parse(urlString)
	if err != nil {
		return err
	}

	req := &http.Request{
		Method: method,
		Header: http.Header{"content-type": []string{"application/json"}},
		URL:    rurl,
	}

	req = req.WithContext(ctx)

	if in != nil {
		buf := bytes.NewBuffer(nil)
		if err := json.NewEncoder(buf).Encode(in); err != nil {
			return err
		}

		req.Body = io.NopCloser(buf)
	}

	response, err := t.client.Do(req)
	if err != nil {
		return err
	}

	if response.StatusCode == http.StatusOK {
		if out == nil {
			return nil
		}

		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			return err
		}

		return nil
	}

	var msg server.Message
	_ = json.NewDecoder(response.Body).Decode(&msg)
	return fmt.Errorf("request failed: (code: %d): %s", response.StatusCode, msg.Message)
}
