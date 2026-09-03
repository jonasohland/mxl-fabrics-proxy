/**
 * The current namespace: a scope on the workspace, a **highlight** on everything else.
 *
 * This exists because fleet health became a page of its own. It used to be the namespace picker's
 * null option — `⟨fleet health⟩` was how an operator *left* a namespace — and once leaving has its
 * own destination the picker stops being a navigation escape hatch and becomes what its label always
 * claimed: which namespace you are working in.
 *
 * So there is one rule. **The picker sets the current namespace.** On a namespace-scoped route the
 * current namespace *is* the URL, so setting it navigates; on a fleet-wide one it only re-marks, and
 * must not yank an operator off the list they are reading onto a grid they did not ask for. That
 * looks like two behaviours and is one.
 *
 * **It is deliberately not in the URL**, which is the opposite of what `model/filters.ts` does with
 * a filter, and the line between them is what each changes. A filter changes which rows *exist*, so
 * `/paths?state=FAILED` must render the same list for whoever opens it. A highlight changes only
 * which rows are *marked* — the page is about the same thing either way — so it is a viewing
 * preference, and putting it in the query would also re-open the question `/paths` settled by not
 * having a namespace filter at all (a path can be claimed from several namespaces at once; that is
 * what the ledger answers, with the selector that made each claim).
 *
 * **Fleet health never takes the mark.** `views/Health.vue` says why at length and it is the trap
 * this store introduces: scoped to a namespace, "is anything wrong" answers "is anything wrong in
 * the namespace I happen to have selected", which is the wrong answer at 3am (`ui.md` §7b).
 */

import { defineStore } from 'pinia'
import { ref } from 'vue'

/**
 * Persisted, because an operator working in one namespace all day should not re-pick it after every
 * reload. Guarded rather than assumed: the unit suite runs in node, where there is no `localStorage`
 * and a store that threw on import would take every test with it.
 */
const KEY = 'mxl.namespace'

function load(): string {
  try {
    return globalThis.localStorage?.getItem(KEY) ?? ''
  } catch {
    return ''
  }
}

function save(name: string): void {
  try {
    if (name) globalThis.localStorage?.setItem(KEY, name)
    else globalThis.localStorage?.removeItem(KEY)
  } catch {
    // A browser with storage denied still works; it just forgets between reloads.
  }
}

export const useCurrentStore = defineStore('current', () => {
  /**
   * The empty string is a real state and not a missing one: *nothing marked*. A first-ever load has
   * no namespace to be in, and there is no sensible one to invent — `default` is auto-created and
   * frequently holds nothing anybody is working on.
   */
  const namespace = ref(load())

  function set(name: string): void {
    if (namespace.value === name) return
    namespace.value = name
    save(name)
  }

  return { namespace, set }
})
