# `mxl-replicator` — architecture

How the system is built and why. It is prescriptive on purpose: points where a design choice
was genuinely open are marked **Settled** and carry the reasoning that closed them, so a reader
can tell a decision from an assumption. §18 collects every such decision in one place.

**The section numbers are load-bearing.** Roughly 1200 comments in `internal/`, `cmd/` and
`src/` cite them (`§10.6`, `§5.2`, …), so sections keep their numbers even where their content
has moved on. Where a decision here supersedes an earlier one, both are recorded rather than
the earlier one being quietly overwritten — the superseded position usually reads plausible,
which is exactly why it has to be argued down in writing.

`rewrite-plan.md` is the companion document: the milestone-by-milestone implementation record,
with a **Plan decision** marker wherever the implementation went past this document or against
it. Everything it settled has been folded back into the sections below.

Required background reading:

1. `docs/worker-runtime-surface.md` (**WRS**) — the contract with the C++ worker binary.
   Everything in §5 and §6 is downstream of it.
2. `docs/third_party/mxl/Addressability.md` — the `mxl://` URI scheme and, importantly, the
   statement that domain-path translation is *out of scope for MXL*. That translation is this
   project's job.
3. `docs/third_party/mxl/Fabrics.md` §2, §4.3, §5 — the target/initiator model and what
   `TargetInfo` actually contains.
4. `docs/third_party/mxl/FabricsDeveloperGuide.md` — "Interfaces" and "Compatibility". §10 is
   built on the statement that the library performs **no** capability negotiation and expects
   the caller's out-of-band channel to do it. That channel is this project.
5. `docs/third_party/mxl-utils/mxl-utils.md` — the discovery and flow-observation library this
   project uses.

---

## 1. Context

MXL flows are ring buffers in memory-mapped files under a *domain* directory
(`<domain>/<flow-id>.mxl-flow/`). Media functions on one host share them with zero copies.
**MXL Fabrics** extends that across hosts over libfabric (`tcp`, `verbs`, `efa`, `shm`),
predominantly via RDMA Remote Write straight into the receiver's media buffer.

The thing that drives MXL Fabrics for flows a host does not own is `mxl-replicator-worker`,
a C++ binary where **one process = one flow, one direction, one peer, one role**. The worker
has no discovery, no control plane and no reconnect logic: it is configured by a JSON file read
once at startup, and restart is its only recovery mechanism. It also performs no capability
negotiation, and requires both ends of a transfer to be handed the *same* negotiated interface
configuration through an out-of-band channel.

`mxl-replicator` is that channel and that supervisor. It decides which flows go where,
negotiates the fabric each transfer uses, and runs the worker processes that move the media. It
never touches grain data.

This replaces a peer-to-peer arrangement in which each node carried a static subscription list
and fetched flow definitions and `target_info` from its peers over HTTP — configuration that
was O(n²) across a fleet, static, and required every node to reach every other node's API. That
proxy is retired; §16 records what carried over from it and what deliberately did not.

---

## 2. What the system does

- Connection establishment runs through a **central connection management server**. Agents
  register with it, receive assignments, and act on them. There is no agent-to-agent traffic on
  the control plane.
- Replication is requested **through an API**, not a per-node config file. A request names a
  source node + domain + flow *selector* and one or more destination node + domain pairs; it is
  identified by a client-supplied name that is also its ID and its idempotency key (§9.1). The
  operator writes those requests to a manifest and applies it.
- Agents **report the flows they observe** in their domains. The server aggregates this
  into a fleet-wide inventory.
- Server and agent ship in **one binary**, with both roles runnable in one process for
  single-host and development use (§2.2, §2.3).
- Two storage backends: **etcd** (HA) and **local sqlite** (single node).
- With etcd, the server is deployable **HA behind a plain third-party HTTP proxy** — no sticky
  sessions, no special L7 features.
- The worker is **reused rather than rewritten** (§15).
- **Filesystem authority stays agent-local**, configured by flags and a YAML file: a node declares
  named **areas**, each granting reading, writing or both (§10.6). What a domain is *called* is not
  agent configuration — operators label domains through the API (§10.7).

### 2.1 Non-goals

- Changing the one-process-per-flow-per-direction worker model. Note the consequence in §14.
- Media-plane changes of any kind. This project never touches grain data.
- Wire or config compatibility with the retired proxy, beyond the provisioning half (§16).

### 2.2 Name and command-line surface

The project is **`mxl-replicator`**. The name is deliberately descriptive rather than a product
name: an official MXL discovery and connection API is in progress and this project is expected
to realign with it, so the name should describe the tool rather than stake out an identity. It
also sits in a family with `mxl-utils`, which it depends on, and it matches the vocabulary this
document uses throughout — request, path, replication.

| | |
|---|---|
| Module path | `github.com/jonasohland/mxl-replicator` |
| Daemon binary | `mxl-replicator` |
| Container image | `jonasohland/mxl-replicator`, `:latest-efa` variant |
| Data-plane worker | `mxl-replicator-worker` — renamed from `mxl-fabrics-proxy-worker`, see below |
| CLI | no separate binary: the manifest verbs are subcommands of `mxl-replicator` |

```
mxl-replicator run [flags]             # both roles — the default
mxl-replicator run --server --agent    # both roles, said explicitly
mxl-replicator run --agent  ...        # agent only: an ordinary fleet node
mxl-replicator run --server ...        # server only: a control-plane node

mxl-replicator apply    -f studio-a.yaml [--dry-run] [--prune -n nab [-l show=x]]
mxl-replicator delete   -f studio-a.yaml | [-n nab] <name>...
mxl-replicator label    domain <node>:<area>/<elements> role=cameras name=cameras role-
mxl-replicator status
mxl-replicator get      nodes|domains|flows|requests|paths|sessions|namespaces [filters]
mxl-replicator describe node|domain|flow|request|path|session|namespace <name>
mxl-replicator events   path|request|node <name> [--since <seq>]
mxl-replicator logs     path <path-id>
```

**Settled: one command with negatable role toggles, not one subcommand per role.** *This
supersedes an earlier position in this document — "roles are subcommands, not flags" — which
rejected role flags because the help text becomes a union of two unrelated tools.* That
argument assumed a fleet is made of single-role nodes, and §2.3 shows it is not: a node running
both roles is an ordinary production deployment, not a special case. Once `run` has to exist
anyway, per-role subcommands become a *second* spelling of the same deployment — `server` and
`run --server` differing only in flag names — and two spellings for one thing is the worse
outcome. The union-help concern is real but small: `run --help` is 37 flags, against 20 and 18
for the roles alone, and the role option structs are embedded with `server-`/`agent-` prefixes,
which group the list by subsystem the way `--store-*` already does.

Naming a role **selects it alone**; naming neither — or both — runs both. The one surprising
edge, recorded because it is: `--server` means "server only", not "also enable the server", so
naming one role implicitly disables the other. Both flags' help text says so, and there is no
way to select nothing. `run` is kong's default command, so `mxl-replicator --agent` and
`mxl-replicator run --agent` are the same invocation.

A combined instance still speaks **HTTP between the two roles** rather than short-circuiting in
memory, so there is exactly one code path. The co-located agent dials its own server over
loopback, derived from `--server-listen` (wildcard binds become loopback) — which also pins it
to a server of its own version (§13.1, §2.3). With TLS terminated by the server, the derived
loopback URL cannot be used (a certificate for the node's routable name will not cover
`127.0.0.1`), so `--agent-server` is required and this is enforced at parse time.

Validation happens at parse time, not at first use: a lease TTL below the heartbeat interval,
`etcd` without endpoints, half-configured TLS, an unknown provider in the preference order, an
area path that is not absolute, an area granting neither read nor write, two areas on one path, an
inverted port range, two role listeners on one address. Only the
enabled roles are validated, so an agent-only node is never asked for a valid store
configuration.

Notable defaults: server `:2283` (the port the retired proxy used), agent metrics `:2284`, port
range `24000-24999`, provider order `efa,verbs,tcp,shm` (§10.4), settling window `3` heartbeats
(§7.3), worker idle timeout `0` = wait indefinitely (§11.1), worker start rate `0.5`/s with a
burst of `2`, where `0` again means no limit (§6.3).

**The worker binary is named for this project.** `mxl-replicator-worker` was renamed from
`mxl-fabrics-proxy-worker`, an artifact of the retired proxy. The `-worker` suffix is unique in
this repository, so the layer — and which binary WRS covers — stays unambiguous.

**Metric prefixes split along the same line** (§12): anything describing a flow keeps `mxl_`,
because it comes off the worker socket and describes MXL rather than this project.
Control-plane metrics get `mxl_repl_`. This follows the Prometheus `<namespace>_<subsystem>_`
convention — namespace `mxl`, subsystem `repl` — and keeps the whole project under one
selectable namespace, so `{__name__=~"mxl_.*"}` catches everything.

### 2.3 Deployment topologies

`mxl-replicator run` starts both roles by default, and the server role in a combined instance
is a **full server**: it binds its configured address and serves every other node's agent
exactly as a standalone server does. Three shapes are in scope.

| Topology | Store | Reconciler | Notes |
|---|---|---|---|
| Dedicated server (`--server`) + N agents (`--agent`) | sqlite or etcd | the one server | Cleanest isolation. |
| One both-roles node + N−1 `--agent` nodes | sqlite or etcd | the both-roles node | Non-HA. The shape most small fleets want. |
| M both-roles nodes + N−M `--agent` nodes | **etcd** | elected leader among the M | HA. |

Co-locating the two roles is sound, but it trades away an isolation property the design
otherwise has, and each consequence is recorded where it belongs rather than as a footnote
here:

- **A control-plane failure now glitches media on that node** (§6.1). Fail-static (§4.2) exists
  so a server outage never stops media; on a combined node a panic, an OOM kill, a liveness
  failure or an image update takes the agent down with the server and every flow on that node
  re-establishes. Hence: panic recovery on every background goroutine, a memory limit sized for
  fleet-wide inventory rather than for a local supervisor, and a preference for running the
  control plane on nodes carrying few or no flows.
- **The rolling-upgrade rule has to be gated on the protocol version**, not the build version,
  because upgrading a combined instance upgrades both roles at once (§13.1).
- **HA adds hazards a single server cannot exhibit** — cursor regression across replicas, a
  demoted leader still writing, leader churn under RT workers (§8.2).
- `MXL_REPLICATOR_AUTH_TOKEN` feeds both halves of a combined instance. Correct — the two share
  a trust domain — but worth knowing it is one value, not two.
- Shutdown ordering: stop serving before the local agent releases its lease, and remember that
  an expired lease is not proof that a node's workers stopped (§4.2).

---

## 3. Identity and terminology

Getting these names fixed matters, because MXL's data-plane naming and this project's
control-plane naming run in *opposite directions*.

| Term | Meaning |
|---|---|
| **Node** | One logical host in the network = one agent. Identified by a unique operator-assigned **node name** (CLI flag, env var, or agent config file). |
| **Domain** | An MXL domain on a node: a directory inside an area, holding flows. One kind, whichever direction this project uses it in — a place rather than a channel (§10.6). Addressed fleet-wide as `<area>/<elements>`, which is its identity for life. |
| **Area** | A directory an operator has designated on a node as somewhere MXL domains live, given a name and two independent grants: `read` (domains here may be discovered and observed) and `write` (replication may create domains and flows here). Declared in agent config, advertised at registration. Between them the grants are the whole of this project's authority over that node's filesystem (§10.6, §13). |
| **Domain label** | A key/value pair an operator attaches to `(node, domain)` through the API. Annotation, never identity: labels are what a request's source selector matches, and relabelling never re-identifies a domain (§10.7). |
| **Namespace** | A partition of requests, and a first-class object (§9.3). It scopes request names, scopes `--prune`, and carries whether requests inside it may share a path. It does **not** partition nodes, domains or destinations. |
| **Flow** | A UUID. Unique to the media, *not* to a location. The same flow ID exists on both nodes after replication — that is the point. |
| **Flow address** | `(node, domain, flow-id)`. The domain component is required: the same flow ID can legitimately exist in two domains on one node. |
| **Request** | Durable user intent: "replicate what these selectors match, from these places to those places". Identified by a client-supplied name **scoped to its namespace**: `(namespace, name)` is its ID and its idempotency key (§9.1, §9.3). Reference-counted. |
| **Path** | The deduplicated logical edge derived from requests: `(src flow address) → (dst node, dst domain)`. N requests → 1 path. Torn down when the last referencing request is cancelled. |
| **Session** | A concrete worker *pair* realising a path, identified by a stable session ID and a target-side **epoch** (§5.2). Ephemeral. Re-established whenever either end restarts. |
| **Epoch** | A content hash identifying one target-worker incarnation. Not a counter — it has no ordering, only equality (§5.2). |
| **Initiator** | MXL term. The **sending** worker, on the source node. Reads the local flow, RDMA-writes to the peer. |
| **Target** | MXL term. The **receiving** worker, on the destination node. Binds `node:service`, creates the local flow, is passive. |

