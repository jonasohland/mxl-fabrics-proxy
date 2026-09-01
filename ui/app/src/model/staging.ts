/**
 * The staged set: what the operator has authored, held apart from what the server holds.
 *
 * `ui.md` §7a gives two defensible commit models and this is the one it recommends. **Apply on
 * click** is what the borrowed idiom suggests — a router's take button is instantaneous — but a click
 * here is durable intent that moves uncompressed video, one click can light several cells of a
 * rectangle, and each toggle would need a confirmation in front of it. **Stage and apply** earns its
 * keep for a reason that was not obvious in advance: a staged set can be dry-run *as a batch*, so the
 * preview reports real outcomes and real blast radius before anything moves, and that is what removes
 * every confirmation dialog from the interface. "3 of 4 paths stop" is worth reading; "are you sure?"
 * is not.
 *
 * **An edit is an intent about one leg, not a draft spec.** The obvious shape — copy the request's
 * spec, mutate it, POST it — goes stale the moment the poll lands, because the fleet is replaced
 * wholesale every three seconds and somebody else's `apply` may have rewritten the request underneath.
 * A draft would then need conflict detection, which is a whole feature. An edit that names
 * `(request, leg, wanted state)` instead is **rebased for free**: the effective spec is recomputed
 * from the freshest stored one every time it is read, and an edit whose target entry has gone away
 * simply stops applying.
 *
 * **The intent is a state, not a verb.** `want: 'on' | 'off'` rather than park/enable/add, for the
 * same reason: after a rebase the verb can change (a leg somebody else removed is now an *add* rather
 * than an *enable*) while what the operator asked for cannot. The verb is derived for display, where
 * being wrong costs a word rather than a write.
 *
 * The two properties that follow are what make the toggle feel right, and both are `ui.md` §7a rules
 * rather than conveniences:
 *
 * - **An edit that matches what the server already holds is not a change.** `unchanged` means do not
 *   write, and a UI that re-POSTs on every interaction turns a resyncing screen into store churn.
 * - **Un-parking deletes the key rather than writing `disabled: false`.** The flag is `omitempty` and
 *   the zero value is the one that keeps media running, so a spec carrying `false` is not identical to
 *   one that was never parked and an apply would write for nothing.
 */

import type { Destination, Path, Request, RequestSpec, Source } from '@/api/types'
import { sourceKey } from './matrix'
import { endpointKey } from './ownership'

/**
 * What the operator wants the leg to be. Not what to do to it — see the note above.
 *
 * `gone` is the one that is not a state of a leg but the absence of one, and it is what the `×`
 * controls stage. It is deliberately a third value here rather than a second kind of edit: it is
 * still an answer to "what should this leg be", it is still keyed on the same `(request, column)`,
 * and it still supersedes whatever was staged there before.
 */
export type Want = 'on' | 'off' | 'gone'

/** How that reads against what is stored right now. Derived for display, never stored. */
export type Verb = 'park' | 'enable' | 'add' | 'remove'

/** A destination entry of one request: what the cell and the two destination-side `×`s stage. */
export interface LegEdit {
  target: 'leg'
  /** `<namespace>/<name>` — the rendered id, which is what a path's refcount list also carries. */
  request: string
  /** {@link endpointKey} of the leg: `(node, rendered domain)`. */
  column: string
  want: Want
  /**
   * The destination as a **structured** entry, kept because `on` may have to create one and a
   * rendered domain must never be split back apart to send it (`ui.md` §5 trap 1).
   */
  destination: Destination
}

/** Whether a source is in the request or not. A state, for the same reason {@link Want} is one. */
export type Membership = 'in' | 'out'

/**
 * A source entry of one request: what the row's `×` and the source editor stage.
 *
 * `out` is the row's `×`. There is no flag on a source, so a row has no parked state to be put into
 * first — which is why that `×` is the one destructive one, and `ui.md` §7a's answer if it ever
 * grates is a flag on a source rather than a rule bent here.
 *
 * `in` is the source editor adding a row to a request that already exists, which is how fan-in is
 * authored: "every camera in studio A, B and C onto the ingest wall" is one intent, one name and one
 * delete. It is a *state* rather than an `add` verb for the same reason a leg's is — after a rebase
 * the verb can change while what the operator asked for cannot — and it costs nothing here: the
 * request's destinations belong to every one of its sources, so a source arriving lights the whole
 * row at once and the grid shows that before Apply.
 */
