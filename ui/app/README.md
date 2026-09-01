# mxl-replicator UI

A Vue 3 SPA for the control plane. `ui.md` is the design and the reasoning; `docs/architecture.md`
is the specification. This file is how to run it and what the code is arranged to protect.

**Status: the landing page, the matrix and the ledger — the matrix's cell gesture, the two editors
that author its axes, and `duplicate` and `split`.** What is left is the detail views and
domain-label writes.

- **The landing page** — `ui.md` §7's fleet health. Counts, then only what is not active, worst-first,
  each with its reason. Fleet-wide, never namespace-scoped.
- **The matrix** — §7a's workspace, over an `exclusive` namespace. Rows are sources, columns are
  destinations, a request is a **rectangle** over them; both axes are read out of the requests, both
  are banded by node, and a parked destination keeps its column.
- **The ledger** — §7c's claims view. Grouped by destination, one row per path, every claim carrying
  the selector that made it, `sole`/`shared` per request, and a rectangle per request so parked legs
  are drawable.

A namespace's `paths` mode decides which of the two the workspace opens on, because a `shared`
namespace **is not a matrix**: two requests may hold one path there, so two cells would be one
session and one worker pair, the counts would stop summing, and clearing either would cancel a
request and stop nothing while going dark exactly as if it had. The grid control is shown and
disabled with the reason rather than omitted. The ledger is offered over **both** modes — nothing in
it requires `shared`, and inside an exclusive namespace the claims list degenerates into the plain
path list `describe path` gives.

**The matrix's cell is a gesture: click to park a leg, click again to switch it back on, click an
unlit cell to add one.** Nothing writes. Every click stages, the staged set is dry-run **as a batch**,
and the bar at the foot of the screen is the confirmation — which is the whole argument for staging
over apply-on-click: the preview reports real outcomes and real blast radius before anything moves,
so no dialog has to ask "are you sure?" and none does. "3 of 4 paths stop" is the sentence that
replaces it, and the difference between those two numbers is the refcount.

**And three `×`s, because `×` only ever removes something already dark.** The cell's takes a parked
leg out of the one request in front of you; the column's takes a destination out of every request
that names it, offered once all of them have parked it; the row's removes a source and is the one
that can stop media, because a source has no parked state to be put into first. Dark means dark on
the **server**, so deleting a live leg is park, apply, then `×` — two deliberate acts. A removal that
empties a spec commits as a `DELETE`, since a request must name at least one source and one
destination.

**And the two editors write the axes.** `+ source` is node → domain → group → how much of it, which
is the order §7a asks for and not a flow picker: the operator browses to discover, but what they mean
is the group. `+ destination` is a node that grants `write`, an area picker that is always shown
because the area is the first segment of the domain's name, and elements — with the directory it
resolves to underneath. Neither writes anything. A destination becomes a column so that there is a
cell to click; a source becomes a row, either of a request that exists or of a **draft** — a request
authored here and not created, which reaches the server through the same dry run and the same Apply
as everything else. `model/draft.ts` carries why a draft is allowed to be a spec when an edit is not.

**And the two controls that copy a rectangle.** `duplicate`, on the rectangle's own row, is §7a's
*"copy the rectangle, change the selector, keep the destinations"* — so it **is** the source editor
with a template rather than a panel of its own, because choosing the new selector is the operation. A
copy that kept the sources would be a second request asking for the identical paths, which
exclusivity refuses the moment a producer appears. `split`, on a row header, takes one source out
into a request of its own and keeps the destinations: §7a consequence 2's other answer to clearing a
cell, and a control rather than a mode of the click because **it creates a name**. Both copy the
entries whole — a parked leg stays parked, a per-destination `provider` override comes along — and
the request-level settings with them.

What is not built: domain-label writes with their `stopped[]`/`started[]` preview, and the detail
views. `ui/prototype/` is no longer the only implementation of anything on screen. The
**manifest pane** `ui.md` §7a asks for is postponed rather than pending — `docs/open-items.md` §3.4,
where it turned into a question about the system: nothing in this tree renders a manifest at all, so
the first thing that does should probably not be a second implementation of the grammar in
TypeScript.

## Run it

Three processes. None of them needs MXL, libfabric or hardware.

```bash
# 1. the control plane, on sqlite, in a temp directory
go build -o /tmp/mxl-replicator ./cmd/mxl-replicator
/tmp/mxl-replicator run --server \
    --server-listen 127.0.0.1:12999 \
    --server-store-sqlite-path /tmp/mxl-store.db \
    --server-heartbeat-interval 1s --server-lease-ttl 8s

# 2. five fake nodes with areas, inventory and labels, leases kept alive
S=http://127.0.0.1:12999 ui/prototype/devfleet.sh

# 3. this app
cd ui/app && nvm use && npm install && npm run dev
```