**Direction of everything.** The fabric connection goes source → destination (the initiator
connects to the target's bound endpoint). The *information* goes the other way first: the
destination produces `target_info`, which the source consumes. And the *request* is usually
authored by whoever wants the flow, i.e. neither. Keep these three straight in code comments
and in the API; they are the main source of bugs in this domain.

---

## 4. State model

Three distinct layers, stored and reasoned about separately. This is the central structural
property of the design.

**Desired state — durable, small, low-churn, written by users.**
- Node registrations (see §7.1 for the difference between a registration and a liveness lease).
- Namespaces, and the replication requests inside them.
- Domain labels: what an operator has called this node's domains (§10.7). Desired state
  written by a *user* about a node, which is why it does not live under the node's registration —
  that key is written by the agent.
- Operator policy: provider preferences, port ranges, bandwidth budgets.

**Observed state — reported by agents, high-churn, cheap to rebuild.**
- Which agents are alive (leased).
- Per-node domain list and per-domain flow list, with flow definitions, group hints and the
  coarse `producing` liveness state (§6, §11.1).
- Per-session status, including epoch, `target_info` and the bound port.

**Derived state — a pure function of the two above, recomputed, never authoritative.**
- Paths (dedup + refcount over requests).
- Session assignments per agent.
- Request status.

The three live under separate top-level key prefixes, so they can carry different compaction
and backup policy (§8.3) and a watch can be scoped to one of them:

```
/state/desired/nodes/<node>           registration: attachments, versions, areas, port range
/state/desired/namespaces/<name>      request partitions and their rules (§9.3)
/state/desired/requests/<ns>/<name>   replication requests, named within a namespace
/state/desired/domains/<node>/<name>  operator labels on this node's domains (§10.7)
/state/desired/policy                 operator policy: provider order, budgets
/state/observed/leases/<node>         leased — instance uuid, agent version
/state/observed/inventory/<node>      leased — full snapshot (§9.2)
/state/observed/status/<node>         leased — full snapshot of sessions actually running
/state/derived/sessions/<session-id>  session record: negotiated interface config
/state/derived/assignments/<node>     written only by the reconciler
/state/derived/reconciler             the leader's readiness record (§7.3)
/events/paths/<path-id>               bounded event ring, one key per object (§12.1)
/events/requests/<ns>/<name>          "
/events/nodes/<node>                  "
/events/logs/<path-id>                the last failing worker's log tail (§12.2)
```

**The three layers are nested under one root, and that is structural rather than cosmetic.**
*This supersedes three unrelated top-level prefixes.* §7.3 requires the fleet snapshot to be **one**
`List`, because three lists give three revisions and a reconcile computed across a skewed snapshot
can conclude that a session both should and should not exist — and until §12.1 the only prefix
covering all three was the empty one, every key in the store. That stopped being acceptable in both
directions at once: an unscoped list drags every object's event ring and every stored log tail into
a snapshot that ignores them, on every user-API read; and an unscoped *watch* sees the events the
reconciler itself just wrote and wakes it for them.

A range built on the layer names would have been the cheaper fix and is refused. They share no
prefix but a leading slash, and two of the three do not even share their first letter, so any such
range is an accident waiting for a fourth layer — and the way it fails is the worst one available: a
layer outside the range is silently absent from every snapshot, which is indistinguishable from a
wiped store and is precisely what §4.2 exists to prevent. Nesting makes the property true by
construction, and a test asserts that every layer is inside the root and that `/events/` and
`/election/` are not.

**`/events/` is therefore outside the state model, not a fourth member of it.** It is diagnostics
*about* the three layers: nothing reconciles against it, no decision reads it, and it is listed here
only because it is a prefix an operator will see in the store.

Observed state is written **under a lease**, so an agent that goes away garbage-collects itself
and the server never has to distinguish "this node reported nothing" from "this node is gone".
Desired state is never leased: a registration outlives the agent that made it (§7.1), and a
request is durable user intent (§11).

Observed state must never be trusted to survive a server restart, and desired state must never
be mutated by an agent report. Everything the server writes into an agent's assignment is
derived; the one piece of derived state that must be *stable* rather than merely recomputable
is discussed in §7.3.

### 4.1 Level-triggered reconciliation

Both sides are reconcilers, not RPC state machines:

- **Agent**: "here is my assigned worker set with its epochs; make the set of processes I am
  running match it." It never receives a "start this" or "stop that" command, only a full
  desired set for that node. See §4.2 for the one way this rule can be read fatally wrong.
- **Server**: "here is the desired path set and the reported inventory; compute the assignment
  set." Recomputed from scratch on every relevant change.

Every operation is idempotent and every message carries full state for its scope. A crash on
either side loses nothing but time. This is what makes the epoch handling in §5 tractable and
what makes HA (§8.2) possible at all.

### 4.2 Fail-static: nobody reconciles against an answer they did not get

The agent's rule above is dangerous as literally stated, because **a failed poll and an empty
assignment set look identical**. The naive implementation reconciles to zero when it cannot
reach the server — that is, the control plane going down stops all media. For a system carrying
live video that is exactly backwards.

**Invariant: the agent acts only on an assignment set it successfully retrieved.** A poll
failure changes nothing. There is no timeout after which the agent gives up and tears workers
down. A server outage of any duration leaves running sessions running.

"Empty" must therefore be a value the agent can only *learn*, never *infer*. In code that means
the reconcile function takes an assignment set, and the poll path either produces one or
produces an error that skips the reconcile entirely — the two must not be able to collapse into
the same call. Structurally: a unit's context descends from the agent's, not from the poll
loop's, so a cancelled poll cannot take workers with it, and re-registration cancels the
session's loops while leaving every worker exactly where it is.

Consequences accepted deliberately:

- **Workers that fail during a partition stay failed.** The agent still runs its local restart
  loop, but a session that needs a new epoch delivered to its peer cannot recover until the
  server is reachable. This is the correct trade: a partition degrades *recovery*, not *steady
  state*.
- **A long-partitioned agent reconciles hard when it returns.** Assignments may have changed or
  been withdrawn while it was gone, so reconnection can glitch flows. That is a deliberate
  reconcile against fresh truth, not silent divergence.
- **An expired lease is not proof that a node's workers stopped.** The server does not reassign
  a session elsewhere on that basis.

#### The same discipline on the server, or the agent's guarantee is hollow

Fail-static protects the agent from *no answer*. It does not protect it from a **successful
answer that is empty** — and the server has three ways to produce one. All three are closed:

1. **Settling** (§7.3). While the server has desired state and no observed state, the
   assignments endpoint returns an explicit not-ready status, and the agent treats not-ready
   *exactly* like a poll failure. Not-ready is not representable as an empty set at any point in
   the pipeline.
2. **A wiped store.** If etcd is restored from an empty backup or has its prefix wiped, every
   agent would poll successfully, receive an empty set, and correctly tear down every worker in
   the fleet. Two things stop it: the reconciler refuses to act while leased agents have
   reported no inventory at all, and no replica serves an assignment set until the leader has
   published its readiness record — with no record, every agent gets not-ready. An ordinary
   server restart is already safe by a different route: assignments are not leased, so they
   survive and the restarted server serves the same set it served before.
3. **A node that stopped reporting.** Observed state is leased, so an agent that stops
   heartbeating reports no inventory and no status — and every naive reading of that says "no
   flows, no sessions, tear it down", including, subtly, a group-hint selector expanding to zero
   paths. Paths touching a node that is not live are therefore **frozen**: their sessions are
   retained and their assignments carried forward verbatim from the store, and the reconciler
   reports that it engaged so the loop can log it.

The rule underneath all three: **"no observation" is never "nothing there"**, and the mechanism
is always freezing rather than converging.

---

## 5. The pairing protocol

The hard part of the system. Read WRS §4 before this section.

### 5.1 Why it is hard

`target_info` is a serialised set of **RDMA memory-registration keys for one specific process's
specific memory mappings** (`docs/third_party/mxl/Fabrics.md` §4.3). Consequences:

- It is invalidated by *any* target-worker restart. A stale blob does not reconnect — it points
  at rkeys that no longer exist.
- Target workers restart routinely. Restart is the worker's only recovery mechanism, and it
  self-terminates on a no-grain timeout — configurable since §15, so that an *idle* source no
  longer triggers it, but a genuinely failing link still cycles.
- Therefore the pairing is inherently stateful, and the **server is on the critical path of
  every re-establishment**, where a peer-to-peer design had the two ends talking directly.

The retired proxy enforced this by shipping `target_info` on every 9 s keepalive and tearing the
pairing down when it changed. That is edge-triggered and does not survive being routed through a
store.

### 5.2 Epochs

**Settled: the epoch is owned by the target side, which owns the fragile resource, and it is a
content hash rather than a counter.**

The destination agent computes an `epoch` per target worker instance and reports
`(session_id, epoch, target_info, bound_port, node_address)` as observed state.

An initiator assignment is keyed `(session_id, epoch)` and carries the `target_info` for exactly
that epoch. The source agent's reconcile rule is a single line:

> If the epoch I am running for session S differs from the epoch I am assigned for S, tear down
> my initiator worker and start a new one with the new `target_info`.

No keepalives, no change-detection RPC, no teardown negotiation. The target restarts, the epoch
changes, the change propagates through observed state, the initiator converges. The same rule
handles server restarts, agent restarts and network partitions with no extra code.

#### What the epoch is

```
epoch  = "<nonce>:<sha256 hex>"
digest = sha256( format tag
               ‖ incarnation_nonce
               ‖ fabricAddress
               ‖ region count ‖ for each region: addr ‖ len ‖ rkey
               ‖ bounceBufferInfo.entryCount ‖ bounceBufferInfo.entrySize )
```

`incarnation_nonce` is a random value the agent generates **once per target worker start**
(`crypto/rand.Text()` — 128 bits, base32, no colon so the separator is unambiguous), held in
memory alongside the process handle. Everything else comes from the `TargetInfo` JSON.

**Settled: the nonce is carried as a plain prefix, and is also inside the digest.** *An earlier
formulation defined the epoch as a hash over the nonce and separately claimed the initiator can
recompute it from the blob it received.* Those cannot both be true — the initiator holds the
blob but never the peer's nonce — so under the literal formula the self-validation property does
not exist. The prefix restores it with no extra field on the wire and no plumbing through the
server. The nonce is not a secret; it is an incarnation discriminator.

A content hash is preferred over a monotonic counter because the initiator's reconcile rule is
an *equality* test, not an ordering test — it never needs to know which epoch is newer, only
whether the one it is running matches the one it was assigned. That buys three properties a
counter does not have:

- **The agent stays stateless.** There is no counter to persist, and none needs to survive an
  agent restart, because an agent restart implies a worker restart implies a fresh nonce implies
  a new epoch implies the initiator reconnects — exactly the desired behaviour (§6.1).
- **It is self-validating.** `Verify(assigned, blob)` recomputes the epoch from the blob the
  initiator received and confirms it matches the one it was assigned, catching a mismatched or
  truncated `target_info` before it is handed to a worker that would silently fail to move data.
  It answers that question *only*: a pair that agrees but is stale verifies happily, and
  noticing staleness is the reconcile loop's equality test. Conflating the two would be a real
  bug, so the distinction is pinned by a test.
- **It cannot desynchronise.** A counter that resets, or is bumped on a path that does not
  actually change the registration, produces either a missed reconnect or a spurious glitch.

The flapping signal a counter would have given is recovered by counting epoch *transitions*
server-side (§12).

#### Why the nonce is the mechanism, not belt-and-braces

Hashing the `TargetInfo` fields alone looks *nearly* sufficient, but the guarantee it needs —
that some hashed field always differs across a target restart — is not promised by anything, and
measurement is worse than the argument suggested. Two consecutive incarnations of the same tcp
target on the same port produced:

| field | across restart |
|---|---|
| `fabricAddress` | **identical** — it encodes `127.0.0.1:24999`, and the agent reuses the port by design (§7.4) |
| `regions[].addr` | **identical, and always `"0"`** |
| `regions[].len` | identical |
| `regions[].rkey` | different |
| `id` | different, but **not hashed** |

So on tcp `addr` is not an address at all — the provider reports `0` for every region, carrying
no entropy whatsoever — and the only varying hashed field is the rkey, which nothing
contractually promises will vary. The failure mode if they all collide is the worst one in the
system: an initiator running happily against a dead endpoint, moving no data, with nothing
reporting an error. The nonce costs one random string and one concatenation and removes the
possibility entirely. The content fields are kept as well — they cost nothing and they make the
mechanism self-documenting about what it protects against.

#### Field selection

`fabricAddress`, `addr` and `rkey` identify the endpoint and the remote memory registration. Two
more are included deliberately:

- **`regions[].len`** — a shrunk region turns into an RDMA protection error rather than
  corruption, since the NIC bounds-checks against the memory region. A clean failure, but a
  restart loop is a worse way to discover it than a reconnect.
- **`bounceBufferInfo`** — this one matters. The initiator computes scatter-gather offsets
  *within* the bounce buffer ring from `entrySize`/`entryCount`. A stale value puts writes at the
  wrong offsets inside a correctly-registered region, so the NIC sees nothing wrong and the
  target unpacks garbage into the audio flow. It is the one field whose omission causes silent
  data corruption rather than a visible failure. Continuous (audio) flows only — a discrete flow
  has no `bounceBufferInfo` at all, so the field is genuinely optional rather than zero-valued.

`id`, `provider` and `addressFormat` are not hashed: `id` is an endpoint identifier of
unspecified derivation, and a change in either of the other two only ever arrives with a new
session, because the server assigns the provider (§10).

The digest is domain-separated by a format tag and frames the region *count* explicitly, so
regions cannot be regrouped without changing it. A golden test pins the output: the framing is a
wire contract between two agents that may be on different builds during a rolling upgrade, so
changing it is a breaking protocol change and needs an `api.ProtocolVersion` bump, not just a new
tag.

#### Coupling to mxl-fabrics

The `TargetInfo` structure is not part of MXL's public API. Reaching into it is acceptable here
only because this project and mxl-fabrics have the same maintainer — but that makes it a coupling
that has to be *recorded on both sides*, or someone will refactor `TargetInfo` and silently
change epoch semantics with no build failure anywhere.

Two guards, both required:

1. A comment at the `TargetInfo` definition in mxl-fabrics naming this project as a consumer of
   the field set. **Still outstanding** — it is not in this tree, and it is the other half of the
   guard.
2. **An unknown-field check on the parse side here.** `Decode` reports anything unrecognised,
   including nested paths (`regions[1].x`, `bounceBufferInfo.x`). When a field is added upstream,
   this project logs it instead of silently omitting it from the hash. It *warns* rather than
   fails — an unknown field is far more likely to be additive and harmless than epoch-relevant,
   and failing closed would take out replication on an unrelated upgrade. The exception is a
   missing `id`, which mirrors the worker's own check (WRS §4) and fails: a truncated blob then
   fails in the agent with a clear message rather than in a worker restart loop where it looks
   like a fabric problem. The regression fixture is a real blob captured from mxl 1.1.0-rc1, not
   a synthetic one — a synthetic fixture would prove nothing about the coupling the guard exists
   to watch.

Integer fields arrive as decimal strings past `MaxInt64` (`"rkey": "17918262359965949928"`) and
are hashed as their parsed 64-bit value, not as text, so a library that stops quoting them cannot
move an epoch on its own.

### 5.3 Establishment sequence

1. Server computes that path P should exist and no session realises it. It creates session S
   with no epoch yet, status `ESTABLISHING`.
2. Server assigns to the **destination** agent: `{session: S, role: target, area name + domain
   elements (resolved by the agent, §10.6), flow_def: <from inventory>, interface: <negotiated
   provider, caps flags and maxMessageSize, §10.3>, no_net_lat_measure, idle and connect
   timeouts, sched_prio}`.
3. Destination agent resolves and `MkdirAll`s the domain path (the worker does not create it),
   allocates a service (§7.4), generates an incarnation nonce, writes `config.json` into a
   **fresh** work directory, execs the worker, and waits for `target-info.json` to appear — via
   inotify on the work directory, not a backoff poll (§6.1).
4. Destination agent computes the epoch (§5.2) and reports `{session: S, epoch, target_info,
   port, state: READY}`.
5. Server assigns to the **source** agent: `{session: S, epoch, role: initiator, domain, flow_id,
   target_info, peer: <node address and service>, interface: <the same negotiated config given to
   the target>, no_net_lat_measure, timeouts, sched_prio}`.
6. Source agent recomputes the epoch from the `target_info` it received and checks it against the
   assigned epoch, then starts its initiator worker and reports `{session: S, epoch, state:
   ESTABLISHING}` — then `PAUSED` or `ACTIVE` per §11 once the destination head index is
   observed.

Steps 2–4 and 5–6 are two independent reconcile passes on each agent. Nothing here is a
handshake.

**Ordering is mandatory, not an optimisation.** The initiator's `openFlow` fails outright if the
flow does not exist, and its connect loop waits for the target to appear. An initiator is never
assigned before an epoch has been reported.

### 5.4 Session identity

**Settled: session identity is `(path, flow-def hash)`, and the path ID excludes the
definition.** A flow deleted and recreated with a different definition makes the destination's
local flow wrong, so the session must be rebuilt rather than repaired; keeping the *path* ID
independent of the definition means a republished flow rebuilds the session while staying the
same path, so its refcount, its request associations and its history do not reset underneath the
operator. `mxl-utils` gives the primitive for detecting the republish on the source side:
`Flow.IsValid()` compares the inode behind the mapping with the inode on disk and returns false
when a flow was replaced under the same ID.

The path identity is `(src node, src domain, flow-id) → (dst node, dst domain)`, where a domain is
its fleet-wide name `<area>/<elements>` (§10.6). *This used to carry the resolved output root as a
separate term*, because a session's target worker writes into a directory derived from it and a
destination that moved to another root had to be a different path rather than the same one
relocated. The area is now the first segment of the name, so the term is redundant rather than
dropped: `fast/ingest` and `bulk/ingest` are already two identities.

What that changes, deliberately: repointing an area's **directory** while keeping its name does
*not* re-identify a path, where moving a domain to another **area** still does. The first is an
operator relocating a mount, which every path through it should survive; the second is an operator
choosing a different destination, which is a different path by any reading (§10.6).

Session IDs are derived deterministically from that identity rather than allocated, which is what
lets a server restart or a leader change recompute the same ID and adopt the running workers
instead of orphaning them (§7.3).

### 5.5 Matched settings

`no_network_latency_measurement` **must match on both ends** or the target reports garbage latency
with no error (WRS §5.3). The server configures both ends of every session from one place, so it
is a session-level field rather than per-side config.

The same applies, more strongly, to the **negotiated interface config** — provider, caps flags
and `maxMessageSize`. The library performs no negotiation of its own and requires both ends to be
given identical values (§10.3), so these are session-level by necessity, not by preference.

The idle and connect timeouts are session-level for the same reason (§11.1): a value two nodes
could disagree about is a bug rather than a configuration choice, so the knobs live on the server
and are written into both halves of every assignment.

---

## 6. Agent

One agent per node. Responsibilities:

**Discovery** — via `github.com/jonasohland/mxl-utils`. Recursive scans of the node's **readable
areas** are the whole of it: every domain this node has is one the discoverer found under one of
them, or one the reconciler materialised (§10.6). Discovery is **not** pruned — a domain this
project writes into is discovered like any other, and the guard against replication feeding itself
lives on the flow rather than on the directory (§10.7).

Discovery never grants write authority. Reading and writing are separate grants on an area, and a
destination is resolved from configuration rather than from anything a scan produced (§10.6).

**Settled: domains are discovered, never configured. The agent has no name→path mapping at all.**
*This supersedes two earlier positions — that `-m` maps input domains, and that registration
advertises those mappings while discovery reaches the server through inventory.* The flag was
doing two jobs at once: granting the agent authority to read a directory, and giving that directory
a fleet-wide name. Those separate cleanly. The grant stays node-local as an area's `read` bit
(§10.6); the naming moves to the API as domain labels, where it becomes runtime state an operator
can change without restarting an agent (§10.7).

What that buys is recorded in three places rather than here — §10.6 loses an exception and a
rejection code, §10.7 has the argument, §16 records the config compatibility it costs — but the
operational half belongs with §6.1: adding or naming a domain used to require an agent restart,
and an agent restart re-establishes every flow on the node. Naming a thing should not interrupt
media.

Two consequences follow immediately. **Registration carries no domains**, because there are none to
carry; a node's domains are purely observed, and reach the server through inventory. And the
`Configured` flag on a domain is gone rather than merely descriptive — it was the security bit
before areas carried their own grants (§10.2, §10.6), and there is now nothing for it to
distinguish.

**Settled: a domain is named `<area>/<elements>`, and that is its identity for life.** *This
supersedes "an input domain is named by its path".* It needs a fleet-wide name; the area supplies
the part that is a decision the operator already made, and the elements supply the part the
filesystem already decided, so nothing has to be invented and nothing has to be stored — which
matters because the agent holds no persistent state (§6.1) and a synthetic ID would have nowhere to
live. The objection the path spelling had to answer — that a name which looks like a path invites an
agent to treat it as one — is no longer constructible: `/etc` is not a name in this grammar. The
structural answer stands anyway, and is unchanged: the inventory lookup is a map lookup with no
fallback, and a destination is not resolved against inventory at all (§10.6).

Labels do not touch this. A label is annotation and a domain's identity is its name, permanently —
which is what makes relabelling free, since the identity is embedded in path identity (§5.4),
session identity and the `domain` metric label, and a rename would re-establish every session
through the domain on a metadata edit (§10.7).

**Inventory reporting** — for each flow: ID, domain name, the full `FlowDefinition` from
`flow_def.json`, the parsed group hint (§9.1), a coarse `producing` liveness state (§11.1), and a
`replicated` boolean. The definition is not optional: the destination worker cannot create its local
flow without it (§5.3 step 2). Definitions travel verbatim, so a field this project does not model
reaches the destination unmodified.

`replicated` is true exactly while one of this agent's own target workers is writing that flow, and
it is what keeps a label selector from matching this project's output (§10.7). The agent cannot be
wrong about it — it is the process that started the worker — and it is low-churn by construction,
changing only when a target starts or stops, so it costs the compare-before-send discipline below
nothing. It is reported to operators as well as consumed by the matcher — it reaches
`GET /v1/flows` and `describe domain`, because a selector that silently skips a flow is otherwise
undiagnosable (§9.1).

`producing` is **hysteretic and coarse**, never a raw head index (§11.1): inventory is a full
snapshot written to the store, so a field that changed every frame would make every snapshot
differ and turn inventory into a per-heartbeat write stream.

**Settled: compare-before-send on both reports, and it is not an optimisation.** The server
writes an inventory or status snapshot without comparing it, and every store write advances the
revision and wakes every watcher — including every agent's assignment long poll, where a spurious
wakeup costs a reconcile. An agent that reported unchanged snapshots on a timer would be the
highest-volume writer in the fleet and would keep every other agent's poll loop spinning. Both
snapshots are deterministically ordered for exactly this reason, and the cache is dropped on
re-registration, because observed state is leased and a new lease may have outlived the old keys.

**Worker supervision** — per-instance fresh work directory holding `config.json`, `metrics.sock`
and (targets only) `target-info.json`; JSON config generation; `SIGTERM` with a grace period;
restart with backoff. A work directory is never reused across restarts: the worker does not
unlink a pre-existing metrics socket before binding, so a leftover file from a `SIGKILL` would be
a fatal `EADDRINUSE` (fixed in the worker as well, §15, but the discipline stands). Stale work
directories are swept at agent startup.

**Settled: one supervision goroutine per worker, and reconcile never blocks on a start.** A
target's start includes waiting for `target-info.json`, the one part of establishment with an
unbounded-looking wait in it — and, since §6.3, a wait for a start permit as well, which is the
same property being relied on a second time. Doing that inline would mean a target that never comes up stops the
node from noticing any *other* assignment for as long as the timeout — including the one that
would withdraw it. A supervision unit owns the whole lifecycle (start, wait, classify, back off,
restart) and reconcile only decides membership. Stops *are* synchronous, because the caller is
usually about to start a replacement for the same session and two workers overlapping would hold
one service name and write into one flow; stops run concurrently with each other, so a fleet-wide
withdrawal costs one grace period rather than N.

**Flow liveness observation** — `mxl-utils`' `Flow.GetInfo()` returns `HeadIndex`,
`LastWriteTime` and `LastReadTime`, and drives both `producing` (§11.1) and the `ACTIVE`
determination on the destination side (§11).

**Interface discovery** — at startup and on re-registration, exec the worker's probe mode
(§10.5) to enumerate what libfabric actually offers, join it against the configured `fabrics:`
block, and advertise only attachments present in both. This doubles as a load probe: it proves
the binary exists and its shared libraries resolve before anything is assigned. The worker's
`-v` output is captured in the same pass and reported as the node's `mxl` and `libfabric`
versions (§10.2).

**Metrics scraping** — connect to each worker's `AF_UNIX` socket, read to EOF, parse, label,
expose. See §12.

**Reporting** — the report loop is *woken*, not polled, for worker state: a target's epoch
reaches the server from here and the peer's initiator cannot be assigned until it does, so a
state change signals immediately. The periodic tick is a backstop for inventory, where nothing is
on the establishment path. A heartbeat that fails is not an event — the lease expires on its own
if they keep failing, and the next report comes back asking to re-register. Doing anything else
with a transport failure would be fail-static read backwards.

Two server answers end a session and nothing else does: `reregister` (the fleet forgot this node)
and `node_claimed` (another instance took its name). Both are logged as what they are and
**neither stops a worker**. A second claimant polls for nothing and starts nothing, and keeps
asking, because the holder may go away.

An assignment this node cannot honour — a domain it does not observe, an area it does not advertise
or does not grant `write` on, a fabric it does not advertise, a blob that fails `epoch.Verify` — is
**reported as a failed session with a reason**, not dropped.
Dropping it would leave a path in `ESTABLISHING` with nothing anywhere to explain why.

### 6.1 Agent restart and worker adoption

**Settled: on agent restart, kill and re-establish all workers. Do not attempt adoption. The
agent holds no persistent state.**

An agent restart glitches every flow on that node. This is accepted, and it is the correct trade
for four reasons:

1. **In the primary deployment it is not a choice.** The reference deployment is a DaemonSet and
   the agent execs workers as children inside its own container. A container restart — crash,
   liveness failure, image update, rolling update — tears down the PID namespace and every worker
   in it. There is nothing to adopt.
2. **The reasons to restart an agent are rare.** Its *operational* state — what is replicated
   where — lives in the API and never touches the agent process. What remains in agent config
   (node name, areas and their grants, fabric attachments, server URL, port range) is
   provisioning-level and changes when the host is built, not when a flow is routed. Restart rate
   collapses to upgrades.

   This got stronger when input domain mappings left (§6). Naming a domain used to be an agent
   config change, so it cost the glitch this section is about — and it is the one thing on that
   list an operator does while routing rather than while building. It is now an API write (§10.7).
3. **The glitch is smaller than one the system already tolerates by design.** A worker
   self-terminates on its no-grain timeout and is restarted after a delay, so a transient fabric
   failure is already a hard outage of seconds on that flow. Optimising agent restart while a
   fabric hiccup costs more is optimising the wrong end. It follows that if this glitch were ever
   deemed unacceptable, the fix is reconnect logic *in the worker*, not adoption in the agent — a
   different and much larger project.
4. **Adoption's failure mode is the worst one in the system.** A wrongly-adopted worker is an
   initiator running against stale rkeys: no error, no data, everything upstream reporting
   healthy. That is the hardest bug class here to diagnose, and it would live on a path exercised
   only during upgrades.

Nothing persists across a restart, including the epoch — a restart produces a fresh incarnation
nonce, which produces a new epoch, which is exactly the reconnect that is wanted (§5.2). No local
database, no PVC, nothing to corrupt or migrate.

**Since the glitch is accepted, it is kept short** — 1–2 s, by four mechanisms: inotify on the
work directory for `target-info.json`; long-polled assignments (§9.2), so the *peer* agent learns
of the new epoch in well under a second; target workers started immediately and in parallel on
agent start rather than serialised behind registration; and writable areas pre-created at startup,
so only the leaf `MkdirAll` is ever on the establishment path.

**The third of those is now qualified: starts are paced (§6.3), so 1–2 s is the budget for a
*flow* and not for a node.** "Immediately and in parallel" was written against a node's whole
worker set going up at once, and that is the arrangement that was found to exhaust the host. Under
§6.3's defaults an agent restart re-establishes its first workers inside the budget and the rest at
the configured rate, so a fifty-flow node is not whole again for well over a minute. That is a
worse number than this section originally promised and it is the right trade, because the failure
it buys off is not a slower restart: it is a node that cannot bring its workers up at all, which
takes the flows that were fine down with the ones that were restarting.

The restart window is also where flow provenance is briefly absent, since `replicated` is derived
from running target workers (§6). That is safe rather than merely brief, and §10.6 records why:
a flow whose target worker is not running is not advancing either, so §11.1's admission rule holds
anything that might match it in `PAUSED` and starts nothing.

Note the interaction with §2.3: on a combined node the control plane and the agent share a
process, so a server crash *is* an agent restart and costs this glitch. That is the main reason
to prefer running the control plane on nodes carrying few or no flows.

### 6.2 Configuration

Flags and YAML. What the agent owns:

- Node name (flag / env / file). Must be unique fleet-wide; see §7.1 for the collision story.
- **Areas** — name → directory, plus a `read` and a `write` grant (§10.6). `read` is where domains
  are discovered from; `write` is where replication may create them. Neither implies the other and
  both default false, so a node with no readable area offers no sources and a node with no writable
  area accepts no destinations — one default, applied per direction: access to a node's filesystem
  is opt-in. There is no name→path mapping for individual domains: naming is an API concern (§10.7).
- Fabric attachments — provider, fabric label, and join selectors: at most one naming selector,
  narrowed by any number of the rest (§10.1).
- Server URL(s), bearer token, listen address, port range, work directory.
- The hysteresis threshold behind `producing` (§11.1).
- **The worker start rate and burst** (§6.3). Node-local for the same reason the port range is:
  it describes what this host can absorb, and the server could not know the answer for a node it
  has never run on.

What the agent does **not** own: what is replicated (API state), and the session-level knobs —
worker idle timeout, long-idle teardown threshold, connect timeout, latency-measurement flag.
Those live on the server (§5.5, §11.1), because both ends of a session must agree on them and
only the server sees both ends.

### 6.3 Rate control on worker starts

**Settled: the agent admits worker starts through a token bucket, and nothing else passes through
it.** Observed in production: enough workers starting at the same instant exhausts the host, and
the workers that fail are not only the ones that were being started.

The moments that produce a simultaneous start are ordinary rather than exotic, which is why this
belongs on the agent and not in an operator's runbook. An agent restart re-establishes every flow
on the node at once and §6.1 goes out of its way to make that parallel. A node returning from a
partition reconciles hard against fresh truth (§4.2). A destination node restarting changes the
epoch of every session it terminates, so every peer initiator is replaced at once (§5.2). A large
apply lands as one assignment set. Each of these is a design property being exercised correctly,
and each of them is a herd.

**What is scarce is consumed by *starting* a worker, not by running one.** A worker coming up
mmaps its flow, registers that memory with the NIC against a pinned-page limit the whole host
shares, execs a process, binds a service and opens a metrics socket; once it is up it is the cheap
thing §14 sizes for. So the thing to spread is the transient, and the mechanism is admission
control on starts.

Three properties, in the order they matter:

- **Every start goes through it, including restarts.** A worker returning from a crash loop
  consumes the same resources as a freshly assigned one, and a fabric outage flapping N workers is
  exactly a start storm. §11.1's backoff bounds each worker's own cycle; this bounds what they do
  to each other.
- **Stops never go through it.** A withdrawal made to wait for a permit would hold a service and a
  flow open for no reason, and it would take away §6's property that a fleet-wide withdrawal costs
  one grace period rather than N. Concretely, a *queued* start must also be cancellable on the
  spot: reconcile stops workers synchronously, so a wait that ignored its cancelled context would
  wedge every other stop on the node behind a permit nobody was going to use.
- **The wait is inside the supervision goroutine.** That is §6's rule — reconcile never blocks on
  a start — arriving exactly where it was needed: a queued start delays its own worker and nothing
  else, including the assignment that would withdraw it. Had the pacing been put in reconcile it
  would have converted a start storm into a node that stops noticing anything for the duration.

#### Why a bucket and not a limit on starts in flight

A concurrency gate is the more adaptive shape, and it was the first answer: it releases a slot the
moment a start finishes, so it costs nothing on a host that is keeping up and paces itself exactly
as hard as one that is not. **It is refused because only half the workers have a signal saying a
start has finished.** A target's is `target-info.json` (§5.3 step 3). An initiator has none at all
— "ready" for it means the process is up and its connect loop has begun (§6), which is *before* it
has registered anything — so a gate would release an initiator's slot immediately and protect
nothing on the side of a session that a fleet-wide re-establishment produces just as many of.
Giving the initiator a readiness signal is a change to WRS and to the worker, which is a much
larger project than this problem justifies (§15).

The bucket's **burst is therefore the operationally meaningful knob**: it is how many workers may
be in setup at the same instant, which is the quantity the host is actually measured against. The
rate bounds the tail.

Ordering among queued starts is arrival order and nothing else. **Prioritising targets over
initiators was considered and refused**: it reads well — a target's start is what unblocks a peer
(§5.3) — but on a node reconciling in bulk the queued initiators are the peers of *other* nodes'
targets that are already up, so promoting one class delays media that is otherwise ready in order
to speed up media that is not, and a priority queue then needs a starvation rule that a bucket
does not.

#### The knobs, and which way zero points

Two, both agent-local (§6.2): starts per second, and the burst. **A rate of `0` means no limit**,
the same sentinel direction §2.2 takes for the worker idle timeout, and the direction is the
decision: a zero that meant *admit nothing* would be a typo that silently stops every flow on the
node. A burst below one is refused at parse time, because a bucket that can never hold a token
admits no worker ever — and a node that comes up healthy with every session sitting in `starting`
is a bad way to discover a configuration mistake.

**The defaults are deliberately conservative — burst 2, rate 0.5/s — and they cost real
re-establishment time.** Fifty workers is then a minute and a half, against §6.1's 1–2 s for a
flow, and that section is amended rather than quietly contradicted. The asymmetry is what settles
it: too fast takes out the whole node, including the flows that were running fine, and too slow
delays a node that is already recovering. A deployment with headroom should raise them, and the
burst is the one to raise first.

**A queued start reports that it is queued** — `starting`, with a reason saying so — rather than
sitting silent. A worker that has not been launched yet is otherwise indistinguishable from one
that was launched and is coming up slowly, which is precisely the distinction an operator watching
a slow recovery needs. The reason is set only on a start that actually waits, so the ordinary case
still produces no status change, no report and therefore no store write (§6, §8.3).

---

## 7. Server

### 7.1 Node registration and fencing

Two separate concepts, not to be merged:

- **Registration** — durable. Records that node `edge-01` exists, its verified fabric
  attachments and other capabilities (§10.2), and its areas with their grants. Survives the agent
  being down. It carries no domains: those are discovered, so they are observed state (§6).
- **Liveness lease** — observed, TTL'd. Records that an agent instance is currently holding that
  node identity.

An agent registers as `(node_name, instance_uuid)` and holds a lease. **Two agents claiming the
same node name is a real failure mode** — a copy-pasted config or a Kubernetes rollout overlap —
and it is nasty: both receive the same assignments, both start workers, they fight over service
names and produce duplicate writes into the destination flow. The lease is therefore exclusive: a
second claimant is rejected while the first lease holds, and the rejection is loud in logs and
metrics. On etcd this is a lease + CAS; on sqlite it is an expiry column and a transaction.

**A heartbeat renews the lease and writes nothing.** Rewriting the lease record would advance the
store revision several times a minute per node forever, waking every watcher including every
agent's long poll — where a spurious wakeup is a worker restart. The cost is that a node's
`LastSeen` reports when the lease was *taken*; liveness itself is the lease's existence, bounded
by its TTL.

### 7.2 Request validation

Rejected at request time rather than left stuck, and the categories are kept distinct:

*Rejectable immediately (`INVALID`, needs user action):*

- `unknown_area` / `area_not_writable` — the destination names an area the node does not advertise,
  or one it advertises without the `write` grant. **A destination is always a name inside an area
  the operator granted writing on** (§10.6). A raw path is never accepted from the API — otherwise
  the API is an arbitrary-filesystem-write primitive on every node in the fleet. A node advertising
  no writable area at all is `unknown_area` with a reason string saying so, rather than a code of
  its own.

  *This supersedes `no_output_root` / `unknown_output_root` / `ambiguous_output_root`.* The third is
  structurally unreachable now that a destination always names its area — there is no "advertises
  several and the request named none" — and the first collapsed into the second once the grant
  became a field on an entry rather than the identity of a separate table.
- `malformed_domain_name` — the destination domain is not an area name plus a list of clean path
  elements.
- `same_endpoint` — a source and a destination are the same `(node, domain)`. Reachable in the
  ordinary way now that a source may name any domain: `{name: fast/ingest}` against a destination
  resolving to `fast/ingest` on one node is exactly the self-pair this exists to catch. **It applies
  to a named source only.** A label selector that matches the destination's own domain produces the
  same pairing without anybody having written it twice, so there it is elided and the rest of the
  expansion stands (§10.7) — which is also what keeps the code decidable from the request plus node
  registrations, and therefore in this list rather than in the per-path one.

  **Checked over every `(source, destination)` pairing, and refusing on any one of them**, now that
  both ends are lists (§9.1). The message names both indices, because "source and destination are
  both edge-01/fast/ingest" does not say which of nine sources it is. Refusing the whole request for
  one bad pairing is right *here* and nowhere else: with both ends named it is a typo, and the
  author can see both halves of it in the file they just wrote.

  It also subsumes the disjointness check §10.8 wants as `overlapping_selectors`. With both ends
  enumerated, "the source and destination sets must not intersect" *is* the pairwise
  `same_endpoint` test, so the two-cycle a cross product would otherwise produce by construction is
  structurally unwritable rather than detected after the fact.
- `duplicate_source_flow` — two of the request's sources pin the same flow UUID and share a
  destination. Two initiators writing one destination ring buffer, which is `flow_conflict`'s harm
  arriving from inside a single request — and when both sources pin a flow ID it is decidable from
  the request body alone, so it belongs on this side of the partition rather than waiting for the
  fleet to produce it.

  **A separate code rather than an early `flow_conflict`**, because a code has one disposition:
  `flow_conflict` invalidates a path and tears the loser down, and giving it a second, request-time
  disposition for one decidable subcase is what the partition exists to prevent. The undecidable
  form — one or both sources selecting rather than pinning, so the collision arrives with a
  producer months later — stays `flow_conflict` and stays per path. Same shape as the
  `same_endpoint` / elision split one bullet up.
- `no_shared_fabric` / `no_shared_provider` / `no_shared_capability` — no viable interface pair
  (§10.3). Three codes rather than one, because they are three different operator problems.
- `pin_not_viable` — a pinned provider (§10.4) that is not among the viable pairs. Never
  substituted.
- `sched_prio_unavailable` — requested, but the node does not have the capability (§10.2).
- `flow_conflict` — the destination `(node, domain)` already holds that flow ID from a different
  source. Replicating two different producers into one flow ID corrupts the ring buffer.
- `loop` — real cycle detection over the per-flow graph. `A→B→C` is fine and useful; `A→B` plus
  `B→A` for the same flow is a loop, and so is `A→B→C→A`, which is the same mistake spelled
  longer. This covers cycles an operator *wrote*; cycles a selector would *grow* are prevented
  structurally instead, by a label selector never matching a flow this project writes (§10.7).

*One code left this list rather than moving between the categories: `domain_name_in_use`, which
caught two output domains sharing a name under different roots on one node. With the area in a
domain's name, `fast/ingest` and `bulk/ingest` no longer render to one address, so the collision is
unconstructible (§10.6).*

*Legitimately not an error:*

- `flow_not_found` — the source flow is not currently observed. `WAITING`; resolves by itself.
- `agent_not_leased` — an agent is not currently up. `WAITING`.
- `source_idle` — the flow exists but nothing is writing to it. **`PAUSED`, not `WAITING`** (see
  below).

**Settled: a path whose source is not producing is `PAUSED`, not `WAITING`.** *An earlier
position filed `source_idle` under `WAITING` while §11.1's own table had the long-idle state as
`PAUSED` with no workers.* Both cannot hold, and the difference between them is only whether
workers happen to be running — which is not something an operator decides differently on.
`PAUSED` says "the source is not sending", which is true in both cases and is exactly the
distinction `PAUSED` exists to draw; `WAITING` says "the flow is not visible", which is false.

**Settled: an `INVALID` request stops new sessions; it does not tear down running ones.** The
naive reading of `INVALID` is "remove it", and applied to a request whose session is already
carrying media that is a teardown triggered by a *registration* changing — an attachment that
disappeared while an agent re-probed, an area edited on the destination node. A request
is durable intent and the system never cancels one on the user's behalf (§11), so validity
governs **admission only**: an invalid request still expands, onto shadow paths that retain
whatever session exists and carry its assignments forward untouched.

The corollary is §10.4's fabric case: a session whose negotiation stops succeeding reports
**`FAILED` / `fabric_gone`**, not `INVALID`, because `INVALID` means "needs user action, never
resolves by itself" and a fabric that stopped being advertised can come back on its own.

**One code is carved out of that rule: `flow_conflict` tears the loser down.** Every other reason a
path is invalid describes *intent* that cannot currently be satisfied, and stopping media over one
is the teardown-triggered-by-a-registration-change this rule exists to prevent. `flow_conflict` is
different in kind — the harm is the running state itself, two initiators writing into one
destination ring buffer, which is the corruption the check exists to catch. Leaving both running
because neither request changed would be the rule applied past the reasoning that produced it.

That case is only reachable because a conflict need not arrive with a write: two paths can both be
established and only later become a conflict, or two reconcilers can each admit one side across a
settling window or a partition. §7.5 is where that is handled.

**Conflicts are decided by §7.5's precedence**, which is one strict order over candidate paths
rather than a rule per code. Requests sharing a path resolve conservatively: pins intersect (an
empty intersection is `pin_not_viable`, tracked separately from "pinned nothing", which would
silently negotiate freely — the substitution §10.4 forbids), the highest `sched_prio` wins, the
longest idle teardown wins, labels merge. **`describe path` names every contributing request and
the merged result** — without it, an operator who lost a pin intersection to a request their own
file does not mention has nothing to read.

**Validation is per path, not per request** — and the leg it runs over is a
`(source, destination)` **pairing**, not a destination, now that both ends of a request are lists
(§9.1). A request whose selector expands onto twenty paths, one of which conflicts, is not refused:
it reports nineteen paths and one invalid one with its reason. `POST` refuses only what is
*structurally* invalid — a malformed selector, an area name no destination advertises, a spec that
cannot expand at all. §7.3's property survives intact, since
request-time rejection and steady-state classification still run one `Compute` and now agree about
a set rather than a verdict. It is also what makes selectors usable at all: a selector's expansion
is not something its author can enumerate before submitting it, so refusing the whole request for
one bad pairing puts the author at the mercy of fleet state they did not write.

**A disabled destination is not a pairing and is therefore validated against nothing** (§9.1). It
contributes no path to refuse, no negotiation to fail and no node whose `sched_prio` could be
missing, so every code in both lists above is silent about it until it is enabled. Structural
validation is the exception and still runs over the whole spec: a malformed domain name is refused
at `POST` whether or not the entry that carries it is switched on, so nothing unspellable can be
parked in the store to fail months later on the click that turns it on.

### 7.3 Reconciler

**Settled: `Compute` is one pure function of a snapshot, and the read handlers run it too.** The
loop is `Compute(fleet, cfg) → Result` plus an `Apply` that writes the diff. `GET /v1/paths`,
`GET /v1/requests` and — the important one — the request `POST` call the same `Compute`.

That last one is what makes §7.2's validation real rather than a second implementation of it: the
POST handler builds the fleet *as it would be* with the candidate request in it, computes, and
refuses if the request comes out `INVALID`. Request-time rejection and steady-state
classification cannot disagree, including for the conflicts only visible across requests (two
sources into one destination flow, loops) which no per-request check could see. It also means a
follower replica renders exactly what the leader is doing, and it is what `?dry_run=true` runs.

The fleet snapshot is **one `List("")`**. Three lists would give three revisions, and a reconcile
computed across a skewed snapshot can conclude that a session both should and should not exist.
Keys outside the three layers are ignored rather than reported as damage, and an undecodable key
is collected as malformed rather than failing the pass: one bad key must not wedge the reconciler
for the fleet.

Session IDs are the derived state that must be *stable* across reconciles, and they are derived
deterministically from the path identity (§5.4) rather than allocated, so a server restart or a
leader change recomputes the same ID and adopts the running workers. Epochs come from agent
reports, so they need no server-side authority at all.

#### Settling: a server restart must not glitch media

Deterministic session IDs are necessary but not sufficient. On restart the server has desired
state (requests) and **no observed state**, so a reconcile that ran immediately would conclude no
sessions exist and issue fresh assignments for sessions that are already running — new nonce, new
epoch, a needless glitch on every server restart and every HA leader change.

Three mechanisms:

1. **A settling window before the first reconcile**, expressed as a small multiple of the agent
   heartbeat interval (3× by default) so it scales with the configured heartbeat rather than
   being an unrelated magic number. It ends early once every leased agent has reported. A newly
   elected leader observes the same window.
2. **Agents report their full running state**, not merely the status of things they were
   assigned. The server can therefore see a worker it never assigned in this process lifetime,
   recognise it by session ID, and adopt it.
3. **The reconciler publishes a readiness record at `/derived/reconciler`, and that is what gates
   the assignments endpoint.** Only the leader runs a loop while every replica serves
   assignments, so a follower has no local way to know whether the fleet has settled; the leader
   writes the record when it has, and every replica reads it. This is also what makes a wiped
   store say so (§4.2): with no record, no replica serves an assignment set at all. The record
   deliberately carries no timestamp or revision that changes per pass — it is watched by the
   loop that writes it, and a field that moved every pass would be a feedback loop with a store
   write in it.

During the window the server serves reads and accepts writes normally; it just does not act.
`GET /v1/paths` says so rather than reporting everything as `WAITING`, which would look like a
fleet-wide outage to anything scraping it.

#### The agent's "already correct" test

The corresponding hazard on the agent side: its "am I already running the right thing?" check
keys on **session ID, role, and the config that materially affects the worker** — domain path,
flow ID, epoch, the negotiated interface config, the matched settings from §5.5. It does *not*
diff the assignment object as a whole.

Otherwise any incidental difference — a re-derived port, a reordered JSON field, a re-serialised
`flow_def` that is semantically identical — reads as a change and restarts a healthy worker. This
class of bug passes every test and flaps in production, because the incidental differences only
appear once there are two server replicas or a store round trip in the path. It has an end-to-end
test that perturbs the assignment *on the wire* for exactly that reason.

#### Writes are fenced

Every session and assignment key is written with a CAS against the revision the snapshot was read
at, so a demoted leader computing from a stale read loses to the new leader's write rather than
fighting it (§8.2). It is optimistic concurrency rather than true fencing, and the residual case
is recorded: two leaders that both read at revision N and compute *identical* content will both
succeed, which is harmless precisely because the content is identical.

Two smaller properties the fleet depends on:

- **Every registered node gets an assignment key, including an empty one**, and the endpoint
  emits `[]` rather than `null`. Absence and emptiness are the same thing to a poll, and this is
  the one field in the system where confusing them stops every worker in the fleet.
- **The assignment set's revision is stamped at serve time**, not stored: it is the store
  revision the set was served at, and storing one would make an otherwise unchanged set look
  different every time the store moved on for unrelated reasons.

### 7.4 Port allocation

**Settled: the agent allocates, from an operator-configured range, and reports what it actually
bound.** The worker binds whatever `service` says and has no fallback. The agent has ground truth
— it can probe, and it knows what it already started — while the server cannot verify a port it
hands out, so the server stays out of a job it cannot do. The range is configurable per node so
operators can write firewall rules, and note that the fabric connection is inbound to the
*destination* node, so the range must be open there.

Allocation is stable per owner: a worker that restarts gets the same service back, which is why
`fabricAddress` can be byte-identical across a restart and why the epoch needs its nonce (§5.2).

**Settled: probe-bind only for `tcp`.** Probe-binding detects a collision with something else on
the host. For `verbs` and `efa` the service is a port in the RDMA CM's own space, a separate
table from the kernel's TCP one, so a successful TCP bind proves nothing and a failed one would
refuse a usable port — a false negative that silently shrinks the operator's configured range.
For those, and for `shm`, the allocator's own bookkeeping is the only claim made, which is sound
on a node where node-name exclusivity means there is no second agent to race with (§7.1).

`shm` needs a service too, but a *host-wide unique name* rather than a port — which is exactly
what the same range allocator produces. One allocator, one collision domain, no per-provider
branch anywhere.

### 7.5 Conflict precedence

**Settled: candidate paths are ordered by `(incumbency, UpdatedAt, id)`, and conflicts are resolved
greedily in that order.** *This supersedes "conflicts are decided oldest-first, ties break on path
ID".*

The constraint that eliminates most of the obvious answers comes first. §7.3 makes `Compute` a pure
function of one snapshot, run by follower read handlers and by a freshly elected leader with no
history, so **every term in the order has to be derivable from the snapshot alone**. No arrival
order, no "whoever held it last pass", nothing remembered between reconciles. Both terms below
qualify: request timestamps are desired state, and session records are derived state, and the
snapshot is one `List("")` over all of it.

#### Why age is not the first term

Oldest-first was justified as "a newly created request never invalidates a path that is probably
already carrying media". Note *probably*: age is standing in for incumbency, and the proxy holds
only while conflicts are caused by someone submitting a request. It inverts the moment observed
state causes one.

```
R1  created January   source {node: studio-a, group_hint: "Cam 1"} → edge-01:ingest
                      matched nothing all year — that camera was offline
R2  created June      source {node: studio-b, flow: F}             → edge-01:ingest
                      ACTIVE since June

August: studio-a's camera comes online and publishes flow F.
```

R1 now expands onto a path carrying F into `edge-01:ingest`, and oldest-first hands it the path —
stopping two months of flowing media because a producer in another studio was switched on. That is
precisely the outcome the rule was written to prevent, produced by the rule.

#### The terms

1. **Incumbency**, and it keys on the **derived session record**, not on whether workers are
   running. The record is not leased, so it survives a worker restart, an epoch change and a node
   freeze; keying on observed worker status instead would flip the winner on every target restart,
   which is a feedback loop with a store write in it — the shape §7.3 refuses for the readiness
   record. It also composes with §4.2 at no cost: a frozen node retains its sessions, stays
   incumbent, keeps its territory through a partition, and reconciles back into shape on return.
2. **`UpdatedAt`**, not creation time, adopting the argument the namespace rule already uses: it
   puts the refusal in front of whoever typed. Creation time would let an old request be edited to
   swallow a newer one's path, refusing nothing at the `POST` that caused it and flipping an
   untouched request to `INVALID` instead.
3. **The path ID**, so every replica agrees on an otherwise arbitrary tie.

The first term only ever decides conflicts that arrived on their own — where nobody typed, and
`UpdatedAt` is therefore arbitrary. The second only ever decides conflicts between two paths that
are both new. They do not compete.

#### Reporting

A path that loses carries the identity of the path that beat it, not only a reason code. A path that
went `ACTIVE` → `INVALID` overnight with nobody applying anything is otherwise undiagnosable, and
the ordering above makes that a supported occurrence rather than a bug.

The message distinguishes **losing to another request** from **two paths of one request colliding**.
The second becomes reachable with source domain selectors (§10.7): the same flow ID may legitimately
exist in two domains on one node (§3), so one selector matching both produces two paths into one
destination flow. Admission cannot catch it, since the second domain may be discovered or labelled
months later, and the two cases send an operator to completely different places — narrow your own
selector, versus talk to whoever owns the other request.

**Fan-in makes the second case routine rather than reachable** (§9.1), and it exposes a property of
the order worth stating: two paths of one request share an `UpdatedAt`, and neither is incumbent
before either is established, so the tie falls all the way through to the path ID. That is
deterministic across replicas and stable over time, which is what the order has to be — and it is
*arbitrary* from the operator's point of view, which no amount of ordering can fix, because nothing
in the request says which of two sources of one flow ID was meant. The decidable form of that
mistake is refused at `POST` as `duplicate_source_flow` (§7.2) precisely so the arbitrary tiebreak
is reached only by the cases that could not have been caught earlier. When it is reached, the
message names **both sources**, not the winner alone.

`mxl_repl_path_conflicts{reason}` joins the leader's fleet gauges (§12). A conflict that resolves
quietly is a request whose expansion silently shrank.

#### Conflicts are never sticky

Because resolution is recomputed rather than recorded, a loser establishes on its own as soon as the
winner's request is deleted or its flow disappears. There is no suppression bit to store, nothing to
unwind and nothing to garbage-collect. That property is worth naming because it is what pays for the
purity §7.3 insists on — and it only holds because incumbency keys on something stable, which is the
whole of the argument for the session record above.

---

## 8. Storage

### 8.1 One interface, two backends

Supporting etcd and sqlite behind one interface is the classic place a project like this gets
stuck. etcd gives you revisions, CAS, watch and leases; sqlite gives you transactions and none of
the other three. Design to sqlite and HA becomes impossible; design to etcd and the sqlite
backend must emulate.

**Settled: define the interface in etcd's terms — a revisioned KV store with CAS, prefix watch
and TTL leases — and implement it over sqlite.** The alternative, a domain-level interface with
two hand-written implementations, avoids the emulation but duplicates the reconciler's
consistency logic twice over, and the agent long-poll (§9.2) wants a revision cursor either way.

The emulation is genuinely small, and the four decisions in it are worth knowing:

- *revision*: a monotonic integer column, bumped in the same transaction as the write.
- *watch*: **reads forward through an append-only history table**, rather than polling current
  state. Polling current state cannot report a delete or an intermediate value, and the watch is
  what the long poll rests on.
- *cursor*: **`(revision, seq)`, not a revision.** One revision can carry several events, so a
  revision-only cursor either replays or skips the tail of a multi-event commit.
- *compaction*: history is bounded and `ErrCompacted` is part of the interface — a watcher that
  fell too far behind is told so rather than silently missing events. The etcd backend does not
  compact; etcd's own compaction policy owns that.
- *lease*: an expiry-timestamp column plus a sweeper timer.
- Watches are **woken by the committing transaction**; the poll interval is only a backstop.

The conformance suite is what keeps the abstraction honest: it is written against sqlite and must
pass **unchanged** against etcd. If it does not, the interface is wrong, and that is precisely
the leak this section is worried about.

The store layer stores bytes. It does not know what a request or an assignment is, and it imports
nothing from `internal/api`: serialisation, validation and the meaning of any particular key
belong to the server.

### 8.2 HA

Two requirements follow from "deployable behind a simple third-party HTTP proxy":

**No sticky sessions.** Every replica must be able to serve every agent request. This is the
argument for polling over server-push streams in §9 — an SSE or gRPC stream lands on one replica,
and state written by another replica has to be watched and fanned out to reach it. Polling with a
revision cursor makes every request self-contained.

**One reconciler.** If every replica reconciled, they would fight: CAS retries, assignment thrash
and duplicate epoch decisions. etcd's concurrency package elects a leader; the leader runs the
reconciler, every replica serves the API. Election runs on the store's own client under the
deployment's prefix. On sqlite there is one process, so it is always the leader.

Three hazards a single server cannot exhibit, each closed:

- **Revision cursors must not regress across replicas.** Behind a plain load balancer,
  consecutive polls from one agent land on different replicas. If replica B's view lags behind
  the cursor the agent obtained from replica A and B answers from its stale view, the agent
  oscillates between two assignment versions and restarts workers on each swing — and §7.3's
  "already correct" test does not help, because the sets genuinely differ. **A replica never
  serves an assignment set at a revision below the client's cursor**; if its local view is
  behind, it waits.
- **A demoted leader must not keep writing.** A partitioned old leader can believe it still holds
  leadership until its lease expires, so every derived write is a CAS on the revision it read
  (§7.3) rather than relying on election alone to serialise writers.
- **Leader churn.** A combined instance runs etcd keepalives, lease renewal, a store watch, held
  long polls and the metrics scrape loop in one process. On a node whose workers run `SCHED_FIFO`
  via `sched_prio`, RT threads can starve the Go runtime; a missed keepalive is a lost
  leadership, and leader churn means repeated settling windows and reconcile thrash. Raise
  `GOMAXPROCS` for the combined role, keep the control plane off nodes running RT workers where
  possible, leave headroom in `sched_rt_runtime_us`, and watch the leader-change metric — frequent
  changes are the symptom and they are otherwise invisible.

### 8.3 Write volume

Worth sizing, since `target_info` churn flows through the store. A blob is roughly 1–2 KB and is
written once per target-worker start. Steady state is ~zero writes: heartbeats write nothing
(§7.1), unchanged snapshots are not sent (§6), and re-applying an unchanged request writes
nothing (§9.1).

Two things that would otherwise dominate are handled in §11.1, and this section is only correct
because they are:

- **Idle sources do not churn.** A configurable idle timeout means a paused session's workers
  wait quietly instead of self-terminating every 10 s, and the admission rule means a dormant
  flow never starts workers at all. Without those, every idle-but-requested flow would write
  ~1.5 KB every 13 s indefinitely — normal operation, not a pathology.
- **Genuine failures are bounded by backoff.** A fabric outage with N flows flapping is capped by
  the agent's restart backoff stretching toward minutes, not by the worker's flat cycle.

Observed state is leased so it is garbage-collected automatically, and it sits under its own key
prefix so the three layers can carry different compaction and backup policy (§4).

---

## 9. API

**The user API and the agent API live under distinct path prefixes** (`/v1/...` and
`/agent/v1/...`). They have different auth, different clients, different rate profiles and
different compatibility guarantees, and an operator must be able to expose one at an ingress
without exposing the other.

### 9.1 User API

```
POST   /v1/namespaces/{ns}/requests      create or update, keyed on the name within {ns}
                                         ?dry_run=true validates and reconciles without writing
GET    /v1/namespaces/{ns}/requests/{name}
DELETE /v1/namespaces/{ns}/requests/{name}   cancel; path torn down only when refcount hits 0
GET    /v1/requests[?namespace=]         fleet-wide list, with status

GET    /v1/namespaces                    list
POST   /v1/namespaces                    create or update, keyed on name (§9.3)
GET    /v1/namespaces/{ns}
DELETE /v1/namespaces/{ns}               refused while any request references it

GET    /v1/nodes                    registered nodes: liveness, capabilities (§10.2), areas
GET    /v1/nodes/{node}
GET    /v1/nodes/{node}/domains     observed domains, with their labels (§10.7)
POST   /v1/nodes/{node}/domains     label one `(node, domain)`: an apply (full declared map) or
                                    a patch (keys set, keys removed) — §9.1
                                    ?dry_run=true reconciles and writes nothing (§10.7)
GET    /v1/flows                    fleet-wide flow inventory, filterable; carries `replicated`
GET    /v1/paths                    derived state: paths, sessions, per-session status

GET    /v1/paths/{id}/events        what happened to this path (§12.1)
GET    /v1/paths/{id}/logs          the last failing worker's log tail (§12.2)
GET    /v1/namespaces/{ns}/requests/{name}/events
GET    /v1/nodes/{node}/events
```

The three `events` reads and `logs` are the only user-API reads that do **not** run `Compute`
(§7.3): a ring is one `Get` on one key, so they are O(response) where every other read here is
O(fleet). That is deliberate rather than incidental — the event log is the endpoint a UI polls
fastest, and it is polled hardest exactly when the fleet is least healthy (§12.1).

`GET /v1/nodes/{node}/domains` reports **observed** domains rather than registration data, since
there is no configured mapping to report (§6), and it joins each one against its label record so
that a labelled domain this node does not currently observe is still listed — that is how an
operator sees a label they applied before the producer came up (§10.7). Domains this node is
replicating *into* are listed like any other, since a domain is a place rather than a direction
(§10.6); each flow carries whether this node is the one writing it.

Two properties of that join, both of which are the *reading* half of §10.7's "accepted and inert":

- **It carries the `settling` flag `GET /v1/paths` does** (§7.3). The join is inventory-dependent,
  so during the window it would otherwise render every label with nothing observed beside it, which
  looks exactly like the labels having been lost.
- **It answers for a node with no registration at all**, listing the label records alone. A label
  write does not validate its node against the fleet (§10.7), so refusing the read would make a
  typo'd node name in a manifest a write that can never be read back — the one shape of mistake
  where an inert record has nowhere to be noticed.

Request body:

```json
{
  "namespace": "nab",
  "name": "cam1-distribution",
  "sources": [
    {
      "node": "studio-a",
      "domain": { "labels": { "role": "cameras" } },
      "select": { "flow": "5592a23b-0974-45bb-9388-89ea81c42537" }
    },
    {
      "node": "studio-b",
      "domain": { "name": "media/cameras" },
      "select": { "all": true }
    }
  ],
  "destinations": [
    { "node": "edge-01", "domain": { "area": "fast", "elements": ["ingest"] } },
    { "node": "edge-02", "domain": { "area": "fast", "elements": ["ingest"] } },
    { "node": "archive-01", "domain": { "area": "bulk", "elements": ["capture"] }, "provider": "tcp" }
  ],
  "provider": "verbs"
}
```

`namespace` is a **real property, not a label** (§9.3). `sources[].domain` is a selector, not a name
(§10.7) — `{"name": "media/cameras"}` addresses one domain directly, `{"labels": {…}}` matches by
label. A source may name any domain, including one another request replicates into, which is how
`A→B→C` is written (§10.6); what a *label* selector will not match is a flow this project is itself
writing (§10.7).

`destinations[].domain` is an **area name and a list of path elements**, not a path —
`{"area": "fast", "elements": ["studio-a","cam1"]}` materialises `<fast>/studio-a/cam1` and renders
as `fast/studio-a/cam1`. A manifest writes it as `domain: fast/studio-a/cam1` and the CLI splits it
there; **nothing else in the system ever parses a domain string** (§10.6). *This supersedes a
separate `root:` field, which could be omitted on a node advertising exactly one root* — the area is
part of the domain's name now, so omitting it would be omitting half the name.

**The request's ID is `(namespace, name)`.** `POST` is create-or-**update**: a controller
re-reconciling with a changed spec expects the change applied, not a 409, and posting an identical
spec returns the existing request having written nothing. Deriving a separate ID and keeping a name
index would add a key and a consistency problem for nothing, since the name is already a required
idempotency key. The server validates the name more strictly than the wire type does (letters,
digits, `-_.:`) because it lands in URLs and store keys.

**Settled: names are scoped to the namespace, not fleet-wide.** A namespace that does not namespace
names is half the concept, and the consumer this partition exists for is the one that proves it: a
Kubernetes adapter naming requests after pods inherits Kubernetes' own namespacing, so two
identically-named pods in two of its namespaces collide here unless the adapter prefixes — and
prefixing is exactly what having a namespace should remove. Two operators both wanting a request
called `cam1` is the same case without the automation. The cost is that every request ID in a URL,
a CLI argument or a UI key gains a second component; nothing downstream is affected, since path
identity does not include the request (§5.4).

The response carries an `X-Mxl-Outcome` header — `created`, `updated` or `unchanged`. A header
rather than a body field, because it describes the *operation* and not the resource: the request
body is byte-identical whether the write happened or was skipped. The client cannot work it out
for itself — the response echoes the spec that was sent, so comparing the two says only that the
server agreed — and the status code cannot carry it either, because skipping an unchanged write
is still a 200.

**Re-applying an unchanged request must not write.** Desired state is low-churn by assumption
(§8.3), and a controller re-applying on every resync would break that assumption if an identical
spec bumped the store revision and triggered a reconcile.

#### Many sources, many destinations

**Settled: a request fans in as well as out. Both ends are lists, and a source's node stays
pinned.** The expansion is the cross product — every source's flows against every destination —
and `sources` is spelled as a list in the model, on the wire and in the manifest, with no singular
form beside it.

*This supersedes "a request fans out, not in", which made the destination side the only list and
called the asymmetry deliberate. It is kept, because it is four separate arguments and three of
them survive the reversal — as requirements, which is what the rest of this subsection and §11 are
discharging. The fourth inverts outright.* The superseded position was:

> **The source side already has a selector and the destination side cannot have one.** A
> destination is a `(node, domain)` pair by necessity. So a *source* list would only add mixing
> several origins into one request, on top of an expansion mechanism that already exists, while a
> destination list adds something otherwise inexpressible in a single request: one camera going
> to two edges and an archive.
>
> **Fan-out has shared fate; fan-in has none.** §11 aggregates a request's status over its paths.
> Every path in a fan-out request shares a source, so an idle producer moves them to `PAUSED`
> together and a republished source flow invalidates them together — the aggregate answers a
> question an operator asks. Several unrelated sources landing in one domain share nothing.
>
> **Fan-in points at the corruption case.** §7.2 rejects a destination `(node, domain)` that
> already holds a flow ID from a different source — two producers into one ring buffer. Grouping
> many sources into one destination is exactly the arrangement that makes that easy to write by
> accident. Fan-out cannot produce it.
>
> **The verbose half is the source**, and sharing it across the list makes the cost legible: one
> source to five destinations is five initiator workers reading the same local flow and 5× egress
> on that node — which is what the §13 bandwidth hook wants to see grouped.
>
> Fan-in is unaffected: several requests sharing a destination domain is the way to express it,
> and the refcount materialises that domain once (§10.6).

**The first argument is about which side carries the list, and it does not reach across nodes.**
A source's domain selector expands over the domains of **one** node, because `source.node` is
pinned and node labels do not exist (§10.8). So the thing a source list adds is precisely the
thing no selector expresses: several *nodes* feeding one destination. "Every camera in studio A,
studio B and studio C onto the ingest wall" is one intent, one name, one lifecycle and one delete,
and under the superseded rule it was three requests an operator had to keep in step by hand.

**It also survives in a form worth keeping: sources stay enumerated.** A source names its node
outright, so both ends of every pairing are written down, and that is what keeps every code in the
left-hand column of §7.2 decidable at `POST` — "studio-c and archive-01 share no fabric" names two
things the author typed. The moment a source's node becomes a selector, that column collapses into
the per-path one and the request's cost stops being readable at the moment it is written. That is
§10.8's territory and it is not entered here.

**The fourth argument inverts.** Fan-out grouped egress: one source to five destinations is five
initiator workers reading one local flow and 5× egress on that node. Fan-in groups **ingress**:
twelve sources into one domain is twelve target workers and 12× ingress on the destination node,
which is the same legibility in the other direction — and it is the *binding* direction for the
arrangement that motivates fan-in at all, since an ingest wall is bounded by what its edge can
take. §13's bandwidth hook wants both groupings; it had one.

**The second and third arguments survive as requirements**, and both are discharged rather than
argued away:

- **Shared fate is genuinely gone**, exactly as the superseded text said. A request's paths no
  longer share a producer, so one dark camera among twelve leaves the aggregate permanently
  non-`ACTIVE` and the top line stops answering the question it was folded to answer. §11 grows
  **`PARTIAL`** for this, plus a per-source breakdown. It is a rendering change and a new word, not
  a new noun — and it is not a fan-in concept: a group-hint request with three flows of which one
  is paused has always been in this state, and has always reported it as `PAUSED`.
- **The corruption case is real and becomes easy to write.** Two sources whose flows carry one ID,
  into one destination domain, is two initiators writing one ring buffer. Where it is decidable
  from the request body — two sources pinning the same flow UUID against a shared destination — it
  is refused at `POST` as `duplicate_source_flow` (§7.2). Where it is not, because one or both
  sides are selectors and the collision arrives with the fleet, it is `flow_conflict` on the path,
  resolved by §7.5 and torn down there. Fan-out could not produce either; fan-in produces both, and
  the split between them is the one new reason code this work adds.

Nothing below the request changes. A path is still `(src flow address) → (dst node, dst domain)`,
sessions are still refcounted per path, and two requests landing on one path still share one worker
pair — which is what makes this a request-level change rather than a reconciler one. Expressing
fan-in as several requests sharing a destination domain still works and still refcounts to one
session (§10.6); what a single request now buys is one unit of intent over the set.

**Validation and negotiation are per `(source, destination)` pair.** They always were per pair in
substance — an interface config is negotiated for a session, and a session has two ends (§10.3) —
and were per destination only because there was one source. With both ends lists, a request can be
viable for eleven pairings and refused for the twelfth, and the reason has to name **both** ends:
a failure that is common to every destination of one source belongs to that source, one that is
common to every source of one destination belongs to that destination, and one that applies to
every pairing belongs to the request. Naming the wrong end sends an operator to the wrong node.

A destination may carry its own `provider` pin, overriding the request-level one. Provider is
negotiated per session and therefore per `(source, destination)` pair, so with either end a list
one pin can be right for one pairing and *unsatisfiable* for another (§10.3).
`idle_teardown_ms` and `sched_prio` stay request-level: they degrade, or are rejected with a
reason naming the node, so splitting the request is the honest fix rather than an override that
hides a node's missing capability. **The pin stays on the destination and does not gain a mirror on
the source**, for the reason it was put there: a provider is unsatisfiable per *pairing*, and the
side that varies is whichever end the operator listed several of. One override on one side already
expresses everything a pin needs to say about a pairing, and a second would make "verbs here, tcp
there" ambiguous about which end wins.

`sched_prio` now has more nodes to be unavailable on — it is checked on every source and every
destination the request names, and the rejection names the node that lacks it rather than the
pairing, because the capability is a property of the host (§10.2).

#### Parking a leg: `disabled`

**Settled: a destination entry carries a `disabled` flag, and that is the only place in a request
where *off* is spelled. There is no flag on the request and none on a source.**

Until this, the desired set had no value for *off*. A route that is not running is a route that does
not exist, so taking a leg out of service for a maintenance window means deleting it and typing it
back afterwards — and the spec that described it, which somebody wrote and somebody reviewed, is
gone in between. That is not only a renderer's problem, though `ui.md` §7a is where it bites
hardest: a manifest edited down to park a route for a night and edited back in the morning is a file
whose history has stopped describing intent, and `--prune` will cancel whatever the edited-down file
no longer names.

```json
{ "node": "edge-02", "domain": {"area": "fast", "elements": ["ingest"]}, "disabled": true }
```

The effective spec is **every source against every *enabled* destination**. A disabled entry is
skipped before expansion, so it produces no pairing, no path, no session and no assignment. Nothing
below the request changes — the same property the fan-in work has, and for the same reason: this is
an arithmetic change to the cross product and not a new noun.

**Spelled `disabled`, never `enabled`.** A default-true boolean is a trap on a wire format where
`omitempty` drops the zero value: the day something round-trips a request through a marshaller that
does not know the field, an inverted flag stops every leg in the fleet and an absent one is
indistinguishable from a deliberate `false`. `disabled` defaults to the zero value in the direction
that keeps media running, which is the same reasoning §6.3 applies to a rate of `0`.

**Why the destination and not the request.** Because it is the operation that already exists rather
than a new one. Validation, negotiation and the path are all per `(source, destination)` pairing
(above, §7.2), and the documented way to stop one leg of a request is to drop the destination — which
clears that column across every one of the request's sources. Disabling *is* that operation, made
non-destructive, so the request keeps its shape and no gesture is invented. The relationship only
runs one way, which settles it: a request whose every destination is disabled asks for nothing, so a
request-level flag is derivable from these and these are not derivable from it.

The cost is an asymmetry with `sources`, put back deliberately after §9.1 spent a subsection removing
one. It is the same asymmetry `provider` already has and it is defensible on the same argument: what
varies is whichever end the operator listed several of, and one flag on one end says everything about
a pairing, because disabling either end of a pairing kills it either way.

**What a source flag would add, and why it is not built.** Exactly one thing: darkening one row of a
fan-in — "studio-b is down for the week, keep it in the request" — which today costs removing the
source and retyping it later. A single-source request needs nothing, since disabling its destinations
darkens it entirely. It is narrow, it is additive, and it re-litigates none of this if it is added;
recorded so it is a decision rather than an oversight.

**A flag on the *pairing* is the one shape that is refused outright.** `disabled` on a
`(source, destination)` cell would stop a request being sources × destinations and make it an
arbitrary bitmap over the grid — a shape the expansion cannot describe, the manifest cannot spell
without becoming a matrix, and a round-trip cannot preserve. A disabled destination darkens a whole
column of the request and a disabled source would darken a whole row; both keep the rectangle
rectangular. `ui.md` §7a states the same rule from the renderer's side and calls it a notch.

Four things that deliberately do **not** change:

- **`Validate` counts entries, not enabled entries.** At least one source and at least one
  destination remain required; a request with one destination, disabled, is legal and is the whole
  point. Tightening this to require an enabled one would forbid precisely the state being built.
- **The duplicate-endpoint rule does not exempt a disabled entry.** Two entries naming one
  `(node, domain)` are still refused, even where one is off. Parking a `tcp` variant of a
  destination beside the live `verbs` one reads well and has no answer for which pin applies when
  both are enabled, which is what that rule already says.
- **`DELETE` is unchanged and is not superseded.** Disabling is not a soft delete: the request still
  exists, still holds its name, still counts against its namespace and is still pruned by a file that
  does not name it. Removing intent is still removing it.
- **Disabling stops media.** It cancels those legs in every respect except that the spec survives, so
  it carries the same blast radius as a cancellation — for each path, whether another request still
  references it (`path.requests[]`) — and `?dry_run=true` previews it identically. A flag whose write
  is cheap and whose effect is a teardown is exactly the shape §10.7 warns about for labels.

Whether an unchanged spec was written is answered as it always was: `SameAs` compares encoded JSON,
so a flipped flag is an `updated` and a re-post of the same one writes nothing.

**A disabled request is not validated against the fleet.** It asks for nothing, so there is nothing to
refuse: `unknown_area`, `no_shared_fabric` and the rest are reported when it is enabled, and
`?dry_run=true` is how an operator finds out before flipping it. Structural validation still runs
unconditionally, so a malformed domain name cannot be parked in the store to fail later. The small
cost is real and is accepted — a parked route can be broken without saying so — and the alternative
is worse: a request that is both `INVALID` and `DISABLED` has two states and one field to report
them in.

#### The source is a selector, not a flow ID

**Settled: `sources[].select` is an extensible selector.** A pinned flow ID is one kind of
selector, not the only shape the API can express.

A UUID is rarely what a user actually means. An operator means "whatever camera 1 is publishing";
the Kubernetes adapter means "everything this pod exposes". Pinning UUIDs forces both of them to
maintain their own discovery loop and rewrite requests whenever a producer republishes a flow
with a new ID — which is exactly the work this server already does.

Three selector kinds:

```json
"select": { "flow": "<uuid>" }
"select": { "group_hint": { "name": "Studio A:Camera 1", "type": "video" } }
"select": { "all": true }
```

The group hint comes free: it is an NMOS tag in `flow_def.json`, and mxl-utils parses
`urn:x-nmos:tag:grouphint/v1.0` into `Name` and `Type`. The agent reports it as part of
inventory; the server matches on it. `type` is optional — omitting it selects every flow sharing
the name, which is how you replicate a camera's video and audio together.

#### `all`, and why omission is safe here and not one level up

**Settled: `all` is the whole of a source's domain, and in a manifest it is spelled by leaving the
flow selector out.** A source that names a domain and says nothing else replicates every flow in
it:

```yaml
sources:
  - {node: studio-b, domain: media/cameras}
```

This is the retired proxy's subscription shape (§16) and it should be the cheap thing to write,
not the verbose one. It expands like a group hint rather than like a pinned flow: it reaches
whatever is currently observed, and matching nothing is a request with zero paths.

**On the wire it is a kind like any other, and an absent `select` stays an error.** Making the
zero value mean "everything" is the one thing the tagged union exists to prevent — a hand-rolled
`POST` with a mistyped key, or a record stored before this kind existed, would decode as
*replicate the entire domain* rather than failing. `internal/api/selector.go` already states the
direction that has to fail in: an unknown kind is refused rather than ignored, because ignoring it
silently *widens* the selection, and for something that moves uncompressed video between hosts
widening is the wrong way to be wrong. The manifest may default it because an unrecognised key is
an error there (§9.1), so a typo cannot reach the default; nothing else may.

**This looks like the empty label selector §10.7 refuses, and the difference is worth stating
rather than assuming.** That rule refuses a `domain: {}` on the same-sounding ground — matching
everything is "reachable by omission rather than by intent". The two are not the same gesture. An
empty *domain* selector widens the set of **places**: it matches domains that do not exist yet, on
whatever that node happens to hold, so its scope is unbounded and grows on its own. `all` selects
within **one place the operator already named**, whose contents can be read before the request is
written (`describe domain`) and cannot grow past the domain. A selector over places and a selector
within one place fail differently, and only the first can expand without anybody touching it.

The accident the analogy does point at is real: deleting a `group_hint:` line turns a one-camera
request into a whole-domain request, and the outcome header (`created` / `updated` / `unchanged`)
says nothing about a request whose expansion went from one path to forty. **`apply` therefore
prints each request's resulting path count**, which costs nothing — the `POST` response already
carries the expansion — and is the same argument §9.1 makes for `label` printing a blast radius.
A hard cap on a request's path count stays a §19 item, tied to bandwidth admission control, since
a count an operator can see is the cheap half of it.

Design rules that keep this extensible:

- **The selector is a tagged union with exactly one kind set**, not a bag of optional fields that
  are implicitly ANDed. Adding `label`, `format` or `regexp` selectors later is then additive and
  cannot change the meaning of an existing request.
- **A request owns a set of paths, not one path.** A selector expands to N paths, which changes
  as flows appear and disappear. Even a pinned-flow request is modelled as a set of size one, or
  the selector case becomes a second concept bolted alongside the first.
- **Expansion is part of reconciliation**, recomputed from inventory like everything else in §4.
  A flow appearing that matches a selector creates a path; disappearing removes it — except while
  its node is not live, where freezing applies (§4.2). This composes with `WAITING` at no extra
  cost: a selector matching nothing is simply a request with zero paths.
- **Refcounting happens at path level.** Two requests whose selectors both expand onto the same
  path share one session.

A request's status is therefore an aggregate over its paths, and the API returns the summary, the
per-path breakdown and — since both ends are lists — a **per-source** breakdown beside it. "1 of 3
active" is the answer an operator needs and it has no meaning in a one-flow-per-request model;
"studio-c is dark, the other two studios are fine" is the answer they need from a fan-in, and it
has no meaning in a one-source model. The aggregate itself is `PARTIAL` whenever the paths
disagree and at least one is `ACTIVE` (§11).

**It also returns what the expansion *excluded*, and that is not decoration.** A path that does not
exist has no status to carry a reason, so a flow a selector skipped is invisible in a
paths-only rendering — and §10.7's self-output rule skips flows deliberately, on a node that is
also a replication destination, which is precisely where an operator's broad selector will meet it.
Under the superseded rule the whole domain was absent, which was at least legible as a category;
per-flow provenance is finer and therefore *less* obvious, and this is where that cost is paid
back. Each entry names `(node, domain, flow)` and a reason, of which there is one today —
`self_output`, a flow this node's own target worker is writing (§10.6). "Did not match the labels"
is not a reason and is never listed: that set is unbounded and is the ordinary case. The list is
capped, and a truncated one **reports how many it dropped**, because a silent cap here reads as
"nothing else was excluded".

#### Requests are normally authored as a manifest

The HTTP API above is the contract, but it is not the interface an operator uses. The desired set
is written to a multi-document YAML file — one object per document, `---` separated — and
applied:

```yaml
kind: namespace
name: nab
paths: exclusive
---
kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras, name: cameras}
---
namespace: nab
name: cam1-distribution
labels: {show: nab}
sources:
  - node: studio-a
    domain: {role: cameras}
    group_hint: {name: "Studio A:Camera 1"}
  - node: studio-b
    domain: media/cameras          # no flow selector: every flow in it
destinations:
  - {node: edge-01, domain: fast/ingest}
  - {node: edge-02, domain: fast/studio-a/cam1}
  - {node: archive-01, domain: bulk/capture, disabled: true}   # parked, not deleted
provider: [verbs, tcp]
```

```
mxl-replicator apply    -f studio-a.yaml [--dry-run] [--prune -n nab [-l show=x]]
mxl-replicator delete   -f studio-a.yaml        (only the kinds and names are read)
mxl-replicator label    domain studio-a:media/cameras role=cameras role-
mxl-replicator status
mxl-replicator get      nodes|domains|flows|requests|paths|sessions|namespaces [filters]
mxl-replicator describe node|domain|flow|request|path|session|namespace <name>
mxl-replicator events   path|request|node <name> [--since <seq>]
mxl-replicator logs     path <path-id>
```

**`events` and `logs` are verbs rather than flags on `describe`** (§12.1, §12.2), on the same
reasoning that keeps `get` and `describe` apart: `describe` answers *what is this* and `events`
answers *what happened to it*. Those are different shapes — a record against a list that grows —
and an operator asks the second repeatedly while the first is stable. They are joined where it
matters: `describe path` prints the last few entries under the status, because the whole premise of
§12.1 is that a state and a reason do not explain a failure on their own.

Three read verbs with three jobs and no overlap: `status` counts the fleet and names only what is
not active, `get` lists so a name can be found, `describe` explains one thing in full. `events` and
`logs` are a fourth job rather than a fourth spelling of one of those — every verb above answers
*what is this now*, and those two answer *what happened to it* (§12.1). `describe`
takes the nouns of §3 — and keeps `path` and `session` apart, because they are separate layers
(§4): a path is derived state that outlives any particular session, while a session is ephemeral
and is re-established whenever either end restarts.

**`label` is the fourth verb, and it is deliberately not a fourth vocabulary.** §19 dropped a
separate `xpt` CLI on the argument that two spellings for one thing is worse than one, and that
argument has to be answered rather than ignored here. It is: `label` writes to
`POST /v1/nodes/{node}/domains`, the same endpoint the `kind: domain` document applies, so there is
one model and one server-side rule. What differs is only the gesture — the manifest is the desired
set an operator keeps in git, `label` is a one-shot edit of one record, and "an operator notices a
domain and names it" is genuinely interactive in a way that authoring a file is not. `key-` removes
a key, following the convention operators already have.

**The two gestures send different bodies, and that is what carries the ownership rule below.** An
apply sends the full map it declares; an edit sends a patch — keys to set, keys to remove, which is
what `role=cameras role-` already *is* on the command line. The server merges an apply against the
keys the last apply declared and merges a patch against nothing.

*This supersedes "`label` is a thin client-side read-modify-write over the endpoint"*, which
followed from the endpoint being a full-set write and stops following once it is not. Sending a
patch is strictly better anyway and the reason is worth recording: a read-modify-write on a shared
record has a lost-update race — two operators labelling one domain between the same read and write
lose one edit, silently, which is the failure mode this record's whole ownership story exists to
avoid. A patch has no such window.

**Settled: an apply owns the keys it declares. It sets them, removes the ones it declared last time
and no longer does, and leaves every other key alone — so an imperative `label` edit *does* survive
a later apply that does not mention it.**

*This supersedes "a `kind: domain` document replaces the whole label set for its `(node, domain)`",
which was reached by consistency with the request `POST` — everything else in the manifest replaces,
so this should too — and which is worth keeping because that consistency argument is the obvious one
and it is wrong for a reason that is easy to miss.* A request's spec has **one writer by
construction**: `(namespace, name)` is its ID, so the file that names it is the only thing that can
mean anything by it, and whole-spec replace is not a policy there so much as a description. A
domain's label map has no such owner. §10.6 spent an entire subsection establishing that a domain is
a shared place with no proprietor — the multicast reading — and the label map is the one record in
this design where several writers on one key set is the *expected* arrangement rather than a
collision to be resolved. Whole-set replace makes the last writer win, which is exactly the
behaviour a shared object must not have.

The rule above is `kubectl apply`'s three-way merge, and adopting it deliberately is the point: this
file format is already close enough to a Kubernetes manifest that §19's adapter is a mechanical
conversion, and an operator arriving from that vocabulary carries strong expectations about what
`apply` does to a field it never mentioned. Surprising them costs more than the consistency the
superseded position bought. Mechanically it needs one thing the record would not otherwise hold: the
key set the last apply declared, which is `last-applied-configuration` doing the one job it exists
for, on a flat map rather than a whole object.

Three consequences, each of which is a property the replace rule did not have:

- **Removing a key from the file removes it.** The file stays declarative over its own keys, which
  is what would have been lost by merging without a memory of what was declared before.
- **`label` and `apply` do not fight, because they own different keys.** The verb's job is therefore
  larger than reconnaissance: an operator may name a domain interactively and keep that name, in a
  fleet whose requests are applied from git by somebody else. That is the arrangement this project
  actually has, and the superseded rule made it impossible.
- **Two files naming one domain still fight**, because there is one declared-key set and not one per
  writer. This is `kubectl apply`'s own limitation before server-side apply, it fails the same way
  — the second apply's memory replaces the first's — and the fix if it is ever needed is named field
  managers, which is additive. Not built, and recorded so it is not discovered.

Scoping is unchanged and is worth restating because it is a *different* rule that reads similarly: a
file carrying three `kind: domain` documents touches three records and leaves every other domain in
the fleet alone. `--prune` does not extend to labels and there is no other mechanism that would
(below) — and with apply now removing its own retired keys, there is nothing left for a prune to do
here.

**Label writes take `--dry-run`, on the same argument requests do.** A label joins or removes a
domain from a request's expansion, so it starts and stops media exactly as a request does — one
level of indirection away, which makes it *easier* to do by accident rather than harder. Removing
`role=cameras` from a domain five requests select can tear down running sessions, and a verb whose
whole purpose is a quick interactive edit is the worst place for that to be unpreviewed.
`?dry_run=true` runs the same `Compute` against a candidate fleet and returns the paths that would
appear and disappear, exactly as it does for a request.

The real write prints the same thing rather than only the dry run: for each path that would stop,
whether another request still references it — which `path.requests[]` already answers, so nothing
new is computed. It is a **blast radius, not a confirmation prompt**: the CLI is scripted by the
same operators who use it interactively, and a verb that blocks on a tty is a verb that hangs in a
pipeline. Note this is a label removing a path while the node is live, so nothing is frozen (§4.2)
and the teardown is immediate.

This falls out of the idempotency key rather than needing anything new: `POST` is create-or-update
on the name within a namespace, and that pair is the request's ID, so a file naming a set of
requests is already an apply. `?dry_run=true` runs the same validation and reconciliation against a
candidate fleet and returns the outcome without writing — which is nearly free, because the accept
path already builds a candidate fleet and reconciles it in order to reject `INVALID`.

The properties of the format worth stating:

- **`kind:` names the object, and defaults to `request`.** *This supersedes "the file is
  deliberately not a Kubernetes manifest — no `apiVersion`, no `kind`", which allowed for exactly
  this: an optional `kind:` defaulting to the request is additive and costs nothing when a second
  object type appears.* Two appeared at once — `namespace` (§9.3) and `domain` (§10.7) — which is
  the reason to add it deliberately and once rather than twice in a row. An unrecognised `kind` is
  an error, per the rule below. There is still no `apiVersion`: that would be ceremony expressing
  nothing, and the shape stays close enough that the roadmapped Kubernetes adapter (§19) is a
  mechanical conversion.
- **Apply orders documents by kind — namespaces, then domains, then requests — regardless of file
  order.** The end state does not depend on it, since `Compute` is recomputed and namespaces
  auto-create on reference (§9.3), but the *intermediate* state does: a request applied before the
  namespace document that makes its namespace exclusive would be admitted and then invalidated,
  which reads as the apply having broken something.
- **The selector is flattened onto each source in the file** (`flow:` or `group_hint:` directly
  under a `sources:` entry; `domain:` taking either a name or a label map) where the wire type
  nests them under `select` and `domain.name` / `domain.labels`. The tagged-union discipline
  survives intact: the exactly-one rule becomes validation rather than syntax. Friendlier in the
  file, structured on the wire — the same discipline the destination domain follows (§10.6).
  **Omitting the flow selector entirely is `select: {all: true}`**, and that default is filled in
  by the CLI rather than by the server (above).

  `sources:` is **always a list, with no singular `source:` beside it**, matching `destinations:`.
  A scalar-or-list spelling was available — `provider:` uses exactly that trick — and is refused
  here for the reason §19 gives for dropping a second CLI: two spellings for one thing is worse
  than one, and unlike `provider:` the singular form would be the *common* case, so both would be
  in every operator's muscle memory rather than one being a rarity. The cost is that every stored
  request and every manifest written against the old spelling is invalid, which is recorded rather
  than mitigated: it rides along with the domain re-identification of §10.6 in the same major
  version jump (§16).

  **A scalar `domain:` is a name and a map is a label set**, which is the whole of the disambiguation
  and needs no marker key: a name is `media/cameras`, a selector is `{role: cameras}`, and YAML
  already tells the two apart. It also disposes of `domain: {}` — an empty map matching every domain
  on the node — which becomes a label selector with no keys and is refused as one rather than needing
  a rule of its own.
- **`disabled: true` on a destination parks that leg**, spelled on the wire exactly as it is in the
  file — it is the one field in a source or destination entry that needs no translation. A file is
  the natural home for it: "this route exists and is off" is a reviewable line in a diff, where under
  the old spelling the same intent was an absence that reads identically to never having written it.

  **An apply that omits the flag enables the leg**, which is the one sharp edge and is settled the
  boring way: the file is authoritative over the requests it names, as it is over every other field
  of them. So a leg parked interactively — through the API, or from the matrix — comes back the next
  time somebody applies the file that names its request. The alternative is §10.7's declared-key
  merge, and it is refused here for the reason that section gives for *adopting* it there: a domain's
  label map has many writers by design and a request's spec has one by construction, so three-way
  merging a request's own fields would buy nothing and cost the property that makes `--prune` and
  `apply` comprehensible together. `--dry-run` reports it as an `updated` request with the paths that
  would appear, which is the warning.

- **An unrecognised key is an error.** Deliberately the opposite of the rule for `TargetInfo`
  (§5.2), where an unknown field arrives from an independently-versioned upstream and failing
  closed would take out replication on an unrelated upgrade. Here the file is written by a human
  against this binary, and a typo'd key that silently does nothing is precisely the failure a
  declarative format exists to prevent.
- **`labels` are identity, not only annotation.** They ride into worker metrics as user labels
  (§12), and together with the namespace they scope `--prune`: a prune cancels matching requests
  the file does not name. `--prune` therefore *requires* a scope — a whole-fleet prune would take
  out anything created by another operator or by the Kubernetes adapter, and the object being
  cancelled is moving video. **A namespace satisfies that requirement better than a label
  selector does**, since it is a declared partition rather than an ad-hoc tag, so `-n` is the
  primary spelling and `-l` narrows within it. A dry run cancels nothing.

**`--prune` covers requests only.** It never removes a namespace and never removes a domain label,
even when the file contains documents of those kinds. Both are the wrong shape for it: a file
naming three domain labels would otherwise prune the other forty on the fleet, and a pruned
namespace is a delete that §9.3 refuses anyway while requests reference it. Prune exists to make a
file authoritative over *intent*, and a domain label is a fact about a host.

### 9.2 Agent API

```
POST   /agent/v1/register           {node, instance, capabilities (§10.2),
                                     areas and their grants (§10.6)} -> lease
POST   /agent/v1/{node}/heartbeat   renew lease
POST   /agent/v1/{node}/inventory   full domain+flow snapshot (level-triggered, not a delta)
POST   /agent/v1/{node}/status      full snapshot of sessions actually running, incl. epoch,
                                    target_info and bound service (§7.3)
POST   /agent/v1/{node}/events      a batch of diagnostic events, and log tails (§12.1, §12.2)
GET    /agent/v1/{node}/assignments?rev=<cursor>&wait=<seconds>
```

**`events` is the one agent report that is a stream rather than a snapshot**, and it is a separate
endpoint for exactly that reason: `inventory` and `status` are compared before sending (§6), and an
event folded into a compared snapshot is dropped when it repeats and re-sent forever when it does
not. The batch is drained on send and delivered at-least-once, so the server de-duplicates on the
agent's per-event sequence number rather than the agent guaranteeing exactly-once (§12.1).

**Settled: long-poll with a revision cursor, not server-push.** The agent `GET`s its complete
assignment set; the server holds the request until the revision advances or the wait expires.
This is proxy-transparent, needs no sticky sessions, is trivially resumable, and degrades to plain
polling if a proxy buffers — degrading is acceptable, hanging is not, so the server caps `wait`
below any plausible intermediate proxy's idle timeout. Recovery latency is bounded by one poll
round trip — sub-second, and the reason a peer learns of a new epoch fast enough to keep the
agent-restart glitch inside the 1–2 s target in §6.1.

**Being a `GET` has a cost, and it is paid with one header.** Every response the server produces
carries `Cache-Control: no-store`. An assignment poll is a cacheable-looking `GET` whose whole
meaning is in its query string, so an intermediary with an ordinary default cache policy — a CDN's,
where a 24 h TTL on an uncontrolled 200 is normal — will serve an agent a set that was correct for
some other revision. **§4.2 does not cover this**: fail-static protects an agent from *no answer*,
never from a successfully-retrieved wrong one, so the stale set is acted on with full confidence,
and if it carries a stale epoch the result is §5.2's silent failure — an initiator running against
rkeys that no longer exist, moving no data, everything reporting healthy. The header is set for the
whole mux rather than for that one route, because no response here is ever cacheable and an
invariant with no exceptions needs nothing policing it: a cached `/readyz` is a load balancer
routing to a replica that said it had not settled (§7.3).

`inventory` and `status` are **full snapshots**, not deltas. Deltas need sequencing,
gap-detection and resync paths; snapshots need none of that, and at realistic fleet sizes they
are small. This is the same level-triggered discipline as §4.1.

The node name is URL-escaped on every per-node path: it is operator-assigned free-form text,
validated for uniqueness (§7.1) rather than for URL safety, and a name with a slash in it must
address the node it names rather than a route that does not exist.

### 9.3 Namespaces

**Settled: a namespace is a first-class object in desired state, and `namespace` is a real property
on a request rather than a reserved label.**

*This supersedes an earlier position in which a namespace was the value of a reserved `namespace`
label, derived as the distinct values over the request set and owning no record of its own.* That
spelling was justified by an existing CLI mechanism — `--prune -l namespace=nab` already meant
"make this namespace equal this file" — rather than by the model, and it cost two things worth more
than the mechanism saved. `namespace` is a legal user label, so a label an operator wrote for their
own reasons silently became a partition key. And the manifest wanted a plain `namespace:` field
while the wire wanted a label, which left the two spellings to be reconciled and a disagreement
between them to be refused rather than resolved. A property removes both.

A namespace holds a name, `paths` (below), and a description. Request-level defaults — a provider
pin, an idle teardown, a bandwidth budget once §13's admission control exists — are all plausible
later and all easier to add than to take back, so v1 carries none of them.

A name is ASCII letters, digits, `-` and `_`, non-empty. Constrained where an ordinary label value
is free text, because this one is a path segment in a URL, a store key and a `-n` argument on a
command line — the same reasoning that validates a request name more strictly than the wire type
does (§9.1).

**It partitions requests and nothing else.** Not nodes, not domains, not destinations. Two
namespaces landing one flow in one destination domain is fan-in across requests, which §9.1
supports — a request may also fan in on its own, but not across a partition boundary, which is what
makes this case a namespace question rather than a request one — and §10.6 refcounts so the domain
is materialised once. Forbidding it would make the fleet genuinely disjoint at the cost of the
arrangement fan-in exists for.

#### Existence

**Auto-create on first reference, explicit create allowed, never auto-delete.** A request naming a
namespace with no object creates it with defaults rather than failing.

The alternative — a namespace must exist before a request may name it — puts an ordering dependency
on exactly the consumer this partition exists for: an adapter would need create authority on
namespaces and a create-if-missing step in front of every request. Auto-creation avoids that while
still making the namespace set authoritative, which is what a first-class object has to mean if
`GET /v1/namespaces` is to be complete. It is also the same move made one level down, where a
request naming no namespace is *written into* `default` rather than left implying it: an implied
value means two different things depending on which record you are holding.

**Settled: the create is eager — a real write, on the request write, before the request itself.**
The alternative is to derive a missing namespace lazily when something reads the set, which would
be cheaper by one write and would quietly give back the property this object exists for: a
`GET /v1/namespaces` that invents rows is the label spelling again, wearing a record's clothes.
Four consequences, each of which is easy to get wrong:

- **Create-if-absent, never write-if-present.** An unconditional write would bump the namespace
  key's revision on every request write and wake every watcher in the fleet, which is the churn
  §8.3 is sized against. It is the same no-write-if-unchanged discipline the request itself
  follows, applied one key over.
- **Namespace first, then the request.** Reversed, a failure in between leaves a request
  referencing a namespace with no record — the exact state that makes the set non-authoritative.
  The opposite failure leaves an empty namespace, which is inert, indistinguishable from a
  deliberately empty one, and cheap. No transaction is needed, because the two failure modes are
  not equally bad.
- **`?dry_run=true` creates nothing.** A dry run writes nothing at all (§9.1), and the create is on
  the write path rather than in validation — worth stating because a namespace is exactly the kind
  of side effect that gets attached to admission by accident.
- **An explicit document still wins**, because apply orders namespaces before requests (§9.1). A
  file declaring `paths: exclusive` and a request in that namespace lands the declaration first; a
  request that arrives on its own gets the defaults. That ordering rule and this one are the same
  rule seen from two ends.

Deletion is refused while any request references it, with the count in the message. The system never
cancels intent on the user's behalf (§11), and a cascading delete here is a cascading teardown of
live media. `default` is not deletable.

#### `paths: shared | exclusive`

Within an `exclusive` namespace, no two requests may hold one path; the loser reports `INVALID` with
`namespace_overlap` naming the incumbent, resolved by §7.5's precedence, and the path — held by the
winner — carries on. **The default is `shared`, and there is no server flag to change it.**

**Settled: this rule protects legibility, not integrity, which is why it is the one conflict rule
that is opt-in.** Intra-namespace overlap is *free*: two requests expanding onto one path share one
path, one session and one worker pair, which is §9.1's refcounting working exactly as designed.
Nothing is doubled and nothing is corrupted. What overlap costs is honesty in a matrix — two lit
cells that are one stream, counts that do not sum, and a cell that goes dark on a click that stopped
nothing. Those are real problems for a renderer and not problems for the fleet.

That gives the governing line, and it is worth stating because this is the only rule on the far side
of it: **conflict rules that protect data integrity are mandatory; conflict rules that protect
legibility belong to whoever is doing the reading.** `flow_conflict` (§7.2) is the mandatory kind —
two initiators writing into one ring buffer — and it is never optional for anybody.

Three consequences of the choices here:

- **Per namespace, not per request.** The property a matrix needs is a property of the whole set it
  renders: one non-conforming request breaks every cell on the screen, and the renderer cannot stop
  the CLI writing one in. A per-request flag also has no coherent answer when an exclusive request
  meets a shareable one — whichever loses is being punished for the other party's setting.
- **Default `shared`, because refcounting is the base model and forbidding is the special case**,
  and because the party that needs the guarantee is the party that can arrange to ask for it. A
  third-party client should not have to know the rule exists in order not to be broken by it. Under
  the exclusive rule applied everywhere, a Kubernetes adapter — one request per pod, the natural
  mapping — makes a pod's status depend on another pod's existence, resolvable only when something
  unrelated is deleted.
- **A permissive namespace is also immune to an overlap that arrives on its own.** Two selectors
  over one source domain can only start overlapping dynamically in one way — a pinned flow and a
  group hint, where a producer retags — so in an exclusive namespace a producer's NMOS tagging can
  flip a request to `INVALID`. An adapter has no way to handle that and should not have to.
- **A parked leg holds nothing, so it releases the path it held** (§9.1). A disabled destination
  produces no path, which means it cannot hold one against another request and cannot make anything
  else `namespace_overlap`. The consequence to know before relying on it: parking a leg lets another
  request claim its path, and re-enabling then *loses*. It resolves the way every other overlap does,
  with the loser reporting `INVALID` and naming the winner rather than anything stopping.

  **The mechanism is recency, not incumbency**, which is worth stating because §7.5's order leads
  with incumbency and this rule does not use it. Two requests over one path both map to the same
  path, whose session exists whoever holds it, so incumbency cannot tell them apart and the contest
  falls to `(UpdatedAt, id)`. What makes the loss reliable is that un-parking is a *write*: every
  request write is stamped, so a request coming back carries the newest `UpdatedAt` and sorts last
  among the contenders. The corollary is the honest one — a running session buys its holder nothing
  here, and a request whose stamp is older takes the path straight back off one that has been
  carrying it.

`default` stays `shared`: it is the catch-all where hand-written manifests land.

---

## 10. Capabilities, providers and addressing

`provider` is mandatory worker config and the local bind address is **provider-dependent**: an IP
for `tcp` and `verbs`, a device address for `efa`, which in practice is link-local.

### 10.1 Provider availability is not reachability

The obvious model — nodes declare `provider → address`, the server intersects the provider sets —
does not hold up:

- Two nodes both offering `verbs` may be on different InfiniBand fabrics.
- Two nodes both offering `efa` may be in different VPCs or subnets, and EFA additionally
  requires the security group to allow traffic to itself.
- Two nodes both offering `tcp` on RFC1918 addresses may have no route between them.

Set intersection on provider names would cheerfully assign a session that cannot connect, and the
failure presents badly: the target comes up clean, the initiator's connect loop spins, and
nothing explains why.

**Nodes declare fabric attachments, not providers.** Each attachment is a
`(provider, fabric, address)` triple where `fabric` is an operator-assigned opaque label:

```yaml
fabrics:
  - provider: verbs
    fabric: ib-fabric-a
    device: mlx5_0
    ip_version: 4               # the HCA also reports a link-local v6 address
  - provider: tcp
    fabric: dc1-data
    network: 10.1.0.0/16        # names no hardware: the same value on every node
  - provider: efa
    fabric: vpc1-subnet-a       # no selector: the node has exactly one EFA device
```

Two nodes may pair on a provider **iff they share a fabric label for it**. The label is opaque to
the server, which does nothing but string equality — no topology database, no reachability
probing, no inference. It matches how these networks are actually provisioned: the operator
already knows which HCA is on which fabric.

`shm` is structurally same-node-only, and its label is derived from the node name so that falls
out with no special case. One exported function derives it and the server canonicalises what it
stores, so an agent that spelled it any other way still matches itself rather than silently
failing to pair with its own second domain.

**A node with no fabric attachments configured gets `shm`.** Such a node could otherwise do
nothing, so the alternative is refusing to start — and that would break `mxl-replicator run` with
no arguments, which is the single-host and development case §2.2 exists to serve. `shm` is an
assumption rather than a placeholder: structurally same-node-only, no address, label derived from
the node name, so replicating between two domains on one host works out of the box. It is warned
about at startup, so a node meant to reach other hosts says what it is missing.

#### Detecting one instead, and why it is opt-in

**Settled: `--agent-detect-default-fabric` picks a single attachment out of the probe and labels it
`default`.** It is the *other* answer to "this node configured nothing", replacing the `shm`
assumption above rather than joining it, and it is a flag rather than the default because of what
the label means.

The selection is the server's own preference order (§10.4) — EFA, then Verbs, then TCP, then SHM —
taking the first provider that has an entry another node could plausibly reach. That makes it the
decision negotiation would have made if every node offered everything, which is the only defensible
choice for a guess: it cannot be surprising relative to the rest of the system. Within a provider it
takes the first usable entry in probe order and says in the log what it passed over; a node with two
of something has a decision its operator has to make, and §10.1's selectorless-ambiguity rule is
where that is already said.

**Only `tcp` filters addresses, and it is the only provider that needs to.** A verbs or efa address
is hardware-derived with one sensible answer per device. tcp is where the host has half a dozen and
all but one are wrong: **loopback**, which pairs only with itself; **CGNAT** (100.64.0.0/10), which
is what a CNI hands out and which is routable inside its scope and never between the scopes a fleet
spans — and which is most likely to be *first* on exactly the deployments that reach for this flag;
**link-local** and the unspecified address, which are never a deliberate data path; and **IPv6**,
where a link-local address needs a zone index the peer cannot use and a ULA is as private as CGNAT.
An operator with a working v6 fabric names it and gets it. RFC1918 is accepted, because §10.1's own
argument says reachability between two private addresses is not decidable here — that is what the
label is for.

**The label is why this is opt-in.** `default` is an ordinary fabric label and the server does
nothing with it but string equality, so detection pairs a node with every other node that also
detected and with nothing else. Two nodes on genuinely different networks will both call theirs
`default` and be paired, producing exactly the failure this section exists to prevent — a target that
comes up clean and an initiator whose connect loop spins with nothing to explain why. Made the
default, that would be this section's argument being lost by accident on every flat-network
deployment; as a flag it is an operator saying "my fleet is one network", which is a claim they are
entitled to make and the server cannot check.

Two smaller consequences, recorded because neither is visible from the flag:

- **It re-runs on every re-registration**, because that is when the probe runs (§10.2, §10.5). A
  node whose tcp address moved therefore re-detects onto the new one and every session through it
  re-establishes, where an operator who named the address would have seen the attachment dropped and
  the reason logged. Explicit configuration remains the better answer for anything that has one.
- **A configured attachment is never joined by a detected one.** Detection is a fallback and a
  fallback that is not taken is not a configuration error, so the flag alongside a `fabrics:` block
  is inert and warned about rather than refused at parse time — the arrangement that produces it is
  layered configuration, where refusing would break the node that got it right.

#### Joining configuration to the probe

**Settled: join selectors come in two classes — *naming* and *narrowing* — and "none" is the
common naming one.** *This supersedes an earlier position — "prefer naming an `interface` over an
`address`" — which is not merely unimplementable on `verbs`/`efa` but inverted.* That advice
justified itself with EFA — link-local, hardware-derived addresses that nobody wants to pin — and
`interface:` is precisely what cannot work there: the probe's only interface-ish field is a
libfabric device name (`rdmap0s6-rdm`), not `efa0` (§10.5). It works only for `tcp`, the one
provider where naming an address was never painful, and it would leave an EFA operator required to
pin exactly the address the advice exists to avoid.

**Naming — which interface. At most one per attachment**, because two names would need a rule for
combining them and every such rule is a worse answer than making the operator say which one they
meant:

| Configured | Matched against | Works for |
|---|---|---|
| `address:` | probe `node`, exactly | all providers |
| `interface:` | the netdev's addresses, resolved locally with `net.Interfaces()`, against probe `node` | `tcp`, `verbs` |
| `device:` | probe `attr.device_name`, exactly | wherever the library reports one |
| *nothing* | the provider alone, which **must** match exactly one probe entry | the common case |

The fourth row is what resolves `efa` and `shm`, and it is a better answer than name matching
rather than a fallback from it: a node has one EFA device and one `shm`, so
`{provider: efa, fabric: vpc1-subnet-a}` is unambiguous and puts no hardware-derived string in the
config file at all. When it *is* ambiguous the agent cannot guess — it refuses the attachment and
logs every candidate, which hands the operator the exact strings they could have written. That is
§10.5's "this node has no verbs" vs "someone typo'd `ib0`" distinction, served better by a
candidate list than by a failed name match.

**Narrowing — which of that interface's addresses counts. Any number**, conjoined with a naming
selector and with each other, the "exactly one probe entry must survive" rule applying to the
conjunction:

| Configured | Matched against | Works for |
|---|---|---|
| `network:` | probe `node` parsed, tested for containment in a CIDR prefix | IP addresses |
| `ip_version:` | probe `node` parsed, `4` or `6` | IP addresses |

*This supersedes "at most one selector per configured attachment", which the naming class keeps and
which was never the right rule for the whole set.* Naming a thing and narrowing what counts as that
thing are different acts, and the case that forces the distinction is a **DaemonSet**, where every
value is fleet-wide and `address:` is therefore unavailable. `device: mlx5_0` is the fleet-wide
string an operator has, and it is ambiguous by construction: the probe prints one entry per
`(interface, address, provider)` (§10.5), so an HCA carrying an IPv4 address and a link-local IPv6
one is two entries under one device name, and the attachment is dropped. The only fix available
before this was a per-node `address:` — an overlay per node, to disambiguate a fact ("we are on
v4") that is true of the whole fleet. `device: mlx5_0` plus `ip_version: 4` states it once.

`network:` is that argument one step further, and it is the selector that needs neither a per-node
value nor a hardware-derived string: it picks each node's own address inside a prefix. On a fleet
whose nodes are alike but not identically named — `eth1` here, `ens5f0` there — it is the only
selector that is at once exact and uniform, and it is usually the right one for `tcp`.

**Neither narrowing selector says anything about reachability, and neither is allowed to.**
§10.1's own argument is that two nodes inside one RFC1918 prefix may have no route between them, so
`network:` picking an address out of a list must not be read as asserting that address is reachable
from anywhere. The fabric label makes that claim and remains the only thing that does — which is
also why `network:` is not a fabric label in disguise: making it one would put a server that this
section exists to keep out of topology in the business of comparing two nodes' prefixes.

Two smaller consequences:

- **An address that is not an IP matches no narrowing selector.** `shm` reports the hostname, and a
  provider may report a device address in any shape. That is not an error; it is what "this address
  has no version and is in no prefix" means, and it is why the narrowing class is documented as
  working for IP addresses rather than for providers.
- **`ip_version` contradicting `network`'s own family is refused at parse time**, since a prefix
  already names a family. Left to the join it would match nothing on any node and present as the
  entire fleet dropping an attachment, rather than as the typo it is.

### 10.2 What a node advertises

**The agent advertises only what it has verified.** Raw kernel capabilities — `CAP_IPC_LOCK`,
`/dev/infiniband`, `RLIMIT_MEMLOCK`, `CAP_SYS_NICE`, `RLIMIT_RTPRIO` — never go over the wire.
They determine whether a provider works at all, so they gate whether the agent *advertises the
attachment in the first place*. The server should never have to reason about them. (`sched_prio`
availability reads `RLIMIT_RTPRIO` **or** `CapEff`, either being sufficient, because a container
commonly has one without the other.)

That gives a sharp test for what belongs in registration: **something is a capability iff the
server would make a wrong decision without it.** Everything else is agent config or a local
precondition.

| Advertised | Why the server needs it |
|---|---|
| Fabric attachments, each with provider, label, address, caps flags, `maxMessageSize` | Negotiation (§10.3) |
| `mxl` and `libfabric` versions | Cross-node compatibility — see below |
| Replicator build version and **protocol version** | Version skew (§13.1) |
| `sched_prio` available | Request-time validation |
| Areas, by name, path and grants | Request validation, destination resolution, and rendering a domain's name (§10.6) |
| Port range | Diagnostics only |

**Readable areas are advertised, and they pass the test rather than bending it.** *An earlier
reading had it the other way: search paths were not advertised, on the correct observation that the
server made no wrong decision without them, and the only argument for exposing them was a
diagnostic one — that a label applied outside every search path is silently inert with no way to
say why. That was left open as a question about whether §10.2's rule bends for a message.* It does
not have to. A domain's name is `<area>/<elements>` (§10.6), so a server that does not hold the
area table cannot render a domain's identity, resolve a name in a request, or tell an operator
which area a label landed outside of. The grants come with the entry because "may this name be a
destination" is request-time validation.

**The version pair is the non-obvious one.** `target_info` is produced by one node's mxl-fabrics
and consumed by another's, so a node pair straddling an mxl version boundary is a compatibility
concern *neither agent can detect alone*. The worker prints `proxy`, `mxl` and `libfabric` via
`-v`, and the agent runs that probe at startup anyway.

Capabilities are **static**: they go in registration and change only by re-registering. If an
attachment disappears, the agent re-registers — and note that re-registration cancels the
session's control-plane loops while leaving every worker exactly where it is (§4.2).

### 10.3 Negotiation

The library does **not** negotiate. From `FabricsDeveloperGuide.md`: *"Both sides (target and
initiator) must receive the same capabilities and maximum message size to be compatible. There is
no internal negotiation. Typically you would serialize the selected `mxlFabricsInterfaceConfig`
alongside the `mxlFabricsTargetInfo` and share both with the initiator through your out-of-band
signalling channel."*

**This project is that out-of-band channel.** Negotiation is squarely the server's job, and it is
not just picking a provider name — it must agree the full interface config across both ends:

1. **Match fabrics.** Candidate attachments are pairs sharing a `(provider, fabric)`.
2. **Apply the pin or the preference order** (§10.4).
3. **Agree capabilities**: the caps flag set is the *intersection* of the two sides' flags, and
   `maxMessageSize` is the *minimum* of the two. At least one of `REMOTE_WRITE` or
   `SEND_RECEIVE` must survive the intersection; if neither does, the pair is not viable. A
   capability name this build does not know is passed through rather than dropped, so an
   intersection stays correct across a version boundary.
4. **Assign the agreed config to both ends**, not just to one.

If no viable pair exists, the request is `INVALID` with a reason naming what failed —
`no_shared_fabric`, `no_shared_provider` or `no_shared_capability`. These are different operator
problems and the message distinguishes them.

Negotiation is deterministic: the same fleet computes the same result on every replica and every
pass.

### 10.4 No silent downgrade

**Settled: an explicit provider is honoured or the request fails. It is never substituted.**

An operator who asks for `verbs` gets verbs. Silently landing on `tcp` is a performance cliff, not
a graceful degradation — a path that carried 1080p60 over verbs may simply not keep up on tcp,
and the resulting dropped grains look like a source problem rather than a routing decision made
on the operator's behalf.

With no pin, the default preference order is **EFA > Verbs > TCP > SHM**, matching the priority
the mxl demo tool uses. The order is configurable.

The pin and any acceptable fallback are one field, so "pinned" and "prefer, but this is
acceptable" are the same mechanism rather than two:

```json
"provider": "verbs"              // verbs or fail
"provider": ["verbs", "tcp"]     // prefer verbs, tcp acceptable
                                 // omitted: the configured order
```

**The negotiated provider is pinned for the lifetime of the session.** If the fabric a session is
using goes away, the session goes `FAILED` / `fabric_gone` with a clear reason — it does *not*
silently re-negotiate onto a slower provider. Re-negotiation at 3am with no operator action is
the same silent downgrade, just harder to notice, and a clearly-failed path triggers the
operator's actual failover procedure while a struggling one does not. The negotiated provider is
always reported in status.

### 10.5 Interface discovery: the worker probe

The agent cannot enumerate libfabric interfaces itself — it is Go and does not link the library.
Guessing from `/dev/infiniband` and interface names is a heuristic whose failure mode is a
confusing restart loop.

```
mxl-replicator-worker --interfaces
```

The probe calls `mxlFabricsGetInterfaces()` and prints a JSON array on **stdout**, one object per
`(interface, address, provider)` combination — the same physical interface appears several times
if it is reachable through several providers or carries several addresses. Per entry: `provider`,
`address.node`, `caps.flags` (`REMOTE_WRITE` / `SEND_RECEIVE` / `BLOCKING_OPERATIONS`),
`caps.maxMessageSize`, and a best-effort JSON `attr` blob.

Four properties of the output that shape everything above:

- **There is no interface-name field.** The physical interface, when known, is
  `attr.device_name`: the netdev name for `tcp` (`eth1`), but the *libfabric device* name for
  `verbs`/`efa` (`mlx5_0`-style). Hence the naming selectors in §10.1, and — because one device
  name covers every address on that device — the narrowing ones beside them.
- **`caps.maxMessageSize` is a genuine `uint64`** and providers report `UINT64_MAX`. A `float64`
  in any decoding step loses it.
- **`shm` reports the hostname** as its node and carries no `attr.device_name` at all —
  consistent with deriving its fabric label from the node name.
- **stdout is also the worker's log stream**, and libfabric's diagnostics go there too, so the
  probe redirects stdout to stderr while probing and restores it before printing. The agent gets
  JSON on stdout and logs on stderr and must capture them separately. (`-v` and `-h` print to
  stderr, WRS §2; this one is stdout because it is data.)

The probe needs a domain directory to exist, so it creates and removes a throwaway one; the agent
does not have to supply a domain. It reports no `service`: the library reports one, but it is
empty for every provider but `shm` and the `shm` value is a per-process artefact of whichever
process ran the probe — nothing a later worker could bind, and carrying it forward would have put
a probe artefact one rename away from the agent-allocated service of §7.4.

The agent joins the probe output against the configured `fabrics:` block and advertises only
attachments that appear in both. A configured attachment with no matching probe result is a
configuration error, logged loudly at startup rather than silently dropped; it is the difference
between "this node has no verbs" and "someone typo'd `ib0`". Re-probe happens on
re-registration, not on every heartbeat.

### 10.6 Domains, areas and grants

**Settled: a node declares *areas* — named directories, each granting reading, writing or both. A
domain is a directory inside an area, identified fleet-wide as `<area>/<elements>`, and there is
one kind of domain. Nodes declare areas, not domains — the same shape as §10.1, and for the same
reason.**

*This supersedes two earlier positions in this document, both argued down below and both kept,
because they read plausible and because what they were protecting has to land somewhere: that
input and output domains are **separate concepts** with separate identities — an input domain
named by its absolute path for life, an output domain named by a request as elements under an
output root — and that discovery is **pruned** at every output root in both directions, so that
"a root is written, not read".*

#### A domain is a place, not a channel

The split was a pipe model: a domain has two ends, one process fills it and another drains it, and
whichever end this project occupies decides what kind of thing the directory is. MXL does not work
that way. A domain is a directory that holds flows, several processes routinely write different
flows into one, and the single-writer constraint the SDK actually enforces is **per flow** — the
ring buffer has one producer, and nothing anywhere says a directory has one owner.

Read as a multicast group rather than as a pipe, this project is one participant among the node's
media functions rather than the proprietor of any directory. Ownership is then a property of a
*flow*, which is where MXL already put it, and two consequences fall out together: there is one
kind of domain, and the thing that must not be fed back into replication is a flow this node is
already writing, not a directory this node was granted.

#### Areas, and the two grants

```yaml
areas:
  - {name: media, path: /dev/shm/mxl,            read: true}
  - {name: fast,  path: /dev/shm/mxl/replicated, read: true, write: true}
  - {name: bulk,  path: /mnt/nvme/mxl,           read: true, write: true}
```

An area is a name, a directory, and two independent grants. **`read` is the whole of this
project's authority to discover and observe domains under that directory; `write` is the whole of
its authority to create them and write flows into them.** Neither implies the other, both default
false, and an area granting neither is refused at startup as a line that does nothing.

*This supersedes `--search-path` and `--output-root` as separate concepts.* The document already
called them counterparts and already instructed that they be read as a pair; making them one noun
with two bits is what that instruction was asking for. The arrangement an operator actually
reaches for — one MXL area per host, with a subtree replication may write into — stops being an
exception to an overlap rule and becomes two ordinary areas.

The grants are unchanged in force. Areas are static agent configuration and go in **registration**,
alongside fabric attachments and for the same reason: they are what the server needs in order to
make a correct decision, and they change when the host is built rather than when a flow is routed.
Nothing about them is settable by the API. A node with no readable area offers no sources; a node
with no writable area accepts no destinations at all. Both defaults are correct, and they are the
same default: access to a node's filesystem is opt-in per node, per direction.

Several areas are supported because "this domain on tmpfs, that one on NVMe" is a real
requirement, and because an area is the natural place to hang a future capacity budget — capacity
is a property of a mount, not of a domain.

The agent creates its writable areas at startup (§6.1), so only the leaf `MkdirAll` is ever on the
establishment path.

#### One name, from the innermost area

A domain's fleet-wide identity is its area's name followed by its path elements relative to that
area. Under the layout above, `/dev/shm/mxl/studio-a/cam1` is `media/studio-a/cam1` and
`/dev/shm/mxl/replicated/ingest` is `fast/ingest`.

**Areas may nest, and the innermost containing area names a directory.** Longest prefix wins,
which is the ordinary rule in the vocabulary this section has borrowed from. `media` being an
ancestor of `fast` therefore produces nothing to disambiguate: `/dev/shm/mxl/replicated/ingest` is
`fast/ingest` and never `media/replicated/ingest`, because `fast` contains it more tightly. Equal
paths are refused at startup, naming both areas, since that is the one arrangement the rule cannot
decide. An area's own directory is not a domain — a domain has at least one element — so
`/dev/shm/mxl/replicated` is the area `fast` and not the domain `media/replicated`.

**This is what makes one identity grammar possible, and everything below depends on it.** A
directory has exactly one name whether discovery found it or the reconciler created it, so the two
namers cannot disagree — which is precisely the disagreement pruning existed to prevent.

Elements are held to one rule: ASCII letters, digits, `-`, `_` and `.`, not starting with `.` or
`-`, at most 64 bytes each. An area name follows the same grammar and must be unique on its node,
which is what keeps a rendered domain name unambiguous about where its first segment stops. The
same grammar governs the value of a domain's optional `name` label (§10.7) — the rule outlived the
`-m` flag that motivated it, because a string that ends up in a metric label wants the same
constraints whether it is identity or decoration. Depth is capped at eight elements and the
rendered name at a length, for legibility rather than safety: safety is carried by the per-element
rule.

#### A domain is an area name and a list of elements

**Settled: a domain is `(area, []string)` in the model and on the wire, and `area/a/path` only in a
manifest file, parsed once by the CLI.** `("fast", ["studio-a","cam1"])` is the directory
`<fast>/studio-a/cam1` and renders as `fast/studio-a/cam1`; a flat domain is a one-element list.

Elements rather than a string, because it is what makes the containment invariant *structural*
rather than argued — see below. Parsed at exactly one boundary, and that is the other half of the
argument: every place that would otherwise need a rule about separators — the server, the
assignment, the agent's resolver — takes structure and never text, so there is no second parser to
disagree with the first. The same discipline the selector follows: flattened in the file,
structured on the wire (§9.1).

Everything downstream still carries a single rendered string — the assignment's domain, the path
and session identity, the `domain` metric label. That is lossless: neither an area name nor an
element can contain the separator, so the rendering is injective.

**One materialised domain may not contain another.** `fast/studio-a` alongside `fast/studio-a/cam1`
would make one domain directory a container for another, a shape nothing else in the design has,
and the element form makes the test an exact slice prefix where the string spelling has to work
around `studio-ab`. This governs what a request may *ask to create*; a directory layout some other
actor produces is that actor's business, and discovery reports what it finds.

**Output domain names are no longer flat per node, and there is nothing to enforce.** *An earlier
position made them a single namespace per node, so that two domains under different roots could
not share a name, and gave the collision a rejection code — `domain_name_in_use` (§7.2).* It
existed because the assignment, the path identity, the session identity and the `domain` metric
label all carry one string, and `fast:ingest` and `bulk:ingest` rendered to one address. With the
area in the name, `fast/ingest` and `bulk/ingest` are two strings and the collision is
unconstructible. The code is gone rather than relaxed.

#### Resolution, and the invariant

A target assignment carries an **area name** and the domain's **elements** — never a path, and
never a string the agent has to split. The agent resolves it as `join(area.Path, elements...)`
after checking that:

- the area name is one this agent advertises, and that it grants `write`;
- every element is a clean path element — no separator, no `.` or `..`, no empty string, and from
  a restricted character set;
- the joined path equals `area.Path + "/" + join(elements)` exactly.

The character-set rule already makes traversal impossible; the containment check makes it
provably impossible, and it is two lines.

The containment check is an **equality on the whole path**, which is what taking elements rather
than a string makes possible. There is no prefix test to get subtly wrong —
`HasPrefix("/dev/shm/mxl-evil", "/dev/shm/mxl")` is true — and no boundary case for a separator
to hide in: if `Join` cleaned anything away, or an element smuggled in a `..`, the two strings
differ.

**The destination resolver never consults observed state.** This is the sharpest property of the
design and it survives the unification unchanged, because it was never a consequence of the split:
it is a property of *resolution*. An initiator's domain is resolved through inventory, because a
source is by definition something the agent observes. A target's is resolved from agent
configuration and the assignment alone, with no dependence on what happens to be on disk. The
security-critical path is therefore a pure function of one config file and one name, and
un-pruning discovery (below) does not touch it — discovery decides what is *visible*, never what
is *writable*.

The `write` grant is checked on the agent as well as on the server, which is the one line the
unification adds here. Under the old split, "this is a root" carried the grant by construction;
now that a name resolves against one table, the grant is a field on the entry and has to be read.

The agent checks all of this even though the server validates it first (§7.2). Same reasoning as
everywhere else this is duplicated: it costs a map lookup and a string check, and it is the
difference between one buggy control plane and files written wherever an area can reach.

A pleasant side effect on the read side: a source domain name is now `<area>/<elements>` too, so
the objection §6 answers structurally — that a name which looks like a path invites an agent to
treat it as one — stops being constructible. `/etc` is not a domain name in this grammar at all.

#### Discovery is not pruned

**Settled: discovery reports every domain in every readable area, including one this project
materialised and one it is currently writing into.**

*This supersedes pruning: "discovery reports nothing at or under a root, in either direction",
and the rule it established — "a root is written, not read. A domain some other actor creates
inside a root is invisible as a source."* Pruning was doing four separable jobs. Two are gone
outright under the naming rule above, one is done by a mechanism that had to exist anyway, and one
survives — relocated to the flow, where the multicast reading says it belongs.

**One directory, one name and one owner.** Gone. The innermost-area rule gives every directory
exactly one name whoever reports it, so there is no second name for an owner to arbitrate between.

**The ordering hazard.** Gone, and it was the sharpest of the four: `<root>/cam1` left holding a
flow by a `SIGKILL`ed worker would be discovered before the reconciler materialised it, and since
a domain was named by whoever reported it first, it would keep its path-shaped name through
materialisation — leaving the server, which matches a session's destination by name, with a path
stuck in `ESTABLISHING` and nothing explaining why. Both namers now produce `fast/cam1`. There is
no second name to keep, and nothing to strand.

**The withdrawal half** — which the superseded text correctly called the one that is easy to leave
out — is done by union rather than by hiding. The discoverer only reports directories that
currently contain a flow, so an unpruned withdrawal would forget a materialised domain the instant
its last flow was released, while a session still targeted it. So membership is the **union**: a
domain is in inventory if discovery reports it *or* the agent materialised it and has not released
it, and it leaves only when both say no. That is a small change to machinery this section already
required, since the agent drives materialised domains into inventory by hand regardless (below).

**Self-amplification is the job that survives**, and the multicast reading is the reason rather
than an exception to it: a network of receivers that forward what they receive is exactly where
loops come from, which is what reverse-path forwarding and spanning tree exist for. But "under an
output root" was a directory-granular proxy for the thing that actually matters, and a blunt one —
a domain holding one replicated flow beside nine local ones was entirely invisible as a source.
The honest discriminator is provenance, per flow: **a label selector never matches a flow this
project is itself writing.** Explicit naming still reaches everything. That is the same cut as
before — explicit chaining is intent, matched chaining is emergence (§10.7) — moved to the
granularity where ownership lives, and it is strictly more precise than what it replaces. The rule
and its consequences are §10.7's; what belongs here is the signal it runs on.

The agent supplies it: inventory carries a **`replicated`** boolean per flow, true exactly while
one of this agent's own target workers is writing that flow (§6). It is low-churn by construction —
it changes when a target starts or stops — so it costs §6's compare-before-send discipline
nothing.

**Provenance can be briefly absent, and §11.1's admission rule is what makes that safe.** In the
1–2 s after an agent restart (§6.1) the target workers are not yet running, so a replicated flow
reports `replicated=false` and looks local for as long as it takes them to come back. That window
cannot amplify, and the reason is structural rather than lucky: §11.1 holds a path in `PAUSED` and
starts no workers at all until the source is actually being produced, and a flow whose target
worker is not running is not advancing, because the target worker is the thing that advances it.
Provenance and production are the same fact observed twice and cannot disagree in the dangerous
direction. The same coincidence covers long-idle teardown (§11.1), where the target is stopped
deliberately and the source is idle by definition. **This makes admission load-bearing for safety
and not only for churn**, which is worth saying where someone optimising it will read it: admitting
a dormant source eagerly would reopen self-amplification through a door that has nothing to do
with admission.

The rule this establishes is worth stating positively, because it is a real widening: **a domain
this project writes into is an ordinary domain.** It appears in `GET /v1/nodes/{node}/domains`, it
can be labelled, it can be named as a source, and a flow in it that this node is not writing — one
a local media function produced beside the replicated ones — is selectable like any other. What it
cannot be is matched into a copy of itself.

The one configuration rule left is the one that was always doing the work: **areas may not share a
path.** Everything else the old text refused — a search path inside a root, a search path equal to
a root, a root that is an ancestor of an input mapping — was arithmetic on a distinction that no
longer exists. It is still said at startup, listing each area with its grants and its nesting, or
the failure is an operator wondering why a domain is called what it is called.

#### Materialising, and observing what was materialised

Materialisation is three steps, all on the destination agent, all triggered by accepting a target
assignment: `MkdirAll` the resolved path, add it to the flow watch set, start the worker. Release
reverses the middle step when the last session on that domain stops.

The watch-set step is the one that is easy to leave out and expensive to debug. §11 derives
`ACTIVE` from the destination flow's head index **as reported by the destination agent's own
inventory** — there is deliberately no separate "destination is receiving" signal — so a domain
this project writes into but does not observe can never leave `ESTABLISHING`. Note that
`mxl-utils`' `Discoverer` fixes its `static` list at construction and only ever reports
directories that already contain a flow, so neither of its mechanisms can deliver a
freshly-materialised, still-empty domain. The agent drives the inventory and the watcher by hand,
preserving the receiver ordering the discoverer otherwise provides.

*Driving them by hand used to be the whole of it*, because pruning meant nothing a scan saw could
name a materialised domain and nothing a scan stopped seeing could withdraw one. It is now the
half of it that covers the empty case: a scan and the reconciler both report the same domain under
the same name, and inventory holds the union of the two (above). The property that mattered is
kept — a materialised domain cannot be *withdrawn* by a scan — and the property that was
incidental, that it could not be *seen* by one, is deliberately given up.

A pleasant consequence, and it is now the ordinary case rather than a special one: **chains work
with no extra design.** In `A→B→C`, the middle node's domain is materialised by the first request,
which puts it in the watch set, which makes it visible as a source for the second — and it exists
exactly as long as the first hop does, which is the correct dependency. The second request names
it in the same grammar as any other domain (§10.7).

#### Leaked directories

When the last path targeting a domain goes away, the directory remains. The MXL SDK removes a flow
directory when its writer is released, so what is left is usually empty, and an empty directory is
not reported as a domain: it is in no inventory and cannot be selected. Re-materialising is an
idempotent `MkdirAll` over it.

*A leaked directory left holding stale content used to be hidden as well* — the superseded text
noted that pruning concealed the directory whether it was empty or not, and counted that as a
strengthening. It is now discovered, which is the honest outcome: it appears as a domain holding a
flow nobody is writing. It cannot amplify, because a flow that is not advancing is not admitted
(§11.1), so a path over it sits in `PAUSED` with nothing running; and it cannot be mistaken for
live media, because `PAUSED` is precisely the state that says nobody is writing. Surfacing it is
better than hiding it. The old behaviour deferred a leak to an ownership model that does not exist
yet and left an operator nothing to notice in the meantime.

Cleanup is still deliberately **out of scope**, and the reason is unchanged — but it is now stated
by the model rather than despite it: this project cannot distinguish a directory it created from
one that was already there, a domain is a shared place by construction, and a domain it did create
may still be in use by another actor on the host after the last replication into it has stopped.
Removing either is data loss with no way to detect the mistake. Deciding it needs an ownership
model, which is a separate piece of design (§19).

#### The superseded split, and what it cost to unify

*Kept because the reasoning reads plausible.* The earlier position was:

| | Input domain | Output domain |
|---|---|---|
| Layer (§4) | observed | derived |
| Comes from | discovery under a search path | a replication request naming it |
| Named by | its absolute path, for life (§6) | the request, under a root |
| Lives as long as | a producer keeps it non-empty | a path targets it — refcounted exactly like a path (§3) |
| Resolved by | a lookup in this agent's inventory | derivation under a root |
| Written to by this project | never | always |

Its argument was that domains straddle two of §4's layers, that almost everything awkward about
them comes from that, and that separating the two roles removes it. **The diagnosis was right and
the treatment was backwards.** A domain is *observed*, full stop: a directory that exists on a node
and is reported by the agent that can see it. What is *derived* is whether a session targets it —
which was always a property of the session rather than of the directory, and modelling it as a
property of the directory is what produced two nouns, two naming schemes, two lifecycles and a
pruning rule to keep them apart.

The half of the old position that survives verbatim is the half that was never about the split:
**a domain needs no lifecycle of its own.** The directory is created by the first path that targets
it and forgotten when the last one goes, on exactly the refcount that already governs paths. There
is no create API, no delete API, no "delete while referenced" conflict, and nothing durable to
reconcile against. The mirror argument that removed input mappings from agent configuration
survives too, and is now the same argument rather than its mirror: a name is not something a node
has, it is something an operator decided, and deciding it on the host means deciding it in the one
place that has to be restarted to change its mind (§6, §6.1).

**What unifying costs.** Every domain identity in the system changes: `/dev/shm/mxl/cameras`
becomes `media/cameras`, so every stored path, session and label record, every manifest and every
dashboard query built on the old spelling is invalidated by the upgrade. Area names must be unique
per node. Renaming an area orphans the labels on its domains, exactly as renaming a node does
(§10.7). And a destination must always name its area, where the old `root:` field could be omitted
on a node advertising exactly one — a small verbosity cost, paid to have one grammar instead of
two.

**What it buys beyond the grammar** is that an area's directory can be repointed without
re-identifying anything on it. `path: /dev/shm/mxl` → `path: /mnt/mxl` under the same area name
keeps every domain's name, so paths and sessions survive the move instead of rebuilding: changing
it restarts the agent, which re-establishes every worker on the node anyway (§6.1), and they come
back against the same identities. Flows left behind in the old directory are leaked and nothing
moves them, which is the operator's problem to sequence. Under path-as-identity this was a
fleet-wide re-identification and was recorded as an accepted cost; it is no longer one.

### 10.7 Domain labels

**Settled: an operator labels domains through the API, before or after they are discovered. Labels
annotate; they never rename. A request's source names a domain directly or selects domains by
label.**

This is the naming half of the old `-m` flag, moved (§6, §10.6). What the flag did well was give a
domain a short fleet-wide name; what it did badly was make that name a startup argument, so naming
a domain cost an agent restart and an agent restart re-establishes every flow on the node (§6.1).

#### Identity is the domain's name, and a label never touches it

A domain's identity is `<area>/<elements>`, permanently (§10.6). Labels are additional key/value
pairs attached to `(node, domain)` and nothing else.

*This used to say "identity is the absolute path".* Only the spelling changed: a label was never
identity and still is not. What the area name buys here is that a label survives an operator
repointing an area's directory, where under path-as-identity it was orphaned along with the
domain it described.

The tempting alternative — a label supplies the domain's *name* — does not survive contact. The
domain name is embedded in path identity (§5.4), session identity and the `domain` metric label, so
renaming a domain re-identifies every path through it: a metadata edit that tears down running media
and splits every series it touches, which is exactly the churn §12 refuses for flow labels. Keeping
identity fixed makes relabelling free, and it turns naming into *selection*.

That is the same move §9.1 already made one layer down. A UUID is rarely what a user means, so the
source is a selector rather than a flow ID — and a domain name is the domain-level UUID, so the
consistent answer is a selector rather than a rename. `sources[].domain` is therefore a tagged union
with exactly one kind set, extensible the same way and for the same reasons:

```json
"domain": { "name": "media/cameras" }
"domain": { "name": "fast/ingest" }
"domain": { "labels": { "role": "cameras" } }
```

**Settled: the direct form is `name` and it addresses any domain.** *This supersedes `{"path": …}`,
which addressed only a discovered domain and left the second hop of a chain unspellable — §10.6
promised that `A→B→C` needs no extra design, and with two identity grammars there was no way to
write it.* One grammar removes the problem rather than reconciling it: `fast/ingest` is a name like
any other, and the union stays two-kinded because the second kind is *selection*, not a second
spelling of *naming*.

The direct form matters and is not a fallback: a manifest that names a domain is self-contained,
where a label selector depends on a `kind: domain` document someone may not have applied. Both
belong in one file, which is what applying by kind order (§9.1) is for.

#### What the selector matches: equality, ANDed, never empty

**Settled: the `labels` kind is equality-only, every key ANDed, and a selector with no keys is
refused.** `{"role": "cameras", "site": "studio-a"}` matches a domain on the named node carrying
both keys with exactly those values, by case-sensitive string equality. There is no `in`, no
`exists`, no negation, no wildcard and no value grammar of any kind — a value is a string compared
whole.

That is the obvious v1 and it is chosen for the same reason §9.1 gives for the flow selector: the
restriction is what keeps the extension additive. `in`, `notin` and `exists` are the operators
anyone will eventually want, and they arrive as a **third union kind** — a list of match
expressions — rather than by widening what a map value may say. Widening the value grammar is the
move that cannot be taken back: a request whose value happened to look like an expression would
change meaning under the upgrade, silently and in the direction of matching *more*, which for a
mechanism moving uncompressed video is the wrong way to be wrong. A new kind cannot do that,
because no existing request has it set.

**A selector with no keys is refused, and the refusal lives in the validator rather than only
here.** An empty map matches every domain on the node, which expands a request's source set across
whatever that node happens to hold — and it is reachable by omission rather than by intent, since
`domain: {}` and a `domain:` whose keys were all deleted are both easy to write. §9.1's
scalar-versus-map rule already disposes of the *syntax* question — a scalar is a name, a map is a
label set, so this is a label selector with no keys rather than a third thing needing a rule of its
own — but the syntax rule does not refuse it, and something must.

#### Labels annotate; areas authorise

**A label is inert unless discovery already reports a domain by that name.** It is never a
permission, and the agent never sees one: label records are joined against inventory server-side, so
nothing new reaches the agent, no new state is held there, and §4.2's fail-static surface is
unchanged. The destination resolver stays a pure function of one config file and one name (§10.6).

That separation is load-bearing rather than tidy. If labels flowed *down* — for instance to feed the
discoverer's `static` list so a labelled-but-empty domain stayed visible — the API could point an
agent at a path the host never granted. Read-only, but still an exfiltration primitive bounded only
by "must look like an MXL flow". Keeping the join server-side means the perimeter is exactly the
areas that grant `read`, which are node-local and owned by whoever builds the host, precisely as the
`write` grant is (§10.6, §13).

One rule follows, where there used to be two:

- **A label on a domain the node does not report is accepted and inert.** It is a pending record,
  not an error, and it covers three cases that used to be spelled separately: a name in no area at
  all, a name in an area that grants only `write`, and a name in a readable area where no producer
  has yet created a flow. The last is the case the "before or after" in the settled line is about —
  the operator labelling a camera's domain before the camera is switched on.
  `GET /v1/nodes/{node}/domains` lists it, so the intent is visible rather than lost, and a request
  selecting it sits in `WAITING`, which §7.2 already files as legitimately not an error.

*The rule this replaces refused a label on a path under an output root, with the root named.* Its
reasoning was that output domains are derived state with an owner and that a label making one
selectable as a source would be the beginning of a selector that matches this project's own output.
The first half is superseded outright — a domain is observed and has no owner (§10.6) — and the
second is now enforced where the danger actually is, one level down, on the flow. Labelling
`fast/ingest` is an ordinary thing to do: it is a domain on a node, it may hold flows this project
did not write, and an operator has the same reasons to name it as any other.

The cost of the server-side choice is that a labelled domain with no flows is not in inventory at
all, so it cannot be *matched* until a producer appears. That is more honest than the old behaviour,
where a domain existed because a config line said so.

#### The `name` label

One key is conventional rather than special: `name`, if present, is what an operator calls this
domain, and it is rendered as an additional `domain_name` metric label beside the identity-valued
`domain` one (§12). Its value is held to the element grammar (§10.6) — the rule outlived the flag it
was written for.

It is deliberately **not** enforced unique per node. Identity is the domain name, so two domains
sharing a `name` label is cosmetic rather than ambiguous, and enforcing it would make a label write
a cross-record check for no invariant.

#### What a domain selector does not reach

**Settled: a label selector never matches a flow this project is itself writing. Naming a domain
directly reaches everything.**

*This supersedes "a source selector never matches a domain under an output root".* The rule is the
same rule and the cut is the same cut; what moved is the granularity, from the directory to the
flow, because that is where the multicast reading of §10.6 puts ownership and because the directory
was only ever a proxy for it.

The danger it exists for is unchanged. Left open, replication feeds itself: a flow copied to a node
becomes visible on that node, a broad selector matches its own output, and the path set grows on
every pass. It terminates — bounded by nodes × domains × flows, with `flow_conflict` blocking a
second producer into any one flow ID — but the topology it settles into is decided by §7.5's
precedence rather than by anything an operator wrote. An emergent routing algorithm is the wrong
thing to have. A network of receivers that forward what they receive is exactly where loops come
from, which is why every multicast fabric has a rule of this shape.

Two things the relocation buys, and they are the reason it is an improvement rather than a
translation:

- **It is more precise.** A domain holding one replicated flow beside nine a local media function
  produced was entirely invisible as a source under the old rule. The nine are now selectable and
  the one is not, which is what an operator would predict from the rule as stated.
- **It is expressible.** "Under an output root" stopped being a property a domain has once roots
  stopped existing. "This node's target worker is writing this flow" is a fact the agent already
  knows, reports as `replicated` in inventory (§6, §10.6), and cannot be wrong about, since it is
  the process that started the worker.

The signal's one soft edge — provenance is briefly absent after an agent restart — is closed by
§11.1's admission rule rather than by anything here, and §10.6 records why the two cannot disagree
in the dangerous direction.

It also has a cost the directory rule did not, and it is paid rather than accepted: **an excluded
flow has to be visible, or the finer granularity is the less legible one.** Under the old rule the
whole domain was missing from the source's options, which an operator could at least see as a
category. Now the domain is present, the flows are in `GET /v1/flows`, they match the labels, and
three of them quietly do not appear in the expansion. Three things carry it: `replicated` is a
field on `GET /v1/flows`, `describe domain` renders it per flow, and a request whose expansion
dropped a flow for this reason says so in its status rather than leaving it indistinguishable from
a flow that did not match (§9.1). The agent cannot be wrong about the flag — it is the process that
started the worker — so the only way this becomes undiagnosable is by not being reported.

**The `all` flow selector meets this rule from both sides, and the cut is unchanged** (§9.1). With
a **named** domain, `all` is not provenance-filtered — naming reaches everything — so
`{node: B, domain: fast/ingest}` with no flow selector is "forward everything B receives". That is
the shortest expression of a chain and also the shortest expression of an amplifier, and it is
`A→B→C` written once rather than a new hazard: it is explicit on its face, a genuine cycle is still
`loop` (§7.2), and the alternative — refusing `all` on a domain this project writes into — would
reintroduce the directory-granular rule this section replaced, on the one selector where an operator
most obviously means what they wrote.

With a **label** selector, `all` is filtered like any other match, and it is the combination that
will exercise the exclusion cap: every replicated flow in every matching domain is reported as
`self_output`, which on a busy destination node runs past §9.1's cap routinely. That is what the
truncated count is for, and it is the first request shape that reaches it in normal operation
rather than in a pathology.

**A self-pair a selector produced is elided, not rejected.** `same_endpoint` (§7.2) catches source
and destination resolving to one `(node, domain)`, and with a named source that is an operator
having written the same string twice — a typo, decidable from the request, and refused. A label
selector matching the destination's own domain is not a typo: it is the selector doing what it was
asked to, and refusing the request would put its author at the mercy of which domains happen to
carry a label. So the pairing is dropped and the rest of the expansion stands. This is §10.8's
`same_endpoint` argument arriving one step early, which it does because domain selectors make it
reachable without multipoint.

Naming a domain explicitly still works, so §10.6's chaining property is untouched: `A→B→C` is two
requests, the second naming B's domain as `fast/ingest`. **Explicit chaining is intent; matched
chaining is emergence**, and that is the cut.

### 10.8 Multipoint: what it would take

Not built — but most of what this section listed as its preconditions is, so what remains is a
smaller and better-defined thing than when it was written.

**What is built.** Both ends of a request are lists (§9.1), so the *cross product* is here: N
sources against M destinations, with the whole cross-product machinery — per-pair validation,
per-pair negotiation, `PARTIAL` over a path set that shares no fate — in place and exercised. Of the
four arguments §9.1 raised against it, three were discharged there rather than deferred here: shared
fate became `PARTIAL` and a per-source breakdown, the corruption case became `duplicate_source_flow`
at `POST` plus `flow_conflict` per path, and cost legibility inverted into an argument for grouping
ingress. Of the three mechanisms this section asked for, two have landed: `same_endpoint` is an
elision when a label selector produced the pairing (§7.2, §10.7), and the self-output exclusion is
built and is what makes domain selectors safe at all.

**What is left is precisely the selectors.** A source's `node` is pinned and a destination is a
`(node, domain)` pair written out; multipoint is what happens when either becomes a selector.
§9.1's first argument — *"the destination side cannot have a selector; a destination is a
`(node, domain)` pair by necessity"* — **is falsified** once nodes carry labels: "every `role=edge`
node's `ingest` domain" is perfectly well defined. Node labels do not exist (§3.2 of
`docs/open-items.md`), and designing them is the first step of anything here. The asymmetry runs
opposite to the one §9.1 assumed: destination selectors are the benign half — they cannot
self-amplify, and their worst failure is partial applicability — while source selectors carry all of
the danger.

