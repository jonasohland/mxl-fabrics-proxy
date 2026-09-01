/**
 * The pending set, its batch dry run, and the one apply.
 *
 * Everything the operator authors lands here and nothing reaches the server until Apply. That is the
 * commit model `ui.md` §7a recommends, and the reason is the preview rather than the undo window:
 * because the staged set can be dry-run **as a batch**, the bar can report real outcomes and real
 * blast radius before anything moves, which is what lets every confirmation dialog come out of the
 * interface. Staging *is* the confirmation — a dialog that appears mid-gesture gets dismissed
 * reflexively, while a staged change has to be read and applied.
 *
 * Three properties worth stating, because each is a rule from somewhere else in the design:
 *
 * - **The set is fleet-wide, not per screen.** A request id carries its namespace, so a staged edit
 *   knows which namespace it belongs to and navigating away cannot silently discard work.
 * - **Drafts are held here too, and they are the one thing that is a spec.** A request that does not
 *   exist has no stored copy for an edit to be rebased against, so the operator's text is the only
 *   text there is (`model/draft.ts`). Everything downstream is unaffected: a draft answers to
 *   `Request`, its legs are ordinary staged edits, and it reaches the server through the same dry
 *   run and the same apply as everything else.
 * - **The dry run is debounced and fires on the staged set, not on the poll.** Each one is a full
 *   store load plus a reconcile (`ui.md` §2), so it re-runs when the *specs* change — which includes
 *   a poll that rebased one underneath — and not merely when the fleet's state moves.
 * - **Apply is one POST per request.** There is no batch create, so a failure part-way through has
 *   written some of them; exactly what landed is dropped from the set and the rest stays staged.
 */

import { defineStore } from 'pinia'
import { computed, ref, shallowRef, watch } from 'vue'

import { ApiError, api } from '@/api/client'
import type { Destination, Outcome, ReasonCode, Request, RequestSpec, Source } from '@/api/types'
import type { Draft, DraftColumn, DraftSettings } from '@/model/draft'
import { draftRequest, newDraft, newDraftColumn, settingsOf } from '@/model/draft'
import { namespaceOf } from '@/model/ledger'
import { sourceKey } from '@/model/matrix'
import { endpointKey } from '@/model/ownership'
import type { BlastRadius, Edit, Verb, Want } from '@/model/staging'
import {
  blastRadius,
  editKey,
  effectiveRequest,
  isChange,
  isEmpty,
  namespaceOfId,
  removalRadius,
  specOf,
  verb,
} from '@/model/staging'
import { useFleetStore } from './fleet'

/**
 * Long enough that a burst of clicks costs one round of reconciles rather than one per click, short
 * enough that the bar is not visibly waiting. `ui.md` §7a asks for the debounce explicitly and for
 * the run to fire on a *structurally complete* change — which every staged edit is, since each one
 * either flips a boolean on an entry that exists or appends a destination that another request has
 * already named.
 */
const DEBOUNCE_MS = 400

/** One request's dry run. Either the request that would result, or the refusal that stopped it. */
export interface Preview {
  id: string
  spec: RequestSpec
  result?: Request
  /** `created` / `updated` / `unchanged` — the status code cannot tell you this. */
  outcome?: Outcome
  /** Server-authored prose, rendered verbatim: it is better than anything this UI would write. */
  error?: string
  /** Decides only what to highlight. */
  reasonCode?: ReasonCode
}

/** One request's worth of the pending set, as the bar renders it. */
export interface StagedRequest {
  id: string
  namespace: string
  name: string
  edits: Edit[]
  /**
   * What the commit is.
   *
   * A request must name at least one source and at least one destination, so a spec the `×` controls
   * have emptied has nothing to POST and committing it is a `DELETE` — `ui.md` §7a's "an empty
   * request is a state, not an event". `spec` is undefined exactly then, so nothing downstream can
   * accidentally send an empty one.
   */
  spec: RequestSpec | undefined
  deleting: boolean
}

