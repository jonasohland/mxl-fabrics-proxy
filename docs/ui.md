# Handoff: a web UI for `mxl-replicator`

For the agent implementing the UI. `docs/architecture.md` is the specification and
`docs/open-items.md` is the list of things it has not closed; this document is neither. It is the
subset of both that a UI needs, plus the things a UI gets wrong that neither document warns about
because neither was written for one.

Section references in the form §N point at `docs/architecture.md`. Everything asserted here about
the API was checked on **2026-09-01** against a server built from this tree and driven with the
fake fleet of §9 — including the response bodies quoted, which are real ones. Where something is
read out of the source rather than exercised, it says so.

*An earlier version of this document was written against `rewrite.md` on 2026-08-28.* That file is
still in the tree and is a frozen predecessor of `docs/architecture.md`; the section numbers are
load-bearing and unchanged, so §7.2 is still §7.2, but the **content** behind several of them
moved. Three changes since then rewrote parts of this document rather than editing them, and each
is marked where it lands: **areas replaced output roots** (§10.6), **a request grew a list of
sources** (§9.1), and **a namespace became a real object** (§9.3).

The control plane is complete and shipped. Nothing here asks you to change it. Where the UI needs
something the server does not have, it is called out as such rather than assumed.

**The UI described here has since been built** — `ui/app/`, Vue 3 and TypeScript, with the landing
page, the matrix and its staged gestures, the ledger, the source, destination, split and label
editors, the unrouted-sources strip and the topology view. `ui-plan.md` is the record of what was
built and what building it cost; this document stays the design argument. Four things it asserted turned out to be
wrong or unbuildable as written, and are corrected **where they land**, in the manner of the three
changes above:

- **`status.sources[]` is not `omitempty`** — always present, always the full list (§4, and §7c,
  where the claim was load-bearing).
- **The reserved label names are not the ones listed** — the enforced set has `format` and
  `media_type` and no `domain_name` (§3).
- **A path held by several requests in a `shared` namespace is not "contested"** — it is refcounting
  working, and the conversion planner built on the other reading was removed after it was built and
  verified (§7c).
- **"Unrouted" cannot mean *no request selects it*** — that is selector evaluation in the browser,
  which this document forbids two sections earlier. It is computed from the path list instead, and
  the one behavioural difference is named rather than smoothed over (§7a, *Unrouted sources*).

Everything else held, including the parts that were argued rather than tested — the rectangle, the
virtual axes, staging over apply-on-click.

**The newest thing in it is `disabled` on a destination** (§9.1), with the `DISABLED` aggregate state
that follows from it (§11). It is here rather than in a footnote because the matrix is what it is for
— without it, a route that is switched off is a route that does not exist, and §7a's axes cannot show
a board an operator laid out, only the subset of it that is currently running. It is **built**, with
the reconciler, CLI and manifest support the rest of this document assumes; the prototype in
`ui/prototype/` predates it and still retains its axes client-side, which is the gap §7a describes.

**There is a working prototype of §7a and §7b in `ui/prototype/`** — plain HTML, CSS and
JavaScript, no build step, no runtime dependencies, served by nginx with the API proxied behind one
origin. It implements the matrix, the namespace picker with its mode check, the two editors,
staged commits and the dry-run preview, and it comes with `devfleet.sh`, which stands up a
five-node fake fleet over the agent API with no MXL, libfabric or hardware, and `verify.mjs`,
which drives the shipped page headlessly against a live server. It has been brought up
to date with everything in this document: rows are sources, a request is a rectangle, and a
`shared` namespace renders as the ledger of §7c rather than as a greyed-out grid.

Read `ui/prototype/README.md` before §7a: several of the rules there were learned by building it
and are recorded with the bug that produced them. It is a prototype of the *interaction*,
deliberately not a foundation — nothing in it is asking to be kept. *It is now superseded for
everything on screen by `ui/app/`, and is kept only until someone decides it is not worth keeping
(`ui-plan.md` §8).*

---

## 0. Read this, skip that

| Read | Why |
|---|---|
| §3, *Identity and terminology* | Six nouns. The UI is a rendering of them and it will be wrong if they blur. ~30 lines. |
| §4, *State model* | Desired / observed / derived. It explains why some things are editable and most are not. |
| §9.1, *User API* | The contract you are building against, including the fan-in subsection — it is what §7a is built on. |
| §9.3, *Namespaces* | Short, and the matrix does not work without the rule it defines. |
| §11 + §11.1, *Status and failure semantics* | Eight states — nine with `DISABLED` — and what an operator decides differently on seeing each. |
| §10.6, *Domains, areas and grants* | The single most constrained part of the create form. |
| §10.7, *Domain labels* | Where a source's domain selector gets its meaning, and it is a mutation the UI has to offer. |
| `README.md` §Manifests, §Commands | The operator's current mental model. The UI must not contradict it. It has been rewritten for areas and for `sources:` and is trustworthy again. |

Skip unless something forces you in: §5 (epochs and pairing), §6 (agent internals, though §6.3 is
worth two minutes — it is why a worker can sit in `starting` for a minute and that is not a fault),
§7.3–7.4, §8 (storage), §14–15 (the worker). They are the machinery under the API and none of it
surfaces. §12 (observability) is worth ten minutes only when you get to the "where do rate graphs
come from" question in §8 below.

Ground truth for wire types is `internal/api/` — thirteen stdlib-only files with doc comments that
are better than any summary of them. `request.go`, `status.go`, `caps.go`, `domain.go`,
`domainselector.go`, `domainlabels.go`, `selector.go`, `inventory.go`, `namespace.go`, `wire.go`,
`routes.go`. Read those rather than trusting the shapes reproduced here.

---

## 1. The model, in one page

```
  Request  ──expands to──▶  Path  ──realised by──▶  Session  ──▶  2 workers
 (durable          S×D×F   (derived,        0..1   (ephemeral)
  intent)                   refcounted)
```

**Node** — one host, one agent, an operator-assigned unique name. Registration is *durable* and
survives the agent being down; a *liveness lease* is separate and TTL'd. `node.live` is the lease.
A node that is registered but not leased is information, not an alarm (§4.2).

**Area** — a directory an operator designated on a node as somewhere MXL domains live, with a name
and two independent grants: `read` (domains here may be discovered and observed) and `write`
(replication may create them). Both default false and neither implies the other, so a node with no
readable area offers no sources and a node with no writable area accepts no destinations. The
grants are the whole of this project's authority over that node's filesystem, and they are the
reason the API is not a remote arbitrary-filesystem-write primitive (§10.6, §13). The UI reads them
from `GET /v1/nodes` → `capabilities.areas[]`.