Then open <http://localhost:5173/>.

**Use 12999, not 2283.** 2283 is what a real `mxl-replicator` listens on, and `devfleet.sh`
registers nodes through the *agent* API — point it at a fleet somebody is running and it writes
fake node registrations into their store, which are durable and have no deregister API.

`devfleet.sh` keeps running on purpose: leases need renewing, and a lease that expires freezes every
path touching that node. Paths reach `ESTABLISHING` and stop, because nothing runs a worker. That is
the useful fixture rather than a limitation — it is the state an operator watches while something
comes up.

## Tests

```bash
npm test        # hermetic; runs anywhere
npm run test:live   # drives the real components against a running control plane
npm run check   # vue-tsc
```

`*.test.ts` is the unit suite and has no dependencies beyond the repo. `*.live.ts` mounts the real
components and lets them talk to a live server with the fake fleet behind it — real DOM, real fetch,
real reconciler, real store. That second class is kept because the prototype established it catches
bugs nothing else does, and the class is consistent: a stale list left over from a previous read,
two reads landing out of order, a selection carried across a reopen. Each is the page's behaviour
against a real *sequence* of reads, which is the only place it exists.

jsdom installs its own `AbortController`, which node's `fetch` refuses by identity — `src/test/live.ts`
works around it and says why. A browser has one matching pair and never sees it.

**The live suites write their own preconditions rather than inheriting them.** `Matrix.live.ts`
deletes and rewrites the `nab` namespace as an exclusive fixture — a two-source rectangle, a leg
another namespace also writes into, a parked destination and a selector that matches no flow — for
the reason the prototype's harness gave: a fixture left behind by an earlier run makes the
assertions a statement about the store's history rather than about the rule. `Staging.live.ts` seeds
and then deletes a `staged` namespace of its own, routed clear of every other fixture, and drives the
gesture end to end: click, stage, dry-run, apply, and the server agreeing that the leg is parked and
its paths gone. `Editors.live.ts` seeds an `authored` namespace and drives the other half — name a
destination, author a source, route them with a cell click, apply, add a second source to the request
that made, then duplicate that rectangle and split a source back out of it. The `k8s` fixture `Ledger.live.ts` reads is the one in `ui.md` §9 and is seeded by
hand.

**Wait on the screen, not only on the server.** An apply refreshes the fleet *after* its POST
returns, so a row is still drawn for however long that read takes; asserting it away on the strength
of the API's answer alone is a race, and this suite has lost it once.

**jsdom does no layout, so the suites are structurally blind to geometry.** Every alignment bug in
this app so far was found by screenshotting the real page — a `th` given `display: flex` stopping
being a table cell, a header row taking its widths from the node bands above it, a two-fact line
running its two facts together, and a line class named `head` inheriting `flex-wrap: wrap` from the
page header's own `.head` so a hidden `×` wrapped onto a second row. Keep this in the loop for
anything visual:

```bash
google-chrome-stable --headless --disable-gpu --no-sandbox --hide-scrollbars \
  --window-size=1400,900 --virtual-time-budget=6000 \
  --screenshot=/tmp/shot.png http://localhost:5173/ns/nab
```

## What the arrangement protects

Things here that are load-bearing rather than incidental. Each mirrors a rule in `ui.md`, and most
have a test named after them.

- **Relative URLs only, and no API base.** The UI is always same-origin with the API — served by the
  server binary, or behind a proxy fronting both. A configurable base is what leads to CORS, and
  production will not have CORS. Development fakes the origin with Vite's proxy (`vite.config.ts`).
- **No route to `/agent/v1`.** The agent surface is privileged: anything that can call it can claim
  to be a node, inject fabricated inventory and read every node's RDMA rkeys. The proxy does not
  forward it and `src/api/client.ts` has no method for it. Both are deliberate; neither is enough
  alone.
- **No auth surface.** Auth is one optional shared bearer token that also guards the agent API, so a
  token the browser holds hands whoever loads the page that whole surface. The supported answer is
  that the proxy or the server injects the header on the way through, which needs nothing from here.
- **One timer, one set of reads, one replacement.** Every user-API GET costs the server a full store
  load plus a reconcile over the whole fleet — O(fleet), not O(response). `stores/fleet.ts` polls
  once for everything the screen needs and skips a hidden tab.
- **All four reads land together or none do.** A partial update would put requests from this poll
  beside paths from the last, and the workspace joins the two. Staleness is honest; a torn read is
  not.
- **A failed poll changes nothing on screen.** The banner says stale and the last good read stays
  rendered — the same discipline the agent runs on, because an unreachable server must not look like
  an empty fleet.
