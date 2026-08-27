package client

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
	"strings"
	"sync"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// DefaultRequestTimeout bounds an ordinary request. Generous rather than tight: a slow store
// makes the server slow, and an agent that gives up early only converts a slow control plane
// into an unreachable one.
const DefaultRequestTimeout = 30 * time.Second

// Options configures a [Client].
type Options struct {
	// Servers is one or more server base URLs, e.g. "http://ctrl-0:2283".
	//
	// More than one is an HA deployment reached without a load balancer in front. The client is
	// **sticky**: it keeps using whichever server last answered and moves on only when one stops
	// answering at the transport level. That matters more than spreading load, because the
	// assignment poll carries a store revision cursor and a replica whose view is behind it must
	// refuse to answer (plan §4.5) — so hopping between replicas on every call would manufacture
	// exactly the case the refusal exists to catch.
	Servers []string

	// Token is the shared bearer token, empty for a deployment with authentication off (§13).
	Token string

	// HTTP is the transport. Nil builds one; note that its Timeout is deliberately left unset,
	// because the assignment long poll is held open on purpose and a client-wide timeout would
	// cut it off mid-wait. Per-call deadlines come from the context instead.
	HTTP *http.Client

	// RequestTimeout bounds an ordinary request. [Client.Assignments] computes its own deadline
	// from the wait it asked for.
	RequestTimeout time.Duration

	Logger *slog.Logger
}

// Client talks to the agent API of one logical control plane.
//
// Safe for concurrent use: the agent's heartbeat, report and poll loops all share one.
type Client struct {
	servers []string
	token   string
	http    *http.Client
	timeout time.Duration
	log     *slog.Logger

	// mu guards cur only. It is a hint, not state: the worst a stale read can do is send one
	// request to a server that has just failed for someone else.
	mu  sync.Mutex
	cur int
}

