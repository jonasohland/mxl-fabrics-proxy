# Open items

Loose ends left by the domain-labelling, namespace, conflict-precedence and **areas** work in
`docs/architecture.md` (§6, §7.2, §7.5, §9.1, §9.3, §10.6, §10.7, §10.8) and `ui.md` §7b.

This is a work list, not a spec. Everything here is either a contradiction inside the document, a
mechanism the design implies but does not describe, or a decision that was deferred rather than
made. Section references point at `docs/architecture.md` unless they say otherwise.

One thing is broken rather than merely missing (§1). Everything else is safe to leave until
someone is in the relevant section anyway.

**`disabled` is settled and built** (§2.8) — the one item that ran the other way round, a decision
written down before the code rather than a document catching up with itself. It is kept struck
through rather than deleted because writing it out first got two things wrong that the
implementation corrected, and both are the kind that read plausible.

**Domain labels and the label selector are now in scope** — change (4) of `docs/domain-changes.md`.
That closes most of §2 and §3 outright, since those items were deferred *to* the label work rather
than alongside it: a mechanism nobody was building needs no dry run and no ownership rule. Each is
struck through below with where it landed, and the argument is kept for the same reason every
superseded position in `architecture.md` is.

**Fan-in is now in scope too** — `sources` is a list, the flow selector has an `all` kind, and the
request aggregate has `PARTIAL` (§7.2, §7.5, §9.1, §10.7, §10.8, §11, §12, §18). It closes nothing
on this list outright and it *changes* three items rather than adding a section of its own: §1.2's
partition gained a code and held its rule, §2.6's `ui.md` pass got larger for the third time, and
§3.2 is now the only thing standing between this design and multipoint rather than one of several.
Two new accepted costs are in §4.

**The event log is built** — §12.1 and §12.2 of `docs/architecture.md`: a bounded per-object ring
anchored on the path, worker log tails pushed on failure, three read endpoints and two CLI verbs.
It closes nothing on this list and adds one item — §2.11, the UI support that does not exist — which
is a `ui.md` gap rather than a design one. *§2.11 has since been built too, and it is the one item
here that was a specification rather than a complaint: the five things it says a renderer gets wrong
are what `components/EventLog.vue` is organised around, and two of them were reachable on the
ordinary dev fixture.* Its one structural consequence is recorded in §4 of
`architecture.md` rather than here: the three state layers moved under a common root so that a
single range covers exactly the state, which is what keeps a diagnostic log out of every read and
out of the reconciler's watch.

**The UI is being built** — a Vue SPA under `ui/app/`, against `ui.md`. That gives a disposition to
two of the three server-side additions `ui.md` §11 raises as worth raising rather than working
around, and both are new items in §2. The negotiated provider on `PathStatus` (§2.10) lands *with*
the UI rather than being asked for, because the thing that wants it is the cell preview. The ETag
(§2.9) is worth asking for on its own terms and worth asking for **narrowly** — the endpoint
`ui.md` names for it is the one endpoint it cannot be sound on, which is most of what that item
says. The third, `GET /v1/nodes/{node}`, is in §5.

It has since added a fourth item of its own (§3.4), and that one is not a UI item however it looks:
the UI's manifest pane is postponed, and postponing it surfaced that **nothing in this tree renders a
manifest at all**. File → fleet is implemented three times; fleet → file is implemented nowhere, and
the first thing that needs it should probably not be a second implementation of the grammar in
another language.

---

## 1. Broken: fix before the sections are read as final

### ~~1.1 Chaining has no spelling~~ — **resolved**

*Landed in §10.6 and §10.7.* The item is kept for its argument, which is now the argument for the
unification rather than for a union type.

The problem was that §10.6 claimed chains work with no extra design while §10.7 removed the only
way to express one: in `A→B→C` the second request has to *name* the middle node's output domain,
and the two kinds of domain were identified differently — an input domain by absolute path, an
output domain by a rendered name under a root. The direct form of the source selector was
`{"path": …}`, which addressed only the first, and the label form deliberately could not match the
second.

The fix proposed here was a single `{"name": …}` field resolving either kind, held apart by a
leading separator. **What was written instead removes the two kinds.** A domain is
`<area>/<elements>` whichever direction it is used in (§10.6), so `{"name": "fast/ingest"}` and
`{"name": "media/cameras"}` are the same kind of string and there is no grammar to disambiguate.
The union stays two-kinded because the second kind is *selection*, which was always the real
asymmetry: you may **name** any domain as a source, but a label selector never matches a flow this
project is writing (§10.7).

`same_endpoint` stays, for the reason given here: `source: {name: fast/ingest}` against a
destination resolving to `fast/ingest` on one node is exactly the self-pair it exists to catch. It
is recorded in §7.2.

### 1.2 §7.2 contradicts itself about what refuses a POST

§7.2 now says validation is **per path** and that `POST` refuses only what is *structurally*
invalid — but the list of codes above it is still headed "Rejectable immediately", and nothing says
which codes fall on which side. As written the section asserts both dispositions for the same set.

**Fix: state the partition, which has a clean rule.**

> A code refuses the `POST` if it is decidable from the request plus node registrations. It
> invalidates one path and leaves the request's others alone if deciding it needs the flow
> inventory or other requests.

| Refuses the POST (400) | Invalidates the path (200, path `INVALID`) |
|---|---|
| `malformed_domain_name` | `flow_conflict` |
| `unknown_area`, `area_not_writable` | `loop` |
| `no_shared_fabric`, `no_shared_provider`, `no_shared_capability` | `namespace_overlap` |
| `pin_not_viable` | |
| `sched_prio_unavailable` | |
| `same_endpoint` | |
| `duplicate_source_flow` | |

