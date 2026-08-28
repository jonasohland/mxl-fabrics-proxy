// Package negotiate agrees the interface configuration for one session (§10.3).
//
// The library performs **no** negotiation of its own. From the Fabrics developer guide: "Both
// sides (target and initiator) must receive the same capabilities and maximum message size to
// be compatible. There is no internal negotiation. Typically you would serialize the selected
// mxlFabricsInterfaceConfig alongside the mxlFabricsTargetInfo and share both with the
// initiator through your out-of-band signalling channel."
//
// This project is that out-of-band channel, so negotiation is the server's job — and it is not
// just picking a provider name. The full interface config has to be agreed and then given to
// *both* ends (§5.5, invariant 8).
//
// Everything here is a pure function. No store, no HTTP, no clock: negotiation is decided by
// what two nodes advertised and what the request pinned, and nothing else.
package negotiate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Result is an agreed configuration for one session.
type Result struct {
	// Fabric is the shared fabric label. It travels in the assignment because the provider
	// alone does not identify a local bind address: a node can hold two verbs attachments on
	// different InfiniBand fabrics, and binding the wrong one produces a target that comes up
	// perfectly and an initiator that never connects (§10.1).
	Fabric string

	// Interface is what both workers are configured with, byte for byte.
	Interface api.InterfaceConfig
}

// Provider is the negotiated provider, a shorthand for Interface.Provider.
func (r Result) Provider() api.Provider { return r.Interface.Provider }

