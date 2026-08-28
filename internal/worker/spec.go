package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"path/filepath"
	"slices"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
)

// Spec is everything needed to run one worker and to recognise it later.
//
// It is built by the agent from an [api.Assignment] plus the two things only the agent knows:
// where the named domain lives on this host, and which service it allocated (§7.4). It is not
// the worker's config file — an implementation of [Launcher] derives that from this — and the
// difference shows up in two places worth knowing about, [Spec.TargetInfo] and [Spec.Epoch].
type Spec struct {
	// SessionID is the session this worker realises. Deterministic and stable across server
	// restarts and leader changes (§7.3), which is what lets a restarted server recognise a
	// worker it never assigned in this process lifetime.
	SessionID string

	// Role is which end of the session this is. MXL's naming, which runs opposite to the
	// control plane's: the initiator sends, the target receives (§3).
	Role api.Role

	// Epoch is the target incarnation this worker belongs to (§5.2). Set for an initiator,
	// which is assigned one; empty for a target, which *produces* one — the agent computes it
	// from the blob this worker writes and reports it upward.
	//
	// The worker itself never sees this. It is here because it is part of the worker's
	// identity, and specifically because [Spec.Key] would otherwise be wrong in the one case
	// the whole epoch mechanism exists for: a target that restarts and produces a
	// **byte-identical** target_info still gets a fresh nonce and therefore a new epoch, and
	// its initiator must reconnect. A key derived from the blob alone would call that "already
	// correct" and leave an initiator running against rkeys that no longer exist — no error, no
	// data, everything upstream reporting healthy (§5.2).
	Epoch string

	// DomainPath is the local filesystem path of the domain, absolute, already resolved from
	// the domain *name* in the assignment.
	//
	// Resolution happens here and only here. The server sends names; an agent that accepted a
	// path from the API would make it a remote arbitrary-filesystem-write primitive on every
	// node in the fleet (§7.2, §13).
	DomainPath string

	// Domain is the domain's fleet-wide *name*, the one the assignment carried, kept for metric
	// labelling (§12). [Spec.DomainPath] is where the worker actually goes.
	//
	// It travels rather than being looked up backwards from the path because that lookup is
	// ambiguous: two configured names may map to one directory, and a metric that guessed
	// between them would attribute a flow to the wrong one.
	//
	// Deliberately excluded from [Spec.Key] — see there.
	Domain string

	// FlowID is the flow this worker carries. Required for an initiator, which opens an
	// existing local flow by ID. Optional but usual for a target, which creates its flow from
	// [Spec.FlowDef] — it is carried there for metric labelling (§12).
	FlowID string

	// FlowDef is the source flow's definition, verbatim, target only: a target cannot create
	// its local flow without it (§5.3).
	//
	// Verbatim is load-bearing. The destination flow must reproduce the source definition
	// exactly, including NMOS fields nothing in this tree models, and the session identity
	// hashes these bytes (§5.4) — so this travels as raw JSON from the producer's
	// flow_def.json to the worker's config with nothing in between decoding and re-encoding it.
	FlowDef json.RawMessage

	// BindAddress is the local fabric bind address, and is the worker config's `node` key
	// (WRS §3) — named Address here because "node" means a host everywhere else in this
	// project.
	//
	// Provider-dependent: an IP for tcp and verbs, a link-local device address for efa, the
	// hostname for shm. The agent takes it from the attachment matching the assignment's
	// (provider, fabric) pair — the provider alone is not enough, because a node can hold two
	// verbs attachments on different InfiniBand fabrics and binding the wrong one produces a
	// target that comes up perfectly and an initiator that never connects (§10.1).
	BindAddress string

	// Service is the local fabric endpoint name, allocated by the agent from its configured
	// range (§7.4). A port number for tcp, verbs and efa; for shm not a port at all, only a
	// host-wide unique name, which the same allocator supplies for free.
	//
	// Deliberately excluded from [Spec.Key] — see there.
	Service string

	// Interface is the negotiated interface configuration, identical on both ends of the
	// session by necessity: the library performs no negotiation of its own and documents that
	// both ends must be handed the same values (§10.3, WRS §3). Deciding it per side is not a
	// configuration choice, it is a bug.
	Interface api.InterfaceConfig

	// TargetInfo is the peer target's serialised blob, **initiator only**, carried verbatim.
	//
	// Note the asymmetry with the worker's config key of the same name, which is two different
	// things by role (WRS §3): for an initiator it is the blob inline, which is this field; for
	// a target it is an output *path* the worker writes to, which the launcher chooses and the
	// caller must leave empty here. That asymmetry belongs to the process, not to the
	// assignment, so it stops at this boundary.
	//
	// Opaque. The agent's one interaction with the contents is to recompute the epoch from it
	// and check that against [Spec.Epoch] before starting anything (§5.3 step 6).
	TargetInfo string

	// NoNetworkLatencyMeasurement must match on both ends or the target reports garbage latency
	// with no error at all (WRS §5.3). Session-level, set by the server for both workers from
	// one place (§5.5).
	NoNetworkLatencyMeasurement bool

	// SchedPrio is the SCHED_FIFO priority for the transfer loop, or nil to leave scheduling
	// alone. Only ever set for a node that advertised the capability, because
	// sched_setscheduler failing is fatal *after* the connection is established, with no
	// graceful degradation (§10.2, WRS §9).
	SchedPrio *int

	// IdleTimeout is how long the worker waits without a grain before terminating, where zero
	// means wait indefinitely — the worker's own sentinel (WRS §3).
	//
	// Long or infinite is the normal setting, and it is what makes PAUSED a real steady state
	// rather than a permanent ~13 s restart cycle on every idle-but-requested flow (§11.1).
	//
	// Sub-millisecond values are rejected by [Spec.Validate] rather than truncated: the worker
	// takes whole milliseconds, and a 500µs timeout silently truncating to 0 would mean the
	// exact opposite of what was asked for.
	IdleTimeout time.Duration

	// ConnectTimeout bounds the initiator's connect loop, zero meaning indefinitely (WRS §3).
	// Ignored by a target, which is passive.
	ConnectTimeout time.Duration

	// Labels are the requesting user's labels for this worker's metrics (§12). The worker
	// ignores the config key of the same name (WRS §3 "fields the C++ worker ignores"); the
	// agent applies them when it scrapes.
	//
	// Deliberately excluded from [Spec.Key] — see there.
	Labels map[string]string
}

