// Package probe joins what libfabric reports against what the operator configured (§10.5).
//
// The agent cannot enumerate fabric interfaces itself — it is Go and does not link the library —
// so the worker's `--interfaces` probe is the ground truth, and running it belongs to
// internal/worker/exec, the only package in the tree that starts a process (invariant 11). What
// is left here is the join, which is a pure function over the probe's output and the `fabrics:`
// block, and which is where M5b's actual work always was.
//
// # Why there are five selectors and why "none" is the common one
//
// §10.1 advises preferring `interface:` over `address:`, justifying it with EFA — link-local,
// hardware-derived addresses that nobody should have to write down. The M0 plan decision
// supersedes that, because the probe reports **no interface name at all**: its only
// interface-ish field is `attr.device_name`, which is the netdev name for tcp (`eth1`) but the
// *libfabric device* name for verbs and efa (`mlx5_0`, `rdmap0s6-rdm`), and nothing for shm. So
// `interface: efa0` cannot be resolved, and the advice is inverted precisely where it argued
// hardest for itself.
//
// What replaces it, in two classes:
//
//	naming        matched against                                          works for
//	address:      the probe's node, exactly                                all providers
//	interface:    the netdev's own addresses, resolved here, vs the node   tcp, verbs
//	device:       the probe's attr.device_name, exactly                    where reported
//	nothing       the provider alone, which must match exactly one entry   the common case
//
//	narrowing     matched against                                          works for
//	network:      the probe's node parsed and tested against a prefix      IP addresses
//	ip_version:   the probe's node parsed, 4 or 6                         IP addresses
//
// The fourth row is what actually resolves efa and shm, and it is better than the name matching
// it replaces rather than a fallback from it: a node has one EFA device and one shm, so
// `{provider: efa, fabric: vpc1-subnet-a}` is unambiguous and puts no hardware-derived string in
// the config file at all.
//
// # Why the narrowing class exists, and why it composes
//
// At most one *naming* selector, still, for the reason it always was: two names would need a rule
// for combining them and every such rule is a worse answer than making the operator say which one
// they meant. But naming a thing and narrowing what counts as that thing are different acts, so
// the narrowing selectors compose — with a naming selector and with each other — and the
// "exactly one survivor" rule applies to the conjunction.
//
// The case that forces it is a DaemonSet, where every selector is a fleet-wide value and
// `address:` is therefore unavailable. `device: mlx5_0` is the fleet-wide string an operator has,
// and it is *ambiguous by construction*: an HCA with both an IPv4 address and a link-local IPv6
// one reports two probe entries under one device name, and the attachment is dropped. Before
// this, the only fix was a per-node `address:` — an overlay per node to disambiguate a fact
// ("we use v4") that is true of the whole fleet. `device: mlx5_0` + `ip_version: 4` says it once.
//
// `network:` is the same argument one step further, and it is the one that needs no per-node
// value *and* no hardware-derived string: `{provider: tcp, fabric: dc1-data, network: 10.1.0.0/16}`
// picks each node's address on the storage network without naming a device, an interface or an
// address. On a fleet whose nodes are alike but not identically named — `eth1` here, `ens5f0`
// there — it is the only selector that is simultaneously exact and uniform.
//
// Neither says anything about reachability, and neither is allowed to: §10.1's whole argument is
// that two nodes on one prefix may still have no route between them. `network:` picks an address
// out of a list; the fabric label is what asserts the two ends can talk.
//
// # Failing legibly
//
// Two failures, both loud, both dropping the attachment rather than guessing: no match at all,
// and an ambiguous match. The second logs every candidate, which hands the operator the exact
// strings they could have written — a better answer than "no match" to the question §10.5 poses,
// which is whether this node has no verbs or whether someone typo'd `ib0`.
package probe

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

