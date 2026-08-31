package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
)

// GetCmd is `mxl-replicator get <kind>`: list one kind of thing.
//
// The three read verbs have three distinct jobs and no overlap between them, which is the whole
// reason there are three:
//
//	status              is the fleet one line at a time — is anything wrong?
//	get <kind>          lists, so you can find the name of the thing you want
//	describe <kind> <n> is everything known about one of them
//
// The kinds are §3's nouns, plural or singular — `get path` and `get paths` are the same command,
// because insisting on one is a rule with no purpose behind it.
type GetCmd struct {
	Kind string `arg:"" enum:"node,nodes,domain,domains,flow,flows,request,requests,path,paths,session,sessions,namespace,namespaces" help:"One of: nodes, domains, flows, requests, paths, sessions, namespaces."`

	// Filters. Each applies to some kinds and not others, and naming one that does not apply is
	// an error rather than a no-op: a filter that silently does nothing is how someone concludes
	// a flow is missing when they only mistyped which field they were narrowing on.
	Node      string   `help:"Only things on this node. Applies to flows, paths and sessions."`
	Domain    string   `help:"Only things in this domain. Applies to flows."`
	Namespace string   `short:"n" help:"Only requests in this namespace. Applies to requests."`
	Selector  []string `short:"l" help:"Only requests with these labels, key=value. Repeatable; all must match."`

	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

func (c *GetCmd) Run(ctx context.Context) error {
	kind := strings.TrimSuffix(c.Kind, "s")

	selector, err := parseSelector(c.Selector)
	if err != nil {
		return err
	}
	if err := c.checkFilters(kind, selector); err != nil {
		return err
	}

	user, err := c.client()
	if err != nil {
		return err
	}

	switch kind {
	case "node":
		return c.nodes(ctx, user)
	case "domain":
		return c.domains(ctx, user)
	case "flow":
		return c.flows(ctx, user)
	case "request":
		return c.requests(ctx, user, selector)
	case "path":
		return c.paths(ctx, user)
	case "session":
		return c.sessions(ctx, user)
	case "namespace":
		return c.namespaces(ctx, user)
	default:
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
}

// checkFilters refuses a filter that cannot apply to the kind being listed.
func (c *GetCmd) checkFilters(kind string, selector map[string]string) error {
	applies := map[string]map[string]bool{
		"node":      {},
		"domain":    {"--node": true},
		"flow":      {"--node": true, "--domain": true},
		"request":   {"--selector": true, "--namespace": true},
		"path":      {"--node": true},
		"session":   {"--node": true},
		"namespace": {},
	}[kind]

	for flag, given := range map[string]bool{
		"--node": c.Node != "", "--domain": c.Domain != "",
		"--namespace": c.Namespace != "", "--selector": len(selector) > 0,
	} {
		if given && !applies[flag] {
			return fmt.Errorf("%s applies to %s, not to %s", flag, kindsFor(flag), c.Kind)
		}
	}
	return nil
}

func kindsFor(flag string) string {
	switch flag {
	case "--node":
		return "domains, flows, paths and sessions"
	case "--domain":
		return "flows"
	default:
		return "requests"
	}
}

func (c *GetCmd) nodes(ctx context.Context, user *client.Client) error {
	nodes, err := user.Nodes(ctx)
	if err != nil {
		return err
	}

	return renderAs(c.Output, nodes, func() {
		if len(nodes) == 0 {
			fmt.Println("no registered nodes")
			return
		}
		// **No domains column.** A node's domains are observed rather than registered (§6), so
		// they are not on the node object at all — `get domains` and `describe node` answer for
		// them, both from inventory. What a node declares is its *areas*.
		out := table("NAME", "LIVE", "AREAS", "FABRICS", "REPLICATOR")
		for _, node := range nodes {
			// Registration is durable and survives the agent being down; only the lease goes
			// away, and an expired lease is not proof that the node's workers stopped (§4.2).
			live := "no"
			if node.Live {
				live = "yes"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
				node.Name, live,
				areaNames(node.Capabilities.Areas),
				fabricNames(node.Capabilities.Fabrics),
				shortVersion(node.Capabilities.Versions.Replicator))
		}
		_ = out.Flush()
	})
}

// domains lists the domains the fleet is observing (§6, §10.6).
//
// One request per node, because domains are per-node observed state and there is no fleet-wide
// endpoint for them. The node list is small and this is a CLI; the alternative is a second
// aggregate endpoint for one verb.
func (c *GetCmd) domains(ctx context.Context, user *client.Client) error {
	nodes, err := user.Nodes(ctx)
	if err != nil {
		return err
	}

	type row struct {
		Node   string `json:"node"`
		Domain string `json:"domain"`
		Flows  int    `json:"flows"`
	}
	var rows []row
	settling := false
	for _, node := range nodes {
		if c.Node != "" && node.Name != c.Node {
			continue
		}
		list, err := user.NodeDomains(ctx, node.Name)
		if err != nil {
			return err
		}
		settling = settling || list.Settling
		for _, domain := range list.Domains {
			rows = append(rows, row{Node: node.Name, Domain: domain.Domain.String(), Flows: len(domain.Flows)})
		}
	}
	if settling {
		warn("still settling: not every node has reported yet")
	}

	return renderAs(c.Output, rows, func() {
		if len(rows) == 0 {
			fmt.Println("no domains observed")
			return
		}
		out := table("NODE", "DOMAIN", "FLOWS")
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\t%d\n", r.Node, r.Domain, r.Flows)
		}
		_ = out.Flush()
	})
}

func (c *GetCmd) flows(ctx context.Context, user *client.Client) error {
	entries, err := user.Flows(ctx, client.FlowFilter{Node: c.Node, Domain: c.Domain})
	if err != nil {
		return err
	}

	return renderAs(c.Output, entries, func() {
		if len(entries) == 0 {
			fmt.Println("no flows observed")
			return
		}
		// REPLICATED is a column of its own, not decoration: a label selector never matches a flow
		// this project is writing, so without it a broad selector silently skips flows and there
		// is nothing anywhere to say why (§9.1, §10.7).
		out := table("NODE", "DOMAIN", "FLOW", "PRODUCING", "REPLICATED", "GROUP HINT")
		for _, entry := range entries {
			// Coarse and hysteretic, never a head index: inventory is a full snapshot written to
			// the store, and a field that changed every frame would turn it into a per-heartbeat
			// write stream (§11.1).
			producing := "idle"
			if entry.Producing {
				producing = "yes"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
				entry.Node, entry.Domain, entry.ID, producing, replicatedText(entry.Replicated),
				groupHintText(entry.GroupHint))
		}
		_ = out.Flush()
	})
}

func (c *GetCmd) requests(ctx context.Context, user *client.Client, selector map[string]string) error {
	requests, err := user.Requests(ctx, c.Namespace)
	if err != nil {
		return err
	}
	if len(selector) > 0 {
		kept := requests[:0]
		for _, request := range requests {
			if matchesSelector(request.Labels, selector) {
				kept = append(kept, request)
			}
		}
		requests = kept
	}

	return renderAs(c.Output, requests, func() { printRequestTable(requests) })
}

// namespaces lists the request partitions (§9.3).
//
// `paths` and the request count are the two things worth a column: the first is the one rule a
// namespace carries, and the second is what a DELETE will be refused over.
func (c *GetCmd) namespaces(ctx context.Context, user *client.Client) error {
	namespaces, err := user.Namespaces(ctx)
	if err != nil {
		return err
	}

	return renderAs(c.Output, namespaces, func() {
		if len(namespaces) == 0 {
			fmt.Println("no namespaces")
			return
		}
		out := table("NAME", "PATHS", "REQUESTS", "DESCRIPTION")
		for _, ns := range namespaces {
			fmt.Fprintf(out, "%s\t%s\t%d\t%s\n", ns.Name, ns.Paths, ns.Requests, ns.Description)
		}
		_ = out.Flush()
	})
}

func (c *GetCmd) paths(ctx context.Context, user *client.Client) error {
	response, err := user.Paths(ctx)
	if err != nil {
		return err
	}
	if response.Settling {
		warn("still settling: this is intent, not what is running")
	}

	paths := filterPaths(response.Paths, c.Node)
	return renderAs(c.Output, paths, func() {
		if len(paths) == 0 {
			fmt.Println("no paths")
			return
		}
		out := table("ID", "FLOW", "FROM", "TO", "STATE", "REQUESTS")
		for _, path := range paths {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%d\n",
				path.ID, path.Source.Flow,
				path.Source.Node+"/"+path.Source.Domain, path.Destination.Endpoint(),
				path.State, len(path.Requests))
		}
		_ = out.Flush()
		printPathReasons(paths)
	})
}

