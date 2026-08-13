package target

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/mxl"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/worker"
	"github.com/samber/lo"
)

type Targets struct {
	mu      sync.Mutex
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
	Tracing          bool

	ID string
}

type RequestError struct {
	Method  string
	URL     string
	Code    int
	Message string
}

func (r *RequestError) Error() string {
	return fmt.Sprintf("request failed: %s %s: (code %d): %s", r.Method, r.URL, r.Code, r.Message)
}

func NewTargets(ctx context.Context, metrics *metrics.Metrics) *Targets {
	return &Targets{mu: sync.Mutex{}, ctx: ctx, metrics: metrics, targets: map[string]*Target{}}
}

func (t *Targets) Create(c *TargetConfig) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	id, err := common.NewCookie()
	if err != nil {
		return "", err
	}

	c.ID = id

	t.targets[id] = NewTarget(t.ctx, t.metrics, c)
	return id, nil
}

func (t *Targets) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.targets)
}

func (t *Targets) Destroy(id string) {
	target := t.remove(id)
	if target != nil {
		target.Stop()
	}
}

func (t *Targets) remove(id string) *Target {
	t.mu.Lock()
	defer t.mu.Unlock()

	target, ok := t.targets[id]
	if !ok {
		return nil
	}

	delete(t.targets, id)
	return target
}

type Target struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	log    *slog.Logger

	client  *http.Client
	metrics *metrics.Metrics

	config *TargetConfig

	flowDefBuf string
	flowDef    *common.FlowDefinition

	activeSubscription string

	fst *mxl.StateTracker

	wctx      context.Context
	wcancel   context.CancelFunc
	wwg       sync.WaitGroup
	worker    *worker.ProxyWorker
	wrestarts uint64
}

func NewTarget(ctx context.Context, metrics *metrics.Metrics, config *TargetConfig) *Target {
	tctx, tcancel := context.WithCancel(ctx)

	logger := slog.With("module", "in")
	if config.Tracing {
		for k, v := range config.Labels {
			logger = logger.With(k, v)
		}
	}

	logger = logger.With("flow-id", config.FlowID,
		"local-domain", config.LocalDomainPath,
		"remote-domain", config.RemoteDomainPath,
		"remote-host", config.RemoteHost)

	t := &Target{
		cancel: tcancel,
		wg:     sync.WaitGroup{},
		log:    logger,

		client:  &http.Client{Timeout: 5 * time.Second},
		metrics: metrics,

		config: config,

		flowDefBuf: "",
		flowDef:    nil,

		fst: nil,

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

	t.log.Info("target created")

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
			reqErr, ok := errors.AsType[*RequestError](err)
			if ok && reqErr.Code == http.StatusNotFound {
				continue
			}

			t.log.Error("failed to get flow definition", "error", err)
			continue
		}

		_ = json.NewEncoder(os.Stdout).Encode(t.flowDef)

		if err := t.startWorker(); err != nil {
			t.log.Error("failed to create subscription", "error", err)
			continue
		}

		subscriptionID, err := t.createSubscription(ctx)
		if err != nil {
			slog.Error("failed to create subscription", "error", err)
			continue
		}

		t.activeSubscription = subscriptionID
		t.log.Info("subscription created", "subscription-id", subscriptionID)

		if err := t.runActive(ctx, subscriptionID); err != nil {
			t.log.Error("keep alive failed", "error", err)
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
		t.log.Warn("failed to create local domain path", "error", err, "path", t.config.LocalDomainPath)
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
			FlowDefinition:  t.flowDefBuf,
			NoNetLatMeasure: t.config.NoNetLatMeasure,
			SchedPrio:       t.config.SchedPrio,

			Labels: t.config.Labels,
		}, t.wrestarts, t.log)

	t.worker.Start(t.wctx, &t.wwg)
	t.metrics.Add(t.worker)

	return nil
}

func (t *Target) getRemoteFlowDef(ctx context.Context) (*common.FlowDefinition, error) {
	var domainInfo common.DomainInfo
	err := t.req(ctx, http.MethodGet,
		"/v1/flows"+t.config.RemoteDomainPath,    // path
		map[string]string{"id": t.config.FlowID}, // query
		nil, &domainInfo)                         // bodyIn, bodyOut
	if err != nil {
		return nil, err
	}

	flow, ok := domainInfo.Flows[t.config.FlowID]
	if !ok {
		return nil, fmt.Errorf("%w: flow not in domain info", server.ErrNotFound)
	}

	return &flow.FlowDefinition, nil
}

func (t *Target) getFlowDefinition(ctx context.Context) error {
	flowDef, err := t.getRemoteFlowDef(ctx)
	if err != nil {
		return err
	}

	buf := bytes.NewBuffer(nil)
	if err := json.NewEncoder(buf).Encode(flowDef); err != nil {
		return err
	}

	labels := flowDef.ToLabels()

	t.flowDefBuf = buf.String()
	t.flowDef = flowDef
	t.config.Labels = lo.Assign(t.config.Labels, labels)

	if t.config.Tracing {
		for k, v := range labels {
			t.log = t.log.With(k, v)
		}
	}

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

func (t *Target) runActive(ctx context.Context, subID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(9 * time.Second):
		}

		if err := t.keepAlive(ctx, subID); err != nil {
			return err
		}
	}
}

func (t *Target) keepAlive(ctx context.Context, subID string) error {
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

	currentDef, err := t.getRemoteFlowDef(ctx)
	if err != nil {
		t.log.Warn("failed to get remote flow definition")
	} else {
		if !currentDef.Equal(t.flowDef) {
			return errors.New("remote flow definition has changed")
		}
	}

	return nil
}

func (t *Target) cleanup() {
	if t.worker == nil {
		return
	}

	if t.activeSubscription != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()

		if err := t.req(ctx, http.MethodDelete, "/v1/subscriptions/"+t.activeSubscription, nil, nil, nil); err != nil {
			t.log.Warn("failed to delete active subscription", "error", err)
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

	t.log.Debug(method, "url", rurl.String())
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
	return &RequestError{Method: method, URL: rurl.String(), Code: msg.Code, Message: msg.Message}
}