Two requirements survive intact, and both are about the *unenumerated* ends rather than about the
cross product:

- **Cost legibility, which is now the binding precondition.** Enumerating both ends is what keeps a
  request's expansion readable at the moment it is written, and it is the whole of why fan-in could
  land without admission control: a fan-in author typed every node in the request. A selector on
  either end destroys that — three lines can expand to hundreds of paths of uncompressed video, on
  nodes nobody named. **Bandwidth admission control (§13) moves from a roadmap item to a
  precondition**, or at minimum a path-count cap with an explicit override. `apply` printing each
  request's path count (§9.1) is the cheap half and is not a substitute: it reports a number after
  the fact rather than refusing one.
- **Disjointness has to come back as a real check.** While both ends are enumerated it is subsumed
  by `same_endpoint` over the pairings (§7.2) — any intra-request cycle puts some endpoint on both
  sides, which puts a self-pair in the cross product, which is refused. Two selectors cannot be
  compared that way: they intersect over a fleet that changes underneath them, so the check becomes
  `overlapping_selectors` on the *selectors*, refused up front. That is cheaper and more honest than
  eliding cycles edge by edge, which would be routing on the operator's behalf (§10.4's objection in
  a different costume).

The item worth building regardless of whether multipoint ever is remains `describe path` naming its
contributors (§7.2): the conservative merge is undiagnosable today and fan-in makes it common
rather than rare.

