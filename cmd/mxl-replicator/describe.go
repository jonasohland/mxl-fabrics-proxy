package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/client"
)

// DescribeCmd is `mxl-replicator describe <kind> <name>`: everything known about one thing.
//
// The five kinds are the five nouns of §3, and they are deliberately not collapsed:
//
//	node      a host, one agent — what it advertises and what it is currently running
//	domain    a place on a node that holds flows: its labels, and what is in it
//	flow      a UUID. Unique to the media, *not* to a location: after replication the same
//	          flow exists on both nodes, so this lists every place it is
//	request   durable user intent, and what its selector currently expands to
//	path      the deduplicated edge a request expands onto, and its refcount
//	session   the concrete worker pair realising a path: epoch, negotiated fabric, both ends
//	namespace a request partition: its path policy, and what is in it
//
// Path and session stay separate even though they are 1:1 in practice, because they are separate
// layers (§4): a path is derived state that outlives any particular session, and a session is
// ephemeral and is re-established whenever either end restarts. Collapsing them would suggest a
// path dies when its workers do, which is exactly the thing this design is built not to do.
//
// `status` is the overview; this is the detail. There is deliberately no third way to see either.
type DescribeCmd struct {
	Kind string `arg:"" enum:"node,domain,flow,request,path,session,namespace" help:"One of: node, domain, flow, request, path, session, namespace."`
	Name string `arg:"" help:"The node name, <node>:<area>/<elements>, flow id, request name, path id, session id or namespace."`

	// Namespace scopes a request name, because a request's ID is the (namespace, name) pair
	// (§9.3). Ignored for every other kind, whose names are fleet-wide.
	Namespace string `short:"n" help:"Namespace the request is in. Applies to requests; defaults to 'default'."`

	ClientFlags `embed:""`
	OutputFlags `embed:""`
}

func (c *DescribeCmd) Run(ctx context.Context) error {
	api, err := c.client()
	if err != nil {
		return err
	}

	switch c.Kind {
	case "node":
		return c.node(ctx, api)
	case "domain":
		return c.domain(ctx, api)
	case "flow":
		return c.flow(ctx, api)
	case "request":
		return c.request(ctx, api)
	case "path":
		return c.path(ctx, api)
	case "session":
		return c.session(ctx, api)
	case "namespace":
		return c.namespace(ctx, api)
	default:
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
}

// --- node -----------------------------------------------------------------------------------

func (c *DescribeCmd) node(ctx context.Context, user *client.Client) error {
	nodes, err := user.Nodes(ctx)
	if err != nil {
		return err
	}

	var node *api.Node
	for i := range nodes {
		if nodes[i].Name == c.Name {
			node = &nodes[i]
		}
	}
	if node == nil {
		return fmt.Errorf("no node %q, the fleet has %s", c.Name, nodeNames(nodes))
	}

	// The paths touching this node are what an operator is usually actually after, and they are
	// not part of the node object — a node knows what it can do, the control plane knows what it
	// is doing.
	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}

	// Domains are **observed**, not registered (§6), so they are a second request rather than a
	// field on the node.
	domains, err := user.NodeDomains(ctx, c.Name)
	if err != nil {
		return err
	}

	return c.render(node, func() { printNode(*node, domains, paths.Paths) })
}

