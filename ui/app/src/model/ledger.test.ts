import { describe, expect, it } from 'vitest'

import type { Path, PathState, Request, Selector } from '@/api/types'
import { buildLedger, needsReading, sourceIndexFor } from './ledger'

function path(id: string, dst: [string, string], src: [string, string, string], state: PathState, requests: string[]): Path {
  return {
    id,
    source: { node: src[0], domain: src[1] as never, flow: src[2] },
    destination: { node: dst[0], domain: { area: dst[1].split('/')[0]!, elements: dst[1].split('/').slice(1) } },
    state,
    requests,
  }
}

function request(
  id: string,
  sources: { select: Selector; paths: string[] }[],
  extra: Partial<Request> = {},
): Request {
  const [namespace, name] = id.split('/') as [string, string]
  return {
    id,
    namespace,
    name,
    created_at: '2026-01-01T00:00:00Z',
    sources: sources.map((source) => ({
      node: 'studio-a',
      domain: { name: { area: 'media', elements: ['cameras'] } },
      select: source.select,
    })),
    destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['ingest'] } }],
    status: {
      state: 'ESTABLISHING',
      paths: [],
      sources: sources.map((source) => ({
        source: { node: 'studio-a', domain: { name: { area: 'media', elements: ['cameras'] } }, select: source.select },
        state: 'ESTABLISHING',
        paths: source.paths,
      })),
    },
    ...extra,
  }
}

// The §7c fixture: `wall` takes a whole domain, `cam1-pin` pins one flow inside it, so one path has
// two claims. `pod-abc12` has its own path on a second destination.
const wall = request('k8s/wall', [{ select: { all: true }, paths: ['p1', 'p2'] }], { updated_at: '2026-01-02T00:00:00Z' })
const cam1Pin = request('k8s/cam1-pin', [{ select: { flow: '5592a23b-0974-45bb-9388-89ea81c42537' }, paths: ['p1'] }], { updated_at: '2026-01-03T00:00:00Z' })
const podAbc = request('k8s/pod-abc12', [{ select: { group_hint: { name: 'Studio B:Camera 1' } }, paths: ['p3'] }])
const foreign = request('nab/talkback', [{ select: { all: true }, paths: ['p4'] }])

const paths = [
  path('p1', ['edge-01', 'fast/ingest'], ['studio-a', 'media/cameras', '5592a23b'], 'ESTABLISHING', ['k8s/cam1-pin', 'k8s/wall']),
  path('p2', ['edge-01', 'fast/ingest'], ['studio-a', 'media/cameras', '9a2b1c33'], 'PAUSED', ['k8s/wall']),
  path('p3', ['edge-02', 'fast/ingest'], ['studio-b', 'media/cameras', '44e0aa17'], 'ACTIVE', ['k8s/pod-abc12']),
  path('p4', ['edge-02', 'fast/ingest'], ['studio-a', 'media/audio', '1b7c9e04'], 'PAUSED', ['nab/talkback']),
]

const requests = [wall, cam1Pin, podAbc, foreign]

describe('buildLedger', () => {
  const ledger = buildLedger(paths, requests, 'k8s')

  // One row per path, so nothing is double-counted and the counts sum — which is the whole reason
  // this view exists rather than a grid.
  it('lists each path once, whatever its refcount', () => {
    expect(ledger.pathCount).toBe(3)
    expect(ledger.groups.flatMap((group) => group.paths)).toHaveLength(3)
  })

  it('excludes a path held entirely by another namespace', () => {
    const ids = ledger.groups.flatMap((group) => group.paths.map((entry) => entry.path.id))
    expect(ids).not.toContain('p4')
  })

  it('groups by destination, ordered by node then domain', () => {
    expect(ledger.groups.map((group) => `${group.node} ${group.domain}`))
      .toEqual(['edge-01 fast/ingest', 'edge-02 fast/ingest'])
  })

  // Two requests on one path is refcounting, not a defect — it is the arrangement a shared
  // namespace exists for. It is reported as a count, never as a condition.
  it('reports every holder of a shared path', () => {
    const shared = ledger.groups.flatMap((g) => g.paths).find((entry) => entry.heldBy > 1)!
    expect(shared.path.id).toBe('p1')
    expect(shared.claims.map((claim) => claim.requestId)).toEqual(['k8s/cam1-pin', 'k8s/wall'])
  })

  it('counts the paths that are not active', () => {
    expect(ledger.notActiveCount).toBe(2)
  })

  // The whole answer to "why do I have two of these", on adjacent lines.
  it('carries the selector that made each claim', () => {
    const shared = ledger.groups.flatMap((g) => g.paths).find((entry) => entry.heldBy > 1)!
    expect(shared.claims.map((claim) => claim.selector))
      .toEqual(['flow 5592a23b…', 'all flows'])
    expect(shared.claims.every((claim) => claim.sourceIndex === 0)).toBe(true)
  })

  describe('sole and shared', () => {
    const tally = (id: string) => ledger.requests.find((entry) => entry.request.id === id)!

    it('counts the paths that stop if a request is deleted', () => {
      expect(tally('k8s/wall')).toMatchObject({ paths: 2, sole: 1, shared: 1, ridesAlong: false })
    })

    // A request with no sole paths is carrying nothing: nothing broken, nothing doubled, and no
    // other symptom anywhere in the product.
    it('flags a request that rides along', () => {
      expect(tally('k8s/cam1-pin')).toMatchObject({ paths: 1, sole: 0, shared: 1, ridesAlong: true })
    })

    it('does not flag a request holding its paths solely', () => {
      expect(tally('k8s/pod-abc12')).toMatchObject({ paths: 1, sole: 1, ridesAlong: false })
    })
  })

  // A parked request produces no path, so it has no claim row — but it must still be listed, which
  // is what the request pane is for.
  it('lists a request with no paths at all', () => {
    const parked = request('k8s/pod-def34', [{ select: { group_hint: { name: 'Studio A:Camera 2' } }, paths: [] }])
    const withParked = buildLedger(paths, [...requests, parked], 'k8s')
    expect(withParked.requests.find((entry) => entry.request.id === 'k8s/pod-def34'))
      .toMatchObject({ paths: 0, sole: 0, ridesAlong: false })
  })
})

