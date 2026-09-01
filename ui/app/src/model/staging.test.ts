import { describe, expect, it } from 'vitest'

import type { Destination, DomainName, Path, PathState, Request, Source } from '@/api/types'
import { endpointKey } from './ownership'
import type { Edit } from './staging'
import {
  blastRadius,
  effectiveDestinations,
  effectiveRequest,
  isChange,
  isEmpty,
  namespaceOfId,
  removalRadius,
  specOf,
  verb,
} from './staging'

const dst = (node: string, elements: string[], disabled?: boolean): Destination => ({
  node,
  domain: { area: 'fast', elements },
  ...(disabled ? { disabled: true } : {}),
})

const src = (node: string): Source => ({
  node,
  domain: { name: { area: 'media', elements: ['cameras'] } },
  select: { all: true },
})

function request(destinations: Destination[], extra: Partial<Request> = {}): Request {
  return {
    id: 'nab/wall',
    namespace: 'nab',
    name: 'wall',
    created_at: '2026-01-01T00:00:00Z',
    sources: [src('studio-a')],
    destinations,
    status: { state: 'ACTIVE', paths: [] },
    ...extra,
  }
}

function path(id: string, state: PathState, requests: string[]): Path {
  return {
    id,
    source: { node: 'studio-a', domain: 'media/cameras' as DomainName, flow: `flow-${id}` },
    destination: dst('edge-01', ['ingest']),
    state,
    requests,
  }
}

const edit = (column: Destination, want: 'on' | 'off' | 'gone'): Edit => ({
  target: 'leg',
  request: 'nab/wall',
  column: endpointKey(column),
  want,
  destination: column,
})

const drop = (source: Source): Edit => ({ target: 'source', request: 'nab/wall', source, want: 'out' })
const join = (source: Source): Edit => ({ target: 'source', request: 'nab/wall', source, want: 'in' })

describe('isChange', () => {
  // Read on every render rather than decided when the edit was made: the fleet is replaced wholesale
  // every poll, so an edit somebody else's apply has already satisfied must stop being pending.
  it('is false once the stored spec already says what the edit asks for', () => {
    const leg = dst('edge-01', ['ingest'])
    expect(isChange(request([leg]), edit(leg, 'off'))).toBe(true)
    expect(isChange(request([dst('edge-01', ['ingest'], true)]), edit(leg, 'off'))).toBe(false)
    expect(isChange(request([dst('edge-01', ['ingest'], true)]), edit(leg, 'on'))).toBe(true)
    expect(isChange(request([leg]), edit(leg, 'on'))).toBe(false)
  })

  // A leg that is not in the spec at all is something to add, and nothing to park.
  it('treats an absent entry as off', () => {
    const other = dst('edge-02', ['ingest'])
    expect(isChange(request([dst('edge-01', ['ingest'])]), edit(other, 'on'))).toBe(true)
    expect(isChange(request([dst('edge-01', ['ingest'])]), edit(other, 'off'))).toBe(false)
  })
})

describe('verb', () => {
  // The intent is a state; the verb is derived, because a rebase can turn an enable into an add
  // while what the operator asked for cannot change.
  it('reads the intent against what is stored', () => {
    const leg = dst('edge-01', ['ingest'])
    expect(verb(request([leg]), edit(leg, 'off'))).toBe('park')
    expect(verb(request([dst('edge-01', ['ingest'], true)]), edit(leg, 'on'))).toBe('enable')
    expect(verb(request([dst('edge-02', ['ingest'])]), edit(leg, 'on'))).toBe('add')
  })
})