---

## 11. Status and failure semantics

**A request is durable intent. A session is never cancelled because it is failing.** Failure is
made *observable* instead:

| Status | Meaning |
|---|---|
| `WAITING` | The flow is not visible in the system, or an agent is not leased. No workers running. Resolves by itself if it appears. |
| `INVALID` | Needs user action. Never resolves by itself. Carries a reason. Stops new sessions; does not tear down running ones (§7.2). |
| `ESTABLISHING` | Connection setup: session created, target assigned, epoch reported, initiator connecting. |
| `PAUSED` | Nothing is being produced at the source — whether or not workers are still up. |
| `ACTIVE` | Media is flowing: the destination flow's head index is advancing. |
| `PARTIAL` | **Aggregates only.** Some of what this request asked for is working and some is not. Never appears on a path, a session or a worker. |
| `DISABLED` | **Aggregates only.** Every destination of this request is parked (§9.1), so it is asking for nothing. Not a fault and never resolves by itself, because nothing is wrong. |
| `DEGRADED` | Established, but flapping — restart count over a threshold in a window. |
| `FAILED` | Repeated permanent-looking failure, or a session whose fabric stopped being viable. Still retried, but surfaced loudly. |

`ESTABLISHING` deliberately covers the whole setup phase rather than splitting it. The sub-steps
(§5.3) are useful in a reason string and in logs, but they are not states an operator makes a
different decision about — everything from "session created" to "first grain received" is one
condition: *coming up*.

