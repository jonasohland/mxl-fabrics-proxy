/**
 * @vitest-environment jsdom
 *
 * The matrix against an `exclusive` namespace, seeded here rather than inherited.
 *
 * `reset()` writes its own preconditions for the same reason the prototype's harness did: a fixture
 * left behind by an earlier run makes the assertions a statement about the store's history rather
 * than about the rule. The four requests are the shapes §7a has to draw —
 *
 * - `wall`, two sources over two studios onto two destinations: the rectangle, and the arrangement
 *   fan-in exists for;
 * - `talkback`, one source into a domain **another namespace also writes into**, which is the one
 *   cross-namespace fact a screen showing one namespace cannot otherwise see;
 * - `archive`, whose only destination is parked: the column that must survive being switched off;
 * - `future`, whose group hint matches no flow anybody has published: accepted, not yet satisfiable,
 *   which is a quiet cell state and not an error.
 *
 * They route clear of `k8s`'s own edges deliberately. Namespaces partition requests and not
 * destinations, so an identical `(source flow → destination)` here would be one path with two
 * claims — true, supported, and not what this file is about.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import type { DomainSelector, RequestSpec } from '@/api/types'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'nab'

const cameras: DomainSelector = { name: { area: 'media', elements: ['cameras'] } }

const FIXTURE: RequestSpec[] = [
  {
    name: 'wall',
    sources: [
      { node: 'studio-a', domain: cameras, select: { group_hint: { name: 'Studio A:Camera 1' } } },
      { node: 'studio-b', domain: { labels: { role: 'cameras' } }, select: { group_hint: { name: 'Studio B:Camera 1' } } },
    ],
    destinations: [
      { node: 'edge-01', domain: { area: 'fast', elements: ['wall'] } },
      { node: 'edge-02', domain: { area: 'fast', elements: ['wall'] } },
    ],
  },
  {
    name: 'talkback',
    sources: [{ node: 'studio-a', domain: { name: { area: 'media', elements: ['audio'] } }, select: { all: true } }],
    destinations: [{ node: 'edge-02', domain: { area: 'fast', elements: ['ingest'] } }],
  },
  {
    name: 'archive',
    sources: [{ node: 'studio-a', domain: cameras, select: { group_hint: { name: 'Studio A:Camera 2' } } }],
    destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['capture'] }, disabled: true }],
  },
  {
    name: 'future',
    sources: [{ node: 'studio-a', domain: cameras, select: { group_hint: { name: 'Studio A:Camera 9' } } }],
    destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['wall'] } }],
  },
]

/** `wall` expands 2 flows × 2 destinations plus 1 flow × 2; `talkback` one idle flow. */
const EXPECTED_PATHS = 7

async function reset(): Promise<void> {
  await api.applyNamespace({ name: NS, paths: 'exclusive' })
  const existing = await api.requests(NS)
  for (const request of existing.requests) await api.deleteRequest(NS, request.name)
  for (const spec of FIXTURE) await api.applyRequest(NS, spec)

  await until(async () => {
    const held = (await api.paths()).paths.filter((path) =>
      path.requests.some((id) => id.startsWith(`${NS}/`)),
    )
    return held.length === EXPECTED_PATHS
  })
}

