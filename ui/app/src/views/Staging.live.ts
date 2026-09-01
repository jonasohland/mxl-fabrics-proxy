/**
 * @vitest-environment jsdom
 *
 * The cell gesture, end to end: click, stage, dry-run, apply, and the server agreeing.
 *
 * This is the class of thing the live harness exists for. A unit test can prove that a park produces
 * a spec with one boolean flipped; only a real server can prove that the spec was accepted, that the
 * paths it was carrying went away, and that switching it back on brings them back — which is the
 * whole claim `disabled` is in the model to make.
 *
 * It seeds its **own** namespace and deletes it again, for the reason `Matrix.live.ts` rewrites `nab`
 * on every run: a fixture left behind by an earlier run makes the assertions a statement about the
 * store's history rather than about the rule. Its destinations are named clear of every other
 * fixture's so that nothing here changes what another file's cross-namespace assertions read.
 *
 * The five requests are the conditions the gesture has to respect:
 *
 * - `duo`, two sources over two destinations — the rectangle, and the proof that parking is a
 *   *column* operation over it rather than a notch in one cell;
 * - `solo`, sharing `duo`'s first source row: a row two requests are on, where an unlit cell has no
 *   one request for a new leg to join;
 * - `third`, whose destination gives that row an unlit cell to try it on — and which matches no flow,
 *   so it also puts a column on the axis that no path has materialised into;
 * - `ghost-a` and `ghost-b`, identical source and destination over a group hint no producer carries:
 *   §7a's bounded hole, one cell with two owners, where which leg a click means is not decidable.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import type { DomainSelector, RequestSpec } from '@/api/types'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'staged'

const cameras: DomainSelector = { name: { area: 'media', elements: ['cameras'] } }
const camera1 = { node: 'studio-a', domain: cameras, select: { group_hint: { name: 'Studio A:Camera 1' } } }

const FIXTURE: RequestSpec[] = [
  {
    name: 'duo',
    sources: [
      camera1,
      { node: 'studio-b', domain: { labels: { role: 'cameras' } }, select: { group_hint: { name: 'Studio B:Camera 1' } } },
    ],
    destinations: [
      { node: 'edge-01', domain: { area: 'fast', elements: ['one'] } },
      { node: 'edge-02', domain: { area: 'fast', elements: ['two'] } },
    ],
  },
  {
    name: 'solo',
    sources: [camera1],
    destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['three'] } }],
  },
  {
    name: 'third',
    sources: [{ node: 'studio-b', domain: cameras, select: { group_hint: { name: 'Studio B:Camera 2' } } }],
    destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['four'] } }],
  },
  ...['ghost-a', 'ghost-b'].map((name) => ({
    name,
    sources: [{ node: 'studio-a', domain: cameras, select: { group_hint: { name: 'Studio A:Camera 42' } } }],
    destinations: [{ node: 'edge-01', domain: { area: 'fast', elements: ['ghost'] } }],
  })),
]

/** `duo` takes 2 + 1 flows across two destinations, `solo` the same 2 across one. `third` matches nothing. */
const EXPECTED_PATHS = 8

/** The three paths on `duo`'s `fast/one` column — one per source flow. */
const ON_COLUMN = 3

const held = async () =>
  (await api.paths()).paths.filter((path) => path.requests.some((id) => id.startsWith(`${NS}/`)))

async function reset(): Promise<void> {
  await api.applyNamespace({ name: NS, paths: 'exclusive' })
  for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
  for (const spec of FIXTURE) await api.applyRequest(NS, spec)
  await until(async () => (await held()).length === EXPECTED_PATHS)
}

