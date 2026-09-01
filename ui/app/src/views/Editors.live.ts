/**
 * @vitest-environment jsdom
 *
 * The two editors, end to end: name an axis, route it, apply, and the server agreeing.
 *
 * This is the sequence the live harness exists for and the one a unit test cannot reach. The
 * prototype's own list of what its harness caught is a list of *sequences* — a dialog showing the
 * previous node's domains, two per-node reads landing out of order, a selection carried across a
 * reopen — and every one of them is a bug in the same place this file drives: a form whose contents
 * come from a read that is not the poll.
 *
 * It writes its own namespace and deletes it again, for the reason the other live suites do: a
 * fixture left behind by an earlier run makes the assertions a statement about the store's history
 * rather than about the rule. Its destinations are named clear of every other fixture's.
 *
 * The path it walks is the whole of `ui.md` §7a's authoring story:
 *
 * - a destination is named on a node that grants `write`, and the ones that cannot are shown with
 *   which of the two refusals they would be;
 * - a source is chosen node → domain → group → how much, and the group is what is picked rather than
 *   a flow;
 * - the cell where the two cross is the request, and it stages like every other cell;
 * - a second source added to that request lights its destinations at once, which is fan-in authored
 *   rather than merely rendered.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'authored'

/** Named clear of every other fixture, so nothing here moves another file's cross-namespace facts. */
const COLUMN = 'fast/authored'