export interface SourceEdit {
  target: 'source'
  request: string
  /** The entry itself: {@link sourceKey} is its identity and the label is read off the same object. */
  source: Source
  want: Membership
}

export type Edit = LegEdit | SourceEdit

/**
 * Identity of an edit's *target*, so a second edit on one target replaces the first.
 *
 * NUL as the separator, and written as an **escape** rather than as a literal byte — the same rule
 * and the same reason as `model/ownership.ts`. It was a literal here, and the file duly stopped being
 * text: `grep` reported it as binary and printed nothing, silently, for every search anybody ran
 * over it.
 */
export function editKey(edit: Edit): string {
  const target = edit.target === 'leg' ? edit.column : sourceKey(edit.source)
  return `${edit.target}\u0000${edit.request}\u0000${target}`
}

/** `(namespace, name)` is the request's identity, and the rendered id carries both. */
export function namespaceOfId(id: string): string {
  return id.slice(0, id.indexOf('/'))
}

function entryAt(request: Request, column: string): Destination | undefined {
  return request.destinations.find((destination) => endpointKey(destination) === column)
}

/**
 * Whether this edit still asks for something the stored spec does not already say.
 *
 * Read on every render rather than decided when the edit was made, so a rebase drops an edit somebody
 * else's apply has already satisfied instead of leaving it pending forever against a spec that
 * matches it. A removal of something that is already gone is the clearest case of that.
 */
export function isChange(request: Request, edit: Edit): boolean {
  if (edit.target === 'source') {
    const key = sourceKey(edit.source)
    const present = request.sources.some((source) => sourceKey(source) === key)
    return edit.want === 'out' ? present : !present
  }

  const entry = entryAt(request, edit.column)
  if (edit.want === 'gone') return entry !== undefined
  return edit.want === 'on'
    ? entry === undefined || entry.disabled === true
    : entry !== undefined && entry.disabled !== true
}

export function verb(request: Request, edit: Edit): Verb {
  if (edit.target === 'source') return edit.want === 'out' ? 'remove' : 'add'
  if (edit.want === 'gone') return 'remove'
  if (edit.want === 'off') return 'park'
  return entryAt(request, edit.column) === undefined ? 'add' : 'enable'
}

/**
 * The source entries the staged set leaves behind.
 *
 * Removals filter and additions **append**, which is not an arbitrary order: `status.sources[i]` is
 * joined to `sources[i]` by index, so a source arriving in the middle would shift every breakdown
 * entry after it and attribute one source's live paths to another's row. Appended, the kept prefix
 * keeps its correspondence and the new entry simply has no breakdown — which is the truth, since it
 * has expanded onto nothing yet.
 */
export function effectiveSources(request: Request, edits: readonly Edit[]): Source[] {
  const sources = edits.filter((edit): edit is SourceEdit => edit.target === 'source')
  if (sources.length === 0) return request.sources

  const removed = new Set(
    sources.filter((edit) => edit.want === 'out').map((edit) => sourceKey(edit.source)),
  )
  const kept = removed.size === 0
    ? request.sources
    : request.sources.filter((source) => !removed.has(sourceKey(source)))

  const present = new Set(kept.map((source) => sourceKey(source)))
  const added: Source[] = []
  for (const edit of sources) {
    if (edit.want !== 'in') continue
    const key = sourceKey(edit.source)
    // A source the request already names is not an addition. Two requests naming the same three
    // things are one row, so this is the same identity the grid's axis is built on.
    if (present.has(key)) continue
    present.add(key)
    added.push(edit.source)
  }

  return added.length === 0 ? kept : [...kept, ...added]
}

