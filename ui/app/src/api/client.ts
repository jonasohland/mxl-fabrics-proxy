/**
 * The HTTP client. Relative URLs only, and nothing here can reach `/agent/v1`.
 *
 * **There is no API base and there must never be one** (`ui.md` §6). Both deployment shapes put the
 * UI and the API on one origin — served by the server binary, or behind a proxy fronting both — and
 * a base-URL setting is the thing that makes someone reach for CORS six months later. Development
 * is a dev-server proxy (see `vite.config.ts`), so both speak the same relative URLs.
 *
 * Authentication is one optional shared bearer token, and the preferred answer is still that the
 * proxy or the server injects the header on the way through so the browser never holds a credential
 * that also opens the privileged agent API. That deployment needs nothing from here and gets nothing
 * from here: the token in `auth.ts` is attached only if one has been entered, and it is only ever
 * *asked* for after a 401. See that module for what the fallback costs.
 */

import { authHeaders, noteStatus } from './auth'
import type {
  ApiErrorBody,
  DomainLabelResult,
  DomainLabelWrite,
  DomainList,
  ErrorCode,
  EventList,
  FlowList,
  LogTail,
  Namespace,
  NamespaceInfo,
  NamespaceList,
  NodeList,
  Outcome,
  PathsResponse,
  Readyz,
  ReasonCode,
  Request,
  RequestList,
  RequestSpec,
} from './types'
import { HEADER_OUTCOME } from './types'

/**
 * A refusal from the server, carrying both shapes of the same information.
 *
 * `message` is prose written by the code that refused and is better than anything the UI would
 * write — render it verbatim. `reasonCode` decides only what to highlight and which field or row to
 * anchor the message to.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode | undefined
  readonly reasonCode: ReasonCode | undefined
  readonly details: Record<string, string> | undefined

  constructor(status: number, body: Partial<ApiErrorBody> | undefined, fallback: string) {
    super(body?.message || fallback)
    this.name = 'ApiError'
    this.status = status
    this.code = body?.code
    this.details = body?.details
    this.reasonCode = body?.details?.['reason_code'] as ReasonCode | undefined
  }

  /** The store is unreachable — the server is fine, its store is not. A different place to look. */
  get storeUnreachable(): boolean {
    return this.status === 503 && this.code === 'internal'
  }

  /** The server has not run its first reconcile. Not an outage. */
  get notReady(): boolean {
    return this.status === 503 && this.code === 'not_ready'
  }

  /**
   * A token is configured on the server and the browser is not carrying it — or is carrying the
   * wrong one, which is the same refusal and deliberately indistinguishable (the server will not
   * tell an unauthenticated caller which of the two it got).
   *
   * The status alone, not the body: this is the one refusal the middleware answers before any
   * handler runs, so there may be no JSON at all if something in front of the server produced it.
   */
  get unauthorized(): boolean {
    return this.status === 401
  }
}

/**
 * Quotes integers too large for a double, so `JSON.parse` cannot silently round them.
 *
 * `max_message_size` is a genuine `uint64` and providers report `UINT64_MAX`: the wire carries
 * `18446744073709551615` and `JSON.parse` returns `18446744073709552000` (`ui.md` §5 trap 2). The
 * rounding happens inside the parser, so a reviver cannot help and the text has to be fixed first.
 *
 * A scanner rather than a regex, because a regex cannot tell a number in the document from digits
 * inside a string value — and a flow definition is arbitrary NMOS content that may contain
 * anything. Strings are copied through untouched, which also covers every object key.
 */
export function quoteBigIntegers(text: string): string {
  let out = ''
  let i = 0
  let inString = false

  while (i < text.length) {
    const c = text[i]!

    if (inString) {
      if (c === '\\') {
        out += c + (text[i + 1] ?? '')
        i += 2
        continue
      }
      if (c === '"') inString = false
      out += c
      i++
      continue
    }

    if (c === '"') {
      inString = true
      out += c
      i++
      continue
    }

    // Outside a string, a digit or a minus can only begin a number.
    if (c === '-' || (c >= '0' && c <= '9')) {
      const start = i
      if (text[i] === '-') i++
      while (i < text.length && text[i]! >= '0' && text[i]! <= '9') i++

      const next = text[i]
      const isInteger = next !== '.' && next !== 'e' && next !== 'E'
      const raw = text.slice(start, i)

      if (isInteger && !Number.isSafeInteger(Number(raw))) {
        out += `"${raw}"`
        continue
      }

      // Consume any fraction and exponent, then copy the number through as written.
      while (i < text.length && /[0-9.eE+-]/.test(text[i]!)) i++
      out += text.slice(start, i)
      continue
    }

    out += c
    i++
  }

  return out
}

