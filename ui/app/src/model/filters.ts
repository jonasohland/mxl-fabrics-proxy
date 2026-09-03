/**
 * Faceted filtering for the three index views (`ui.md` §7's `get`).
 *
 * **A filter is a predicate applied at render, never a stored subset.** The fleet is replaced
 * wholesale on every poll — `stores/fleet.ts` and `ui.md` §5 trap 15 — so a list filtered once and
 * held is a torn read wearing a filter: rows from this poll beside rows from the last, with no
 * banner to say so. Everything here is pure and takes the rows as an argument for exactly that
 * reason; the views run it inside a `computed` over the live arrays.
 *
 * **The selection lives in the URL and nowhere else.** A filter changes which rows *exist*, so
 * `/paths?state=FAILED` has to render the same list for whoever opens it — which a component-local
 * `ref` cannot promise. (The current namespace goes the other way and stays out of the URL: it
 * changes only which rows are *marked*, and the page is about the same thing either way.)
 *
 * Within a facet the selected values are ORed and across facets they are ANDed, which is what a
 * chip row reads as: two states means "either", a state and a node means "both".
 */

import type { LocationQuery, LocationQueryRaw } from 'vue-router'

/** The free-text box. One key, on every table, so the address bar reads the same everywhere. */
export const TEXT_KEY = 'q'

/**
 * One dimension of a table.
 *
 * `valuesOf` returns a *list* because a row can sit under several values of one facet at once — a
 * path touches two nodes, and on a loopback they are the same node twice. Anything that returned a
 * single value here would have to pick an end, and picking the source is the bug `pathsTouching`
 * exists to avoid (`model/detail.ts`).
 */
export interface Facet<T> {
  key: string
  label: string
  valuesOf: (row: T) => readonly string[]
  /**
   * Display order when the vocabulary is closed — states, say, which have a worst-first order that
   * alphabetical would destroy. Values outside it still appear, after the ones in it.
   */
  vocabulary?: readonly string[]
  /** A class per value, so a state chip carries its state's colour. */
  chipClass?: (value: string) => string
  title?: string
}

export interface FilterSpec<T> {
  facets: readonly Facet<T>[]
  /** Everything the free-text box searches, as one string. Matched case-insensitively. */
  textOf: (row: T) => string
}

/** Which values are selected per facet key. An absent or empty entry is *no constraint*. */
export type Selection = Readonly<Record<string, readonly string[]>>

export interface FacetOption {
  value: string
  count: number
  on: boolean
}

/** One facet, ready to render: the chip row `components/Facets.vue` draws. */
export interface FacetGroup {
  key: string
  label: string
  title?: string
  options: readonly FacetOption[]
  chipClass?: (value: string) => string
}

// ---------------------------------------------------------------------------
// The URL codec
// ---------------------------------------------------------------------------

/**
 * One query value as a list. `?state=FAILED,DEGRADED` — comma-separated, because a repeated key
 * reads badly in an address bar an operator is expected to share.
 *
 * Nothing this is used for can contain a comma: state names are a closed vocabulary, node names are
 * DNS labels, and the one facet over a namespace is bound by `naming.ts`. A domain would be the
 * exception and is deliberately not a facet — it is what the text box is for.
 */
function readList(value: LocationQuery[string] | undefined): string[] {
  const raw = Array.isArray(value) ? value[0] : value
  if (typeof raw !== 'string' || raw === '') return []
  return raw.split(',').map((part) => part.trim()).filter((part) => part !== '')
}

export function readSelection<T>(query: LocationQuery, spec: FilterSpec<T>): Selection {
  const selection: Record<string, string[]> = {}
  for (const facet of spec.facets) {
    const values = readList(query[facet.key])
    if (values.length) selection[facet.key] = values
  }
  return selection
}

export function readText(query: LocationQuery): string {
  const raw = Array.isArray(query[TEXT_KEY]) ? query[TEXT_KEY][0] : query[TEXT_KEY]
  return typeof raw === 'string' ? raw : ''
}

/**
 * The selection as a query, **omitting every empty key**.
 *
 * The same instinct as `disabled` being `omitempty` and as un-parking deleting the key rather than
 * writing `false` (`ui.md` §5 trap 15): a URL carrying `?state=` says something happened here, and
 * an unfiltered list should be spelled one way rather than two.
 */