func printNode(node api.Node, domains *api.DomainList, paths []api.Path) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Node      %s\n", node.Name)

	// Registration is durable and survives the agent being down; only the lease goes away. An
	// expired lease is not proof that this node's workers stopped, which is why the two are
	// reported separately rather than as one "up" (§4.2, §7.1).
	live := "no lease"
	if node.Live {
		live = "leased by " + node.Instance
	}
	fmt.Fprintf(out, "  liveness\t%s\n", live)
	if !node.RegisteredAt.IsZero() {
		fmt.Fprintf(out, "  registered\t%s\n", node.RegisteredAt.Format(time.RFC3339))
	}
	if !node.LastSeen.IsZero() {
		fmt.Fprintf(out, "  last seen\t%s (%s ago)\n", node.LastSeen.Format(time.RFC3339), since(node.LastSeen))
	}

	versions := node.Capabilities.Versions
	fmt.Fprintf(out, "  versions\treplicator %s, protocol %d\n", versions.Replicator, versions.Protocol)
	if versions.MXL != "" || versions.Libfabric != "" {
		// The mxl version is the non-obvious one: target_info is produced by one node's
		// mxl-fabrics and consumed by another's, so a pair straddling a version boundary is a
		// compatibility concern neither agent can see alone (§10.2). An empty label cell keeps it
		// under the line above without counting the indent out by hand.
		fmt.Fprintf(out, "\tworker %s, mxl %s, libfabric %s\n", versions.Proxy, versions.MXL, versions.Libfabric)
	}
	fmt.Fprintf(out, "  sched_prio\t%t\n", node.Capabilities.SchedPrio)
	if node.Capabilities.PortRange != "" {
		fmt.Fprintf(out, "  port range\t%s (inbound)\n", node.Capabilities.PortRange)
	}

	// **Observed domains**, since there is no configured mapping to report (§6). A domain this
	// project replicates *into* is listed like any other: a domain is a place rather than a
	// direction (§10.6).
	heading(out, "  Domains")
	switch {
	case domains == nil:
	case domains.Settling:
		fmt.Fprintln(out, "    still settling: this node has not reported yet")
	case len(domains.Domains) == 0:
		fmt.Fprintln(out, "    none observed")
	}
	if domains != nil {
		for _, domain := range domains.Domains {
			fmt.Fprintf(out, "    %s\t%d flow(s)\n", domain.Domain, len(domain.Flows))
		}
	}

	// Between them the whole of this project's authority over this node's filesystem (§10.6,
	// §13). A node with no writable area is not a replication destination at all, and saying so
	// plainly here saves an operator working it out from an INVALID request later.
	heading(out, "  Areas")
	if len(node.Capabilities.Areas) == 0 {
		fmt.Fprintln(out, "    none: no sources, no destinations")
	}
	for _, area := range node.Capabilities.Areas {
		fmt.Fprintf(out, "    %s\t%s\t%s\n", area.Name, grantText(area), area.Path)
	}

	heading(out, "  Fabric attachments")
	if len(node.Capabilities.Fabrics) == 0 {
		fmt.Fprintln(out, "    none")
	}
	for _, fabric := range node.Capabilities.Fabrics {
		// A line each, as for labels: an attachment is a set of named fields, and four positional
		// columns made the reader count which was the fabric label and which was the address —
		// with the capability flags and the message limit sharing the last one.
		fmt.Fprintln(out, "    "+string(fabric.Provider))
		fmt.Fprintf(out, "      fabric\t%s\n", fabric.Fabric)
		if fabric.Address != "" {
			fmt.Fprintf(out, "      address\t%s\n", fabric.Address)
		}
		if fabric.Device != "" {
			// Diagnostics only, and not the netdev name for verbs or efa — but it is what an
			// operator matches against `fi_info` when an attachment does not come up (§10.5).
			fmt.Fprintf(out, "      device\t%s\n", fabric.Device)
		}
		fmt.Fprintf(out, "      capabilities\t%s\n", capFlags(fabric.CapFlags))
		if fabric.MaxMessageSize > 0 {
			fmt.Fprintf(out, "      max message\t%s\n", byteSize(fabric.MaxMessageSize))
		}
	}

	printNodePaths(out, node.Name, paths)
}

func printNodePaths(out *tabwriter.Writer, name string, paths []api.Path) {
	// Both branches, not a switch: a node is routinely both ends of a path. Same node, *different*
	// domain is legitimate and is exactly what the loopback configuration does (§7.2), and a
	// switch that matched the source first would hide half of what that node is running.
	var rows [][]string
	for _, path := range paths {
		if path.Source.Node == name {
			rows = append(rows, []string{"initiator", path.ID, path.Source.Flow, path.Destination.Endpoint(), string(path.State)})
		}
		if path.Destination.Node == name {
			rows = append(rows, []string{"target", path.ID, path.Source.Flow, path.Source.Node + "/" + path.Source.Domain, string(path.State)})
		}
	}

	fmt.Fprintln(out)
	if len(rows) == 0 {
		fmt.Fprintln(out, "  No paths touch this node.")
		return
	}

	fmt.Fprintln(out, "  ROLE\tPATH\tFLOW\tPEER\tSTATE")
	for _, row := range rows {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\t%s\n", row[0], row[1], row[2], row[3], row[4])
	}
}

