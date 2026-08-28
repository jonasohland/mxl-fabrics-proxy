package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// The user API (§9.1). Separate from the agent API in this package for the same reason the two
// have separate path prefixes on the server: different callers, different privileges, different
// compatibility guarantees. Of the two the *agent* API is the privileged one (§13).

// Applied is one request as an apply left it, or would leave it under a dry run.
type Applied struct {
	Request api.Request

	// Outcome is one of [api.OutcomeCreated], [api.OutcomeUpdated] or [api.OutcomeUnchanged].
	//
	// It comes from the server rather than being inferred here, and it has to: the response body
	// echoes the spec that was sent, so comparing them says only that the server agreed, and the
	// status code cannot separate "updated" from "unchanged" because both are 200.
	Outcome string
}

// Apply performs POST /v1/requests: create or update, keyed on the spec's name.
//
// dryRun asks the server to validate and reconcile without writing (§9.1). Everything else is
// identical, including which status code comes back, so a dry run reports the same outcome the
// real apply will.
func (c *Client) Apply(ctx context.Context, spec api.RequestSpec, dryRun bool) (*Applied, error) {
	query := url.Values{}
	if dryRun {
		query.Set(api.QueryDryRun, "true")
	}

	var out api.Request
	var header http.Header
	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   api.PathRequests,
		query:  query,
		body:   spec,
		out:    &out,
		header: &header,
	}); err != nil {
		return nil, err
	}

	// A server that did not send the header is older than this client, which §13.1 says should not
	// happen — the server is always upgraded first. Reported as "updated" rather than guessed at:
	// it is the honest answer when the only thing known is that the request now exists as asked.
	outcome := header.Get(api.HeaderOutcome)
	if outcome == "" {
		outcome = api.OutcomeUpdated
	}
	return &Applied{Request: out, Outcome: outcome}, nil
}

// DeleteRequest performs DELETE /v1/requests/{name}: cancel the intent.
//
// The name is the ID (§9.1), which is what lets `delete -f` work off the same file that created
// the requests without the file still having to be accurate about anything else.
//
// A request that is not there is not an error to this method — it returns found=false — because
// deleting what a manifest names is idempotent by nature: the second run of a delete should
// succeed, not fail because the first one worked.
func (c *Client) DeleteRequest(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, request{
		method: http.MethodDelete,
		path:   api.RequestPath(name),
	})
	if Code(err) == api.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Requests performs GET /v1/requests.
func (c *Client) Requests(ctx context.Context) ([]api.Request, error) {
	var out api.RequestList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathRequests, out: &out}); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Request performs GET /v1/requests/{name}.
func (c *Client) Request(ctx context.Context, name string) (*api.Request, error) {
	var out api.Request
	if err := c.do(ctx, request{method: http.MethodGet, path: api.RequestPath(name), out: &out}); err != nil {
		return nil, err
	}
	return &out, nil
}

// Paths performs GET /v1/paths: derived state, which is what an operator actually looks at.
func (c *Client) Paths(ctx context.Context) (*api.PathsResponse, error) {
	var out api.PathsResponse
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathPaths, out: &out}); err != nil {
		return nil, err
	}
	return &out, nil
}

// FlowFilter narrows GET /v1/flows. Every field is optional and they are ANDed.
//
// The same flow ID exists in more than one place by design — that is what replication *is* — so
// filtering on Flow alone returns every location it has reached, which is the useful answer for
// "where is this flow" rather than an ambiguity to resolve.
type FlowFilter struct {
	Node      string
	Domain    string
	Flow      string
	GroupHint string
	Type      string
}

func (f FlowFilter) query() url.Values {
	query := url.Values{}
	for key, value := range map[string]string{
		"node": f.Node, "domain": f.Domain, "flow": f.Flow,
		"group_hint": f.GroupHint, "type": f.Type,
	} {
		if value != "" {
			query.Set(key, value)
		}
	}
	return query
}

// Flows performs GET /v1/flows: the fleet-wide inventory, filterable.
func (c *Client) Flows(ctx context.Context, filter FlowFilter) ([]api.FlowEntry, error) {
	var out api.FlowList
	err := c.do(ctx, request{
		method: http.MethodGet,
		path:   api.PathFlows,
		query:  filter.query(),
		out:    &out,
	})
	if err != nil {
		return nil, err
	}
	return out.Flows, nil
}

// Nodes performs GET /v1/nodes.
func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var out api.NodeList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathNodes, out: &out}); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}