- **Replace, never merge.** `disabled` on a destination is `omitempty`, so a re-enabled leg comes
  back with no key at all; anything merging a poll over its previous state shows the leg parked
  forever after it came back.
- **`settling` is a banner, not an error**, and it is distinct from an unreachable store. The three
  conditions send an operator to three different places and two of them are not failures.
- **A domain is structured going in and rendered coming out.** `{area, elements}` is what may be
  *sent*; `fast/ingest` identifies something that already exists. `DomainName` is a branded type, so
  a rendered domain cannot be passed where a structured one is wanted — the manifest parser is the
  only thing in the system that turns a domain string back into parts, and this keeps the UI from
  becoming the second one.
- **The two selectors are unions, not structs with optional fields.** Two kinds set is a compile
  error rather than a 400. Ignoring a selector kind would silently *widen* what gets replicated.
- **A path is never `PARTIAL` or `DISABLED`.** `PathState` and `RequestState` are different types.
- **Big integers survive parsing.** `max_message_size` is a genuine `uint64` and providers report
  `UINT64_MAX`, which `JSON.parse` silently rounds. The client quotes anything too long for a double
  before parsing — with a scanner rather than a regex, because a flow definition is arbitrary NMOS
  content and a regex cannot tell digits in a string from numbers in the document.
- **A request's own state comes from the server, never from the local fold.** `model/state.ts`
  reproduces the server's fold for the subsets the server does not aggregate — a matrix cell over
  its own paths. It cannot reproduce a *request's* state, because the server also folds leg failures
  that produce no path at all, and there is nothing in `status.paths[]` to recompute them from.
- **Ownership comes from `/v1/paths` → `requests[]`, never from a request's own status.** The loser
  of a namespace overlap reports the contested path with the incumbent's state, so a request holding
  nothing can show an `ACTIVE` leg. The join lives once, in `model/ownership.ts`, and the ledger's
  claims, the matrix's cells and every rectangle go through it — so all three render honestly in an
  exclusive namespace as well as a shared one, and the trap has one place to be got right.
- **Both axes of the matrix are read out of the requests, never out of the fleet.** A row is a pair
  of selectors and a column is a domain that does not exist until a request names it, so neither is a
  handle on an object and neither can be derived from inventory. It is also what keeps a parked
  destination on the board: columns come from `destinations[]`, so switching a leg off does not
  delete the row and the column it lived on and rearrange the grid under the pointer.
- **The cells account for every path the namespace holds**, and say so when they do not. Nothing
  should produce an unplaced path; a grid that silently drops an edge is the failure this model
  exists to avoid, so the number is on the screen rather than assumed away. Two things could produce
  one and both should be impossible: the join dropped an edge it had a row and a column for, or the
  server reported a path through a destination its own **stored** spec does not name. A leg *staged*
  for parking or removal is neither — it is drawn nowhere and counted nowhere, so the header and the
  cells still agree while a change is pending, and how many of those paths stop is the bar's to say.
- **The `×` controls only ever remove something already dark, and dark means dark on the server.** A
  leg staged for parking looks identical and is still carrying media, so a `×` offered over it would
  be a small control in a corner that tears something down. Park, apply, then `×`. The row's `×` is
  the exception and stays destructive, because there is no flag on a source and so no dark state to
  require — it says what it will stop rather than being made to look safe.
- **A draft is a spec, and it is the boundary of the rule below rather than an exception to it.** The
  reason an edit must not be a copied spec is that the server's copy moves underneath it. A request
  that does not exist has no copy, so the operator's text is the only text there is. It is held to
  the same discipline everywhere it can be: it is complete only when it names a source *and* a
  destination, it can never commit as a `DELETE`, and it stops being a draft the moment the server
  holds its name, whoever created it. **Every cell of a draft is unwritten** — a fact about the
  request, which is what the renderer asks, rather than about any one leg.
- **Drafts are applied first, and that ordering is load-bearing.** A split creates one request and
  updates another; create first and the new one merely loses the contest for the contested path
  (newer stamp), while the incumbent keeps it — then the update lands and the reconcile sees exactly
  one claimant. No reconcile ever sees the path with none. The other order has one that does, which
  is a teardown and a rebuild of a session that had no reason to move. A newly created request can
  never *win* a contest, so creating first can never take a path off anything either.
- **A dry run reconciles a candidate fleet with one request changed, so a split previews as an
  overlap it is itself about to resolve.** The bar says so rather than reporting a refusal, and only
  on the evidence: every holder of every contested path is in this set, and none of them would still
  hold it after their own dry run. The other half is corrected too — "2 of 2 paths stop" is true of
  that write alone and false of the set it is in, so those paths are counted as changing hands.
