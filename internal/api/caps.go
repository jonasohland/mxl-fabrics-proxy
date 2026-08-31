package api

import "slices"

// Provider is a libfabric provider name, spelled as the worker spells it (WRS §3).
type Provider string

const (
	ProviderTCP   Provider = "tcp"
	ProviderVerbs Provider = "verbs"
	ProviderEFA   Provider = "efa"
	ProviderSHM   Provider = "shm"
)

// DefaultProviderOrder is the preference order used when a request pins nothing (§10.4),
// matching the priority the mxl demo tool uses. SHM sits below TCP and only ever matches
// same-node. Operators can override it; the order is not part of the wire contract.
var DefaultProviderOrder = []Provider{ProviderEFA, ProviderVerbs, ProviderTCP, ProviderSHM}

// KnownProvider reports whether p is one this project can negotiate.
//
// The worker also accepts "any" (WRS §3), and it is deliberately absent here: the server
// always resolves a concrete provider before anything is assigned, so "any" would be a value
// that could only ever mean "negotiation did not run".
func KnownProvider(p Provider) bool {
	return slices.Contains(DefaultProviderOrder, p)
}

// CapFlag is a libfabric transfer capability.
//
// The names are exactly what the worker's --interfaces probe prints and exactly what its
// caps_flags config key accepts (WRS §2, §3), so two nodes' reported sets can be intersected
// and written straight back out with no bit/name translation anywhere in the system.
type CapFlag string

const (
	CapRemoteWrite        CapFlag = "REMOTE_WRITE"
	CapSendReceive        CapFlag = "SEND_RECEIVE"
	CapBlockingOperations CapFlag = "BLOCKING_OPERATIONS"
)

// InterfaceConfig is the negotiated interface configuration for one session (§10.3).
//
// The library performs **no** capability negotiation of its own and documents that both ends
// must be handed identical values, with the caller's out-of-band channel responsible for
// agreeing them. This project is that channel, so this type is session-level by necessity,
// not by preference: the server computes one of these and writes it into *both* workers'
// configs. Deciding it per side is not a configuration choice, it is a bug.
//
// The shape matches the worker's config keys 1:1 (WRS §3) so an assignment maps onto a
// worker config without interpretation.
type InterfaceConfig struct {
	Provider Provider  `json:"provider"`
	CapFlags []CapFlag `json:"caps_flags"`

	// MaxMessageSize is in bytes, and is a genuine uint64: providers do report UINT64_MAX.
	// Never let it pass through a float64 — decoding into `any` loses it (WRS §2).
	// Zero leaves it to the library, which warns that the field will be required in a future
	// version.
	MaxMessageSize uint64 `json:"max_message_size"`
}

// HasCap reports whether the negotiated config carries a capability.
func (c InterfaceConfig) HasCap(flag CapFlag) bool { return slices.Contains(c.CapFlags, flag) }

// CanTransfer reports whether the negotiated capabilities can move data at all. At least one
// of REMOTE_WRITE or SEND_RECEIVE must survive the intersection or the pair is not viable
// (§10.3).
func (c InterfaceConfig) CanTransfer() bool {
	return c.HasCap(CapRemoteWrite) || c.HasCap(CapSendReceive)
}

// FabricAttachment is one way a node can be reached: a (provider, fabric, address) triple
// with the capabilities the library reports for it (§10.1, §10.2).
//
// Nodes declare attachments rather than providers because **provider availability is not
// reachability**. Two nodes both offering verbs may be on different InfiniBand fabrics; two
// both offering efa may be in different VPCs; two both offering tcp on RFC1918 addresses may
// have no route between them. Set intersection on provider names alone will cheerfully assign
// a session that cannot connect, and the failure presents badly — the target comes up clean,
// the initiator's connect loop spins, and nothing explains why.
//
// An agent advertises an attachment only if it appears in *both* its configured `fabrics:`
// block and the worker's --interfaces probe (§10.5). Raw kernel capabilities (CAP_IPC_LOCK,
// /dev/infiniband, RLIMIT_MEMLOCK) never go over the wire; they gate whether the attachment is
// advertised at all (§10.2).
type FabricAttachment struct {
	Provider Provider `json:"provider"`

	// Fabric is an operator-assigned opaque label. Two nodes may pair on a provider iff they
	// share a label for it. The server does nothing with it but string equality — no topology
	// database, no reachability probing, no inference — which matches how these networks are
	// actually provisioned: the operator already knows which HCA is on which fabric.
	//
	// shm's label is derived from the node name, which makes same-node-only fall out with no
	// special case. That derivation assumes node names are unique fleet-wide (§7.1 enforces
	// it) and that both sides normalise identically.
	Fabric string `json:"fabric"`

	// Address is the local fabric bind address, and is provider-dependent: an IP for tcp and
	// verbs, a link-local device address for efa, and the hostname for shm. It is what goes in
	// the worker config's `node` key — named Address here because "node" already means a host
	// everywhere else in this API.
	//
	// It is always the probe's reported address, resolved agent-side before registration. An
	// interface name never reaches this field or the wire: the probe names no interface, so
	// the join that produces this attachment is where a configured `ib0` is resolved, and it
	// is the only place that can be (§10.5, WRS §2).
	Address string `json:"address"`

	// There is deliberately no Service. The probe reports one, but it is empty for every
	// provider but shm and the shm value is a per-process artefact of the probing process
	// (WRS §2). The agent allocates a service for every provider from its own port range —
	// for shm that is not a port, only a host-wide unique endpoint name, which the same
	// allocator supplies for free (§7.4, WRS §9). What a target actually bound is reported in
	// [SessionStatus], not here.

	CapFlags       []CapFlag `json:"caps_flags"`
	MaxMessageSize uint64    `json:"max_message_size"`

	// Device is the probe's best-effort attr.device_name: the netdev name for tcp, but the
	// libfabric device name for verbs and efa, which is *not* the netdev name, and absent
	// entirely for shm (WRS §2). Diagnostics only — the agent may join on it locally when an
	// attachment configures `device:`, but the server never interprets it.
	Device string `json:"device,omitempty"`
}

