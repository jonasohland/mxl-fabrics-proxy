# Handoff: a web UI for `mxl-replicator`

This document is for whoever implements the web UI. `docs/architecture.md` is the specification, and
`docs/open-items.md` lists the questions the specification has not closed. This document is neither.
It collects the parts of both that a UI needs, and adds the mistakes a UI is likely to make that
neither document warns about, because neither was written with a UI in mind.

A reference of the form §N points at `docs/architecture.md`. Every statement here about the API was
checked on **2026-09-01** against a server built from this tree and driven by the fake fleet of §9
of this document. The response bodies quoted are real ones. Where something was read from the source
code rather than exercised against a server, the text says so.

*An earlier version of this document was written on 2026-08-28 against `rewrite.md`.* That file is
still in the tree as a frozen predecessor of `docs/architecture.md`. The section numbers did not
change (§7.2 is still §7.2), but the content behind several of them did. Three changes since then
required rewriting parts of this document rather than editing them, and each is marked where it
applies:

- **Areas replaced output roots** (§10.6).
- **A request has a list of sources** (§9.1).
- **A namespace is a real object** (§9.3).

The control plane is complete and shipped, and nothing here asks you to change it. Where the UI needs
something the server does not have, the text says so explicitly instead of assuming it exists.

**The UI described here has since been built** in `ui/app/`, with Vue 3 and TypeScript. It has the
landing page, the matrix and its staged gestures, the ledger, the source, destination, split and label
editors, the unrouted-sources strip and the topology view. This document remains the design
argument. Four things this document asserted
turned out to be wrong or impossible to build as written. Each is corrected in the section it
affects, in the same way as the three changes above:

- **`status.sources[]` is always present and always the full list**, not an optional field that is
  absent when there is nothing to say (§4 of this document, and §7c, where the original claim mattered).
- **The reserved label names are not the ones this document listed.** The enforced set includes
  `format` and `media_type` and does not include `domain_name` (§3 of this document).
- **A path held by several requests in a `shared` namespace is not "contested".** That is refcounting
  working as designed. The conversion planner built on the other reading was removed after it had
  been built and verified (§7c).
- **"Unrouted" cannot mean "no request selects it".** Computing that would mean evaluating selectors
  in the browser, which this document forbids two sections earlier. Unrouted is computed from the
  path list instead, and the one behavioural difference this causes is described explicitly (§7a,
  *Unrouted sources*).

Everything else held, including the parts that were argued rather than tested: a request as a
rectangle, rows and columns as selectors rather than stored objects, and staging changes instead
of applying each click.

**The newest feature covered here is `disabled` on a destination** (§9.1), together with the
`DISABLED` aggregate state that follows from it (§11). It is covered in the main text rather than a
footnote because the matrix depends on it. Without it, a route that is switched off is a route that
does not exist, so §7a's axes could only show the routes currently running, not the board an
operator laid out. It is **built**, with the reconciler, CLI and manifest support the rest of this
document assumes.

---

## 0. Read this, skip that

| Read | Why |
|---|---|
| §3, *Identity and terminology* | The nouns. The UI is a rendering of them and it will be wrong if they blur. ~30 lines. |
| §4, *State model* | Desired / observed / derived. It explains why some things are editable and most are not. |
| §9.1, *User API* | The contract you are building against, including the fan-in subsection — it is what §7a is built on. |
| §9.3, *Namespaces* | Short, and the matrix does not work without the rule it defines. |
| §11 + §11.1, *Status and failure semantics* | Nine states, including `DISABLED`, and what an operator decides differently on seeing each. |
| §10.6, *Domains, areas and grants* | The single most constrained part of the create form. |
| §10.7, *Domain labels* | Where a source's domain selector gets its meaning, and it is a mutation the UI has to offer. |
| `README.md` §Manifests, §Commands | The operator's current mental model. The UI must not contradict it. It has been rewritten for areas and for `sources:` and is trustworthy again. |

Skip these unless something forces you to read them. They describe the machinery under the API, and
none of it is visible through the API:

- §5 (epochs and pairing).
- §6 (agent internals). §6.3 is worth two minutes: it explains why a worker can sit in `starting`
  for a minute without that being a fault.
- §7.3–7.4.
- §8 (storage).
- §14–15 (the worker).

§12 (observability) is worth ten minutes once you reach the question of where rate graphs come from,
in §8 of this document.

The ground truth for wire types is `internal/api/`: fourteen non-test files that import only the
standard library, with doc comments that are better than any summary of them. The ones a UI needs
are `request.go`, `status.go`, `caps.go`, `domain.go`, `domainselector.go`, `domainlabels.go`,
`selector.go`, `inventory.go`, `namespace.go`, `event.go`, `wire.go` and `routes.go`. Read those
rather than trusting the shapes reproduced here.

---

## 1. The model, in one page

In short: a user writes a **request**, which names some sources and some destinations. The server
expands each request into **paths**, one for every (source flow, destination) pair it covers. Paths
from different requests that cover the same pair are merged into one. Each path is carried by a
**session**, which is a pair of workers, one on each node. Everything else in this section defines
the nouns in that sentence and the ones they depend on.

```
  Request  ──expands to──▶  Path  ──realised by──▶  Session  ──▶  2 workers
 (durable          S×D×F   (derived,        0..1   (ephemeral)
  intent)                   refcounted)
```

**Node.** One host running one agent, with a unique name assigned by the operator. A node has two
separate records:

- A **registration**, which is durable and survives the agent being down.
- A **liveness lease**, which has a TTL. `node.live` reports the lease.

A node that is registered but has no lease is information, not an alarm (§4.2).

**Area.** A directory that an operator has designated on a node as a place where MXL domains live.
It has a name and two independent grants:

- `read`: domains in this area may be discovered and observed.
- `write`: replication may create domains in this area.

Both grants default to false and neither implies the other. A node with no readable area offers no
sources, and a node with no writable area accepts no destinations. The grants are the only authority
this project has over a node's filesystem, and they are why the API cannot be used to write
arbitrary files on remote hosts (§10.6, §13). The UI reads them from `GET /v1/nodes` →
`capabilities.areas[]`.

**Domain.** A directory inside an area that holds flows. There is **one kind** of domain, whichever
direction this project uses it in: a domain is a place, not a channel. Its identity across the fleet
is `<area>/<elements>`, for example `media/cameras` or `fast/studio-a/cam1`, and it keeps that
identity for as long as it exists. On the wire it is sent as
`{"area": "fast", "elements": ["studio-a", "cam1"]}`. The rendered string is used wherever a domain
has to be a single token: a metric label, a path's source address, an error message.

*This supersedes the earlier model, in which input and output domains were separate concepts with
separate identities: an input domain was named by its absolute path, and an output domain by
elements under an output root.* Both spellings are gone, and so is the section on "two kinds" of
domain that this document used to open with. Its operational rules still hold:

- The API never creates a domain as an action of its own.
- A domain that a request materialises has no lifecycle beyond the paths that target it.
- **There is no API to create or delete a domain, and the UI must not look as if there were one.**

What changed is that a domain replication writes into is discovered, observed and listed like any
other domain, so it is no longer a different noun.

**Domain label.** A key/value pair an operator attaches to a `(node, domain)` through the API. Labels
annotate a domain; they are never part of its identity, so relabelling never moves a path, a session
or a metric series. A request's source selector matches on labels, so **writing a label is a
mutation that can start and stop media**, with one level of indirection between it and the request
(§10.7). Give it the same confirmation and preview as a request edit.

**Flow.** A UUID that identifies the *media*, not a location. After replication the same flow ID
exists on both nodes, which is the intended result. A flow is addressed by `(node, domain, flow-id)`.
The domain component is required, because the same ID can legitimately exist twice on one host.
**Never key a UI list on flow ID alone.**

**Request.** Durable user intent, and the only thing a user writes. A request has a **list of
sources** and a **list of destinations**, lives in a namespace, and is identified by
`(namespace, name)`, which serves as both its ID and its idempotency key. A request is **never
cancelled because its session is failing** (§11); the UI must not offer or imply that. A destination
may be marked `disabled`, which parks that leg without removing it: the entry stays in the spec and
expands to nothing. This is the only way the model can express *off* (§9.1; §7a of this document
explains why the matrix needs it).

**Namespace.** A first-class object that partitions requests. It has a name, a `paths` mode and a
description. It scopes request names and `--prune`, and its mode says whether requests inside it may
share a path. It does **not** partition nodes, domains or destinations (§9.3; §7b of this document).

**Path.** The deduplicated edge `(src node, src domain, flow) → (dst node, dst domain)`. Paths are
derived and recomputed on every reconcile. They are refcounted: if N requests expand onto the same
edge, they share one path and one worker pair. `path.requests[]` is the refcount, and it answers
"what happens if I cancel this request".

**Session.** The concrete worker pair that carries a path. Sessions are ephemeral and are
re-established whenever either end restarts. In practice a session and a path are 1:1, but they are
**deliberately separate layers**, because a path outlives any particular session. The CLI keeps
`describe path` and `describe session` apart for this reason, and the UI should keep the same
separation. Merging them in the UI would imply that a path dies when its workers do, which is the
opposite of what the design guarantees.

**Cardinality.** This is the part that most often surprises people. A request expands to the **cross
product** of every source's matched flows and every destination. Two sources matching 2 and 1 flows,
sent to 3 destinations, give **9 paths**. The request's status is an aggregate over those paths:

- `status.counts` is the breakdown by state.
- `status.sources[]` is the same breakdown per source. This is the one an operator reads when a
  request spans several studios.

A request with one flow and one destination is a set of size one, not a different shape. Model it as
a set from the first line of code; adding this later is expensive.

---

## 2. The API you have

The base path is `/v1`, and everything is JSON. The other prefix, `/agent/v1`, is the **privileged**
API. Anything that can call it can claim to be a node, inject fake inventory, and read other nodes'
`target_info` (RDMA rkeys). **The UI must never call it**, except in the local seeding harness of §9
of this document.

| Method | Path | Returns |
|---|---|---|
| `GET` | `/v1/requests[?namespace=]` | `{requests: Request[]}` — fleet-wide list |
| `GET` | `/v1/namespaces/{ns}/requests` | the same, one namespace |
| `GET` | `/v1/namespaces/{ns}/requests/{name}` | `Request` — the id is `(namespace, name)` |
| `POST` | `/v1/namespaces/{ns}/requests[?dry_run=true]` | `Request`, 201 created / 200 existed, `X-Mxl-Outcome` header |
| `DELETE` | `/v1/namespaces/{ns}/requests/{name}` | 204, or 404 `not_found` |
| `GET` | `/v1/namespaces` | `{namespaces: [{name, paths, description, requests}]}` |
| `GET` | `/v1/namespaces/{ns}` | one of them |
| `POST` | `/v1/namespaces` | create or update, keyed on name |
| `DELETE` | `/v1/namespaces/{ns}` | 204, or 409 while any request references it |
| `GET` | `/v1/nodes` | `{nodes: Node[]}` — liveness, capabilities, **areas with their grants** |
| `GET` | `/v1/nodes/{node}/domains` | `{node, settling?, domains: DomainInfo[]}` — observed, joined with labels |
| `POST` | `/v1/nodes/{node}/domains[?dry_run=true]` | write labels on one `(node, domain)`; returns the record and its blast radius |
| `GET` | `/v1/flows[?node=&domain=&flow=&group_hint=&type=]` | `{flows: FlowEntry[]}` — carries `producing` and `replicated` |
| `GET` | `/v1/paths` | `{settling?: bool, paths: Path[]}` |

Outside both prefixes, and unauthenticated: `GET /healthz`, `GET /readyz`, `GET /metrics`.

### What is not there

Design around the absence of each of these from the start:

- **No `GET /v1/nodes/{node}`.** §9.1 lists it, but the mux does not register it and it returns
  **404** (verified). Fetch the list and filter it, as `describe node` does. If the route is added
  later nothing breaks, but do not build on it now.
- **No `/v1/sessions`.** Sessions are reached through paths, inline on `GET /v1/paths` as
  `path.session`. This is deliberate: a session has no identity apart from the path it carries.
- **No `/v1/paths/{id}`.** Fetch the list and match `id` exactly.
- **No watch, stream, SSE or websocket on the user API.** The revision-cursor long poll exists only
  on the agent API, and only for assignments. **The UI polls.**
- **No ETag, `If-None-Match` or revision cursor on any read.** There is currently no cheap way to ask
  whether anything has changed. See the cost note below.
- **No pagination, sorting or filtering**, apart from `?namespace=` on requests and the five query
  parameters on `/v1/flows`.
- **No batch create.** Each request is its own POST.
- **No CORS, and none is wanted.** An `OPTIONS` preflight to `/v1/paths` returns
  `405 Method Not Allowed`, and a cross-origin `GET` carries no `Access-Control-*` header. **The UI
  is always same-origin with the API** (§6 of this document). In production this is not an issue; in
  development it is solved with a dev-server proxy, not by adding CORS middleware.
- ~~**No event log or history.**~~ **There is one now.** This entry said the opposite for long enough
  that it is struck through rather than deleted. Architecture §12.1 and §12.2 are built, so "this
  path failed twice in the last hour" can be answered. Each object has a bounded ring of events in
  which repeated entries are coalesced. The rings are read at:
  - `GET /v1/paths/{id}/events`
  - `GET /v1/namespaces/{ns}/requests/{name}/events`
  - `GET /v1/nodes/{node}/events`

  and the output of the last failing worker is at `GET /v1/paths/{id}/logs`. The "History or events"
  bullet in §8 of this document changed in the same way.

  **These are the only reads on this API whose cost does not grow with the fleet.** An event read is
  one `Get` on one key and does not run `Compute`: a ring records what already happened, so there is
  nothing to recompute. They are the exception to the next subsection, and the one class of endpoint
  where polling faster is affordable. The request's event read is an exception to that exception: it
  merges the rings of the request's paths, and working out which paths those are is a derived
  computation.

  `docs/open-items.md` §2.11 lists five things an event renderer must get right that are not visible
  in the JSON. `components/EventLog.vue` and `model/events.ts` handle all five, and each is covered by
  a test. Two of them are named here because the tempting implementation gets them wrong:
  - A coalesced entry is **one row, not `count` rows**.
  - `severity` is **not** the state vocabulary. Designed behaviour never produces a warning, so a
    `PAUSED` row is often `info`. Colouring rows by `state` would recreate the board full of false
    faults that §4 of this document spends two paragraphs avoiding.

  There is still no **fleet-wide** event stream. The rings are per object by design, and the fleet
  ring is merged into object reads rather than served by its own endpoint. Building a fleet-wide
  stream in the client would mean reading the events of every path, which costs more than the full
  reconciles described below. The health view names everything that is not active, and every row
  links to an object whose log is one click away. If that stops being enough, the fix is a new server
  endpoint.

### Every read costs a full reconcile

Each user-API GET does one `state.Load`, which is a single `List("")` over the whole store, and then
runs `reconcile.Compute` over the result (`internal/server/userapi.go`). This is deliberate and
useful: what the UI shows and what the fleet is being told to do come from the same function, so
they cannot disagree, and a follower replica shows what the leader is doing. The cost is
that a read is O(fleet), not O(response).

What this means for the UI:

- Poll on a **single timer** that fetches all the endpoints you need together, not one timer per
  component.
- Poll every 2–5 s. The control plane's own heartbeat defaults to 5 s and its settling window is 3
  heartbeats. (`ui/app/` polls every 3 s, `POLL_MS` in `stores/fleet.ts`.)
- Do not poll faster than once a second. The underlying state does not change faster than the
  reconciler runs, and each extra poll is another full store read, multiplied by the number of open
  tabs.

If the UI needs to feel more live than that, the right server-side addition is an ETag keyed on the
store revision, which is a small change. Ask for it rather than compensating with a faster poll.

