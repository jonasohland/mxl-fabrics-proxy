package probe

import (
	"fmt"
	"net"
	"strings"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker/exec"
)

// DefaultFabric is the label given to an detected attachment (§10.1).
//
// It is a fabric label like any other and the server does nothing with it but string equality,
// so **detection only pairs two nodes that both used it**. That is the whole of the
// mechanism and it is also its whole limitation: two nodes on genuinely different networks that
// both detect will both call theirs `default`, and the server will pair them and produce
// exactly the failure §10.1 exists to prevent — a target that comes up clean and an initiator
// whose connect loop spins. Auto-detection is therefore a convenience for a flat network, not a
// replacement for the operator knowing which HCA is on which fabric.
const DefaultFabric = "default"

// Auto picks one attachment out of what the probe reported, for a node that configured none.
//
// It walks the preference order (§10.4) and takes the first provider that has an entry another
// node could plausibly reach, which makes it the same decision the server's negotiation would
// make if every node offered everything — EFA over Verbs over TCP, and shm only when nothing
// else is there at all. Within a provider it takes the first usable entry in probe order: a node
// with two of something is a node whose operator has a decision to make that this function
// cannot make for them, and §10.1's selectorless-ambiguity rule is the right place to say so.
//
// The result is an ordinary configured [Attachment], not a finished [api.FabricAttachment], so
// it goes through [Join] like anything an operator wrote. Detection produces *configuration*;
// the verification, the capability flags and shm's derived label all stay in one place.
//
// The second return is one line per provider passed over, in order, for the log — the operator
// needs to see that efa was skipped because libfabric reports none, not merely that tcp won. A
// zero Provider means nothing was viable, which is a node that can do nothing and is the
// caller's error to report.
func Detect(probed []exec.Interface, order []api.Provider) (Attachment, []string) {
	if len(order) == 0 {
		order = api.DefaultProviderOrder
	}

	var skipped []string
	for _, provider := range order {
		candidates := providerEntries(probed, provider)
		if len(candidates) == 0 {
			skipped = append(skipped, fmt.Sprintf("%s: libfabric reports none on this node", provider))
			continue
		}

		usable := filter(candidates, func(entry exec.Interface) bool {
			return reachable(provider, entry.Address)
		})
		if len(usable) == 0 {
			skipped = append(skipped, fmt.Sprintf("%s: none of the %d reported has an address another node could reach (%s)",
				provider, len(candidates), strings.Join(render(candidates), "; ")))
			continue
		}

		// Selected by address rather than by device, because address is the one selector every
		// provider supports (§10.1) and it is the field the probe is guaranteed to report. If a
		// provider ever reports two entries on one address — it prints one object per
		// (interface, address, provider), so it should not — Join drops the attachment with the
		// candidate list rather than picking silently, which is the failure this would want.
		return Attachment{Provider: provider, Fabric: DefaultFabric, Address: usable[0].Address}, skipped
	}

	return Attachment{}, skipped
}

// reachable reports whether an address is one another node has any chance of connecting to.
//
// **Only tcp is filtered, and it is the only provider that needs to be.** A verbs or efa address
// is hardware-derived and there is exactly one sensible answer per device; a host with an HCA it
// does not want used is a host whose operator configures attachments explicitly. tcp is the
// provider where the machine running this has half a dozen addresses and all but one of them are
// wrong:
//
//   - **loopback**, which every host has and which pairs only with itself. Picking it would make
//     a node look connected and fail every off-host session.
//   - **CGNAT** (100.64.0.0/10), which is what a Kubernetes CNI or a carrier NAT hands out. It is
//     routable within its own scope and never between the scopes a fleet spans, and it is the
//     address most likely to be *first* in the list on exactly the deployments that would reach
//     for this flag.
//   - **IPv6**, excluded for reachability reasons that are not this project's to judge — a
//     link-local v6 address needs a zone index the peer cannot use, and a ULA is as private as
//     CGNAT. An operator with a working v6 fabric names it, and gets it.
//   - **link-local v4** (169.254/16) and the unspecified address, which are never a deliberate
//     data path. This one is not in the stated rule and is included anyway: an APIPA address is
//     the failure mode of address configuration, so detecting onto it would turn one broken
//     thing into two.
//
// Everything left — RFC1918, CGNAT-free public v4 — is accepted. This function does not probe
// reachability and cannot: §10.1's whole point is that two nodes on RFC1918 addresses may have
// no route between them, and that is what the fabric label exists to decide.
func reachable(provider api.Provider, address string) bool {
	address = strings.TrimSpace(address)
	if address == "" {
		return false
	}
	if provider != api.ProviderTCP {
		// shm reports the hostname and efa a device address; neither is an IP and neither is
		// this function's business.
		return true
	}

	ip := net.ParseIP(zoneless(address))
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	if v4.IsLoopback() || v4.IsUnspecified() || v4.IsLinkLocalUnicast() || v4.IsMulticast() {
		return false
	}
	return !cgnat.Contains(v4)
}

// cgnat is RFC 6598 shared address space: a CNI's pod network, or a carrier NAT.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
