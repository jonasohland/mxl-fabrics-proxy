import { describe, expect, it } from 'vitest'
import type { LocationQuery } from 'vue-router'

import type { Facet, FilterSpec } from './filters'
import {
  facetOptions,
  filterRows,
  isEmpty,
  readSelection,
  readText,
  selectionQuery,
  toggle,
} from './filters'

interface Row {
  id: string
  state: string
  nodes: string[]
  text: string
}

const state: Facet<Row> = {
  key: 'state',
  label: 'state',
  valuesOf: (row) => [row.state],
  vocabulary: ['FAILED', 'ACTIVE'],
}

const node: Facet<Row> = {
  key: 'node',
  label: 'node',
  valuesOf: (row) => row.nodes,
}

const spec: FilterSpec<Row> = { facets: [state, node], textOf: (row) => row.text }

const rows: Row[] = [
  { id: 'a', state: 'FAILED', nodes: ['studio-a', 'edge-01'], text: 'studio-a media/cameras' },
  { id: 'b', state: 'ACTIVE', nodes: ['studio-b', 'edge-01'], text: 'studio-b media/cameras' },
  { id: 'c', state: 'ACTIVE', nodes: ['edge-01'], text: 'edge-01 fast/ingest loopback' },
]

const ids = (result: Row[]) => result.map((row) => row.id)

describe('the URL codec', () => {
  it('reads a comma-separated list', () => {
    expect(readSelection({ state: 'FAILED,ACTIVE' }, spec)).toEqual({ state: ['FAILED', 'ACTIVE'] })
  })

  it('ignores keys no facet declares', () => {
    expect(readSelection({ nonsense: 'x' }, spec)).toEqual({})
  })

  // A repeated key is not the spelling this writes, but a hand-edited URL may carry one and the
  // table must still render rather than throw on an array where it wanted a string.
  it('survives a repeated key', () => {
    expect(readSelection({ state: ['FAILED', 'ACTIVE'] }, spec)).toEqual({ state: ['FAILED'] })
  })

  it('drops empty members rather than filtering on the empty string', () => {
    expect(readSelection({ state: 'FAILED,,' }, spec)).toEqual({ state: ['FAILED'] })
  })

  it('omits an empty key entirely', () => {
    expect(selectionQuery({ state: [] }, '')).toEqual({})
  })

  it('round-trips a selection and its text', () => {
    const query = selectionQuery({ state: ['FAILED'], node: ['edge-01'] }, 'cam')
    expect(query).toEqual({ state: 'FAILED', node: 'edge-01', q: 'cam' })
    expect(readSelection(query as LocationQuery, spec)).toEqual({
      state: ['FAILED'],
      node: ['edge-01'],
    })
    expect(readText(query as LocationQuery)).toBe('cam')
  })

  it('toggles without mutating', () => {
    const before = { state: ['FAILED'] }
    expect(toggle(before, 'state', 'ACTIVE')).toEqual({ state: ['FAILED', 'ACTIVE'] })
    expect(toggle(before, 'state', 'FAILED')).toEqual({})
    expect(before).toEqual({ state: ['FAILED'] })
  })

  it('knows when nothing is filtered', () => {
    expect(isEmpty({}, '')).toBe(true)
    expect(isEmpty({ state: [] }, '')).toBe(true)
    expect(isEmpty({ state: ['FAILED'] }, '')).toBe(false)
    expect(isEmpty({}, 'cam')).toBe(false)
  })
})

describe('the predicate', () => {
  it('admits everything when nothing is selected', () => {
    expect(ids(filterRows(rows, spec, {}, ''))).toEqual(['a', 'b', 'c'])
  })

  it('ORs within a facet', () => {
    expect(ids(filterRows(rows, spec, { state: ['FAILED', 'ACTIVE'] }, ''))).toEqual(['a', 'b', 'c'])
  })

  it('ANDs across facets', () => {
    const result = filterRows(rows, spec, { state: ['ACTIVE'], node: ['studio-b'] }, '')
    expect(ids(result)).toEqual(['b'])
  })

  // The reason `valuesOf` returns a list: a path touches two nodes, and filtering on either end has
  // to find it. A facet that picked the source would hide half of what a node is running.
  it('matches a row on any of its values', () => {
    expect(ids(filterRows(rows, spec, { node: ['edge-01'] }, ''))).toEqual(['a', 'b', 'c'])
  })

  it('requires every text term, in any order', () => {
    expect(ids(filterRows(rows, spec, {}, 'cameras studio-b'))).toEqual(['b'])
    expect(ids(filterRows(rows, spec, {}, 'CAMERAS'))).toEqual(['a', 'b'])
    expect(ids(filterRows(rows, spec, {}, '   '))).toEqual(['a', 'b', 'c'])
  })
})

describe('facet options', () => {
  // The whole reason `filterRows` takes an `except`: counted after its own facet had been applied,
  // every chip but the selected one would read 0 and the row would stop being a way to move.
  it('counts against the other facets but not its own', () => {
    const options = facetOptions(rows, spec, { state: ['FAILED'] }, '', 'state')
    expect(options).toEqual([
      { value: 'FAILED', count: 1, on: true },
      { value: 'ACTIVE', count: 2, on: false },
    ])
  })

  it('narrows one facet by another', () => {
    const options = facetOptions(rows, spec, { node: ['studio-a'] }, '', 'state')
    expect(options).toEqual([{ value: 'FAILED', count: 1, on: false }])
  })

  it('counts a row once even when it holds the value twice', () => {
    const loopback: Row[] = [{ id: 'l', state: 'ACTIVE', nodes: ['edge-01', 'edge-01'], text: '' }]
    expect(facetOptions(loopback, spec, {}, '', 'node')).toEqual([
      { value: 'edge-01', count: 1, on: false },
    ])
  })

  // A selection that has stopped matching must stay on screen at zero, or there is no way to click
  // it off and the table looks permanently empty.
  it('keeps a selected value that matches nothing', () => {
    const options = facetOptions(rows, spec, { state: ['INVALID'] }, '', 'state')
    expect(options).toContainEqual({ value: 'INVALID', count: 0, on: true })
  })

  it('orders by the vocabulary, then alphabetically outside it', () => {
    const extra = [...rows, { id: 'd', state: 'PAUSED', nodes: [], text: '' }]
    expect(facetOptions(extra, spec, {}, '', 'state').map((option) => option.value))
      .toEqual(['FAILED', 'ACTIVE', 'PAUSED'])
    expect(facetOptions(rows, spec, {}, '', 'node').map((option) => option.value))
      .toEqual(['edge-01', 'studio-a', 'studio-b'])
  })
})