// --- domain ---------------------------------------------------------------------------------

// domain describes one `(node, domain)`: what it is called, what is in it, and — for each flow —
// whether **this node is the one writing it** (§10.7).
//
// That last column is not decoration. A label selector never matches a flow this project is
// itself writing, so a broad selector silently skips those flows; under the superseded
// directory-granular rule the whole domain was missing from the source's options, which an
// operator could at least see as a category. Per-flow provenance is finer and therefore *less*
// obvious, and this is where that cost is paid back (§10.7).
func (c *DescribeCmd) domain(ctx context.Context, user *client.Client) error {
	node, domain, err := parseLabelTarget(c.Name)
	if err != nil {
		return err
	}

	list, err := user.NodeDomains(ctx, node)
	if err != nil {
		return err
	}

	var found *api.DomainInfo
	for i := range list.Domains {
		if list.Domains[i].Domain.Equal(domain) {
			found = &list.Domains[i]
		}
	}
	if found == nil {
		return fmt.Errorf("node %q has no domain %q, observed or labelled", node, domain)
	}

	return c.render(found, func() { printDomain(node, *found, list.Settling) })
}

func printDomain(node string, domain api.DomainInfo, settling bool) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Domain    %s:%s\n", node, domain.Domain)
	fmt.Fprintf(out, "  area\t%s\n", domain.Domain.Area)

	switch {
	case settling:
		fmt.Fprintf(out, "  observed\tstill settling: this node has not reported yet\n")
	case domain.Observed:
		fmt.Fprintf(out, "  observed\tyes\n")
	default:
		// A label on a domain the node does not report is accepted and inert — a pending record,
		// not an error, and it resolves by itself when a producer appears (§10.7).
		fmt.Fprintf(out, "  observed\tno: labelled but not reported, a request selecting it waits\n")
	}

	if name := domain.Name(); name != "" {
		fmt.Fprintf(out, "  name\t%s (the `name` label, rendered as domain_name in metrics)\n", name)
	}

	if len(domain.Labels) > 0 {
		heading(out, "  Labels")
		for _, key := range sortedKeys(domain.Labels) {
			fmt.Fprintf(out, "    %s\t%s\n", key, domain.Labels[key])
		}
	}

	heading(out, "  Flows")
	if len(domain.Flows) == 0 {
		fmt.Fprintln(out, "    none")
		return
	}
	fmt.Fprintln(out, "    FLOW\tPRODUCING\tREPLICATED\tGROUP HINT")
	for _, flow := range domain.Flows {
		producing := "idle"
		if flow.Producing {
			producing = "yes"
		}
		// A flow this node's own target worker is writing is never matched by a label selector,
		// which is the self-amplification guard (§10.7). Saying so here is what keeps a silently
		// skipped flow diagnosable.
		replicated := "no"
		if flow.Replicated {
			replicated = "yes: written by this node, so no label selector matches it"
		}
		fmt.Fprintf(out, "    %s\t%s\t%s\t%s\n", flow.ID, producing, replicated, groupHintText(flow.GroupHint))
	}
}

// --- flow -----------------------------------------------------------------------------------

func (c *DescribeCmd) flow(ctx context.Context, user *client.Client) error {
	// A flow ID is unique to the media, not to a location (§3), so this is a list: after
	// replication the same ID exists on both nodes, and that is the point rather than a
	// duplicate to disambiguate.
	entries, err := user.Flows(ctx, client.FlowFilter{Flow: c.Name})
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("flow %q is not observed anywhere in the fleet", c.Name)
	}

	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}

	return c.render(entries, func() { printFlow(c.Name, entries, paths.Paths) })
}

