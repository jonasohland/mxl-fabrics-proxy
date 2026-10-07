# `mxl-replicator` — architecture

This document describes how the system is built and why. It is prescriptive. Where a design
choice was open, the section marks it **Settled** and gives the reasoning that decided it, so a
reader can tell a decision from an assumption. §18 lists every such decision in one place.

**Do not renumber sections.** About 1200 comments in `internal/`, `cmd/` and `src/` cite section
numbers (`§10.6`, `§5.2`, …), so a section keeps its number even after its content changes. When
a decision replaces an earlier one, both are recorded; the earlier one is not silently
overwritten. The superseded position often still looks reasonable on first reading, so the
document states why it was dropped.

The companion document is `rewrite-plan.md`, the milestone-by-milestone implementation record. It
has a **Plan decision** marker wherever the implementation went beyond this document or
contradicted it. Everything it decided has since been folded back into the sections below.

Read these first:

1. `docs/worker-runtime-surface.md` (**WRS**) — the contract with the C++ worker binary. §5 and
   §6 depend on it.
2. `docs/third_party/mxl/Addressability.md` — the `mxl://` URI scheme. It states that translating
   domain paths is *out of scope for MXL*. That translation is this project's job.
3. `docs/third_party/mxl/Fabrics.md` §2, §4.3, §5 — the target/initiator model and what
   `TargetInfo` contains.
4. `docs/third_party/mxl/FabricsDeveloperGuide.md` — the "Interfaces" and "Compatibility"
   sections. They state that the library does **no** capability negotiation and expects the
   caller to do it over an out-of-band channel. This project is that channel, and §10 is built on
   that statement.
5. `docs/third_party/mxl-utils/mxl-utils.md` — the library this project uses for discovery and
   for observing flows.

---

## 1. Context

An MXL flow is a ring buffer in memory-mapped files under a *domain* directory
(`<domain>/<flow-id>.mxl-flow/`). Media functions on the same host share flows without copying.
**MXL Fabrics** extends this across hosts over libfabric (`tcp`, `verbs`, `efa`, `shm`), mostly by
RDMA Remote Write directly into the receiver's media buffer.

`mxl-replicator-worker` is the C++ binary that runs MXL Fabrics for flows a host does not own.
**One worker process handles one flow, in one direction, with one peer, in one role.** The worker
has no discovery, no control plane and no reconnect logic. It reads a JSON config file once at
startup, and the only way it recovers from a failure is to be restarted. It also does no
capability negotiation: both ends of a transfer must be given the *same* negotiated interface
configuration through some out-of-band channel.

`mxl-replicator` is that out-of-band channel, and it supervises the workers. It decides which
flows go where, negotiates the fabric each transfer uses, and runs the worker processes that move
the media. It never touches grain data.

It replaces an earlier peer-to-peer proxy. In that design each node had a static subscription
list and fetched flow definitions and `target_info` from its peers over HTTP. Configuration grew
as O(n²) across a fleet, could not change at runtime, and required every node to reach every
other node's API. The proxy is retired. §16 records what was carried over from it and what was
deliberately left behind.

---

## 2. What the system does

- A **central connection management server** handles connection setup. Agents register with it,
  receive assignments from it, and act on them. Agents never talk to each other on the control
  plane.
- Replication is requested **through an API**, not through a per-node config file. A request
  names a source (node + domain + flow *selector*) and one or more destinations (node + domain).
  The client supplies its name, and that name is also its ID and its idempotency key (§9.1).
  Operators write requests into a manifest file and apply it.
- Agents **report the flows they observe** in their domains. The server combines these reports
  into a fleet-wide inventory.
- Server and agent ship as **one binary**. Both roles can run in one process, for single-host
  setups and development (§2.2, §2.3).
- There are two storage backends: **etcd** (for HA) and **local sqlite** (single node).
- With etcd, the server can run **HA behind an ordinary third-party HTTP proxy**. The proxy needs
  no sticky sessions and no special L7 features.
- The existing worker is **reused, not rewritten** (§15).
- **Filesystem access is configured on the agent**, through flags and a YAML file. A node declares
  named **areas**, and each area grants reading, writing or both (§10.6). Domain names are not
  agent configuration: operators label domains through the API (§10.7).

### 2.1 Non-goals

- Changing the worker model of one process per flow per direction. §14 describes what this costs.
- Any change to the media plane. This project never touches grain data.
- Wire or config compatibility with the retired proxy, except for the provisioning half (§16).

### 2.2 Name and command-line surface

The project is called **`mxl-replicator`**. The name describes what the tool does rather than
being a product name, because an official MXL discovery and connection API is in progress and
this project is expected to align with it later. The name also groups it with `mxl-utils`, which
it depends on, and it matches the terms this document uses: request, path, replication.

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

**Settled: one `run` command with role flags, not one subcommand per role.** This replaces an
earlier position in this document, "roles are subcommands, not flags". That position rejected
role flags because `--help` would then list the options of two unrelated tools together. It
assumed that a fleet consists of single-role nodes. §2.3 shows that it does not: a node running
both roles is a normal production deployment. Since `run` has to exist for that case anyway,
per-role subcommands would be a second way to write the same deployment (`server` and
`run --server` differing only in flag names), and having two ways to write one thing is worse.
The combined help text is a real cost but a small one. `run --help` lists 37 flags, against 20
for the server role and 18 for the agent role. The role option structs are embedded with
`server-` and `agent-` prefixes, so the list is grouped by subsystem, as `--store-*` already is.

Naming a role **runs only that role**. Naming neither, or both, runs both. One consequence can
surprise: `--server` means "server only", not "also enable the server", so naming one role turns
the other off. Both flags' help text says this. There is no way to run neither role. `run` is
kong's default command, so `mxl-replicator --agent` and `mxl-replicator run --agent` are the same
invocation.

When both roles run in one process, they still **talk to each other over HTTP** instead of
calling each other in memory, so there is only one code path. The co-located agent connects to
its own server over loopback, at an address derived from `--server-listen` (a wildcard bind
address becomes loopback). This also guarantees that the agent talks to a server of its own
version (§13.1, §2.3). If the server terminates TLS, the derived loopback URL cannot work,
because a certificate for the node's routable name does not cover `127.0.0.1`. In that case
`--agent-server` is required, and this is checked when flags are parsed.

All validation happens when flags are parsed, not when a value is first used. It rejects:

- a lease TTL shorter than the heartbeat interval;
- `etcd` without endpoints;
- half-configured TLS;
- an unknown provider in the preference order;
- an area path that is not absolute, an area granting neither read nor write, or two areas on one
  path;
- a port range whose start is after its end;
- two role listeners on the same address.

Only the enabled roles are validated, so an agent-only node never needs a valid store
configuration.

Defaults:

| Setting | Default |
|---|---|
| Server listen address | `:2283` (the port the retired proxy used) |
| Agent metrics listen address | `:2284` |
| Fabric port range | `24000-24999` |
| Provider order | `efa,verbs,tcp,shm` (§10.4) |
| Settling window | `3` heartbeats (§7.3) |
| Worker idle timeout | `0`, meaning wait indefinitely (§11.1) |
| Worker start rate | `0.5`/s with a burst of `2`; `0` means no limit (§6.3) |

**The worker binary is named after this project.** `mxl-replicator-worker` was renamed from
`mxl-fabrics-proxy-worker`, a name left over from the retired proxy. No other binary in this
repository ends in `-worker`, so the name identifies both the layer and the binary that WRS
describes.

**Metric prefixes follow the same split** (§12). Metrics that describe a flow keep the `mxl_`
prefix, because they come from the worker socket and describe MXL, not this project.
Control-plane metrics use `mxl_repl_`. This follows the Prometheus `<namespace>_<subsystem>_`
convention (namespace `mxl`, subsystem `repl`) and keeps the whole project under one namespace,
so `{__name__=~"mxl_.*"}` selects all of it.

### 2.3 Deployment topologies

`mxl-replicator run` starts both roles by default. The server role in a combined instance is a
**full server**: it binds its configured address and serves every other node's agent, the same
as a standalone server. Three deployment shapes are supported.

| Topology | Store | Reconciler | Notes |
|---|---|---|---|
| Dedicated server (`--server`) + N agents (`--agent`) | sqlite or etcd | the one server | Cleanest isolation. |
| One both-roles node + N−1 `--agent` nodes | sqlite or etcd | the both-roles node | Non-HA. The shape most small fleets want. |
| M both-roles nodes + N−M `--agent` nodes | **etcd** | elected leader among the M | HA. |

Running both roles on one node is supported, but it gives up an isolation property the design
otherwise has. Each consequence is covered in its own section:

- **A control-plane failure interrupts media on that node** (§6.1). Fail-static (§4.2) ensures a
  server outage never stops media. On a combined node, though, a panic, an OOM kill, a failed
  liveness check or an image update takes the agent down together with the server, and every
  flow on that node has to be re-established. For this reason: every background goroutine
  recovers from panics, the memory limit is sized for the fleet-wide inventory rather than for a
  local supervisor, and the control plane should preferably run on nodes that carry few or no
  flows.
- **Rolling upgrades must be gated on the protocol version**, not the build version, because
  upgrading a combined instance upgrades both roles at once (§13.1).
- **HA introduces failure modes a single server cannot have**: cursor regression across
  replicas, a demoted leader that is still writing, and frequent leader changes when workers run
  with real-time scheduling (§8.2).
- `MXL_REPLICATOR_AUTH_TOKEN` is used by both halves of a combined instance. This is correct,
  since both are in the same trust domain, but it is one value, not two.
- Shutdown order: stop serving before the local agent releases its lease. An expired lease does
  not prove that a node's workers have stopped (§4.2).

---

## 3. Identity and terminology

MXL names things from the data-plane point of view, and this project names things from the
control-plane point of view. The two often run in *opposite directions* (see "Direction of
everything" below), so these terms need fixed meanings.

| Term | Meaning |
|---|---|
| **Node** | One logical host in the network. Each node runs exactly one agent. Identified by a unique **node name** that the operator assigns (CLI flag, env var, or agent config file). If none is given, `run` uses the hostname and logs a warning (`cmd/mxl-replicator/run.go`). |
| **Domain** | An MXL domain on a node: a directory inside an area that holds flows. There is only one kind of domain, whether this project reads from it or writes to it; a domain is a location, not a channel (§10.6). Addressed across the fleet as `<area>/<elements>`, and that address is its identity for as long as it exists. |
| **Area** | A directory on a node that the operator has designated as a place where MXL domains live. It has a name and two independent grants: `read` (domains here may be discovered and observed) and `write` (replication may create domains and flows here). Declared in the agent config and advertised when the agent registers. These two grants are the only authority this project has over the node's filesystem (§10.6, §13). |
| **Domain label** | A key/value pair that an operator attaches to a `(node, domain)` through the API. Labels annotate a domain but never identify it. A request's source selector matches on labels, and changing a label never changes which domain is which (§10.7). |
| **Namespace** | A partition of requests, stored as its own object (§9.3). It scopes request names and `--prune`, and it records whether requests inside it may share a path. It does **not** partition nodes, domains or destinations. |
| **Flow** | Identified by a UUID. The UUID identifies the media, *not* its location: after replication the same flow ID exists on both nodes, by design. |
| **Flow address** | `(node, domain, flow-id)`. The domain is required because the same flow ID can validly exist in two domains on one node. |
| **Request** | Durable user intent: "replicate whatever these selectors match, from these places to those places". Its name is supplied by the client and is **scoped to its namespace**: `(namespace, name)` is its ID and its idempotency key (§9.1, §9.3). Paths count the requests that reference them, so a request is reference-counted from the path's side (see **Path**). |
| **Path** | A logical edge `(src flow address) → (dst node, dst domain)`, derived from requests with duplicates removed. N requests that want the same edge produce 1 path. The path is torn down when the last request that references it is cancelled. |
| **Session** | A concrete *pair* of workers (initiator and target) that carries one path. Identified by a stable session ID and a target-side **epoch** (§5.2). Short-lived: re-established whenever either end restarts. |
| **Epoch** | A content hash that identifies one run of a target worker. It is not a counter: epochs can only be compared for equality, not ordered (§5.2). |
| **Initiator** | MXL term. The **sending** worker, on the source node. It reads the local flow and RDMA-writes it to the peer. |
| **Target** | MXL term. The **receiving** worker, on the destination node. It binds `node:service`, creates the local flow, and is passive. |

**Direction of everything.** Three things flow in three different directions:

- The **fabric connection** goes from source to destination: the initiator connects to the
  endpoint the target has bound.
- The **connection information** goes the other way, and first: the destination produces
  `target_info`, and the source consumes it.
- The **request** is usually written by whoever wants the flow, which is usually neither the
  source nor the destination.

Code comments and the API must keep these three apart. Mixing them up is the main source of bugs
in this area.

---

## 4. State model

State is split into three layers. Each is stored and reasoned about separately. This split is the
central structural property of the design.

**Desired state: durable, small, rarely changing, written by users.**
- Node registrations (§7.1 explains how a registration differs from a liveness lease).
- Namespaces, and the replication requests inside them.
- Domain labels: the names an operator has given this node's domains (§10.7). A label is desired
  state written by a *user* about a node. It does not live under the node's registration because
  the agent writes that key.
- Operator policy: provider preferences, port ranges, bandwidth budgets.

**Observed state: reported by agents, changes often, cheap to rebuild.**
- Which agents are alive (held under a lease).
- Per node, the list of domains; per domain, the list of flows, with flow definitions, group hints
  and the coarse `producing` liveness state (§6, §11.1).
- Per session, its status, including epoch, `target_info` and bound port.

**Derived state: computed from the two layers above, recomputed on demand, never
authoritative.**
- Paths (requests deduplicated and reference-counted).
- Session assignments for each agent.
- Request status.

Each layer has its own key prefix. This lets each carry its own compaction and backup policy
(§8.3), and lets a watch cover just one layer:

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

**Why the three layers share the `/state/` root.** This replaces an earlier layout with three
unrelated top-level prefixes. §7.3 requires the fleet snapshot to be read with **one** `List`.
Three separate lists return three different revisions, and a reconcile computed from a snapshot
mixed across revisions can conclude that a session both should and should not exist. With three
unrelated prefixes, the only prefix that covered all three layers was the empty one, which is
every key in the store. Once the event log (§12.1) existed, that caused two problems:

- An unscoped `List` pulls every object's event ring and every stored log tail into the snapshot,
  which then ignores them. This happens on every user-API read.
- An unscoped *watch* sees the events the reconciler has just written, and wakes the reconciler
  for them.

A cheaper fix would have been a key range built on the layer names. It was rejected. The names
share no prefix except the leading slash, and two of them do not share even their first letter,
so any such range would work only by coincidence and could break when a fourth layer is added. If
it broke, the layer outside the range would be silently missing from every snapshot. The server
cannot tell that apart from a wiped store, which is the failure §4.2 exists to prevent. With a
shared root, every layer is inside the snapshot range by construction. A test asserts that every
layer is inside the root and that `/events/` and `/election/` are not.

**`/events/` is outside the state model; it is not a fourth layer.** It holds diagnostics *about*
the three layers. Nothing reconciles against it and no decision reads it. It appears in the
listing above only because operators will see it in the store.

Observed state is written **under a lease**. When an agent goes away, its lease expires and its
observed state is deleted with it. The server therefore never has to tell "this node reported
nothing" apart from "this node is gone". Desired state is never leased: a registration outlives
the agent that created it (§7.1), and a request is durable user intent (§11).

Two rules follow. The server must never assume observed state survives a server restart, and an
agent report must never change desired state. Everything the server writes into an agent's
assignment is derived state. §7.3 covers the one piece of derived state that must stay *stable*,
not merely be recomputable.

### 4.1 Level-triggered reconciliation

Both sides are reconcilers, not RPC state machines:

- **Agent**: "here is my assigned worker set, with epochs; make the processes I am running match
  it." The agent never receives a "start this" or "stop that" command, only the full desired set
  for its node. §4.2 describes the one way this rule can be misread with serious consequences.
- **Server**: "here is the desired path set and the reported inventory; compute the assignment
  set." The assignment set is recomputed from scratch on every relevant change.

Every operation is idempotent, and every message carries the full state for its scope. A crash
on either side costs only time; no state is lost. This is what keeps the epoch handling in §5
manageable, and it is a precondition for HA (§8.2).

### 4.2 Fail-static: nobody reconciles against an answer they did not get

Taken literally, the agent's rule above is dangerous, because **a failed poll and an empty
assignment set look the same**. A naive implementation reconciles to zero workers when it cannot
reach the server, so a control-plane outage would stop all media. For a system carrying live
video that is the wrong behaviour.

**Invariant: the agent acts only on an assignment set it has successfully retrieved.** A failed
poll changes nothing. There is no timeout after which the agent gives up and stops workers. A
server outage of any length leaves running sessions running.

The agent must therefore only ever treat "empty" as something the server told it, never as
something it concluded from a missing answer. In code, the reconcile function takes an assignment
set as input. The poll path either produces an assignment set or produces an error, and an error
skips the reconcile entirely; the code must not allow the two outcomes to reach the same call.
The context tree enforces this too: a unit's context is derived from the agent's context, not
from the poll loop's, so cancelling a poll cannot stop workers. Re-registration cancels the
session's loops but leaves every worker running as it was.

Consequences accepted on purpose:

- **Workers that fail during a network partition stay failed.** The agent still runs its local
  restart loop. But if a session needs a new epoch delivered to its peer, it cannot recover until
  the server is reachable again. This is the right trade-off: a partition affects *recovery*,
  not *steady state*.
- **An agent returning after a long partition may reconcile with visible effects.** Assignments
  may have changed or been withdrawn while it was away, so flows may glitch when it reconnects.
  That is an intended reconcile against current desired state, and it is better than the agent
  silently diverging.
- **An expired lease does not prove that a node's workers have stopped.** The server does not
  move a session to another node on that basis.

#### The server must never send an empty set that means "unknown"

Fail-static protects the agent when there is *no answer*. It does not protect the agent from a
**successful answer that is empty**, and there are three ways the server could produce one. All
three are prevented:

1. **Settling** (§7.3). While the server has desired state but no observed state yet, the
   assignments endpoint returns an explicit not-ready status. The agent handles not-ready the
   same way as a failed poll. Not-ready cannot be represented as an empty set anywhere in the
   pipeline.
2. **A wiped store.** If etcd is restored from an empty backup or its prefix is deleted, every
   agent would poll successfully, receive an empty set, and, following its rules, stop every
   worker in the fleet. Two checks prevent this. The reconciler refuses to act while leased agents
   exist but none has reported any inventory. And no replica serves an assignment set until the
   leader has published its readiness record; without that record, every agent gets not-ready.
   An ordinary server restart is safe for a different reason: assignments are not leased, so they
   survive the restart and the server serves the same set as before.
3. **A node that stopped reporting.** Observed state is leased, so when an agent stops sending
   heartbeats, its inventory and status disappear. Every naive interpretation of that says "no
   flows, no sessions, tear it down". This includes a less obvious case: a group-hint selector
   that now matches no flows and so expands to zero paths. Therefore paths that touch a node that
   is not live are **frozen**: their sessions are kept, their assignments are copied forward
   unchanged from the store, and the reconciler reports that freezing happened so the loop can log
   it.

The rule behind all three: **a missing observation never means "nothing is there"**. The
response is always to freeze the current state, never to converge towards an empty one.

---

## 5. The pairing protocol

This is the most intricate part of the system. Read WRS §4 before this section.

### 5.1 Why it is hard

`target_info` is the blob a target worker writes so that an initiator can connect to it. It is a
serialised set of **RDMA memory-registration keys for the memory mappings of one specific
process** (`docs/third_party/mxl/Fabrics.md` §4.3). Three consequences follow:

- *Any* restart of the target worker invalidates it. An initiator given an old blob does not
  reconnect: the blob points at rkeys that no longer exist.
- Target workers restart routinely. Restart is the worker's only recovery mechanism, and the
  worker exits by itself when no grain arrives within a timeout. Since §15 that timeout is
  configurable, so an idle source no longer triggers it, but a link that is actually failing
  still makes the worker restart repeatedly.
- The pairing therefore has state, and **every re-establishment goes through the server**. In a
  peer-to-peer design the two ends would exchange the new blob directly.

The retired proxy handled this by sending `target_info` with every 9 s keepalive and tearing the
pairing down when the blob changed. That is edge-triggered: it reacts to the change as an
event. A store holds current values, not change events, so that approach does not work once the
blob is passed through a store.

### 5.2 Epochs

**Settled: the epoch is owned by the target side, which owns the fragile resource, and it is a
content hash rather than a counter.**

An **epoch** is a string that identifies one run of one target worker. Every time a target worker
starts, it gets a new epoch. The mechanism works as follows.

1. The destination agent computes an `epoch` for each target worker instance it runs, and
   reports `(session_id, epoch, target_info, bound_port, node_address)` as observed state.
2. The server gives the source agent an initiator assignment keyed by `(session_id, epoch)`. The
   assignment carries the `target_info` that belongs to that epoch.
3. The source agent applies one rule:

   > If the epoch I am running for session S differs from the epoch I am assigned for S, tear down
   > my initiator worker and start a new one with the new `target_info`.

For example: the target worker for session S crashes and the destination agent restarts it. The
new worker writes a new `target_info`, the agent computes a new epoch from it and reports it. The
server copies the new epoch and blob into the source agent's assignment. The source agent sees
that its running initiator has the old epoch, stops it, and starts one with the new blob.

There are no keepalives, no RPC to announce a change, and no teardown negotiation. Server
restarts, agent restarts and network partitions are handled by the same rule with no extra code,
because each of them either leaves the epoch unchanged (nothing to do) or changes it (reconnect).

#### What the epoch is

```
epoch  = "<nonce>:<sha256 hex>"
digest = sha256( format tag
               ‖ incarnation_nonce
               ‖ fabricAddress
               ‖ region count ‖ for each region: addr ‖ len ‖ rkey
               ‖ bounceBufferInfo.entryCount ‖ bounceBufferInfo.entrySize )
```

`incarnation_nonce` is a random value that the agent generates **once per target worker start**
with `crypto/rand.Text()`: 128 bits, base32-encoded. Base32 has no colon, so the `:` separator is
unambiguous. The agent keeps the nonce in memory next to the process handle. All other inputs
come from the `TargetInfo` JSON.

**Settled: the nonce is carried as a plain prefix, and is also inside the digest.** An earlier
formulation defined the epoch as only the hash over the nonce and the blob fields. It also said
that the initiator can recompute the epoch from the blob it received. Both cannot be true: the
initiator has the blob but never sees the target's nonce, so under that formula it could not
recompute anything. Putting the nonce in front of the digest makes the recomputation possible
without adding a field to the wire format or passing anything extra through the server. The nonce
is not a secret. Its only job is to tell one worker run from the next.

The epoch is a content hash and not a monotonic counter because the initiator's rule is an
*equality* test, not an ordering test. The initiator never needs to know which epoch is newer,
only whether the one it runs matches the one it was assigned. A hash gives three properties a
counter does not:

- **The agent stays stateless.** There is no counter to persist across agent restarts. An agent
  restart restarts the workers, each restarted worker gets a fresh nonce and so a new epoch, and
  the initiator reconnects. That is the wanted behaviour (§6.1).
- **It is self-validating.** `Verify(assigned, blob)` recomputes the epoch from the blob the
  initiator received and checks that it equals the assigned epoch. This catches a mismatched or
  truncated `target_info` before it reaches a worker, which would otherwise run without moving
  data and without reporting an error. `Verify` answers only that question. A pair of epoch and
  blob that match each other but are out of date passes `Verify`; detecting that is the job of
  the reconcile loop's equality test. Mixing up the two checks would be a real bug, so a test
  asserts the distinction.
- **It cannot get out of sync.** A counter that resets, or that is incremented on a code path
  where the registration did not actually change, causes either a missed reconnect or an
  unnecessary glitch.

A counter would have shown how often a target is restarting. The server gets the same signal by
counting epoch *transitions* (§12).

#### Why the nonce is required

Hashing only the `TargetInfo` fields looks almost sufficient. It would require that at least one
hashed field always differs between two runs of a target worker, and nothing guarantees that.
Measurement confirmed the concern. Two consecutive runs of the same tcp target on the same port
produced:

| field | across restart |
|---|---|
| `fabricAddress` | **identical** — it encodes `127.0.0.1:24999`, and the agent reuses the port by design (§7.4) |
| `regions[].addr` | **identical, and always `"0"`** |
| `regions[].len` | identical |
| `regions[].rkey` | different |
| `id` | different, but **not hashed** |

On tcp, `addr` is not an address at all: the provider reports `0` for every region, so the field
carries no information. The only hashed field that changed was the rkey, and nothing in the
library's contract promises that it will change. If all hashed fields ever came out the same,
the result would be the worst failure in the system: an initiator connected to a dead endpoint,
moving no data, with nothing reporting an error. The nonce costs one random string and one
concatenation and rules this out completely. The content fields stay in the hash as well. They
cost nothing, and they document what the epoch is meant to detect.

#### Field selection

`fabricAddress`, `addr` and `rkey` identify the endpoint and the remote memory registration. Two
more fields are included on purpose:

- **`regions[].len`.** If a region shrinks, the NIC bounds-checks writes against the memory
  region, so a stale length causes an RDMA protection error, not corruption. That failure is
  clean, but finding it through a worker restart loop is worse than a reconnect.
- **`bounceBufferInfo`.** This is the important one. The initiator computes scatter-gather
  offsets *within* the bounce buffer ring from `entrySize` and `entryCount`. With a stale value
  it writes at the wrong offsets inside a region that is correctly registered, so the NIC sees
  nothing wrong and the target unpacks garbage into the audio flow. It is the only field whose
  omission would cause silent data corruption instead of a visible failure. Only continuous
  (audio) flows have it. A discrete flow has no `bounceBufferInfo` at all, so the field is
  optional in the blob rather than present with zero values. (In the digest, an absent bounce
  buffer is hashed as `entryCount = 0`, `entrySize = 0`.)

Three fields are not hashed:

- `id` is an endpoint identifier, and how it is derived is not specified.
- `provider` and `addressFormat` only change together with a new session, because the server
  assigns the provider (§10).

The digest starts with a format tag, which separates it from any other use of sha256 over similar
input. It also includes the region *count* explicitly, so regions cannot be regrouped without
changing the digest. A golden test fixes the output. The framing is a wire contract between two
agents that may run different builds during a rolling upgrade. Changing it is therefore a
breaking protocol change and needs an `api.ProtocolVersion` bump, not just a new format tag.

#### Coupling to mxl-fabrics

`TargetInfo` is not part of MXL's public API. This project reads its fields anyway, which is
acceptable only because this project and mxl-fabrics have the same maintainer. The coupling must
be recorded on both sides. Otherwise someone will refactor `TargetInfo`, epoch behaviour will
change, and no build will fail.

Two guards are required:

1. A comment at the `TargetInfo` definition in mxl-fabrics that names this project as a consumer
   of its field set. **Still outstanding** — it is not in this tree, and it is the other half of
   the guard.
2. **A check for unknown fields when this project parses the blob.** `Decode` reports every field
   it does not recognise, including nested ones (`regions[1].x`, `bounceBufferInfo.x`). When
   upstream adds a field, this project logs it instead of silently leaving it out of the hash.
   It *warns* and does not fail. An unknown field is far more likely to be an additive, harmless
   change than one that matters to the epoch, and failing would stop replication because of an
   unrelated upgrade. The exception is a missing `id`. That mirrors the worker's own check
   (WRS §4) and does fail, so a truncated blob fails in the agent with a clear message instead of
   in a worker restart loop, where it would look like a fabric problem. The regression fixture is
   a real blob captured from mxl 1.1.0-rc1. A synthetic fixture would prove nothing about the
   upstream format this guard exists to watch.

Integer fields larger than `MaxInt64` arrive as decimal strings
(`"rkey": "17918262359965949928"`). They are hashed as their parsed 64-bit value, not as text, so
a library change that stops quoting them does not change any epoch.

### 5.3 Establishment sequence

1. The server computes that path P should exist and that no session carries it. It creates
   session S with no epoch and status `ESTABLISHING`.
2. The server assigns to the **destination** agent: `{session: S, role: target, area name + domain
   elements (resolved by the agent, §10.6), flow_def: <from inventory>, interface: <negotiated
   provider, caps flags and maxMessageSize, §10.3>, no_net_lat_measure, idle and connect
   timeouts, sched_prio}`.
3. The destination agent resolves the domain path and creates it with `MkdirAll` (the worker does
   not create it). It allocates a service (§7.4), generates an incarnation nonce, writes
   `config.json` into a **fresh** work directory, and execs the worker. It then waits for
   `target-info.json` to appear, using inotify on the work directory rather than polling with
   backoff (§6.1).
4. The destination agent computes the epoch (§5.2) and reports `{session: S, epoch, target_info,
   port, state: READY}`.
5. The server assigns to the **source** agent: `{session: S, epoch, role: initiator, domain,
   flow_id, target_info, peer: <node address and service>, interface: <the same negotiated config
   given to the target>, no_net_lat_measure, timeouts, sched_prio}`.
6. The source agent recomputes the epoch from the `target_info` it received and checks it
   against the assigned epoch. It then starts its initiator worker and reports `{session: S,
   epoch, state: ESTABLISHING}`. Once the destination's head index is observed, the state moves
   to `PAUSED` or `ACTIVE` as described in §11.

Steps 2–4 and steps 5–6 are separate reconcile passes, one on each agent. There is no handshake
between them.

**The order is required for correctness.** The initiator's `openFlow` fails outright if the flow
does not exist yet, and its connect loop waits for the target to appear. So an initiator is never
assigned before the target's epoch has been reported.

### 5.4 Session identity

**Settled: session identity is `(path, flow-def hash)`, and the path ID excludes the
definition.** If a flow is deleted and recreated with a different definition, the destination's
local flow no longer matches, so the session must be rebuilt, not repaired. Because the *path* ID
does not include the definition, a republished flow rebuilds the session but stays the same path.
The path's reference count, its request associations and its history are therefore not reset
without the operator noticing. On the source side, `mxl-utils` provides the check for a
republished flow: `Flow.IsValid()` compares the inode behind the mapping with the inode on disk,
and returns false when a flow was replaced under the same ID.

The path identity is `(src node, src domain, flow-id) → (dst node, dst domain)`. A domain here is
its fleet-wide name `<area>/<elements>` (§10.6). The identity used to include the resolved output
root as a separate term. The reason was that the target worker writes into a directory derived
from that root, and a destination moved to another root had to count as a different path, not as
the same path relocated. The area is now the first segment of the domain name, so that term is
redundant and has been removed: `fast/ingest` and `bulk/ingest` are already two different
identities.

This changes one behaviour on purpose:

- Pointing an area at a different **directory** while keeping its name does *not* change any
  path identity. That is an operator relocating a mount, and every path through it should
  survive.
- Moving a domain to a different **area** still does. That is an operator choosing a different
  destination, which is a different path (§10.6).

Session IDs are derived deterministically from that identity, not allocated. After a server
restart or a leader change the server therefore computes the same session IDs and adopts the
running workers instead of orphaning them (§7.3).

### 5.5 Matched settings

Some settings must be identical on both ends of a session. The server configures both ends from
one place, so these are session-level fields, not per-side configuration:

- **`no_network_latency_measurement`.** If the two ends disagree, the target reports wrong
  latency values and no error (WRS §5.3).
- **The negotiated interface config**: provider, caps flags and `maxMessageSize`. This is the
  strictest case. The library does no negotiation of its own and requires both ends to be given
  identical values (§10.3), so these fields cannot be anything but session-level.
- **The idle and connect timeouts** (§11.1). A value the two nodes could disagree about would be
  a bug, not a configuration choice, so these settings live on the server and are written into
  both halves of every assignment.

---

## 6. Agent

There is one agent per node. Its responsibilities are listed below.

**Discovery** uses `github.com/jonasohland/mxl-utils`. It consists only of recursive scans of the
node's **readable areas**. Every domain a node has was either found by one of those scans or
created by the reconciler (§10.6). Discovery is **not** filtered: a domain this project writes
into is discovered like any other. The protection against replication feeding on its own output
is a property of the flow, not of the directory (§10.7).

Discovery never grants write access. Reading and writing are separate grants on an area, and a
destination is resolved from configuration, never from scan results (§10.6).

**Settled: domains are discovered, never configured. The agent has no name→path mapping at all.**
This supersedes two earlier positions: that the `-m` flag maps input domains, and that
registration advertises those mappings while discovered domains reach the server through
inventory. The `-m` flag did two jobs at once. It gave the agent permission to read a directory,
and it gave that directory a fleet-wide name. These two jobs separate cleanly:

- The permission stays node-local, as an area's `read` grant (§10.6).
- The naming moves to the API as domain labels. There it is runtime state that an operator can
  change without restarting the agent (§10.7).

The benefits are recorded elsewhere: §10.6 loses an exception and a rejection code, §10.7 gives
the argument, and §16 records the config compatibility this costs. The operational benefit
belongs with §6.1. Adding or naming a domain used to require an agent restart, and an agent
restart re-establishes every flow on the node. Naming a domain should not interrupt media.

Two consequences follow directly:

- **Registration carries no domains**, because the agent has none configured. A node's domains
  are purely observed state and reach the server through inventory.
- The `Configured` flag on a domain has been removed. It was the security bit before areas had
  their own grants (§10.2, §10.6), and there is nothing left for it to distinguish.

**Settled: a domain is named `<area>/<elements>`, and that is its identity for as long as it
exists.** This supersedes "an input domain is named by its path". A domain needs a fleet-wide
name. The area supplies the part the operator already chose, and the path elements below it
supply the part the filesystem already fixed. Nothing has to be invented and nothing has to be
stored. That matters because the agent holds no persistent state (§6.1), so a synthetic ID would
have nowhere to live. The path spelling had to answer an objection: a name that looks like a
path invites an agent to use it as one. That can no longer happen, because `/etc` is not a valid
name in this grammar. The structural safeguard also still applies unchanged: the inventory lookup
is a map lookup with no fallback, and a destination is never resolved against inventory
(§10.6).

Labels do not affect this. A label annotates a domain, and a domain's identity is its name,
permanently. That is why relabelling is free. The domain name is part of path identity (§5.4),
session identity and the `domain` metric label, so renaming a domain would re-establish every
session through it just because of a metadata edit (§10.7).

**Inventory reporting.** For each flow the agent reports: ID, domain name, the full
`FlowDefinition` from `flow_def.json`, the parsed group hint (§9.1), a coarse `producing`
liveness state (§11.1), and a `replicated` boolean.

- The definition is required: the destination worker cannot create its local flow without it
  (§5.3 step 2). Definitions are passed on verbatim, so fields this project does not model reach
  the destination unchanged.
- `replicated` is true while one of this agent's own target workers is writing that flow. It is
  what stops a label selector from matching this project's own output (§10.7). The agent cannot
  get it wrong, because it is the process that started the worker. It also changes rarely, only
  when a target starts or stops, so it does not work against the compare-before-send rule below.
  It is shown to operators as well as used by the matcher: it appears in `GET /v1/flows` and
  `describe domain`, because otherwise a selector that skips a flow could not be diagnosed
  (§9.1).
- `producing` is **hysteretic and coarse**, never a raw head index (§11.1). Inventory is a full
  snapshot written to the store. A field that changed every frame would make every snapshot
  different and turn inventory into a store write on every heartbeat.

**Settled: compare-before-send on both reports, and it is required, not an optimisation.** The
agent sends an inventory or status snapshot only if it differs from the last one it sent. The
server writes whatever it receives without comparing. Every store write advances the revision and
wakes every watcher, including every agent's assignment long poll, where each unnecessary wakeup
costs a reconcile. An agent that re-sent unchanged snapshots on a timer would be the busiest
writer in the fleet and would keep every other agent's poll loop running. For the same reason,
both snapshots are sorted deterministically. The cache of the last sent snapshot is dropped on
re-registration: observed state is leased, and the old keys may have expired with the old lease.

**Worker supervision.** Each worker instance gets a fresh work directory holding `config.json`,
`metrics.sock` and, for targets only, `target-info.json`. The agent generates the JSON config,
stops workers with `SIGTERM` and a grace period, and restarts them with backoff. A work directory
is never reused across restarts. The worker does not delete an existing metrics socket before
binding, so a file left behind after a `SIGKILL` would cause a fatal `EADDRINUSE`. The worker has
been fixed as well (§15), but the rule stays. Stale work directories are deleted at agent
startup.

**Settled: one supervision goroutine per worker, and reconcile never blocks on a start.** Starting
a target includes waiting for `target-info.json`, which is the one step in establishment whose
wait has no clear bound. Since §6.3, a start can also wait for a start permit, which relies on
the same rule. If reconcile waited inline, a target that never came up would stop the node from
seeing any *other* assignment change until the timeout, including the change that would withdraw
that target. So a supervision unit owns the whole lifecycle of its worker (start, wait, classify
the exit, back off, restart), and reconcile only decides which units should exist.

Stops, by contrast, *are* synchronous. The caller is usually about to start a replacement for the
same session, and two overlapping workers would hold the same service name and write into the
same flow. Stops run concurrently with each other, so withdrawing every worker on a node takes
one grace period, not N.

**Flow liveness observation.** `mxl-utils`' `Flow.GetInfo()` returns `HeadIndex`,
`LastWriteTime` and `LastReadTime`. These drive both `producing` (§11.1) and the decision that a
destination is `ACTIVE` (§11).

**Interface discovery.** At startup and on re-registration, the agent runs the worker's probe
mode (§10.5) to list what libfabric actually offers. It joins that list with the configured
`fabrics:` block and advertises only the attachments present in both. This also checks that the
worker can load: it proves the binary exists and its shared libraries resolve before anything is
assigned. The worker's `-v` output is captured in the same pass and reported as the node's `mxl`
and `libfabric` versions (§10.2).

**Metrics scraping.** The agent connects to each worker's `AF_UNIX` socket, reads to EOF, parses,
adds labels and exposes the result. See §12.

**Reporting.** For worker state changes, the report loop is woken immediately rather than waiting
for its next tick. The reason is that a target's epoch reaches the server through this loop, and
the peer's initiator cannot be assigned until it does. The periodic tick is a fallback for
inventory, which is not on the establishment path. A failed heartbeat triggers nothing. If
heartbeats keep failing, the lease expires by itself, and the next successful report gets an
answer asking the agent to re-register. Reacting to a transport failure in any other way would
contradict fail-static (§4.2).

Only two server answers end the agent's registration session (not to be confused with a
replication session):

