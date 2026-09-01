import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { useFleetStore } from './fleet'

/** A fleet response set, so a test can change one read and leave the others alone. */
function fixture(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    '/v1/paths': { paths: [{ id: 'p1', state: 'ACTIVE', requests: ['nab/a'] }] },
    '/v1/nodes': { nodes: [{ name: 'studio-a', live: true, capabilities: {} }] },
    '/v1/requests': { requests: [{ id: 'nab/a', name: 'a', status: { state: 'ACTIVE', paths: [] } }] },
    '/v1/namespaces': { namespaces: [{ name: 'nab', paths: 'exclusive', requests: 1 }] },
    '/readyz': { status: 'ok', leader: 'replica-1' },
    ...overrides,
  }
}

let responses: Record<string, unknown>
let fail: Set<string>

function installFetch() {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input).split('?')[0]!
    if (fail.has(url)) throw new TypeError('network down')
    return new Response(JSON.stringify(responses[url] ?? {}), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  }) as typeof fetch
}

describe('fleet poll', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    responses = fixture()
    fail = new Set()
    installFetch()
  })

  afterEach(() => vi.restoreAllMocks())

  it('loads the fleet', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    expect(fleet.loaded).toBe(true)
    expect(fleet.stale).toBe(false)
    expect(fleet.nodes).toHaveLength(1)
    expect(fleet.leader).toBe('replica-1')
  })

  // "A failed poll changes nothing on screen." The banner says the view is stale and the last good
  // read stays rendered — the same discipline the agent runs on, because an unreachable server must
  // not look like an empty fleet.
  it('keeps the last good read when a poll fails', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    fail.add('/v1/paths')
    await fleet.refresh()

    expect(fleet.stale).toBe(true)
    expect(fleet.nodes).toHaveLength(1)
    expect(fleet.paths).toHaveLength(1)
    expect(fleet.lastError?.message).toContain('network down')
  })

  // A partial update would put requests from this poll beside paths from the last, and the
  // workspace joins the two. A torn snapshot renders relationships that never existed.
  it('does not apply a half-successful cycle', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    responses = fixture({
      '/v1/nodes': { nodes: [{ name: 'edge-01', live: true, capabilities: {} }, { name: 'edge-02', live: false, capabilities: {} }] },
    })
    fail.add('/v1/requests')
    await fleet.refresh()

    expect(fleet.nodes).toHaveLength(1)
    expect(fleet.nodes[0]!.name).toBe('studio-a')
  })

  it('recovers when the API comes back', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    fail.add('/v1/paths')
    await fleet.refresh()
    expect(fleet.stale).toBe(true)

    fail.clear()
    await fleet.refresh()
    expect(fleet.stale).toBe(false)
  })

  // `disabled` is omitempty, so a re-enabled destination comes back with no key at all. Anything
  // that decodes a poll *over* the previous response leaves a stale `true` and shows the leg parked
  // forever after it came back (`ui.md` §5 trap 15). Replace, never merge.
  it('replaces state rather than merging it, so a cleared omitempty flag clears', async () => {
    const parked = { id: 'nab/a', name: 'a', destinations: [{ node: 'edge-01', disabled: true }], status: { state: 'DISABLED', paths: [] } }
    const live = { id: 'nab/a', name: 'a', destinations: [{ node: 'edge-01' }], status: { state: 'ACTIVE', paths: [] } }

    responses = fixture({ '/v1/requests': { requests: [parked] } })
    const fleet = useFleetStore()
    await fleet.refresh()
    expect(fleet.requests[0]!.destinations[0]!.disabled).toBe(true)

    responses = fixture({ '/v1/requests': { requests: [live] } })
    await fleet.refresh()
    expect(fleet.requests[0]!.destinations[0]!.disabled).toBeUndefined()
  })

  // not_ready is the settling condition arriving as a status code. It is not a failure and must not
  // blank the view or raise an error surface — a restart is not a fleet-wide outage.
  it('treats a 503 not_ready as settling rather than an error', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify({ code: 'not_ready', message: 'still settling' }), { status: 503 }),
    ) as typeof fetch
    await fleet.refresh()

    expect(fleet.settling).toBe(true)
    expect(fleet.stale).toBe(false)
    expect(fleet.storeUnreachable).toBe(false)
    expect(fleet.nodes).toHaveLength(1)
  })

  // A 503 with code internal means the store is unreachable: the server is fine, its store is not.
  // The two send an operator to different places, so they are different banners.
  it('distinguishes an unreachable store from an unreachable server', async () => {
    const fleet = useFleetStore()
    await fleet.refresh()

    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify({ code: 'internal', message: 'store: context deadline exceeded' }), { status: 503 }),
    ) as typeof fetch
    await fleet.refresh()

    expect(fleet.storeUnreachable).toBe(true)
    expect(fleet.stale).toBe(true)
  })
})