// Attachment is one entry of the operator's `fabrics:` block: a (provider, fabric) pair plus the
// selectors saying which of the node's fabric interfaces it means (§10.1).
type Attachment struct {
	Provider api.Provider `yaml:"provider" json:"provider"`

	// Fabric is an operator-assigned opaque label. Two nodes may pair on a provider **iff** they
	// share a label for it, and the server does nothing with it but string equality — no topology
	// database, no reachability probing, no inference. That matches how these networks are
	// provisioned: the operator already knows which HCA is on which fabric, and nothing else does.
	//
	// Required, except for shm, whose label is derived from the node name so that same-node-only
	// falls out of ordinary fabric matching with no special case.
	Fabric string `yaml:"fabric" json:"fabric"`

	// Address selects a probe entry by its reported address, exactly. Works for every provider,
	// and is the escape hatch when a node genuinely has two of something.
	Address string `yaml:"address" json:"address"`

	// Interface selects by netdev name — `eth1`, `ib0` — resolved here against the addresses the
	// kernel reports for it. Works for tcp and verbs; see the package comment for why it cannot
	// work for efa.
	Interface string `yaml:"interface" json:"interface"`

	// Device selects by the probe's attr.device_name. Note this is *not* a netdev name in
	// general: it is one for tcp, and the libfabric device name for verbs and efa.
	Device string `yaml:"device" json:"device"`

	// Network narrows to entries whose reported address falls inside a CIDR prefix — the one
	// selector that is exact without being per-node, and therefore the one a DaemonSet wants.
	// Combines with a naming selector and with IPVersion.
	//
	// An entry whose address is not an IP at all — shm reports a hostname — never matches.
	Network string `yaml:"network" json:"network"`

	// IPVersion narrows to entries whose reported address is IPv4 (4) or IPv6 (6). Zero is unset.
	//
	// This exists for the ambiguity `device:` cannot resolve on its own: one HCA with a v4 address
	// and a link-local v6 one is two probe entries under one device name, and which of them the
	// operator means is a fleet-wide fact rather than a per-node one.
	IPVersion int `yaml:"ip_version" json:"ip_version"`
}

// Validate checks one configured attachment in isolation.
//
// At most one *naming* selector, because two names would need a rule for combining them and every
// such rule is a worse answer than making the operator say which one they meant. The narrowing
// selectors compose freely; see the package comment.
func (a Attachment) Validate() error {
	if a.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if !api.KnownProvider(a.Provider) {
		return fmt.Errorf("provider %q is not one this project can negotiate (known: %s)",
			a.Provider, providerList())
	}
	if a.Fabric == "" && a.Provider != api.ProviderSHM {
		return fmt.Errorf("fabric label is required for provider %s", a.Provider)
	}

	var set []string
	for name, value := range map[string]string{"address": a.Address, "interface": a.Interface, "device": a.Device} {
		if value != "" {
			set = append(set, name)
		}
	}
	if len(set) > 1 {
		slices.Sort(set)
		return fmt.Errorf("at most one of address, interface or device may be set, got %s", strings.Join(set, " and "))
	}
	if a.Interface != "" && a.Provider == api.ProviderEFA {
		// Not merely unsupported: the probe has no netdev name for efa to match against — the
		// library reports a device name, `rdmap0s6-rdm`-style — so this would always drop the
		// attachment at startup with a confusing "no match" rather than here with the reason.
		return fmt.Errorf("interface: an efa attachment is selected by device, not by netdev name")
	}

	if a.IPVersion != 0 && a.IPVersion != 4 && a.IPVersion != 6 {
		return fmt.Errorf("ip_version: want 4 or 6, got %d", a.IPVersion)
	}
	if a.Network != "" {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(a.Network))
		if err != nil {
			return fmt.Errorf("network %q: not a CIDR prefix (want e.g. 10.1.0.0/16 or fd00:1::/64)", a.Network)
		}
		// A prefix already names a family, so the only thing `ip_version` can add here is a
		// contradiction — and a contradiction matches nothing on any node, which would present as
		// a drop on every node in the fleet rather than as the configuration error it is.
		if version := networkVersion(prefix); a.IPVersion != 0 && a.IPVersion != version {
			return fmt.Errorf("network %s is IPv%d and contradicts ip_version: %d", a.Network, version, a.IPVersion)
		}
	}
	return nil
}