describe('cross-namespace facts', () => {
  // Namespaces partition requests, not destinations. Two of them holding one *path* is refcounting,
  // and it decides whether deleting a claim stops anything.
  it('names a foreign holder of a shared path and excludes it from sole', () => {
    const shared = [
      path('p1', ['edge-01', 'fast/ingest'], ['studio-a', 'media/cameras', '5592a23b'], 'ACTIVE', ['k8s/wall', 'nab/talkback']),
    ]
    const ledger = buildLedger(shared, requests, 'k8s')
    const entry = ledger.groups[0]!.paths[0]!

    expect(entry.heldBy).toBe(2)
    expect(entry.foreign).toEqual(['nab/talkback'])
    expect(ledger.requests.find((r) => r.request.id === 'k8s/wall')!.sole).toBe(0)
  })

  // The wider question, and the one the group header answers: another namespace landing *anything*
  // in this domain. It shares no path here — a different flow entirely — so per-path refcounts say
  // nothing about it, and emptying this group would still not empty the domain.
  it('names another namespace writing a different flow into the same domain', () => {
    const ledger = buildLedger(paths, requests, 'k8s')
    const edge02 = ledger.groups.find((group) => group.node === 'edge-02')!

    expect(edge02.paths.every((entry) => entry.foreign.length === 0)).toBe(true)
    expect(edge02.foreignNamespaces).toEqual(['nab'])
  })

  it('says nothing about a domain only this namespace writes into', () => {
    const ledger = buildLedger(paths, requests, 'k8s')
    expect(ledger.groups.find((group) => group.node === 'edge-01')!.foreignNamespaces).toEqual([])
  })
})

describe('sourceIndexFor', () => {
  it('attributes a path through the per-source path IDs', () => {
    const multi = request('k8s/multi', [
      { select: { all: true }, paths: ['p1'] },
      { select: { flow: 'abc' }, paths: ['p2'] },
    ])
    expect(sourceIndexFor(multi, 'p1')).toBe(0)
    expect(sourceIndexFor(multi, 'p2')).toBe(1)
  })

  it('falls back to the only source when the join misses', () => {
    const single = request('k8s/single', [{ select: { all: true }, paths: [] }])
    expect(sourceIndexFor(single, 'unknown')).toBe(0)
  })

  it('refuses to guess when there are several sources', () => {
    const multi = request('k8s/multi', [
      { select: { all: true }, paths: [] },
      { select: { flow: 'abc' }, paths: [] },
    ])
    expect(sourceIndexFor(multi, 'unknown')).toBe(-1)
  })
})

describe('needsReading', () => {
  const allRequests = requests
  const of = (state: PathState, requests: string[]) =>
    buildLedger(
      [path('p1', ['edge-01', 'fast/ingest'], ['studio-a', 'media/cameras', 'f'], state, requests)],
      allRequests,
      'k8s',
    )
      .groups[0]!.paths[0]!

  it('keeps a path that is not active', () => {
    expect(needsReading(of('PAUSED', ['k8s/wall']))).toBe(true)
    expect(needsReading(of('FAILED', ['k8s/wall']))).toBe(true)
  })

  // The correction that matters: a path several requests hold is ordinary in a shared namespace, so
  // it is hidden like any other active path rather than promoted for review.
  it('hides an active path however many requests hold it', () => {
    expect(needsReading(of('ACTIVE', ['k8s/wall']))).toBe(false)
    expect(needsReading(of('ACTIVE', ['k8s/wall', 'k8s/cam1-pin']))).toBe(false)
  })
})
