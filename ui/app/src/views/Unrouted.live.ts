/**
 * @vitest-environment jsdom
 *
 * The unrouted strip, end to end — and the loop it closes: *see what exists, route it*.
 *
 * The assertion worth the harness is the last one. Everything up to it could be made against a mock
 * fleet; that one cannot, because it is a statement about the strip and the reconciler agreeing —
 * **the strip is the complement of what this namespace routes**, so a flow it lists, routed through
 * the panel its own click opens, must leave it once the server has the request. Nothing in the model
 * enforces that: it falls out of asking the path list rather than evaluating selectors, and the only
 * way to know the join is right is to make the server produce the path.
 *
 * The fixture is the *fleet's* inventory rather than a namespace's, which is inherent — the strip's
 * subject is what exists, and only the question asked of it is namespace-scoped. So this writes an
 * empty namespace of its own and reads the devfleet's flows through it. It routes `edge-01
 * media/local`, which nothing else in `ui.md` §9's fixture takes as a source: `nab` and `k8s` both
 * read the studios and write into `fast/*`, so the one genuinely unclaimed entry stays unclaimed
 * whichever other suites have run.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'unrouted'

/** Named clear of every other fixture's destinations, `detail` included. */
const COLUMN = 'bulk/unrouted'

/** The one entry nothing in the fixture routes, and the one beside it that replication wrote. */
const LOCAL = '6d3f2a91'
const ONWARD = '2f9c6b18'

