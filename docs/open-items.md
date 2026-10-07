# Open items

Loose ends left by four pieces of work in `docs/architecture.md`: domain labelling, namespaces,
conflict precedence and **areas** (§6, §7.2, §7.5, §9.1, §9.3, §10.6, §10.7, §10.8), plus `ui.md`
§7b.

This is a work list, not a spec. Each entry is one of three things: a contradiction inside the
document, a mechanism the design implies but does not describe, or a decision that was deferred
rather than made. Section references point at `docs/architecture.md` unless they say otherwise.

§1 lists what was broken rather than merely missing; both of its items are now resolved.
Everything else can wait until someone is working in the relevant section anyway.

How later work changed this list:

- **`disabled` is settled and built** (§2.8). It is the one item that went the other way round: the
  decision was written down before the code existed, instead of the document catching up with the
  code. It stays on the list, struck through, because the written version got two things wrong
  that the implementation corrected, and both wrong versions sound plausible.
- **Domain labels and the label selector are now in scope.** That closes most of §2 and §3, because those items had been deferred
  *to* the label work: a mechanism nobody is building needs no dry run and no ownership rule. Each
  closed item is struck through below with where it landed. The argument is kept, for the same
  reason `architecture.md` keeps every superseded position.
- **Fan-in is now in scope.** `sources` is a list, the flow selector has an `all` kind, and the
  request aggregate has `PARTIAL` (§7.2, §7.5, §9.1, §10.7, §10.8, §11, §12, §18). It closes no item
  and gets no section of its own. It changes three items instead:
  - §1.2's partition gained a code, and its rule still held.
  - §2.6's `ui.md` pass got larger for the third time.
  - §3.2 is now the only thing between this design and multipoint, where before it was one of
    several.

  It also adds two accepted costs to §4.
- **The event log is built** (§12.1 and §12.2 of `docs/architecture.md`): a bounded ring per
  object, anchored on the path; worker log tails pushed on failure; three read endpoints; two CLI
  verbs. It closed no item and added one, §2.11 (no UI support), which was a gap in `ui.md` rather
  than in the design. *§2.11 has since been built too. It is the one item here that was a
  specification rather than a complaint: `components/EventLog.vue` is organised around the five
  things §2.11 says a renderer gets wrong, and two of them were reachable on the ordinary dev
  fixture.* The event log's one structural consequence is recorded in §4 of `architecture.md`, not
  here: the three state layers moved under a common root, so a single range read covers the state
  and nothing else. That keeps the diagnostic log out of every state read and out of the reconciler's watch.
- **The UI is being built**, as a Vue SPA under `ui/app/`, against `ui.md`. `ui.md` §11 names three
  server-side additions as worth asking for rather than working around. The UI work settles two of
  them, and both are new items in §2:
  - The negotiated provider on `PathStatus` (§2.10) ships *with* the UI rather than as a separate
    request, because the cell preview is what needs it.
  - The ETag (§2.9) is worth asking for on its own merits, but **narrowly**. The endpoint `ui.md`
    named for it is the one endpoint where it cannot be correct, and explaining that is most of the
    item.

  The third, `GET /v1/nodes/{node}`, is in §5.

  The UI work has since added a fourth item (§3.4). Despite appearances it is not a UI item. The
  UI's manifest pane is postponed, and postponing it exposed that **nothing in this tree renders a
  manifest at all**. File → fleet is implemented three times; fleet → file is implemented nowhere.
  The first thing that needs fleet → file should probably not be a second implementation of the
  grammar in another language.

---

## 1. Broken: fix before the sections are read as final

### ~~1.1 Chaining has no spelling~~ — **resolved**

*Landed in §10.6 and §10.7.* The item is kept for its argument, which is now the argument for
unifying the two kinds of domain rather than for a union type.

The problem: §10.6 claimed chains work with no extra design, but §10.7 removed the only way to
express one. In `A→B→C` the second request has to *name* the middle node's output domain. The two
kinds of domain were identified differently: an input domain by absolute path, an output domain by
a rendered name under a root. The direct form of the source selector was `{"path": …}`, which could
address only input domains, and the label form was deliberately unable to match output domains.

This item proposed a single `{"name": …}` field that resolves either kind, with a leading separator
to tell them apart. **What was written instead removes the two kinds.** A domain is
`<area>/<elements>` whichever direction it is used in (§10.6), so `{"name": "fast/ingest"}` and
`{"name": "media/cameras"}` are the same kind of string, and there is nothing to disambiguate. (On
the wire the name is the structured `{area, elements}` object; see §2.7.) The source selector union
still has two kinds, because the second kind is *selection*, and that was always the real
asymmetry: you may **name** any domain as a source, but a label selector never matches a flow this
project is writing (§10.7).

`same_endpoint` stays, for the reason given here: `source: {name: fast/ingest}` against a
destination that resolves to `fast/ingest` on the same node is the self-pair the code exists to
catch. It is recorded in §7.2.

### ~~1.2 §7.2 contradicts itself about what refuses a POST~~ — **resolved**

*Landed in §7.2, which now states the rule once and lists the codes in two lists.* The code already
implemented the partition: `validate.Request` and `validate.Pairing` failures go into
`reconcile.Result.Structural` and refuse the `POST`; `validate.Conflicts` and namespace overlap mark
paths. The table below misses two codes that §7.2 now lists: `node_not_registered` (left column)
and `domain_name_in_use` in its nesting meaning (right column). The item is kept for its argument.

§7.2 says validation is **per path** and that `POST` refuses only what is *structurally* invalid.
But the list of codes above that statement is still headed "Rejectable immediately", and nothing
says which codes are structural and which are per path. As written, the section gives both
dispositions to the same set of codes.

**Fix: state the partition. It follows a simple rule.**

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

The right-hand column is everything that depends on other requests or on the fleet. That is the
set a selector's author cannot enumerate before submitting, which is why refusing the whole request
for one bad pairing is the wrong response there.

*Both columns changed with fan-in* (§9.1), and the partition rule held without amendment:

- `duplicate_source_flow` is new and goes in the left column. Two sources that *pin* one flow UUID
  against a shared destination can be detected from the request body alone. The same collision
  arriving through a selector stays `flow_conflict`, in the right column. The split exists because
  the rule gives each **code** one disposition, not each occurrence. Using `flow_conflict` for the
  decidable subcase would have given one code two dispositions, which is what this table exists to
  prevent.
