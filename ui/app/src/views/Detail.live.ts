/**
 * @vitest-environment jsdom
 *
 * The six detail views against a live control plane and the fake fleet of `ui.md` §9.
 *
 * Most of what these assert is a join over data the poll already has, and the reason they are here
 * rather than in a hermetic test is the same reason the rest of the live suite exists: the joins are
 * against a *real* reconciler's output, where a path's holder list, a request's per-source breakdown
 * and a session's missing endpoints are what the server actually emits rather than what a fixture
 * author believed it emits.
 *
 * The one thing this file writes is its own namespace, `detail`, and it writes it to produce a
 * condition no read-only fixture contains: **a request listing a path it does not hold** (`ui.md` §5
 * trap 14). It is routed clear of every other fixture — `archive-01 bulk/detail` is written into by
 * nothing else — so nothing it does moves another file's cross-namespace assertions.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import type { Path, Request, RequestSpec } from '@/api/types'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'detail'

/** The exclusive namespace and the two requests that contest one path. */
async function seed(): Promise<void> {
  await api.applyNamespace({ name: NS, paths: 'exclusive', description: 'Detail.live.ts' })

  const spec = (name: string): RequestSpec => ({
    name,
    sources: [{ node: 'studio-b', domain: { name: { area: 'media', elements: ['cameras'] } }, select: { all: true } }],
    destinations: [{ node: 'archive-01', domain: { area: 'bulk', elements: ['detail'] } }],
  })

  // Order matters and is the point: precedence is **recency**, so the one written second is the one
  // that loses. `primary` first, then `shadow`.
  await api.applyRequest(NS, spec('primary'))
  await api.applyRequest(NS, spec('shadow'))
}

async function teardown(): Promise<void> {
  await api.deleteRequest(NS, 'shadow')
  await api.deleteRequest(NS, 'primary')
  // Refused with 409 while any request references it, which is why the requests go first.
  await api.deleteNamespace(NS).catch(() => undefined)
}

describe('node detail', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt('/nodes/edge-01')
  })

  afterAll(() => wrapper?.unmount())

  // The two grants are the whole of this project's authority over that node's filesystem, and a node
  // granting `write` on nothing is not a destination at all — the first thing to check behind a
  // refused request.
  it('names each area with its two grants', () => {
    const text = wrapper.text()
    expect(text).toContain('Areas')
    expect(text).toMatch(/media\s*read\b/)
    expect(text).toMatch(/fast\s*read\+write/)
  })

  it('reports liveness from the lease and labels last_seen as the lease, not a heartbeat', () => {
    expect(wrapper.text()).toContain('leased by')
    // Never rendered as staleness: a heartbeat renews the TTL and deliberately writes nothing, so a
    // healthy node can show this an hour ago.
    expect(wrapper.text()).toContain('lease acquired')
    expect(wrapper.text()).not.toContain('last seen')
  })

  // The read the fleet poll does not make. A domain replication writes into is listed like any
  // other — a place, not a direction.
  it('lists the domains this node is observing, with their labels', async () => {
    await until(() => wrapper.text().includes('media/local'))
    expect(wrapper.text()).toContain('role=onward')
  })

  it('gives every path touching the node a row with this node’s role in it', async () => {
    const paths = (await api.paths()).paths
    const touching = paths.filter((p) => p.source.node === 'edge-01' || p.destination.node === 'edge-01')
    expect(touching.length, 'fixture has no path touching edge-01').toBeGreaterThan(0)

    // Selected by its `role` column rather than as "the last table on the page", which it stopped
    // being when the event log pane landed under it. A positional selector on a view that grows
    // sections is a test that fails for the next author rather than for a bug.
    const table = wrapper.findAll('.dt-table').find((el) => el.find('thead').text().includes('role'))
    expect(table, 'no table on the node view has a role column').toBeTruthy()

    const rows = table!.findAll('tbody tr')
    expect(rows).toHaveLength(touching.length)
    expect(rows[0]!.text()).toMatch(/initiator|target/)
  })
})