*Both columns moved again with fan-in* (§9.1), and the partition held without amendment, which is
the useful thing to record about it. `duplicate_source_flow` is new and lands left because two
sources *pinning* one flow UUID against a shared destination is decidable from the request body
alone; the same collision arriving from a selector stays `flow_conflict` on the right. That split
exists because the rule is one disposition **per code**, not per occurrence — an early
`flow_conflict` for the decidable subcase would have given one code two dispositions, which is what
this table is for. `same_endpoint` stays left and is now checked over every `(source, destination)`
pairing rather than the single one; refusing the whole request for one bad pairing is still right
there, because with both ends named the author can see both halves of the typo.

Every code in the left column also survives fan-in *because sources stay enumerated*. A source
names its node outright, so both ends of every pairing are decidable from the request plus node
registrations. The moment a source's node becomes a selector — §10.8's multipoint — this column
collapses into the right-hand one, which is worth knowing before that work starts.

*Both columns changed with the areas work.* `no_output_root`/`unknown_output_root`/
`ambiguous_output_root` became `unknown_area`/`area_not_writable` and `domain_name_in_use` is gone
outright, since the area is now part of a domain's name and the collision it caught is
unconstructible (§10.6, §7.2). The partition itself is unaffected: every code that moved stays
decidable from the request plus node registrations.

The right-hand column is everything cross-request or fleet-dependent — precisely the set that a
selector's author cannot enumerate before submitting, which is why refusing the whole request for
one bad pairing is the wrong disposition there.

*`same_endpoint` stays in the left column with the label work in scope, but only just, and it is
worth knowing why it did not move.* A label selector matching the request's own destination domain
makes the pairing fleet-dependent, which by the rule above would push it right. Instead the pairing
is **elided** rather than refused (§7.2, §10.7): with a named source it is a typo and stays
decidable from the request; with a selector it is the selector working, and there is nothing to
report. The code therefore keeps one disposition rather than acquiring one per source kind, which is
the outcome the partition exists to produce.

**One framing correction the table needs.** Every code in the left column can *also* invalidate a
path in steady state — an area removed from a node's config after the request was accepted is
`unknown_area` on a live path, not a rejected `POST`. The left column is only the subset that
*additionally* refuses the write, and saying otherwise breaks §7.3's one-`Compute` property. The
governing reason for the cut is that a `POST` refusal must be actionable by its author.

`ui.md` §7a's three-outcomes table depends on this and is already updated for
`namespace_overlap`; the rest of the right-hand column belongs in its middle row too.

---

## 2. Mechanisms the design implies but does not describe

### ~~2.1 Label writes need a dry run~~ — **resolved**

*Landed in §9.1, and it is step 11 of `docs/domain-changes.md`.* The argument was accepted as
written and is kept because it is the reason the item survived being called polish: a label edit
joins or removes a domain from a request's expansion, so it **starts and stops media** exactly as a
request does — and removing a label tears down running sessions with no preview and no refcount
shown. §9.1 gives requests `?dry_run=true` precisely because they move uncompressed video; labels
do the same thing one level of indirection away, which makes it *easier* to do by accident, not
harder.

Both halves landed: `POST /v1/nodes/{node}/domains?dry_run=true` running the same `Compute` against
a candidate fleet, and `label --dry-run` plus a printed blast radius on the real thing.

**One thing was decided beyond what was asked for here: it prints rather than prompts.** The item
said "a printed blast radius" without saying whether the real write should also stop and ask. It
does not — the CLI is scripted by the same operators who use it interactively, and a verb that
blocks on a tty is a verb that hangs in a pipeline.

Note this is a *label* removing a path, not §4.2 freezing: the node is live, so nothing is frozen
and the teardown is immediate.

### ~~2.2 Manifest versus imperative: replace or merge?~~ — **resolved: neither, three-way merge**

*Landed in §9.1.* **An apply owns the keys it declares**: it sets them, removes the ones it declared
on a previous pass and no longer does, and leaves every other key alone. An imperative
`label domain … role=cameras` therefore **survives** the next apply that does not mention `role`.

**This item put up a false pair, and the reasoning it recommended is the part that was wrong.** It
argued: everything else in the manifest replaces — `POST` on a request replaces the whole spec — so
a `kind: domain` document should replace the entire label set, and it called this "the ordinary
kubectl tension" resolved "in the ordinary direction". That is a misreading of the vocabulary the
argument appeals to. `kubectl apply` is a three-way patch against a last-applied record: it does not
touch a field it never declared, and an imperatively-added label survives it. So the consistency
argument and the Kubernetes argument point in *opposite* directions, and the item was resolved on
the second, deliberately — this file format is close enough to a Kubernetes manifest that §19's
adapter is a mechanical conversion, and surprising an operator about what `apply` does to a field it
never mentioned costs more than internal consistency buys.

The consistency argument also turns out to be weaker than it reads, and §9.1 records why: a request
spec has **one writer by construction**, since `(namespace, name)` is its ID, so whole-spec replace
there is a description rather than a policy. A domain's label map has no owner at all — §10.6 spends
a subsection establishing that a domain is a shared place — so it is the one record in the design
where several writers on one key set is expected, and whole-set replace makes the last writer win.

Three things that follow, none of which the item anticipated:

- **The record grows a `Declared` key set** — `last-applied-configuration` reduced to what a flat
  map needs. Without a memory of what the file declared *before*, "remove what this apply no longer
  declares" is indistinguishable from "remove what it never mentioned".
- **`label` sends a patch, not a read-modify-write**, superseding §9.1's earlier description of the
  verb. RMW was only needed because the endpoint was a full-set write, and it has a lost-update race
  on precisely the record this decision identifies as multi-writer.
- **Two files naming one domain still fight.** One declared-key set, not one per writer — the same
  limitation `kubectl apply` had before server-side apply, failing the same way. Named field
  managers are the additive fix and are not built.

