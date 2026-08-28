// Package probe joins what libfabric reports against what the operator configured (§10.5).
//
// The agent cannot enumerate fabric interfaces itself — it is Go and does not link the library —
// so the worker's `--interfaces` probe is the ground truth, and running it belongs to
// internal/worker/exec, the only package in the tree that starts a process (invariant 11). What
// is left here is the join, which is a pure function over the probe's output and the `fabrics:`
// block, and which is where M5b's actual work always was.
//
// # Why there are four selectors and why "none" is the common one
//
// §10.1 advises preferring `interface:` over `address:`, justifying it with EFA — link-local,
// hardware-derived addresses that nobody should have to write down. The M0 plan decision
// supersedes that, because the probe reports **no interface name at all**: its only
// interface-ish field is `attr.device_name`, which is the netdev name for tcp (`eth1`) but the
// *libfabric device* name for verbs and efa (`mlx5_0`, `rdmap0s6-rdm`), and nothing for shm. So
// `interface: efa0` cannot be resolved, and the advice is inverted precisely where it argued
// hardest for itself.
//
// What replaces it:
//
//	configured     matched against                                          works for
//	address:       the probe's node, exactly                                all providers
//	interface:     the netdev's own addresses, resolved here, vs the node   tcp, verbs
//	device:        the probe's attr.device_name, exactly                    where reported
//	nothing        the provider alone, which must match exactly one entry   the common case
//
// The last row is what actually resolves efa and shm, and it is better than the name matching it
// replaces rather than a fallback from it: a node has one EFA device and one shm, so
// `{provider: efa, fabric: vpc1-subnet-a}` is unambiguous and puts no hardware-derived string in
// the config file at all.
//
// # Failing legibly
//
// Two failures, both loud, both dropping the attachment rather than guessing: no match at all,
// and an ambiguous selectorless match. The second logs every candidate, which hands the operator
// the exact strings they could have written — a better answer than "no match" to the question
// §10.5 poses, which is whether this node has no verbs or whether someone typo'd `ib0`.
package probe

import (
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

// Attachment is one entry of the operator's `fabrics:` block: a (provider, fabric) pair plus at
// most one selector saying which of the node's fabric interfaces it means (§10.1).
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
}

// Validate checks one configured attachment in isolation.
//
// At most one selector, because two would need a rule for combining them and every such rule is a
// worse answer than making the operator say which one they meant.
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
	return nil
}

// selector returns the configured selector as a (kind, value) pair, or ("", "") for none.
func (a Attachment) selector() (string, string) {
	switch {
	case a.Address != "":
		return "address", a.Address
	case a.Interface != "":
		return "interface", a.Interface
	case a.Device != "":
		return "device", a.Device
	default:
		return "", ""
	}
}

func (a Attachment) String() string {
	kind, value := a.selector()
	if kind == "" {
		return fmt.Sprintf("%s on fabric %q", a.Provider, a.Fabric)
	}
	return fmt.Sprintf("%s on fabric %q (%s: %s)", a.Provider, a.Fabric, kind, value)
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

// match applies the attachment's selector to the probe entries for its provider.
func match(attachment Attachment, candidates []exec.Interface, resolve func(string) ([]string, error)) (*exec.Interface, string) {
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("libfabric reports no %s interface on this node at all", attachment.Provider)
	}

	kind, value := attachment.selector()

	var matches []exec.Interface
	switch kind {
	case "":
		// The selectorless case is not a wildcard: it means "this node has exactly one of these,
		// and I am not going to write down a hardware-derived string to say which". Ambiguity is
		// therefore a real error rather than a reason to pick the first.
		matches = candidates
	case "address":
		matches = filter(candidates, func(entry exec.Interface) bool {
			return sameAddress(entry.Address, value)
		})
	case "device":
		matches = filter(candidates, func(entry exec.Interface) bool {
			return entry.Device() == value
		})
	case "interface":
		addresses, err := resolve(value)
		if err != nil {
			return nil, fmt.Sprintf("interface %q: %s", value, err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Sprintf("interface %q has no addresses", value)
		}
		matches = filter(candidates, func(entry exec.Interface) bool {
			return slices.ContainsFunc(addresses, func(address string) bool {
				return sameAddress(entry.Address, address)
			})
		})
	}

	switch len(matches) {
	case 1:
		return &matches[0], ""
	case 0:
		if kind == "" {
			// Unreachable — candidates is non-empty and the selectorless case takes all of them —
			// but spelled out rather than left to fall through to a confusing message.
			return nil, "no candidates"
		}
		return nil, fmt.Sprintf("no %s interface matches %s %q", attachment.Provider, kind, value)
	default:
		if kind == "" {
			return nil, fmt.Sprintf("this node has %d %s interfaces and the attachment has no address:, interface: or device: selector",
				len(matches), attachment.Provider)
		}
		return nil, fmt.Sprintf("%s %q matches %d %s interfaces", kind, value, len(matches), attachment.Provider)
	}
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
func render(entries []exec.Interface) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		text := fmt.Sprintf("address: %s", entry.Address)
		if device := entry.Device(); device != "" {
			text += fmt.Sprintf(", device: %s", device)
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
