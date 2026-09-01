/**
 * @vitest-environment jsdom
 *
 * Labelling a domain, end to end — the second mutation, and the only control on this screen that
 * writes on the click.
 *
 * The sequence is the whole reason it is worth driving live: a **standing** label selector matches
 * nothing, a label is written, and the request it was waiting for expands — then the label is
 * removed and the media it brought up goes away again. Nothing about that is visible to a unit test,
 * because every step of it is the server's reconciler answering, and the panel's whole claim is that
 * it said what would happen *before* it happened.
 *
 * The fixture is a request nobody else's namespace can see, into a destination named clear of every
 * other file's, selecting on a label key no other fixture uses. Adding a key to a domain cannot move
 * another selector — matching is equality over the *selector's* keys, ANDed — so the shared
 * `studio-a` inventory is safe to label as long as this file removes what it wrote.
 */
import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { api } from '@/api/client'
import { installFetchBase, requireServer, until } from '@/test/live'
import { mountAt } from '@/test/mount'

installFetchBase()

const NS = 'labelled'
const REQUEST = 'zoned'

/** The key this file owns. No other fixture selects on it, so writing it moves only this request. */
const KEY = 'zone'
const VALUE = 'labelled'

const DOMAIN = { area: 'media', elements: ['cameras'] }

