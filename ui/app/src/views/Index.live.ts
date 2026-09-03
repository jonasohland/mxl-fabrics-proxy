/**
 * @vitest-environment jsdom
 *
 * The three index tables and the navigation around them, against a live fleet.
 *
 * What the harness is for here is the same class it has always caught: the page's behaviour against
 * a real *sequence* of reads and a real URL. Every assertion below is about a join or a route rather
 * than a rendering — that a filter survives being the only thing in the address bar, that the
 * current namespace marks rows without removing any, and that a URL naming a screen the namespace
 * cannot have corrects itself instead of lying.
 *
 * It writes its own preconditions, as every live suite here does: a fixture left by an earlier run
 * makes an assertion a statement about the store's history rather than about the rule. Two
 * namespaces of its own, `idx` exclusive and `idxs` shared, routed clear of every other suite —
 * `edge-01 fast/idx` and `archive-01 bulk/idx`.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import type { Path } from '@/api/types'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'idx'
const SHARED = 'idxs'

const rowsOf = (wrapper: Awaited<ReturnType<typeof mountAt>>) =>
  wrapper.findAll('.ls-table tbody tr')

const facetLabels = (wrapper: Awaited<ReturnType<typeof mountAt>>) =>
  wrapper.findAll('.ls-facet-label').map((label) => label.text())

/** The chip carrying one value, whatever facet it belongs to. */
function chip(wrapper: Awaited<ReturnType<typeof mountAt>>, value: string) {
  const found = wrapper.findAll('.ls-chip')
    .find((button) => button.find('.ls-chip-value').text() === value)
  if (!found) throw new Error(`no chip for ${value} among ${wrapper.findAll('.ls-chip').length}`)
  return found
}

async function seed() {
  await api.applyNamespace({ name: NS, paths: 'exclusive' })
  await api.applyNamespace({ name: SHARED, paths: 'shared' })
  for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
  for (const request of (await api.requests(SHARED)).requests) {
    await api.deleteRequest(SHARED, request.name)
  }

  await api.applyRequest(NS, {
    name: 'marked',
    sources: [{
      node: 'studio-a',
      domain: { name: { area: 'media', elements: ['cameras'] } },
      select: { group_hint: { name: 'Studio A:Camera 1' } },
    }],
    destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['idx'] } }],
  })
  await api.applyRequest(SHARED, {
    name: 'shared-claim',
    sources: [{
      node: 'studio-b',
      domain: { name: { area: 'media', elements: ['cameras'] } },
      select: { all: true },
    }],
    destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['idx'] } }],
  })

  await until(async () => (await api.requests(NS)).requests[0]?.status.paths.length ? true : false)
}

async function cleanup() {
  for (const namespace of [NS, SHARED]) {
    for (const request of (await api.requests(namespace)).requests) {
      await api.deleteRequest(namespace, request.name)
    }
    await api.deleteNamespace(namespace).catch(() => undefined)
  }
}

