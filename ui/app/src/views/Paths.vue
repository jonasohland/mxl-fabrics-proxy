<script setup lang="ts">
/**
 * `get paths` — every edge the fleet is maintaining, fleet-wide.
 *
 * This is the list the app was missing: `/paths/:id` and `/sessions/:id` had no entry point at all
 * outside a matrix cell or a node page, so reaching a path meant first guessing which namespace
 * routed it. It costs nothing to add — `stores/fleet.ts` already polls the whole path list for the
 * workspace, so this is the same read rendered a second way rather than a second read.
 *
 * **There is no namespace filter, and that is a decision rather than an omission.** A namespace is
 * not a property of a path: a path can be claimed by requests in several namespaces at once, which
 * is what `requests[]` is and the whole reason refcounting exists (`ui.md` §5 trap 14). `?ns=nab`
 * could therefore only mean *paths claimed by at least one request in nab* — which is the claims
 * question, and the ledger answers it properly, with the selector that made each claim and
 * `sole`/`shared` beside it. So the current namespace **marks** rows here and never hides any, and
 * the honest way to ask the scoped question is the link at the top of the page.
 *
 * The facets are the things that *are* properties of an edge: its state, the nodes at its ends, and
 * whether a session is realising it.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import type { Path } from '@/api/types'
import { PATH_STATES } from '@/api/types'
import Facets from '@/components/Facets.vue'
import { endpointText } from '@/model/detail'
import type { FilterSpec } from '@/model/filters'
import { plural, shortId } from '@/model/labels'
import { namespaceOfId } from '@/model/staging'
import { DISPLAY_ORDER, displayRank } from '@/model/state'
import { claimsRoute, domainRoute, flowRoute, nodeRoute, pathRoute, requestRoute, sessionRoute } from '@/router'
import { useCurrentStore } from '@/stores/current'
import { useFilters } from '@/stores/filters'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()
const current = useCurrentStore()

/** Worst-first, as the chips should read and as alphabetical would destroy. */
const STATE_ORDER = DISPLAY_ORDER.filter((state) => (PATH_STATES as readonly string[]).includes(state))

const SPEC: FilterSpec<Path> = {
  facets: [
    {
      key: 'state',
      label: 'state',
      valuesOf: (path) => [path.state],
      vocabulary: STATE_ORDER,
      chipClass: (value) => `state-${value}`,
    },
    {
      key: 'node',
      // Either end, and both are checked independently — a node can be *both* ends of one path,
      // same node different domain, which is what the loopback configuration does. A facet that
      // matched the source first would hide half of what a node is running (`model/detail.ts`).
      label: 'node',
      title: 'Either end of the path. A node can be both.',
      valuesOf: (path) => [path.source.node, path.destination.node],
    },
    {
      key: 'session',
      label: 'session',
      title: 'A path in WAITING has none, and neither has one torn down for being idle',
      valuesOf: (path) => [path.session ? 'running' : 'none'],
      vocabulary: ['running', 'none'],
    },
  ],
  // What the row renders, including what it renders in a title: the holders are the refcount's
  // tooltip, and a search that could not find them would be a search of half the row.
  textOf: (path) => [
    path.id,
    path.source.node,
    path.source.domain,
    path.source.flow,
    endpointText(path.destination),
    path.state,
    path.reason ?? '',
    ...path.requests,
  ].join(' '),
}

/**
 * Worst-first, then by the edge itself.
 *
 * The tie-break is total and comes out of the row rather than out of the response, so the list does
 * not reorder under the pointer every three seconds — the same discipline the topology's layout
 * follows for the same reason.
 */
const sorted = computed(() =>
  [...fleet.paths].sort((a, b) =>
    displayRank(a.state) - displayRank(b.state) ||
    a.source.node.localeCompare(b.source.node) ||
    a.source.domain.localeCompare(b.source.domain) ||
    a.source.flow.localeCompare(b.source.flow) ||
    endpointText(a.destination).localeCompare(endpointText(b.destination))),
)

// Destructured so the template reads `rows` rather than `filters.rows.value`: a ref returned at the
// top level of `setup` unwraps in the template, and one hanging off a plain object does not.
const { rows, groups, text, filtered, toggle, setText, clear } = useFilters(SPEC, () => sorted.value)

