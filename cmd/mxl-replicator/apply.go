package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/manifest"
)

// ApplyCmd is `mxl-replicator apply -f <manifest>`.
//
// The file is the interface an operator keeps; the HTTP API underneath is the contract. This
// needs no protocol of its own because `POST /v1/namespaces/{ns}/requests` is already
// create-or-update on the request's `(namespace, name)` pair (§9.1, §9.3) — one document is one
// POST. `kind: namespace` and `kind: domain` documents go to their own endpoints on the same
// principle.
//
// **Not atomic, deliberately.** Documents are applied in file order; a failure reports which one
// failed, leaves the earlier ones applied, and exits non-zero. Requests are independent durable
// intent, so a partial apply is a partial success rather than a broken half-state — and stopping
// at the first failure is what makes the failure findable.
//
// **The output is a list, not a table** — `<name> <what happened>` per document, in the shape
// `kubectl apply` uses. It was padded to a fixed column width and read as a table with a missing
// header row; it is not one. The second field is prose of unbounded length (`created`, but also
// `INVALID (unknown_area): …`), so no honest heading exists for it, and the padding broke
// on any name longer than the column anyway. Tabwriter is not the fix either: lines are printed
// as each POST returns, which is the feedback that matters when a document is about to fail, and
// a tabwriter would buffer them all until the end. The tables live in `get` and `status`.
type ApplyCmd struct {
	Files []string `short:"f" required:"" help:"Manifest file, directory of *.yaml, or - for stdin. Repeatable."`

	DryRun bool `help:"Validate and report what would happen, without writing anything."`

	Prune     bool     `help:"Cancel requests in --namespace that this manifest does not name. Requires --namespace."`
	Namespace string   `short:"n" help:"Namespace to scope --prune to."`
	Selector  []string `short:"l" help:"Label selector, key=value, narrowing --prune within --namespace. Repeatable; all must match."`

	ClientFlags `embed:""`
}

func (c *ApplyCmd) Run(ctx context.Context) error {
	selector, err := parseSelector(c.Selector)
	if err != nil {
		return err
	}

	// **--prune requires a scope** (invariant 14). A whole-fleet prune would cancel anything
	// created by another operator or by the Kubernetes adapter, and the object being cancelled is
	// moving video. There is no confirmation prompt for the same reason there is none anywhere
	// else here: a prompt in a tool meant to run in CI is a trap, and this has a --dry-run instead.
	//
	// **A namespace satisfies that requirement better than a label selector does** (§9.1), since it
	// is a declared partition rather than an ad-hoc tag — so `-n` is the primary spelling and `-l`
	// narrows within it. *This supersedes "--prune requires --selector".*
	if c.Prune && c.Namespace == "" {
		return errors.New("--prune requires --namespace")
	}
	if !c.Prune && (c.Namespace != "" || len(selector) > 0) {
		return errors.New("--namespace and --selector are only meaningful with --prune")
	}

	docs, err := manifest.Load(c.Files)
	if err != nil {
		return err
	}
	// Namespaces, then domains, then requests, whatever order the file is in (§9.1). The end state
	// does not depend on it, since namespaces auto-create on reference and `Compute` is
	// recomputed; the intermediate state does — a request applied ahead of the document that makes
	// its namespace exclusive is admitted and then invalidated, and one applied ahead of the labels
	// its selector matches expands to nothing and then to something. Both read as the apply having
	// broken something.
	docs = manifest.SortForApply(docs)

	cl, err := c.client()
	if err != nil {
		return err
	}

	// A dry run sees this request plus *stored* state, so two new documents in one file that
	// conflict with each other both pass and the second fails on the real apply. Said rather than
	// hidden; a batch endpoint would fix it and is not worth the API surface.
	if c.DryRun {
		warn("dry run: nothing will be written")
	}

	named := make(map[api.RequestID]bool, len(docs))
	var failed, total int
	for _, doc := range docs {
		total++
		switch doc.Kind {
		case manifest.KindNamespace:
			failed += c.applyNamespace(ctx, cl, doc)
		case manifest.KindDomain:
			failed += c.applyDomain(ctx, cl, doc)
		default:
			id := doc.ID()
			named[id] = true
			failed += c.applyRequest(ctx, cl, doc, id)
		}
	}

	if c.Prune {
		pruned, err := c.prune(ctx, cl, selector, named)
		if err != nil {
			return err
		}
		failed += pruned
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d documents could not be applied", failed, total)
	}
	return nil
}

// applyRequest posts one request document and prints its line. Returns 1 if it failed.
func (c *ApplyCmd) applyRequest(ctx context.Context, cl *client.Client, doc manifest.Document, id api.RequestID) int {
	spec, err := doc.Request.Spec()
	if err != nil {
		fmt.Printf("%s error: %s\n", id, err)
		return 1
	}
	applied, err := cl.Apply(ctx, spec, c.DryRun)
	if err != nil {
		fmt.Printf("%s %s\n", id, applyError(err))
		return 1
	}
	fmt.Printf("%s %s%s%s\n", id, applied.Outcome, pathCount(applied.Request.Status), statusSuffix(applied.Request.Status))
	return 0
}