*An earlier version of this document named `/v1/paths` as the place to add that ETag, here and in
§7a and §11 of this document, but `/v1/paths` is the one endpoint where it would be wrong.* `reconcile.Compute` takes a clock and uses
it for idle teardown and session ages, so two reads at the same store revision can legitimately
return different results. A `304` keyed on the revision would hide exactly the time-driven
transitions the read exists to report. The endpoints where an ETag is sound are `/v1/nodes` and
`/v1/namespaces`, which return pure store state and are two of the workspace's four reads.
`docs/open-items.md` §2.9 has the full argument, including why a quiet fleet has a stable revision
at all.

---

## 3. Creating and cancelling

`POST /v1/namespaces/{ns}/requests` is **create-or-update, keyed on `name` within `{ns}`**. There is
no create-only mode and no 409 for an existing name: a POST with an existing name and a different
spec *updates* that request. If the UI has a "new request" form and an "edit request" form, both make
the same call, and the UI is responsible for stopping "new" from silently overwriting an existing
request. Either fetch first and check, or dry-run first and show the `X-Mxl-Outcome` header, which
reports what happened:

| `X-Mxl-Outcome` | Meaning |
|---|---|
| `created` | did not exist |
| `updated` | existed, spec differed, rewritten |
| `unchanged` | existed with exactly this spec, **nothing was written** |

The status code cannot tell you this: an unchanged apply is still a 200 (verified). The response
body echoes the spec in every case, so comparing it with what you sent only tells you the server
accepted it. Read the header.

`?dry_run=true` runs the same code and skips only the write. **Use it.** It validates against the
real fleet, including conflicts that only show up across requests (two sources into one destination
flow, namespace overlaps, loops), which no client-side check can see. A create form should work like
this: debounce the input, send a dry run, and show the server's reason. Do not reimplement §7.2
validation in the browser. You may reproduce the *structural* rules below for immediate feedback, but
the server decides everything else.

The request body, verified:

```json
{
  "name": "cam1-distribution",
  "sources": [
    {
      "node": "studio-a",
      "domain": { "name": { "area": "media", "elements": ["cameras"] } },
      "select": { "group_hint": { "name": "Studio A:Camera 1" } }
    },
    {
      "node": "studio-b",
      "domain": { "labels": { "role": "cameras" } },
      "select": { "all": true }
    }
  ],
  "destinations": [
    { "node": "edge-01", "domain": { "area": "fast", "elements": ["ingest"] } },
    { "node": "archive-01", "domain": { "area": "bulk", "elements": ["studio-a", "cam1"] },
      "provider": "tcp" }
  ],
  "provider": ["verbs", "tcp"],
  "idle_teardown_ms": 0,
  "sched_prio": null,
  "labels": { "show": "nab" }
}
```

`namespace` is a **real property, not a label**. It may be left out of the body because the URL
carries it. If it is present it must match the URL; a mismatch is refused with
`body names namespace "other" but the URL names "nab"` (verified).

### Both ends are lists

*This is the change that required rewriting §7a.* A request fans in as well as out. `sources` is
always a list, and there is no singular `source:` field. Every stored request or hand-written
manifest that uses the old singular form is now invalid; §9.1 and §16 record that cost and do not
provide a migration.

A source's **node is fixed**; only its domain and its flows are selected. So one source expands over
the domains of one node. A *list* of sources adds what no selector can express: several nodes
feeding one destination. "Every camera in studios A, B and C onto the ingest wall" becomes one
intent, with one name, one lifecycle and one delete.

This has two consequences the UI has to handle. §9.1 addresses both:

- **A request's paths no longer share a producer**, so disagreement among them is normal and the
  aggregate state needed a name for it. That name is `PARTIAL` (§4 of this document).
- **Two sources can collide on one destination flow**, which fan-out alone could never cause. When
  the collision can be decided from the body (two sources pinning the same UUID into a shared
  destination), it is refused at POST as `duplicate_source_flow`. When it cannot (one or both
  sources use a selector, and the collision only appears once a producer does), it is reported as
  `flow_conflict` on the path.

### Three tagged unions

**The flow selector**, `sources[].select`, takes exactly one of `flow`, `group_hint` and `all`.
Setting two is a parse error, and an unknown kind is a parse error in both directions. This is a
deliberate exception to the API's rule of ignoring unknown keys: ignoring an unknown selector kind
would silently *widen* what gets replicated. In the UI this means a radio choice, never two fields
that are both submitted.

- `{"flow": "<uuid>"}` is a bare string, not an object.
- `{"group_hint": {"name": "...", "type": "..."}}` has an optional `type`. Leaving it out selects every
  flow with that name, which is how a camera's video and audio travel together. Make this option
  prominent; it is the selector operators actually want.
- `{"all": true}` selects every flow in the selected domain. **It is not the default and cannot be
  expressed by omission:** a missing `select` is an error on the wire. The manifest may default it;
  the API may not.

**The domain selector**, `sources[].domain`, takes exactly one of `name` and `labels`.

- `{"name": {"area": "media", "elements": ["cameras"]}}` addresses one domain directly. **It is a
  structured `Domain`, not the string `"media/cameras"`.** Sending the string is a decode error
  (verified): `source.domain: json: cannot unmarshal string into Go struct field plain.name of type
  api.Domain`. The rendered string is the manifest's spelling, and the CLI is the only thing that
  parses it. *(An earlier §9.1 example body showed the string form; it has been corrected, and the
  wire never accepted it. Trust the type.)*
- `{"labels": {"role": "cameras"}}` matches every domain on that node that has all of these keys
  with exactly these values. Matching is by equality, the keys are ANDed, and the map may **never be
  empty**. An empty map is refused, because otherwise "every domain on the node" could be selected
  by accident through omission rather than on purpose.

  A label selector is how one row of the matrix can cover several source domains. It is also where a
  broad selector meets flows that this project is itself writing. Those flows are **excluded, not
  replicated**, and the request reports which ones (see `status.excluded[]` in §4 of this document).

*The `{"path": …}` direct form was removed together with path-named domains.* One naming scheme now
covers both the domains a scan found and the domains replication created. That is what makes a
chain `A→B→C` possible to write: the second hop names the domain the first hop created.

**The destination domain**, `destinations[].domain`, is written as
`{"area": "fast", "elements": ["studio-a", "cam1"]}`. It creates the directory `<fast>/studio-a/cam1`
and renders as `fast/studio-a/cam1`. **This is the most important invariant in the design** (§13): a
destination is always a name inside an area the operator granted `write` on, never a path sent
through the API. That is what stops this API from being usable to write arbitrary directories on
every node in the fleet, and it holds whatever authentication is configured.

The rule behind it is that **nothing outside the CLI's manifest parser turns a domain string into an
area and elements**. A UI text box that accepts `fast/studio-a/cam1` and splits it makes the UI a
second parser, which the invariant forbids. Two input designs are acceptable:

- **An area picker plus element chips.** This matches what the field actually is, and it shows the
  grants: the picker lists only areas the node grants `write` on.
- **A single field split on `/`**, provided that the first segment is checked against the areas the
  node advertises with `write`, the rest is checked against `ValidDomainElements`, and every
  submission is dry-run first. If you choose this, mirror `internal/api/domain.go` and write a
  comment saying that you know the UI is acting as a second parser here and why that is acceptable.

The naming rules:

- Each element uses ASCII letters, digits, `-`, `_` and `.`; does not start with `.` or `-`; is not
  `.` or `..`; and is at most 64 bytes.
- At most 8 elements.
- The whole rendered `<area>/<elements>` is at most 255 bytes.
- The area name follows the same character rule as an element.

**A node that advertises no writable area cannot be a destination.** Filter it out or disable it,
and say why, because this is the first thing to check when a request is refused. There are two
distinct codes, and each message names what to fix:

```
{"code":"invalid_request",
 "message":"node \"studio-b\" advertises area \"media\" but does not grant writing on it",
 "details":{"reason_code":"area_not_writable"}}

{"code":"invalid_request",
 "message":"node \"edge-01\" advertises no area \"nope\", it has \"bulk\" (writable), \"fast\" (writable), \"media\" (read-only)",
 "details":{"reason_code":"unknown_area"}}
```

*This supersedes `no_output_root`, `unknown_output_root` and `ambiguous_output_root`, and the
`root:` field that could be omitted when a node advertised exactly one root.* There is nothing to
omit now: the area is the first segment of the domain's name, so leaving it out means leaving out
half the name. **The root picker is gone from the create form.** The area picker is not a
replacement for it; choosing the area is part of naming the domain, and the UI should present it
that way. `area.path` is still advertised. It is for diagnostics only and may be absent, so guard it.
When present, the form can show `/dev/shm/mxl/studio-a/cam1` under the name while the operator
types, which is the most useful hint available for an otherwise abstract name.

One collision is still checked, and it is narrower than before: `domain_name_in_use` now means that
the destination domain **nests** with a domain another path is already creating on that node, for
example `fast/studio-a` against `fast/studio-a/cam1`. Two domains with the same elements under
different areas cannot collide, because `fast/ingest` and `bulk/ingest` are different names. *If you
read the old version of this document: its warning that "a root is not a namespace" no longer
applies, and the new rule is the intuitive one.*

### Provider

`"provider"` is a string, an array, or absent, and it is returned in the form it was written:

```
"verbs"            pinned: verbs or the request fails
["verbs", "tcp"]   prefer verbs, tcp acceptable
absent             the server's order, default EFA > Verbs > TCP > SHM
```

**A pinned provider is used or the request fails; another provider is never substituted** (§10.4).
Do not build a UI control that reads as "fall back automatically". Silently ending up on tcp when
verbs was asked for is a large performance drop, and its symptom looks like a problem with the
source. The per-destination `provider` **replaces** the request-level setting for that destination
rather than being intersected with it, because "verbs here, tcp there" is an ordinary request, not a
conflict. There is deliberately no equivalent on the source side: whether a provider is usable is
decided per (source, destination) pairing, and one override on one side already says everything a
pin needs to say about a pairing.

### Request labels are identity, not annotation

Request labels are copied into worker metrics, and together with the namespace they scope
`apply --prune`. The server validates them when the request is written:

- The key is a valid Prometheus label name: letters, digits and underscore, not starting with a
  digit or `__`.
- The key is at most 63 bytes and the value at most 253.
- The key is not one of the names the project reserves for itself.

Removing a label from a request changes which `--prune` runs can delete it, so a UI that offers
label editing should say so.

*This section used to list the reserved names as `direction`, `domain`, `domain_name`, `flow_id`,
`session`, `namespace`, `quantile`, and that is not what the server enforces.* `validateLabels`
reserves `metrics.WorkerLabelNames()` plus `quantile`, which is **`direction`, `domain`, `flow_id`,
`session`, `namespace`, `format`, `media_type`, `quantile`**. That is two more names than the old
list, and it does not include `domain_name`. So a request label called `domain_name` is accepted
today, even though §10.7 says the `name` domain label is exported as a metric dimension of that name.
This was found on 2026-09-01 while mirroring the rule in the UI. Mirror the **code**. The
discrepancy is for the server to resolve, not for a client to hide. The same applies to domain
labels, because `validateDomainLabels` uses the same function.

**No request label key has special meaning to the server.** `namespace` briefly did and is now a real
property, so request labels are plain tags. Do not confuse them with *domain* labels, which are a
different record on a different endpoint with a different owner (§10.7). Request labels annotate
intent; domain labels annotate a place and are what a source selector matches.

### Delete

`DELETE /v1/namespaces/{ns}/requests/{name}` removes the intent. It does **not** necessarily stop
media, because a path keeps running while another request still references it. A confirmation dialog
should say what will actually happen, and the UI can compute it: for each path the request holds,
check whether `path.requests.length > 1`. If it is, that leg keeps running.

Deleting a request that does not exist returns `404` (verified). The CLI treats that as success,
because deleting what a manifest names should be idempotent. A UI acting on a row the user can see
should probably treat it as "already gone, refresh" rather than showing an error dialog.

### Parking, which is not deleting

`disabled` on a destination entry (§9.1) is how the model says *off*. The server implements it, and
the request state `DISABLED` and reason code `all_destinations_disabled` come with it:

```json
{ "node": "edge-02", "domain": {"area": "fast", "elements": ["ingest"]}, "disabled": true }
```

The effective spec is every source against every **enabled** destination. A parked entry produces no
pairing, no path and no session. When no destination is left enabled, the request reports `DISABLED`
(§4 of this document). Four things for the UI to get right; all four make parking simpler than the
delete it replaces:

- **It is a spec edit, so it uses the same POST as everything else.** Take the stored spec, flip one
  boolean, dry-run it, and apply it. There is no new endpoint, verb or failure mode.
- **It stops media.** Parking cancels those legs while keeping their text, so it needs the same
  cancellation preview as a delete: for each path, is `path.requests.length > 1`? Do not let the
  fact that the flag can be flipped back make the click feel reversible. The flag comes back; the
  session does not.
- **It is not a soft delete.** The request still exists, still holds its name in the namespace, and
  is still pruned by a manifest that stops naming it. `DELETE` is unchanged.
- **An `apply` from a manifest that does not set the flag turns the leg back on** (§9.1). If the UI
  parks a leg and someone's CI then applies the file that defines that request, the leg comes back.
  That is correct, because the file is authoritative, but it is the most surprising thing about this
  feature, so say it in the interface and not only here.

Parking exists because of §7a. Without it, the matrix axes are derived from the routes that are
currently on, so switching a route off would delete its row and column, and the operator's board
would rearrange itself.

### Labelling a domain is the second mutation

`POST /v1/nodes/{node}/domains` writes the labels on one `(node, domain)`. It accepts `?dry_run=true`
for the same reason requests do: adding or removing a label adds or removes a domain from a request's
expansion, so it starts and stops media with one level of indirection. That indirection makes it
*easier* to do by accident, not harder.

There are two body shapes. They differ in which keys the write owns, not just in convenience:

```json
{"node": "studio-a", "domain": {"area":"media","elements":["cameras"]},
 "apply": {"role": "cameras", "name": "cameras"}}

{"node": "studio-a", "domain": {"area":"media","elements":["cameras"]},
 "patch": {"set": {"name": "cameras"}, "remove": ["role"]}}
```

- An **apply** owns the keys it declares. It sets them, removes any key it declared last time and no
  longer declares, and leaves every other key alone. This is `kubectl apply`'s three-way merge; the
  declared set is stored on the record as `declared`.
- A **patch** sets and removes exactly the keys it names and does not compare against anything.

A UI editing one domain's labels interactively should use `patch`. The UI has no declared set of its
own. A read-modify-write with `apply` would silently take ownership of keys that someone else's
manifest declared, and a later write could then delete them.

The response is the resulting record plus its blast radius, and the UI should read the blast radius
from the dry run. Verified, removing `role` from a labelled domain:

```json
{"node": "studio-b", "domain": {"area": "media", "elements": ["cameras"]},
 "labels": {"name": "cameras"}, "declared": ["name", "role"],
 "stopped": [ { "id": "b2adff89…", "source": {…}, "destination": {…},
                "state": "ESTABLISHING", "requests": ["nab/wall"], "session": {…} } ]}
```

`stopped[]` and `started[]` contain **full `Path` objects**, so the UI can say which requests lose
which legs, and whether another request still holds them, without a second read.

---

## 4. Status: what to render and what it means

There are nine states (§11). Seven describe one path. The other two apply only to requests:
`PARTIAL` describes disagreement among a request's paths, and `DISABLED` describes the request's
spec rather than anything in the fleet. The first seven rows below are worst-first, which is the
order the CLI sorts by and the order the UI should use. (In the aggregate fold, `PARTIAL` takes
precedence over the failure states; see below.)

| State | Meaning | Is it a problem? |
|---|---|---|
| `INVALID` | Needs user action. Never resolves by itself. | Yes — and it is the only one a user can fix. |
| `FAILED` | Repeated permanent-looking failure, or a fabric that stopped being viable. Still retried. | Yes. |
| `DEGRADED` | Established but flapping — restarts over a threshold in a window. | Yes. |
| `WAITING` | The flow is not visible, or an agent is not leased. No workers. Resolves by itself. | Usually not. |
| `ESTABLISHING` | Coming up: session created → target assigned → epoch reported → initiator connecting. | No. |
| `PAUSED` | Nothing is being produced at the source, whether or not workers are running. | **No.** |
| `ACTIVE` | Media is flowing — the destination flow's head index is advancing. | No. |
| `PARTIAL` | **Aggregates only.** Some of what this request asked for is working and some is not. | Depends — read the breakdown. |
| `DISABLED` | **Aggregates only.** Every destination is parked, so the request is asking for nothing. | No — somebody switched it off on purpose. |