**`PAUSED` is the valuable one**, because it separates the two questions an operator has to
distinguish at 3am: *is the plumbing broken*, or *is the source not producing?* Those look
identical from a "no media at the destination" alarm, and they have completely different owners.
`PAUSED` says nobody is writing on the far end — and it says that whether the workers are running
and idle or have been torn down for being idle too long (§11.1, §7.2).

**`ACTIVE` is determined from the flow, not from the worker.** The destination agent reads
`HeadIndex` and `LastWriteTime` through mxl-utils' `Flow.GetInfo()` on the *destination* flow and
reports whether the head is advancing. That is the ground truth for "media arrived", and it is
independent of the worker's own accounting — a worker can report healthy transfers while producing
a flow nothing can read. Worker metrics (`mxl_grains_total`) stay useful as corroboration and for
rate, not as the state signal.

Each status carries a human-readable reason, a machine-readable reason code (§7.2), and the
identity of the component that reported it. Status is visible at every level: request → path →
session → worker, and a request covering several paths (§9.1) aggregates.

#### `PARTIAL`, and why the aggregate needed a word of its own

**Settled: a request whose paths disagree, at least one of which is `ACTIVE`, is `PARTIAL`.
Otherwise the fold is unchanged: worst-state-first over the path set, with a request-wide
`INVALID` leg short-circuiting it.**