- `reregister`: the server no longer knows this node.
- `node_claimed`: another agent instance has taken this node name.

Both are logged as what they are, and **neither stops a worker**. An agent that lost the name to
another instance polls for no assignments and starts no workers, but keeps trying to register,
because the current holder may go away.

An assignment the node cannot carry out is **reported as a failed session with a reason**, not
ignored. Examples: a domain the node does not observe, an area it does not advertise or does not
grant `write` on, a fabric it does not advertise, or a blob that fails `epoch.Verify`. Ignoring it
would leave the path in `ESTABLISHING` with no explanation anywhere.

### 6.1 Agent restart and worker adoption

**Settled: on agent restart, kill and re-establish all workers. Do not attempt adoption. The
agent holds no persistent state.**

An agent restart interrupts every flow on that node. This is accepted, for four reasons:

1. **In the primary deployment there is no alternative.** The reference deployment is a
   DaemonSet, and the agent execs workers as child processes inside its own container. A
   container restart (crash, liveness failure, image update, rolling update) removes the PID
   namespace and every worker in it. There is nothing left to adopt.
2. **Agent restarts are rare.** The agent's *operational* state, meaning what is replicated where,
   lives in the API and never in the agent process. What remains in agent config (node name,
   areas and their grants, fabric attachments, server URL, port range) is set up when the host is
   built, not changed when a flow is routed. In practice, agents restart for upgrades.

   This became more true when input domain mappings were removed (§6). Naming a domain used to be
   an agent config change and so cost the interruption this section is about. It was also the one
   item on that list an operator changes while routing flows rather than while building the host.
   It is now an API write (§10.7).
3. **The interruption is smaller than one the system already accepts.** A worker exits by itself
   on its no-grain timeout and is restarted after a delay, so a transient fabric failure already
   causes an outage of several seconds on that flow. Optimising agent restarts while a fabric
   hiccup costs more would be optimising the wrong thing. If this interruption were ever judged
   unacceptable, the fix would be reconnect logic *in the worker*, not adoption in the agent, and
   that is a different and much larger project.
4. **A wrong adoption produces the worst failure in the system.** A worker adopted by mistake is
   an initiator running against stale rkeys: no error, no data, and everything upstream reporting
   healthy. That is the hardest kind of bug here to diagnose, and it would sit in a code path that
   only runs during upgrades.

Nothing survives a restart, including the epoch. A restart produces a fresh incarnation nonce and
so a new epoch, which causes the reconnect that is wanted (§5.2). There is no local database, no
PVC, and nothing to corrupt or migrate.

**Because the interruption is accepted, it is kept short**: 1–2 s, using four mechanisms:

- inotify on the work directory to detect `target-info.json`;
- long-polled assignments (§9.2), so the *peer* agent learns of the new epoch in well under a
  second;
- target workers started immediately and in parallel when the agent starts, not one after another
  behind registration;
- writable areas created at startup, so only the final `MkdirAll` of the domain directory is on
  the establishment path.

**The third mechanism has since been limited: starts are paced (§6.3), so 1–2 s is the target for
one *flow*, not for a whole node.** "Immediately and in parallel" assumed that a node's whole
worker set could start at once, and that turned out to exhaust the host. With §6.3's defaults, an
agent restart re-establishes its first workers within 1–2 s and the rest at the configured rate,
so a node with fifty flows takes well over a minute to be complete again. That is worse than this
section originally promised, and it is the right trade. Without pacing, the failure is not a
slower restart. It is a node that cannot start its workers at all, which also takes down the flows
that were running fine.

During the restart window, flow provenance is briefly missing, because `replicated` is derived
from running target workers (§6). This is safe, not just brief, and §10.6 explains why: a flow
whose target worker is not running is not advancing either, so §11.1's admission rule keeps
anything that might match it in `PAUSED` and starts nothing.

Note the interaction with §2.3. On a combined node the control plane and the agent share a
process, so a server crash *is* an agent restart and causes this interruption. That is the main
reason to run the control plane on nodes that carry few or no flows.

### 6.2 Configuration

The agent is configured with flags and YAML. The agent owns these settings:

- Node name (`--agent-node`, `MXL_REPLICATOR_NODE`, or the config file; defaults to the hostname,
  with a warning). It must be unique across the fleet; §7.1 covers what happens on a collision.
- **Areas**: name → directory, plus a `read` grant and a `write` grant (§10.6). `read` marks where
  domains are discovered; `write` marks where replication may create them. Neither implies the
  other, and both default to false. A node with no readable area offers no sources, and a node
  with no writable area accepts no destinations. The same default applies in both directions:
  access to a node's filesystem is opt-in. There is no name→path mapping for individual domains,
  because naming is done through the API (§10.7).
- Fabric attachments: provider, fabric label, and join selectors. At most one selector names the
  interface, and any number of the others narrow the match (§10.1).
- Server URL(s), bearer token, listen address, port range, work directory.
- The hysteresis threshold behind `producing`: `--agent-flow-idle-after`, default 3 s (§11.1).
- **The worker start rate and burst** (§6.3). These are node-local for the same reason as the
  port range: they describe what this host can handle, and the server cannot know that for a
  node it has never run on.

The agent does **not** own what is replicated (that is API state), or the session-level settings:
worker idle timeout, long-idle teardown threshold, connect timeout and the latency-measurement
flag. Those live on the server (§5.5, §11.1), because both ends of a session must agree on them
and only the server sees both ends.

### 6.3 Rate control on worker starts

**Settled: the agent admits worker starts through a token bucket, and nothing else passes through
it.** This was observed in production: when enough workers start at the same moment, the host
runs out of resources, and workers fail that were not the ones being started.

Simultaneous starts come from ordinary events, not rare ones, which is why the agent must handle
them itself instead of leaving it to an operator's runbook:

- An agent restart re-establishes every flow on the node at once, and §6.1 deliberately does this
  in parallel.
- A node returning from a partition reconciles against the current desired state, all at once
  (§4.2).
- A destination node restarting changes the epoch of every session it is the target of, so every
  peer initiator is replaced at the same time (§5.2).
- A large apply arrives as one assignment set.

Each of these is the design working correctly, and each produces a burst of starts.

**The scarce resources are used by *starting* a worker, not by running one.** A worker that is
starting mmaps its flow, registers that memory with the NIC against a pinned-page limit shared by
the whole host, execs a process, binds a service and opens a metrics socket. Once it is running,
it is the cheap process that §14 sizes for. So the thing to spread out is the startup, and the
mechanism is admission control on starts.

Three properties, in order of importance:

- **Every start goes through the bucket, including restarts.** A worker coming back from a crash
  loop uses the same resources as a newly assigned one, and a fabric outage that makes N workers
  restart repeatedly is a burst of starts like any other. §11.1's backoff limits how often each
  worker restarts; the bucket limits how many start at once across all of them.
- **Stops never go through the bucket.** A withdrawal that waited for a permit would keep a
  service and a flow open for no reason, and it would break §6's property that withdrawing every
  worker takes one grace period, not N. For the same reason, a start that is *waiting* for a
  permit must be cancellable immediately. Reconcile stops workers synchronously, so a wait that
  ignored its cancelled context would block every other stop on the node behind a permit nobody
  would use. A cancelled wait also returns its reservation, so a withdrawn worker does not use up
  a permit.
- **The wait happens inside the supervision goroutine.** This is §6's rule that reconcile never
  blocks on a start, applied where it matters: a waiting start delays only its own worker. It
  does not delay any other assignment, including the one that would withdraw it. If the pacing were in
  reconcile, a burst of starts would turn into a node that stops reacting to anything until the burst is
  through.

#### Why a bucket and not a limit on starts in flight

A concurrency limit adapts better, and it was the first design. It frees a slot as soon as a
start finishes, so it costs nothing on a host that keeps up and slows down as much as a host that
is struggling needs. **It was rejected because only half of the workers signal that their start
has finished.** A target does: its signal is `target-info.json` (§5.3 step 3). An initiator has
no such signal. For an initiator, "ready" means the process is up and its connect loop has begun
(§6), which is *before* it has registered anything. A concurrency limit would therefore free
an initiator's slot immediately and give no protection for initiators, even though a fleet-wide
re-establishment produces as many initiator starts as target starts. Giving the initiator a
readiness signal would require changing WRS and the worker, which is a much larger project than
this problem justifies (§15).

So the bucket's **burst is the setting that matters in operation**. It is the number of workers
that may be in setup at the same moment, which is what the host's limits are measured against.
The rate limits how long the rest take.

Waiting starts are admitted in arrival order, with no other ordering. **Giving targets priority
over initiators was considered and rejected.** It sounds right, because a target's start is what
unblocks its peer (§5.3). But on a node reconciling many sessions at once, the waiting initiators
are the peers of targets on *other* nodes that are already up. Prioritising targets would delay
media that is otherwise ready in order to speed up media that is not. A priority queue would also
need a rule against starvation, which a bucket does not.

#### The two settings, and what zero means

There are two settings, both agent-local (§6.2): starts per second, and the burst.

- **A rate of `0` means no limit.** This is the same convention §2.2 uses for the worker idle
  timeout, and the choice matters: if zero meant *admit nothing*, a typo would silently stop
  every flow on the node.
- **A burst below one is rejected when flags are parsed.** A bucket that can never hold a token
  never admits a worker, and a node that comes up healthy with every session stuck in `starting`
  is a bad way to find a configuration mistake.

**The defaults are deliberately conservative — burst 2, rate 0.5/s — and they make
re-establishment noticeably slower.** Fifty workers then take about a minute and a half, compared
with §6.1's 1–2 s for one flow; §6.1 has been amended to say so. The two risks are unequal, and
that decides it: too fast takes down the whole node, including flows that were running fine,
while too slow only delays a node that is already recovering. A deployment with spare capacity
should raise both settings, the burst first.

**A start that is waiting for a permit reports that it is waiting**: state `starting`, with a
reason saying the start is queued behind the node's start rate limit. Without it, a worker that
has not been launched yet looks the same as one that was launched and is slow to come up, and an
operator watching a slow recovery needs to tell those apart. The reason is set only when a start
actually has to wait. In the normal case there is no status change, no report, and therefore no
store write (§6, §8.3).

---

## 7. Server

### 7.1 Node registration and fencing

The server keeps two separate records per node. They must not be merged:

- **Registration** is durable. It records that node `edge-01` exists, its verified fabric
  attachments and other capabilities (§10.2), and its areas with their grants. It stays in place
  while the agent is down. It does not list domains: domains are discovered, so they are observed
  state (§6).
- **Liveness lease** is observed state with a TTL. It records that an agent instance currently
  holds that node identity.

An agent registers as `(node_name, instance_uuid)` and holds a lease. **Two agents claiming the
same node name is a real failure mode.** It happens with a copy-pasted config or an overlapping
Kubernetes rollout. The damage is serious: both agents receive the same assignments, both start
workers, the workers compete for service names, and both write into the destination flow. So the
lease is exclusive. While the first lease is held, a second claimant is rejected, and the
rejection is logged and counted in metrics so it is easy to see. On etcd this is a lease plus a
CAS; on sqlite it is an expiry column and a transaction.

**A heartbeat renews the lease and writes nothing.** If each heartbeat rewrote the lease record,
every node would advance the store revision several times a minute, forever. Each advance wakes
every watcher, including every agent's long poll, and on an agent a spurious wakeup restarts a
worker. The trade-off is that a node's `LastSeen` shows when the lease was *acquired*, not when
the last heartbeat arrived. Liveness is whether the lease exists, which is bounded by its TTL.

### 7.2 Request validation

Invalid requests are rejected when they are submitted instead of sitting in the store stuck. The
reasons fall into separate categories, and the categories are kept distinct: `INVALID` codes need
user action, and the codes under "Not an error" clear by themselves. The `INVALID` codes are split
again by whether they refuse the `POST`.

**Settled: a code refuses the `POST` if it can be decided from the request plus node
registrations. If deciding it needs the flow inventory or other requests, it marks one path
`INVALID` and leaves the request's other paths alone.** Each code has one of these two
dispositions, never both. The code implements this split: `validate.Request` and
`validate.Pairing` failures are recorded in `reconcile.Result.Structural`, and the `POST` handler
refuses those with a 400 (`internal/server/reconcile/reconcile.go`, `internal/server/userapi.go`).
`validate.Conflicts` and the namespace overlap check only ever mark paths.

*An earlier version of this section put every `INVALID` code in one list headed "Rejectable
immediately (`INVALID`, needs user action)", and a later paragraph said that `POST` refuses only
what is structurally invalid.* The heading dates from when the `POST` was refused whenever
`Compute` returned `INVALID` for any reason. Once validation became per path, the two statements
gave every code both dispositions and did not say which codes were structural (open-items §1.2).
The two lists below replace the single list.

A code in the first list can also mark a path `INVALID` later, in steady state, because stored
requests are re-validated on every reconcile. For example, an area removed from a node's config
after the request was accepted produces `unknown_area` on a live path, not a refused `POST`. The
first list is the set of codes that *also* refuse the write. The line is drawn there because a
`POST` refusal must be something its author can act on.

*Refuses the `POST` (400; decidable from the request plus node registrations):*

- `unknown_area` / `area_not_writable`: the destination names an area the node does not
  advertise, or one it advertises without the `write` grant. **A destination is always a name
  inside an area the operator has granted write access to** (§10.6). The API never accepts a raw
  path; if it did, it would let any API client write anywhere on the filesystem of every node in
  the fleet. A node that advertises no writable area at all gets `unknown_area`, with a reason
  string that says so, rather than a code of its own.

  *This replaces `no_output_root` / `unknown_output_root` / `ambiguous_output_root`.*
  `ambiguous_output_root` can no longer occur: a destination always names its area, so there is
  no case where a node advertises several and the request named none. `no_output_root` was folded
  into `unknown_output_root` once the write grant became a field on an area entry instead of
  being defined by membership in a separate table.
- `malformed_domain_name`: the destination domain is not an area name followed by a list of
  clean path elements. The user API normally refuses such a name earlier, in
  `RequestSpec.Validate`; this code is for stored requests that are re-validated.
- `node_not_registered`: a source or destination node that no agent has ever registered. This is
  `INVALID`, not `WAITING`, because an agent creates its own registration and the registration is
  durable. An unknown name is a typo or a node that was never deployed, and only a user can fix
  that. A registered node whose agent is down is `agent_not_leased`, which is `WAITING`.
- `same_endpoint`: a source and a destination are the same `(node, domain)`. This happens in
  ordinary use now that a source may name any domain: a source `{name: fast/ingest}` paired with a
  destination that resolves to `fast/ingest` on the same node is the self-pair this code exists to
  catch. **It applies to a named source only.** When a label selector matches the destination's
  own domain, nobody wrote the pairing explicitly, so that one pairing is dropped and the rest of
  the expansion stays (§10.7). Dropping it also keeps `same_endpoint` decidable from the request
  plus node registrations alone, which is why it belongs in this list and not in the per-path one.

  **It is checked for every `(source, destination)` pairing, and one failing pairing refuses the
  request**, because both ends are now lists (§9.1). The message gives both indices, because
  "source and destination are both edge-01/fast/ingest" does not say which of nine sources is
  meant. Refusing the whole request over one pairing is correct here and nowhere else: both ends
  were named explicitly, so the pairing is a typo, and the author can see both halves of it in the
  file they just wrote.

  It also covers the disjointness check §10.8 asks for as `overlapping_selectors`. With both ends
  enumerated, "the source set and the destination set must not intersect" is the same thing as
  the pairwise `same_endpoint` test. So the two-node cycle that a cross product would otherwise
  create cannot be written at all, rather than being detected afterwards.
- `duplicate_source_flow`: two of the request's sources pin the same flow UUID and share a
  destination. That means two initiators writing one destination ring buffer, which is the harm
  `flow_conflict` guards against, arising inside a single request. When both sources pin a flow
  ID, this is decidable from the request body alone, so it is refused here instead of waiting for
  the fleet to produce the collision.

  **It is a separate code, not an early `flow_conflict`**, because each code has one disposition.
  `flow_conflict` invalidates a path and tears the losing path down. Giving it a second,
  request-time disposition for one decidable subcase would break the split between request-time
  and per-path codes. The undecidable form stays `flow_conflict` and stays per path: that is when
  one or both sources use a selector instead of a pinned flow, so the collision can appear months
  later when a producer starts. This mirrors the `same_endpoint` / dropped-pairing split in the
  previous bullet.
- `no_shared_fabric` / `no_shared_provider` / `no_shared_capability`: no viable interface pair
  (§10.3). There are three codes because each points the operator at a different problem.
- `pin_not_viable`: a pinned provider (§10.4) is not among the viable pairs. The server never
  substitutes another provider.
- `sched_prio_unavailable`: `sched_prio` was requested, but a node the request names does not
  have the capability (§10.2). It is checked once per node over the whole request, not per
  pairing, so one missing capability is reported once.

*Marks the path `INVALID` (the `POST` succeeds; needs other requests or the flow inventory):*

- `flow_conflict`: the destination `(node, domain)` already holds that flow ID from a different
  source. Replicating two different producers into one flow ID corrupts the ring buffer.
- `loop`: cycle detection over the per-flow graph. `A→B→C` is a valid and useful chain. `A→B` plus
  `B→A` for the same flow is a loop, and so is `A→B→C→A`, which is the same mistake with more
  hops. This covers cycles an operator *wrote*. Cycles that a selector could *create* over time are
  prevented differently: a label selector never matches a flow this project writes (§10.7).
- `domain_name_in_use`: a destination domain nests with one that another path already
  materialises on the same node, for example `fast/studio-a` and `fast/studio-a/cam1` (§10.6). It
  is per path because the two paths need not share a source, a flow or a request, so neither
  request can see the collision on its own.
- `namespace_overlap`: two requests in one namespace expand onto the same path (§9.3).

*`domain_name_in_use` used to mean something else.* It caught two output domains with the same
name under different roots on one node. Now that the area is part of a domain's name,
`fast/ingest` and `bulk/ingest` no longer map to the same address, so that collision cannot be
constructed (§10.6). An earlier version of this section said the code had been removed outright.
The name was kept for the nesting check above, which moved it from the request-time list to the
per-path one.

*Not an error:*

- `flow_not_found`: the source flow is not currently observed. `WAITING`; clears by itself.
- `agent_not_leased`: an agent is not currently up. `WAITING`.
- `source_idle`: the flow exists but nothing is writing to it. **`PAUSED`, not `WAITING`** (see
  below).

**Settled: a path whose source is not producing is `PAUSED`, not `WAITING`.** *An earlier version
filed `source_idle` under `WAITING`, while §11.1's table listed the long-idle state as `PAUSED`
with no workers.* Both cannot be right. The only difference between the two situations is
whether workers happen to be running, and an operator would not act differently on that.
`PAUSED` means "the source is not sending", which is true in both cases and is the distinction
`PAUSED` exists for. `WAITING` means "the flow is not visible", which is false.

**Settled: an `INVALID` request stops new sessions; it does not tear down running ones.** The
obvious reading of `INVALID` is "remove it". But a request can become invalid because a
*registration* changed: an attachment disappeared while an agent re-probed, or an area was edited
on the destination node. If the request's session is already carrying media, removing it would
stop media because of that registration change. A request is durable intent and the system never
cancels one on the user's behalf (§11). So validity controls **admission only**. An invalid
request still expands, onto shadow paths. A shadow path keeps whatever session already exists and
carries its assignments forward unchanged.

The same rule gives §10.4's fabric case. When a session's negotiation stops succeeding, it reports
**`FAILED` / `fabric_gone`**, not `INVALID`. `INVALID` means "needs user action, never resolves by
itself", and a fabric that stopped being advertised can come back on its own.

**One code is an exception to that rule: `flow_conflict` tears the losing path down.** Every other
invalidity reason describes *intent* that cannot currently be satisfied, and stopping media for it
would be the registration-triggered teardown the rule exists to prevent. `flow_conflict` is
different: the harm is the running state itself, two initiators writing into one destination ring
buffer, which is the corruption the check is meant to catch. Leaving both running because neither
request changed would apply the rule to a case its reasoning does not cover.

This case can only happen because a conflict does not always arrive with a write. Two paths can
both be established and only later come into conflict, or two reconcilers can each admit one side
during a settling window or a partition. §7.5 describes how that is handled.

**Conflicts are decided by §7.5's precedence**, which is one strict order over candidate paths,
not a separate rule per code. When several requests share a path, their settings are merged
conservatively:

- pins intersect. An empty intersection is `pin_not_viable`. This is tracked separately from
  "pinned nothing", which would negotiate freely and so silently substitute a provider, which
  §10.4 forbids;
- the highest `sched_prio` wins;
- the longest idle teardown wins;
- labels merge.

**`describe path` names every contributing request and the merged result.** Without that, an
operator whose pin lost an intersection to a request not in their own file has nothing to look at.

**Validation is per path, not per request.** The unit it runs over is a `(source, destination)`
**pairing**, not a destination, because both ends of a request are now lists (§9.1). A request
whose selector expands onto twenty paths, one of which conflicts, is not refused: it reports
nineteen paths plus one invalid path with its reason. `POST` refuses only what is *structurally*
invalid: a spec that fails `RequestSpec.Validate` (a malformed selector or domain name), and the
codes in the first list above. §7.3's property still holds, because request-time rejection and
steady-state classification still run the same `Compute`; they now agree about a set of paths rather than a
single verdict. This is also what makes selectors usable. The author of a selector cannot list its
expansion before submitting it, so refusing the whole request for one bad pairing would make the
author depend on fleet state they did not write.

**A disabled destination is not a pairing, so it is validated against nothing** (§9.1). It adds no
path that could be refused, no negotiation that could fail, and no node whose `sched_prio` could
be missing. Every code in the lists above ignores it until it is enabled. Structural validation
is the exception and still runs over the whole spec: a malformed domain name is refused at `POST`
whether or not its entry is enabled. That way nothing invalid can be stored in a disabled entry
and then fail months later when someone enables it.

### 7.3 Reconciler

**Settled: `Compute` is one pure function of a snapshot, and the read handlers run it too.** The
loop is `Compute(fleet, cfg) → Result` plus an `Apply` that writes the diff. `GET /v1/paths`,
`GET /v1/requests` and, most importantly, the request `POST` call the same `Compute`.

The `POST` case is what makes §7.2's validation a single implementation rather than a second copy
of it. The POST handler builds the fleet as it would look with the candidate request added, runs
`Compute`, and refuses the request if `Compute` reports a structural failure for it (§7.2's first
list). So request-time rejection and steady-state classification cannot disagree. That includes
conflicts only visible across requests (two sources into one destination flow, loops), which no
per-request check could see; `Compute` reports those as `INVALID` paths in the response rather
than refusing the `POST`. The same property means a follower replica shows what the leader is
doing, and `?dry_run=true` runs this same computation.

The fleet snapshot is **one `List("")`**. Three separate lists would be read at three revisions,
and a reconcile computed from a skewed snapshot can conclude that a session both should and should
not exist. Keys outside the three layers are ignored, not reported as damage. A key that cannot be
decoded is collected as malformed instead of failing the pass, so one bad key cannot stop the
reconciler for the whole fleet.

Session IDs are the derived state that must stay the same across reconciles. They are derived
deterministically from the path identity (§5.4), not allocated, so after a server restart or a
leader change the server computes the same ID and adopts the running workers. Epochs come from
agent reports, so the server does not need to own them at all.

#### Settling after a server restart

Deterministic session IDs are necessary but not sufficient. On restart the server has desired
state (requests) but **no observed state**. A reconcile that ran immediately would conclude that
no sessions exist and issue fresh assignments for sessions that are already running. Each would
get a new nonce and a new epoch, causing an unnecessary media glitch on every server restart and
every HA leader change.

Three mechanisms prevent this:

1. **A settling window before the first reconcile.** Its length is a small multiple of the agent
   heartbeat interval (3× by default), so it scales with the configured heartbeat instead of being
   an unrelated constant. It ends early once every leased agent has reported. A newly elected
   leader waits out the same window.
2. **Agents report their full running state**, not only the status of what they were assigned. So
   the server can see a worker it did not assign in its current process lifetime, recognise it by
   session ID, and adopt it.
3. **The reconciler publishes a readiness record at `/derived/reconciler`, and the assignments
   endpoint only serves once that record exists.** Only the leader runs the loop, but every
   replica serves assignments, so a follower cannot tell locally whether the fleet has settled.
   The leader writes the record when it has, and every replica reads it. This is also how a wiped
   store becomes visible (§4.2): with no record, no replica serves an assignment set at all. The
   record intentionally has no timestamp or per-pass revision. The loop that writes it also
   watches it, so a field that changed every pass would trigger another pass, which would write
   again, in an endless cycle of store writes.

During the window the server serves reads and accepts writes as usual; it just does not act on
them. `GET /v1/paths` reports that the server is settling, instead of reporting every path as
`WAITING`, which would look like a fleet-wide outage to anything scraping it.

#### The agent's "already correct" test

The agent has the matching hazard. Its check "am I already running the right thing?" keys on
**session ID, role, and the config that actually affects the worker**: domain path, flow ID,
epoch, the negotiated interface config, and the matched settings from §5.5. It does *not* compare
the assignment object as a whole.

If it did, any incidental difference would count as a change and restart a healthy worker: a
re-derived port, a reordered JSON field, a re-serialised `flow_def` with the same meaning. This
kind of bug passes every test and then causes restarts in production, because the incidental
differences only appear once there are two server replicas or a store round trip in the path. For
that reason there is an end-to-end test that modifies the assignment *on the wire*.

#### Writes are fenced

Every session and assignment key is written with a CAS against the revision the snapshot was read
at. A demoted leader computing from a stale read therefore fails its write instead of overwriting
the new leader's (§8.2). This is optimistic concurrency, not true fencing, and the remaining gap is
known: two leaders that both read at revision N and compute *identical* content will both succeed.
That is harmless because the content is identical.

Two smaller properties the fleet depends on:

