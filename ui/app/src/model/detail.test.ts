import { describe, expect, it } from 'vitest'

import type { Area, Domain, Path } from '@/api/types'
import { UINT64_MAX } from '@/api/types'
import {
  addressText,
  byteSize,
  capFlagsText,
  describeDefinition,
  endpointText,
  firstGroupHint,
  grantText,
  groupHintText,
  locationsOf,
  pathsAtDomain,
  pathsCarrying,
  pathsTouching,
  providerText,
  since,
} from './detail'

const domain = (area: string, ...elements: string[]): Domain => ({ area, elements })

function path(over: Partial<Path> & Pick<Path, 'id'>): Path {
  return {
    source: { node: 'studio-a', domain: 'media/cameras' as Path['source']['domain'], flow: 'f1' },
    destination: { node: 'edge-01', domain: domain('fast', 'ingest') },
    state: 'ACTIVE',
    requests: ['nab/wall'],
    ...over,
  }
}

describe('byteSize', () => {
  // The wire carries 18446744073709551615 and JSON.parse turns it into 18446744073709552000, so the
  // client quotes it before parsing and the value arrives as a string. Rendering it as a number
  // would be less use than saying so even if it survived the round trip.
  it('renders UINT64_MAX as unlimited, from either spelling', () => {
    expect(byteSize(UINT64_MAX.toString())).toBe('unlimited')
    expect(byteSize('18446744073709551615')).toBe('unlimited')
  })

  it('scales and keeps small values exact', () => {
    expect(byteSize(512)).toBe('512 B')
    expect(byteSize(2048)).toBe('2.0 KiB')
    expect(byteSize(3 * 1024 * 1024)).toBe('3.0 MiB')
  })

  it('has an answer for an absent field, since every one of them may be absent', () => {
    expect(byteSize(undefined)).toBe('·')
  })
})

describe('since', () => {
  const now = Date.parse('2026-09-01T12:00:00Z')

  it('is coarse: seconds, then minutes, then hours, then days', () => {
    expect(since('2026-09-01T11:59:30Z', now)).toBe('30s')
    expect(since('2026-09-01T11:30:00Z', now)).toBe('30m')
    expect(since('2026-09-01T06:00:00Z', now)).toBe('6h')
    expect(since('2026-08-29T12:00:00Z', now)).toBe('3d')
  })

  // Timestamps use omitzero: absent, not null, not epoch. Guarding every one is the rule, and a
  // formatter that returned "56 years" for a missing field is how it gets broken.
  it('says nothing about an absent or unparseable timestamp', () => {
    expect(since(undefined, now)).toBeUndefined()
    expect(since('', now)).toBeUndefined()
    expect(since('not a time', now)).toBeUndefined()
  })
})

describe('providerText', () => {
  it('renders both spellings of the pin, in preference order', () => {
    expect(providerText('verbs')).toBe('verbs')
    expect(providerText(['verbs', 'tcp'])).toBe('verbs > tcp')
    expect(providerText(undefined)).toBe('')
  })
})

describe('grantText', () => {
  const area = (read: boolean, write: boolean): Area => ({ name: 'fast', read, write })

  // Both default false and neither implies the other: a node with no readable area offers no
  // sources and one with no writable area accepts no destinations. "No grants" is a real answer.
  it('spells the two independent grants', () => {
    expect(grantText(area(true, true))).toBe('read+write')
    expect(grantText(area(false, true))).toBe('write')
    expect(grantText(area(true, false))).toBe('read')
    expect(grantText(area(false, false))).toBe('no grants')
  })
})

describe('capFlagsText and groupHintText', () => {
  it('names an empty capability set rather than rendering nothing', () => {
    expect(capFlagsText([])).toBe('no capabilities')
    expect(capFlagsText(undefined)).toBe('no capabilities')
    expect(capFlagsText(['REMOTE_WRITE', 'SEND_RECEIVE'])).toBe('REMOTE_WRITE, SEND_RECEIVE')
  })

  it('shows a hint type only when there is one', () => {
    expect(groupHintText({ name: 'Camera 1', type: 'urn:x-nmos:tag:grouphint/v1.0' }))
      .toBe('Camera 1 (urn:x-nmos:tag:grouphint/v1.0)')
    expect(groupHintText({ name: 'Camera 1', type: '' })).toBe('Camera 1')
    expect(groupHintText(undefined)).toBe('')
  })
})

describe('addressText', () => {
  // A session in ESTABLISHING legitimately has a fabric and an interface config and no endpoints at
  // all, and an endpoint that has bound may still have reported no service.
  it('joins host and service, and has an answer for neither', () => {
    expect(addressText({ node: 'a', state: 'ready', restarts: 0, address: '10.0.0.1', service: '9000' }))
      .toBe('10.0.0.1:9000')
    expect(addressText({ node: 'a', state: 'ready', restarts: 0, address: '10.0.0.1' })).toBe('10.0.0.1')
    expect(addressText({ node: 'a', state: 'starting', restarts: 0 })).toBe('·')
    expect(addressText(undefined)).toBe('·')
  })
})