The rest of this section covers what UIs routinely get wrong about this table.

**`PARTIAL` never appears on a path, a session or a worker.** It is an aggregate-only state, and the
code makes that explicit: `api.States()` returns the seven states a path can be in, and
`api.RequestStates()` returns those plus `PARTIAL` and `DISABLED`. A renderer must be able to show
`PARTIAL` on a request and must never expect it on anything below one. This is the rule that produces
it; the UI should copy it exactly so that its own aggregates agree with the server's:

> If the paths disagree **and at least one is `ACTIVE`**, the aggregate is `PARTIAL`. Otherwise it
> is worst-first over the set. A set with no `ACTIVE` path is never `PARTIAL` — `PARTIAL` claims
> something is working, and it must not be said when nothing is.

One consequence is deliberate but surprising: **`PARTIAL` takes precedence over `INVALID`, `FAILED`
and `DEGRADED`**. Suppose a request's selector expands onto twenty paths and one of them conflicts.
The request reports `PARTIAL`, with nineteen good paths and one bad one, because its top-line state
answers "is this request doing its job?". The failing detail belongs in the counts, the per-source
breakdown and the path list, not in the line an operator reads first. Verified on a live server:

```
nab/wall -> PARTIAL {'ACTIVE': 2, 'ESTABLISHING': 1} | 2 of 3 paths active; ESTABLISHING: target worker is starting
   source studio-a ACTIVE {'ACTIVE': 2}
   source studio-b ESTABLISHING {'ESTABLISHING': 1} target worker is starting
```

**When a request has several sources, lead with `status.sources[]`.** Each entry has the source, its
own state, its own counts and the IDs of its paths. "Studio B is dark, studio A is fine" is what an
operator needs to know about a fan-in request, and the per-path list alone does not say it; the
distinction did not exist when a request had only one source.

*This section used to say the field was optional and absent when there was nothing to report.*
Against a server built from this tree it is **always present and always the full list**, including
for single-source requests (verified 2026-09-01; the doc comment on `RequestStatus.Sources` says the
same). Keeping a fallback that attributes paths without it costs one line and is worth it for older
servers. But a renderer that treats a missing `sources[]` as the normal single-source case is coding
against behaviour that does not exist. The same correction appears in §7c, where the old claim
mattered.

**`status.excluded[]` lists flows the expansion deliberately skipped.** A path that does not exist
has no status that could carry a reason, so a flow a selector passed over would otherwise be
invisible in a paths-only view. There is one reason today, `self_output`: a flow that this node's own
target worker is writing. It appears where a broad label selector meets a node that is also a
replication destination. Verified:

```json
"excluded": [{"node":"edge-01","domain":"fast/ingest",
              "flow":"5592a23b-0974-45bb-9388-89ea81c42537","reason":"self_output"}]
```

"Did not match the labels" is never listed, because that set is unbounded and is the normal case.
The list is capped. When it is truncated, `excluded_dropped` reports how many entries were dropped.
Show that number when it is non-zero; otherwise a silent cap reads as "nothing else was excluded".

**`PAUSED` is not an error; it is the most useful state in the vocabulary.** It separates the two
questions an operator has at 3am: *is the plumbing broken*, or *is the source not producing?* A "no
media at the destination" alarm looks the same for both, and they have different owners. Show
`PAUSED` as its own state, visually distinct from both green and red. A UI that puts it in a red
"not working" group has thrown away the one signal this design added. `PAUSED` means the same thing
whether the workers are running and idle or were torn down after being idle too long (§11.1), so
"PAUSED with no session" is not a contradiction.

**`INVALID` does not stop running media.** An invalid request starts no *new* sessions, but any
session already carrying media keeps its assignments. A request can be `INVALID` while video is
flowing, and §7b of this document describes a case where that is routine. Do not show `INVALID` as
"stopped"; show it as "will not establish anything further, for this reason".

**`DISABLED` is also aggregate-only, and it is *derived*.** There is no `disabled` field on a request
to read. The state is computed from the destination entries, and a request reports it exactly when
none of them is enabled. A path is never `DISABLED`, for the same reason a path is never `PARTIAL`: a
parked destination produces no path for the state to describe. A request with one enabled destination
and one parked one is **not** `DISABLED`; its state is folded over the paths it still has, exactly
as if the parked entry did not exist.

Show `DISABLED` as *off*, never as *broken*:

- It sorts after `ACTIVE`, not before `INVALID`.
- It does not belong in the landing page's list of problems.
- It must remain countable. A namespace with fifteen parked legs is something an operator should see
  without searching, because this feature makes it possible to have a leg switched off for a reason
  nobody remembers.

**Registered is not the same as live.** `node.live` is the lease. A node that is registered but has
no lease has lost its agent. An expired lease does *not* prove that the node's workers stopped
(§4.2), which is why the server freezes that node's assignments instead of reassigning them. Show it
as information ("no agent currently holds this node's identity"), not as "node down, its flows are
gone".

**`ESTABLISHING` is deliberately not split into sub-states.** The steps are useful in a reason string
and in logs, but an operator does not act differently on any of them. Do not build a four-stage
progress bar; show the state and the `reason`, which already names the current step. One reason to
recognise: since §6.3, the agent paces worker starts with a token bucket, so on a node that is
re-establishing many sessions at once, a worker can sit in `starting` for a minute and its reason
says so. That is rate limiting working, not a fault.

### Reasons are machine-readable

Every non-`ACTIVE` state carries two fields:

- `reason`: prose, which may change at any time.
- `reason_code`: a stable code. Switch on this one.

The full list is in `internal/api/wire.go`; read it once. The three negotiation
failures, `no_shared_fabric`, `no_shared_provider` and `no_shared_capability`, are three *different
operator problems*, and the codes exist so a UI can tell them apart without matching English text.
A sensible treatment: display the prose, and use the code to choose an icon, a severity, and a link
to the thing that needs fixing.

Codes that are new or renamed since the areas and fan-in work, all verified: `unknown_area`,
`area_not_writable`, `malformed_domain_name`, `same_endpoint`, `duplicate_source_flow`,
`namespace_overlap`. `domain_name_in_use` still exists with a narrower meaning (nesting only). Parking
added `all_destinations_disabled`, which accompanies `DISABLED`. `domain_not_mapped` is no longer
emitted, and its constant has since been removed from `internal/api/wire.go`, so only an old server
can send it. `source_idle` accompanies `PAUSED`, not `WAITING`.

### `settling` and `not_ready`

Two reads may return `{"settling": true, ...}`:

- `GET /v1/paths`.
- `GET /v1/nodes/{node}/domains`. This one joins labels against inventory, so during the settling
  window it would otherwise show every label with no observed domain beside it, which looks
  like the labels having been lost.

`settling` means the server has not yet run its first reconcile, because it has just started or an
HA leader has just changed. The server reports this **explicitly instead of reporting everything as
`WAITING`**, so that a restart does not look like a fleet-wide outage. The field is `omitempty`, so
it is absent when false.

The UI must show a banner while `settling` is set, and must not present the state underneath as if
it were steady; it is correct, but nothing has acted on it yet. `GET /readyz` returns `503` with
`{"code": "not_ready"}` in the same condition, and `{"status":"ok","leader":"<replica>"}` otherwise.
It is reasonable to poll it alongside the other reads, and the leader name is the only place the API
shows which replica is reconciling.

A `503` with `code: internal` means the store is unreachable: the server itself is fine, but its
store is not. Show the two differently in whatever error display you build, because they send an
operator to different places.

---

## 5. Traps

Each of these is real and was confirmed against the running server, and each one produces a UI that
looks correct but is not.

1. **A domain is structured when you send it and rendered when you read it, and both forms appear in
   one response.** `path.destination.domain` is `{"area":"fast","elements":["ingest"]}`;
   `path.source.domain` is the string `"media/cameras"`; `flow.domain` is a string;
   `domainInfo.domain` is an object. This asymmetry is intentional (§10.6) and is not a bug to
   normalise away: the structured form is what may be *sent*, and the rendered form identifies
   something that already exists. Keep the object whenever you will send it back, and join with `/`
   for display. Never split a rendered string back into parts to send it.

2. **`max_message_size` is a real `uint64`, and providers report `UINT64_MAX`.** The wire carries
   `18446744073709551615`; `JSON.parse` turns it into `18446744073709552000`. Parse this field with
   BigInt support, or read it from the raw text, or (simplest) display `UINT64_MAX` as "unlimited"
   and everything else with a units formatter, after checking that the value was not rounded. It
   appears in `node.capabilities.fabrics[].max_message_size` and in
   `session.interface.max_message_size`.

3. **`node.last_seen` is when the lease was *acquired*, not the last heartbeat.** A heartbeat renews
   the lease and deliberately writes nothing. Writing on every heartbeat would advance the store
   revision several times a minute per node, forever, and wake every agent's long poll; a spurious
   wakeup there costs a worker restart. So a healthy node can show a `last_seen` of an hour ago.
   **Never display it as staleness or drive a health indicator from it.** Liveness is `live` and
   nothing else. If you show the field at all, label it "lease acquired".

4. **Flow IDs are not unique to a location.** `GET /v1/flows` returns one entry per
   `(node, domain, flow)`. After replication the same UUID appears on both nodes. That is success,
   not duplication, and the destination copy has `replicated: true`, which is how you tell them
   apart. Key rows on the triple. A flow detail view should list every place the flow exists; that
   list is the answer the view is for.

5. **`replicated` explains why a selector skipped a flow.** It is true exactly while one of that
   node's own target workers is writing the flow, and it is what stops a label selector from
   matching this project's own output. It is briefly absent during an agent restart or a long-idle
   teardown. That is safe, because a flow whose target worker is not running is not advancing
   either, but it means the flag is a live fact, not a stored one. Display it; otherwise a selector
   that silently skips a flow cannot be diagnosed.

6. **`status.counts` omits zeros.** A request with one establishing path returns
   `{"ESTABLISHING": 1}` and nothing else. Render the full vocabulary in a fixed order with missing
   entries shown as 0, or a chart will show a gap where it should show a zero. Use the nine states of
   `RequestStates()` for a request and the seven of `States()` for anything below it. The request
   list grew from eight to nine when `DISABLED` was added, so treat the vocabulary as a list to
   iterate over, not a fixed set of columns.

7. **Timestamps use `omitzero`.** `registered_at`, `last_seen` and `started_at` may be *absent*, not
   null and not the epoch. Guard every one.

8. **`request.name` is unique within its namespace, not across the fleet.** The identity is
   `(namespace, name)`, and `request.id` is the string `"nab/wall"`. A UI that keys rows or a map on
   `name` alone will silently merge two requests as soon as a second namespace uses the same name.
   "Route the new camera like the last one", across two shows, is how that happens. Key on
   the pair; `path.requests[]` carries the joined form.

9. **IDs are 32 hex characters.** Truncating them for display is fine, but matching must be
   **exact**. If the UI shows a shortened ID anywhere, make sure whatever it links to carries the
   full one.

10. **`flow_def` is a verbatim `json.RawMessage`.** It is arbitrary NMOS content, including fields
    nothing in this tree models. Display it or pretty-print it, but never decode and re-encode it
    into anything that is sent back over the wire. The session identity hashes those bytes (§5.4),
    so a re-serialisation that reordered keys would look like a different flow and cause a healthy
    session to be rebuilt.

11. **Empty arrays are `[]`, not `null`**, on `requests`, `nodes`, `flows`, `paths`, `domains` and
    `namespaces`. The server normalises this deliberately, because on one endpoint (agent
    assignments) confusing absence with emptiness would stop every worker in the fleet. Add a
    defensive `?? []` anyway, but do not *rely* on needing it.

12. **`session` is absent on a path in `WAITING`**, and `session.epoch`, `session.target` and
    `session.initiator` are each absent until the agent reports them. A session in `ESTABLISHING`
    can legitimately have a fabric and an interface config and no endpoints at all (verified).

13. **The user API never discloses `target_info`.** `session.target` has `address`, `service`,
    `state`, `restarts` and `started_at`, but not the blob, which contains RDMA rkeys and is only
    available on the agent API. Keep it that way: do not look for a way to expose it.

14. **A request can report a path it does not hold.** This is the most dangerous trap here, and it
    came with namespace exclusivity. When two requests in an exclusive namespace overlap, the loser
    goes `INVALID` with `namespace_overlap`, but it still lists the contested path *with the
    winner's state*. So a request that carries nothing can show `{"ACTIVE": 1}` in its own counts.
    `/v1/paths` is the authority on ownership: `path.requests[]` names only the winner (verified
    both ways). Anything the UI computes from a request's own `status.paths[]`, such as a cell state
    or a "what stops if I delete this" preview, must check ownership against `/v1/paths`, or it will
    report another request's media as this one's.

15. **`disabled` is absent when false, so a reused decode target keeps a stale `true`.** The field is
    `omitempty`, because its zero value is the one that keeps media running (§9.1), so a re-enabled
    destination comes back with no `disabled` key at all. Code that decodes a poll response *into*
    the previous response instead of replacing it keeps the old `true` and shows the leg as parked
    forever after it was re-enabled. This broke a Go test in this repository, because
    `json.Unmarshal` reuses a slice's existing elements. JavaScript is safer only because
    `JSON.parse` always allocates new objects; the same bug moves to whatever code merges the parsed
    object into your state. Replace, never merge. The same applies to every other `omitempty`
    boolean the API adds.

16. **The default namespace is `shared`, not `exclusive`.** `default` is created automatically on
    first reference, with the permissive mode (verified). Everything §7a assumes about a cell
    meaning one thing depends on `exclusive`, so this is a requirement, not a preference; see §7b.

---

## 6. Where the UI runs

**Settled: the UI is always same-origin with the API.** Either the server process serves it
directly, or it is served next to the API behind a proxy that fronts both. **No CORS**, now or
later.

This has three consequences, and they are constraints on the code:

**Every API call uses a relative URL**, such as `/v1/paths`, never a configured API base. Both
deployment options put the UI and the API on one origin. A base-URL setting is what would lead
someone to add CORS six months later, so there is deliberately nothing to configure here.

