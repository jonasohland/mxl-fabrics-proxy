/**
 * @vitest-environment jsdom
 *
 * The ledger against the `k8s` fixture of `ui.md` §9 — the arrangement §7c is written for: `wall`
 * takes a whole domain, `cam1-pin` pins one flow inside it so one path has two claims, `pod-abc12`
 * has two destinations, and `pod-def34` is fully parked.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

describe('ledger over a shared namespace', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    const namespaces = await api.namespaces()
    expect(
      namespaces.namespaces.find((entry) => entry.name === 'k8s')?.paths,
      'fixture needs a shared k8s namespace — see ui/app/README.md',
    ).toBe('shared')

    wrapper = await mountAt('/ns/k8s')
  })

  afterAll(() => wrapper?.unmount())

  // The mode decides which screen this is, so it is always on screen — as a chip, not a paragraph.
  it('shows the namespace mode', () => {
    expect(wrapper.text()).toContain('⟨shared⟩')
  })

  it('reports requests, paths and what is not active on one line', () => {
    expect(wrapper.find('.counts').text()).toMatch(/\d+ requests · \d+ paths · \d+ not active/)
  })

  // One row per path, so nothing is double-counted and the counts sum. That is the whole reason
  // this view exists rather than a grid.
  it('renders one row per path, not one per claim', async () => {
    const owned = (await api.paths()).paths.filter((path) =>
      path.requests.some((id) => id.startsWith('k8s/')),
    )
    await wrapper.find('.filter input').setValue(false)

    expect(wrapper.findAll('.path')).toHaveLength(owned.length)
    // The shared path has two claims but is still exactly one row.
    expect(wrapper.findAll('.claim').length).toBeGreaterThan(owned.length)
  })

  it('groups claims by destination domain, ordered by node then domain', () => {
    const heads = wrapper
      .findAll('.group-head')
      .map((head) => `${head.find('.dst-node').text()} ${head.find('.dst-domain').text()}`)

    expect(heads).toContain('edge-01 fast/ingest')
    expect(heads).toEqual([...heads].sort())
  })

  // `nab/talkback` writes a different flow into edge-02/fast/ingest. It shares no path with k8s, so
  // no refcount here mentions it — and emptying this group would still not empty the domain.
  it('names another namespace writing into the same destination domain', () => {
    const edge02 = wrapper.findAll('.group').find((group) => group.text().includes('edge-02'))!
    expect(edge02.find('.foreign').exists()).toBe(true)
    expect(edge02.find('.foreign').text()).toContain('nab')

    const edge01 = wrapper.findAll('.group').find((group) => group.text().includes('edge-01'))!
    expect(edge01.find('.foreign').exists()).toBe(false)
  })

  // A path two requests hold is refcounting working as designed in a shared namespace, so the
  // refcount is reported and nothing about it is highlighted.
  it('reports the refcount of a shared path without marking it', () => {
    const held = wrapper.findAll('.held').map((node) => node.text())
    expect(held).toContain('held by 2')
    expect(wrapper.find('.held.attention').exists()).toBe(false)
  })

  // The mark is state, in the state's own colour — PAUSED comes up calm where FAILED comes up red.
  it('marks paths that are not active, in their own state colour', () => {
    const marked = wrapper.findAll('.path.attention')
    expect(marked.length).toBeGreaterThan(0)
    for (const path of marked) {
      expect(path.find('.state-ACTIVE').exists()).toBe(false)
    }
    expect(wrapper.findAll('.path.mark-PAUSED').length).toBeGreaterThan(0)
  })

  // The whole answer to "why do I have two of these", on adjacent lines.
  it('names both claims with their source entry and selector', () => {
    const path = wrapper.findAll('.path').find((node) => node.text().includes('held by 2'))!
    const claims = path.findAll('.claim').map((node) => node.text())

    expect(claims).toHaveLength(2)
    expect(claims.some((text) => text.includes('k8s/cam1-pin') && text.includes('flow 5592a23b…'))).toBe(true)
    expect(claims.some((text) => text.includes('k8s/wall') && text.includes('all flows'))).toBe(true)
    expect(claims.every((text) => text.includes('sources[0]'))).toBe(true)
  })

  // Sole is the cancellation preview, standing rather than summoned by a dialog. A request with no
  // sole paths is carrying nothing, which has no other symptom anywhere.
  it('reports sole and shared, and flags a request that rides along', () => {
    const rows = wrapper.findAll('.request')
    const pin = rows.find((row) => row.text().includes('cam1-pin'))!
    const wall = rows.find((row) => row.text().includes('wall'))!

    expect(pin.text()).toContain('0 sole')
    expect(pin.find('.rides').exists()).toBe(true)
    expect(wall.find('.rides').exists()).toBe(false)
    expect(wall.text()).toMatch(/[1-9]\d* sole/)
  })

  // A parked leg produces no path, so the rectangle is the only place it can be drawn — and it must
  // not look like a leg nobody ever wrote.
  it('draws a fully parked request as DISABLED with its rectangle open', () => {
    const parked = wrapper.findAll('.request').find((row) => row.text().includes('pod-def34'))!

    expect(parked.find('.state-DISABLED').exists()).toBe(true)
    expect(parked.text()).toContain('0 path')

    const rect = parked.find('.rect')
    expect(rect.exists(), 'a parked request opens its rectangle').toBe(true)
    expect(rect.text()).toContain('archive-01')
    expect(rect.text()).toContain('parked')
  })

  // Geometry: a cell is always exactly two lines. Nothing of variable length ever goes in the box,
  // because cells in a row share a height and prose in one would resize the whole row.
  it('gives every rectangle cell exactly two lines and no reason prose', () => {
    for (const cell of wrapper.findAll('.rect .cell')) {
      expect(cell.findAll('.line')).toHaveLength(2)
      expect(cell.text().length).toBeLessThan(30)
    }
  })

  // Opens summarised, in the landing page's idiom — the reason this view scales where a grid does
  // not. The filter is what restores the rest.
  it('filters to what needs reading and restores on demand', async () => {
    const input = wrapper.find('.filter input')
    await input.setValue(true)
    const filtered = wrapper.findAll('.path')

    await input.setValue(false)
    expect(wrapper.findAll('.path').length).toBeGreaterThanOrEqual(filtered.length)

    // Everything the filter keeps is something to look at, and nothing it keeps is active.
    await input.setValue(true)
    for (const path of wrapper.findAll('.path')) {
      expect(path.find('.state-ACTIVE').exists()).toBe(false)
    }
  })
})

describe('ledger over an exclusive namespace', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt('/ns/nab')
  })

  afterAll(() => wrapper?.unmount())

  // Nothing in the ledger requires `shared`: inside an exclusive namespace every in-namespace
  // refcount is 1 and the claims list degenerates into the plain path list `describe path` gives.
  // One component, both modes.
  it('renders as a plain path list', () => {
    expect(wrapper.text()).toContain('⟨exclusive⟩')
    for (const held of wrapper.findAll('.held')) {
      expect(held.text()).toBe('held by 1')
    }
  })
})
