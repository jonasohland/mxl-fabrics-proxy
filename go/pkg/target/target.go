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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/common"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/metrics"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
	"github.com/samber/lo"
)

type Targets struct {
	ctx     context.Context
	metrics *metrics.Metrics

	targets map[string]*Target
}

type TargetConfig struct {
	LocalDomainPath  string
	RemoteDomainPath string
	RemoteHost       string
	Provider         string
	Node             string
	FlowID           string
	NoNetLatMeasure  bool
	Labels           map[string]string
	SchedPrio        *int

	ID string
}

func NewTargets(ctx context.Context, metrics *metrics.Metrics) *Targets {
	return &Targets{ctx: ctx, metrics: metrics, targets: map[string]*Target{}}
}

func (t *Targets) Create(c *TargetConfig) (string, error) {
	slog.Info("create target", "config", c)

	id, err := common.NewCookie()
	if err != nil {
		return "", err
	}

	c.ID = id

	t.targets[id] = NewTarget(t.ctx, t.metrics, c)
	return id, nil
}

func (t *Targets) Destroy(id string) {
	target, ok := t.targets[id]
	if !ok {
		return
	}

	target.Stop()
}

type Target struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	client  *http.Client
	metrics *metrics.Metrics

	config *TargetConfig

	flowDefinition string

	activeSubscription string

	wctx      context.Context
	wcancel   context.CancelFunc
	wwg       sync.WaitGroup
	worker    *worker.ProxyWorker
	wrestarts uint64
}

func NewTarget(ctx context.Context, metrics *metrics.Metrics, config *TargetConfig) *Target {
	tctx, tcancel := context.WithCancel(ctx)

	t := &Target{
		cancel: tcancel,
		wg:     sync.WaitGroup{},

		client:  &http.Client{Timeout: 5 * time.Second},
		metrics: metrics,

		config: config,

		flowDefinition: "",

		wctx:      nil,
		wcancel:   nil,
		wwg:       sync.WaitGroup{},
		worker:    nil,
		wrestarts: 0,
	}

	t.wg.Add(1)
	go t.run(tctx, &t.wg)
	return t
}

func (t *Target) Stop() {
	if t.cancel != nil {
		t.cancel()
	}

	t.cancel = nil
	t.wg.Wait()
}

func (t *Target) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer t.cleanup()

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
		t.wrestarts = t.worker.NumRestarts()
	}

	if err := os.MkdirAll(t.config.LocalDomainPath, 0755); err != nil {
		slog.Warn("failed to create local domain path", "error", err, "path", t.config.LocalDomainPath)
	}

	wctx, wcancel := context.WithCancel(context.Background())
	t.wctx = wctx
	t.wcancel = wcancel
	t.worker = worker.NewWorker(
		worker.Config{
			Target:          true,
			ProxyID:         t.config.ID,
			Domain:          t.config.LocalDomainPath,
			Provider:        t.config.Provider,
			Node:            t.config.Node,
			FlowID:          t.config.FlowID,
			FlowDefinition:  t.flowDefinition,
			NoNetLatMeasure: t.config.NoNetLatMeasure,
			SchedPrio:       t.config.SchedPrio,

			Labels: t.config.Labels,
		}, t.wrestarts)

	t.worker.Start(t.wctx, &t.wwg)
	t.metrics.Add(t.worker)

	return nil
}

func (t *Target) getFlowDefinition(ctx context.Context) error {
	var domainInfo common.DomainInfo
	err := t.req(ctx, http.MethodGet,
		"/v1/flows"+t.config.RemoteDomainPath,    // path
		map[string]string{"id": t.config.FlowID}, // query
		nil, &domainInfo)                         // bodyIn, bodyOut
	if err != nil {
		return err
	}

	flow, ok := domainInfo.Flows[t.config.FlowID]
	if !ok {
		return fmt.Errorf("%w: flow not in domain info", server.ErrNotFound)
	}

	buf := bytes.NewBuffer(nil)
	if err := json.NewEncoder(buf).Encode(&flow.FlowDefinition); err != nil {
		return err
	}

	t.flowDefinition = buf.String()
	t.config.Labels = lo.Assign(t.config.Labels, flow.FlowDefinition.ToLabels())

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
		FlowURL:         fmt.Sprintf("mxl://%s?id=%s", t.config.RemoteDomainPath, t.config.FlowID),
		TargetInfo:      info,
		Provider:        t.config.Provider,
		Labels:          t.config.Labels,
		NoNetLatMeasure: t.config.NoNetLatMeasure,
		SchedPrio:       t.config.SchedPrio,
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
	urlString := fmt.Sprintf("http://%s%s", t.config.RemoteHost, path)
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

	slog.Debug(method, "url", rurl.String())
	req := &http.Request{
		Method: method,
		Header: http.Header{"content-type": []string{"application/json"}},
		URL:    rurl,
	}

	req = req.WithContext(ctx)
	req.Close = true

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