The item's stated worry — that operators will use the imperative verb for something they expect to
survive — is answered by making it survive rather than by documenting that it does not.

### 2.3 `UpdatedAt` must not be bumped by the server

§7.5 keys precedence on it, so any server-side rewrite of a request record — a normalisation, a
defaulted field, a re-serialisation — would reorder the whole fleet in one pass. Nothing does this
today now that the namespace migration is gone (major version jump), but it needs to be a stated
rule rather than an accident of the current code.

### 2.4 `README.md` contradicts the spec

It still documents `-m`, the old request routes and the pre-namespace CLI — and now also
`--search-path`/`--output-root` and path-shaped domain names. *The singular `source:` came off this
list when fan-in landed:* every example, the field table, the `get requests` sample and the
"one source, many destinations" section were rewritten with the code, because a README whose
manifests no longer parse is a different kind of wrong from one that is merely out of date.
`ui.md` §0 sends the UI author to it
as "the operator's current mental model", so it is load-bearing rather than decorative. This got
worse with the areas work, not better.

### ~~2.5 `replicated` has no user-facing surface~~ — **resolved**

*Landed in §6, §9.1 and §10.7, and it is step 11 of `docs/domain-changes.md`.* All three things
this item asked for are now required rather than wanted: the flag on `GET /v1/flows` and in
`describe domain`, and a per-request exclusion list naming the flows an expansion dropped to the
self-output rule.

The argument is kept because it is the one that makes this not-cosmetic, and it is the item most
likely to be dropped under time pressure. An operator whose broad selector quietly skips three flows
on an edge node has no way to see why — the flows are in `GET /v1/flows`, they match the labels, and
there is no field saying "this node is writing this one". Under the old rule the whole domain was
absent, which was at least legible as a category. **This is the cost of moving the guard from the
directory to the flow, and it is the one place where the finer granularity is *less* obvious rather
than more.**

Two shape decisions the item left implicit and §9.1 now states: "did not match the labels" is never
listed, because that set is unbounded and is the ordinary case; and the list is capped, with a
truncated one reporting how many it dropped, because a silent cap reads as "nothing else was
excluded".

### ~~2.6 `ui.md` §7a needs the renamed codes~~ — **resolved: `ui.md` rewritten**

*Done on 2026-09-01, and it was the `ui.md` pass this item kept predicting rather than the
find-and-replace it started as.* Every assertion in the new document was checked against a server
built from this tree and driven by `ui/prototype/devfleet.sh`, which was itself updated to register
areas instead of output roots. The argument is kept below because it is the record of what the three
waves of work cost a reader who was not in the room for them.

What landed, in the order the waves arrived:

- **The codes.** `no_output_root` / `unknown_output_root` / `ambiguous_output_root` → `unknown_area`
  / `area_not_writable`; `domain_name_in_use` kept with the narrower nesting meaning; `same_endpoint`
  and `duplicate_source_flow` added. The root picker is gone from the create form rather than
  renamed — the area is part of the domain's name, so it is part of naming rather than a setting
  beside it.
- **The label work.** A source that is a *set* of domains, the `excluded[]` list with `self_output`,
  and label writes as a first-class mutation with `?dry_run=true` and a `stopped[]` / `started[]`
  blast radius. "Every mutation is still dry-run first" now means two mutations, and says so.
- **Fan-in.** `ui.md` §7a's central claim — "a request *is* a row" — is superseded rather than
  patched: **a request is a rectangle**, sources × destinations, and a single-source request is a
  1×N one, so the common case is unchanged. The three consequences this item listed are each
  discharged where they land: the node bands are justified in both directions, `PARTIAL` is the
  rectangle's word and is never expected on a path, and §7b's overlap table is marked still-correct
  but no longer complete. The framing that made it hold together is that **both axes are virtual** —
  a row is a pair of selectors and a column is a domain that does not exist yet — so the matrix
  never wires anything and the cell is the only place real objects appear.
- **Exclusivity is now a precondition**, not a preference: the matrix is an editor only over an
  `exclusive` namespace, and a `shared` one renders read-only with an offer to convert. Two facts
  found by driving the server made that sharper than the old text: the API's default is `shared` and
  `default` is auto-created that way, and the rule is enforced on *materialised* paths, so two
  requests with one source and one destination are both accepted while the selector matches nothing
  — one cell, two owners, until a producer appears.

**`ui/prototype/` was brought along with it**, so the document and the thing it describes agree:
rows are sources, a request is a rectangle drawn by an accent rather than a border, a `shared`
namespace renders read-only with an offer to convert, and the source editor takes a domain
*selector* — with the flows a label selector will skip marked before the request is written.
`devfleet.sh` registers areas, and `verify.mjs` drives the shipped page headlessly against a live
server: 71 checks, all passing.

That last one paid for itself immediately and is the reason it is recorded here rather than in the
prototype alone. Three bugs it found, none of which is visible in a unit test of anything: a dialog
list left showing the *previous* node's domains until the new read landed — invisible as staleness,
because domain names repeat across nodes; two per-node reads landing out of order, so the node the
operator had already left won; and a domain selection carried across a reopen onto a node that does
not have it. Each is the page's behaviour against a real sequence of reads, which is the only place
they exist.

The original item, kept:

`no_output_root` / `unknown_output_root` / `ambiguous_output_root` → `unknown_area` /
`area_not_writable`, and `domain_name_in_use` is gone (§7.2, §1.2 above). `ui.md` §7a's
three-outcomes table enumerates them.

