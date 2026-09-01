import { describe, expect, it } from 'vitest'

import type { Destination, DomainName, Node, Path, PathState, Request, Selector, Source } from '@/api/types'
import { buildMatrix, sourceKey } from './matrix'

const dst = (node: string, elements: string[], disabled?: boolean): Destination => ({
  node,
  domain: { area: 'fast', elements },
  ...(disabled ? { disabled: true } : {}),
})

const src = (node: string, elements: string[], select: Selector = { all: true }): Source => ({
  node,
  domain: { name: { area: 'media', elements } },
  select,
})

function path(
  id: string,
  from: Source,
  flow: string,
  destination: Destination,
  state: PathState,
  requests: string[],
): Path {
  return {
    id,
    source: {
      node: from.node,
      domain: `media/${(from.domain.name?.elements ?? []).join('/')}` as DomainName,
      flow,
    },
    destination,
    state,
    requests,
  }
}

function request(
  name: string,
  sources: Source[],
  destinations: Destination[],
  pathIds: string[][],
  state: Request['status']['state'] = 'ESTABLISHING',
  sourceStates: Request['status']['state'][] = [],
): Request {
  return {
    id: `nab/${name}`,
    namespace: 'nab',
    name,
    created_at: '2026-01-01T00:00:00Z',
    sources,
    destinations,
    status: {
      state,
      paths: [],
      sources: sources.map((source, index) => ({
        source,
        state: sourceStates[index] ?? state,
        paths: pathIds[index] ?? [],
      })),
    },
  }
}

const node = (name: string, live = true): Node => ({
  name,
  live,
  capabilities: { fabrics: [], versions: { protocol: 1, replicator: 'test' }, sched_prio: false },
})

const NODES = [node('studio-a'), node('studio-b'), node('edge-01'), node('edge-02')]

describe('sourceKey', () => {
  // A row is a query, not a request's property: two requests naming the same three things are one
  // row, which is what lets a fan-in rectangle and a single-source request share one honestly.
  it('is the node, the domain selector and the flow selector', () => {
    expect(sourceKey(src('studio-a', ['cameras']))).toBe(sourceKey(src('studio-a', ['cameras'])))
    expect(sourceKey(src('studio-a', ['cameras']))).not.toBe(sourceKey(src('studio-b', ['cameras'])))
    expect(sourceKey(src('studio-a', ['cameras'], { all: true }))).not.toBe(
      sourceKey(src('studio-a', ['cameras'], { group_hint: { name: 'Camera 1' } })),
    )
  })

  // Equality, ANDed, order-independent — the wire's map has no order, so neither can its identity.
  it('does not depend on the order label pairs arrived in', () => {
    const one: Source = { node: 'studio-a', domain: { labels: { role: 'cameras', studio: 'a' } }, select: { all: true } }
    const two: Source = { node: 'studio-a', domain: { labels: { studio: 'a', role: 'cameras' } }, select: { all: true } }
    expect(sourceKey(one)).toBe(sourceKey(two))
  })
})