*This supersedes "`ACTIVE` only when all of its paths are", which was the whole of the rule.* It
was correct while every path of a request shared a source (§9.1): an idle producer moved them to
`PAUSED` together, so the fold never had to describe disagreement, only the one thing they were all
doing. With `sources` a list, disagreement is the *ordinary* state — a twelve-camera ingest wall has
one camera dark most of the time — and the old fold reports that request as `PAUSED`, which is a
true statement about one path and a false one about the request.

**It is not a fan-in concept.** A group-hint request matching three flows of which one is paused
has always been in this condition and has always reported `PAUSED`; `1 of 3 active` was carried
only in the counts. Fan-in is what made it the common case rather than what created it, and the
state applies to every request shape — the vocabulary must not fork by which end of a request is a
list.

**`PARTIAL` outranks `INVALID`, `FAILED` and `DEGRADED`**, which is the surprising half and follows
from §7.2 rather than from taste. That section already settled that a request whose selector expands
onto twenty paths, one of which conflicts, "is not refused: it reports nineteen paths and one
invalid one with its reason". A fold that promoted the one bad path to the top line would undo that
at exactly the level an operator reads first. So the aggregate answers *is this request doing its
job*, and the loud detail lives where it is actionable: in `Counts`, in the per-source breakdown, in
`status`, which names every non-`ACTIVE` request and what is wrong inside it, and in the per-path
gauges of §12 — a fleet alert on failing **paths** is unaffected by how their requests fold.

**Reusing `DEGRADED` for this was the alternative and is refused.** `DEGRADED` means flapping —
restart count over a threshold in a window (§15.1) — and it is a state a path and a session really
are in. Giving it a second, set-shaped meaning at the request level would make
`mxl_repl_requests{state="DEGRADED"}` uninterpretable, since the two populations need different
responses and would be summed under one label.

`PARTIAL` is therefore the one state in the vocabulary that is **aggregate-only**. Everything else
describes one thing; this describes disagreement among many, so there is nothing for a path or a
session to report it about. That is a real widening of §11's "one vocabulary at every level" and it
is stated here rather than discovered by a UI author: a renderer may show `PARTIAL` on a request
row and must never expect it on a path row.

Two smaller consequences:

- **The reason names the worst non-`ACTIVE` state and how many paths are in it**, and for a request
  with several sources it names the source, not the destination, when the failure is common to
  every destination of one source. Naming the wrong end sends an operator to the wrong node (§9.1).
- **A request with no `ACTIVE` path is never `PARTIAL`.** Mixed `WAITING` and `ESTABLISHING` folds
  worst-first as before. `PARTIAL` claims that something is working, and it must not be said when
  nothing is.

#### `DISABLED`, and why it is derived rather than stored

**Settled: a request with no enabled destination is `DISABLED`. Like `PARTIAL` it is aggregate-only;
unlike every other word here it describes the spec rather than the fleet.**

Neither existing state can carry it. `WAITING` promises the condition resolves by itself when a flow
appears, and this one never will. `INVALID` promises user action is needed and something is wrong,
and nothing is — a parked route is one an operator has already decided about, and rendering it as a
fault means a board with twenty parked legs reads as twenty problems. Reusing either is the mistake
§11's whole table exists to prevent: a state earns a word by being something an operator decides
differently on, and "I turned this off" is decided differently from both.

**It is computed, never stored.** §9.1 puts the flag on the destination entries, so there is no
request-level `disabled` field for this state to agree or disagree with — the fold reads the spec it
was given. That is worth more than it looks: a stored flag and a destination list can drift, and the
drifted state is one where the API says a request is off while its legs are running.

Three consequences, and they mirror `PARTIAL`'s:

- **A path is never `DISABLED`.** A disabled destination produces no pairing and therefore no path,
  so there is nothing underneath for the state to be reported about. `States()` is unchanged and
  `RequestStates()` grows a ninth word — the same split `PARTIAL` already forced, applied a second
  time and for the same structural reason.
- **A partly parked request is not `DISABLED`.** One enabled destination and one parked one folds
  over the paths the enabled one produced, exactly as though the parked one had never been written.
  `DISABLED` is only for a request that expands to nothing *because* everything is off — which is
  distinguishable from a selector matching nothing, and must be, since that one is `WAITING` and does
  resolve by itself.
- **It ranks below `ACTIVE`, not above `INVALID`.** Worst-first ordering is a queue of things to
  look at and this is not one of them. `status` counts disabled requests and names them on a line of
  their own rather than folding them into "what is not active" — parked intent has to stay *visible*,
  because a leg that is off for a reason nobody remembers is the thing this feature makes possible,
  but it must not be *loud*.

`DEGRADED` and `FAILED` are classified from **restart rate and time-to-death**, not from the
worker's exit status — see §15.1.

### 11.1 Idle sources, and why `PAUSED` needs three mechanisms

The sharpest interaction in the design, and easy to miss until it is in production.

Both worker roles self-terminate after a period without a grain — the initiator when it reads
nothing from the local flow, the target when it receives nothing — and the agent restarts them
after a delay. With the timeout at its original hardcoded 10 s, a session that is genuinely
paused, because the source has no producer writing to it, would not sit quietly in `PAUSED`. It
would enter a permanent ~13 s restart cycle.

That is worse than cosmetic, because with hash epochs (§5.2) **every target restart changes the
epoch**, and every epoch change is a report to the server, a recomputed assignment, and an
initiator restart on another node. An idle-but-requested flow would generate a full control-plane
round trip every 13 s, forever, per flow. §8.3 sizes that kind of churn as a fabric-outage
pathology; an idle source is not a pathology at all — replicating a camera that is not currently
live is an ordinary thing to ask for.

**Settled: all three mechanisms, because they cover different timescales.**

**1. The worker's no-grain timeout is configurable** (§15), with a sentinel for "wait
indefinitely", and the agent defaults it to indefinite. This is what makes `PAUSED` a real steady
state rather than a restart loop, and it is the only one of the three that costs nothing on
resume: the workers are still up, the fabric connection is still established, and the first grain
the producer writes moves immediately.

**2. The agent observes the source flow's head index** and reports coarse liveness as part of
inventory (§6). This drives two things:

- *Admission*: the server holds a path in `PAUSED` and starts no workers at all until the source
  is actually being produced. A request for a flow that exists but is dormant costs nothing.
- *Long-idle teardown*: a session whose source has been idle beyond a configurable threshold is
  withdrawn — both workers stopped, path still `PAUSED` with nothing running.

**Admission is load-bearing for safety and not only for churn**, which is worth knowing before
weakening it. §10.7 keeps a label selector from matching a flow this project is writing, and the
signal it runs on — `replicated`, derived from running target workers — is briefly absent whenever
those workers are not up: an agent restart (§6.1), a long-idle teardown, a worker crash. Every one
of those windows is also a window in which the flow is *not advancing*, because the target worker
is the thing that advances it, so admission refuses to start anything over it and the gap cannot
amplify. Admitting a dormant source eagerly would reopen self-amplification through a door that has
nothing to do with idle sources.

**3. Agent-side backoff** on restart, so that if a worker does die repeatedly for any reason, the
cycle stretches toward minutes rather than sitting at a flat delay. This is the catch-all for
everything mechanisms 1 and 2 do not anticipate.

Both knobs are **server-side** (§5.5): the worker timeout is written into both ends of every
assignment, and the teardown threshold needs the source's liveness, which only the server sees.
The idle tracker behind the teardown is leader-local memory, so a leader change delays a long-idle
teardown by one threshold; persisting it would put a continuously-changing value back into the
store, which is the churn this section exists to remove.

#### The two-tier idle policy

Mechanisms 1 and 2 are not redundant — they trade resume latency against resource cost, and the
threshold between them is the knob:

| Source idle for | State | Workers | Resume cost |
|---|---|---|---|
| seconds to minutes | `PAUSED` | running, waiting patiently | immediate |
| beyond the threshold | `PAUSED` | none | one re-establish, 1–2 s (§6.1) |

Tearing down too eagerly means a source that stops and starts frequently loses its first grains to
a re-establish every time. Never tearing down means dormant flows hold ports, memory registrations
and processes indefinitely. The threshold defaults generously — minutes, not seconds — and is
configurable per request as well as globally, because "this feed is bursty, keep it hot" is a real
operational requirement.

#### Observing and reporting liveness

The observation is **local and derived**. The agent watches the head index of every flow in its
domains through the machinery it already has for flow liveness (§6, `Flow.GetInfo()`), and
reports a single boolean `producing` per flow. The server never sees an index.

Three details that matter:

- **Compare head indices across samples rather than deriving liveness from `LastWriteTime`.** The
  timestamp looks more convenient — one sample, no state, `now - LastWriteTime < threshold` — but
  it is TAI nanoseconds and only means anything if the host's TAI offset is configured. A correct
  TAI clock is a deployment requirement for the broadcast datacentres this targets, so this is not
  a scenario to design around; a head-index delta simply needs no clock at all, so take the
  version that cannot be wrong. `LastWriteTime` is kept for diagnostics. The same rule covers read
  activity: `LastReadTime` is read as a number that changes when a reader reads, never as a
  timestamp that means anything on its own.
- **Check `IsValid()` on every sample.** A flow deleted and recreated under the same ID is a
  different `data` file; the old mapping keeps working and keeps returning stale values forever.
  Without the check, a republished flow reports `producing=false` permanently and is never
  replicated again. This is precisely what `IsValid` exists for — reopen when it goes false.
- **Observe every flow in inventory, not only flows with sessions.** Admission needs the liveness
  of flows nothing is replicating yet. `GetInfo()` decodes from a live mapping and is documented
  as cheap enough to call on every scrape, so this is affordable at the §14 flow counts.

**No raw head index in inventory.** It changes every frame, so every snapshot would differ, and
inventory would write to the store on every heartbeat forever — trading the churn this section
exists to eliminate for a slower version of the same thing. Hysteresis (advancing → idle only
after the threshold, idle → advancing on first movement) makes the value change only on genuine
transitions. Rate and head index stay in metrics (§12), where a continuously-changing value
belongs. Read activity is deliberately absent from the inventory snapshot altogether: reporting it
would mean a store write every time a downstream consumer starts or stops, waking every watcher in
the fleet for something no reconcile depends on.

---

## 12. Observability

**Prefixes split by what the metric describes, not by which process emits it** (§2.2). `mxl_*` for
anything about a flow or a transfer; `mxl_repl_*` for control-plane metrics that only exist
because of this project. Each role has its own registry rather than the default one, because two
roles in one process must not merge their expositions.

### Agent

Worker counters scraped from the `AF_UNIX` sockets — `mxl_grains_total`, `mxl_grains_lost`,
`mxl_octets_total`, `mxl_payload_octets_total`, `mxl_last_grain`, `mxl_network_latency_ns`,
`mxl_source_latency_ns` — plus supervisor-level `mxl_worker_restarts`, `mxl_writer_active` and
`mxl_reader_active`. Labels: `direction`, `domain`, `domain_name`, `flow_id`, `session`,
`namespace`, the flow definition's `format` and `media_type`, and the request's user labels.

- **A `session` label.** One flow replicated to two destinations puts two initiators on the source
  node whose `direction`, `domain` and `flow_id` are all identical. Without a discriminator that
  is one series collected twice, which is a gather error that discards the *whole family* — every
  worker's counters on the node, not just those two. It also makes a series joinable to
  `GET /v1/paths`.
- **`format` and `media_type`, and no other definition labels.** Both are low cardinality and
  stable for a flow's life, which is what qualifies a definition field to be a label at all. The
  tempting others fail one of those: a flow's label churns when someone renames it, which splits
  the series, and `source_id`/`device_id` are UUID-cardinality where `flow_id` already carries
  that. Resolved once per worker and frozen, because a label whose value changes mid-life splits
  one series in two.
- **`domain` carries `<area>/<elements>`, the same value on both sides.** *This supersedes a label
  that held an absolute path on the source side and a rendered output name on the destination side*
  — two grammars in one label, which meant a dashboard could not join the two ends of a chain and a
  PromQL author had to know which side they were looking at. A domain's identity is now one string
  (§10.6), so the label holds it; cardinality is bounded by domains per node, which is fine, and the
  value is stable for life, which is the test that matters.

  The unification also takes host filesystem paths off `/metrics`, which is worth recording as a
  security consequence rather than a cosmetic one: `/metrics` is commonly unauthenticated (§13), and
  the old source-side label published the node's directory layout to anything that could scrape it.
  An area name is a name an operator chose for a fleet-wide identifier, which is a different kind of
  disclosure.

  A rendered identity is still not the friendliest thing on a dashboard, which is what `domain_name`
  is for: the value of the domain's optional `name` label (§10.7), empty when there is none — a
  family must have one label dimension, so this follows the same rule as a user label a worker does
  not carry. **Both are resolved once per worker and frozen.** A relabel therefore takes effect on
  the next worker start rather than splitting a live series, which is the only sound treatment given
  that `name` is runtime state an operator can change at any moment.
- **`namespace` is a label, deliberately rather than incidentally.** It rode into metrics for free
  while it was a user label (§9.3); now that it is a real property the decision has to be made
  rather than inherited. Keep it: knowing which partition a transfer belongs to is a question
  dashboards actually ask, it is low cardinality, and it is fixed for a session's life.
- **Quantiles are gauges, not a Prometheus summary.** The worker reports quantile estimates over a
  sliding 30 s window and has no observation count or sum to give (WRS §6). A `_count 0` beside a
  populated p50 is a new series that says something false rather than nothing. The series a
  dashboard selects — `mxl_source_latency_ns{quantile="0.5"}` — is identical either way.
- **User labels are unioned across the pass, not per worker.** A metric family must have one label
  dimension, and user labels come from the request that created each session, so two sessions on
  one node routinely disagree about which keys exist. A worker without a key reports it empty.
  Invalid names, and names that collide with a label this project sets itself, are dropped rather
  than mangled. The reserved set is `direction`, `domain`, `domain_name`, `flow_id`, `session`,
  `namespace` and `quantile` — the last two added with §9.3 and this section's `domain_name`.
- **The liveness gauges are emitted only for a flow this agent observes.** A destination flow does
  not exist until its target creates it, and emitting `0` for "I am not looking at this" would
  report it identically to "nothing is reading this" — which reads as a dead consumer on a healthy
  path.
- **Restarts are counted twice, in two shapes**: a monotonic total for the metric, and the
  windowed list `DEGRADED`/`FAILED` are classified from (§15.1). A counter that decays reads as a
  reset to `rate()` every time the window slides, so the two cannot be one field.
- **The start gate is exposed, and it is not decoration** (§6.3): `mxl_repl_worker_starts_waiting`,
  `mxl_repl_worker_starts_delayed_total` and `mxl_repl_worker_start_delay_seconds_total`.
  Unlabelled, because the gate is one thing per node rather than one per worker. Without them a
  node deliberately spreading a re-establishment over minutes looks exactly like one whose workers
  are failing to come up — and the series an operator would reach for to tell those apart, the
  restart counters, say nothing in the first case. The gauge and the counters are both kept for the
  usual reason: a permit wait is over in seconds, so a gauge alone reads zero during most of the
  event, and a counter alone cannot say the node is queued *now*.

**Settled: scrape the workers on demand, inside the request, through a bounded pool.** The
collecting scraper decides the rate; nothing in this process second-guesses it.

*This reverses an earlier position in this document, which asked for a background scrape on a
fixed interval serving a cached snapshot. The reasoning is recorded because the earlier one reads
plausible.* A cached snapshot is sampled at interval C and served against a scrape interval S, and
the two are unrelated: when they are close, consecutive scrapes return byte-identical counters —
`rate()` reads zero — and the next one jumps, which is a beat frequency in every transfer graph
rather than a uniform small lag. It also lies about liveness in both directions, serving a dead
worker's frozen counters as healthy-but-idle and hiding a new worker until the next refresh, where
an on-demand scrape makes a series appear and disappear with the process and lets Prometheus' own
staleness handling do the rest. And `mxl_source_latency_ns` is already a CKMS estimate over a
sliding 30 s window computed inside the worker (WRS §6), so caching it stacks a second, unrelated
lag onto an estimate that is lagged by design.

The cost the cache was meant to avoid is not there: `Metrics` runs its own listen thread with its
own epoll loop (`src/metrics.hpp:35`), so a scrape never runs on the transfer loop's thread; the
two share only the counter mutex, held for as long as it takes to format five counters and two
five-quantile summaries.

What on-demand does need, and it is less machinery than the cache was: a **bounded pool** so the
fan-out is a fixed cost rather than one goroutine and one socket per worker per request; a
**per-worker deadline and an overall collection deadline**, returning partial results, so that N
wedged workers cannot push the endpoint past the collector's scrape timeout and take the healthy
workers' series with it; and a cap on **concurrent requests**, so two collectors or a retry cannot
multiply the fan-out. The deadlines live on the collector rather than on the request, because
`prometheus.Collector.Collect` takes no context and the registry calls it without one.
`mxl_repl_worker_scrape_duration_seconds`, `mxl_repl_workers_scraped` and
`mxl_repl_worker_scrapes_failed_total` are emitted from *inside* the collection that measured
them, so the duration always describes the exposition it is served with.

### Server

All `mxl_repl_`-prefixed: `requests`, `paths` and `sessions` by state, `nodes_registered`,
`agents_leased`, `leader`, `leader_acquisitions_total`, `registrations_rejected_total{reason}`,
`epoch_transitions_total`, `reconciles_total`, `reconcile_duration_seconds`, `reconciled_revision`,
`store_operation_duration_seconds`, `store_operations_failed_total`, `events_recorded_total{kind}`,
`events_dropped_total{reason}`, `agent_versions` and `build_info`.

- **The fleet gauges come from the last reconcile, and a follower reports none of them.** Every
  one is a property of the whole store, and the only cheap way to have them is to count them while
  something already holds a consistent read — a fresh load per scrape would put a full List on
  every Prometheus interval on every replica, which against etcd is a quorum read of the entire
  key space to answer a question nothing is waiting on. Only the leader reconciles, so only the
  leader has the numbers, and a follower emitting zeroes would render "nothing is replicating" and
  "ask the other replica" identically. `mxl_repl_leader` is the one series every replica always
  exports, and it is what tells the two apart. Leadership ending **drops** the observation, or a
  demoted replica would publish a second, frozen copy of every fleet number beside the new
  leader's live ones.
- **`mxl_repl_requests{state="PARTIAL"}` is a new series and a genuinely new signal** (§11).
  "Requests that are working but not completely" had no spelling before: such a request folded to
  `PAUSED` or `WAITING` and was indistinguishable from one that was doing nothing at all. It is the
  series to alert on for a fan-in request quietly losing a source. The per-path gauges are
  unaffected, which is the point — an alert on failing paths must not change because the request
  aggregating them learned a gentler word.
- **`mxl_repl_requests{state="DISABLED"}` is the second new series and is the opposite kind of
  signal** (§9.1, §11). It counts intent that is deliberately switched off, so it is the one state
  in the vocabulary nobody should page on — but it is worth graphing, because parked legs accumulate
  and a namespace whose disabled count only ever rises is one where somebody is turning things off
  and nobody is deleting them. The per-path gauges are again unaffected, and here they are unaffected
  by *arithmetic* rather than by policy: a disabled destination expands to no path at all, so there
  is nothing for it to be counted as.
- **Epoch transitions are an excellent flapping signal** — a session changing epoch is exactly a
  target restarting — and they are counted server-side because the epoch is a hash and carries no
  ordering (§5.2). Labelled **by node, not by session**: a per-session counter is unbounded over a
  long-running leader, since a session that goes away leaves its series behind forever, while the
  node hosting the target is bounded by fleet size and "which node is flapping" is the actionable
  question anyway. Note the subtlety a live run caught: a restarting target reports *no epoch at
  all* in between, because its old blob describes registrations that died with it, so the last
  known value has to be carried across the gap or the restart this metric exists to catch is
  precisely the one it misses.
- **Store latency is measured at the seam, not in the backends.** There are two backends and the
  point of measuring is that the same control plane runs on both (§8.1), so the numbers have to be
  comparable. `ErrNotFound` and `ErrCompareFailed` are excluded from the failure counter: both are
  ordinary answers this control plane asks for on purpose, and counting them would turn the
  failure rate into a measure of how busy the reconciler is. `Watch` is timed for its call, never
  its channel — a watch's duration is the interval between changes, not a latency.
- Instruments are per-server rather than package globals, because two servers in one process is a
  real configuration (§2.3, §17).

`MXL_LOG_LEVEL` is set on the worker environment from the agent's own log level, and worker output
is re-emitted through the agent's logger.

- **`events_dropped_total` is labelled by where the loss happened**, because the two are different
  problems: `queue` is an agent whose in-memory batch overflowed before it could report, `ring` is
  an object whose oldest entries aged out of its bounded ring (§12.1). Both are expected in a bad
  hour and only the first is a signal that something is being missed.

### 12.1 The event log

Everything in §11 is level-triggered and last-write-wins: it says what is true *now*. An operator
debugging a failing path needs what *happened*, and none of it is currently retained anywhere the
control plane can serve. A path that flapped for ten minutes and is `ACTIVE` again reports nothing
about the ten minutes; a request that went `PARTIAL` overnight and recovered by morning is
indistinguishable from one that never moved. And the sentence that would explain any of it is on
the node, in the agent's log, reachable only with shell access to a fleet member — which is the
thing centralising the control plane was supposed to remove (§1).

**Settled: a bounded event ring per object, anchored on the path, stored as a snapshot rather than
appended to, and excluded from the fleet snapshot.**

The shape the design is measured against:

```
$ mxl-replicator describe path edge-01/fast/ingest/5592a23b-0974-45bb-9388-89ea81c42537
state: FAILED   reason: worker_restarts   session: 7f3a… (epoch 9c21…, verbs/mlx5_0)

events
  12:04:11  info   session established     epoch 9c21…, verbs/mlx5_0
  12:04:12  info   ACTIVE                  first grain received
  12:41:03  warn   epoch changed           target restarted on edge-01
  12:41:04  error  worker exited           ×47 over 6m, last 12:47:22        [log]
  12:47:31  error  FAILED                  worker_restarts
```