describe('the editors', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    // Exclusive, always: the grid is an editor only over a namespace where two requests cannot hold
    // one path, and the API's default is `shared`.
    await api.applyNamespace({ name: NS, paths: 'exclusive' })
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    wrapper = await mountAt(`/ns/${NS}`)
  })

  afterAll(async () => {
    wrapper?.unmount()
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    await api.deleteNamespace(NS)
  })

  // -- reaching into the two panels -----------------------------------------

  const panel = () => wrapper.find('.ed-panel')
  const lists = () => panel().findAll('.ed-list')
  const selects = () => panel().findAll('.ed-field select')
  const notes = () => panel().findAll('.ed-note').map((node) => node.text()).join(' ')
  const addButton = () => panel().find('.ed-go')

  const open = async (which: 'source' | 'destination') => {
    const button = wrapper.findAll('.new').find((node) => node.text().includes(which))!
    await button.trigger('click')
  }

  /** The rectangle's own row, at the foot of the grid, which is where request-level controls live. */
  const rectangle = (name: string) =>
    wrapper.findAll('.request').find((row) => row.find('.name').text() === name)!

  /** Click the row of a list whose text contains this, once the list has been read. */
  const pick = async (list: number, text: string) => {
    await until(() => lists()[list]!.findAll('.ed-item').some((item) => item.text().includes(text)))
    const item = lists()[list]!.findAll('.ed-item').find((entry) => entry.text().includes(text))!
    await item.trigger('click')
  }

  const columnHeads = () => wrapper.findAll('.col-head th:not(.corner)')
  const rowHeads = () => wrapper.findAll('.rowhead')
  const columnOf = (domain: string) => columnHeads().findIndex((th) => th.text().includes(domain))
  const rowOf = (text: string) => rowHeads().findIndex((th) => th.text().includes(text))
  const cellFor = (row: string, column: string) => wrapper.findAll('tbody tr')
    .filter((tr) => tr.find('.rowhead').exists())[rowOf(row)]!
    .findAll('.cell')[columnOf(column)]!

  const bar = () => wrapper.find('.pending')
  const applyButton = () => bar().find('.go')
  const previewed = () => until(() => applyButton().attributes('disabled') === undefined)

  // A node with no area it grants `write` on is not a destination at all, and the two refusals are
  // different problems. Shown disabled with the reason rather than omitted: "where is studio-a?" is
  // the question omission produces.
  it('names a destination, and declines the nodes that cannot be one', async () => {
    await open('destination')

    const studio = panel().findAll('option').find((option) => option.text().startsWith('studio-a'))!
    expect(studio.attributes('disabled')).toBeDefined()
    expect(studio.text()).toContain('area_not_writable')

    await selects()[0]!.setValue('edge-02')
    // The area picker is shown even for a node with exactly one, because the area is the first
    // segment of the domain's name and omitting it is omitting half the name.
    expect(selects()[1]!.findAll('option').map((option) => option.text().trim())).toEqual(['fast'])

    await panel().find('.ed-field input').setValue('authored')
    // `area.path` is advertised for diagnostics and may be absent — the name above it is what the
    // fleet calls this domain either way.
    expect(panel().find('.ed-resolved').text()).toContain(COLUMN)

    await addButton().trigger('click')
    expect(wrapper.find('.ed-panel').exists()).toBe(false)

    // On the axis, drawn as authored-and-not-written, and discardable — there is no leg on it to be
    // dark first, which is what the other two `×`s require.
    expect(columnOf(COLUMN)).toBeGreaterThanOrEqual(0)
    const head = columnHeads()[columnOf(COLUMN)]!
    expect(head.classes()).toContain('draft')
    expect(head.find('.drop-column').attributes('style')).toContain('visible')
    expect(head.find('.drop-column').attributes('title')).toContain('nothing is written')
  })

  // A nesting name is the one destination collision that survives, and it is refused while typing
  // rather than on apply — `fast/ingest` and `bulk/ingest` are simply two domains.
  it('refuses a name that nests with a column already there', async () => {
    await open('destination')
    await selects()[0]!.setValue('edge-02')
    await panel().find('.ed-field input').setValue('authored/deeper')

    expect(panel().find('.ed-fail').text()).toContain('nests')
    expect(panel().find('.ed-fail').text()).toContain('domain_name_in_use')
    expect(addButton().attributes('disabled')).toBeDefined()

    await panel().find('.ed-x').trigger('click')
  })

  // The finding the prototype's harness paid for: a list belonging to a node the operator is no
  // longer looking at is not even obviously stale, because domain names repeat across nodes.
  it('reads one node\'s domains and does not carry them onto another', async () => {
    await open('source')
    // The panel opens on the first node with a readable area, which is not necessarily one with
    // anything on it — a node with no observed or labelled domains says so rather than looking
    // broken, and choosing is the operator's first move either way.
    await selects()[0]!.setValue('studio-a')
    await until(() => lists()[0]!.text().includes('media/cameras'))

    await selects()[0]!.setValue('edge-01')
    await until(() => lists()[0]!.text().includes('media/local'))
    expect(lists()[0]!.text()).not.toContain('media/cameras')

    await selects()[0]!.setValue('studio-a')
    await until(() => lists()[0]!.text().includes('media/cameras'))
    expect(lists()[0]!.text()).not.toContain('media/local')
  })

  // A group, then how much of it — and the modes that cannot express this group say why rather than
  // being absent. A flow with no NMOS hint has no name for a group selector to match.
  it('offers only a pinned ID for flows carrying no group hint', async () => {
    await pick(0, 'media/cameras')
    await pick(1, '(no group hint)')

    // The second radio group: the first is which *domains*, this one is how much of the group.
    const modes = panel().findAll('.ed-modes')[1]!.findAll('.ed-mode')
    expect(modes[0]!.classes()).toContain('off')
    expect(modes[0]!.attributes('title')).toContain('no name for a group selector to match')
    expect(modes[2]!.classes()).not.toContain('off')
  })

  // The default and the one to make attractive: omitting the type is how a camera's video and audio
  // travel together, and it is a *standing* selection.
  it('creates a draft row from a group, named after it', async () => {
    await pick(1, 'Studio A:Camera 1')

    expect(panel().find('.ed-field input').element).toHaveProperty('value', 'studio-a-camera-1')
    expect(notes()).toContain('Creates 1 request')
    expect(notes()).toContain('2 flows')

    await addButton().trigger('click')

    // A row on the axis with no destinations: a new request has none, and lighting a cell is what
    // gives it one. Nothing is written and nothing is in the bar to write.
    const row = rowHeads()[rowOf('studio-a-camera-1')]!
    expect(row.classes()).toContain('draft')
    expect(row.find('.name').attributes('title')).toContain('not created yet')
    expect(bar().exists()).toBe(false)
  })

  // The cell where the authored row crosses the authored column is the request, and it stages like
  // every other cell on the screen — one dry run, one outcome, one Apply.
  it('routes it with a cell click and applies', async () => {
    const cell = cellFor('studio-a-camera-1', COLUMN)
    await cell.trigger('click')

    expect(cellFor('studio-a-camera-1', COLUMN).text()).toContain('staged')
    expect(cellFor('studio-a-camera-1', COLUMN).text()).toContain('add')

    await previewed()
    // `created`, from the server's own header — the status code cannot tell you this.
    expect(bar().find('.outcome').text()).toBe('created')
    expect(bar().text()).toContain('2 paths start')

    await applyButton().trigger('click')
    await until(async () => (await api.requests(NS)).requests.length === 1)

    const applied = await api.request(NS, 'studio-a-camera-1')
    expect(applied.sources).toHaveLength(1)
    expect(applied.sources[0]!.select).toEqual({ group_hint: { name: 'Studio A:Camera 1' } })
    expect(applied.destinations).toEqual([{ node: 'edge-02', domain: { area: 'fast', elements: ['authored'] } }])

    // The draft is a request now: the row and the column stay exactly where they were, and stop
    // being drawn as authored-and-unwritten.
    await until(() => rowOf('studio-a-camera-1') >= 0 && !rowHeads()[rowOf('studio-a-camera-1')]!.classes().includes('draft'))
    expect(columnHeads()[columnOf(COLUMN)]!.classes()).not.toContain('draft')
    expect(bar().exists()).toBe(false)
  })

  // Fan-in authored rather than only rendered: a request is its sources against its destinations, so
  // a row arriving lights every one of its columns at once — said before it commits.
  it('adds a second source to the request it just created', async () => {
    await open('source')
    await selects()[0]!.setValue('studio-b')
    await pick(0, 'media/cameras')
    await pick(1, 'Studio B:Camera 1')

    await selects()[1]!.setValue(`${NS}/studio-a-camera-1`)
    expect(notes()).toContain('Adds 1 source')
    expect(notes()).toContain('lights 1 cell')

    await addButton().trigger('click')

    expect(cellFor('Studio B:Camera 1', COLUMN).text()).toContain('staged')
    await previewed()
    expect(bar().find('.outcome').text()).toBe('updated')
    expect(bar().find('.leg').text()).toContain('add')
    expect(bar().text()).toContain('1 path start')

    await applyButton().trigger('click')
    await until(async () => (await api.request(NS, 'studio-a-camera-1')).sources.length === 2)
  })

  // §7a calls this "probably the most-used control on the screen", and its sentence is the whole
  // design: *copy the rectangle, change the selector, keep the destinations*. Choosing the new
  // selector is the operation — a copy that kept the sources would ask for the identical paths, which
  // exclusivity refuses the moment a producer appears.
  it('duplicates a rectangle: its destinations, a source you choose', async () => {
    await rectangle('studio-a-camera-1').find('.dup').trigger('click')

    expect(panel().find('h2').text()).toBe('duplicate')
    // A duplicate always creates a request: its destinations are the ones it is copying. Shown and
    // disabled with the reason rather than omitted, as everywhere else on this screen.
    const target = selects().find((select) => select.attributes('disabled') !== undefined)!
    expect(target.attributes('title')).toContain('always creates a request')

    await selects()[0]!.setValue('studio-a')
    await pick(0, 'media/cameras')
    await pick(1, 'Studio A:Camera 2')

    expect(notes()).toContain('Creates 1 request')
    expect(notes()).toContain("studio-a-camera-1's 1 destination")

    await addButton().trigger('click')

    // On the axis as a row of its own, with the copied destination already lit — and unwritten, so
    // the cell says so rather than borrowing the state of the request it was copied from.
    const cell = cellFor('studio-a-camera-2', COLUMN)
    expect(cell.text()).toContain('staged')
    expect(cell.text()).toContain('add')

    await previewed()
    expect(bar().find('.outcome').text()).toBe('created')
    // A duplicate authors the request whole, so there is no leg edit to list — the shape is what
    // there is to read.
    expect(bar().find('.leg').text()).toContain('create')
    expect(bar().text()).toContain('1 path start')

    await applyButton().trigger('click')
    await until(async () => (await api.requests(NS)).requests.length === 2)

    const applied = await api.request(NS, 'studio-a-camera-2')
    expect(applied.sources[0]!.select).toEqual({ group_hint: { name: 'Studio A:Camera 2' } })
    expect(applied.destinations).toEqual([{ node: 'edge-02', domain: { area: 'fast', elements: ['authored'] } }])
  })

  // §7a consequence 2's other answer to clearing one cell of a rectangle, and the one that has to be
  // a control rather than a mode of the click, because it creates a name.
  it('splits a source out, and says what both requests become', async () => {
    // Offered only where there is something to keep behind: a single-source request has nothing.
    const single = rowHeads()[rowOf('Studio A:Camera 2')]!.find('.split-row')
    expect(single.attributes('style')).toContain('hidden')
    expect(single.attributes('title')).toContain('nothing to keep behind')

    const control = rowHeads()[rowOf('Studio B:Camera 1')]!.find('.split-row')
    expect(control.attributes('style')).toContain('visible')
    await control.trigger('click')

    expect(panel().text()).toContain('1 destination')
    expect(panel().text()).toContain('keeps 1 source')
    await panel().find('.ed-field input').setValue('studio-b-only')
    await panel().find('.ed-go').trigger('click')

    // Two staged changes, and the new request is first: a create can never win a contest, so the
    // incumbent keeps the path until the update that gives it up lands — and no session is torn down.
    expect(bar().findAll('.entry')).toHaveLength(2)
    expect(bar().findAll('.entry')[0]!.find('.id').text()).toBe(`${NS}/studio-b-only`)

    await previewed()
    // The dry run reconciles a candidate fleet with *one* request changed, so the new half previews
    // as an overlap with the half that is giving the path up. Said as the hand-off it is.
    expect(bar().find('.handoff').text()).toContain(`takes 1 path over from ${NS}/studio-a-camera-1`)
    // And the other half says the same thing from its own side: the path leaves it and keeps
    // running, which is neither a stop nor a start.
    expect(bar().findAll('.entry')[1]!.text()).toContain('1 path change hands')
    expect(bar().find('.summary').text()).not.toContain('stop')

    await applyButton().trigger('click')
    await until(async () => (await api.requests(NS)).requests.length === 3)

    const split = await api.request(NS, 'studio-b-only')
    expect(split.sources).toHaveLength(1)
    expect(split.sources[0]!.node).toBe('studio-b')
    expect(split.destinations).toHaveLength(1)
    expect((await api.request(NS, 'studio-a-camera-1')).sources).toHaveLength(1)

    // The path changed hands rather than stopping: it is the same edge, and nothing about its
    // identity names the request that asked for it.
    await until(async () =>
      (await api.paths()).paths.some(
        (path) => path.source.node === 'studio-b' && path.requests.includes(`${NS}/studio-b-only`),
      ),
    )
  })

  // A POST is create-or-**update** with no create-only mode and no 409, so a name that exists is not
  // a refusal the server will make — it is a silent overwrite of somebody's running request.
  it('refuses a name the namespace already holds', async () => {
    await open('source')
    await selects()[0]!.setValue('studio-a')
    await pick(0, 'media/cameras')
    await pick(1, 'Studio A:Camera 1')

    expect(notes()).toContain('already exists')
    expect(addButton().attributes('disabled')).toBeDefined()

    // Renaming it is all it takes: `(namespace, name)` is the identity, so this is a second request.
    await panel().find('.ed-field input').setValue('studio-a-camera-1-again')
    expect(notes()).not.toContain('already exists')
    expect(addButton().attributes('disabled')).toBeUndefined()

    await panel().find('.ed-x').trigger('click')
  })
})