- **Every registered node gets an assignment key, even an empty one**, and the endpoint returns
  `[]`, not `null`. To a poll, absence and emptiness mean the same thing, and this is the one field
  in the system where confusing them would stop every worker in the fleet.
- **The assignment set's revision is set when it is served**, not stored. It is the store revision
  at which the set was served. A stored revision would make an unchanged set look different
  whenever the store advanced for unrelated reasons.

### 7.4 Port allocation

**Settled: the agent allocates ports from an operator-configured range, and reports what it
actually bound.** The worker binds whatever `service` it is given and has no fallback. The agent
knows the real situation: it can probe, and it knows what it has already started. The server
cannot verify a port it hands out, so it does not allocate ports. The range is configurable per
node so operators can write firewall rules. Note that the fabric connection is inbound to the
*destination* node, so the range must be open there.

Allocation is stable per owner: a worker that restarts gets the same service back. That is why
`fabricAddress` can be byte-identical across a restart, and why the epoch needs its nonce (§5.2).

**Settled: probe-bind only for `tcp`.** A probe-bind detects a collision with some other process on
the host. For `verbs` and `efa`, the service is a port in the RDMA CM's own port space, which is a
separate table from the kernel's TCP ports. A successful TCP bind proves nothing there, and a
failed one would reject a usable port, silently shrinking the operator's configured range. For
those providers, and for `shm`, the allocator relies on its own bookkeeping alone. That is safe
because node-name exclusivity (§7.1) guarantees there is no second agent on the node to race with.

`shm` needs a service too, but it needs a *host-wide unique name* rather than a port, and the same
range allocator produces one. So there is one allocator, one collision domain, and no
per-provider branch anywhere.

### 7.5 Conflict precedence

**Settled: candidate paths are ordered by `(incumbency, UpdatedAt, id)`, and conflicts are resolved
greedily in that order.** *This replaces "conflicts are decided oldest-first, ties break on path
ID".*

*Not built yet.* `validate.Conflicts` still uses the superseded order: it sorts paths by `Since`,
the creation time of the earliest request on the path, then by path ID
(`internal/server/validate/validate.go`, `internal/server/reconcile/reconcile.go`). It does not
consult session records or `UpdatedAt`. The `mxl_repl_path_conflicts` metric below does not exist
either. The design in this section is what the code should move to (open-items §2.12). The
namespace overlap contest (§9.3) is separate and already orders on `(UpdatedAt, id)`.

One constraint rules out most of the obvious options. §7.3 makes `Compute` a pure function of one
snapshot. It is run by follower read handlers and by a newly elected leader with no history. So
**every term in the order must be derivable from the snapshot alone**: no arrival order, no
"whoever held it last pass", nothing remembered between reconciles. Both terms below meet this.
Request timestamps are desired state, session records are derived state, and the snapshot is one
`List("")` over all of it.

#### Why age is not the first term

Oldest-first was justified as "a newly created request never invalidates a path that is probably
already carrying media". The word *probably* is the problem: age is used as a stand-in for "already
running". That works only while conflicts are caused by someone submitting a request. It gives the
wrong answer as soon as a conflict is caused by a change in observed state.

```
R1  created January   source {node: studio-a, group_hint: "Cam 1"} → edge-01:ingest
                      matched nothing all year — that camera was offline
R2  created June      source {node: studio-b, flow: F}             → edge-01:ingest
                      ACTIVE since June

August: studio-a's camera comes online and publishes flow F.
```

R1 now expands onto a path carrying F into `edge-01:ingest`. Oldest-first gives R1 the path and
stops media that has been flowing for two months, because a producer in another studio was
switched on. That is the outcome the rule was written to prevent, caused by the rule itself.

#### The three terms

1. **Incumbency.** A path is incumbent if it has a **derived session record**, regardless of
   whether its workers are running. The session record is not leased, so it survives a worker
   restart, an epoch change and a node freeze. Keying on observed worker status instead would flip
   the winner every time a target restarted, and each flip would write to the store and trigger
   another reconcile: the same feedback loop §7.3 avoids for the readiness record. It also fits
   §4.2 with no extra work: a frozen node keeps its sessions, so it stays incumbent, keeps its
   paths through a partition, and reconciles normally when it returns.
2. **`UpdatedAt`**, not creation time, for the same reason the namespace rule uses it: the refusal
   goes to whoever made the change. With creation time, an old request could be edited to take
   over a newer request's path. The `POST` that caused the conflict would succeed, and the
   untouched newer request would flip to `INVALID` instead.
3. **The path ID**, so every replica breaks an otherwise arbitrary tie the same way.

The first term only decides conflicts that arose without anyone submitting anything, where
`UpdatedAt` would be arbitrary. The second only decides conflicts between two paths that are both
new. The two terms never compete.

#### Reporting

A path that loses records the identity of the path that beat it, not only a reason code. Otherwise
a path that went `ACTIVE` → `INVALID` overnight with nobody applying anything could not be
diagnosed, and under the order above that is an expected occurrence, not a bug.

The message distinguishes **losing to another request** from **two paths of one request
colliding**. The second case becomes possible with source domain selectors (§10.7). The same flow
ID may legitimately exist in two domains on one node (§3), so a selector matching both produces two
paths into one destination flow. Admission cannot catch this, since the second domain may be
discovered or labelled months later. The two cases need different fixes: in the first, the
operator narrows their own selector; in the second, they talk to whoever owns the other request.

**With fan-in the second case becomes routine, not just possible** (§9.1), and it shows a property
of the order. Two paths of one request share an `UpdatedAt`, and neither is incumbent until one is
established, so the tie falls through to the path ID. That result is deterministic across replicas
and stable over time, as the order requires. From the operator's point of view it is still
arbitrary, and no ordering can fix that, because nothing in the request says which of two sources
of one flow ID was intended. The decidable form of this mistake is refused at `POST` as
`duplicate_source_flow` (§7.2), so that only the cases that could not be caught earlier reach the
arbitrary tiebreak. When they do, the message names **both sources**, not only the winner.

`mxl_repl_path_conflicts{reason}` is added to the leader's fleet gauges (§12), because a conflict
that resolves without any signal is a request whose expansion shrank without anyone noticing.

#### Conflicts are recomputed every pass

Resolution is recomputed on every reconcile, not recorded. So a losing path establishes itself as
soon as the winner's request is deleted or the winner's flow disappears. There is no suppression
flag to store, nothing to undo and nothing to garbage-collect. This is the practical benefit of the
purity §7.3 requires. It only works because incumbency keys on something stable, the session
record, which is the argument for using the session record above.

---

## 8. Storage

### 8.1 One interface, two backends

Supporting etcd and sqlite behind one interface is a common place for a project like this to get
stuck. etcd provides revisions, CAS, watch and leases. sqlite provides transactions and none of
the others. If the interface is designed around sqlite, HA becomes impossible. If it is
designed around etcd, the sqlite backend has to emulate the missing features.

**Settled: define the interface in etcd's terms — a revisioned KV store with CAS, prefix watch
and TTL leases — and implement it over sqlite.** The alternative is a domain-level interface with
two hand-written implementations. That avoids emulation but duplicates the reconciler's
consistency logic in both, and the agent long poll (§9.2) needs a revision cursor either way.

The emulation is small. These are the decisions in it:

- *revision*: a monotonic integer column, incremented in the same transaction as the write.
- *watch*: **reads forward through an append-only history table**, instead of polling current
  state. Polling current state cannot report a delete or an intermediate value, and the long poll
  depends on the watch.
- *cursor*: **`(revision, seq)`, not a revision alone.** One revision can carry several events,
  so a revision-only cursor either replays or skips the rest of a multi-event commit.
- *compaction*: history is bounded, and `ErrCompacted` is part of the interface. A watcher that
  fell too far behind is told so instead of silently missing events. The etcd backend does not
  compact; etcd's own compaction policy handles that.
- *lease*: an expiry-timestamp column plus a sweeper timer.
- Watches are **woken by the committing transaction**; the poll interval is only a fallback.

The conformance suite keeps the abstraction correct. It is written against sqlite and must pass
**unchanged** against etcd. If it does not, the interface is wrong: backend-specific behaviour has
leaked into it, which is the risk this section is about.

The store layer stores bytes. It does not know what a request or an assignment is, and it imports
nothing from `internal/api`. Serialisation, validation and the meaning of each key belong to the
server.

### 8.2 HA

Being deployable behind a simple third-party HTTP proxy leads to two requirements.

**No sticky sessions.** Every replica must be able to serve every agent request. This is the
reason §9 uses polling instead of server-push streams. An SSE or gRPC stream is attached to one
replica, so state written by another replica has to be watched and forwarded to reach it. Polling
with a revision cursor makes every request self-contained.

**One reconciler.** If every replica reconciled, they would conflict: CAS retries, assignments
changing back and forth, and duplicate epoch decisions. etcd's concurrency package elects a
leader. The leader runs the reconciler; every replica serves the API. Election uses the store's
own client under the deployment's prefix. On sqlite there is only one process, so it is always the
leader.

Running several replicas creates three hazards that a single server does not have. Each is
handled:

- **Revision cursors must not go backwards across replicas.** Behind a plain load balancer,
  consecutive polls from one agent hit different replicas. Suppose the agent got a cursor from
  replica A, and replica B's view is behind that cursor. If B answers from its stale view, the
  agent switches between two assignment versions and restarts workers on every switch. §7.3's
  "already correct" test does not help, because the two sets really do differ. **A replica never
  serves an assignment set at a revision below the client's cursor**; if its local view is behind,
  it waits.
- **A demoted leader must not keep writing.** A partitioned old leader can believe it still leads
  until its lease expires. So every derived write is a CAS on the revision it read (§7.3), rather
  than relying on election alone to ensure a single writer.
- **Leader churn.** A combined instance runs etcd keepalives, lease renewal, a store watch, held
  long polls and the metrics scrape loop in one process. On a node whose workers run `SCHED_FIFO`
  via `sched_prio`, real-time threads can starve the Go runtime. A missed keepalive loses
  leadership, and frequent leader changes mean repeated settling windows and repeated
  reconciles. Mitigations:
  - raise `GOMAXPROCS` for the combined role;
  - where possible, keep the control plane off nodes that run real-time workers;
  - leave headroom in `sched_rt_runtime_us`;
  - watch the leader-change metric. Frequent changes are the symptom, and nothing else shows them.

### 8.3 Write volume

Write volume needs an estimate, because `target_info` updates go through the store. A blob is
roughly 1–2 KB and is written once per target-worker start. In steady state there are almost no
writes: heartbeats write nothing (§7.1), unchanged snapshots are not sent (§6), and re-applying an
unchanged request writes nothing (§9.1).

Two sources of writes would otherwise dominate. §11.1 handles both, and the estimate above depends
on that:

- **Idle sources do not generate writes.** A configurable idle timeout means a paused session's
  workers wait instead of exiting every 10 s, and the admission rule means a dormant flow never
  starts workers at all. Without these, every idle but requested flow would write ~1.5 KB every
  13 s indefinitely, as part of normal operation.
- **Real failures are limited by backoff.** When a fabric outage makes N flows restart
  repeatedly, the write rate is limited by the agent's restart backoff, which grows towards
  minutes, not by the worker's fixed restart cycle.

Observed state is leased, so it is garbage-collected automatically. It sits under its own key
prefix so the three layers can have different compaction and backup policies (§4).

---

## 9. API

**The user API and the agent API use separate path prefixes**: `/v1/...` and `/agent/v1/...`.
They differ in auth, in clients, in request rates and in compatibility guarantees, and an operator
must be able to expose one through an ingress without exposing the other.

### 9.1 User API

```
POST   /v1/namespaces/{ns}/requests      create or update, keyed on the name within {ns}
                                         ?dry_run=true validates and reconciles without writing
GET    /v1/namespaces/{ns}/requests      the requests in {ns}; same set as /v1/requests?namespace={ns}
GET    /v1/namespaces/{ns}/requests/{name}
DELETE /v1/namespaces/{ns}/requests/{name}   cancel; path torn down only when refcount hits 0
GET    /v1/requests[?namespace=]         fleet-wide list, with status

GET    /v1/namespaces                    list
POST   /v1/namespaces                    create or update, keyed on name (§9.3)
GET    /v1/namespaces/{ns}
DELETE /v1/namespaces/{ns}               refused while any request references it

GET    /v1/nodes                    registered nodes: liveness, capabilities (§10.2), areas
GET    /v1/nodes/{node}             not served yet (404); see below
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

`GET /v1/nodes/{node}` is part of the design but the server does not register the route
(`internal/server/http.go`), so it returns 404. Clients, including `describe node` and the UI,
read `GET /v1/nodes` and filter it. Whether to serve the route or drop it is open-items §5.

The three `events` reads and `logs` are the only user-API reads that do **not** run `Compute`
(§7.3). Each one reads a ring, which is a single `Get` on a single key, so its cost grows with the
size of the response. Every other read here costs in proportion to the fleet. This is deliberate:
the event log is the endpoint a UI polls most often, and it polls it hardest when the fleet is
least healthy (§12.1).

`GET /v1/nodes/{node}/domains` reports the domains the node **observes**, not registration data,
because there is no configured domain mapping to report (§6). It joins each observed domain with
its label record, and it also lists labelled domains the node does not currently observe. That is
how an operator sees a label they applied before the producer came up (§10.7). Domains this node
replicates *into* are listed like any other, because a domain is a place, not a direction
(§10.6); each flow says whether this node is the one writing it.

Two properties of this join are the read side of §10.7's rule that such a label is "accepted and
inert":

- **It carries the same `settling` flag as `GET /v1/paths`** (§7.3). The join depends on
  inventory. During settling it would otherwise show every label with no observed domain beside
  it, which looks the same as the labels having been lost.
- **It answers for a node that has no registration at all**, listing only the label records. A
  label write does not check its node against the fleet (§10.7). If this read refused unknown
  nodes, a typo in a manifest's node name would produce a record that can be written but never
  read back, so the operator would have no place to notice it.

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
      "domain": { "name": { "area": "media", "elements": ["cameras"] } },
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

`namespace` is a **real property of the request, not a label** (§9.3). If the body names a
namespace, it must match the `{ns}` in the URL; a mismatch is refused.

`sources[].domain` is a selector, not a name (§10.7). `{"name": {"area": "media", "elements":
["cameras"]}}` addresses one domain directly (a manifest writes this as `media/cameras`), and
`{"labels": {…}}` matches domains by label. A source may name any domain, including one that
another request replicates into; that is how a chain `A→B→C` is written (§10.6). A *label*
selector, on the other hand, never matches a flow this project is itself writing (§10.7).

`destinations[].domain` is an **area name and a list of path elements**, not a path.
`{"area": "fast", "elements": ["studio-a","cam1"]}` materialises `<fast>/studio-a/cam1` and renders
as `fast/studio-a/cam1`. A manifest writes it as `domain: fast/studio-a/cam1` and the CLI splits
the string. **Nothing else in the system ever parses a domain string** (§10.6). *This supersedes a
separate `root:` field, which could be omitted on a node that advertised exactly one root.* The
area is now part of the domain's name, so omitting it would mean omitting half the name.

**A request's ID is `(namespace, name)`.** `POST` is create-or-**update**. A controller that
re-reconciles with a changed spec expects the change to be applied, not a 409. Posting a spec
identical to the stored one returns the existing request and writes nothing. A separately derived
ID would need a name index, which is one more key and one more thing to keep consistent, and it
would gain nothing because the name is already a required idempotency key. The server validates
the name more strictly than the wire type does (letters, digits, `-_.:`), because the name appears
in URLs and store keys.

**Settled: names are scoped to the namespace, not fleet-wide.** A namespace that does not scope
names does only half of what a namespace is for. The main consumer this partition exists for shows
why. A Kubernetes adapter that names requests after pods inherits Kubernetes' own namespacing, so
two pods with the same name in two Kubernetes namespaces would collide here unless the adapter
added a prefix, and removing the need for such prefixes is what a namespace is for. Two operators
who both want a request called `cam1` is the same case without the automation. The cost is that
every request ID in a URL, a CLI argument or a UI key has two components. Nothing further down is
affected, because path identity does not include the request (§5.4).

The response carries an `X-Mxl-Outcome` header with the value `created`, `updated` or `unchanged`.
It is a header rather than a body field because it describes the *operation*, not the resource:
the response body is byte-identical whether the write happened or was skipped. The client cannot
work the outcome out for itself, since the response echoes the spec it sent, and comparing the two
only shows that the server accepted it. The status code cannot carry it either, because a skipped
unchanged write is still a 200 (a new request is a 201).

**Re-applying an unchanged request must not write.** Desired state is assumed to change rarely
(§8.3). A controller re-applying on every resync would break that assumption if each identical spec
bumped the store revision and triggered a reconcile.

#### Many sources, many destinations

**Settled: a request fans in as well as out. Both ends are lists, and each source's node is still
named explicitly.** The expansion is the cross product: every source's flows against every
destination. `sources` is a list in the model, on the wire and in the manifest, and there is no
singular form.

*This supersedes the rule "a request fans out, not in", under which only the destination side was
a list and the asymmetry was called deliberate.* The old reasoning is kept below because it was
four separate arguments. Three of them still hold, but now as requirements the design has to meet;
the rest of this subsection and §11 meet them. The fourth now points the other way. The superseded
position was:

> **The source side already has a selector and the destination side cannot have one.** A
> destination is necessarily a `(node, domain)` pair. A *source* list would only add the ability
> to mix several origins in one request, on top of an expansion mechanism that already exists. A
> destination list adds something that otherwise cannot be said in one request: one camera going
> to two edges and an archive.
>
> **Fan-out has shared fate; fan-in has none.** §11 aggregates a request's status over its paths.
> Every path in a fan-out request shares one source, so an idle producer moves them all to
> `PAUSED` together, and a republished source flow invalidates them all together. The aggregate
> therefore answers a question an operator actually asks. Several unrelated sources landing in one
> domain share nothing.
>
> **Fan-in leads to the corruption case.** §7.2 rejects a destination `(node, domain)` that
> already holds a flow ID from a different source, which would be two producers writing one ring
> buffer. Grouping many sources onto one destination makes that easy to write by accident.
> Fan-out cannot produce it.
>
> **The source is the verbose half**, and sharing it across the destination list makes the cost
> visible: one source to five destinations is five initiator workers reading the same local flow
> and 5× egress on that node, which is the grouping the §13 bandwidth hook wants to see.
>
> Fan-in is still possible: several requests sharing a destination domain express it, and the
> refcount materialises that domain once (§10.6).

**The first argument is about which side carries the list, and it does not cover several nodes.**
A source's domain selector expands over the domains of **one** node, because `source.node` is
fixed and there are no node labels (§10.8). So what a source list adds is what no selector
can express: several *nodes* feeding one destination. "Every camera in studio A, studio B and
studio C onto the ingest wall" is one intent with one name, one lifecycle and one delete. Under the
old rule it was three requests that an operator had to keep in step by hand.

**Part of the first argument survives: sources stay enumerated.** Each source names its node
explicitly, so both ends of every pairing are written down. That is what keeps every code in
§7.2's "Refuses the `POST`" list decidable at `POST`: "studio-c and archive-01 share no
fabric" names two things the author typed. If a source's node could be a selector, those checks
would become per-path checks, and the request's cost could no longer be read off when it is
written. That is §10.8's territory and this design does not go there.

**The fourth argument now points the other way.** Fan-out grouped egress: one source to five
destinations is five initiator workers reading one local flow and 5× egress on the source node.
Fan-in groups **ingress**: twelve sources into one domain is twelve target workers and 12× ingress
on the destination node. The cost is just as visible in the other direction, and for the
arrangement that motivates fan-in, it is the direction that limits capacity, since an ingest wall
is bounded by what its edge node can take in. §13's bandwidth hook needs both groupings; before,
it had only one.

**The second and third arguments survive as requirements**, and the design meets both:

- **Shared fate is gone**, as the old text said. A request's paths no longer share a producer, so
  one dark camera among twelve would leave the aggregate permanently non-`ACTIVE`, and the summary
  line would stop answering the question it exists to answer. §11 adds the state **`PARTIAL`** for
  this, plus a per-source breakdown. This is a new state name and a new rendering, not a new
  object. It is also not specific to fan-in: a group-hint request with three flows, one of them
  paused, has always been in this state, and until now reported it as `PAUSED`.
- **The corruption case is real and becomes easy to write.** Two sources whose flows share one ID,
  into one destination domain, means two initiators writing one ring buffer. Where this is
  decidable from the request body, because two sources pin the same flow UUID and share a
  destination, it is refused at `POST` as `duplicate_source_flow` (§7.2). Where it is not
  decidable, because one or both sources use a selector and the collision only appears once the
  fleet produces it, it is `flow_conflict` on the path, resolved by §7.5 and torn down there.
  Fan-out could produce neither; fan-in can produce both. `duplicate_source_flow` is the only new
  reason code this change adds.

Nothing below the request changes. A path is still `(src flow address) → (dst node, dst domain)`,
sessions are still refcounted per path, and two requests that land on one path still share one
worker pair. That is why this is a change to requests, not to the reconciler. Writing fan-in as
several requests that share a destination domain still works and still refcounts to one session
(§10.6). What a single request adds is one unit of intent covering the whole set.

**Validation and negotiation are per `(source, destination)` pair.** They always were in
substance, because an interface config is negotiated for a session and a session has two ends
(§10.3). They looked per-destination only because there was one source. With lists on both ends, a
request can be viable for eleven pairings and refused for the twelfth, and the reason has to name
**both** ends. A failure common to every destination of one source belongs to that source. A
failure common to every source of one destination belongs to that destination. A failure that
applies to every pairing belongs to the request. Naming the wrong end sends the operator to the
wrong node.

A destination may carry its own `provider` pin, which overrides the request-level one. The provider
is negotiated per session, so per `(source, destination)` pair. With a list on either end, one pin
can be right for one pairing and *unsatisfiable* for another (§10.3). `idle_teardown_ms` and
`sched_prio` stay request-level. They either degrade or are rejected with a reason naming the node,
so the right fix is to split the request, not to add an override that hides a node's missing
capability.

**The pin stays on the destination; sources do not get a matching one.** The reason is the same
one that put it on the destination: a provider is unsatisfiable per *pairing*, and the side that
varies is whichever end the operator listed several of. One override on one side already says
everything a pin needs to say about a pairing. A second override on the other side would make
"verbs here, tcp there" ambiguous about which end wins.

`sched_prio` now has more nodes on which it can be unavailable. It is checked on every source and
every destination the request names. The rejection names the node that lacks it, not the pairing,
because the capability belongs to the host (§10.2).

#### Parking a leg: `disabled`

**Settled: a destination entry carries a `disabled` flag, and that is the only place in a request
where *off* can be written. There is no such flag on the request and none on a source.**

Before this flag, desired state had no way to say *off*. A route that was not running was a route
that did not exist. Taking a leg out of service for a maintenance window meant deleting it and
typing it back afterwards, and in between, the spec that someone wrote and someone reviewed was
gone. This is not only a UI problem, though `ui.md` §7a is where it hurts most. A manifest edited
down to park a route overnight and edited back in the morning has a history that no longer
describes intent, and `--prune` cancels whatever the edited-down file no longer names.

```json
{ "node": "edge-02", "domain": {"area": "fast", "elements": ["ingest"]}, "disabled": true }
```

The effective spec is **every source against every *enabled* destination**. A disabled entry is
skipped before expansion, so it produces no pairing, no path, no session and no assignment. Nothing
below the request changes, for the same reason as with fan-in: this changes the arithmetic of the
cross product and adds no new object.

**Spelled `disabled`, never `enabled`.** A boolean that defaults to true is a trap in a wire format
where `omitempty` drops zero values. If a request is ever round-tripped through a marshaller that
does not know the field, an `enabled` flag would be dropped and every leg in the fleet would stop,
and an absent flag could not be told apart from a deliberate `false`. `disabled` has its zero value
on the side that keeps media running. §6.3 applies the same reasoning to a rate of `0`.

**Why on the destination and not on the request.** Because it makes an existing operation
non-destructive instead of inventing a new one. Validation, negotiation and the path are all per
`(source, destination)` pairing (above, §7.2). The documented way to stop one leg of a request is
to remove the destination, which clears that column across all of the request's sources.
Disabling is that same operation, except that the request keeps its shape. The relationship runs
only one way, which decides the question: a request whose destinations are all disabled asks for
nothing, so a request-level flag can be derived from the destination flags, but the destination
flags cannot be derived from a request-level one.

The cost is an asymmetry with `sources`, reintroduced on purpose after the previous subsection
removed one. `provider` has the same asymmetry, for the same reason: what varies is whichever end
the operator listed several of, and a flag on one end says everything about a pairing, because
disabling either end of a pairing stops it.

**What a source flag would add, and why it is not built.** It would add one thing: disabling one
row of a fan-in ("studio-b is down for the week, keep it in the request"). Today that means
removing the source and typing it back later. A single-source request needs no source flag,
because disabling all its destinations turns it off entirely. A source flag would be narrow and
additive, and adding it later would not reopen any of this. It is recorded here so its absence is
a decision, not an oversight.

**A flag on the *pairing* is refused outright.** `disabled` on a `(source, destination)` cell would
mean a request is no longer sources × destinations but an arbitrary bitmap over the grid. The
expansion cannot describe that shape, a manifest could only write it as a matrix, and a round-trip
could not preserve it. A disabled destination turns off a whole column of the request, and a
disabled source would turn off a whole row; both keep the grid rectangular. `ui.md` §7a states the
same rule from the renderer's side and calls it a notch.

Four things that deliberately do **not** change:

- **`Validate` counts entries, not enabled entries.** At least one source and at least one
  destination are still required. A request with a single destination that is disabled is legal;
  that is the state the flag exists to represent. Requiring an enabled destination would forbid
  it.
- **The duplicate-endpoint rule applies to disabled entries too.** Two entries naming one
  `(node, domain)` are still refused, even if one of them is off. Parking a `tcp` variant of a
  destination next to the live `verbs` one looks useful, but there is no answer to which pin
  applies once both are enabled, which is why the rule exists.
- **`DELETE` is unchanged and still needed.** Disabling is not a soft delete. The request still
  exists, still holds its name, still counts against its namespace, and is still pruned by a file
  that does not name it. Removing intent still means deleting the request.
- **Disabling stops media.** It cancels those legs in every respect except that the spec is kept.
  So it has the same blast radius as a cancellation (for each path, whether another request still
  references it, via `path.requests[]`), and `?dry_run=true` previews it the same way. A cheap
  write whose effect is a teardown is the same risk §10.7 warns about for labels.

Whether an unchanged spec gets written is decided as before: `SameAs` compares the encoded JSON, so
flipping the flag is an `updated`, and re-posting the same value writes nothing.

**A request with no enabled destination is not validated against the fleet.** It asks for
nothing, so there is nothing to refuse. `unknown_area`, `no_shared_fabric` and the other fleet
checks are reported once a destination is enabled, and `?dry_run=true` lets an operator find out
before enabling it. Structural validation always runs, so a malformed domain name cannot be parked
in the store only to fail later. The cost is accepted: a parked route can be broken without anyone
being told. The alternative is worse, because a request that is both `INVALID` and `DISABLED` would
have two states and only one field to report them in.

#### The source is a selector, not a flow ID

**Settled: `sources[].select` is an extensible selector.** A pinned flow ID is one kind of
selector, not the only thing the API can express.

A UUID is rarely what a user means. An operator means "whatever camera 1 is publishing"; the
Kubernetes adapter means "everything this pod exposes". If requests had to pin UUIDs, both would
need their own discovery loop and would have to rewrite requests whenever a producer republished a
flow under a new ID. The server already does that discovery work.

Three selector kinds:

```json
"select": { "flow": "<uuid>" }
"select": { "group_hint": { "name": "Studio A:Camera 1", "type": "video" } }
"select": { "all": true }
```

The group hint costs nothing extra: it is an NMOS tag in `flow_def.json`, and mxl-utils parses
`urn:x-nmos:tag:grouphint/v1.0` into `Name` and `Type`. The agent reports it as part of inventory
and the server matches on it. `type` is optional. Without it, the selector matches every flow with
that name, which is how a camera's video and audio are replicated together.

#### `all`, and why the manifest may spell it by omission

**Settled: `all` selects every flow in a source's domain, and in a manifest it is written by
leaving out the flow selector.** A source that names a domain and nothing else replicates every
flow in it:

```yaml
sources:
  - {node: studio-b, domain: media/cameras}