export function parseJSON<T>(text: string): T {
  return JSON.parse(quoteBigIntegers(text)) as T
}

async function readBody(response: Response): Promise<unknown> {
  const text = await response.text()
  if (text.trim() === '') return undefined
  try {
    return parseJSON(text)
  } catch {
    return undefined
  }
}

interface RequestOptions {
  method?: string
  body?: unknown
  signal?: AbortSignal
}

async function call(path: string, options: RequestOptions = {}): Promise<Response> {
  const init: RequestInit = {
    method: options.method ?? 'GET',
    // Same-origin by construction; stated rather than left to the default so that a future
    // reviewer sees the deployment decision rather than an omission.
    credentials: 'same-origin',
  }
  if (options.signal) init.signal = options.signal

  // Every call, including the reads, because the token guards both prefixes wholesale and there is
  // no anonymous subset of `/v1` to keep it off. Empty where none is held, which is the ordinary
  // case: no token configured, or a proxy injecting the header in front of this browser.
  const headers: Record<string, string> = authHeaders()
  if (options.body !== undefined) {
    init.body = JSON.stringify(options.body)
    headers['Content-Type'] = 'application/json'
  }
  if (Object.keys(headers).length > 0) init.headers = headers

  const response = await fetch(path, init)
  noteStatus(path, response.status)
  if (!response.ok) {
    const body = (await readBody(response)) as Partial<ApiErrorBody> | undefined
    throw new ApiError(response.status, body, `${init.method} ${path} failed: ${response.status}`)
  }
  return response
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await call(path, signal ? { signal } : {})
  return (await readBody(response)) as T
}

const ns = (namespace: string) => `/v1/namespaces/${encodeURIComponent(namespace)}`
const requestPath = (namespace: string, name: string) =>
  `${ns(namespace)}/requests/${encodeURIComponent(name)}`

const dryRunSuffix = (dryRun: boolean) => (dryRun ? '?dry_run=true' : '')

/** The result of a create-or-update. Read the outcome, not the status code. */
export interface ApplyResult {
  request: Request
  /**
   * `unchanged` means **nothing was written**. A UI that re-POSTs on every interaction turns a
   * resyncing screen into store churn — the server already skips the write, but the client should
   * not be asking.
   */
  outcome: Outcome | undefined
}

