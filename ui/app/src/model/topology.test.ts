import { describe, expect, it } from 'vitest'

import type { DomainName, Path, PathState } from '@/api/types'
import { buildTopology } from './topology'

let counter = 0

function path(
  from: string,
  to: string,
  state: PathState = 'ACTIVE',
  requests: string[] = ['nab/wall'],
  flow = `flow-${counter++}`,
): Path {
  return {
    id: `p${counter}`,
    source: { node: from, domain: 'media/cameras' as DomainName, flow },
    destination: { node: to, domain: { area: 'fast', elements: ['ingest'] } },
    state,
    requests,
  }
}

const registered = (...names: string[]) => names.map((name) => ({ name, live: true }))

describe('buildTopology layering', () => {
  // The whole reason the view exists: two rows in `/v1/paths` that are one chain, and there is
  // nowhere else in the product the middle of it can be seen.
  it('lays a chain out left to right and names the relay', () => {
    const topology = buildTopology(
      [path('studio-a', 'edge-01'), path('edge-01', 'archive-01')],
      registered('studio-a', 'edge-01', 'archive-01'),
    )

    expect(topology.layers.map((layer) => layer.map((node) => node.name))).toEqual([
      ['studio-a'],
      ['edge-01'],
      ['archive-01'],
    ])
    expect(topology.relays).toBe(1)
    expect(topology.nodes.find((node) => node.name === 'edge-01')!.relay).toBe(true)
    expect(topology.nodes.find((node) => node.name === 'studio-a')!.relay).toBe(false)
  })

  // Longest path, not shortest: with A→B, A→C and B→C, C belongs right of B or the edge from B
  // would point backwards through a column it should be to the right of.
  it('places a node right of its furthest predecessor', () => {
    const topology = buildTopology(
      [path('a', 'b'), path('a', 'c'), path('b', 'c')],
      registered('a', 'b', 'c'),
    )
    const layerOf = (name: string) => topology.nodes.find((node) => node.name === name)!.layer

    expect(layerOf('a')).toBe(0)
    expect(layerOf('b')).toBe(1)
    expect(layerOf('c')).toBe(2)
    expect(topology.edges.every((edge) => !edge.back)).toBe(true)
  })

  // The loopback the design supports — same node, two domains. A self-edge must not be counted as an
  // incoming edge for layering, or the node never reaches in-degree zero and is reported as a cycle.
  it('treats a self-loop as a loop and not as a cycle', () => {
    const topology = buildTopology([path('edge-01', 'edge-01')], registered('edge-01'))

    expect(topology.cyclic).toEqual([])
    expect(topology.edges).toHaveLength(1)
    expect(topology.edges[0]!.loop).toBe(true)
    expect(topology.edges[0]!.back).toBe(false)
    // In and out both, because it is both — which makes the loopback a chain with one host in it.
    expect(topology.nodes[0]!.relay).toBe(true)
  })

  // The server refuses a `loop`, so this is a defensive branch. What it must not do is quietly
  // straighten the cycle into a picture that cannot be checked against the fleet.
  it('names the nodes of a cycle rather than drawing it as a line', () => {
    const topology = buildTopology([path('a', 'b'), path('b', 'a')], registered('a', 'b'))

    expect(topology.cyclic.length).toBeGreaterThan(0)
    expect(topology.edges.some((edge) => edge.back)).toBe(true)
  })

  it('is stable: the same fleet in a different order is the same picture', () => {
    const paths = [path('studio-b', 'edge-01'), path('studio-a', 'edge-01'), path('edge-01', 'archive-01')]
    const first = buildTopology(paths, registered('studio-a', 'studio-b', 'edge-01', 'archive-01'))
    const second = buildTopology(
      [...paths].reverse(),
      registered('archive-01', 'edge-01', 'studio-b', 'studio-a'),
    )

    expect(second.layers.map((layer) => layer.map((node) => node.name)))
      .toEqual(first.layers.map((layer) => layer.map((node) => node.name)))
    expect(second.edges.map((edge) => edge.key)).toEqual(first.edges.map((edge) => edge.key))
  })
})

describe('buildTopology edges and vertices', () => {
  const paths = [
    path('studio-a', 'edge-01', 'ACTIVE', ['nab/wall'], 'cam1'),
    path('studio-a', 'edge-01', 'FAILED', ['nab/wall'], 'cam2'),
    path('studio-b', 'edge-01', 'ACTIVE', ['k8s/pod'], 'cam3'),
  ]
  const topology = buildTopology(paths, registered('studio-a', 'studio-b', 'edge-01', 'archive-01'))

  // One edge per node pair, whatever it carries: the graph's altitude is the host, and two flows
  // between two hosts is one relationship with two things in it.
  it('bundles paths between one pair into one edge', () => {
    expect(topology.edges).toHaveLength(2)
    const bundle = topology.edges.find((edge) => edge.from === 'studio-a')!
    expect(bundle.paths).toHaveLength(2)
    expect(bundle.flows).toBe(2)
  })

  // The same fold every other aggregate in this app uses, so an edge and a matrix cell over the same
  // paths say the same word.
  it('folds an edge worst-first, and says PARTIAL when some of it works', () => {
    expect(topology.edges.find((edge) => edge.from === 'studio-a')!.state).toBe('PARTIAL')
    expect(topology.edges.find((edge) => edge.from === 'studio-b')!.state).toBe('ACTIVE')
  })

  // Namespaces partition requests, not nodes — so an edge may carry more than one, and that is the
  // fan-in-across-shows fact a namespace-scoped screen cannot see.
  it('names every namespace on an edge', () => {
    expect(topology.edges.find((edge) => edge.from === 'studio-b')!.namespaces).toEqual(['k8s'])
    expect(topology.edges.find((edge) => edge.from === 'studio-a')!.namespaces).toEqual(['nab'])
  })

  // Ingress is the binding direction for an ingest wall — an edge is bounded by what it can take.
  it('counts what arrives and what leaves', () => {
    const edge01 = topology.nodes.find((node) => node.name === 'edge-01')!
    expect(edge01.in).toBe(3)
    expect(edge01.out).toBe(0)
  })

  // Registered and idle is a fact worth seeing, and it is not the same sentence as "origin".
  it('keeps a node no path touches out of the layers and lists it', () => {
    expect(topology.isolated.map((node) => node.name)).toEqual(['archive-01'])
    expect(topology.layers.flat().map((node) => node.name)).not.toContain('archive-01')
  })

  // Should never happen; drawn rather than dropped if it does, because dropping it would silently
  // lose the edge that names it.
  it('draws a path endpoint that is not a registered node, and says so', () => {
    const ghost = buildTopology([path('studio-a', 'nowhere')], registered('studio-a'))
    expect(ghost.unregistered).toEqual(['nowhere'])
    expect(ghost.nodes.find((node) => node.name === 'nowhere')!.registered).toBe(false)
    expect(ghost.nodes.find((node) => node.name === 'nowhere')!.live).toBeUndefined()
    expect(ghost.edges).toHaveLength(1)
  })

  it('is empty rather than throwing on an empty fleet', () => {
    const empty = buildTopology([], [])
    expect(empty.layers).toEqual([])
    expect(empty.edges).toEqual([])
    expect(empty.relays).toBe(0)
  })
})