func (c *GetCmd) sessions(ctx context.Context, user *client.Client) error {
	response, err := user.Paths(ctx)
	if err != nil {
		return err
	}

	// Sessions are reached through paths because that is what they are attached to: a session is
	// the concrete worker pair *realising* a path, and it has no identity apart from one (§3).
	type row struct {
		path    api.Path
		session api.Session
	}
	var rows []row
	for _, path := range filterPaths(response.Paths, c.Node) {
		if path.Session != nil {
			rows = append(rows, row{path: path, session: *path.Session})
		}
	}

	sessions := make([]api.Session, 0, len(rows))
	for _, r := range rows {
		sessions = append(sessions, r.session)
	}

	return renderAs(c.Output, sessions, func() {
		if len(rows) == 0 {
			fmt.Println("no sessions")
			return
		}
		out := table("ID", "PATH", "FROM", "TO", "PROVIDER", "TARGET", "INITIATOR")
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.session.ID, r.path.ID,
				r.path.Source.Node+"/"+r.path.Source.Domain, r.path.Destination.Endpoint(),
				r.session.Interface.Provider,
				workerText(r.session.Target), workerText(r.session.Initiator))
		}
		_ = out.Flush()
	})
}

// --- shared ---------------------------------------------------------------------------------

// table starts a block of aligned output: every column is as wide as its own widest cell, rather
// than as wide as a number counted out in a format string and re-counted whenever a field is
// added. Shared by get, status and describe.
//
// One writer per view rather than one per table. tabwriter aligns tab-terminated cells only while
// the lines carrying them are contiguous, so any line without tabs — a blank line, a section
// heading — ends the block and the next table sizes its columns independently. Everything written
// to one writer also reaches stdout in the order it was written, which interleaving a writer with
// bare Printf calls does not guarantee.
//
// Headings are optional: a label/value block is a two-column table without them.
func table(headings ...string) *tabwriter.Writer {
	out := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(headings) > 0 {
		fmt.Fprintln(out, strings.Join(headings, "\t"))
	}
	return out
}