func networkVersion(prefix netip.Prefix) int {
	if prefix.Addr().Unmap().Is4() {
		return 4
	}
	return 6
}

func (a Attachment) String() string {
	labels := a.selectorLabels()
	if len(labels) == 0 {
		return fmt.Sprintf("%s on fabric %q", a.Provider, a.Fabric)
	}
	return fmt.Sprintf("%s on fabric %q (%s)", a.Provider, a.Fabric, strings.Join(labels, ", "))
}

// selectorLabels renders the configured selectors, in a fixed order, for a log line or a drop
// reason. Naming selector first, because it is the one the operator thinks of as *the* selector.
func (a Attachment) selectorLabels() []string {
	var labels []string
	switch {
	case a.Address != "":
		labels = append(labels, fmt.Sprintf("address: %s", a.Address))
	case a.Interface != "":
		labels = append(labels, fmt.Sprintf("interface: %s", a.Interface))
	case a.Device != "":
		labels = append(labels, fmt.Sprintf("device: %s", a.Device))
	}
	if a.Network != "" {
		labels = append(labels, fmt.Sprintf("network: %s", a.Network))
	}
	if a.IPVersion != 0 {
		labels = append(labels, fmt.Sprintf("ip_version: %d", a.IPVersion))
	}
	return labels
}

// Options configures [Join].
type Options struct {
	// Node is this agent's node name, used to derive the shm fabric label (§10.1).
	Node string

	// Interfaces resolves a netdev name to the addresses the kernel reports for it. Nil uses the
	// host's own; tests replace it.
	Interfaces func(name string) ([]string, error)

	Logger *slog.Logger
}

// Dropped is a configured attachment that did not survive the join, with why.
type Dropped struct {
	Attachment Attachment
	Reason     string

	// Candidates is every probe entry for the attachment's provider, rendered. Empty when the
	// library reported none at all, which is the "this node has no verbs" half of §10.5's
	// distinction; non-empty it is the "someone typo'd ib0" half, and it lists the strings the
	// operator could have written instead.
	Candidates []string
}

// Result is what a join produced.
type Result struct {
	// Attachments is what the node may advertise: present in the configured block *and* in the
	// probe. Only these go over the wire (§10.2).
	Attachments []api.FabricAttachment

	// Dropped is everything configured that did not survive, in configuration order.
	Dropped []Dropped
}

// Join reconciles the configured attachments against the probe's output.
//
// An attachment survives only if it appears in both, which is what makes [api.FabricAttachment]
// mean "verified" rather than "hoped for" (§10.2). A dropped attachment is logged at error level
// — a configuration error must be loud at startup rather than silently absent from a
// negotiation that then fails with `no_shared_fabric` on a completely different node.
//
// It never fails as a whole. A node with one good attachment and one typo should replicate over
// the good one, and refusing to start would convert a partial misconfiguration into an outage.
func Join(configured []Attachment, probed []exec.Interface, opts Options) Result {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	resolve := opts.Interfaces
	if resolve == nil {
		resolve = NetdevAddresses
	}

	var result Result
	for _, attachment := range configured {
		candidates := providerEntries(probed, attachment.Provider)

		matched, reason := match(attachment, candidates, resolve)
		if matched == nil {
			dropped := Dropped{
				Attachment: attachment,
				Reason:     reason,
				Candidates: render(candidates),
			}
			result.Dropped = append(result.Dropped, dropped)
			log.Error("dropping a configured fabric attachment",
				"attachment", attachment.String(),
				"reason", reason,
				"candidates", dropped.Candidates)
			continue
		}

		fabric := attachment.Fabric
		if attachment.Provider == api.ProviderSHM {
			// Derived, not configured. shm is structurally same-node-only and the derivation is
			// what makes that fall out of ordinary fabric matching; the server canonicalises what
			// it stores from the same function, so the two cannot disagree (§10.1).
			fabric = api.SHMFabric(opts.Node)
		}

		result.Attachments = append(result.Attachments, api.FabricAttachment{
			Provider:       attachment.Provider,
			Fabric:         fabric,
			Address:        matched.Address,
			CapFlags:       slices.Clone(matched.Caps.Flags),
			MaxMessageSize: matched.Caps.MaxMessageSize,
			Device:         matched.Device(),
		})
	}

	return result
}