**Larger now that labels are in scope**, and it should be treated as a `ui.md` pass rather than a
find-and-replace. §7a's matrix has no notion of a source that is a *set* of domains, and the label
work gives it three things to render that it currently cannot: a row whose source expanded by label
rather than by name, a flow excluded from that expansion with a reason (§9.1), and a label edit that
is itself a mutation needing the dry-run treatment §7a already requires of every other mutation. The
last is the one that matters: `ui.md` §7a says "every mutation is still dry-run first", and a label
write is now a mutation.

**Larger again with fan-in, and this time the document's central assumption is what moved.**
`ui.md` §7a's "Why it fits: a request *is* a row" builds the whole matrix on "a request is **one
source with a selector, and a list of destinations** — the list is on the destination side and
deliberately cannot be on the other". It now can be. A request with several sources is not a row.
Three consequences, none of which is a rename:

- **The grouping argument inverts along with §9.1's fourth.** §7a's "Group both axes by node" cites
  "one source to five destinations is 5× egress on one node" as what makes the node bands
  meaningful. A fan-in request is 12× *ingress* on one destination node, so the same argument now
  runs down the columns as well as along the rows, and a grid that only makes one of them legible
  renders half its requests badly.
- **`PARTIAL` is a new row state** (§11), and it is the only state that appears on a request and
  never on a path — so §7a's "click a lit cell to see its path" has, for the first time, a state
  with nothing underneath it to show.
- **§7b's opening reasons about "two selectors over one source domain"** to establish when two rows
  are one edge, refcount 2. Still correct, no longer complete: two selectors in *one* request, over
  two source domains on two nodes, now reach one destination flow — which is not one edge but
  `duplicate_source_flow` or `flow_conflict` (§7.2), a collision the grid has no way to draw. (That
  passage also still spells path identity with "plus the resolved output root", which the areas work
  removed — §5.4.)

### 2.7 `sources[].domain.name` is documented as a string and is not one

Found by driving the server while rewriting `ui.md`. §9.1's example request body spells a source's
direct domain selector as `"domain": { "name": "media/cameras" }`, and `internal/api/domainselector.go`'s
own doc comment repeats it. The wire type is `Name *Domain`, so the string is refused:
`source.domain: json: cannot unmarshal string into Go struct field plain.name of type api.Domain`.

The **code is right and the prose is wrong**, which is the direction that matters: the structured
form is what makes "parsed at exactly one boundary" true, and the string is the manifest's spelling.
Two lines to fix, both in documentation, and worth fixing because the example body is what an API
client will copy — the manifest example three pages further down is the one place the string form is
correct, so the two read as though the wire accepts either.

### ~~2.8 `disabled` is designed and not built~~ — **resolved: built**

*Landed as written, and the item is kept for the two things the writing got wrong.*

It was the one item on this list where the document was ahead of the code. §9.1 settles a `disabled`
flag on a destination entry, §11 the `DISABLED` aggregate that follows, and §7.2, §9.3, §12 and §18
the consequences; all of it now exists, with `README.md` documenting the manifest field and `ui.md`
carrying it as built.

**Two corrections the implementation forced**, both recorded because the plausible version is the
wrong one:

- **The overlap contest is decided by recency, not incumbency.** §9.3 originally said a re-enabled
  request loses because "incumbency is the first term of §7.5's order". It is not: `namespaceOverlaps`
  orders legs on `(UpdatedAt, id)` and consults no session record, and it *cannot* use incumbency —
  two requests contesting one path both map to the same path, whose session exists whoever holds it.
  The practical claim survives because un-parking is a write and every write is stamped, so a
  returning request always carries the newest `UpdatedAt`. §9.3 and `ui.md` §7b now say so, and
  `TestParkingReleasesThePathToAnotherRequestInAnExclusiveNamespace` pins both halves — including
  that an older stamp takes the path back off a request that has been carrying it.
- **`omitempty` plus a reused decode target keeps a stale `true`.** A re-enabled destination comes
  back with no `disabled` key, so anything decoding a poll *over* its previous response — which is
  what `json.Unmarshal` does to a slice's existing elements — shows the leg parked forever. It caught
  a test in this tree before it caught anybody else, and it is now `ui.md` trap 15.

The surface, as it was written:

- `api.Destination` gains the field, spelled `disabled` and **never `enabled`** (§9.1). `Validate`
  keeps counting entries rather than enabled ones, and the duplicate-endpoint rule keeps applying to
  parked entries — both are rules that look like oversights and are not, so both want a comment
  saying so and a test pinning it.
- Expansion skips disabled destinations before pairing, in `internal/server/reconcile`. That is the
  whole of the behaviour: no assignment path, no agent change, no worker change, nothing below the
  request.
- `api.RequestStates()` gains `DISABLED`; `api.States()` does not. `reconcile_test.go` already
  asserts that shape for `PARTIAL` and the same pair of assertions is what pins this one.
- The status fold reports `DISABLED` when no enabled destination remains, and `status` counts it on a
  line of its own rather than among the non-active list (§11).
- The CLI manifest parser accepts `disabled: true` on a destination and round-trips it; `describe
  request` shows it; §9.1's "an apply that omits the flag enables the leg" is the behaviour that
  falls out of create-or-update and wants a test rather than code.

Both of the things flagged as worth checking held. A parked destination does release its path for
another request to claim in an exclusive namespace — though for the reason corrected above rather
than the one predicted — and a request whose every destination is parked stays distinguishable in the
fold from one whose selectors match nothing: the first is `DISABLED`, the second `WAITING`, and
`TestParkedIsNotTheSameAsWaiting` holds them apart.

### 2.9 The UI polls, and every poll is a full reconcile

