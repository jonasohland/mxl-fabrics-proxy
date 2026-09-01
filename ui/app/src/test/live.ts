/**
 * Harness for the integration tests: the real components, the real client, a real server.
 *
 * These drive the shipped code against a live control plane and a fake fleet — real DOM, real
 * fetch, real reconciler, real store. The prototype's `verify.mjs` established that this catches a
 * class of bug nothing else does, and the class is consistent: a stale dialog list, two per-node
 * reads landing out of order, a selection carried across a reopen. Each is the page's behaviour
 * against a real *sequence* of reads, which is the only place it exists.
 *
 * Run them with `npm run test:live` after `npm run devfleet`. They are deliberately not part of
 * `npm test`, which stays hermetic.
 */

const BASE = process.env.API_BASE ?? 'http://127.0.0.1:12999'

/**
 * Resolve the app's relative URLs against a real origin.
 *
 * Production code uses relative URLs only and must keep doing so (`ui.md` §6) — the same-origin
 * decision is what removes CORS and keeps a configurable API base from ever existing. Node's fetch
 * requires an absolute URL, so the base is supplied *here*, in the harness, rather than by giving
 * the client a base it would then carry into production.
 */
export function installFetchBase(): void {
  const original = globalThis.fetch

  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' && input.startsWith('/') ? BASE + input : input
    return call(original, url, init)
  }) as typeof fetch
}

/**
 * Run the request without handing the caller's `AbortSignal` to node's fetch.
 *
 * jsdom installs its own `AbortController`/`AbortSignal` globals, so the signal the app constructs
 * is jsdom's while `fetch` here is node's (undici), which type-checks its `signal` by identity and
 * refuses: *"Expected signal to be an instance of AbortSignal"*. A browser has one matching pair
 * and never sees this, so it is an artefact of the harness rather than anything about the client —
 * which is why the fix lives here and the client keeps passing its signal.
 *
 * Abort stays observable: the returned promise rejects as soon as the caller's signal fires, which
 * is the behaviour the poll's `signal.aborted` check depends on. Only the transport-level cancel is
 * lost, and an extra in-flight read against a dev fixture costs nothing.
 */
function call(fetchImpl: typeof fetch, url: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const signal = init?.signal
  if (!signal) return fetchImpl(url, init)

  const { signal: _dropped, ...rest } = init ?? {}
  const response = fetchImpl(url, rest)

  return new Promise<Response>((resolve, reject) => {
    const abort = () => reject(new DOMException('The operation was aborted.', 'AbortError'))
    if (signal.aborted) return abort()
    signal.addEventListener('abort', abort, { once: true })
    response.then(resolve, reject)
  })
}

export async function requireServer(): Promise<void> {
  try {
    const response = await fetch(`${BASE}/readyz`)
    if (!response.ok) throw new Error(`readyz returned ${response.status}`)
  } catch (error) {
    throw new Error(
      `No control plane at ${BASE}. Start one with \`npm run devfleet\` (see ui/app/README.md). ` +
        `Cause: ${error instanceof Error ? error.message : String(error)}`,
    )
  }
}

/** Poll until `predicate` holds, so a test waits on the reconciler rather than on a sleep. */
export async function until(
  predicate: () => boolean | Promise<boolean>,
  { timeoutMs = 8000, stepMs = 100 }: { timeoutMs?: number; stepMs?: number } = {},
): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await predicate()) return
    await new Promise((resolve) => setTimeout(resolve, stepMs))
  }
  throw new Error(`condition not reached within ${timeoutMs}ms`)
}
