/**
 * @vitest-environment jsdom
 *
 * The event log pane against a live control plane (architecture §12.1, `docs/open-items.md` §2.11).
 *
 * **Every assertion here is about a property of the real response**, not about a fixture author's
 * idea of one — which is the whole reason this file exists beside `model/events.test.ts`. Two of
 * them were found by driving this server rather than predicted from the JSON:
 *
 * - a request's merged view returns **several entries carrying the same `seq`**, because the rings
 *   it merges number independently. A row keyed on the sequence alone silently collapses them.
 * - a `PAUSED` path arrives as `severity: info` with `reason_code: source_idle` on the ordinary
 *   fixture, so the "designed behaviour never warns" rule is not an edge case to construct — it is
 *   the first thing on screen.
 *
 * It seeds its own namespace, routed clear of every other fixture's destinations: `archive-01
 * bulk/events` is written into by nothing else.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import type { EventList, Path, RequestSpec } from '@/api/types'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'events'

/** Counts what the app actually asked for, so "never fetched eagerly" is an assertion. */
const requested: string[] = []

{
  const inner = globalThis.fetch
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    requested.push(typeof input === 'string' ? input : String(input))
    return inner(input, init)
  }) as typeof fetch
}

const spec: RequestSpec = {
  name: 'probe',
  sources: [
    { node: 'studio-a', domain: { name: { area: 'media', elements: ['cameras'] } }, select: { all: true } },
  ],
  destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['events'] } }],
}

async function seed(): Promise<void> {
  await api.applyNamespace({ name: NS, paths: 'exclusive', description: 'Events.live.ts' })
  await api.applyRequest(NS, spec)
}

async function teardown(): Promise<void> {
  await api.deleteRequest(NS, spec.name)
  await api.deleteNamespace(NS).catch(() => undefined)
}

/** A path this request produced, once the reconciler has computed one. */
async function seededPath(): Promise<Path> {
  let found: Path | undefined
  await until(async () => {
    const paths = (await api.paths()).paths
    found = paths.find((path) => path.requests.includes(`${NS}/${spec.name}`))
    return found !== undefined
  }, { timeoutMs: 15000 })
  return found!
}

/**
 * Wait for the entries themselves, which is **not** the same as waiting for the object.
 *
 * A read handler runs `Compute` over a fresh snapshot, so `/v1/paths` returns a path as soon as one
 * is derivable — while events are a side effect of `Apply`, which is the leader's pass and has not
 * necessarily happened yet. Waiting on the path and then reading its ring is therefore a race, and
 * this suite lost it on the first run: the path was there and its log was empty.
 *
 * It is the same lesson as `ui/app/README.md`'s "wait on the screen, not only on the server", one
 * layer down — an object existing is not evidence that anything has been recorded about it.
 */
async function ringOf(read: () => Promise<EventList>, minimum = 1): Promise<EventList> {
  let list: EventList = { events: [], next: 0 }
  await until(async () => {
    list = await read()
    return list.events.length >= minimum
  }, { timeoutMs: 15000 })
  return list
}

/**
 * Wait until the pane has drawn exactly the entries the API reports.
 *
 * Exact rather than "at least", because the count is what catches the failure this pane is most
 * exposed to: rows silently collapsing into one another. The `until` is for the poll's cadence — the
 * read here is immediate and the screen is up to one fleet tick behind it.
 */
async function rowsSettle(
  wrapper: Awaited<ReturnType<typeof mountAt>>,
  expected: number,
): Promise<void> {
  await until(() => wrapper.findAll('.ev-table tbody tr').length === expected, { timeoutMs: 15000 })
  expect(wrapper.findAll('.ev-table tbody tr')).toHaveLength(expected)
}

describe('a node’s log', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let list: EventList

  beforeAll(async () => {
    await requireServer()
    list = await ringOf(() => api.nodeEvents('edge-01'))
    wrapper = await mountAt('/nodes/edge-01')
    await until(() => wrapper.text().includes('Events'))
  })

  afterAll(() => wrapper?.unmount())

  // The entry that answers "why did every path on edge-01 re-establish at 12:04" in one line rather
  // than in fifty identical path entries.
  it('renders the registration and the inventory baseline', () => {
    expect(list.events.length, 'the fixture node has no events').toBeGreaterThan(0)

    const text = wrapper.text()
    expect(text).toContain('node registered')
    // A leader cannot honestly report a first observation as an appearance, so it states where the
    // record begins instead. Silence there is indistinguishable from a node whose flows never came.
    expect(text).toContain('inventory baseline')
  })

  it('gives every entry a row and renders the server’s own message verbatim', async () => {
    await rowsSettle(wrapper, list.events.length)
    // The message is the emitter's own prose and is better than anything this UI would write.
    for (const event of list.events) expect(wrapper.text()).toContain(event.message)
  })

  /*
   * A node's log outlives its paths and its lease, and the endpoint answers for a node with no
   * registration at all — so the pane is outside the "no such node" branch. This is the page an
   * operator lands on holding a name that is no longer in `/v1/nodes`, and it is the only place left
   * that says what happened to it.
   */
  it('is on the page for a node that is not registered', async () => {
    const gone = await mountAt('/nodes/no-such-node')
    expect(gone.text()).toContain('No node')
    expect(gone.find('.dt-section').exists()).toBe(true)
    expect(gone.text()).toContain('Events')
    gone.unmount()
  })
})

