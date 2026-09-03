import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { useSectionStore } from './sections'

/**
 * The suite runs in node, where there is no `localStorage` — which is the store's guarded-absent case
 * and also half of what it does, so it is installed rather than skipped. Same shape as
 * `api/auth.test.ts`'s, for the same reason.
 */
function installStorage(initial: Record<string, string> = {}) {
  const values = new Map<string, string>(Object.entries(initial))
  Object.defineProperty(globalThis, 'localStorage', {
    value: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => void values.set(key, value),
      removeItem: (key: string) => void values.delete(key),
    },
    configurable: true,
  })
  return values
}

/** The store reads storage when it is created, so every case builds a fresh pinia after installing. */
function store(initial?: Record<string, string>) {
  installStorage(initial)
  setActivePinia(createPinia())
  return useSectionStore()
}

describe('folded sections', () => {
  beforeEach(() => setActivePinia(createPinia()))

  // Everything on screen until somebody says otherwise: the key only ever records a choice, so a
  // fresh profile can never open the workspace with the strip already hidden.
  it('starts with nothing folded', () => {
    const sections = store()
    expect(sections.folded('requests')).toBe(false)
    expect(sections.folded('unrouted')).toBe(false)
  })

  it('folds and unfolds one section without touching the other', () => {
    const sections = store()

    sections.toggle('unrouted')
    expect(sections.folded('unrouted')).toBe(true)
    expect(sections.folded('requests')).toBe(false)

    sections.toggle('unrouted')
    expect(sections.folded('unrouted')).toBe(false)
  })

  // The whole reason it is a store rather than a `ref` in the view: an operator who folded the strip
  // on a fleet this large did not mean "until the next reload".
  it('persists the fold and reads it back', () => {
    const values = installStorage()
    setActivePinia(createPinia())
    useSectionStore().toggle('requests')
    expect(values.get('mxl.folded')).toBe('requests')

    setActivePinia(createPinia())
    expect(useSectionStore().folded('requests')).toBe(true)
  })

  /** Unfolding the last one clears the key rather than storing an empty string. */
  it('forgets the preference once nothing is folded', () => {
    const values = installStorage({ 'mxl.folded': 'requests' })
    setActivePinia(createPinia())
    useSectionStore().toggle('requests')
    expect(values.has('mxl.folded')).toBe(false)
  })

  // A hand-editable value: a key nothing renders would sit in storage forever folding a section that
  // does not exist, and the one beside it still has to survive.
  it('drops a stored key no section claims', () => {
    const sections = store({ 'mxl.folded': 'unrouted,ledger' })
    expect(sections.folded('unrouted')).toBe(true)
    expect(sections.foldedKeys).toEqual(['unrouted'])
  })

  // The store is imported by two views; a browser with storage denied still has to render them.
  it('works where storage is denied', () => {
    Object.defineProperty(globalThis, 'localStorage', {
      get() {
        throw new Error('denied')
      },
      configurable: true,
    })
    setActivePinia(createPinia())

    const sections = useSectionStore()
    sections.toggle('unrouted')
    expect(sections.folded('unrouted')).toBe(true)
  })
})