describe('effectiveDestinations', () => {
  // Parking is not deleting: the entry stays in the spec, expands to nothing, and keeps its column on
  // the axis. Removing it would delete the column it lived on and rearrange the board.
  it('parks an entry rather than removing it', () => {
    const leg = dst('edge-01', ['ingest'])
    const result = effectiveDestinations(request([leg]), [edit(leg, 'off')])
    expect(result).toHaveLength(1)
    expect(result[0]!.disabled).toBe(true)
  })

  // `disabled` is omitempty and the zero value is the one that keeps media running, so a spec
  // carrying `false` is not identical to one that was never parked and an apply would write for
  // nothing (`ui.md` §5 trap 15).
  it('deletes the flag when un-parking rather than writing false', () => {
    const leg = dst('edge-01', ['ingest'])
    const result = effectiveDestinations(request([dst('edge-01', ['ingest'], true)]), [edit(leg, 'on')])
    expect('disabled' in result[0]!).toBe(false)
  })

  // The representative entry an edit arrives with is whichever request defined the column first, and
  // it may carry that request's provider override or its parked flag. Neither was asked for.
  it('creates a new entry with nothing but its node and domain', () => {
    const carrier: Destination = { node: 'edge-02', domain: { area: 'fast', elements: ['ingest'] }, provider: 'tcp', disabled: true }
    const result = effectiveDestinations(request([dst('edge-01', ['ingest'])]), [edit(carrier, 'on')])
    expect(result).toHaveLength(2)
    expect(result[1]).toEqual({ node: 'edge-02', domain: { area: 'fast', elements: ['ingest'] } })
  })

  // The stored request is never mutated: it is the poll's own object and the grid renders it beside
  // the effective one.
  it('leaves the stored spec alone', () => {
    const stored = request([dst('edge-01', ['ingest'])])
    effectiveDestinations(stored, [edit(dst('edge-01', ['ingest']), 'off')])
    expect(stored.destinations[0]!.disabled).toBeUndefined()
  })

  // An entry somebody else removed between the click and the apply has nothing to park.
  it('ignores an edit whose entry has gone', () => {
    const stored = request([dst('edge-01', ['ingest'])])
    expect(effectiveDestinations(stored, [edit(dst('edge-09', ['ingest']), 'off')])).toHaveLength(1)
  })
})

describe('removals', () => {
  const leg = dst('edge-01', ['ingest'])

  // `×` only ever removes something already dark, and this is the one edit that takes the entry out
  // rather than flipping a flag on it.
  it('takes the entry out of the spec', () => {
    const result = effectiveDestinations(request([leg, dst('edge-02', ['ingest'])]), [edit(leg, 'gone')])
    expect(result.map((entry) => entry.node)).toEqual(['edge-02'])
  })

  it('has nothing to remove once the entry has gone', () => {
    expect(isChange(request([leg]), edit(leg, 'gone'))).toBe(true)
    expect(isChange(request([dst('edge-02', ['ingest'])]), edit(leg, 'gone'))).toBe(false)
  })

  // `status.sources[i]` joins to `sources[i]` **by index**, so dropping a source without its
  // breakdown would attribute one source's live paths to another's row — the same class of silent
  // error as trap 14.
  it('drops a source and its status breakdown together', () => {
    const a = src('studio-a')
    const b: Source = { ...src('studio-b'), select: { all: true } }
    const stored = request([leg], {
      sources: [a, b],
      status: {
        state: 'ACTIVE',
        paths: [],
        sources: [
          { source: a, state: 'ACTIVE', paths: ['p1'] },
          { source: b, state: 'WAITING', paths: [] },
        ],
      },
    })

    const effective = effectiveRequest(stored, [drop(a)])
    expect(effective.sources).toEqual([b])
    expect(effective.status.sources).toEqual([{ source: b, state: 'WAITING', paths: [] }])
  })

  // An added source **appends**, for the same reason a removed one takes its breakdown with it: the
  // join is by index, so a source arriving in the middle would shift every entry after it. Appended,
  // the kept prefix keeps its correspondence and the new one has no breakdown — which is the truth.
  it('appends an added source and leaves the breakdown aligned', () => {
    const a = src('studio-a')
    const b: Source = { ...src('studio-b'), select: { all: true } }
    const stored = request([leg], {
      sources: [a],
      status: { state: 'ACTIVE', paths: [], sources: [{ source: a, state: 'ACTIVE', paths: ['p1'] }] },
    })

    const effective = effectiveRequest(stored, [join(b)])
    expect(effective.sources).toEqual([a, b])
    expect(effective.status.sources).toEqual([{ source: a, state: 'ACTIVE', paths: ['p1'] }])
    // Adding a source the request already names is not a change, so it is not staged and not applied
    // twice: two requests naming the same three things are one row, and so are two entries.
    expect(isChange(stored, join(a))).toBe(false)
    expect(effectiveRequest(stored, [join(a)]).sources).toEqual([a])
  })

  // A request must name at least one source and one destination, so an emptied spec has nothing to
  // POST and committing it is a DELETE. The ordering hazard `ui.md` §7a warns about does not arise:
  // the staged set is a set, so removing the last leg and adding another reach the same spec
  // whichever order they were clicked in.
  it('reports an emptied spec, from either end', () => {
    const stored = request([leg])
    expect(isEmpty(effectiveRequest(stored, [edit(leg, 'gone')]))).toBe(true)
    expect(isEmpty(effectiveRequest(stored, [drop(stored.sources[0]!)]))).toBe(true)
    expect(isEmpty(effectiveRequest(stored, [edit(leg, 'off')]))).toBe(false)

    const other = dst('edge-02', ['ingest'])
    expect(isEmpty(effectiveRequest(stored, [edit(leg, 'gone'), edit(other, 'on')]))).toBe(false)
  })

  // A DELETE has no dry run and needs none: cancelling drops every path the request holds, and which
  // of them stop is `path.requests[]` and nothing else.
  it('reads a deletion off the refcounts', () => {
    const paths = [
      path('p1', 'ACTIVE', ['nab/wall']),
      path('p2', 'ACTIVE', ['nab/wall', 'other/keeps-it']),
    ]
    const radius = removalRadius(request([leg]), paths)
    expect(radius.stopping).toHaveLength(2)
    expect(radius.ridesAlong.map((entry) => entry.id)).toEqual(['p2'])
    expect(radius.stops).toBe(1)
    expect(radius.starting).toEqual([])
  })
})