```

This is the retired proxy's subscription shape (§16), and it should be the short thing to write,
not the long one. It expands like a group hint rather than like a pinned flow: it covers whatever
is currently observed, and if nothing matches, the request has zero paths.

**On the wire, `all` is a kind like any other, and a missing `select` is still an error.** The
tagged union exists to stop a zero value from meaning "everything". If it did, a hand-written
`POST` with a mistyped key, or a record stored before this kind existed, would decode as
*replicate the entire domain* instead of failing. `internal/api/selector.go` already states which
way this must fail: an unknown kind is refused, not ignored, because ignoring it would silently
*widen* the selection, and for a system that moves uncompressed video between hosts, selecting too
much is the worse error. The manifest may default to `all` because an unrecognised key is an error
there (§9.1), so a typo cannot fall through to the default. Nothing else may default to it.

**This looks like the empty label selector that §10.7 refuses, so the difference needs stating.**
§10.7 refuses `domain: {}` on similar grounds: it matches everything, and it can be reached by
leaving something out rather than by intent. But the two select different things. An empty
*domain* selector widens the set of **places**: it matches domains that do not exist yet, on
whatever that node happens to hold, so its scope has no bound and grows by itself. `all` selects
within **one place the operator already named**. Its contents can be checked before the request is
written (`describe domain`), and it cannot grow beyond that domain. Only a selector over places can
expand without anyone touching it.

The accident that the comparison points to is real, though. Deleting a `group_hint:` line turns a
one-camera request into a whole-domain request, and the outcome header (`created` / `updated` /
`unchanged`) does not show that the expansion went from one path to forty. **So `apply` prints
each request's resulting path count.** This costs nothing, because the `POST` response already
carries the expansion, and it follows the same argument as `label` printing a blast radius
(below). A hard cap on a request's path count remains a §19 item, tied to bandwidth admission
control; a count the operator can see is the cheap part of that.

Design rules that keep the selector extensible:

- **The selector is a tagged union with exactly one kind set**, not a set of optional fields that
  are implicitly ANDed. Adding `label`, `format` or `regexp` kinds later is then purely additive
  and cannot change the meaning of an existing request.
- **A request owns a set of paths, not one path.** A selector expands to N paths, and N changes as
  flows appear and disappear. A pinned-flow request is modelled as a set of size one; otherwise
  selectors would need a second concept alongside the first.
- **Expansion is part of reconciliation** and is recomputed from inventory like everything else in
  §4. A new flow that matches a selector creates a path; a flow that disappears removes it, except
  while its node is not live, when the frozen state applies (§4.2). This fits `WAITING` with no
  extra work: a selector that matches nothing is just a request with zero paths.
- **Refcounting happens at the path level.** Two requests whose selectors expand onto the same path
  share one session.

A request's status is therefore an aggregate over its paths. The API returns the summary, the
per-path breakdown and, since both ends are lists, a **per-source** breakdown. "1 of 3 active" is
what an operator needs to know, and it means nothing in a one-flow-per-request model. "studio-c is
dark, the other two studios are fine" is what they need from a fan-in, and it means nothing in a
one-source model. The aggregate is `PARTIAL` whenever the paths disagree and at least one is
`ACTIVE` (§11).

**The status also lists what the expansion *excluded*.** A path that does not exist has no status
to carry a reason, so a flow that a selector skipped is invisible if only paths are shown. §10.7's
self-output rule skips flows on purpose, on nodes that are also replication destinations, which is
where an operator's broad selector will run into it. Under the superseded rule, the whole
domain was left out, which was at least visible as a category. Skipping individual flows is
finer-grained and so *less* visible, and this list makes up for that. Each entry names
`(node, domain, flow)` and a reason. There is one reason today: `self_output`, a flow that this
node's own target worker is writing (§10.6). "Did not match the labels" is not a reason and is
never listed, because that set is unbounded and is the normal case. The list is capped, and a
truncated list **reports how many entries it dropped**, because a silent cap would read as
"nothing else was excluded".

#### Requests are normally authored as a manifest

The HTTP API above is the contract, but operators do not normally use it directly. They write the
desired set as a multi-document YAML file, one object per document, separated by `---`, and apply
it:

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

The read verbs each have one job, with no overlap:

- `status` counts the fleet and names only what is not active.
- `get` lists things, so a name can be found.
- `describe` explains one thing in full. It takes the nouns of §3, and keeps `path` and `session`
  separate because they are separate layers (§4): a path is derived state that outlives any
  particular session, while a session is short-lived and is re-established whenever either end
  restarts.
- `events` and `logs` answer *what happened to it*, where the other three answer *what is this
  now* (§12.1, §12.2).

**`events` and `logs` are verbs, not flags on `describe`**, for the same reason `get` and
`describe` are separate verbs. `describe` answers *what is this* and `events` answers *what
happened to it*. The first is a record and the second is a growing list, and an operator asks the
second repeatedly while the first stays the same. They are combined where it helps: `describe path`
prints the last few events under the status, because §12.1 starts from the observation that a
state and a reason alone do not explain a failure.

**`label` is a separate verb but not a separate vocabulary.** §19 dropped a separate `xpt` CLI
because two ways of writing one thing are worse than one, and that argument has to be answered
here. `label` writes to `POST /v1/nodes/{node}/domains`, the same endpoint a `kind: domain`
document applies to, so there is one model and one server-side rule. Only the gesture differs: the
manifest is the desired set an operator keeps in git, while `label` is a one-off edit of one record.
"An operator notices a domain and names it" is an interactive action in a way that writing a file
is not. `key-` removes a key, following the convention operators already know.

**The two gestures send different bodies, and that difference is what implements the ownership rule
below.** An apply sends the full map it declares. An edit sends a patch: keys to set and keys to
remove, which is what `role=cameras role-` already expresses on the command line. The server merges
an apply against the keys the previous apply declared, and merges a patch against nothing.

*This supersedes "`label` is a thin client-side read-modify-write over the endpoint".* That
followed from the endpoint being a full-set write, and no longer follows now that it is not.
Sending a patch is better regardless: a read-modify-write on a shared record has a lost-update
race. Two operators labelling one domain between the same read and write silently lose one edit,
which is the failure this record's ownership rules are designed to prevent. A patch has no such
window.

**Settled: an apply owns the keys it declares. It sets them, removes keys it declared last time but
no longer declares, and leaves every other key alone. So an interactive `label` edit *does*
survive a later apply that does not mention it.**

*This supersedes "a `kind: domain` document replaces the whole label set for its `(node, domain)`".*
That rule came from consistency with the request `POST`: everything else in the manifest replaces,
so this should too. It is recorded because that consistency argument is the obvious one, and it is
wrong for a reason that is easy to miss. A request's spec has **one writer by construction**:
`(namespace, name)` is its ID, so the file that names it is the only thing that can mean anything
by it, and replacing the whole spec simply describes that. A domain's label map has no such owner.
§10.6 establishes that a domain is a shared place with no single owner (the multicast reading), and
the label map is the one record in this design where several writers on one key set is the
*expected* arrangement, not a collision to resolve. Replacing the whole set makes the last writer
win, which is what a shared object must not do.

The rule above is `kubectl apply`'s three-way merge, and it is adopted on purpose. The manifest
format is already close enough to a Kubernetes manifest that §19's adapter is a mechanical
conversion, and operators coming from Kubernetes have firm expectations about what `apply` does to
a field it never mentioned. Surprising them costs more than the consistency the old rule gave.
Mechanically, the rule needs one thing the record would not otherwise hold: the set of keys the
last apply declared. This is what `last-applied-configuration` does in Kubernetes, here applied to
a flat map rather than a whole object.

Three consequences, none of which the replace rule had:

- **Removing a key from the file removes it.** The file stays declarative over its own keys. A
  merge without a record of what was previously declared would lose this.
- **`label` and `apply` do not fight, because they own different keys.** So the verb is more than
  a way to explore: an operator can name a domain interactively and keep that name, in a fleet
  whose requests someone else applies from git. That is how this project is actually used, and the
  old rule made it impossible.
- **Two files naming one domain still fight**, because there is one declared-key set, not one per
  writer. This is `kubectl apply`'s own limitation from before server-side apply, and it fails the
  same way: the second apply's record replaces the first's. The fix, if it is ever needed, is named
  field managers, which would be additive. Not built; recorded so nobody has to discover it.

Scoping is unchanged. It is a *different* rule that sounds similar, so it is restated here: a file
with three `kind: domain` documents touches three records and leaves every other domain in the
fleet alone. `--prune` does not extend to labels, and no other mechanism does (see below). Since an
apply now removes its own retired keys, there is nothing left for a prune to do for labels anyway.

**Label writes accept `--dry-run`, for the same reason requests do.** A label adds a domain to, or
removes it from, a request's expansion, so it starts and stops media just as a request does. It
does so one step removed, which makes it *easier* to do by accident. Removing `role=cameras` from a
domain that five requests select can tear down running sessions, and a verb built for quick
interactive edits is the worst place for that to happen without a preview. `?dry_run=true` runs the
same `Compute` against a candidate fleet and returns the paths that would appear and disappear, as
it does for a request.

The real write prints the same information, not only the dry run: for each path that would stop,
whether another request still references it. `path.requests[]` already answers that, so nothing new
is computed. It is a **blast-radius report, not a confirmation prompt**. The same operators script
the CLI and use it interactively, and a verb that waits for a tty hangs in a pipeline. Note that
this is a label removing a path while the node is live, so nothing is frozen (§4.2) and the
teardown is immediate.

Manifests need no new server mechanism. `POST` is create-or-update on the name within a namespace,
and that pair is the request's ID, so posting every request a file names already is an apply.
`?dry_run=true` runs the same validation and reconciliation against a candidate fleet and returns
the outcome without writing. That is nearly free, because the accept path already builds a
candidate fleet and reconciles it in order to reject `INVALID` requests.

Properties of the format:

- **`kind:` names the object type and defaults to `request`.** *This supersedes "the file is
  deliberately not a Kubernetes manifest — no `apiVersion`, no `kind`".* That position already
  allowed for this change: an optional `kind:` that defaults to a request is additive and costs
  nothing when a second object type appears. Two appeared at once, `namespace` (§9.3) and `domain`
  (§10.7), which is the reason to add `kind:` deliberately, once. An unrecognised `kind` is an
  error, per the rule below. There is still no `apiVersion`: it would express nothing, and the
  format stays close enough to Kubernetes that the roadmapped Kubernetes adapter (§19) is a
  mechanical conversion.
- **Apply orders documents by kind (namespaces, then domains, then requests) regardless of their
  order in the file.** The end state does not depend on the order, because `Compute` is recomputed
  and namespaces are created automatically when first referenced (§9.3). The *intermediate* state
  does depend on it. A request applied before the namespace document that makes its namespace
  exclusive would be admitted and then invalidated, which looks as if the apply broke something.
- **The selector is flattened onto each source in the file.** `flow:` or `group_hint:` goes
  directly under a `sources:` entry, and `domain:` takes either a name or a label map. The wire type
  nests these under `select` and `domain.name` / `domain.labels`. The tagged-union rule still
  holds; "exactly one" becomes a validation check instead of a matter of syntax. The file is easier
  to write and the wire stays structured, which is the same approach the destination domain takes
  (§10.6). **Omitting the flow selector entirely means `select: {all: true}`**, and the CLI fills
  in that default, not the server (see `all` above).

  `sources:` is **always a list, with no singular `source:` form**, matching `destinations:`. A
  scalar-or-list spelling was possible (`provider:` uses exactly that) and is refused here for the
  reason §19 gives for dropping a second CLI: two ways of writing one thing are worse than one.
  Unlike `provider:`, where the singular form is rare, a singular `source:` would be the *common*
  case, so operators would need to know both forms, not just one. The cost is that every stored
  request and every manifest written with the old field name becomes invalid. This is accepted
  rather than mitigated: it ships with the domain re-identification of §10.6 in the same major
  version bump (§16).

  **A scalar `domain:` is a name and a map is a label set.** That is the whole disambiguation, and
  no marker key is needed: a name is `media/cameras`, a selector is `{role: cameras}`, and YAML
  already distinguishes the two. It also handles `domain: {}`, an empty map that would match every
  domain on the node: it is read as a label selector with no keys and refused as one, without
  needing a separate rule.
- **`disabled: true` on a destination parks that leg.** It is written the same way on the wire as
  in the file; it is the only field in a source or destination entry that needs no translation. A
  file is a natural place for it: "this route exists and is off" is a reviewable line in a diff.
  Before the flag, the same intent was an absence, which looked identical to never having written
  the route.

  **An apply that omits the flag enables the leg.** This is the one surprising case, and it is
  settled the simple way: the file is authoritative over the requests it names, as it is over
  every other field of them. So a leg parked interactively, through the API or from the matrix,
  comes back the next time someone applies the file that names its request. The alternative would
  be §10.7's declared-key merge, and it is refused here for the reason that section gives for using
  it there: a domain's label map has many writers by design, while a request's spec has one by
  construction. Three-way merging a request's own fields would gain nothing and would lose the
  property that makes `--prune` and `apply` understandable together. `--dry-run` reports the case
  as an `updated` request, with the paths that would appear, and that is the warning.
- **An unrecognised key is an error.** This is deliberately the opposite of the rule for
  `TargetInfo` (§5.2). There, an unknown field comes from an independently versioned upstream, and
  failing closed would stop replication after an unrelated upgrade. Here, a person writes the file
  against this binary, and a mistyped key that silently does nothing is the failure a declarative
  format is supposed to prevent.
- **`labels` identify requests; they are not only annotations.** They are passed into worker
  metrics as user labels (§12), and together with the namespace they scope `--prune`: a prune
  cancels matching requests that the file does not name. So `--prune` *requires* a scope. A
  fleet-wide prune would cancel anything created by another operator or by the Kubernetes adapter,
  and what it cancels is moving video. **A namespace is a better scope than a label selector**,
  because it is a declared partition rather than an ad-hoc tag, so `-n` is the main way to scope a
  prune and `-l` narrows within it. A dry run cancels nothing.

**`--prune` covers requests only.** It never removes a namespace and never removes a domain label,
even when the file contains documents of those kinds. Neither fits. A file naming three domain
labels would otherwise prune the other forty on the fleet, and pruning a namespace would be a
delete that §9.3 refuses anyway while requests reference it. Prune exists to make a file
authoritative over *intent*, and a domain label is a fact about a host.

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

**`events` is the only agent report that is a stream rather than a snapshot**, and that is why it
has its own endpoint. The agent compares `inventory` and `status` with what it last sent before
sending them (§6). An event folded into a compared snapshot would be dropped when it repeats and
re-sent forever when it does not. The agent drains the batch when it sends it, and delivery is
at-least-once: the server de-duplicates on the agent's per-event sequence number, rather than the
agent guaranteeing exactly-once delivery (§12.1).

**Settled: long-poll with a revision cursor, not server push.** The agent `GET`s its complete
assignment set, and the server holds the request until the revision advances or the wait expires.
This works through proxies, needs no sticky sessions, is easy to resume, and falls back to plain
polling if a proxy buffers the response. Falling back is acceptable, but hanging is not, so the
server caps `wait` below the idle timeout of any plausible intermediate proxy. Recovery latency is
at most one poll round trip, which is sub-second. That is why a peer learns of a new epoch quickly
enough to keep the glitch from an agent restart within the 1–2 s target in §6.1.

**Using a `GET` has a cost, and one header covers it.** Every response the server sends carries
`Cache-Control: no-store`. An assignment poll is a `GET` that looks cacheable and whose meaning is
entirely in its query string. An intermediary with an ordinary default cache policy would serve an
agent a set that was correct for some other revision; a CDN, for example, commonly applies a 24 h
TTL to a 200 response with no cache headers. **§4.2 does not protect against this.** Fail-static
protects an agent from getting *no answer*, not from getting a wrong answer successfully. The agent
would act on the stale set with full confidence. If that set carries an old epoch, the result is
§5.2's silent failure: an initiator running against rkeys that no longer exist, moving no data,
with everything reporting healthy. The header is set on the whole mux, not only on that route,
because no response from this server is ever cacheable, and a rule with no exceptions needs no
checking. A cached `/readyz`, for example, would have a load balancer routing to a replica that
reported it had not settled (§7.3).

`inventory` and `status` are **full snapshots**, not deltas. Deltas need sequence numbers, gap
detection and resync paths. Snapshots need none of that, and at realistic fleet sizes they are
small. This is the same level-triggered approach as §4.1.

The node name is URL-escaped in every per-node path. It is operator-assigned free-form text,
validated for uniqueness (§7.1) but not for URL safety, and a name containing a slash must still
address that node rather than a route that does not exist.

### 9.3 Namespaces

**Settled: a namespace is a first-class object in desired state, and `namespace` is a real property
of a request, not a reserved label.**

*This supersedes an earlier design in which a namespace was the value of a reserved `namespace`
label: the set of namespaces was derived from the distinct values across all requests, and a
namespace had no record of its own.* That design was justified by an existing CLI mechanism
(`--prune -l namespace=nab` already meant "make this namespace match this file"), not by the
model. It had two costs that outweighed what the mechanism saved. First, `namespace` was a legal
user label, so a label an operator added for their own reasons silently became a partition key.
Second, the manifest wanted a plain `namespace:` field while the wire wanted a label, so the two
spellings had to be reconciled, and a disagreement between them had to be refused because there was
no way to resolve it. A real property removes both problems.

A namespace holds a name, `paths` (below) and a description. Request-level defaults, such as a
provider pin, an idle teardown or a bandwidth budget once §13's admission control exists, are all
plausible later. They are easier to add than to take back, so v1 has none of them.

A namespace name is ASCII letters, digits, `-` and `_`, and must not be empty. It is more
constrained than an ordinary label value, which is free text, because it is used as a URL path
segment, a store key and a `-n` argument on the command line. The same reasoning is why a request
name is validated more strictly than the wire type requires (§9.1).

**A namespace partitions requests and nothing else**: not nodes, not domains, not destinations.
Two namespaces sending one flow into one destination domain is fan-in across requests, which §9.1
supports, and §10.6's refcount materialises the domain once. (A single request can also fan in,
but not across a namespace boundary, which is why this case is a question for namespaces rather
than for requests.) Forbidding it would make namespaces fully disjoint, at the cost of the
arrangement fan-in exists for.

#### Existence

**A namespace is auto-created on first reference, may be created explicitly, and is never
auto-deleted.** A request that names a namespace with no record creates it with defaults instead of
failing.

The alternative is to require that a namespace exist before a request may name it. That would put
an ordering dependency on the consumer this partition exists for: an adapter would need permission
to create namespaces, and a create-if-missing step before every request. Auto-creation avoids that
and still keeps the namespace set authoritative, which is what being a first-class object has to
mean if `GET /v1/namespaces` is to be complete. The same approach is used one level down: a
request that names no namespace has `default` *written into* it, rather than left implied, because
an implied value means different things depending on which record you are looking at.

**Settled: the create is eager. It is a real write, done as part of the request write and before
the request itself.** The alternative is to fill in missing namespaces lazily when something reads
the set. That would save one write, but would quietly give up what this object exists for: a
`GET /v1/namespaces` that invents rows is the old label design again, dressed up as a record. Four
consequences, each easy to get wrong:

- **Create if absent; never write if present.** An unconditional write would bump the namespace
  key's revision on every request write and wake every watcher in the fleet, which is the churn
  §8.3's sizing assumes away. This is the same no-write-if-unchanged rule the request itself
  follows, applied to one more key.
- **The namespace is written first, then the request.** In the reverse order, a failure between
  the two writes would leave a request referencing a namespace with no record, which is the
  state that makes the set non-authoritative. In this order, a failure leaves an empty
  namespace, which does nothing, looks the same as a deliberately empty one, and costs nothing. No
  transaction is needed, because the two failure modes are not equally bad.
- **`?dry_run=true` creates nothing.** A dry run writes nothing at all (§9.1), and the create
  happens in the write path, not in validation. It is stated here because a namespace create is the
  kind of side effect that gets attached to admission by accident.
- **An explicit namespace document still takes precedence**, because apply orders namespaces
  before requests (§9.1). A file that declares `paths: exclusive` and contains a request in that
  namespace writes the declaration first. A request that arrives on its own gets the defaults.
  The ordering rule and this rule are the same rule seen from two sides.

Deleting a namespace is refused while any request references it, and the error message gives the
count. The system never cancels intent on the user's behalf (§11), and a cascading delete here
would be a cascading teardown of live media. `default` cannot be deleted.

#### `paths: shared | exclusive`

In an `exclusive` namespace, no two requests may hold the same path. The losing request reports
`INVALID` with `namespace_overlap`, naming the request that holds the path. The winner is decided by
§7.5's precedence, and the path, held by the winner, keeps running. **The default is `shared`, and
there is no server flag to change it.**

**Settled: this rule protects legibility, not data integrity, which is why it is the only conflict
rule that is opt-in.** Overlap within a namespace costs nothing in the fleet: two requests that
expand onto one path share one path, one session and one worker pair, which is §9.1's refcounting
working as designed. Nothing is duplicated and nothing is corrupted. What overlap costs is accuracy
in a matrix view: two lit cells that are really one stream, counts that do not add up, and a cell
that goes dark on a click that stopped nothing. Those are real problems for a renderer, but not for
the fleet.

That gives the general principle, and this is the only rule on the optional side of it: **conflict
rules that protect data integrity are mandatory; conflict rules that protect legibility belong to
whoever is reading.** `flow_conflict` (§7.2), two initiators writing into one ring buffer, is the
mandatory kind and is never optional for anyone.

Consequences of these choices:

- **Per namespace, not per request.** A matrix needs the property to hold for the whole set it
  displays: one non-conforming request breaks every cell on the screen, and the renderer cannot
  stop the CLI from writing such a request. A per-request flag also has no sensible answer when an
  exclusive request meets a shareable one, because whichever loses is penalised for the other
  request's setting.
- **Default `shared`**, because refcounting is the base model and forbidding overlap is the special
  case, and because the party that needs the guarantee is the one able to ask for it. A third-party
  client should not have to know the rule exists in order not to be broken by it. If exclusive
  were applied everywhere, a Kubernetes adapter using the natural mapping of one request per pod
  would make one pod's status depend on another pod's existence, resolvable only by deleting
  something unrelated.
- **A shared namespace is also immune to overlap that appears without anyone writing anything.**
  Two selectors over one source domain can only start overlapping dynamically in one way: a pinned
  flow and a group hint, when a producer changes its tags. In an exclusive namespace, a producer's
  NMOS tagging could therefore flip a request to `INVALID`. An adapter has no way to handle that
  and should not need one.
- **A parked leg holds nothing, so it releases the path it held** (§9.1). A disabled destination
  produces no path, so it cannot hold a path against another request and cannot cause a
  `namespace_overlap` for anything else. What to know before relying on this: parking a leg lets
  another request claim its path, and re-enabling the leg then *loses*. That resolves like every
  other overlap: the loser reports `INVALID` and names the winner, and nothing stops.

  **The deciding factor is recency, not incumbency.** This needs saying because §7.5's order
  starts with incumbency and this rule does not use it. Two requests over one path map to the same
  path, whose session exists whichever request holds it, so incumbency cannot tell them apart and
  the decision falls to `(UpdatedAt, id)`. What makes the re-enabled leg lose reliably is that
  un-parking is a *write*: every request write is timestamped, so the request coming back has the
  newest `UpdatedAt` and sorts last among the contenders. The consequence is that a running session
  gives its holder no advantage here: a request with an older timestamp takes the path straight
  back from one that has been carrying it.

`default` stays `shared`, because it is the catch-all where hand-written manifests end up.

---

## 10. Capabilities, providers and addressing

Every worker must be configured with a `provider`. The local bind address depends on the
provider: an IP address for `tcp` and `verbs`, and a device address for `efa`, which in practice
is link-local.

### 10.1 Provider availability is not reachability

The obvious model is that each node declares `provider → address` and the server intersects the
provider sets of two nodes. That model is wrong, because two nodes offering the same provider may
still be unable to reach each other:

- Two nodes both offering `verbs` may be on different InfiniBand fabrics.
- Two nodes both offering `efa` may be in different VPCs or subnets. EFA also requires the
  security group to allow traffic to itself.
- Two nodes both offering `tcp` on RFC1918 addresses may have no route between them.

Intersecting provider names would therefore assign sessions that cannot connect, and the failure
is hard to diagnose: the target comes up cleanly, the initiator's connect loop keeps retrying, and
nothing says why.

**Nodes declare fabric attachments, not providers.** Each attachment is a
`(provider, fabric, address)` triple. `fabric` is an opaque label that the operator assigns:

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

Two nodes may pair on a provider **if and only if they share a fabric label for it**. The server
treats the label as an opaque string and only compares labels for equality. It keeps no topology
database, does no reachability probing and infers nothing. This matches how these networks are
provisioned in practice: the operator already knows which HCA is on which fabric.

`shm` can only connect two domains on the same node. Its fabric label is derived from the node
name, so two `shm` attachments share a label only if they are on the same node, and no special case
is needed. One exported function derives the label (`api.SHMFabric`, which yields
`shm:<node>`), and the server canonicalises the label it stores. An agent that spelled the label
some other way therefore still matches itself, instead of silently failing to pair two domains on
its own node.

**A node with no fabric attachments configured gets `shm`.** Without any attachment the node
could do nothing, so the only alternative would be to refuse to start. That would break
`mxl-replicator run` with no arguments, which is the single-host and development case that §2.2
exists to serve. The `shm` attachment is a real assumption, not a placeholder: it is
same-node-only, has no address, and takes its label from the node name, so replicating between two
domains on one host works out of the box. The agent logs a warning at startup, so a node that was
meant to reach other hosts reports what it is missing.

#### Detecting an attachment instead, and why it is opt-in

**Settled: `--agent-detect-default-fabric` picks a single attachment out of the probe and labels it
`default`.** This is the other possible answer to "this node configured no attachments". It
replaces the `shm` assumption above; it does not add to it. It is a flag rather than the default
behaviour because of what the label means (see below).

The selection uses the server's own preference order (§10.4): EFA, then Verbs, then TCP, then SHM.
It takes the first provider that has an entry another node could plausibly reach. That is the
choice negotiation would have made if every node offered everything, so the guess is consistent
with the rest of the system and cannot surprise anyone who knows the preference order. Within a
provider it takes the first usable entry in probe order and logs the entries it skipped. If a node
has two usable entries for a provider, the operator has to choose between them, and §10.1's rule
for ambiguous selectorless attachments already covers that.

**Only `tcp` filters addresses, because it is the only provider that needs to.** A `verbs` or
`efa` address is derived from the hardware, and each device has one sensible answer. A host
typically has half a dozen `tcp` addresses, and all but one are wrong. Detection skips:

- **loopback**, which can only pair with itself;
- **CGNAT** (100.64.0.0/10), which is what a Kubernetes CNI hands out. It is routable inside its
  own scope but never between the scopes a fleet spans, and it is the address most likely to be
  listed *first* on the deployments most likely to use this flag;
- **link-local** and the unspecified address, which are never a deliberate data path;
- **IPv6**, because a link-local v6 address needs a zone index the peer cannot use, and a ULA is
  as private as CGNAT. An operator with a working v6 fabric can configure it explicitly.

RFC1918 addresses are accepted. §10.1 already argues that reachability between two private
addresses cannot be decided here; the fabric label exists to state it.

**The label is why detection is opt-in.** `default` is an ordinary fabric label, and the server
only compares it for equality. Detection therefore pairs a node with every other node that also
detected, and with nothing else. Two nodes on different networks will both call their attachment
`default` and be paired, which produces the exact failure this section exists to prevent: a target
that comes up cleanly and an initiator whose connect loop retries with no explanation. If detection
were the default, every flat-network deployment would reintroduce that failure without anyone
choosing it. As a flag, it is the operator stating "my fleet is one network", which is a claim the
operator is entitled to make and the server cannot check.

Two further consequences, neither visible from the flag itself:

- **Detection re-runs on every re-registration**, because that is when the probe runs (§10.2,
  §10.5). If a node's tcp address changes, the node re-detects onto the new address and every
  session through it re-establishes. With an explicitly configured address, the operator would
  instead have seen the attachment dropped and the reason logged. Explicit configuration remains
  the better choice wherever it is possible.
- **A configured attachment is never joined by a detected one.** Detection is a fallback, and a
  fallback that is not used is not a configuration error. So the flag combined with a `fabrics:`
  block is ignored with a warning rather than refused at parse time. This combination arises from
  layered configuration, and refusing it would break a node whose configuration is correct.

#### Joining configuration to the probe

The agent matches each configured attachment against the entries the worker probe reports (§10.5)
using **selectors**.

**Settled: join selectors come in two classes — *naming* and *narrowing* — and "none" is the
common naming one.** *This supersedes an earlier rule, "prefer naming an `interface` over an
`address`", which cannot be implemented on `verbs`/`efa` and in fact points the wrong way.* That
rule was justified with EFA: EFA addresses are link-local and hardware-derived, and nobody wants to
pin them in configuration. But `interface:` is the one selector that cannot work for EFA. The
probe's only interface-like field is a libfabric device name (`rdmap0s6-rdm`), not a netdev name
like `efa0` (§10.5). `interface:` works only for `tcp`, the one provider where naming an address
was never a problem, and under the old rule an EFA operator would have had to pin the very address
the rule was meant to avoid.

**Naming selectors choose which interface. At most one per attachment**, because two names would
need a rule for combining them, and asking the operator which one they meant is always better than
any such rule:

| Configured | Matched against | Works for |
|---|---|---|
| `address:` | probe `node`, exactly | all providers |
| `interface:` | the netdev's addresses, resolved locally with `net.Interfaces()`, against probe `node` | `tcp`, `verbs` |
| `device:` | probe `attr.device_name`, exactly | wherever the library reports one |
| *nothing* | the provider alone, which **must** match exactly one probe entry | the common case |

The fourth row is how `efa` and `shm` are resolved, and it is better than name matching, not a
fallback from it. A node has one EFA device and one `shm`, so
`{provider: efa, fabric: vpc1-subnet-a}` is unambiguous and puts no hardware-derived string in the
config file at all. When the provider alone *is* ambiguous, the agent does not guess. It refuses
the attachment and logs every candidate, which gives the operator the exact strings they could
write. This serves §10.5's distinction between "this node has no verbs" and "someone mistyped
`ib0`" better than a failed name match would.

**Narrowing selectors choose which of that interface's addresses counts. Any number may be
used.** They are combined (AND) with the naming selector and with each other, and the rule that
exactly one probe entry must survive applies to the combination:

| Configured | Matched against | Works for |
|---|---|---|
| `network:` | probe `node` parsed, tested for containment in a CIDR prefix | IP addresses |
| `ip_version:` | probe `node` parsed, `4` or `6` | IP addresses |

*This supersedes an earlier rule, "at most one selector per configured attachment".* The naming
class keeps that rule; it was never right for the whole set. Naming a thing and narrowing what
counts as that thing are different operations. The case that forces the distinction is a
**DaemonSet**, where every value is the same on every node, so `address:` cannot be used.
`device: mlx5_0` is the fleet-wide string the operator has, and it is ambiguous by construction:
the probe prints one entry per `(interface, address, provider)` (§10.5). An HCA that carries an
IPv4 address and a link-local IPv6 address shows up as two entries under one device name, so the
attachment would be dropped. Before narrowing selectors existed, the only fix was a per-node
`address:`, which meant a per-node overlay just to express a fact that is true for the whole fleet
("we use v4"). `device: mlx5_0` plus `ip_version: 4` states it once.

`network:` goes one step further. It needs neither a per-node value nor a hardware-derived string:
it picks each node's own address inside a prefix. On a fleet whose nodes are alike but name their
interfaces differently (`eth1` on one, `ens5f0` on another), it is the only selector that is both
exact and identical on every node, and it is usually the right choice for `tcp`.

**Neither narrowing selector says anything about reachability, and neither may be treated as if it
did.** §10.1's argument is that two nodes inside one RFC1918 prefix may have no route between them,
so `network:` choosing an address from a list does not assert that the address is reachable from
anywhere. Only the fabric label makes that claim. This is also why `network:` is not a fabric label
in disguise: treating it as one would require the server to compare two nodes' prefixes, which is
the kind of topology reasoning this section keeps out of the server.

Two further consequences:

- **An address that is not an IP matches no narrowing selector.** `shm` reports the hostname, and a
  provider may report a device address in any format. This is not an error: such an address has no
  IP version and is in no prefix. It is why the narrowing class is documented as working for IP
  addresses rather than for particular providers.
- **An `ip_version` that contradicts the address family of `network` is refused at parse time**,
  because a prefix already implies a family. If it were left to the join, it would match nothing on
  any node, and the operator would see the whole fleet dropping an attachment instead of a typo.

### 10.2 What a node advertises

**The agent advertises only what it has verified.** Raw kernel capabilities — `CAP_IPC_LOCK`,
`/dev/infiniband`, `RLIMIT_MEMLOCK`, `CAP_SYS_NICE`, `RLIMIT_RTPRIO` — are never sent to the server.
They decide whether a provider works at all, so the agent uses them to decide whether to
*advertise the attachment in the first place*. The server never has to reason about them.
(`sched_prio` is available if `RLIMIT_RTPRIO` **or** `CapEff` allows it; either is enough,
because a container commonly has one without the other.)

This gives a clear test for what belongs in registration: **something is a capability if and only
if the server would make a wrong decision without it.** Everything else is agent configuration or a
local precondition.

| Advertised | Why the server needs it |
|---|---|
| Fabric attachments, each with provider, label, address, caps flags, `maxMessageSize` | Negotiation (§10.3) |
| `mxl` and `libfabric` versions | Cross-node compatibility — see below |
| Replicator build version and **protocol version** | Version skew (§13.1) |
| `sched_prio` available | Request-time validation |
| Areas, by name, path and grants | Request validation, destination resolution, and rendering a domain's name (§10.6) |
| Port range | Diagnostics only |

**Readable areas are advertised, and they meet the test above.** *An earlier version did not
advertise search paths (the predecessor of readable areas). The reasoning was correct at the time:
the server made no wrong decision without them. The only argument for sending them was diagnostic
— a label applied outside every search path has no effect, and nothing could explain why. That was
left open as a question of whether §10.2's rule should bend for a diagnostic message.* It no longer
needs to. A domain's name is `<area>/<elements>` (§10.6), so a server without the area table cannot
render a domain's identity, resolve a domain name in a request, or tell an operator which area a
label fell outside of. The grants are sent with each area because "may this domain be a
destination" is checked at request time.

**The mxl/libfabric version pair needs explanation.** `target_info` is produced by one node's
mxl-fabrics and consumed by another's. A node pair that straddles an mxl version boundary is
therefore a compatibility problem that *neither agent can detect on its own*. The worker prints
`proxy`, `mxl` and `libfabric` versions with `-v`, and the agent already runs that at startup.

Capabilities are **static**: they are sent at registration and change only by re-registering. If an
attachment disappears, the agent re-registers. Re-registration cancels the session's control-plane
loops but leaves every worker running as it is (§4.2).

### 10.3 Negotiation

The library does **not** negotiate. From `FabricsDeveloperGuide.md`: *"Both sides (target and
initiator) must receive the same capabilities and maximum message size to be compatible. There is
no internal negotiation. Typically you would serialize the selected `mxlFabricsInterfaceConfig`
alongside the `mxlFabricsTargetInfo` and share both with the initiator through your out-of-band
signalling channel."*

**This project is that out-of-band channel.** Negotiation is the server's job. It is more than
choosing a provider name: the server must agree the full interface config for both ends.

1. **Match fabrics.** Candidate attachment pairs are those that share a `(provider, fabric)`.
2. **Apply the pin or the preference order** (§10.4).
3. **Agree capabilities.** The caps flag set is the *intersection* of both sides' flags, and
   `maxMessageSize` is the *minimum* of the two. At least one of `REMOTE_WRITE` or `SEND_RECEIVE`
   must be in the intersection; if neither is, the pair is not viable. A capability name this build
   does not recognise is passed through rather than dropped, so the intersection stays correct
   across a version boundary.
4. **Assign the agreed config to both ends**, not just one.

If no viable pair exists, the request is `INVALID`, with a reason saying which step failed:
`no_shared_fabric`, `no_shared_provider` or `no_shared_capability`. Each points the operator at a
different problem, so the message distinguishes them.

Negotiation is deterministic: the same fleet produces the same result on every replica and every
pass.

### 10.4 No silent downgrade

**Settled: an explicit provider is honoured or the request fails. It is never substituted.**

An operator who asks for `verbs` gets verbs. Falling back to `tcp` without saying so is not a
graceful degradation; it can be a large performance drop. A path that carried 1080p60 over verbs
may not keep up over tcp, and the resulting dropped grains look like a problem with the source, not
like a routing decision made on the operator's behalf.

With no pin, the default preference order is **EFA > Verbs > TCP > SHM**, which is the priority
the mxl demo tool uses. The order is configurable on the server with `--server-provider-order`
(default `efa,verbs,tcp,shm`). The agent's `--agent-detect-default-fabric` probe always uses the
built-in order (`api.DefaultProviderOrder`).

A pin and its acceptable fallbacks are one field. "Use only this" and "prefer this, but these are
acceptable" are the same mechanism:

```json
"provider": "verbs"              // verbs or fail
"provider": ["verbs", "tcp"]     // prefer verbs, tcp acceptable
                                 // omitted: the configured order