// flush is deferred rather than called at the end of a view, because several views return early
// and output that is buffered but never flushed is output that never appears.
func flush(out *tabwriter.Writer) { _ = out.Flush() }

func filterPaths(paths []api.Path, node string) []api.Path {
	if node == "" {
		return paths
	}
	// Both ends, not a switch: a node is routinely both ends of a path — same node, different
	// domain is legitimate and is what the loopback configuration does (§7.2).
	var kept []api.Path
	for _, path := range paths {
		if path.Source.Node == node || path.Destination.Node == node {
			kept = append(kept, path)
		}
	}
	return kept
}

// printPathReasons puts the reasons under the table rather than in it: the reason is what names
// the thing to fix, and a column would truncate it.
func printPathReasons(paths []api.Path) {
	for _, path := range paths {
		if path.Reason != "" && path.State != api.StateActive {
			fmt.Printf("\n%s: %s\n", path.ID, path.Reason)
		}
	}
}

func workerText(endpoint *api.SessionEndpoint) string {
	if endpoint == nil {
		return "—"
	}
	text := fmt.Sprintf("%s on %s", endpoint.State, endpoint.Node)
	if endpoint.Restarts > 0 {
		// A restart count is the DEGRADED/FAILED signal, never an exit code (§15.1), so it
		// belongs where it will be seen rather than only in the detail view.
		text += fmt.Sprintf(" (%d restarts)", endpoint.Restarts)
	}
	return text
}

// replicatedText renders the provenance flag. "no" rather than blank, because the distinction
// between "this node is not writing it" and "nobody said" is the whole point of the column.
func replicatedText(replicated bool) string {
	if replicated {
		return "yes"
	}
	return "no"
}

func groupHintText(hint *api.GroupHint) string {
	switch {
	case hint == nil:
		return ""
	case hint.Type != "":
		return fmt.Sprintf("%s (%s)", hint.Name, hint.Type)
	default:
		return hint.Name
	}
}

// areaNames renders a node's areas with their grants, `name:rw` — the same spelling the
// `--agent-area` flag takes, so what an operator reads back matches what they typed.
func areaNames(areas []api.Area) string {
	if len(areas) == 0 {
		// Worth saying rather than leaving blank: a node with no area offers no sources and
		// accepts no destinations, and that is the first thing to check when a request is INVALID.
		return "none"
	}
	names := make([]string, 0, len(areas))
	for _, area := range areas {
		grants := ""
		if area.Read {
			grants += "r"
		}
		if area.Write {
			grants += "w"
		}
		names = append(names, area.Name+":"+grants)
	}
	return strings.Join(names, ",")
}

// shortVersion keeps a table column to a column's worth.
//
// The full string carries the build metadata Go stamps in, which is exactly what `describe node`
// is for; in a list it is a paragraph in a cell that pushes every other column off the terminal.
func shortVersion(version string) string {
	if version == "" {
		return "—"
	}
	short, _, _ := strings.Cut(version, " ")
	return short
}

func fabricNames(fabrics []api.FabricAttachment) string {
	names := make([]string, 0, len(fabrics))
	for _, fabric := range fabrics {
		names = append(names, string(fabric.Provider)+":"+fabric.Fabric)
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, ",")
}
