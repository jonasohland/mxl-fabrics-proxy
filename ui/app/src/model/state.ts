/**
 * The state fold, copied from the server so this screen's aggregates and its aggregates agree.
 *
 * Mirrors `aggregateOrder` and `fold` in `internal/server/reconcile/reconcile.go`. Where the two
 * disagree the server is right — but see {@link aggregate} for the one place they *cannot* agree,
 * which is why a request's own state is never recomputed here.
 */

import type { PathState, RequestState } from '@/api/types'

/**
 * Worst-first, and the order a request's state folds to: the first state present among its paths
 * wins.
 *
 * One deliberate exception to plain severity — `ESTABLISHING` outranks `PAUSED` and `ACTIVE` but not
 * `WAITING`. A request with one path coming up and one running is "coming up"; a request with one
 * path waiting on a missing flow is waiting, whatever the rest are doing. The counts carry the
 * detail an operator actually reads, so this only has to be defensible.
 *
 * `PARTIAL` is deliberately absent: it is not a state a path can be in, so it cannot be found among
 * them. It is applied on top of this fold, where the whole set is visible.
 */
export const AGGREGATE_ORDER: readonly PathState[] = [
  'INVALID', 'FAILED', 'DEGRADED', 'WAITING', 'ESTABLISHING', 'PAUSED', 'ACTIVE',
]

/** The worst state in a set, by {@link AGGREGATE_ORDER}. Undefined for an empty set. */
export function worst(states: readonly PathState[]): PathState | undefined {
  let best: PathState | undefined
  let bestRank = Number.POSITIVE_INFINITY
  for (const state of states) {
    const rank = AGGREGATE_ORDER.indexOf(state)
    if (rank >= 0 && rank < bestRank) {
      best = state
      bestRank = rank
    }
  }
  return best
}

/**
 * Fold a set of **path** states to the one word that describes them.
 *
 * `PARTIAL` claims that something is working, so it is never said when nothing is: a set with no
 * `ACTIVE` member folds worst-first. It also outranks `INVALID`, `FAILED` and `DEGRADED`, which is
 * the surprising half and is deliberate — a request expanding onto twenty paths with one conflict is
 * doing its job, and the loud detail belongs in the counts and the per-source breakdown rather than
 * on the line an operator reads first.
 *
 * **Use this for a cell, never for a request row.** Over a set of real paths it agrees with the
 * server exactly. Over a *request* it cannot: the server also folds leg failures — pairings refused
 * during validation, which produce no path at all — so a request whose every materialised path is
 * `ACTIVE` beside one invalid pairing is `PARTIAL` on the wire and would fold to `ACTIVE` here.
 * Those failures are not in `status.paths[]` and there is nothing to recompute them from. Render
 * `request.status.state`; this function is for the subsets the server does not aggregate.
 */
export function aggregate(states: readonly PathState[]): RequestState | undefined {
  if (states.length === 0) return undefined
  const distinct = new Set(states)
  if (distinct.size > 1 && distinct.has('ACTIVE')) return 'PARTIAL'
  return worst(states)
}

/**
 * Display order: worst-first, as the CLI sorts and the UI should.
 *
 * `PARTIAL` leads because it outranks everything the fold can produce. `DISABLED` sorts **below**
 * `ACTIVE` rather than above `INVALID` — worst-first is a queue of things to look at, and parked
 * intent is not one of them. It must stay countable without being loud.
 */
export const DISPLAY_ORDER: readonly RequestState[] = [
  'PARTIAL', ...AGGREGATE_ORDER, 'DISABLED',
]

export function displayRank(state: RequestState): number {
  const rank = DISPLAY_ORDER.indexOf(state)
  return rank < 0 ? DISPLAY_ORDER.length : rank
}

/** Worst-first comparator for rows, requests or paths. */
export function byWorstFirst<T>(stateOf: (item: T) => RequestState) {
  return (a: T, b: T) => displayRank(stateOf(a)) - displayRank(stateOf(b))
}

/**
 * Whether this state is something an operator should be looking at.
 *
 * `PAUSED` is **not** a problem and is the most valuable state in the vocabulary — it separates "is
 * the plumbing broken" from "is the source not producing", which look identical from a no-media
 * alarm and have completely different owners. A UI that folds it into a red bucket has destroyed
 * the one signal this design added. `DISABLED` is not a problem either: somebody switched it off.
 */
export function needsAttention(state: RequestState): boolean {
  return state === 'INVALID' || state === 'FAILED' || state === 'DEGRADED' || state === 'PARTIAL'
}

/**
 * Counts with a floor of zero, in a fixed order.
 *
 * `status.counts` omits zeros — a request with one establishing path returns `{"ESTABLISHING": 1}`
 * and nothing else — so a chart built from its keys shows a gap where it should show a floor.
 */
export function countsInOrder(
  counts: Partial<Record<RequestState, number>> | undefined,
  vocabulary: readonly RequestState[],
): { state: RequestState; count: number }[] {
  return vocabulary.map((state) => ({ state, count: counts?.[state] ?? 0 }))
}