- `same_endpoint` stays left, and is now checked over every `(source, destination)` pairing instead
  of the single one. Refusing the whole request for one bad pairing is still right, because with
  both ends named the author can see both halves of the typo.

Every code in the left column survives fan-in *because sources are still enumerated*. A source
names its node explicitly, so both ends of every pairing are decidable from the request plus node
registrations. As soon as a source's node becomes a selector (§10.8's multipoint), the left column
collapses into the right one. Anyone starting that work should know this first.

*Both columns also changed with the areas work.* `no_output_root`, `unknown_output_root` and
`ambiguous_output_root` became `unknown_area` and `area_not_writable`. `domain_name_in_use` lost its
original job: the area is now part of a domain's name, so the collision it caught (two output
domains with the same name under different roots on one node) cannot be constructed (§10.6, §7.2).
The partition is unaffected, because every code that changed is still decidable from the request
plus node registrations. (The code name `domain_name_in_use` survives in `internal/api/wire.go` with
a narrower meaning, a materialised domain nested inside another, reported by
`validate.Conflicts`; see §2.6.)

*With the label work in scope, `same_endpoint` still stays in the left column, though only
narrowly.* A label selector that matches the request's own destination domain makes the pairing
depend on the fleet, and by the rule above that would move the code right. Instead, such a pairing
is **dropped**, not refused (§7.2, §10.7). With a named source, a self-pair is a typo and is
decidable from the request. With a selector, it is the selector working as intended, and there is
nothing to report. So the code keeps one disposition instead of getting one per source kind, which
is the outcome the partition exists to produce.

**One correction to how the table is framed.** Every code in the left column can *also* invalidate
a path in steady state. For example, an area removed from a node's config after the request was
accepted produces `unknown_area` on a live path, not a rejected `POST`. The left column is the
subset that *additionally* refuses the write. Saying otherwise would break §7.3's property that one
`Compute` produces every answer. The reason for drawing the line where it is: a `POST` refusal must
be something its author can act on.

`ui.md` §7a's three-outcomes table ("The three outcomes of a request POST, and how to render each")
depends on this partition. It now says that the right-hand column's codes arrive on individual paths
of an accepted request, never as a `400`.

---

## 2. Mechanisms the design implies but does not describe

### ~~2.1 Label writes need a dry run~~ — **resolved**

*Landed in §9.1.* The argument was accepted as written. It is
kept because it is why this item was not dismissed as polish. A label edit adds a domain to, or
removes it from, a request's expansion, so it **starts and stops media** just as a request does.
Removing a label tears down running sessions, with no preview and no refcount shown. §9.1 gives
requests `?dry_run=true` because they move uncompressed video. Labels do the same thing through one
level of indirection, which makes an accident *easier*, not harder.

Both halves landed:

- `POST /v1/nodes/{node}/domains?dry_run=true`, which runs the same `Compute` against a candidate
  fleet.
- `label --dry-run`, and a printed blast radius on a real write.

**One decision went beyond what this item asked: the CLI prints the blast radius and does not
prompt.** The item asked for "a printed blast radius" without saying whether a real write should
also stop and ask for confirmation. It does not. The same operators script the CLI and use it
interactively, and a verb that waits for input on a tty hangs when run in a pipeline.

This is a *label* removing a path, not §4.2's fail-static freezing. The node is live, so nothing is
frozen and the teardown is immediate.

### ~~2.2 Manifest versus imperative: replace or merge?~~ — **resolved: neither, three-way merge**

*Landed in §9.1.* **An apply owns the keys it declares.** It sets them, removes keys it declared on
a previous apply and no longer declares, and leaves every other key alone. So an imperative
`label domain … role=cameras` **survives** the next apply that does not mention `role`.

**This item offered a false choice, and the reasoning it recommended was wrong.** Its argument:
everything else in the manifest replaces (a `POST` on a request replaces the whole spec), so a
`kind: domain` document should replace the entire label set. It called this "the ordinary kubectl
tension", resolved "in the ordinary direction". That misreads kubectl. `kubectl apply` is a
three-way patch against a last-applied record. It does not touch a field it never declared, and a
label added imperatively survives it. So the consistency argument and the Kubernetes argument point
in *opposite* directions. The item was resolved on the Kubernetes argument, deliberately, for two
reasons:

- This file format is close enough to a Kubernetes manifest that §19's adapter is a mechanical
  conversion.
- Surprising an operator about what `apply` does to a field they never mentioned costs more than
  internal consistency gains.

The consistency argument is also weaker than it looks, and §9.1 records why. A request spec has
**one writer by construction**, because `(namespace, name)` is its ID, so whole-spec replace there
describes what happens anyway rather than setting a policy. A domain's label map has no owner at
all. §10.6 spends a subsection establishing that a domain is a shared place, so the label map is the
one record in the design where several writers on one key set are expected. Whole-set replace there
means the last writer wins.

Three consequences the item did not anticipate:

- **The record gains a `Declared` key set** (`internal/api/domainlabels.go`). It is kubectl's
  `last-applied-configuration`, reduced to what a flat map needs. Without a record of what the file
  declared *last time*, "remove what this apply no longer declares" cannot be told apart from
  "remove what it never mentioned".
- **`label` sends a patch, not a read-modify-write.** This supersedes §9.1's earlier description of
  the verb. Read-modify-write was only needed because the endpoint was a full-set write, and it has a
  lost-update race on the very record this decision identifies as multi-writer.
- **Two files naming one domain still conflict.** There is one declared-key set per record, not one
  per writer. `kubectl apply` had the same limitation before server-side apply, and it fails the same
  way. Named field managers are the additive fix; they are not built.

The item worried that operators would use the imperative verb for something they expect to survive
an apply. The resolution makes it survive, rather than documenting that it does not.

### 2.3 `UpdatedAt` must not be bumped by the server

§7.5 bases conflict precedence on `UpdatedAt`. If the server ever rewrote a request record (to
normalise it, fill a defaulted field, or re-serialise it), it would reorder precedence across the
whole fleet in one pass. Nothing does this today, now that the namespace migration is gone (it was
dropped with the major version jump). But it should be a stated rule, not an accident of the current
code. (The conflict order that uses `UpdatedAt` is itself not built yet; see §2.12.)

