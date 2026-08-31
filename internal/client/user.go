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

// Apply performs POST /v1/namespaces/{ns}/requests: create or update, keyed on the spec's
// `(namespace, name)` pair (§9.3).
//
// The namespace comes from the spec and defaults to [api.DefaultNamespace], so a caller that does
// not care about partitions never has to name one.
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
		path:   api.NamespaceRequestsPath(spec.NamespaceOrDefault()),
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

// DeleteRequest performs DELETE /v1/namespaces/{ns}/requests/{name}: cancel the intent.
//
// The `(namespace, name)` pair is the ID (§9.1, §9.3), which is what lets `delete -f` work off the
// same file that created the requests without the file still having to be accurate about anything
// else.
//
// A request that is not there is not an error to this method — it returns found=false — because
// deleting what a manifest names is idempotent by nature: the second run of a delete should
// succeed, not fail because the first one worked.
func (c *Client) DeleteRequest(ctx context.Context, id api.RequestID) (bool, error) {
	err := c.do(ctx, request{
		method: http.MethodDelete,
		path:   api.RequestPath(id),
	})
	if Code(err) == api.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Requests performs GET /v1/requests: every request in the fleet.
//
// namespace narrows it; empty is every partition. The fleet-wide default is what `get requests`
// and `status` want, since neither can know which namespaces exist before asking.
func (c *Client) Requests(ctx context.Context, namespace string) ([]api.Request, error) {
	query := url.Values{}
	if namespace != "" {
		query.Set(api.QueryNamespace, namespace)
	}

	var out api.RequestList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathRequests, query: query, out: &out}); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Request performs GET /v1/namespaces/{ns}/requests/{name}.
func (c *Client) Request(ctx context.Context, id api.RequestID) (*api.Request, error) {
	var out api.Request
	if err := c.do(ctx, request{method: http.MethodGet, path: api.RequestPath(id), out: &out}); err != nil {
		return nil, err
	}
	return &out, nil
}

// Namespaces performs GET /v1/namespaces.
func (c *Client) Namespaces(ctx context.Context) ([]api.NamespaceInfo, error) {
	var out api.NamespaceList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathNamespaces, out: &out}); err != nil {
		return nil, err
	}
	return out.Namespaces, nil
}

// Namespace performs GET /v1/namespaces/{ns}.
func (c *Client) Namespace(ctx context.Context, name string) (*api.NamespaceInfo, error) {
	var out api.NamespaceInfo
	if err := c.do(ctx, request{method: http.MethodGet, path: api.NamespacePath(name), out: &out}); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApplyNamespace performs POST /v1/namespaces: create or update, keyed on the name.
func (c *Client) ApplyNamespace(ctx context.Context, spec api.Namespace, dryRun bool) (*api.NamespaceInfo, string, error) {
	query := url.Values{}
	if dryRun {
		query.Set(api.QueryDryRun, "true")
	}

	var out api.NamespaceInfo
	var header http.Header
	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   api.PathNamespaces,
		query:  query,
		body:   spec,
		out:    &out,
		header: &header,
	}); err != nil {
		return nil, "", err
	}

	outcome := header.Get(api.HeaderOutcome)
	if outcome == "" {
		outcome = api.OutcomeUpdated
	}
	return &out, outcome, nil
}

// DeleteNamespace performs DELETE /v1/namespaces/{ns}.
//
// Refused by the server while any request references it (§9.3), which surfaces here as an
// ordinary error carrying the count — the system never cancels intent on the user's behalf, and a
// cascading delete would be a cascading teardown of live media.
func (c *Client) DeleteNamespace(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, request{method: http.MethodDelete, path: api.NamespacePath(name)})
	if Code(err) == api.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// NodeDomains performs GET /v1/nodes/{node}/domains: the domains that node is **observing**
// (§9.1).
//
// Not registration data: a node's domains are discovered, so they are observed state and reach the
// server through inventory (§6). The `settling` flag comes back with them, because the answer is
// inventory-dependent and an empty list during the window would look exactly like a node with no
// domains at all.
func (c *Client) NodeDomains(ctx context.Context, node string) (*api.DomainList, error) {
	var out api.DomainList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.NodeDomainsPath(node), out: &out}); err != nil {
		return nil, err
	}
	return &out, nil
}

// LabelDomain performs POST /v1/nodes/{node}/domains (§9.1).
//
// The body is an **apply** or a **patch**, and which one carries the ownership rule: an apply owns
// the keys it declares, a patch merges against nothing and leaves what a future apply believes it
// owns untouched. That is what makes an interactive `label` edit survive a later apply from git.
//
// dryRun asks the server to write nothing and return what the record *would* become — which is
// what `label --dry-run` shows, on the same argument requests take: a label joins or removes a
// domain from a request's expansion, so it starts and stops media one level of indirection away.
func (c *Client) LabelDomain(ctx context.Context, node string, body api.DomainLabelWrite, dryRun bool) (*api.DomainLabelResult, string, error) {
	query := url.Values{}
	if dryRun {
		query.Set(api.QueryDryRun, "true")
	}

	var out api.DomainLabelResult
	var header http.Header
	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   api.NodeDomainsPath(node),
		query:  query,
		body:   body,
		out:    &out,
		header: &header,
	}); err != nil {
		return nil, "", err
	}

	outcome := header.Get(api.HeaderOutcome)
	if outcome == "" {
		outcome = api.OutcomeUpdated
	}
	return &out, outcome, nil
}

// Nodes performs GET /v1/nodes.
func (c *Client) Nodes(ctx context.Context) ([]api.Node, error) {
	var out api.NodeList
	if err := c.do(ctx, request{method: http.MethodGet, path: api.PathNodes, out: &out}); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}