describe('buildMatrix', () => {
  it('reads both axes out of the requests and sorts them by node', () => {
    const requests = [
      request('wall', [src('studio-b', ['cameras']), src('studio-a', ['cameras'])],
        [dst('edge-02', ['ingest']), dst('edge-01', ['ingest'])], [[], []]),
    ]

    const matrix = buildMatrix([], requests, NODES, 'nab')
    expect(matrix.rows.map((row) => row.node)).toEqual(['studio-a', 'studio-b'])
    expect(matrix.columns.map((column) => `${column.node} ${column.domain}`))
      .toEqual(['edge-01 fast/ingest', 'edge-02 fast/ingest'])
  })

  // A request is sources × destinations, so its cells cannot be toggled independently — the
  // rectangle has no notches, and it is carried as index sets because rows sort by node and its rows
  // need not be adjacent.
  it('lights a rectangle over every source and every destination', () => {
    const requests = [
      request('wall', [src('studio-a', ['cameras']), src('studio-b', ['cameras'])],
        [dst('edge-01', ['ingest']), dst('edge-02', ['ingest'])], [[], []]),
    ]

    const matrix = buildMatrix([], requests, NODES, 'nab')
    expect(matrix.cells.flat().filter((cell) => cell.lit)).toHaveLength(4)
    expect(matrix.requests[0]).toMatchObject({ rows: [0, 1], columns: [0, 1] })
    // Only a rectangle with more than one source gets an accent: a 1×N is a row already.
    expect(matrix.requests[0]!.accent).toBe(0)
  })

  it('gives a single-source request no accent', () => {
    const matrix = buildMatrix([], [request('one', [src('studio-a', ['cameras'])], [dst('edge-01', ['ingest'])], [[]])], NODES, 'nab')
    expect(matrix.requests[0]!.accent).toBeUndefined()
  })

  // Two requests naming the same source are one row. It is the row a click cannot extend once the
  // grid is editable, and it must be *rendered* either way.
  it('shares a row between two requests that wrote the same source', () => {
    const source = src('studio-a', ['cameras'])
    const requests = [
      request('a', [source], [dst('edge-01', ['ingest'])], [[]]),
      request('b', [source], [dst('edge-02', ['ingest'])], [[]]),
    ]

    const matrix = buildMatrix([], requests, NODES, 'nab')
    expect(matrix.rows).toHaveLength(1)
    expect(matrix.rows[0]!.requests).toEqual(['nab/a', 'nab/b'])
    expect(matrix.columns).toHaveLength(2)
  })

  it('folds a cell over its own paths and counts the flows its row matched', () => {
    const source = src('studio-a', ['cameras'])
    const one = dst('edge-01', ['ingest'])
    const two = dst('edge-02', ['ingest'])
    const paths = [
      path('p1', source, 'f1', one, 'ACTIVE', ['nab/wall']),
      path('p2', source, 'f2', one, 'PAUSED', ['nab/wall']),
      path('p3', source, 'f1', two, 'ACTIVE', ['nab/wall']),
      path('p4', source, 'f2', two, 'ACTIVE', ['nab/wall']),
    ]

    const matrix = buildMatrix(paths, [request('wall', [source], [one, two], [['p1', 'p2', 'p3', 'p4']])], NODES, 'nab')

    expect(matrix.cells[0]![0]).toMatchObject({ state: 'PARTIAL', count: 2 })
    expect(matrix.cells[0]![1]).toMatchObject({ state: 'ACTIVE', count: 2 })
    // The row's flow count and the cell's path count are the same fact from two sides.
    expect(matrix.rows[0]!.flows).toBe(2)
    expect(matrix.pathCount).toBe(4)
    expect(matrix.notActiveCount).toBe(1)
    expect(matrix.unplaced).toBe(0)
  })

  // Trap 14: the loser of a namespace overlap lists the contested path *with the incumbent's state*,
  // so a request carrying nothing can show `{"ACTIVE": 1}`. A cell drawn without the cross-check
  // reports another request's media as this one's — in the one situation where the operator most
  // needs to know they are not carrying it.
  it('ignores a path a request lists but does not hold', () => {
    const source = src('studio-a', ['cameras'])
    const one = dst('edge-01', ['ingest'])
    const paths = [path('p1', source, 'f1', one, 'ACTIVE', ['nab/incumbent'])]

    const matrix = buildMatrix(paths, [request('loser', [source], [one], [['p1']], 'INVALID')], NODES, 'nab')
    expect(matrix.cells[0]![0]!.count).toBe(0)
    expect(matrix.cells[0]![0]!.state).toBe('INVALID')
    expect(matrix.pathCount).toBe(0)
  })

  // Without this the axes would be derived from routes that are currently on, so switching one off
  // would delete the row and the column it lived on — the board rearranging itself under the pointer.
  it('keeps the column of a parked destination and draws the cell dark', () => {
    const matrix = buildMatrix(
      [],
      [request('parked', [src('studio-a', ['cameras'])], [dst('edge-01', ['ingest'], true)], [[]], 'DISABLED')],
      NODES,
      'nab',
    )

    expect(matrix.columns).toHaveLength(1)
    expect(matrix.columns[0]).toMatchObject({ parked: true, parkedRequests: 1, sources: 0 })
    expect(matrix.cells[0]![0]).toMatchObject({ lit: true, parked: true, state: 'DISABLED', count: 0 })
  })

  // The flag lives on a request's own entry, so a domain two requests write into is off here only
  // when both of them parked it — one live claim keeps the leg running, and a column drawn dark over
  // it would be a lie about media that is flowing.
  it('does not park a column one request still writes into', () => {
    const source = src('studio-a', ['cameras'])
    const other = src('studio-b', ['cameras'])
    const destination = dst('edge-01', ['ingest'])
    const paths = [path('p1', other, 'f1', destination, 'ACTIVE', ['nab/live'])]

    const matrix = buildMatrix(
      paths,
      [
        request('live', [other], [destination], [['p1']]),
        request('off', [source], [dst('edge-01', ['ingest'], true)], [[]], 'DISABLED'),
      ],
      NODES,
      'nab',
    )

    expect(matrix.columns[0]).toMatchObject({ parked: false, parkedRequests: 1 })
    expect(matrix.cells[0]![0]).toMatchObject({ parked: true })
    expect(matrix.cells[1]![0]).toMatchObject({ parked: false, state: 'ACTIVE', count: 1 })
  })

  // "Accepted, not yet satisfiable" is a quiet cell state and not an error: pre-provisioning
  // replication for a camera that is not live yet costs nothing and is explicitly supported.
  it('borrows the source state for a pairing whose source has matched nothing', () => {
    const matrix = buildMatrix(
      [],
      [request('early', [src('studio-a', ['cameras'], { group_hint: { name: 'Camera 9' } })],
        [dst('edge-01', ['ingest'])], [[]], 'WAITING')],
      NODES,
      'nab',
    )

    expect(matrix.cells[0]![0]).toMatchObject({ lit: true, state: 'WAITING', count: 0 })
  })

  // A source with legs elsewhere says nothing about this pairing. A leg refused during validation
  // produces no path and appears in no list, so borrowing another leg's word for it would be a
  // guess dressed as a reading.
  it('says nothing where a source has legs on another destination', () => {
    const source = src('studio-a', ['cameras'])
    const one = dst('edge-01', ['ingest'])
    const two = dst('edge-02', ['ingest'])
    const paths = [path('p1', source, 'f1', one, 'ACTIVE', ['nab/wall'])]

    const matrix = buildMatrix(paths, [request('wall', [source], [one, two], [['p1']], 'PARTIAL')], NODES, 'nab')
    expect(matrix.cells[0]![1]).toMatchObject({ lit: true, state: undefined, count: 0 })
  })

  // The bounded hole: exclusivity is enforced on materialised paths, so two requests with an
  // identical source and destination are both accepted while the selector matches nothing.
  it('reports a pairing two requests name', () => {
    const source = src('studio-a', ['cameras'], { group_hint: { name: 'Camera 9' } })
    const destination = dst('edge-01', ['ingest'])
    const matrix = buildMatrix(
      [],
      [
        request('a', [source], [destination], [[]], 'WAITING'),
        request('b', [source], [destination], [[]], 'WAITING'),
      ],
      NODES,
      'nab',
    )

    expect(matrix.cells[0]![0]!.requests).toEqual(['nab/a', 'nab/b'])
  })

  // Each grouping carries a real resource fact: egress down the rows, ingress across the columns.
  it('bands both axes by node, with each direction of the resource fact', () => {
    const a = src('studio-a', ['cameras'])
    const b = src('studio-b', ['cameras'])
    const edge = dst('edge-01', ['ingest'])
    const paths = [
      path('p1', a, 'f1', edge, 'ACTIVE', ['nab/wall']),
      path('p2', b, 'f2', edge, 'ACTIVE', ['nab/wall']),
    ]

    const matrix = buildMatrix(paths, [request('wall', [a, b], [edge], [['p1'], ['p2']])], NODES, 'nab')

    expect(matrix.rowBands.map((band) => band.node)).toEqual(['studio-a', 'studio-b'])
    expect(matrix.rowBands.every((band) => band.paths === 1)).toBe(true)
    expect(matrix.columnBands).toHaveLength(1)
    expect(matrix.columnBands[0]).toMatchObject({ node: 'edge-01', paths: 2, live: true })
  })

  // Registered but not leased is information, not an alarm — and a node nothing has registered at
  // all is a third fact, so the band distinguishes them rather than folding both into "down".
  it('carries the lease of each band node, and says when there is no record at all', () => {
    const matrix = buildMatrix(
      [],
      [request('wall', [src('studio-c', ['cameras'])], [dst('edge-02', ['ingest'])], [[]])],
      [node('edge-02', false)],
      'nab',
    )

    expect(matrix.rowBands[0]!.live).toBeUndefined()
    expect(matrix.columnBands[0]!.live).toBe(false)
  })

  // Namespaces partition requests, not destinations. Another namespace writing into this domain is
  // ordinary fan-in, and it is what makes "emptying this column empties the domain" false.
  it('names other namespaces writing into a column', () => {
    const source = src('studio-a', ['cameras'])
    const destination = dst('edge-01', ['ingest'])
    const paths = [
      path('p1', source, 'f1', destination, 'ACTIVE', ['nab/wall']),
      path('p2', src('studio-b', ['cameras']), 'f2', destination, 'ACTIVE', ['archive/dump', 'k8s/pod']),
    ]

    const matrix = buildMatrix(paths, [request('wall', [source], [destination], [['p1']])], NODES, 'nab')
    expect(matrix.columns[0]!.foreignNamespaces).toEqual(['archive', 'k8s'])
    // The foreign path is not this namespace's business and appears in no cell.
    expect(matrix.pathCount).toBe(1)
  })

  it('renders only the namespace it was asked for', () => {
    const requests = [
      request('here', [src('studio-a', ['cameras'])], [dst('edge-01', ['ingest'])], [[]]),
      { ...request('there', [src('studio-b', ['cameras'])], [dst('edge-02', ['ingest'])], [[]]),
        id: 'k8s/there', namespace: 'k8s' },
    ]

    const matrix = buildMatrix([], requests, NODES, 'nab')
    expect(matrix.rows).toHaveLength(1)
    expect(matrix.columns).toHaveLength(1)
  })

  // No arrangement should produce one. It is counted rather than assumed away, because a grid that
  // silently drops an edge is the failure this whole model exists to avoid.
  it('counts a held path no cell could claim', () => {
    const source = src('studio-a', ['cameras'])
    const listed = dst('edge-01', ['ingest'])
    const elsewhere = dst('edge-09', ['ingest'])
    const paths = [path('p1', source, 'f1', elsewhere, 'ACTIVE', ['nab/wall'])]

    const matrix = buildMatrix(paths, [request('wall', [source], [listed], [['p1']])], NODES, 'nab')
    expect(matrix.unplaced).toBe(1)
    // Not in the count the cells sum to either — the board cannot draw it, so claiming it would put
    // a number on the screen with nothing behind it. The alarm is where it is accounted for.
    expect(matrix.pathCount).toBe(0)
  })
})