func printFlow(id string, entries []api.FlowEntry, paths []api.Path) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Flow      %s\n", id)

	if hint := firstGroupHint(entries); hint != nil {
		// Built before it is written: a tabwriter cell cannot be assembled across calls, because
		// the line is only measured once it is complete.
		name := fmt.Sprintf("%q", hint.Name)
		if hint.Type != "" {
			name += fmt.Sprintf(" (%s)", hint.Type)
		}
		fmt.Fprintf(out, "  group hint\t%s\n", name)
	}
	if summary := describeDefinition(entries[0].Definition); summary != "" {
		fmt.Fprintf(out, "  definition\t%s\n", summary)
	}

	heading(out, "  Locations")
	fmt.Fprintln(out, "  NODE/DOMAIN\tPRODUCING")
	for _, entry := range entries {
		// Coarse and hysteretic on purpose — never a head index, which would change every frame
		// and turn inventory into a per-heartbeat write stream (§11.1).
		producing := "idle"
		if entry.Producing {
			producing = "yes"
		}
		fmt.Fprintf(out, "  %s\t%s\n", entry.Node+"/"+entry.Domain, producing)
	}

	var carrying []api.Path
	for _, path := range paths {
		if path.Source.Flow == id {
			carrying = append(carrying, path)
		}
	}

	fmt.Fprintln(out)
	if len(carrying) == 0 {
		fmt.Fprintln(out, "  No path replicates this flow.")
		return
	}
	fmt.Fprintln(out, "  PATH\tFROM\tTO\tSTATE")
	for _, path := range carrying {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\n",
			path.ID, path.Source.Node+"/"+path.Source.Domain, path.Destination.Endpoint(), path.State)
	}
}

func firstGroupHint(entries []api.FlowEntry) *api.GroupHint {
	for _, entry := range entries {
		if entry.GroupHint != nil {
			return entry.GroupHint
		}
	}
	return nil
}

// describeDefinition pulls the handful of NMOS fields worth a line of summary out of the verbatim
// flow_def bytes.
//
// Decoded here for display only. The definition travels as [json.RawMessage] everywhere it
// matters, because the destination flow must reproduce it exactly — including fields no struct in
// this tree knows about — and the session identity hashes those bytes (§5.4).
func describeDefinition(raw json.RawMessage) string {
	var def struct {
		Label     string `json:"label"`
		Format    string `json:"format"`
		MediaType string `json:"media_type"`
		GrainRate *struct {
			Numerator   int `json:"numerator"`
			Denominator int `json:"denominator"`
		} `json:"grain_rate"`
		Width  int `json:"frame_width"`
		Height int `json:"frame_height"`
	}
	if json.Unmarshal(raw, &def) != nil {
		return ""
	}

	parts := make([]string, 0, 4)
	if def.Label != "" {
		parts = append(parts, fmt.Sprintf("%q", def.Label))
	}
	if def.MediaType != "" {
		parts = append(parts, def.MediaType)
	} else if def.Format != "" {
		parts = append(parts, strings.TrimPrefix(def.Format, "urn:x-nmos:format:"))
	}
	if def.Width > 0 && def.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", def.Width, def.Height))
	}
	if def.GrainRate != nil && def.GrainRate.Numerator > 0 {
		denominator := def.GrainRate.Denominator
		if denominator == 0 {
			denominator = 1
		}
		parts = append(parts, fmt.Sprintf("%.3f Hz", float64(def.GrainRate.Numerator)/float64(denominator)))
	}
	return strings.Join(parts, ", ")
}

// --- request --------------------------------------------------------------------------------

func (c *DescribeCmd) request(ctx context.Context, user *client.Client) error {
	ns := c.Namespace
	if ns == "" {
		ns = api.DefaultNamespace
	}
	request, err := user.Request(ctx, api.RequestID{Namespace: ns, Name: c.Name})
	if err != nil {
		return err
	}
	return c.render(request, func() { printRequest(*request) })
}

// --- namespace ------------------------------------------------------------------------------

// namespace describes one request partition (§9.3).
//
// It lists the requests in it as well as its rules, because the two questions an operator has
// about a namespace are "what does `paths` say" and "what will stop me deleting this".
func (c *DescribeCmd) namespace(ctx context.Context, user *client.Client) error {
	info, err := user.Namespace(ctx, c.Name)
	if err != nil {
		return err
	}
	requests, err := user.Requests(ctx, c.Name)
	if err != nil {
		return err
	}
	return c.render(info, func() { printNamespace(*info, requests) })
}