describe('the paths table', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let paths: Path[]

  beforeAll(async () => {
    await requireServer()
    await seed()
    globalThis.localStorage.clear()
    wrapper = await mountAt('/paths')
    paths = (await api.paths()).paths
  })

  afterAll(cleanup)

  it('draws every path in the fleet, fleet-wide', () => {
    expect(paths.length).toBeGreaterThan(0)
    expect(rowsOf(wrapper)).toHaveLength(paths.length)
  })

  /**
   * The decision this page is built around. A namespace is not a property of an edge — a path can
   * be claimed by requests in several at once — so `?ns=` could only mean *claimed by at least one
   * request in*, which is the ledger's question and is answered there with the selector that made
   * each claim. The facets are the things that genuinely are properties of an edge.
   */
  it('offers no namespace filter', () => {
    expect(facetLabels(wrapper)).toEqual(['state', 'node', 'session'])
  })

  it('narrows on a chip and says so in the URL', async () => {
    const establishing = paths.filter((path) => path.state === 'ESTABLISHING')
    expect(establishing.length).toBeGreaterThan(0)

    await chip(wrapper, 'ESTABLISHING').trigger('click')
    // Wait on the address bar rather than on a tick: a router navigation is asynchronous even when
    // nothing guards it, and asserting on the strength of one `nextTick` is a race this suite has
    // no reason to run.
    await until(() => window.location.search.includes('state=ESTABLISHING'))
    await wrapper.vm.$nextTick()

    expect(rowsOf(wrapper)).toHaveLength(establishing.length)
  })

  /**
   * The whole reason the selection lives in the URL rather than in a `ref`: the address bar has to
   * be enough on its own, because that is what an operator pastes to somebody else. Mounted fresh,
   * with no click behind it.
   */
  it('reproduces a filter from the URL alone', async () => {
    const paused = paths.filter((path) => path.state === 'PAUSED')
    const linked = await mountAt('/paths?state=PAUSED')
    expect(rowsOf(linked)).toHaveLength(paused.length)
    linked.unmount()
  })

  /**
   * A chip row counted after its own facet had been applied would read 0 beside every state the
   * operator did not pick, and stop being a way to move between them. So a facet counts against the
   * *other* facets only — which is what `filterRows`' `except` argument exists for.
   */
  it('counts a chip against the other facets and not against its own', async () => {
    const scoped = await mountAt('/paths?state=PAUSED')
    const establishing = paths.filter((path) => path.state === 'ESTABLISHING')
    expect(establishing.length).toBeGreaterThan(0)
    expect(chip(scoped, 'ESTABLISHING').find('.ls-chip-count').text())
      .toBe(String(establishing.length))
    scoped.unmount()
  })

  it('finds a path by free text without a facet for it', async () => {
    const searched = await mountAt('/paths?q=fast%2Fidx')
    const expected = paths.filter((path) =>
      path.destination.domain.area === 'fast' && path.destination.domain.elements[0] === 'idx')
    expect(expected.length).toBeGreaterThan(0)
    expect(rowsOf(searched)).toHaveLength(expected.length)
    searched.unmount()
  })
})

describe('the current namespace', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let paths: Path[]

  beforeAll(async () => {
    await requireServer()
    await seed()
    // How a namespace picked in an earlier session arrives: the store reads it back on creation.
    globalThis.localStorage.setItem('mxl.namespace', NS)
    wrapper = await mountAt('/paths')
    paths = (await api.paths()).paths
  })

  afterAll(async () => {
    globalThis.localStorage.clear()
    await cleanup()
  })

  /**
   * Marked, never filtered — the distinction the whole design turns on. A highlight changes which
   * rows are noticed; a filter changes which rows exist, and only the second belongs in a URL.
   */
  it('marks its rows and hides none', () => {
    const held = paths.filter((path) => path.requests.some((id) => id.startsWith(`${NS}/`)))
    expect(held.length).toBeGreaterThan(0)
    expect(rowsOf(wrapper)).toHaveLength(paths.length)
    expect(wrapper.findAll('.ls-table tbody tr.ls-mine')).toHaveLength(held.length)
  })

  it('reads back into the picker', () => {
    expect((wrapper.find('select').element as HTMLSelectElement).value).toBe(NS)
  })

  /**
   * The other half of "the select does not navigate from a fleet-wide screen".
   *
   * Not yanking an operator off the list they are reading is right, and on its own it left the
   * workspace unreachable from every fleet-wide screen in the bar — the select would mark rows and
   * offer nothing onward. The picker is a pair: the select says which namespace, the link goes
   * there.
   */
  it('offers a way into the workspace beside the picker', async () => {
    const open = wrapper.find('.picker .open')
    expect(open.exists()).toBe(true)
    expect(open.attributes('href')).toBe(`/ns/${NS}`)

    await open.trigger('click')
    await until(() => window.location.pathname === `/ns/${NS}`)
    expect(window.location.pathname).toBe(`/ns/${NS}`)
  })
})

describe('the workspace link with no namespace chosen', () => {
  beforeAll(async () => {
    await requireServer()
    globalThis.localStorage.clear()
  })

  // Shown and disabled with the reason rather than omitted, as everything off in this app is: a
  // control that vanishes produces "where did that go?", which is a worse question than one that
  // says why it is off.
  it('is shown and disabled rather than omitted', async () => {
    const wrapper = await mountAt('/paths')
    const open = wrapper.find('.picker .open')
    expect(open.exists()).toBe(true)
    expect(open.element.tagName).toBe('SPAN')
    expect(open.classes()).toContain('off')
    expect(open.attributes('title')).toContain('Pick a namespace')
    wrapper.unmount()
  })
})