export const api = {
  // -- reads ---------------------------------------------------------------

  /**
   * Every one of these costs the server a full store load plus a reconcile, whatever was asked for
   * (`ui.md` §2). Poll them together on a single timer; do not give each component its own.
   */
  paths: (signal?: AbortSignal) => get<PathsResponse>('/v1/paths', signal),

  nodes: (signal?: AbortSignal) => get<NodeList>('/v1/nodes', signal),

  namespaces: (signal?: AbortSignal) => get<NamespaceList>('/v1/namespaces', signal),

  namespace: (name: string, signal?: AbortSignal) => get<NamespaceInfo>(ns(name), signal),

  requests: (namespace?: string, signal?: AbortSignal) =>
    get<RequestList>(
      namespace ? `/v1/requests?namespace=${encodeURIComponent(namespace)}` : '/v1/requests',
      signal,
    ),

  request: (namespace: string, name: string, signal?: AbortSignal) =>
    get<Request>(requestPath(namespace, name), signal),

  flows: (query: Partial<Record<'node' | 'domain' | 'flow' | 'group_hint' | 'type', string>> = {}, signal?: AbortSignal) => {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== '') params.set(key, value)
    }
    const suffix = params.toString()
    return get<FlowList>(suffix ? `/v1/flows?${suffix}` : '/v1/flows', signal)
  },

  /**
   * Observed domains joined against their label records. There is no `GET /v1/nodes/{node}` —
   * `ui.md` §9.1 lists one, the mux does not register it, and it returns 404. Fetch the list and
   * filter, which is what `describe node` does.
   */
  domains: (node: string, signal?: AbortSignal) =>
    get<DomainList>(`/v1/nodes/${encodeURIComponent(node)}/domains`, signal),

  readyz: (signal?: AbortSignal) => get<Readyz>('/readyz', signal),

  // -- events --------------------------------------------------------------
  //
  // **The only reads here that do not cost a full reconcile.** Every other one goes through the
  // server's `view()`, which loads the whole store and reconciles it whatever was asked for; an
  // event read is one `Get` on one key, because a ring is a record of what already happened and
  // there is nothing to recompute (`internal/server/eventsapi.go`).
  //
  // **None of them takes a cursor, deliberately.** The endpoints accept `?since=` and this UI never
  // sends one: coalescing *rewrites the last entry in place with a new sequence number*, so an
  // incremental poll is handed the same row twice and the only way to dedup it is to reimplement
  // `Event.coalescesWith`'s key in TypeScript. That is the same second-implementation-of-a-Go-rule
  // trap `docs/open-items.md` §3.4 argues against for the manifest grammar, and the thing it would
  // buy is re-fetching at most fifty entries. Read the whole ring; replace what is on screen.

  pathEvents: (id: string, signal?: AbortSignal) =>
    get<EventList>(`/v1/paths/${encodeURIComponent(id)}/events`, signal),

  /**
   * A request's own ring merged with those of the paths it **currently** expands onto — the server
   * does the merge, and it is the one event read that does pay for a fleet load, because which paths
   * a request expands onto is derived and there is nowhere else to learn it from.
   */
  requestEvents: (namespace: string, name: string, signal?: AbortSignal) =>
    get<EventList>(`${requestPath(namespace, name)}/events`, signal),

  /** Answered for a node with no registration: a node's log outlives its paths and its lease. */
  nodeEvents: (node: string, signal?: AbortSignal) =>
    get<EventList>(`/v1/nodes/${encodeURIComponent(node)}/events`, signal),

  /**
   * The tail behind a `has_log` marker. **Fetched on a deliberate act and never on a poll** — that
   * split is the whole reason the marker exists rather than the bytes (§12.2).
   *
   * A 404 is the ordinary answer for a path nothing has captured a log for, so it is `undefined`
   * here rather than a throw.
   */
  async pathLogs(id: string, signal?: AbortSignal): Promise<LogTail | undefined> {
    try {
      return await get<LogTail>(`/v1/paths/${encodeURIComponent(id)}/logs`, signal)
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) return undefined
      throw error
    }
  },

  // -- mutations -----------------------------------------------------------

  /**
   * Create **or update**, keyed on `(namespace, name)`. There is no create-only mode and no 409, so
   * a POST with a name that exists and a different spec rewrites it — the UI is responsible for not
   * letting "new" silently overwrite something.
   *
   * Dry-run first. It runs the identical path and skips only the write, validating against the real
   * fleet including conflicts no client-side check can see: two sources into one destination flow,
   * namespace overlaps, loops.
   */
  async applyRequest(
    namespace: string,
    spec: RequestSpec,
    opts: { dryRun?: boolean; signal?: AbortSignal } = {},
  ): Promise<ApplyResult> {
    const response = await call(`${ns(namespace)}/requests${dryRunSuffix(opts.dryRun ?? false)}`, {
      method: 'POST',
      body: spec,
      ...(opts.signal ? { signal: opts.signal } : {}),
    })
    return {
      request: (await readBody(response)) as Request,
      outcome: (response.headers.get(HEADER_OUTCOME) as Outcome | null) ?? undefined,
    }
  },

  /**
   * Cancels the intent. It does **not** necessarily stop media — a path survives while another
   * request still references it, which `path.requests[]` says at no cost.
   *
   * A 404 is "already gone", which the CLI treats as success because deleting what a manifest names
   * is idempotent by nature. Reported here as `false` so a UI acting on a row the user can see can
   * refresh rather than raise a dialog.
   */
  async deleteRequest(namespace: string, name: string): Promise<boolean> {
    try {
      await call(requestPath(namespace, name), { method: 'DELETE' })
      return true
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) return false
      throw error
    }
  },

  /** Create or update, keyed on name. The matrix creates its namespaces `exclusive`. */
  applyNamespace: (namespace: Namespace) =>
    call('/v1/namespaces', { method: 'POST', body: namespace }).then(
      (response) => readBody(response) as Promise<NamespaceInfo>,
    ),

  /** Refused with 409 while any request references it. */
  deleteNamespace: (name: string) => call(ns(name), { method: 'DELETE' }).then(() => undefined),

  /**
   * Writes the labels on one `(node, domain)`.
   *
   * The second mutation, and it takes `?dry_run=true` on exactly the argument requests do: a label
   * joins or removes a domain from a request's expansion, so it starts and stops media one level of
   * indirection away — which makes it *easier* to do by accident, not harder. The result carries
   * `stopped[]` and `started[]` as full paths, so the blast radius needs no second read.
   */
  async writeDomainLabels(
    node: string,
    write: DomainLabelWrite,
    opts: { dryRun?: boolean } = {},
  ): Promise<DomainLabelResult> {
    const response = await call(
      `/v1/nodes/${encodeURIComponent(node)}/domains${dryRunSuffix(opts.dryRun ?? false)}`,
      { method: 'POST', body: write },
    )
    return (await readBody(response)) as DomainLabelResult
  },
}
