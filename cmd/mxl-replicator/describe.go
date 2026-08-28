package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
//	flow      a UUID. Unique to the media, *not* to a location: after replication the same
//	          flow exists on both nodes, so this lists every place it is
//	request   durable user intent, and what its selector currently expands to
//	path      the deduplicated edge a request expands onto, and its refcount
//	session   the concrete worker pair realising a path: epoch, negotiated fabric, both ends
//
// Path and session stay separate even though they are 1:1 in practice, because they are separate
// layers (§4): a path is derived state that outlives any particular session, and a session is
// ephemeral and is re-established whenever either end restarts. Collapsing them would suggest a
// path dies when its workers do, which is exactly the thing this design is built not to do.
//
// `status` is the overview; this is the detail. There is deliberately no third way to see either.
type DescribeCmd struct {
	Kind string `arg:"" enum:"node,flow,request,path,session" help:"One of: node, flow, request, path, session."`
	Name string `arg:"" help:"The node name, flow id, request name, path id or session id."`

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
	case "flow":
		return c.flow(ctx, api)
	case "request":
		return c.request(ctx, api)
	case "path":
		return c.path(ctx, api)
	case "session":
		return c.session(ctx, api)
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
		return fmt.Errorf("no node %q; the fleet has %s", c.Name, nodeNames(nodes))
	}

	// The paths touching this node are what an operator is usually actually after, and they are
	// not part of the node object — a node knows what it can do, the control plane knows what it
	// is doing.
	paths, err := user.Paths(ctx)
	if err != nil {
		return err
	}

	return c.render(node, func() { printNode(*node, paths.Paths) })
}

func printNode(node api.Node, paths []api.Path) {
	fmt.Printf("Node      %s\n", node.Name)

	// Registration is durable and survives the agent being down; only the lease goes away. An
	// expired lease is not proof that this node's workers stopped, which is why the two are
	// reported separately rather than as one "up" (§4.2, §7.1).
	live := "no lease"
	if node.Live {
		live = "leased by " + node.Instance
	}
	fmt.Printf("  liveness      %s\n", live)
	if !node.RegisteredAt.IsZero() {
		fmt.Printf("  registered    %s\n", node.RegisteredAt.Format(time.RFC3339))
	}
	if !node.LastSeen.IsZero() {
		fmt.Printf("  last seen     %s (%s ago)\n", node.LastSeen.Format(time.RFC3339), since(node.LastSeen))
	}

	versions := node.Capabilities.Versions
	fmt.Printf("  versions      replicator %s, protocol %d\n", versions.Replicator, versions.Protocol)
	if versions.MXL != "" || versions.Libfabric != "" {
		// The mxl version is the non-obvious one: target_info is produced by one node's
		// mxl-fabrics and consumed by another's, so a pair straddling a version boundary is a
		// compatibility concern neither agent can see alone (§10.2).
		fmt.Printf("                worker %s, mxl %s, libfabric %s\n", versions.Proxy, versions.MXL, versions.Libfabric)
	}
	fmt.Printf("  sched_prio    %t\n", node.Capabilities.SchedPrio)
	if node.Capabilities.PortRange != "" {
		fmt.Printf("  port range    %s (inbound)\n", node.Capabilities.PortRange)
	}

	fmt.Println("\n  Input domains")
	if len(node.Domains) == 0 {
		fmt.Println("    none")
	}
	for _, domain := range node.Domains {
		origin := "configured"
		if !domain.Configured {
			origin = "discovered"
		}
		fmt.Printf("    %-24s %-10s %s\n", domain.Name, origin, domain.Path)
	}

	// The whole of the authority the API has over this node's filesystem (§10.6, §13). A node
	// with none is not a replication destination at all, and saying so plainly here saves an
	// operator working it out from an INVALID request later.
	fmt.Println("\n  Output roots")
	if len(node.Capabilities.OutputRoots) == 0 {
		fmt.Println("    none — not a replication destination")
	}
	for _, root := range node.Capabilities.OutputRoots {
		fmt.Printf("    %-24s %s\n", root.Name, root.Path)
	}

	fmt.Println("\n  Fabric attachments")
	if len(node.Capabilities.Fabrics) == 0 {
		fmt.Println("    none")
	}
	for _, fabric := range node.Capabilities.Fabrics {
		fmt.Printf("    %-8s %-18s %-20s %s\n", fabric.Provider, fabric.Fabric, fabric.Address, capsText(fabric.CapFlags, fabric.MaxMessageSize))
	}

	printNodePaths(node.Name, paths)
}