func printNamespace(info api.NamespaceInfo, requests []api.Request) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Namespace %s\n", info.Name)
	fmt.Fprintf(out, "  paths\t%s\n", pathPolicyText(info.Paths))
	if info.Description != "" {
		fmt.Fprintf(out, "  description\t%s\n", info.Description)
	}
	if info.Name == api.DefaultNamespace {
		fmt.Fprintf(out, "  note\tthe default namespace; it cannot be deleted\n")
	}

	fmt.Fprintln(out)
	if len(requests) == 0 {
		fmt.Fprintln(out, "  no requests")
		return
	}
	fmt.Fprintln(out, "  REQUEST\tSOURCE\tSTATE\tPATHS")
	for _, request := range requests {
		fmt.Fprintf(out, "  %s\t%s/%s\t%s\t%d\n",
			request.Name, request.Source.Node, request.Source.Domain,
			request.Status.State, len(request.Status.Paths))
	}
}

// pathPolicyText spells out what the policy means, because the word alone does not say which way
// round it is.
func pathPolicyText(policy api.PathPolicy) string {
	if policy.Exclusive() {
		return "exclusive — two requests here may not hold one path"
	}
	return "shared — requests here may share a path (the default)"
}

func printRequest(request api.Request) {
	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Request   %s\n", request.ID)
	fmt.Fprintf(out, "  namespace\t%s\n", request.NamespaceOrDefault())
	fmt.Fprintf(out, "  source\t%s/%s %s\n", request.Source.Node, request.Source.Domain, selectorText(request.Source.Select))
	fmt.Fprintf(out, "  created\t%s (%s ago)\n", request.CreatedAt.Format(time.RFC3339), since(request.CreatedAt))
	if !request.Provider.IsEmpty() {
		fmt.Fprintf(out, "  provider\t%s (pinned)\n", providerText(request.Provider))
	}
	if request.IdleTeardown != nil {
		teardown := "never — workers stay hot"
		if request.IdleTeardown.Duration() > 0 {
			teardown = request.IdleTeardown.Duration().String()
		}
		fmt.Fprintf(out, "  idle teardown\t%s\n", teardown)
	}
	if request.SchedPrio != nil {
		fmt.Fprintf(out, "  sched_prio\t%d\n", *request.SchedPrio)
	}
	if len(request.Labels) > 0 {
		// A line each rather than the joined `key=value,…` cell the tables use. Both the key and
		// the value are free text, so the joined form is where a value containing a comma or an
		// equals sign stops being readable, and this view has the room.
		fmt.Fprintln(out, "  labels")
		for _, key := range sortedKeys(request.Labels) {
			fmt.Fprintf(out, "    %s\t%s\n", key, request.Labels[key])
		}
	}

	heading(out, "  Destinations")
	fmt.Fprintln(out, "  NODE/DOMAIN\tPROVIDER")
	for _, dst := range request.Destinations {
		pin := "inherited"
		if !dst.Provider.IsEmpty() {
			pin = providerText(dst.Provider)
		}
		fmt.Fprintf(out, "  %s\t%s\n", dst.Endpoint(), pin)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  state\t%s %s\n", request.Status.State, pathSummary(request.Status))
	if request.Status.Reason != "" {
		fmt.Fprintf(out, "  reason\t%s\n", request.Status.Reason)
	}

	// A request owns a *set* of paths, including in the pinned-flow case. "1 of 3 active" is the
	// answer an operator needs and it has no meaning in a one-flow-per-request model (§9.1).
	fmt.Fprintln(out)
	if len(request.Status.Paths) == 0 {
		fmt.Fprintln(out, "  no paths")
		return
	}
	fmt.Fprintln(out, "  PATH\tFLOW\tDESTINATION\tSTATE\tREASON")
	for _, path := range request.Status.Paths {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\t%s\n",
			path.ID, path.Source.Flow, path.Destination.Endpoint(), path.State, path.Reason)
	}

	printExclusions(out, request.Status)
}