describe('the unrouted strip', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    await api.applyNamespace({ name: NS, paths: 'exclusive' })
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    wrapper = await mountAt(`/ns/${NS}`)
  })

  afterAll(async () => {
    wrapper?.unmount()
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    await api.deleteNamespace(NS)
  })

  const strip = () => wrapper.find('.strip')
  const rows = () => strip().findAll('.row')
  const row = (flow: string) => rows().find((entry) => entry.text().includes(flow))
  const filter = () => strip().find('.filter input')

  /** `N routed here` — the other side of the complement, and the number the last test watches. */
  const routedCount = () => Number(/(\d+) routed here/.exec(strip().find('.counts').text())![1])

  const panel = () => wrapper.find('.ed-panel')
  const selects = () => panel().findAll('.ed-field select')
  const notes = () => panel().findAll('.ed-note').map((node) => node.text()).join(' ')

  const bar = () => wrapper.find('.pending')
  const applyButton = () => bar().find('.go')

  const listed = async (flow: string) =>
    until(() => row(flow) !== undefined, { timeoutMs: 15000 })

  // The whole point: an empty namespace routes nothing, so the fleet's inventory is all still there
  // to be found — which is the state the grid alone renders as a blank screen.
  it('shows work the grid cannot, in a namespace with no requests', async () => {
    await listed(LOCAL)
    expect(strip().text()).toContain('edge-01')
    expect(strip().text()).toContain('media/local')
    expect(strip().find('.work').text()).toContain('unclaimed')
  })

  // The fleet's inventory is unbounded and this sits under the grid, so on a large fleet the strip
  // is what pushes the board off the screen. Folded, the list goes and the counts stay — the
  // unclaimed number is what says whether opening it again is worth anything, and a section that hid
  // its own reason to exist would be one an operator folds once and never opens.
  it('folds the list away and keeps the counts', async () => {
    const counts = () => strip().find('.counts').text()
    const fold = () => strip().find('.fold')
    const before = counts()

    await fold().trigger('click')
    expect(rows()).toHaveLength(0)
    expect(fold().attributes('aria-expanded')).toBe('false')
    // The filter decides which rows are listed, so it goes with them — and it is not what the counts
    // are computed from, which is what keeps a folded header true.
    expect(strip().find('.filter').exists()).toBe(false)
    expect(counts()).toBe(before)

    await fold().trigger('click')
    await listed(LOCAL)
  })

  // The attention filter defaults on, and the two accounted-for kinds are *hidden*, never dropped:
  // the toggle has to be able to say how many it is holding back.
  it('hides this project\'s own output behind the filter and counts it', async () => {
    expect(row(ONWARD)).toBeUndefined()
    expect(strip().find('.filter').text()).toContain('hidden')

    await filter().setValue(false)
    await until(() => row(ONWARD) !== undefined)

    // Legitimately a source — that is how a chain A→B→C is written — but not *unrouted*.
    expect(row(ONWARD)!.text()).toContain('written here by this project')
    expect(row(ONWARD)!.classes()).toContain('marked')
    // The mark explains itself in prose rather than by naming `self_output`: the code is the
    // server's word for it and appears on the request's exclusion list, where it can be matched on.
    // Here the question is why this row is dimmed, and the answer is a sentence.
    expect(row(ONWARD)!.find('.mark').attributes('title'))
      .toContain('own target workers')
  })

  // Namespaces partition requests, not nodes. Fleet-wide this entry would vanish from the one view
  // where routing is built; silently namespace-scoped it would read as untouched, and routing it
  // would double egress on studio-a without the screen ever having said so.
  it('names the namespace already routing an entry, rather than hiding or ignoring it', async () => {
    const held = rows().find((entry) => entry.text().includes('routed by'))
    expect(held).toBeDefined()
    expect(held!.find('.mark').attributes('title')).toContain('doubles egress')

    await filter().setValue(true)
    await until(() => rows().every((entry) => !entry.text().includes('routed by')))
  })

  // "Clicking one starts a new row pre-filled" — the same panel `+ source` opens, with its first
  // steps made and every one of them still on screen and changeable.
  it('opens the source editor on the flow\'s group, not pinned to its ID', async () => {
    await row(LOCAL)!.find('.route').trigger('click')

    expect(panel().exists()).toBe(true)
    expect((selects()[0]!.element as HTMLSelectElement).value).toBe('edge-01')

    // The group, because the group is what an operator means and it is a *standing* selection: a
    // flow the producer adds to it later joins on its own. A pin would have authored the narrowest
    // selector from the broadest gesture.
    await until(() => notes().includes('Creates 1 request'))
    expect(notes()).toContain('every flow of the group')
    expect(panel().find('.ed-note').text()).not.toContain(LOCAL)

    await panel().find('.ed-go').trigger('click')
    expect(wrapper.find('.ed-panel').exists()).toBe(false)
  })

  // The complement, closed: route the entry the strip surfaced and it stops being in the strip.
  // This is the assertion no mock can make — it needs the server to expand the request into a path.
  it('drops the entry once the fleet is carrying it', async () => {
    const draft = wrapper.findAll('.rowhead').find((head) => head.text().includes('media/local'))
    expect(draft).toBeDefined()

    const routedBefore = routedCount()

    const newButton = wrapper.findAll('.new').find((node) => node.text().includes('destination'))!
    await newButton.trigger('click')
    await selects()[0]!.setValue('archive-01')
    await panel().find('.ed-field input').setValue('unrouted')
    await panel().find('.ed-go').trigger('click')

    const rowIndex = wrapper.findAll('.rowhead').findIndex((head) => head.text().includes('media/local'))
    const columnIndex = wrapper.findAll('.col-head th:not(.corner)')
      .findIndex((th) => th.text().includes(COLUMN))
    const cell = wrapper.findAll('tbody tr')
      .filter((tr) => tr.find('.rowhead').exists())[rowIndex]!
      .findAll('.cell')[columnIndex]!
    await cell.trigger('click')

    await until(() => applyButton().attributes('disabled') === undefined)
    await applyButton().trigger('click')
    await until(async () => (await api.requests(NS)).requests.length === 1)

    // Waited on the *screen*, not on the API: `apply()` refreshes the fleet after its POST returns,
    // and the strip's own read rides that same clock — so an assertion made on the strength of the
    // server's answer alone races the render it is about.
    await until(() => row(LOCAL) === undefined, { timeoutMs: 15000 })

    // The entry did not merely stop being *rendered*: it moved to the other side of the count. An
    // assertion that only checked the row was gone would pass just as well against a strip that had
    // failed its read and drawn nothing at all.
    expect(routedCount()).toBe(routedBefore + 1)
  })
})
