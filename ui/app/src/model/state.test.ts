import { describe, expect, it } from 'vitest'

import type { PathState } from '@/api/types'
import { aggregate, byWorstFirst, displayRank, needsAttention, worst } from './state'

describe('worst', () => {
  it('is undefined for an empty set', () => {
    expect(worst([])).toBeUndefined()
  })

  it('orders INVALID above FAILED above DEGRADED', () => {
    expect(worst(['DEGRADED', 'INVALID', 'FAILED'])).toBe('INVALID')
    expect(worst(['ACTIVE', 'FAILED', 'DEGRADED'])).toBe('FAILED')
  })

  // The one deliberate exception to plain severity, mirroring the server's aggregateOrder.
  it('puts ESTABLISHING above PAUSED and ACTIVE but below WAITING', () => {
    expect(worst(['ACTIVE', 'ESTABLISHING'])).toBe('ESTABLISHING')
    expect(worst(['PAUSED', 'ESTABLISHING'])).toBe('ESTABLISHING')
    expect(worst(['ESTABLISHING', 'WAITING'])).toBe('WAITING')
  })
})

describe('aggregate', () => {
  it('is undefined for an empty set — a cell with no paths has no state to fold', () => {
    expect(aggregate([])).toBeUndefined()
  })

  it('agrees with the set when everything agrees', () => {
    expect(aggregate(['ACTIVE', 'ACTIVE'])).toBe('ACTIVE')
    expect(aggregate(['PAUSED'])).toBe('PAUSED')
  })

  it('is PARTIAL when the set disagrees and something is ACTIVE', () => {
    expect(aggregate(['ACTIVE', 'ESTABLISHING'])).toBe('PARTIAL')
    expect(aggregate(['ACTIVE', 'INVALID'])).toBe('PARTIAL')
  })

  // PARTIAL claims something is working, so it must not be said when nothing is.
  it('is never PARTIAL when nothing is ACTIVE', () => {
    expect(aggregate(['INVALID', 'WAITING'])).toBe('INVALID')
    expect(aggregate(['PAUSED', 'ESTABLISHING', 'FAILED'])).toBe('FAILED')
  })

  // The surprising half, and it follows from §7.2 rather than from taste: one bad path among
  // twenty does not condemn the other nineteen, and the top line is where promoting it would undo
  // that.
  it('outranks INVALID, FAILED and DEGRADED', () => {
    expect(aggregate(['ACTIVE', 'ACTIVE', 'INVALID'])).toBe('PARTIAL')
    expect(aggregate(['ACTIVE', 'FAILED'])).toBe('PARTIAL')
    expect(aggregate(['ACTIVE', 'DEGRADED'])).toBe('PARTIAL')
  })
})

describe('display order', () => {
  it('leads with PARTIAL and sorts DISABLED below ACTIVE', () => {
    expect(displayRank('PARTIAL')).toBeLessThan(displayRank('INVALID'))
    expect(displayRank('DISABLED')).toBeGreaterThan(displayRank('ACTIVE'))
  })

  it('sorts worst-first', () => {
    const rows = [
      { state: 'ACTIVE' as const },
      { state: 'FAILED' as const },
      { state: 'PARTIAL' as const },
      { state: 'DISABLED' as const },
    ]
    expect(rows.sort(byWorstFirst((row) => row.state)).map((row) => row.state))
      .toEqual(['PARTIAL', 'FAILED', 'ACTIVE', 'DISABLED'])
  })
})

describe('needsAttention', () => {
  // PAUSED is not an error — it is the most valuable state in the vocabulary, separating "the
  // plumbing is broken" from "the source is not producing". DISABLED is somebody's decision.
  it('excludes PAUSED and DISABLED', () => {
    expect(needsAttention('PAUSED')).toBe(false)
    expect(needsAttention('DISABLED')).toBe(false)
    expect(needsAttention('ACTIVE')).toBe(false)
    expect(needsAttention('WAITING')).toBe(false)
  })

  it('includes the four an operator acts on', () => {
    for (const state of ['INVALID', 'FAILED', 'DEGRADED', 'PARTIAL'] satisfies PathState[] | string[]) {
      expect(needsAttention(state as never)).toBe(true)
    }
  })
})