describe('staging a cell', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    await reset()
    wrapper = await mountAt(`/ns/${NS}`)
  })

  afterAll(async () => {
    wrapper?.unmount()
    for (const spec of FIXTURE) await api.deleteRequest(NS, spec.name)
    await api.deleteNamespace(NS)
  })

  const columnHeads = () => wrapper.findAll('.col-head th:not(.corner)')
  const rowHeads = () => wrapper.findAll('.rowhead')
  const columnOf = (domain: string) => columnHeads().findIndex((th) => th.text().includes(domain))
  const rowOf = (text: string) => rowHeads().findIndex((th) => th.text().includes(text))
  const cellAt = (row: number, column: number) => wrapper.findAll('tbody tr')
    .filter((tr) => tr.find('.rowhead').exists())[row]!
    .findAll('.cell')[column]!
  const cellFor = (row: string, column: string) => cellAt(rowOf(row), columnOf(column))
  const dropFor = (row: string, column: string) => wrapper.findAll('tbody tr')
    .filter((tr) => tr.find('.rowhead').exists())[rowOf(row)]!
    .findAll('.cellbox')[columnOf(column)]!
    .find('.drop-leg')

  const staged = () => wrapper.findAll('.cell').filter((cell) => cell.classes().includes('staged'))
  const bar = () => wrapper.find('.pending')
  const applyButton = () => bar().find('.go')

  /** The bar is waiting on a debounce and then on a real reconcile per staged request. */
  const previewed = () => until(() => applyButton().attributes('disabled') === undefined)

  // Both clicks the screen declines, and both are conditions the model already knows rather than
  // accidents. A control that is merely inert teaches nothing, so each says which one it is.
  it('declines the two clicks it cannot interpret, and says which', () => {
    const shared = cellFor('Studio A:Camera 1', 'fast/four')
    expect(shared.attributes('aria-disabled')).toBe('true')
    expect(shared.attributes('title')).toContain('no one request for a new leg to join')

    const contested = cellFor('Studio A:Camera 42', 'fast/ghost')
    expect(contested.attributes('aria-disabled')).toBe('true')
    expect(contested.attributes('title')).toContain('not decidable')
    // The bounded two-owner case is drawn as such: sharing markup earns its keep exactly here.
    expect(contested.classes()).toContain('duo')
  })

  // A rectangle has no notches. `disabled` is a flag on a *destination*, so one click takes out that
  // column across every row of the request — and the grid says so before it commits, from the model
  // rather than from the dry run.
  it('stages a park across the whole column of the rectangle', async () => {
    const cell = cellFor('Studio A:Camera 1', 'fast/one')
    expect(cell.attributes('title')).toContain('also parks 1 other cell')

    await cell.trigger('click')

    expect(staged()).toHaveLength(2)
    expect(cellFor('Studio A:Camera 1', 'fast/one').text()).toContain('staged')
    expect(cellFor('Studio B:Camera 1', 'fast/one').text()).toContain('park')
    // Never the request's own state and never a path count: the leg has not been written.
    expect(cellFor('Studio A:Camera 1', 'fast/one').text()).not.toContain('ACTIVE')

    // The other column of the same rectangle is untouched — this is a column operation, not a
    // request-wide one.
    expect(cellFor('Studio A:Camera 1', 'fast/two').classes()).not.toContain('staged')

    // `unplaced` is the alarm for a grid that dropped an edge, and switching a leg off is not that.
    expect(wrapper.find('.unplaced').exists()).toBe(false)

    // The header and the cells still agree: a leg staged for parking is drawn nowhere and counted
    // nowhere, so the property the exclusive partition buys survives a pending change. How many of
    // those paths actually stop is the bar's to say, and it does.
    const inCells = wrapper
      .findAll('.cell .line.dim')
      .map((line) => /^(\d+) paths?$/.exec(line.text())?.[1])
      .filter((count): count is string => count !== undefined)
      .reduce((total, count) => total + Number(count), 0)

    expect(inCells).toBe(EXPECTED_PATHS - ON_COLUMN)
    expect(wrapper.find('.counts').text()).toContain(`${EXPECTED_PATHS - ON_COLUMN} paths`)
  })

  // Nothing is written until Apply, and the bar reports what the server itself said it would do.
  it('previews the batch and names the blast radius before anything moves', async () => {
    expect(bar().exists()).toBe(true)
    expect(bar().find('.count').text()).toBe('1 change staged')

    await previewed()

    // `updated`, not a status code: an unchanged apply is still a 200, so the header is the answer.
    expect(bar().find('.outcome').text()).toBe('updated')
    expect(bar().text()).toContain(`${ON_COLUMN} of ${ON_COLUMN} paths stop`)
    expect(bar().find('.leg').text()).toContain('park')
    expect(bar().find('.leg').text()).toContain('edge-01 fast/one')

    // Still nothing on the server.
    expect((await api.request(NS, 'duo')).destinations.every((d) => d.disabled === undefined)).toBe(true)
  })

  // Staging is the confirmation, so discarding has to be as reachable as applying.
  it('undoes a staged cell on a second click', async () => {
    await cellFor('Studio A:Camera 1', 'fast/one').trigger('click')
    expect(staged()).toHaveLength(0)
    expect(bar().exists()).toBe(false)

    await cellFor('Studio A:Camera 1', 'fast/one').trigger('click')
    await previewed()
    expect(staged()).toHaveLength(2)
  })

  // The claim `disabled` exists to make: the entry stays in the spec and keeps its column, and the
  // media on it stops.
  it('applies, parks the leg, and keeps the column on the axis', async () => {
    await applyButton().trigger('click')
    await until(async () => (await held()).length === EXPECTED_PATHS - ON_COLUMN)

    const applied = await api.request(NS, 'duo')
    expect(applied.destinations).toHaveLength(2)
    expect(applied.destinations.find((d) => d.node === 'edge-01')!.disabled).toBe(true)
    expect(applied.destinations.find((d) => d.node === 'edge-02')!.disabled).toBeUndefined()

    // The staged set is spent, and the grid has drawn the applied state without being told twice.
    expect(bar().exists()).toBe(false)
    await until(() => cellFor('Studio A:Camera 1', 'fast/one').classes().includes('parked'))
    expect(columnHeads()[columnOf('fast/one')]!.classes()).toContain('parked')
    expect(cellFor('Studio A:Camera 1', 'fast/one').text()).toContain('DISABLED')
  })

  // `×` is offered over any leg the screen can attribute to one request, and the cost goes in the
  // title rather than into a gate: requiring the leg to be dark on the server first meant an apply
  // cycle in the middle of one intent, purely to reach a control. What the gate protected is kept by
  // reading the **stored** flag for the wording — a leg only *staged* for parking still has its paths
  // up, so it must not be described as parked and safe.
  it('offers the leg × on a live leg and names what it stops', async () => {
    // The applied park from the previous test is still in place, so this leg is dark on the server.
    expect(cellFor('Studio A:Camera 1', 'fast/one').classes()).toContain('parked')
    expect(dropFor('Studio A:Camera 1', 'fast/one').attributes('title')).toContain('nothing stops')

    // A live one is offered too, and says what it costs instead of being made to look safe.
    const live = () => dropFor('Studio A:Camera 1', 'fast/two')
    expect(live().exists()).toBe(true)
    expect(live().attributes('title')).toContain('which stop')

    // Staged dark is not dark. The park has not been written, so the paths are still up and the
    // wording must not change.
    await cellFor('Studio A:Camera 1', 'fast/two').trigger('click')
    expect(cellFor('Studio A:Camera 1', 'fast/two').text()).toContain('staged')
    expect(live().attributes('title')).toContain('which stop')
    await cellFor('Studio A:Camera 1', 'fast/two').trigger('click')
  })

  // Un-parking deletes the key rather than writing `disabled: false`: the flag is omitempty and the
  // zero value is the one that keeps media running, so a spec carrying `false` is not identical to
  // one that was never parked.
  it('switches the leg back on without leaving a false behind', async () => {
    await cellFor('Studio A:Camera 1', 'fast/one').trigger('click')
    expect(cellFor('Studio A:Camera 1', 'fast/one').text()).toContain('enable')

    await previewed()
    expect(bar().text()).toContain(`${ON_COLUMN} paths start`)

    await applyButton().trigger('click')
    await until(async () => (await held()).length === EXPECTED_PATHS)

    const applied = await api.request(NS, 'duo')
    const entry = applied.destinations.find((d) => d.node === 'edge-01')!
    expect('disabled' in entry).toBe(false)
  })

  // The column's × is the second honest meaning of "remove this destination": a domain several
  // requests write into is ordinary fan-in, so one takes the leg out of the request in front of you
  // and the other takes it out of all of them. It reaches the legs the cell click cannot, which is
  // what makes it the answer for a contested cell rather than a bulk convenience.
  it('takes a destination out of every request that names it', async () => {
    const ghost = () => columnHeads()[columnOf('fast/ghost')]!.find('.drop-column')
    expect(ghost().attributes('style')).toContain('visible')
    expect(ghost().attributes('title')).toContain('2 requests')
    // No producer carries `Studio A:Camera 42`, so this column has never materialised.
    expect(ghost().attributes('title')).toContain('nothing stops')

    // The cell itself is shared, so which leg a click means is not decidable and it is declined.
    await cellFor('Studio A:Camera 42', 'fast/ghost').trigger('click')
    expect(bar().exists()).toBe(false)

    await ghost().trigger('click')
    expect(staged()).toHaveLength(0) // the column is gone from the axis, not marked on it
    expect(bar().findAll('.entry')).toHaveLength(2)
    expect(bar().text()).toContain('delete')
    expect(bar().text()).toContain('no path changes')

    await applyButton().trigger('click')
    await until(async () => (await api.requests(NS)).requests.length === FIXTURE.length - 2)
    expect(columnOf('fast/ghost')).toBe(-1)
  })

  // The row's × is the one that stays destructive, because there is no flag on a source and so no
  // dark state to require first. It says so rather than being made to look safe.
  it('removes a source and says what it stops', async () => {
    const row = () => rowHeads()[rowOf('Studio A:Camera 1')]!.find('.drop-row')
    // Shared by `duo` and `solo`: which one to remove it from is not decidable here.
    expect(row().attributes('aria-disabled')).toBe('true')
    expect(row().attributes('title')).toContain('not decidable')

    const only = () => rowHeads()[rowOf('Studio B:Camera 1')]!.find('.drop-row')
    expect(only().attributes('aria-disabled')).toBe('false')
    expect(only().attributes('title')).toContain('which stop')

    await only().trigger('click')
    await previewed()
    expect(bar().text()).toContain('remove')
    // `duo` keeps its other source, so this is an update rather than a delete.
    expect(bar().find('.outcome').text()).toBe('updated')
    expect(bar().text()).toContain('2 of 2 paths stop')

    await applyButton().trigger('click')
    await until(async () => (await api.request(NS, 'duo')).sources.length === 1)
    // Waited on the screen rather than only on the server: the apply refreshes the fleet after the
    // POST returns, so the row is still drawn for however long that read takes and asserting it away
    // on the strength of the API's answer alone is a race this suite has lost once.
    await until(() => rowOf('Studio B:Camera 1') === -1)
  })
})