### ~~2.4 `README.md` contradicts the spec~~ — **resolved**

*Resolved by a README rewrite.* The README now documents areas, namespaces and `sources:` lists, and says `-m` is gone. The original item follows.

It still documents `-m`, the old request routes and the pre-namespace CLI, and now also
`--search-path`/`--output-root` and path-shaped domain names. *The singular `source:` is no longer
on this list:* when fan-in landed, every example, the field table, the `get requests` sample and the
"one source, many destinations" section were rewritten along with the code. A README whose
manifests no longer parse is a worse problem than one that is merely out of date.

`ui.md` §0 points the UI author at the README as "the operator's current mental model", so its
errors matter. The areas work made it more wrong, not less.

### ~~2.5 `replicated` has no user-facing surface~~ — **resolved**

*Landed in §6, §9.1 and §10.7.* All three things this item
asked for are now required:

- the `replicated` flag on `GET /v1/flows`,
- the flag in `describe domain`,
- a per-request exclusion list naming the flows an expansion dropped because of the self-output
  rule.

The argument is kept because it explains why this is not cosmetic, and this is the item most likely
to be dropped under time pressure. Suppose an operator's broad selector silently skips three flows
on an edge node. Without these fields they cannot see why: the flows are in `GET /v1/flows`, they
match the labels, and nothing says "this node is writing this one". Under the old rule the whole
domain was absent, which the operator could at least recognise as a category. **This is the cost of
moving the guard from the directory to the flow: here the finer granularity makes the behaviour
harder to see, not easier.**

§9.1 also states two shape decisions the item left implicit:

- "Did not match the labels" is never listed as an exclusion reason. That set is unbounded and is the
  ordinary case.
- The list is capped, and a truncated list reports how many entries it dropped. A silent cap would
  read as "nothing else was excluded".

### ~~2.6 `ui.md` §7a needs the renamed codes~~ — **resolved: `ui.md` rewritten**

*Done on 2026-09-01.* It turned into the full `ui.md` pass this item kept predicting, not the
find-and-replace it started as. Every statement in the new document was checked against a server
built from this tree, with a fake fleet driven over the agent API that registers areas instead of
output roots. The argument is kept below as the record of what three waves of work
cost a reader who was not involved in them.

What changed, in the order the waves arrived:

- **The codes.** `no_output_root` / `unknown_output_root` / `ambiguous_output_root` →
  `unknown_area` / `area_not_writable`; `domain_name_in_use` kept with its narrower nesting meaning;
  `same_endpoint` and `duplicate_source_flow` added. The root picker was removed from the create
  form rather than renamed: the area is part of the domain's name, so choosing it is part of naming,
  not a separate setting.
- **The label work.** A source that is a *set* of domains; the `excluded[]` list with `self_output`;
  and label writes as a first-class mutation with `?dry_run=true` and a `stopped[]` / `started[]`
  blast radius. "Every mutation is still dry-run first" now covers two mutations and says so.
- **Fan-in.** `ui.md` §7a's central claim, "a request *is* a row", was replaced rather than patched:
  **a request is a rectangle**, sources × destinations. A single-source request is a 1×N rectangle,
  so the common case looks the same. The three consequences listed below in the original item are
  each dealt with where they apply: the node bands are justified in both directions; `PARTIAL` is
  the rectangle's state and never appears on a path; and §7b's overlap table is marked still correct
  but no longer complete. What holds this together is that **rows and columns are selectors, not
  real objects**: a row is a pair of selectors, and a column is a domain that does not exist yet. So
  the matrix never wires anything, and a cell is the only place real paths appear (`ui.md` §7a,
  "Rows and columns are selectors; only cells hold real paths").
- **An exclusive namespace is now a requirement**, not a preference. The matrix is an editor only
  over an `exclusive` namespace; a `shared` one renders read-only with an offer to convert (since
  replaced by the ledger of `ui.md` §7c, with no offer to convert). Two facts
  found by driving the server made this stricter than the old text:
  - The API's default mode is `shared`, and the `default` namespace is auto-created that way.
  - The exclusivity rule is enforced on *materialised* paths. So two requests with the same source
    and destination are both accepted while the selector matches nothing: one cell has two owners
    until a producer appears.

Driving the page headlessly against a live server found real bugs straight away. It found three,
none of which a unit test of any single piece would show:

- A dialog list kept showing the *previous* node's domains until the new read returned. This did not
  look stale, because domain names repeat across nodes.
- Two per-node reads returned out of order, so the node the operator had already left won.
- A domain selection was carried across a reopen onto a node that does not have that domain.

Each is the page's behaviour against a real sequence of reads, and only shows up there.

The original item, kept (it refers to headings of the old `ui.md`):

`no_output_root` / `unknown_output_root` / `ambiguous_output_root` → `unknown_area` /
`area_not_writable`, and `domain_name_in_use` is gone (§7.2, §1.2 above). `ui.md` §7a's
three-outcomes table enumerates them.

**Larger now that labels are in scope**, and it should be treated as a `ui.md` pass rather than a
find-and-replace. §7a's matrix has no notion of a source that is a *set* of domains. The label work
gives it three things to render that it currently cannot:

- a row whose source expanded by label rather than by name;
- a flow excluded from that expansion, with a reason (§9.1);
- a label edit, which is itself a mutation and needs the dry-run treatment §7a already requires of
  every other mutation.

The last one matters most: `ui.md` §7a says "every mutation is still dry-run first", and a label
write is now a mutation.

**Larger again with fan-in, and this time the document's central assumption changed.** `ui.md`
§7a's "Why it fits: a request *is* a row" builds the whole matrix on "a request is **one source with
a selector, and a list of destinations** — the list is on the destination side and deliberately
cannot be on the other". Now it can be. A request with several sources is not a row. Three
consequences, none of them a rename:

- **The grouping argument now also runs the other way, like §9.1's fourth argument.** §7a's "Group
  both axes by node" uses "one source to five destinations is 5× egress on one node" to justify the
  node bands. A fan-in request is 12× *ingress* on one destination node. So the argument now applies
  down the columns as well as along the rows, and a grid that makes only one of them readable
  renders half its requests badly.