describe('labelling a domain', () => {
  let wrapper: Awaited<ReturnType<typeof mountAt>>

  beforeAll(async () => {
    await requireServer()
    await api.applyNamespace({ name: NS, paths: 'exclusive' })
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    await api.writeDomainLabels('studio-a', { domain: DOMAIN, patch: { remove: [KEY] } })

    // A standing query with nothing to match yet: the source names a label no domain carries, so the
    // request is accepted and expands onto nothing. That is `ui.md` §7a's middle outcome, and it is
    // the state a label write is about to resolve.
    await api.applyRequest(NS, {
      name: REQUEST,
      sources: [{
        node: 'studio-a',
        domain: { labels: { [KEY]: VALUE } },
        select: { group_hint: { name: 'Studio A:Camera 2' } },
      }],
      destinations: [{ node: 'edge-02', domain: { area: 'fast', elements: ['labelled'] } }],
    })

    wrapper = await mountAt(`/ns/${NS}`)
  })

  afterAll(async () => {
    wrapper?.unmount()
    // Whatever the assertions did, the shared inventory goes back as it was found.
    await api.writeDomainLabels('studio-a', { domain: DOMAIN, patch: { remove: [KEY] } })
    for (const request of (await api.requests(NS)).requests) await api.deleteRequest(NS, request.name)
    await api.deleteNamespace(NS)
  })

  // -- reaching into the two panels -----------------------------------------

  const source = () => wrapper.find('.ed-panel')
  const labels = () => wrapper.find('.label-editor')
  const pairs = () => labels().findAll('.ed-pair')
  const notes = () => labels().findAll('.ed-note').map((node) => node.text()).join(' ')
  const writeButton = () => labels().find('.ed-go')

  const row = (domain: string) =>
    source().findAll('.ed-row').find((entry) => entry.text().includes(domain))!

  const openLabels = async () => {
    await until(() => source().findAll('.ed-row').some((entry) => entry.text().includes('media/cameras')))
    await row('media/cameras').find('.ed-tag').trigger('click')
  }

  /** The preview is debounced and then a real dry run, so it is waited on rather than assumed. */
  const previewed = () => until(() => writeButton().attributes('disabled') === undefined)

  const storedLabels = async () => {
    const list = await api.domains('studio-a')
    return list.domains.find((entry) => entry.domain.elements.join('/') === 'cameras')?.labels ?? {}
  }

  const requestPaths = async () =>
    (await api.paths()).paths.filter((path) => path.requests.includes(`${NS}/${REQUEST}`))

  it('shows what a domain is already labelled, on its row', async () => {
    const button = wrapper.findAll('.new').find((node) => node.text().includes('source'))!
    await button.trigger('click')
    await source().findAll('.ed-field select')[0]!.setValue('studio-a')

    await until(() => source().findAll('.ed-row').some((entry) => entry.text().includes('media/cameras')))
    // Its labels and its `name` label among them — that is what an operator called this domain.
    expect(row('media/cameras').text()).toContain('name=cameras')
    expect(row('media/cameras').find('.ed-labels').attributes('title')).toContain('role=cameras')
  })

  // The panel exists for this: a label is what a source selector matches, so writing one starts
  // media one level of indirection away from a request — and the operator reads which.
  it('previews the paths a new label starts, and names the request that gains them', async () => {
    await openLabels()
    expect(labels().text()).toContain('media/cameras')

    // Nothing changed yet is not a refusal: there is simply nothing to preview and nothing to write.
    expect(writeButton().attributes('disabled')).toBeDefined()
    expect(notes()).toContain('Nothing changed yet')

    await labels().find('.ed-more .ed-act').trigger('click')
    const added = pairs()[pairs().length - 1]!
    await added.findAll('input')[0]!.setValue(KEY)
    await added.findAll('input')[1]!.setValue(VALUE)

    await previewed()
    expect(notes()).toContain('1 path start')
    expect(notes()).toContain(`gains ${NS}/${REQUEST} (1)`)
    // The path itself, off the server's own `started[]`: the edge, its state and who holds it.
    expect(labels().find('.ed-path').text()).toContain('studio-a media/cameras')
    expect(labels().find('.ed-path').text()).toContain('edge-02 fast/labelled')
  })

  it('writes it, and the standing selector picks the domain up', async () => {
    await writeButton().trigger('click')

    // The panel closes onto the list it changed, and the list is re-read rather than patched: the
    // record it wrote is the server's now.
    await until(() => !wrapper.find('.label-editor').exists())
    expect(await storedLabels()).toMatchObject({ [KEY]: VALUE, name: 'cameras' })
    // The list is emptied while it is re-read — empty-while-loading is the honest state — so the
    // wait is over the rows rather than over one of them.
    await until(() =>
      source().findAll('.ed-row').some((entry) => entry.text().includes(`${KEY}=${VALUE}`)),
    )

    await until(async () => (await requestPaths()).length === 1)
    const [path] = await requestPaths()
    expect(path!.source.node).toBe('studio-a')
    expect(path!.destination.domain).toEqual({ area: 'fast', elements: ['labelled'] })
  })

  // The other direction, and the one that is a teardown: removing a label takes the domain out of
  // the selector, so the path goes — and the preview says so with the request that loses it.
  it('previews the paths a removal stops, and stops them', async () => {
    await openLabels()
    // A stored key is a disabled input rather than text, so the row is found by its value.
    const zone = pairs().find((pair) => (pair.find('input').element as HTMLInputElement).value === KEY)!
    await zone.find('.ed-x').trigger('click')

    await previewed()
    expect(notes()).toContain('1 path stop')
    expect(notes()).toContain(`loses ${NS}/${REQUEST} (1)`)
    expect(labels().find('.ed-path').classes()).toContain('going')

    await writeButton().trigger('click')
    await until(() => !wrapper.find('.label-editor').exists())

    // The key is gone and the ones this file did not write are untouched — a patch merges against
    // nothing and removes exactly what it names.
    await until(async () => (await storedLabels())[KEY] === undefined)
    expect(await storedLabels()).toMatchObject({ name: 'cameras', role: 'cameras', studio: 'a' })
    await until(async () => (await requestPaths()).length === 0)
  })

  // Mirrored from `metrics.ValidLabelName` and this server's reserved set, so the operator finds out
  // while typing rather than on the write.
  it('refuses a key the project sets itself, before any read', async () => {
    await openLabels()
    await labels().find('.ed-more .ed-act').trigger('click')
    const added = pairs()[pairs().length - 1]!
    await added.findAll('input')[0]!.setValue('session')
    await added.findAll('input')[1]!.setValue('x')

    expect(notes()).toContain('reserved for worker metrics')
    expect(writeButton().attributes('disabled')).toBeDefined()

    await labels().find('.ed-x').trigger('click')
    expect(wrapper.find('.label-editor').exists()).toBe(false)
  })
})
