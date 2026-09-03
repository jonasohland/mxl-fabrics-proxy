/**
 * The fleet poll: one timer, one set of reads, one replacement.
 *
 * There is no watch, stream, SSE or websocket on the user API and no ETag or revision cursor on any
 * read, so the UI polls (`ui.md` §2). Every read costs the server a full store load plus a
 * reconcile over the whole fleet — O(fleet), not O(response) — so this is deliberately a *single*
 * timer fetching what the screen needs together, rather than one timer per component. Sub-second
 * polling buys nothing: the underlying state is not changing faster than the reconciler runs, and it
 * multiplies a full store read by the number of open tabs.
 */

import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'

import { ApiError, api } from '@/api/client'
import type { NamespaceInfo, Node, Path, Request } from '@/api/types'

/**
 * The control plane's own heartbeat defaults to 5 s and its settling window is three heartbeats, so
 * something in the 2–5 s range is appropriate. Faster is not more live, only more expensive.
 */
const POLL_MS = 3000

export const useFleetStore = defineStore('fleet', () => {
  // Replaced wholesale on every successful poll, never merged into. `disabled` on a destination is
  // `omitempty`, so a re-enabled leg comes back with no key at all and anything that merges a poll
  // over its previous state leaves a stale `true` and shows the leg parked forever (`ui.md` §5 trap
  // 15). The same applies to every other omitempty boolean the API grows, so the rule is replace.
  const nodes = shallowRef<Node[]>([])
  const paths = shallowRef<Path[]>([])
  const requests = shallowRef<Request[]>([])
  const namespaces = shallowRef<NamespaceInfo[]>([])

  /**
   * The server has not yet run its first reconcile — it just started, or an HA leader changed.
   *
   * It says so explicitly rather than reporting everything as `WAITING`, precisely so that a restart
   * does not look like a fleet-wide outage. The state underneath is correct but not yet being acted
   * on, and must not be rendered as steady.
   */
  const settling = ref(false)

  /** Which replica is reconciling. The only place the API exposes it. */
  const leader = ref<string | undefined>(undefined)

  /** The store is unreachable — the server is fine, its store is not. A different place to look. */
  const storeUnreachable = ref(false)

  const lastError = ref<Error | undefined>(undefined)
  const lastGoodAt = ref<number | undefined>(undefined)
  const started = ref(false)

  /**
   * A failed poll changes nothing on screen: the last good read stays rendered and this goes true.
   * Same discipline the agent runs on — an unreachable server must not look like an empty fleet.
   */
  const stale = computed(() => lastError.value !== undefined)

  /** True until the first successful read, so a first paint can say "loading" rather than "empty". */
  const loaded = computed(() => lastGoodAt.value !== undefined)

  let timer: ReturnType<typeof setTimeout> | undefined
  let inFlight: AbortController | undefined

  /**
   * One cycle.
   *
   * All four reads land together or none of them do. A partial update would put requests from this
   * poll beside paths from the last, and the workspace joins the two — ownership comes from
   * `path.requests[]`, cell states from a request's own paths — so a mixed snapshot renders
   * relationships that never existed at any instant. Staleness is honest; a torn read is not.
   */
  async function poll(): Promise<void> {
    inFlight?.abort()
    const controller = new AbortController()
    inFlight = controller

    try {
      const [pathsResponse, nodeList, requestList, namespaceList] = await Promise.all([
        api.paths(controller.signal),
        api.nodes(controller.signal),
        api.requests(undefined, controller.signal),
        api.namespaces(controller.signal),
      ])

      paths.value = pathsResponse.paths ?? []
      nodes.value = nodeList.nodes ?? []
      requests.value = requestList.requests ?? []
      namespaces.value = namespaceList.namespaces ?? []
      settling.value = pathsResponse.settling === true

      storeUnreachable.value = false
      lastError.value = undefined
      lastGoodAt.value = Date.now()
    } catch (error) {
      if (controller.signal.aborted) return

      // `not_ready` is the settling condition arriving as a status code rather than a field. It is
      // not a failure and must not blank the view or raise an error surface.
      if (error instanceof ApiError && error.notReady) {
        settling.value = true
        lastError.value = undefined
        return
      }

      // Nor is a 401 a failure to report here. The transport has already raised the prompt
      // (`api/auth.ts`), which says what to do about it; a `Stale` banner over the top would be the
      // same fact told worse, and would keep counting minutes at an operator whose answer is to type
      // a token rather than to go looking for a server.
      if (error instanceof ApiError && error.unauthorized) {
        lastError.value = undefined
        return
      }

      storeUnreachable.value = error instanceof ApiError && error.storeUnreachable
      lastError.value = error instanceof Error ? error : new Error(String(error))
    } finally {
      if (inFlight === controller) inFlight = undefined
    }
  }

  /** Polled alongside the reads; the leader name comes from here and nowhere else. */
  async function pollReady(): Promise<void> {
    try {
      const ready = await api.readyz()
      leader.value = ready.leader
      settling.value = false
    } catch (error) {
      if (error instanceof ApiError && error.notReady) settling.value = true
    }
  }

  function schedule(): void {
    if (!started.value) return
    timer = setTimeout(() => void cycle(), POLL_MS)
  }

  async function cycle(): Promise<void> {
    // A hidden tab still costs the server a full reconcile per cycle for a view nobody is reading.
    // Skipping is safe because `refresh()` runs on the way back, so the first visible frame is
    // fresh rather than however old the last background poll was.
    if (typeof document === 'undefined' || !document.hidden) {
      await Promise.all([poll(), pollReady()])
    }
    schedule()
  }

  function start(): void {
    if (started.value) return
    started.value = true
    void cycle()

    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', onVisibility)
    }
  }

  function stop(): void {
    started.value = false
    if (timer !== undefined) clearTimeout(timer)
    timer = undefined
    inFlight?.abort()
    inFlight = undefined
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }

  function onVisibility(): void {
    if (!document.hidden) void refresh()
  }

  /** Force a cycle now — after a mutation, or on becoming visible. Does not disturb the timer. */
  async function refresh(): Promise<void> {
    await Promise.all([poll(), pollReady()])
  }

  return {
    nodes,
    paths,
    requests,
    namespaces,
    settling,
    leader,
    storeUnreachable,
    lastError,
    lastGoodAt,
    stale,
    loaded,
    start,
    stop,
    refresh,
  }
})
