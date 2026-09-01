import { describe, expect, it } from 'vitest'

import { parseJSON, quoteBigIntegers } from './client'
import { UINT64_MAX, asBigInt, domainNestedIn, renderDomain } from './types'

describe('quoteBigIntegers', () => {
  // The wire carries 18446744073709551615 and JSON.parse returns 18446744073709552000. The
  // rounding happens inside the parser, so the text has to be fixed before it gets there.
  it('survives UINT64_MAX', () => {
    const text = '{"max_message_size":18446744073709551615}'
    const parsed = parseJSON<{ max_message_size: string }>(text)
    expect(parsed.max_message_size).toBe('18446744073709551615')
    expect(asBigInt(parsed.max_message_size)).toBe(UINT64_MAX)
  })

  it('leaves safe integers as numbers', () => {
    const parsed = parseJSON<{ restarts: number; size: number }>('{"restarts":3,"size":-9007199254740991}')
    expect(parsed.restarts).toBe(3)
    expect(typeof parsed.restarts).toBe('number')
    expect(parsed.size).toBe(-9007199254740991)
  })

  it('leaves floats and exponents alone', () => {
    expect(quoteBigIntegers('{"a":1.5,"b":1e300,"c":-0.25}')).toBe('{"a":1.5,"b":1e300,"c":-0.25}')
  })

  // A flow definition is arbitrary NMOS content, so digits inside a string are ordinary. A regex
  // over the text cannot tell those from numbers in the document; this is why it is a scanner.
  it('does not touch digits inside strings', () => {
    const text = '{"label":"18446744073709551615","escaped":"a \\" 18446744073709551615"}'
    expect(quoteBigIntegers(text)).toBe(text)
  })

  it('handles a big integer nested in an array', () => {
    const parsed = parseJSON<{ sizes: (string | number)[] }>('{"sizes":[1,18446744073709551615,2]}')
    expect(parsed.sizes).toEqual([1, '18446744073709551615', 2])
  })
})

describe('domains', () => {
  it('renders area first', () => {
    expect(renderDomain({ area: 'fast', elements: ['studio-a', 'cam1'] })).toBe('fast/studio-a/cam1')
  })

  // Nesting is the only destination collision that survives. Two domains sharing a name under
  // different areas is unconstructible — they are already two different strings.
  it('nests only within one area', () => {
    const parent = { area: 'fast', elements: ['studio-a'] }
    const child = { area: 'fast', elements: ['studio-a', 'cam1'] }
    expect(domainNestedIn(child, parent)).toBe(true)
    expect(domainNestedIn(parent, child)).toBe(false)
    expect(domainNestedIn({ area: 'bulk', elements: ['studio-a', 'cam1'] }, parent)).toBe(false)
  })

  // The string spelling of this question has to work around studio-ab looking like a child of
  // studio-a. The element form does not.
  it('is not fooled by a shared string prefix', () => {
    expect(domainNestedIn(
      { area: 'fast', elements: ['studio-ab'] },
      { area: 'fast', elements: ['studio-a'] },
    )).toBe(false)
  })
})