**Development uses a dev-server proxy, not CORS.** A dev server on `:5173` calling the API on another
port is cross-origin and *will* fail: the server returns 405 to the preflight and sends no
`Access-Control-*` headers on anything. Proxy `/v1`, `/healthz`, `/readyz` and `/metrics` to the API
(Vite's `server.proxy`, as `ui/app/vite.config.ts` does, or the equivalent in another stack), so that
development and production use the same relative URLs. Without a framework, the equivalent is an
unprivileged nginx, under its own prefix, serving the page and proxying the user API and nothing
else. Do **not** work around this by adding CORS middleware to the server
for development; that is how the deployment decision above stops being true.

**Serving from the server binary.** When this document was first written there was no static-asset
route (no `go:embed` and no `http.FileServer` anywhere in the tree), and adding one was described
here as a small server-side change for the UI implementer to make in `internal/server/http.go`: embed
the built assets, serve them outside both API prefixes, and fall back to the index for client-side
routes. That change has since been made. `ui/embed.go` embeds `ui/app/dist`, `internal/server/static.go`
serves it at `/`, and the `--server-ui` flag enables it; a binary built without `make ui` refuses the
flag at startup. It follows the two rules this section set:

- Mount it *outside* `s.authenticate`. The assets are not what the bearer token protects, and a page
  that asks for a credential cannot be behind that credential.
- Keep it clear of `/v1`, `/agent/v1`, `/healthz`, `/readyz` and `/metrics`. (`static.go` does this by
  registering bare `/`, which the mux tries after every more specific pattern.)

### The token: decided, and why the browser only holds it as a fallback

**Settled: the browser may hold the token, and asks for it only after a 401.** The reasoning that
argued against this is kept in full below, because all of it is still true. The implementation is
designed around that reasoning rather than overriding it.

The two options the original reasoning offered were not the only cases. A common deployment has a
token configured and no proxy that injects it, because the token is a server flag and the proxy is
someone else's infrastructure. That deployment could not use the UI at all, and making the web
interface unusable does not improve security; it just encourages running the fleet with auth off. So
there is a third case, and the implementation handles all three:

- **No token configured.** Nothing changes. No prompt is ever shown and nothing is stored.
- **A proxy or the server injects `Authorization`.** This is still the recommended setup, and the
  fallback does not affect it: the browser only sees 200s, so it never asks and never stores
  anything.
- **A token is configured and nothing injects it.** `components/TokenGate.vue` asks for it, and
  `api/auth.ts` stores it in `localStorage` and attaches it to every call.

What keeps these three apart is that **the prompt is triggered by a refusal, not by the absence of a
token** (`api/auth.ts`). A 401 on a `/v1` read raises the prompt, a 2xx on a `/v1` read clears it,
and nothing else changes it. The `/v1` condition matters: `/readyz` is outside the auth middleware and
is polled *at the same time* as the reads. If any success cleared the prompt, the `/readyz` 200 would
undo the `/v1` 401 next to it twice per poll, and the gate would flicker.

The prompt itself is just a field and a button. The cost is real: the token also opens `/agent/v1`,
and a browser profile is a worse place to keep it than a proxy config. But that explanation belongs
here and in the code, not on the screen. An operator who has just been refused has one thing to do
and is not deciding whether to do it, and a paragraph of architecture above the field would be read
once and then ignored. The one control that follows from the cost is a `token · forget` button in the
header (`App.vue`), shown only when a token is actually stored, so that someone on a shared
workstation can remove it without clearing site data.

### The reasoning that shaped it (kept)

Same-origin removes the need for CORS, but it does not solve authentication, and authentication
matters more here than in most systems:

- Auth is **one optional shared bearer token**, checked in middleware on both prefixes
  (`internal/server/auth.go`). There are no sessions, cookies, per-user identities or mTLS. Running
  with no auth is supported for a trusted network and for development.
- **The same token protects the agent API.** Anything holding it can claim to be any node, inject fake
  inventory, and read every node's RDMA rkeys (§13).
- The user API can itself be abused to consume resources: a replication request moves uncompressed
  video between hosts, so unauthenticated access lets anyone exhaust bandwidth across the fleet.

So a token pasted into a JS constant, or typed into a field and kept in `localStorage`, gives
whoever loads the page the privileged agent API as well. On a trusted network running without a
token, that is no worse than the deployment already is. Where a token *is* configured, the
same-origin decision makes the good answer easy, and it should be chosen deliberately: **the proxy
in front (or the server itself, when it serves the UI directly) injects the `Authorization` header
on the way through.** The browser never holds the token, and the UI's own code has no auth logic at
all. This fits the deployment shape already chosen and needs nothing from the frontend.

Confirm which of the two applies (a token-injecting proxy, or no token because the network is
trusted) before building anything that assumes one of them. What to avoid by default is the third
option nobody chose: the browser holding the fleet-wide credential.

*(Only that last paragraph was superseded. The implementation does avoid the third option by
default: the browser holds nothing until the server has refused a request, and in both
configurations this section recommends, that never happens.)*

---

## 7. What to show

The CLI has three read verbs, and each has a separate job. The same split works for a UI. Do not build
a fourth view that repeats one of them.

| Verb | Job | UI equivalent |
|---|---|---|
| `status` | Counts the fleet, then names **only what is not active**. Not a list. | The landing page. |
| `get <kind>` | Lists, so a name can be found. | The tables. |
| `describe <kind> <name>` | Everything known about one thing. | The detail pages. |

**Landing page.** It shows counts by state for requests and paths, nodes registered versus leased,
and sessions running. Below the counts is a short list of *only* the things that are not `ACTIVE`,
worst-first, each with its reason. The CLI answers "is anything wrong" in two lines rather than a
screen to scan, and the UI should do the same. The landing page shows two things that no per-request
view can show, and both should stay:

- nodes that are registered but not leased;
- nodes that advertise no writable area.

**Detail pages**, one for each noun of §3:

- **Node**: what the agent advertises (**areas with their name, path and two grants**, fabric
  attachments with their caps, versions, sched_prio, port range), the domains it is currently
  observing, and every path that touches it, **with this node's role in each**. A node can be *both*
  ends of one path (same node, different domain). The loopback configuration does this, and so does
  `edge-01` in the fixture of §9 of this document. A live run caught the bug this causes; a unit test would not have.
  Check both ends independently; do not `switch` on source-then-destination.
- **Domain**: `<node>:<area>/<elements>`. Its labels, whether the node currently reports it, and for
  each flow whether **this node is the one writing it**. Labelling belongs on this page.
- **Flow**: every location where the ID exists, whether each copy is `producing`, whether each is
  `replicated`, and which paths carry it.
- **Request**: the stored spec, its sources and destinations, the per-source breakdown, the per-path
  breakdown and the exclusions. This is where "2 of 3 active" and "studio-b is dark" are shown.
- **Path**: the edge, its state and reason, its **refcount** (`requests[]`), and a link to the
  session.
- **Session**: the negotiated fabric and interface config, the epoch, and for each end its state,
  bound endpoint, restart count and uptime.

**What a UI can add that the CLI cannot**, roughly in order of value:

1. **The routing matrix** (§7a). It shows the desired set and its live state on one screen, and the
   operator can edit it in place. This is the main reason to build a UI, and it is the screen the
   operator will use most.
2. **Live updating**: a poll and a diff. Each CLI invocation is one snapshot.
3. **A topology view**: nodes as vertices, paths as edges, coloured by state. It needs only
   `GET /v1/paths`, and it shows what an operator cannot put together in a terminal: chains
   (`A→B→C`) and fan-in are obvious in a graph and invisible in a table. It is a read view, not an
   editor (§7a).
   *Built: `ui/app/src/views/Topology.vue`. It costs **zero extra reads, not
   one**: paths and nodes are both already on the single poll, so it is the only screen in the app
   that adds a view without adding load. Building it settled two things this item did not say:*
   - *It must be **fleet-wide**. A chain can cross namespaces, and a graph scoped to one namespace
     would cut it in half. The current namespace is a highlight over the graph, not a filter on it.*
   - *The **layout** matters more than the drawing. A graph that is re-laid-out on every 3 s poll is
     unreadable whatever it shows, so the layers and the order within each layer are deterministic
     functions of the sorted fleet, with the node name as the only tie-break.*
4. **Create-with-discovery.** With the CLI you must know the node, domain and flow before you can
   write the manifest. The matrix's unrouted-sources strip lets the operator see what exists and
   route it from there.
5. **Blast radius before a mutation.** For a cancellation it is computed from `path.requests[]`; for
   a label write it is read directly from `stopped[]` / `started[]`.

The `describe` nouns stay as the detail views behind the matrix: a cell links to its paths, a path to
its session, a column header to its node. Keep `path` and `session` separate there, even though a
matrix cell aggregates over both.

---

## 7a. The workspace: a routing matrix instead of a form

Assume an operator spends most of the day on one screen. A parameter form with a submit button does
not suit that. It is a page the operator *visits* to perform one transaction, and it shows one request
at a time, while the operator's actual question is "what is routed where, and what is broken".

**Use a routing matrix**: sources down the side, destinations across the top, and each cell is a
connection. Broadcast operators already know crosspoint matrices well, so they need little new
learning. This project's own `xpt` CLI proposal used the same vocabulary. It was dropped because it
was a second *command-line* dialect competing with the manifest, not because the mental model was
wrong. A matrix is not a second vocabulary: it renders the desired set, which is what the manifest
describes too.

```
                        edge-01        edge-01         archive-01
                        fast/ingest    fast/arch/cam1  bulk/capture
                        + archive                                     + destination
   ┌──────────────────┬──────────────┬───────────────┬──────────────┐
   │ studio-a         │              │               │              │
   │  media/cameras   │    ACTIVE    │    ACTIVE     │              │
   │  ⌗ Camera 1  2fl │      2       │       2       │      ·       │
   ├──────────────────┼──────────────┼───────────────┼──────────────┤
   │ studio-b         │              │               │              │
   │  {role: cameras} │ ESTABLISHING │               │    ACTIVE    │
   │  ⌗ all       1fl │      1       │       ·       │      1       │
   ├──────────────────┼──────────────┼───────────────┼──────────────┤
   │ studio-c         │              │               │              │
   │  media/cameras   │   WAITING    │               │              │
   │  ⌗ Camera 3  0fl │  no flow yet │       ·       │      ·       │
   └──────────────────┴──────────────┴───────────────┴──────────────┘
   + source          └──────── cam1-distribution · PARTIAL ────────┘

   UNROUTED   studio-a media/cameras 8b3f… audio · edge-01 media/local 6d3f… video · …
```

### Rows and columns are selectors; only cells hold real paths

The areas and fan-in work forced this way of reading the matrix, and every rule below follows from
it.

A row is not a domain, and a column is not a directory.

- A row is a **source**: a node, a domain **selector** and a flow **selector**. A
  `{labels: {role: cameras}}` row matches domains that may not exist yet, and a
  `{group_hint: {name: "Camera 3"}}` row matches flows that no producer has published yet. Neither is
  a handle on an existing object.
- A column is a **destination**: `(node, area, elements)`. A domain that a request materialises
  **does not exist until a request names it**, so there is no pre-existing list of destination
  domains to put on the axis either.

So the operator writes both axes, and the server turns them into paths at reconcile. The cells are the
only place where real objects appear: the paths that this (source, destination) pairing expanded to,
each with a real ID, a real session and a real state.

Three rules follow. The earlier model, in which a row was a request, had to argue for each of them
separately:

- **A cell with no paths is not an error.** It means the selectors have not matched anything yet,
  which is the normal state of a route set up in advance. This is the middle row of the
  three-outcomes table below, and it is no longer a special case once the axes are understood as
  queries.
- **This is why a matrix works better than a node graph.** A graph needs concrete endpoints to
  connect, but a selector is a query whose matches change over time. The matrix never connects
  anything: it writes two queries and asks the server what they match.
- **The count in a cell is required, not decoration.** It is the only place the operator sees how
  many paths a query produced. A row showing `⌗ Camera 1 · 2fl` and a cell showing `2` are the same
  fact seen from the two sides.

### A request is a rectangle of cells

*This supersedes "a request **is** a row", which the whole of §7a used to be built on.* That model
held while destinations were the only list. Now that `sources` is a list too, a request is
**sources × destinations**: a block of cells in the grid. The UI draws it as a rectangle over those
cells, labelled with the request's name and aggregate state.

A single-source request is a 1×N rectangle, which is the old picture unchanged, so the common case
gets no more complicated. What the change adds is that the grid can show the case fan-in exists for —
"every camera in studios A, B and C onto the ingest wall", with one name, one lifecycle and one
delete — instead of three requests that the operator has to keep in step by hand.

| Matrix | Model |
|---|---|
| Row | one source: `node` + `domain` selector + `select` |
| Column | one destination: `(node, area, elements)` |
| Lit cell | that `(source, destination)` pairing is in some request |
| Cell contents | one state word and a path count — **nothing of variable length** |
| Rectangle | one request: all of its sources against all of its destinations |
| New row | add a source — to a new request, or to an existing one |
| New column | name a destination |

The change has five consequences, and they are its whole cost. Other sections cite them by number.

**1. The cells of one request cannot be toggled independently.** Suppose a request has sources
{`studio-a`, `studio-b`}. Lighting `studio-a → edge-02` also lights `studio-b → edge-02`, because a
rectangle has no notches. The UI must show this *before* it commits. The dry run provides it at no
extra cost, since the response carries every path the change would produce. The button should say
what will happen: "Lighting this also lights 1 other cell".

**2. Clearing a cell in a multi-source request is ambiguous, and the UI must ask.** There are two
real operations:

- **Drop the destination** from the request. This clears that column across the whole rectangle.
- **Split the source out** into a request of its own, keeping the other sources.

Dropping the destination is the default, because it is what the request says. Splitting silently
creates a second name, a second lifecycle and a second thing to delete later, so it must be an
explicit choice with the new name visible. Do not pick either one silently.

**3. Request-level settings belong to the rectangle, not to the row.** `provider`,
`idle_teardown_ms`, `sched_prio` and `labels` are request-level, and a row can belong to more than
one request, so a settings panel attached to a row header shows the wrong thing whenever it does.
Attach them to the rectangle, which is the request. The row header carries only what a row *is*: the
node, the domain selector, the flow selector and the match count. The per-destination `provider`
override is the one setting that stays at cell level, because that is where the API puts it.

**4. `PARTIAL` belongs to the rectangle.** It never appears on a path (§4 of this document). A cell may show it only
as an aggregate *computed* over the cell's own paths, using the same fold the server uses, and a path
row in a detail view must never show it. Each row's per-source breakdown is its own part of that
state. This is what makes a twelve-source ingest wall readable: the rectangle says `PARTIAL`, the row
headers say which studio is dark, and only after that does anyone need a path list.

**5. Grouping by node now matters in both directions.** *§7a used to justify grouping by node with
"one source to five destinations is 5× egress on one node".* That argument still holds along the
rows. Fan-in adds the mirror case down the columns: twelve sources into one domain means twelve
target workers and 12× **ingress** on that destination node. For an ingest wall, ingress is the
limiting direction, because an edge node is limited by what it can receive. Group **both** axes by
node: a spanning header over the columns, and a band above each block of rows. Each grouping now
shows a real resource fact, and a grid that makes only one of them readable renders half its
requests badly.

### Switching a route off: parked destinations keep their row and column

Both axes are derived from requests. Before `disabled` existed, a route that was switched off did not
exist in the desired set at all, so the row and column it was on disappeared with it. Clearing the
last cell of a 1×1 request removed both the source row and the destination column. The operator had
not asked for that; it happened because the desired set had no way to record *off*. The board
rearranged itself under the pointer, which a routing board must never do.

Before `disabled`, the only place this could be worked around was the client, with this rule:

> **Nothing you authored disappears because it became unused.** An emptied request's sources survive
> as draft rows ready to re-route; a column survives its last cell being unlit, **for the rest of the
> session**. Both have their own `×`. In the fleet neither exists any more.

That behaviour is correct, but it is client-side and lasts only for the browser session: it is lost
on reload and was never visible to a second operator. The fix is `disabled` on a destination entry
(§3 of this document, §9.1). It is a model change rather than a UI change, because what was missing was a *value* in
the desired set, not a place in the UI to cache one.

**A cell click parks the leg; it does not delete it.** The default for clearing a cell (consequence 2)
is already "drop the destination from the request, which clears that column across the whole
rectangle". Parking is the same operation with the entry kept, so no new gesture is needed and the
rectangle keeps its shape. The rectangle is now **sources × enabled destinations**. A parked
destination darkens one whole column of the rectangle, so it is a column operation and does not
create a notch. The ambiguity in consequence 2 remains, and so does its resolution (drop the
destination, which now means parking it, or split the source out). But the default answer is no
longer destructive, which was most of what made it uncomfortable.

**`×` only ever removes something that is already dark.** Build to this rule. It is what makes a
small target in the corner of a chip safe next to the large cell target:

| Control | Means | Moves media? |
|---|---|---|
| the cell | park this leg, or light it again | **yes** — it is a cancellation with the text kept |
| `×` in the corner of the chip | remove this destination from *this* request | no — offered only on a parked leg |
| `×` on a column | remove this destination from *every* request that names it | no — offered only when every cell in it is parked |
| `×` on a row | remove this source, or its request if it is the last | **yes**, if its request still has a live destination |

So the large click target is the one that moves media, and the small one only tidies up. That is the
right way round for a control that sits in a corner and is reached past something else. Deleting a
live leg takes two deliberate acts: park it, then `×`. The live cell's tooltip should say so, because
a control that is simply missing does not tell the operator what to do instead.

**There are two `×` controls on the destination side, because "remove this destination" has two
meanings.** Several requests writing into one domain is ordinary fan-in. The chip's `×` removes the
leg from the one request the operator is looking at, and the column's `×` removes it from all of
them. A single control that guessed between the two would be wrong half the time, and when it was
wrong it would be a bulk teardown.

**The row's `×` is the exception and stays destructive.** This follows from the model, and is not an
inconsistency. `disabled` is a flag on a *destination*, so a row of a multi-source rectangle has no
parked state to be put into first. Requiring it to be dark would mean parking the whole request,
which also darkens the other sources. The row's `×` says so when it would stop media. If this becomes
a problem, the fix is a flag on a source, not an exception to this rule.

**Draw a parked cell; do not leave it blank.** A parked leg is authored intent, and the grid's job is
to render the desired set. So a parked cell keeps its two fixed-shape lines: the state word, and in
place of the count, something that does not vary in length. An unlit cell and a parked cell must not
look the same. One means "nobody has ever routed this" and the other means "somebody routed this and
switched it off", and an operator needs to tell them apart at a glance. This is also what the change
achieves overall: the grid stops showing only what happens to be running and becomes a board that was
laid out once, which is what an operator used to a crosspoint router expects.

**What parking does not cover.** There is no flag on a source, so one *row* of a multi-source
rectangle cannot be darkened on its own. "Studio-b is down for the week; keep it in the request" still
means removing or splitting the source. A source flag is designed to be additive (§9.1), and the case
is narrower than it sounds, because a single-source request goes dark entirely when its destinations
do. Do not simulate a source flag by parking destinations, because that also darkens the other
sources.

**Two implementation traps with the chip's `×`**, both found while building it:

- The `×` must be a **sibling** of the chip, absolutely positioned over its corner, never a child. A
  `<button>` inside a `<button>` is invalid HTML, and the inner click bubbles to the outer button, so
  a nested `×` parks the leg on its way to deleting it.
- Because it is positioned out of flow, it also takes no space in the cell. A control that took space
  inside the cell would resize every row it appeared in, which is the problem the next section
  describes. Where a control *is* in flow, as on the row and column headers, render it
  unconditionally and toggle `visibility`, as the split control and the rectangle badge do.

### Cell and grid sizes must not depend on content

**A cell's size must not depend on its content.** A cell holds one state word and a count, both of
fixed shape; the reason goes in a tooltip. `reason` is prose of any length, and all cells in a row
share one height, so a reason shown in a cell would resize its row, and the grid would reflow
whenever one leg started reporting a reason. The tempting case is a leg with no count to show because
its selector matches nothing yet. That case occurs as soon as a new request is added, so the operator
would see adding a request break the layout.

**Nothing else in the grid may change size with state either.** Examples:

- a discard control shown only on draft columns;
- a badge shown only on a node whose lease expired;
- an extra line shown only when a row is empty;
- a rectangle outline shown only once a request has two sources.

Each adds a few pixels, and a table shares heights across a row and widths down a column, so each one
moves the whole grid under the pointer at the moment the operator is clicking in it. Reserve the space
unconditionally instead of adding elements conditionally: render the control and hide it, and keep
the number of lines fixed while varying the text.

### Unlike a crosspoint router, a column accepts many sources

**A crosspoint matrix is exclusive: one source per output, and a take *replaces* the previous one.
This system is not.** A destination domain holds many flows from many requests. Several lit cells in
one column is normal and correct. Fan-in is the supported way to land several sources in one domain,
and it is refcounted so the domain is materialised once.

An operator trained on an SDI router will expect a second click in a column to displace the first. It
does not. **Design against that expectation explicitly.** Make a column read as additive, with
stacked chips, a count, and a column header that says "3 sources". Never draw a cell as a latched
crosspoint button on an exclusive bus. This is the biggest risk in borrowing the router idiom, and it
is a problem of visual language, not of logic.

A related trap is **take semantics**. A router take is instantaneous. Here a click records durable
intent, which may legitimately sit in `WAITING` for hours before anything happens. The state in a
cell is not a switch position; it is the aggregate state of a set of paths. There is no take button,
and there should not be one.

### The matrix requires an `exclusive` namespace

**Settled: the matrix is an editor only over a namespace whose `paths` mode is `exclusive`.** §7b
gives the reasoning; this section says what it means for the screen.

Two lit cells must always be two distinct claims. If two requests can expand onto one path, a cell no
longer means what it appears to mean; §7b lists the three ways. The dangerous one is un-lighting a
cell: it cancels a request but stops nothing, while the cell goes dark as if it had, so the operator
believes the route was torn down while the egress is still there.

So:

- **The namespace picker shows every namespace's mode** at all times, not only on hover.
- **A `shared` namespace does not get a matrix.** It gets a different view, the ledger of §7c,
  rather than this matrix greyed out, because the grid is as misleading to *read* in that mode as it
  is to click. *This item used to add "offer the conversion to `exclusive` from there".* It no longer
  does: a shared namespace is a supported arrangement, not a state to plan an exit from, and §7c
  records why the conversion planner built on the other reading was removed.
- **The matrix creates its own namespaces as `exclusive`.** The API's default is `shared`, and
  `default` is auto-created that way, so every create path in the UI has to set the mode
  deliberately. *Not built yet:* `ui/app` has no control that creates a namespace or changes its
  mode. Only the `*.live.ts` test fixtures call `api.applyNamespace` (open-items §2.14).

**The one gap, and its limits.** Exclusivity is enforced on *materialised paths*. So two requests
with an identical source and destination are both accepted while their selector matches nothing.
Verified: two requests naming a group hint that no flow carries both come back `WAITING` /
`flow_not_found` with zero paths, and no overlap is reported. That is **one cell with two owners**,
and it turns into one `INVALID` request as soon as a producer appears. Handle it in two places:

- **Refuse it on the client when creating.** This can be decided structurally, by comparing source
  entries, without asking the fleet.
- **Render it correctly when it arrives anyway**, from the CLI or an adapter. This is the one place
  where the sharing markup that §7b rejects is useful. It applies only to cells with no paths, and
  that condition cannot last.

*In `ui/app` today:* the second half is built. The matrix draws a two-owner cell with sharing markup
and disables clicks on it (`views/Staging.live.ts`). A check that refuses the case when a request
is created was not found (open-items §2.14).

### Collisions between two sources: `duplicate_source_flow` and `flow_conflict`

Fan-in brings two collisions. Both are properties of a *pair* of cells, not of one cell:

- **`duplicate_source_flow`**: two sources pin the same flow UUID into a shared destination. It is
  refused at POST, so it blocks the change, and the message names both sources by index:
  `sources[0] (studio-a/media/cameras) and sources[1] (studio-b/media/cameras) both pin flow 5592… into the same destination`.
  Anchor it to the two **rows**, which means a row must be able to point at another row.
- **`flow_conflict`**: the same problem arriving later. One or both sides were selectors, and a
  producer published the colliding flow months after the request was written. It is reported on a
  path, so the cell shows `INVALID` with a reason that names the other path.

The grid should not try to draw a line between two rows. Naming the other row in the reason and
letting the operator jump to it is enough.

### The three editors around the matrix

The matrix is the workspace. Forms do not disappear; they become the places where the operator does
what the matrix cannot express by itself.

**New row: the source editor.** The steps are node, then domain, then **a group, then how much of
that group to take.** Do not start with a flat list of flows. The operator browses flows to
*discover* what exists, but what they mean to select is the group, and a UUID picker recreates the
problem that selectors were introduced to solve.

The domain step is a choice between two kinds of selector. Make the choice visible rather than
defaulting silently:

| Domain | Selector | When |
|---|---|---|
| **this one** | `{name: {area, elements}}` | the operator picked a domain out of the list |
| **anything labelled** | `{labels: {…}}` | the operator picked labels |

A manifest that names a domain is self-contained. A manifest that names labels depends on a
`kind: domain` document having been applied. Tell the operator which one the row will produce,
because a label row is a standing query: a domain labelled tomorrow joins it. That is the intended
behaviour, and it also surprises people.

Then the flows:

```
   Studio A:Camera 1      audio + video     2 flows
   Studio A:Camera 2      video             1 flow
   (no group hint)        —                 1 flow

   which of its flows:  [ all ][ select type ][ select flows ]
```

| Mode | Selector | The box below holds |
|---|---|---|
| **all** | `{group_hint: {name}}`, or `{all: true}` for the whole domain | nothing — say so, and say what it means |
| **select type** | `{group_hint: {name, type}}` | the types present, pick one |
| **select flows** | `{flow: <uuid>}` | the flows, pick any |

`all` is the default, and it should be the most attractive option. Omitting `type` is how a camera's
video and audio travel together, and it is a *standing* selection: a flow the producer adds later
joins it. In this mode the box below is empty by design, so write that into the box rather than
leaving it blank. There are now two ways to say "everything": `{all: true}` takes the whole domain,
and a group hint with no type takes one group of it. Explain the difference in a sentence in the UI,
because `{all: true}` is the subscription shape of the retired proxy, and it is what an operator
migrating from that proxy will reach for.

**`select flows` creates one row per flow, and must say so.** A flow selector pins exactly one flow ID,
so three pinned flows are three sources. Now that sources are a list, those three can be one request
instead of three. Both are defensible, and they differ in lifecycle:

- One request means one name, one delete, and one aggregate that goes `PARTIAL` when one camera is
  dark.
- Three requests mean three of everything.

Show the names that will be created before creating them, and make the choice an explicit control
rather than a side effect of a checkbox.

**Ungrouped flows must stay reachable.** A producer that never set the NMOS tag still produces flows
the operator may need to replicate. But there is no name for a group-hint selector to match, so `all`
and `select type` cannot be expressed for those flows. Show them as a pseudo-group, disable those two
modes for it, and note that `{all: true}` over the domain does include them.

The source-domain list comes from `GET /v1/nodes/{node}/domains`. It reports **observed** domains
joined with their label records, so it covers what the agent sees and also labelled domains the agent
is not currently observing. That is how an operator sees a label they applied before the producer
came up. `domainInfo.observed` is the flag that tells the two apart, and a labelled but unobserved
domain is information, not an error.

Show each domain's labels, and its `name` label if it has one, since that is what an operator called
it. Offer labelling from here. It is `POST /v1/nodes/{node}/domains`, and "see an unnamed domain,
name it" is the task the CLI's `label` verb exists for. A label write is a mutation with its own
preview (§3 of this document), so it gets the same dry-run treatment as everything else on this screen.

**New column: the destination editor.** A router matrix has no equivalent of this step, and the
reason is structural: **the destination domain does not exist until a request names it.** So the
column set is every destination named by some request, plus the one being created. Creating a column
is real work and needs a proper control:

- **Only nodes with an area they grant `write` on can be destinations.** Show the other nodes
  disabled with the reason (`area_not_writable`, or `unknown_area` for a node with no areas at all)
  rather than leaving them out. Leaving them out makes the operator ask "where is edge-03?".
- **The area is part of the name, not a separate setting.** Render the area picker and the elements
  as one field that reads `fast/studio-a/cam1`, list only writable areas in the picker, and never
  fill the area in invisibly: omitting it omits half the name.
- **Show the resolved directory as the operator types.** `area.path` is advertised (for diagnostics
  only, and it may be absent, so guard it), so the field can show `/dev/shm/mxl/studio-a/cam1` under
  the name.
- **Names are unique per node across areas, and nesting is the only collision.** `fast/ingest` and
  `bulk/ingest` are two different domains, and both are allowed. What is refused is `fast/studio-a`
  against an existing `fast/studio-a/cam1`, where one domain directory would contain another. The
  code is `domain_name_in_use`, and the message names the other domain. It marks the path
  `INVALID`; it does not refuse the `POST` (architecture §7.2). *If the operator has read
  older docs, note that this rule has been inverted; the current one is the intuitive one.*
- At most 8 elements, and at most 255 bytes for the whole rendered name.

**Cell detail: the per-leg editor.** The per-destination `provider` override lives here, and so does
the breakdown: which paths this leg expands to, their states and their sessions. A cell is an
aggregate, and §11 requires both the summary and the breakdown to be reachable. The cell detail is
also where the negotiated provider finally becomes visible, as `path.session.interface.provider`;
nothing shows it before apply. *Not built yet:* `ui/app` has no cell detail view. The source and
split editors carry an existing per-destination `provider` override along, but nothing in the UI
sets one, and the negotiated provider is shown only on the path and session detail views
(open-items §2.14).

### Unrouted sources

*Built on 2026-09-01: `ui/app/src/components/UnroutedStrip.vue`.* **One sentence
below could not be implemented as written, and was not.** The strip was specified as "flows present
in inventory that **no request selects**". Computing that needs selector evaluation in the browser:
label sets ANDed over a node's domains, group hints with and without a type, pinned IDs, and
`self_output` on top. That would be a second expansion engine next to `reconcile.Compute`, which could
disagree with it without anyone noticing, and §3 of this document says not to build one. The
implementation asks a question the API answers directly instead: **is there a path whose source is
this flow?** `path.source` is a `FlowAddress`, and an inventory entry is the same triple, so this is a
set-membership test and nothing is guessed.

The two definitions differ in one case, and the implemented one gives the better answer there. A
request whose selector matches a flow, but whose destinations are all **parked** (§9.1), expands to
no path, so the strip lists that flow as unrouted. The operator's question is "is this going
anywhere?", and a parked route is going nowhere. "No path carries it" is a different claim from "no
request selects it", and the code makes only the first.

A matrix shows only what someone has already asked for, so "camera 5 is going nowhere" is invisible
in it. Keep a strip or panel of flows present in inventory that no request selects. The flow browser
lives there, and clicking a flow starts a new row pre-filled with it. This covers discovery, which
the matrix alone does not.

*Building it showed a problem that the "clicking a flow" sentence hides.* A strip entry is **one
flow**, and routing one flow by pinning its ID writes the narrowest selector the API has from the
broadest gesture on the screen. The flow's siblings would be back in the strip the next day, and the
operator would end up with one request per UUID. So the click opens the editor on the flow's
**group** instead, which is the standing selection the operator meant. Only a flow with no group hint
is pinned, because there is no name for a group selector to match. This is also why the click opens a
pre-filled panel rather than staging an edit directly: the strip's choice selects more than the
operator pointed at, so it has to stay visible and changeable.

Two filters apply to the strip, and both are required:

- **"No request selects" means no request in this namespace**, with a note on the flows that another
  namespace already routes. §7b explains why; neither of the two simpler readings works.
- **Flows with `replicated: true` are this project's own output.** They are valid sources (that is
  how a chain `A→B→C` is written), but they are not "unrouted"; they are the far end of something
  already routed. Mark them rather than hiding them. Note also that a *label* selector never matches
  them (`self_output`), so routing one onward means naming its domain.