describe('the matrix over an exclusive namespace', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    await reset()
    wrapper = await mountAt(`/ns/${NS}`)
  })

  afterAll(() => wrapper?.unmount())

  const columnHeads = () => wrapper.findAll('.col-head th:not(.corner)')
  const rowHeads = () => wrapper.findAll('.rowhead')
  const columnOf = (domain: string) => columnHeads().findIndex((th) => th.text().includes(domain))
  const rowOf = (text: string) => rowHeads().findIndex((th) => th.text().includes(text))
  const cellAt = (row: number, column: number) => wrapper.findAll('tbody tr')
    .filter((tr) => tr.find('.rowhead').exists())[row]!
    .findAll('.cell')[column]!

  // The mode decides which of two screens this is, so it is on the screen rather than on a hover.
  it('says which namespace and which mode', () => {
    expect(wrapper.text()).toContain('⟨exclusive⟩')
    expect(wrapper.find('.counts').text()).toMatch(/\d+ requests · \d+ paths · \d+ not active/)
  })

  // Both axes are read out of the requests. Neither is a handle on an object: a row is a pair of
  // selectors and a column is a domain that did not exist until a request named it.
  it('reads both axes out of the requests, banded by node in both directions', () => {
    expect(wrapper.findAll('.col-band .node').map((node) => node.text()))
      .toEqual(['archive-01', 'edge-01', 'edge-02'])
    expect(wrapper.findAll('.band .node').map((node) => node.text()))
      .toEqual(['studio-a', 'studio-b'])

    expect(columnHeads().map((th) => th.find('.mono').text()))
      .toEqual(['bulk/capture', 'fast/wall', 'fast/ingest', 'fast/wall'])
    // A row is what the operator wrote, so a label selector reads as one rather than as the domains
    // it happens to match today — it is a standing query and a domain labelled tomorrow joins it.
    expect(rowHeads().some((th) => th.text().includes('{role=cameras}'))).toBe(true)
  })

  // Without this the axes would be derived from routes that are currently on, so switching one off
  // would delete the row and the column it lived on and the board would rearrange itself.
  it('keeps a parked destination on the axes and draws it dark', () => {
    const column = columnOf('bulk/capture')
    expect(column).toBeGreaterThanOrEqual(0)
    expect(columnHeads()[column]!.classes()).toContain('parked')
    // Counts the legs it has rather than claiming a source it is not carrying.
    expect(columnHeads()[column]!.text()).toContain('0 sources')

    const cell = cellAt(rowOf('Studio A:Camera 2'), column)
    expect(cell.classes()).toContain('parked')
    expect(cell.text()).toContain('DISABLED')
    expect(cell.text()).toContain('parked')
  })

  // A request is sources × destinations. Its rows need not be adjacent — rows sort by node — so the
  // rectangle is an accent and a badge rather than a border whose geometry would depend on the sort.
  it('draws a two-source request as a rectangle over all four of its cells', () => {
    const accented = wrapper.findAll('.cell').filter((cell) =>
      cell.classes().some((name) => name.startsWith('acc-')),
    )
    expect(accented).toHaveLength(4)

    const rows = rowHeads().filter((th) => th.classes().some((name) => name.startsWith('acc-')))
    expect(rows).toHaveLength(2)
    for (const row of rows) {
      const badge = row.find('.badge')
      expect(badge.text()).toBe('⧉2 srcs')
      expect(badge.attributes('style')).toContain('visible')
    }
  })

  // Accepted, not yet satisfiable: pre-provisioning replication for a camera that is not live yet
  // costs nothing and is explicitly supported, so the click stands and the cell stays quiet.
  it('lights a pairing that has matched nothing with its source state', () => {
    const cell = cellAt(rowOf('Studio A:Camera 9'), columnOf('fast/wall'))
    expect(cell.text()).toContain('WAITING')
    expect(cell.find('.line.dim').text()).toBe('·')
  })

  // Geometry is a correctness property, not styling: cells in a row share a height, so prose in one
  // resizes the whole grid under the pointer. The reason is in the tooltip, where length costs
  // nothing — and the tooltip is where it must be found.
  it('gives every cell two fixed-shape lines and no prose', () => {
    for (const cell of wrapper.findAll('.cell')) {
      expect(cell.findAll('.line').length).toBeLessThanOrEqual(2)
      expect(cell.text().length).toBeLessThan(24)
    }

    const lit = wrapper.findAll('.cell').filter((cell) => cell.classes().includes('lit'))
    expect(lit.length).toBe(7)
    expect(lit.every((cell) => cell.findAll('.line').length === 2)).toBe(true)
    // And the reason really is *in* the tooltip: a lit cell's title carries a line per path —
    // shortened id, shortened flow, state — which is the detail two fixed lines cannot hold. Matched
    // on that shape rather than on a separator character, which is prose and has moved once already.
    expect(lit.some((cell) => /\w{8}… \w{8}… [A-Z]+/.test(cell.attributes('title') ?? ''))).toBe(true)
  })

  // Nothing conditional may resize what surrounds it: every one of these is always in the DOM and
  // merely hidden, because a table shares heights across a row and widths down a column.
  it('reserves the space for controls that do not apply', () => {
    expect(wrapper.findAll('.badge')).toHaveLength(rowHeads().length)
    expect(wrapper.findAll('.col-head .foreign')).toHaveLength(columnHeads().length)
    expect(wrapper.findAll('.lease').length).toBeGreaterThan(0)

    const hidden = wrapper.findAll('.badge').filter((badge) =>
      (badge.attributes('style') ?? '').includes('hidden'),
    )
    expect(hidden.length).toBeGreaterThan(0)
  })

  // Namespaces partition requests, not destinations. `k8s` lands a different flow in the same
  // domain, which shares no path with anything here — and is what makes "emptying this column
  // empties the domain" false.
  it('names another namespace writing into a column', () => {
    const shared = columnHeads()[columnOf('fast/ingest')]!.find('.foreign')
    expect(shared.text()).toContain('k8s')
    expect(shared.attributes('style')).toContain('visible')

    const own = columnHeads()[columnOf('fast/wall')]!.find('.foreign')
    expect(own.attributes('style')).toContain('hidden')
  })

  // `PARTIAL` is the rectangle's word and never appears on a path; `DISABLED` is derived from the
  // spec. Both come from the server, because it also folds leg failures that produce no path and
  // there is nothing here to recompute them from.
  it('reports each request with the state the server gave it', () => {
    const rows = wrapper.findAll('.request')
    expect(rows.length).toBe(FIXTURE.length)

    const archive = rows.find((row) => row.text().includes('archive'))!
    expect(archive.find('.state-DISABLED').exists()).toBe(true)
    expect(archive.text()).toContain('0 paths')

    const wall = rows.find((row) => row.text().startsWith('wall'))!
    expect(wall.text()).toContain('2 sources × 2 destinations')
    expect(wall.text()).toContain('6 paths')
  })

  // The list is bounded by what the namespace holds rather than by the window, and it sits under the
  // grid — which is the thing the screen is for. So it folds, and the header keeps the count that
  // says what was folded away.
  it('folds the request list away and still says how many there are', async () => {
    const fold = () => wrapper.find('.requests .fold')

    await fold().trigger('click')
    expect(wrapper.findAll('.request')).toHaveLength(0)
    expect(wrapper.find('.requests h2').text()).toContain(`${FIXTURE.length} requests`)
    expect(fold().attributes('aria-expanded')).toBe('false')

    await fold().trigger('click')
    expect(wrapper.findAll('.request')).toHaveLength(FIXTURE.length)
  })

  // The counts sum, which is the property the exclusive partition exists to buy: every lit cell is a
  // distinct claim, so what the cells report is what the namespace holds.
  it('accounts in its cells for every path the namespace holds', () => {
    const inCells = wrapper
      .findAll('.cell .line.dim')
      .map((line) => /^(\d+) paths?$/.exec(line.text())?.[1])
      .filter((count): count is string => count !== undefined)
      .reduce((total, count) => total + Number(count), 0)

    expect(inCells).toBe(EXPECTED_PATHS)
    expect(wrapper.find('.counts').text()).toContain(`${EXPECTED_PATHS} paths`)
    expect(wrapper.find('.unplaced').exists()).toBe(false)
  })

  // Nothing in the ledger requires `shared`, and it is the plain path list `describe path` gives
  // when every in-namespace refcount is 1. One component, both modes — so it is offered here too.
  // The tabs are links now, so which screen is on is in the URL and a navigation is asynchronous
  // even when nothing guards it — waited on rather than asserted after a tick.
  it('offers the ledger beside the grid', async () => {
    await wrapper.findAll('.view').find((tab) => tab.text() === 'claims')!.trigger('click')
    await until(() => wrapper.find('.claims').exists())
    expect(wrapper.find('.matrix').exists()).toBe(false)

    await wrapper.findAll('.view').find((tab) => tab.text() === 'grid')!.trigger('click')
    await until(() => wrapper.find('.matrix').exists())
  })
})

describe('the matrix over a shared namespace', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt('/ns/k8s')
  })

  afterAll(() => wrapper?.unmount())

  // A shared namespace is **not the matrix greyed out**: two requests may hold one path, so two
  // cells would be one session and one worker pair, the counts would stop summing, and clearing
  // either would cancel a request and stop nothing while going dark exactly as if it had. The
  // control is shown and disabled rather than omitted — omission produces "where is the grid?".
  it('declines to be a grid, and says why', () => {
    const grid = wrapper.findAll('.view').find((tab) => tab.text() === 'grid')!
    // Not a link, because there is nowhere to go — a disabled anchor is still an anchor, and the
    // route it would name is one this namespace corrects away from anyway.
    expect(grid.element.tagName).toBe('SPAN')
    expect(grid.classes()).toContain('off')
    expect(grid.attributes('title')).toContain('shared namespace')

    expect(wrapper.find('.matrix').exists()).toBe(false)
    expect(wrapper.find('.claims').exists()).toBe(true)
  })
})
