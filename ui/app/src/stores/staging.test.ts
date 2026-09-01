import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { Destination } from '@/api/types'
import { endpointKey } from '@/model/ownership'
import { useFleetStore } from './fleet'
import { useStagingStore } from './staging'

const INGEST: Destination = { node: 'edge-01', domain: { area: 'fast', elements: ['ingest'] } }
const WALL: Destination = { node: 'edge-02', domain: { area: 'fast', elements: ['wall'] } }

function stored(destinations: Destination[]) {
  return {
    id: 'nab/wall',
    namespace: 'nab',
    name: 'wall',
    created_at: '2026-01-01T00:00:00Z',
    sources: [{ node: 'studio-a', domain: { name: { area: 'media', elements: ['cameras'] } }, select: { all: true } }],
    destinations,
    status: { state: 'ACTIVE', paths: [{ id: 'p1', state: 'ACTIVE' }] },
  }
}

let requests: unknown[]
let posted: { url: string; body: unknown }[]
let deleted: string[]
let refuse: string | undefined
/** What a dry run answers, by request name. The default is the fixture echoed back. */
let results: Record<string, unknown>
let livePaths: unknown[]

function installFetch() {
  posted = []
  deleted = []
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const bare = url.split('?')[0]!

    if (init?.method === 'DELETE') {
      deleted.push(bare)
      return new Response(null, { status: 204 })
    }

    if (init?.method === 'POST') {
      const body = JSON.parse(String(init.body)) as { name: string }
      posted.push({ url, body })
      if (refuse !== undefined) {
        return new Response(
          JSON.stringify({ code: 'invalid_request', message: refuse, details: { reason_code: 'unknown_area' } }),
          { status: 400 },
        )
      }
      return new Response(JSON.stringify(results[body.name] ?? stored([INGEST])), {
        status: 200,
        headers: { 'X-Mxl-Outcome': 'updated' },
      })
    }

    const bodies: Record<string, unknown> = {
      '/v1/paths': { paths: livePaths },
      '/v1/nodes': { nodes: [] },
      '/v1/requests': { requests },
      '/v1/namespaces': { namespaces: [{ name: 'nab', paths: 'exclusive', requests: 1 }] },
      '/readyz': { status: 'ok', leader: 'r1' },
    }
    return new Response(JSON.stringify(bodies[bare] ?? {}), { status: 200 })
  }) as typeof fetch
}