func printNodePaths(name string, paths []api.Path) {
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

	fmt.Println()
	if len(rows) == 0 {
		fmt.Println("  No paths touch this node.")
		return
	}

	out := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(out, "  ROLE\tPATH\tFLOW\tPEER\tSTATE")
	for _, row := range rows {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\t%s\n", row[0], row[1], row[2], row[3], row[4])
	}
	_ = out.Flush()
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
	fmt.Printf("Flow      %s\n", id)

	if hint := firstGroupHint(entries); hint != nil {
		fmt.Printf("  group hint    %q", hint.Name)
		if hint.Type != "" {
			fmt.Printf(" (%s)", hint.Type)
		}
		fmt.Println()
	}
	if summary := describeDefinition(entries[0].Definition); summary != "" {
		fmt.Printf("  definition    %s\n", summary)
	}

	fmt.Printf("\n  Locations\n")
	out := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
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
	_ = out.Flush()

	var carrying []api.Path
	for _, path := range paths {
		if path.Source.Flow == id {
			carrying = append(carrying, path)
		}
	}

	fmt.Println()
	if len(carrying) == 0 {
		fmt.Println("  No path replicates this flow.")
		return
	}
	out = tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(out, "  PATH\tFROM\tTO\tSTATE")
	for _, path := range carrying {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\n",
			path.ID, path.Source.Node+"/"+path.Source.Domain, path.Destination.Endpoint(), path.State)
	}
	_ = out.Flush()
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
	request, err := user.Request(ctx, c.Name)
	if err != nil {
		return err
	}
	return c.render(request, func() { printRequest(*request) })
}

