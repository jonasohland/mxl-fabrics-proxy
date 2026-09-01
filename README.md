# mxl-replicator

[![Docker Image](https://img.shields.io/badge/docker-jonasohland%2Fmxl--replicator-blue?logo=docker)](https://hub.docker.com/r/jonasohland/mxl-replicator)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)

Replicate MXL flows between hosts over MXL Fabrics, driven by a central control plane.

## Disclaimer

**This project is currently considered experimental.**

It is provided as-is for evaluation and testing. Active work is underway on a **standard discovery
and connection API** for MXL-enabled media functions, and this project is expected to realign with
it — which is why it is named for what it does rather than as a product. The manifest format and
the HTTP API are subject to change.

**Use in production at your own risk.** Pin a specific image tag.

> `mxl-replicator` replaces `mxl-fabrics-proxy`, which lives on under `legacy/go/` until the new
> implementation is at parity. There is **no configuration or wire compatibility** between them —
> see [Migrating](#migrating-from-mxl-fabrics-proxy).

## Overview

MXL flows are ring buffers in memory-mapped files under a *domain* directory. Media functions on
one host share them with zero copies. **MXL Fabrics** extends that across hosts over libfabric
(`tcp`, `verbs`, `efa`, `shm`), predominantly by RDMA Remote Write straight into the receiver's
media buffer.

`mxl-replicator` decides which flows go where, and supervises the `mxl-replicator-worker`
processes that move them. It never touches grain data.

Two roles, one binary:

- **Server** — holds replication requests, aggregates what every node reports, negotiates the
  fabric each session uses, and hands each agent the complete set of workers it should be running.
- **Agent** — one per node. Discovers local flows, reports them, runs the workers it is assigned,
  and exposes their metrics.

Replication is requested **through an API**, not by editing a config file on every host. You write
the desired set to a manifest and apply it:

```console
$ mxl-replicator apply -f studio-a.yaml
nab/cam1-distribution created (3 path(s)) (ESTABLISHING: waiting for the destination agent to start its target worker)
nab/talkback created (1 path(s))

$ mxl-replicator get requests
NAMESPACE  NAME               STATE   PATHS  SOURCES                                        DESTINATIONS                             LABELS
nab        cam1-distribution  ACTIVE  3      studio-a/{role=cameras},studio-b/media/cameras edge-01/fast/ingest,edge-02/fast/ingest  show=nab
nab        talkback           ACTIVE  1      studio-a/media/audio                           edge-01/fast/ingest                      show=nab

$ mxl-replicator status
nodes      3 registered, 3 leased
requests   2  (2 ACTIVE)
paths      3  (3 ACTIVE)
sessions   3 running

everything is active
```

## Features

- **Central control plane.** Agents register and receive assignments; there is no agent-to-agent
  traffic. Configuration is O(n) in the fleet rather than O(n²).
- **Declarative.** `apply` / `delete` over a manifest, with `--dry-run` and a scoped `--prune`.
- **Selectors, not just flow IDs.** Replicate "whatever camera 1 is publishing" by NMOS group hint
  and let the fleet follow republished flows, or pin a UUID. Domains are selected the same way —
  by labels an operator attaches through the API, without restarting an agent.
- **Fan-in and fan-out.** Many sources, many destinations, with one status that aggregates over
  them and a per-source breakdown beside it.
- **Fail-static.** A control-plane outage never stops running media. An agent acts only on an
  assignment set it actually retrieved; a server restart adopts running sessions rather than
  re-establishing them.
- **Two storage backends.** sqlite for a single node, etcd for HA behind a plain HTTP proxy — no
  sticky sessions required.
- **Prometheus metrics** for every worker, plus the control plane.

## Quick start

Both roles in one process, on one host, replicating between two domains over `tcp` — no RDMA
hardware needed:

```bash
mxl-replicator run \
    --agent-node loopback \
    --agent-area media=/dev/shm/mxl0:r \
    --agent-area fast=/dev/shm/mxl:rw \
    --agent-fabric provider=tcp,fabric=loopback,address=127.0.0.1
```

An **area** is a directory this node has designated as somewhere MXL domains live, with two
independent grants: `r` lets this project discover and observe domains under it, `w` lets
replication create them. A domain inside one is addressed fleet-wide as `<area>/<elements>` —
`media/cameras`, `fast/ingest` — and that is its identity for life.

Then, from anywhere that can reach it:

```bash
mxl-replicator apply -f loopback.yaml
mxl-replicator status
mxl-replicator get paths
```

[`loopback.yaml`](loopback.yaml) is the smallest complete example.

## Manifests

Multi-document YAML, `---` separated, one object per document. `kind:` names the object and
defaults to `request`. An unknown key — or an unknown kind — is an error, not a warning: a
misspelled field that silently does nothing is the failure a declarative format exists to prevent.

```yaml
kind: namespace
name: nab
paths: exclusive           # two requests here may not hold one path

---

kind: domain
node: studio-a
domain: media/cameras
labels: {role: cameras, name: cameras}

---

name: cam1-distribution
namespace: nab
sources:
  - node: studio-a
    domain: {role: cameras}                   # a label set; a scalar would be a name
    group_hint: {name: "Studio A Camera 1"}   # video + audio, both legs
  - node: studio-b
    domain: media/cameras                     # no selector: every flow in the domain
destinations:
  - {node: edge-01,    domain: fast/ingest}
  - {node: edge-02,    domain: fast/ingest, disabled: true}   # on file, switched off
  - {node: archive-01, domain: bulk/capture, provider: tcp}
provider: [verbs, tcp]
labels:
  show: nab

---

name: talkback
namespace: nab
sources:
  - node: studio-a
    domain: media/audio                       # a scalar: this domain, by name
    flow: 5592a23b-0974-45bb-9388-89ea81c42537
destinations:
  - {node: edge-01, domain: fast/ingest}
idle_teardown_ms: 0        # bursty feed, keep it hot
```

Documents are applied by kind — namespaces, then domains, then requests — whatever order the file
is in. The end state does not depend on it; the *intermediate* state does, and a request that
lands ahead of the labels its selector matches reads as the apply having broken and then fixed
itself.

| Field | Meaning |
|---|---|
| `name` | The request's identity, its ID and its idempotency key. Applying the same name again updates rather than duplicating. |
| `sources[]` | Where to read from. **Always a list**, and at least one; a request is the cross product of this and `destinations[]`. |
| `sources[].node` | Which node to read from. Pinned, not selected. |
| `sources[].domain` | **A scalar is a name and a map is a label set.** `media/cameras` addresses that domain; `{role: cameras}` matches every domain on the node carrying that label. An empty map is refused — it would match everything the node happens to hold. |
| `sources[].flow` | Pin one flow ID. |
| `sources[].group_hint` | Select every flow whose `urn:x-nmos:tag:grouphint/v1.0` matches. `{name, type}`; omitting `type` selects every flow sharing the name, which is how a camera's video and audio travel together. At most one of `flow` and `group_hint`. |
| *(neither)* | A source that names a domain and says nothing else replicates **every flow in it** — the retired proxy's subscription shape. Spelled by omission in a manifest only; on the wire it is `"select": {"all": true}` and an absent selector is an error. |
| `destinations[]` | Where it goes: `node` and `domain`. |
| `destinations[].domain` | `<area>/<elements>`: `fast/ingest`, or `fast/studio-a/cam1` to nest. The first segment is the area — which the destination node must advertise **and grant `write` on** — and the rest are path elements, each a plain name, at most 8 of them. On the wire it is `{area, elements}`, and the manifest is the only place it is ever a string. |
| `destinations[].provider` | Override the request-level pin for this destination alone. |
| `destinations[].disabled` | Park this leg. The entry stays in the request and expands to nothing — no path, no session, no workers — so a route can be switched off without being deleted and retyped. **It stops media**, so preview it with `--dry-run` like any other change. A request with every destination parked reports `DISABLED`, which is not a fault and is kept out of `status`' list of what is wrong. Note that an apply which *omits* the flag enables the leg: the file is authoritative, so a leg parked through the API comes back the next time somebody applies the file that names its request. |
| `provider` | `verbs`, or `[verbs, tcp]` for "prefer verbs, tcp acceptable". Omitted, the server negotiates in its configured order (EFA > Verbs > TCP > SHM). **Never silently substituted** — a pin is honoured or the request fails. |
| `idle_teardown_ms` | Stop this request's workers when its source has been idle this long. `0` keeps them hot. |
| `sched_prio` | Ask for `SCHED_FIFO`. Rejected at request time if a participating node lacks the capability. |
| `namespace` | The partition this request belongs to, and half its identity: `(namespace, name)` is the ID, so two requests called `cam1` in two namespaces are two requests. Omitted, it is `default`. Letters, digits, `-` and `_`. |
| `labels` | Ride into worker metrics as user labels, and narrow `apply --prune` within its namespace. |

### Namespaces

A namespace is a partition of the request set, and a first-class object: it has a record, a
description, and one rule — whether two of its requests may hold the same path.

**Names are scoped to it.** `(namespace, name)` is a request's ID and its idempotency key, so two
operators can both have a `cam1`, and a Kubernetes adapter naming requests after pods inherits
Kubernetes' own namespacing instead of having to prefix.

```yaml
kind: namespace
name: nab
paths: exclusive           # default is `shared`
```

`paths: shared` — **the default** — lets two requests expand onto one path. That is refcounting
working as designed: one path, one session, one worker pair, nothing doubled and nothing
corrupted, and across namespaces it is exactly how fan-in is expressed.

`paths: exclusive` refuses it. The loser reports `INVALID` naming the incumbent, and the path —
held by the winner — carries on. What overlap costs is not integrity but *legibility*: in a matrix
of one namespace's requests, two lit cells that are one stream do not sum, and a cell goes dark on
a click that stopped nothing. So it is opt-in, and the party that needs the guarantee is the party
that asks for it: a third-party client should not have to know the rule exists in order not to be
broken by it.

A namespace is created by first reference and is never removed on your behalf; deleting one is
refused while any request references it, with the count. `default` cannot be deleted.

One namespace is one file, and `--prune` is what makes that literal:

```bash
mxl-replicator apply -f nab.yaml --prune -n nab [-l show=x]
```

Everything in the `nab` namespace that the file does not name is cancelled; every other namespace
is untouched. `-n` is required — a declared partition is a better guard than an ad-hoc tag — and
`-l` narrows within it.

### Domain labels

A domain's **identity** is `<area>/<elements>`, permanently. Labels are annotation attached to
`(node, domain)` through the API, and what they are for is *selection*:

```bash
mxl-replicator label domain studio-a:media/cameras role=cameras name=cameras
mxl-replicator label domain studio-a:media/cameras site-          # remove a key
```

Renaming a domain is not on offer, and the reason is worth stating: the domain name is embedded in
path identity, session identity and the `domain` metric label, so a rename would tear down running
media and split every series it touches — on a metadata edit. Keeping identity fixed makes
relabelling free.

**A label can be applied before the domain exists.** It is a pending record, listed by
`get domains` and `describe domain`, and a request selecting it sits in `WAITING` until a producer
appears. That is the point: labelling a camera's domain before the camera is switched on is an
ordinary thing to want.

Two writers, two semantics, one endpoint:

| Gesture | Body | Merge |
|---|---|---|
| `kind: domain` in a manifest | an **apply** — the full map it declares | owns the keys it declares: sets them, removes the ones it declared last time and no longer does, and leaves every other key alone |
| `mxl-replicator label` | a **patch** — keys to set, keys to remove | merges against nothing, and does not change what a future apply believes it owns |

So `label` and `apply` do not fight, because they own different keys: an operator can name a
domain interactively and keep that name, in a fleet whose requests are applied from git by someone
else. It is `kubectl apply`'s three-way merge, adopted deliberately — this file format is close
enough to a Kubernetes manifest that surprising someone who arrives with those expectations costs
more than internal consistency would buy.

**`--prune` never touches a label.** A file naming three domains would otherwise prune the other
forty, and a label is a fact about a host rather than intent.

A label write starts and stops media, one level of indirection away, so it takes `--dry-run` and
prints a **blast radius** on the real write too: for each path that would stop, which requests were
feeding it. It prints rather than prompts — the CLI is scripted by the same people who use it
interactively, and a verb that blocks on a tty hangs in a pipeline.

#### What a label selector will not match

**A label selector never matches a flow this project is itself writing.** Left open, replication
feeds itself: a flow copied to a node becomes visible on that node, a broad selector matches its
own output, and the path set grows on every pass. It terminates, but the topology it settles into
is decided by conflict precedence rather than by anything an operator wrote, and an emergent
routing algorithm is the wrong thing to have.

Naming a domain directly still reaches everything, which is how `A→B→C` is written:

```yaml
sources: [{node: edge-01, domain: fast/ingest, flow: "…"}]   # explicit: intent
sources: [{node: edge-01, domain: {role: onward}}]           # matched: never its own output
```

The signal is per **flow**, not per directory, so a domain holding one replicated flow beside nine
a local media function produced offers the nine and withholds the one. `get flows` and
`describe domain` both carry a `REPLICATED` column, and a request whose expansion dropped a flow
for this reason says so in its status — a silently skipped flow is otherwise undiagnosable.

### Many sources, many destinations

Both ends are lists, and a request is the cross product of them: three studios into two edges is
six pairings, each expanding its own source's selector. A source's **node** stays pinned, so the
list is always something the author typed rather than a second selector — which is what keeps the
cost of a request readable at the moment it is written.

Validation and negotiation are per pairing, and a request can be viable for eleven of them and
refused for the twelfth. The reason names the end that failed: the source when it is common to
every destination of one source, the destination when it is common to every source of one, and
neither when it applies to every pairing. Naming the wrong end sends you to a node where
everything is fine.

Two things follow from the ends no longer sharing a fate:

- **A request whose paths disagree, with at least one `ACTIVE`, is `PARTIAL`** — one dark camera
  among twelve is the ordinary state of an ingest wall, and `PAUSED` would be a true statement
  about one path and a false one about the request. `describe request` prints a row per source,
  which is what says *which* camera.
- **Two sources of one flow ID into one destination is refused.** Where both sources pin the UUID
  it is `duplicate_source_flow` at `POST`; where it arrives later through a selector it is
  `flow_conflict` on the path, and the loser is torn down.

Fan-in as several documents sharing a destination domain still works and still refcounts to one
session. What a single request buys is one unit of intent over the set.

## Commands

```
mxl-replicator run      [--server] [--agent] [flags]    the daemon; both roles by default
mxl-replicator apply    -f <manifest> [--dry-run] [--prune -n nab [-l k=v]]
mxl-replicator delete   -f <manifest> | [-n nab] <name>...   only the kinds and names are read
mxl-replicator label    domain <node>:<area>/<elements> k=v k-   [--dry-run]
mxl-replicator status   [-o json|yaml]
mxl-replicator get      nodes|domains|flows|requests|paths|sessions|namespaces [filters] [-o json|yaml]
mxl-replicator describe node|domain|flow|request|path|session|namespace <name> [-o json|yaml]
```

`--server` and `--agent` select a role *alone*; naming neither runs both, which is the single-host
and development case. Both roles in one process still speak HTTP to each other, so there is exactly
one code path.

### apply

Create-or-update, keyed on each document's `name`. Applying an unchanged request writes nothing and
says `unchanged` — a controller re-applying on every resync costs the store nothing.

Documents are applied in file order. **This is not atomic**: a failure reports which document
failed, leaves the earlier ones applied, and exits non-zero. Requests are independent durable
intent, so a partial apply is a partial success.

`apply` and `delete` print one line per document — the name, then what happened to it. It is a
list, not a table; `get` and `status` are where the tables are.

`-f` is repeatable and takes a file, a directory of `*.yaml` (flat, sorted, not recursive), or `-`
for stdin.

`--dry-run` validates and reconciles against the real fleet and reports what would happen, without
writing. It sees stored state plus the one request, so two *new* documents in one file that
conflict with each other both pass and the second fails on the real apply.

### delete

`delete -f` reads **only the kinds and names** from a manifest and ignores everything else in it, so
a document containing nothing but `kind:` and `name:` is a complete instruction — and a file that
has drifted from what is deployed still removes what it named. That is the case that matters: "delete what this
file created" is wanted most exactly when the file is no longer an accurate description of
anything. `delete [-n nab] <name>...` takes names directly. Documents go in reverse apply order —
requests, then the namespaces they live in — and `kind: domain` documents are skipped, because
removing labels is `label key-` or an apply that no longer declares them, both of which say what
they mean.

Deleting something that is not there succeeds. Removing what a manifest names is idempotent by
nature, so a second run should not fail because the first one worked.

`--prune` cancels requests in `--namespace` that the manifest does not name. **The namespace is
required**: pruning everything a file does not name would cancel requests it knows nothing about,
and the object being cancelled is moving video. A declared partition is a better guard than an
ad-hoc tag, so `-n` is the scope and `-l` narrows within it. There is no confirmation prompt —
`--dry-run` shows the set.

```bash
mxl-replicator apply -f studio-a.yaml --prune -n nab -l show=nab
```

**Prune covers requests only.** It never removes a namespace and never removes a domain label, even
when the file contains documents of those kinds.

### status, get and describe

Three read verbs, three jobs, no overlap:

| | |
|---|---|
| `status` | Counts the fleet, then names **only what is not active**. Not a list — the answer at 3am is "these two things are broken", not a screen to scan. |
| `get <kind>` | Lists, so you can find the name of the thing you want. |
| `describe <kind> <name>` | Everything known about one of them. |

```console
$ mxl-replicator status
nodes      3 registered, 3 leased
requests   4  (1 WAITING, 3 ACTIVE)
paths      7  (1 WAITING, 6 ACTIVE)
sessions   6 running

KIND     NAME      STATE    REASON
request  cam3      WAITING  the selector matches no flow in studio-b/media/cameras
```

States, worst-first: `INVALID`, `FAILED`, `DEGRADED`, `WAITING`, `ESTABLISHING`, `PAUSED`,
`ACTIVE`, `DISABLED`. A request aggregates over its paths, and `get requests`' `PATHS` column
carries the "1 of 3" that a one-flow-per-request model has no way to express.

Two more **aggregate only** — they describe a set, so they never appear on a path, a session or a
worker. `PARTIAL`: a request whose paths disagree, with at least one `ACTIVE`, reports it instead of
the worst one, because one bad path among twenty must not condemn the other nineteen at the line you
read first. The detail lives in `describe request`'s per-source rows and in the per-path metrics.

`DISABLED`: every destination of the request is parked (`disabled: true`), so it is asking for
nothing. It sorts after `ACTIVE` and is deliberately left out of `status`' list of what is wrong —
somebody switched it off on purpose — but it is counted beside every other state, because a parked
route nobody remembers is exactly what wants finding. One live destination beside a parked one is
not `DISABLED`; it folds over the legs it still has.

`PAUSED` is the one worth knowing: it separates *the plumbing is broken* from *the source is not
producing*, which look identical from a "no media at the destination" alarm and have completely
different owners.

`get` takes the same nouns in the plural (singular works too), with filters that are checked
rather than ignored — `--node` on domains, flows, paths and sessions, `--domain` on flows, `-n` and
`-l` on requests. Naming one that cannot apply is an error, because a filter that silently does
nothing is how you conclude a flow is missing when you only narrowed on the wrong field.

`describe` takes one of seven nouns:

| | |
|---|---|
| `node` | What the agent advertises — areas and their grants, fabric attachments, versions — the domains it is currently observing, and every path touching it, with this node's role in each. |
| `domain` | `<node>:<area>/<elements>`: its labels, whether the node reports it, and for each flow whether **this node is the one writing it**. |
| `flow` | A flow ID is unique to the media, **not** to a location: after replication the same ID exists on both nodes. So this lists every place it is, whether each is being produced, and which paths carry it. |
| `request` | The stored intent, its destinations and pins, the per-path breakdown, and **what the expansion excluded** — a flow a label selector skipped has no path to carry a reason, so it is listed here or it is invisible. |
| `path` | The deduplicated edge, its state, and its **refcount** — which requests share it, and therefore what happens if you cancel one. |
| `session` | The concrete worker pair: negotiated fabric and interface config, the epoch, and each end's state, bound endpoint, restart count and uptime. |
| `namespace` | The partition: its `paths` policy, and the requests in it. |

Path and session stay separate even though they are 1:1 in practice, because they are separate
layers: a path is derived state that outlives any particular session, and a session is ephemeral —
re-established whenever either end restarts. Collapsing them would suggest a path dies when its
workers do, which is precisely what this design is built not to do.

```console
$ mxl-replicator describe path b895e698
Path      b895e698
  source        studio-a/media/cameras 5592a23b-0974-45bb-9388-89ea81c42537
  destination   edge-01/fast/ingest
  state         ACTIVE
  requests      cam1-distribution, talkback (refcount 2)

  Session 290fd86a — describe session 290fd86a for its workers
    fabric      ib-fabric-a / verbs
    state       target ready on edge-01, initiator ready on studio-a
```

`-o json` and `-o yaml` emit the API object verbatim, so a script written against them is written
against the documented API rather than against the command.

## Agent configuration

Flags and YAML, and all of it provisioning-level — it changes when the host is built, not when a
flow is routed.

| Flag | Meaning |
|---|---|
| `--agent-node` | Fleet-wide unique node name. Defaults to the hostname. |
| `--agent-server` | Control-plane URL. Repeatable for HA. |
| `--agent-area name=/path:rw` | Declare an area, with its grants: `r` to discover and observe domains under it, `w` to create them. Repeatable. **A node with no readable area offers no sources; one with no writable area accepts no destinations.** |
| `--agent-fabric provider=,fabric=,device=` | Declare a fabric attachment. Repeatable. Naming selectors `address=`, `interface=`, `device=` or none; narrowed by `network=10.1.0.0/16` and `ip_version=4\|6`. |
| `--agent-detect-default-fabric` | With no attachment configured, detect one from what libfabric reports — best provider first, and for `tcp` the first routable IPv4 address — and label it `default`. Nodes pair only with others carrying the same label, so this is for a flat network. |
| `--agent-port-range` | Range the agent binds target workers in. Inbound to the *destination* node, so open it there. |
| `--agent-config` | YAML file supplying any of the above. |

### Areas and domains

**A domain is a place, not a channel.** There is one kind, whichever direction this project uses
it in: a directory inside an area, holding flows. Several processes routinely write different
flows into one directory, and the single-writer constraint MXL actually enforces is per *flow* —
so this project is one participant among a node's media functions rather than the proprietor of
any directory.

An **area** is a directory an operator designated, with a name and two independent grants:

```yaml
areas:
  - {name: media, path: /dev/shm/mxl,            read: true}
  - {name: fast,  path: /dev/shm/mxl/replicated, read: true, write: true}
  - {name: bulk,  path: /mnt/nvme/mxl,           read: true, write: true}
```

`read` is the whole of this project's authority to discover and observe domains under that
directory; `write` is the whole of its authority to create them and write flows into them. Neither
implies the other, both default false, and an area granting neither is refused at startup as a
line that does nothing. Access to a node's filesystem is opt-in, per node and per direction.

**A domain's fleet-wide identity is its area's name followed by its path elements**, and that is
its identity for life. Under the layout above, `/dev/shm/mxl/studio-a/cam1` is `media/studio-a/cam1`
and `/dev/shm/mxl/replicated/ingest` is `fast/ingest`.

**Areas may nest, and the innermost containing area names a directory.** Longest prefix wins, so
`media` being an ancestor of `fast` produces nothing to disambiguate — `fast` contains
`.../replicated/ingest` more tightly, so it is `fast/ingest` and never `media/replicated/ingest`.
Two areas on **one** directory are refused at startup, naming both, because that is the one
arrangement the rule cannot decide. Everything else is legal, and the one-MXL-area-per-host layout
with a subtree replication writes into is now two ordinary areas rather than an exception to a
rule.

That one name is what makes the rest work: a directory has exactly one identity whether discovery
found it or the reconciler created it, so the two cannot disagree.

A destination is **always a name inside an area the operator granted `write` on**. A raw path is
never accepted from the API — that invariant is what stops the API from being a remote
arbitrary-filesystem-write on every node in the fleet, and it holds whatever authentication is
configured. The area is the entire perimeter, it is node-local configuration, and the API cannot
set it. Both the server and the agent check it, and the agent is the authority about its own
filesystem.

**Discovery is not pruned.** A domain this project writes into is reported like any other: it
appears in `get domains`, it can be labelled, and a flow in it that this node is *not* writing —
one a local media function produced beside the replicated ones — is selectable like any other. What
it cannot be is matched into a copy of itself, which is a rule about the flow rather than about the
directory.

A domain may itself be nested — `fast/studio-a/cam1` — which is how you group destinations without
provisioning an area per group. One materialised domain may not *contain* another, so
`fast/studio-a` and `fast/studio-a/cam1` cannot both be created on a node.

**Repointing an area's directory keeps every identity on it.** `path: /dev/shm/mxl` →
`path: /mnt/mxl` under the same area name leaves every domain called what it was, so paths and
sessions survive the move rather than rebuilding. Moving a domain to a different *area* does
re-identify it, which is the correct reading: the first is an operator relocating a mount, the
second is choosing a different destination.

### Fabric attachments

Nodes declare `(provider, fabric, address)` triples, not bare provider names. `fabric` is an
operator-assigned opaque label, and two nodes may pair on a provider **only if they share its
label**:

```yaml
fabrics:
  - {provider: verbs, fabric: ib-fabric-a,   device: mlx5_0, ip_version: 4}
  - {provider: tcp,   fabric: dc1-data,      network: 10.1.0.0/16}
  - {provider: efa,   fabric: vpc1-subnet-a, device: rdmap0s6-rdm}
```

Provider availability is not reachability. Two nodes both offering `verbs` may be on different
InfiniBand fabrics; two both offering `efa` may be in different VPCs. Intersecting provider *names*
would cheerfully assign a session that cannot connect, and it fails invisibly — the target comes up
clean and the initiator's connect loop spins.

Selectors come in two classes. A **naming** selector says which interface — `address`, `interface`,
`device`, or none when the node has exactly one of that provider — and there is at most one per
attachment. **Narrowing** selectors say which of its addresses counts — `network` and `ip_version`
— and they compose, with a name and with each other. The agent resolves the lot at startup by
running the worker's `--interfaces` probe and asking libfabric what the node actually has; exactly
one probe entry must survive, and zero or several is a loud startup error rather than a guess.

The naming selectors are not interchangeable. `device` is the **libfabric** device name — `mlx5_0`,
`rdmap0s6-rdm` — and `interface` is the netdev, which exists for `tcp` and `verbs` and **not for
`efa`**: an efa attachment naming an interface is refused at startup, because the probe has no
netdev name to match it against.

A name alone is often ambiguous: an HCA reporting both an IPv4 and a link-local IPv6 address is two
entries under one device name. `address` is the exact-and-always-unique escape hatch, but it costs
a per-node value — which a DaemonSet does not have — so `device: mlx5_0` plus `ip_version: 4` is
usually the better answer, and `network: 10.1.0.0/16` is better still where it applies: it picks
each node's own address inside a prefix while naming no hardware at all. Neither narrowing selector
asserts anything about reachability; two nodes inside one prefix may still have no route between
them, and that is what the fabric label decides.

## Docker images

```bash
# Regular
docker pull jonasohland/mxl-replicator:latest

# EFA optimized
docker pull jonasohland/mxl-replicator:latest-efa
```

Both carry `mxl-replicator` and `mxl-replicator-worker`. `make image` / `make image-efa` build
them; `make image-test` adds `mxl-mock-src` and `mxl-mock-sink` for the end-to-end suite, and must
not be published as `:latest`.

## Kubernetes

[`deployment/mxl-replicator/`](deployment/mxl-replicator/) is a Helm chart: the server as a
Deployment, the agent as a DaemonSet, and nothing else required.

```bash
helm install mxl-replicator ./deployment/mxl-replicator \
    --namespace mxl --create-namespace \
    --set image.tag=v0.3.0 \
    --set server.persistence.node=<node> \
    --set-json 'agent.fabrics=[{"provider":"tcp","fabric":"dc1-data","interface":"eth1"}]'

kubectl label node <node> mxl.ebu.org/mxl-replicator=true
```

The chart provisions the fleet; it never decides what is replicated — that stays an `apply`
against the API. Three things it will not guess at: which node holds the sqlite store
(`server.persistence.node`, a directory on that node by default), what each node can be reached on
(`agent.fabrics`), and what filesystem authority each node grants (`agent.areas`).

`agent.efa.enabled` requests `vpc.amazonaws.com/efa` from the AWS device plugin and switches the
agent onto the `-efa` image; `agent.pools` runs several agent DaemonSets over a fleet whose nodes
are not alike. The [chart README](deployment/mxl-replicator/README.md) covers both, and
[`examples/`](deployment/mxl-replicator/examples/) has complete values files for a single node, an
EFA cluster and a mixed fleet.

## Building from source

```bash
# Requires CMake, a C++20 compiler and Go 1.26+
make all

# Or just the Go side
make replicator
```

## Observability

Prometheus metrics on the agent's `--agent-listen` (`:2284`) and the server's `--server-listen`
(`:2283`), at `/metrics`.

Prefixes split by **what the metric describes**, not by which process emits it:

- `mxl_*` — anything about a flow or a transfer. Unchanged from `mxl-fabrics-proxy`, so existing
  dashboards keep working.
- `mxl_repl_*` — control-plane metrics that exist only because of this project: requests by status,
  sessions, leased agents, epoch transitions per session (an excellent flapping signal), reconcile
  duration, store latency.

Worker metrics are scraped on demand, inside the request, through a bounded pool with per-worker
and overall deadlines. `/healthz` stays green when a transfer is failing — a peer being unreachable
is no reason to restart and drop every other flow — so watch status and metrics for failure rather
than the probe.

The server serves `/healthz` and `/readyz`, and they are not the same question: readiness reports
whether the reconciler has settled, so it is the one a load balancer should use and the wrong one
to restart on. **The agent serves `/healthz` only** — there is no definition yet of what an agent
being *ready* would mean — so its readiness probe is the same endpoint, and proves the process is
up rather than that it has registered.

## Migrating from mxl-fabrics-proxy

There is **no configuration or wire compatibility**, and no importer. The legacy file is a per-node
config whose subscriptions are addressed by `mxl://` URL and whose destination is a `-m` mapping —
three things this design deliberately moved. Re-author the `subscriptions:` block as a manifest.

What does carry over:

- **`mxl_*` metric names.** Five label changes do not:

  | Was | Is | Why |
  |---|---|---|
  | `flowID="…"` | `flow_id="…"` | Prometheus convention |
  | `domain="/dev/shm/mxl0"` | `domain="media/cameras"` | The fleet-wide identity, not a path — stable across hosts, and the same value on both ends of a transfer |
  | `quantile="0.010"` | `quantile="0.01"` | Three fixed decimals are not how anything else renders a quantile |
  | — | `session="…"` | New; without it two initiators on one flow are one duplicated series |
  | — | `namespace="…"` | New; which partition a transfer belongs to |

Three things need a decision rather than a translation:

- **`-m name=/path` and the `domains:` block are gone.** They did two jobs at once — granting the
  agent authority to read a directory, and giving that directory a fleet-wide name — and those
  separate. The grant becomes an area's `read` bit; the naming becomes the area's own name plus
  what the filesystem already decided, with anything friendlier expressed as a label. That also
  means naming a domain no longer costs an agent restart, and an agent restart re-establishes every
  flow on the node.
- A legacy mapping used as a subscription **destination** becomes a domain name inside an area the
  node grants `write` on.
- `defaults.provider` was a per-side setting; a provider is now negotiated per session against
  declared fabric attachments. Carry it over as a request-level `provider` pin and relax it
  deliberately — silently widening what an existing deployment asked for is exactly the
  substitution this project refuses to make on your behalf.

## Contributing

Contributions are welcome! Please open an issue or pull request.

## License

Apache-2.0 License — see [LICENSE](LICENSE) for details.