- **`PARTIAL` is a new row state** (§11). It is the only state that appears on a request and never
  on a path. So §7a's "click a lit cell to see its path" now has a state with no path underneath it
  to show.
- **§7b's opening reasons about "two selectors over one source domain"** to establish when two rows
  are one edge with refcount 2. That is still correct but no longer complete. Two selectors in *one*
  request, over two source domains on two nodes, can now reach one destination flow. That is not one
  edge; it is `duplicate_source_flow` or `flow_conflict` (§7.2), a collision the grid has no way to
  draw. (That passage also still defines path identity as including "the resolved output root",
  which the areas work removed; see §5.4.)

### ~~2.7 `sources[].domain.name` is documented as a string and is not one~~ — **resolved**

*Fixed in the documentation, as the item proposed.* The example bodies in §9.1 and §10.7 and the doc
comment in `internal/api/domainselector.go` now use the object form
`{ "name": { "area": "media", "elements": ["cameras"] } }`, and each says that `media/cameras` is
only the manifest spelling.

Found by driving the server while rewriting `ui.md`. §9.1's example request body writes a source's
direct domain selector as `"domain": { "name": "media/cameras" }`, and the doc comment in
`internal/api/domainselector.go` repeats it. The wire type is `Name *Domain`, so the string is
refused:
`source.domain: json: cannot unmarshal string into Go struct field plain.name of type api.Domain`.

The **code is right and the prose is wrong**, which is the less harmful direction. The structured
form is what makes "parsed at exactly one boundary" true; the string is how the manifest spells it.
The fix is two lines, both documentation. It is worth doing because an API client will copy the
example body. The manifest example a few pages further on is the one place the string form is
correct, so together the two examples suggest the wire accepts either form.

### ~~2.8 `disabled` is designed and not built~~ — **resolved: built**

*Landed as written. The item is kept for the two things the written design got wrong.*

This was the one item on the list where the document was ahead of the code. §9.1 settles a
`disabled` flag on a destination entry, §11 the `DISABLED` aggregate state that follows from it, and
§7.2, §9.3, §12 and §18 the consequences. All of it now exists; `README.md` documents the manifest
field and `ui.md` describes it as built.

**Two corrections the implementation forced.** Both are recorded because the plausible version was
the wrong one:

- **The overlap contest is decided by recency, not incumbency.** §9.3 originally said a re-enabled
  request loses because "incumbency is the first term of §7.5's order". It does not.
  `namespaceOverlaps` orders legs on `(UpdatedAt, id)` and consults no session record. It *cannot*
  use incumbency: two requests contesting one path both map to the same path, and that path's
  session exists whichever request holds it. The practical claim still holds, because un-parking is
  a write, every write is timestamped, and so a returning request always carries the newest
  `UpdatedAt`. §9.3 and `ui.md` §7b now say this.
  `TestParkingReleasesThePathToAnotherRequestInAnExclusiveNamespace` tests both halves, including
  that an older timestamp takes the path back from a request that has been carrying it.