describe('domain detail', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt('/nodes/studio-a/domains/media/cameras')
    await until(() => wrapper.text().includes('Flows'))
  })

  afterAll(() => wrapper?.unmount())

  it('is addressed as <node>:<area>/<elements> and says what it is labelled', () => {
    expect(wrapper.text()).toContain('studio-a:media/cameras')
    expect(wrapper.text()).toContain('cameras')
    expect(wrapper.text()).toContain('studio')
  })

  // Coarse and hysteretic on purpose — never a head index. A flow nobody is producing into is not
  // an error, and the fixture has one.
  it('separates producing from idle, and says which flows this node writes itself', async () => {
    const domains = await api.domains('studio-a')
    const info = domains.domains.find((d) => d.domain.elements.join('/') === 'cameras')!
    expect(info.flows?.length, 'fixture domain has no flows').toBeGreaterThan(0)

    const text = wrapper.text()
    expect(text).toContain('idle')
    expect(text).toContain('replicated')
  })

  it('answers both directions: what leaves here and what lands here', async () => {
    const paths = (await api.paths()).paths
    const leaving = paths.filter(
      (p) => p.source.node === 'studio-a' && p.source.domain === 'media/cameras',
    )
    expect(leaving.length).toBeGreaterThan(0)
    expect(wrapper.text()).toContain('source')
  })
})

describe('flow detail', () => {
  const FLOW = '5592a23b-0974-45bb-9388-89ea81c42537'
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt(`/flows/${FLOW}`)
    await until(() => wrapper.text().includes('Locations'))
  })

  afterAll(() => wrapper?.unmount())

  it('lists every location the ID exists — the multiplicity is the answer', async () => {
    const entries = (await api.flows({ flow: FLOW })).flows
    expect(entries.length).toBeGreaterThan(0)

    const rows = wrapper.findAll('.dt-table').at(0)!.findAll('tbody tr')
    expect(rows).toHaveLength(entries.length)
    expect(wrapper.text()).toContain('studio-a')
  })

  it('summarises the definition and shows it verbatim', () => {
    const text = wrapper.text()
    expect(text).toContain('Studio A:Camera 1')
    expect(text).toContain('video')
    // Displayed, never decoded and re-encoded into anything that goes back over the wire.
    expect(wrapper.find('.dt-raw').text()).toContain(FLOW)
  })

  it('names the paths carrying it', async () => {
    const carrying = (await api.paths()).paths.filter((p) => p.source.flow === FLOW)
    expect(carrying.length).toBeGreaterThan(0)
    for (const path of carrying) expect(wrapper.text()).toContain(path.id.slice(0, 8))
  })
})

describe('request detail', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let request: Request

  beforeAll(async () => {
    await requireServer()
    request = await api.request('nab', 'wall')
    wrapper = await mountAt('/ns/nab/requests/wall')
  })

  afterAll(() => wrapper?.unmount())

  // The breakdown to lead with when a request has several sources: "studio B is dark, studio A is
  // fine" is the answer a fan-in needs and it has no meaning in a one-source model.
  it('gives every source entry a row, spelled as the server spells its refusals', () => {
    expect(request.sources.length, 'nab/wall is not a fan-in').toBeGreaterThan(1)
    const text = wrapper.text()
    expect(text).toContain('sources[0]')
    expect(text).toContain('sources[1]')
    for (const source of request.sources) expect(text).toContain(source.node)
  })

  it('renders the server’s own state rather than folding one locally', () => {
    // `Compute` also folds leg failures, which produce no path at all and are absent from
    // status.paths[] — so a locally folded state can disagree and there is nothing to fix it with.
    expect(wrapper.find(`.state-${request.status.state}`).exists()).toBe(true)
  })

  it('renders the full counts vocabulary with a floor of zero', () => {
    const tally = wrapper.find('.tally').text()
    for (const state of ['WAITING', 'ACTIVE', 'FAILED', 'PARTIAL', 'DISABLED']) {
      expect(tally, `missing ${state}`).toContain(state)
    }
  })

  it('lists the destinations, parked ones included', async () => {
    const archive = await api.request('nab', 'archive')
    expect(archive.destinations.some((d) => d.disabled), 'nab/archive is not parked').toBe(true)

    const parked = await mountAt('/ns/nab/requests/archive')
    expect(parked.text()).toContain('parked')
    parked.unmount()
  })
})