// match applies the attachment's selectors to the probe entries for its provider.
//
// Every configured selector is a predicate and they are conjoined: an entry survives only if it
// satisfies all of them, and exactly one entry must survive. That is the same rule the single
// selector always had, applied to a set — so `device: mlx5_0` on a device with two addresses is
// still ambiguous, and `device: mlx5_0, ip_version: 4` is not.
func match(attachment Attachment, candidates []exec.Interface, resolve func(string) ([]string, error)) (*exec.Interface, string) {
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("libfabric reports no %s interface on this node at all", attachment.Provider)
	}

	predicates, reason := attachment.selectors(resolve)
	if reason != "" {
		return nil, reason
	}

	// No predicates is not a wildcard: it means "this node has exactly one of these, and I am not
	// going to write down a hardware-derived string to say which". Ambiguity is therefore a real
	// error rather than a reason to pick the first.
	matches := candidates
	for _, predicate := range predicates {
		matches = filter(matches, predicate.keep)
	}

	switch len(matches) {
	case 1:
		return &matches[0], ""
	case 0:
		if len(predicates) == 0 {
			// Unreachable — candidates is non-empty and no predicate takes all of them — but
			// spelled out rather than left to fall through to a confusing message.
			return nil, "no candidates"
		}
		return nil, fmt.Sprintf("no %s interface matches %s", attachment.Provider, describe(predicates))
	default:
		if len(predicates) == 0 {
			return nil, fmt.Sprintf("this node has %d %s interfaces and the attachment has no address:, interface:, device:, network: or ip_version: selector",
				len(matches), attachment.Provider)
		}
		return nil, fmt.Sprintf("%s matches %d %s interfaces", describe(predicates), len(matches), attachment.Provider)
	}
}

// selector is one configured selector compiled into a predicate over probe entries.
type selector struct {
	// label is how the selector is named in a message — `device: mlx5_0` — so a drop reason
	// quotes the operator's own configuration back at them.
	label string
	keep  func(exec.Interface) bool
}