```

**The negotiated provider is fixed for the lifetime of the session.** If the fabric a session uses
goes away, the session goes `FAILED` / `fabric_gone` with a clear reason. It does *not* silently
re-negotiate onto a slower provider. Re-negotiating at 3am with no operator action would be the
same silent downgrade, only harder to notice. A path that has clearly failed triggers the
operator's actual failover procedure; a path that is struggling does not. The negotiated provider
is always reported in status.

### 10.5 Interface discovery: the worker probe

The agent cannot enumerate libfabric interfaces itself, because it is written in Go and does not
link the library. Guessing from `/dev/infiniband` and interface names would be a heuristic, and when
it guessed wrong the result would be a confusing restart loop. Instead, the worker has a probe mode:

```
mxl-replicator-worker --interfaces
```

The probe calls `mxlFabricsGetInterfaces()` and prints a JSON array on **stdout**, one object per
`(interface, address, provider)` combination. The same physical interface therefore appears several
times if it is reachable through several providers or carries several addresses. Each entry has
`provider`, `address.node`, `caps.flags` (`REMOTE_WRITE` / `SEND_RECEIVE` /
`BLOCKING_OPERATIONS`), `caps.maxMessageSize`, and a best-effort JSON `attr` blob.

Four properties of this output shape the design above:

- **There is no interface-name field.** The physical interface, when known, is in
  `attr.device_name`. For `tcp` that is the netdev name (`eth1`); for `verbs`/`efa` it is the
  *libfabric device* name (`mlx5_0`-style). That is why §10.1 has naming selectors, and, because one
  device name covers every address on the device, why it also has narrowing selectors.
- **`caps.maxMessageSize` is a real `uint64`**, and providers report `UINT64_MAX`. Decoding it
  through a `float64` at any step loses the value.
- **`shm` reports the hostname** as its node and has no `attr.device_name` at all. This is
  consistent with deriving its fabric label from the node name.
- **stdout is also the worker's log stream**, and libfabric writes its diagnostics there too. The
  probe therefore redirects stdout to stderr while probing and restores it before printing the
  result. The agent reads JSON from stdout and logs from stderr and must capture them separately.
  (`-v` and `-h` print to stderr, WRS §2; the probe prints to stdout because its output is data.)

The probe needs a domain directory to exist, so it creates a temporary one and removes it
afterwards; the agent does not have to supply a domain. The probe does not report a `service`. The
library does report one, but it is empty for every provider except `shm`, and for `shm` it belongs
to whichever process ran the probe. A later worker could not bind it, and carrying it forward would
have put a probe-specific value one rename away from the agent-allocated service of §7.4.

The agent joins the probe output against the configured `fabrics:` block and advertises only
attachments that appear in both. A configured attachment with no matching probe entry is a
configuration error. It is logged prominently at startup rather than silently dropped, because the
operator needs to tell "this node has no verbs" apart from "someone mistyped `ib0`". The probe
re-runs on re-registration, not on every heartbeat.

### 10.6 Domains, areas and grants

**Settled: a node declares *areas* — named directories, each granting reading, writing or both. A
domain is a directory inside an area, identified fleet-wide as `<area>/<elements>`, and there is
one kind of domain. Nodes declare areas, not domains — the same shape as §10.1, and for the same
reason.**

The three terms, in brief:

- An **area** is a directory on a node that the operator configures in the agent, with a short
  name and two independent permissions ("grants").
- A **grant** is `read` (this project may discover and observe domains under the directory) or
  `write` (it may create domains there and write flows into them).
- A **domain** is an MXL domain directory inside an area. Its fleet-wide name is the area name
  followed by the domain's path relative to the area.

A worked example. With this agent configuration:

```yaml
areas:
  - {name: media, path: /dev/shm/mxl,            read: true}
  - {name: fast,  path: /dev/shm/mxl/replicated, read: true, write: true}
  - {name: bulk,  path: /mnt/nvme/mxl,           read: true, write: true}