`s.view()` (`internal/server/userapi.go:32`) is `state.Load` — one `List("")` over the whole store —
followed by `reconcile.Compute` over the result, on **every** user-API GET, whatever was asked for.
§7.3 wants exactly that: the read handlers and the reconciler run the same function over the same
snapshot, so what an operator is shown and what the fleet is being told to do cannot drift. The cost
is that a read is O(fleet) rather than O(response) — and §9.1 gives the user API no watch, no stream
and no revision cursor, so a UI polls. The workspace of `ui.md` §7a needs four reads per cycle
(requests, paths, nodes, flows), which at the prototype's 2 s cadence is roughly two full reconciles
a second per open tab, multiplied by tab count.

**The ETag value already exists and is thrown away.** `state.Fleet` carries `Revision`
(`state.go:224`) — the store-wide counter `List` returns, advanced by exactly one per mutating write
(`store.go:108`). Nothing needs to be hashed or derived; it is computed on the read path today and
discarded.

**Two properties make it viable, and both were decided for other reasons.** A heartbeat deliberately
writes nothing (`state.go:62`), so a quiet fleet has a quiet revision — the obvious objection, that
lease renewal would churn the counter several times a minute per node forever, is the thing that
decision already bought off. And a lease *expiring* does advance it: `sweepExpired` revokes each
expired lease "each at its own revision" (`sqlite/lease.go:186`), and etcd deletes lease-attached
keys the same way, so a node going dark invalidates the tag rather than being cached through.

**What decides the shape is that `Compute` is not a pure function of the revision.**
`reconcile.Config` takes an injectable clock (`reconcile.go:85`) and uses it for idle teardown and
session ages, so two reads at one revision can legitimately differ — a path crossing its
idle-teardown threshold transitions with no store write behind it.

*That inverts the endpoint, and it is the reason this item is written down rather than just done.*
`ui.md` §2, §7a and §11 all name `/v1/paths` as where an ETag belongs, and it is the one endpoint it
cannot be sound on: paths carry all of the time-derived state, so a revision-keyed `304` would
freeze exactly the transitions the read exists to report. The sound targets are **`/v1/nodes` and
`/v1/namespaces`** — capabilities, areas, grants and namespace records are pure store state that
changes on the order of days, and they are two of the workspace's four reads. Paths and requests
keep recomputing, which is what they are for. An ETag on paths is still constructible with a bounded
max-age, or a coarse time bucket folded into the tag, and that is more machinery than the reconcile
it saves.

**It saves the `Compute`, not the `List`.** Nothing on the `Store` interface reports the current
revision cheaply — `List` is the only thing that returns one (`store.go:119`) — so the handler still
reads the whole store to learn whether it may answer `304`. On a fleet of any size that is the
expensive half, so the win is real, but it is a halving rather than an elimination and should be
described as one. A `Revision(ctx)` accessor is trivial on both backends (etcd hands one back on any
read; sqlite is a `SELECT MAX(rev)`) and is an interface change with a conformance suite behind it —
a separate decision, and not needed for the narrow version above to be worth taking.

Not built.

### 2.10 `PathStatus` does not carry the negotiated provider — **in scope with the UI**

`PathStatus` carries id, source, destination, state, reason, reason code and session id
(`internal/api/status.go:184`) and no interface config, so *which* provider a leg actually got is
answerable only after apply, from `GET /v1/paths` → `path.session.interface`. §10.4 makes that a
performance cliff rather than a detail: landing on tcp when verbs was preferred has a symptom that
reads as a source problem, and the reason a pin is never substituted is that the silent version is
the dangerous one.

It matters **only when a fallback list is in play**. A hard pin either works or is refused at
admission (`pin_not_viable`), so there is nothing to disclose. `provider: [verbs, tcp]` — prefer
verbs, tcp acceptable — is the case with an answer worth reading and no way to read it.

**The value is available where the status is built**, which is what makes the change small.
`Compute` negotiates before it emits: `negotiated.Fabric` and `negotiated.Interface` go onto the
session record (`reconcile.go:1207`), and `emit` carries the session onto `api.Path` while
`PathStatus` keeps only `SessionID`. Copying the pinned `(provider, fabric)` onto `PathStatus` adds
no computation and no read.

The consequence worth having is that it then works in a **dry run**. `?dry_run=true` runs the same
`Compute` against a candidate fleet, so a cell in `ui.md` §7a can report what a leg would negotiate
*before* it is applied — which nothing shows today, and which is the half `ui.md` records as a gap.
The negotiation *failures* already surface (`no_shared_fabric`, `no_shared_provider`,
`no_shared_capability`), so what is missing is the disclosure on success.

**Lands with the UI implementation** rather than being asked for separately: the preview is the
thing that wants it, and the change is two fields on one struct.

### ~~2.11 The event log has no UI at all~~ — **resolved: built**

*Landed as `components/EventLog.vue`, `model/events.ts` and the four reads in `api/client.ts`, on
the three detail views. `ui.md` §2 and §8 are corrected — both said there was no event log and no
worker log, which was the half of this item that was actively misleading rather than merely
missing.*

**The five traps were the specification**, and treating them that way is what made this one
component rather than three panes: each is a decision taken in §12.1 that is invisible from the
JSON, so three renderers would have been three chances to get each of them wrong. Every one is
pinned by a test in `model/events.test.ts`, and two of them turned out to be reachable on the
*ordinary* fixture rather than in a constructed case — which is the useful thing to record, because
both would have shipped:

- **`severity` is not the state vocabulary** — the fixture's idle camera arrives as `PAUSED` with
  `severity: info` on the first screen anyone opens, so a renderer coloured from `state` paints a
  routine event as a fault immediately rather than eventually.
- **The cursor is a per-ring sequence** — and the consequence is sharper than "do not render a
  timeline". A request's merged view returns *several entries all stamped `seq: 1`*, because the
  rings it merges number independently. Vue reuses a DOM node for a repeated key, so a row keyed on
  the sequence silently renders one of five. That is the same class of loss §12.1 records finding in
  a live fleet when coalescing dropped three of four flow names, and it was found the same way.