// SHMFabric is the fabric label for a node's shm attachment (§10.1).
//
// shm is structurally same-node-only, and deriving its label from the node name is what makes
// that fall out of ordinary fabric matching with no special case anywhere: two attachments can
// only share this label if they are on one node, and node names are unique fleet-wide (§7.1).
//
// It lives here, in the shared wire package, because **both sides must derive the identical
// string or a same-node shm pair silently fails to match** — the agent labels its own
// attachment with it, and the server canonicalises what it stores so that an agent which
// labelled it some other way still matches. One definition, no normalisation to agree on.
func SHMFabric(node string) string { return "shm:" + node }

// Versions is what a node reports about the software it is running (§10.2).
type Versions struct {
	// Protocol is the control-plane wire protocol, and the only field version-skew gating
	// keys on (plan §4.1).
	Protocol int `json:"protocol"`

	// Replicator is this binary's build version.
	Replicator string `json:"replicator"`

	// Proxy, MXL and Libfabric come from the worker's -v output (WRS §2), which the agent runs
	// at startup as a load probe anyway.
	//
	// The mxl version pair is the non-obvious entry in this struct. target_info is produced by
	// one node's mxl-fabrics and consumed by another's, so a node pair straddling an mxl
	// version boundary is a compatibility concern *neither agent can detect alone* — only the
	// server sees both sides (§10.2).
	Proxy     string `json:"proxy,omitempty"`
	MXL       string `json:"mxl,omitempty"`
	Libfabric string `json:"libfabric,omitempty"`
}

// Capabilities is everything a node advertises at registration (§10.2).
//
// The test for membership is sharp: something is a capability iff the server would make a
// wrong decision without it. Everything else is agent config or a local precondition.
//
// Capabilities are static — they change only by re-registering. An agent whose attachments
// change (a probe that now reports different interfaces, say) re-registers.
type Capabilities struct {
	Fabrics  []FabricAttachment `json:"fabrics"`
	Versions Versions           `json:"versions"`

	// SchedPrio reports whether the node can actually apply SCHED_FIFO (CAP_SYS_NICE /
	// RLIMIT_RTPRIO). Used at request-validation time so a request that cannot be honoured
	// fails immediately instead of producing workers that silently run at normal priority.
	SchedPrio bool `json:"sched_prio"`

	// PortRange is the agent's configured fabric port range, as "low-high". Diagnostics only:
	// the agent allocates and reports what it actually bound (§7.4), and the server is
	// deliberately kept out of a job it cannot verify.
	PortRange string `json:"port_range,omitempty"`

	// Areas are the directories an operator has designated on this node as somewhere MXL domains
	// live, each with its own `read` and `write` grants (§10.6). Empty means a node that offers no
	// sources and accepts no destinations — the default, and the right posture for the one piece of
	// configuration that grants this project any authority over a host's filesystem.
	//
	// **Readable areas are advertised too, and they pass the capability test rather than bending
	// it** (§10.2). *An earlier reading had it the other way — search paths were not advertised, on
	// the correct observation that the server made no wrong decision without them.* It does now: a
	// domain's name is `<area>/<elements>`, so a server that does not hold the area table cannot
	// render a domain's identity, resolve a name in a request, or tell an operator which area a
	// label landed outside of. The grants come with the entry because "may this name be a
	// destination" is request-time validation.
	Areas []Area `json:"areas,omitempty"`
}

