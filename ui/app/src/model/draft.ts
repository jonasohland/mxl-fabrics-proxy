/**
 * A request that does not exist yet, and a destination no request names yet.
 *
 * **These are the one place a spec is the right shape for authored work, and the boundary is worth
 * stating** — `model/staging.ts` argues at length that an edit must be an intent about one leg
 * rather than a copy of a spec, because the fleet is replaced wholesale every poll and somebody
 * else's apply may have rewritten the request underneath. That argument is about a request the
 * *server holds*. A request that does not exist has no stored copy to be rebased against, so there
 * is nothing for an intent to be an intent about: the operator's text is the only text there is.
 * A draft is therefore a spec, and that is the edge of the rule rather than an exception to it.
 *
 * Two consequences follow and both are enforced by the shape of {@link Draft} rather than by care:
 *
 * - **A draft is complete exactly when it names a source and a destination**, which is the API's own
 *   rule. Short of that it is not a commit at all: it sits on the axis as a row waiting to be routed
 *   rather than appearing in the bar as a change that cannot be applied.
 * - **Every cell of a draft is unwritten**, whether its destination came from the operator authoring
 *   the request or from a leg staged onto it afterwards. That is the property the renderer needs, and
 *   it is a fact about the *request* rather than about any one leg.
 *
 * *This supersedes "a draft carries sources and never destinations", which held while the source
 * editor was the only thing that made one.* The invariant bought exactly one thing — every leg being
 * an ordinary staged edit, so a draft's cells rendered as unwritten for free — and it cost the two
 * controls that copy a rectangle: `duplicate` and `split` carry the **entries**, not just the places,
 * and a copy that dropped a per-destination `provider` override or a parked flag would silently start
 * media on a leg the original deliberately has switched off. The renderer asks whether the request is
 * a draft instead, which is what it should have been asking.
 *
 * **Drafts are session-scoped, and that is inherent here rather than a shortcut.** `ui.md` §7a
 * records the prototype's client-side retention of emptied rows as a workaround for *off* having
 * nowhere to be written down, and `disabled` replaced it. This is the other case and it has no such
 * fix: a request nobody has created has no server-side home by definition, so an authored row is
 * gone on reload and was never there for a second operator. The cost is bounded by the same
 * property that makes it acceptable — the moment a draft has one destination it can be applied, and
 * applying is what gives it a home.
 */

import type { Destination, Request, RequestSpec, Source } from '@/api/types'
import { requestId } from './labels'
import { endpointKey } from './ownership'

/**
 * The request-level tail: what belongs to the **rectangle** rather than to any one of its legs.
 *
 * Carried on a draft because `duplicate` and `split` copy a request rather than a route, and every
 * one of these changes what the copy does. A dropped `provider` pin is a performance cliff whose
 * symptom looks like a source problem (architecture §10.4); dropped `labels` change what
 * `apply --prune` will delete. The per-destination `provider` override is *not* here — that one
 * lives on the entry, which is where the API puts it and where the copy carries it.
 */
export type DraftSettings = Pick<
  RequestSpec,
  'provider' | 'idle_teardown_ms' | 'sched_prio' | 'labels'
>

/** The tail of an existing request, with absent fields left absent rather than written as undefined. */
export function settingsOf(request: Request): DraftSettings {
  return {
    ...(request.provider !== undefined ? { provider: request.provider } : {}),
    ...(request.idle_teardown_ms !== undefined ? { idle_teardown_ms: request.idle_teardown_ms } : {}),
    ...(request.sched_prio !== undefined ? { sched_prio: request.sched_prio } : {}),
    ...(request.labels !== undefined ? { labels: { ...request.labels } } : {}),
  }
}

/** A request the operator has authored and not yet applied. */
export interface Draft {
  /** `<namespace>/<name>` — the same identity spelling a real request carries, so one map holds both. */
  id: string
  namespace: string
  name: string
  /** At least one. A draft that loses its last source is discarded rather than kept empty. */
  sources: Source[]
  /**
   * The entries as authored — **copied whole** when a rectangle is duplicated or split.
   *
   * A parked entry stays parked and a per-destination `provider` override comes with it, because the
   * operator asked for a copy of the request rather than for a list of the places it writes to. A leg
   * staged onto a draft afterwards is an ordinary edit and carries only its node and its domain, for
   * the opposite reason: nobody asked to copy the flag of whichever request defined the column.
   */
  destinations: Destination[]
  /** Empty for a request authored from nothing: the server's own defaults are the right ones. */
  settings: DraftSettings
}

/**
 * A destination named but not yet routed.
 *
 * The column set is "every destination some request names", so a newly named one has nowhere to
 * come from until a request names it — and it has to be on the axis first, because naming it *is*
 * how an operator gets a cell to click. It leaves the list on its own the moment a request names
 * it, which is what keeps the two sources of columns from disagreeing.
 */
export interface DraftColumn {
  namespace: string
  /** {@link endpointKey} — `(node, rendered domain)`, the same identity the matrix's columns use. */
  key: string
  destination: Destination
}

export function newDraft(
  namespace: string,
  name: string,
  sources: Source[],
  destinations: Destination[] = [],
  settings: DraftSettings = {},
): Draft {
  return { id: requestId({ namespace, name }), namespace, name, sources, destinations, settings }
}

export function newDraftColumn(namespace: string, destination: Destination): DraftColumn {
  return { namespace, key: endpointKey(destination), destination }
}

/**
 * The draft as the rest of the app sees a request.
 *
 * Everything downstream — the staged set, the effective spec, the matrix's axes and cells — is
 * written against `Request`, and none of it needs to know that this one is not on the server. What
 * would be a lie is the **status**, so it says the least it can: no paths, because it holds none,
 * and `WAITING` because that is the closest true word in a vocabulary that has none for *not
 * written*. Nothing renders it — every surface that shows a draft's state asks the staging store
 * whether it is a draft first, and shows that instead.
 */
export function draftRequest(draft: Draft): Request {
  return {
    ...draft.settings,
    id: draft.id,
    namespace: draft.namespace,
    name: draft.name,
    sources: draft.sources,
    destinations: draft.destinations,
    created_at: '',
    status: { state: 'WAITING', paths: [] },
  }
}