// Error is a negotiation that produced no viable pair.
//
// The reason code matters as much as the message: the three ways this can fail are three
// different operator problems — "these nodes are not on a common network", "they are, but they
// do not offer the same provider on it", and "they do, but the provider cannot move data
// between them" — and §10.3 requires the API to distinguish them.
type Error struct {
	Code    api.ReasonCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Config is the server's negotiation policy.
type Config struct {
	// Order is the provider preference used when a request pins nothing. Empty means
	// [api.DefaultProviderOrder] (EFA > Verbs > TCP > SHM).
	//
	// A provider absent from this list is still negotiable — it simply sorts last. Dropping it
	// would make an operator's shortened order a silent deny-list, and the mechanism for
	// refusing a provider is the request's pin, not this.
	Order []api.Provider
}

// candidate is one (provider, fabric) pair offered by both nodes.
type candidate struct {
	provider api.Provider
	fabric   string
	src, dst api.FabricAttachment
}

// Negotiate agrees a configuration for a session between two nodes' advertised attachments
// (§10.3).
//
// The steps, in order, because the order is what makes the failure codes meaningful:
//
//  1. Candidates are attachment pairs sharing a (provider, fabric). A shared provider on
//     different fabrics is not a candidate — provider availability is not reachability.
//  2. The pin is applied. An explicitly requested provider is honoured or the request fails;
//     it is **never** substituted, because silently landing on tcp when verbs was asked for is
//     a performance cliff whose dropped grains look like a source problem rather than like a
//     routing decision made on the operator's behalf (§10.4, invariant 7).
//  3. Capabilities are intersected and maxMessageSize minimised. At least one of REMOTE_WRITE
//     or SEND_RECEIVE must survive, or that candidate cannot move data.
//  4. The surviving candidates are ranked, and the first is taken.
//
// The result is deterministic for a given input: two replicas, or the same replica on two
// reconciles, must agree, or an assignment would flap between them.
func Negotiate(src, dst []api.FabricAttachment, pin api.ProviderPin, cfg Config) (Result, error) {
	candidates := pairs(src, dst)
	if len(candidates) == 0 {
		return Result{}, noCandidateError(src, dst)
	}

	if !pin.IsEmpty() {
		allowed := make([]candidate, 0, len(candidates))
		for _, c := range candidates {
			if pin.Allows(c.provider) {
				allowed = append(allowed, c)
			}
		}
		if len(allowed) == 0 {
			return Result{}, &Error{
				Code: api.ReasonPinNotViable,
				Message: fmt.Sprintf("provider pin %s is not viable: the nodes share %s",
					providerList(pin), pairList(candidates)),
			}
		}
		candidates = allowed
	}

	// Rank before intersecting, so the first viable candidate in preference order wins rather
	// than the first that happens to be listed.
	order := cfg.Order
	if len(order) == 0 {
		order = api.DefaultProviderOrder
	}
	if !pin.IsEmpty() {
		// A pin is itself an ordered preference: ["verbs", "tcp"] means verbs if it works and
		// tcp if it does not, so it outranks the server's configured order.
		order = []api.Provider(pin)
	}
	rank(candidates, order)

	for _, c := range candidates {
		iface := api.InterfaceConfig{
			Provider:       c.provider,
			CapFlags:       intersectCaps(c.src.CapFlags, c.dst.CapFlags),
			MaxMessageSize: minMessageSize(c.src.MaxMessageSize, c.dst.MaxMessageSize),
		}
		if !iface.CanTransfer() {
			continue
		}
		return Result{Fabric: c.fabric, Interface: iface}, nil
	}

	return Result{}, &Error{
		Code: api.ReasonNoSharedCapability,
		Message: fmt.Sprintf("no shared transfer capability on %s: neither %s nor %s survives the intersection",
			pairList(candidates), api.CapRemoteWrite, api.CapSendReceive),
	}
}

// pairs returns every (provider, fabric) offered by both sides.
//
// A node may legitimately advertise the same (provider, fabric) more than once — two NICs on
// one fabric, say — so this is a cross product rather than a lookup, and every combination is a
// real candidate.
func pairs(src, dst []api.FabricAttachment) []candidate {
	var out []candidate
	for _, s := range src {
		for _, d := range dst {
			if s.Provider == d.Provider && s.Fabric == d.Fabric {
				out = append(out, candidate{provider: s.Provider, fabric: s.Fabric, src: s, dst: d})
			}
		}
	}
	return out
}

// noCandidateError distinguishes "no fabric in common" from "a fabric in common, but no
// provider on it". They are different operator problems: the first is a network that was never
// connected, the second is a node missing hardware or a driver on a network that is (§10.3).
func noCandidateError(src, dst []api.FabricAttachment) error {
	shared := sharedFabrics(src, dst)
	if len(shared) == 0 {
		return &Error{
			Code: api.ReasonNoSharedFabric,
			Message: fmt.Sprintf("no shared fabric: source offers %s, destination offers %s",
				fabricList(src), fabricList(dst)),
		}
	}
	return &Error{
		Code: api.ReasonNoSharedProvider,
		Message: fmt.Sprintf("no shared provider on fabric %s: source offers %s, destination offers %s",
			quoteList(shared), fabricList(src), fabricList(dst)),
	}
}

func sharedFabrics(src, dst []api.FabricAttachment) []string {
	var out []string
	for _, s := range src {
		for _, d := range dst {
			if s.Fabric == d.Fabric && !slices.Contains(out, s.Fabric) {
				out = append(out, s.Fabric)
			}
		}
	}
	slices.Sort(out)
	return out
}

// rank sorts candidates into the order they should be tried: preference order first, then
// fabric label, then addresses. Everything after the provider is arbitrary but must be
// *stable*, so that two replicas negotiating the same session agree.
func rank(candidates []candidate, order []api.Provider) {
	position := func(p api.Provider) int {
		if i := slices.Index(order, p); i >= 0 {
			return i
		}
		// Unlisted providers sort after listed ones rather than being excluded: a shortened
		// --provider-order is a preference, not a deny-list.
		return len(order)
	}

	slices.SortStableFunc(candidates, func(a, b candidate) int {
		if d := position(a.provider) - position(b.provider); d != 0 {
			return d
		}
		if d := strings.Compare(string(a.provider), string(b.provider)); d != 0 {
			return d
		}
		if d := strings.Compare(a.fabric, b.fabric); d != 0 {
			return d
		}
		if d := strings.Compare(a.src.Address, b.src.Address); d != 0 {
			return d
		}
		return strings.Compare(a.dst.Address, b.dst.Address)
	})
}

// capOrder is the canonical order capability flags are emitted in.
//
// Canonical because the negotiated config is compared — an assignment set is diffed before it
// is written (§7.3) and the agent keys its "already correct" test on the same values — and an
// intersection that came out in a different order on two reconciles would read as a change and
// restart a healthy worker.
var capOrder = []api.CapFlag{api.CapRemoteWrite, api.CapSendReceive, api.CapBlockingOperations}

// intersectCaps returns the flags both sides offer, in canonical order.
//
// Flags this build does not know about are intersected too, and sorted after the known ones. A
// future library capability must not be silently dropped from a config both ends would have
// supported — the rule everywhere in this API is that an unrecognised value is passed through,
// not discarded (§13.1).
func intersectCaps(src, dst []api.CapFlag) []api.CapFlag {
	var both []api.CapFlag
	for _, flag := range src {
		if slices.Contains(dst, flag) && !slices.Contains(both, flag) {
			both = append(both, flag)
		}
	}

	slices.SortStableFunc(both, func(a, b api.CapFlag) int {
		ia, ib := slices.Index(capOrder, a), slices.Index(capOrder, b)
		switch {
		case ia >= 0 && ib >= 0:
			return ia - ib
		case ia >= 0:
			return -1
		case ib >= 0:
			return 1
		default:
			return strings.Compare(string(a), string(b))
		}
	})
	return both
}

// minMessageSize agrees the maximum message size: the smaller of the two, since a message
// larger than either end can handle is one neither can.
//
// Zero means "not reported", not "zero bytes", and is therefore ignored rather than winning the
// minimum — a node whose probe reported nothing must not silently cap a peer that did. Zero on
// both sides leaves the field unset, which hands the decision to the library (with the warning
// that it will be required in a future version).
func minMessageSize(a, b uint64) uint64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}

func fabricList(attachments []api.FabricAttachment) string {
	if len(attachments) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(attachments))
	for _, a := range attachments {
		part := fmt.Sprintf("%s/%s", a.Provider, a.Fabric)
		if !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func pairList(candidates []candidate) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		part := fmt.Sprintf("%s/%s", c.provider, c.fabric)
		if !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func providerList(providers []api.Provider) string {
	parts := make([]string, len(providers))
	for i, p := range providers {
		parts[i] = fmt.Sprintf("%q", p)
	}
	return strings.Join(parts, ", ")
}

func quoteList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(parts, ", ")
}
