/**
 * @vitest-environment jsdom
 *
 * The topology view against a live fleet.
 *
 * The assertion worth the harness is the **relay**: a node with a path in and a path out, which is
 * the one thing this view shows that no table in the product can. It cannot be made against a
 * read-only fixture, because `ui.md` §9's fleet has no node that is both a destination and a
 * source — every request in it reads a studio and writes an edge. So this file builds one, in a
 * namespace of its own, and the relay it asserts on is the server's own expansion rather than a mock's.
 *
 * The two hops are deliberately not the same media. A literal chain has hop two read the domain hop
 * one *materialised*, and against a fake fleet that would be asserting on whether the harness
 * invents inventory for a domain no real worker wrote. What the model actually computes is node
 * degree — in and out — so the fixture produces exactly that and the assertion is about the thing
 * under test rather than about the fixture's imagination.
 *
 * Its destinations are named clear of every other suite's: `edge-01 fast/topo` and `archive-01
 * bulk/topo`.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'topo'

describe('the topology view', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    await api.applyNamespace({ name: NS, paths: 'exclusive' })
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)

    // Into edge-01…
    await api.applyRequest(NS, {
      name: 'hop-in',
      sources: [{
        node: 'studio-a',
        domain: { name: { area: 'media', elements: ['cameras'] } },
        select: { group_hint: { name: 'Studio A:Camera 1' } },
      }],
      destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['topo'] } }],
    })
    // …and out of it again, which is what makes edge-01 a relay.
    await api.applyRequest(NS, {
      name: 'hop-out',
      sources: [{
        node: 'edge-01',
        domain: { name: { area: 'media', elements: ['local'] } },
        select: { all: true },
      }],
      destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['topo'] } }],
    })

    wrapper = await mountAt('/topology')
  })

  afterAll(async () => {
    wrapper?.unmount()
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    await api.deleteNamespace(NS)
  })

  const nodes = () => wrapper.findAll('.node')
  const node = (name: string) => nodes().find((entry) => entry.text().includes(name))
  const edges = () => wrapper.findAll('.edge')
  const edge = (from: string, to: string) =>
    edges().find((entry) => entry.find('title').text().startsWith(`${from} → ${to}`))
  const head = () => wrapper.find('.head').text()
  const panel = () => wrapper.find('.panel')

  it('draws a vertex per node and an edge per node pair', async () => {
    await until(() => node('archive-01') !== undefined, { timeoutMs: 15000 })
    expect(node('studio-a')).toBeDefined()
    expect(node('edge-01')).toBeDefined()
    expect(edge('studio-a', 'edge-01')).toBeDefined()
    expect(edge('edge-01', 'archive-01')).toBeDefined()
  })

  // The whole reason the view exists. In `/v1/paths` these are two unrelated rows.
  it('marks a node that is both a destination and a source', async () => {
    await until(() => node('edge-01')!.classes().includes('relay'), { timeoutMs: 15000 })

    expect(node('edge-01')!.text()).toContain('relay')
    expect(head()).toContain('relaying')
    // Not a relay: nothing arrives at a studio.
    expect(node('studio-a')!.classes()).not.toContain('relay')
  })

  // Left to right by longest path, so a relay sits between what feeds it and what it feeds.
  it('lays the relay out between its two ends', () => {
    const x = (name: string) => Number(node(name)!.find('rect').attributes('x'))
    expect(x('studio-a')).toBeLessThan(x('edge-01'))
    expect(x('edge-01')).toBeLessThan(x('archive-01'))
  })

  // A read view: the click focuses and fills the panel, and never wires anything.
  it('focuses a node and hands off to describe', async () => {
    const before = nodes().length
    await node('edge-01')!.trigger('click')

    expect(panel().exists()).toBe(true)
    expect(panel().text()).toContain('edge-01')
    expect(panel().findAll('tbody tr').length).toBeGreaterThan(0)

    // Selecting a node keeps its **neighbours** lit, which is the point: the question a click asks
    // is "what does this talk to". `studio-b` feeds `edge-01`, so it stays. `edge-02` does not touch
    // it in either direction, and it is the one that goes quiet.
    expect(node('studio-b')!.classes()).not.toContain('dim')
    expect(node('edge-02')!.classes()).toContain('dim')
    expect(edge('studio-a', 'edge-02')!.classes()).toContain('dim')

    // Dimmed, not removed — a graph that dropped what it was not about would be a different shape
    // on every click and the operator would lose the picture they were reading.
    expect(nodes()).toHaveLength(before)

    await node('edge-01')!.trigger('click')
    expect(wrapper.find('.panel').exists()).toBe(false)
  })

  it('focuses one edge and lists the paths it bundles', async () => {
    const bundle = edge('edge-01', 'archive-01')!
    await bundle.trigger('click')

    const rows = panel().findAll('tbody tr')
    expect(rows.length).toBeGreaterThan(0)
    // Ownership comes from `path.requests[]`, which is the only statement of it in the API.
    expect(panel().text()).toContain(`${NS}/hop-out`)
    expect(node('studio-b')!.classes()).toContain('dim')

    await wrapper.find('svg').trigger('click')
    expect(wrapper.find('.panel').exists()).toBe(false)
  })

  // Fleet-wide with a namespace *highlight*, never a namespace filter: a chain may cross namespaces,
  // and a filtered graph would cut it in half — which is the one thing this view exists to show.
  it('highlights a namespace without filtering the graph', async () => {
    await wrapper.find('.pick select').setValue(NS)

    expect(edge('edge-01', 'archive-01')!.classes()).not.toContain('dim')
    // Still drawn, and still in the same place. Only the emphasis moved.
    const foreign = edges().filter((entry) => entry.classes().includes('dim'))
    expect(foreign.length).toBeGreaterThan(0)
    expect(edges().length).toBeGreaterThan(foreign.length)

    await wrapper.find('.pick select').setValue('')
    expect(edges().every((entry) => !entry.classes().includes('dim'))).toBe(true)
  })

  // Neither should ever fire. They are asserted at zero so that the day one does, something says so.
  it('reports no cycle and no unregistered endpoint', () => {
    expect(head()).not.toContain('in a cycle')
    expect(head()).not.toContain('not registered')
  })
})