**Three decisions the item did not anticipate**, each recorded where it is made:

- **No `?since=` cursor, ever.** The endpoints take one and this UI reads the whole ring every time.
  Coalescing *rewrites the last entry in place with a new sequence number*, so an incremental poller
  is handed the same row twice and the only way to dedup it is to reimplement `coalescesWith`'s key
  in TypeScript — §3.4's argument about the manifest grammar, for a bounded fifty entries.
- **The affordability is recorded rather than spent.** The item's "half worth being pleased about"
  is real — these are the first O(response) reads — but the pane still rides the fleet clock and
  `ui.md` §2's one-timer rule holds without an exception. A second cadence is the thing that gets
  copied to the next component that cannot afford it.
- **The node pane renders for a node that is not registered.** The endpoint answers for one, because
  a node's log outlives its paths and its lease — so the pane sits outside the "no such node"
  branch. It is the page an operator lands on holding a name that is no longer in `/v1/nodes`, and
  the only place left that says what happened to it.

**Placement went the way the item pointed: per-object panes, no fleet-wide stream** — and the reason
is firmer than "later". There is no endpoint for one: the rings are per-object by design and the
fleet ring is merged into object reads rather than served on its own, so the only client-side
construction is a fan-out read over every path, which costs more than the full reconciles §2.9 is
about. Health names what is not active and every row reaches an object whose log is one click away.
If that stops being enough, the answer is a server endpoint and not a UI loop.

**One thing it cost outside its own files**, and it is worth knowing before the next section lands
anywhere: `Detail.live.ts` selected the node view's paths table as *the last `.dt-table` on the
page*, which it stopped being. Fixed by selecting on the `role` column instead. A positional
selector on a view that grows sections fails for the next author rather than for a bug.

The original item, kept:

`docs/architecture.md` §12.1 and §12.2 are built and `ui.md` predates all of it. Nothing in the
described workspace renders an event, and the surface with no consumer is now sizeable: three
`events` reads (path, request, node), a `logs` read for worker tails, a fleet ring merged into every
answer, and the `events` and `logs` CLI verbs that are currently the only way to see any of it.

**The reason to write this down rather than leave it to whoever builds the panel** is that a renderer
which treats these as an ordinary list gets five things wrong, and each of them is a decision taken
in §12.1 that is invisible from the JSON:

- **A coalesced entry is one row, not `count` rows.** `count`, `first_at` and `at` mean *this
  happened N times between these two moments*; expanding them re-creates the flapping the ring
  exists to compress, and does it in the UI where the ring can no longer protect anything.
- **`has_log` is a marker, not content.** The tail is a second fetch on purpose, so that the list a
  UI polls stays cheap exactly when things are failing (§12.2). A renderer that eagerly fetches
  every tail undoes the reason the split exists.
- **`severity` is not the [api.State] vocabulary and must not be coloured from `state`.** §12.1
  settled that designed behaviour never warns — an idle teardown, a queued start, a producer
  stopping — so a row whose state is `PAUSED` is routinely `info`. Colouring from the state instead
  reproduces exactly the board-full-of-false-faults §11 avoids twice.
- **The cursor is `next`, a per-ring sequence, never a timestamp.** Entries are stamped by whoever
  emitted them, so a merged request view interleaves two agents' clocks and a leader's. Rendering
  one as a causal timeline invites an operator to read ordering across hosts that is not there.
- **`dropped` and an `events_dropped` entry are different losses.** The first is history aged out of
  a full ring, the second is entries an agent never managed to report. Both need saying, and saying
  them the same way loses the distinction that matters — only one means something was never seen.

**The opportunity, which is the half worth being pleased about.** The `events` reads are the first
user-API reads that are O(response) rather than O(fleet): they are one `Get` on one key and do not
run `Compute` (§9.1). §2.9 of this file is entirely about the cost of the opposite, so a workspace
that polls events fast and everything else slowly is now a coherent thing to build, where before
every poll of anything was a full reconcile.

**What is genuinely undecided is placement, not mechanism.** A per-object detail pane is obvious and
`describe` already does it — path, request and node each render their last eight entries under the
status. What is not obvious is whether the workspace wants a *fleet-wide* stream, which is a
different object: there is no endpoint for it, the rings are per-object by design, and building one
would mean either a fan-out read or a new key that duplicates what the rings already hold. Worth
deciding before it is built by accident.

~~Not built.~~ *Decided above: the panes, and no stream.*

---

## 3. Decisions deferred

### ~~3.1 Label selector semantics~~ — **resolved**

*Landed in §10.7.* Equality-only, all keys ANDed, exactly as proposed: it is the obvious v1 and it
matches §9.1's tagged-union extensibility discipline — `in`, `notin` and `exists` stay additive.

**What was written adds the reason the restriction is load-bearing rather than merely minimal.**
Those operators arrive later as a *third union kind*, not by widening what a map value may say.
Widening the value grammar is the move that cannot be taken back: a request whose value happened to
look like an expression would change meaning under the upgrade, silently and in the direction of
matching *more*, which is the wrong way to be wrong for something that moves uncompressed video. A
new kind cannot do that, because no existing request has it set. That is `api.Selector`'s argument
applied to the second union, and `internal/api/selector.go` already carries it in prose.

~~**An empty label map must be refused.**~~ **Resolved** by the manifest's scalar-vs-map rule: a
scalar `domain:` is a name and a map is a label set (§9.1), so `domain: {}` is a label selector with
no keys and is refused as one rather than needing a rule of its own. The reason it mattered stands —
a selector matching every domain on the node expands a request's source set — so the refusal has to
be in the validator, not only in the prose. §10.7 now says exactly that, because the syntax rule
disposes of the spelling and refuses nothing.