/** The debounce is real time, and the dry run is what the bar is waiting for. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 600))

describe('the staged set', () => {
  beforeEach(async () => {
    setActivePinia(createPinia())
    requests = [stored([INGEST])]
    results = {}
    livePaths = [{ id: 'p1', state: 'ACTIVE', requests: ['nab/wall'] }]
    refuse = undefined
    installFetch()
    await useFleetStore().refresh()
  })

  afterEach(() => vi.restoreAllMocks())

  // Every click stages and nothing writes. That is the whole commit model: the preview is what
  // removes the confirmation dialog, and it cannot exist if the write already happened.
  it('writes nothing until apply', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)

    expect(staging.pending).toHaveLength(1)
    expect(posted.filter((call) => !call.url.includes('dry_run'))).toHaveLength(0)

    await settle()
    // The dry run runs the identical path and skips only the write, so it is the one POST allowed
    // before Apply — and the batch is what makes the preview report real outcomes.
    expect(posted).toHaveLength(1)
    expect(posted[0]!.url).toContain('dry_run=true')
    expect(staging.previews.get('nab/wall')?.outcome).toBe('updated')
  })

  // Parking is a spec edit, so it is the same POST as everything else: take the stored spec, flip one
  // boolean. And un-parking deletes the key rather than writing `disabled: false`.
  it('sends the stored spec with one entry flipped', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)
    await settle()

    expect(posted[0]!.body).toMatchObject({
      name: 'wall',
      destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['ingest'] }, disabled: true }],
    })
    // Nothing derived rides along.
    expect(posted[0]!.body).not.toHaveProperty('status')
    expect(posted[0]!.body).not.toHaveProperty('id')
  })

  // Clicking back is undoing yourself, not a change to stage: `unchanged` means do not write, and a
  // screen that re-POSTs on every interaction turns a resync into store churn.
  it('drops an edit that returns the leg to what the server already holds', () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)
    staging.set('nab/wall', endpointKey(INGEST), 'on', INGEST)
    expect(staging.pending).toHaveLength(0)
    expect(staging.staged).toHaveLength(0)
  })

  // An edit is an intent about one leg, not a draft spec, so the poll rebases it for free — and an
  // edit somebody else's apply has already satisfied stops being pending rather than being applied
  // a second time.
  it('rebases onto the poll and drops what the fleet already did', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)
    expect(staging.pending).toHaveLength(1)

    requests = [stored([{ ...INGEST, disabled: true }])]
    await useFleetStore().refresh()

    expect(staging.pending).toHaveLength(0)
  })

  // A request somebody deleted has nothing to apply to.
  it('drops an edit whose request has gone', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(WALL), 'on', WALL)
    expect(staging.pending).toHaveLength(1)

    requests = []
    await useFleetStore().refresh()
    expect(staging.pending).toHaveLength(0)
  })

  // The grid renders the staged world, which is what makes "a rectangle has no notches" render
  // itself rather than be explained in a dialog.
  it('offers the requests as the staged set would have them', () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(WALL), 'on', WALL)

    const effective = staging.effectiveRequests[0]!
    expect(effective.destinations).toHaveLength(2)
    // …and leaves the poll's own object alone.
    expect(useFleetStore().requests[0]!.destinations).toHaveLength(1)
  })

  // A refusal never resolves by itself, so Apply is off until the offending change is discarded —
  // and the server's own prose is what the bar renders.
  it('blocks apply on a refusal and keeps the message', async () => {
    refuse = 'node "edge-02" advertises no area "fast"'
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(WALL), 'on', WALL)
    await settle()

    expect(staging.previews.get('nab/wall')?.error).toContain('advertises no area')
    expect(staging.previews.get('nab/wall')?.reasonCode).toBe('unknown_area')
    expect(staging.blocked).toBe(true)
    expect(staging.canApply).toBe(false)

    staging.discardAll()
    expect(staging.staged).toHaveLength(0)
  })

  // A request must name at least one source and one destination, so a spec the × controls have
  // emptied has nothing to POST and committing it is a DELETE — and there is nothing to dry-run,
  // because DELETE takes no dry_run and its whole consequence is already on screen from the
  // refcounts.
  it('commits an emptied spec as a delete, with no dry run', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'gone', INGEST)

    expect(staging.staged[0]).toMatchObject({ deleting: true, spec: undefined, name: 'wall' })
    expect(staging.radius.get('nab/wall')?.stopping).toHaveLength(1)
    // Nothing waits on a preview that cannot exist.
    expect(staging.canApply).toBe(true)

    await settle()
    expect(posted).toHaveLength(0)

    await staging.apply()
    expect(deleted).toEqual(['/v1/namespaces/nab/requests/wall'])
    expect(staging.pending).toHaveLength(0)
  })

  // The ordering hazard `ui.md` §7a warns about does not arise, because the staged set is a set:
  // removing the last leg and adding another reach the same spec whichever order they were clicked.
  it('stops being a delete once something else is staged in', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'gone', INGEST)
    staging.set('nab/wall', endpointKey(WALL), 'on', WALL)

    expect(staging.staged[0]!.deleting).toBe(false)
    expect(staging.staged[0]!.spec!.destinations).toEqual([WALL])
  })

  // A draft is the one thing here that is a spec, because a request the server does not hold has no
  // stored copy for an intent to be about. It is otherwise an ordinary member of the staged set.
  it('carries a draft as a request and applies it as a create', async () => {
    const staging = useStagingStore()
    const source = { node: 'studio-b', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    const id = staging.createDraft('nab', 'new-one', [source])!

    expect(id).toBe('nab/new-one')
    // On the axes and nowhere near the bar: a request must name a destination, so a draft with none
    // is a row waiting to be routed rather than a change that cannot be applied.
    expect(staging.effectiveRequests.map((request) => request.id)).toContain(id)
    expect(staging.staged).toHaveLength(0)

    staging.set(id, endpointKey(WALL), 'on', WALL)
    expect(staging.staged[0]).toMatchObject({ id, deleting: false })
    expect(staging.staged[0]!.spec).toMatchObject({ name: 'new-one', sources: [source], destinations: [WALL] })

    await settle()
    await staging.apply()

    const writes = posted.filter((call) => !call.url.includes('dry_run'))
    expect(writes).toHaveLength(1)
    // Never a DELETE: there is nothing on the server to cancel, and the name may be somebody else's.
    expect(deleted).toEqual([])
    expect(staging.drafts).toHaveLength(0)
  })

  // A draft's sources live on the draft, so the row's `×` has one place to look. Split between the
  // draft and the edit list, a source added to a draft could be removed from neither.
  it('adds and removes a draft\'s sources on the draft itself', () => {
    const staging = useStagingStore()
    const one = { node: 'studio-a', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    const two = { node: 'studio-b', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    const id = staging.createDraft('nab', 'pair', [one])!

    staging.addSource(id, two)
    expect(staging.drafts[0]!.sources).toHaveLength(2)
    expect(staging.pending).toHaveLength(0)

    staging.removeSource(id, two)
    expect(staging.drafts[0]!.sources).toEqual([one])

    // Its last source goes and the draft goes with it: a request must name one, and there is nothing
    // on the server to leave behind.
    staging.removeSource(id, one)
    expect(staging.drafts).toHaveLength(0)
  })

  // The same rebase discipline the edits run on: whoever created it, the server's copy is now the
  // request and the draft is a stale second spelling of it.
  it('stops being a draft once the server holds the name', async () => {
    const staging = useStagingStore()
    staging.createDraft('nab', 'wall', [])
    expect(staging.drafts).toHaveLength(0)
    expect(staging.isDraft('nab/wall')).toBe(false)
  })

  // A column nothing routes yet is authored scaffolding: it is on the axis so there is a cell to
  // click, and it leaves the list the moment a request names it.
  it('holds a named column until a request names it', () => {
    const staging = useStagingStore()
    staging.addColumn('nab', WALL)
    expect(staging.columnsIn('nab')).toEqual([WALL])
    // Idempotent: naming one that is already there is arriving at it, not a second column.
    staging.addColumn('nab', WALL)
    expect(staging.columnsIn('nab')).toHaveLength(1)

    staging.set('nab/wall', endpointKey(WALL), 'on', WALL)
    expect(staging.columnsIn('nab')).toEqual([])
  })

  // `ui.md` §7a consequence 2's second answer: take a source out into a request of its own, keeping
  // the destinations. Two staged changes, and the entries are copied **whole** — a parked leg stays
  // parked, and a copy that turned one back on would start media the original has switched off.
  it('splits a source out as a request of its own', async () => {
    const parked = { ...WALL, disabled: true }
    const a = { node: 'studio-a', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    const b = { node: 'studio-b', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    requests = [{ ...stored([INGEST, parked]), sources: [a, b], provider: 'verbs', labels: { show: 'nab' } }]
    await useFleetStore().refresh()

    const staging = useStagingStore()
    const request = useFleetStore().requests[0]!
    staging.split(request, b, 'studio-b')

    // The new request first, and that order is what keeps the media up: a create can never win a
    // contest, so the incumbent keeps the path until the update that gives it up lands.
    expect(staging.staged.map((entry) => entry.id)).toEqual(['nab/studio-b', 'nab/wall'])

    const created = staging.staged[0]!.spec!
    expect(created.sources).toEqual([b])
    expect(created.destinations).toEqual([INGEST, parked])
    // The rectangle's own settings come with it: a dropped `provider` pin would move those legs onto
    // another fabric, and dropped labels change what `apply --prune` deletes.
    expect(created.provider).toBe('verbs')
    expect(created.labels).toEqual({ show: 'nab' })

    // …and the original gives the source up, keeping the other.
    expect(staging.staged[1]!.spec!.sources).toEqual([a])
    expect(staging.staged[1]!.deleting).toBe(false)
  })

  // A dry run reconciles a candidate fleet with *one* request changed, so the new half of a split
  // previews as an overlap with the half that is giving the paths up. Reported as the hand-off it is
  // rather than as a refusal it is not — and only when the evidence says so: every holder of every
  // contested path is in this set, and none of them would still be holding it.
  it('recognises an overlap this set is itself about to resolve', async () => {
    const source = { node: 'studio-a', domain: { labels: { role: 'cameras' } }, select: { all: true } as const }
    results['wall'] = { ...stored([INGEST]), status: { state: 'ACTIVE', paths: [] } }
    results['taker'] = {
      ...stored([INGEST]),
      id: 'nab/taker',
      name: 'taker',
      status: {
        state: 'INVALID',
        reason_code: 'namespace_overlap',
        // Trap 14: the loser lists the contested path with the incumbent's state.
        paths: [{ id: 'p1', state: 'ACTIVE' }],
      },
    }

    const staging = useStagingStore()
    staging.createDraft('nab', 'taker', [source], [INGEST])
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)
    await settle()

    expect(staging.handoffs.get('nab/taker')).toEqual({ paths: ['p1'], from: ['nab/wall'] })
    // And the request giving it up says so rather than reporting a stop: "1 of 1 paths stop" is true
    // of that write alone and false of the set it is in.
    expect(staging.radius.get('nab/wall')).toMatchObject({ stops: 0 })
    expect(staging.radius.get('nab/wall')!.handedOff.map((path) => path.id)).toEqual(['p1'])
    // And it stays a refusal when the incumbent is not in the set at all.
    staging.discardLeg('nab/wall', endpointKey(INGEST))
    await settle()
    expect(staging.handoffs.get('nab/taker')).toBeUndefined()
    expect(staging.radius.get('nab/taker')?.holdsNothing).toBe(true)
  })

  // Apply is one POST per request and it clears exactly what landed.
  it('applies for real and clears the set', async () => {
    const staging = useStagingStore()
    staging.set('nab/wall', endpointKey(INGEST), 'off', INGEST)
    await settle()
    expect(staging.canApply).toBe(true)

    await staging.apply()

    const writes = posted.filter((call) => !call.url.includes('dry_run'))
    expect(writes).toHaveLength(1)
    expect(writes[0]!.url).toBe('/v1/namespaces/nab/requests')
    expect(staging.pending).toHaveLength(0)
  })
})