/**
 * The request's destination list with the edits applied.
 *
 * **A parked entry is not removed**, which is the whole of why `disabled` exists: the entry stays in
 * the spec, expands to nothing, and keeps its column on the axis. Removing it would delete the column
 * it lived on and rearrange the board under the pointer (`ui.md` §7a, "Off is a value, not an
 * absence").
 *
 * **`gone` is the one thing here that does remove it**, and it is the `×` rather than the cell: `×`
 * only ever removes something already dark, so by the time this runs the leg is parked and carrying
 * nothing. The column it lived on goes with it if no other request names it — which is the board
 * rearranging itself *because the operator asked*, not because something became unused.
 *
 * A created entry carries **only** its node and its domain. The representative destination an edit
 * arrives with is whichever request defined the column first, and it may hold that request's
 * `provider` override or its parked flag — neither of which the operator asked to copy into this one.
 */
export function effectiveDestinations(request: Request, edits: readonly Edit[]): Destination[] {
  const legs = edits.filter((edit): edit is LegEdit => edit.target === 'leg')
  if (legs.length === 0) return request.destinations

  const destinations = request.destinations.map((destination) => ({ ...destination }))
  for (const edit of legs) {
    const index = destinations.findIndex((destination) => endpointKey(destination) === edit.column)

    if (edit.want === 'gone') {
      if (index >= 0) destinations.splice(index, 1)
      continue
    }

    if (edit.want === 'off') {
      // Nothing to park: the entry has gone since the edit was made. The edit is dropped by
      // `isChange` on the next read; skipping here keeps this function total.
      if (index >= 0) destinations[index]!.disabled = true
      continue
    }

    if (index >= 0) delete destinations[index]!.disabled
    else destinations.push({ node: edit.destination.node, domain: edit.destination.domain })
  }
  return destinations
}

/**
 * The request as the staged set would have it: the stored status, the authored spec.
 *
 * The status is deliberately the server's own and is **not** adjusted to match the edits. A path that
 * exists still exists until an apply stops it, and the grid drawing a staged park over live paths is
 * the honest picture — what those paths will do is the preview's job, and the cell says `staged`
 * rather than borrowing a state it has not reached.
 */
export function effectiveRequest(request: Request, edits: readonly Edit[]): Request {
  if (edits.length === 0) return request

  const sources = effectiveSources(request, edits)

  // `status.sources[i]` is joined to `sources[i]` **by index** — that is how a path is attributed to
  // the source that asked for it (`model/ownership.ts`). Dropping a source without dropping its
  // breakdown would shift every entry after it and attribute one source's live paths to another's
  // row, which is the same class of silent error as trap 14 and would show media on a row that is
  // not carrying it. Filtered by the same predicate, so the correspondence survives — and an added
  // source appends, so it lands past the end of the breakdown and correctly owns nothing.
  const keep = new Set(sources.map((source) => sourceKey(source)))
  const breakdown = request.status.sources?.filter((entry) => keep.has(sourceKey(entry.source)))

  return {
    ...request,
    sources,
    destinations: effectiveDestinations(request, edits),
    status: { ...request.status, ...(breakdown ? { sources: breakdown } : {}) },
  }
}

/**
 * A spec with no sources or no destinations cannot be POSTed, so committing it is a `DELETE`.
 *
 * `ui.md` §7a: *"an empty request is a state, not an event"* — let it sit empty, say what applying
 * will do, and let the commit work out that an empty spec means delete. The ordering hazard the
 * passage warns about does not arise here, because the staged set is a set: removing the last
 * destination and then adding another reaches the same effective spec whichever order they are
 * clicked in, and neither click destroyed anything on the way.
 */
export function isEmpty(request: Request): boolean {
  return request.sources.length === 0 || request.destinations.length === 0
}

/**
 * The POST body, stripped of everything derived.
 *
 * An explicit pick rather than a rest destructure: `Request` extends `RequestSpec`, so anything the
 * API adds to the former would otherwise ride into the body of every apply this screen makes.
 * Undefined keys are omitted rather than written, because a spec is compared for equality server-side
 * and `unchanged` is the outcome worth reaching.
 */
export function specOf(request: Request): RequestSpec {
  return {
    ...(request.namespace !== undefined ? { namespace: request.namespace } : {}),
    name: request.name,
    sources: request.sources,
    destinations: request.destinations,
    ...(request.provider !== undefined ? { provider: request.provider } : {}),
    ...(request.idle_teardown_ms !== undefined ? { idle_teardown_ms: request.idle_teardown_ms } : {}),
    ...(request.sched_prio !== undefined ? { sched_prio: request.sched_prio } : {}),
    ...(request.labels !== undefined ? { labels: request.labels } : {}),
  }
}