// IsTarget reports whether this is the receiving end.
func (s Spec) IsTarget() bool { return s.Role.IsTarget() }

// keyFormatTag domain-separates [Spec.Key] and makes its framing explicit.
//
// Unlike the epoch's tag, this one is *not* a protocol contract: the key never leaves the
// agent process, is compared only against another key computed by the same binary, and is
// recomputed from scratch on every reconcile. Changing the framing costs one round of worker
// restarts on upgrade and nothing else. Changing the *field set* is the part that needs
// thought, and the doc comment on [Spec.Key] is where that argument lives.
const keyFormatTag = "mxl-replicator/worker/key/v1"

// Key is the agent's "am I already running the right thing?" test (§7.3), as a single
// comparable value: the session, the role, the epoch, and the configuration that materially
// affects the process.
//
// # Never diff the assignment instead
//
// This is invariant 2 of the plan's checklist, and it is a bug class that passes every naive
// test and then flaps in production. Any incidental difference — a re-derived port, a
// reordered JSON field, a re-serialised flow definition that is semantically identical — reads
// as a change and restarts a healthy worker. Those differences only start appearing once there
// are two server replicas or a store round trip in the path, which is to say once it is in
// production.
//
// So the exclusions are the point of this method, and each is deliberate:
//
//   - [Spec.Service] is allocated by this agent, not by the server (§7.4). If the allocator
//     would pick a different port today than it did an hour ago, that is not a reason to
//     restart a worker that is bound and moving media.
//   - [Spec.Labels] and [Spec.Domain] exist for metric labelling and the worker reads neither
//     (WRS §3). They are applied at scrape time, so a relabelled request — or a domain renamed
//     onto the same directory — must not glitch a flow. [Spec.DomainPath], which is where the
//     worker actually goes, *is* included.
//   - [Spec.TargetInfo] is covered by [Spec.Epoch], which is a hash over exactly that blob plus
//     the target's incarnation nonce, and the pair is verified before a worker is started
//     (§5.3 step 6). Hashing the blob as well would add a second way for an incidental
//     difference to force a restart and no way to catch anything the epoch does not.
//   - Capability *order* is normalised away, because the flag set comes out of a server-side
//     intersection and its order is an implementation detail of that intersection, not part of
//     the negotiated result. The library takes a set (WRS §3).
//
// Everything else is included, including the timeouts, because a change to one of those is an
// operator asking for different behaviour from a running process, and the worker reads its
// config exactly once (WRS §1).
func (s Spec) Key() string {
	h := sha256.New()
	f := framed{h}

	f.str(keyFormatTag)
	f.str(s.SessionID)
	f.str(string(s.Role))
	f.str(s.Epoch)
	f.str(s.DomainPath)
	f.str(s.FlowID)
	f.bytes(canonicalJSON(s.FlowDef))
	f.str(s.BindAddress)

	f.str(string(s.Interface.Provider))
	flags := slices.Clone(s.Interface.CapFlags)
	slices.Sort(flags)
	f.u64(uint64(len(flags)))
	for _, flag := range flags {
		f.str(string(flag))
	}
	f.u64(s.Interface.MaxMessageSize)

	f.u64(uint64(s.IdleTimeout))
	f.u64(uint64(s.ConnectTimeout))
	f.bool(s.NoNetworkLatencyMeasurement)
	f.bool(s.SchedPrio != nil)
	if s.SchedPrio != nil {
		f.u64(uint64(int64(*s.SchedPrio)))
	}

	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON compacts a flow definition so that insignificant whitespace cannot restart a
// worker.
//
// Key order is deliberately *not* normalised. It survives every hop by construction — the
// definition travels as raw bytes from flow_def.json through the API to the worker (§2a) — and
// the session identity hashes it too (§5.4), so a canonicaliser that reordered keys here would
// disagree with the session ID about what "the same flow" means. Invalid JSON is passed
// through unchanged; [Spec.Validate] is what rejects it.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// framed writes length-prefixed fields into a hash, so that no concatenation of one field's
// value with the next can produce another field's framing.
type framed struct{ h hash.Hash }

func (f framed) bytes(b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = f.h.Write(n[:])
	_, _ = f.h.Write(b)
}

func (f framed) str(s string) { f.bytes([]byte(s)) }

func (f framed) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, _ = f.h.Write(b[:])
}