#### Churn is the constraint, and it settles the write granularity

An event log is the first edge-triggered, append-shaped thing in a design that spends §6, §8.3 and
§11.1 removing writers, so the interaction with the assignment long poll has to be checked rather
than assumed. It is safe, and for a reason worth recording because it is not obvious from §9.2's
prose: the poll watches the node's **own** assignment key and gates on that key's `ModRevision`, so
a write under a different prefix does not wake it. Events do not cost a fleet-wide reconcile.

What they do cost is store revisions, and the sqlite backend bounds watch history by revision
*count* (§8.1). A writer chatty enough can therefore compact out a long poll's cursor. That is
survivable — the agent re-reads, and §7.3's already-correct test means an unchanged set restarts
nothing — but it is exactly the pressure this design does not otherwise apply, and it settles the
granularity: **one write per reconcile pass, never one per event.**

#### One key per object, holding a ring

An object's events are a bounded ring inside a **single value**, rewritten on each flush, not one
key per event. This is §9.2's full-snapshot discipline arriving a third time: an append-only stream
needs sequencing, gap detection, compaction and a garbage collector, and a ring in one value needs
none of them. It reads in one `Get`, it is bounded by construction, and it is deleted by deleting
one key.

**The ring is bounded by count and not by age.** Fifty entries per object. An age bound reads more
principled and is the wrong choice here: the overnight failure an operator arrives to at 09:00 is
the case this log most exists for, and an age bound expires it precisely then. The cost of
count-only is that a path which failed last week still holds the ring saying so — a fixed, small,
per-object amount of store, on objects that are already one key each.

**Coalescing is what makes that bound hold, and it is also the better rendering.** Consecutive
entries of the same kind on the same object collapse into one carrying a count, a first-seen and a
last-seen. Forty-seven identical worker exits become one row that says *flapping*, which is what an
operator needs to read, and they stop evicting the establishment history that explains them. Fifty
entries is a generous ring once nothing repeating can fill it.

**A kind whose identity lives in its message does not coalesce at all**, and that exception is
load-bearing rather than a caveat. The merge deliberately ignores the message — "exited after 1.2s"
and "exited after 0.9s" are one worker failing twice — but an entry that *names* what it is about is
a different fact each time it is written, so folding two of them keeps the newest and silently
discards the other's contents. It was found in a live fleet rather than predicted here: four flows
appearing in four consecutive passes rendered as `1 flow appeared: …b8d6c502 ×3`, naming one of the
four and losing three. The property is declared on the kind rather than patched into the comparison
field by field, because the field-by-field version is what whoever adds the next kind forgets.

**The fleet snapshot excludes `/events/`,** and §7.3's "keys outside the three layers are ignored"
is already the hook. This is not tidiness. Every user-API read is O(fleet) rather than O(response),
because it loads the whole store and runs `Compute` (§7.3, and the cost is why it is an open item),
so folding a diagnostic log into that key space would make every unrelated read pay for it —
including the reads a UI makes most often.

**Events are never an input to `Compute`.** They are a side effect of `Apply`, and the purity §7.5
depends on is untouched. A reconciler that read its own event log would have history affecting a
decision, which is the one thing §7.3 forbids outright.

#### The path is the unit of retention

**Settled: events are anchored on the path. A session is a *field* on an event, not a log of its
own.**

The failure being debugged belongs to the path: a request's state is a fold over paths, which is
why `PARTIAL` had to exist at all (§11), and a session is ephemeral by definition (§3). A
session-scoped log fragments the story at exactly the boundary under investigation — a
re-establishment is where one session ends and the next begins, so the events on either side of the
interesting moment would land in two logs, one of which is being deleted as the operator reads it.
Anchoring on the path also gives the log a **stable key for free**: path IDs are derived
deterministically and survive server restarts and leader changes (§5.4, §7.3), where a session ID
changes whenever the source flow definition does.

**A request carries a small log of its own, and it is not a wrapper over its paths.** What goes in
it is what is genuinely request-scoped and has no path to live on: an admission refusal, an
expansion that changed (a selector that matched three flows and now matches two), a path lost to
§7.5's precedence, a leg parked or un-parked (§9.1). The case that proves it is a request expanding
onto **nothing** — there is no path, and *"why is this `WAITING`"* is the question with nowhere else
to be asked. Its rendered view is its own entries merged with those of the paths it currently
expands onto.

**A node carries one, and it is cheap by construction:** registration and re-registration, lease
expiry, `node_claimed` (§6), interface probe results (§10.5), start-permit saturation (§6.3). It is
what answers *"why did every path on edge-01 re-establish at 12:04"* in one line instead of in fifty
identical path entries — and it is the log that still exists after the paths are gone.

**No log on a flow or a domain — but flows and domains moving is recorded on the *node*.** *This
supersedes "a flow appearing and disappearing is inventory and belongs nowhere here", which was
argued from cardinality alone.* The objection is real and is answered by where the entries go and
how they are batched rather than by leaving the fact unrecorded: a flow that vanished is the
explanation for a request whose selector quietly stopped matching and for a path that went `WAITING`
with nothing else to say why, and an operator who cannot see it has to infer it from an absence.

Four properties make it affordable, and the last one is the one that would have been got wrong:

- **On the node's ring, not on a flow's.** A flow is not an object here (§3 — a flow ID is not
  unique to a location) and most flows have no path. The node is the thing that gained or lost them.
- **A disappearance is `info`, not a warning**, and so is an appearance. A producer stopping is an
  ordinary thing for a fleet to do; it is the same fact `PAUSED` exists to keep out of the fault
  vocabulary (§11), and the argument is the one that section makes a second time for `DISABLED` — a
  board where routine churn renders as a fault is a board where twenty non-problems read as twenty
  problems. Where a disappearance actually causes something, that consequence is recorded where it
  belongs and carries its own severity: the request whose expansion shrank, the path that went
  `WAITING`. These entries are the fact, not the verdict.
- **Batched per reconcile pass, never per flow.** A node restarting takes fifty flows away and
  brings fifty back (§14); one entry each would evict a fifty-entry ring twice over and take the
  registration entry that explains the whole episode with it. One entry per kind per pass names what
  it can and counts the rest — the same shape a request's excluded-flow list already uses.
- **It is the one part of the log with a switch**, on by default, because it is the one part whose
  volume is set by the *fleet* rather than by the control plane: a node's flows are whatever its
  producers are doing, and a deployment where that churns constantly should be able to turn this off
  without losing the rest.
- **A node observed for the first time gets a baseline, not a flood.** A leader cannot honestly
  report a first observation as an appearance — those flows may have been there for days, and saying
  they just arrived is the fabricated storm the takeover marker exists to avoid. But the alternative
  it originally took, silence, is indistinguishable from a node whose flows never appeared, which is
  how it was caught: an operator reading a source node's log after an episode saw nothing and
  concluded the feature was broken. So the leader states where the record begins —
  `first observed holding 4 flows in 1 domain` — which is the same honesty in a form that can be
  read. It is emitted on the pass *after* the seeded one, deliberately, so that whether a node gets
  one does not depend on a race between its first inventory report and the settling window.
- **A node that is not leased is skipped entirely.** Its inventory is leased state, so it is gone
  from the snapshot the moment the lease expires — and diffing against that reports every flow on
  the node as having disappeared at the exact moment nothing happened to any of them. This is §4.2's
  closing line arriving one layer up: *"no observation" is never "nothing there"*, and the mechanism
  is freezing rather than converging. The node's inventory memory is carried forward exactly as its
  paths' assignments are, so its return is silent too.

**The honest limitation: a flow that appears and disappears between two passes is not recorded.**
The journal is level-triggered like everything else here, so it compares snapshots rather than
watching a stream. A producer that flaps faster than the reconcile cadence shows up as nothing at
all — which is the correct trade in the direction that matters, since the alternative is an
edge-triggered watcher on the highest-churn state in the system.

**A path's log dies with the path, and a request's with the request.** No tombstone, no grace
period. Retention outliving its object needs a second lifecycle, a TTL and a sweeper for one
question — *"why did the thing I deleted fail?"* — which the node log still answers, and which
is asked after a deliberate act rather than in the middle of an incident.

#### Two producers, and the leader has a gap to declare

**The leader emits from `Apply`'s diff, which already exists.** `Apply` computes what changed in
order to write it; an event is that diff rendered for a human instead of for the store. Only the
leader reconciles (§8.2), so there is one writer and no de-duplication problem.

Path *state* is derived and not stored, so its transitions are **not** in that diff: they need the
previous pass's computed states, which is leader-local memory. The consequence is stated here
rather than discovered: **a newly elected leader has no baseline, so its first pass emits no state
transitions and writes one `reconciler_took_over` entry — the gap is marked rather than left to read
as quiet.** The alternative, emitting every current state as though it had just happened, produces a
fabricated storm on every leader change and every server restart. That is the same mistake §7.3's
settling window exists to prevent, one layer up, and the same remedy: a server that has just started
must not act as though what it is seeing for the first time just happened.

**Agents never write the store (§4), so agent events reach it through the agent API** — and not on
the status snapshot, for the reason §9.2 gives. `POST /agent/v1/{node}/events` carries a batch from
a bounded in-memory queue, drained on send, delivered at-least-once and de-duplicated server-side on
a per-agent sequence number. What the agent contributes is the half the server cannot see: why a
worker exited, that a start is queued behind a permit, that an assignment could not be honoured
(§6), and the log tail of §12.2.

**The agent holds no persistent state (§6.1), so an agent restart loses whatever is pending, and an
overflowing queue drops its oldest.** Both are accepted and both are announced — an overflow emits
an `events_dropped` entry carrying the count, on the same principle as the leader's takeover marker:
a gap in this log is always visible in this log. It follows, and must be said before anybody builds
on it, that **this is a diagnostic aid and not an audit log.** Nothing here is a record of what
happened; it is the best account two processes with bounded memory can give of it.

#### Reading, ordering and vocabulary

`describe` renders an object's log under its status, and the three `events` endpoints serve it
(§9.1).

**A withdrawal is recorded only when its path survives it.** A session taken away by a long-idle
teardown (§11.1) or a lost conflict leaves a path with no session and nothing in its status saying
that used to be different — that is the case the entry exists for. A request being deleted takes the
path with it, so the entry would go to a ring being deleted in the same pass; and a *rebuild* — a
new epoch, a republished definition — replaces the session and already reports itself as an
establishment, so recording both would put two entries on every target restart.

**The gap marker is written once and merged into every read.** A leader change leaves a gap in
every object's log, and the entry explaining it belongs to no object — so it goes to a fleet ring of
its own, and a read merges that ring into what it returns. One write, visible wherever the gap is,
against the thousand writes that putting the marker in each path's ring would cost at exactly the
moment the fleet is already churning.

**Two rules bound that merge, and both exist because the marker makes a claim** — *transitions
before this point were not recorded* — **which is only true of some objects.** An object with no
entries of its own reads as empty, or a path whose log died with it comes back holding the control
plane's entries, rendering a deleted object as one that still exists. And only fleet entries inside
the object's own lifetime are merged: a takeover that happened before a path existed cannot have
lost any of *its* transitions, and putting the marker there tells an operator to distrust a log that
is in fact complete. Lifetime is taken from the oldest entry the object still holds, which errs the
safe way — a ring that has dropped its oldest entries hides a marker from before that point, and
that gap is already reported by the ring's own dropped count on the same read.

Each entry carries a **monotonically increasing per-ring sequence number**, starting at one. That is
what orders the ring and what a poller resumes from. Starting at one rather than zero is not
cosmetic: a cursor of zero means *everything this ring still holds*, and a reader resumes from
entries above its cursor, so a zero-numbered entry is indistinguishable from one already seen and
would be filtered out of the first read of every ring.

**Timestamps are for display only.** An entry is stamped by whoever emitted it, so a request's
merged view interleaves the clocks of two agents and a leader.
TAI correctness is a deployment assumption (§11.1), but it is an assumption about offsets rather
than about ordering across hosts, and a log that implied otherwise would invite an operator to read
causality out of two nodes' timestamps.

**The vocabulary is closed and every word in it is emitted by something.** Two candidates were
written down and then removed rather than left unemitted, and both failed the same way — the thing
they would describe happens where no ring exists to hold it:

- **`request_rejected`.** A refusal at `POST` happens *before* the request is written (§7.2): the
  handler computes against a candidate fleet, finds it structurally invalid and returns 400 without
  creating anything. There is no request, so there is no ring. Every refusal that is recordable has
  a request behind it and arrives as a state change carrying `INVALID` and the code that refused it.
- **`interfaces_probed`.** The probe's result *is* the registration body (§10.5), so the server
  already holds every number such an entry could carry and records them on the registration entry —
  a separate one would be the same fact written twice, one store write apart. And a probe that
  fails cannot be recorded at all: the agent has no lease, so it is not registered, so it has
  nothing to report through. That surfaces as a node which never appears.

The general rule they are both instances of: **an entry needs an object that exists at the moment it
is written.** A vocabulary word with no such moment is not a gap in the implementation, it is a
category error, and leaving it defined would invite someone to go looking for the emission site.

**A kind is a closed vocabulary and a reason code is §7.2's, not a second one.** Free text is
unqueryable, untranslatable and impossible to coalesce on; the message is the human rendering of a
kind and its fields, computed at the point of display. This is the same rule §11 applies to status
reasons, and the two vocabularies must not fork — an event about a path going `INVALID` carries the
code that path is reporting.

### 12.2 Worker log tails

**Settled: the agent keeps a byte-bounded tail of each worker start's output and pushes it with the
transition into `FAILED`.** *This supersedes §19's "worker log retrieval through the API", which is
built rather than roadmapped, in the narrower shape described here — a tail attached to a failure,
not a general log-retrieval facility.*

The line that explains a failure is usually the worker's own: `fatal: unknown error: failed to
create flow writer` says in one sentence what `FAILED` / `worker_restarts` cannot say at all.
Without this it is on the node and nowhere else.

**The capture point is the pump that already reads every line.** The agent re-emits worker output
through its own logger, parsing spdlog's format and passing through what it does not recognise
(§12); a ring beside that costs one buffer per running worker and no new plumbing. That the parser
already treats unrecognised lines as things to keep rather than drop matters here too: a tail that
held only what the parser understood would omit whatever a library on the link line printed on its
way out.

**Bounded in bytes, not in lines.** A flow definition inside an error message is a line the size of
a flow definition (§15), and a line budget would let one of them evict a whole start's history.

**The tail, not the head.** A worker's fatal line is its last, in both failure shapes — one that
never comes up, and one that dies after hours of healthy transfer. The cost is real and accepted: a
run with `FI_LOG_LEVEL=debug` puts libfabric's own diagnostics through the same logger (§12) and
can push the setup lines out of the window. Those lines are reproducible; a fatal is not.

**Pushed on the first death of a crash loop, not on each restart of it.** Forty-seven restarts
produce one tail — §12.1's coalescing rule applied to the payload rather than to the entry, and for
the same reason: the forty-seventh copy of one message is not evidence, it is volume.

**What re-arms it is time-to-death, not the worker having reached ready.** Worth recording, because
the obvious rule is wrong in a way that passes a casual reading: a target binds, writes its blob and
genuinely *is* ready before it dies on a timeout, so it reaches ready on every turn of the loop, and
resetting there pushes a tail per restart while looking like it does not. The signal that works is
the one §15.1 already classifies from — a worker that ran for a while before dying is a new incident,
and whatever killed it after minutes of healthy transfer is not what the first attempt's output
describes.

**Stored in its own key and fetched by its own endpoint.** The event carries a marker that a tail
exists; `GET /v1/paths/{id}/logs` returns it. Inlining a few KiB per failure into the ring a UI
polls would make the cheap read expensive exactly when things are failing, which is when it is read
most.

**The capture size is agent-local and the accepted size is server-side**, which looks like two knobs
for one thing and is not. The buffer is a property of the host, like the port range and the start
rate (§6.2). The cap on what the endpoint will store is a property of the store, and it has to exist
independently: an endpoint that accepts unbounded bytes from a node is a store-filling primitive
handed to every member of the fleet. Anything over the cap is truncated at the head, keeping the
tail, and says so in the response.

**A disclosure note, because §12 points the other way.** Worker output carries filesystem paths, and
§12 deliberately took host paths *off* `/metrics` on the grounds that `/metrics` is commonly
unauthenticated (§13). The event log and the tail endpoint are on the authenticated user API, so
this is a different exposure and the two decisions are consistent. They read as contradictory, which
is why the reason is written down instead of inferred: the rule is not "host paths are secret", it
is "an unauthenticated endpoint publishes to anyone who can reach it".

---

## 13. Security

The threat model is stated, because it changed when the control plane centralised.

**Scope:**

- **A single shared bearer token**, configured on the server and on every agent. Optional —
  no-auth is a supported configuration for a trusted network and for development.
- **TLS optional**, terminated either by the server or by an HTTP proxy in front of it.
- **No mTLS.** Deliberately deferred: certificate distribution and rotation across a DaemonSet is
  a larger operational commitment than this project should take on before it has users asking for
  it.

The token check lives in middleware with the identity it establishes carried on the request
context, so per-node credentials or mTLS can be added later without touching handlers.

The threat model, recorded so the deferral is a decision rather than an oversight:

- The **agent API is the privileged one**. Anything that can call it can claim to be a node,
  inject fabricated flow inventory, and read other nodes' `target_info` — a set of RDMA rkeys. A
  shared token means any holder can impersonate *any* node; per-node credentials are the first
  upgrade if that matters.
- The **user API is a resource lever**: a replication request moves uncompressed video between
  hosts, so an unauthenticated one is a fleet-wide bandwidth exhaustion primitive. This is the
  main argument for turning the token on outside a trusted network.
- **Applying a domain label is as powerful as writing a request** (§10.7). A label is what a source
  selector matches, so labelling a domain can join it to an existing request's expansion and start
  moving media without anyone touching a request. Under one shared token this changes nothing —
  a holder can already do both — but it is worth recording, because it forecloses a per-credential
  split between "may provision a node" and "may route media", which is the first separation an
  operator asks for after per-node credentials. If that split is ever wanted, labels and requests
  have to be separately authorised.

  What labelling does **not** grant is any widening of the perimeter: a label is inert unless the
  node already reports a domain by that name, so it can only ever name something the host already
  exposed through an area granting `read` (§10.7). *An earlier version of this bullet also said
  labels were refused under an output root; they are not any more, and nothing here depends on it —
  a domain this project writes into is reported like any other and labelling it names something the
  host exposed just the same.*
- **A destination is always a name inside an area the operator granted `write` on** (§7.2, §10.6).
  This is the single most important invariant in the design — it is what stops the API from being
  a remote arbitrary-filesystem-write, and it holds regardless of what authentication is
  configured. A node with no writable area is not a destination at all.

  *This is the same invariant as "always a name inside an operator-configured output root", restated
  for areas.* Unifying search paths and output roots into one noun (§10.6) does not widen it: the
  grants stayed two, they stayed independent, they stayed node-local, and neither is settable by the
  API. What changed is that "may this be written" is a field on an entry rather than which of two
  tables the entry is in — so the agent reads it explicitly where it used to be carried by
  construction.

  Note what the API *can* do once an area grants writing, because it is a real escalation over a
  supervisor that only ever started processes: the server can cause directories to be created on
  that node. They are confined to the area by construction, they are named by a list of validated
  path elements — up to eight of them, so the reach is a bounded tree inside the area rather than
  a single directory (§10.6) — and their content is MXL flows. But the server's reach is no longer
  "processes only". The grant is the entire perimeter, it is one line of node-local configuration,
  and it is owned by the host rather than by the control plane. That is the right place for it,
  and it is why it is not something the API can set.
- **Discovery is not a grant, and un-pruning it does not make one.** Domains this project writes
  into are now discovered and reported (§10.6), which widens what a scrape of the user API reveals
  about a node — nothing more. Reading is still bounded by the `read` grant, writing still by
  `write`, and the guard against replication feeding itself moved to the flow rather than being
  dropped (§10.7).
- Admission control is a natural future policy hook: `docs/third_party/mxl/FabricsBandwidth.md`
  gives exact per-flow wire bandwidth from the flow definition, so the server can compute committed
  bandwidth per node and per link and refuse requests over a configured budget. Not implemented;
  the request path is shaped so it can be inserted (§19).

### 13.1 Version skew

**Settled: the server is always upgraded first.** The server tolerates agents one or more versions
behind; agents may assume the server is at least as new as they are. This matches the deployment
shape — a Deployment rolls faster than a DaemonSet — and it means new assignment fields must be
additive and ignorable by an older agent.

**Settled: the gate is the protocol version, not the build version.** Agents report both at
registration; the server surfaces the fleet's version spread as a metric, warns about an agent
behind its own protocol, and **refuses** one whose protocol is newer — the one direction the
compatibility promise does not cover. Keying a hard refusal on the *build* version instead would
be unsatisfiable on a combined instance (§2.3): upgrading one upgrades both roles at once, so
during any rolling upgrade of M combined nodes there are older and newer servers in the fleet at
once and a newer agent can reach an older server. Two mitigations, both in place: the gate is the
protocol version, and the co-located agent dials its own server over loopback, which is by
construction its own version.

---

## 14. Scale

**Settled: one process per flow per direction is fine.** The per-worker overhead is small next to
the processing the downstream media functions are doing on the same flows, so the process count is
not the binding constraint at the scales in view. A node receiving 50 flows runs 50 target
processes, each mmapping and RDMA-registering memory and holding a metrics socket; that is
acceptable.

**Amended: the steady count is fine, and the *transient* is what binds.** Fifty workers running is
what this section sized and it holds. Fifty workers coming up **at the same instant** does not, and
it was found in production rather than predicted here: memory registration is pinned-page
allocation against a host-wide limit, and it is paid at start. The correction is a rate, not a
smaller number of workers — §6.3 paces starts — and it is worth stating in this section because the
distinction it draws is the one this section originally missed: what a node can *hold* and what it
can *bring up at once* are two different capacities, and only the first was ever sized.

This could change. If a deployment ever wants **hundreds** of flows per node, the
one-process-per-flow model becomes the constraint and a multi-flow worker is the answer. The
design guards against that being a rewrite rather than a substitution:

**The worker is a replaceable module.** The agent talks to it through an interface — start a
transfer for this session with this config, tell me when it is ready and what its `target_info`
is, give me its metrics, stop it — not through `os/exec` calls scattered through the supervision
code. Nothing above that interface assumes a 1:1 process-to-session mapping, a filesystem work
directory, or an `AF_UNIX` metrics socket; those are properties of *this* worker implementation,
and `os/exec` appears nowhere outside `internal/worker/exec`.

This is not speculation-driven: the same interface is what makes the control plane testable
without MXL or RDMA hardware (§17), so it pays for itself immediately and the future-proofing is a
side effect.

---

## 15. The worker

**Settled: the worker source may be modified.** It lives in this repository under `src/`, built by
the top-level `CMakeLists.txt`. "Reusing the worker" means not rewriting it — it does not mean
freezing it. `docs/worker-runtime-surface.md` is the contract document and must not drift in the
same commit range that changes the contract.

Three changes were required by the design and are in place:

1. **A configurable no-grain timeout.** `idle_timeout_ms`, default 10000, `0` or `-1` for "wait
   indefinitely". Without it `PAUSED` is not a sustainable state and every idle replicated flow
   generates a control-plane round trip every ~13 s, forever (§11.1).
2. **An interface probe mode.** `--interfaces` calls `mxlFabricsGetInterfaces()` and prints the
   list as JSON (§10.5). This is how the agent learns what the node can actually do, instead of
   guessing from `/dev/infiniband` and interface names. Purely additive; it touches nothing on the
   transfer path.
3. **The negotiated interface config.** The config carries `caps_flags` and `max_message_size`
   alongside `provider` and passes them to `mxlFabricsTargetSetup` and the initiator setup, because
   the library does no negotiation of its own and both ends must be given the same values (§10.3).
   Absent means the library default, so an older config still works. **`caps_flags` is an array of
   the same names the probe prints** (`REMOTE_WRITE`, `SEND_RECEIVE`, `BLOCKING_OPERATIONS`)
   rather than a bitmask, so the intersection in §10.3 is a set operation on one vocabulary end to
   end, with no bit/name translation anywhere.

Plus the hygiene that came with them: a `connect_timeout_ms` on the initiator's connect loop,
which otherwise waits forever for an unreachable target; `unlink()` on the metrics socket before
`bind()`; a `return` after logging a non-interrupt `mxl::Exception`, which used to fall through to
`return 0` and report success after printing `fatal:`; and a shadowed variable that made
`mxl_grains_lost` always read 0 on the initiator side.

Two more found by running it: `target-info.json` was written with a **trailing NUL byte** (the
library's size includes the terminator), which `encoding/json` rejects — stripped at the source,
though the Go decoder still trims one so a mixed-version deployment does not look like a corrupt
blob. And an over-long `metrics_socket` path was silently truncated into `sun_path`, so two
workers under a long parent directory bound *the same* truncated path and the second died with
`EADDRINUSE` naming a socket it never configured; it is now a clear `ENAMETOOLONG` at startup.

### 15.1 Why the exit code is not a status signal

Worth recording, because it is a tempting-looking dependency that does not survive contact:

**The agent already knows a death was unexpected, because it did not send the signal.** Exit
status adds nothing to that. And as a plain 0→non-zero change it classifies nothing either:
`mxl::Exception` covers invalid config and bad providers (permanent) alongside timeouts and the
flow-not-found startup race (transient), so both outcomes land in the same bucket. Meaningful
classification would need distinct exit codes per error class — a much larger change, and not one
anything here needs. The exit-code fix above is therefore *not* load-bearing; it is there because
reporting success after printing `fatal:` is simply wrong.

The signals that actually work are behavioural, and the agent computes all of them:

- **Restart rate over a window** → `DEGRADED`. A worker cycling repeatedly is degraded whatever it
  returns.
- **Time to death.** Dying in under a second, every attempt, is a permanent error — bad config,
  missing domain, incompatible provider. Dying after minutes of healthy transfer is transient.
- **Source liveness from the head index** (§11.1) → `PAUSED`. Positive evidence that the producer
  stopped, rather than an inference drawn from a worker's death. This is the better signal for the
  idle case regardless of what the worker reports, because it is available even when no worker is
  running at all.

The brief window where the agent has decided to tear a session down and the worker exits on its
own for an unrelated reason is a non-issue: a momentary `DEGRADED` that the next reconcile clears.

---

## 16. Relationship to `mxl-fabrics-proxy`

`mxl-replicator` replaces `mxl-fabrics-proxy`, in which each node carried a static subscription
list, fetched flow definitions from its peers over HTTP, and exchanged `target_info` peer-to-peer
under a 9 s keepalive with a 20 s far-side expiry. **That proxy is retired.** There is no wire
compatibility with it and none was planned.

**Settled: there is no config compatibility for the operational half either.** *An earlier version
of this section promised a one-shot importer reading a legacy `config.yaml` and emitting requests,
and made the manifest format's job partly to stay expressible from the old one.* That was dropped
before v1: the legacy file is a *per-node* config whose subscriptions are addressed by `mxl://`
URL and whose destination is a `-m` mapping — three things this design deliberately moved. Intent
is fleet-scoped, sources are selectors, and destinations are names inside a writable area (§10.6).
A format shaped to remain convertible from it would inherit constraints that no longer apply, to
save hand-editing on deployments small enough to re-author in an afternoon.

What carried over is the **provisioning** half, and it is the reason a few things look the way
they do:

- ~~**The domain mapping config.**~~ **Settled, superseding this bullet: `-m` and the `domains:`
  YAML block are removed, and there is no domain-mapping compatibility with the retired proxy
  either.** *The earlier position kept the `-m name=/path` syntax byte-compatible, including the
  legacy spellings, on the reasoning that "it costs nothing to keep — it changes when a host is
  built, not when a flow is routed."*

  Both halves of that turned out to be wrong. It did not cost nothing: it bought §10.6 an exception
  and a second rule to police it, a `domain_path_in_use` rejection code checked at both ends, and a
  `Configured` flag that outlived its purpose as a security bit and stayed on the wire as
  decoration. And it *is* what changes when a flow is routed — naming a domain is the one thing on
  the agent's configuration list an operator does while routing rather than while building, and
  doing it there cost an agent restart, which re-establishes every flow on the node (§6.1).

  Once it costs something, the argument inverts. Domains are discovered under a readable area and
  named by API labels (§6, §10.7); a legacy mapping used as a subscription destination becomes a
  domain name inside a writable area, and one used as a source becomes a label. The *rule* on names
  survives where the syntax did not: the same grammar now governs a domain's elements, an area's
  name and a `name` label's value (§10.6).

  *A later note on the same argument.* The exception `-m` bought §10.6 is gone twice over — the
  mapping removed it once, and unifying search paths and output roots into areas removed the rule
  it was an exception to. What remains of it is one line: areas may not share a path.
- **`mxl_*` metric names are unchanged**, so existing dashboards and alerts keep working — worth
  more than nominal consistency with the control-plane prefix (§2.2). The `session` label is
  additive, so selectors carried over keep working.
- **The default server port is 2283**, the port the proxy used.

An importer remains plausible, and it is better written against the manifest format than against
the API — its output is then reviewable before it is applied, which a one-shot design would not
allow (§19). If it is ever written, `defaults.provider` must become a request-level pin rather
than a widened default: it was a per-side setting, whereas a provider is now negotiated per
session against declared fabric attachments (§10), and silently widening what an existing
deployment asked for is exactly the substitution §10.4 forbids.

---

## 17. Testing

**The worker launcher is an interface with a fake implementation, and that is structural.** It is
what makes the entire control plane testable without MXL, without libfabric and without RDMA
hardware — the same interface that keeps the worker replaceable (§14), one abstraction with two
payoffs.

With the fake worker plus `mxl-utils`' `pkg/testutil` — which builds synthetic flows on disk that
`pkg/mxl` can open, including the delete-and-recreate case `IsValid` catches — the whole control
plane is exercised in a temp directory:

- discovery → inventory → request → path → session → assignment, end to end, in-process;
- epoch changes and initiator convergence (§5.2), by having the fake target return a different
  `target_info`; and the degenerate case the nonce exists for — a fake target that returns a
  **byte-identical** `target_info` after a restart must still cause the initiator to reconnect;
- both storage backends against the same conformance suite, written against sqlite and run
  unchanged against a real etcd;
- HA leader change mid-reconcile, and a server restart with the settling window (§7.3) — which
  must produce **no** worker restarts when the fleet is already in the desired state;
- **fail-static behaviour (§4.2)**: with the server unreachable *and* with the server answering
  not-ready, a reconcile must be skipped entirely rather than run against an empty set;
- **incidental differences restart nothing** (§7.3), with the perturbation applied on the wire
  rather than in memory, because that is where it comes from in production;