### Every mutation is still dry-run first

`POST /v1/namespaces/{ns}/requests?dry_run=true` runs validation, builds the candidate fleet,
reconciles it and returns the `api.Request` that *would* result. It skips only the write. Verified: a
two-source, two-destination group-hint request comes back with all six expanded paths, their real
IDs, their real session IDs, their real states and the per-source breakdown, with
`X-Mxl-Outcome: created`, and the store's request list is still empty afterwards.

So the UI needs no expansion logic of its own. It renders `status.counts`, `status.sources[]`,
`status.paths[]` and `status.excluded[]`. It never needs to know that two sources matching three
flows across two destinations make six paths; it asks the server.

**"Every mutation" includes label writes.** `POST /v1/nodes/{node}/domains?dry_run=true` returns the
resulting record plus `stopped[]` and `started[]` as full `Path` objects. A label edit offered from
the source editor moves media and has a preview available. Use the preview, and show it in the same
place as the request preview.

In the matrix, this is what makes a click safe. Lighting a cell works like this:

1. Take the request's stored spec.
2. Append one entry to `destinations[]` (or one to `sources[]`, or create a new request).
3. Dry-run it.
4. Show the operator the paths that would appear, including those in *other cells of the same
   rectangle*, and any refusal.
5. POST for real.

**The cell can preview its own consequences before it commits**, which a crosspoint button has never
been able to do.

Debounce the dry run, because each one is a full store load plus a reconcile (§2 of this document). Send it only when
a change is *structurally complete*, not on every keystroke in a domain field.

