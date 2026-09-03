<script setup lang="ts">
/**
 * `get requests` — every request in the fleet, across every namespace.
 *
 * Two things this is not. It is not the landing page, which names *only what is not active* and is
 * deliberately not a list; a healthy fleet's health screen is empty, and until this page existed a
 * healthy fleet had no route to a request at all. And it is not the matrix: the workspace renders
 * one namespace's intent as geometry and edits it, where this lists requests so that a name can be
 * found (`ui.md` §7). Nothing here writes.
 *
 * **This one does filter by namespace**, where `views/Paths.vue` deliberately does not, and the
 * asymmetry is the model rather than a preference: `namespace` is a field on a request spec and a
 * request is in exactly one, while a path is claimed by however many requests expand onto it.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import type { Request } from '@/api/types'
import Facets from '@/components/Facets.vue'
import type { FilterSpec } from '@/model/filters'
import { plural, sourceLabel } from '@/model/labels'
import { endpointText } from '@/model/detail'
import { DISPLAY_ORDER, displayRank } from '@/model/state'
import { namespaceRoute, requestRoute } from '@/router'
import { useCurrentStore } from '@/stores/current'
import { useFilters } from '@/stores/filters'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()
const current = useCurrentStore()

const namespaceOf = (request: Request) => request.namespace || 'default'

const SPEC: FilterSpec<Request> = {
  facets: [
    {
      key: 'state',
      label: 'state',
      valuesOf: (request) => [request.status.state],
      vocabulary: DISPLAY_ORDER,
      chipClass: (value) => `state-${value}`,
    },
    {
      key: 'ns',
      label: 'namespace',
      valuesOf: (request) => [namespaceOf(request)],
    },
  ],
  textOf: (request) => [
    request.id,
    request.status.state,
    request.status.reason ?? '',
    ...request.sources.map(sourceLabel),
    ...request.destinations.map(endpointText),
  ].join(' '),
}

/**
 * Worst-first, then by id.
 *
 * `request.status.state` and never a local fold: the server also folds leg failures that produce no
 * path at all, so a request whose every materialised path is `ACTIVE` beside one refused pairing is
 * `PARTIAL` on the wire and there is nothing in `status.paths[]` to recompute that from
 * (`model/state.ts`).
 */
const sorted = computed(() =>
  [...fleet.requests].sort((a, b) =>
    displayRank(a.status.state) - displayRank(b.status.state) || a.id.localeCompare(b.id)),
)

const { rows, groups, text, filtered, toggle, setText, clear } = useFilters(SPEC, () => sorted.value)

const parked = (request: Request) =>
  request.destinations.filter((destination) => destination.disabled).length
</script>

<template>
  <main class="ls-page">
    <header class="ls-head">
      <h1>Requests</h1>
      <span class="ls-count-of">
        {{ rows.length }} of {{ plural(fleet.requests.length, 'request') }}
      </span>
    </header>

    <Facets
      :groups="groups"
      :text="text"
      :filtered="filtered"
      @toggle="toggle"
      @update:text="setText"
      @clear="clear"
    />

    <p v-if="fleet.requests.length === 0" class="ls-empty">
      No requests. Nothing has been asked for yet.
    </p>
    <p v-else-if="rows.length === 0" class="ls-empty">
      No request matches. {{ plural(fleet.requests.length, 'request') }} filtered out.
    </p>

    <table v-else class="ls-table">
      <thead>
        <tr>
          <th>namespace</th>
          <th>name</th>
          <th>state</th>
          <th>sources</th>
          <th>destinations</th>
          <th>paths</th>
          <th class="ls-wide">reason</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="request in rows"
          :key="request.id"
          :class="{ 'ls-mine': namespaceOf(request) === current.namespace }"
        >
          <!-- To the workspace, because the next question about a namespace is what is routed in
               it, and that is a screen rather than a list row. -->
          <td>
            <RouterLink class="link mono" :to="namespaceRoute(namespaceOf(request))">
              {{ namespaceOf(request) }}
            </RouterLink>
          </td>

          <td>
            <RouterLink class="link mono" :to="requestRoute(request.id)">{{ request.name }}</RouterLink>
          </td>

          <td :class="`state-${request.status.state}`">{{ request.status.state }}</td>

          <td :title="request.sources.map(sourceLabel).join('\n')">
            {{ request.sources.length }}
          </td>

          <td :title="request.destinations.map((d) => endpointText(d)).join('\n')">
            {{ request.destinations.length }}
            <!-- Parked is authored, so it is counted rather than subtracted: a request with three
                 destinations of which two are off is not a request with one. -->
            <span v-if="parked(request)" class="dt-parked"> · {{ parked(request) }} parked</span>
          </td>

          <td class="mono">{{ request.status.paths.length }}</td>

          <td class="ls-wide">{{ request.status.reason }}</td>
        </tr>
      </tbody>
    </table>
  </main>
</template>
