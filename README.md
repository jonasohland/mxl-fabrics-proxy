# mxl-replicator

[![Docker Image](https://img.shields.io/badge/docker-jonasohland%2Fmxl--replicator-blue?logo=docker)](https://hub.docker.com/r/jonasohland/mxl-replicator)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)

Replicate MXL flows between hosts over MXL Fabrics, driven by a central control plane.

## Disclaimer

This project is experimental. Use it for evaluation and testing.

Work on a standard discovery and connection API for MXL-enabled media functions is in progress, and
this project will realign with that API. The name says what the program does instead of claiming to
be a product. The manifest format and the HTTP API can change.

Use in production at your own risk. Pin a specific image tag.

> `mxl-replicator` replaces `mxl-fabrics-proxy`. The old program stays under `legacy/go/` until the
> new implementation reaches parity. The two have no configuration compatibility and no wire
> compatibility. See [Migrating](#migrating-from-mxl-fabrics-proxy).

## Overview

An MXL flow is a ring buffer in memory-mapped files. The files live on tmpfs under a *domain*
directory: `<domain>/<flow-id>.mxl-flow/`. Each flow holds a `data` header. Video and ancillary data
go into a `grains/` directory beside it, and audio goes into a `channels` blob.

Media functions on one host open the same files. They share media with no packetization and no copy.
Readers map the files read-only, and they synchronize on futexes and not on shared mutexes. As a
result, a domain can be a read-only mount, and access control is ordinary UNIX permissions. The
permissions apply to the domain directory and to each flow inside it. See
[Architecture](docs/third_party/mxl/Architecture.md).

MXL Fabrics extends that model across hosts over libfabric. It offers the providers `tcp`, `verbs`,
`efa` and `shm`. The best path, and the one built today, is RDMA Remote Write with immediate data.
For a discrete flow, the initiator writes the grain payload and its metadata straight into the media
buffer of the target. The target does nothing while the transfer is in flight. For a continuous
flow, the initiator writes scatter-gather into a bounce buffer, and the target unpacks that buffer
into its per-channel buffers.

The specification defines Send/Recv fallbacks, but they are not built yet. See
[Fabrics](docs/third_party/mxl/Fabrics.md).

Fabrics does not find the other end, by design. A target wraps a flow writer and produces a
`TargetInfo`. The `TargetInfo` carries the fabric address, the registered memory regions and their
remote keys. The library does no capability negotiation at all. Both ends must get the same
interface configuration. The `TargetInfo` must reach the initiator over an out-of-band channel that
the caller supplies. See the [Fabrics Developer
Guide](docs/third_party/mxl/FabricsDeveloperGuide.md), section "Compatibility".

MXL leaves domain-path translation out of scope in the same way. One domain is `/dev/shm/domain1` on
a host and `/dev/shm/mxl` inside a container. The orchestration layer must reconcile the two
([Addressability](docs/third_party/mxl/Addressability.md)).

`mxl-replicator` is that channel and that orchestration layer. It decides which flows go where. It
negotiates the interface that each transfer uses. It carries the `TargetInfo` between the two ends.
It supervises the `mxl-replicator-worker` processes that move the data, where one process is one
flow, one direction, one peer and one role. It never touches grain data.

One binary carries two roles:

- Server. It holds the replication requests. It aggregates what every node reports. It negotiates
  the fabric that each session uses. It gives each agent the complete set of workers to run.
- Agent. One agent runs per node. It discovers the local flows and reports them. It runs the workers
  that the server assigns to it, and it exposes their metrics.

You request replication through an API, and not by editing a configuration file on every host. You
write the set that you want into a manifest, and you apply it:

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

### How a transfer gets established

The vocabulary of `describe` comes straight from these four steps:

1. The destination agent starts a *target* worker. The worker creates the flow writer and registers
   the buffers. It binds an endpoint in the configured port range and reports its `TargetInfo`.
2. The server passes that `TargetInfo`, plus the negotiated interface configuration, to the source
   agent. Both ends get identical configuration.
3. The source agent starts an *initiator* worker. The worker opens a flow reader and adds the
   target. The connection is asynchronous and completes inside the progress calls of the worker.
4. Grains flow. The initiator reads from the local ring buffer and writes into the remote one. The
   target commits the grains that already arrived.

That pairing is a *session*, and a session is ephemeral. When either end restarts, the server
establishes the session again under a new epoch. A stale `TargetInfo` points at remote keys that no
longer exist. An initiator that uses one moves no data and still reports perfect health. The *path*
that the session serves is derived state, and the path outlives the session.

## Features

- Central control plane. Agents register and receive assignments, and there is no agent-to-agent
  traffic. Configuration grows as O(n) in the fleet size and not as O(n²).
- Declarative. `apply` and `delete` work over a manifest, with `--dry-run` and a scoped `--prune`.
- Selectors, and not only flow IDs. You can replicate whatever camera 1 publishes, by NMOS group
  hint, and the fleet follows republished flows. You can also pin a UUID. You select domains the
  same way, by labels that an operator attaches through the API without an agent restart.
- Fan-in and fan-out. Many sources and many destinations, with one status that aggregates over them
  and a per-source breakdown beside it.
- Fail-static. A control-plane outage never stops running media. An agent acts only on an assignment
  set that it retrieved, and a server restart adopts running sessions instead of establishing them
  again.
- Two storage backends. sqlite serves a single node. etcd serves high availability behind a plain
  HTTP proxy, and it needs no sticky sessions.
- A web UI. The same binary carries it, and the server serves it same-origin with the API.
- Prometheus metrics for every worker and for the control plane.

## Quick start

This command runs both roles in one process on one host. It replicates between two domains over
`tcp`, and it needs no RDMA hardware:

```bash
mxl-replicator run \
    --agent-node loopback \
    --agent-area media=/dev/shm/mxl0:r \
    --agent-area fast=/dev/shm/mxl:rw \
    --agent-fabric provider=tcp,fabric=loopback,address=127.0.0.1
```

An *area* is a directory on this node that holds MXL domains. An area carries two independent
grants. The grant `r` lets this project discover and observe the domains under the directory. The
grant `w` lets replication create them. A domain inside an area has the fleet-wide address
`<area>/<elements>`, for example `media/cameras` or `fast/ingest`. That address is the identity of
the domain for life.

Point a media function at `/dev/shm/mxl0/cameras`. The whole manifest is then:

```yaml
name: loopback
sources:
  - node: loopback
    domain: media/cameras     # no selector: every flow in the domain
destinations:
  - {node: loopback, domain: fast/ingest}
provider: tcp
```

Then run these commands from any host that can reach the server:

```bash
mxl-replicator apply -f loopback.yaml
mxl-replicator status
mxl-replicator get paths
```

## Manifests

A manifest is a multi-document YAML file. The separator is `---`, and each document is one object.
The key `kind:` names the object and defaults to `request`. An unknown key or an unknown kind is an
error and not a warning. As a result, a misspelled field fails the apply instead of doing nothing.

```yaml
kind: namespace
name: nab
paths: exclusive           # two requests here cannot hold one path

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
    domain: {role: cameras}                   # a label set. a scalar is a name
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

The server applies documents by kind: namespaces first, then domains, then requests. The order
inside the file does not change the end state. The *intermediate* state does depend on it. A request
that lands before the labels that its selector matches looks like an apply that broke and then
repaired itself.

| Field | Meaning |
|---|---|
| `name` | The identity of the request. It is the ID and the idempotency key. An apply of the same name updates the request instead of adding a second one. |
| `sources[]` | Where to read from. Always a list, with at least one entry. A request is the cross product of this list and `destinations[]`. |
| `sources[].node` | Which node to read from. You pin it, you do not select it. |
| `sources[].domain` | A scalar is a name and a map is a label set. `media/cameras` addresses that domain. `{role: cameras}` matches every domain on the node that carries the label. An empty map is refused, because it matches everything the node happens to hold. |
| `sources[].flow` | Pin one flow ID. |
| `sources[].group_hint` | Select every flow whose `urn:x-nmos:tag:grouphint/v1.0` matches. The value is `{name, type}`. Without `type` it selects every flow that shares the name, which is how the video and the audio of one camera travel together. You can set `flow` or `group_hint`, and not both. |
| *(neither)* | A source that names a domain and says nothing else replicates every flow in it. This is the subscription shape of the retired proxy. You can spell it by omission in a manifest only. On the wire it is `"select": {"all": true}`, and an absent selector is an error. |
| `destinations[]` | Where the flows go: `node` and `domain`. |
| `destinations[].domain` | `<area>/<elements>`, for example `fast/ingest`, or `fast/studio-a/cam1` to nest. The first segment is the area, and the destination node must advertise it and grant `write` on it. The rest are path elements, each a plain name, at most 8 of them. On the wire it is `{area, elements}`, and the manifest is the only place where it is a string. |
| `destinations[].provider` | Override the request-level pin for this destination alone. |
| `destinations[].disabled` | Park this leg. The entry stays in the request and expands to nothing: no path, no session, no workers. So you can switch a route off without deleting it and typing it again. Parking a leg stops media, so preview it with `--dry-run` like any other change. A request with every destination parked reports `DISABLED`. That state is not a fault, and `status` keeps it out of the list of what is wrong. An apply that *omits* the flag enables the leg. The file is authoritative, so a leg parked through the API comes back the next time somebody applies the file that names its request. |
| `provider` | `verbs`, or `[verbs, tcp]` for "prefer verbs, tcp acceptable". Without it, the server negotiates in its configured order (EFA > Verbs > TCP > SHM). The server never substitutes a pin silently. It honors the pin, or the request fails. |
| `idle_teardown_ms` | Stop the workers of this request after its source stays idle for this long. `0` keeps them hot. |
| `sched_prio` | Ask for `SCHED_FIFO`. The server rejects the request at request time, unless every participating node has the capability. |
| `namespace` | The partition that this request belongs to, and half of its identity. `(namespace, name)` is the ID, so two requests called `cam1` in two namespaces are two requests. It defaults to `default`. The allowed characters are letters, digits, `-` and `_`. |
| `labels` | Labels ride into the worker metrics as user labels. They also narrow `apply --prune` inside its namespace. |

### Namespaces

A namespace is a partition of the request set, and a first-class object. It has a record, a
description and one rule. The rule says whether two of its requests can hold the same path.

A namespace scopes the names inside it. `(namespace, name)` is the ID of a request and its
idempotency key. Two operators can both own a request called `cam1`. A Kubernetes adapter that names
requests after pods inherits the namespacing of Kubernetes and needs no prefix.

```yaml
kind: namespace
name: nab
paths: exclusive           # default is `shared`
```

`paths: shared` is the default. It lets two requests expand onto one path. Reference counting
handles that by design: one path, one session, one worker pair, nothing doubled and nothing
corrupted. Across namespaces, this is how you express fan-in.

`paths: exclusive` refuses the overlap. The losing request reports `INVALID` and names the
incumbent. The winner holds the path, and the path carries on. Overlap costs legibility and not
integrity. In a matrix of the requests of one namespace, two lit cells that are one stream do not
sum. A cell also goes dark on a click that stopped nothing.

So the rule is opt-in, and the party that needs the guarantee is the party that asks for it.
Otherwise a third-party client has to know that the rule exists, to avoid being broken by it.

The first reference to a namespace creates it, and nothing removes a namespace on your behalf. A
delete is refused while any request references the namespace, and the error carries the count. You
cannot delete `default`.

One namespace is one file, and `--prune` makes that literal:

```bash
mxl-replicator apply -f nab.yaml --prune -n nab [-l show=x]
```

The command cancels everything in the `nab` namespace that the file does not name, and it touches no
other namespace. `-n` is required, and `-l` narrows inside it.

### Domain labels

The identity of a domain is `<area>/<elements>`, permanently. A label is an annotation that you
attach to `(node, domain)` through the API, and labels exist for *selection*:

```bash
mxl-replicator label domain studio-a:media/cameras role=cameras name=cameras
mxl-replicator label domain studio-a:media/cameras site-          # remove a key
```

You cannot rename a domain. The domain name sits inside the path identity, the session identity and
the `domain` metric label. A rename stops running media and splits every metric series that it
touches, for a metadata edit. Labels give you the same effect at no cost.

You can apply a label before the domain exists. Labeling the domain of a camera before somebody
switches the camera on is an ordinary thing to want. The label is a pending record, and
`get domains` and `describe domain` both list it. A request that selects it stays in `WAITING` until
a producer appears.

Two writers share one endpoint with two semantics:

| Gesture | Body | Merge |
|---|---|---|
| `kind: domain` in a manifest | An apply. It carries the full map that the document declares. | It owns the keys that it declares. It sets them, it removes the ones that it declared last time and no longer declares, and it leaves every other key alone. |
| `mxl-replicator label` | A patch. It carries keys to set and keys to remove. | It merges against nothing, and it does not change what a future apply believes it owns. |

So `label` and `apply` do not fight, because they own different keys. An operator can name a domain
interactively and keep that name, in a fleet where somebody else applies the requests from git. This
is the three-way merge of `kubectl apply`, adopted deliberately. This file format is close enough to
a Kubernetes manifest that people arrive with those expectations. A surprise there costs more than a
tidier rule earns.

`--prune` never touches a label. Otherwise a file that names three domains prunes the other forty. A
label records what a host has, and not what an operator wants from it.

A label write starts and stops media, one level of indirection away. So the command takes
`--dry-run`, and it prints a *blast radius* on the real write as well: for each path that stops, the
requests that fed it. The command prints and does not prompt. The same people script this CLI and
use it interactively, and a verb that blocks on a tty hangs in a pipeline.

#### What a label selector will not match

A label selector never matches a flow that this project writes itself. Without that rule,
replication feeds itself. A flow copied to a node becomes visible on that node. A broad selector
then matches its own output, and the path set grows on every pass. The growth stops, but only after
conflict precedence decides the topology instead of the operator.

Naming a domain directly still reaches every flow, and that is how you write `A→B→C`:

```yaml
sources: [{node: edge-01, domain: fast/ingest, flow: "…"}]   # explicit: intent
sources: [{node: edge-01, domain: {role: onward}}]           # matched: never its own output
```

The signal belongs to the *flow* and not to the directory. A domain can hold one replicated flow
beside nine that a local media function produced, and it offers the nine and withholds the one. `get
flows` and `describe domain` both carry a `REPLICATED` column. When an expansion drops a flow for
this reason, the status of the request says so. Otherwise you cannot tell a skipped flow from a flow
that was never there.

### Many sources, many destinations

Both ends are lists, and a request is their cross product. Three studios into two edges is six
pairings, and each pairing expands the selector of its own source. The node of a source stays
pinned, so the list is always something the author typed and never a second selector. As a result,
you can count the pairings of a request by reading it.

The server makes sure that every pairing is valid and negotiates each one separately. A request can
be viable for eleven pairings and refused for the twelfth. The reason names the end that failed:

- A failure common to every destination of one source names the source.
- A failure common to every source of one destination names the destination.
- A failure that applies to every pairing names neither end.

A wrong attribution sends an operator to a node where everything is fine.

Two things follow, because the two ends no longer share one fate:

- A request whose paths disagree, with at least one path `ACTIVE`, is `PARTIAL`. One dark camera
  among twelve is the ordinary state of an ingest wall. `PAUSED` is then a true statement about one
  path and a false one about the request. `describe request` prints one row per source, and that row
  names the camera.
- Two sources of one flow ID into one destination are refused. When both sources pin the UUID, the
  error is `duplicate_source_flow` at `POST`. When the second source arrives later through a
  selector, the error is `flow_conflict` on the path, and the server tears the loser down.

Fan-in as several documents that share a destination domain still works, and it still counts
references down to one session. One request over the whole set gives you a single unit of intent
instead.

## Commands

```
mxl-replicator run      [--server] [--agent] [flags]    the daemon; both roles by default
mxl-replicator apply    -f <manifest> [--dry-run] [--prune -n nab [-l show=x]]
mxl-replicator delete   -f <manifest> | [-n nab] <name>...   only the kinds and names are read
mxl-replicator label    domain <node>:<area>/<elements> k=v k-   [--dry-run]
mxl-replicator status   [-o json|yaml]
mxl-replicator get      nodes|domains|flows|requests|paths|sessions|namespaces [filters] [-o json|yaml]
mxl-replicator describe node|domain|flow|request|path|session|namespace <name> [-o json|yaml]
mxl-replicator events   path|request|node <name> [--since <seq>]
mxl-replicator logs     path <path-id>
```

`--server` and `--agent` each select one role *alone*. When you name neither, the process runs both,
which is the single-host case and the development case. Two roles in one process still speak HTTP to
each other, so there is exactly one code path.

### apply

`apply` creates or updates, keyed on the `name` of each document. An apply of an unchanged request
writes nothing and prints `unchanged`, so a controller that applies again on every resync costs the
store nothing.

The command applies documents in file order. This is not atomic. A failure reports which document
failed, leaves the earlier ones applied, and exits non-zero. Requests are independent durable intent,
so the command does not roll back the ones that landed.

`apply` and `delete` print one line per document: the name, then what happened to it. The output is
a list and not a table. `get` and `status` are where the tables are.

`-f` is repeatable. It takes a file, a directory of `*.yaml` files (flat, sorted, not recursive), or
`-` for stdin.

`--dry-run` makes sure that the manifest is valid, reconciles it against the real fleet, and reports
the outcome without writing. It sees the stored state plus the one request. So two *new* documents
in one file that conflict with each other both pass, and the second one fails on the real apply.

### delete

`delete -f` reads only the kinds and the names from a manifest, and it ignores everything else. A
document that holds nothing but `kind:` and `name:` is a complete instruction. A file that
drifted from what is deployed still removes what it named. That is the case that matters,
because you want to delete what a file created at the point where the file stopped describing
anything accurately.

`delete [-n nab] <name>...` takes the names directly. The command deletes documents in reverse apply
order: requests first, then the namespaces they live in. It skips `kind: domain` documents. To remove
labels, use `label key-` or an apply that no longer declares them, because both say what they mean.

A delete of something that is not there succeeds, so a second run of a delete does not fail because
the first run worked.

`--prune` cancels the requests in `--namespace` that the manifest does not name. The namespace is
required. A prune of everything a file does not name cancels requests that the file knows nothing
about, and the canceled object is moving video. So the scope is a partition that you declared and
not a tag that you happened to apply, and `-l` narrows inside it. There is no confirmation prompt,
and `--dry-run` shows the set.

```bash
mxl-replicator apply -f studio-a.yaml --prune -n nab -l show=nab
```

Prune covers requests only. It removes no namespace and no domain label, not even for the kinds that
the file contains.

### status, get and describe

Three read verbs do three jobs and do not overlap:

| | |
|---|---|
| `status` | Counts the fleet, then names only what is not active. It is not a list. The answer at 3am is "these two things are broken", and not a screen to scan. |
| `get <kind>` | Lists objects, so you can find the name of the one you want. |
| `describe <kind> <name>` | Prints everything known about one of them. |

```console
$ mxl-replicator status
nodes      3 registered, 3 leased
requests   4  (1 WAITING, 3 ACTIVE)
paths      7  (1 WAITING, 6 ACTIVE)
sessions   6 running

KIND     NAME      STATE    REASON
request  cam3      WAITING  the selector matches no flow in studio-b/media/cameras
```

The states, worst first, are `INVALID`, `FAILED`, `DEGRADED`, `WAITING`, `ESTABLISHING`, `PAUSED`,
`ACTIVE` and `DISABLED`. A request aggregates over its paths. The `PATHS` column of `get requests`
carries the "1 of 3" that a one-flow-per-request model cannot express.

Two more states are aggregate only. They describe a set, so they never appear on a path, a session
or a worker.

`PARTIAL`. A request whose paths disagree, with at least one path `ACTIVE`, reports this instead of
the worst one. One bad path among twenty must not condemn the other nineteen on the line that you
read first. The detail lives in the per-source rows of `describe request` and in the per-path
metrics.

`DISABLED`. Every destination of the request is parked with `disabled: true`, so the request asks
for nothing. It sorts after `ACTIVE`. `status` deliberately keeps it out of the list of what is
wrong, because somebody switched it off on purpose. `status` still counts it beside every other
state, because a parked route that nobody remembers is exactly what wants finding. A request with
one live destination beside a parked one is not `DISABLED`, and it folds over the legs that it still
has.

`PAUSED` is the state worth knowing. It separates *the plumbing is broken* from *the source is not
producing*. The two look identical from a "no media at the destination" alarm, and they have
completely different owners.

`get` takes the same nouns in the plural, and the singular works too. It makes sure that each filter
applies instead of ignoring it. `--node` applies to domains, flows, paths and sessions. `--domain`
applies to flows. `-n` and `-l` apply to requests. A filter that cannot apply is an error. Otherwise
a silent filter makes you conclude that a flow is missing after you narrowed on the wrong field.

`describe` takes one of seven nouns:

| | |
|---|---|
| `node` | What the agent advertises: its areas and their grants, its fabric attachments and its versions. It also lists the domains that the node observes now, and every path that touches the node, with the role of the node in each. |
| `domain` | `<node>:<area>/<elements>`: its labels, whether the node reports it, and for each flow whether this node is the one that writes it. |
| `flow` | A flow ID is unique to the media and not to a location. After replication the same ID exists on both nodes. This lists every place where the flow is, whether each place produces it, and which paths carry it. |
| `request` | The stored intent, its destinations and pins, the per-path breakdown, and what the expansion excluded. A flow that a label selector skipped has no path to carry a reason, so this command lists it or nothing does. |
| `path` | The deduplicated edge, its state, and its reference count: which requests share it, and so the effect of canceling one. |
| `session` | The concrete worker pair: the negotiated fabric and interface configuration, the epoch, and the state, bound endpoint, restart count and uptime of each end. |
| `namespace` | The partition: its `paths` policy, and the requests in it. |

Path and session stay separate, although they are 1:1 in practice, because they are separate layers.
A path is derived state that outlives any single session. A session is ephemeral, and the server
establishes it again whenever either end restarts. One combined object suggests that a path dies
with its workers, and a path does not.

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

`-o json` and `-o yaml` print the API object verbatim. A script that reads them is written against
the documented API and not against the command.

### events and logs

`describe` says what a thing is now. These two commands say how it got there.

`events path|request|node <name>` prints the ordered history: state transitions, epoch changes and
worker restarts. `--since <seq>` picks up where a previous read stopped. `logs path <path-id>` prints
the output of the last failing worker for that path. That output is the one place where a libfabric
error message survives the process that produced it.

Only `path` takes `logs`. A tail belongs to a failure, and failures belong to paths.

## Web UI

`make ui` builds a Vue 3 single-page app into the binary, and `--server-ui` serves it at `/`. The app
is always same-origin with the API, whether this binary serves it or a proxy in front of both does.
So there is no API base URL to configure, and the server emits no CORS headers at all.

The app is a routing matrix and not a form over the API. Sources run down one axis and destination
domains run across the other, with one cell per pairing. Gestures stage a change and apply it as one
manifest. A `shared` namespace renders as a ledger instead, for the reason that
[Namespaces](#namespaces) gives: where two requests can hold one path, a lit cell is not an honest
answer to "is this route on". The app also has a topology view, an unrouted-sources strip, and
editors for sources, destinations, splits and labels.

`--server-ui` on a binary built without the assets is a startup error and not an empty page, so
`make replicator-ui` is the target that produces both.

## Agent configuration

The agent takes flags and YAML, and all of it is provisioning-level. It changes with the build of a
host, and not with the routing of a flow.

| Flag | Meaning |
|---|---|
| `--agent-node` | Fleet-wide unique node name. It defaults to the hostname. |
| `--agent-server` | Control-plane URL. Repeatable for high availability. |
| `--agent-area name=/path:rw` | Declare an area with its grants: `r` to discover and observe the domains under it, `w` to create them. Repeatable. A node with no readable area offers no sources, and a node with no writable area accepts no destinations. |
| `--agent-fabric provider=,fabric=,device=` | Declare a fabric attachment. Repeatable. The naming selectors are `address=`, `interface=`, `device=` or none. `network=10.1.0.0/16` and `ip_version=4\|6` narrow the choice. |
| `--agent-detect-default-fabric` | With no attachment configured, detect one from what libfabric reports, and label it `default`. It takes the best provider first, and for `tcp` the first routable IPv4 address. Nodes pair only with other nodes that carry the same label, so this flag suits a flat network. |
| `--agent-port-range` | The range that the agent binds target workers in. It is inbound to the *destination* node, so open it there. |
| `--agent-config` | A YAML file that supplies any of the above. |

### Areas and domains

A domain is a place and not a channel. There is one kind of domain, whichever direction this project
uses it in: a directory inside an area that holds flows. Several processes routinely write different
flows into one directory. The security model of MXL is per-file UNIX permissions and not
per-directory ownership. So this project is one participant among the media functions of a node, and
never the owner of a directory.

An area is a directory that an operator designated. It has a name and two independent grants:

```yaml
areas:
  - {name: media, path: /dev/shm/mxl,            read: true}
  - {name: fast,  path: /dev/shm/mxl/replicated, read: true, write: true}
  - {name: bulk,  path: /mnt/nvme/mxl,           read: true, write: true}
```

`read` is the whole authority of this project to discover and observe the domains under that
directory. `write` is the whole authority to create them and to write flows into them. Neither grant
implies the other, and both default to false. So access to the filesystem of a node is opt-in per
node and per direction. An area that grants neither is a line that does nothing, and the agent
refuses it at startup.

A read-only grant can be a read-only mount, and that is not a coincidence. MXL flow readers map
`PROT_READ` and synchronize on futexes, precisely so that a reader never needs write access to the
volume.

The fleet-wide identity of a domain is the name of its area followed by its path elements, and that
is its identity for life. Under the layout above, `/dev/shm/mxl/studio-a/cam1` is
`media/studio-a/cam1`, and `/dev/shm/mxl/replicated/ingest` is `fast/ingest`. This is the
domain-path translation that MXL leaves to the orchestration layer. This project does it once,
fleet-wide, so that `media/cameras` means the same thing on a host, in a container and in a metric
label.

Areas can nest, and the innermost containing area names a directory. The longest prefix wins. So
`media` as an ancestor of `fast` leaves nothing to disambiguate: `fast` contains
`.../replicated/ingest` more tightly, so the name is `fast/ingest` and never
`media/replicated/ingest`. Two areas on one directory are the one arrangement that the rule cannot
decide, and the agent refuses them at startup and names both. Everything else is legal. The common
layout is one MXL area per host with a subtree that replication writes into. That layout is now two
ordinary areas and not an exception to a rule.

So a directory has exactly one identity, whether discovery found it or the reconciler created it,
and the two cannot disagree.

A destination is always a name inside an area that the operator granted `write` on. The API never
accepts a raw path. That invariant stops the API from being an arbitrary remote filesystem write on
every node in the fleet, and it holds whatever authentication you configure. The area is the entire
perimeter, it is node-local configuration, and the API cannot set it. The server and the agent both
make sure that a destination is inside a granted area. The agent is the final authority about its
own filesystem.

Discovery prunes nothing. The agent reports a domain that this project writes into like any other:
it appears in `get domains`, and you can label it. A flow in it that this node does not write is
selectable like any other. An example is a flow that a local media function produced beside the
replicated ones. What a flow cannot do is match into a copy of itself, which is a rule about the
flow and not about the directory.

A domain can nest. `fast/studio-a/cam1` groups destinations without an area per group. One
materialized domain cannot contain another, so `fast/studio-a` and `fast/studio-a/cam1` cannot both
exist on one node.

A new directory for an area keeps every identity on it. A change from `path: /dev/shm/mxl` to
`path: /mnt/mxl` under the same area name leaves every domain called what it was. Paths and
sessions survive the move instead of rebuilding. A move of a domain to a different *area* does
re-identify it. That asymmetry is deliberate, because relocating a mount is not the same gesture as
choosing a different destination.

### Fabric attachments

Nodes declare `(provider, fabric, address)` triples, and not bare provider names. `fabric` is an
opaque label that an operator assigns. Two nodes pair on a provider only with a shared label:

```yaml
fabrics:
  - {provider: verbs, fabric: ib-fabric-a,   device: mlx5_0, ip_version: 4}
  - {provider: tcp,   fabric: dc1-data,      network: 10.1.0.0/16}
  - {provider: efa,   fabric: vpc1-subnet-a, device: rdmap0s6-rdm}
```

An available provider is not a reachable one. Two nodes that both offer `verbs` can sit on different
InfiniBand fabrics. Two nodes that both offer `efa` can sit in different VPCs. An intersection of
provider *names* cheerfully assigns a session that cannot connect, and that failure is invisible:
the target starts clean, and the connect loop of the initiator spins.

Selectors come in two classes. A naming selector says which interface to use: `address`, `interface`
or `device`. A node with exactly one interface of that provider needs none, and an attachment
carries at most one naming selector. Narrowing selectors say which of the addresses counts. They are
`network` and `ip_version`, and they combine with a naming selector and with each other.

The agent resolves all of them at startup. It runs the `--interfaces` probe of the worker, which
calls `mxlFabricsGetInterfaces()` and reports what libfabric actually finds on the node. Exactly one
probe entry must survive. Zero entries or several entries are a loud startup error and not a guess.

An entry is one combination of physical interface, address and provider, which is why a name alone
is often ambiguous. An HCA that reports both an IPv4 address and a link-local IPv6 address is two
entries under one device name. The naming selectors are not interchangeable either. `device` is the
libfabric device name, for example `mlx5_0` or `rdmap0s6-rdm`. `interface` is the netdev name, which
exists for `tcp` and `verbs` and not for `efa`.

An `efa` attachment that names an interface is refused at startup, because the probe has no netdev
name to match it against.

`address` is the exact escape hatch and it is always unique, but it costs one value per node, which
a DaemonSet does not have. So `device: mlx5_0` with `ip_version: 4` is usually the better answer.
`network: 10.1.0.0/16` is better still where it applies, because it picks the own address of each
node inside a prefix and names no hardware at all. Neither narrowing selector asserts anything about
reachability. Two nodes inside one prefix can still have no route between them, and the fabric label
is where you say so.

### Network security

The media plane has no authentication and no encryption, by the design of the layer underneath. A
`TargetInfo` carries registered memory addresses and RDMA remote keys, and a remote key is not an
access control mechanism. It exists to catch a write aimed at the wrong region. Any process that
reaches the endpoint of a target and knows its key can write into that memory. Transfers go over the
wire in the clear.

So the fabric network is the perimeter, and it must be an infrastructure-level one: ACLs, firewall
rules, an isolated VLAN or an isolated subnet. The control plane decides who can *ask* for a
transfer. It cannot decide who can perform one.

Plan around two consequences:

- `--agent-port-range` is inbound on the destination node. Open it to the source nodes and to
  nothing else.
- A fabric label that spans a trust boundary lets the control plane assign sessions straight across
  that boundary.

## Docker images

```bash
# Regular
docker pull jonasohland/mxl-replicator:latest

# EFA optimized
docker pull jonasohland/mxl-replicator:latest-efa
```

Both images carry `mxl-replicator` and `mxl-replicator-worker`. `make image` and `make image-efa`
build them. `make image-test` adds `mxl-mock-src` and `mxl-mock-sink` for the end-to-end suite. Do
not publish that image as `:latest`.

## Kubernetes

[`deployment/mxl-replicator/`](deployment/mxl-replicator/) is a Helm chart. It runs the server as a
Deployment and the agent as a DaemonSet, and it requires nothing else.

```bash
helm install mxl-replicator ./deployment/mxl-replicator \
    --namespace mxl --create-namespace \
    --set image.tag=v0.3.0 \
    --set server.persistence.node=<node> \
    --set-json 'agent.fabrics=[{"provider":"tcp","fabric":"dc1-data","interface":"eth1"}]'

kubectl label node <node> mxl.ebu.org/mxl-replicator=true
```

The chart provisions the fleet. It never decides what the fleet replicates, which stays an `apply`
against the API. The chart does not guess at three things:

- Which node holds the sqlite store (`server.persistence.node`, a directory on that node by
  default).
- What each node can be reached on (`agent.fabrics`).
- What filesystem authority each node grants (`agent.areas`).

`agent.efa.enabled` requests `vpc.amazonaws.com/efa` from the AWS device plugin and switches the
agent onto the `-efa` image. `agent.pools` runs several agent DaemonSets over a fleet whose nodes are
not alike. The [chart README](deployment/mxl-replicator/README.md) covers both, and
[`examples/`](deployment/mxl-replicator/examples/) has complete values files for a single node, an
EFA cluster and a mixed fleet.

## Building from source

```bash
# Requires CMake, a C++20 compiler and Go 1.26+
make all

# Or just the Go side
make replicator

# With the web UI (adds an npm toolchain)
make replicator-ui
```

`make all` builds the worker through CMake and builds every Go binary. That includes the legacy
proxy, which stays in the tree until the new implementation reaches parity. The UI build is
deliberately not a prerequisite of either Go target, so a plain `go build` needs no npm toolchain.

## Observability

The agent serves Prometheus metrics on `--agent-listen` (`:2284`) and the server serves them on
`--server-listen` (`:2283`), both at `/metrics`.

The prefixes split by what the metric describes, and not by which process emits it:

- `mxl_*`. Anything about a flow or a transfer. These names are unchanged from `mxl-fabrics-proxy`,
  so existing dashboards keep working.
- `mxl_repl_*`. Control-plane metrics that exist only because of this project: requests by status,
  sessions, leased agents, epoch transitions per session, reconcile duration and store latency.
  Epoch transitions per session are an excellent flapping signal.

The agent scrapes the worker metrics on demand, inside the request, through a bounded pool with
per-worker deadlines and an overall deadline. `/healthz` stays green while a transfer fails, because
an unreachable peer is no reason to restart the agent and drop every other flow. Watch the status
and the metrics for failures instead.

The server serves `/healthz` and `/readyz`, and they answer different questions. Readiness reports
whether the reconciler settled. A load balancer must use readiness, and a restart policy must
not. The agent serves `/healthz` only. There is no definition yet of what a *ready* agent is, so its
readiness probe uses the same endpoint. That probe proves that the process runs, and not that the
agent registered.

## Migrating from mxl-fabrics-proxy

There is no configuration compatibility, no wire compatibility and no importer. The legacy file is a
per-node configuration. Its subscriptions are addressed by `mxl://` URL, and its destination is a
`-m` mapping. This design moved all three deliberately. Write the `subscriptions:` block again as a
manifest.

What carries over:

- The `mxl_*` metric names. Five label changes do not:

  | Was | Is | Why |
  |---|---|---|
  | `flowID="…"` | `flow_id="…"` | Prometheus convention. |
  | `domain="/dev/shm/mxl0"` | `domain="media/cameras"` | The fleet-wide identity and not a path. It is stable across hosts, and it is the same value on both ends of a transfer. |
  | `quantile="0.010"` | `quantile="0.01"` | Three fixed decimals are not how anything else renders a quantile. |
  | (none) | `session="…"` | New. Without it, two initiators on one flow are one duplicated series. |
  | (none) | `namespace="…"` | New. It names the partition that a transfer belongs to. |

Three things need a decision rather than a translation:

- `-m name=/path` and the `domains:` block are gone. They did two jobs at once: they granted the
  agent authority to read a directory, and they gave that directory a fleet-wide name. Those two
  jobs are now separate. The grant becomes the `read` bit of an area. The naming becomes the name of
  the area plus what the filesystem already decided, and you express anything friendlier as a label.
  That also means that naming a domain no longer costs an agent restart, and an agent restart
  re-establishes every flow on the node.
- A legacy mapping used as a subscription destination becomes a domain name inside an area that the
  node grants `write` on.
- `defaults.provider` was a per-side setting. The server now negotiates a provider per session
  against the declared fabric attachments. Carry the old value over as a request-level `provider`
  pin, then relax it deliberately. Nothing here widens the request of an existing deployment on your
  behalf.

## Documentation

| | |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | The specification: how the system is built and why, with every settled design decision and the reasoning that closed it. |
| [`docs/worker-runtime-surface.md`](docs/worker-runtime-surface.md) | The contract with the C++ `mxl-replicator-worker` binary. |
| [`docs/ui.md`](docs/ui.md) | The design argument of the web UI, and the parts of the API that a UI gets wrong. |
| [`docs/open-items.md`](docs/open-items.md) | What is not closed. |

[`docs/third_party/mxl/`](docs/third_party/mxl/) holds the upstream MXL documentation that this
project is built against. [Architecture](docs/third_party/mxl/Architecture.md) covers the shared
memory model. [Fabrics](docs/third_party/mxl/Fabrics.md) and the [Fabrics Developer
Guide](docs/third_party/mxl/FabricsDeveloperGuide.md) cover the transport.
[Addressability](docs/third_party/mxl/Addressability.md) covers the `mxl://` scheme and the
domain-path translation that it leaves to callers.

## Contributing

Contributions are welcome. Open an issue or a pull request.

## License

Apache-2.0 License. See [LICENSE](LICENSE) for details.