func printRequest(request api.Request) {
	fmt.Printf("Request   %s\n", request.Name)
	fmt.Printf("  source        %s/%s %s\n", request.Source.Node, request.Source.Domain, selectorText(request.Source.Select))
	fmt.Printf("  created       %s (%s ago)\n", request.CreatedAt.Format(time.RFC3339), since(request.CreatedAt))
	if !request.Provider.IsEmpty() {
		fmt.Printf("  provider      %s (pinned)\n", providerText(request.Provider))
	}
	if request.IdleTeardown != nil {
		teardown := "never — workers stay hot"
		if request.IdleTeardown.Duration() > 0 {
			teardown = request.IdleTeardown.Duration().String()
		}
		fmt.Printf("  idle teardown %s\n", teardown)
	}
	if request.SchedPrio != nil {
		fmt.Printf("  sched_prio    %d\n", *request.SchedPrio)
	}
	if len(request.Labels) > 0 {
		fmt.Printf("  labels        %s\n", sortedLabels(request.Labels))
	}

	fmt.Println("\n  Destinations")
	out := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(out, "  NODE/DOMAIN\tROOT\tPROVIDER")
	for _, dst := range request.Destinations {
		root := dst.Root
		if root == "" {
			root = "(the node's only one)"
		}
		pin := "inherited"
		if !dst.Provider.IsEmpty() {
			pin = providerText(dst.Provider)
		}
		fmt.Fprintf(out, "  %s\t%s\t%s\n", dst.Endpoint(), root, pin)
	}
	_ = out.Flush()

	fmt.Printf("\n  state         %s %s\n", request.Status.State, pathSummary(request.Status))
	if request.Status.Reason != "" {
		fmt.Printf("  reason        %s\n", request.Status.Reason)
	}

	// A request owns a *set* of paths, including in the pinned-flow case. "1 of 3 active" is the
	// answer an operator needs and it has no meaning in a one-flow-per-request model (§9.1).
	fmt.Println()
	if len(request.Status.Paths) == 0 {
		fmt.Println("  No paths — the selector matches nothing yet.")
		return
	}
	out = tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(out, "  PATH\tFLOW\tDESTINATION\tSTATE\tREASON")
	for _, path := range request.Status.Paths {
		fmt.Fprintf(out, "  %s\t%s\t%s\t%s\t%s\n",
			path.ID, path.Source.Flow, path.Destination.Endpoint(), path.State, path.Reason)
	}
	_ = out.Flush()
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
	fmt.Printf("Path      %s\n", path.ID)
	fmt.Printf("  source        %s/%s %s\n", path.Source.Node, path.Source.Domain, path.Source.Flow)
	fmt.Printf("  destination   %s", path.Destination.Endpoint())
	if path.Destination.Root != "" {
		fmt.Printf(" (root %s)", path.Destination.Root)
	}
	fmt.Println()
	fmt.Printf("  state         %s\n", path.State)
	if path.Reason != "" {
		fmt.Printf("  reason        %s\n", path.Reason)
	}

	// The refcount (§3). N requests naming one edge share one path, one session and one worker
	// pair, and the path goes away when the last of them is cancelled — so this is the answer to
	// "what happens if I delete that request".
	fmt.Printf("  requests      %s (refcount %d)\n", strings.Join(path.Requests, ", "), len(path.Requests))

	fmt.Println()
	if path.Session == nil {
		fmt.Println("  No session — nothing is running on this path yet.")
		return
	}
	fmt.Printf("  Session %s\n", path.Session.ID)
	fmt.Printf("    fabric      %s / %s\n", path.Session.Fabric, path.Session.Interface.Provider)
	fmt.Printf("    state       %s\n", endpointSummary(*path.Session))
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
	return fmt.Errorf("no session %q; it may have been re-established under a new id", c.Name)
}

func printSession(path api.Path) {
	session := *path.Session

	fmt.Printf("Session   %s\n", session.ID)
	fmt.Printf("  path          %s\n", path.ID)
	fmt.Printf("  source        %s/%s %s\n", path.Source.Node, path.Source.Domain, path.Source.Flow)
	fmt.Printf("  destination   %s\n", path.Destination.Endpoint())
	fmt.Printf("  state         %s\n", path.State)

	// Both ends are given the *same* negotiated config, and it is pinned for the session's
	// lifetime. The library does no negotiation of its own and requires identical values, so this
	// is one value describing two workers rather than one per side (§5.5, §10.3).
	fmt.Printf("\n  fabric        %s\n", session.Fabric)
	fmt.Printf("  provider      %s (pinned)\n", session.Interface.Provider)
	fmt.Printf("  interface     %s\n", capsText(session.Interface.CapFlags, session.Interface.MaxMessageSize))

	// A content hash of the target worker's incarnation, not a counter: it has no ordering, only
	// equality. It changes on every target restart, and that change is what makes the initiator
	// reconnect (§5.2).
	epoch := session.Epoch
	if epoch == "" {
		epoch = "not yet reported"
	}
	fmt.Printf("  epoch         %s\n", epoch)

	fmt.Println()
	out := tabwriter.NewWriter(os.Stdout, 2, 0, 2, ' ', 0)
	fmt.Fprintln(out, "  ROLE\tNODE\tSTATE\tENDPOINT\tRESTARTS\tUP\tREASON")
	printEndpoint(out, "target", session.Target)
	printEndpoint(out, "initiator", session.Initiator)
	_ = out.Flush()
}

func printEndpoint(out *tabwriter.Writer, role string, endpoint *api.SessionEndpoint) {
	if endpoint == nil {
		fmt.Fprintf(out, "  %s\t—\tnot running\t—\t—\t—\t\n", role)
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

func capsText(flags []api.CapFlag, maxMessage uint64) string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, string(flag))
	}
	caps := strings.Join(names, ",")
	if caps == "" {
		caps = "no capabilities"
	}
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
