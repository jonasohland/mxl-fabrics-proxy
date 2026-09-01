/**
 * What the picker offers, and what mode each namespace is in.
 *
 * The mode is not a detail: it decides **which of two screens** the workspace is. A matrix is an
 * editor only over an `exclusive` namespace, because two requests can otherwise hold one path and a
 * cell stops meaning what it looks like; a `shared` one gets the ledger. So the picker shows every
 * namespace's mode, always, rather than on hover.
 */

import { defineStore } from 'pinia'
import { computed } from 'vue'

import { DEFAULT_NAMESPACE } from '@/api/types'
import type { NamespaceInfo, PathPolicy } from '@/api/types'
import { useFleetStore } from './fleet'

export const useNamespaceStore = defineStore('namespaces', () => {
  const fleet = useFleetStore()

  /**
   * Every namespace, with `default` synthesised when it has no record.
   *
   * `GET /v1/namespaces` lists only namespaces that exist as records, so a fleet with no requests
   * at all lists nothing — and a request that names no namespace is written into `default`. Showing
   * an empty picker would hide most fleets entirely, so the entry is synthesised with the mode it
   * would be auto-created with.
   */
  const all = computed<NamespaceInfo[]>(() => {
    const listed = fleet.namespaces
    if (listed.some((entry) => entry.name === DEFAULT_NAMESPACE)) {
      return [...listed].sort((a, b) => a.name.localeCompare(b.name))
    }
    return [
      ...listed,
      { name: DEFAULT_NAMESPACE, paths: 'shared' as PathPolicy, requests: 0 },
    ].sort((a, b) => a.name.localeCompare(b.name))
  })

  function info(name: string): NamespaceInfo | undefined {
    return all.value.find((entry) => entry.name === name)
  }

  /**
   * The `paths` mode, defaulting the way the API does.
   *
   * `shared` is the default and `default` is auto-created that way, so `exclusive` is an active
   * choice on every create path rather than something to assume.
   */
  function mode(name: string): PathPolicy {
    return info(name)?.paths ?? 'shared'
  }

  return { all, info, mode }
})