```

directories map to domain names like this:

| Directory | Domain name | Why |
|---|---|---|
| `/dev/shm/mxl/studio-a/cam1` | `media/studio-a/cam1` | inside `media` only |
| `/dev/shm/mxl/replicated/ingest` | `fast/ingest` | inside both `media` and `fast`; `fast` is the innermost |
| `/mnt/nvme/mxl/capture` | `bulk/capture` | inside `bulk` |
| `/dev/shm/mxl/replicated` | not a domain | it is the directory of area `fast` itself |

In the other direction, when the server assigns this node a target for domain `fast/ingest`, it
sends the area name `fast` and the elements `["ingest"]`. The agent looks up its own area `fast`,
checks that it grants `write`, and joins the elements onto the area's path to get
`/dev/shm/mxl/replicated/ingest`. The server never sends a filesystem path. The rest of this
section explains each of these rules and why it was chosen.

*This supersedes two earlier positions in this document. Both are kept below, and the arguments
against them are given, because both look plausible and because what they were protecting still
has to be handled somewhere. The first was that input and output domains are **separate
concepts** with separate identities: an input domain named by its absolute path for life, and an
output domain named by a request as elements under an output root. The second was that discovery
is **pruned** at every output root in both directions, so that "a root is written, not read".*

#### A domain is a place, not a channel

The old input/output split treated a domain as a pipe: it has two ends, one process fills it and
another drains it, and whichever end this project occupies decides what kind of directory it is.
MXL does not work that way. A domain is a directory that holds flows. Several processes routinely
write different flows into the same domain. The single-writer constraint the SDK actually enforces
is **per flow** (a flow's ring buffer has one producer); nothing says a directory has one owner.

It is more accurate to think of a domain like a multicast group than like a pipe. This project is
one participant among the node's media functions, not the owner of any directory. Ownership is a
property of a *flow*, which is where MXL already puts it. Two consequences follow. There is only one
kind of domain. And the thing that must not be fed back into replication is a flow this node is
already writing, not a directory this node was granted.

#### Areas, and the two grants

An area is a name, a directory and two independent grants (see the example above). **`read` is the
whole of this project's authority to discover and observe domains under that directory; `write` is
the whole of its authority to create them and write flows into them.** Neither implies the other,
and both default to false. An area that grants neither is refused at startup, because the line
would do nothing and the operator who wrote it would believe it did. On the command line an area is
written `name=path:grants`, for example `fast=/dev/shm/mxl/replicated:rw`.

*This supersedes `--search-path` and `--output-root` as separate concepts.* The document already
called them counterparts and said they should be read as a pair; one noun with two flags is the
direct way to express that. The layout operators most often want — one MXL directory per host, with
a subtree that replication may write into — used to be an exception to an overlap rule. It is now
simply two ordinary areas.

The grants have the same force as before. Areas are static agent configuration and are sent in
**registration**, alongside fabric attachments, for the same reason: the server needs them to make
correct decisions, and they change when the host is built, not when a flow is routed. The API
cannot set them. A node with no readable area offers no sources; a node with no writable area
accepts no destinations at all. Both defaults are correct and they are the same default: access to
a node's filesystem is opt-in per node and per direction.

Several areas are supported because "this domain on tmpfs, that one on NVMe" is a real requirement,
and because an area is the natural place for a future capacity budget: capacity belongs to a mount,
not to a domain.

The agent creates its writable areas at startup (§6.1), so the only `MkdirAll` on the session
establishment path is for the domain's own leaf directory.

#### One name, from the innermost area

A domain's fleet-wide identity is its area's name followed by its path elements relative to that
area. With the layout above, `/dev/shm/mxl/studio-a/cam1` is `media/studio-a/cam1` and
`/dev/shm/mxl/replicated/ingest` is `fast/ingest`.

**Areas may nest, and the innermost area that contains a directory names it.** This is the
longest-prefix-wins rule used in routing, which is where this section borrows its vocabulary from.
`media` being an ancestor of `fast` therefore needs no disambiguation:
`/dev/shm/mxl/replicated/ingest` is `fast/ingest` and never `media/replicated/ingest`, because
`fast` is the tighter match. Two areas with the same path are refused at startup, naming both,
because that is the one arrangement the rule cannot decide. An area's own directory is not a
domain, since a domain has at least one element; so `/dev/shm/mxl/replicated` is the area `fast`,
not the domain `media/replicated`.

**Everything below depends on this rule, because it gives every directory exactly one name.** A
directory has the same name whether discovery found it or the reconciler created it, so the two
cannot disagree. Preventing that disagreement was the main job pruning used to do.

Elements follow one rule: ASCII letters, digits, `-`, `_` and `.`, not starting with `.` or `-`,
at most 64 bytes each. An area name follows the same rule and must be unique on its node, so a
rendered domain name is unambiguous about where its first segment ends. The same rule also governs
the value of a domain's optional `name` label (§10.7). The rule was written for the `-m` flag, which
no longer exists, and was kept because a string that ends up in a metric label needs the same
constraints whether it is identity or decoration. A domain has at most eight elements and its
rendered name at most 255 bytes, area segment included. These limits are for legibility, not
safety; safety comes from the per-element rule.

#### A domain is an area name and a list of elements

**Settled: a domain is `(area, []string)` in the model and on the wire, and `area/a/path` only in a
manifest file, parsed once by the CLI.** `("fast", ["studio-a","cam1"])` is the directory
`<fast>/studio-a/cam1` and renders as `fast/studio-a/cam1`. A flat domain is a one-element list.

There are two reasons for this representation:

- **A list of elements rather than a string** makes the containment check part of the data
  structure instead of something that has to be argued (see "Resolution, and the invariant" below).
- **Parsing happens at exactly one boundary.** Every component that would otherwise need rules
  about separators — the server, the assignment, the agent's resolver — receives structure, never
  text, so there is no second parser that could disagree with the first. The flow selector follows
  the same pattern: a flat string in the file, structured on the wire (§9.1).

Everything downstream still carries a single rendered string: the assignment's domain, the path and
session identity, and the `domain` metric label. No information is lost, because neither an area
name nor an element can contain the separator, so each rendered string corresponds to exactly one
`(area, elements)` pair.

**One materialised domain may not contain another.** `fast/studio-a` alongside
`fast/studio-a/cam1` would make one domain directory a container for another, a shape nothing else
in the design has. With elements, the check is an exact prefix comparison of two lists; with
strings it would have to avoid treating `studio-ab` as a child of `studio-a`. This rule governs what
a request may *ask to create*. A directory layout that some other process creates is that process's
business, and discovery reports whatever it finds.

**Domain names in different areas cannot collide, so there is no per-node uniqueness rule to
enforce.** *An earlier position made output domain names a single namespace per node, so two domains
under different roots could not share a name, and gave the collision a rejection code,
`domain_name_in_use` (§7.2).* That rule existed because the assignment, the path identity, the
session identity and the `domain` metric label each carry one string, and `fast:ingest` and
`bulk:ingest` rendered to the same string. With the area in the name, `fast/ingest` and
`bulk/ingest` are two different strings and the collision cannot happen. That use of the code was
removed, not relaxed. (The code name `domain_name_in_use` itself survives: it is now the rejection
for one materialised domain nested inside another, above.)

#### Resolution, and the invariant

A target assignment carries an **area name** and the domain's **elements**. It never carries a
path, and never a string the agent has to split. The agent resolves it as
`join(area.Path, elements...)` after checking that:

- the area name is one this agent advertises, and that area grants `write`;
- every element is a clean path element: no separator, not `.` or `..`, not empty, and only
  characters from the restricted set;
- the joined path equals `area.Path + "/" + join(elements)` exactly.

The character-set rule already makes directory traversal impossible. The containment check makes
that provable, in two lines of code.

The containment check compares the **whole path for equality**, which is possible because the agent
receives elements rather than a string. There is no prefix test to get subtly wrong
(`HasPrefix("/dev/shm/mxl-evil", "/dev/shm/mxl")` is true), and no edge case around separators: if
`Join` cleaned anything away, or an element contained a `..`, the two strings differ.

**The destination resolver never consults observed state.** This is the most important security
property of the design. It is unchanged by merging input and output domains, because it was never a
consequence of the split; it is a property of how destinations are *resolved*. An initiator's
domain is resolved through inventory, because a source is by definition something the agent
observes. A target's domain is resolved only from agent configuration and the assignment, and does
not depend on what is currently on disk. The security-critical path is therefore a pure function of
one config file and one domain name. Removing pruning from discovery (below) does not affect it:
discovery decides what is *visible*, never what is *writable*.

The agent checks the `write` grant as well as the server. This is the one check the merge adds.
Under the old split, "this is an output root" implied the write permission; now that every name
resolves against one area table, the grant is a field on the entry and has to be checked.

The agent performs all of these checks even though the server has already validated the same
things (§7.2). The reasoning is the same as everywhere else this project duplicates a check: it
costs a map lookup and a string comparison, and it is the difference between one buggy control
plane and files written anywhere an area can reach.

There is also a benefit on the read side. A source domain name is now `<area>/<elements>` too, so
the concern §6 addresses — that a name which looks like a path invites an agent to treat it as one
— no longer arises. `/etc` is not a valid domain name in this grammar at all.

#### Discovery is not pruned

**Settled: discovery reports every domain in every readable area, including one this project
materialised and one it is currently writing into.**

*This supersedes pruning, which said "discovery reports nothing at or under a root, in either
direction", and the rule that followed from it: "a root is written, not read. A domain some other
actor creates inside a root is invisible as a source."* Pruning did four separate jobs. Two of them
are no longer needed because of the naming rule above. One is now done by a mechanism that had to
exist anyway. The fourth is still needed and has moved to the flow level, where the multicast view
says it belongs.

1. **One directory, one name and one owner.** No longer needed. The innermost-area rule gives every
   directory exactly one name regardless of who reports it, so there are never two names for an
   owner to choose between.

2. **The ordering hazard.** No longer needed. This was the most serious of the four. Suppose a
   worker was killed with `SIGKILL` and left a flow in `<root>/cam1`. Discovery would find that
   directory before the reconciler materialised it. Because a domain was named by whoever reported
   it first, it would keep its path-based name through materialisation. The server matches a
   session's destination by name, so the path would stay in `ESTABLISHING` with nothing explaining
   why. Now both discovery and the reconciler name it `fast/cam1`, so there is no second name that
   could stick.

3. **Withdrawal.** The old text rightly called this the job that is easy to forget. It is now done
   with a union rather than by hiding. The discoverer only reports directories that currently
   contain a flow. Without pruning, discovery alone would drop a materialised domain from inventory
   the moment its last flow was released, while a session still targeted it. So inventory
   membership is the **union** of two sources: a domain is in inventory if discovery reports it
   *or* the agent materialised it and has not released it, and it leaves only when both say no.
   This is a small change to machinery this section already required, since the agent adds
   materialised domains to inventory itself in any case (below).

4. **Preventing self-amplification.** This job remains. The multicast view is the reason it
   remains, not an exception: a network of receivers that forward what they receive is where loops
   come from, which is why reverse-path forwarding and spanning tree exist. But "under an output
   root" was a coarse, directory-level stand-in for what actually matters. A domain holding one
   replicated flow beside nine local ones was entirely invisible as a source. The accurate test is
   per-flow provenance: **a label selector never matches a flow this project is itself writing.**
   Naming a domain explicitly still reaches everything. This is the same distinction as before —
   explicit chaining is intent, matched chaining is emergence (§10.7) — applied at the level where
   ownership actually lives, and it is strictly more precise than the rule it replaces. The rule and
   its consequences are described in §10.7; this section covers the signal it depends on.

The agent provides that signal: inventory carries a **`replicated`** boolean per flow, which is
true exactly while one of this agent's own target workers is writing the flow (§6). It changes only
when a target starts or stops, so it adds almost nothing to the update traffic that §6's
compare-before-send rule keeps low.

**Provenance can be briefly missing, and §11.1's admission rule makes that safe.** For the 1–2 s
after an agent restart (§6.1), the target workers are not yet running, so a replicated flow reports
`replicated=false` and looks local until they come back. This window cannot cause amplification,
and not by luck. §11.1 holds a path in `PAUSED`, with no workers started, until its source flow is
actually being produced. A flow whose target worker is not running is not advancing, because the
target worker is what advances it. "Replicated" and "being produced" are the same fact seen twice,
so they cannot disagree in the dangerous direction. The same reasoning covers long-idle teardown
(§11.1): the target is stopped on purpose, and the source is idle by definition. **Admission is
therefore required for safety, not only for reducing churn.** Anyone optimising admission should
know this: admitting a dormant source eagerly would reopen self-amplification by a route unrelated
to admission's own purpose.

This establishes a rule that is best stated positively, because it widens what is allowed: **a domain
this project writes into is an ordinary domain.** It appears in `GET /v1/nodes/{node}/domains`, it
can be labelled, and it can be named as a source. A flow in it that this node is not writing — for
example one a local media function produced beside the replicated ones — can be selected like any
other flow. The only thing that cannot happen is a selector matching a replicated flow and copying
it again.

One configuration rule remains, and it is the one that was doing the real work all along: **areas
may not share a path.** Everything else the old text refused — a search path inside a root, a
search path equal to a root, a root that is an ancestor of an input mapping — was arithmetic on a
distinction that no longer exists. The agent still logs at startup each area with its grants and
its nesting; otherwise an operator has no way to see why a domain has the name it has.

#### Materialising, and observing what was materialised

**Materialising** a domain means creating it so a target worker can write into it. It is three
steps, all on the destination agent, all triggered by accepting a target assignment:

1. `MkdirAll` the resolved path.
2. Add the path to the flow watch set.
3. Start the worker.

Release undoes step 2 when the last session on that domain stops.

Step 2 is the one that is easy to forget and expensive to debug. §11 derives `ACTIVE` from the
destination flow's head index **as reported by the destination agent's own inventory**; there is
deliberately no separate "destination is receiving" signal. A domain this project writes into but
does not observe can therefore never leave `ESTABLISHING`. `mxl-utils`' `Discoverer` fixes its
`static` list at construction and only reports directories that already contain a flow, so neither
of its mechanisms can report a freshly materialised, still-empty domain. The agent therefore updates
the inventory and the watcher itself, keeping the receiver ordering the discoverer would otherwise
provide.

*Doing this by hand used to be the whole mechanism*, because with pruning, nothing a scan saw could
name a materialised domain and nothing a scan stopped seeing could withdraw one. Now it is the part
that covers the empty-domain case: a scan and the reconciler both report the same domain under the
same name, and inventory holds the union of the two (above). The property that mattered is kept —
a scan cannot *withdraw* a materialised domain. The property that was incidental — a scan could not
*see* one — is deliberately given up.

A useful consequence, which is now the normal case rather than a special one: **chains work with no
extra design.** In `A→B→C`, the first request materialises the domain on the middle node, which
puts it in the watch set, which makes it visible as a source for the second request. It exists for
as long as the first hop does, which is the correct dependency. The second request names it
in the same grammar as any other domain (§10.7).

#### Leaked directories

When the last path targeting a domain goes away, the directory remains. The MXL SDK removes a flow
directory when the flow's writer is released, so what is left is usually empty. An empty directory
is not reported as a domain: it is in no inventory and cannot be selected. Materialising it again
is an idempotent `MkdirAll` over the existing directory.

*A leaked directory that still holds stale content used to be hidden as well.* The superseded text
noted that pruning hid the directory whether or not it was empty, and counted that as an
advantage. Now it is discovered, and it appears as a domain holding a flow nobody is writing. That
is the correct outcome. It cannot cause amplification, because a flow that is not advancing is not
admitted (§11.1), so a path over it stays in `PAUSED` with nothing running. It cannot be mistaken
for live media, because `PAUSED` is the state that means nobody is writing. Showing it is better
than hiding it: the old behaviour left the leak for an ownership model that does not exist yet, and
gave the operator nothing to notice in the meantime.

Cleanup is still deliberately **out of scope**, for the same reason as before, which the model now
states directly. This project cannot tell a directory it created from one that already existed. A
domain is a shared place by design, and a domain this project did create may still be in use by
another process on the host after the last replication into it has stopped. Removing either kind is
data loss that nobody could detect. Deciding when removal is safe needs an ownership model, which is
a separate piece of design (§19).

#### The superseded split, and what it cost to unify

*Kept because the reasoning looks plausible.* The earlier position was:

| | Input domain | Output domain |
|---|---|---|
| Layer (§4) | observed | derived |
| Comes from | discovery under a search path | a replication request naming it |
| Named by | its absolute path, for life (§6) | the request, under a root |
| Lives as long as | a producer keeps it non-empty | a path targets it — refcounted exactly like a path (§3) |
| Resolved by | a lookup in this agent's inventory | derivation under a root |
| Written to by this project | never | always |

Its argument was that domains straddle two of §4's layers, that almost everything awkward about
them comes from that, and that separating the two roles removes the awkwardness. **The diagnosis was
right but the fix went the wrong way.** A domain is simply *observed*: a directory that exists on a
node and is reported by the agent that can see it. What is *derived* is whether a session targets
it. That was always a property of the session, not of the directory. Modelling it as a property of
the directory is what produced two nouns, two naming schemes, two lifecycles, and a pruning rule to
keep them apart.

The part of the old position that survives unchanged is the part that was never about the split:
**a domain needs no lifecycle of its own.** The directory is created by the first path that targets
it and forgotten when the last one goes, using the same refcount that already governs paths. There
is no create API, no delete API, no "delete while referenced" conflict, and nothing durable to
reconcile against. The argument that removed input mappings from agent configuration also survives,
and it is now the same argument rather than a mirror image of it: a domain name is not something a
node has, it is something an operator decided. Deciding it on the host means deciding it in the one
place that has to be restarted to change it (§6, §6.1).

**What unifying costs:**

- Every domain identity in the system changes: `/dev/shm/mxl/cameras` becomes `media/cameras`. Every
  stored path, session and label record, every manifest and every dashboard query built on the old
  spelling is invalidated by the upgrade.
- Area names must be unique per node.
- Renaming an area orphans the labels on its domains, just as renaming a node does (§10.7).
- A destination must always name its area. The old `root:` field could be omitted on a node that
  advertised exactly one root. This small extra verbosity is the price of one grammar instead of
  two.

**What it gains beyond one grammar:** an area's directory can be moved without re-identifying
anything in it. Changing `path: /dev/shm/mxl` to `path: /mnt/mxl` under the same area name keeps
every domain's name, so paths and sessions survive the move instead of being rebuilt. Changing the
path restarts the agent, which re-establishes every worker on the node anyway (§6.1), and the
workers come back with the same identities. Flows left in the old directory are leaked and nothing
moves them; sequencing that is the operator's job. When the path was the identity, this was a
fleet-wide re-identification, recorded as an accepted cost. It no longer is.

### 10.7 Domain labels

**Settled: an operator labels domains through the API, before or after they are discovered. Labels
annotate; they never rename. A request's source names a domain directly or selects domains by
label.**

This is the naming half of the old `-m` flag, moved to the API (§6, §10.6). The flag's useful part
was giving a domain a short fleet-wide name. Its problem was that the name was a startup argument,
so naming a domain cost an agent restart, and an agent restart re-establishes every flow on the node
(§6.1).

#### Identity is the domain's name, and a label never touches it

A domain's identity is `<area>/<elements>`, permanently (§10.6). Labels are extra key/value pairs
attached to `(node, domain)` and nothing else.

*This used to say "identity is the absolute path".* Only the spelling of the identity changed: a
label was never identity and still is not. What the area name adds is that a label survives an
operator moving an area's directory. When identity was the path, the label was orphaned along with
the domain it described.

The obvious alternative is to let a label supply the domain's *name*. It does not work. The domain
name is part of the path identity (§5.4), the session identity and the `domain` metric label, so
renaming a domain would re-identify every path through it. A metadata edit would tear down running
media and split every metric series it touches — the churn §12 refuses to allow for flow labels.
Keeping identity fixed makes relabelling free, and turns naming into *selection*.

§9.1 made the same choice one layer down. A flow UUID is rarely what a user means, so a source is
a selector rather than a flow ID. A domain name plays the same role for domains that a UUID plays
for flows, so the consistent answer is again a selector, not a rename. `sources[].domain` is
therefore a tagged union with exactly one kind set, extensible in the same way and for the same
reasons:

```json
"domain": { "name": { "area": "media", "elements": ["cameras"] } }
"domain": { "name": { "area": "fast", "elements": ["ingest"] } }
"domain": { "labels": { "role": "cameras" } }
```

In a manifest the first two are written `media/cameras` and `fast/ingest` (§9.1, §10.6).

**Settled: the direct form is `name` and it addresses any domain.** *This supersedes
`{"path": …}`, which could only address a discovered domain. With two identity grammars, the second
hop of a chain could not be written at all, even though §10.6 says `A→B→C` needs no extra design.*
One grammar removes the problem instead of working around it: `fast/ingest` is a name like any
other. The union still has two kinds because the second kind is *selection*, not a second way of
*naming*.

The direct form is not a fallback. A manifest that names a domain is self-contained, whereas a
label selector depends on a `kind: domain` document that someone may not have applied. Both can go
in one file, which is what applying documents in kind order (§9.1) is for.

#### What the selector matches: equality, ANDed, never empty

**Settled: the `labels` kind is equality-only, every key ANDed, and a selector with no keys is
refused.** `{"role": "cameras", "site": "studio-a"}` matches a domain on the named node that has
both keys with exactly those values, compared as case-sensitive strings. There is no `in`, no
`exists`, no negation, no wildcard and no value syntax of any kind; a value is a string compared as
a whole.

This is the simplest first version, chosen for the same reason §9.1 gives for the flow selector: the
restriction keeps future extensions additive. `in`, `notin` and `exists` are the operators people
will eventually want, and they will arrive as a **third union kind** (a list of match expressions),
not by extending what a map value may contain. Extending the value syntax could not be undone: a
request whose value happened to look like an expression would change meaning on upgrade, silently
and towards matching *more*, which for a system moving uncompressed video is the worse direction to
fail in. A new kind cannot do that, because no existing request sets it.

**A selector with no keys is refused, and the validator enforces it, not only this document.** An
empty map matches every domain on the node, which expands a request's sources to whatever that node
happens to hold. It is easy to reach by accident: `domain: {}` and a `domain:` whose keys were all
deleted are both easy to write. §9.1's scalar-versus-map rule already answers the *syntax* question
(a scalar is a name, a map is a label set, so this is a label selector with no keys, not a third
case needing its own rule). But the syntax rule does not refuse it, so a separate check has to.

#### Labels annotate; areas authorise

**A label has no effect unless discovery already reports a domain by that name.** It is never a
permission, and the agent never sees labels. The server joins label records against inventory, so
nothing new is sent to the agent, no new state is held there, and §4.2's fail-static behaviour is
unchanged. The destination resolver remains a pure function of one config file and one domain name
(§10.6).

This separation matters for security. If labels were sent *down* to the agent — for instance to add
to the discoverer's `static` list so that a labelled but empty domain stayed visible — the API could
point an agent at a path the host never granted. That access would be read-only, but it would still
let the API read data out of any directory, limited only by what looks like an MXL flow. Keeping the
join on the server means the boundary is the set of areas that grant `read`. Those are local to the
node and controlled by whoever builds the host, just like the `write` grant (§10.6, §13).

One rule follows, where there used to be two:

- **A label on a domain the node does not report is accepted and has no effect.** It is a pending
  record, not an error. It covers three cases that used to be handled separately: a name in no area
  at all, a name in an area that grants only `write`, and a name in a readable area where no
  producer has created a flow yet. The last case is what "before or after" in the Settled line above
  refers to: the operator labels a camera's domain before the camera is switched on.
  `GET /v1/nodes/{node}/domains` lists the label, so the intent is visible rather than lost, and a
  request selecting it waits in `WAITING`, which §7.2 already classifies as not an error.

*The rule this replaces refused a label on a path under an output root, naming the root.* Its
reasoning was that output domains are derived state with an owner, and that a label making one
selectable as a source would be the start of a selector matching this project's own output. The
first half no longer holds: a domain is observed and has no owner (§10.6). The second half is now
enforced where the danger actually is, one level down, on the flow. Labelling `fast/ingest` is a
normal thing to do: it is a domain on a node, it may hold flows this project did not write, and an
operator has the same reasons to label it as any other domain.

The cost of doing the join on the server is that a labelled domain with no flows is not in
inventory at all, so a selector cannot *match* it until a producer appears. That is more accurate
than the old behaviour, where a domain existed because a config line said so.

#### The `name` label

One key is a convention rather than a special case: `name`, if present, is what an operator calls
the domain. It is exported as an additional `domain_name` metric label beside the identity-valued
`domain` label (§12). (That label is designed but not emitted yet; see §12.) Its value must
follow the element rule (§10.6), which outlived the `-m` flag it was written for.

It is deliberately **not** required to be unique per node. Identity is the domain name, so two
domains with the same `name` label is a cosmetic issue, not an ambiguity. Enforcing uniqueness would
make every label write a cross-record check that protects no invariant.

#### What a domain selector does not reach

**Settled: a label selector never matches a flow this project is itself writing. Naming a domain
directly reaches everything.**

*This supersedes "a source selector never matches a domain under an output root".* The rule and the
distinction it draws are the same; only the granularity changed, from the directory to the flow.
That is where §10.6's multicast view puts ownership, and the directory was only ever a stand-in for
it.

The danger it prevents is unchanged. Without the rule, replication feeds itself: a flow copied to a
node becomes visible on that node, a broad selector matches its own output, and the path set grows
on every reconcile pass. The growth does stop — it is bounded by nodes × domains × flows, and
`flow_conflict` blocks a second producer into any one flow ID — but the topology it ends up in is
decided by §7.5's precedence rules, not by anything an operator wrote. The system should not route
media by an emergent algorithm. A network of receivers that forward what they receive is where
loops come from, which is why every multicast fabric has a rule of this kind.

Moving the rule to the flow level is an improvement, not just a translation, for two reasons:

- **It is more precise.** Under the old rule, a domain holding one replicated flow beside nine
  flows a local media function produced was entirely invisible as a source. Now the nine can be
  selected and the one cannot, which is what an operator would expect from the rule as stated.
- **It can be expressed at all.** "Under an output root" stopped being something a domain could be
  once roots no longer existed. "This node's target worker is writing this flow" is a fact the
  agent already knows and reports as `replicated` in inventory (§6, §10.6). The agent cannot be
  wrong about it, because it is the process that started the worker.

The signal has one weak spot: provenance is briefly missing after an agent restart. §11.1's
admission rule covers that, not anything in this section, and §10.6 explains why the two cannot
disagree in the dangerous direction.

The flow-level rule also has a cost the directory rule did not, and the design pays it rather than
accepting it: **an excluded flow must be visible, or the finer rule is harder to understand than the
coarse one.** Under the old rule the whole domain was missing from the source's options, which an
operator could at least see. Now the domain is present, its flows are listed in `GET /v1/flows`,
they match the labels, and some of them are silently absent from the expansion. Three things make
the exclusion visible: `replicated` is a field on `GET /v1/flows`, `describe domain` shows it per
flow, and a request whose expansion dropped a flow for this reason says so in its status, so it can
be told apart from a flow that did not match (§9.1). The agent cannot be wrong about the flag, so
the only way this becomes impossible to diagnose is if it is not reported.

**The `all` flow selector interacts with this rule in both directions, and the distinction stays
the same** (§9.1):

- With a **named** domain, `all` is not filtered by provenance, because naming reaches everything.
  `{node: B, domain: fast/ingest}` with no flow selector means "forward everything B receives".
  That is the shortest way to write a chain and also the shortest way to write an amplifier. It is
  not a new hazard: it is `A→B→C` written once, it is explicit on its face, and a real cycle is still
  refused as `loop` (§7.2). The alternative, refusing `all` on a domain this project writes into,
  would bring back the directory-level rule this section replaced, on the one selector where an
  operator most clearly means what they wrote.
- With a **label** selector, `all` is filtered like any other match. This is the combination that
  will hit the exclusion cap: every replicated flow in every matching domain is reported as
  `self_output`, which on a busy destination node routinely exceeds §9.1's cap. The truncated count
  exists for this case, and this is the first request shape that reaches the cap in normal
  operation rather than in a pathological case.

**A self-pair produced by a selector is dropped, not rejected.** `same_endpoint` (§7.2) catches a
source and destination that resolve to the same `(node, domain)`. With a named source, that means
the operator wrote the same string twice. That is a typo, it can be decided from the request alone,
and it is refused. A label selector matching the destination's own domain is not a typo: the selector
is doing what it was asked to do. Refusing the request would make its outcome depend on which
domains happen to carry a label. So that one pairing is dropped and the rest of the expansion stands.
§10.8 makes the same `same_endpoint` argument for multipoint; it applies here already because domain
selectors make the case reachable without multipoint.

Naming a domain explicitly still works, so §10.6's chaining property is unaffected: `A→B→C` is two
requests, the second naming B's domain as `fast/ingest`. **Explicit chaining is intent; matched
chaining is emergence**, and that is where the line is drawn.

### 10.8 Multipoint: what it would take

Multipoint is not built. But most of what this section originally listed as its preconditions now
is, so what remains is smaller and better defined than when this section was first written.

**What is built.** Both ends of a request are lists (§9.1), so the *cross product* exists: N sources
against M destinations. The cross-product machinery — per-pair validation, per-pair negotiation,
and `PARTIAL` status over a set of paths that do not share fate — is in place and in use. Of the
four arguments §9.1 raised against this, three were resolved there rather than left for this
section:

- shared fate became `PARTIAL` plus a per-source breakdown;
- the corruption case became `duplicate_source_flow` at `POST` plus `flow_conflict` per path;
- cost legibility turned into an argument *for* grouping ingress.

Of the three mechanisms this section asked for, two now exist: `same_endpoint` becomes a silent drop
when a label selector produced the pairing (§7.2, §10.7), and the self-output exclusion is built and
is what makes domain selectors safe at all.

**What is left is the selectors themselves.** A source's `node` is fixed, and a destination is a
`(node, domain)` pair written out in full. Multipoint is what happens when either of these becomes
a selector. §9.1's first argument — *"the destination side cannot have a selector; a destination is
a `(node, domain)` pair by necessity"* — **stops being true** once nodes carry labels: "every
`role=edge` node's `ingest` domain" is perfectly well defined. Node labels do not exist yet (§3.2 of
`docs/open-items.md`), and designing them is the first step of any multipoint work. The asymmetry
is the opposite of what §9.1 assumed: destination selectors are the harmless half, because they
cannot amplify and their worst failure is that they apply to fewer nodes than intended, while source
selectors carry all of the danger.

Two requirements remain unchanged. Both concern the ends that are *not written out*, not the cross
product itself:

- **Cost legibility, which is now the blocking precondition.** Writing out both ends is what lets a
  reader see a request's expansion at the moment it is written. It is the only reason fan-in could
  ship without admission control: a fan-in author typed every node into the request. A selector on
  either end removes that — three lines can expand to hundreds of paths of uncompressed video, on
  nodes nobody named. **Bandwidth admission control (§13) therefore moves from a roadmap item to a
  precondition**, or at minimum a path-count cap with an explicit override. `apply` printing each
  request's path count (§9.1) is the cheap half and does not replace it, because it reports a number
  after the fact instead of refusing one.
- **Disjointness has to become a real check again.** While both ends are written out,
  `same_endpoint` over the pairings covers it (§7.2): any cycle within a request puts some endpoint
  on both sides, which puts a self-pair into the cross product, which is refused. Two selectors
  cannot be compared that way, because whether they intersect depends on a fleet that changes
  underneath them. The check becomes `overlapping_selectors`, applied to the *selectors* and refused
  up front. That is cheaper and clearer than dropping cycles edge by edge, which would amount to
  routing on the operator's behalf (the same objection as §10.4, in a different form).

One item should be built whether or not multipoint ever is: `describe path` naming its contributors
(§7.2). The conservative merge cannot be diagnosed today, and fan-in makes it common rather than
rare.

---

## 11. Status and failure semantics

**A request is durable intent. A session is never cancelled because it is failing.** Instead, the
failure is reported through status.

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
| `FAILED` | Repeated permanent-looking failure, or a session whose fabric stopped being viable. Still retried, but reported prominently. |

Each status carries a human-readable reason, a machine-readable reason code (§7.2), and the
identity of the component that reported it. Status is visible at every level: request → path →
session → worker. A request that covers several paths (§9.1) reports an aggregate of their states.

**`ESTABLISHING` covers the whole setup phase on purpose.** The sub-steps (§5.3) are useful in a
reason string and in logs. They are not separate states because an operator does not act
differently on any of them: everything from "session created" to "first grain received" means the
path is coming up.

**`PAUSED` is the most useful status for an operator.** A "no media at the destination" alarm can
have two causes: the replication is broken, or the source is not producing. The alarm looks the
same in both cases, but different people own the fix. `PAUSED` says that nobody is writing at the
source end. It says this whether the workers are running and idle, or have been torn down after
being idle too long (§11.1, §7.2).

**`ACTIVE` is determined from the flow, not from the worker.** The destination agent reads
`HeadIndex` and `LastWriteTime` from the *destination* flow through mxl-utils' `Flow.GetInfo()`,
and reports whether the head is advancing. That is the direct evidence that media arrived. It does
not depend on the worker's own accounting, which matters because a worker can report healthy
transfers while producing a flow nothing can read. Worker metrics (`mxl_grains_total`) are still
useful to confirm the state and to measure rate, but they do not decide it.

`DEGRADED` and `FAILED` are classified from **restart rate and time-to-death**, not from the
worker's exit status (§15.1).

#### `PARTIAL`: why requests need a state of their own

**Settled: a request whose paths disagree, at least one of which is `ACTIVE`, is `PARTIAL`.
Otherwise the fold is unchanged: worst-state-first over the path set, with a request-wide
`INVALID` leg short-circuiting it.**

*This supersedes "`ACTIVE` only when all of its paths are", which was the whole of the old rule.*
That rule was correct while every path of a request shared one source (§9.1). An idle producer then
moved all the paths to `PAUSED` together, so the aggregate only ever had to describe one shared
state, never disagreement. Now that `sources` is a list, disagreement is the normal case: a
twelve-camera ingest wall usually has one camera dark. The old rule reports that request as
`PAUSED`, which is true of one path and false of the request.

**`PARTIAL` is not specific to fan-in.** A group-hint request matching three flows, one of them
paused, has always been in this condition and has always reported `PAUSED`; "1 of 3 active" was
visible only in the counts. Fan-in made the case common, but did not create it. The state applies
to every request shape, so that the status vocabulary does not differ depending on which end of a
request is a list.

**`PARTIAL` outranks `INVALID`, `FAILED` and `DEGRADED`.** This is the surprising part, and it
follows from §7.2. That section settled that a request whose selector expands onto twenty paths,
one of which conflicts, "is not refused: it reports nineteen paths and one invalid one with its
reason". If the aggregate promoted the one bad path to the request's top-line status, it would undo
that decision at the level an operator reads first. So the aggregate answers "is this request doing
its job?", and the details of what is wrong are reported where they can be acted on:

- in `Counts` and the per-source breakdown;
- in `status`, which names every non-`ACTIVE` request and what is wrong inside it;
- in the per-path gauges of §12. A fleet alert on failing **paths** does not change with how their
  requests fold.

**Reusing `DEGRADED` for this was considered and rejected.** `DEGRADED` means flapping — restart
count over a threshold in a window (§15.1) — and paths and sessions are really in that state. If it
also meant "some paths failing" at the request level, `mxl_repl_requests{state="DEGRADED"}` would
sum two populations that need different responses, and nobody could interpret it.

`PARTIAL` is therefore the one state that is **aggregate-only**. Every other state describes one
thing; `PARTIAL` describes disagreement among several, so a path or a session can never be in it.
This widens §11's rule of one vocabulary at every level, and it is stated here so that UI authors
know it in advance: a renderer may show `PARTIAL` on a request row and must never expect it on a
path row.

Two further rules:

- **The reason names the worst non-`ACTIVE` state and how many paths are in it.** For a request
  with several sources, when a failure is common to every destination of one source, the reason
  names that source, not the destinations, because naming the wrong end sends an operator to the
  wrong node (§9.1).
- **A request with no `ACTIVE` path is never `PARTIAL`.** A mix of `WAITING` and `ESTABLISHING`
  folds worst-first as before. `PARTIAL` claims that something is working, so it must not be
  reported when nothing is.

#### `DISABLED`, and why it is derived rather than stored

**Settled: a request with no enabled destination is `DISABLED`. Like `PARTIAL` it is aggregate-only;
unlike every other status it describes the spec rather than the fleet.**

No existing state can express it:

- `WAITING` promises that the condition resolves by itself when a flow appears. A parked request
  never will.
- `INVALID` says something is wrong and needs user action. Nothing is wrong: an operator has
  already decided to park the route. Showing it as a fault makes a board with twenty parked legs
  read as twenty problems.

A state gets its own name when an operator acts differently on it, which is the rule the §11 table
is built on. "I turned this off" calls for a different response from both `WAITING` and `INVALID`.

**It is computed, never stored.** §9.1 puts the `disabled` flag on the destination entries, so
there is no request-level field for this state to agree or disagree with; the fold reads the spec
it is given. This avoids a real failure: a stored flag and a destination list can drift apart, and
then the API reports a request as off while its legs are running.

Three consequences, which mirror `PARTIAL`'s:

- **A path is never `DISABLED`.** A disabled destination produces no pairing and therefore no path,
  so there is nothing underneath for the state to describe. `States()` is unchanged and
  `RequestStates()` gains a ninth value — the same split `PARTIAL` already required, for the same
  reason.
- **A partly parked request is not `DISABLED`.** With one enabled destination and one parked one,
  the request folds over the paths the enabled destination produced, as though the parked one had
  never been written. `DISABLED` applies only to a request that expands to nothing *because*
  everything is off. That must stay distinguishable from a selector that matches nothing, which is
  `WAITING` and does resolve by itself.
- **It ranks below `ACTIVE`, not above `INVALID`.** The worst-first ordering is a list of things to
  look at, and a parked request is not one of them. `status` counts disabled requests and names
  them on a line of their own, instead of including them in "what is not active". Parked intent
  has to stay visible, because this feature makes it possible for a leg to stay off for a reason
  nobody remembers, but it must not look like a fault.

### 11.1 Idle sources, and why `PAUSED` needs three mechanisms

This is the interaction in the design that is easiest to miss until it happens in production.

Both worker roles exit by themselves after a period without a grain: the initiator when it reads
nothing from the local flow, the target when it receives nothing. The agent then restarts them
after a delay. With the timeout at its original hardcoded 10 s, a session whose source has no
producer would not stay in `PAUSED`. It would go through a restart every ~13 s, permanently.

This is more than a cosmetic problem. With hash epochs (§5.2), **every target restart changes the
epoch**, and every epoch change causes a report to the server, a recomputed assignment, and an
initiator restart on another node. A requested flow that is idle would cause a full control-plane
round trip every 13 s, per flow, indefinitely. §8.3 treats that amount of churn as a symptom of a
fabric outage. An idle source is not a fault at all: asking to replicate a camera that is not live
right now is an ordinary request.

**Settled: all three mechanisms, because they cover different timescales.**

**1. The worker's no-grain timeout is configurable** (§15). A sentinel value means "wait
indefinitely", and the agent uses it by default. This is what makes `PAUSED` a stable state
instead of a restart loop. It is also the only one of the three mechanisms with no cost on resume:
the workers are still up, the fabric connection is still established, and the first grain the
producer writes is transferred immediately.

**2. The agent observes the source flow's head index** and reports coarse liveness as part of
inventory (§6). The server uses this for two things:

- *Admission*: the server keeps a path in `PAUSED` and starts no workers until the source is
  actually being produced. A request for a flow that exists but is dormant costs nothing.
- *Long-idle teardown*: when a session's source has been idle longer than a configurable threshold,
  the session is withdrawn. Both workers stop, and the path stays `PAUSED` with nothing running.

**Admission also protects against self-amplification, not only against churn.** Keep this in mind
before weakening it. §10.7 prevents a label selector from matching a flow this project is writing.
The signal it relies on, `replicated`, is derived from running target workers, so it is briefly
missing whenever those workers are down: during an agent restart (§6.1), a long-idle teardown, or a
worker crash. In each of those windows the flow is also *not advancing*, because the target worker
is what advances it. Admission therefore refuses to start anything on top of it, and the gap in the
`replicated` signal cannot lead to amplification. Admitting dormant sources eagerly would remove
that protection, for reasons unrelated to idle sources.

**3. Agent-side backoff on restart.** If a worker keeps dying for any reason, the delay between
restarts grows toward minutes instead of staying fixed. This catches everything mechanisms 1 and 2
do not anticipate.

Both settings are **server-side** (§5.5). The worker timeout is written into both ends of every
assignment, and the teardown threshold depends on the source's liveness, which only the server
sees. The idle tracker behind the teardown is held in the leader's memory, so a leader change
delays a long-idle teardown by one threshold. Persisting the tracker would put a continuously
changing value back into the store, which is the churn this section is trying to remove.

#### The two-tier idle policy

Mechanisms 1 and 2 are not redundant. They trade resume latency against resource cost, and the
teardown threshold decides where one gives way to the other:

| Source idle for | State | Workers | Resume cost |
|---|---|---|---|
| seconds to minutes | `PAUSED` | running, waiting patiently | immediate |
| beyond the threshold | `PAUSED` | none | one re-establish, 1–2 s (§6.1) |

If teardown is too eager, a source that stops and starts often loses its first grains to a
re-establish every time. If there is no teardown, dormant flows hold ports, memory registrations
and processes indefinitely. The threshold has a generous default — minutes, not seconds
(`--server-idle-teardown` defaults to 5m; 0 disables teardown). It can be set per request
(`idle_teardown_ms`) as well as globally, because "this feed is bursty, keep it hot" is a real
operational requirement.

#### Observing and reporting liveness

The observation is **local and derived**. The agent watches the head index of every flow in its
domains, using the same `Flow.GetInfo()` machinery it already uses for flow liveness (§6), and
reports one boolean, `producing`, per flow. The server never sees an index.

Three details matter:

- **Compare head indices across samples; do not derive liveness from `LastWriteTime`.** The
  timestamp looks more convenient — one sample, no state, `now - LastWriteTime < threshold` — but
  it is in TAI nanoseconds and only means something if the host's TAI offset is configured. A
  correct TAI clock is a deployment requirement in the broadcast datacentres this project targets,
  so a wrong clock is not a case to design around. A head-index delta, though, needs no clock at
  all, so it is the version that cannot be wrong. `LastWriteTime` is kept for diagnostics. The same
  rule applies to read activity: `LastReadTime` is treated as a number that changes when a reader
  reads, never as a timestamp that means anything by itself.
- **Check `IsValid()` on every sample.** A flow that is deleted and recreated under the same ID has
  a new `data` file. The old mapping keeps working and keeps returning stale values forever.
  Without the check, a republished flow reports `producing=false` permanently and is never
  replicated again. `IsValid` exists for this case: when it returns false, reopen the flow.
- **Observe every flow in inventory, not only flows with sessions.** Admission needs the liveness
  of flows that nothing is replicating yet. `GetInfo()` decodes from a live mapping and is
  documented as cheap enough to call on every scrape, so this is affordable at the flow counts of
  §14.

**Inventory carries no raw head index.** The index changes every frame, so every snapshot would
differ and inventory would write to the store on every heartbeat, forever. That would replace the
churn this section removes with a slower version of the same churn. Instead the boolean uses
hysteresis: it goes from advancing to idle only after the threshold (`--agent-flow-idle-after`,
default 3 s), and from idle to advancing on the first movement, so it changes only on real
transitions. Rate and head index stay in metrics (§12), which is where continuously changing values
belong. Read activity is left out of the
inventory snapshot entirely. Reporting it would cause a store write every time a downstream
consumer starts or stops, waking every watcher in the fleet for something no reconcile depends on.

---

## 12. Observability

**Metric prefixes are chosen by what the metric describes, not by which process emits it** (§2.2):

- `mxl_*` for anything about a flow or a transfer;
- `mxl_repl_*` for control-plane metrics that exist only because of this project.

Each role has its own registry instead of the default one, because two roles in one process must
not merge their expositions.

### Agent

The agent exports:

- worker counters, scraped from the workers' `AF_UNIX` sockets: `mxl_grains_total`,
  `mxl_grains_lost`, `mxl_octets_total`, `mxl_payload_octets_total`, `mxl_last_grain`,
  `mxl_network_latency_ns`, `mxl_source_latency_ns`;
- supervisor-level series: `mxl_worker_restarts`, `mxl_writer_active` and `mxl_reader_active`.

Labels: `direction`, `domain`, `domain_name`, `flow_id`, `session`, `namespace`, the flow
definition's `format` and `media_type`, and the request's user labels.

The reasons behind the labels and series:

- **`session`.** One flow replicated to two destinations puts two initiators on the source node,
  and their `direction`, `domain` and `flow_id` are identical. Without a label to tell them apart,
  the collector emits the same series twice. That is a gather error, and it discards the *whole
  metric family* — every worker's counters on the node, not only those two. The label also makes a
  series joinable to `GET /v1/paths`.
- **`format` and `media_type`, and no other flow-definition labels.** A definition field can be a
  label only if it has low cardinality and is stable for the flow's life. These two meet both
  conditions. The other candidates fail one: a flow's label changes when someone renames it, which
  splits the series, and `source_id`/`device_id` have UUID cardinality, which `flow_id` already
  covers. The values are resolved once per worker and then frozen, because a label value that
  changes during a worker's life splits one series into two.
- **`domain` carries `<area>/<elements>`, the same value on both sides.** *This supersedes a label
  that held an absolute path on the source side and a rendered output name on the destination
  side.* That label mixed two formats, so a dashboard could not join the two ends of a chain, and a
  PromQL author had to know which side they were looking at. A domain's identity is now one string
  (§10.6), and the label holds that string. Its cardinality is bounded by the number of domains per
  node, which is acceptable, and it is stable for the domain's life, which is the requirement that
  matters.

  The change also removes host filesystem paths from `/metrics`, which is a security improvement.
  `/metrics` is commonly unauthenticated (§13), and the old source-side label published the node's
  directory layout to anything that could scrape it. An area name is a name an operator chose as a
  fleet-wide identifier, which is a different kind of disclosure.

  A rendered identity is still not very readable on a dashboard. `domain_name` covers that: it holds
  the value of the domain's optional `name` label (§10.7), or is empty when there is none. It is
  empty rather than absent because a metric family must have one set of label dimensions, the same
  rule as for a user label a worker does not carry. **`domain` and `domain_name` are resolved once
  per worker and frozen.** A relabel therefore takes effect on the next worker start instead of
  splitting a live series. Since `name` is runtime state that an operator can change at any time,
  this is the only workable treatment.

  *Not built yet.* The agent does not emit `domain_name` today: it is not in
  `metrics.WorkerLabelNames()` and `internal/agent/metrics.go` does not set it (open-items §5).
- **`namespace` is a label by decision.** It was included in metrics automatically while it was a
  user label (§9.3). Now that it is a real property, including it had to be decided. It stays:
  dashboards do ask which partition a transfer belongs to, it has low cardinality, and it is fixed
  for a session's life.
- **Quantiles are exported as gauges, not as a Prometheus summary.** The worker reports quantile
  estimates over a sliding 30 s window and has no observation count or sum to give (WRS §6).
  Exporting a summary would add a `_count 0` series next to a populated p50, which states something
  false. The series a dashboard selects, `mxl_source_latency_ns{quantile="0.5"}`, is the same either
  way.
- **User labels are the union across the whole collection, not per worker.** A metric family must
  have one set of label dimensions. User labels come from the request that created each session, so
  two sessions on one node often have different keys. A worker without a key reports it as empty.
  Invalid label names, and names that collide with a label this project sets itself, are dropped
  rather than renamed. The reserved set is this project's own labels (`direction`, `domain`,
  `domain_name`, `flow_id`, `session`, `namespace`, `format`, `media_type`) plus `quantile`. The
  code reserves `metrics.WorkerLabelNames()` plus `quantile`, which does not yet include
  `domain_name`, because that label is not built.
- **The liveness gauges (`mxl_writer_active`, `mxl_reader_active`) are emitted only for flows this
  agent observes.** A destination flow does not exist until its target creates it. Emitting `0` to
  mean "I am not looking at this flow" would look the same as "nothing is reading this flow", and a
  healthy path would appear to have a dead consumer.
- **Restarts are counted twice, in two forms**: a monotonic total for the metric, and the windowed
  list that `DEGRADED`/`FAILED` are classified from (§15.1). They cannot be one field, because a
  counter that decays looks like a counter reset to `rate()` every time the window slides.
- **The start gate is exported** (§6.3): `mxl_repl_worker_starts_waiting`,
  `mxl_repl_worker_starts_delayed_total` and `mxl_repl_worker_start_delay_seconds_total`. These
  have no labels, because there is one gate per node, not one per worker. Without them, a node that
  is deliberately spreading a re-establishment over minutes looks the same as one whose workers are
  failing to start. The restart counters, which an operator would check to tell the two apart, show
  nothing in the first case. Both the gauge and the counters are kept: a permit wait is over in
  seconds, so a gauge alone reads zero for most of the event, and a counter alone cannot say that
  the node is queued right now.

**Settled: scrape the workers on demand, inside the request, through a bounded pool.** The
Prometheus server doing the scraping decides the rate; the agent does not add its own schedule.

*This reverses an earlier position in this document: a background scrape on a fixed interval,
serving a cached snapshot. That position looked reasonable, so the reasons for dropping it are
recorded here.*

- **It distorts rates.** A cached snapshot is taken every C seconds and served to scrapes every S
  seconds, and the two intervals are unrelated. When they are close, consecutive scrapes return
  identical counters, so `rate()` reads zero, and then the next scrape jumps. Every transfer graph
  shows a beat pattern instead of a small uniform lag.
- **It misreports liveness in both directions.** It serves a dead worker's frozen counters as if
  the worker were healthy and idle, and it hides a new worker until the next refresh. With an
  on-demand scrape, a series appears and disappears with the process, and Prometheus' own staleness
  handling does the rest.
- **It adds a second lag.** `mxl_source_latency_ns` is already a CKMS estimate over a sliding 30 s
  window computed inside the worker (WRS §6). Caching it adds a second, unrelated delay to an
  estimate that is already delayed by design.

The cost the cache was meant to avoid does not exist. The worker's `Metrics` class runs its own
listen thread with its own epoll loop (`src/metrics.hpp:35`), so a scrape never runs on the
transfer loop's thread. The two share only the counter mutex, held just long enough to format five
counters and two five-quantile summaries.

On-demand scraping needs less machinery than the cache did:

- a **bounded pool**, so the fan-out has a fixed cost instead of one goroutine and one socket per
  worker per request;
- a **per-worker deadline and an overall collection deadline**, returning partial results, so that
  N stuck workers cannot push the endpoint past the Prometheus scrape timeout and lose the healthy
  workers' series along with theirs;
- a cap on **concurrent requests**, so two Prometheus servers or a retry cannot multiply the
  fan-out.

The deadlines are set on the collector, not taken from the request, because
`prometheus.Collector.Collect` takes no context and the registry calls it without one.
`mxl_repl_worker_scrape_duration_seconds`, `mxl_repl_workers_scraped` and
`mxl_repl_worker_scrapes_failed_total` are emitted from *inside* the collection they measure, so the
duration always describes the exposition it is served with.

`MXL_LOG_LEVEL` is set in the worker's environment from the agent's own log level, and worker
output is re-emitted through the agent's logger.

### Server

All metrics are `mxl_repl_`-prefixed: `requests`, `paths` and `sessions` by state,
`nodes_registered`, `agents_leased`, `sessions_frozen`, `leader`, `leader_acquisitions_total`,
`registrations_rejected_total{reason}`, `epoch_transitions_total`, `reconciles_total`,
`reconcile_duration_seconds`, `reconciled_revision`, `store_operation_duration_seconds`,
`store_operations_failed_total`, `events_recorded_total{kind}`, `events_dropped_total{reason}`,
`agent_versions` and `build_info`.

- **The fleet gauges come from the last reconcile, and a follower reports none of them.** Each one
  is a property of the whole store. The only cheap way to get them is to count while something
  already holds a consistent read. Loading the store fresh on every scrape would mean a full List
  on every Prometheus interval on every replica — against etcd, a quorum read of the entire key
  space, for a question nothing is waiting on. Only the leader reconciles, so only the leader has
  the numbers. If a follower emitted zeroes, "nothing is replicating" and "ask the other replica"
  would look the same. `mxl_repl_leader` is the one series every replica always exports, and it
  tells the two cases apart. When leadership ends, the replica **drops** its last observation;
  otherwise a demoted replica would publish a second, frozen copy of every fleet number next to the
  new leader's live ones.
- **`mxl_repl_requests{state="PARTIAL"}` is a new series with a new signal** (§11). Before it,
  there was no way to count requests that are working but not completely: such a request folded to
  `PAUSED` or `WAITING` and looked the same as one doing nothing at all. It is the series to alert
  on when a fan-in request silently loses a source. The per-path gauges are unaffected, and that is
  intended: an alert on failing paths must not change because the aggregate for their requests now
  has a milder state.
- **`mxl_repl_requests{state="DISABLED"}` is the second new series, and signals the opposite**
  (§9.1, §11). It counts intent that is deliberately switched off, so it is the one state nobody
  should page on. It should still be graphed, because parked legs accumulate: a namespace whose
  disabled count only ever rises is one where people turn things off and nobody deletes them. The
  per-path gauges are again unaffected, and here that follows from the arithmetic rather than from
  a policy choice: a disabled destination expands to no path at all, so there is no path to count.
- **Epoch transitions are a good flapping signal**, because a session changing epoch means its
  target restarted. They are counted on the server because the epoch is a hash and carries no
  ordering (§5.2). The counter is labelled **by node, not by session**. A per-session counter grows
  without bound over a long-running leader, because a session that goes away leaves its series
  behind forever. The node hosting the target is bounded by fleet size, and "which node is
  flapping" is the question an operator acts on anyway. One detail was found in a live run: a
  restarting target reports *no epoch at all* in between, because its old blob describes
  registrations that died with it. The last known value has to be carried across that gap;
  otherwise the counter misses the very restart it exists to count.
- **Store latency is measured at the store interface, not inside the backends.** There are two
  backends, and the purpose of measuring is to compare the same control plane on both (§8.1), so
  the numbers must be comparable. `ErrNotFound` and `ErrCompareFailed` are not counted as failures:
  the control plane asks for both on purpose as ordinary answers, and counting them would turn the
  failure rate into a measure of how busy the reconciler is. `Watch` is timed for the call, never
  for its channel, because a watch's duration is the time between changes, not a latency.
- **`events_dropped_total` is labelled by where the loss happened**, because the cases are
  different problems (`internal/server/events/events.go`):
  - `ring`: an object's oldest entries aged out of its bounded ring (§12.1). This is expected in a
    bad hour and loses only old history.
  - `store`: a write of the ring to the store failed, so the batch was not recorded.
  - `contention`: the write kept losing its compare-and-swap to other writers and gave up, so the
    batch was not recorded.

  `store` and `contention` mean entries were never recorded. An agent whose in-memory queue
  overflowed before it could report does not increment this counter. The agent sends the count of
  lost entries with its next report, and the server records it as an `events_dropped` entry on the
  node's log (§12.1). *An earlier version of this bullet named `queue` and `ring` as the two
  values.* The metric's help text still lists `queue`, but no code path emits it.
- Instruments belong to each server instance rather than being package globals, because two servers
  in one process is a real configuration (§2.3, §17).

### 12.1 The event log

Everything in §11 is level-triggered and last-write-wins: it describes what is true *now*. An
operator debugging a failing path needs to know what *happened*, and without an event log the
control plane keeps none of that. A path that flapped for ten minutes and is `ACTIVE` again reports
nothing about those ten minutes. A request that went `PARTIAL` overnight and recovered by morning
looks the same as one that never changed. The log line that would explain any of it is in the
agent's log on the node, reachable only with shell access to a fleet member — the access that
centralising the control plane was meant to make unnecessary (§1).

**Settled: a bounded event ring per object, anchored on the path, stored as a snapshot rather than
appended to, and excluded from the fleet snapshot.**

The target output the design is measured against:

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

#### Store churn, and why events are written once per pass

An event log is the first edge-triggered, append-style component in a design that spends §6, §8.3
and §11.1 removing store writes. So its effect on the assignment long poll has to be checked, not
assumed. It is safe, for a reason that is not obvious from §9.2: the poll watches the node's
**own** assignment key and compares that key's `ModRevision`, so a write under a different prefix
does not wake it. Events do not trigger a fleet-wide reconcile.

Events do consume store revisions, and the sqlite backend bounds watch history by revision *count*
(§8.1). A writer that writes often enough can therefore compact away a long poll's cursor. The
agent recovers from that — it re-reads, and §7.3's already-correct test means an unchanged
assignment set restarts nothing — but it is store pressure the rest of the design avoids. This
decides the write granularity: **one write per reconcile pass, never one per event.**

#### One key per object, holding a ring

An object's events are a bounded ring inside a **single value**, rewritten on each flush. There is
not one key per event. This is the full-snapshot rule of §9.2 applied for a third time. An
append-only stream needs sequencing, gap detection, compaction and a garbage collector; a ring in
one value needs none of them. It is read with one `Get`, it is bounded by construction, and it is
removed by deleting one key.

**The ring is bounded by count, not by age: fifty entries per object.** An age bound seems more
principled, but it is the wrong choice here. The main case for this log is the overnight failure
an operator finds at 09:00, and an age bound would expire exactly those entries by then. The cost
of a count-only bound is that a path which failed last week still holds the ring that says so — a
small, fixed amount of store per object, on objects that are already one key each.

**Coalescing keeps the bound workable, and it also reads better.** Consecutive entries of the same
kind on the same object are merged into one entry with a count, a first-seen time and a last-seen
time. Forty-seven identical worker exits become one row that says the worker is flapping, which is
what an operator needs to read. They also no longer push out the establishment history that
explains them. Fifty entries is plenty once repeated events cannot fill the ring.

**A kind whose identity is in its message does not coalesce at all.** This exception is required
for correctness. The merge deliberately ignores the message, because "exited after 1.2s" and
"exited after 0.9s" are one worker failing twice. But an entry that *names* what it is about
records a different fact each time. Merging two such entries keeps the newest and silently discards
the other's contents. This was found in a live fleet, not predicted: four flows appearing in four
consecutive passes were rendered as `1 flow appeared: …b8d6c502 ×3`, naming one of the four and
losing three. Whether a kind coalesces is declared on the kind, not handled field by field in the
comparison, because the field-by-field approach is the one the next person adding a kind will
forget to update.

**The fleet snapshot excludes `/events/`.** §7.3's rule that "keys outside the three layers are
ignored" already provides for this. The exclusion is needed for performance: every user-API read
costs O(fleet) rather than O(response), because it loads the whole store and runs `Compute` (§7.3;
that cost is why it is an open item). Putting a diagnostic log into that key space would make every
unrelated read pay for it, including the reads a UI makes most often.

**Events are never an input to `Compute`.** They are a side effect of `Apply`, so the purity that
§7.5 depends on is preserved. A reconciler that read its own event log would let history influence
a decision, which §7.3 forbids outright.

#### The path is the unit of retention

**Settled: events are anchored on the path. A session is a *field* on an event, not a log of its
own.**

The failure being debugged belongs to the path. A request's state is a fold over its paths (which
is why `PARTIAL` had to exist, §11), and a session is ephemeral by definition (§3). A per-session
log would split the history at the point under investigation: a re-establishment is where one
session ends and the next begins, so the events on either side would end up in two logs, and one of
them would be deleted while the operator is reading it. Anchoring on the path also gives the log a
**stable key**: path IDs are derived deterministically and survive server restarts and leader
changes (§5.4, §7.3), whereas a session ID changes whenever the source flow definition does.

**A request has a small log of its own, and it is not a wrapper over its paths.** It holds events
that belong to the request and have no path to live on:

- an admission refusal;
- an expansion that changed (a selector that matched three flows and now matches two);
- a path lost to §7.5's precedence;
- a leg parked or un-parked (§9.1).

The case that shows it is needed is a request expanding onto **nothing**: there is no path, and
"why is this `WAITING`?" has nowhere else to be answered. A request's rendered view is its own
entries merged with those of the paths it currently expands onto.

**A node has a log too, and it is cheap because node events are rare:** registration and
re-registration, lease expiry, `node_claimed` (§6), interface probe results (§10.5), start-permit
saturation (§6.3). It answers "why did every path on edge-01 re-establish at 12:04?" in one line
instead of fifty identical path entries, and it is the log that still exists after the paths are
gone.

**Flows and domains have no log of their own, but their appearing and disappearing is recorded on
the *node*.** *This supersedes "a flow appearing and disappearing is inventory and belongs nowhere
here", which was argued from cardinality alone.* The cardinality concern is real. It is addressed by
choosing where the entries go and how they are batched, not by leaving the fact out. A flow that
vanished explains a request whose selector silently stopped matching, and a path that went
`WAITING` with no other explanation. An operator who cannot see the disappearance has to infer it
from an absence.

The following properties make it affordable. The last one was the one first gotten wrong.

- **It goes on the node's ring, not on a flow's.** A flow is not an object here (§3: a flow ID is
  not unique to a location), and most flows have no path. The node is what gained or lost them.
- **A disappearance is `info`, not a warning, and so is an appearance.** A producer stopping is
  normal in a fleet. It is the same fact `PAUSED` keeps out of the fault vocabulary (§11), and the
  same argument §11 makes again for `DISABLED`: if routine churn is shown as a fault, twenty
  non-problems read as twenty problems. Where a disappearance does cause something, that
  consequence is recorded on the object it affects, with its own severity: the request whose
  expansion shrank, the path that went `WAITING`. These entries record the fact, not a judgement
  about it.
- **Entries are batched per reconcile pass, never written per flow.** A node restarting removes
  fifty flows and brings fifty back (§14). One entry per flow would overwrite a fifty-entry ring
  twice, and would evict the registration entry that explains the whole episode. Instead there is
  one entry per kind per pass, which names what it can and counts the rest — the same format a
  request's excluded-flow list already uses.
- **It is the only part of the log with an on/off switch** (`--[no-]server-inventory-events`, on by
  default). It is the one part whose volume depends on the *fleet* rather than on the control plane: a node's flows follow whatever its producers are
  doing. A deployment where that changes constantly should be able to turn this off and keep the
  rest.
- **A node observed for the first time gets a baseline entry, not a flood.** A leader cannot report
  its first observation of a node as flows appearing: those flows may have existed for days, and
  saying they just arrived is the fabricated event storm that the takeover marker exists to avoid.
  The first implementation reported nothing instead. But silence looks the same as a node whose
  flows never appeared, and that is how the problem was found: an operator reading a source node's
  log after an incident saw nothing and concluded the feature was broken. So the leader now records
  where its knowledge begins, e.g. `first observed holding 4 flows in 1 domain`. This entry is
  emitted on the pass *after* the seeded one, on purpose, so that whether a node gets one does not
  depend on a race between its first inventory report and the settling window.
- **A node that is not leased is skipped entirely.** Its inventory is leased state, so it vanishes
  from the snapshot as soon as the lease expires. Diffing against that snapshot would report every
  flow on the node as disappeared at a moment when nothing happened to any of them. This is §4.2's
  closing rule applied one layer up: "no observation" never means "nothing there", and the correct
  response is to freeze rather than converge. The node's inventory memory is carried forward, in the
  same way its paths' assignments are, so its return is not reported as a flood of appearances
  either.

**Limitation: a flow that appears and disappears between two passes is not recorded.** The journal
is level-triggered like the rest of the system: it compares snapshots rather than watching a
stream. A producer that flaps faster than the reconcile cadence does not show up at all. That is
the right trade-off, because the alternative is an edge-triggered watcher on the highest-churn
state in the system.

**A path's log is deleted with the path, and a request's log with the request.** There is no
tombstone and no grace period. Keeping a log after its object is gone would need a second
lifecycle, a TTL and a sweeper, all for one question — "why did the thing I deleted fail?" The node
log still answers that question, and it is asked after a deliberate delete, not during an incident.

#### The two producers, and the leader's takeover gap

**The leader emits events from `Apply`'s diff, which already exists.** `Apply` computes what
changed in order to write it; an event is that diff rendered for a person instead of for the store.
Only the leader reconciles (§8.2), so there is one writer and nothing to de-duplicate.

Path *state* is derived, not stored, so state transitions are **not** in that diff. Detecting them
requires the previous pass's computed states, which live only in the leader's memory. The
consequence is stated here up front: **a newly elected leader has no baseline, so its first pass
emits no state transitions and writes one `reconciler_took_over` entry. The gap is marked, so it
does not read as a quiet period.** The alternative, emitting every current state as though it had
just changed, would produce a fabricated storm of events on every leader change and every server
restart. That is the mistake §7.3's settling window exists to prevent, one layer up, and the remedy
is the same: a server that has just started must not treat what it sees for the first time as
something that just happened.

**Agents never write the store (§4), so agent events reach it through the agent API**, and not in
the status snapshot, for the reason §9.2 gives. `POST /agent/v1/{node}/events` carries a batch from
a bounded in-memory queue. The queue is drained on send, delivered at-least-once, and
de-duplicated on the server by a per-agent sequence number. The agent contributes what the server
cannot see: why a worker exited, that a start is queued waiting for a permit, that an assignment
could not be carried out (§6), and the log tail of §12.2.

**The agent holds no persistent state (§6.1), so an agent restart loses any pending events, and a
full queue drops its oldest entries.** Both losses are accepted, and both are reported: an overflow
emits an `events_dropped` entry with the count, on the same principle as the leader's takeover
marker — a gap in this log is always recorded in this log. It follows, and needs saying before
anyone builds on it, that **this is a diagnostic aid, not an audit log.** It is not a complete
record of what happened; it is the best account that two processes with bounded memory can give.

#### Reading, ordering and vocabulary

`describe` shows an object's log under its status, and the three `events` endpoints serve it
(§9.1).

**A withdrawal is recorded only when its path survives it.** A session removed by a long-idle
teardown (§11.1) or by a lost conflict leaves a path with no session, and nothing in its status
shows that this changed; that is the case the entry is for. Other cases are not recorded:

- When a request is deleted, its paths go with it, so the entry would be written to a ring that is
  deleted in the same pass.
- A *rebuild* — a new epoch, a republished definition — replaces the session and is already
  reported as an establishment. Recording the withdrawal too would put two entries on every target
  restart.

**The takeover marker is written once and merged into every read.** A leader change leaves a gap in
every object's log, but the entry explaining it belongs to no single object. It is written to a
separate fleet ring, and each read merges that ring into its result. That costs one write, visible
wherever the gap is, instead of a thousand writes into every path's ring at the moment the fleet is
already busiest.

**Two rules limit that merge. Both exist because the marker claims that transitions before it were
not recorded, and that is only true for some objects.**

1. **An object with no entries of its own reads as empty.** Otherwise a path whose log was deleted
   with it would come back holding the control plane's entries, and a deleted object would look as
   if it still existed.
2. **Only fleet entries within the object's own lifetime are merged.** A takeover that happened
   before a path existed cannot have lost any of *its* transitions. Showing the marker there would
   tell an operator to distrust a log that is actually complete. The lifetime is taken from the
   oldest entry the object still holds. This errs on the safe side: if a ring has dropped its
   oldest entries, a marker from before that point is hidden, but that gap is already reported by
   the ring's own dropped count in the same read.

Each entry has a **per-ring sequence number that increases monotonically, starting at one.** It
orders the ring, and a poller resumes from it. Starting at one rather than zero matters: a cursor of
zero means "everything this ring still holds", and a reader resumes from entries above its cursor.
An entry numbered zero would look already seen and would be filtered out of the first read of every
ring.

**Timestamps are for display only.** Each entry is stamped by whoever emitted it, so a request's
merged view mixes the clocks of two agents and a leader. TAI correctness is a deployment assumption
(§11.1), but it is an assumption about clock offsets, not about ordering across hosts. A log that
implied otherwise would invite an operator to infer causality from two nodes' timestamps.

**The vocabulary of event kinds is closed, and every kind in it is emitted by something.** Two
candidate kinds were defined and then removed rather than left unused. Both failed for the same
reason: the thing they would describe happens where no ring exists to hold it.

- **`request_rejected`.** A refusal at `POST` happens *before* the request is written (§7.2). The
  handler computes against a candidate fleet, finds the request structurally invalid and returns 400
  without creating anything. With no request, there is no ring. Every refusal that can be recorded
  has a request behind it, and appears as a state change to `INVALID` carrying the code that
  refused it.
- **`interfaces_probed`.** The probe's result *is* the registration body (§10.5). The server
  already has every number such an entry could carry and records them on the registration entry, so
  a separate entry would record the same fact twice, one store write apart. A probe that fails
  cannot be recorded at all: the agent has no lease, so it is not registered and has nothing to
  report through. That case shows up as a node that never appears.

The general rule behind both: **an entry needs an object that exists at the moment it is written.**
A kind with no such moment is not a missing implementation but a design error, and keeping it
defined would send someone looking for an emission site that cannot exist.

**Kinds form a closed vocabulary, and reason codes are §7.2's, not a second set.** Free text cannot
be queried, translated or coalesced on. The message is the human-readable rendering of a kind and
its fields, computed when it is displayed. §11 applies the same rule to status reasons, and the two
vocabularies must stay the same: an event about a path going `INVALID` carries the code that path
is reporting.

### 12.2 Worker log tails

**Settled: the agent keeps a byte-bounded tail of each worker start's output and pushes it with the
transition into `FAILED`.** *This supersedes the §19 roadmap item "worker log retrieval through the
API". It is now built, in the narrower form described here: a tail attached to a failure, not a
general log-retrieval facility.*

The line that explains a failure is usually the worker's own. `fatal: unknown error: failed to
create flow writer` says in one sentence what `FAILED` / `worker_restarts` cannot say at all.
Without log tails, that line exists only on the node.

**The tail is captured where the agent already reads every output line.** The agent re-emits worker
output through its own logger, parsing spdlog's format and passing through lines it does not
recognise (§12). A ring buffer next to that costs one buffer per running worker and no new
plumbing. Because the parser keeps unrecognised lines rather than dropping them, the tail also
includes whatever a linked library printed on its way out, which a tail of only parsed lines would
miss.

**Bounded in bytes, not in lines.** An error message that contains a flow definition is a line the
size of a flow definition (§15), and under a line budget one such line could push out a whole
start's history.

**The tail, not the head.** A worker's fatal line is its last one in both failure cases: a worker
that never comes up, and one that dies after hours of healthy transfer. The cost is accepted: a run
with `FI_LOG_LEVEL=debug` sends libfabric's own diagnostics through the same logger (§12) and can
push the setup lines out of the window. Those lines can be reproduced; a fatal error often cannot.

**The tail is pushed on the first death of a crash loop, not on each restart.** Forty-seven
restarts produce one tail. This is §12.1's coalescing rule applied to the payload rather than to
the entry, for the same reason: the forty-seventh copy of the same message adds volume, not
evidence.

**Pushing is re-armed by time-to-death, not by the worker having reached ready.** The obvious rule
is wrong in a way that is easy to miss. A target binds, writes its blob and really *is* ready
before it dies on a timeout, so it reaches ready on every turn of the loop. Re-arming on ready would
push a tail on every restart. The signal that works is the one §15.1 already classifies from: a
worker that ran for a while before dying is a new incident, and whatever killed it after minutes
of healthy transfer is not what the first attempt's output describes.

**The tail is stored in its own key and fetched by its own endpoint.** The event carries a marker
saying a tail exists; `GET /v1/paths/{id}/logs` returns it. Putting a few KiB per failure inline
into the ring a UI polls would make that cheap read expensive during failures, which is when it is
read most.

**The capture size is set on the agent, and the accepted size is set on the server.** These are
two different limits, not two settings for one thing (both default to 8192 bytes:
`--agent-log-tail-bytes` and `--server-log-tail-bytes`). The capture buffer is a property of the
host, like the port range and the start rate (§6.2). The cap on what the endpoint will store is a
property of the store, and it has to exist independently: an endpoint that accepts unbounded bytes
from a node would let any fleet member fill the store. Anything over the cap is truncated at the
head, keeping the end, and the response says so.

**A note on disclosure, since §12 made the opposite choice for `/metrics`.** Worker output contains
filesystem paths, and §12 removed host paths from `/metrics` because `/metrics` is commonly
unauthenticated (§13). The event log and the tail endpoint are on the authenticated user API, so the
exposure is different and the two decisions are consistent. They look contradictory, which is why
the reasoning is written down: the rule is not "host paths are secret", it is "an unauthenticated
endpoint publishes to anyone who can reach it".

---

## 13. Security

This section states the threat model explicitly, because the model changed when the control plane
became centralised.

**Scope:**

- **A single shared bearer token**, configured on the server and on every agent. It is optional:
  running without auth is supported on a trusted network and for development.
- **TLS is optional.** Either the server terminates it, or an HTTP proxy in front of the server
  does.
- **No mTLS.** This is deliberately deferred. Distributing and rotating certificates across a
  DaemonSet is a larger operational commitment than this project should make before users ask for
  it.

The token check is middleware. It puts the identity it establishes on the request context, so
per-node credentials or mTLS can be added later without changing the handlers.

The threat model is written down so that the deferral is a recorded decision rather than an
oversight:

- **The agent API is the privileged one.** Anything that can call it can claim to be a node,
  inject fabricated flow inventory, and read other nodes' `target_info`, which contains RDMA rkeys.
  With a shared token, any holder can impersonate any node. Per-node credentials are the first
  upgrade if that matters.
- **The user API controls resource use.** A replication request moves uncompressed video between
  hosts, so an unauthenticated user API lets anyone exhaust bandwidth across the whole fleet. This
  is the main reason to turn the token on outside a trusted network.
- **Applying a domain label is as powerful as writing a request** (§10.7). Source selectors match
  labels, so labelling a domain can add it to an existing request's expansion and start moving
  media without anyone changing a request. With one shared token this changes nothing, because a
  token holder can already do both. It is recorded because it rules out a simple per-credential
  split between "may provision a node" and "may route media", which is the first separation
  operators ask for after per-node credentials. If that split is ever wanted, labels and requests
  must be authorised separately.

  Labelling does **not** widen the perimeter. A label has no effect unless the node already
  reports a domain by that name, so a label can only name something the host already exposed
  through an area that grants `read` (§10.7). *An earlier version of this bullet also said labels
  were refused on domains under an output root. That is no longer true, and nothing here depends on
  it: a domain this project writes into is reported like any other, and labelling it still only
  names something the host exposed.*
- **A destination is always a name inside an area the operator granted `write` on** (§7.2, §10.6).
  This is the most important invariant in the design. It is what stops the API from being a remote
  arbitrary-filesystem-write, and it holds whatever authentication is configured. A node with no
  writable area cannot be a destination.

  *This is the same invariant as the earlier "always a name inside an operator-configured output
  root", restated for areas.* Merging search paths and output roots into one concept (§10.6) did
  not widen it:

  - there are still two grants;
  - they are still independent of each other;
  - they are still configured on the node;
  - the API can set neither.

  The only change is where the agent finds the answer to "may this be written". It used to follow
  from which of two tables an entry was in. Now it is a field on the entry, and the agent checks it
  explicitly.

  Once an area grants writing, the API can do more than a supervisor that only started processes
  could, and this is a real escalation: the server can cause directories to be created on that
  node. Those directories are:

  - confined to the area by construction;
  - named by a list of validated path elements, up to eight of them, so the server can reach a
    bounded tree inside the area rather than a single directory (§10.6);
  - filled only with MXL flows.

  So the server's reach is no longer "processes only". The `write` grant is the entire perimeter.
  It is one line of node-local configuration, owned by the host and not by the control plane. That
  is the right owner, and it is why the API cannot set it.
- **Discovery is not a grant, and reporting output domains does not create one.** Domains this
  project writes into are now discovered and reported like other domains (§10.6). That increases
  what a scrape of the user API reveals about a node, and nothing more. Reading is still limited by
  the `read` grant and writing by the `write` grant. The guard against replication feeding its own
  output was not dropped; it moved to the flow level (§10.7).
- **Admission control is a likely future policy hook.** `docs/third_party/mxl/FabricsBandwidth.md`
  gives the exact wire bandwidth of a flow from its flow definition. The server could therefore
  compute committed bandwidth per node and per link, and refuse requests over a configured budget.
  This is not implemented; the request path is structured so it can be added (§19).

### 13.1 Version skew

**Settled: the server is always upgraded first.** The server tolerates agents that are one or more
versions behind it. An agent may assume the server is at least as new as itself. This matches how
the project is deployed, since a Deployment rolls out faster than a DaemonSet. It also means new
assignment fields must be additive, so that an older agent can ignore them.

**Settled: the gate is the protocol version, not the build version.** Agents report both versions
at registration. The server:

- exports the fleet's spread of versions as a metric (`mxl_repl_agent_versions`, §12);
- logs a warning for an agent whose protocol version is behind its own;
- **refuses** an agent whose protocol version is newer than its own, because that is the one
  direction the compatibility promise does not cover.

A hard refusal keyed on the build version would be impossible to satisfy on a combined instance
(§2.3). Upgrading a combined instance upgrades both its roles at once. During a rolling upgrade of M
combined nodes, the fleet therefore has older and newer servers at the same time, and a newer agent
can reach an older server. Two measures handle this, and both are in place: the gate uses the
protocol version, and the co-located agent connects to its own server over loopback, which is by
construction the same version.

---

## 14. Scale

**Settled: one process per flow per direction is fine.** A worker's overhead is small compared with
the processing that downstream media functions do on the same flows, so the number of processes is
not the limiting factor at the scales considered. A node receiving 50 flows runs 50 target
processes. Each one mmaps and RDMA-registers memory and holds a metrics socket. That is acceptable.

**Amended: the steady-state count is fine; the start-up burst is the limit.** Fifty running
workers, the case this section sized, is still fine. Fifty workers starting **at the same moment**
is not. This was found in production, not predicted here. Memory registration allocates pinned
pages against a host-wide limit, and that cost is paid at start. The fix is to limit the start
rate, not the number of workers: §6.3 paces starts. It is recorded here because this section
originally missed the distinction. What a node can hold and what it can start at once are two
different capacities, and only the first was sized.

This could change. If a deployment ever needs **hundreds** of flows per node, one process per flow
becomes the limit, and the answer is a worker that handles several flows. The design makes that a
replacement of one module rather than a rewrite:

**The worker is a replaceable module.** The agent talks to it through an interface: start a
transfer for this session with this config, tell me when it is ready and what its `target_info` is,
give me its metrics, stop it. It does not make `os/exec` calls scattered through the supervision
code. Nothing above the interface assumes one process per session, a work directory on the
filesystem, or an `AF_UNIX` metrics socket. Those are properties of this particular worker
implementation, and no production code uses `os/exec` outside `internal/worker/exec`.

This is not built only for a hypothetical future. The same interface is what makes the control
plane testable without MXL or RDMA hardware (§17). It is useful now, and being ready for a
multi-flow worker is a side effect.

---

## 15. The worker

**Settled: the worker source may be modified.** It lives in this repository under `src/` and is
built by the top-level `CMakeLists.txt`. "Reusing the worker" means not rewriting it; it does not
mean never changing it. `docs/worker-runtime-surface.md` is the contract document. Any commit range
that changes the contract must update that document too.

The design required three changes to the worker. All three are in place:

1. **A configurable no-grain timeout.** `idle_timeout_ms`, default 10000; `0` or a negative value
   means wait indefinitely. Without it, `PAUSED` cannot be held for long: every idle replicated
   flow causes a control-plane round trip about every 13 s, forever (§11.1).
2. **An interface probe mode.** `--interfaces` calls `mxlFabricsGetInterfaces()` and prints the
   result as JSON (§10.5). This is how the agent learns what the node can actually do, instead of
   guessing from `/dev/infiniband` and interface names. It only adds a mode and does not touch the
   transfer path.
3. **The negotiated interface config.** The config carries `caps_flags` and `max_message_size`
   next to `provider`, and the worker passes them to `mxlFabricsTargetSetup` and to the initiator
   setup. Both ends must be given the same values, because the library does no negotiation itself
   (§10.3). If they are absent, the library default applies, so an older config still works.
   **`caps_flags` is an array of the same names the probe prints** (`REMOTE_WRITE`,
   `SEND_RECEIVE`, `BLOCKING_OPERATIONS`), not a bitmask. The intersection in §10.3 is then a set
   operation over one vocabulary from probe to config, with no translation between bits and names.

Smaller fixes made at the same time:

- `connect_timeout_ms` on the initiator's connect loop. Without it, the initiator waits forever for
  an unreachable target.
- `unlink()` on the metrics socket before `bind()`.
- A `return` after logging a non-interrupt `mxl::Exception`. Previously the code fell through to
  `return 0`, so the worker printed `fatal:` and then reported success.
- A shadowed variable that made `mxl_grains_lost` always read 0 on the initiator.

Two more were found by running the worker:

- `target-info.json` was written with a **trailing NUL byte**, because the library's reported size
  includes the terminator. `encoding/json` rejects that. The worker now strips it. The Go decoder
  still trims one trailing NUL, so that in a mixed-version deployment an older worker's file does
  not look like a corrupt blob.
- An over-long `metrics_socket` path was silently truncated to fit `sun_path`. Two workers under a
  long parent directory then bound the same truncated path, and the second one died with
  `EADDRINUSE` for a socket path it had never been given. This is now a clear `ENAMETOOLONG` at
  startup.

### 15.1 Why the exit code is not a status signal

Using the worker's exit code to classify failures looks attractive, but it does not work. This
section records why.

**The agent already knows a death was unexpected, because the agent did not send the signal.** The
exit status adds nothing to that. A plain change from exit 0 to non-zero does not classify
anything either. `mxl::Exception` covers permanent errors (invalid config, a bad provider) and
transient ones (timeouts, the startup race where the flow is not yet found), so both kinds end up
with the same exit code. Useful classification would need a distinct exit code per error class.
That is a much larger change, and nothing here needs it. The exit-code fix in §15 (the `return`
after `mxl::Exception`) is therefore not something the status logic depends on. It was made
because reporting success after printing `fatal:` is simply wrong.

The signals that do work are based on behaviour, and the agent computes all of them:

- **Restart rate over a window** → `DEGRADED`. A worker that keeps restarting is degraded,
  whatever exit code it returns.
- **Time to death.** A worker that dies within a second on every attempt has a permanent error:
  bad config, missing domain, incompatible provider. A worker that dies after minutes of healthy
  transfer has a transient one.
- **Source liveness from the head index** (§11.1) → `PAUSED`. This is direct evidence that the
  producer stopped, not an inference from a worker's death. It is the better signal for the idle
  case whatever the worker reports, because it is available even when no worker is running.

One overlap is harmless: the agent has decided to tear a session down, and in that short window
the worker exits on its own for an unrelated reason. The result is a brief `DEGRADED` that the next
reconcile clears.

---

## 16. Relationship to `mxl-fabrics-proxy`

`mxl-replicator` replaces `mxl-fabrics-proxy`. In the proxy, each node had a static subscription
list, fetched flow definitions from its peers over HTTP, and exchanged `target_info` directly with
peers under a 9 s keepalive with a 20 s expiry on the far side. **That proxy is retired.** There is
no wire compatibility with it, and none was planned.

**Settled: there is no config compatibility for the operational half either.** *An earlier version
of this section promised a one-shot importer that would read a legacy `config.yaml` and emit
requests, and gave the manifest format the partial job of staying expressible from the old
format.* That was dropped before v1. The legacy file is a per-node config, its subscriptions are
addressed by `mxl://` URL, and its destination is a `-m` mapping. This design deliberately changed
all three: intent is fleet-scoped, sources are selectors, and destinations are names inside a
writable area (§10.6). A format kept convertible from the old one would carry constraints that no
longer apply, only to save hand-editing on deployments small enough to rewrite in an afternoon.