- **An edit is an intent about one leg, never a draft spec.** The fleet is replaced wholesale every
  poll, so a copied-and-mutated spec goes stale and needs conflict detection; `(request, leg, wanted
  state)` rebases for free against whatever is stored, and an edit the fleet has already satisfied
  stops being pending instead of being written a second time. The intent is a *state* rather than a
  verb for the same reason — after a rebase the verb can change and what the operator asked for
  cannot.
- **The grid renders the staged set, not the server's list.** That is what makes "a rectangle has no
  notches" render itself: parking is per *destination*, so one click darkens that column across every
  row of the request, on screen, before anything is written. The paths underneath stay the server's
  own — a staged cell says `staged` and never borrows a state it has not reached. **A cell lit by a
  staged *source* is as unwritten as one lit by a staged leg** and says so: a source arriving lights
  every column of its request at once, and a live run caught those cells reporting `ESTABLISHING` —
  a state belonging to the request's other, applied, source.
- **Un-parking deletes the key rather than writing `disabled: false`.** The flag is `omitempty` and
  its zero value is the one that keeps media running, so a spec carrying `false` is not identical to
  one that was never parked and an apply would write for nothing.
- **A refused change blocks Apply, and the server's prose is what the bar shows.** A refusal never
  resolves by itself; `reason_code` decides only what to highlight.
- **Both axes are banded by node, because each direction carries a real resource fact.** One source
  to five destinations is 5× egress on that node; twelve sources into one domain is 12× ingress on
  that one, which is the binding direction for an ingest wall.
- **A column is additive and is never drawn as an exclusive crosspoint.** An operator trained on an
  SDI router expects the second click in a column to displace the first; here a destination domain
  holds flows from many requests and fan-in is the supported way to land several sources in one. The
  column header counts its sources for exactly that reason.
- **The rectangle is an accent and a badge, never a border.** A request's rows need not be adjacent —
  rows sort by source node — so there is no outline whose geometry would survive the sort, and a
  border is layout: a row would grow the moment a second source joined its request.
- **Under `table-layout: fixed` the widths are declared in a `<colgroup>`.** The first row otherwise
  decides them, and the first row is the node bands, whose colspans hand a two-column band's width to
  whichever columns it covers. Every column is the same width whatever lands in it, which is the
  point of the rule.
- **Two different cross-namespace questions, and the wider one is easy to miss.** A foreign *holder*
  of a path is refcounting and decides whether deleting a claim stops anything. A foreign
  *namespace writing into the same domain* shares no path at all — it is ordinary fan-in, invisible
  from inside one namespace, and it is what makes "emptying this group empties the domain" false.
  The first is on the claim, the second on the group header, and computing the second needs the
  whole path list rather than this namespace's claims.
- **A cell is always exactly two lines.** Geometry is a correctness property, not styling: cells in
  a row share a height, so prose in one resizes the row. Reasons live in tooltips. A parked cell is
  drawn rather than blanked, because "nobody ever routed this" and "somebody did and switched it
  off" are different sentences.
- **Rows are grids with fixed tracks, never flex.** Under flex every item is content-sized, so a
  longer domain name moves the state word and everything after it. Only the last track may depend on
  content. Claim rows share the path row's stops — which also means they must share its *font size*,
  since a `ch` resolves against the element that uses it.
- **The structural guides carry meaning, not decoration.** The rule down a destination group is
  where one group ends; the rule and ticks under a path row are what tie its claims to it; the
  accent on a path marks that it is not `ACTIVE`, in that state's **own** colour, so the gutter reads
  as a distribution rather than a row of identical alarms — `PAUSED` comes up calm and blue where
  `FAILED` comes up red, which is the distinction §11 exists to preserve. The gutter is reserved on
  every path and filled only where there is something to mark, so the accent cannot move the layout
  it appears in — an inset shadow, never a border, for the same reason the prototype gives.
- **The editors' chrome is one global stylesheet, prefixed `ed-`.** A `<style scoped>` scopes a
  *component*, not the class names inside it, and this app has already paid for that once — a line
  class named `head` inherited `flex-wrap: wrap` from the page header's own `.head` in the same file
  and made one column's header a line taller than its neighbours. Two panels sharing one idiom need
  one sheet; a prefix nothing else uses is the honest version of what a scoped sheet was pretending
  to be. The grid's geometry rules do not apply inside a panel — nothing there shares a height with
  a cell — but *shown and disabled with the reason* does, everywhere.
- **No explanatory copy.** Operators are trained; the screen shows data. What a label means goes in
  a `title`, and the reasoning goes in these comments. Server-authored prose — reasons, refusals —
  is rendered verbatim, because it is diagnostic data rather than UI copy.

## Toolchain

Node is pinned in `.nvmrc` — `nvm use` before anything. Vite 7 and Vitest 3 both want a current LTS,
and jsdom's dependency tree is specific about the patch level.