// pathCount is the blast radius of what was just applied, and it is not decoration (§9.1).
//
// The outcome header says `created` / `updated` / `unchanged` and says nothing about a request whose
// expansion went from one path to forty — which is exactly what deleting a `group_hint:` line does,
// since an omitted selector means every flow in the domain. It costs nothing to print: the POST
// response already carries the expansion.
func pathCount(status api.RequestStatus) string {
	return fmt.Sprintf(" (%d path(s))", len(status.Paths))
}

// applyNamespace posts one namespace document and prints its line. Returns 1 if it failed.
func (c *ApplyCmd) applyNamespace(ctx context.Context, cl *client.Client, doc manifest.Document) int {
	spec, err := doc.Namespace.Spec()
	if err != nil {
		fmt.Printf("namespace/%s error: %s\n", doc.Name(), err)
		return 1
	}
	_, outcome, err := cl.ApplyNamespace(ctx, spec, c.DryRun)
	if err != nil {
		fmt.Printf("namespace/%s %s\n", spec.Name, applyError(err))
		return 1
	}
	fmt.Printf("namespace/%s %s\n", spec.Name, outcome)
	return 0
}

// prune cancels requests in the scoped namespace that the manifest did not name.
//
// **Requests only** (§9.1). It never removes a namespace and never removes a domain label, even
// when the file contains documents of those kinds: a file naming three domain labels would
// otherwise prune the other forty on the fleet, and a pruned namespace is a delete the server
// refuses anyway while requests reference it. Prune exists to make a file authoritative over
// *intent*, and a label is a fact about a host.
//
// Matching is **client-side**, over the namespace's request list. The list is small, the server has
// no index for it, and adding a selector query language to the user API for one CLI feature is the
// wrong trade.
func (c *ApplyCmd) prune(ctx context.Context, cl *client.Client, selector map[string]string, named map[api.RequestID]bool) (int, error) {
	existing, err := cl.Requests(ctx, c.Namespace)
	if err != nil {
		return 0, fmt.Errorf("list requests to prune: %w", err)
	}

	var failed int
	for _, request := range existing {
		id := request.RequestID()
		if named[id] || !matchesSelector(request.Labels, selector) {
			continue
		}
		if c.DryRun {
			fmt.Printf("%s would be pruned\n", id)
			continue
		}
		if _, err := cl.DeleteRequest(ctx, id); err != nil {
			fmt.Printf("%s prune failed: %s\n", id, err)
			failed++
			continue
		}
		fmt.Printf("%s pruned\n", id)
	}
	return failed, nil
}

// parseSelector turns repeated key=value flags into a label set every one of which must match.
func parseSelector(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	selector := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("selector %q: expected key=value", pair)
		}
		selector[key] = value
	}
	return selector, nil
}

// matchesSelector reports whether a request's labels satisfy every term.
//
// An empty selector matches everything, which is the usual convention and is now correct here:
// the blast radius is bounded by `--prune`'s required `--namespace`, so `-l` narrows within a
// scope rather than being the scope. *Under the superseded "--prune requires --selector" rule this
// returned false for an empty selector*, because there the empty case was reachable only through a
// bug and "matches everything" would have cancelled the fleet.
func matchesSelector(labels, selector map[string]string) bool {
	for key, want := range selector {
		if labels[key] != want {
			return false
		}
	}
	return true
}

// statusSuffix appends what the request will do next, when that is not simply "fine".
func statusSuffix(status api.RequestStatus) string {
	switch status.State {
	case api.StateActive, "":
		return ""
	case api.StateEstablishing, api.StateWaiting:
		if status.Reason == "" {
			return " (" + string(status.State) + ")"
		}
		return fmt.Sprintf(" (%s: %s)", status.State, status.Reason)
	default:
		return fmt.Sprintf(" (%s: %s)", status.State, status.Reason)
	}
}

// applyError renders a rejection compactly. The server's reason string is the useful part — it
// names what to change — so the transport wrapping around it is dropped.
func applyError(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		if code := client.Detail(err, "reason_code"); code != "" {
			return fmt.Sprintf("INVALID (%s): %s", code, apiErr.Body.Message)
		}
		return "error: " + apiErr.Body.Message
	}
	return "error: " + err.Error()
}

// sortedLabels renders a label set deterministically, for status output.
// sortedLabels renders labels as one `key=value,key=value` cell, for the table column that has
// room for exactly one. [DescribeCmd] has room for a line each and uses [sortedKeys] instead.
func sortedLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, key := range sortedKeys(labels) {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func sortedKeys(labels map[string]string) []string {
	keys := slices.Collect(maps.Keys(labels))
	sort.Strings(keys)
	return keys
}