describe('buildMatrix over a staged spec', () => {
  const source = src('studio-a', ['cameras'])
  const destination = dst('edge-01', ['ingest'])
  const live = path('p1', source, 'flow-1', destination, 'ACTIVE', ['nab/wall'])
  const stored = request('wall', [source], [destination], [['p1']])

  // `unplaced` asks whether the *server* handed us a path we cannot place. A leg the operator has
  // parked but not applied is not that: the stored spec still names it and it is still carrying
  // media, so the alarm must stay quiet while the cell draws it dark.
  it('does not report a staged park as a dropped edge', () => {
    const staged = { ...stored, destinations: [dst('edge-01', ['ingest'], true)] }

    const matrix = buildMatrix([live], [staged], NODES, 'nab', [stored])
    expect(matrix.unplaced).toBe(0)
    expect(matrix.cells[0]![0]).toMatchObject({ parked: true, state: 'DISABLED', count: 0 })
    // And out of the count the cells sum to, so the two agree while the change is pending. What the
    // path will do is the pending bar's to say.
    expect(matrix.pathCount).toBe(0)
    expect(buildMatrix([live], [stored], NODES, 'nab').pathCount).toBe(1)
  })

  // The same for a staged removal, where the column goes off the axis entirely.
  it('does not report a staged removal as a dropped edge', () => {
    const staged = { ...stored, destinations: [] }

    const matrix = buildMatrix([live], [staged], NODES, 'nab', [stored])
    expect(matrix.columns).toHaveLength(0)
    expect(matrix.unplaced).toBe(0)
  })

  // And the alarm still fires for the thing it is for: a path whose destination the *stored* spec
  // does not name either.
  it('still reports a path the stored spec cannot place', () => {
    const elsewhere = path('p2', source, 'flow-2', dst('edge-09', ['ingest']), 'ACTIVE', ['nab/wall'])
    const holding = request('wall', [source], [destination], [['p1', 'p2']])

    expect(buildMatrix([live, elsewhere], [holding], NODES, 'nab', [holding]).unplaced).toBe(1)
  })
})