### The three outcomes of a request POST, and how to render each

Confusing these is the most likely way this screen goes wrong, because two of the three are success:

| Server says | Meaning | Render as |
|---|---|---|
| `400` + `details.reason_code` | **Refused.** Never resolves by itself. | Blocking. Refuse the cell, anchor the message to the field or the row the code names. |
| `200/201`, `state: WAITING`, `paths: []` | **Accepted, not yet satisfiable.** | A quiet cell state, not an error. The click goes through. |
| `200/201`, paths with states | Accepted and expanding. | The cell lights with its aggregate state. |

The per-path `INVALID` codes (`flow_conflict`, `loop`, `domain_name_in_use`, `namespace_overlap`)
arrive in the third row, on individual paths of an accepted request, never as a `400`. Only the
codes in the first list of architecture §7.2 refuse the `POST`.

The middle row is the one to get right. A cell with no paths means the selectors have not matched yet
(see "Rows and columns are selectors" above). Setting up replication in advance for a camera that is
not live yet costs nothing and is explicitly supported, so a UI that refuses that click has removed a
supported workflow. Light the cell as `WAITING` with "no flow yet" and leave it; it becomes active by
itself when the producer appears.

Compare a refusal, whose prose states its own fix:
*"node "edge-01" advertises no area "nope", it has "bulk" (writable), "fast" (writable), "media"
(read-only)"*. Render these messages verbatim, since they are better than anything the UI would
write, and use `reason_code` only to decide *what to highlight*.

The refusals that anchor to a **row** rather than to a field are the fan-in ones: `same_endpoint`
(whose message names both indices, `sources[0] and destinations[0] are both edge-01/fast/ingest`) and
`duplicate_source_flow`. Both are typos where both conflicting entries are on screen in front of the
operator. That is why refusing the whole request is right for these two, and for nothing else.

### Committing: apply each click, or stage and apply

`POST` is create-or-**update** on the name, with no create-only mode and no 409, and the dry run
reports which of the two it will be via `X-Mxl-Outcome` before anything is written. That allows two
reasonable commit models, and the choice should be made deliberately:

- **Apply on click.** Each toggle is its own POST. Requests are independent durable intent, and an
  unchanged apply writes nothing, so this is safe, and it feels like a router.
- **Stage and apply.** Toggles accumulate as pending changes, and one Apply commits them. The pending
  set is a manifest diff. This is closer to how the desired set is actually managed, and it gives the
  operator a chance to undo before live media changes.

Staging matches the declarative model better and is the safer default for something that moves
uncompressed video. Apply-on-click matches the router idiom the matrix borrows. The UI uses staging,
and it paid off more than expected. A staged set can be dry-run as a batch, so the preview
reports real outcomes and real blast radius before anything moves, and that made every confirmation
dialog unnecessary. Apply-on-click would need those dialogs back, and needs them more now that one
click can light several cells of a rectangle.

Rules for either model:

- **`unchanged` means do not write.** The server already skips the write, but a UI that re-POSTs on
  every interaction turns a screen that is resyncing into store churn, against the sizing in §8.3.
- **An empty request is a state, not an event.** A request must name at least one source and at
  least one destination, so a rectangle with no sources or no destinations has no spec to POST, and
  committing it is a `DELETE`. It is tempting to make un-lighting the last cell perform that delete,
  with a confirmation in front of it. Don't. It makes the order of two clicks matter for the same end
  state: clearing a cell before lighting its replacement destroys the request, and doing it the other
  way round does not. Let the request sit empty, say what applying will do, and let the commit work
  out that an empty spec means `DELETE`.

  **`disabled` removes the need for this rule.** With parking, a destination entry never leaves the
  spec, so there is no empty spec, nothing for the commit to reinterpret, and no `DELETE` inferred
  from an absence. The ordering problem no longer exists, rather than being carefully avoided:
  park-then-light and light-then-park reach the same spec, because both are edits to the same entry
  list. The paragraph above is kept because it describes the mistake; the mechanism it prescribes is
  replaced by the one in "Switching a route off" above.

  The draft and the request are different things. The draft is what the operator has written, and the
  request is what exists on the server. Keeping them separate is what makes re-routing easy for the
  operator. It also gives the "remove" control a distinct meaning: *I am done with this source*,
  rather than *I am done routing it here*.

- **Staging is the confirmation.** When there is a pending bar with preview and discard, a modal adds
  nothing. A dialog that appears in the middle of a gesture gets dismissed by reflex, whereas a staged
  change has to be read and applied. Put the detail in the preview instead, where it can be specific.
  For a cancellation, show which paths actually stop, computed from `path.requests[]`, since a path
  keeps running while any other request references it. "3 of 4 paths stop" is worth reading; "are
  you sure?" is not.

### The request name

A request needs a name before anything can be dry-run, because the name is the idempotency key, and
it is validated (letters, digits, `-_.:`, no leading dot). Suggest a name as soon as there is a source
and a first destination (`cam1-to-edge-01`), keep it editable, and let the operator own it. Names end
up in the manifest and in `delete` commands, so they are not an implementation detail.

**Renaming does not rename anything.** `(namespace, name)` is the ID, so a changed name creates a
*new* request, and the old one stays, still running and still holding its cells. Lock the field once
the request exists, or make "duplicate" an explicit action that shows both requests will exist.

**Duplicating is probably the most-used control on the screen.** When a new camera arrives, it should
be routed like the last one: copy the rectangle, change the selector, keep the destinations. That is
shorter than any create flow, and it starts from something known to work.

### The matrix corresponds to one manifest file

*Postponed, not built* (open-items §3.4). The rest of this section is the design.

Each rectangle is one manifest document, so the whole grid serialises one-to-one to the
multi-document YAML file. Show that: a panel or drawer with the current manifest, copyable, and, if
you use the staged-commit model, the pending changes as a diff against it.

This matters. The manifest is the documented operator interface and the thing that lives in git. A UI
that only ever creates requests through its own controls creates a second source of truth for the
desired set without saying so. Showing the file makes the UI teach the format instead of competing
with it, and "I worked it out on screen, now commit it" becomes a copy rather than a rewrite. It also
answers "how do I do this for forty cameras?": with a file, and the operator has just seen what one
looks like.

The rendered file differs from the wire format in three ways, and must get all three right:

```yaml
namespace: nab
name: cam1-distribution
labels: {show: nab}
sources:
  - node: studio-a
    domain: media/cameras                # a scalar is a name...
    group_hint: {name: "Studio A:Camera 1"}
  - node: studio-b
    domain: {role: cameras}              # ...a map is a label selector
                                         # no flow selector: every flow in it
destinations:
  - {node: edge-01, domain: fast/ingest}
  - {node: archive-01, domain: bulk/capture, provider: tcp}
provider: [verbs, tcp]
```

- The flow selector is **flattened** onto each source (`flow:` or `group_hint:` directly under the
  entry), where the wire nests it under `select`.
- The domain is a **string or a map**, where the wire has `{name: {area, elements}}` or
  `{labels: {}}`.
- An **omitted flow selector means `{all: true}`**. The CLI fills it in; the server never does.

`sources:` is always a list. An unrecognised key is an error in a manifest. That is the opposite of
the wire's rule, and it is deliberate: a declarative format exists to prevent a typo that silently
does nothing.

### What not to build

- **A multi-step wizard.** Steps make repetition slow, and routing is the task operators repeat.
- **A node graph as the editor.** Build it as the topology view instead. *Done, and the reasoning held
  up in practice: a vertex is a **node**, and no request names a node pair. A request names a source
  selector and a destination domain, so a wire drawn between two vertices has nothing to turn into.
  The graph's only gesture is selection (§7 of this document, item 3).*
- **Exclusive-crosspoint visuals.** This is the most important item on the list.
- **A client-side reimplementation of §7.2.** Structural hints for immediate feedback are fine.
  Everything else is the dry run's job, and only the server can see conflicts between requests.
- **A rectangle with notches.** If the UI ever lets a request's cells be lit individually, it is
  modelling a grid rather than the API, and the next POST will not round-trip. `disabled` does not
  weaken this rule; it confirms it. Parking is per *destination*, so it takes out a whole column of
  the rectangle. A flag on a `(source, destination)` cell is refused in the model for the same reason
  it is refused here: a request would stop being sources × destinations and become an arbitrary
  bitmap (§9.1).

### Two gaps in the server API

