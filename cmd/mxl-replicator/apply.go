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
// needs no protocol of its own because `POST /v1/requests` is already create-or-update on the
// request's name (§9.1) — one document is one POST.
//
// **Not atomic, deliberately.** Documents are applied in file order; a failure reports which one
// failed, leaves the earlier ones applied, and exits non-zero. Requests are independent durable
// intent, so a partial apply is a partial success rather than a broken half-state — and stopping
// at the first failure is what makes the failure findable.
//
// **The output is a list, not a table** — `<name> <what happened>` per document, in the shape
// `kubectl apply` uses. It was padded to a fixed column width and read as a table with a missing
// header row; it is not one. The second field is prose of unbounded length (`created`, but also
// `INVALID (unknown_output_root): …`), so no honest heading exists for it, and the padding broke
// on any name longer than the column anyway. Tabwriter is not the fix either: lines are printed
// as each POST returns, which is the feedback that matters when a document is about to fail, and
// a tabwriter would buffer them all until the end. The tables live in `get` and `status`.
type ApplyCmd struct {
	Files []string `short:"f" required:"" help:"Manifest file, directory of *.yaml, or - for stdin. Repeatable."`

	DryRun bool `help:"Validate and report what would happen, without writing anything."`

	Prune    bool     `help:"Cancel requests matching --selector that this manifest does not name. Requires --selector."`
	Selector []string `short:"l" help:"Label selector, key=value. Repeatable; all must match."`

	ClientFlags `embed:""`
}

func (c *ApplyCmd) Run(ctx context.Context) error {
	selector, err := parseSelector(c.Selector)
	if err != nil {
		return err
	}

	// **--prune requires a selector** (invariant 14). A whole-fleet prune would cancel anything
	// created by another operator or by the Kubernetes adapter, and the object being cancelled is
	// moving video. The selector is the guard, which is also why there is no confirmation prompt:
	// a prompt in a tool meant to run in CI is a trap, and this has a --dry-run instead.
	if c.Prune && len(selector) == 0 {
		return errors.New("--prune requires --selector")
	}
	if !c.Prune && len(selector) > 0 {
		return errors.New("--selector is only meaningful with --prune")
	}

	docs, err := manifest.Load(c.Files)
	if err != nil {
		return err
	}
	// apply needs every document to be a complete, valid request; delete needs only the names.
	specs, err := manifest.Specs(docs)
	if err != nil {
		return err
	}

	api, err := c.client()
	if err != nil {
		return err
	}

	// A dry run sees this request plus *stored* state, so two new documents in one file that
	// conflict with each other both pass and the second fails on the real apply. Said rather than
	// hidden; a batch endpoint would fix it and is not worth the API surface.
	if c.DryRun {
		warn("dry run: nothing will be written")
	}

	named := make(map[string]bool, len(specs))
	var failed int
	for _, spec := range specs {
		named[spec.Name] = true

		applied, err := api.Apply(ctx, spec, c.DryRun)
		if err != nil {
			fmt.Printf("%s %s\n", spec.Name, applyError(err))
			failed++
			continue
		}
		fmt.Printf("%s %s%s\n", spec.Name, applied.Outcome, statusSuffix(applied.Request.Status))
	}

	if c.Prune {
		pruned, err := c.prune(ctx, api, selector, named)
		if err != nil {
			return err
		}
		failed += pruned
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d requests could not be applied", failed, len(specs))
	}
	return nil
}

// prune cancels requests matching the selector that the manifest did not name.
//
// Matching is **client-side**, over the full request list. The list is small, the server has no
// index for it, and adding a selector query language to the user API for one CLI feature is the
// wrong trade.
func (c *ApplyCmd) prune(ctx context.Context, api *client.Client, selector map[string]string, named map[string]bool) (int, error) {
	existing, err := api.Requests(ctx)
	if err != nil {
		return 0, fmt.Errorf("list requests to prune: %w", err)
	}

	var failed int
	for _, request := range existing {
		if named[request.Name] || !matchesSelector(request.Labels, selector) {
			continue
		}
		if c.DryRun {
			fmt.Printf("%s would be pruned\n", request.Name)
			continue
		}
		if _, err := api.DeleteRequest(ctx, request.Name); err != nil {
			fmt.Printf("%s prune failed: %s\n", request.Name, err)
			failed++
			continue
		}
		fmt.Printf("%s pruned\n", request.Name)
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
// An empty selector matches nothing rather than everything. That is the opposite of the usual
// convention and it is deliberate: the only caller is prune, where "matches everything" is the
// blast radius the required-selector rule exists to prevent, and a bug that produced an empty
// selector would otherwise cancel the fleet.
func matchesSelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
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
