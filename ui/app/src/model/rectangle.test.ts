import { describe, expect, it } from 'vitest'

import type { Destination, Path, Request } from '@/api/types'
import { buildRectangle } from './rectangle'

const dst = (node: string, elements: string[], disabled?: boolean): Destination => ({
  node,
  domain: { area: 'fast', elements },
  ...(disabled ? { disabled: true } : {}),
})

function path(id: string, destination: Destination, state: Path['state'], requests: string[]): Path {
  return {
    id,
    source: { node: 'studio-a', domain: 'media/cameras' as never, flow: 'f' },
    destination,
    state,
    requests,
  }
}

function request(sources: string[][], destinations: Destination[]): Request {
  return {
    id: 'k8s/r',
    namespace: 'k8s',
    name: 'r',
    created_at: '2026-01-01T00:00:00Z',
    sources: sources.map(() => ({
      node: 'studio-a',
      domain: { name: { area: 'media', elements: ['cameras'] } },
      select: { all: true },
    })),
    destinations,
    status: {
      state: 'ESTABLISHING',
      paths: [],
      sources: sources.map((paths) => ({
        source: { node: 'studio-a', domain: { name: { area: 'media', elements: ['cameras'] } }, select: { all: true } },
        state: 'ESTABLISHING',
        paths,
      })),
    },
  }
}

describe('buildRectangle', () => {
  it('is sources × destinations', () => {
    const rect = buildRectangle(request([[], []], [dst('edge-01', ['ingest']), dst('edge-02', ['ingest'])]), new Map())
    expect(rect.rows).toHaveLength(2)
    expect(rect.columns).toHaveLength(2)
    expect(rect.cells).toHaveLength(2)
    expect(rect.cells[0]).toHaveLength(2)
  })

  it('folds a cell over the paths that pairing produced', () => {
    const a = dst('edge-01', ['ingest'])
    const b = dst('edge-02', ['ingest'])
    const paths = new Map([
      ['p1', path('p1', a, 'ACTIVE', ['k8s/r'])],
      ['p2', path('p2', a, 'PAUSED', ['k8s/r'])],
      ['p3', path('p3', b, 'ACTIVE', ['k8s/r'])],
    ])

    const rect = buildRectangle(request([['p1', 'p2', 'p3']], [a, b]), paths)
    expect(rect.cells[0]![0]).toMatchObject({ state: 'PARTIAL', count: 2, parked: false })
    expect(rect.cells[0]![1]).toMatchObject({ state: 'ACTIVE', count: 1, parked: false })
  })

  // A parked leg is authored intent. It keeps a lit cell's geometry — two fixed-shape lines — and
  // must not look like a cell nobody ever routed.
  it('draws a parked destination rather than blanking it', () => {
    const parked = dst('archive-01', ['capture'], true)
    const rect = buildRectangle(request([[]], [parked]), new Map())
    expect(rect.columns[0]!.parked).toBe(true)
    expect(rect.cells[0]![0]).toMatchObject({ state: 'DISABLED', parked: true, count: 0 })
  })

  // An axis that has not materialised yet is the ordinary state of a pre-provisioned route, not an
  // error — so the cell says nothing rather than saying something wrong.
  it('leaves a pairing with no paths without a state', () => {
    const rect = buildRectangle(request([[]], [dst('edge-01', ['ingest'])]), new Map())
    expect(rect.cells[0]![0]).toMatchObject({ state: undefined, count: 0, parked: false })
  })

  // Trap 14: a request can list a path it does not hold. `path.requests[]` is the only ownership
  // statement, and a rectangle drawn without the cross-check shows another request's media as this
  // one's — in the one situation where the operator most needs to know they are not carrying it.
  it('ignores a path the request lists but does not own', () => {
    const a = dst('edge-01', ['ingest'])
    const paths = new Map([['p1', path('p1', a, 'ACTIVE', ['k8s/incumbent'])]])

    const rect = buildRectangle(request([['p1']], [a]), paths)
    expect(rect.cells[0]![0]).toMatchObject({ state: undefined, count: 0 })
  })

  it('attributes each source entry to its own row', () => {
    const a = dst('edge-01', ['ingest'])
    const paths = new Map([
      ['p1', path('p1', a, 'ACTIVE', ['k8s/r'])],
      ['p2', path('p2', a, 'FAILED', ['k8s/r'])],
    ])

    const rect = buildRectangle(request([['p1'], ['p2']], [a]), paths)
    expect(rect.cells[0]![0]!.state).toBe('ACTIVE')
    expect(rect.cells[1]![0]!.state).toBe('FAILED')
  })
})