- **`omitempty` plus a reused decode target keeps a stale `true`.** A re-enabled destination comes
  back with no `disabled` key at all. Anything that decodes a poll *into* its previous response
  (which is what `json.Unmarshal` does to a slice's existing elements) keeps the old `true` and shows
  the leg as parked forever. This broke a test in this tree before it affected anyone else, and it is
  now `ui.md` trap 15.

The surface, as originally written:

- `api.Destination` gains the field, spelled `disabled` and **never `enabled`** (§9.1). `Validate`
  still counts entries, not enabled entries, and the duplicate-endpoint rule still applies to parked
  entries. Both rules look like oversights and are not, so each needs a comment saying so and a test
  pinning it.
- Expansion in `internal/server/reconcile` skips disabled destinations before pairing. That is the
  entire behaviour: no assignment path, no agent change, no worker change, nothing below the
  request.
- `api.RequestStates()` gains `DISABLED`; `api.States()` does not. `reconcile_test.go` already
  asserts that shape for `PARTIAL`, and the same pair of assertions pins this one.
- The status fold reports `DISABLED` when no enabled destination remains, and `status` counts it on
  its own line rather than in the non-active list (§11).
- The CLI manifest parser accepts `disabled: true` on a destination and round-trips it; `describe
  request` shows it. §9.1's "an apply that omits the flag enables the leg" follows from
  create-or-update, so it needs a test rather than code.

Both things flagged as worth checking held:

- A parked destination does release its path for another request to claim in an exclusive
  namespace, though for the corrected reason above rather than the predicted one.
- A request whose every destination is parked is distinguishable in the fold from one whose
  selectors match nothing: the first is `DISABLED`, the second `WAITING`.
  `TestParkedIsNotTheSameAsWaiting` keeps them apart.

### 2.9 The UI polls, and every poll is a full reconcile

On **every** user-API GET, whatever was asked for, `s.view()` (`internal/server/userapi.go:32`) runs
`state.Load`, which is one `List("")` over the whole store, and then `reconcile.Compute` over the
result. §7.3 requires this: the read handlers and the reconciler run the same function over the same
snapshot, so what an operator is shown cannot drift from what the fleet is being told to do. The
cost is that a read is O(fleet) rather than O(response). §9.1 gives the user API no watch, no stream
and no revision cursor, so a UI has to poll. The workspace of `ui.md` §7a needs four reads per cycle
(requests, paths, nodes, flows). At a 2 s poll that is roughly two full reconciles per second per
open tab. (`ui/app` polls every 3 s: `POLL_MS` in `stores/fleet.ts`.)

**The value an ETag needs already exists and is discarded.** `state.Fleet` carries `Revision`
(`state.go:224`): the store-wide counter that `List` returns, advanced by exactly one per mutating
write (`store.go:108`). Nothing needs to be hashed or derived. It is computed on the read path today
and thrown away.

**Two existing properties make it workable, and both were decided for other reasons:**

- A heartbeat deliberately writes nothing (`state.go:61`), so a quiet fleet has an unchanging
  revision. The obvious objection, that lease renewal would advance the counter several times a
  minute per node indefinitely, is already answered by that decision.
- A lease *expiring* does advance the revision. `sweepExpired` revokes each expired lease "each at
  its own revision" (`sqlite/lease.go:186`), and etcd deletes lease-attached keys the same way. So a
  node going dark invalidates the tag instead of being served from cache.

**What decides the design is that `Compute` is not a pure function of the revision.**
`reconcile.Config` has an injectable clock (`Now`, `reconcile.go:85`), used for idle teardown and
session ages. So two reads at the same revision can legitimately differ: a path crossing its
idle-teardown threshold changes state with no store write behind it.

*This reverses which endpoint should get the ETag, and it is why this item is written down rather
than just implemented.* `ui.md` §2, §7a and §11 originally named `/v1/paths` as where an ETag belongs
(§2 now carries a note correcting this). It is the one endpoint where an ETag cannot be correct:
paths carry all of the time-derived state, so a revision-keyed `304` would freeze the very
transitions the read exists to report. The correct targets are **`/v1/nodes` and `/v1/namespaces`**.
Capabilities, areas, grants and namespace records are pure store state that changes over days, and
they are two of the workspace's four reads. Paths and requests keep recomputing, which is their job.
An ETag on paths could still be built, with a bounded max-age or a coarse time bucket folded into
the tag, but that is more machinery than the reconcile it saves.

**It saves the `Compute`, not the `List`.** Nothing on the `Store` interface reports the current
revision cheaply; `List` is the only method that returns one (`store.go:119`). So the handler still
reads the whole store to learn whether it may answer `304`. On a fleet of any size that is the
expensive half. The saving is real, but it roughly halves the cost rather than removing it, and
should be described that way. A `Revision(ctx)` accessor would be trivial on both backends (etcd
returns a revision on any read; sqlite is a `SELECT MAX(rev)`). But it is an interface change with a
conformance suite behind it, which makes it a separate decision, and the narrow version above is
worth doing without it.

Not built.

### 2.10 `PathStatus` does not carry the negotiated provider — **in scope with the UI**

`PathStatus` (`internal/api/status.go:184`) carries id, source, destination, state, reason, reason
code and session id, and no interface config. So *which* provider a leg actually got can only be
found after apply, from `GET /v1/paths` → `path.session.interface`. §10.4 explains why this matters
for performance: falling back to tcp when verbs was preferred produces a symptom that looks like a
source problem. It is also why a pinned provider is never substituted: the silent version is the
dangerous one.

It matters **only when a fallback list is in use**. A hard pin either works or is refused at
admission (`pin_not_viable`), so there is nothing to disclose. With `provider: [verbs, tcp]` (prefer
verbs, accept tcp) there is an answer worth reading and no way to read it.

**The value is already available where the status is built**, which makes the change small.
`Compute` negotiates before it emits: `negotiated.Fabric` and `negotiated.Interface` go onto the
session record (`reconcile.go:1212`), and `emit` carries the session onto `api.Path`, while
`PathStatus` keeps only `SessionID`. Copying the chosen `(provider, fabric)` onto `PathStatus` adds
no computation and no read.

The useful consequence is that it then also works in a **dry run**. `?dry_run=true` runs the same
`Compute` against a candidate fleet, so a cell in `ui.md` §7a can show what a leg would negotiate
*before* it is applied. Nothing shows that today, and it is the half `ui.md` records as a gap (§7a,
"Two gaps in the server API"). Negotiation *failures* are already reported (`no_shared_fabric`,
`no_shared_provider`, `no_shared_capability`); what is missing is the result on success.

**Ships with the UI implementation** rather than being requested separately: the preview is what
needs it, and the change is two fields on one struct.

### ~~2.11 The event log has no UI at all~~ — **resolved: built**

*Landed as `components/EventLog.vue`, `model/events.ts` and the four reads in `api/client.ts`, on
the three detail views. `ui.md` §2 and §8 are corrected. Both had said there was no event log and no
worker log, which was the part of this item that was actively misleading rather than just missing.*

**The five traps listed in the original item served as the specification.** Treating them that way
is why this became one component instead of three panes. Each trap is a decision taken in §12.1 that
cannot be seen from the JSON, so three separate renderers would have been three chances to get each
one wrong. Every trap is pinned by a test in `model/events.test.ts`. Two of them turned out to be
reachable on the *ordinary* dev fixture, not only in a constructed case. They are recorded here
because both would otherwise have shipped:

- **`severity` is not the state vocabulary.** The fixture's idle camera arrives as `PAUSED` with
  `severity: info`, on the first screen anyone opens. A renderer that colours from `state` shows a
  routine event as a fault immediately.
- **The cursor is a per-ring sequence**, and the consequence is worse than "do not render a
  timeline". A request's merged view returns *several entries all stamped `seq: 1`*, because the
  rings it merges number their entries independently. Vue reuses a DOM node for a repeated key, so a
  list keyed on the sequence silently renders one entry out of five. This is the same kind of loss
  §12.1 records finding in a live fleet, when coalescing dropped three of four flow names, and it was
  found the same way.

**Three decisions the item did not anticipate**, each recorded where it is made:

- **No `?since=` cursor, ever.** The endpoints accept one; this UI reads the whole ring every time.
  Coalescing *rewrites the last entry in place with a new sequence number*, so an incremental poller
  receives the same row twice. The only way to deduplicate it would be to reimplement
  `coalescesWith`'s key in TypeScript. That is §3.4's argument about the manifest grammar, applied to
  a ring of at most fifty entries.
- **The cheaper reads are not used to poll faster.** The original item's opportunity is real (these
  are the first O(response) reads), but the pane still polls on the fleet timer, and `ui.md` §2's
  one-timer rule holds with no exception. A second polling cadence would get copied into the next
  component, which might not be able to afford it.
- **The node pane renders for a node that is not registered.** The endpoint answers for one, because
  a node's log outlives its paths and its lease, so the pane sits outside the "no such node" branch.
  An operator holding a name that is no longer in `/v1/nodes` lands on this page, and it is the only
  place left that says what happened to the node.

**Placement went the way the item suggested: per-object panes, no fleet-wide stream.** The reason is
firmer than "later". There is no endpoint for a stream: the rings are per-object by design, and the
fleet ring is merged into object reads rather than served on its own. The only way to build one on
the client is a fan-out read over every path, which costs more than the full reconciles §2.9 is
about. The health view lists what is not active, and every row leads to an object whose log is one
click away. If that stops being enough, the answer is a server endpoint, not a client-side loop.

