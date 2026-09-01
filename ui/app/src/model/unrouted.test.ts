import { describe, expect, it } from 'vitest'

import type { DomainName, FlowEntry, Path, PathState } from '@/api/types'
import { flowAddressKey } from './ownership'
import { buildUnrouted } from './unrouted'

function flow(
  node: string,
  domain: string,
  id: string,
  extra: Partial<FlowEntry> = {},
): FlowEntry {
  return {
    node,
    domain: domain as DomainName,
    id,
    flow_def: {},
    producing: true,
    ...extra,
  }
}

function path(
  id: string,
  source: [string, string, string],
  requests: string[],
  state: PathState = 'ACTIVE',
): Path {
  return {
    id,
    source: { node: source[0], domain: source[1] as DomainName, flow: source[2] },
    destination: { node: 'edge-01', domain: { area: 'fast', elements: ['ingest'] } },
    state,
    requests,
  }
}

// Four inventory entries on two nodes, and one of them is this project's own output on the edge.
const flows = [
  flow('studio-a', 'media/cameras', 'aaaa1111', { group_hint: { name: 'Studio A:Camera 1', type: 'video' } }),
  flow('studio-a', 'media/cameras', 'bbbb2222', { group_hint: { name: 'Studio A:Camera 2', type: 'video' } }),
  flow('studio-a', 'media/audio', 'cccc3333', { producing: false }),
  flow('edge-01', 'fast/ingest', 'aaaa1111', { replicated: true }),
]

// `nab/wall` carries camera 1; `k8s/pod` carries the audio flow, from another namespace.
const paths = [
  path('p1', ['studio-a', 'media/cameras', 'aaaa1111'], ['nab/wall']),
  path('p2', ['studio-a', 'media/audio', 'cccc3333'], ['k8s/pod']),
]

describe('buildUnrouted', () => {
  const strip = buildUnrouted(flows, paths, 'nab')

  // Asserted on the **triple**, because `aaaa1111` is also on `edge-01` as the copy replication
  // made: an assertion on the ID alone fails here, and would have been the same trap-4 mistake the
  // model exists to avoid, arriving from the test side.
  it('drops what this namespace already carries and counts it', () => {
    const keys = strip.domains.flatMap((group) => group.flows.map((entry) => entry.key))
    expect(keys).not.toContain(flowAddressKey('studio-a', 'media/cameras', 'aaaa1111'))
    expect(keys).toContain(flowAddressKey('edge-01', 'fast/ingest', 'aaaa1111'))
    expect(strip.routed).toBe(1)
  })

  // Namespaces partition requests, not nodes: fleet-wide the flow would vanish from the one view
  // where routing is built, and silently namespace-scoped it would talk the operator into doubling
  // egress on studio-a without ever saying so.
  it('lists a flow another namespace routes, and names the namespace', () => {
    const audio = strip.domains
      .flatMap((group) => group.flows)
      .find((entry) => entry.flow === 'cccc3333')
    expect(audio?.elsewhere).toEqual(['k8s'])
    expect(audio?.unclaimed).toBe(false)
  })

  // Legitimately a source — a chain A→B→C is written by naming it — but not *unrouted*.
  it("marks this project's own output rather than hiding it", () => {
    const written = strip.domains
      .flatMap((group) => group.flows)
      .find((entry) => entry.node === 'edge-01')
    expect(written?.replicated).toBe(true)
    expect(written?.unclaimed).toBe(false)
  })

  it('counts only what nothing accounts for as work', () => {
    expect(strip.unclaimed).toBe(1)
    expect(strip.elsewhere).toBe(1)
    expect(strip.replicated).toBe(1)
    expect(strip.count).toBe(3)
  })

  it('groups by (node, domain), which is the unit a source is written against', () => {
    expect(strip.domains.map((group) => `${group.node} ${group.domain}`)).toEqual([
      'edge-01 fast/ingest',
      'studio-a media/audio',
      'studio-a media/cameras',
    ])
  })

  // A UUID exists in two places after replication and that is success, not duplication.
  it('keys on the triple, so one flow in two places is two entries', () => {
    const both = buildUnrouted(flows, [], 'nab')
    const entries = both.domains.flatMap((group) => group.flows).filter((entry) => entry.flow === 'aaaa1111')
    expect(entries).toHaveLength(2)
    expect(new Set(entries.map((entry) => entry.key)).size).toBe(2)
  })
})

describe('buildUnrouted filtering and caps', () => {
  it('hides the accounted-for kinds but keeps counting them', () => {
    const strip = buildUnrouted(flows, paths, 'nab', { unclaimedOnly: true })
    expect(strip.domains.flatMap((group) => group.flows)).toHaveLength(1)
    expect(strip.count).toBe(1)
    // The filter must not make the toggle unable to say what it is hiding.
    expect(strip.elsewhere + strip.replicated).toBe(2)
  })

  // A silent cap reads as "nothing else was unrouted" — the failure `excluded_dropped` exists to
  // prevent on the wire, and the same one here.
  it('reports what the cap discarded', () => {
    const strip = buildUnrouted(flows, [], 'nab', { limit: 2 })
    expect(strip.domains.flatMap((group) => group.flows)).toHaveLength(2)
    expect(strip.count).toBe(4)
    expect(strip.dropped).toBe(2)
  })

  // Sorted before the cap, so what survives it is the part worth reading. `edge-01`'s entry is
  // replicated and sorts last within its own group, but the groups are ordered by node first.
  it('sorts work first within a domain', () => {
    const many = [
      flow('studio-a', 'media/cameras', 'dddd4444', { replicated: true }),
      flow('studio-a', 'media/cameras', 'eeee5555'),
    ]
    const strip = buildUnrouted(many, [], 'nab')
    expect(strip.domains[0]!.flows.map((entry) => entry.flow)).toEqual(['eeee5555', 'dddd4444'])
  })

  it('is empty rather than throwing on an empty fleet', () => {
    const strip = buildUnrouted([], [], 'nab')
    expect(strip.domains).toEqual([])
    expect(strip.count).toBe(0)
    expect(strip.routed).toBe(0)
  })
})