describe('a path’s log', () => {
  let path: Path
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let list: EventList

  beforeAll(async () => {
    await requireServer()
    await teardown()
    await seed()

    path = await seededPath()
    // The ring, not the path — see `ringOf`. A read derives the path; only `Apply` records it.
    list = await ringOf(() => api.pathEvents(path.id))
    wrapper = await mountAt(`/paths/${path.id}`)
    await until(() => wrapper.text().includes('Events'))
  })

  afterAll(async () => {
    wrapper?.unmount()
    await teardown()
  })

  // The path is the unit of retention, and this is what a state and a reason cannot say on their
  // own: a path that flapped for ten minutes and is ACTIVE again reports nothing about the ten.
  it('renders the transitions the status cannot show', async () => {
    expect(list.events.length, 'the seeded path has no events').toBeGreaterThan(0)
    await rowsSettle(wrapper, list.events.length)
    expect(wrapper.text()).toContain('path created')
  })

  /*
   * **The trap, against a real server.** `severity` is not the state vocabulary: designed behaviour
   * never warns, so an idle source arrives as `PAUSED` *and* `info`. A renderer that coloured the
   * row from the state would paint the fixture's ordinary quiet camera in the palette of a fault —
   * which is the board-full-of-false-faults §11 avoids twice over.
   */
  it('colours a row from its severity and never from its state', async () => {
    const paths = (await api.paths()).paths
    const paused = paths.find((entry) => entry.state === 'PAUSED')
    expect(paused, 'no path in the fixture is PAUSED').toBeTruthy()

    const pausedEvents = await api.pathEvents(paused!.id)
    const quiet = pausedEvents.events.find((e) => e.state === 'PAUSED' && e.severity === 'info')
    expect(quiet, 'the fixture has no info-severity PAUSED entry').toBeTruthy()

    const view = await mountAt(`/paths/${paused!.id}`)
    await until(() => view.text().includes('Events'))

    const row = view.findAll('.ev-table tbody tr').find((tr) => tr.text().includes('PAUSED'))!
    expect(row.classes()).toContain('ev-info')
    expect(row.classes()).not.toContain('ev-error')
    expect(row.classes()).not.toContain('ev-warn')
    // The state keeps its own colour, on its own element — two vocabularies, two decisions.
    expect(row.find('.state-PAUSED').exists()).toBe(true)

    view.unmount()
  })

  /*
   * **`has_log` is a marker, not content.** The tail is a second fetch on purpose, so that the list
   * a UI polls stays cheap exactly when things are failing — which is when it is polled hardest. A
   * pane that fetched every tail as it rendered would undo that decision from the far end, and the
   * screen would look identical while it did.
   */
  it('never fetches a worker log tail on a render or a poll', () => {
    expect(requested.length, 'nothing was requested').toBeGreaterThan(0)
    expect(requested.filter((url) => url.includes('/logs'))).toEqual([])
  })

  // The cursor exists and this UI does not use it: coalescing rewrites the last entry in place with
  // a new sequence number, so an incremental poll is handed the same row twice.
  it('reads the whole ring rather than resuming from a cursor', () => {
    const reads = requested.filter((url) => url.includes('/events'))
    expect(reads.length).toBeGreaterThan(0)
    expect(reads.filter((url) => url.includes('since='))).toEqual([])
  })
})

describe('a request’s merged log', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let list: EventList

  beforeAll(async () => {
    await requireServer()
    await teardown()
    await seed()
    await seededPath()

    // More than one, because the collision this file exists to pin needs two rings merged.
    list = await ringOf(() => api.requestEvents(NS, spec.name), 2)

    wrapper = await mountAt(`/ns/${NS}/requests/${spec.name}`)
    await until(() => wrapper.text().includes('Events'))
  })

  afterAll(async () => {
    wrapper?.unmount()
    await teardown()
  })

  /*
   * **Found by driving the server.** A request's view is its own ring merged with those of the paths
   * it currently expands onto, and each of those rings numbers from one independently — so this
   * response carries five entries all stamped `seq: 1`. Vue reuses a DOM node for a repeated key, so
   * a row keyed on the sequence alone renders one of them and silently discards the other four,
   * which is the same class of loss as the coalescing bug §12.1 records finding in a live fleet.
   */
  it('renders every entry even where several share one sequence number', async () => {
    const collisions = new Map<number, number>()
    for (const event of list.events) collisions.set(event.seq, (collisions.get(event.seq) ?? 0) + 1)
    const repeated = [...collisions.values()].filter((count) => count > 1)
    expect(repeated.length, 'the merged view has no repeated seq to test against').toBeGreaterThan(0)

    await rowsSettle(wrapper, list.events.length)
  })

  // Its own entries are what is genuinely request-scoped and has no path to live on. The aggregate
  // moving is the one every request has.
  it('carries the request’s own entries beside its paths’', () => {
    expect(list.events.some((e) => e.kind === 'request_state_changed')).toBe(true)
    expect(list.events.some((e) => e.kind === 'path_state_changed')).toBe(true)
    expect(wrapper.text()).toContain('request is')
  })

  it('does not offer a worker log on a request, which has no path to fetch one from', () => {
    expect(wrapper.findAll('.ev-log')).toHaveLength(0)
  })
})