**One cost outside its own files**, to know before the next section is added anywhere:
`Detail.live.ts` located the node view's paths table as *the last `.dt-table` on the page*, which it
stopped being. It now selects the table by its `role` column. A positional selector on a view that
gains sections breaks for the next person to add one, not because of a bug in the view.

The original item, kept:

`docs/architecture.md` §12.1 and §12.2 are built, and `ui.md` predates all of it. Nothing in the
described workspace renders an event, and there is now a sizeable API with no consumer:

- three `events` reads (path, request, node);
- a `logs` read for worker tails;
- a fleet ring merged into every answer;
- the `events` and `logs` CLI verbs, currently the only way to see any of it.

**This is written down rather than left to whoever builds the panel** because a renderer that
treats these as an ordinary list gets five things wrong. Each is a decision taken in §12.1 that
cannot be seen from the JSON:

- **A coalesced entry is one row, not `count` rows.** `count`, `first_at` and `at` mean *this
  happened N times between these two moments*. Expanding them recreates the flapping the ring exists
  to compress, in the UI, where the ring can no longer limit anything.
- **`has_log` is a marker, not content.** The tail is a deliberate second fetch, so that the list a
  UI polls stays cheap when things are failing (§12.2). A renderer that fetches every tail eagerly
  defeats the reason for the split.
- **`severity` is not the [api.State] vocabulary, and must not be coloured from `state`.** §12.1
  settled that designed behaviour never warns (an idle teardown, a queued start, a producer
  stopping), so a row whose state is `PAUSED` is routinely `info`. Colouring from the state instead
  produces the board full of false faults that §11 avoids in two places.
- **The cursor is `next`, a per-ring sequence, never a timestamp.** Entries are timestamped by
  whoever emitted them, so a merged request view interleaves two agents' clocks and the leader's.
  Rendering it as a causal timeline invites an operator to read an ordering across hosts that does
  not exist.
- **`dropped` and an `events_dropped` entry are different losses.** `dropped` counts history aged
  out of a full ring. An `events_dropped` entry records entries an agent never managed to report.
  Both need showing, and showing them the same way loses the important distinction: only the second
  means something was never seen.

**The opportunity.** The `events` reads are the first user-API reads that are O(response) rather
than O(fleet): each is one `Get` on one key and does not run `Compute` (§9.1). §2.9 of this file is
about the cost of the opposite. So a workspace that polls events often and everything else rarely
is now a coherent design, where before every poll of anything was a full reconcile.

**What is undecided is placement, not mechanism.** A per-object detail pane is the obvious choice,
and `describe` already does this: path, request and node each show their last eight entries under
the status. What is not obvious is whether the workspace wants a *fleet-wide* stream, which is a
different object. There is no endpoint for it, the rings are per-object by design, and building one
would need either a fan-out read or a new key duplicating what the rings already hold. This should
be decided before someone builds it by accident.

~~Not built.~~ *Decided above: the panes, and no stream.*

### 2.12 §7.5's conflict precedence is designed and not built

§7.5 settles that candidate paths are ordered by `(incumbency, UpdatedAt, id)`, where incumbency
means the path has a derived session record. The code still uses the order §7.5 says it replaces.
`validate.Conflicts` (`internal/server/validate/validate.go`) sorts paths by `Since`, the creation
time of the earliest request on the path (`internal/server/reconcile/reconcile.go`), and then by
path ID. It does not read session records or `UpdatedAt`.

So the failure §7.5 was written to prevent still happens: an old request whose selector starts
matching a flow in August takes the destination flow from a newer request that has been `ACTIVE`
since June, and the newer request's media stops. Also missing:

- the `mxl_repl_path_conflicts{reason}` gauge §7.5 adds to the leader's fleet gauges;
- the §17 test for the precedence case.

§2.3, and §4's "no preemption" and same-request tiebreak entries, describe the designed order.
They become true when this is built. The namespace overlap contest (§9.3) is a separate code path
and already orders on `(UpdatedAt, id)`, as §2.8 records.

### 2.13 §17 lists tests that do not exist

§17 lists what the tests cover. Four of its bullets have no matching test (checked by grepping
`internal/**/*_test.go` and `cmd/**/*_test.go`):

- conflict precedence (§7.5): an `ACTIVE` path beating a newly matched path from an older request.
  This test would fail today (§2.12);
- the provenance gap: an agent restart briefly reporting `replicated=false` starts nothing;
- a label on a domain in a write-only area is accepted and has no effect;
- a real (not dry-run) label removal reporting, for each stopped path, whether another request still
  references it. Only the dry-run form is tested.

§17 now marks each of these. Either write the tests or take the bullets out of §17.

### 2.14 `ui.md` §7a–§7c describe UI features `ui/app` does not have

`ui.md` is the design and `ui/app/` is the implementation. These parts of the design are not built,
and `ui.md` now says so at each place:

- a control that creates a namespace as `exclusive`, or changes a namespace's mode (§7a, "The
  matrix requires an `exclusive` namespace"). Only the live-test fixtures call
  `api.applyNamespace`;
- a check that refuses a "one cell with two owners" request when it is created. The matrix does draw
  the case and disables clicks on it;
