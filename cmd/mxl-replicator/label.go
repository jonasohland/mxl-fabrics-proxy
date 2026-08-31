package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
	"github.com/jonasohland/mxl-replicator/internal/manifest"
)

// LabelCmd is `mxl-replicator label domain <node>:<area>/<elements> key=value key-`.
//
//	mxl-replicator label domain studio-a:media/cameras role=cameras name=cameras
//	mxl-replicator label domain studio-a:media/cameras role-
//
// # Why a fourth verb, and why it is not a fourth vocabulary
//
// §19 dropped a separate `xpt` CLI on the argument that two spellings for one thing is worse than
// one, and that argument has to be answered rather than ignored. It is: this writes to
// `POST /v1/nodes/{node}/domains`, the same endpoint a `kind: domain` document applies, so there
// is one model and one server-side rule. What differs is only the gesture — the manifest is the
// desired set an operator keeps in git, this is a one-shot edit of one record, and "an operator
// notices a domain and names it" is genuinely interactive in a way that authoring a file is not
// (§9.1).
//
// # It sends a patch, and that is what makes it durable
//
// `key=value` sets, `key-` removes, following the convention operators already have — and the
// server merges a patch against nothing, so an edit made here survives a later apply that does not
// mention it (§9.1). Sending a patch rather than a read-modify-write is also strictly better for a
// reason worth recording: RMW on a shared record has a lost-update race, and two operators
// labelling one domain between the same read and write lose one edit silently. That is the exact
// failure mode this record's ownership story exists to avoid.
type LabelCmd struct {
	Kind   string   `arg:"" enum:"domain" help:"What to label. Only 'domain' today."`
	Target string   `arg:"" help:"<node>:<area>/<elements>, e.g. studio-a:media/cameras."`
	Labels []string `arg:"" optional:"" help:"key=value to set, key- to remove. Repeatable."`

	DryRun bool `help:"Report what would change and what it would stop, without writing anything."`

	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

func (c *LabelCmd) Run(ctx context.Context) error {
	node, domain, err := parseLabelTarget(c.Target)
	if err != nil {
		return err
	}

	patch, err := parseLabelEdits(c.Labels)
	if err != nil {
		return err
	}

	cl, err := c.client()
	if err != nil {
		return err
	}

	if c.DryRun {
		warn("dry run: nothing will be written")
	}

	record, outcome, err := cl.LabelDomain(ctx, node, api.DomainLabelWrite{Domain: domain, Patch: patch}, c.DryRun)
	if err != nil {
		return err
	}

	return renderAs(c.Output, record, func() {
		out := table()
		defer flush(out)

		fmt.Fprintf(out, "%s:%s\t%s\n", node, domain, outcome)
		if len(record.Labels) == 0 {
			fmt.Fprintln(out, "  no labels")
		}
		for _, key := range sortedKeys(record.Labels) {
			fmt.Fprintf(out, "  %s\t%s\n", key, record.Labels[key])
		}

		printBlastRadius(out, record)
	})
}

// printBlastRadius says what this label write moved, **on the real write as well as on a dry run**
// (§9.1).
//
// It prints rather than prompts: the CLI is scripted by the same operators who use it
// interactively, and a verb that blocks on a tty is a verb that hangs in a pipeline. A label joins
// or removes a domain from a request's expansion, so it starts and stops media exactly as a
// request does — one level of indirection away, which makes it easier to do by accident rather
// than harder.
func printBlastRadius(out *tabwriter.Writer, record *api.DomainLabelResult) {
	if len(record.Stopped) > 0 {
		heading(out, "  Stops")
		for _, path := range record.Stopped {
			// The requests that were feeding it, which is what `path.requests[]` already answers —
			// so this is a renderer and not a computation. It is also the answer to "who else cares
			// about this", which is what an operator needs before deciding whether to proceed.
			fmt.Fprintf(out, "  %s\t%s → %s\t%s\n",
				path.ID, path.Source.Node+"/"+path.Source.Domain, path.Destination.Endpoint(),
				strings.Join(path.Requests, ", "))
		}
		// Nothing is frozen here (§4.2): a label removing a path is not a node going away, so the
		// teardown is immediate.
		fmt.Fprintln(out, "  \tthese stop immediately\t")
	}

	if len(record.Started) > 0 {
		heading(out, "  Starts")
		for _, path := range record.Started {
			fmt.Fprintf(out, "  %s\t%s → %s\t%s\n",
				path.ID, path.Source.Node+"/"+path.Source.Domain, path.Destination.Endpoint(),
				strings.Join(path.Requests, ", "))
		}
	}
}

// parseLabelTarget splits `<node>:<area>/<elements>`.
//
// **The colon splits node from domain, and the domain half is not re-split anywhere else**: it
// goes through [manifest.ParseDomain], which is the one parser of a domain string in the tree
// (§10.6). A node name may itself contain a colon — it is operator-assigned free text — so the
// split is on the *last* one, which is the only reading that makes `rack:1:media/cameras`
// unambiguous.
func parseLabelTarget(target string) (string, api.Domain, error) {
	index := strings.LastIndex(target, ":")
	if index < 0 {
		return "", api.Domain{}, fmt.Errorf("target %q: expected <node>:<area>/<elements>, e.g. studio-a:media/cameras", target)
	}

	node, rest := target[:index], target[index+1:]
	if node == "" {
		return "", api.Domain{}, fmt.Errorf("target %q: names no node", target)
	}

	domain, err := manifest.ParseDomain(rest)
	if err != nil {
		return "", api.Domain{}, fmt.Errorf("target %q: domain %w", target, err)
	}
	return node, domain, nil
}

// parseLabelEdits turns `key=value` and `key-` arguments into a patch.
func parseLabelEdits(args []string) (*api.DomainLabelPatch, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("nothing to do: give key=value to set or key- to remove")
	}

	patch := &api.DomainLabelPatch{}
	for _, arg := range args {
		switch {
		case strings.HasSuffix(arg, "-") && !strings.Contains(arg, "="):
			key := strings.TrimSuffix(arg, "-")
			if key == "" {
				return nil, fmt.Errorf("%q removes no key", arg)
			}
			patch.Remove = append(patch.Remove, key)

		default:
			key, value, ok := strings.Cut(arg, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("%q is neither key=value nor key-", arg)
			}
			if patch.Set == nil {
				patch.Set = map[string]string{}
			}
			patch.Set[key] = value
		}
	}
	return patch, nil
}

// applyDomain posts one `kind: domain` document and prints its line. Returns 1 if it failed.
func (c *ApplyCmd) applyDomain(ctx context.Context, cl *client.Client, doc manifest.Document) int {
	node, write, err := doc.Domain.Write()
	if err != nil {
		fmt.Printf("domain/%s error: %s\n", doc.Name(), err)
		return 1
	}
	_, outcome, err := cl.LabelDomain(ctx, node, write, c.DryRun)
	if err != nil {
		fmt.Printf("domain/%s:%s %s\n", node, write.Domain, applyError(err))
		return 1
	}
	fmt.Printf("domain/%s:%s %s\n", node, write.Domain, outcome)
	return 0
}
