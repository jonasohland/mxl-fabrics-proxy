/**
 * The wiring between an index table and its address bar.
 *
 * The route's query **is** the selection — there is no `ref` mirroring it. A second copy would have
 * to be reconciled with the URL on every arrival, and the cases where the two disagree are exactly
 * the ones this design exists to serve: a link somebody pasted, a back button, a hand-edited query.
 * Reading the route and writing the route is the whole of it.
 *
 * `replace` rather than `push`, deliberately. A filtered list has to be linkable, which `replace`
 * gives just as well; what `push` would add is a history entry per chip, so backing out of a table
 * an operator narrowed four times means pressing back five. The URL stays honest either way, and
 * the browser's back button keeps meaning *the last screen* rather than *the last click*.
 */

import { computed } from 'vue'
import type { ComputedRef } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import type { FacetGroup, FilterSpec, Selection } from '@/model/filters'
import {
  facetGroups,
  filterRows,
  isEmpty,
  readSelection,
  readText,
  selectionQuery,
  toggle,
} from '@/model/filters'

export interface Filters<T> {
  /** The rows to draw: a predicate over the live array, recomputed every poll. */
  rows: ComputedRef<T[]>
  groups: ComputedRef<FacetGroup[]>
  text: ComputedRef<string>
  filtered: ComputedRef<boolean>
  toggle: (key: string, value: string) => void
  setText: (value: string) => void
  clear: () => void
}

export function useFilters<T>(spec: FilterSpec<T>, source: () => readonly T[]): Filters<T> {
  const route = useRoute()
  const router = useRouter()

  const selection = computed<Selection>(() => readSelection(route.query, spec))
  const text = computed(() => readText(route.query))

  // The whole query, not a merge into it: every key these routes carry is a filter key, and a merge
  // would preserve a stale one that no facet claims any more.
  function write(next: Selection, nextText: string): void {
    void router.replace({ query: selectionQuery(next, nextText) })
  }

  return {
    rows: computed(() => filterRows(source(), spec, selection.value, text.value)),
    groups: computed(() => facetGroups(source(), spec, selection.value, text.value)),
    text,
    filtered: computed(() => !isEmpty(selection.value, text.value)),
    toggle: (key, value) => write(toggle(selection.value, key, value), text.value),
    setText: (value) => write(selection.value, value),
    clear: () => write({}, ''),
  }
}