export function selectionQuery(selection: Selection, text: string): LocationQueryRaw {
  const query: LocationQueryRaw = {}
  for (const [key, values] of Object.entries(selection)) {
    if (values.length) query[key] = values.join(',')
  }
  if (text !== '') query[TEXT_KEY] = text
  return query
}

/** Add a value to a facet, or take it out if it is already there. Never mutates. */
export function toggle(selection: Selection, key: string, value: string): Selection {
  const current = selection[key] ?? []
  const next = current.includes(value)
    ? current.filter((entry) => entry !== value)
    : [...current, value]

  const result: Record<string, readonly string[]> = { ...selection }
  if (next.length) result[key] = next
  else delete result[key]
  return result
}

export function isEmpty(selection: Selection, text: string): boolean {
  return text === '' && Object.values(selection).every((values) => values.length === 0)
}

// ---------------------------------------------------------------------------
// The predicate
// ---------------------------------------------------------------------------

/**
 * Every term must match, in any of the row's text. Split on whitespace so `edge cam` finds a row
 * carrying both words without either having to be typed in the order the row happens to render.
 */
function matchesText(text: string, haystack: string): boolean {
  const terms = text.toLowerCase().split(/\s+/).filter((term) => term !== '')
  if (terms.length === 0) return true
  const lower = haystack.toLowerCase()
  return terms.every((term) => lower.includes(term))
}

function matchesFacet<T>(row: T, facet: Facet<T>, selected: readonly string[]): boolean {
  if (selected.length === 0) return true
  const values = facet.valuesOf(row)
  return selected.some((value) => values.includes(value))
}

/**
 * The rows a table should draw.
 *
 * `except` drops one facet from the predicate, which is what {@link facetOptions} counts against —
 * a chip row whose counts were computed after its own facet had been applied would show `1` beside
 * the state you picked and `0` beside every other, and stop being a way to move between them.
 */
export function filterRows<T>(
  rows: readonly T[],
  spec: FilterSpec<T>,
  selection: Selection,
  text: string,
  except?: string,
): T[] {
  return rows.filter((row) => {
    if (!matchesText(text, spec.textOf(row))) return false
    return spec.facets.every(
      (facet) => facet.key === except || matchesFacet(row, facet, selection[facet.key] ?? []),
    )
  })
}

/**
 * The chips for one facet, with the count each would leave.
 *
 * Options are the values actually present among the rows the *other* facets admit, plus anything
 * currently selected — so a selection that has stopped matching anything still shows, at zero, and
 * can be clicked off. A chip row of every state in the vocabulary at zero is the other failure and
 * is noise: unlike the landing page's counts, this is a control rather than a reading, and there is
 * nothing to learn from an option that would empty the table.
 */
export function facetOptions<T>(
  rows: readonly T[],
  spec: FilterSpec<T>,
  selection: Selection,
  text: string,
  key: string,
): FacetOption[] {
  const facet = spec.facets.find((entry) => entry.key === key)
  if (!facet) return []

  const admitted = filterRows(rows, spec, selection, text, key)
  const counts = new Map<string, number>()
  for (const row of admitted) {
    // A row sitting under one value twice — a loopback path, whose two ends are one node — counts
    // once. It is one row and the chip says how many rows remain.
    for (const value of new Set(facet.valuesOf(row))) {
      counts.set(value, (counts.get(value) ?? 0) + 1)
    }
  }

  const selected = selection[key] ?? []
  for (const value of selected) if (!counts.has(value)) counts.set(value, 0)

  const order = facet.vocabulary ?? []
  return [...counts.entries()]
    .map(([value, count]) => ({ value, count, on: selected.includes(value) }))
    .sort((a, b) => {
      const rankA = order.indexOf(a.value)
      const rankB = order.indexOf(b.value)
      if (rankA !== rankB) {
        // Outside the vocabulary sorts after everything in it, then alphabetically.
        if (rankA < 0) return 1
        if (rankB < 0) return -1
        return rankA - rankB
      }
      return a.value.localeCompare(b.value)
    })
}

/** Every facet of a table, ready to render. What each view hands `components/Facets.vue`. */
export function facetGroups<T>(
  rows: readonly T[],
  spec: FilterSpec<T>,
  selection: Selection,
  text: string,
): FacetGroup[] {
  return spec.facets.map((facet) => ({
    key: facet.key,
    label: facet.label,
    ...(facet.title !== undefined ? { title: facet.title } : {}),
    ...(facet.chipClass !== undefined ? { chipClass: facet.chipClass } : {}),
    options: facetOptions(rows, spec, selection, text, facet.key),
  }))
}
