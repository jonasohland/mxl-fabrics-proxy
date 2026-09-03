import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { authRequired, clearToken, setToken, token } from './auth'
import { api } from './client'

/**
 * The suite runs in node, so `localStorage` is the module's guarded-absent case by default. Installed
 * here because persistence is half of what this module does — the other half is which responses are
 * allowed to change [authRequired], which is where the flapping bug lived.
 */
function installStorage() {
  const values = new Map<string, string>()
  const storage = {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => void values.set(key, value),
    removeItem: (key: string) => void values.delete(key),
  }
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true })
  return values
}

/** Answers every call with one status, and records what it was asked. */
function installFetch(status: number) {
  const calls: RequestInit[] = []
  globalThis.fetch = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    calls.push(init ?? {})
    return new Response(status === 204 ? '' : '{}', { status })
  }) as typeof fetch
  return calls
}

function header(init: RequestInit): string | undefined {
  return (init.headers as Record<string, string> | undefined)?.['Authorization']
}

describe('the bearer token', () => {
  let stored: Map<string, string>

  beforeEach(() => {
    stored = installStorage()
    setToken('')
    authRequired.value = false
  })

  afterEach(() => vi.restoreAllMocks())

  it('is attached to every call once held', async () => {
    const calls = installFetch(200)
    setToken('s3cret')
    await api.nodes()
    expect(header(calls[0]!)).toBe('Bearer s3cret')
  })

  // The ordinary case, and the one the recommended deployment stays in: no token, no header, and a
  // proxy in front supplying its own.
  it('sends no header when none is held', async () => {
    const calls = installFetch(200)
    await api.nodes()
    expect(header(calls[0]!)).toBeUndefined()
  })

  it('survives a reload and is forgettable', () => {
    setToken('  s3cret  ')
    expect(token.value).toBe('s3cret')
    expect(stored.get('mxl.token')).toBe('s3cret')

    clearToken()
    expect(token.value).toBe('')
    expect(stored.has('mxl.token')).toBe(false)
  })

  it('raises the prompt on a refusal', async () => {
    installFetch(401)
    await expect(api.nodes()).rejects.toThrow()
    expect(authRequired.value).toBe(true)
  })

  // `/readyz` is outside the auth middleware and answers 200 to anyone, and the fleet store polls it
  // concurrently with the reads. If its success counted as evidence about the credential it would
  // undo the 401 landing beside it, twice a poll, and the gate would flap.
  it('is not cleared by a success on an unauthenticated path', async () => {
    installFetch(401)
    await expect(api.nodes()).rejects.toThrow()

    installFetch(200)
    await api.readyz()
    expect(authRequired.value).toBe(true)
  })

  // Cleared by evidence and nothing else: `setToken` deliberately does not, so that a submitted
  // token that is refused again leaves the prompt mounted and able to say so.
  it('is cleared only by a read that works', async () => {
    installFetch(401)
    await expect(api.nodes()).rejects.toThrow()

    setToken('s3cret')
    expect(authRequired.value).toBe(true)

    installFetch(200)
    await api.nodes()
    expect(authRequired.value).toBe(false)
  })
})