// selectors compiles the attachment's selectors, or returns the reason one could not be compiled.
//
// The `interface:` resolution is the only one that can fail, and it fails here rather than
// producing a predicate that matches nothing: "no such network interface" and "no verbs interface
// matches interface: ib0" are different problems and an operator needs to be told which one.
func (a Attachment) selectors(resolve func(string) ([]string, error)) ([]selector, string) {
	labels := a.selectorLabels()
	if len(labels) == 0 {
		return nil, ""
	}

	// The pushes below run in the same order selectorLabels emits, which is what lets each take
	// the next label rather than re-deriving its own.
	out := make([]selector, 0, len(labels))
	push := func(keep func(exec.Interface) bool) {
		out = append(out, selector{label: labels[len(out)], keep: keep})
	}

	switch {
	case a.Address != "":
		push(func(entry exec.Interface) bool { return sameAddress(entry.Address, a.Address) })
	case a.Interface != "":
		addresses, err := resolve(a.Interface)
		if err != nil {
			return nil, fmt.Sprintf("interface %q: %s", a.Interface, err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Sprintf("interface %q has no addresses", a.Interface)
		}
		push(func(entry exec.Interface) bool {
			return slices.ContainsFunc(addresses, func(address string) bool {
				return sameAddress(entry.Address, address)
			})
		})
	case a.Device != "":
		push(func(entry exec.Interface) bool { return entry.Device() == a.Device })
	}

	if a.Network != "" {
		// Validate rejects a prefix that does not parse, so this cannot fail for an attachment
		// that went through it; a zero Prefix contains nothing, which is the safe answer for one
		// that did not.
		prefix, _ := netip.ParsePrefix(strings.TrimSpace(a.Network))
		prefix = prefix.Masked()
		push(func(entry exec.Interface) bool {
			address, ok := entryAddress(entry)
			return ok && prefix.Contains(address)
		})
	}
	if a.IPVersion != 0 {
		push(func(entry exec.Interface) bool {
			address, ok := entryAddress(entry)
			if !ok {
				return false
			}
			if a.IPVersion == 4 {
				return address.Is4()
			}
			return address.Is6()
		})
	}

	return out, ""
}

func describe(selectors []selector) string {
	labels := make([]string, 0, len(selectors))
	for _, s := range selectors {
		labels = append(labels, s.label)
	}
	return strings.Join(labels, " and ")
}

// entryAddress parses a probe entry's reported address as an IP, if it is one.
//
// It need not be: shm reports the hostname, and a provider is free to report a device address in
// any shape it likes. Not being one is not an error — it simply cannot be inside a prefix or be
// version 4 or 6, so the narrowing selectors exclude it. Unmapped, so that a v4-mapped v6 address
// answers `ip_version: 4` and falls inside a v4 prefix, which is what an operator looking at
// `::ffff:10.1.0.7` means by both.
func entryAddress(entry exec.Interface) (netip.Addr, bool) {
	address, err := netip.ParseAddr(zoneless(strings.TrimSpace(entry.Address)))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func providerEntries(probed []exec.Interface, provider api.Provider) []exec.Interface {
	return filter(probed, func(entry exec.Interface) bool { return entry.Provider == provider })
}

func filter(entries []exec.Interface, keep func(exec.Interface) bool) []exec.Interface {
	out := make([]exec.Interface, 0, len(entries))
	for _, entry := range entries {
		if keep(entry) {
			out = append(out, entry)
		}
	}
	return out
}

// render describes probe entries for an operator, so that a drop message carries the strings that
// would have worked.
//
// One entry holds no comma and no space outside its parentheses, because this is logged as a
// string slice and a structured logger separates the elements with a space: `10.0.0.1 (device
// eth0)` survives that, where an earlier `address: 10.0.0.1, device: eth0` ran two candidates
// together into something an operator had to parse by eye — on exactly the ambiguous-device
// message that is the most common reason to be reading this list at all.
func render(entries []exec.Interface) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		text := entry.Address
		if device := entry.Device(); device != "" {
			text += fmt.Sprintf(" (device %s)", device)
		}
		out = append(out, text)
	}
	return out
}

// sameAddress compares two fabric addresses.
//
// Textual equality is not enough on its own: the same IPv6 address has several spellings, and a
// link-local one may or may not carry a `%zone` suffix depending on which side produced it — and
// efa's addresses are exactly the link-local, hardware-derived kind. Parsed comparison where both
// sides parse, exact otherwise, since a device address need not be an IP at all.
func sameAddress(left, right string) bool {
	l, r := strings.TrimSpace(left), strings.TrimSpace(right)
	if l == r {
		return true
	}

	lip := net.ParseIP(zoneless(l))
	rip := net.ParseIP(zoneless(r))
	if lip == nil || rip == nil {
		return false
	}
	return lip.Equal(rip)
}

func zoneless(address string) string {
	if index := strings.IndexByte(address, '%'); index >= 0 {
		return address[:index]
	}
	return address
}

// NetdevAddresses returns the addresses the kernel reports for a network interface.
//
// This is the whole of the `interface:` selector: the resolution happens agent-side, before
// registration, so an interface name never reaches the wire and the server only ever sees an
// address the library itself reported (§10.1, [api.FabricAttachment.Address]).
func NetdevAddresses(name string) ([]string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("no such network interface: %w", err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("read addresses: %w", err)
	}

	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		switch typed := addr.(type) {
		case *net.IPNet:
			out = append(out, typed.IP.String())
		case *net.IPAddr:
			out = append(out, typed.IP.String())
		default:
			out = append(out, addr.String())
		}
	}
	return out, nil
}

func providerList() string {
	names := make([]string, 0, len(api.DefaultProviderOrder))
	for _, provider := range api.DefaultProviderOrder {
		names = append(names, string(provider))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