describe('the workspace routes', () => {
  beforeAll(async () => {
    await requireServer()
    await seed()
    globalThis.localStorage.clear()
  })

  afterAll(cleanup)

  it('links the claims view of an exclusive namespace', async () => {
    const wrapper = await mountAt(`/ns/${NS}/claims`)
    expect(window.location.pathname).toBe(`/ns/${NS}/claims`)
    expect(wrapper.text()).toContain('Claims')
    wrapper.unmount()
  })

  /**
   * A URL asking for the grid of a shared namespace is corrected rather than rendered. The address
   * bar naming what is on screen is the entire point of putting the view in it, so `grid` beside a
   * ledger is worse than either screen — and the grid is genuinely wrong there, since two requests
   * may hold one path and a cell would stop meaning what it looks like.
   */
  it('corrects a grid URL in a shared namespace', async () => {
    const wrapper = await mountAt(`/ns/${SHARED}/grid`)
    await until(() => window.location.pathname === `/ns/${SHARED}/claims`)
    expect(window.location.pathname).toBe(`/ns/${SHARED}/claims`)
    wrapper.unmount()
  })

  it('leaves a bare namespace URL to the mode', async () => {
    const wrapper = await mountAt(`/ns/${NS}`)
    // Not rewritten: bare means *whichever screen this namespace has*, which is the link to send
    // when you neither know nor care whether it is exclusive.
    expect(window.location.pathname).toBe(`/ns/${NS}`)
    wrapper.unmount()
  })

  /**
   * The picker navigates only from a scoped screen, and carries the view across — which is the
   * behaviour the old component-local `ref` gave, kept now that the view lives in the URL.
   */
  it('carries the view across a namespace switch', async () => {
    const wrapper = await mountAt(`/ns/${SHARED}/claims`)
    const select = wrapper.find('select')
    await select.setValue(NS)
    await until(() => window.location.pathname === `/ns/${NS}/claims`)
    expect(window.location.pathname).toBe(`/ns/${NS}/claims`)
    wrapper.unmount()
  })

  it('does not navigate from a fleet-wide screen', async () => {
    const wrapper = await mountAt('/paths')
    await wrapper.find('select').setValue(NS)
    await wrapper.vm.$nextTick()
    // Re-marks and stays put: an operator reading the path list must not be thrown onto a grid.
    expect(window.location.pathname).toBe('/paths')
    expect(wrapper.findAll('.ls-table tbody tr.ls-mine').length).toBeGreaterThan(0)
    wrapper.unmount()
  })
})

describe('the nodes and requests tables', () => {
  beforeAll(async () => {
    await requireServer()
    await seed()
    globalThis.localStorage.clear()
  })

  afterAll(cleanup)

  it('lists every registered node, leased or not', async () => {
    const wrapper = await mountAt('/nodes')
    const nodes = (await api.nodes()).nodes
    expect(rowsOf(wrapper)).toHaveLength(nodes.length)
    wrapper.unmount()
  })

  /**
   * Where `/paths` refuses one, `/requests` filters by namespace — and the asymmetry is the model
   * rather than a preference: `namespace` is a field on a request spec and a request is in exactly
   * one, while a path is claimed by however many requests expand onto it.
   */
  it('filters requests by namespace', async () => {
    const wrapper = await mountAt(`/requests?ns=${NS}`)
    const mine = (await api.requests(NS)).requests
    expect(mine.length).toBeGreaterThan(0)
    expect(rowsOf(wrapper)).toHaveLength(mine.length)
    wrapper.unmount()
  })

  it('lists every request across every namespace when unfiltered', async () => {
    const wrapper = await mountAt('/requests')
    const all = (await api.requests()).requests
    expect(all.length).toBeGreaterThan((await api.requests(NS)).requests.length)
    expect(rowsOf(wrapper)).toHaveLength(all.length)
    wrapper.unmount()
  })
})