**Domain** — a directory inside an area, holding flows. **One kind**, whichever direction this
project uses it in: a place, not a channel. Its fleet-wide identity is `<area>/<elements>` —
`media/cameras`, `fast/studio-a/cam1` — and that is its identity for life. On the wire it is
`{"area": "fast", "elements": ["studio-a", "cam1"]}`; the rendered string appears wherever a domain
has to be one token (a metric label, a path's source address, an error message).

*This supersedes input and output domains being separate concepts with separate identities — an
input domain named by its absolute path, an output domain named as elements under an output root.*
Both spellings are gone, and with them the whole "two kinds" section this document used to open
with. What survives from it is the operational half: the API still never creates a domain as an act
of its own, a domain a request materialises still has no lifecycle beyond the paths that target it,
and **there is still no create or delete API for a domain, and there should not appear to be one in
the UI.** What changed is that a domain replication writes into is discovered, observed and
listed like any other, so it is not a different noun.

**Domain label** — a key/value pair an operator attaches to `(node, domain)` through the API.
Annotation, never identity: a relabel never moves a path, a session or a metric series. Labels are
what a request's source selector matches, so **writing one is a mutation that can start and stop
media**, one level of indirection away from a request (§10.7). Treat it with the same ceremony.

**Flow** — a UUID identifying the *media*, not a location. After replication the same flow ID
exists on both nodes; that is the point. A flow is addressed by `(node, domain, flow-id)` and the
domain component is mandatory — the same ID can legitimately exist twice on one host. **Never key a
UI list on flow ID alone.**

**Request** — durable user intent, the only thing a user writes. A **list of sources** and a **list
of destinations**, in a namespace, identified by `(namespace, name)`, which is its ID and its
idempotency key. A request is **never cancelled because its session is failing** (§11) — the UI
must not offer or imply otherwise. A destination may be `disabled`, which parks that leg without
removing it: the entry stays in the spec, expands to nothing, and is the model's only spelling of
*off* (§9.1, and §7a for why the matrix needs it).

**Namespace** — a partition of requests and a first-class object with a name, a `paths` mode and a
description. It scopes request names, scopes `--prune`, and carries whether requests inside it may
share a path. It does **not** partition nodes, domains or destinations (§9.3, §7b).

**Path** — the deduplicated edge `(src node, src domain, flow) → (dst node, dst domain)`. Derived,
recomputed every reconcile, refcounted: N requests whose selectors expand onto the same edge share
one path and one worker pair. `path.requests[]` *is* the refcount, and it is the answer to "what
happens if I cancel this request".

**Session** — the concrete worker pair realising a path. Ephemeral, re-established whenever either
end restarts. 1:1 with a path in practice but **a separate layer on purpose**: a path outlives any
particular session. The CLI keeps `describe path` and `describe session` apart for exactly this
reason and the UI should keep the same seam — collapsing them quietly asserts that a path dies when
its workers do, which is the opposite of what the design guarantees.

**Cardinality that trips people up.** A request's expansion is the **cross product**: every source's
matched flows against every destination. Two sources matching 2 and 1 flows, across 3 destinations,
is **9 paths**. Its status is an aggregate over them, `status.counts` is the breakdown, and
`status.sources[]` is the same breakdown sliced by source — which is the one an operator reads when
a request spans several studios. The single-flow, single-destination case is a set of size one, not
a different shape. Model it as a set from the first line of code; retrofitting this is the expensive
version.

---

## 2. The API you have

Base `/v1`. Everything is JSON. The other prefix, `/agent/v1`, is the **privileged** surface —
anything that can call it can claim to be a node, inject fabricated inventory and read other nodes'
`target_info` (RDMA rkeys). **The UI must never call it** except in the local seeding harness of §9.

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

Unauthenticated, outside both prefixes: `GET /healthz`, `GET /readyz`, `GET /metrics`.

### What is not there

Assume none of these and design around their absence rather than discovering it:

- **No `GET /v1/nodes/{node}`.** §9.1 lists it; the mux does not register it and it returns **404**
  — verified. Fetch the list and filter, which is what `describe node` does. If it appears later
  nothing breaks; do not build on it now.
- **No `/v1/sessions`.** Sessions are reached through paths, inline on `GET /v1/paths`
  (`path.session`). That is deliberate — a session has no identity apart from the path it realises.
- **No `/v1/paths/{id}`.** Same: fetch the list, match `id` exactly.
- **No watch, stream, SSE or websocket on the user API.** The revision-cursor long poll exists only
  on the agent API and only for assignments. **The UI polls.**
- **No ETag, `If-None-Match` or revision cursor on any read.** There is no cheap "has anything
  changed" check today. See the cost note below.
- **No pagination, no sort, no filter** beyond `?namespace=` on requests and the five query
  parameters on `/v1/flows`.
- **No batch create.** Each request is its own POST.
- **No CORS, and none is wanted.** An `OPTIONS` preflight to `/v1/paths` returns
  `405 Method Not Allowed` and a cross-origin `GET` carries no `Access-Control-*` header. **The UI
  is always same-origin with the API** (§6), so this is a settled non-issue in production and a
  dev-server proxy problem in development — not a reason to add CORS middleware.
- ~~**No event log or history.**~~ **There is one now**, and this entry said the opposite for long
  enough to be worth striking rather than deleting: architecture §12.1 and §12.2 are built, and
  "this path failed twice last hour" *is* answerable. A bounded, coalescing ring per object, read at
  `GET /v1/paths/{id}/events`, `GET /v1/namespaces/{ns}/requests/{name}/events` and
  `GET /v1/nodes/{node}/events`, with the last failing worker's output at
  `GET /v1/paths/{id}/logs`. §8's "History or events" bullet went the same way.

  **They are the only reads on this API that are not O(fleet).** An event read is one `Get` on one
  key and does not run `Compute`, because a ring is a record of what already happened and there is
  nothing to recompute — the exception to everything the next section says, and the one endpoint
  class where polling faster is affordable. The request view is the exception to the exception: it
  merges its paths' rings, and which paths those are is derived.

  Five things a renderer must get right, none of which is visible in the JSON, are in
  `docs/open-items.md` §2.11; `components/EventLog.vue` and `model/events.ts` discharge them and
  each one is pinned by a test. Two are worth naming here because they are wrong in the tempting
  direction: a coalesced entry is **one row, not `count` rows**, and `severity` is **not** the state
  vocabulary — designed behaviour never warns, so a `PAUSED` row is routinely `info` and colouring
  from `state` rebuilds the board of false faults §4 spends two paragraphs avoiding.

  What is still not there is a **fleet-wide** stream. The rings are per-object by design and the
  fleet ring is merged into object reads rather than served on its own, so there is no endpoint for
  one and constructing it client-side means a fan-out read over every path — which costs more than
  the full reconciles below. Health names what is not active and every row links to an object whose
  log is one click away; if that stops being enough, the answer is a server endpoint.

### Every read costs a full reconcile

Each user-API GET does one `state.Load` — a single `List("")` over the whole store — and then runs
`reconcile.Compute` over the result (`internal/server/userapi.go`). That is deliberate and it is a
good property: what the UI renders and what the fleet is being told to do are computed by the same
function, so they cannot drift, and a follower replica renders exactly what the leader is doing. But
it means a read is O(fleet), not O(response).

Consequences for the UI: poll on a **single timer** that fetches the endpoints you need together,
not one timer per component. Something in the 2–5 s range is appropriate for a control plane whose
own heartbeat defaults to 5 s and whose settling window is 3 heartbeats. Sub-second polling buys
nothing — the underlying state is not changing faster than the reconciler runs — and it multiplies a
full store read by your tab count. If a live-feeling UI matters more than that, an ETag keyed on the
store revision is the right server-side addition, and it is small; raise it rather than compensating
with a faster poll.

*This document names `/v1/paths` as where to put it, here and in §7a and §11, and that is the one
endpoint it cannot be sound on* — `reconcile.Compute` takes a clock and uses it for idle teardown and
session ages, so two reads at one revision can legitimately differ and a revision-keyed `304` would
freeze exactly the transitions the read exists to report. The sound targets are `/v1/nodes` and
`/v1/namespaces`, which are pure store state and are two of the workspace's four reads.
`docs/open-items.md` §2.9 carries the full argument, including why a quiet fleet has a quiet revision
at all.

---

## 3. Creating and cancelling

`POST /v1/namespaces/{ns}/requests` is **create-or-update keyed on `name` within `{ns}`**. There is
no create-only mode and no 409 on an existing name — a POST with a name that exists and a different
spec *updates* it. If the UI has a "new request" form and an "edit request" form, they are the same
call, and the UI is responsible for not letting "new" silently overwrite something. Fetch-then-check,
or dry-run first and show the `X-Mxl-Outcome` header, which is the honest answer:

| `X-Mxl-Outcome` | Meaning |
|---|---|
| `created` | did not exist |
| `updated` | existed, spec differed, rewritten |
| `unchanged` | existed with exactly this spec, **nothing was written** |

The status code cannot tell you this — an unchanged apply is still a 200, verified — and the
response body echoes the spec either way, so comparing it tells you only that the server agreed.
Read the header.

`?dry_run=true` runs the identical path and skips only the write. **Use it.** It validates against
the real fleet, including conflicts that are only visible across requests (two sources into one
destination flow, namespace overlaps, loops), which no client-side check can see. The right shape
for a create form is: debounce, dry-run, render the server's own reason. Do not reimplement §7.2
validation in the browser — reproduce the *structural* rules below for immediate feedback if you
like, but let the server be the authority on everything else.

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

`namespace` is a **real property, not a label**. It may be omitted in the body, since the URL
carries it; if present it must agree, and a disagreement is refused with
`body names namespace "other" but the URL names "nab"` — verified.

### Both ends are lists

*This is the change that rewrote §7a.* A request fans in as well as out. `sources` is always a
list, with no singular `source:` beside it, and every stored request or hand-written manifest using
the old singular form is invalid — that cost is recorded rather than mitigated (§9.1, §16).

A source's **node is pinned**; only its domain and its flows are selected. So one source expands
over the domains of one node, and what a source *list* adds is the thing no selector can express:
several nodes feeding one destination. "Every camera in studio A, B and C onto the ingest wall" is
one intent, one name, one lifecycle and one delete.

Two consequences the UI has to carry, both discharged in §9.1 rather than argued away:

- **A request's paths no longer share a producer**, so disagreement among them is the ordinary
  state and the aggregate needed a word for it. That word is `PARTIAL` (§4).
- **Two sources can collide on one destination flow**, which fan-out could never produce. Decidable
  from the body — two sources pinning the same UUID into a shared destination — it is refused at
  POST as `duplicate_source_flow`. Undecidable, because one or both sides are selectors and the
  collision arrives with a producer later, it is `flow_conflict` on the path.

### Three tagged unions

**The flow selector**, `sources[].select`. Exactly one of `flow`, `group_hint` and `all`. Two set is
a parse error; an unknown kind is a parse error in *both* directions. This is a deliberate exception
to the API's "ignore unknown keys" rule, because ignoring a selector kind would silently *widen*
what gets replicated. In the UI that means a radio choice, never two fields that are both submitted.

- `{"flow": "<uuid>"}` — a bare string, not an object.
- `{"group_hint": {"name": "...", "type": "..."}}` — `type` optional; omitting it selects every flow
  sharing the name, which is how a camera's video and audio travel together. Offer this
  prominently; it is the selector operators actually want.
- `{"all": true}` — every flow in the selected domain. **Not the zero value and not an omission:**
  an absent `select` is an error on the wire. The manifest may default it, the API may not.

**The domain selector**, `sources[].domain`. Exactly one of `name` and `labels`.

- `{"name": {"area": "media", "elements": ["cameras"]}}` addresses one domain directly. **It is a
  structured `Domain`, not the string `"media/cameras"`** — sending the string is a decode error,
  verified: `source.domain: json: cannot unmarshal string into Go struct field plain.name of type
  api.Domain`. The rendered form is the manifest's spelling and the CLI is the only thing that parses
  it. *(§9.1's example body shows the string form; the wire does not accept it. Trust the type.)*
- `{"labels": {"role": "cameras"}}` matches every domain on that node carrying all of those keys
  with exactly those values. Equality, ANDed, **never empty** — an empty map is refused, because
  matching every domain on a node would be reachable by omission rather than by intent.

  A label selector is how one row can cover several source domains, and it is also how a broad
  selector meets flows this project is itself writing. Those are **excluded, not replicated**, and
  the request reports which (below).

*The `{"path": …}` direct form is gone with path-named domains.* One identity grammar covers both
what a scan found and what replication materialised, which is what makes a chain `A→B→C` writable
at all: the second hop names the domain the first hop created.

**The destination domain**, `destinations[].domain`: `{"area": "fast", "elements": ["studio-a",
"cam1"]}` materialises `<fast>/studio-a/cam1` and renders as `fast/studio-a/cam1`. **This is the
single most important invariant in the design** (§13): a destination is always a name inside an
area the operator granted `write` on, never a path from the API, and that is what stops this API
being a remote arbitrary-filesystem-write primitive on every node in the fleet. It holds regardless
of what authentication is configured.

The rule underneath is that **nothing outside the CLI's manifest parser turns a domain string into
an area and elements**. A UI text box accepting `fast/studio-a/cam1` and splitting it makes the UI
the second parser, which is exactly what the invariant forbids. Two acceptable shapes:

- **An area picker plus element chips** — the honest rendering of what the field is, and it makes
  the grants visible: the picker lists only areas the node grants `write` on.
- **A single field split on `/`**, if the first segment is validated as an area the node advertises
  and grants writing on, the rest against `ValidDomainElements`, and every submission is dry-run
  first. If you take this route, mirror `internal/api/domain.go` and say in a comment that you are
  aware you are the second parser and why it is acceptable here.

Per element: ASCII letters, digits, `-`, `_`, `.`; not starting with `.` or `-`; not `.` or `..`; at
most 64 bytes. At most 8 elements, and the whole rendered `<area>/<elements>` at most 255 bytes. The
area name follows the same character rule.

**A node advertising no writable area is not a destination at all** — filter or disable it, and say
why, because it is the first thing to check behind a refused request. The two codes are distinct and
both name what to fix:

```
{"code":"invalid_request",
 "message":"node \"studio-b\" advertises area \"media\" but does not grant writing on it",
 "details":{"reason_code":"area_not_writable"}}

{"code":"invalid_request",
 "message":"node \"edge-01\" advertises no area \"nope\", it has \"bulk\" (writable), \"fast\" (writable), \"media\" (read-only)",
 "details":{"reason_code":"unknown_area"}}
```

*This supersedes `no_output_root` / `unknown_output_root` / `ambiguous_output_root`, and the whole
notion of a `root:` field that could be omitted when a node advertised exactly one.* There is
nothing to omit: the area is the first segment of the domain's name, so leaving it out is leaving
out half the name. **The root picker is gone from the create form** and an area picker is not its
replacement — it is part of naming the domain, and the UI should render it that way. `area.path` is
still advertised (diagnostics only, may be absent — guard it), so the field can show
`/dev/shm/mxl/studio-a/cam1` under the name as the operator types, which for an otherwise abstract
name is the strongest affordance available.

One collision survives and it is narrower than it was: `domain_name_in_use`, which now means the
destination domain **nests** with one another path is already materialising on that node —
`fast/studio-a` against `fast/studio-a/cam1`. Two domains sharing a name under different areas is
unconstructible, because `fast/ingest` and `bulk/ingest` are two different strings. *If you have
read the old version of this document: its "a root is not a namespace" warning has inverted, and the
new rule is the intuitive one.*

### Provider

`"provider"` is scalar, array, or absent — and it round-trips in the form it was written:

```
"verbs"            pinned: verbs or the request fails
["verbs", "tcp"]   prefer verbs, tcp acceptable
absent             the server's order, default EFA > Verbs > TCP > SHM
```

**A pin is honoured or the request fails; it is never substituted** (§10.4). Do not build a UI
affordance that reads as "fall back automatically" — the whole point is that landing on tcp when
verbs was asked for is a performance cliff whose symptom looks like a source problem. The
per-destination `provider` **overrides** the request-level pin rather than intersecting with it,
because "verbs here, tcp there" is an ordinary request, not a conflict. There is deliberately no
mirror of it on the source side: a provider is unsatisfiable per *pairing*, and one override on one
side already says everything a pin needs to say about a pairing.

### Labels are identity, not annotation

They ride into worker metrics *and*, together with the namespace, they scope `apply --prune`. The
server validates them at request time: valid Prometheus label names (letters, digits, underscore;
not starting with a digit or `__`), key ≤ 63 bytes, value ≤ 253, and not one of the reserved names
the project sets itself. Removing a label from a request changes what can cancel it — worth a word
in the UI if you offer label editing.

*The reserved list here used to read `direction`, `domain`, `domain_name`, `flow_id`, `session`,
`namespace`, `quantile`, and that is not what is enforced.* `validateLabels` takes
`metrics.WorkerLabelNames()` plus `quantile`, which is **`direction`, `domain`, `flow_id`,
`session`, `namespace`, `format`, `media_type`, `quantile`** — two more than the old list and
without `domain_name`, so a label called `domain_name` is accepted today even though §10.7 renders
the `name` domain label as exactly that metric dimension. Found by mirroring the rule in the UI on
2026-09-01. Mirror the **code**; the discrepancy is a server-side decision and not one a client
should paper over, and it applies to domain labels too — `validateDomainLabels` shares the same
function.

**No key means anything special to the server.** `namespace` was briefly one and is now a real
property, so request labels are uniformly tagging. Do not confuse these with *domain* labels, which
are a different record on a different endpoint with a different owner (§10.7): request labels
annotate intent, domain labels annotate a place and are what a source selector matches.

### Delete

`DELETE /v1/namespaces/{ns}/requests/{name}` cancels the intent. It does **not** necessarily stop
media: the path survives while another request still references it. Any confirmation should say
what will actually happen, which the UI can compute — for each path the request owns, is
`path.requests.length > 1`? If so, that leg keeps running.

`404` on a request that is not there, verified. The CLI treats that as success because deleting what
a manifest names is idempotent by nature; a UI acting on a row the user can see should probably
treat it as "already gone, refresh" rather than an error dialog.

### Parking, which is not deleting

`disabled` on a destination entry (§9.1) is the model's spelling of *off*, and it is the one thing in
this document the server does not do yet:

```json
{ "node": "edge-02", "domain": {"area": "fast", "elements": ["ingest"]}, "disabled": true }
```

The effective spec is every source against every **enabled** destination. A parked entry expands to
no pairing, no path and no session, and the request reports `DISABLED` when nothing is left enabled
(§4). Four things a UI has to hold on to, and all four make it *easier* than the delete it replaces:

- **It is a spec edit, so it is the same POST as everything else** — take the stored spec, flip one
  boolean, dry-run it, apply. No new endpoint, no new verb, no new failure mode.
- **It stops media.** Parking is a cancellation of those legs with the text kept, so it needs the
  cancellation preview: for each path, is `path.requests.length > 1`. Do not let the reversibility of
  the flag make the click feel reversible — the flag comes back, the session does not.
- **It is not a soft delete.** The request still exists, still holds its name against the namespace,
  and is still pruned by a manifest that stops naming it. `DELETE` is unchanged and still means what
  it means.
- **An `apply` from a manifest that omits the flag turns the leg back on** (§9.1). If the UI parks a
  leg and somebody's CI applies the file that names its request, it comes back. That is the file
  being authoritative, which is correct and is also the single most surprising thing about this
  feature — worth saying in the interface, not just here.

The reason it exists at all is §7a: without it the axes of the matrix are derived from routes that
are currently on, so switching a route off deletes the row and the column it lived on and the
operator's board rearranges itself under them.

### Labelling a domain is the second mutation

`POST /v1/nodes/{node}/domains` writes the labels on one `(node, domain)`, and it takes
`?dry_run=true` on exactly the argument requests do: a label joins or removes a domain from a
request's expansion, so it starts and stops media one level of indirection away, which makes it
*easier* to do by accident rather than harder.

Two body shapes, and the difference is the ownership rule, not a convenience:

```json
{"node": "studio-a", "domain": {"area":"media","elements":["cameras"]},
 "apply": {"role": "cameras", "name": "cameras"}}

{"node": "studio-a", "domain": {"area":"media","elements":["cameras"]},
 "patch": {"set": {"name": "cameras"}, "remove": ["role"]}}
```

An **apply** owns the keys it declares: it sets them, removes the ones it declared last time and no
longer does, and leaves every other key alone — `kubectl apply`'s three-way merge, with the
declared set carried on the record as `declared`. A **patch** sets and removes exactly what it
names and merges against nothing. A UI editing one domain's labels interactively wants `patch`: it
has no declared set of its own to own, and a read-modify-write with `apply` would silently adopt —
then later delete — keys someone else's manifest declared.

The response is the resulting record plus its blast radius, and the dry run is where the UI should
be reading it. Verified, removing `role` from a labelled domain:

```json
{"node": "studio-b", "domain": {"area": "media", "elements": ["cameras"]},
 "labels": {"name": "cameras"}, "declared": ["name", "role"],
 "stopped": [ { "id": "b2adff89…", "source": {…}, "destination": {…},
                "state": "ESTABLISHING", "requests": ["nab/wall"], "session": {…} } ]}
```

`stopped[]` and `started[]` are **full `Path` objects**, so the UI can say which requests lose which
legs and whether anything else still holds them, without a second read.

---

## 4. Status: what to render and what it means

Eight states (§11), nine once `DISABLED` lands. Seven describe one thing; the others do not — one
describes disagreement among many and one describes a spec rather than a fleet.
Worst-first, which is the order the CLI sorts by and the order the UI should:

| State | Meaning | Is it a problem? |
|---|---|---|
| `INVALID` | Needs user action. Never resolves by itself. | Yes — and it is the only one a user can fix. |
| `FAILED` | Repeated permanent-looking failure, or a fabric that stopped being viable. Still retried. | Yes. |
| `DEGRADED` | Established but flapping — restarts over a threshold in a window. | Yes. |
| `WAITING` | The flow is not visible, or an agent is not leased. No workers. Resolves by itself. | Usually not. |
| `ESTABLISHING` | Coming up: session created → target assigned → epoch reported → initiator connecting. | No. |
| `PAUSED` | Established end to end, but nobody is producing. | **No.** |
| `ACTIVE` | Media is flowing — the destination flow's head index is advancing. | No. |
| `PARTIAL` | **Aggregates only.** Some of what this request asked for is working and some is not. | Depends — read the breakdown. |
| `DISABLED` | **Aggregates only.** Every destination is parked, so the request is asking for nothing. | No — somebody switched it off on purpose. |

Eight states, or nine once `DISABLED` lands. The things about this table that a UI routinely gets
wrong:

**`PARTIAL` never appears on a path, a session or a worker.** It is the one aggregate-only word in
the vocabulary, and the code says so: `api.States()` returns the seven a path can be in and
`api.RequestStates()` returns those plus `PARTIAL`. A renderer must be able to show it on a request
and must never expect it underneath one. The fold that produces it, which the UI should copy exactly
so its own aggregates agree with the server's:

> If the paths disagree **and at least one is `ACTIVE`**, the aggregate is `PARTIAL`. Otherwise it
> is worst-first over the set. A set with no `ACTIVE` path is never `PARTIAL` — `PARTIAL` claims
> something is working, and it must not be said when nothing is.

Note the ordering consequence, which is deliberate and surprising: **`PARTIAL` outranks `INVALID`,
`FAILED` and `DEGRADED`**. A request whose selector expands onto twenty paths, one of which
conflicts, reports nineteen good paths and one bad one, and the top line answers *is this request
doing its job*. The loud detail belongs in the counts, the per-source breakdown and the paths list —
not promoted to the line an operator reads first. Verified live:

```
nab/wall -> PARTIAL {'ACTIVE': 2, 'ESTABLISHING': 1} | 2 of 3 paths active; ESTABLISHING: target worker is starting
   source studio-a ACTIVE {'ACTIVE': 2}
   source studio-b ESTABLISHING {'ESTABLISHING': 1} target worker is starting
```

**`status.sources[]` is the breakdown to lead with when a request has several sources.** Each entry
carries the source, its own state, its own counts and the IDs of its paths. "Studio B is dark, studio
A is fine" is the answer an operator needs from a fan-in, and it has no meaning in a one-source
model — which is why the per-path list alone is not enough.

*This used to say the field is `omitempty` and absent when there is nothing to say.* Against a
server built from this tree it is **always present and always the full list**, single-source
requests included — verified on 2026-09-01, and the doc comment on `RequestStatus.Sources` says so.
A fallback for attributing a path without it costs one line and is worth keeping for an older
server; a renderer that treats its absence as the ordinary case for one source is coding to a
behaviour that does not exist. The same correction lands in §7c, where the claim was load-bearing.

**`status.excluded[]` names flows the expansion deliberately skipped.** A path that does not exist
has no status to carry a reason, so a flow a selector passed over is invisible in a paths-only
rendering. There is one reason today, `self_output` — a flow this node's own target worker is
writing — and it fires exactly where a broad label selector meets a node that is also a replication
destination. Verified:

```json
"excluded": [{"node":"edge-01","domain":"fast/ingest",
              "flow":"5592a23b-0974-45bb-9388-89ea81c42537","reason":"self_output"}]
```

"Did not match the labels" is never listed — that set is unbounded and is the ordinary case. The
list is capped and a truncated one reports how many it dropped in `excluded_dropped`; render that
number if it is non-zero, because a silent cap reads as "nothing else was excluded".

**`PAUSED` is not an error, it is the most valuable state in the vocabulary.** It exists to separate
the two questions an operator has at 3am — *is the plumbing broken* or *is the source not
producing?* — which look identical from a "no media at the destination" alarm and have completely
different owners. Render it as its own thing, visually distinct from both green and red. A UI that
folds it into a red "not working" bucket has destroyed the one signal this design added. Note it
means the same whether the workers are up and idle or were torn down for being idle too long
(§11.1), so "PAUSED with no session" is not a contradiction.

**`INVALID` does not stop running media.** An invalid request stops *new* sessions; any session
already carrying media keeps its assignments untouched. A request can be `INVALID` while video
flows — and §7b has a case where that is not just possible but routine. Do not render INVALID as
"stopped"; render it as "will not establish anything further, and here is why".

**`DISABLED` is aggregate-only too, and it is *derived***. There is no `disabled`
field on a request to read: the state is computed from the destination entries, so a request reports
it exactly when none of them is enabled. A path is never `DISABLED`, for the same structural reason a
path is never `PARTIAL` — a parked destination produces no path for the word to be about. And a
request with one live destination and one parked one is **not** `DISABLED`; it folds over the paths
it still has, exactly as if the parked entry had never been written.

Render it as *off*, never as *broken*. It sorts below `ACTIVE` rather than above `INVALID`, it does
not belong in the landing page's list of things that are wrong, and it must stay countable — a
namespace with fifteen parked legs is a fact an operator should be able to see without hunting for
it, because a leg switched off for a reason nobody remembers is what this feature makes possible.

**Registered ≠ live.** `node.live` is the lease. A node that is registered but not leased has lost
its agent — and an expired lease is *not* proof its workers stopped (§4.2), which is why the server
freezes rather than reassigning. Surface it as information ("no agent currently holding this node's
identity"), not as "node down, its flows are gone".

`ESTABLISHING` is deliberately not split. The sub-steps are useful in a reason string and in logs,
but they are not states an operator decides differently on. Do not invent a progress bar with four
stages; show the state and the `reason`, which already says which step it is on. One reason worth
recognising: since §6.3 the agent paces worker starts through a token bucket, so a worker can
legitimately sit in `starting` for a minute on a node that is re-establishing in bulk, and it says
so. That is rate control working, not a fault.

### Reasons are machine-readable

Every non-`ACTIVE` state carries `reason` (prose, may change freely) and `reason_code` (stable,
switch on this). The full list is in `internal/api/wire.go` and it is worth reading once — the three
negotiation failures in particular are three *different operator problems* (`no_shared_fabric`,
`no_shared_provider`, `no_shared_capability`) and the codes exist so a UI can tell them apart
without matching on English. Sensible UI treatment: render the prose, and use the code to pick an
icon, a severity, and a link to the right thing to fix.

Codes that are new or renamed since the areas and fan-in work, all verified: `unknown_area`,
`area_not_writable`, `malformed_domain_name`, `same_endpoint`, `duplicate_source_flow`,
`namespace_overlap`. `domain_name_in_use` survives with a narrower meaning (nesting only).
`ReasonDomainNotMapped` is retained for old servers and is no longer emitted. `ReasonSourceIdle`
accompanies `PAUSED`, not `WAITING`.

### `settling` and `not_ready`

`GET /v1/paths` may return `{"settling": true, ...}`, and so may `GET /v1/nodes/{node}/domains` —
the second because it joins labels against inventory, so during the window it would otherwise render
every label with nothing observed beside it, which looks exactly like the labels having been lost.
It means the server has not yet run its first reconcile — it has just started, or an HA leader just
changed — and it says so **explicitly rather than reporting everything as WAITING**, precisely so
that a restart does not look like a fleet-wide outage. The field is `omitempty`, so it is absent when
false.

The UI must have a banner for this and must not render the (correct, but not yet acted on) state
underneath as if it were steady. `GET /readyz` returns `503` with `{"code": "not_ready"}` for the
same condition and `{"status":"ok","leader":"<replica>"}` otherwise; that is a reasonable thing to
poll alongside, and the leader name is the only place the API exposes which replica is reconciling.

A `503` with `code: internal` means the store is unreachable — the server is fine, its store is not.
Distinguish the two in whatever error surface you build; they send an operator to different places.

---

## 5. Traps

Each of these is real, each was confirmed against the running server, and each produces a UI that
looks right.

1. **A domain is structured on the way in and rendered on the way out, and both spellings appear in
   one response.** `path.destination.domain` is `{"area":"fast","elements":["ingest"]}`;
   `path.source.domain` is the string `"media/cameras"`; `flow.domain` is a string;
   `domainInfo.domain` is an object. That asymmetry is load-bearing (§10.6) and not a bug to
   normalise away — the structured form is what may be *sent*, the rendered form is what identifies
   a thing that already exists. Keep the object whenever you will send it back, and render with `/`
   for display. Never split a rendered one back into parts to send it.

2. **`max_message_size` is a genuine `uint64` and providers report `UINT64_MAX`.** The wire carries
   `18446744073709551615`; `JSON.parse` turns it into `18446744073709552000`. Use a BigInt-aware
   parse for this field, or read it out of the raw text, or — simplest — render `UINT64_MAX` as
   "unlimited" and everything else with a units formatter, having first confirmed you did not round
   it. It appears in `node.capabilities.fabrics[].max_message_size` and in
   `session.interface.max_message_size`.

3. **`node.last_seen` is when the lease was *taken*, not the last heartbeat.** A heartbeat renews
   the lease and deliberately writes nothing — rewriting it would advance the store revision several
   times a minute per node forever and wake every agent's long poll, where a spurious wakeup is a
   worker restart. So a healthy node can show `last_seen` an hour ago. **Never render it as
   staleness or drive a health indicator from it.** Liveness is `live`, full stop. Label the field
   "lease acquired" if you show it at all.

4. **Flow IDs are not unique to a location.** `GET /v1/flows` returns one entry per
   `(node, domain, flow)`. After replication the same UUID appears on both nodes and that is
   success, not duplication — and the destination copy carries `replicated: true`, which is how you
   tell them apart. Key rows on the triple. A "flow detail" view should list every place the flow is;
   the multiplicity *is* the answer.

5. **`replicated` is why a selector skipped something.** It is true exactly while one of that node's
   own target workers is writing that flow, and it is what keeps a label selector from matching this
   project's own output. It is briefly absent during an agent restart or a long-idle teardown, which
   is safe because a flow whose target worker is not running is not advancing either — but it means
   the flag is a live fact, not a stored one. Render it; a selector that silently skips a flow is
   otherwise undiagnosable.

6. **`status.counts` omits zeros.** A request with one establishing path returns
   `{"ESTABLISHING": 1}` and nothing else. Render the full vocabulary in fixed order with a floor of
   0, or a chart will show a gap where it should show a zero. Use `RequestStates()`' eight for a
   request and `States()`' seven for anything below it — and note the first of those becomes nine
   when `DISABLED` lands, so treat the vocabulary as a list to iterate rather than a fixed set of
   columns.

7. **Timestamps use `omitzero`** — `registered_at`, `last_seen`, `started_at` may be *absent*, not
   null, not epoch. Guard every one.

8. **`request.name` is unique within its namespace, not fleet-wide.** The identity is
   `(namespace, name)` and `request.id` is the string `"nab/wall"`. A UI keying rows or a map on
   `name` alone will silently merge two requests the moment a second namespace uses the same one —
   and "route the new camera like the last one" across two shows is exactly how that happens. Key on
   the pair, and note `path.requests[]` carries the joined form.

9. **IDs are 32 hex characters.** Truncating for display is fine; matching is **exact**. If the UI
   offers a shortened ID anywhere, make sure the thing it links to carries the full one.

10. **`flow_def` is verbatim `json.RawMessage`** — arbitrary NMOS content, including fields nothing
    in this tree models. Display it, pretty-print it, do not decode-and-re-encode it into anything
    that goes back over the wire. The session identity hashes those bytes (§5.4), so a
    re-serialisation that reordered keys would read as a different flow and rebuild a healthy
    session.

11. **Empty arrays are `[]`, not `null`**, on `requests`, `nodes`, `flows`, `paths`, `domains` and
    `namespaces` — the server normalises this deliberately, because on one endpoint (agent
    assignments) confusing absence with emptiness stops every worker in the fleet. Do add a
    defensive `?? []`; do not *rely* on needing it.

12. **`session` is absent on a path in `WAITING`**, and `session.epoch`, `session.target` and
    `session.initiator` are each absent until reported. A session in `ESTABLISHING` legitimately has
    a fabric and an interface config and no endpoints at all — verified.

13. **The user API never discloses `target_info`.** `session.target` carries `address`, `service`,
    `state`, `restarts`, `started_at` — not the blob, which is a set of RDMA rkeys and lives only on
    the agent API. That is a property worth preserving: do not go looking for a way to surface it.

14. **A request can report a path it does not hold.** This is the sharpest one and it is new with
    namespace exclusivity. The loser of an overlap goes `INVALID` / `namespace_overlap`, and it
    still lists the contested path *with the incumbent's state* — so a request that is carrying
    nothing shows `{"ACTIVE": 1}` in its own counts. `/v1/paths` is the authority on ownership:
    `path.requests[]` names only the winner. Verified both ways. Anything the UI computes from a
    request's own `status.paths[]` — a cell state, a "what stops if I delete this" preview — must
    cross-check ownership, or it will report another request's media as this one's.

15. **`disabled` is absent when false, so a reused decode target keeps a stale `true`.** It is
    `omitempty` — the zero value is the one that keeps media running (§9.1) — so a re-enabled
    destination comes back with no `disabled` key at all. Anything that decodes a poll *over* the
    previous response rather than replacing it leaves the old `true` in place and shows a leg as
    parked forever after it came back. This bit a test in this repository, in Go, where
    `json.Unmarshal` reuses a slice's existing elements; JS is safer only because `JSON.parse` always
    allocates, so the hazard moves to whatever merges the parsed object into your state. Replace,
    never merge — and note the same applies to every other `omitempty` boolean the API grows.

16. **The namespace you land in by default is `shared`, not `exclusive`.** `default` is auto-created
    on first reference with the permissive mode, verified. Everything §7a assumes about a cell
    meaning one thing depends on `exclusive`, so this is not a preference — see §7b.

---

## 6. Where the UI runs

**Settled: the UI is always same-origin with the API.** Either served by the server process
directly, or served alongside it behind a proxy that fronts both. **No CORS**, now or later.

Three things follow, and they are constraints on the code rather than background:

**Every API call uses a relative URL.** `/v1/paths`, never a configured API base. Both deployment
shapes put the UI and the API on one origin, and a base-URL setting is the thing that makes someone
reach for CORS six months later. There is no configuration to plumb here, which is the point.

**Development is a dev-server proxy, not CORS.** A dev server on `:5173` pointed at the API on
another port is cross-origin and *will* fail — the server returns 405 to the preflight and no
`Access-Control-*` headers on anything. Proxy `/v1`, `/healthz`, `/readyz` and `/metrics` through to
the API (Vite's `server.proxy`, or whatever the chosen stack's equivalent is) so that development and
production speak the same relative URLs. `ui/prototype/serve.sh` is the no-framework version of
exactly this: nginx, unprivileged, under its own prefix, serving the page and proxying the user API
and nothing else. Do **not** patch around it by adding CORS middleware to the server for
development's sake; that is how the deployment shape decided above stops being true.

**Serving from the server binary is a small server-side change you own.** There is no static-asset
route today — verified, no `go:embed` and no `http.FileServer` anywhere in the tree. Adding one is a
handful of lines in `internal/server/http.go`: embed the built assets, serve them outside both API
prefixes, and fall through to the index for client-side routes. Two things to get right when you do
— mount it *outside* `s.authenticate`, since the assets are not the thing the bearer token protects
and a login-shaped page cannot be behind the credential it collects; and keep it clear of `/v1`,
`/agent/v1`, `/healthz`, `/readyz` and `/metrics`.

### The token: decided, and the reasoning below is why it is a fallback

**Settled: the browser may hold the token, and asks for it only after a 401.** The reasoning that
argued against it is kept below in full because none of it stopped being true — it is what the
implementation is shaped around rather than what it overrode.

What was missing from the two options offered below is that they are not exhaustive. A deployment
with a token configured and no injecting proxy in front of it — which is the common one, since the
token is a server flag and the proxy is somebody else's infrastructure — could not use the UI at all,
and "you cannot open the web interface" is not a security property, it is a reason to run the fleet
with auth off. So there is a third state, and it is the honest one:

- **No token configured.** Nothing changes. No prompt is ever shown, nothing is stored.
- **A proxy or the server injects `Authorization`.** Still the recommended shape, and it is
  *untouched* by the fallback existing: the browser sees 200s, so it never asks and never stores.
- **A token configured and nothing injecting it.** `components/TokenGate.vue` asks for it, and
  `api/auth.ts` keeps it in `localStorage` and attaches it to every call.

The mechanism that keeps those three apart is that **the prompt is driven by the refusal, not by the
absence of a token** (`api/auth.ts`): a 401 on a `/v1` read raises it, a 2xx on a `/v1` read clears
it, and nothing else moves it. Note the path condition — `/readyz` is outside the middleware and is
polled *concurrently* with the reads, so a rule of "any success clears" would have its 200 undo the
`/v1` 401 beside it twice a poll and flap the gate.

The prompt itself is a field and a button. The cost is real — the token opens `/agent/v1` too, and a
browser profile is a worse place for it than a proxy config — but it belongs here and in the code,
not on the screen: an operator who has been refused has one thing to do and is not choosing whether
to do it, and a paragraph of architecture above the field is read once and then scrolled past
forever. The one affordance that follows from the cost is a `token · forget` control in the header,
shown only where a token is actually held, so a shared workstation has a way out that is not "clear
site data".

### The reasoning that shaped it (kept)

Same-origin removes CORS; it does not answer authentication, and the answer matters more here than
in most systems:

- Auth is **one optional shared bearer token**, checked in middleware on both prefixes
  (`internal/server/auth.go`). No sessions, no cookies, no per-user identity, no mTLS. No-auth is a
  supported configuration for a trusted network and for development.
- **The same token guards the agent API.** Anything holding it can claim to be any node, inject
  fabricated inventory, and read every node's RDMA rkeys (§13).
- The user API is itself a resource lever: a replication request moves uncompressed video between
  hosts, so an unauthenticated one is a fleet-wide bandwidth-exhaustion primitive.

So a token pasted into a JS constant, or typed into a field and kept in `localStorage`, hands
whoever loads the page the privileged agent surface as well. In a trusted network running without a
token that is no worse than the deployment already is. Where one *is* configured, the same-origin
decision makes the good answer easy and it is worth taking deliberately: **the proxy in front (or
the server itself, for the direct case) injects the `Authorization` header on the way through**, so
the browser never holds the token and the UI's own code has no auth surface at all. That composes
with the deployment shape already chosen and needs nothing from the frontend.

Confirm which of the two — token-injecting proxy, or no token because the network is trusted —
before building anything that assumes it. What to avoid by default is the third option nobody
picked: the browser holding the fleet-wide credential.

*(That last paragraph is what was superseded, and only that paragraph. "Avoid by default" is exactly
what the implementation does — the browser holds nothing until the server has refused a request, and
in both configurations this section recommends, it never does.)*

---

## 7. What to show

The CLI's three read verbs were given three jobs with no overlap, and the reasoning carries over to
a UI. Do not build a fourth spelling of the same view.

| Verb | Job | UI equivalent |
|---|---|---|
| `status` | Counts the fleet, then names **only what is not active**. Not a list. | The landing page. |
| `get <kind>` | Lists, so a name can be found. | The tables. |
| `describe <kind> <name>` | Everything known about one thing. | The detail pages. |

**Landing page.** Counts by state for requests and paths; nodes registered vs leased; sessions
running. Then a short list of *only* the things not `ACTIVE`, worst-first, each with its reason. The
CLI's answer to "is anything wrong" is two lines, not a screen to scan, and that is the right
instinct to carry over. Two things it surfaces that no per-request view can, both worth keeping:
nodes registered but not leased, and nodes advertising no writable area.

**Detail pages**, the nouns of §3:

- **Node** — what the agent advertises (**areas with their name, path and two grants**, fabric
  attachments with their caps, versions, sched_prio, port range), the domains it is currently
  observing, and every path touching it, **with this node's role in each**. Note the bug a live run
  caught and a unit test would not: a node can be *both* ends of a path — same node, different
  domain, which is what the loopback configuration does and what `edge-01` does in the §9 fixture.
  Check both ends independently; do not `switch` on source-then-destination.
- **Domain** — `<node>:<area>/<elements>`: its labels, whether the node currently reports it, and for
  each flow whether **this node is the one writing it**. This is also where labelling belongs.
- **Flow** — every location the ID exists, whether each is `producing`, whether each is
  `replicated`, and which paths carry it.
- **Request** — the stored spec, its sources and destinations, the per-source breakdown, the
  per-path breakdown and the exclusions. This is where "2 of 3 active" and "studio-b is dark" live.
- **Path** — the edge, its state and reason, its **refcount** (`requests[]`), and a link to the
  session.
- **Session** — negotiated fabric and interface config, the epoch, and each end's state, bound
  endpoint, restart count and uptime.

**What a UI can add that the CLI genuinely cannot**, in rough order of value:

1. **The routing matrix** — see §7a. The desired set and its live state on one screen, editable in
   place. This is the reason to build a UI at all, and it is where the operator lives.
2. **Live updating.** A poll and a diff. The CLI is a snapshot per invocation.
3. **A topology view.** Nodes as vertices, paths as edges, coloured by state. One `GET /v1/paths`
   away, and the view an operator cannot assemble from a terminal — chains (`A→B→C`) and fan-in are
   obvious here and invisible in a table. A read view, not an editor (§7a).
   *Built — `ui/app/src/views/Topology.vue`, and `ui-plan.md` §1. **Zero reads away, not one**: paths
   and nodes are both already on the single poll, so it is the only screen in the app that adds a view
   without adding load. Two things this line does not say and building it settled: it must be
   **fleet-wide**, because a chain may cross namespaces and a scoped graph cuts it in half — a
   namespace is a highlight over it rather than a filter through it; and the **layout** is the feature
   rather than the drawing, since a graph re-laid-out on a 3 s poll is unreadable whatever it
   contains, so layers and within-layer order are deterministic functions of the sorted fleet with the
   node name as the only tie-break.*
4. **Create-with-discovery.** The CLI makes you know the node, domain and flow before you can write
   the manifest. The matrix's unrouted-sources strip is this: see what exists, route it.
5. **Blast radius before mutating.** Computed from `path.requests[]` for a cancellation, and read
   straight off `stopped[]` / `started[]` for a label write.

The `describe` nouns remain the detail views behind the matrix — a cell drills to its paths, a path
to its session, a column header to its node. Keep `path` and `session` apart there, even though the
matrix cell aggregates over both.

---

## 7a. The workspace — a routing matrix, not a form

Assume an operator spends most of their time on one screen. A parameter form with a submit button is
the wrong shape for that: it is a page you *visit* to perform a transaction, and it shows one request
at a time when the operator's actual question is "what is routed where, and what is broken".

**Use a routing matrix.** Sources down the side, destinations across the top, the cell is the
connection. Broadcast operators know crosspoint matrices cold, and the idiom transfers almost for
free — this project's own dropped `xpt` CLI proposal reached for the same vocabulary, and was dropped
for being a second *command-line* dialect competing with the manifest, not because the mental model
was wrong. A matrix is not a second vocabulary: it is a rendering of the desired set, which is
exactly what the manifest is.

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

### The axes are virtual; only the cells are real

This is the reframing the areas and fan-in work forced, and it is worth stating before anything
else, because every rule below follows from it.

**A row is not a domain and a column is not a directory.** A row is a *source*: a node, a domain
**selector** and a flow **selector**. A `{labels: {role: cameras}}` row matches domains that do not
exist yet; a `{group_hint: {name: "Camera 3"}}` row matches flows no producer has published yet.
Neither is a handle on an object. A column is a *destination*: `(node, area, elements)` — and a
domain a request materialises **does not exist until a request names it**, so there is no
pre-existing list of them to put on the axis either.

So both axes are things the operator *writes*, and the server materialises them into paths at
reconcile. The cell is the only place real objects appear: the paths that this pairing expanded to,
each with a real ID, a real session and a real state.

Three things fall out, and each of them is a rule the old "a row is a request" framing had to argue
for separately:

- **A cell with no paths is not an error.** It is an axis that has not materialised yet, which is
  the ordinary state of a pre-provisioned route. This is the middle row of the three-outcomes table
  below, and it stops being a special case once the axes are understood as queries.
- **The matrix beats a node graph for exactly this reason.** A graph wants concrete endpoints to
  wire; the whole point of a selector is that the source is a query whose match set changes
  underneath you. The matrix never wires anything — it writes two queries and asks the server what
  they mean.
- **The count in a cell is load-bearing, not decoration.** It is the only place the operator learns
  how much a query turned into. A row saying `⌗ Camera 1 · 2fl` and a cell saying `2` are the same
  fact seen from two sides.

### A request is a rectangle

*This supersedes "a request **is** a row", which this document's whole §7a used to be built on.*
That held while the destination side was the only list. With `sources` a list too, a request is
**sources × destinations** — a combinatorial block over the grid, and the honest rendering is a
rectangle drawn over its cells with the request's name and aggregate state on it.

The important part first: **a single-source request is a 1×N rectangle, which is exactly the old
picture.** Nothing about the common case gets heavier. What changes is that the grid can now draw
the one intent fan-in exists for — "every camera in studio A, B and C onto the ingest wall", one
name, one lifecycle, one delete — instead of three requests an operator has to keep in step by hand.

| Matrix | Model |
|---|---|
| Row | one source: `node` + `domain` selector + `select` |
| Column | one destination: `(node, area, elements)` |
| Lit cell | that `(source, destination)` pairing is in some request |
| Cell contents | one state word and a path count — **nothing of variable length** |
| Rectangle | one request: all of its sources against all of its destinations |
| New row | add a source — to a new request, or to an existing one |
| New column | name a destination |

Five consequences, and they are the whole cost of the change:

**1. The cells of one request cannot be toggled independently.** If a request has sources
{`studio-a`, `studio-b`} and you light `studio-a → edge-02`, `studio-b → edge-02` lights too — the
rectangle has no notches. The UI must show that *before* it commits, which the dry run does for free:
the response carries every path the change would produce. "Lighting this also lights 1 other cell"
is the honest label on the button.

**2. Clearing a cell in a multi-source request is ambiguous, and the UI must ask.** Two real
operations: **drop the destination** from the request, which clears that column across the whole
rectangle; or **split the source out** into a request of its own, keeping the others. The first is
the default because it is what the request says; the second silently creates a second name, a second
lifecycle and a second thing to delete later, so it must be an explicit choice with the new name
visible. Do not pick one silently.

**3. Request-level settings belong to the rectangle, not to the row.** `provider`,
`idle_teardown_ms`, `sched_prio` and `labels` are request-level, and a row can participate in more
than one request, so a settings panel hung off a row header is lying whenever it does. Hang them off
the rectangle — the request — and let the row header carry only what a row *is*: the node, the
domain selector, the flow selector and the match count. The per-destination `provider` override is
the one that stays at cell level, because that is where the API puts it.

**4. `PARTIAL` is the rectangle's word.** It never appears on a path (§4), so a cell may only show
it as a *computed* aggregate over its own paths, using the same fold the server uses, and a path row
in a detail view must never show it at all. The per-source breakdown is the row's own slice of it,
which is what makes a twelve-source ingest wall legible: the rectangle says `PARTIAL`, the row
headers say which studio is dark, and only then does anyone need a path list.

**5. The node bands now matter in both directions.** *§7a used to justify grouping by node with
"one source to five destinations is 5× egress on one node".* That argument still runs along the
rows, and fan-in adds its mirror down the columns: twelve sources into one domain is twelve target
workers and 12× **ingress** on that destination node, which is the binding direction for an ingest
wall, since an edge is bounded by what it can take. Group **both** axes by node — a spanning header
over the columns, a band above each block of rows — because each grouping now carries a real
resource fact and a grid that makes only one of them legible renders half its requests badly.

### Off is a value, not an absence

Both axes are derived from requests, so a route that is switched off is a route that does not exist,
and the row and column it lived on go with it. Clear the last cell of a 1×1 request and the source
row and the destination column are both gone — not because the operator asked for that, but because
there is nowhere in the desired set for *off* to be written down. The board rearranges itself under
the pointer, which is the one thing a board must never do.

The prototype hit this immediately and worked around it in the only place it could:

> **Nothing you authored disappears because it became unused.** An emptied request's sources survive
> as draft rows ready to re-route; a column survives its last cell being unlit, **for the rest of the
> session**. Both have their own `×`. In the fleet neither exists any more.

That is right about the behaviour and it is client-side and session-scoped, so it is gone on reload
and was never there for a second operator. The fix is `disabled` on a destination entry (§3, §9.1),
and it is a model change rather than a UI one because the thing that is missing is a *value*, not a
place to cache one.

**A cell click disables; it does not delete.** §7a's default for clearing a cell is already "drop the
destination from the request, which clears that column across the whole rectangle" — parking is that
same operation with the entry kept, so no gesture is invented and the rectangle keeps its shape. The
rectangle is now **sources × enabled destinations**: a parked destination darkens one whole column of
it, which is a column operation and not a notch. The ambiguity in consequence 2 above survives
unchanged and so does its resolution — drop the destination (now: park it) or split the source out —
but the default answer has stopped being destructive, which is most of why it was uncomfortable.

**`×` only ever removes something already dark.** That is the rule to build to, and it is what makes
a small target in the corner of a chip safe next to a large one:

| Control | Means | Moves media? |
|---|---|---|
| the cell | park this leg, or light it again | **yes** — it is a cancellation with the text kept |
| `×` in the corner of the chip | remove this destination from *this* request | no — offered only on a parked leg |
| `×` on a column | remove this destination from *every* request that names it | no — offered only when every cell in it is parked |
| `×` on a row | remove this source, or its request if it is the last | **yes**, if its request still has a live destination |

So the destructive click is the big one and the tidying click is the small one, which is the right way
round for a control that sits in a corner and is reached past something else. Deleting a live leg is
park-then-`×`, two deliberate acts; the live cell's own tooltip should say so, because a control that
is merely absent teaches nothing.

**Two `×`s on the destination side, because "remove this destination" has two honest meanings.** A
domain several requests write into is ordinary fan-in, so the cell's `×` takes the leg out of the one
request the operator is looking at and the column's takes it out of all of them. One control that
guessed between those would be wrong half the time, and the wrong half is a bulk teardown.

**The row's `×` is the exception and stays destructive.** That is the model showing through rather
than an inconsistency: `disabled` is a flag on a *destination*, so a row of a multi-source rectangle
has no parked state to be put into first, and requiring darkness would mean parking the whole request
— which darkens the other sources too. It says so when it would stop media, and the fix if this ever
grates is a flag on a source, not a rule bent here.

**Draw a parked cell, do not blank it.** A parked leg is authored intent and the grid's job is to
render the desired set, so it keeps its two fixed-shape lines — the state word and, in place of a
count, nothing that varies in length. An unlit cell and a parked cell must not look the same: one is
"nobody has ever routed this" and the other is "somebody routed this and switched it off", and those
are different sentences an operator needs to read at a glance. This is also the payoff for the whole
change: the grid stops being a rendering of what happens to be running and becomes a board that was
laid out once, which is what an operator arriving from a crosspoint router expects it to be.

**What it does not reach.** There is no flag on a source, so a *row* of a multi-source rectangle
cannot be darkened on its own — "studio-b is down for the week, keep it in the request" is still
remove-or-split. It is designed to be additive (§9.1) and it is a narrower case than it sounds, since
a single-source request goes dark entirely when its destinations do. Do not simulate it by parking
destinations, which would darken the other sources too.

**And two traps that come with the chip's `×`,** both found building it. It must be a **sibling** of
the chip, absolutely positioned over its corner, never a child: a `<button>` inside a `<button>` is
invalid, and the inner click bubbles, so a nested `×` parks the leg on its way to deleting it. Being
out of flow then buys the second one for free — a control that took up space inside the cell would
resize every row it appeared in, which is the failure the next section is about. Where a control
*is* in flow, as on the row and column headers, render it unconditionally and toggle `visibility`,
exactly as the prototype already does for the split control and the rectangle badge.

### Geometry is a correctness property, not styling

**A cell's geometry must not depend on its content.** One state word and a count, both fixed-shape;
the reason goes in a tooltip. `reason` is prose of any length, cells in a row share a height, and a
grid that reflows when one leg starts explaining itself has stopped being a grid. The temptation is
to show the reason where there is no count to show — a leg whose selector matches nothing yet — and
that is exactly the case that fires the moment a new request is added, so it reads as the act of
adding having broken the layout.

**And nothing else in the grid may change size with state either.** A discard control that appears
only on draft columns, a badge that appears only on a node whose lease expired, an extra line that
appears only when a row empties, a rectangle outline that appears only once a request has two
sources. Each is a few pixels, and a table shares heights across a row and widths down a column, so
each one moves the entire grid under the pointer at the exact moment the operator is clicking in it.
Reserve the space unconditionally — render the control and hide it, keep the line count fixed and
vary the text — rather than appending conditionally.

### The one thing the idiom gets wrong: columns are not exclusive

**A crosspoint matrix is exclusive. One source per output; a take *replaces*. This system is not.**
An output domain holds many flows from many requests; several lit cells in one column is normal and
correct, and fan-in is explicitly the supported way to land several sources in one domain, refcounted
so the domain is materialised once.

An operator trained on an SDI router will expect the second click in a column to displace the first.
It will not. **Design against that expectation explicitly**: make a column read as *additive* —
stacked chips, a count, a column header that says "3 sources" — and never draw a cell as a latched
crosspoint button on an exclusive bus. This is the single biggest risk in borrowing the idiom, and it
is a visual-language problem rather than a logic one.

The related trap is **take semantics**. Router takes are instantaneous. Here a click is durable
intent that may legitimately sit in `WAITING` for hours before anything happens, and the state in the
cell is not a switch position but the aggregate of a set of paths. There is no take button and there
should not be one.

### Exclusive path ownership is a precondition, not a preference

**Settled: the matrix is an editor only over a namespace whose `paths` mode is `exclusive`.** §7b
carries the argument; this is what it means for the screen.

Two lit cells may only ever be two distinct claims. The moment two requests can expand onto one
path, a cell stops meaning what it looks like: two cells are one session and one worker pair,
the cell counts stop summing to what lands on the node, and un-lighting either one cancels a request
and stops nothing while the cell goes dark exactly as if it had. The last is the dangerous one —
nothing breaks, so the operator believes they tore it down and does not come back when the egress is
still there.

So:

- **The picker shows every namespace's mode**, always, not on hover.
- **A `shared` namespace is not a matrix.** It gets a different view — the ledger of §7c — rather
  than this one greyed out, because the grid is as wrong to *read* in that mode as it is to click.
  *This used to add "offer the conversion to `exclusive` from there".* It does not: a shared
  namespace is a supported arrangement rather than a state to plan an exit from, and §7c records why
  the planner that was built on that reading came out again.
- **The matrix creates its own namespaces `exclusive`.** The API's default is `shared` and
  `default` is auto-created that way, so this is a deliberate act on every create path.

**The one hole, and it is bounded.** Exclusivity is enforced on *materialised paths*, so two
requests with the identical source and destination are both accepted while the selector matches
nothing — verified: two requests naming a group hint no flow carries both come back `WAITING` /
`flow_not_found` with zero paths, and no overlap fires. That is **one cell with two owners**, and it
resolves itself into one `INVALID` the moment a producer appears. Handle it in two places: refuse it
client-side on create, which is structurally decidable by comparing source entries without asking the
fleet; and render it honestly when it arrives anyway from the CLI or an adapter. This is the one
place §7b's rejected sharing markup earns its keep, and it is bounded to cells with no paths — a
condition that cannot persist.

### Collisions the grid cannot draw

Two of them, both from fan-in, and both are properties of a *pair* of cells rather than of one:

- **`duplicate_source_flow`** — two sources pinning the same flow UUID into a shared destination.
  Refused at POST, so it is blocking, and the message names both by index:
  `sources[0] (studio-a/media/cameras) and sources[1] (studio-b/media/cameras) both pin flow 5592… into the same destination`.
  Anchor it to the two **rows**, which means a row must be able to point at another row.
- **`flow_conflict`** — the same harm arriving later, because one or both sides were selectors and a
  producer published the collision months after the request was written. It lands on a path, so the
  cell shows `INVALID` with a reason naming the other path.

The grid should not try to draw an edge between two rows. Naming the sibling in the reason and
letting the operator jump to it is the whole of what is needed.

### The three editors around the matrix

The matrix is the workspace. Forms do not disappear — they are re-homed as the things a matrix
cannot express on its own.

**New row — the source editor.** Node, then domain, then **a group, then how much of it to take.**
Do not open with a flat list of flows: the operator browses to *discover*, but what they mean is the
group, and a UUID picker recreates exactly the problem selectors exist to solve.

The domain step is now a choice of two kinds, and it is worth making the choice visible rather than
defaulting silently:

| Domain | Selector | When |
|---|---|---|
| **this one** | `{name: {area, elements}}` | the operator picked a domain out of the list |
| **anything labelled** | `{labels: {…}}` | the operator picked labels |

A manifest naming a domain is self-contained; one naming labels depends on a `kind: domain` document
having been applied. Say which one the row will produce, because a label row is a standing query —
a domain labelled tomorrow joins it, which is the point and is also the surprise.

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

`all` is the default and the one to make attractive: omitting `type` is how a camera's video and
audio travel together, and it is a *standing* selection — a flow the producer adds later joins it.
The empty box is the point, so write that in the box rather than leaving a blank. Note there are now
two spellings of "everything": `{all: true}` takes the whole domain, a group hint with no type takes
one group of it, and the difference is worth a sentence in the UI because the first is the retired
proxy's subscription shape and the one an operator migrating will reach for.

**`select flows` creates one row per flow, and must say so.** A request's *selector* pins exactly one
flow ID, so three pinned flows is three sources — which, now that sources are a list, may be one
request rather than three. Both are defensible and they differ in lifecycle: one request means one
name, one delete and one aggregate that goes `PARTIAL` when one camera is dark; three requests mean
three of everything. Show the names it will create before it creates them, and make the choice
explicit rather than implicit in a checkbox.

**Ungrouped flows must stay reachable.** A producer that never set the NMOS tag is not a flow you can
decline to replicate, but there is no name for a group-hint selector to match, so `all` and
`select type` cannot be expressed for it. Give it a pseudo-group, disable the two modes, and note
that `{all: true}` over the domain does reach it.

The source-domain list comes from `GET /v1/nodes/{node}/domains`, which reports **observed** domains
joined against their label records — so it covers what the agent sees and also labelled domains it is
not currently observing, which is how an operator sees a label they applied before the producer came
up. `domainInfo.observed` is the flag that tells them apart, and a labelled-but-unobserved domain is
information, not an error.

Show each domain's labels and its `name` label if it has one — that is what an operator called it.
Offer labelling from here: it is `POST /v1/nodes/{node}/domains`, "see an unnamed domain, name it" is
the gesture the CLI's `label` verb exists for, and it is now a mutation with a preview of its own
(§3), so it gets the same dry-run treatment as everything else on this screen.

**New column — the destination editor.** This is the step a router matrix does not have, and it is
structural rather than incidental: **the destination domain does not exist until a request names
it.** So the column set is "every destination named by some request", plus the one being created.
Creating a column is real work and deserves a real control:

- **Only nodes with an area they grant `write` on can be destinations.** Show the others disabled
  with the reason — `area_not_writable`, or `unknown_area` for a node with no areas at all — rather
  than omitting them. "Where is edge-03?" is the question omission produces.
- **The area is part of the name, not a separate setting.** Render the area picker and the elements
  as one field that reads `fast/studio-a/cam1`, list only writable areas in the picker, and never
  default it invisibly — omitting it is omitting half the name.
- **Show the resolved directory as they type.** `area.path` is advertised (diagnostics only, may be
  absent — guard it), so the field can render `/dev/shm/mxl/studio-a/cam1` under the name.
- **Names are unique per node across areas, and nesting is the only collision.** `fast/ingest` and
  `bulk/ingest` are two different domains and both are fine. What is refused is `fast/studio-a`
  against an existing `fast/studio-a/cam1` — one domain directory containing another —
  `domain_name_in_use`, with the message naming the other. *If the operator has read older docs,
  this rule has inverted; the current one is the intuitive one.*
- The elements are capped at 8, and the whole rendered name at 255 bytes.

**Cell detail — the per-leg editor.** The per-destination `provider` override lives here, and so does
the breakdown: which paths this leg expands to, their states, their sessions. A cell is an aggregate,
and §11 requires both the summary and the breakdown to be reachable. It is also where the negotiated
provider is finally visible — `path.session.interface.provider`, which nothing shows before apply.

### Unrouted sources

*Built, on 2026-09-01 — `ui/app/src/components/UnroutedStrip.vue`, and `ui-plan.md` §1. **One
sentence below could not be implemented as written and was not.*** "Flows present in inventory that
**no request selects**" needs selector evaluation in the browser — label sets ANDed over a node's
domains, group hints with and without a type, pinned IDs, and `self_output` over the top — which is a
second expansion engine beside `reconcile.Compute` whose disagreements with it are silent, and which
§3 of this document tells you not to build. What is asked instead is the question the API answers
directly: **is there a path carrying this flow as its source?** `path.source` is a `FlowAddress` and
an inventory entry is the same triple, so it is a set membership test and nothing is guessed.

The two readings differ in exactly one place, and it is the better answer rather than a compromise: a
request whose selector matches a flow but whose every destination is **parked** (§9.1) expands to no
path, so the strip counts that flow as unrouted. The operator's question is *is this going anywhere*,
and a parked route is going nowhere. It is not the same claim, so the code does not make it.

A matrix shows only what someone has already asked for, so "camera 5 is going nowhere" is invisible.
Keep a strip or panel of flows present in inventory that no request selects — that is where the flow
browser lives, and clicking one starts a new row pre-filled. It closes the discovery loop that the
matrix alone leaves open.

*One thing the "clicking one" sentence hides, found by building it:* a strip entry is **one flow**,
and routing one flow by pinning its ID authors the narrowest selector the API has from the broadest
gesture the screen offers — so its siblings are back in the strip tomorrow and the operator has a
request per UUID. The click opens the editor on the flow's **group** instead, which is the standing
selection they meant; only a flow carrying no group hint is pinned, because there is no name for a
group selector to match. That is also why the click is a pre-filled panel rather than a staged edit:
what the strip decides has to stay visible and changeable, because it decides more than was pointed at.

Two filters on it, both load-bearing:

- **"No request selects" means no request in this namespace**, with a note on the ones another
  namespace already routes. §7b argues that out; neither plain reading works.
- **Flows carrying `replicated: true` are this project's own output.** They are legitimate sources —
  that is how a chain `A→B→C` is written — but they are not "unrouted", they are the far end of
  something already routed. Mark them rather than hiding them, and note that a *label* selector will
  never match them anyway (`self_output`), so routing one onward means naming its domain.

### Every mutation is still dry-run first

`POST /v1/namespaces/{ns}/requests?dry_run=true` runs validation, builds the candidate fleet,
reconciles it and returns the `api.Request` that *would* result — then skips only the write.
Verified: a two-source, two-destination group-hint request comes back with all six expanded paths,
their real IDs, their real session IDs, their real states and the per-source breakdown, with
`X-Mxl-Outcome: created`, and the store's request list still empty afterwards.

That means the UI needs no expansion logic of its own. It renders `status.counts`,
`status.sources[]`, `status.paths[]` and `status.excluded[]`. It never has to know that two sources
matching three flows across two destinations makes six paths — it asks.

**And "every mutation" now includes label writes.** `POST /v1/nodes/{node}/domains?dry_run=true`
returns the resulting record plus `stopped[]` and `started[]` as full `Path` objects. A label edit
offered from the source editor is a media-moving operation with a preview available; use it, and put
the preview in the same place the request preview goes.

In a matrix this is what makes a click safe. Lighting a cell is: take the request's stored spec,
append one entry to `destinations[]` (or one to `sources[]`, or create a new request), dry-run it,
show the operator the paths that would appear — including the ones in *other cells of the same
rectangle* — and any refusal, then POST for real. **The cell can preview its own consequences before
it commits**, which is something a crosspoint button has never been able to do.

Debounce it — each dry run is a full store load plus a reconcile (§2) — and fire only on a
*structurally complete* change, not per keystroke in a domain field.

### Three outcomes, and they must look different

Getting these confused is the most likely way this screen goes wrong, because two of them are
success:

| Server says | Meaning | Render as |
|---|---|---|
| `400` + `details.reason_code` | **Refused.** Never resolves by itself. | Blocking. Refuse the cell, anchor the message to the field or the row the code names. |
| `200/201`, `state: WAITING`, `paths: []` | **Accepted, not yet satisfiable.** | A quiet cell state, not an error. The click goes through. |
| `200/201`, paths with states | Accepted and expanding. | The cell lights with its aggregate state. |

The middle row is the one to get right, and the virtual-axes framing is why: a cell with no paths is
an axis that has not materialised, and pre-provisioning replication for a camera that is not live yet
costs nothing and is explicitly supported. A UI that refuses that click has removed a supported
workflow. Light it as `WAITING` with "no flow yet" and let it stand; it will come good on its own when
the producer appears.

Compare the refusal, which carries its own fix in the prose:
*"node "edge-01" advertises no area "nope", it has "bulk" (writable), "fast" (writable), "media"
(read-only)"*. Render those messages verbatim — they are better than anything the UI would write —
and use `reason_code` only to decide *what to highlight*.

The refusals that anchor to a **row** rather than a field are the fan-in ones: `same_endpoint` (whose
message names both indices, `sources[0] and destinations[0] are both edge-01/fast/ingest`) and
`duplicate_source_flow`. Both are typos with both halves visible in front of the operator, which is
why refusing the whole request is right there and nowhere else.

### Committing: per-click, or a batch take?

`POST` is create-or-**update** on the name, with no create-only mode and no 409, and the dry run
reports which via `X-Mxl-Outcome` before anything is written. That gives two defensible commit models
and the choice is worth making deliberately:

- **Apply on click.** Each toggle is its own POST. Requests are independent durable intent and an
  unchanged apply writes nothing, so this is safe and it feels like a router.
- **Stage and apply.** Toggles accumulate as pending changes; one Apply commits them, and the pending
  set is exactly a manifest diff. Closer to how the desired set is actually managed, and it gives an
  undo window before live media moves.

Staging matches the declarative model better and is the safer default for something that moves
uncompressed video; apply-on-click matches the idiom the matrix borrows. The prototype staged, and
the payoff was larger than expected: because a staged set can be dry-run as a batch, the preview
reports real outcomes and real blast radius before anything moves, and that is what let every
confirmation dialog come out of the interface. Apply-on-click would want them back — and it wants
them *more* now that one click can light several cells of a rectangle.

Rules either way:

- **`unchanged` means do not write.** The server already skips it, but a UI that re-POSTs on every
  interaction turns a resyncing screen into store churn against §8.3's sizing.
- **An empty request is a state, not an event.** A request must name at least one source and at
  least one destination, so a rectangle with none of either has no spec to POST and committing it is
  a `DELETE`. It is tempting to make un-lighting the last cell *be* that delete, with a confirmation
  in front of it. Don't: it makes the order of two clicks matter, because clearing a cell before
  lighting its replacement destroys the request while doing it the other way round does not, for the
  same end state. Let it sit empty, say what applying will do, and let the commit work out that an
  empty spec means `DELETE`.

  ***`disabled` removes this rule rather than satisfying it***. With a parked
  destination the entries never leave the spec, so there is no empty spec, nothing for the commit to
  reinterpret, and no `DELETE` inferred from an absence. The ordering hazard stops existing instead
  of being carefully arranged around: park-then-light and light-then-park reach the same spec because
  both are edits to the same entry list. Keep the paragraph above for the shape of the mistake it
  describes; the mechanism it prescribes is superseded by the one in "Off is a value, not an
  absence".

  The draft and the request are different things — the draft is what the operator has authored, the
  request is what exists on the server — and keeping them apart is what makes re-routing feel
  weightless. It also gives the "remove" control something distinct to mean: *I am done with this
  source*, rather than *I am done routing it here*.

- **Staging is the confirmation.** Given a pending bar with preview and discard, a modal adds
  nothing: a dialog that appears mid-gesture gets dismissed reflexively, while a staged change has to
  be read and applied. Put the weight in the preview instead, where it can be specific — for a
  cancellation, which paths actually stop, computed from `path.requests[]`, since a path survives
  while any other request references it. "3 of 4 paths stop" is worth reading; "are you sure?" is
  not.

### The request name

A request needs a name before anything can be dry-run: it is the idempotency key and it is validated
(letters, digits, `-_.:`, no leading dot). Suggest one as soon as there is a source and a first
destination (`cam1-to-edge-01`), keep it editable, and let the operator own it — names end up in the
manifest and in `delete` commands, so they are not an implementation detail.

**Renaming does not rename anything.** `(namespace, name)` is the ID, so a changed name is a *new*
request and the old one stays, still running, still holding its cells. Lock the field once the
request exists, or make "duplicate" an explicit action that shows both will exist.

**Duplicating is probably the most-used control on the screen.** A new camera arrives and it should
be routed like the last one: copy the rectangle, change the selector, keep the destinations. That is
shorter than any create flow and it starts from something known to work.

### The matrix *is* the manifest

One rectangle is one document, so the whole grid serialises to the multi-document YAML file exactly.
Make that visible — a panel or a drawer showing the current manifest, copyable, and if you take the
staged-commit model, the pending changes as a diff against it.

This is not a nicety. The manifest is the documented operator interface and the thing that lives in
git, so a UI that only ever produces requests through its own controls quietly creates a second
source of truth for the desired set. Rendering the file makes the UI teach the format instead of
competing with it, and turns "I worked it out on screen, now commit it" into a copy rather than a
re-derivation. It is also the honest answer to "how do I do this for forty cameras": the answer is a
file, and the operator has just been shown what one looks like.

Three things the rendered file must get right, all of which differ from the wire:

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

The selector is **flattened** onto each source (`flow:` or `group_hint:` directly under the entry)
where the wire nests them under `select`; the domain is a **string or a map** where the wire is
`{name: {area, elements}}` or `{labels: {}}`; and an **omitted flow selector means `{all: true}`**,
filled in by the CLI, never by the server. `sources:` is always a list. An unrecognised key is an
error in a manifest, which is the opposite of the wire's rule and is deliberate: a typo that silently
does nothing is precisely the failure a declarative format exists to prevent.

### What not to build

- **A multi-step wizard.** Steps punish repetition, and this is the repeated task.
- **A node graph as the editor.** Build it as the topology view instead. *Done, and the reasoning
  held at the keyboard: a vertex is a **node**, and no request names a node pair — it names a source
  selector and a destination domain. There is nothing for a wire between two vertices to become, so
  the graph's only gesture is selection (§7, item 3).*
- **Exclusive-crosspoint visuals.** The single most important don't.
- **A client-side reimplementation of §7.2.** Structural hints for immediate feedback are fine.
  Everything else is the dry run's job, and only the server can see cross-request conflicts at all.
- **A rectangle with notches.** If the UI ever lets a request's cells be lit individually, it has
  stopped modelling the API and started modelling a grid, and the next POST will not round-trip.
  `disabled` does not weaken this and is the case that proves it: parking is per *destination*, so it
  takes out a whole column of the rectangle, and a flag on a `(source, destination)` cell is refused
  in the model for exactly the reason it is refused here — a request would stop being sources ×
  destinations and become an arbitrary bitmap (§9.1).

### Two gaps worth raising

**The dry run does not tell you which provider was negotiated.** `PathStatus` carries id, source,
destination, state, reason and session id — no interface config; verified. Negotiation *failures*
surface as `no_shared_fabric` / `no_shared_provider` / `no_shared_capability`, so a cell can report
that a leg will not work. But for `provider: [verbs, tcp]` — "prefer verbs, tcp acceptable" — *which
one did I get* is only answerable after apply, from `GET /v1/paths` → `path.session.interface`. Since
§10.4 makes provider choice a performance cliff rather than a detail, showing it in the cell preview
is worth something. Adding the negotiated provider and fabric to `PathStatus` is a small server
change. It only matters when a fallback list is in play — a hard pin either works or is refused.

*This is now in scope and lands with the UI* — `docs/open-items.md` §2.10. `Compute` negotiates
before it emits, so the value is already on the session record when the status is built and the dry
run has it too; it is two fields on one struct, and the preview is the thing that wants it.

**A matrix wants one read, and there are four.** Rectangles come from `GET /v1/requests`, cell states
from the same call's `status.paths[]`, column metadata from `GET /v1/nodes`, and the unrouted strip
from `GET /v1/flows`. Each is a full store load plus a reconcile (§2), and this screen is the one that
polls continuously. If the matrix turns out to be the whole product, a single composite read — or an
ETag so some of the four usually return 304 — is the addition to ask for, and it is a better answer
than four independent timers. Not *three* of the four, though: only `/v1/nodes` is soundly cacheable
of the ones this screen reads, because the other three are either time-derived or the whole point of
the poll (§2, `docs/open-items.md` §2.9).

---

## 7b. Namespaces: the matrix shows one partition

Two requests can expand onto **the same path**. The path identity is
`(src node, src domain, flow-id) → (dst node, dst domain)`, and nothing in it names the request that
asked for it. So a group-hint request and a pinned-flow request over one source domain, pointed at
one column, are **one edge, one session, one worker, refcount 2**.

*(That identity used to carry the resolved output root as a separate term. The areas work removed it:
the area is the first segment of the domain's name, so `fast/ingest` and `bulk/ingest` are already
two identities and the extra term was redundant — §5.4.)*

Rendered as a matrix that is a lie in three places at once. Two lit cells in a column mean two streams
when there is one; the cell counts no longer sum to what lands on the node; and un-lighting either
cell cancels a request and stops nothing, while the cell goes dark exactly as if it had. The last is
the dangerous one — nothing breaks, so the operator believes they tore it down and does not come back
when the egress is still there.

That can be marked up in the cell, and it was, and it is the wrong shape of fix: it decorates every
cell on the screen to describe a condition that should not exist. **Partition the requests instead.**
A request belongs to exactly one namespace; in a namespace whose `paths` mode is `exclusive`, no two
requests may hold one path; the matrix shows one namespace. Then a cell means what it looks like —
cell counts sum to the column's, and clearing a cell stops exactly the paths in it — and none of the
markup is needed.

**The default is not the mode this screen needs.** Namespaces default to `shared`, and `default` is
auto-created that way on first reference — verified, `[('default', 'shared', 1), ('nab',
'exclusive', 1)]`. The rule is opt-in because it protects *this screen's* legibility rather than
anything about the fleet (below). So the matrix creates its namespaces `exclusive`, shows the mode in
the picker, and refuses to be an editor over a `shared` one (§7a).

### It is enforceable, which is what makes it a partition rather than a convention

`FlowInventory.GroupHint` is a single pointer: a flow carries **at most one** parsed hint. So two
selectors over one source domain intersect only in these ways, and most of them are decidable without
looking at the fleet at all:

| Two selectors | Overlap? |
|---|---|
| different `group_hint` names | never, whatever producers do |
| same name, different `type` | never |
| different pinned flows | never |
| `{group_hint: X}` and `{group_hint: X, type: T}` | yes, statically |
| the same pinned flow twice | yes, statically |
| `{all: true}` and anything else over one domain | yes, statically |
| a pinned flow and a hint | **only dynamically** — a producer can retag |

That distinction is the difference between a rule an operator can hold in their head and one that
fires at random. Nearly every overlap is refusable at admission with a message naming the sibling
request. Only pin-vs-hint can arrive later, and it must therefore be a **reconcile-time state, not
only an admission check** — a create-time-only rule would be a lie the first time a producer
republished.

Still correct, and no longer complete: **two selectors in one request** can now reach one destination
flow too, over two source domains on two nodes. That is not one edge — it is `duplicate_source_flow`
or `flow_conflict` (§7a), a different code with a different disposition, and the grid draws it
differently.

### How the loser behaves, exactly

`namespaceOverlaps` in `internal/server/reconcile` enforces it. Three properties, all verified:

- **Admission comes free.** `handleCreateRequest` already reconciles a candidate fleet, so the rule
  is written once and covers both the POST and the reconcile that discovers an overlap later.
- **The POST succeeds.** The colliding request comes back `INVALID` with
  `reason_code: namespace_overlap` and a message naming the incumbent:
  *"request "wall" already replicates studio-a/media/cameras 5592a23b… to edge-01/fast/ingest in
  namespace "nab""*. Refusing the whole request would mean one bad pairing killing nineteen good
  ones, which selectors make routine.
- **Precedence is incumbency, then `UpdatedAt`, then ID** — the same order every conflict rule uses.
  The request already carrying media keeps the path.
- **Losing does not stop media.** The losing leg gets no new session; the path, held by the winner,
  carries on. Its sibling legs are unaffected. That is what makes the rule safe to switch on over a
  fleet that already has overlaps: they surface as `INVALID` requests rather than as an outage.

**A parked leg holds nothing, and re-enabling it can lose**. A disabled destination produces no
path, so it cannot hold one and cannot make anything else `namespace_overlap`. The consequence is
the one to surface: park a leg, let another request claim its path, un-park it, and *this* one is
now the loser. It stops no media, but it means "switch it back on" is not guaranteed to be the
inverse of "switch it off" — so read the dry run before flipping the flag back, the same as for any
other write.

The mechanism is worth knowing because it is not the obvious one. `namespaceOverlaps` orders on
`(UpdatedAt, id)` and consults no session — incumbency cannot separate two requests over one path,
since the path's session exists whoever holds it — so the contest is decided by recency. Un-parking
is a write and every write is stamped, which is what makes the returning request reliably the newest
and therefore the loser. Do not build a UI that explains this as "the one carrying media keeps it":
a request with an older stamp will take the path straight back off one that has been running for a
week.

**And the loser still reports the path.** This is trap 14 and it is worth repeating here because it
is the one thing a matrix must not get wrong: the loser's own `status.paths[]` lists the contested
path *with the incumbent's state*, so a request holding nothing can show `{"ACTIVE": 1}`. Only
`/v1/paths` → `path.requests[]` names the owner. A cell drawn from a request's own status, with no
ownership cross-check, will show another request's media as this one's — and it will show it in the
one situation where the operator most needs to know they are not the ones carrying it.

### Where a namespace lives

**A first-class object, and a real `namespace` property on the request** (§9.3). *An earlier version
of this section put it in a reserved `namespace` label, spelled as a plain `namespace:` field in the
manifest and as a label on the wire.* That was justified by `--prune -l namespace=nab` already
spelling *make the fleet's `nab` namespace equal this file* — an argument from an existing CLI
mechanism rather than from the model — and it cost two things worth more than it saved. `namespace`
is a legal user label, so a label an operator wrote for their own reasons silently became a partition
key. And the two spellings had to be reconciled, with a disagreement between them refused rather than
resolved. `--prune` takes `-n` now, which is the natural spelling anyway.

Namespace = prune scope = one manifest document set = one matrix. The manifest pane in §7a is one
namespace, one file, which is what an operator was going to commit to git anyway.

Five things to be explicit about:

- **Request names are scoped to the namespace.** `(namespace, name)` is the ID, so two operators — or
  two of an adapter's sources — can both hold a request called `cam1`. The canonical route is
  `/v1/namespaces/{ns}/requests/{name}`; `/v1/requests` is the fleet-wide list and takes
  `?namespace=`. **A name is unique within its matrix, not within the fleet** — do not build a
  uniqueness check against everything.
- **Labels are tagging; a namespace is a partition.** Labels stay many-per-request and freely
  overlapping. `namespace` is no longer reserved among them.
- **It is not called a group, and that is deliberate.** The source editor's whole vocabulary is the
  NMOS group hint — "a group, then how much of it to take" — and the row header's selector chip says
  `group`. Calling the partition a group too would put two unrelated nouns, spelled the same, three
  inches apart on one screen. The NMOS one is external vocabulary an operator already has, so ours is
  the one that moves.
- **A request that names no namespace is written into `default`.** Show `default` in the picker as
  its own namespace, with its mode; hiding it would hide most fleets entirely. Note that
  `GET /v1/namespaces` lists only namespaces that exist as records, so a fleet with no requests at
  all lists nothing — synthesize `default` in the picker rather than showing an empty list.
- **Namespaces auto-create on first reference and never auto-delete**, and deletion is refused while
  any request references it. `GET /v1/namespaces` carries each one's `requests` count, so the picker
  gets "3 requests" for free rather than counting client-side.

### The exclusivity rule is opt-in per namespace, and the matrix opts in

The reason it is opt-in rather than universal is worth carrying into the UI, because it decides what
the screen is allowed to assume: **intra-namespace overlap is free.** Two requests on one path share
one path, one session and one worker pair, which is refcounting working as designed. Nothing is
doubled and nothing is corrupted; what overlap costs is honesty in a grid. That is a real cost and it
is *this screen's* cost, not the fleet's — so the rule is one the reader opts into, not one every API
client is held to. A Kubernetes adapter creating one request per pod wants `shared`, because several
pods asking for one flow is ordinary and marking the second one `INVALID` would make a pod's status
depend on another pod's existence.

Which is exactly why the matrix must state its requirement rather than assume it. Offering to switch
a namespace from `shared` to `exclusive` is a reasonable control to build, and its consequences are
what a dry run already previews: existing overlaps become `INVALID` requests, and stop no media.

### What still crosses namespaces, and where to put it

Namespaces partition *requests*, not destinations. Two of them may still route one flow into one
domain — `production` and `archive` both landing camera 1 in `edge-01/fast/ingest` — and that is
fan-in, which the API supports and which refcounts so the domain is materialised once. So one honest
fact survives, and a screen showing a single namespace cannot see it at all.

Put it on the **column header**: *"+ archive"*, or *"+ 2 namespaces"*. That is the right altitude — an
operator thinking in namespaces asks whether another show writes here, not whether one particular
edge is shared — and it is the fact that changes what emptying a column means. Keep the removal
preview honest for the same reason, one level down: dropping a leg another namespace holds stops
nothing, and `path.requests[]` already says so at no cost.

The alternative is to partition destinations too, so a namespace *owns* its output domains. It makes
the fleet genuinely disjoint and it forbids two shows deliberately fanning into one archive domain,
which is the arrangement fan-in exists for. Partition the requests; leave the destinations shared.

### What scopes to the namespace and what does not

**The landing page does not.** Health is a fleet fact. Scoped to a namespace, "is anything wrong"
answers "is anything wrong in the namespace I happen to have selected", which is the wrong answer at
3am. Namespaces scope the workspace, not the health view.

**The unrouted-sources strip does**, with a marker. Neither plain answer works. Fleet-wide, a flow
another namespace routes vanishes from the strip — and since the strip is also where the flow browser
lives, it becomes undiscoverable in the one view where routing is built. Namespace-scoped and silent,
it shows the flow as untouched, the operator routes it, no `namespace_overlap` fires because the
duplicate is in *another* namespace, and the strip has just talked them into doubling egress on the
source node. So: namespace-scoped, with a note on entries another namespace already routes, naming it.

The asymmetry is the point. Health is a fleet fact. Work-to-do is a namespace fact whose
*consequences* are fleet-wide, so it needs the caveat attached rather than the whole view rescoped.

---

## 7c. A `shared` namespace is a ledger, not a board

§7a's matrix has a precondition and §7b argues it: the grid is an editor only over a namespace whose
`paths` mode is `exclusive`. That leaves a hole rather than closing one. `shared` is the API's
default, `default` is auto-created that way, and §7b names a first-class reason to stay in it — a
Kubernetes adapter writing one request per pod, where several pods asking for one flow is ordinary
and marking the second one `INVALID` would make a pod's status depend on another pod's existence. So
a shared namespace is not a mistake to be converted out of; it is a supported arrangement the
workspace cannot draw. **It gets its own view, and the view is not a grid.**

***Corrected on 2026-09-01, while building it: a path held by several requests is not a condition to
surface.*** The rest of this section was written treating one as *contested* — it had a name, a count
on the header line, a highlight on the claim, a place in the attention filter, and a whole feature
built on it, the conversion planner below. That is wrong for the mode this section is about. A path
with N claims in a `shared` namespace is **refcounting working exactly as designed** — one path, one
session, one worker pair, nothing doubled — and it is the arrangement the mode exists for; the
sentence two sections earlier in §7b says so itself: *"intra-namespace overlap is free… what overlap
costs is honesty in a grid"*. The cost is the **grid's**, and this view is not one. A Kubernetes
adapter writing one request per pod produces the state routinely and correctly.

So `held by N` is plain data, with no highlight and no judgement; the attention filter and the header
count are about **state** alone; and the planner is gone (see below). What survives untouched is the
part worth the most and it is unaffected by the correction: **one row per path**, the **selector on
every claim**, and **`sole` / `shared`** with *rides along*. The rest of this section reads correctly
with `contested` deleted rather than replaced — the paragraphs that depended on it are marked where
they stand.

The prototype's answer up to now was to render the matrix read-only behind a banner. That is honest
about the editing and dishonest about the reading: the grid still draws two lit cells for one path
and still shows counts that do not sum, and greying the cells does not stop an operator believing
them. Read-only was the cheapest thing that was not *wrong to click*; it was never right to look at.

### Why not a decorated matrix

*§7b rejects the decorated cell already, on the grounds that it "decorates every cell on the screen to
describe a condition that should not exist". That argument does not carry here* — in a shared
namespace the condition is ordinary, so paying for it everywhere would be paying for what the screen
is about. The reason to reject it here is different and it is worse: **it is the axis that collapses,
not the cell.**

In an exclusive namespace two rows cannot expand onto one path, so a row is a distinct query and the
row set partitions the desired intent. Drop the rule and two requests may hold overlapping selectors
over one source domain — `{all: true}` against a pinned flow inside it is the canonical shape, and
§7b's own overlap table says it collides statically. Two rows are then *different queries with the
same match set*, and there is no correct answer to which of them a path belongs on. That is not a
property of a cell and no markup in one can fix it.

The same reasoning disposes of the other near-miss, a matrix with one row per *request*: it puts the
same path on several rows by construction, which is the thing to be rid of.

### The inversion: a claim is the object

The matrix renders **intents**, deduplicated by nothing. This view renders **claims** — the triple
`(request, source entry, path)` — over a list of paths deduplicated by the server, which is the same
deduplication the fleet is doing. All three of §7b's lies go out with the framing rather than being
handled: one path is one row so nothing is double-counted, the counts sum because the rows are the
real edges, and *un-lighting stops nothing* becomes visible as a refcount that stays above zero.

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

That is a real reading of the `k8s` fixture in §9, verified on 2026-09-01: `wall` takes the whole of
`studio-a/media/cameras`, `cam1-pin` pins one flow inside it, and `/v1/paths` reports
`["k8s/cam1-pin", "k8s/wall"]` on the one path they share.

**Group by destination domain, then by source flow.** That is the axis fan-in runs along, it is the
binding resource direction on the ingress side (§7a, consequence 5), and it is what puts the two
claims on `5592a23b` on adjacent lines, where the diagnosis is a two-second read rather than a
hunt. The alternative grouping — by source flow, "who is consuming this camera" — is the same data
transposed and is worth offering as a toggle, not as the default: a shared path requires *both* ends
to agree, so the destination is where two claims always land adjacent and the source is where they
only sometimes do.

### Show the selector on every claim

This is the part of the design worth most, and nothing else in the product surfaces it. A path with
two claims raises exactly one question — *why do I have two of these?* — and the answer is always the
pair of selectors that produced them. Rendering `all flows` and `flow 5592a23b…` on adjacent lines
turns an invisible interaction into something an operator reads without knowing the rule.

**It needs no server change.** `status.sources[]` carries each source entry's own path IDs (§4), so a
path joins back to the source that produced it and thus to its selector. That is the same join
`pathsForSource` already does in the prototype for the matrix's rows, used in the other direction.
Say `sources[i]` with the index, because `duplicate_source_flow` and `same_endpoint` name their
operands that way (§7a) and the two spellings should match.

*This paragraph used to say the field is `omitempty` and absent for a single-source request, so the
attribution falls back.* It is always present and always the full list (§4) — keep a one-line
fallback for an older server, but do not design the join around an absence that does not occur.

### `sole` and `shared` is the number the matrix cannot show

For a request, the count of its paths where `path.requests.length == 1`. Those are the paths that stop
if it is deleted; everything else keeps running on somebody else's claim.

It is the whole cancellation preview, precomputed and standing rather than summoned by a confirmation
dialog, and it makes one condition legible that has no other symptom: **a request with zero sole paths
is carrying nothing.** Nothing is broken, nothing is doubled — refcounting is working exactly as
designed and the egress is not duplicated (§7b) — but somebody wrote an intent that is entirely
subsumed by another, and in an adapter-populated namespace that is usually a bug in the thing writing
the requests. It is invisible in the matrix, invisible in `get requests`, and one column wide here.

### The request pane keeps the rectangle

A path-first view loses exactly one thing, and it is the thing `disabled` was added to protect: a
parked destination produces no path, so it has no claim to appear as, and a ledger built only from
`/v1/paths` renders a switched-off leg identically to one that was never written. That is the same
"off has nowhere to be written down" failure §7a describes, arriving by a different route.

So each request keeps **its own** small sources × destinations grid. A per-request rectangle is never
ambiguous — rectangles only overlap each *other*, and alone one is exactly the request's spec — so it
can draw parked legs dark, and the component §7a already needs is reused rather than a second
spelling invented. The global grid is what fails in a shared namespace; the rectangle is fine.

`DISABLED` therefore renders on the request row and nowhere else, which agrees with §4: it is
aggregate-only and derived, and a path is never `DISABLED` because a parked destination produces no
path for the word to be about.

### Two things that get easier, and they are free

**Trap 14 does not apply.** `namespace_overlap` fires only in an exclusive namespace, so in a shared
one there is no losing request reporting an incumbent's path as its own. Every claim the ledger lists
is genuinely held. The view should still be built from `/v1/paths` and its `requests[]` rather than
from each request's `status.paths[]` — that is the ownership authority either way, and it is the one
read that has already done the deduplication — but the cross-check the matrix must perform is not
load-bearing here.

**It is two reads, not four** (§7a's last gap): `GET /v1/paths` and `GET /v1/requests?namespace=`.
Nodes are needed only for the request pane's destination editor and flows only if the unrouted strip
is carried over.

### Editing

Offer one mutation and no cell gestures: **edit the request document** — form or YAML, dry-run,
apply — plus delete, whose blast radius is now read off the ledger rather than computed. §7a's
consequence 2, the ambiguity between dropping a destination and splitting a source out, does not
arise, because there is no cell to clear: the operator is always editing one named request.

That is deliberately less than the matrix offers and it is the right amount. The matrix's gestures
are worth their cost because the grid *is* the desired set; here the desired set is a population of
independently-owned intents, mostly written by something that is not this UI, and the operator's job
is to read it and occasionally correct one document. Do not reinvent toggling on top of a list.

### Scale, and what to show first

An adapter-populated namespace can hold thousands of requests, so the ledger opens **summarised**, in
the landing page's idiom (§7): the counts line, then only the paths that are not `ACTIVE`, with the
full list behind a filter. That is the same instinct as `status` naming only what is not active,
applied one level down, and it is the reason this view scales where a grid of the same cardinality
does not. *The filter used to admit contested paths as well; sharing is not a condition, so state is
the whole of the criterion.*

### ~~It doubles as the conversion planner~~ — removed

***Superseded on 2026-09-01. It was built, verified end to end against a live server — including a
test that converted a scratch namespace and confirmed the server invalidated exactly the request the
plan named — and then deleted, because the premise under it was wrong rather than because it did not
work.***

What it was: the ledger holds every input the `(UpdatedAt, id)` precedence rule needs, so switching a
namespace to `exclusive` could be previewed with no write at all — "1 contested path → `cam1-pin`
becomes INVALID" named before the button rather than discovered after.

Why it went: it presents a shared namespace as a state to plan an exit from, and the correction at
the head of this section is that the state is the mode working. A view whose one mutation is
*convert away from what you are looking at* teaches the wrong thing about the mode it exists to
serve, and every operator arriving in the `default` namespace — which is `shared`, and auto-created
that way — would meet it. The conversion itself remains available; it is a namespace `POST`, and
`docs/open-items.md` is where a case for previewing it belongs if one is made again.

The two honesty notes it carried survive it and belong wherever a conversion is offered from: the
precedence is **recency, not incumbency**, so a request with an older stamp takes a path off one that
has been running for a week (§7b) — name the predicted loser rather than describing the rule — and
any prediction is a snapshot, since a write to either request between the plan and the conversion
changes the answer.

### It is also the right read view for an exclusive namespace

Nothing in the ledger requires `shared`. Inside an exclusive namespace every refcount within the
namespace is 1 by the rule, so the claims list degenerates into a plain path list — which is exactly
what `describe path` gives and is still worth having next to the grid. Across namespaces the refcount
can exceed 1 in either mode, since namespaces partition requests and not destinations (§7b), so the
`+archive` fact the matrix puts on a column header is the same fact this view puts on a claim line,
at a better altitude. **Build one component and point it at either mode**; do not write a second
path list for the exclusive case.

### What not to build here

- **A matrix with sharing markup.** §7b rejected it for the exclusive case and the axis argument
  above rejects it for this one.
- **A row per request.** It re-duplicates the path, which is the one thing this view exists to undo.
- **A graph.** Same objection as §7a: the endpoints are queries, and a shared namespace has more of
  them, not fewer.
- **Cell-shaped gestures on a list.** If a click in the claims list starts mutating requests, the
  ambiguity §7a consequence 2 has to ask about comes back with nowhere to ask it.

---

## 8. What is deliberately unavailable

Do not design a view that needs any of these, and do not add them to the API on your own initiative.

- ~~**Worker logs.**~~ **Built, in a narrower shape than this entry refused.** Architecture §12.2
  settles a byte-bounded tail of the last failing worker's output, pushed with the transition into
  `FAILED` and fetched from `GET /v1/paths/{id}/logs` — a tail attached to a failure, not the
  general log-retrieval facility this bullet was declining. Every constraint it lists is met rather
  than waived: the agent stays a client, the tail is captured from the pump that already reads every
  line, and it is the log of the worker that already died, which is the case that mattered. The
  disclosure question had an answer too — worker output does carry filesystem paths, and this
  endpoint is on the authenticated user API, where `/metrics` is not.

  **One entry carries one tail per crash loop, not one per restart**, and the event carries a
  `has_log` *marker* rather than the bytes. Fetching is a deliberate act and must never happen on a
  poll: inlining a few KiB per failure into a list a UI polls makes the cheap read expensive exactly
  when things are failing, which is when it is polled hardest.
- **Rates, grain counters, latency.** These are Prometheus metrics, not API state. `mxl_*` series
  (per flow: direction, domain, flow_id, session, plus flow-definition and user labels) are on the
  **agent's** `/metrics`, one per node. `mxl_repl_*` control-plane series (requests by status, paths,
  sessions by status, agents leased, epoch transitions per session, reconcile duration, store
  latency) are on the **server's** `/metrics`, unauthenticated. A UI wanting graphs needs a Prometheus
  behind it; it cannot scrape N agents from a browser and should not try.
- ~~**History or events.**~~ **Built** — see §2. "This flapped three times last hour" is exactly
  what a coalesced entry says, and `×47 over 6m` on one row is the rendering. `session.*.restarts`
  and the epoch-transition counter are still the *rate* answer and still live in Prometheus; the log
  is the *narrative* one, and the two are not substitutes.

  **It is a diagnostic aid and not an audit log**, and nothing may be built on it as though it were.
  The agent's queue is memory and a restart loses whatever is pending, a ring drops its oldest, and
  a leader change leaves a gap. Every loss announces itself — which is the property worth having —
  but the log is the best account two processes with bounded memory can give of what happened, not a
  record of it.
- **Bandwidth or capacity.** Admission control is a roadmap item; nothing computes committed
  bandwidth today.
- **Materialised domains with no flows.** `GET /v1/nodes/{node}/domains` reports what the agent
  observes, joined with label records. A domain a request just materialised, whose target worker has
  not created anything yet, is not observed and has no label record, so it is in no list — reach it
  through the path that targets it, which is where it has meaning anyway. *(This is a smaller gap
  than it was: once a flow lands, the domain is observed and listed like any other, because there is
  only one kind of domain now.)*

---

## 9. Running it locally, with data, and no MXL

You do **not** need the C++ worker, libfabric, RDMA hardware or a real MXL domain to develop the UI.
The agent API is ordinary HTTP, so a fleet can be faked with `curl`. `ui/prototype/devfleet.sh` does
it — it is the harness to use rather than anything hand-rolled:

```bash
go build -o /tmp/mxl-replicator ./cmd/mxl-replicator

# 1. the control plane, server role only, on sqlite
/tmp/mxl-replicator run --server \
    --server-listen 127.0.0.1:12999 \
    --server-store-sqlite-path /tmp/mxl-store.db \
    --server-heartbeat-interval 1s --server-lease-ttl 8s

# 2. five fake nodes with areas, inventory and labels, leases kept alive
S=http://127.0.0.1:12999 ui/prototype/devfleet.sh

# 3. the UI, same-origin with the API behind nginx
PORT=8080 API=127.0.0.1:12999 ui/prototype/serve.sh
```

`--server` means *server only*. It comes up on sqlite, settles in 3 heartbeats, and serves both
prefixes with no auth: `curl -s localhost:12999/readyz` → `{"leader":"…","status":"ok"}`.

**Use a port that is not 2283.** That is the default a real `mxl-replicator` listens on, and this
harness registers nodes through the *agent* API — point it at a fleet somebody is running and it
writes fake node registrations into their store, which are durable and have no deregister API.

What the fixture gives you, and why each piece is there:

| | |
|---|---|
| `studio-a` | produces, and grants `read` only — a source that can never be a destination. `media/cameras` holds Camera 1 video **and** audio, so a group-hint selector with no type matches two flows; Camera 2 is video only; one flow carries no hint at all. `media/audio` holds an idle Talkback |
| `studio-b` | the same shape, with its own Camera 1 — so a fan-in request over both studios is one intent with two sources, which is the arrangement §7a has to draw as a rectangle |
| `edge-01` | `media` read-only, `fast` and `bulk` read-write — two writable areas, **and** an observed domain `media/local`, so a node being both ends of a path is represented |
| `edge-02`, `archive-01` | one writable area each, the common case |
| labels | `role`/`name`/`studio` applied to every source domain, so `{labels: {role: cameras}}` selectors have something to match |
| `Studio A:Talkback` | seeded idle, so routing it produces a `PAUSED` path — the state a UI most often gets wrong |

`devfleet.sh` keeps running on purpose: leases need renewing, and a lease that expires *freezes* every
path touching that node, which is correct behaviour and baffling if you did not mean to cause it. It
re-registers when a heartbeat comes back `reregister`, so it survives the server restarting or
`rm /tmp/mxl-store.db` without being restarted itself.

Paths reach `ESTABLISHING` and stop, because nothing runs a worker. That is the useful fixture rather
than a limitation — it is the state an operator watches while something comes up. **To drive the rest
of the state machine**, read each node's `GET /agent/v1/{node}/assignments`, post back
`/agent/v1/{node}/status` snapshots with `state: "ready"` and an epoch (any string; only the peer
agent verifies it), and report the destination flow in that node's inventory with
`"producing": true, "replicated": true`. Doing that for some sessions and not others is how you reach
`PARTIAL` — which is worth building, because it is the state this document's §4 is most emphatic
about and the hardest to imagine. `internal/e2e/` and `internal/server/*_test.go` are the
authoritative examples of every one of those bodies; copy from them rather than from any document.

For an `INVALID` fixture, dry-run a request naming an area that does not exist:

```
{"code":"invalid_request",
 "message":"node \"edge-01\" advertises no area \"nope\", it has \"bulk\" (writable), \"fast\" (writable), \"media\" (read-only)",
 "details":{"reason_code":"unknown_area"}}
```

Note the `reason_code` lives under `details` on the 400 path, while on a `Request` object it is
`status.reason_code`. Two shapes for the same information.

For a `namespace_overlap` fixture: create the namespace `exclusive`, POST one request, then POST a
second with the same source and destination. The second comes back `INVALID` — and reports the
first's path as its own, which is trap 14 and worth seeing once.

**For a `shared` fixture — the one §7c is written against**, and the mirror of the above: the same
overlap in a `shared` namespace is not a contest at all, it is one path with two claims. Verified on
2026-09-01; `verify.mjs` seeds exactly this as its own precondition.

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

`GET /v1/paths` then reports six paths, one of which carries
`"requests": ["k8s/cam1-pin", "k8s/wall"]`. That single path is the whole of what §7c exists to
render honestly: `cam1-pin` holds one path, none of it solely, so deleting it stops nothing — and
the matrix has no way to say so.

**No browser is required to test the UI itself.** The prototype was developed and verified without
one, by loading the page into jsdom and driving it against the live server — real DOM, real fetch,
real reconciler, real store. `ui/prototype/verify.mjs` is that harness, 71 checks, and it needs one
dev dependency (`npm i jsdom@24`; newer releases pull an ESM-only transitive dep node 18 cannot
require). jsdom has no `<dialog>` and no form submission, so both are stubbed; everything else is
the shipped code.

It catches more than it looks like it will, and the class of bug is consistent: a class-name
collision, an ungrouped-sorts-last regression, a header-resize bug, and — in the rewrite for this
document — a dialog list left showing the *previous* node's domains, two per-node reads landing out
of order, and a domain selection carried across a reopen onto a node that does not have it. None of
those is visible in a unit test of anything, because each is the page's behaviour against a real
sequence of reads.

**The full-fat option**, if you want real media moving: `make all` builds the worker, then the README
quick start runs both roles on one host over `tcp` on loopback with `mxl-mock-src`/`mxl-mock-sink`
producing into a domain. You should not need it.

---

## 10. If the code lands in this repository

Match what is there. The house style is unusual and consistent: doc comments carry the *reasoning*
and the rejected alternative, not just the behaviour, and decisions that were genuinely open are
recorded where someone will hit them. `internal/api/request.go` and `internal/api/domainselector.go`
are good samples. If you write Go, put it under `internal/` — nothing here is a library for third
parties — and add dependencies only when the code that needs them lands.

`rewrite.md`, `rewrite-plan.md` and `rewrite.v0.md` are local-only planning documents, excluded via
`.git/info/exclude` rather than `.gitignore`, and `rewrite.md` is superseded by `docs/architecture.md`
— do not read it for current truth. **This file and `ui/prototype/` are neither excluded nor
tracked**, so they will land in the next `git add -A`. That is a decision nobody has made yet: the
prototype is arguably a deliverable and this document arguably is not. Decide it before committing
rather than by accident.

Two verification habits worth copying, because both are visible in the plan and both caught real bugs
that unit tests did not: check assertions against the running binary rather than against the
document, and when a live run contradicts a document, fix the document in the same change. This
document's §3 says the wire refuses a string domain selector because a live run refused it, and
§9.1's example body says otherwise.

---

## 11. Decisions to settle before writing code

*Settled:* **the UI is always same-origin with the API** — served by the server process or behind a
proxy fronting both. No CORS, relative URLs only, a dev-server proxy for development (§6).

*Settled:* **the primary screen is a routing matrix** — rows are sources, columns are destinations,
cells are the paths a pairing expands to, and **a request is a rectangle over them** (§7a). Both axes
are virtual: a row is a pair of selectors and a column is a domain that may not exist yet, and the
server materialises them into paths. A node graph is the topology *view*, not the editor.

*Settled:* **the matrix requires a namespace whose `paths` mode is `exclusive`** (§7b), because two
requests can otherwise hold one path and the grid has no honest way to draw that. The API's default
is `shared`, so this is an active choice on every create path and a precondition to check on every
namespace switch. A `shared` namespace is not the matrix greyed out: it gets **the ledger** (§7c), a
path-first view where a claim rather than an intent is the object — and not a prompt to convert away
from the mode, which is the one thing §7c had to take back out.

*Settled, and now built:* **`disabled` on a destination** (§3, §7a, §9.1). It decides whether the
matrix is a board or a live view — without it the axes are derived from routes that are currently
on, so switching one off deletes its row and column, and the prototype's session-scoped retention is
the measure of that gap. The API accepts and returns the field, the reconciler skips parked legs, and
a fully parked request reports `DISABLED`. **Use it from the first version of the grid rather than
retrofitting**: "the axes retain what you authored" is not a property that can be added later to a
renderer written assuming they cannot.

1. ~~**How does the browser authenticate** when a shared token is configured?~~ **Decided** (§6). The
   injecting proxy is still the recommendation and is unaffected; where nothing injects, a 401 raises
   a prompt and the token is kept in `localStorage`. The prompt is driven by the refusal rather than
   by the absence of a token, which is what keeps the recommended deployment from ever seeing it.
2. **Apply on click, or stage and take?** §7a. Staging earns its keep for a reason that was not
   obvious in advance: with a staged set, the preview can dry-run the whole batch and report real
   outcomes and blast radius before anything moves, and *that* is what removes every confirmation
   dialog from the interface. It earns more now that one click can light several cells of a
   rectangle. Still worth confirming against real operator habit — a router's take button is muscle
   memory.
3. **Stack.** Still open. The prototype is deliberately vanilla — no build step, no runtime
   dependencies — because the question it exists to answer is whether the matrix is the right shape,
   and a framework choice would have been an unrelated commitment made under cover of that one. It
   is ~2000 lines of plain JS and it is past the size where hand-rolled rendering pays: every poll
   re-renders the whole grid, which is fine at this scale and is the first thing that will hurt.
   Choose a stack on its own merits rather than by inheriting this one.
4. **Read-only first, or mutating from the start?** A read-only matrix is already most of the value —
   the whole desired set and its live state on one screen — and it needs neither of decisions 1 and
   2. A strong argument for shipping it first, and stronger than it was: the rectangle model has real
   editing subtleties (§7a, consequences 1 and 2) that a read-only view does not have to answer.
5. **Does the UI author fan-in, or only render it?** New, and worth deciding deliberately. Rendering
   it is mandatory — the CLI and the Kubernetes adapter will produce multi-source requests whether or
   not the UI can create them. Authoring it is what the source-list editing in §7a costs. Rendering
   first, authoring second, is a defensible split; pretending every request has one source is not.

Three smaller ones that can wait but should be raised rather than worked around: an ETag or revision
cursor so the matrix's polling is cheap (§2, §7a), the negotiated provider on `PathStatus` so a cell
can show it before apply (§7a), and `GET /v1/nodes/{node}`, which §9.1 documents and the mux does not
serve.

*All three now have a disposition, in `docs/open-items.md`.* The negotiated provider lands **with the
UI** (§2.10) rather than being asked for, since the cell preview is what wants it and `Compute`
already has the value. The ETag is worth asking for but **narrowly** and not on `/v1/paths` (§2.9).
`GET /v1/nodes/{node}` is unclaimed and is the only one nothing is waiting on (§5).