func (f framed) bool(v bool) {
	if v {
		f.u64(1)
		return
	}
	f.u64(0)
}

// Validate rejects a Spec that cannot produce a working worker.
//
// It is cheap and it runs before anything is started, which mirrors the worker's own config
// validation: a bad config fails in milliseconds rather than after a connection attempt
// (WRS §3). Every [Launcher] implementation calls it, the fake included, so a control-plane
// test that builds a nonsensical Spec fails in that test rather than in a restart loop that
// looks like a fabric problem.
func (s Spec) Validate() error {
	if s.SessionID == "" {
		return fmt.Errorf("worker: session id is required")
	}
	switch s.Role {
	case api.RoleTarget, api.RoleInitiator:
	default:
		return fmt.Errorf("worker: unknown role %q", s.Role)
	}
	if s.DomainPath == "" {
		return fmt.Errorf("worker: domain path is required")
	}
	if !filepath.IsAbs(s.DomainPath) {
		// The agent resolved this from its own mappings, so a relative path is a configuration
		// bug on this host — and it would be interpreted against the worker's working
		// directory, which is not a thing the agent controls.
		return fmt.Errorf("worker: domain path %q is not absolute", s.DomainPath)
	}
	if s.BindAddress == "" {
		return fmt.Errorf("worker: bind address is required")
	}
	if s.Service == "" {
		// The worker accepts an empty service and lets the provider choose, but then nothing
		// can report where the target actually bound (§7.4, WRS §3).
		return fmt.Errorf("worker: service is required")
	}
	if !api.KnownProvider(s.Interface.Provider) {
		return fmt.Errorf("worker: unknown provider %q", s.Interface.Provider)
	}
	if !s.Interface.CanTransfer() {
		// Neither REMOTE_WRITE nor SEND_RECEIVE survived the intersection, so the pair cannot
		// move data at all and negotiation should have refused it (§10.3).
		return fmt.Errorf("worker: negotiated capabilities %v can transfer nothing", s.Interface.CapFlags)
	}

	if err := validateTimeout("idle_timeout", s.IdleTimeout); err != nil {
		return err
	}
	if err := validateTimeout("connect_timeout", s.ConnectTimeout); err != nil {
		return err
	}

	if s.IsTarget() {
		if len(s.FlowDef) == 0 {
			return fmt.Errorf("worker: target needs a flow definition")
		}
		if !json.Valid(s.FlowDef) {
			return fmt.Errorf("worker: target flow definition is not valid json")
		}
		if s.TargetInfo != "" {
			// The worker's target_info key is an output path for this role, chosen by the
			// launcher (WRS §3). A caller setting it here has the two meanings confused.
			return fmt.Errorf("worker: target must not be given a target info blob")
		}
		return nil
	}

	if s.FlowID == "" {
		return fmt.Errorf("worker: initiator needs a flow id")
	}
	if len(s.FlowDef) != 0 {
		return fmt.Errorf("worker: initiator must not be given a flow definition")
	}
	if s.TargetInfo == "" {
		return fmt.Errorf("worker: initiator needs a target info blob")
	}
	if s.Epoch == "" {
		// An initiator is never started before an epoch exists for the session (§5.3, invariant
		// 3). Without one there is nothing to compare the blob against, and nothing to
		// converge on when the target restarts.
		return fmt.Errorf("worker: initiator needs an epoch")
	}
	return nil
}

func validateTimeout(name string, d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("worker: %s is negative", name)
	}
	if d != 0 && d < time.Millisecond {
		// Zero already means "wait indefinitely" (WRS §3). Truncating 500µs to 0 ms would turn
		// the shortest possible timeout into no timeout at all.
		return fmt.Errorf("worker: %s of %s truncates to zero milliseconds", name, d)
	}
	return nil
}
