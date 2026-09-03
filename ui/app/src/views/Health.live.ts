/**
 * @vitest-environment jsdom
 *
 * The landing page against a live control plane and the fake fleet of `ui.md` §9.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

describe('landing page', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    wrapper = await mountAt('/')
  })

  afterAll(() => wrapper?.unmount())

  it('reports the leader, which is the only place the API exposes it', () => {
    expect(wrapper.text()).toMatch(/leader \S+/)
  })

  it('counts nodes registered and agents leased separately', () => {
    expect(wrapper.text()).toContain('nodes registered')
    expect(wrapper.text()).toContain('agents leased')
  })

  // status.counts omits zeros, so the full vocabulary is rendered with a floor rather than built
  // from the keys of a counts map — otherwise the row shows a gap where it should show a zero.
  it('renders the full request vocabulary including states nothing is in', () => {
    const text = wrapper.text()
    for (const state of ['WAITING', 'INVALID', 'ESTABLISHING', 'PAUSED', 'ACTIVE', 'DEGRADED', 'FAILED', 'PARTIAL', 'DISABLED']) {
      expect(text, `missing ${state}`).toContain(state)
    }
  })

  // A path is never DISABLED: a parked destination produces no path for the word to be about.
  it('offers PARTIAL and DISABLED for requests and not for paths', () => {
    const pathGroup = wrapper.findAll('.group').find((group) => group.text().startsWith('Paths'))
    expect(pathGroup, 'no Paths group').toBeTruthy()
    expect(pathGroup!.text()).not.toContain('PARTIAL')
    expect(pathGroup!.text()).not.toContain('DISABLED')
  })

  it('keeps a DISABLED request out of the not-active list and still counts it', async () => {
    const disabled = (await api.requests()).requests.filter((r) => r.status.state === 'DISABLED')
    expect(disabled.length, 'fixture has no DISABLED request').toBeGreaterThan(0)

    const attention = wrapper.find('.attention')
    for (const request of disabled) expect(attention.text()).not.toContain(request.id)
    expect(attention.text()).toContain('disabled')
  })

  // PAUSED means the fabric is fine and nobody is producing. Not an error, but not active — so it
  // belongs in the list, with its own colour rather than a red one.
  it('lists a PAUSED request as not-active, styled as its own state', async () => {
    const paused = (await api.requests()).requests.filter((r) => r.status.state === 'PAUSED')
    expect(paused.length, 'fixture has no PAUSED request').toBeGreaterThan(0)

    const row = wrapper.findAll('.attention tr').find((tr) => tr.text().includes(paused[0]!.id))
    expect(row, `no row for ${paused[0]!.id}`).toBeTruthy()
    expect(row!.find('.state-PAUSED').exists()).toBe(true)
    expect(row!.find('.state-FAILED').exists()).toBe(false)
  })

  // studio-a grants read only, so it can never be a destination. That is the first thing to check
  // behind a refused request, and no per-request view can surface it.
  it('names nodes advertising no writable area', () => {
    const note = wrapper.findAll('.notes p').find((p) => p.text().includes('no writable area'))
    expect(note, 'no writable-area note').toBeTruthy()
    expect(note!.text()).toContain('studio-a')
  })

  it('is not settling and not stale against a healthy server', () => {
    expect(wrapper.find('.banner-warn').exists()).toBe(false)
    expect(wrapper.find('.banner-bad').exists()).toBe(false)
  })

  // The picker must show every namespace's mode, always — it decides which screen the workspace is.
  it('shows each namespace with its paths mode', () => {
    const options = wrapper.findAll('select option').map((option) => option.text())
    expect(options.some((text) => text.includes('k8s') && text.includes('shared'))).toBe(true)
    expect(options.some((text) => text.includes('nab') && text.includes('exclusive'))).toBe(true)
  })
})