**The dry run does not report which provider was negotiated.** `PathStatus` carries id, source,
destination, state, reason and session id, but no interface config (verified). Negotiation
*failures* are reported as `no_shared_fabric` / `no_shared_provider` / `no_shared_capability`, so a
cell can report that a leg will not work. But for `provider: [verbs, tcp]` ("prefer verbs, tcp
acceptable"), the question "which one did I get?" can only be answered after apply, from
`GET /v1/paths` → `path.session.interface`. §10.4 makes the choice of provider a large performance
difference rather than a detail, so showing it in the cell preview is useful. Adding the negotiated
provider and fabric to `PathStatus` is a small server change. It matters only when a fallback list is
in use; a hard pin either works or is refused.

*This is now in scope and is to ship with the UI* (`docs/open-items.md` §2.10). `Compute` negotiates
before it emits, so the value is already on the session record when the status is built, and the dry
run has it too. It is two fields on one struct, and the preview is what needs them.

**A matrix wants one read, and there are four.** Rectangles come from `GET /v1/requests`, cell states
from the same call's `status.paths[]`, column metadata from `GET /v1/nodes`, and the unrouted strip
from `GET /v1/flows`. Each is a full store load plus a reconcile (§2 of this document), and this screen polls
continuously. If the matrix turns out to be the whole product, the addition to ask for is a single
composite read, or an ETag so that some of the four usually return 304. Either is better than four
independent timers. But an ETag does not help three of the four: of the reads this screen makes, only
`/v1/nodes` can soundly be cached, because the other three are either time-dependent or are what the
poll exists to fetch (§2 of this document, `docs/open-items.md` §2.9).

---

## 7b. Namespaces: the matrix shows one namespace

Two requests can expand onto **the same path**. The path identity is
`(src node, src domain, flow-id) → (dst node, dst domain)`, and nothing in it names the request that
asked for it. So a group-hint request and a pinned-flow request over one source domain, pointed at
one column, produce **one edge, one session, one worker, refcount 2**.

*(That identity used to include the resolved output root as a separate term. The areas work removed
it: the area is the first segment of the domain's name, so `fast/ingest` and `bulk/ingest` are
already two identities and the extra term was redundant (§5.4).)*

Drawn as a matrix, that shared path is wrong in three ways at once:

- Two lit cells in a column suggest two streams when there is one.
- The cell counts no longer add up to what arrives on the node.
- Un-lighting either cell cancels a request and stops nothing, while the cell goes dark as if it had.

The last is the dangerous one. Nothing fails, so the operator believes the route was torn down and
does not come back to it while the egress is still there.

This can be marked up in the cell, and it was. That is the wrong kind of fix: it adds decoration to
every cell on the screen to describe a condition that should not exist. **Partition the requests
instead.** A request belongs to exactly one namespace. In a namespace whose `paths` mode is
`exclusive`, no two requests may hold one path. The matrix shows one namespace. Then a cell means
what it looks like: the cell counts add up to the column's, clearing a cell stops exactly the paths in
it, and no markup is needed.

**The default mode is not the one this screen needs.** Namespaces default to `shared`, and `default`
is auto-created that way on first reference. Verified: `[('default', 'shared', 1), ('nab',
'exclusive', 1)]`. The rule is opt-in because it protects *this screen's* readability rather than
anything about the fleet (see below). So the matrix creates its namespaces as `exclusive`, shows the
mode in the picker, and refuses to be an editor over a `shared` namespace (§7a).

### How the server decides whether two selectors overlap

The rule can be enforced by the server, which is what makes a namespace a real partition and not a
convention. `FlowInventory.GroupHint` is a single pointer, so a flow carries **at most one** parsed
hint. That limits how two selectors over one source domain can intersect, and most cases can be
decided without looking at the fleet:

| Two selectors | Overlap? |
|---|---|
| different `group_hint` names | never, whatever producers do |
| same name, different `type` | never |
| different pinned flows | never |
| `{group_hint: X}` and `{group_hint: X, type: T}` | yes, statically |
| the same pinned flow twice | yes, statically |
| `{all: true}` and anything else over one domain | yes, statically |
| a pinned flow and a hint | **only dynamically** — a producer can retag |

This distinction is what makes the rule predictable for an operator instead of seeming to fire at
random. Nearly every overlap can be refused at admission, with a message naming the other request.
Only pin-versus-hint can appear later, so it must be a **reconcile-time state and not only an
admission check**. A rule checked only at create time would be wrong the first time a producer
republished its flows with a different tag.

This still holds, but it is no longer the whole picture: **two selectors in one request** can now
also reach one destination flow, through two source domains on two nodes. That is not one edge. It
is `duplicate_source_flow` or `flow_conflict` (§7a), which is a different code with different
handling, and the grid shows it differently.

### What happens to the losing request

`namespaceOverlaps` in `internal/server/reconcile` enforces the rule. Its behaviour, all verified:

- **Admission needs no extra code.** `handleCreateRequest` already reconciles a candidate fleet, so
  the rule is written once and covers both the POST and a later reconcile that discovers an overlap.
- **The POST succeeds.** The colliding request comes back `INVALID` with
  `reason_code: namespace_overlap` and a message naming the request that holds the path:
  *"request "wall" already replicates studio-a/media/cameras 5592a23b… to edge-01/fast/ingest in
  namespace "nab""*. Refusing the whole request would mean one bad pairing blocking nineteen good
  ones, and selectors make that common.
- **Precedence is stated as incumbency, then `UpdatedAt`, then ID**, the same order every conflict
  rule uses, with the intent that the request already carrying media keeps the path. For namespace
  overlap, incumbency never decides in practice; see below.
- **Losing does not stop media.** The losing leg gets no new session, and the path, held by the
  winner, carries on. The loser's other legs are unaffected. This is what makes the rule safe to turn
  on over a fleet that already has overlaps: they appear as `INVALID` requests, not as an outage.

**A parked leg holds nothing, and re-enabling it can lose.** A disabled destination produces no path,
so it cannot hold one and cannot make another request `namespace_overlap`. The consequence to show
the operator: park a leg, let another request claim its path, un-park it, and *this* request is now
the loser. That stops no media, but it means "switch it back on" is not guaranteed to undo "switch it
off". Read the dry run before flipping the flag back, as for any other write.

The mechanism is not the obvious one. `namespaceOverlaps` orders on `(UpdatedAt, id)` and does not
look at sessions. Incumbency cannot separate two requests over one path, because the path's session
exists whichever request holds it. So the contest is decided by recency: the request with the older
`UpdatedAt` wins. Un-parking is a write, and every write updates the stamp, which reliably makes the
returning request the newest and therefore the loser. Do not build a UI that explains this as "the
one carrying media keeps it": a request with an older stamp will take the path straight back from one
that has been running for a week.

**The loser still reports the path.** This is trap 14, repeated here because a matrix must not get it
wrong. The loser's own `status.paths[]` lists the contested path *with the winner's state*, so a
request that holds nothing can show `{"ACTIVE": 1}`. Only `/v1/paths` → `path.requests[]` names the
owner. A cell drawn from a request's own status, without an ownership check, shows another request's
media as this one's, and it does so in the one situation where the operator most needs to know they
are not the ones carrying it.

### A namespace is an object, not a label

**A namespace is a first-class object, and `namespace` is a real property on the request** (§9.3).
*An earlier version of this section put the namespace in a reserved `namespace` label: a plain
`namespace:` field in the manifest, and a label on the wire.* The justification was that
`--prune -l namespace=nab` already meant "make the fleet's `nab` namespace equal this file". That was
an argument from an existing CLI mechanism rather than from the model, and it cost more than it
saved:

- `namespace` was a legal user label, so a label an operator wrote for their own reasons silently
  became a partition key.
- The two spellings had to be kept in agreement, and a disagreement between them was refused rather
  than resolved.

`--prune` now takes `-n`, which is the natural spelling anyway.

One namespace = one prune scope = one manifest document set = one matrix. The manifest pane in §7a
shows one namespace as one file, which is what an operator would commit to git anyway.

Five things to be explicit about:

- **Request names are scoped to the namespace.** `(namespace, name)` is the ID, so two operators, or
  two sources feeding one adapter, can both have a request called `cam1`. The canonical route is
  `/v1/namespaces/{ns}/requests/{name}`; `/v1/requests` is the fleet-wide list and takes
  `?namespace=`. **A name is unique within its matrix, not within the fleet**, so do not build a
  uniqueness check against every request.
- **Labels are for tagging; a namespace is a partition.** A request can have many labels, and they
  may overlap freely. `namespace` is no longer reserved among them.
- **It is deliberately not called a group.** The source editor's vocabulary is the NMOS group hint
  ("a group, then how much of it to take"), and the row header's selector chip says `group`. Calling
  the partition a group too would put two unrelated things with the same name a few centimetres apart
  on one screen. The NMOS term is external vocabulary operators already know, so ours is the one that
  changed.
- **A request that names no namespace is written into `default`.** Show `default` in the picker as a
  namespace in its own right, with its mode; hiding it would hide most fleets entirely.
  `GET /v1/namespaces` lists only namespaces that exist as records, so a fleet with no requests at
  all lists nothing. Add `default` to the picker in the client rather than showing an empty list.
- **Namespaces are auto-created on first reference and never auto-deleted**, and deletion is refused
  while any request references the namespace. `GET /v1/namespaces` includes each namespace's
  `requests` count, so the picker can show "3 requests" without counting on the client.

### Why exclusivity is opt-in per namespace

The UI should understand why the rule is opt-in rather than universal, because that decides what the
screen may assume. **Overlap within a namespace costs the fleet nothing.** Two requests on one path
share one path, one session and one worker pair, which is refcounting working as designed. Nothing
is doubled and nothing is corrupted. What overlap costs is accuracy in a grid. That is a real cost,
but it is *this screen's* cost, not the fleet's, so the rule is one a reader opts into, not one every
API client is held to. A Kubernetes adapter creating one request per pod wants `shared`: several pods
asking for one flow is ordinary, and marking the second one `INVALID` would make one pod's status
depend on whether another pod exists.

That is why the matrix must state its requirement rather than assume it. A control to switch a
namespace from `shared` to `exclusive` is reasonable to build, and a dry run already previews its
consequences: existing overlaps become `INVALID` requests, and no media stops.

### Shared destinations across namespaces, shown on the column header

Namespaces partition *requests*, not destinations. Two namespaces may still route one flow into one
domain, for example `production` and `archive` both landing camera 1 in `edge-01/fast/ingest`. That
is fan-in, which the API supports and refcounts, so the domain is materialised once. A screen showing
a single namespace cannot see this at all.

Show it on the **column header**: *"+ archive"*, or *"+ 2 namespaces"*. That is the right level of
detail. An operator thinking in namespaces asks whether another show writes into this domain, not
whether one particular edge is shared, and this is the fact that changes what emptying a column
means. For the same reason, keep the removal preview accurate one level down: dropping a leg that
another namespace also holds stops nothing, and `path.requests[]` already says so at no cost.

The alternative is to partition destinations too, so that a namespace *owns* its output domains. That
makes the fleet fully disjoint, but it forbids two shows from deliberately fanning into one archive
domain, which is the arrangement fan-in exists for. Partition the requests, and leave the
destinations shared.

### Which views are scoped to the namespace

**The landing page is not.** Health is a fleet fact. Scoped to a namespace, "is anything wrong" would
answer "is anything wrong in the namespace I happen to have selected", which is the wrong answer at
3am. Namespaces scope the workspace, not the health view.

**The unrouted-sources strip is scoped, with a marker.** Neither simple answer works:

- **Fleet-wide:** a flow that another namespace routes disappears from the strip. The strip is also
  where the flow browser lives, so that flow becomes impossible to find in the one view where routing
  is done.
- **Namespace-scoped with no marker:** the strip shows the flow as unrouted, the operator routes it,
  no `namespace_overlap` fires because the duplicate is in *another* namespace, and the strip has led
  the operator to double the egress on the source node.

So the strip is namespace-scoped, with a note on entries that another namespace already routes,
naming that namespace.

The two views are treated differently on purpose. Health is a fleet fact. Work to be done is a
namespace fact whose *consequences* are fleet-wide, so it needs the caveat attached rather than the
whole view rescoped.

---

## 7c. A `shared` namespace view: the ledger of claims

§7a's matrix has a precondition, and §7b gives the reasoning for it: the grid is an editor only over
a namespace whose `paths` mode is `exclusive`. That leaves `shared` namespaces with no view. `shared`
is the API's default, `default` is auto-created that way, and §7b names a real reason to stay in it:
a Kubernetes adapter writing one request per pod, where several pods asking for one flow is ordinary
and marking the second one `INVALID` would make one pod's status depend on whether another pod
exists. So a shared namespace is not a mistake to be converted out of. It is a supported arrangement
that the workspace grid cannot draw. **It gets its own view, and that view is not a grid.** The
view is called the **ledger**: a list of paths, each with the requests that claim it.

**Corrected on 2026-09-01, while building it: a path held by several requests is not a condition to
flag.** The rest of this section was first written treating such a path as *contested*. That word
had a name in the UI, a count on the header line, a highlight on the claim, a place in the attention
filter, and a whole feature built on it: the conversion planner described below. That reading is
wrong for the mode this section is about. A path with N claims in a `shared` namespace is
**refcounting working as designed**: one path, one session, one worker pair, nothing doubled. It is
the arrangement the mode exists for. §7b says so itself: overlap within a namespace costs the fleet
nothing, and what it costs is accuracy in a grid. The cost belongs to the **grid**, and this
view is not a grid. A Kubernetes adapter writing one request per pod produces this state routinely
and correctly.

So `held by N` is plain data, with no highlight and no judgement. The attention filter and the header
count are about **state** only, and the planner is gone (see below). The most valuable parts are
unaffected by the correction: **one row per path**, the **selector on every claim**, and the **`sole`
/ `shared`** counts with *rides along*. The rest of this section reads correctly with "contested"
deleted rather than replaced; the paragraphs that depended on it are marked where they appear.

Before this view existed, a shared namespace was rendered as the matrix, read-only, behind a
banner. That was accurate about editing and misleading about reading. The grid still drew two lit
cells for one path and still showed counts that did not add up, and greying the cells did not stop an
operator believing them. Making the grid read-only prevented wrong edits, but not wrong readings.

### Why not a matrix with sharing markers

*§7b already rejects marking shared paths in the cell, because it "adds decoration to every cell on
the screen to describe a condition that should not exist". That argument does not apply here*: in a
shared namespace the condition is ordinary, so paying for it in every cell would be paying for what
the screen is about. The reason to reject it here is different and more serious: **the axis breaks,
not just the cell.**

In an exclusive namespace, two rows cannot expand onto one path, so each row is a distinct query and
the set of rows partitions the desired intent. Without the rule, two requests may hold overlapping
selectors over one source domain. `{all: true}` against a pinned flow inside that domain is the
typical case, and §7b's overlap table says it collides statically. Two rows are then *different
queries with the same matches*, and there is no correct answer to which row a path belongs on. That is
not a property of a cell, and no markup in a cell can fix it.

The same reasoning rules out the other near-alternative, a matrix with one row per *request*. It puts
the same path on several rows by construction, which is the problem this view exists to remove.

### Rows are paths, and each path lists its claims

The matrix renders **intents**, with no deduplication. This view renders **claims**, where a claim is
the triple `(request, source entry, path)`. The claims are listed under the paths, and the path list
is the one the server has already deduplicated, which is the same deduplication the fleet does. This
removes all three problems §7b lists, rather than working around them:

- One path is one row, so nothing is counted twice.
- The counts add up, because the rows are the real edges.
- "Un-lighting stops nothing" is visible as a refcount that stays above zero.

```
  namespace [ k8s  ⟨shared⟩ ▾ ]   4 requests · 6 paths · 6 not active

  ── CLAIMS ─────────────────────────────────────────────────────────────────────────
  edge-01  fast/ingest                                            5 flows
    ← studio-a media/cameras 5592a23b  ESTABLISHING        held by 2
        wall          sources[0]  all flows
        cam1-pin      sources[0]  flow 5592a23b…
    ← studio-a media/cameras 9a2b1c33  PAUSED              held by 1   nobody is producing
        wall          sources[0]  all flows
    ← studio-b media/cameras 44e0aa17  ESTABLISHING        held by 1
        pod-abc12     sources[0]  group Studio B:Camera 1

  edge-02  fast/ingest                                            1 flow
    ← studio-b media/cameras 44e0aa17  ESTABLISHING        held by 1
        pod-abc12     sources[0]  group Studio B:Camera 1

  ── REQUESTS ───────────────────────────────────────────────────────────────────────
  wall         ESTABLISHING   4 paths · 3 sole · 1 shared
  cam1-pin     ESTABLISHING   1 path  · 0 sole · 1 shared   rides along — deleting it stops nothing
  pod-abc12    ESTABLISHING   2 paths · 2 sole · 0 shared
  pod-def34    DISABLED       0 paths · every destination parked
```

This is a real reading of the `k8s` fixture in §9 of this document, verified on 2026-09-01. `wall` takes the whole of
`studio-a/media/cameras`, `cam1-pin` pins one flow inside it, and `/v1/paths` reports
`["k8s/cam1-pin", "k8s/wall"]` on the one path they share.

**Group by destination domain, then by source flow.** That is the direction fan-in runs, and on the
ingress side it is the limiting resource direction (§7a, consequence 5). It also puts the two claims
on `5592a23b` on adjacent lines, so the operator can see the cause in a couple of seconds instead of
searching for it. The alternative grouping, by source flow ("who is consuming this camera"), is the
same data transposed. Offer it as a toggle, not as the default: a shared path needs *both* ends to
match, so grouping by destination always puts two claims on one path next to each other, while
grouping by source only sometimes does. *The toggle is not built yet:* `views/Ledger.vue` groups by
destination only (open-items §2.14).

### Show the selector on every claim

This is the most valuable part of the design, and nothing else in the product shows it. A path with
two claims raises one question, *why do I have two of these?*, and the answer is always the pair of
selectors that produced them. Showing `all flows` and `flow 5592a23b…` on adjacent lines lets an
operator see an interaction between selectors without having to know the rule behind it.

**It needs no server change.** `status.sources[]` carries each source entry's own path IDs (§4 of this document), so a
path can be joined back to the source that produced it, and from there to its selector. That is the
same join the matrix does for its rows, used in the other direction. Write `sources[i]` with the index, because `duplicate_source_flow` and `same_endpoint`
name their operands that way (§7a), and the two spellings should match.

*This paragraph used to say the field is `omitempty` and absent for a single-source request, so the
attribution needed a fallback.* The field is always present and always the full list (§4 of this document). Keep a
one-line fallback for older servers, but do not design the join around an absence that does not
happen.

### `sole` and `shared` counts per request

For a request, **`sole`** is the number of its paths where `path.requests.length == 1`. Those are the
paths that stop if the request is deleted. The **`shared`** paths keep running on another request's
claim.

This is the whole cancellation preview, computed in advance and always on screen rather than shown in
a confirmation dialog on demand. It also makes one condition visible that has no other symptom: **a
request with zero sole paths is carrying nothing.** Nothing is broken and nothing is doubled:
refcounting is working as designed, and the egress is not duplicated (§7b). But someone wrote an
intent that is entirely covered by another one, and in a namespace populated by an adapter that is
usually a bug in whatever writes the requests. It is invisible in the matrix and in `get requests`,
and here it takes one column.

### Each request keeps its own rectangle, so parked legs stay visible

A view built from paths loses one thing, and it is what `disabled` was added to protect. A parked
destination produces no path, so it has no claim to show, and a ledger built only from `/v1/paths`
would show a switched-off leg the same way as a leg that was never written. That is the same failure
§7a describes (the desired set with nowhere to record *off*), arriving by a different route.

So each request keeps **its own** small sources × destinations grid. A single request's rectangle is
never ambiguous: rectangles only overlap *each other*, and on its own a rectangle is the
request's spec as written. So it can draw parked legs dark, and it reuses the component §7a already needs rather
than inventing a second way to show the same thing. The shared grid is what fails in a shared
namespace; a per-request rectangle works.

`DISABLED` is therefore shown on the request row and nowhere else, which agrees with §4 of this document: it is
aggregate-only and derived, and a path is never `DISABLED`, because a parked destination produces no
path for the state to describe.

### Two simplifications compared with the matrix

**Trap 14 does not apply.** `namespace_overlap` fires only in an exclusive namespace, so in a shared
one there is no losing request reporting the winner's path as its own, and every claim the ledger
lists is really held. Still build the view from `/v1/paths` and its `requests[]` rather than from
each request's `status.paths[]`. That read is the authority on ownership in either mode, and it has
already done the deduplication. But the ownership cross-check that the matrix must perform is not
needed for correctness here.

**It needs two reads, not four** (see the last gap in §7a): `GET /v1/paths` and
`GET /v1/requests?namespace=`. Nodes are needed only for the request pane's destination editor, and
flows only if the unrouted strip is carried over.

### Editing

*Not built yet:* `views/Ledger.vue` is read-only. It offers no edit and no delete (open-items
§2.14). The design follows.

Offer one mutation and no cell gestures: **edit the request document** (form or YAML, dry-run,
apply), plus delete, whose blast radius is now read from the ledger rather than computed. The
ambiguity of §7a consequence 2, between dropping a destination and splitting a source out, does not
arise, because there is no cell to clear: the operator is always editing one named request.

That is deliberately less than the matrix offers, and it is the right amount. The matrix's gestures
are worth their complexity because the grid *is* the desired set. Here the desired set is a
collection of independently owned intents, mostly written by something other than this UI, and the
operator's job is to read it and occasionally correct one document. Do not rebuild toggling on top of
a list.

### Large namespaces: open on what is not `ACTIVE`

A namespace populated by an adapter can hold thousands of requests, so the ledger opens
**summarised**, like the landing page (§7 of this document): the counts line, then only the paths that are not
`ACTIVE`, with the full list behind a filter. This is the same approach as `status` naming only what
is not active, applied one level down, and it is why this view scales where a grid of the same size
does not. *The filter used to include contested paths as well. Sharing is not a condition, so state
is now the only criterion.*

### ~~It doubles as the conversion planner~~ — removed

**Superseded on 2026-09-01.** The planner was built and verified end to end against a live server,
including a test that converted a scratch namespace and confirmed that the server invalidated exactly
the request the plan named. It was then deleted, because its premise was wrong, not because it did not
work.

What it was: the ledger has every input the `(UpdatedAt, id)` precedence rule needs, so switching a
namespace to `exclusive` could be previewed with no write at all. The planner showed, for example,
"1 contested path → `cam1-pin` becomes INVALID" before the button was pressed, rather than the
operator finding out afterwards.

Why it was removed: it presented a shared namespace as a state to plan an exit from, and the
correction at the start of this section is that this state is the mode working as intended. A view
whose one mutation is *convert away from what you are looking at* teaches the wrong thing about the
mode it exists to serve, and every operator arriving in the `default` namespace (which is `shared`,
and auto-created that way) would see it. Converting is still possible: it is a namespace `POST`. If
someone makes the case for previewing it again, that case belongs in `docs/open-items.md`.

Two cautions from the planner still apply wherever a conversion is offered:

- The precedence is **recency, not incumbency**, so a request with an older stamp takes a path from
  one that has been running for a week (§7b). Name the predicted loser rather than describing the
  rule.
- Any prediction is a snapshot. A write to either request between the plan and the conversion
  changes the answer.

### The same view works for an exclusive namespace

Nothing in the ledger requires `shared`. In an exclusive namespace, every refcount within the
namespace is 1 by the rule, so the claims list reduces to a plain path list. That is what
`describe path` gives, and it is still worth having next to the grid. Across namespaces, the refcount
can exceed 1 in either mode, because namespaces partition requests and not destinations (§7b). So the
"+ archive" fact that the matrix shows on a column header is the same fact this view shows on a claim
line, at a more useful level of detail. **Build one component and use it for either mode**; do not
write a second path list for the exclusive case.

### What not to build here

- **A matrix with sharing markers.** §7b rejects it for the exclusive case, and the axis argument
  above rejects it for this one.
- **A row per request.** It duplicates the path again, which is what this view exists to undo.
- **A graph.** The objection is the same as in §7a: the endpoints are queries, and a shared namespace
  has more of them, not fewer.
- **Cell-style gestures on a list.** If a click in the claims list starts changing requests, the
  ambiguity that §7a consequence 2 has to ask about comes back, with nowhere to ask it.

---

## 8. What is deliberately unavailable

Do not design a view that needs any of these, and do not add them to the API on your own initiative.

- ~~**Worker logs.**~~ **Built, in a narrower form than this entry originally refused.** Architecture
  §12.2 settles it: when a path goes to `FAILED`, the agent pushes a byte-bounded tail of the last
  failing worker's output with that transition, and the UI fetches it from
  `GET /v1/paths/{id}/logs`. This is a tail attached to a failure. It is not the general
  log-retrieval facility this entry declined. Every constraint the entry listed is still met:
  - the agent stays a client;
  - the tail is captured by the pump that already reads every line of worker output;
  - it is the log of the worker that already died, which is the case that mattered.

  The disclosure question also has an answer. Worker output does contain filesystem paths, so the
  endpoint is on the authenticated user API. `/metrics` is not authenticated.

  **An event entry carries one tail per crash loop, not one per restart.** The event carries a
  `has_log` flag, not the bytes. Fetching the tail is a separate, deliberate request and must never
  happen on a poll. If a few KiB per failure were inlined into a list that the UI polls, the cheap
  read would become expensive when things are failing, which is when the UI polls it most.
- **Rates, grain counters, latency.** These are Prometheus metrics, not API state.
  - `mxl_*` series are on each **agent's** `/metrics`, one endpoint per node. Their labels are per
    flow: direction, domain, flow_id, session, namespace, plus flow-definition and user labels.
  - `mxl_repl_*` control-plane series are on the **server's** `/metrics`, which is unauthenticated:
    requests by status, paths, sessions by status, agents leased, epoch transitions per node,
    reconcile duration, store latency.

  A UI that wants graphs needs a Prometheus behind it. It cannot scrape N agents from a browser and
  should not try.
- ~~**History or events.**~~ **Built** — see §2. "This flapped three times last hour" is what a
  coalesced entry says, rendered as `×47 over 6m` on one row. The `session.*.restarts` counter and
  the epoch-transition counter in Prometheus still answer "how often"; the event log answers "what
  happened, in order". One does not replace the other.

  **The event log is a diagnostic aid, not an audit log**, and nothing may be built on it as though
  it were one. It loses entries in three ways: the agent's queue is in memory, so an agent restart
  loses whatever is pending; a ring buffer drops its oldest entries; and a leader change leaves a gap.
  Every loss is announced in the log, which is the property that makes it usable. But it is the best
  account that two processes with bounded memory can give of what happened, not a complete record.
- **Bandwidth or capacity.** Admission control is a roadmap item. Nothing computes committed
  bandwidth today.
- **Materialised domains with no flows.** `GET /v1/nodes/{node}/domains` reports the domains the
  agent observes, joined with label records. Suppose a request has just materialised a destination
  domain and its target worker has not created anything in it yet. The agent does not observe it and
  it has no label record, so it appears in no list. Reach it through the path that targets it, which
  is where it has meaning anyway. *(This gap is smaller than it used to be. Once a flow exists in the
  domain, the agent observes it and it is listed like any other domain, because there is only one
  kind of domain now.)*

---

## 9. Running it locally, with fake data and no MXL

You do **not** need the C++ worker, libfabric, RDMA hardware or a real MXL domain to develop the UI.
The agent API is ordinary HTTP, so a fleet can be faked with `curl`: register nodes with areas,
inventory and labels through the agent API, and keep their leases alive.
`ui/app/scripts/devfleet.sh` (`npm run devfleet` in `ui/app`) is that harness: it builds the fleet
described below, applies the `nab` and `k8s` namespaces the live suites read, and starts the server
shown here on a throwaway store if nothing is listening on `$S` yet.

```bash
go build -o /tmp/mxl-replicator ./cmd/mxl-replicator

# the control plane, server role only, on sqlite
/tmp/mxl-replicator run --server \
    --server-listen 127.0.0.1:12999 \
    --server-store-sqlite-path /tmp/mxl-store.db \
    --server-heartbeat-interval 1s --server-lease-ttl 8s
```

`--server` means *server role only*. The server starts on sqlite, finishes settling after 3
heartbeats, and serves both API prefixes with no auth. To check it is up:
`curl -s localhost:12999/readyz` → `{"leader":"…","status":"ok"}`.

**Use a port other than 2283.** 2283 is the default port of a real `mxl-replicator`. A fake fleet
registers nodes through the *agent* API. If you point it at a fleet somebody else is running, it
writes fake node registrations into their store. Those registrations are durable, and there is no
API to deregister them.

The fake fleet the rest of this section and the live tests assume has five nodes. What it contains,
and why each piece is there:

| | |
|---|---|
| `studio-a` | produces, and grants `read` only — a source that can never be a destination. `media/cameras` holds Camera 1 video **and** audio, so a group-hint selector with no type matches two flows; Camera 2 is video only; one flow carries no hint at all and is idle, which is what gives `k8s/wall` a `PAUSED` path. `media/audio` holds an idle Talkback |
| `studio-b` | the same shape, with its own Camera 1 — so a fan-in request over both studios is one intent with two sources, which is the arrangement §7a has to draw as a rectangle |
| `edge-01` | `media` read-only, `fast` and `bulk` read-write — two writable areas, **and** an observed domain `media/local`, so a node being both ends of a path is represented |
| `edge-02`, `archive-01` | one writable area each, the common case |
| labels | `role`/`name`/`studio` applied to every source domain, so `{labels: {role: cameras}}` selectors have something to match |
| `Studio A:Talkback` | seeded idle, so routing it produces a `PAUSED` path — the state a UI most often gets wrong |

The fake fleet has to keep running, because leases need renewing. When a node's lease expires,
every path touching that node is frozen. That is correct behaviour, but confusing if you did not
mean to cause it. When a heartbeat response says `reregister`, register the nodes again; then the
fake fleet keeps working after a server restart or `rm /tmp/mxl-store.db` without being restarted.

Paths reach `ESTABLISHING` and stop there, because nothing runs a worker. This is a useful fixture
rather than a limitation: it is the state an operator watches while something comes up.

**To drive the rest of the state machine**, act as the agent for each node:

1. Read the node's `GET /agent/v1/{node}/assignments`.
2. Post `/agent/v1/{node}/status` snapshots back with `state: "ready"` and an epoch. The epoch can be
   any string; only the peer agent verifies it.
3. Report the destination flow in that node's inventory with `"producing": true, "replicated": true`.

Doing this for some sessions and not others gets a request to `PARTIAL`. That is worth building,
because §4 of this document stresses `PARTIAL` more than any other state and it is the hardest to
picture. `internal/e2e/` and `internal/server/*_test.go` are the authoritative examples of every one
of those bodies. Copy from them, not from any document.

For an `INVALID` fixture, dry-run a request that names an area that does not exist:

```
{"code":"invalid_request",
 "message":"node \"edge-01\" advertises no area \"nope\", it has \"bulk\" (writable), \"fast\" (writable), \"media\" (read-only)",
 "details":{"reason_code":"unknown_area"}}
```

Note that on this 400 response the `reason_code` is under `details`, while on a `Request` object it
is `status.reason_code`. The same information comes in two shapes.

For a `namespace_overlap` fixture: create a namespace with `paths` set to `exclusive`, POST one
request, then POST a second with the same source and destination. The second comes back `INVALID`,
and it also reports the first request's path as its own. That is trap 14, and it is worth seeing once.

**For a `shared` fixture — the one §7c is written against.** This is the mirror image of the
previous fixture: in a `shared` namespace the same overlap is not a conflict. It produces one path
with two claims. Verified on 2026-09-01.

```bash
S=http://127.0.0.1:12999
curl -s -XPOST $S/v1/namespaces -d '{"name":"k8s","paths":"shared","description":"one request per pod"}'
r() { curl -s -XPOST "$S/v1/namespaces/k8s/requests" -d "$1" >/dev/null; }

# takes the whole domain …
r '{"name":"wall","sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
    "select":{"all":true}}],"destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}}]}'
# … and pins one flow inside it: one path, two claims
r '{"name":"cam1-pin","sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
    "select":{"flow":"5592a23b-0974-45bb-9388-89ea81c42537"}}],
    "destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}}]}'
# a second destination node, so the claims list has two groups
r '{"name":"pod-abc12","sources":[{"node":"studio-b","domain":{"labels":{"role":"cameras"}},
    "select":{"group_hint":{"name":"Studio B:Camera 1"}}}],
    "destinations":[{"node":"edge-01","domain":{"area":"fast","elements":["ingest"]}},
                    {"node":"edge-02","domain":{"area":"fast","elements":["ingest"]}}]}'
# fully parked, so the request pane has a DISABLED row with no claims to render it
r '{"name":"pod-def34","sources":[{"node":"studio-a","domain":{"name":{"area":"media","elements":["cameras"]}},
    "select":{"group_hint":{"name":"Studio A:Camera 2"}}}],
    "destinations":[{"node":"archive-01","domain":{"area":"bulk","elements":["capture"]},"disabled":true}]}'
```

`GET /v1/paths` then reports six paths. One of them carries
`"requests": ["k8s/cam1-pin", "k8s/wall"]`. That one path is the case §7c exists to render
correctly: `cam1-pin` holds one path and holds none of it alone, so deleting `cam1-pin` stops
nothing. The matrix has no way to show that.

**You do not need a browser to test the UI itself.** `ui/app`'s live tests (`npm run test:live`,
see `ui/app/README.md`) mount the real components in jsdom and drive them against the live server,
so the DOM, fetch, reconciler and store are all real.