What carried over is the **provisioning** half, and it explains why a few things look the way they
do:

- ~~**The domain mapping config.**~~ **Settled, superseding this bullet: `-m` and the `domains:`
  YAML block are removed, and there is no domain-mapping compatibility with the retired proxy
  either.** *The earlier position kept the `-m name=/path` syntax byte-compatible, including the
  legacy spellings. The reasoning was that "it costs nothing to keep — it changes when a host is
  built, not when a flow is routed."*

  Both parts of that reasoning turned out to be wrong.

  - **It did cost something.** It forced an exception into §10.6 and a second rule to enforce that
    exception. It needed a `domain_path_in_use` rejection code, checked on both server and agent.
    And it kept a `Configured` flag on the wire after the flag no longer served any security
    purpose.
  - **It does change when a flow is routed.** Naming a domain is the one item on the agent's
    configuration list that an operator does while routing rather than while building a host.
    Doing it in agent config meant an agent restart, which re-establishes every flow on the node
    (§6.1).

  Once keeping it has a cost, the argument goes the other way. Domains are discovered under a
  readable area and named with API labels (§6, §10.7). A legacy mapping used as a subscription
  destination becomes a domain name inside a writable area; one used as a source becomes a label.
  The naming rule survived even though the syntax did not: the same grammar now governs a domain's
  elements, an area's name and a `name` label's value (§10.6).

  *A later note on the same argument.* The exception `-m` forced into §10.6 has now been removed
  twice. Removing the mapping removed it once. Merging search paths and output roots into areas
  then removed the rule it was an exception to. All that is left of it is one rule: areas may not
  share a path.
- **`mxl_*` metric names are unchanged**, so existing dashboards and alerts keep working. That
  matters more than naming consistency with the control-plane prefix (§2.2). The `session` label is
  new but additive, so existing selectors keep working.
- **The default server port is 2283**, the port the proxy used.

An importer is still possible. It would be better written against the manifest format than against
the API, because its output could then be reviewed before it is applied, which a one-shot design
would not allow (§19). If it is ever written, `defaults.provider` must become a request-level pin,
not a wider default. In the proxy it was a per-side setting; a provider is now negotiated per
session against the fabric attachments each node declares (§10). Silently widening what an existing
deployment asked for is the substitution §10.4 forbids.

---

## 17. Testing

**The worker launcher is an interface with a fake implementation, by design.** This is what makes
the entire control plane testable without MXL, libfabric or RDMA hardware. It is the same interface
that keeps the worker replaceable (§14), so one abstraction serves both purposes.

The fake worker is combined with `mxl-utils`' `pkg/testutil`, which builds synthetic flows on disk
that `pkg/mxl` can open, including the delete-and-recreate case that `IsValid` detects. Together
they exercise the whole control plane in a temp directory. The tests cover:

- discovery → inventory → request → path → session → assignment, end to end, in-process;
- epoch changes and initiator convergence (§5.2), by having the fake target return a different
  `target_info`. Also the edge case the nonce exists for: a fake target that returns a
  **byte-identical** `target_info` after a restart must still make the initiator reconnect;
- both storage backends against the same conformance suite, written against sqlite and run
  unchanged against a real etcd;
- an HA leader change in the middle of a reconcile, and a server restart with the settling window
  (§7.3). When the fleet is already in the desired state, this must cause **no** worker restarts;
- **fail-static behaviour (§4.2)**: both when the server is unreachable and when it answers
  not-ready, the agent must skip the reconcile entirely rather than reconcile against an empty set;
- **incidental differences restart nothing** (§7.3). The perturbation is applied on the wire, not
  in memory, because that is where it comes from in production;
- selector expansion (§9.1): a group-hint request gaining and losing paths as `testutil` creates
  and removes matching flows;
- fan-in (§9.1):
  - two sources on different nodes into one destination domain, producing two paths, one
    materialised domain and one target worker for each path;
  - a pairing that fails validation marking only its own leg invalid, while the request's other
    legs are established;
  - the reason naming the correct end, which is three cases: the **source** when the failure is
    common to every destination of one source; the destination when it is common to every source
    of one destination; and neither when it applies to every pairing. All three are tested because
    naming the wrong end is the failure this rule prevents, and a test that only checks the
    message is non-empty would pass anyway;
- the corruption case (§7.2, §7.5): two sources pinning one flow UUID into a shared destination are
  refused at `POST` as `duplicate_source_flow`. The same collision arriving later through a
  selector is classified `flow_conflict` on the path; the losing path is torn down, and the reason
  names **both** sources, not only the winner;
- `same_endpoint` over the cross product (§7.2): a request whose source and destination sets
  intersect is refused, with both indices in the message. This also tests that a cycle inside one
  request cannot be written, because a cycle always puts some endpoint on both sides;
- the `all` selector (§9.1):
  - a source with no flow selector replicates every flow in a named domain, and gains a path when
    a producer adds a flow;
  - the same selector against a label-matched domain excludes this node's own output, while a
    locally produced sibling flow still matches;
  - an absent `select` on the **wire** is refused, while the same omission in a manifest is filled
    in. The manifest is the one place the default may be applied;
- `PARTIAL` (§11), four cases:
  - a request with one dark source out of three reports `PARTIAL` rather than `PAUSED`, with the
    counts and per-source breakdown beside it;
  - a request with one `INVALID` leg and working paths elsewhere still reports `PARTIAL`. This is
    the property that §7.2's per-path validation would otherwise lose at the top-line status, and a
    worst-state-wins fold would pass every other test without having it;
  - a request with no `ACTIVE` path never reports `PARTIAL`;
  - `PARTIAL` never appears on a path, a session or a worker;
- materialisation (§10.6):
  - a destination directory that does not exist before the request is watched immediately after
    it is created, so the path can reach `ACTIVE`;
  - two requests sharing a destination materialise it once;
  - a domain in a read-only area is refused as a destination;
  - every rejection is made by the server and, independently, by the agent;
- areas and naming (§10.6):
  - a domain under nested areas is named by the innermost area;
  - a directory that is both reported by discovery and materialised by the reconciler resolves to
    one name and one inventory entry, in either order. This includes the case pruning used to
    handle: a leftover directory holding a flow, discovered before the assignment that
    materialises it, must reach `ACTIVE` rather than stay stuck in `ESTABLISHING`;
  - a materialised domain stays in inventory when its last flow is released while a session still
    targets it, and leaves only when both discovery and the reconciler have released it;
  - an area pointed at a different directory keeps every domain identity on the node, so paths and
    sessions survive the restart instead of being rebuilt;
- domain labelling (§10.7):
  - a label applied before its domain is discovered takes effect by itself when a producer
    appears;
  - a relabel changes a request's expansion **without** restarting a worker on a path the request
    still matches. This is the property the decision to annotate rather than rename exists for;
  - a label on a domain in a write-only area is accepted and has no effect (no test yet;
    open-items §2.13);
  - a label selector does not match a flow this node is writing, while a locally written sibling
    flow in the same domain still matches, and a request naming that domain directly still
    chains;
- label selector semantics (§10.7):
  - two keys are ANDed: a domain carrying only one of them does not match;
  - a selector with no keys is refused by the validator, not only by the manifest's scalar-or-map
    rule;
  - a selector that matches the request's own destination domain drops that pairing
    while the rest of the expansion stands, whereas the same pairing written with a named source is
    refused as `same_endpoint`;