// printExclusions renders what the expansion deliberately left out (§9.1).
//
// **Not decoration.** A path that does not exist has no status to carry a reason, so a flow a
// selector skipped is invisible in a paths-only rendering — and §10.7's self-output rule skips
// flows deliberately, on a node that is also a replication destination, which is precisely where
// an operator's broad selector will meet it.
func printExclusions(out *tabwriter.Writer, status api.RequestStatus) {
	if len(status.Excluded) == 0 && status.ExcludedDropped == 0 {
		return
	}

	heading(out, "  Excluded")
	for _, excluded := range status.Excluded {
		fmt.Fprintf(out, "  %s/%s\t%s\t%s\n",
			excluded.Node, excluded.Domain, excluded.Flow, exclusionText(excluded.Reason))
	}
	if status.ExcludedDropped > 0 {
		// A silent cap reads as "nothing else was excluded", which is the one thing this list must
		// not say when it is untrue.
		fmt.Fprintf(out, "  \t\t(and %d more)\n", status.ExcludedDropped)
	}
}

func exclusionText(reason api.ExclusionReason) string {
	switch reason {
	case api.ExclusionSelfOutput:
		return "this node writes it: a label selector never matches its own output"
	default:
		return string(reason)
	}
}

// --- path -----------------------------------------------------------------------------------

func (c *DescribeCmd) path(ctx context.Context, user *client.Client) error {
	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}
	for _, path := range paths.Paths {
		if path.ID == c.Name {
			return c.render(path, func() { printPath(path) })
		}
	}
	return fmt.Errorf("no path %q", c.Name)
}

func printPath(path api.Path) {
	out := table()
	defer flush(out)

	destination := path.Destination.Endpoint()

	fmt.Fprintf(out, "Path      %s\n", path.ID)
	fmt.Fprintf(out, "  source\t%s/%s %s\n", path.Source.Node, path.Source.Domain, path.Source.Flow)
	fmt.Fprintf(out, "  destination\t%s\n", destination)
	fmt.Fprintf(out, "  state\t%s\n", path.State)
	if path.Reason != "" {
		fmt.Fprintf(out, "  reason\t%s\n", path.Reason)
	}

	// The refcount (§3). N requests naming one edge share one path, one session and one worker
	// pair, and the path goes away when the last of them is cancelled — so this is the answer to
	// "what happens if I delete that request".
	fmt.Fprintf(out, "  requests\t%s (refcount %d)\n", strings.Join(path.Requests, ", "), len(path.Requests))

	fmt.Fprintln(out)
	if path.Session == nil {
		fmt.Fprintln(out, "  no session")
		return
	}
	fmt.Fprintf(out, "  Session %s\n", path.Session.ID)
	fmt.Fprintf(out, "    fabric\t%s / %s\n", path.Session.Fabric, path.Session.Interface.Provider)
	fmt.Fprintf(out, "    state\t%s\n", endpointSummary(*path.Session))
}

// --- session --------------------------------------------------------------------------------

func (c *DescribeCmd) session(ctx context.Context, user *client.Client) error {
	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}
	for _, path := range paths.Paths {
		if path.Session != nil && path.Session.ID == c.Name {
			return c.render(path.Session, func() { printSession(path) })
		}
	}
	return fmt.Errorf("no session %q", c.Name)
}

func printSession(path api.Path) {
	session := *path.Session

	out := table()
	defer flush(out)

	fmt.Fprintf(out, "Session   %s\n", session.ID)
	fmt.Fprintf(out, "  path\t%s\n", path.ID)
	fmt.Fprintf(out, "  source\t%s/%s %s\n", path.Source.Node, path.Source.Domain, path.Source.Flow)
	fmt.Fprintf(out, "  destination\t%s\n", path.Destination.Endpoint())
	fmt.Fprintf(out, "  state\t%s\n", path.State)

	// Both ends are given the *same* negotiated config, and it is pinned for the session's
	// lifetime. The library does no negotiation of its own and requires identical values, so this
	// is one value describing two workers rather than one per side (§5.5, §10.3).
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  fabric\t%s\n", session.Fabric)
	fmt.Fprintf(out, "  provider\t%s (pinned)\n", session.Interface.Provider)
	fmt.Fprintf(out, "  interface\t%s\n", capsText(session.Interface.CapFlags, session.Interface.MaxMessageSize))

	// A content hash of the target worker's incarnation, not a counter: it has no ordering, only
	// equality. It changes on every target restart, and that change is what makes the initiator
	// reconnect (§5.2).
	epoch := session.Epoch
	if epoch == "" {
		epoch = "not yet reported"
	}
	fmt.Fprintf(out, "  epoch\t%s\n", epoch)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "  ROLE\tNODE\tSTATE\tENDPOINT\tRESTARTS\tUP\tREASON")
	printEndpoint(out, "target", session.Target)
	printEndpoint(out, "initiator", session.Initiator)
}