It catches more than you would expect, and the bugs it finds are of one consistent kind. Examples:

- a class-name collision;
- a regression where ungrouped items no longer sorted last;
- a header-resize bug;
- a dialog list that still showed the *previous* node's
  domains, two per-node reads that completed out of order, and a domain selection carried across a
  reopen onto a node that does not have that domain.

No unit test of any single component would show these, because each one is the page's behaviour
against a real sequence of reads.

**The full setup**, if you want real media moving: `make all` builds the worker. Then the README's
quick start runs both roles on one host over `tcp` on loopback, with `mxl-mock-src`/`mxl-mock-sink`
producing into a domain. You should not need it.

---

## 10. If the code lands in this repository

Match the existing code. Its style is unusual and consistent: doc comments record the *reasoning*
and the rejected alternative, not only the behaviour, and decisions that were open are recorded in
the place where someone will run into them. `internal/api/request.go` and
`internal/api/domainselector.go` are good samples. If you write Go, put it under `internal/`, since
nothing here is a library for third parties. Add a dependency only in the change that adds the code
that needs it.

`rewrite.md`, `rewrite-plan.md` and `rewrite.v0.md` are local-only planning documents. They are
excluded via `.git/info/exclude`, not `.gitignore`. `rewrite.md` is superseded by
`docs/architecture.md`; do not read it for current truth. Decide whether a planning file belongs in
the tree before committing, not by accident through `git add -A`.

Two verification habits are worth copying. Both show up in the plan, and both caught real bugs that
unit tests missed:

- Check assertions against the running binary, not against the document.
- When a live run contradicts a document, fix the document in the same change.

For example, this document's §3 says the wire format refuses a string domain selector because a live
run refused it, while architecture §9.1's example body used the string form until it was corrected.

---

## 11. Decisions to settle before writing code

*Settled:* **the UI is always same-origin with the API.** It is served either by the server process
or from behind a proxy that fronts both. There is no CORS, URLs are relative, and development uses a
dev-server proxy (§6 of this document).

*Settled:* **the primary screen is a routing matrix.** Rows are sources, columns are destinations,
cells are the paths a source/destination pairing expands to, and **a request is a rectangle over
those cells** (§7a). Rows and columns are not real objects: a row is a pair of selectors, a column
is a domain that may not exist yet, and the server materialises them into paths. A node graph is the
topology *view*, not the editor.

*Settled:* **the matrix requires a namespace whose `paths` mode is `exclusive`** (§7b). Otherwise two
requests can hold one path, and the grid cannot draw that correctly. The API's default is `shared`,
so the UI must choose `exclusive` explicitly every time it creates a namespace, and must check the
mode every time the user switches namespace. A `shared` namespace does not get a greyed-out matrix.
It gets **the ledger** (§7c), a path-first view whose object is a claim rather than an intent. The
UI must not prompt the user to convert the namespace to `exclusive`; §7c originally included that
prompt and had to remove it.

*Settled, and now built:* **`disabled` on a destination** (§3 and §7a of this document, architecture §9.1). This field
decides whether the matrix keeps what the user authored or only shows what is live. Without it, the
rows and columns are derived from routes that are currently on, so switching a route off deletes its
row and column. Before the field existed, the only workaround was to remember switched-off routes for
the browser session, which shows how large the gap was. The API now accepts and returns the field, the
reconciler skips parked legs, and a request with every destination parked reports `DISABLED`. **Use
it from the first version of the grid; do not retrofit it.** A renderer written on the assumption
that the rows and columns can disappear cannot later be made to keep what the user authored.

1. ~~**How does the browser authenticate** when a shared token is configured?~~ **Decided** (§6 of this document). The
   recommendation is still a proxy that injects the token, and that is unchanged. Where nothing
   injects it, a 401 opens a prompt and the token is stored in `localStorage`. The prompt is
   triggered by the 401, not by the absence of a stored token, so the recommended deployment never
   shows it.
2. **Apply on click, or stage and apply?** §7a. Staging has an advantage that was not obvious in
   advance. With a staged set, the preview can dry-run the whole batch and report the real outcomes
   and how many paths are affected before anything changes, and that is what removes every
   confirmation dialog from the interface. The advantage is larger now that one click can light
   several cells of a rectangle. It is still worth checking against how operators actually work: on a
   broadcast router, pressing a take button is a habit.
3. **Stack.** Choose a stack on its own merits. Hand-written rendering in plain JS stops being worth
   it at around 2000 lines, because every poll re-renders the whole grid; that is fine at small
   scale and is the first thing to cause problems. `ui/app/` uses Vue 3 and TypeScript.
4. **Read-only first, or mutating from the start?** A read-only matrix already provides most of the
   value: the whole desired set and its live state on one screen. It needs neither decision 1 nor
   decision 2. That is a strong argument for shipping it first, and a stronger one than before,
   because the rectangle model has real editing subtleties (§7a, consequences 1 and 2) that a
   read-only view does not have to solve.
5. **Does the UI author fan-in, or only render it?** New, and worth deciding deliberately. Rendering
   it is mandatory: the CLI and the Kubernetes adapter will produce multi-source requests whether or
   not the UI can create them. Authoring it costs the source-list editing described in §7a. Rendering
   first and authoring later is a reasonable split. Assuming every request has one source is not.

Three smaller items can wait, but should be raised with the server side rather than worked around:

- an ETag or revision cursor, so the matrix's polling is cheap (§2 and §7a of this document);
- the negotiated provider on `PathStatus`, so a cell can show it before apply (§7a);
- `GET /v1/nodes/{node}`, which architecture §9.1 documents and the mux does not serve.

*All three now have a disposition in `docs/open-items.md`:*

- The negotiated provider will be added **as part of the UI work** (open-items §2.10) rather than
  requested separately, since the cell preview is what needs it and `Compute` already has the value.
- The ETag is worth asking for, but **narrowly**, and not on `/v1/paths` (open-items §2.9).
- `GET /v1/nodes/{node}` has no owner and is the only one of the three that nothing is waiting on
  (open-items §5).