- the cell detail view, with its per-leg editor and per-destination `provider` override (§7a, "The
  three editors around the matrix"). The negotiated provider it would show is §2.10;
- the ledger's group-by-source toggle (§7c);
- editing and deleting a request from the ledger (§7c, "Editing"). `views/Ledger.vue` is read-only.

The manifest pane is also not built; it is §3.4.

---

## 3. Decisions deferred

### ~~3.1 Label selector semantics~~ — **resolved**

*Landed in §10.7.* Equality only, with all keys ANDed, as proposed. It is the obvious first version
and it follows §9.1's rule for extending tagged unions: `in`, `notin` and `exists` can be added later
without breaking anything.

**What was written adds the reason the restriction matters, beyond being minimal.** Those operators
will arrive as a *third union kind*, not by widening what a map value may contain. Widening the value
grammar is the change that cannot be undone: a request whose value happened to look like an
expression would change meaning on upgrade, silently, and toward matching *more*. For something that
moves uncompressed video, that is the wrong direction to fail in. A new kind cannot do this, because
no existing request has it set. This is `api.Selector`'s argument applied to the second union, and
`internal/api/selector.go` already states it in its comments.

~~**An empty label map must be refused.**~~ **Resolved** by the manifest's scalar-vs-map rule: a
scalar `domain:` is a name and a map is a label set (§9.1), so `domain: {}` is a label selector with
no keys and is refused as one, without needing a rule of its own. The reason it mattered still
stands: a selector that matches every domain on the node expands a request's source set. So the
refusal has to be in the validator, not only in the prose. §10.7 now says this, because the syntax
rule only decides how the empty map is read; it does not refuse anything.

### 3.2 Node labels do not exist

**Still open, and deliberately not included in the label work.**

§10.8's case against §9.1's *"the destination side cannot have a selector"* depends on *"once nodes
carry labels"*, and no node carries labels today. Domain selectors do not need node labels
(`source.node` stays pinned), so deferring them is correct. But §10.8 used to read as though the
mechanism were available. This item asked it to say that it is not, and that designing it is the
first step of anything in that section. **§10.8 now says so**, which closes the half of this item
about the section's wording. The mechanism itself stays open.

**Fan-in raised this item's importance, but not its urgency.** The cross product is built (§9.1), and
two of §10.8's three mechanisms landed with it. So node labels went from one of several missing
pieces to the *one remaining gate*: multipoint now means exactly "either end is selected rather than
named", and the source half of that is node labels. That does **not** make it more urgent. It makes
it the item that must not be built casually. Every rejection code in §1.2's left column is decidable
today only because a source names its node. A node-label selector on the source side moves that
whole column into the per-path one on the day it ships. The first step is still designing node
labels; the second is deciding what §7.2 looks like afterwards.

One part of §10.8 *did* land early: the `same_endpoint` argument, that a self-pair produced by a
selector rather than by a typo should be dropped rather than refused. It landed because a label
selector matching the request's own destination domain produces such a self-pair without multipoint
(§7.2, §10.7). The rest of §10.8 is unchanged.

### ~~3.3 Should search paths be advertised?~~ — **moot**

*Landed in §10.2.* The question was whether §10.2's rule ("something is a capability if and only if
the server would make a wrong decision without it") should bend to carry a field used only for
diagnostics. Without search paths the server made no wrong decision, but it could not explain why a
label outside one had no effect.

The rule does not need to bend. A domain's identity is `<area>/<elements>` (§10.6), so a server
without the area table cannot render a domain's name, resolve one in a request, or say which area a
label fell outside of. Readable areas pass §10.2's own test. They are advertised with their grants,
because "may this name be a destination" is checked when a request is validated.

### 3.4 Nothing renders a manifest, and the UI's manifest pane is postponed

`ui.md` §7a ("The matrix corresponds to one manifest file") asks the workspace for a panel showing
the current namespace as the multi-document YAML file, copyable, with staged changes shown as a diff
against it. It argues strongly for this: *"a UI that only ever creates requests through its own
controls creates a second source of truth for the desired set without saying so"*. Rendering the
file means:

- the UI teaches the format instead of competing with it;
- "I worked it out on screen, now commit it" becomes a copy rather than a rewrite;
- "how do I do this for forty cameras?" gets its real answer, a file, and the operator has just
  seen what one looks like.

**It is postponed deliberately, and postponing it is cheap for one specific reason.** The staged set
that the grid's gestures build (`ui/app/src/stores/staging.ts`) is already a diff: one effective
spec per touched request, recomputed from the stored one on every read. So the pane is a renderer
over state that already exists, not a feature the model has to make room for, and nothing in the
mutation design has to be decided differently in the meantime.

**The cost accepted in the meantime is larger than "a panel is missing".** File → fleet is
implemented three times: `internal/manifest`, the wire API, and now the UI's own controls. **Fleet →
file is implemented nowhere.** `internal/manifest` is only a parser (its one `yaml.Marshal` re-encodes
a node inside the strict decoder; it is not an emitter), and `get` has no manifest output. So an
operator who lays out a board on screen can only get the file that belongs in git by writing it by
hand from what they see. That is the second-source-of-truth failure `ui.md` describes, arriving
through the missing feature rather than through the UI's controls. It was tolerable while the UI was
read-only. It gets worse as soon as the editors ship, and that should raise this item's priority.

**When it is built there are two options, and the obvious one is worse.** A renderer inside the UI
would be a *second implementation of the manifest grammar*, in a second language, with the Go parser
as the authority and no compiler checking that they agree. The grammar deliberately differs from the
wire format in three places, and each is a way to be silently wrong:

- the flow selector is **flattened** onto the source entry, where the wire nests it under `select`;
- the domain is a **scalar or a map**, where the wire has `{name: {area, elements}}` or
  `{labels: {}}`;
- an **omitted flow selector means `{all: true}`**, filled in by the CLI and never by the server.

A pane that got any of these wrong would show a file that does not parse, or worse, one that parses
into a different request.

The better option is an **emitter in Go beside the parser**, exposed as `get -o manifest` (and so
available to the UI as an ordinary read). That gives one implementation of the grammar, in the
language that owns it, in the same package as the parser that must agree with it, where a round-trip
test (`parse(render(doc)) == doc`) is an ordinary unit test rather than a cross-language integration
problem. It also gives the CLI a feature it lacks and that operators will ask for on its own merits,
which suggests the boundary is in the right place. It is the same argument as §2.10: use the value
where it is already known instead of recomputing it at the far end.

**Not decided here.** The pane is postponed, not designed. What is decided: if it is built as a
TypeScript renderer, it needs a round-trip test against the real parser, because the three
differences above are the kind of mistake that looks right.

---

## 4. Accepted costs worth recording

These are not problems to fix. They are consequences that should be written in the document, so that
people read about them instead of discovering them.

- **Renaming a node orphans every domain label on it.** Under `-m` the labels lived in the same file
  as the host configuration, so rebuilding a node kept them. They are now control-plane state keyed
  on `(node, domain)`. The orphaned records are visible in `GET /v1/nodes/{node}/domains`, which is
  the mitigation, but nothing moves them. **Renaming an *area* does the same** to the labels on its
  domains, for the same reason; recorded in §10.6.
- ~~**Moving a node's MXL area re-identifies every domain on it.**~~ **No longer true.** Identity is
  `<area>/<elements>`, so pointing an area at a different directory while keeping its name preserves
  every domain identity on the node, and paths and sessions survive the restart instead of being
  rebuilt (§10.6, §5.4). Flows left in the old directory are leaked and nothing moves them; the
  operator has to sequence that. Moving a domain to a different *area* still re-identifies it, as it
  should.
- **Full recovery of a node now takes minutes, not seconds** (§6.3, §6.1). Rate control on worker
  starts keeps 1–2 s as the budget for re-establishing *one flow*. Under the shipped defaults, a node
  with fifty flows takes well over a minute to re-establish all of them. This is deliberate: the
  failure rate control prevents takes the whole node down, rather than slowing it. It is recorded
  because §6.1 reads as a promise about agent restart in general, and is now a promise about one
  flow. Two gaps follow, and nothing addresses them yet:
  - The *server* has no notion of a node being deliberately paced. A bulk re-establishment shows as a
    long tail of `ESTABLISHING` paths with no explanation on the path itself. The explanation is in
    the agent's metrics and in the session's reason (§12).
  - Nothing sizes the default to the host, so it is a number an operator has to tune after seeing a
    symptom.
- **There is no preemption.** Incumbency comes first in §7.5, so a request already holding a path
  keeps it, however much better a competing request is. To hand a path to a different request, delete
  the incumbent first. This is deliberate, and it is stated here because "why won't my new request
  take over?" is the question it produces. (True once §7.5's order is built; see §2.12.)
- ~~**The source-side `domain` metric label now publishes host filesystem paths**~~ **Fixed.** The
  label carries `<area>/<elements>` on both sides now, so `/metrics` exposes an operator-chosen name,
  not the node's directory layout. Recorded in §12 and in §13's list, because that endpoint is
  commonly unauthenticated.
- **A conflict between two paths of the same request is decided by an arbitrary term.** §7.5 orders
  on `(incumbency, UpdatedAt, id)`, and two paths of one request share an `UpdatedAt`, so before
  either is established the tie falls through to the path ID. That is deterministic and stable, which
  the order must be. From the operator's point of view it is arbitrary, and no ordering can fix that:
  nothing in the request says which of two sources of one flow ID was meant. Fan-in makes this
  reachable in ordinary use rather than only in unusual setups. The decidable form is refused at
  `POST` (`duplicate_source_flow`), so what reaches the tiebreak is only what could not have been
  caught earlier, and the message names both sources. Nothing to fix, but someone will ask why one of
  their two studios won.
- **`sources` invalidates every stored request and every manifest.** The list has no singular
  spelling alongside it (§9.1), so a file or record written with `source:` is refused, not migrated.
  It ships in the same major version as the areas break (§16) and belongs in the same release notes.
  It is a smaller break than the domain re-identification below, but it is the one an operator hits
  first, because it is a parse error in a file they are holding.
- **Every domain identity in the fleet changes with the areas work.** The upgrade invalidates every
  stored path, session and label record, every manifest, and every dashboard query built on the old
  spelling. §16 already takes a major version jump with no config compatibility, so this needs no
  migration of its own. But it is the largest single break in the document and belongs in the release
  notes, not only in §10.6.

---

## 5. Small

- ~~**Namespace name grammar**~~ **Resolved.** §9.3 now defines it (letters, digits, `-`, `_`,
  non-empty), with the reason it is restricted when an ordinary label value is not: a namespace name
  is a path segment in a URL, a store key and a `-n` argument.
- ~~**`GET /v1/nodes/{node}/domains` is inventory-dependent now.**~~ **Resolved:** it carries the
  `settling` flag, as `GET /v1/paths` does (§9.1). The failure this prevents: during the settling
  window the endpoint would otherwise show every label with no observed domain beside it, which looks
  the same as the labels having been lost. A second rule came with it: the endpoint answers for a
  node with no registration at all, so a mistyped node name in a manifest does not produce a write
  that can never be read back.
- **`GET /v1/nodes/{node}` is documented and not served.** §9.1 lists it, but the mux does not
  register it (`internal/server/http.go`), so it returns `404`. This was verified while rewriting
  `ui.md`, which tells its reader to fetch the list and filter it, as `describe node` already does.
  Either serve it or remove it from §9.1: as things stand, an API client will write code against the
  route and get a 404. It is the third of the three additions `ui.md` §11 raises, and the only one
  nothing is waiting on.
- **`--server-config` is accepted and ignored.** `ServerOptions.Config`
  (`cmd/mxl-replicator/server.go:22`) declares a repeatable `type:"existingfile"` flag, just like the
  agent's, and kong parses it, but nothing reads it. There is no `config.LoadServer`;
  `internal/config` loads only the agent's config. So the file must exist, which makes the flag look
  like it works, and every value in it is silently discarded. That is the worse way to fail: a flag
  that rejects an unreadable path and then ignores a readable one is harder to notice than one that
  errors outright.

  Either implement it or remove it, the same choice as for `GET /v1/nodes/{node}` above.
  Implementing it is the better option if the server ever gets a list-valued setting, which is why
  the agent has a config file at all (`internal/config/agent.go`). `--server-provider-order` is
  already a list, and the etcd endpoint list is written as a repeated flag today. A config file would
  also let the Helm chart give the server a ConfigMap, as it already does for the agent, instead of a
  flat argument list.

  Nothing depends on it, so it is safe to leave. It is listed because the flag's own help text
  documents it, and an operator who uses it gets no error.
- **A namespace auto-created from a typo cannot be told apart from a deliberately empty one**
  (§9.3). Both are inert and cheap, and `DELETE` is available. Probably nothing to do, but someone
  will ask. **The same now applies one level over:** a domain label on a node or domain that does not
  exist is accepted and inert by design (§10.7), and nothing cleans it up. The mitigation is the
  same, on the read side: the label is listed, so it is visible and not only harmless.
- **The `domain_name` metric label is designed and not emitted.** §10.7 and §12 describe a
  `domain_name` label holding the domain's optional `name` label, frozen per worker. It is not in
  `metrics.WorkerLabelNames()`, and `internal/agent/metrics.go` does not set it, so the reserved
  user-label set does not include it either. `ui.md` already records the reserved-set half of this.