describe('specOf', () => {
  // `Request` extends `RequestSpec`, so a rest destructure would ride every field the API adds into
  // the body of every apply this screen makes.
  it('carries the spec and nothing derived', () => {
    const stored = request([dst('edge-01', ['ingest'])], {
      updated_at: '2026-02-02T00:00:00Z',
      labels: { show: 'nab' },
    })
    expect(specOf(stored)).toEqual({
      namespace: 'nab',
      name: 'wall',
      sources: stored.sources,
      destinations: stored.destinations,
      labels: { show: 'nab' },
    })
  })

  it('omits what was not set rather than writing undefined', () => {
    const spec = specOf(request([dst('edge-01', ['ingest'])]))
    expect('provider' in spec).toBe(false)
    expect('sched_prio' in spec).toBe(false)
  })
})

describe('blastRadius', () => {
  const stored = request([dst('edge-01', ['ingest'])])

  // "3 of 4 paths stop" is worth reading; "are you sure?" is not. The difference between the two
  // numbers is the refcount, which says whether a leg leaving this request stops any media at all.
  it('separates a path that stops from one another request keeps alive', () => {
    const paths = [
      path('p1', 'ACTIVE', ['nab/wall']),
      path('p2', 'ACTIVE', ['nab/wall', 'other/keeps-it']),
    ]
    const result = request([dst('edge-01', ['ingest'], true)], { status: { state: 'DISABLED', paths: [] } })

    const radius = blastRadius(stored, result, paths)
    expect(radius.stopping.map((entry) => entry.id)).toEqual(['p1', 'p2'])
    expect(radius.ridesAlong.map((entry) => entry.id)).toEqual(['p2'])
    expect(radius.stops).toBe(1)
  })

  // A path that already exists under somebody else's claim is joined and refcounted, so nothing
  // starts — ordinary across namespaces, which partition requests and not destinations.
  it('separates a path that starts from one it would only join', () => {
    const paths = [path('p2', 'ACTIVE', ['archive/wall'])]
    const result = request([dst('edge-01', ['ingest'])], {
      status: {
        state: 'ESTABLISHING',
        paths: [
          { id: 'p1', source: paths[0]!.source, destination: paths[0]!.destination, state: 'WAITING' },
          { id: 'p2', source: paths[0]!.source, destination: paths[0]!.destination, state: 'ACTIVE' },
        ],
      },
    })

    const radius = blastRadius(stored, result, paths)
    expect(radius.starting).toEqual(['p1'])
    expect(radius.joining).toEqual(['p2'])
    expect(radius.stops).toBe(0)
  })

  // Trap 14: the loser of a namespace overlap lists the contested path with the incumbent's state. A
  // dry run answers with a request and no candidate path list, so its own reason code is the only
  // cross-check available — and it is enough to stop the counts reading as this request's own.
  it('says when the result would hold none of the paths it lists', () => {
    const result = request([dst('edge-01', ['ingest'])], {
      status: {
        state: 'INVALID',
        reason_code: 'namespace_overlap',
        reason: 'request "other" already replicates …',
        paths: [{ id: 'p9', source: path('p9', 'ACTIVE', []).source, destination: dst('edge-01', ['ingest']), state: 'ACTIVE' }],
      },
    })
    expect(blastRadius(stored, result, []).holdsNothing).toBe(true)
  })

  // No dry run yet is not "nothing happens": the bar says the preview is pending rather than
  // reporting a radius of zero it has not been told.
  it('reports nothing at all without a result', () => {
    const radius = blastRadius(stored, undefined, [path('p1', 'ACTIVE', ['nab/wall'])])
    expect(radius).toMatchObject({ stops: 0, stopping: [], starting: [], joining: [] })
  })
})

describe('namespaceOfId', () => {
  it('reads the namespace off the rendered id', () => {
    expect(namespaceOfId('nab/wall')).toBe('nab')
  })
})