describe('buildMatrix with a named but unrouted destination', () => {
  const source = src('studio-a', ['cameras'])
  const stored = request('wall', [source], [dst('edge-01', ['ingest'])], [[]])

  // A column is a domain that does not exist until a request names it, so a newly named one has
  // nowhere to come from — and it has to be on the axis first, because naming it is how the operator
  // gets a cell to click.
  it('puts it on the axis, unlit and marked as a draft', () => {
    const matrix = buildMatrix([], [stored], NODES, 'nab', [stored], [dst('edge-02', ['fresh'])])

    expect(matrix.columns.map((column) => column.domain)).toEqual(['fast/ingest', 'fast/fresh'])
    const fresh = matrix.columns[1]!
    expect(fresh).toMatchObject({ draft: true, requests: [], sources: 0, paths: 0 })
    // Not parked: nothing has parked it, and a column drawn dark would say "somebody routed this and
    // switched it off" about a destination nobody has routed at all.
    expect(fresh.parked).toBe(false)
    expect(matrix.cells[0]![1]).toMatchObject({ lit: false })
    // Its node joins the bands like any other.
    expect(matrix.columnBands.map((band) => band.node)).toEqual(['edge-01', 'edge-02'])
  })

  // The request is the authority on its own column, so one it names is not also a draft — and the
  // key is the same either way, so nothing moves on screen when the hand-off happens.
  it('defers to a request that names the same destination', () => {
    const matrix = buildMatrix([], [stored], NODES, 'nab', [stored], [dst('edge-01', ['ingest'])])

    expect(matrix.columns).toHaveLength(1)
    expect(matrix.columns[0]).toMatchObject({ draft: false, requests: ['nab/wall'] })
  })
})