/** Marked, never filtered — see the note at the top of this file. */
function mine(path: Path): boolean {
  const namespace = current.namespace
  if (!namespace) return false
  return path.requests.some((id) => namespaceOfId(id) === namespace)
}
</script>

<template>
  <main class="ls-page">
    <header class="ls-head">
      <h1>Paths</h1>
      <span class="ls-count-of">
        {{ rows.length }} of {{ plural(fleet.paths.length, 'path') }}
      </span>
      <span class="spacer" />
      <!-- The scoped question this page deliberately does not answer, offered where it is asked. -->
      <RouterLink
        v-if="current.namespace"
        class="link ls-dim"
        :to="claimsRoute(current.namespace)"
        title="Which of these this namespace claims, with the selector that made each claim"
      >claims in {{ current.namespace }} →</RouterLink>
    </header>

    <Facets
      :groups="groups"
      :text="text"
      :filtered="filtered"
      @toggle="toggle"
      @update:text="setText"
      @clear="clear"
    />

    <!-- Two different sentences on purpose: an empty fleet and an empty filter send an operator to
         two different places, and a table that said "no paths" to both would hide a filter that is
         still on from the last link they followed. -->
    <p v-if="fleet.paths.length === 0" class="ls-empty">
      No paths. Nothing is routed, or nothing has expanded onto an edge yet.
    </p>
    <p v-else-if="rows.length === 0" class="ls-empty">
      No path matches. {{ plural(fleet.paths.length, 'path') }} filtered out.
    </p>

    <table v-else class="ls-table">
      <thead>
        <tr>
          <th>id</th>
          <th>state</th>
          <th>source</th>
          <th>destination</th>
          <th title="How many requests hold this edge. More than one, and cancelling any of them leaves it running">refs</th>
          <th>session</th>
          <th class="ls-wide">reason</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="path in rows" :key="path.id" :class="{ 'ls-mine': mine(path) }">
          <td>
            <RouterLink class="link mono" :to="pathRoute(path.id)" :title="path.id">
              {{ shortId(path.id) }}
            </RouterLink>
          </td>

          <td :class="`state-${path.state}`">{{ path.state }}</td>

          <td class="mono">
            <RouterLink class="link" :to="nodeRoute(path.source.node)">{{ path.source.node }}</RouterLink>
            <RouterLink class="link" :to="domainRoute(path.source.node, path.source.domain)">
              {{ path.source.domain }}
            </RouterLink>
            <RouterLink class="link" :to="flowRoute(path.source.flow)" :title="path.source.flow">
              {{ shortId(path.source.flow) }}
            </RouterLink>
          </td>

          <td class="mono">
            <span class="ls-arrow">→</span>
            <RouterLink class="link" :to="domainRoute(path.destination.node, path.destination.domain)">
              {{ endpointText(path.destination) }}
            </RouterLink>
            <!-- Authored, not broken: a parked leg is the model's only spelling of *off*. -->
            <span v-if="path.destination.disabled" class="dt-parked"> · parked</span>
          </td>

          <td :title="path.requests.join(', ')">
            <template v-if="path.requests.length === 1">
              <RouterLink class="link mono" :to="requestRoute(path.requests[0]!)">1</RouterLink>
            </template>
            <span v-else class="mono">{{ path.requests.length }}</span>
          </td>

          <td>
            <RouterLink
              v-if="path.session"
              class="link mono"
              :to="sessionRoute(path.session.id)"
              :title="path.session.id"
            >{{ shortId(path.session.id) }}</RouterLink>
            <span v-else class="ls-faint">·</span>
          </td>

          <td class="ls-wide">{{ path.reason }}</td>
        </tr>
      </tbody>
    </table>
  </main>
</template>

<style scoped>
/* Three identifiers on one line and each is its own link, as `PathDetail.vue` renders the same
   triple — spaced rather than run together, so node, domain and flow read as the triple they are. */
.mono .link + .link { margin-left: 8px; }

/* The domain column is the one thing here that has no bound, and a 60-character element list would
   otherwise push the reason off the screen. Elided rather than wrapped: every column but the last
   is `nowrap`, and a wrapping cell would make one row two lines tall. The full name is a link away
   and the title carries it. */
td.mono { max-width: 42ch; overflow: hidden; text-overflow: ellipsis; }
</style>