func printEndpoint(out *tabwriter.Writer, role string, endpoint *api.SessionEndpoint) {
	if endpoint == nil {
		// No trailing tab: an empty tab-terminated cell is padded to the column width, which is a
		// line of trailing spaces where the reason would have been.
		fmt.Fprintf(out, "  %s\t—\tnot running\t—\t—\t—\n", role)
		return
	}

	address := "—"
	if endpoint.Address != "" {
		address = endpoint.Address
		if endpoint.Service != "" {
			address += ":" + endpoint.Service
		}
	}
	uptime := "—"
	if !endpoint.StartedAt.IsZero() {
		uptime = since(endpoint.StartedAt)
	}
	fmt.Fprintf(out, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\n",
		role, endpoint.Node, endpoint.State, address, endpoint.Restarts, uptime, endpoint.Reason)
}

// --- shared ---------------------------------------------------------------------------------

// heading opens a section: a blank line and a title, neither of which carries a tab. That is what
// ends the preceding column block, so each section's columns are sized to that section alone
// rather than to the widest cell anywhere in the view.
func heading(out *tabwriter.Writer, title string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
}

// render writes the machine formats verbatim from the API types, so a script written against
// `-o json` is written against the documented API rather than against this command.
func (c *DescribeCmd) render(value any, text func()) error {
	return renderAs(c.Output, value, text)
}

func endpointSummary(session api.Session) string {
	parts := make([]string, 0, 2)
	for _, side := range []struct {
		role     string
		endpoint *api.SessionEndpoint
	}{{"target", session.Target}, {"initiator", session.Initiator}} {
		if side.endpoint == nil {
			parts = append(parts, side.role+" not running")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s on %s", side.role, side.endpoint.State, side.endpoint.Node))
	}
	return strings.Join(parts, ", ")
}

func capFlags(flags []api.CapFlag) string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, string(flag))
	}
	if len(names) == 0 {
		return "no capabilities"
	}
	return strings.Join(names, ",")
}

// capsText is the one-cell form, for the session view's single `interface` line. Where there is
// room for a line per field — the node's attachments — the two facts are printed separately.
func capsText(flags []api.CapFlag, maxMessage uint64) string {
	caps := capFlags(flags)
	if maxMessage == 0 {
		return caps
	}
	return fmt.Sprintf("%s  max message %s", caps, byteSize(maxMessage))
}

// byteSize renders max_message_size, which is a genuine uint64 — providers do report UINT64_MAX,
// and printing that as a number is less use than saying so.
func byteSize(bytes uint64) string {
	const unlimited = ^uint64(0)
	if bytes == unlimited {
		return "unlimited"
	}
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func providerText(pin api.ProviderPin) string {
	names := make([]string, 0, len(pin))
	for _, provider := range pin {
		names = append(names, string(provider))
	}
	return strings.Join(names, " > ")
}

// grantText renders an area's two bits the way the flag spells them, so the output and the input
// read alike.
func grantText(area api.Area) string {
	switch {
	case area.Read && area.Write:
		return "read+write"
	case area.Write:
		return "write"
	default:
		return "read"
	}
}

func nodeNames(nodes []api.Node) string {
	if len(nodes) == 0 {
		return "no registered nodes"
	}
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return strings.Join(names, ", ")
}

// since renders an age coarsely. Seconds matter for a worker that keeps restarting; nothing above
// an hour needs more than whole hours.
func since(at time.Time) string {
	elapsed := time.Since(at)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	}
}