// New validates the options and builds a client.
func New(opts Options) (*Client, error) {
	if len(opts.Servers) == 0 {
		return nil, errors.New("client: no server URL")
	}

	servers := make([]string, 0, len(opts.Servers))
	for _, raw := range opts.Servers {
		parsed, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil {
			return nil, fmt.Errorf("client: server url %q: %w", raw, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("client: server url %q: scheme must be http or https", raw)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("client: server url %q: no host", raw)
		}
		servers = append(servers, parsed.String())
	}

	c := &Client{
		servers: servers,
		token:   opts.Token,
		http:    opts.HTTP,
		timeout: opts.RequestTimeout,
		log:     opts.Logger,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.timeout <= 0 {
		c.timeout = DefaultRequestTimeout
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// Servers returns the configured base URLs, in order.
func (c *Client) Servers() []string { return c.servers }

// APIError is a response the server answered with deliberately: a status and a machine-readable
// [api.Error] body.
//
// Distinguished from a transport failure on purpose. A transport failure is worth trying the
// next replica for; a typed answer is this control plane's considered position and every replica
// would give the same one, since they share a store.
type APIError struct {
	// Status is the HTTP status code.
	Status int

	// Server is the base URL that answered, so a fleet-wide problem and one bad replica read
	// differently in the logs.
	Server string

	Body api.Error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %d %s", e.Server, e.Status, e.Body.Error())
}

// Code returns the error code the server sent, or "" for anything that is not an [APIError] —
// a transport failure, a cancelled context, a body that would not decode.
func Code(err error) api.ErrorCode {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Body.Code
	}
	return ""
}

// Detail returns one of the error's details, or "".
func Detail(err error, key string) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Body.Details[key]
	}
	return ""
}

// IsNotReady reports the server declining to answer because it has not settled, has no observed
// state to reconcile against, or holds a view behind the caller's revision cursor (§7.3, plan
// §4.2).
//
// The caller must treat this **exactly** like a transport failure: change nothing, retry later.
// It is emphatically not an empty answer, and the whole not-ready discipline exists so that a
// store restored from an empty backup cannot present as "every session was cancelled".
func IsNotReady(err error) bool { return Code(err) == api.CodeNotReady }

// IsReregister reports that this node's liveness lease is gone — expired during a partition,
// revoked, or lost with the store's contents.
//
// Not a teardown signal. The agent registers again and keeps its workers running while it does:
// losing a lease says the fleet has forgotten this node, not that its media should stop.
func IsReregister(err error) bool { return Code(err) == api.CodeReregister }

// IsNodeClaimed reports that another agent instance holds this node name (§7.1). The holder's
// instance UUID is in the "holder" detail.
func IsNodeClaimed(err error) bool { return Code(err) == api.CodeNodeClaimed }

// IsVersionSkew reports a server refusing an agent whose protocol version it cannot serve
// (§13.1).
func IsVersionSkew(err error) bool { return Code(err) == api.CodeVersionSkew }

// request is one call, described so that [Client.do] can retry it against another server.
type request struct {
	method string
	path   string
	query  url.Values

	// body is encoded once per attempt; every caller passes a value, never a reader, precisely
	// so that a retry against the next server has something to send.
	body any

	// out receives the decoded 2xx body. Nil discards it.
	out any

	// timeout bounds one attempt. Zero uses the client's default.
	timeout time.Duration
}

// do runs a request, moving to the next server on a transport failure.
//
// It fails over on **transport** failures only: a connection refused, a TLS failure, a read that
// dies mid-body. A typed [APIError] is returned as-is, because it is an answer rather than a
// failure to answer, and every replica shares one store and would give the same one. The
// exception that proves the rule is [api.CodeNotReady] from a replica whose view lags: it is
// genuinely replica-local, and it resolves on the next poll, which is cheaper than teaching this
// layer to distinguish the two.
func (c *Client) do(ctx context.Context, req request) error {
	timeout := req.timeout
	if timeout <= 0 {
		timeout = c.timeout
	}

	var encoded []byte
	if req.body != nil {
		var err error
		if encoded, err = json.Marshal(req.body); err != nil {
			return fmt.Errorf("client: encode %s %s: %w", req.method, req.path, err)
		}
	}

	var errs []error
	for attempt := range c.servers {
		server := c.server(attempt)

		err := c.attempt(ctx, server, req, encoded, timeout)
		if err == nil {
			c.prefer(server)
			return nil
		}

		var apiErr *APIError
		if errors.As(err, &apiErr) || ctx.Err() != nil {
			// An answer, or the caller went away. Neither is a reason to ask somebody else.
			c.prefer(server)
			return err
		}

		errs = append(errs, err)
		if len(c.servers) > 1 {
			c.log.Debug("server did not answer, trying the next one",
				"server", server, "method", req.method, "path", req.path, "error", err)
		}
	}

	return errors.Join(errs...)
}

func (c *Client) attempt(ctx context.Context, server string, req request, body []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := server + req.path
	if len(req.query) > 0 {
		endpoint += "?" + req.query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("client: build %s %s: %w", req.method, endpoint, err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", req.method, endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(server, resp)
	}

	if req.out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(req.out); err != nil {
		// A 200 whose body will not decode is a transport-class failure, not an answer: the
		// server meant to say something and this client did not receive it.
		return fmt.Errorf("client: %s %s: decode response: %w", req.method, endpoint, err)
	}
	return nil
}

const (
	// maxResponseBody bounds a decoded body. Fleet-wide inventory is the largest thing either
	// API returns and is nowhere near this; the limit is here so a wrong endpoint or a captive
	// portal cannot make an agent allocate without bound.
	maxResponseBody = 32 << 20

	// maxErrorBody bounds how much of a non-2xx body is read.
	maxErrorBody = 1 << 20
)

// decodeError turns a non-2xx response into an [APIError], inventing a body when the server did
// not send a usable one — so that a proxy's HTML error page still arrives as a typed error with
// the status intact rather than as a decode failure that hides it.
func decodeError(server string, resp *http.Response) error {
	body := api.Error{}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Code == "" {
		body = api.Error{
			Code:    statusCode(resp.StatusCode),
			Message: strings.TrimSpace(http.StatusText(resp.StatusCode) + " " + summarise(raw)),
		}
	}
	return &APIError{Status: resp.StatusCode, Server: server, Body: body}
}

// statusCode maps a status onto a code, for the case where the responder was not this project.
func statusCode(status int) api.ErrorCode {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return api.CodeUnauthorized
	case http.StatusNotFound:
		return api.CodeNotFound
	case http.StatusServiceUnavailable:
		// A proxy's 503 while the server is restarting reaches an agent as "not ready", which is
		// the conservative reading: change nothing, poll again. Reading it as anything else would
		// let an intermediary's outage look like an answer.
		return api.CodeNotReady
	default:
		return api.CodeInternal
	}
}

func summarise(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return strings.Join(strings.Fields(text), " ")
}

func (c *Client) server(offset int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.servers[(c.cur+offset)%len(c.servers)]
}

// prefer makes server the first one tried next time.
func (c *Client) prefer(server string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, candidate := range c.servers {
		if candidate == server {
			c.cur = i
			return
		}
	}
}