// Area is one directory an operator has designated on a node as somewhere MXL domains live,
// with a name and two independent grants (§10.6).
//
// **`read` is the whole of this project's authority to discover and observe domains under that
// directory; `write` is the whole of its authority to create them and write flows into them.**
// Neither implies the other, both default false, and an area granting neither is refused at
// startup as a line that does nothing.
//
// *This supersedes `--search-path` and `--output-root` as separate concepts.* They were already
// counterparts and already had to be read as a pair; making them one noun with two bits is what
// that was asking for. The arrangement an operator actually reaches for — one MXL area per host,
// with a subtree replication may write into — stops being an exception to an overlap rule and
// becomes two ordinary areas.
//
// Areas rather than domains, which is the same shape as fabric attachments and for the same
// reason: a node declares what it *has*, and a request names what it *wants* inside that. A
// domain has no lifecycle of its own — it is materialised by the first path that targets it and
// forgotten when the last one goes — so modelling one as durable node configuration would mean
// provisioning every destination twice.
type Area struct {
	// Name is how a domain in this area is addressed: it is the **first segment of that domain's
	// fleet-wide name** (§10.6). It never becomes part of a path — the agent looks it up among
	// the areas it advertises and joins the domain's elements onto the path it finds.
	//
	// Unique per node, which is what keeps a rendered domain name unambiguous about where its
	// first segment stops.
	Name string `json:"name"`

	// Path is the local directory. Diagnostics only — the server never sends a path to an agent;
	// it sends the name and the agent resolves it against its own configuration.
	//
	// Repointing it while keeping the name **does not re-identify anything** on the node: a
	// domain's name is `<area>/<elements>`, so paths and sessions survive the move rather than
	// rebuilding (§5.4).
	Path string `json:"path,omitempty"`

	// Read grants discovering and observing domains under this directory. A node with no readable
	// area offers no sources.
	Read bool `json:"read"`

	// Write grants creating domains here and writing flows into them. A node with no writable
	// area accepts no destinations at all.
	Write bool `json:"write"`
}

// FindArea returns the advertised area of that name, or nil.
func (c Capabilities) FindArea(name string) *Area {
	for i := range c.Areas {
		if c.Areas[i].Name == name {
			return &c.Areas[i]
		}
	}
	return nil
}

// FindFabric returns the attachment for a (provider, fabric) pair, or nil.
func (c Capabilities) FindFabric(provider Provider, fabric string) *FabricAttachment {
	for i := range c.Fabrics {
		if c.Fabrics[i].Provider == provider && c.Fabrics[i].Fabric == fabric {
			return &c.Fabrics[i]
		}
	}
	return nil
}

// NodeRegistration is POST /agent/v1/register (§9.2).
//
// Registration (durable: this node exists, and here is what it can do) and the liveness lease
// (observed, TTL'd: an agent instance currently holds this identity) are separate concepts and
// must not be merged (§7.1).
type NodeRegistration struct {
	// Node is the operator-assigned name, unique fleet-wide.
	Node string `json:"node"`

	// Instance is a fresh UUID per agent *process*. It is what makes a lease claim
	// attributable: a second instance claiming a name whose lease is still held is rejected
	// with [CodeNodeClaimed] rather than quietly taking over.
	Instance string `json:"instance"`

	// **Registration carries no domains** (§6). *This supersedes a `domains` field carrying the
	// agent's configured name→path mappings.* There are none to carry: a node's domains are
	// discovered, so they are observed state and reach the server through inventory, and what they
	// are *called* is an API concern rather than agent configuration (§10.7).
	Capabilities Capabilities `json:"capabilities"`
}

// RegistrationResponse is the server's answer to a successful registration.
type RegistrationResponse struct {
	// Lease is the liveness lease the agent must renew by heartbeating.
	Lease string `json:"lease"`

	// TTL is how long the lease survives without a heartbeat, and HeartbeatInterval is how
	// often the server expects one. Both are server-configured, so the agent takes its cadence
	// from here rather than from its own flags — that keeps the settling window (§7.3), which
	// is expressed as a multiple of the heartbeat, meaningful.
	TTL               Milliseconds `json:"ttl_ms"`
	HeartbeatInterval Milliseconds `json:"heartbeat_interval_ms"`

	// Server reports what the agent is talking to, so version skew is visible in agent logs
	// and not only server-side.
	Server Versions `json:"server"`
}

// Heartbeat is POST /agent/v1/{node}/heartbeat, and is also the identity block every other
// agent report carries.
//
// The lease ID is here because renewing is the one thing an agent does that must not be
// possible for a *different* instance of the same node: the lease is what holds node identity
// (§7.1), so an agent that lost the race for a name must not be able to keep the winner's
// lease alive.
type Heartbeat struct {
	Node     string `json:"node"`
	Instance string `json:"instance"`
	Lease    string `json:"lease"`
}

// HeartbeatResponse is the answer to POST /agent/v1/{node}/heartbeat.
type HeartbeatResponse struct {
	// Reregister tells the agent its registration is gone — a store that lost its contents, or
	// a lease that expired while the agent was partitioned — and it should register again
	// before continuing. It is not a teardown signal: an agent that must re-register keeps its
	// workers running while it does (§4.2).
	Reregister bool `json:"reregister,omitempty"`
}