export const useStagingStore = defineStore('staging', () => {
  const fleet = useFleetStore()

  /** Insertion order, which is the order the bar lists them in. */
  const edits = ref<Edit[]>([])
  const previews = shallowRef(new Map<string, Preview>())
  const previewing = ref(false)
  const applying = ref(false)
  const applyError = ref<string | undefined>(undefined)

  /** What the editors have authored. Read through {@link drafts} and {@link columns}, never here. */
  const authoredDrafts = ref<Draft[]>([])
  const authoredColumns = ref<DraftColumn[]>([])

  /**
   * Drafts, less any the server has since come to hold.
   *
   * The same rebase discipline the edits run on, for the same reason: the fleet is replaced wholesale
   * every poll, and a name this UI authored may have been created by an apply here, by another
   * operator or by an adapter in between. Whichever it was, the server's copy is now the request and
   * the draft is a stale second spelling of it — so it stops being one on the next poll rather than
   * having to be cleaned up by whatever wrote it.
   */
  const drafts = computed(() => {
    const held = new Set(fleet.requests.map((request) => request.id))
    return authoredDrafts.value.filter((draft) => !held.has(draft.id))
  })

  const draftIds = computed(() => new Set(drafts.value.map((draft) => draft.id)))

  const isDraft = (id: string) => draftIds.value.has(id)

  /**
   * Every request the screen knows about, the server's and the authored.
   *
   * The server's are added second and would win a collision, which cannot arise — {@link drafts}
   * has already dropped any draft whose id the server holds — but is the right way round anyway.
   */
  const byId = computed(() => {
    const map = new Map<string, Request>()
    for (const draft of drafts.value) map.set(draft.id, draftRequest(draft))
    for (const request of fleet.requests) map.set(request.id, request)
    return map
  })

  /**
   * The edits that still ask for something.
   *
   * Filtered on every read rather than pruned on write, because the fleet is replaced wholesale every
   * poll: a request somebody deleted has nothing to apply to, and one somebody else already parked
   * has nothing left to ask for. Either way the edit is not a pending change and must not be counted
   * as one or POSTed as one.
   */
  const pending = computed(() =>
    edits.value.filter((edit) => {
      const request = byId.value.get(edit.request)
      return request !== undefined && isChange(request, edit)
    }),
  )

  const byRequest = computed(() => {
    const grouped = new Map<string, Edit[]>()
    for (const edit of pending.value) {
      const list = grouped.get(edit.request)
      if (list) list.push(edit)
      else grouped.set(edit.request, [edit])
    }
    return grouped
  })

  /**
   * One commit per touched request, built from the freshest stored spec every time it is read.
   *
   * **Drafts first, and that ordering is load-bearing rather than tidy.** `apply` walks this list in
   * order, and the case that decides it is a split: one request gives a source up and a new one takes
   * it over. Create the new one first and it is refused the contested path — it is the newer stamp,
   * so it loses (`ui.md` §7b) — but the path stays held by the incumbent; the update that follows
   * drops the source, and the reconcile it triggers sees exactly one claimant, so **no reconcile ever
   * sees the path with none**. Do it the other way round and there is one in between that does, which
   * is a teardown and a rebuild of a session that had no reason to move. A newly created request can
   * never *win* a contest, so creating first can never take a path off anything either.
   *
   * *Reasoned from the reconciler's rules rather than observed.* The fake fleet cannot tell the two
   * orders apart: a session id is a deterministic hash of the path identity and the flow definition
   * (`state.SessionID`), so it survives a rebuild unchanged, and nothing in `ui/prototype/devfleet.sh`
   * runs a worker whose restart count would show.
   */
  const staged = computed<StagedRequest[]>(() => {
    const ids = [
      ...drafts.value.map((draft) => draft.id),
      ...[...byRequest.value.keys()].filter((id) => !isDraft(id)),
    ]

    const out: StagedRequest[] = []
    for (const id of ids) {
      const list = byRequest.value.get(id) ?? []
      const effective = effectiveRequest(byId.value.get(id)!, list)
      // A draft short of a source or a destination is not a commit: it is a row on the axis waiting
      // to be routed. And a draft can never become a `DELETE` — there is nothing on the server to
      // cancel, and the name may be somebody else's by the time it would be sent.
      if (isDraft(id)) {
        if (isEmpty(effective)) continue
      } else if (list.length === 0) {
        continue
      }

      const deleting = !isDraft(id) && isEmpty(effective)
      out.push({
        id,
        namespace: namespaceOfId(id),
        name: effective.name,
        edits: list,
        spec: deleting ? undefined : specOf(effective),
        deleting,
      })
    }
    return out
  })

  /**
   * Every request as the staged set would have it. **The grid renders this, not the server's list.**
   *
   * That is what makes the rectangle's own rule render itself: a request is sources × destinations, so
   * parking one leg darkens that column across the whole rectangle, and the operator sees it in the
   * grid before Apply rather than reading about it in a dialog.
   */
  const effectiveRequests = computed(() =>
    [...byId.value.values()].map((request) =>
      effectiveRequest(request, byRequest.value.get(request.id) ?? []),
    ),
  )

  /**
   * Destinations the operator has named that no request routes yet, per namespace.
   *
   * Filtered against the **effective** request list rather than the server's, so a column stops
   * being a draft the moment a leg is staged onto it: the request is then the authority on it, and
   * a draft entry beside that would be a second source of truth for one column. The key is the
   * same either way, so nothing moves on screen when the hand-off happens.
   */
  const columns = computed(() => {
    // Keyed on `namespaceOf` rather than on the field: `namespace` is omitempty on the wire and
    // empty reads as `default`, so a request in the default namespace and a draft column in it
    // would key differently and the column would survive its own request naming it.
    const named = new Set(
      effectiveRequests.value.flatMap((request) =>
        request.destinations.map((destination) => `${namespaceOf(request)} ${endpointKey(destination)}`),
      ),
    )
    return authoredColumns.value.filter(
      (column) => !named.has(`${column.namespace} ${column.key}`),
    )
  })

  /** The ones this screen puts on its axis. */
  const columnsIn = (namespace: string) =>
    columns.value.filter((column) => column.namespace === namespace).map((column) => column.destination)

  const draftsIn = (namespace: string) =>
    drafts.value.filter((draft) => draft.namespace === namespace)

  /**
   * The overlaps this set is itself about to resolve, and who it takes them from.
   *
   * A dry run reconciles a candidate fleet with **one** request changed, so a `split` previews badly
   * through no fault of its own: the new request claims paths the original still holds, loses the
   * contest for being the newer stamp, and reports `namespace_overlap` — while the very same staged
   * set contains the update that gives them up. Rendered as "would hold none of the paths it lists"
   * that is a true sentence about a fleet nobody is going to create, and it reads as a refusal.
   *
   * The evidence is already on hand and needs no extra read: the loser lists the contested paths
   * (`ui.md` §5 trap 14), `/v1/paths` says who holds each one, and each holder's own dry run says
   * whether it would still be holding it. A holder that is not staged at all is a real overlap and
   * stays one — this only ever quiets the case the set is about to fix.
   */
  const handoffs = computed(() => {
    const out = new Map<string, { paths: string[]; from: string[] }>()
    const byPath = new Map(fleet.paths.map((path) => [path.id, path]))

    for (const entry of staged.value) {
      const result = previews.value.get(entry.id)?.result
      if (result?.status.reason_code !== 'namespace_overlap') continue

      const from = new Set<string>()
      const resolved = result.status.paths.every((listed) => {
        const path = byPath.get(listed.id)
        // Not live: nothing holds it, so it is no evidence of a contest either way.
        if (!path) return true
        return path.requests.every((holder) => {
          if (holder === entry.id) return true
          const other = staged.value.find((candidate) => candidate.id === holder)
          if (!other) return false
          from.add(holder)
          // A cancellation gives up everything it holds; anything else is read off its own dry run.
          if (other.deleting) return true
          const kept = previews.value.get(holder)?.result
          return kept !== undefined && !kept.status.paths.some((entry) => entry.id === listed.id)
        })
      })

      if (resolved && from.size > 0) {
        out.set(entry.id, {
          paths: result.status.paths.map((listed) => listed.id),
          from: [...from].sort(),
        })
      }
    }
    return out
  })

  /**
   * Every path some staged request is taking over, so it leaves its current holder without stopping.
   *
   * This is what makes the other half of a split honest. Read on its own, the incumbent's dry run
   * says it loses two paths and the refcounts say nobody else holds them — "2 of 2 paths stop", which
   * is true of that write alone and false of the set it is in.
   */
  const handedOver = computed(
    () => new Set([...handoffs.value.values()].flatMap((entry) => entry.paths)),
  )

  /**
   * What applying each staged request would do to media. Live, because it reads the current paths.
   *
   * The one thing the model cannot work out for itself is filled in here: a path leaving this request
   * because **another request in this set** is taking it over does not stop, and only the store can
   * see the rest of the set. Subtracted from `stops` rather than added beside it, because `stops` is
   * the number the bar leads with and it has to be the honest one.
   */
  const radius = computed(() => {
    const map = new Map<string, BlastRadius>()
    const taken = handedOver.value
    for (const entry of staged.value) {
      const request = byId.value.get(entry.id)!
      // A `DELETE` has no dry run to read this off and needs none: cancelling an intent drops every
      // path it holds, and which of them stop is `path.requests[]`.
      const computedRadius = entry.deleting
        ? removalRadius(request, fleet.paths)
        : blastRadius(request, previews.value.get(entry.id)?.result, fleet.paths)

      const handedOff = computedRadius.stopping.filter(
        (path) => taken.has(path.id) && !computedRadius.ridesAlong.includes(path),
      )
      map.set(entry.id, {
        ...computedRadius,
        handedOff,
        stops: computedRadius.stops - handedOff.length,
      })
    }
    return map
  })

  /**
   * How many changes are staged.
   *
   * An edit is one and so is a **draft with no edits**, which is what `duplicate` and `split` produce:
   * the request itself is the change there, and a bar reading "0 changes staged" over a list of two
   * of them would be counting the wrong thing.
   */
  const changes = computed(
    () =>
      pending.value.length +
      staged.value.filter((entry) => isDraft(entry.id) && entry.edits.length === 0).length,
  )

  /**
   * A refusal never resolves by itself, so Apply is off until the offending change is discarded.
   *
   * Blocking the whole set rather than the one request is deliberate: the staged set is applied as a
   * unit and writing the rest of it would leave the screen half committed with no record of which
   * half — and the `×` on the refused row is one click away.
   */
  const blocked = computed(() =>
    staged.value.some((entry) => previews.value.get(entry.id)?.error !== undefined),
  )

  /**
   * Every staged request has a dry run behind it. Apply waits for this — it is the confirmation.
   *
   * A deletion is exempt because it cannot have one: `DELETE` takes no `dry_run`, and its whole
   * consequence is already on the screen from the refcounts.
   */
  const previewed = computed(() =>
    staged.value.length > 0 &&
    staged.value.every((entry) => entry.deleting || previews.value.has(entry.id)),
  )

  const canApply = computed(
    () => staged.value.length > 0 && previewed.value && !blocked.value && !applying.value,
  )

  // -- authoring ------------------------------------------------------------

  /** Stage one edit, replacing whatever was staged against the same target. */
  function stage(edit: Edit): void {
    const key = editKey(edit)
    const next = edits.value.filter((existing) => editKey(existing) !== key)
    const live = byId.value.get(edit.request)
    // An edit that returns the target to what the server already holds is the operator undoing
    // themselves, not a change to stage: keeping it would POST a spec identical to the stored one.
    if (live === undefined || isChange(live, edit)) next.push(edit)
    edits.value = next
  }

  function set(request: string, column: string, want: Want, destination: Destination): void {
    stage({ target: 'leg', request, column, want, destination })
  }

  /**
   * The row's `×`: the one removal that can stop media, because a source has no parked state.
   *
   * A draft's source is dropped from the draft rather than staged as an edit — there is no stored
   * spec for an intent to be about, so the authored list is the thing to change — and a draft that
   * loses its last source is discarded rather than kept as a request with nothing in it.
   */
  function removeSource(request: string, source: Source): void {
    if (!isDraft(request)) {
      stage({ target: 'source', request, source, want: 'out' })
      return
    }
    const key = sourceKey(source)
    const remaining = drafts.value
      .find((draft) => draft.id === request)!
      .sources.filter((entry) => sourceKey(entry) !== key)

    if (remaining.length === 0) {
      discardDraft(request)
      return
    }
    authoredDrafts.value = authoredDrafts.value.map((draft) =>
      draft.id === request ? { ...draft, sources: remaining } : draft,
    )
  }

  /**
   * The source editor's other answer: a row added to a request that already exists.
   *
   * That is how fan-in is authored, and the rectangle rule does the rest — a request is its sources
   * against its destinations, so the new row lights every one of its columns at once, staged, before
   * anything is written.
   */
  function addSource(request: string, source: Source): void {
    // A draft's sources live on the draft, exactly as a removal takes one off it: there is no stored
    // spec for an intent to be about, and keeping half of a draft in the edit list would mean the
    // row's `×` had two places to look and could remove from neither.
    if (isDraft(request)) {
      const key = sourceKey(source)
      authoredDrafts.value = authoredDrafts.value.map((draft) =>
        draft.id === request && !draft.sources.some((entry) => sourceKey(entry) === key)
          ? { ...draft, sources: [...draft.sources, source] }
          : draft,
      )
      return
    }
    stage({ target: 'source', request, source, want: 'in' })
  }

  /**
   * A request the operator has authored. Returns its id, which is `(namespace, name)` rendered.
   *
   * Refused rather than merged if the name is taken, here as well as in the editor: a POST is
   * create-or-**update** with no create-only mode and no 409, so a duplicate name is not an error the
   * server will raise — it is a silent overwrite of somebody's running request.
   */
  function createDraft(
    namespace: string,
    name: string,
    sources: Source[],
    destinations: Destination[] = [],
    settings: DraftSettings = {},
  ): string | undefined {
    const draft = newDraft(namespace, name, sources, destinations, settings)
    if (byId.value.has(draft.id)) return undefined
    authoredDrafts.value = [...authoredDrafts.value, draft]
    return draft.id
  }

  /**
   * `ui.md` §7a's other answer to clearing one cell of a rectangle: take a source out into a request
   * of its own, keeping the destinations.
   *
   * Two staged changes rather than one, and the pair is the point — the new request is created and
   * the old one gives the source up, so the operator reads both in the bar before either happens. The
   * entries are copied **whole**, so a parked leg stays parked and a per-destination `provider`
   * override comes with it, and the request-level tail comes too: a split that quietly dropped a
   * `provider` pin would move those legs onto another fabric.
   *
   * It is a control rather than a mode of the cell click because **it creates a name** — a second
   * lifecycle and a second thing to delete later — which §7a says must be an explicit choice with the
   * name visible.
   */
  function split(request: Request, source: Source, name: string): string | undefined {
    const id = createDraft(
      namespaceOf(request),
      name,
      [source],
      request.destinations.map((destination) => ({ ...destination })),
      settingsOf(request),
    )
    if (id === undefined) return undefined
    removeSource(request.id, source)
    return id
  }


  function discardDraft(id: string): void {
    authoredDrafts.value = authoredDrafts.value.filter((draft) => draft.id !== id)
    edits.value = edits.value.filter((edit) => edit.request !== id)
  }

  /** A destination named and not yet routed. Idempotent: naming an existing column is a no-op. */
  function addColumn(namespace: string, destination: Destination): void {
    const column = newDraftColumn(namespace, destination)
    if (authoredColumns.value.some((entry) => entry.namespace === namespace && entry.key === column.key)) return
    authoredColumns.value = [...authoredColumns.value, column]
  }

  function discardColumn(namespace: string, key: string): void {
    authoredColumns.value = authoredColumns.value.filter(
      (column) => !(column.namespace === namespace && column.key === key),
    )
  }

  function discard(edit: Edit): void {
    const key = editKey(edit)
    edits.value = edits.value.filter((existing) => editKey(existing) !== key)
  }

  function discardLeg(request: string, column: string): void {
    edits.value = edits.value.filter(
      (edit) => !(edit.target === 'leg' && edit.request === request && edit.column === column),
    )
  }

  /** For a draft this discards the request itself: the draft *is* the change, so half of it is not. */
  function discardRequest(request: string): void {
    if (isDraft(request)) {
      discardDraft(request)
      return
    }
    edits.value = edits.value.filter((edit) => edit.request !== request)
  }

  /**
   * Everything authored and not applied, including the drafts and the named columns.
   *
   * They are not all in the bar — an unrouted draft has nothing to preview and no commit to make —
   * but "discard all" that left authored rows and columns behind would be a button that clears some
   * of the screen. The title on it says so.
   */
  function discardAll(): void {
    edits.value = []
    authoredDrafts.value = []
    authoredColumns.value = []
    applyError.value = undefined
  }

  /** How an edit reads against the stored spec right now. Display only. */
  function verbOf(edit: Edit): Verb {
    const request = byId.value.get(edit.request)
    return request ? verb(request, edit) : 'park'
  }

  // -- the batch dry run ----------------------------------------------------

  let timer: ReturnType<typeof setTimeout> | undefined
  let token = 0

  /**
   * The specs, not the fleet.
   *
   * Watching this rather than the poll is what keeps the dry runs proportional to what the operator
   * does: a quiet poll re-serialises to the same string and fires nothing, while a poll that rebased
   * a staged request underneath changes it and re-runs — which is exactly when the previous preview
   * stopped being about the spec that would be sent.
   */
  const fingerprint = computed(() =>
    JSON.stringify(staged.value.map((entry) => entry.spec ?? `delete ${entry.id}`)),
  )

  watch(fingerprint, () => {
    if (timer !== undefined) clearTimeout(timer)
    timer = undefined

    if (staged.value.length === 0) {
      token++
      previews.value = new Map()
      previewing.value = false
      return
    }
    timer = setTimeout(() => void preview(), DEBOUNCE_MS)
  })

  async function preview(): Promise<void> {
    const targets = staged.value
    const seq = ++token
    previewing.value = true

    const results = new Map<string, Preview>()
    // Sequential: each dry run validates against the real fleet by building a candidate one and
    // reconciling it, so a batch fired in parallel multiplies the most expensive operation the
    // server has by the size of the staged set for no gain in wall-clock the operator will notice.
    for (const target of targets) {
      // A deletion has nothing to dry-run: `DELETE` takes no `dry_run`, and running the POST it is
      // *not* going to make would preview the wrong operation entirely.
      if (target.spec === undefined) continue
      try {
        const applied = await api.applyRequest(target.namespace, target.spec, { dryRun: true })
        results.set(target.id, {
          id: target.id,
          spec: target.spec,
          result: applied.request,
          ...(applied.outcome ? { outcome: applied.outcome } : {}),
        })
      } catch (error) {
        results.set(target.id, {
          id: target.id,
          spec: target.spec,
          error: error instanceof Error ? error.message : String(error),
          ...(error instanceof ApiError && error.reasonCode
            ? { reasonCode: error.reasonCode }
            : {}),
        })
      }
    }

    // A newer edit has already superseded this run. Its results describe specs nobody is sending.
    if (seq !== token) return
    previews.value = results
    previewing.value = false
  }

  // -- the apply ------------------------------------------------------------

  async function apply(): Promise<void> {
    if (applying.value) return
    applying.value = true
    applyError.value = undefined

    const done = new Set<string>()
    try {
      for (const target of staged.value) {
        // An emptied spec commits as a `DELETE`, which is the same intent the `×` expressed and not
        // a second decision. A 404 is "already gone", which is success for an idempotent cancel.
        if (target.spec === undefined) await api.deleteRequest(target.namespace, target.name)
        else await api.applyRequest(target.namespace, target.spec)
        done.add(target.id)
      }
    } catch (error) {
      applyError.value = error instanceof Error ? error.message : String(error)
    } finally {
      // Each request is its own POST and there is no batch create, so a failure part-way through has
      // already written some of them. Drop exactly what landed and leave the rest staged, rather
      // than clearing a set that is now half applied.
      edits.value = edits.value.filter((edit) => !done.has(edit.request))
      // A draft that has been created is a request now. `drafts` would stop listing it as soon as
      // the poll below lands, but the authored copy has to go too: left behind, it would come back
      // as a row the day somebody deletes the request it became.
      authoredDrafts.value = authoredDrafts.value.filter((draft) => !done.has(draft.id))
      applying.value = false
      await fleet.refresh()
    }
  }

  return {
    edits,
    pending,
    staged,
    previews,
    previewing,
    previewed,
    applying,
    applyError,
    blocked,
    canApply,
    effectiveRequests,
    radius,
    handoffs,
    changes,
    drafts,
    columns,
    isDraft,
    draftsIn,
    columnsIn,
    set,
    removeSource,
    addSource,
    createDraft,
    split,
    discardDraft,
    addColumn,
    discardColumn,
    discard,
    discardLeg,
    discardRequest,
    discardAll,
    verbOf,
    apply,
  }
})
