import { describe, expect, it } from 'vitest'

import { domainError, elementsError, parseElements, requestNameError, slug } from './naming'

// Every case here is a rule the server enforces. They are mirrored so the operator finds out while
// typing; the server stays the authority, and the dry run is what decides anything about the fleet.
describe('domain elements', () => {
  it('refuses the two names that are not names', () => {
    expect(elementsError(['..'])).toContain('existing directory')
    expect(elementsError(['.'])).toContain('existing directory')
  })

  // A leading dot is a hidden directory — invisible to an operator listing the area and skipped by
  // discovery, so a domain named that way could never be observed and could never reach ACTIVE.
  it('refuses a leading dot or dash', () => {
    expect(elementsError(['.hidden'])).toContain('must not begin')
    expect(elementsError(['-flag'])).toContain('must not begin')
  })

  it('refuses a separator or anything outside the set, and accepts the set', () => {
    expect(elementsError(['a b'])).toContain('letters, digits')
    expect(elementsError(['café'])).toContain('letters, digits')
    expect(elementsError(['studio-a', 'cam_1', 'v.2'])).toBeUndefined()
  })

  it('bounds the depth and the element length', () => {
    expect(elementsError(Array.from({ length: 9 }, (_, i) => `e${i}`))).toContain('at most 8')
    expect(elementsError(['x'.repeat(65)])).toContain('longer than 64')
  })

  it('requires at least one', () => {
    expect(elementsError([])).toContain('required')
  })
})

// The whole rendered name is what the 255-byte cap is on, and it begins with the area — measuring
// only the elements would let the cap loosen silently now that the area is part of the name.
describe('domainError', () => {
  it('counts the area in the rendered length', () => {
    const area = 'a'.repeat(60)
    const elements = Array.from({ length: 4 }, () => 'b'.repeat(60))
    expect(domainError({ area, elements })).toContain('at most 255')
  })

  it('accepts an ordinary destination', () => {
    expect(domainError({ area: 'fast', elements: ['studio-a', 'cam1'] })).toBeUndefined()
  })

  it('names the area when the area is what is wrong', () => {
    expect(domainError({ area: '', elements: ['ingest'] })).toContain('no area')
    expect(domainError({ area: '.hidden', elements: ['ingest'] })).toContain('area')
  })
})

// Splitting is bounded to elements: the area is never typed, it comes from a picker over what the
// node advertises and grants `write` on. Empty segments are dropped rather than refused, so a
// trailing slash while typing is not an error that appears and disappears under the operator.
describe('parseElements', () => {
  it('drops empty and surrounding whitespace', () => {
    expect(parseElements('fast/ingest/')).toEqual(['fast', 'ingest'])
    expect(parseElements(' studio-a / cam1 ')).toEqual(['studio-a', 'cam1'])
    expect(parseElements('')).toEqual([])
  })
})

// The server's rule, not the wire type's: `RequestSpec.Validate` stays permissive because it is the
// contract, while the server decides what it is willing to name things — the name is a URL segment
// and a store key.
describe('requestNameError', () => {
  it('takes the character set the server takes, colon included', () => {
    expect(requestNameError('cam1-to-edge-01')).toBeUndefined()
    expect(requestNameError('studio-a:cam1')).toBeUndefined()
    expect(requestNameError('cam 1')).toContain('only letters')
    expect(requestNameError('.hidden')).toContain('must not begin with a dot')
    expect(requestNameError('')).toContain('required')
  })
})

describe('slug', () => {
  it('makes a name out of what the operator already chose', () => {
    expect(slug('Studio A:Camera 1')).toBe('studio-a-camera-1')
    expect(slug('  --video--  ')).toBe('video')
  })
})