### 3.2 Node labels do not exist

**Still open, and deliberately not pulled in with the label work** — `docs/domain-changes.md` §13
says so explicitly now, which is half of what this item asked for.

§10.8's falsification of §9.1's *"the destination side cannot have a selector"* is stated as
*"once nodes carry labels"* — and nothing carries node labels today. Domain selectors do not need
them (`source.node` stays pinned), so the deferral is correct, but §10.8 currently reads as though
the mechanism is available. Say that it is not, and that designing it is the first step of anything
in that section. **§10.8 now says so**, which closes the half of this item that was about the
section's phrasing; what stays open is the mechanism.

**Fan-in raised what this item is worth, and it is worth reading the change carefully rather than as
promotion.** The cross product is built (§9.1), and two of §10.8's three mechanisms landed with it,
so node labels went from one of several missing pieces to the *gate*: multipoint is now exactly
"either end is selected rather than named", and the source half of that is node labels. That does
**not** make it more urgent. It makes it the item that must not be built casually — every rejection
code in §1.2's left column is decidable today only because a source names its node, and a node-label
selector on the source side collapses that column into the per-path one on the day it ships. The
first step is still designing it; the second is deciding what §7.2 looks like afterwards.

Worth noting what *did* move: §10.8's `same_endpoint` argument — that a self-pair produced by
construction rather than by typo should be elided rather than refused — landed early, because a
label selector matching the request's own destination domain reaches it without multipoint (§7.2,
§10.7). The rest of §10.8 is untouched.

### ~~3.3 Should search paths be advertised?~~ — **moot**

*Landed in §10.2.* The question was whether §10.2's rule ("something is a capability iff the server
would make a wrong decision without it") bends to carry a diagnostics-only field, since the server
made no wrong decision without search paths but could not explain why a label outside one was inert.

It does not have to bend. A domain's identity is `<area>/<elements>` (§10.6), so a server without
the area table cannot render a domain's name, resolve one in a request, or say which area a label
landed outside of. Readable areas pass §10.2's own test, and they are advertised with their grants
because "may this name be a destination" is request-time validation.

### 3.4 Nothing renders a manifest, and the UI's manifest pane is postponed

`ui.md` §7a asks the workspace for a panel showing the current namespace as the multi-document YAML
file, copyable, with the staged changes as a diff against it, and it argues the point in the strongest
available terms: *"a UI that only ever produces requests through its own controls quietly creates a
second source of truth for the desired set"*. Rendering the file makes the UI teach the format
instead of competing with it, turns "I worked it out on screen, now commit it" into a copy rather
than a re-derivation, and is the honest answer to "how do I do this for forty cameras" — the answer
is a file, and the operator has just been shown what one looks like.

**Postponed deliberately, and the postponement is cheap for one specific reason**: the staged set the
grid's gestures build (`ui/app/src/stores/staging.ts`) *is* a diff already — one effective spec per
touched request, recomputed from the stored one on every read. So the pane is a renderer over state
that exists rather than a feature the model has to grow a place for, and nothing about the mutation
design has to be decided differently in the meantime.

**The cost accepted meanwhile is larger than "a panel is missing", and it is worth stating in the
direction it actually runs.** File → fleet is implemented three times over — `internal/manifest`,
the wire, and now the UI's own controls. **Fleet → file is implemented nowhere.** `internal/manifest`
is a parser (its one `yaml.Marshal` is a re-encode inside the strict decoder, not an emitter) and
`get` has no manifest output. So an operator who lays a board out on screen has no way to obtain the
file that belongs in git except by writing it by hand from what they can see — which is precisely the
second-source-of-truth failure `ui.md` names, arriving through the gap rather than through the UI's
ambition. It was survivable while the UI was read-only. It gets worse the moment the editors land,
which is what should raise this item's priority.

**When it is built there is a fork, and the obvious half is the worse one.** A renderer inside the UI
is a *second implementation of the manifest grammar*, in a second language, with the Go parser as the
authority and no compiler between them — and the grammar deliberately differs from the wire in three
places, each of which is a way to be silently wrong: the flow selector is **flattened** onto the
source entry where the wire nests it under `select`, the domain is a **scalar or a map** where the
wire is `{name: {area, elements}}` or `{labels: {}}`, and an **omitted flow selector means
`{all: true}`**, filled in by the CLI and never by the server. A pane that got any of those wrong
would display a file that does not parse, or worse, one that parses into a different request.

The better half is an **emitter in Go beside the parser**, exposed as `get -o manifest` (and
therefore available to the UI as an ordinary read): one implementation of the grammar, in the language
that owns it, in the same package as the parser that must agree with it — where a round-trip test
(`parse(render(doc)) == doc`) is a unit test rather than a cross-language integration concern. It
also gives the CLI something it does not have and that operators will ask for on its own merits,
which is the usual sign that the seam is in the right place. It is the same shape of argument as
§2.10: put the value where it is already known rather than recomputing it at the far end.

**Not decided here** — the pane is postponed, not designed. What is decided is that if it is built as
a TypeScript renderer, it owes a round-trip test against the real parser, because the three
divergences above are exactly the kind that read plausible.

---

## 4. Accepted costs worth recording

Not problems to fix — consequences that should be in the document so they are read rather than
discovered.

- **Renaming a node orphans every domain label on it.** Under `-m` the names lived in the same file
  as the host configuration, so rebuilding a node carried them along; they are now control-plane
  state keyed on `(node, domain)`. The orphaned records are visible in
  `GET /v1/nodes/{node}/domains`, which is the mitigation, but nothing moves them. **Renaming an
  *area* does the same thing** to the labels on its domains, and for the same reason — recorded in
  §10.6.