describe('a request that lists a path it does not hold', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>
  let loser: Request

  beforeAll(async () => {
    await requireServer()
    await teardown()
    await seed()

    // The overlap is decided by the reconciler, not by the write, so wait for it rather than for
    // the POST. Recency decides: `shadow` was written second and is the one that loses.
    await until(async () => {
      loser = await api.request(NS, 'shadow')
      return loser.status.reason_code === 'namespace_overlap'
    }, { timeoutMs: 15000 })

    wrapper = await mountAt(`/ns/${NS}/requests/shadow`)
  })

  afterAll(async () => {
    wrapper?.unmount()
    await teardown()
  })

  // The sharpest trap in the API: the loser goes INVALID and **still lists the contested path with
  // the incumbent's state**, so a request carrying nothing can report {"ACTIVE": 1} in its own
  // counts. `/v1/paths` is the only statement of ownership, and this is the one screen where the
  // discrepancy is worth rendering rather than merely defending against.
  it('says so on the row, and names who holds it instead', async () => {
    expect(loser.status.state).toBe('INVALID')
    expect(loser.status.paths.length, 'the loser lists no path').toBeGreaterThan(0)

    await until(() => wrapper.text().includes('not this one'))
    expect(wrapper.text()).toContain('1 not held')
    expect(wrapper.text()).toContain(`${NS}/primary`)
  })

  it('renders the refusal verbatim rather than a word of its own', () => {
    expect(wrapper.text()).toContain(loser.status.reason)
  })

  it('shows the winner holding the same path alone', async () => {
    const winner = await mountAt(`/ns/${NS}/requests/primary`)
    await until(() => winner.text().includes('this alone'))
    expect(winner.text()).not.toContain('not this one')
    winner.unmount()
  })
})

describe('path and session, which are two views on purpose', () => {
  let path: Path
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    const paths = (await api.paths()).paths
    path = paths.find((entry) => entry.session !== undefined)!
    expect(path, 'no path in the fixture has a session').toBeTruthy()
    wrapper = await mountAt(`/paths/${path.id}`)
  })

  afterAll(() => wrapper?.unmount())

  // `requests[]` *is* the refcount, and it is the answer to "what happens if I cancel this".
  it('renders the refcount and every request holding the edge', () => {
    const text = wrapper.text()
    expect(text).toContain(`refcount ${path.requests.length}`)
    for (const holder of path.requests) expect(text).toContain(holder)
  })

  it('summarises the session and links to it rather than being it', () => {
    expect(wrapper.text()).toContain(path.session!.id)
    expect(wrapper.text()).toContain(path.session!.fabric)
    // The negotiated interface belongs to `describe session`; the path view does not carry it.
    expect(wrapper.text()).not.toContain('max message')
  })

  it('describes the session, including an end that has not reported', async () => {
    const session = await mountAt(`/sessions/${path.session!.id}`)
    const text = session.text()

    expect(text).toContain(path.id)
    expect(text).toContain('max message')
    // Absent until reported: a session in ESTABLISHING legitimately has a fabric and an interface
    // config and no endpoints at all.
    if (!path.session!.target) expect(text).toContain('not running')
    // The user API never discloses target_info, and nothing here goes looking for it.
    expect(text).not.toContain('target_info')

    session.unmount()
  })

  it('says a path that is not computed is not there, without calling it an error', async () => {
    const gone = await mountAt('/paths/00000000000000000000000000000000')
    const missing = gone.find('.dt-missing')
    expect(missing.text()).toContain('No path')
    // The absence of a banner is the assertion. There is deliberately no explanatory title here:
    // a detail view addressed by URL is routinely a thing that has gone away, and saying so plainly
    // is the whole of what this line owes an operator (`styles/detail.css`, `.dt-missing`).
    expect(gone.find('.banner-bad').exists()).toBe(false)
    gone.unmount()
  })
})

describe('reachability', () => {
  // A view nothing links to is a view nobody opens. The landing page names only what is not active,
  // and the next question is always "why" — which is this row's request, one click away.
  it('reaches a request from the landing page’s not-active list', async () => {
    const wrapper = await mountAt('/')

    const link = wrapper.find('.attention .id a')
    expect(link.exists(), 'nothing is not-active in the fixture').toBe(true)

    const href = link.attributes('href')!
    expect(href).toMatch(/^\/ns\/[^/]+\/requests\//)

    const target = await mountAt(href)
    expect(target.text()).toContain('Request')
    target.unmount()

    wrapper.unmount()
  })
})