- selector expansion (§9.1): a group-hint request gaining and losing paths as `testutil` creates
  and removes matching flows;
- fan-in (§9.1): two sources on different nodes into one destination domain, producing two paths,
  one materialised domain and one target worker each; a pairing that fails validation invalidating
  its own leg while the request's others establish; and the reason naming the **source** when the
  failure is common to every destination of one source, the destination when it is common to every
  source of one destination, and neither when it applies to every pairing — three cases, because
  naming the wrong end is the failure this rule exists to prevent and it passes any test that only
  checks the message is non-empty;
- the corruption case (§7.2, §7.5): two sources pinning one flow UUID against a shared destination
  refused at `POST` as `duplicate_source_flow`; the same collision arriving from a selector after
  the fact classified `flow_conflict` on the path, tearing the loser down and naming **both**
  sources rather than the winner alone;
- `same_endpoint` over the cross product (§7.2): a request whose source and destination sets
  intersect refused with both indices in the message — which is also the test that an intra-request
  cycle is unwritable, since a cycle always puts some endpoint on both sides;
- the `all` selector (§9.1): a source with no flow selector replicating every flow in a named
  domain and gaining a path when a producer adds one; the same selector against a label-matched
  domain excluding this node's own output while a locally-produced sibling flow still matches; and
  an absent `select` on the **wire** refused, where the same omission in a manifest is filled in —
  the one place the default may be applied;
- `PARTIAL` (§11), which is four cases: a request with one dark source among three reporting
  `PARTIAL` rather than `PAUSED`, with the counts and the per-source breakdown beside it; a request
  with one `INVALID` leg and working paths elsewhere still reporting `PARTIAL`, which is the
  property §7.2's per-path validation would otherwise lose at the top line and the one a worst-wins
  fold passes every other test without having; a request with no `ACTIVE` path never reporting
  `PARTIAL`; and `PARTIAL` never appearing on a path, a session or a worker;
- materialisation (§10.6): a destination directory that does not exist before the request and is
  watched immediately after, so the path can reach `ACTIVE`; two requests sharing a destination
  materialising it once; a domain in a read-only area refused as a destination; every rejection
  refused by the server *and* independently by the agent;
- areas and naming (§10.6): a domain under nested areas named by the innermost one; a directory
  reported by discovery *and* materialised by the reconciler resolving to one name and one
  inventory entry, in both orders — including the case pruning existed for, a leftover directory
  holding a flow discovered before the assignment that materialises it, which must reach `ACTIVE`
  rather than strand in `ESTABLISHING`; a materialised domain surviving in inventory when its last
  flow is released while a session still targets it, and leaving when both discovery and the
  reconciler have let go; an area repointed to a different directory keeping every domain identity
  on the node, so paths and sessions survive the restart rather than rebuilding;
- domain labelling (§10.7): a label applied before its domain is discovered, resolving by itself
  when a producer appears; a relabel changing a request's expansion **without** restarting a worker
  on a path it still matches — the property the annotate-don't-rename decision exists for; a label
  on a domain in a write-only area accepted and inert; a label selector declining to match a flow
  this node is writing while a sibling flow in the same domain, written locally, still matches, and
  a request naming that domain directly still chains;
- label selector semantics (§10.7): two keys ANDed, a domain carrying one of them not matching; a
  selector with no keys refused by the validator and not merely by the manifest's scalar-versus-map
  rule; a selector matching the request's own destination domain **eliding** that pairing while the
  rest of the expansion stands, against the named-source form of the same pairing refused as
  `same_endpoint`;
- label ownership (§9.1), which is three cases and not one: an apply **leaving** a key an imperative
  `label` added and never declared; an apply **removing** a key it declared on a previous pass and
  no longer declares; and an apply leaving a domain the file does not name untouched at all. The
  first is the property the three-way merge exists for and the one a whole-set replace would pass
  every other test without having;
- label writes (§9.1): `?dry_run=true` writing nothing while returning the paths a label removal
  would stop; the same removal for real reporting, per stopped path, whether another request still
  references it; a `label` patch and a concurrent apply both landing, which a read-modify-write
  would have lost one of;
- exclusion reporting (§9.1, §10.7): a request whose selector matched a flow this node is writing
  naming it in its status with `self_output`, where a flow that simply did not match the labels is
  not listed at all, and a truncated list reporting its own count;
- self-amplification (§10.7, §11.1): a broad label selector on a node that is a replication
  destination expanding to a fixed path set and not growing on subsequent reconciles; and the
  provenance gap — an agent restart making a replicated flow report `replicated=false` briefly
  must start nothing, because the same restart makes it non-producing and admission holds it;
- conflict precedence (§7.5): a path that has been `ACTIVE` for a while beating a newly matched
  path from an *older* request — the case oldest-first got wrong — and the loser establishing on
  its own once the winner is deleted, with no stored suppression state;
- namespaces (§9.3): overlap permitted in a `shared` namespace and refused in an `exclusive` one,
  the refusal naming the incumbent and stopping no media; two requests of the same name in
  different namespaces coexisting; a namespace auto-created by first reference and its deletion
  refused while referenced;
- the `WAITING` → `ESTABLISHING` → `PAUSED` → `ACTIVE` progression, driving the head index with
  `testutil`'s `UpdateRuntime` to separate `PAUSED` from `ACTIVE`;
- the matched settings of §5.5 reaching both workers identically;
- inventory events (§12.1): a flow appearing and disappearing on a node reaching that node's ring
  and not any path's; fifty flows arriving at once as **one** entry that counts the remainder; and
  the case the feature would otherwise get wrong — **a node losing its lease reporting no inventory
  change at all**, and its return reporting none either, because leased state going away is not the
  same as the flows going away (§4.2);
- the event log (§12.1), which is four properties and only the first is about content: a path's
  transitions recorded in order and coalesced when they repeat, so a flapping worker is one entry
  with a count rather than fifty; a **leader change emitting no state transitions and one takeover
  marker**, which is the case a naive differ passes by fabricating a storm; a path deleted taking
  its ring with it; and — the one that guards everything else — **a reconcile that changes nothing
  writing no events**, since the bullet above is worthless if the log is the writer that breaks it;
- log tails (§12.2): a fake worker that dies printing a `fatal:` line making that line readable
  from `GET /v1/paths/{id}/logs`; a crash loop producing **one** tail rather than one per restart;
  a tail over the server's cap truncated at the head so the fatal line survives;
- **a second reconcile of an unchanged fleet writes nothing and does not move the store
  revision** — the property everything in §8.3 rests on.

For real end-to-end coverage, `shm` or `tcp` on loopback exercises the actual worker on a single
host with no special hardware, with `mxl-mock-src` and `mxl-mock-sink` producing and consuming
real flows so the replicated payload is verified **byte-exact** rather than merely observed to be
advancing.

---

## 18. Decision record

The record of every settled question. Everything else in this document is description.

**Identity**

- The project is **`mxl-replicator`**; the worker binary keeps its existing name (§2.2). Metric
  prefixes are `mxl_` for flows and `mxl_repl_` for the control plane.
- **Roles are negatable toggles on `run`, not subcommands** (§2.2), superseding this document's
  earlier position. Naming a role selects it alone; naming neither or both runs both. A combined
  instance still speaks HTTP between the two roles.
- **There is no separate CLI binary** (§9.1, §19). `apply`, `delete`, `status`, `get` and
  `describe` are flat subcommands beside `run`. The three read verbs have three jobs and no
  overlap, and `describe`/`get` keep `path` and `session` separate because §4 does.

**Protocol and state**

- **The epoch is a content hash plus an incarnation nonce**, not a counter (§5.2), computed over
  `fabricAddress`, `regions[].{addr,len,rkey}` and `bounceBufferInfo`, and spelled
  `<nonce>:<sha256 hex>` so the initiator can verify a blob against it. This couples the project to
  `TargetInfo`'s internal structure, which is acceptable given shared maintainership of
  mxl-fabrics, and is guarded on both sides.
- **Agents are fail-static** (§4.2). A server outage never stops running media; the agent acts only
  on an assignment set it actually retrieved. The server holds the same discipline: not-ready is
  never an empty set, the reconciler refuses to act with leased agents and no observed state, and
  paths touching a node that is not live are frozen rather than converged.
- **The server observes a settling window before its first reconcile** (§7.3), sized as a multiple
  of the heartbeat interval, agents report their full running state, and readiness is published as
  a record every replica reads — so a server restart or leader change does not glitch media, and a
  wiped store says so instead of looking like a fleet-wide cancellation.
- **A glitch on agent restart is acceptable** (§6.1). Kill and re-establish; no adoption; the agent
  holds no persistent state. The restart is made fast instead.
- **Session identity is `(path, flow-def hash)`** and the path ID excludes the definition (§5.4), so
  a republished flow rebuilds the session without resetting the path. It no longer carries the
  resolved output root as a separate term: the area is the first segment of a domain's name.

**API**

- **The source of a request is an extensible selector** (§9.1), with flow-ID, group-hint and `all`
  kinds. A request owns a *set* of paths, including in the single-flow case.
- **`all` selects a source domain entire, and a manifest spells it by omission** (§9.1). On the
  wire it is a kind like any other and an absent `select` stays an error, because a zero value that
  means "everything" is what the tagged union exists to prevent. It does not contradict §10.7's
  refusal of an empty *domain* selector: that one widens the set of places unboundedly and over
  time, this one selects within one place the operator already named.
- **A request fans in as well as out: both ends are lists, and a source's node stays pinned**
  (§9.1), superseding "a request fans out, not in". Of that position's four arguments, one was
  about which side carries the list and never reached across nodes, one inverted — fan-in groups
  the *ingress* a destination shares — and two survive as requirements discharged elsewhere:
  shared fate by `PARTIAL` (§11), the corruption case by `duplicate_source_flow` and per-path
  `flow_conflict` (§7.2). Nothing below the request changes: paths, sessions and refcounting are
  untouched. Validation and negotiation are per `(source, destination)` pairing. A destination may
  override the provider pin; the source may not, because the pin is unsatisfiable per pairing and
  one side already says everything about one.
- **`sources` is always a list, with no singular spelling beside it** (§9.1). A scalar-or-list
  form was available and is refused: unlike `provider`, the singular would be the common case, so
  both spellings would be in daily use. Every stored request and manifest written the old way is
  invalid, riding along with §10.6's re-identification in the same major version (§16).
- **A destination entry carries `disabled`, and that is the only place a request spells *off***
  (§9.1). The effective spec is every source against every *enabled* destination. It is put there
  rather than on the request because dropping a destination is already the operation that stops one
  leg, so this is that operation made non-destructive; a request-level flag is derivable from these
  and not the reverse. A flag on the *pairing* is refused outright — it would make a request an
  arbitrary bitmap over the grid rather than sources × destinations, which the expansion cannot
  describe and the manifest cannot spell. A flag on a source is not built, would add only the muting
  of one row of a fan-in, and is additive. `Validate` counts entries rather than enabled ones,
  duplicate endpoints are still refused where one is off, `DELETE` is not superseded, and disabling
  stops media with a cancellation's blast radius. A disabled request is not validated against the
  fleet; structural validation still runs.
- **`DISABLED` is the second aggregate-only state, and it is derived** (§11). A request with no
  enabled destination reports it. `WAITING` and `INVALID` were the alternatives and both lie — one
  promises the condition resolves, the other that something is wrong. It is computed from the
  destination entries rather than stored, so no flag can drift from the legs it describes; a path is
  never `DISABLED`; a partly parked request folds over the paths it still has; and it ranks below
  `ACTIVE`, counted by `status` on a line of its own rather than among the faults.
- **An apply that omits `disabled` enables the leg** (§9.1). The file is authoritative over the
  requests it names, so a leg parked through the API returns on the next apply of the file that names
  its request. §10.7's declared-key merge is refused here for the reason it was adopted there: a
  domain's label map has many writers by design, a request's spec has one by construction.
- **The request's ID is `(namespace, name)`** (§9.1). `POST` is create-or-update, an identical spec
  writes nothing, and the outcome is reported in a header. Names are scoped to the namespace, not
  fleet-wide — a namespace that does not namespace names is half the concept, and the Kubernetes
  adapter that motivates the partition inherits namespacing of its own.
- **A namespace is a first-class object, not a reserved label** (§9.3), superseding this document's
  earlier position. It auto-creates on first reference, never auto-deletes, refuses deletion while
  referenced, and partitions requests only — not nodes, domains or destinations.
- **Path exclusivity within a namespace is opt-in, defaulting to `shared`** (§9.3). It is the one
  conflict rule in the system that protects legibility rather than integrity: intra-namespace
  overlap is free, since two requests on one path share one session and one worker pair. Hence the
  governing line — integrity rules are mandatory, legibility rules belong to whoever is reading.
- **Conflicts are ordered `(incumbency, UpdatedAt, id)`** (§7.5), superseding "oldest-first". Age
  was a proxy for incumbency and inverts the moment observed state rather than a user creates the
  conflict. Incumbency keys on the derived session record, not on running workers, so it survives a
  worker restart and composes with §4.2's freezing.
- **Validation is per path, not per request** (§7.2), over a `(source, destination)` pairing rather
  than a destination. `POST` refuses only structural invalidity; a conflicting pairing invalidates
  its own path and leaves the request's others alone.
- **`flow_conflict` is the one `INVALID` code that tears the loser down** (§7.2), because its harm
  is the running state rather than the intent. Its decidable form inside one request —
  two sources pinning one flow UUID against a shared destination — is a **separate** code,
  `duplicate_source_flow`, refused at `POST`, because a code has one disposition.
- **`same_endpoint` is checked over every pairing and refuses on any one of them** (§7.2). That
  subsumes §10.8's `overlapping_selectors` while both ends are enumerated: an intra-request cycle
  always puts an endpoint on both sides, which puts a self-pair in the cross product.
- **Requests are authored as a multi-document YAML manifest and applied** (§9.1). This needs
  nothing from the API beyond `?dry_run=true`: create-or-update on the idempotency key *is* apply.
  `--prune` requires a scope, for which a namespace is a better one than a label selector, and it
  covers requests only — never a namespace, never a domain label.
- **`kind:` names the manifest object and defaults to `request`** (§9.1), superseding "no
  `apiVersion`, no `kind`", which anticipated exactly this. Two object types arrived together —
  `namespace` and `domain` — which is the reason to add it once and deliberately. Apply orders
  documents by kind regardless of file order.
- **`label` is a fourth verb and not a fourth vocabulary** (§9.1): it writes to the same endpoint the
  `kind: domain` document applies, sending a patch where the document sends its declared map. The
  manifest is the desired set; `label` is a one-shot edit, and noticing a domain and naming it is
  genuinely interactive.
- **`INVALID` governs admission only** (§7.2): it stops new sessions and never tears down a running
  one, and a fabric that stopped being viable is `FAILED`/`fabric_gone` rather than `INVALID`.
- **Status vocabulary** is `WAITING` / `INVALID` / `ESTABLISHING` / `PAUSED` / `ACTIVE` /
  `PARTIAL` / `DISABLED` / `DEGRADED` / `FAILED` (§11), of which `PARTIAL` and `DISABLED` are
  aggregate-only, with `ACTIVE` determined from the destination flow's head
  index rather than from worker accounting, and a non-producing source classified `PAUSED` rather
  than `WAITING` (§7.2).
- **`PARTIAL` is the aggregate-only state**, and it supersedes "`ACTIVE` only when all of its paths
  are" (§11). A request whose paths disagree with at least one `ACTIVE` is `PARTIAL`; it outranks
  `INVALID`, `FAILED` and `DEGRADED`, because §7.2 already settled that one bad path does not
  condemn a request and a worst-wins fold would undo that at the level read first. `DEGRADED` was
  the alternative and is refused: it means flapping, a real path state, and a second set-shaped
  meaning would make its fleet gauge uninterpretable. It applies to every request shape, not only
  fan-in.
- **Auth is a single optional shared bearer token**, TLS optional, no mTLS (§13).
- **The server is always upgraded first, and the gate is the protocol version** (§13.1).

**Capabilities and providers**

- **Nodes declare fabric attachments, not providers** (§10.1). A `(provider, fabric, address)`
  triple where `fabric` is an operator-assigned opaque label; two nodes may pair on a provider only
  if they share its label. Provider availability is not reachability. A node with none configured
  gets `shm`.
- **Configuration joins to the probe through two classes of selector** (§10.1): a *naming* one —
  `address`, `interface`, `device`, or none, with "none" the common case — superseding this
  document's earlier "prefer naming an interface over an address", which cannot work for
  `verbs`/`efa`; and *narrowing* ones — `network`, `ip_version` — which conjoin with a name and
  with each other. Exactly one probe entry must survive the conjunction.
- **The agent advertises only what it has verified** (§10.2). Raw kernel capabilities never go over
  the wire; they gate whether an attachment is advertised at all.
- **There is one kind of domain, and a node declares *areas*** (§10.6), superseding this document's
  "input and output domains are separate concepts". A domain is a directory inside an area,
  identified fleet-wide as `<area>/<elements>`, and it is *observed* — what is derived is whether a
  session targets it. The old split diagnosed the right problem (domains straddled two of §4's
  layers) and treated it backwards, by making the directory carry a distinction that belongs to the
  session. A domain still needs no lifecycle of its own: refcounted like a path, no create or delete
  API, nothing durable to reconcile against. The destination resolver still never consults observed
  state, and cleanup of leaked directories is still deferred pending an ownership model.
- **An area carries two independent grants, `read` and `write`; neither is settable by the API**
  (§10.6), superseding `--search-path` and `--output-root` as separate concepts. Between them they
  are the whole of this project's authority over a node's filesystem. Areas may nest and the
  innermost containing area names a directory; only equal paths are refused. That naming rule is
  what makes one identity grammar possible, and everything below rests on it.
- **A domain is `(area, []string)`** (§10.6), spelled `area/a/path` only in a manifest and parsed
  there once. That is what keeps containment an equality on the whole path rather than a prefix
  argument. One materialised domain may not contain another. Names are no longer flat per node and
  `domain_name_in_use` is gone: the area is in the name, so the collision is unconstructible.
- **Discovery is not pruned** (§6, §10.6), superseding "a root is written, not read". A domain this
  project writes into is discovered like any other, and inventory membership is the *union* of what
  discovery reports and what the reconciler materialised. Of pruning's four jobs, two are dissolved
  by the innermost-area naming rule (one directory one name; the ordering hazard of a leftover
  directory acquiring a path-shaped name), one moves to the union, and one survives relocated.
- **A label selector never matches a flow this project is itself writing** (§10.7), superseding "a
  source selector never matches a domain under an output root". Same rule, same cut — explicit
  chaining is intent, matched chaining is emergence — moved from the directory to the flow, where
  MXL already puts ownership. The agent reports it as `replicated` in inventory. It is more precise
  than what it replaces: a domain holding one replicated flow beside nine local ones is no longer
  invisible as a source.
- **§11.1's admission rule is load-bearing for safety, not only for churn** (§10.6, §11.1).
  Provenance is briefly absent after an agent restart, and what closes the window is that a flow
  whose target worker is not running is also not advancing — the target worker is what advances it —
  so admission holds it in `PAUSED` and starts nothing.
- **Domains are discovered, never configured** (§6, §10.7), superseding both `-m` and this
  document's "registration advertises configured mappings only". Operators attach **labels** through
  the API, and a request's source names a domain directly or selects by label. Labels annotate and
  never rename — a rename would re-identify every path through the domain on a metadata edit. Labels
  are joined server-side and never reach the agent, and a label on a domain the node does not report
  is accepted and inert; the refusal under an output root is gone with the roots.
- **The direct source form is `{"name": …}` and addresses any domain** (§10.7), superseding
  `{"path": …}`, which addressed only a discovered domain and left the second hop of a chain
  unspellable. In a manifest a scalar `domain:` is a name and a map is a label set, which also
  disposes of `domain: {}`.
- **The label selector is equality-only with every key ANDed, and an empty one is refused** (§10.7).
  `in`, `notin` and `exists` arrive later as a third union kind rather than by widening what a map
  value may say, because widening it would silently change the meaning of an existing request in the
  direction of matching more.
- **An apply owns the keys it declares** (§9.1), superseding "a `kind: domain` document replaces the
  whole label set". It sets what it declares, removes what it declared last time and no longer does,
  and leaves every other key alone — `kubectl apply`'s three-way merge, adopted deliberately because
  a domain's label map is the one record here with no owner (§10.6) and whole-set replace makes the
  last writer win. An imperative `label` edit therefore persists. Two files naming one domain still
  fight; named field managers are the additive fix and are not built.
- **The two gestures send different bodies** (§9.1): an apply sends its full declared map, `label`
  sends a patch. This supersedes "`label` is a client-side read-modify-write", which had a
  lost-update race on a record several operators are expected to write.
- **Label writes take `--dry-run`, and the real write prints its blast radius** (§9.1). A label edit
  starts and stops media exactly as a request does, one indirection away, which makes it easier to
  do by accident rather than harder. It prints rather than prompts: the CLI is scripted by the same
  people who use it interactively.
- **An excluded flow is reported** (§9.1, §10.7). A request's status names the flows its expansion
  dropped to the self-output rule, because a path that does not exist has no status to carry a
  reason and the finer granularity is otherwise the less legible one. `replicated` is on
  `GET /v1/flows` and in `describe domain` for the same reason.
- **A self-pair a label selector produced is elided rather than refused** (§7.2, §10.7). With a
  named source it is a typo and `same_endpoint` refuses it; with a selector it is the selector doing
  what it was asked, and refusing the request would put its author at the mercy of which domains
  happen to carry a label. §10.8's argument, arriving one step early.
- **Multipoint is not built, and what remains of it is the *selectors*** (§10.8). The cross product
  itself landed with fan-in, along with two of the three mechanisms that section asked for; what is
  left is a source node or a destination that is selected rather than named, which needs node
  labels, bandwidth admission control as a genuine precondition, and `overlapping_selectors` as a
  real check rather than one subsumed by `same_endpoint`.
- **The server owns full interface negotiation** (§10.3), not just provider choice — caps flags
  intersected, `maxMessageSize` minimised, the result assigned to *both* ends.
- **No silent provider downgrade** (§10.4). An explicit provider is honoured or the request fails;
  `provider` accepts a list to express an acceptable fallback. Default order is
  EFA > Verbs > TCP > SHM. The negotiated provider is pinned for the session's lifetime.
- **The agent allocates services from a configured range** (§7.4), probe-binding for `tcp` only,
  and `shm` gets a name from the same allocator.
- **TAI clock correctness is assumed**, not verified. It is already a deployment requirement for
  the target environments, so it is not modelled as a capability or condition.

**Observability**

- **Prefixes split by what a metric describes, not by which process emits it** (§2.2, §12): `mxl_`
  for a flow or a transfer, `mxl_repl_` for the control plane. Workers are scraped on demand inside
  the request through a bounded pool, superseding a cached background scrape, which beats against
  the collector's own interval and lies about liveness in both directions.
- **There is a bounded event ring per object, and the path is the unit of retention** (§12.1). A
  session is a field on an event rather than a log of its own: a session-scoped log would split the
  story at the re-establishment being investigated, and a path ID is stable where a session ID is
  not. Requests and nodes carry their own logs for what is genuinely theirs; flows and domains carry
  none.
- **An object's log is one key holding a snapshot, not one key per event** (§12.1) — §9.2's
  level-triggered discipline a third time, which removes sequencing, gap detection, compaction and a
  garbage collector at once. Bounded by **count and not by age**, because an age bound expires the
  overnight failure exactly when someone arrives to read it, and coalescing repeated entries is what
  keeps the count honest.
- **Events are a side effect of `Apply` and never an input to `Compute`** (§12.1), they live under a
  prefix the fleet snapshot excludes, and one write covers a whole reconcile pass. The poll is
  unaffected — it gates on its own key's `ModRevision` — but revisions are still consumed, and
  sqlite bounds watch history by revision count (§8.1).
- **A newly elected leader emits no state transitions on its first pass and marks the gap**
  (§12.1). Emitting the current state as though it had just happened would fabricate a storm on
  every leader change; a marked gap is honest and is §7.3's settling argument one layer up. Agent
  events ride a **separate** endpoint rather than the status snapshot, which is compared before
  sending, and a dropped batch announces itself. The log is therefore a diagnostic aid and
  explicitly **not** an audit log.
- **Flows and domains entering and leaving a node's inventory are recorded on that node's ring**
  (§12.1), superseding this document's own "that belongs nowhere here". Batched per pass rather than
  per flow, switchable and on by default — the one part of the log whose volume the fleet sets
  rather than the control plane — and **skipped entirely for a node that is not leased**, because
  its inventory is leased state and diffing against its absence would report every flow as gone at
  the moment nothing happened to any of them (§4.2).
- **The agent tails each worker start's output and pushes it with the transition into `FAILED`**
  (§12.2), superseding §19's broader "worker log retrieval". Byte-bounded rather than line-bounded,
  the tail rather than the head, one tail per crash loop rather than one per restart, and stored
  behind its own endpoint so the ring a UI polls stays cheap. The capture bound is agent-local and
  the accepted bound is server-side, because an endpoint taking unbounded bytes from a node is a
  store-filling primitive.

**Storage**

- **The three state layers are nested under one root** (§4), superseding three unrelated top-level
  prefixes. §7.3 needs the snapshot to be one `List`, and the only prefix covering three unrelated
  ones is the empty prefix — which, once the event log exists, both drags diagnostics into every
  read and wakes the reconciler with its own writes. A byte range over the layer names was the
  cheaper fix and is refused: it fails by making a layer *silently absent*, which is §4.2's wiped
  store arriving through the back door.
- **The store interface is in etcd's terms and emulated over sqlite** (§8.1), with an append-only
  history table behind the watch, a `(revision, seq)` cursor, bounded history and `ErrCompacted`.
  The conformance suite runs unchanged against both.
- **Long-poll with a revision cursor, not server-push** (§9.2), so every request is self-contained
  and no sticky sessions are needed. `inventory` and `status` are full snapshots, and an agent does
  not send an unchanged one.

**The worker**

- **The worker source may be modified** (§15). Three required changes — configurable no-grain
  timeout, an interface probe mode, and accepting the negotiated interface config — plus hygiene.
  The exit-code fix is explicitly *not* load-bearing: failures are classified from restart rate,
  time-to-death and source liveness (§15.1).
- **Interfaces are discovered by the worker probe** (§10.5) using `mxlFabricsGetInterfaces()`, not
  guessed from `/dev/infiniband`.
- **Idle sources are handled by three mechanisms at different timescales** (§11.1): a long or
  infinite worker idle timeout for short gaps, head-index observation for admission and long-idle
  teardown, and agent-side restart backoff as the catch-all. Both session-level knobs live on the
  server.
- **Worker starts are rate limited on the agent, through a token bucket** (§6.3), because what a
  node can bring up at once is a different capacity from what it can hold and only the second was
  ever sized (§14). Every start passes through it, including restarts; stops never do, and a queued
  start is cancellable on the spot so a withdrawal is not held behind a permit. A limit on starts
  *in flight* was the first answer and is refused: an initiator has no signal saying its start
  finished, so a concurrency gate would protect nothing on half the workers. The wait sits in the
  supervision goroutine, which is §6's "reconcile never blocks on a start" doing a second job. The
  knobs are agent-local, `0` on the rate means no limit, and the defaults are conservative enough
  to amend §6.1: 1–2 s is the budget for a flow, not for a node.
- **One process per flow per direction is fine** (§14). The worker sits behind a
  replaceable-module interface so a future multi-flow worker is a substitution rather than a
  rewrite — justified by testing (§17) regardless.

---

## 19. Roadmap

`docs/open-items.md` is the companion list and is **not** part of this roadmap: it holds the loose
ends the domain-labelling, namespace and conflict-precedence work left in *this* document — two
contradictions inside sections above, mechanisms those sections imply without describing, and
decisions deferred rather than made. Read it before treating §7.2, §7.5, §9.3, §10.7 or §10.8 as
finished. The roadmap below is work this design does not do yet; that file is work this design
says it does and has not fully specified.

- Kubernetes adapter turning pod labels or annotations into replication requests. Depends on the
  idempotency key in §9.1 and on the no-write-if-unchanged rule that comes with it — a controller
  re-reconciling on every resync is the case both exist for. It should hold its own namespace and
  leave it `shared` (§9.3): one request per pod is the natural mapping, several pods asking for one
  flow is ordinary, and refcounting is this design's answer to that.
- Web UI for managing and inspecting agents and replication.
- Bandwidth admission control (§13). **A precondition for multipoint** (§10.8) rather than an
  optional improvement, since a selector on both ends makes a request's cost unreadable at the
  moment it is written.
- Multipoint requests (§10.8), which is where that list of conditions lives.
- An ownership model for domains, which is what cleanup of leaked directories needs (§10.6). It got
  more visible rather than more urgent: un-pruning discovery means a leaked directory holding stale
  content is now *reported* instead of hidden, so an operator can at least see the thing this item
  exists to clean up.
- ~~Worker log retrieval through the API.~~ **Superseded by §12.2**, which takes the narrow half:
  a byte-bounded tail per worker start, pushed with the transition into `FAILED` and fetched from
  its own endpoint. The prediction here held — it landed as a change to the exec launcher *and* the
  supervision unit, not the former alone. What remains unbuilt is the general form this item
  originally imagined: a *live* log stream from a running worker, which needs a streaming agent API
  and has no obvious client until the UI wants one.
- Splitting the etcd backend behind a build tag. Linking it takes a trivial binary from 9.2 MB to
  24.1 MB — **+15 MB**, almost all of it grpc, protobuf and zap — and the agent never needs an etcd
  client. A fleet-wide DaemonSet is where that either matters or does not.
- Multi-flow worker, if §14 ever says hundreds.
- Worker adoption across agent restarts (§6.1).
- Softening the newer-agent refusal to a warning (§13.1), which §2.3 argues for and which is a
  behaviour change worth deciding on its own.
- A legacy `config.yaml` importer, emitting manifest documents rather than API calls (§16).

**Dropped: a separate `xpt` CLI.** An earlier version of this section proposed one — the broadcast
abbreviation for *crosspoint*, with `xpt take|list|status|clear` as domain-native verbs and
`mxl-replicator ctl` as the discoverable long form. It is superseded by the manifest and the
subcommands in §9.1, which cover the same surface declaratively. Two vocabularies for one thing is
worse than one, the file is the better interface for a desired set an operator keeps, and a second
binary is a build target and a packaging story for no gain. A one-shot `take` verb, if ever
wanted, is another subcommand beside the rest.

---

*This document supersedes `rewrite.md`, the rewrite proposal it grew out of, and keeps its section
numbering: roughly 1200 comments in `internal/`, `cmd/` and `src/` cite these numbers, as does
`rewrite-plan.md`, whose own §4 (deployment topologies) is folded into §2.3, §8.2 and §13.1 here.
The one number that changed meaning is §16, which was the migration section and is now the record
of what the retired proxy left behind.*