- ~~**Moving a node's MXL area re-identifies every domain on it.**~~ **No longer true.** Identity is
  `<area>/<elements>`, so repointing an area's directory while keeping its name preserves every
  domain identity on the node, and paths and sessions survive the restart instead of rebuilding
  (§10.6, §5.4). Flows left in the old directory are leaked and nothing moves them, which is the
  operator's problem to sequence. Moving a domain to a different *area* still re-identifies it, and
  should.
- **A node's full recovery is now measured in minutes, not seconds** (§6.3, §6.1). Rate control on
  worker starts keeps 1–2 s as the budget for re-establishing *a flow* and gives a fifty-flow node
  well over a minute to re-establish all of them under the shipped defaults. Deliberate — the
  failure it buys off takes the whole node down rather than slowing it — and worth recording
  because §6.1 reads as a promise about agent restart in general and is now a promise about one
  flow. Two things follow that nothing does yet: the *server* has no notion of a node being
  deliberately paced, so a bulk re-establishment renders as a long tail of `ESTABLISHING` paths
  with no explanation on the path itself (the explanation is in the agent's metrics and in the
  session's reason, §12); and nothing sizes the default against the host, so it is a number an
  operator has to tune from a symptom.
- **There is no preemption.** Incumbency leads in §7.5, so a request already holding a path keeps it
  regardless of how much better a competing request is. Handing a path to a different request means
  deleting the incumbent first. Deliberate, and worth saying once, because "why will my new request
  not take over" is the question it generates.
- ~~**The source-side `domain` metric label now publishes host filesystem paths**~~ **Fixed.** The
  label carries `<area>/<elements>` on both sides now, so `/metrics` exposes an operator-chosen name
  rather than the node's directory layout. Recorded in §12 and in §13's list, since the endpoint is
  commonly unauthenticated.
- **A conflict between two paths of one request resolves on an arbitrary term.** §7.5 orders on
  `(incumbency, UpdatedAt, id)`, and two paths of one request share an `UpdatedAt` — so before
  either establishes, the tie falls through to the path ID. Deterministic and stable, which is what
  the order must be, and arbitrary from the operator's point of view, which no ordering can fix:
  nothing in the request says which of two sources of one flow ID was meant. Fan-in makes this
  reachable in ordinary use rather than exotically. The decidable form is refused at `POST`
  (`duplicate_source_flow`), so what reaches the tiebreak is exactly the set that could not have
  been caught earlier, and the message names both sources. Nothing to fix; somebody will ask why one
  of their two studios won.
- **`sources` invalidates every stored request and every manifest.** The list has no singular
  spelling beside it (§9.1), so a file or a record written as `source:` is refused rather than
  migrated. It rides along with the areas break in the same major version (§16) and belongs in the
  same release notes — it is a smaller break than the domain re-identification below, but it is the
  one an operator hits first, because it is a parse error in a file they are holding.
- **Every domain identity in the fleet changes with the areas work**, so the upgrade invalidates
  every stored path, session and label record, every manifest and every dashboard query built on the
  old spelling. §16 already takes a major version jump with no config compatibility, so this rides
  along rather than needing a migration — but it is the largest single break in the document and
  belongs in release notes rather than only in §10.6.

---

## 5. Small

- ~~**Namespace name grammar**~~ **Resolved.** §9.3 now carries it (letters, digits, `-`, `_`,
  non-empty), with the reason it is constrained where an ordinary label value is not: it is a path
  segment in a URL, a store key and a `-n` argument.
- ~~**`GET /v1/nodes/{node}/domains` is inventory-dependent now.**~~ **Resolved:** it carries the
  `settling` flag the way `GET /v1/paths` does (§9.1). The failure this avoids is worth keeping —
  during the window it would otherwise render every label with nothing observed beside it, which
  looks exactly like the labels having been lost. A second rule landed with it: the endpoint answers
  for a node with no registration at all, so a typo'd node name in a manifest is not a write that
  can never be read back.
- **`GET /v1/nodes/{node}` is documented and not served.** §9.1 lists it; the mux does not register
  it and it returns `404` — verified while rewriting `ui.md`, which tells its reader to fetch the
  list and filter, as `describe node` already does. Either serve it or strike it from §9.1, because
  the current state is a route an API client will write against and get a 404 from. The third of the
  three additions `ui.md` §11 raises, and the only one nothing is waiting on.
- **`--server-config` is accepted and ignored.** `ServerOptions.Config`
  (`cmd/mxl-replicator/server.go:20`) declares a repeatable `type:"existingfile"` exactly as the
  agent's does, and kong parses it, but nothing reads it: there is no `config.LoadServer`, and
  `internal/config` loads the agent only. So the file has to exist, which makes the flag look like
  it works, and every value in it is silently discarded. That is the worse half of the failure —
  a flag that rejected an unreadable path and then ignored a readable one is harder to notice than
  one that errored outright.

  Either serve it or strike it, the same disposition as `GET /v1/nodes/{node}` above. Serving it is
  the better half if the server ever grows a list-shaped setting, which is the reason the agent has
  a file at all (`internal/config/agent.go`): `--server-provider-order` is already a list, and the
  etcd endpoint list is spelled as a repeated flag today. It would also let the Helm chart hand the
  server a ConfigMap the way it already hands one to the agent, instead of a flat argument list.

  Nothing depends on it, so this is safe to leave. It is listed because the flag is documented by
  its own help text and an operator who uses it gets no error.
- **An auto-created namespace from a typo is indistinguishable from a deliberately empty one**
  (§9.3). Both are inert and cheap; `DELETE` is available. Probably nothing to do, but somebody will
  ask. **The same is now true one level over**: a domain label on a node or domain that does not
  exist is accepted and inert by design (§10.7), and nothing collects it. The mitigation is the same
  and is the read side — it is listed, so it is visible rather than merely harmless.
