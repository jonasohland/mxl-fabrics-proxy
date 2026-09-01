import { describe, expect, it } from 'vitest'

import type { DomainName, Path } from '@/api/types'

import {
  byRequest,
  declaredWarning,
  impactOf,
  labelKeyError,
  labelValueError,
  patchEmpty,
  patchOf,
  rowsError,
  rowsOf,
  type LabelRow,
} from './domainlabels'

// Mirrors of `metrics.ValidLabelName`, this server's bounds and its reserved set. The server stays
// the authority; these exist so the operator finds out while typing.
describe('a label key', () => {
  it('takes the Prometheus grammar and nothing else', () => {
    expect(labelKeyError('role')).toBeUndefined()
    expect(labelKeyError('_studio2')).toBeUndefined()
    expect(labelKeyError('2studio')).toContain('not starting with a digit')
    expect(labelKeyError('studio-a')).toContain('letters, digits and underscore')
    expect(labelKeyError('__name__')).toContain('Prometheus reserves')
  })

  // The enforced list is `metrics.WorkerLabelNames()` plus `quantile` — which is not the list
  // `ui.md` §3 gives: it names `domain_name` and omits `format` and `media_type`.
  it('refuses the keys the project sets itself', () => {
    for (const key of ['direction', 'domain', 'flow_id', 'session', 'namespace', 'format', 'media_type', 'quantile']) {
      expect(labelKeyError(key)).toContain('reserved')
    }
    expect(labelKeyError('domain_name')).toBeUndefined()
  })

  it('bounds the key and the value', () => {
    expect(labelKeyError('k'.repeat(64))).toContain('longer than 63')
    expect(labelValueError('role', 'v'.repeat(254))).toContain('longer than 253')
  })
})

// `name` is the one conventional key and the one whose *value* is constrained: it is rendered as
// the `domain_name` metric label, so it takes the domain element grammar.
describe('the name label', () => {
  it('holds its value to the element grammar', () => {
    expect(labelValueError('name', 'cameras')).toBeUndefined()
    expect(labelValueError('name', 'studio a')).toContain('letters, digits')
    expect(labelValueError('name', '..')).toContain('existing directory')
    // Any other key takes anything short enough — only `name` reaches a metric label as a value.
    expect(labelValueError('role', 'studio a')).toBeUndefined()
  })
})

describe('the patch', () => {
  const stored = { name: 'cameras', role: 'cameras' }

  it('sends the diff and not the map on screen', () => {
    const rows = rowsOf(stored)
    expect(patchOf(rows)).toEqual({})
    expect(patchEmpty(patchOf(rows))).toBe(true)

    rows[1]!.value = 'audio'
    // Only the pair that moved: re-asserting `name` would overwrite another writer's edit between
    // the read and the write, which is the lost update a patch exists to avoid.
    expect(patchOf(rows)).toEqual({ set: { role: 'audio' } })
  })

  it('removes only what the server holds', () => {
    const rows = rowsOf(stored)
    rows[0]!.removed = true
    expect(patchOf(rows)).toEqual({ remove: ['name'] })

    // A row the operator added and then discarded is nothing at all, rather than a remove for a key
    // that was never there.
    const added: LabelRow[] = [{ key: 'zone', value: 'east', removed: true }]
    expect(patchEmpty(patchOf(added))).toBe(true)
  })

  it('carries an added pair as a set', () => {
    const rows: LabelRow[] = [...rowsOf(stored), { key: 'zone', value: 'east', removed: false }]
    expect(patchOf(rows)).toEqual({ set: { zone: 'east' } })
  })

  // An empty value is a value: selectors match on equality, so `role=` is a pair a request can name.
  it('keeps an emptied value as a set rather than a remove', () => {
    const rows = rowsOf(stored)
    rows[1]!.value = ''
    expect(patchOf(rows)).toEqual({ set: { role: '' } })
  })
})

describe('the rows', () => {
  it('ignores a blank row and refuses a duplicate key', () => {
    expect(rowsError([{ key: '', value: '', removed: false }])).toBeUndefined()
    expect(rowsError([
      { key: 'role', value: 'cameras', removed: false },
      { key: 'role', value: 'audio', removed: false },
    ])).toContain('written twice')
  })

  it('reports the first thing wrong with a key or a value', () => {
    expect(rowsError([{ key: 'ro le', value: 'x', removed: false }])).toContain('letters, digits')
    expect(rowsError([{ key: 'name', value: '../etc', removed: false }])).toContain('must not begin')
  })
})

// The blast radius is read straight off the result: `stopped[]` and `started[]` are full paths, so
// `path.requests[]` — the refcount — is already there and needs no second read.
describe('the impact', () => {
  const path = (id: string, requests: string[]): Path => ({
    id,
    source: { node: 'studio-a', domain: 'media/cameras' as DomainName, flow: `${id}-flow` },
    destination: { node: 'edge-01', domain: { area: 'fast', elements: ['ingest'] } },
    state: 'ACTIVE',
    requests,
  })

  it('names the requests losing legs, biggest first', () => {
    const impact = impactOf({
      node: 'studio-a',
      domain: { area: 'media', elements: ['cameras'] },
      stopped: [path('a', ['nab/wall']), path('b', ['nab/wall', 'k8s/pod']), path('c', ['nab/wall'])],
    })
    // A path two requests hold counts for both: the refcount is what says whether anything else
    // keeps this edge alive, and a label write removes the edge rather than one claim on it.
    expect(impact.losing).toEqual([{ id: 'nab/wall', paths: 3 }, { id: 'k8s/pod', paths: 1 }])
    expect(impact.gaining).toEqual([])
    expect(byRequest([])).toEqual([])
  })
})

// A patch does not touch `declared`, which is what makes an interactive edit survive an apply — and
// the other half of that is the surprise: the file is authoritative and will write its value back.
describe('declared keys', () => {
  it('says so for a key a manifest owns, and stays quiet otherwise', () => {
    expect(declaredWarning({ set: { role: 'audio' } }, ['name', 'role'])).toContain('authoritative')
    expect(declaredWarning({ remove: ['name'] }, ['name'])).toContain('name was declared')
    expect(declaredWarning({ set: { zone: 'east' } }, ['name', 'role'])).toBeUndefined()
  })
})
