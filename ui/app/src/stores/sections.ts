/**
 * Which of the workspace's stacked sections are folded away.
 *
 * The grid is the screen, and everything under it is context: the rectangle list, and the unrouted
 * strip. Both are bounded by what the fleet happens to hold rather than by anything the operator
 * wrote — a fleet with two hundred unrouted flows pushes the grid into a strip of the window and
 * makes the thing the page exists for the hardest part of it to reach. So the two sections fold, and
 * the grid gets the room back.
 *
 * **A fold hides a section's body and never its header.** A collapsed strip still says how many
 * entries are unclaimed, which is the number that decides whether it is worth opening — a control
 * that hid its own reason to exist would be one an operator folds once and never opens again. It is
 * also why folding does not stop the strip's read: the counts are live either way, and the read
 * rides the fleet poll's existing clock (`stores/read.ts`), so a folded strip costs what an open one
 * does and stays honest.
 *
 * **Not in the URL**, on `stores/current.ts`'s line: a fold changes which rows are *drawn*, not which
 * ones exist, so it is a viewing preference rather than part of what the address names. Persisted for
 * the same reason the current namespace is — an operator who folded the strip on a fleet this large
 * did not mean "until the next reload".
 */

import { defineStore } from 'pinia'
import { ref } from 'vue'

/** One key per foldable section. A string union rather than free text: these are the only two. */
export type SectionKey = 'requests' | 'unrouted'

const KEY = 'mxl.folded'

/**
 * Guarded rather than assumed, exactly as `stores/current.ts` is: the unit suite runs in node, where
 * there is no `localStorage` and a store that threw on import would take every test with it.
 */
function load(): SectionKey[] {
  try {
    const stored = globalThis.localStorage?.getItem(KEY) ?? ''
    // Filtered against the union rather than trusted: this is a value a person can hand-edit, and a
    // key nothing renders would sit in storage forever folding a section that no longer exists.
    return stored.split(',').filter((key): key is SectionKey => key === 'requests' || key === 'unrouted')
  } catch {
    return []
  }
}

function save(keys: SectionKey[]): void {
  try {
    if (keys.length) globalThis.localStorage?.setItem(KEY, keys.join(','))
    else globalThis.localStorage?.removeItem(KEY)
  } catch {
    // A browser with storage denied still folds; it just forgets between reloads.
  }
}

export const useSectionStore = defineStore('sections', () => {
  /**
   * Folded, not open: the default is everything on screen, so a fresh profile sees the whole page and
   * the storage key only ever records a choice somebody made.
   */
  const foldedKeys = ref<SectionKey[]>(load())

  const folded = (key: SectionKey): boolean => foldedKeys.value.includes(key)

  function toggle(key: SectionKey): void {
    foldedKeys.value = folded(key)
      ? foldedKeys.value.filter((entry) => entry !== key)
      : [...foldedKeys.value, key]
    save(foldedKeys.value)
  }

  return { foldedKeys, folded, toggle }
})