- label ownership (§9.1), three separate cases:
  - an apply **leaves** a key that an imperative `label` added and the file never declared;
  - an apply **removes** a key it declared on an earlier pass and no longer declares;
  - an apply leaves a domain the file does not name completely untouched.

  The first case is the reason the three-way merge exists, and a whole-set replace would pass every
  other test without having it;
- label writes (§9.1):
  - `?dry_run=true` writes nothing and returns the paths a label removal would stop;
  - the same removal done for real reports, for each stopped path, whether another request still
    references it (only the dry-run form is tested today; open-items §2.13);
  - a `label` patch and a concurrent apply both take effect, where a read-modify-write would have
    lost one of them;
- exclusion reporting (§9.1, §10.7): a request whose selector matched a flow this node is writing
  lists that flow in its status with reason `self_output`. A flow that simply did not match the
  labels is not listed at all. A truncated list reports how many entries it dropped;
- self-amplification (§10.7, §11.1):
  - a broad label selector on a node that is also a replication destination expands to a fixed set
    of paths and does not grow on later reconciles;
  - the provenance gap: an agent restart briefly makes a replicated flow report
    `replicated=false`. This must start nothing, because the same restart makes the flow
    non-producing and admission holds it back (no test yet; open-items §2.13);
- conflict precedence (§7.5): a path that has been `ACTIVE` for a while wins over a newly matched
  path from an *older* request, which is the case the oldest-first rule got wrong. When the winner
  is deleted, the loser is established by itself, with no stored suppression state. No such test
  exists, and it would fail today, because the code still orders conflicts oldest-first (§7.5,
  open-items §2.12);
- namespaces (§9.3):
  - overlap is permitted in a `shared` namespace and refused in an `exclusive` one. The losing
    request reports `INVALID` naming the request that holds the path, and no media stops;
  - two requests with the same name in different namespaces coexist;
  - a namespace is auto-created on first reference, and deleting it is refused while it is
    referenced;
- the `WAITING` → `ESTABLISHING` → `PAUSED` → `ACTIVE` progression, using `testutil`'s
  `UpdateRuntime` to drive the head index so `PAUSED` and `ACTIVE` can be told apart;
- the matched settings of §5.5 reaching both workers identically;
- inventory events (§12.1):
  - a flow appearing and disappearing on a node is recorded in that node's ring and not in any
    path's ring;
  - fifty flows arriving at once produce **one** entry that counts the rest;
  - the case the feature would most easily get wrong: **a node losing its lease reports no
    inventory change**, and neither does its return, because leased state disappearing is not the
    same as the flows disappearing (§4.2);
- the event log (§12.1), four properties, of which only the first is about content:
  - a path's transitions are recorded in order and coalesced when they repeat, so a flapping
    worker is one entry with a count, not fifty entries;
  - a **leader change emits no state transitions and one takeover marker**. A naive differ would
    fail this by inventing a burst of transitions;
  - deleting a path deletes its ring;
  - **a reconcile that changes nothing writes no events**. This guards everything else: the final
    bullet of this list is worthless if the event log itself breaks it;
- log tails (§12.2):
  - a fake worker that dies printing a `fatal:` line makes that line readable from
    `GET /v1/paths/{id}/logs`;
  - a crash loop produces **one** tail, not one per restart;
  - a tail over the server's size cap is truncated at the head, so the fatal line survives;
- **a second reconcile of an unchanged fleet writes nothing and does not move the store
  revision**, which is the property all of §8.3 depends on.

For real end-to-end coverage, `shm` or `tcp` on loopback runs the actual worker on a single host
with no special hardware. `mxl-mock-src` and `mxl-mock-sink` produce and consume real flows, so the
replicated payload is checked **byte for byte**, not just observed to be advancing.

---

## 18. Decision record

This section lists every settled question in one place. Each entry states the decision, names the
section that argues it, and gives the short reason. The rest of the document describes the design;
this section records what was decided.

**Identity**

- The project is **`mxl-replicator`**, and the worker binary is `mxl-replicator-worker` (§2.2).
  Metric prefixes are `mxl_` for flows and `mxl_repl_` for the control plane.
- **Roles are selected by flags on `run`, not by subcommands** (§2.2). This replaces the document's
  earlier position. `--server` and `--agent` are plain on/off flags: naming one role runs only that
  role, and naming neither or both runs both. A combined instance still uses HTTP between its two
  roles, so there is one code path.
- **There is no separate CLI binary** (§9.1, §19). `apply`, `delete`, `label`, `status`, `get`,
  `describe`, `events` and `logs` are subcommands of `mxl-replicator`, beside `run`. Each read verb
  has one job, and `describe`/`get` keep `path` and `session` separate because §4 treats them as
  separate layers.
- **A domain is named `<area>/<elements>`, and that name is its identity for as long as it
  exists** (§6), replacing "an input domain is named by its path". The area is the part the
  operator chose and the elements are the part the filesystem fixed, so nothing has to be invented
  or stored, which matters because the agent holds no persistent state (§6.1).
- **There is no config compatibility with the legacy per-node config** (§16). The promised one-shot
  importer was dropped before v1: intent is now fleet-scoped, sources are selectors and
  destinations are names inside a writable area, so a format kept convertible from the old one
  would carry constraints that no longer apply.

**Protocol and state**

- **The epoch is a content hash plus an incarnation nonce, not a counter** (§5.2). It is computed
  over `fabricAddress`, `regions[].{addr,len,rkey}` and `bounceBufferInfo`, and written as
  `<nonce>:<sha256 hex>` so the initiator can check a blob against it. This ties the project to the
  internal structure of `TargetInfo`. That is acceptable because mxl-fabrics shares maintainers
  with this project, and both sides check for drift. The epoch is owned by the target side, which
  owns the fragile resource.
- **The nonce is carried as a plain prefix and is also inside the digest** (§5.2). An earlier
  formulation hashed the nonce into the digest only, and also said the initiator could recompute
  the epoch from the blob. The initiator never sees the nonce, so it could not; the prefix makes
  the check possible without a new wire field.
- **The agent sends an inventory or status snapshot only when it differs from the last one it
  sent** (§6). This is required, not an optimisation: every store write wakes every agent's
  assignment long poll, so an agent that re-sent unchanged snapshots would keep the whole fleet
  reconciling. Both snapshots are sorted deterministically, and the cache is dropped on
  re-registration.
- **`Compute` is one pure function of one snapshot, and the read handlers and the request `POST`
  run it too** (§7.3). Request-time rejection, steady-state classification, follower reads and
  `?dry_run=true` therefore cannot disagree.
- **Agents are fail-static** (§4.2). A server outage never stops running media, and an agent acts
  only on an assignment set it actually received. The server follows the same rule: it never
  reports "not ready" as an empty set, the reconciler does not act while agents hold leases but no
  observed state has arrived, and paths that touch a node that is not live are frozen instead of
  converged.
- **After a restart the server waits a settling window before its first reconcile** (§7.3). The
  window is a multiple of the heartbeat interval, agents report their full running state, and
  readiness is published as a record that every replica reads. As a result a server restart or a
  leader change does not interrupt media, and a wiped store is reported as a wiped store instead of
  looking like a fleet-wide cancellation.
- **A media glitch on agent restart is acceptable** (§6.1). The agent kills its workers and
  re-establishes them; it does not adopt them, and it holds no persistent state. The effort goes
  into making the restart fast instead.
- **Session identity is `(path, flow-def hash)`, and the path ID excludes the flow definition**
  (§5.4). A republished flow therefore rebuilds the session without resetting the path. The path ID
  no longer includes the resolved output root as a separate term, because the area is now the first
  segment of a domain's name.

**API**

- **The source of a request is an extensible selector** (§9.1), with flow-ID, group-hint and `all`
  kinds. A request owns a set of paths, even when it selects a single flow.
- **`all` selects a whole source domain, and a manifest writes it by leaving `select` out** (§9.1).
  On the wire `all` is an explicit kind like any other, and an absent `select` is still an error: the
  tagged union exists so that a zero value can never mean "everything". This does not contradict
  §10.7's refusal of an empty domain selector. That selector would match an unbounded and growing set
  of places; `all` selects within one place the operator already named.
- **A request fans in as well as out: both ends are lists, and each source's node stays pinned**
  (§9.1). This replaces "a request fans out, not in". That position had four arguments:
  - one was about which side carries the list, and never applied across nodes;
  - one turned out to point the other way, because fan-in groups the ingress that a destination
    shares;
  - two still hold, as requirements met elsewhere: shared fate by `PARTIAL` (§11), and the
    corruption case by `duplicate_source_flow` and per-path `flow_conflict` (§7.2).

  Nothing below the request changes: paths, sessions and refcounting are the same. Validation and
  negotiation run per `(source, destination)` pairing. A destination may override the provider pin;
  a source may not, because a source's pin cannot be satisfied per pairing and one side already
  determines the pairing's provider.
- **`sources` is always a list, with no singular spelling** (§9.1). A form accepting either a scalar
  or a list was possible and is refused: unlike `provider`, the singular would be the common case,
  so both spellings would be in daily use. Every stored request and manifest written the old way
  becomes invalid; this break ships in the same major version as §10.6's re-identification (§16).
- **A destination entry carries `disabled`, and that is the only place a request can say "off"**
  (§9.1). The effective spec is every source against every enabled destination. The flag sits on
  the destination because removing a destination is already how one leg is stopped, and `disabled`
  makes that reversible. A request-level flag can be derived from per-destination flags, but not
  the reverse. Rejected alternatives:
  - A flag on a pairing is refused. It would turn a request into an arbitrary bitmap over the grid
    instead of sources × destinations, which the expansion cannot describe and the manifest cannot
    write.
  - A flag on a source is not built. It would only add muting one row of a fan-in, and it can be
    added later without breaking anything.

  Details: `Validate` counts entries, not enabled entries; duplicate endpoints are still refused
  when one of them is disabled; `DELETE` is still the way to remove a request; and disabling stops
  media with the same blast radius as a cancellation. A disabled request is not validated against
  the fleet, but structural validation still runs.
- **`DISABLED` is the second aggregate-only state, and it is derived** (§11). A request with no
  enabled destination reports it. The alternatives were `WAITING`, which would promise that the
  condition resolves on its own, and `INVALID`, which would say something is wrong; both are
  false. `DISABLED` is computed from the destination entries rather than stored, so it cannot drift
  from them. A path is never `DISABLED`. A request with only some legs disabled folds over the paths
  it still has. `DISABLED` ranks below `ACTIVE`, and `status` counts it on its own line rather than
  among the faults.
- **An apply that omits `disabled` enables the leg** (§9.1). The file is authoritative over the
  requests it names, so a leg disabled through the API comes back on the next apply of the file that
  names its request. §10.7's declared-key merge is refused here for the same reason it was adopted
  there: a domain's label map has many writers by design, while a request's spec has one writer.
- **A request's ID is `(namespace, name)`** (§9.1). `POST` is create-or-update, an identical spec
  writes nothing, and the outcome is reported in a header. Names are scoped to the namespace, not
  fleet-wide: a namespace that does not scope names would be only half a namespace, and the
  Kubernetes adapter that motivates namespaces brings its own namespacing.
- **A namespace is a first-class object, not a reserved label** (§9.3). This replaces the
  document's earlier position. A namespace is created on first reference, is never deleted
  automatically, cannot be deleted while a request references it, and partitions requests only —
  not nodes, domains or destinations.
- **The namespace is created eagerly, as a real write before the request itself** (§9.3). Filling
  in missing namespaces when the set is read would save one write, but `GET /v1/namespaces` would
  then invent rows, which is the old label design again. The create writes only if the namespace
  is absent, so it adds no churn.
- **Path exclusivity within a namespace is opt-in, and the default is `shared`** (§9.3). It is the
  only conflict rule that protects legibility rather than integrity: overlap inside a namespace
  costs the fleet nothing, because two requests on one path share one session and one worker pair.
  The general rule: conflict rules that protect integrity are mandatory, and rules that protect
  legibility are chosen by whoever reads the result.
- **Conflicts are ordered by `(incumbency, UpdatedAt, id)`** (§7.5), replacing "oldest first". Age
  was standing in for incumbency, and it gives the wrong answer as soon as a conflict is created by
  a change in observed state rather than by a user. Incumbency is read from the derived session
  record, not from running workers, so it survives a worker restart and works with §4.2's freezing.
  Not built yet: the code still orders by the earliest request's creation time (§7.5).
- **Validation is per path, not per request** (§7.2), and it runs over a `(source, destination)`
  pairing rather than a destination. `POST` refuses only structurally invalid requests; a
  conflicting pairing makes its own path invalid and leaves the request's other paths alone.
- **Each `INVALID` code has one disposition: it refuses the `POST` if it can be decided from the
  request plus node registrations, and otherwise marks one path `INVALID`** (§7.2). This replaces a
  single list headed "Rejectable immediately", which, next to the per-path rule, gave every code
  both dispositions (open-items §1.2). The refusing codes can also mark a path `INVALID` in steady
  state; they are the ones that additionally refuse the write.
- **`flow_conflict` is the only `INVALID` code that tears down the losing path** (§7.2), because
  the harm it prevents is in the running state, not in the intent. Its form that can be decided
  inside one request — two sources pinning the same flow UUID into a shared destination — is a
  separate code, `duplicate_source_flow`, refused at `POST`, because each code has one disposition.
- **`same_endpoint` is checked over every pairing, and one failing pairing refuses the request**
  (§7.2). While both ends are enumerated this covers §10.8's `overlapping_selectors`: a cycle
  inside one request always puts some endpoint on both sides, so the cross product contains a pair
  of that endpoint with itself.
- **Requests are written as a multi-document YAML manifest and applied** (§9.1). The API needs
  nothing extra beyond `?dry_run=true`, because create-or-update on the idempotency key already is
  apply. `--prune` requires a scope, and a namespace is a better scope than a label selector.
  `--prune` covers requests only; it never removes a namespace or a domain label.
- **`kind:` names the manifest object and defaults to `request`** (§9.1). This replaces "no
  `apiVersion`, no `kind`", which had anticipated this change. Two object types, `namespace` and
  `domain`, arrived together, which was the reason to add `kind:` once and on purpose. Apply orders
  documents by kind, whatever their order in the file.
- **`label` is a separate verb but not a separate vocabulary** (§9.1). It writes to the same endpoint
  a `kind: domain` document is applied to; it sends a patch where the document sends its declared
  map. The manifest is the desired set, `label` is a one-off edit, and noticing a domain and naming
  it is an interactive task.
- **A path whose source is not producing is `PAUSED`, not `WAITING`** (§7.2), whether or not its
  workers are running. An earlier version filed `source_idle` under `WAITING` while §11.1 listed
  the long-idle state as `PAUSED`. `WAITING` means the flow is not visible, which is false here.
- **`INVALID` governs admission only** (§7.2). It stops new sessions and never tears down a running
  one. A fabric that is no longer viable is reported as `FAILED`/`fabric_gone`, not `INVALID`.
- **The status vocabulary** is `WAITING` / `INVALID` / `ESTABLISHING` / `PAUSED` / `ACTIVE` /
  `PARTIAL` / `DISABLED` / `DEGRADED` / `FAILED` (§11). `PARTIAL` and `DISABLED` apply only to
  requests, not to paths. `ACTIVE` is decided from the destination flow's head index rather than
  from worker accounting, and a source that is not producing is `PAUSED`, not `WAITING` (§7.2).
- **`PARTIAL` is the first aggregate-only state** (§11), replacing "`ACTIVE` only when all of its
  paths are". A request whose paths disagree, with at least one `ACTIVE`, is `PARTIAL`. It outranks
  `INVALID`, `FAILED` and `DEGRADED`: §7.2 already settled that one bad path does not condemn a
  request, and a worst-state-wins fold would undo that in the status people read first. `DEGRADED`
  was the alternative and is refused, because it already means a flapping path; giving it a second
  meaning about sets would make its fleet gauge impossible to interpret. `PARTIAL` applies to every
  request shape, not only fan-in.
- **Auth is a single optional shared bearer token**, TLS is optional, and there is no mTLS (§13).
- **The server is always upgraded first, and compatibility is gated on the protocol version**, not
  the build version (§13.1).

**Capabilities and providers**

- **Nodes declare fabric attachments, not providers** (§10.1). An attachment is a
  `(provider, fabric, address)` triple, where `fabric` is an opaque label the operator assigns. Two
  nodes may pair on a provider only if they share its label, because having the same provider does
  not mean the nodes can reach each other. A node with no attachments configured gets `shm`, unless
  `--agent-detect-default-fabric` is set, in which case it picks one attachment from the probe and
  labels it `default`.
- **Configuration is matched to the probe through two classes of selector** (§10.1):
  - *naming*: `address`, `interface`, `device`, or none, where none is the common case. This
    replaces the earlier "prefer naming an interface over an address", which cannot work for
    `verbs`/`efa`.
  - *narrowing*: `network`, `ip_version`. These combine with a naming selector and with each other
    (logical AND).

  Exactly one probe entry must match the combination.
- **The agent advertises only what it has verified** (§10.2). Raw kernel capabilities never go over
  the wire; they decide whether an attachment is advertised at all.
- **There is one kind of domain, and a node declares areas** (§10.6). This replaces "input and
  output domains are separate concepts". A domain is a directory inside an area, identified
  fleet-wide as `<area>/<elements>`, and it is observed; what is derived is whether a session
  targets it. The old split identified the right problem (domains spanned two of §4's layers) but
  solved it the wrong way round, by making the directory carry a distinction that belongs to the
  session. A domain still has no lifecycle of its own: it is refcounted like a path, has no create
  or delete API, and has nothing durable to reconcile against. The destination resolver still never
  reads observed state, and cleanup of leaked directories is still deferred until there is an
  ownership model.
- **An area carries two independent grants, `read` and `write`, and the API cannot set either**
  (§10.6). This replaces `--search-path` and `--output-root` as separate concepts. Together the
  grants are all the authority this project has over a node's filesystem. Areas may nest, and a
  directory is named by the innermost area that contains it; only two areas on the same path are
  refused. This naming rule is what makes a single identity grammar possible, and the entries below
  depend on it.
- **A domain is `(area, []string)`** (§10.6). It is written `area/a/path` only in a manifest, where
  it is parsed once. Using a list keeps containment an equality check on whole elements instead of
  a string-prefix check. One materialised domain may not contain another. Domain names are no
  longer unique per node, and the per-node collision rule is gone, because the area is part of the
  name and two areas cannot produce the same name. (The code `domain_name_in_use` survives with a
  new meaning: it now rejects a materialised domain nested inside another.)
- **Discovery is not pruned** (§6, §10.6), replacing "a root is written, not read". A domain this
  project writes into is discovered like any other, and inventory membership is the union of what
  discovery reports and what the reconciler materialised. Pruning did four jobs: the innermost-area
  naming rule removes the need for two of them (one directory has one name; a leftover directory
  can no longer pick up a name shaped like a path), one moves to the union, and one survives in a
  different place.
- **A label selector never matches a flow this project is itself writing** (§10.7), replacing "a
  source selector never matches a domain under an output root". The rule and the reason are the
  same — explicitly naming a chain is intent, a chain formed by a matching selector is an accident
  ("Explicit chaining is intent; matched chaining is emergence") — but the check moved from the
  directory to the flow, where MXL already records ownership. The agent reports it as `replicated`
  in inventory. The new rule is more precise: a domain holding one replicated flow beside nine
  local ones is still usable as a source.
- **§11.1's admission rule is required for safety, not only for reducing churn** (§10.6, §11.1).
  After an agent restart, provenance (`replicated`) is briefly missing. What keeps this safe is that
  a flow whose target worker is not running is also not advancing, since the target worker is what
  advances it, so admission holds it in `PAUSED` and starts nothing.
- **Domains are discovered, never configured** (§6, §10.7). This replaces both `-m` and the earlier
  "registration advertises configured mappings only". Operators attach **labels** through the API,
  and a request's source either names a domain directly or selects domains by label. Labels
  annotate and never rename, because a rename would change the identity of every path through the
  domain on a metadata edit. Labels are joined on the server and never reach the agent. A label on a
  domain the node does not report is accepted and has no effect. The old refusal of labels under an
  output root went away with output roots.
- **The direct source form is `{"name": …}` and addresses any domain** (§10.7), replacing
  `{"path": …}`, which addressed only discovered domains and made the second hop of a chain
  impossible to write. In a manifest a scalar `domain:` is a name and a map is a label set, which
  also settles what `domain: {}` means.
- **The label selector is equality-only, all keys are ANDed, and an empty selector is refused**
  (§10.7). `in`, `notin` and `exists` will come later as a third union kind, not by widening what
  a map value may contain, because widening it would silently change the meaning of existing
  requests so that they match more.
- **An apply owns the keys it declares** (§9.1), replacing "a `kind: domain` document replaces the
  whole label set". It sets the keys it declares, removes keys it declared last time and no longer
  does, and leaves every other key alone. This is `kubectl apply`'s three-way merge, chosen because
  a domain's label map is the one record here with no single owner (§10.6), and replacing the whole
  set would let the last writer win. An imperative `label` edit therefore persists across applies.
  Two files that name the same domain still overwrite each other; named field managers would fix
  that and are not built.
- **The two ways of labelling send different bodies** (§9.1): an apply sends its full declared map,
  and `label` sends a patch. This replaces "`label` is a client-side read-modify-write", which could
  lose updates on a record that several operators are expected to write.
- **Label writes accept `--dry-run`, and a real write prints its blast radius** (§9.1). A label edit
  starts and stops media just as a request does, one step removed, which makes it easier to do by
  accident. The CLI prints the effect instead of prompting for confirmation, because the people who
  use it interactively also script it.
- **An excluded flow is reported** (§9.1, §10.7). A request's status names the flows its expansion
  dropped under the self-output rule, because a path that was never created has no status in which
  to give a reason, and without this report the per-path view would be the less readable one.
  `replicated` appears on `GET /v1/flows` and in `describe domain` for the same reason.
- **A pair of an endpoint with itself produced by a label selector is dropped, not refused** (§7.2,
  §10.7). With a named source it is a typo, and `same_endpoint` refuses it. With a selector it is
  the selector doing what it was asked, and refusing the request would make its validity depend on
  which domains happen to carry a label. This is §10.8's argument, applied one step early.
- **Multipoint is not built; what remains of it is selectors** (§10.8). The cross product itself
  arrived with fan-in, together with two of the three mechanisms that section asked for. What is
  left is a source node or a destination that is selected rather than named. That needs node
  labels, bandwidth admission control as a real precondition, and `overlapping_selectors` as a real
  check rather than one covered by `same_endpoint`.
- **The server owns full interface negotiation** (§10.3), not only the choice of provider: caps
  flags are intersected, `maxMessageSize` is the minimum of the two, and the result is assigned to
  both ends.
- **No silent provider downgrade** (§10.4). An explicit provider is honoured or the request fails.
  `provider` accepts a list to express an acceptable fallback. The default order is
  EFA > Verbs > TCP > SHM. The negotiated provider is fixed for the life of the session.
- **The agent allocates services from a configured range** (§7.4). It probe-binds for `tcp` only,
  and `shm` gets a name from the same allocator.
- **Correct TAI clocks are assumed, not verified.** They are already a deployment requirement in
  the target environments, so they are not modelled as a capability or a condition.

**Observability**

- **Metric prefixes are split by what a metric describes, not by which process emits it** (§2.2,
  §12): `mxl_` for a flow or a transfer, `mxl_repl_` for the control plane. Workers are scraped on
  demand, inside the scrape request, through a bounded pool. This replaces a cached background
  scrape, whose timing interferes with the collector's own interval and which misreports liveness
  in both directions.
- **Each object has a bounded event ring, and the path is the unit of retention** (§12.1). A
  session is a field on an event, not a log of its own: a per-session log would split the history
  at the re-establishment being investigated, and a path ID stays stable where a session ID
  does not. Requests and nodes have their own logs for events that belong to them; flows and domains
  have none.
- **An object's log is one key holding a snapshot, not one key per event** (§12.1). This is §9.2's
  level-triggered approach applied again, and it removes sequencing, gap detection, compaction and
  garbage collection in one step. The ring is bounded by **count, not age**, because an age bound
  would expire the overnight failure just as someone arrives to read it. Repeated entries are
  coalesced so that the count bound keeps real history.
- **Events are a side effect of `Apply` and never an input to `Compute`** (§12.1). They live under
  a prefix the fleet snapshot excludes, and a whole reconcile pass writes them in one write. The
  agent poll is unaffected, because it waits on its own key's `ModRevision`, but event writes still
  consume revisions, and sqlite bounds watch history by revision count (§8.1).
- **A newly elected leader emits no state transitions on its first pass and marks the gap instead**
  (§12.1). Emitting the current state as if it had just changed would produce a burst of false
  events on every leader change; a marked gap is accurate, and it is §7.3's settling argument
  applied to the log. Agent events use a **separate** endpoint rather than the status snapshot,
  which is compared before sending, and a dropped batch is recorded as such. The log is therefore a
  diagnostic aid and explicitly **not** an audit log.
- **Flows and domains entering and leaving a node's inventory are recorded on that node's ring**
  (§12.1), replacing the document's earlier "that belongs nowhere here". Entries are batched per
  pass rather than per flow. The feature can be switched off and is on by default, because this is
  the one part of the log whose volume is set by the fleet rather than by the control plane. It is
  **skipped entirely for a node without a lease**: that node's inventory is leased state, and
  diffing against its absence would report every flow as gone when nothing happened to any of them
  (§4.2).
- **The agent keeps a tail of each worker start's output and pushes it with the transition into
  `FAILED`** (§12.2), replacing §19's broader "worker log retrieval". The tail is bounded in bytes
  rather than lines, keeps the end rather than the start, is kept once per crash loop rather than
  once per restart, and is stored behind its own endpoint so the ring a UI polls stays cheap. The
  capture limit is set on the agent and the accepted limit on the server, because an endpoint that
  accepts unbounded bytes from a node could be used to fill the store.

**Storage**

- **The three state layers share one root** (§4), replacing three unrelated top-level prefixes.
  §7.3 needs the snapshot to be a single `List`, and the only prefix that covers three unrelated
  prefixes is the empty prefix. Once the event log exists, listing the empty prefix would pull
  diagnostics into every read and wake the reconciler with its own writes. A byte range over the
  layer names would have been cheaper and is refused: when it goes wrong, a layer is silently
  missing from the snapshot, which is the same failure as §4.2's wiped store.
- **The store interface uses etcd's model and is emulated over sqlite** (§8.1), with an append-only
  history table behind the watch, a `(revision, seq)` cursor, bounded history and `ErrCompacted`.
  The same conformance suite runs against both backends.
- **Agents long-poll with a revision cursor; the server does not push** (§9.2), so every request is
  self-contained and no sticky sessions are needed. `inventory` and `status` are full snapshots, and
  an agent does not send one that has not changed.

**The worker**

- **The worker source may be modified** (§15). There are three required changes — a configurable
  no-grain timeout, an interface probe mode, and accepting the negotiated interface config — plus
  hygiene fixes. The exit-code fix is explicitly not relied on: failures are classified from
  restart rate, time to death and source liveness (§15.1).
- **Interfaces are discovered by the worker probe** (§10.5) using `mxlFabricsGetInterfaces()`, not
  guessed from `/dev/infiniband`.
- **Idle sources are handled by three mechanisms on different timescales** (§11.1): a long or
  infinite worker idle timeout for short gaps, head-index observation for admission and for
  teardown after a long idle period, and agent-side restart backoff for everything else. Both
  session-level settings are on the server.
- **Worker starts are rate limited on the agent by a token bucket** (§6.3). How many workers a node
  can bring up at once is a different capacity from how many it can run, and only the second had
  been sized (§14). Every start goes through the bucket, including restarts; stops never do. A
  queued start can be cancelled immediately, so a withdrawal does not wait behind a permit. A limit
  on starts in flight was the first answer and is refused: an initiator has no signal that its
  start has finished, so a concurrency limit would protect nothing for half the workers. The wait
  happens in the supervision goroutine, which already exists so that reconcile never blocks on a
  start (§6). The settings are agent-local, a rate of `0` means no limit, and the defaults are
  conservative enough that §6.1 was amended: 1–2 s is the budget for one flow, not for a whole
  node.
- **Each worker has one supervision goroutine, and reconcile never blocks on a start** (§6). A
  start can wait on `target-info.json` or on a start permit, and neither wait has a firm bound, so
  a target that never came up would otherwise hide every other assignment change from the node,
  including the one that withdraws it. Stops are synchronous, because the caller is usually about
  to start a replacement for the same session.
- **One process per flow per direction is fine** (§14). The worker sits behind a replaceable module
  interface, so a future multi-flow worker would be a substitution rather than a rewrite; testing
  (§17) justifies that interface anyway.

---

## 19. Roadmap

`docs/open-items.md` is a companion list and is **not** part of this roadmap. It holds loose ends
that the domain-labelling, namespace and conflict-precedence work left in this document: two
contradictions between sections above (both since resolved), mechanisms those sections imply but do not describe, and
decisions that were deferred rather than made. Read it before treating §7.2, §7.5, §9.3, §10.7 or
§10.8 as finished. The roadmap below is work this design does not do yet; open-items is work this
design says it does but has not fully specified.

- A Kubernetes adapter that turns pod labels or annotations into replication requests. It depends
  on the idempotency key in §9.1 and on the rule that an unchanged request is not rewritten; a
  controller that re-reconciles on every resync is the case both exist for. It should use its own
  namespace and leave it `shared` (§9.3): one request per pod is the natural mapping, several pods
  asking for one flow is normal, and refcounting is how this design handles that.
- A web UI for managing and inspecting agents and replication.
- Bandwidth admission control (§13). **This is a precondition for multipoint** (§10.8), not an
  optional improvement, because with a selector on both ends nobody can tell what a request will
  cost when it is written.
- Multipoint requests (§10.8), which lists the conditions they need.
- An ownership model for domains, which is what cleanup of leaked directories needs (§10.6). It is
  now more visible, not more urgent: because discovery is no longer pruned, a leaked directory with
  stale content is reported instead of hidden, so an operator can at least see what this item would
  clean up.
- ~~Worker log retrieval through the API.~~ **Superseded by §12.2**, which implements the narrow
  part: a byte-bounded tail per worker start, pushed with the transition into `FAILED` and fetched
  from its own endpoint. As this item predicted, it needed changes to both the exec launcher and
  the supervision unit, not only the launcher. What remains unbuilt is the general form this item
  originally described: a live log stream from a running worker. That needs a streaming agent API
  and has no obvious client until the UI wants one.
- Moving the etcd backend behind a build tag. Linking it grows a trivial binary from 9.2 MB to
  24.1 MB — **+15 MB**, almost all of it grpc, protobuf and zap — and the agent never needs an etcd
  client. A fleet-wide DaemonSet is where that size will or will not matter.
- A multi-flow worker, if §14 ever says hundreds of flows per node.
- Worker adoption across agent restarts (§6.1).
- Relaxing the refusal of newer agents to a warning (§13.1). §2.3 argues for it, and it is a
  behaviour change that deserves its own decision.
- An importer for legacy `config.yaml` that emits manifest documents rather than API calls (§16).

**Dropped: a separate `xpt` CLI.** An earlier version of this section proposed one. `xpt` is the
broadcast abbreviation for *crosspoint*; the proposal had `xpt take|list|status|clear` as
domain-specific verbs and `mxl-replicator ctl` as the discoverable long form. It is superseded by
the manifest and the subcommands in §9.1, which cover the same operations declaratively. Two
vocabularies for one thing are worse than one, a file is the better interface for a desired set
that an operator maintains, and a second binary would add a build target and packaging work for no
benefit. If a one-shot `take` verb is ever wanted, it can be another subcommand beside the rest.

---

*This document supersedes `rewrite.md`, the rewrite proposal it grew out of, and keeps its section
numbering: roughly 1200 comments in `internal/`, `cmd/` and `src/` cite these numbers, as does
`rewrite-plan.md`, whose own §4 (deployment topologies) is folded into §2.3, §8.2 and §13.1 here.
The one number that changed meaning is §16, which was the migration section and is now the record
of what the retired proxy left behind.*