/**
 * What applying one request's staged edits would do to media, read off the dry run.
 *
 * The dry run runs the identical path and skips only the write, so `result.status.paths[]` is what
 * this request *would* expand onto — with real IDs, which is what makes the comparison against the
 * live path list possible at all.
 *
 * Two distinctions the count would be dishonest without:
 *
 * - **A path this request loses does not necessarily stop.** It survives while another request still
 *   references it, and `path.requests[]` says so at no cost. That is the difference between "4 paths
 *   leave this request" and "3 paths stop", and only the second is what an operator is deciding on.
 * - **A path that appears is not necessarily new.** One that already exists under somebody else's
 *   claim is joined, refcounted, and starts nothing — which is ordinary across namespaces, since
 *   namespaces partition requests and not destinations.
 */
export interface BlastRadius {
  /** Paths this request holds now that the result does not carry. */
  stopping: Path[]
  /** Of those, the ones another request keeps alive. Nothing stops. */
  ridesAlong: Path[]
  /**
   * Of those, the ones a request **in the staged set** is taking over — a split, seen from the side
   * that is giving them up.
   *
   * Filled by the staging store rather than here: it is the only thing that can see the rest of the
   * set and each of its dry runs. Left empty, every caller reads the same number it always did.
   */
  handedOff: Path[]
  /** `stopping` less `ridesAlong` and `handedOff` — the number that is worth reading. */
  stops: number
  /** Paths in the result that exist nowhere today. */
  starting: string[]
  /** Paths in the result that exist already under another claim: refcounted, nothing starts. */
  joining: string[]
  /**
   * The result would hold nothing it lists.
   *
   * `ui.md` §5 trap 14: the loser of a namespace overlap goes `INVALID` and **still lists the
   * contested path with the incumbent's state**. A dry run answers with an `api.Request` and no
   * candidate path list, so there is no `path.requests[]` to cross-check against — the request's own
   * `reason_code` is the only signal available, and it is enough to stop the counts being read as
   * this request's own.
   */
  holdsNothing: boolean
}

export function blastRadius(
  request: Request,
  result: Request | undefined,
  paths: readonly Path[],
): BlastRadius {
  const held = paths.filter((path) => path.requests.includes(request.id))
  const empty: BlastRadius = {
    stopping: [], ridesAlong: [], handedOff: [], stops: 0,
    starting: [], joining: [], holdsNothing: false,
  }
  if (result === undefined) return empty
  return against(held, new Set(result.status.paths.map((path) => path.id)), paths, result)
}

/**
 * The same, for a commit that is a `DELETE`.
 *
 * There is no dry run to read it off — `DELETE` takes no `dry_run` — and there does not need to be:
 * cancelling an intent drops every path it holds, and which of those actually stop is `path.requests[]`
 * and nothing else. That is the preview `ui.md` §3 asks any cancellation to show, and it is exactly
 * the ledger's `sole`/`shared` count arriving from the other side.
 */
export function removalRadius(request: Request, paths: readonly Path[]): BlastRadius {
  return against(paths.filter((path) => path.requests.includes(request.id)), new Set(), paths)
}

function against(
  held: Path[],
  wanted: Set<string>,
  paths: readonly Path[],
  result?: Request,
): BlastRadius {
  const stopping = held.filter((path) => !wanted.has(path.id))
  const ridesAlong = stopping.filter((path) => path.requests.length > 1)

  const heldIds = new Set(held.map((path) => path.id))
  const live = new Set(paths.map((path) => path.id))
  const appearing = [...wanted].filter((id) => !heldIds.has(id))

  return {
    stopping,
    ridesAlong,
    handedOff: [],
    stops: stopping.length - ridesAlong.length,
    starting: appearing.filter((id) => !live.has(id)),
    joining: appearing.filter((id) => live.has(id)),
    holdsNothing: result?.status.reason_code === 'namespace_overlap',
  }
}