describe('describeDefinition', () => {
  it('summarises the NMOS fields worth a line', () => {
    expect(
      describeDefinition({
        label: 'Camera 1 video',
        format: 'urn:x-nmos:format:video',
        frame_width: 1920,
        frame_height: 1080,
        grain_rate: { numerator: 60000, denominator: 1001 },
      }),
    ).toBe('"Camera 1 video", video, 1920x1080, 59.940 Hz')
  })

  it('prefers media_type over format, and defaults an absent denominator to 1', () => {
    expect(describeDefinition({ media_type: 'video/raw', grain_rate: { numerator: 50 } }))
      .toBe('video/raw, 50.000 Hz')
  })

  // Arbitrary content, including fields nothing in this tree models. A summary is a best effort over
  // whatever is there and must never be the reason a page fails to render.
  it('says nothing about a definition it does not recognise', () => {
    expect(describeDefinition({ something: 'else' })).toBe('')
    expect(describeDefinition(undefined)).toBe('')
    expect(describeDefinition('a string')).toBe('')
    expect(describeDefinition(null)).toBe('')
  })
})

describe('pathsTouching', () => {
  // The bug a live run caught and a unit test would not, so here is the unit test: a node can be
  // *both* ends of a path — same node, different domain — which is what the loopback configuration
  // does. A switch that matched the source first would hide half of what the node is running.
  it('reports a node that is both ends of one path twice', () => {
    const loopback = path({
      id: 'p1',
      source: { node: 'edge-01', domain: 'media/local' as Path['source']['domain'], flow: 'f1' },
      destination: { node: 'edge-01', domain: domain('fast', 'ingest') },
    })

    const rows = pathsTouching('edge-01', [loopback])

    expect(rows.map((row) => row.role)).toEqual(['initiator', 'target'])
    expect(rows[0]!.peer).toBe('edge-01 fast/ingest')
    expect(rows[1]!.peer).toBe('edge-01 media/local')
  })

  it('gives each end its own row and names the other end as the peer', () => {
    const rows = pathsTouching('edge-01', [path({ id: 'p1' })])
    expect(rows).toHaveLength(1)
    expect(rows[0]!.role).toBe('target')
    expect(rows[0]!.peer).toBe('studio-a media/cameras')
  })

  it('is empty for a node no path touches', () => {
    expect(pathsTouching('archive-01', [path({ id: 'p1' })])).toEqual([])
  })
})

describe('pathsAtDomain', () => {
  const outgoing = path({ id: 'out' })
  const incoming = path({
    id: 'in',
    source: { node: 'studio-b', domain: 'media/cameras' as Path['source']['domain'], flow: 'f2' },
    destination: { node: 'edge-01', domain: domain('fast', 'ingest') },
  })

  // A domain is a place, not a direction: the one a request writes into is discovered and observed
  // like any other, so both questions are about one object.
  it('answers what leaves here and what lands here', () => {
    expect(pathsAtDomain('studio-a', 'media/cameras' as Path['source']['domain'], [outgoing, incoming]))
      .toEqual([{ role: 'initiator', path: outgoing, peer: 'edge-01 fast/ingest' }])

    const landing = pathsAtDomain('edge-01', 'fast/ingest' as Path['source']['domain'], [outgoing, incoming])
    expect(landing.map((row) => row.path.id)).toEqual(['out', 'in'])
    expect(landing.every((row) => row.role === 'target')).toBe(true)
  })

  // fast/ingest and bulk/ingest are two different strings and two different directory trees. The
  // rendered name is already injective over what the wire accepts, which is why it is the key.
  it('does not confuse the same elements under a different area', () => {
    expect(pathsAtDomain('edge-01', 'bulk/ingest' as Path['source']['domain'], [outgoing])).toEqual([])
  })
})

describe('pathsCarrying', () => {
  // After replication the same UUID exists at the destination too. A path whose *destination*
  // carries the ID is the one writing it, not one carrying it, so the match is on the source.
  it('matches on the source address', () => {
    const carrying = path({ id: 'p1' })
    expect(pathsCarrying('f1', [carrying])).toEqual([carrying])
    expect(pathsCarrying('f2', [carrying])).toEqual([])
  })
})

describe('locationsOf and firstGroupHint', () => {
  const entry = (node: string, id: string, hint?: { name: string; type: string }) => ({
    node,
    domain: 'media/cameras' as Path['source']['domain'],
    id,
    flow_def: {},
    producing: true,
    ...(hint ? { group_hint: hint } : {}),
  })

  it('keeps every location of one ID — the multiplicity is the answer', () => {
    const entries = [entry('studio-a', 'f1'), entry('edge-01', 'f1'), entry('studio-a', 'f2')]
    expect(locationsOf('f1', entries).map((e) => e.node)).toEqual(['studio-a', 'edge-01'])
  })

  it('takes the hint from whichever location reports one', () => {
    const hint = { name: 'Camera 1', type: '' }
    expect(firstGroupHint([entry('studio-a', 'f1'), entry('edge-01', 'f1', hint)])).toBe(hint)
    expect(firstGroupHint([entry('studio-a', 'f1')])).toBeUndefined()
  })
})

describe('endpointText', () => {
  it('is `<node> <area>/<elements>`, as a destination is headed everywhere else', () => {
    expect(endpointText({ node: 'edge-01', domain: domain('fast', 'studio-a', 'cam1') }))
      .toBe('edge-01 fast/studio-a/cam1')
  })
})
