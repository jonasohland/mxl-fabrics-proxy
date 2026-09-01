/**
 * A read the fleet poll does not make, kept fresh by the fleet poll's own clock.
 *
 * Three of the six detail views need something the workspace never asks for: a node's observed
 * domains (`GET /v1/nodes/{node}/domains`) and a flow's locations (`GET /v1/flows?flow=`). Neither
 * belongs in `stores/fleet.ts` — they are per-*subject* reads, and putting them there would make
 * every open tab pay for whichever node one of them happens to be looking at.
 *
 * **It does not start a timer.** `ui.md` §2's rule is one timer, because every user-API GET costs
 * the server a full store load plus a reconcile over the whole fleet, and a second timer multiplies
 * that by however many views are mounted. So this rides the fleet store's `lastGoodAt` instead: one
 * extra read per existing cycle, only while a view that needs it is on screen, and in step with the
 * fleet data it is rendered beside — a domain list from this tick next to paths from the last one is
 * exactly the torn read the single poll exists to prevent.
 *
 * Two kinds of refresh, and the difference is what the screen does between them:
 *
 * - **the subject changed** — navigating from one node to another. The old answer is about a
 *   different thing, so it is dropped and the view says it is loading.
 * - **the clock ticked** — the same subject, read again. The previous answer stays on screen until
 *   the new one lands, so a poll does not blink the page the operator is reading.
 */

import { onUnmounted, ref, shallowRef, watch } from 'vue'
import type { Ref } from 'vue'

import { useFleetStore } from './fleet'

export interface Read<T> {
  data: Ref<T | undefined>
  error: Ref<Error | undefined>
  /** True only until the *first* answer about the current subject. A refresh is not loading. */
  loading: Ref<boolean>
  /** Read again now, keeping what is on screen — after a mutation, without waiting for the tick. */
  refresh: () => Promise<void>
}

export function useRead<T>(
  load: (signal: AbortSignal) => Promise<T>,
  subject: () => unknown,
): Read<T> {
  const fleet = useFleetStore()

  const data = shallowRef<T | undefined>(undefined)
  const error = ref<Error | undefined>(undefined)
  const loading = ref(false)

  let inFlight: AbortController | undefined

  async function run(fresh: boolean): Promise<void> {
    inFlight?.abort()
    const controller = new AbortController()
    inFlight = controller

    if (fresh) {
      data.value = undefined
      error.value = undefined
      loading.value = true
    }

    try {
      const value = await load(controller.signal)
      if (controller.signal.aborted) return
      data.value = value
      error.value = undefined
    } catch (thrown) {
      if (controller.signal.aborted) return
      // A failed refresh leaves the last good answer on screen and says so, which is the same
      // discipline the fleet poll runs on: an unreachable server must not look like an empty node.
      error.value = thrown instanceof Error ? thrown : new Error(String(thrown))
    } finally {
      if (inFlight === controller) {
        inFlight = undefined
        loading.value = false
      }
    }
  }

  watch(subject, () => void run(true), { immediate: true })
  watch(() => fleet.lastGoodAt, () => void run(false))
  onUnmounted(() => inFlight?.abort())

  return { data, error, loading, refresh: () => run(false) }
}
