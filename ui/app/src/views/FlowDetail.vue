<script setup lang="ts">
/**
 * `describe flow` — every location the ID exists, and every path carrying it.
 *
 * **A flow ID identifies the media, not a location.** After replication the same UUID exists on both
 * nodes and that is success rather than duplication — the destination copy carries `replicated:
 * true`, which is how the two are told apart. So this is a list, and the multiplicity *is* the
 * answer. Nothing on this page is keyed on the ID alone; the addressable thing is
 * `(node, domain, flow)`.
 *
 * The definition is shown verbatim and read-only. It is arbitrary NMOS content including fields
 * nothing in this tree models, the destination flow must reproduce it exactly, and the session
 * identity hashes those bytes — so a re-serialisation that reordered keys would read as a different
 * flow and rebuild a healthy session (`ui.md` §5 trap 10). Displaying it is safe; anything that sent
 * it back would not be.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { api } from '@/api/client'
import {
  describeDefinition,
  endpointText,
  firstGroupHint,
  groupHintText,
  pathsCarrying,
} from '@/model/detail'
import { shortId } from '@/model/labels'
import { domainRoute, nodeRoute, pathRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'
import { useRead } from '@/stores/read'

const props = defineProps<{ flow: string }>()

const fleet = useFleetStore()

// One of the five query parameters `/v1/flows` takes. Filtered server-side rather than by pulling
// the fleet's whole inventory and searching it, which on a real fleet is thousands of entries for
// the handful this page is about.
const read = useRead(
  (signal) => api.flows({ flow: props.flow }, signal),
  () => props.flow,
)

const locations = computed(() => read.data.value?.flows ?? [])
const hint = computed(() => firstGroupHint(locations.value))
const summary = computed(() => describeDefinition(locations.value[0]?.flow_def))

/** Pretty-printed for reading. Display only — see the note at the head of this file. */
const definition = computed(() => {
  const def = locations.value[0]?.flow_def
  return def === undefined ? '' : JSON.stringify(def, null, 2)
})

const carrying = computed(() => pathsCarrying(props.flow, fleet.paths))
</script>

<template>
  <main class="dt-page">
    <h1 class="dt-title">
      <span class="dt-kind">Flow</span>
      <span class="mono">{{ props.flow }}</span>
    </h1>

    <p v-if="read.error.value" class="dt-read-error">{{ read.error.value.message }}</p>
    <p v-else-if="read.loading.value" class="dt-missing">Loading…</p>
    <p v-else-if="locations.length === 0" class="dt-missing">
      Flow <span class="mono">{{ props.flow }}</span> is not observed anywhere in the fleet.
    </p>

    <template v-else>
      <dl class="dt-fields">
        <template v-if="hint">
          <dt>group hint</dt>
          <dd>{{ groupHintText(hint) }}</dd>
        </template>
        <template v-if="summary">
          <dt>definition</dt>
          <dd>{{ summary }}</dd>
        </template>
      </dl>

      <section class="dt-section">
        <h2>Locations <span class="dim">{{ locations.length }}</span></h2>
        <table class="dt-table">
          <thead>
            <tr><th>node</th><th>domain</th><th>producing</th><th class="dt-wide">replicated</th></tr>
          </thead>
          <tbody>
            <!-- Keyed on the triple. The same UUID on two nodes is the point, and a list keyed on
                 the ID alone would render one row where there are two. -->
            <tr v-for="entry in locations" :key="`${entry.node}/${entry.domain}`">
              <td class="mono">
                <RouterLink class="link" :to="nodeRoute(entry.node)">{{ entry.node }}</RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="domainRoute(entry.node, entry.domain)">
                  {{ entry.domain }}
                </RouterLink>
              </td>
              <td :class="entry.producing ? 'state-ACTIVE' : 'dim'">
                {{ entry.producing ? 'yes' : 'idle' }}
              </td>
              <!-- True exactly while one of that node's own target workers is writing it: this is
                   the copy replication made, and a label selector will never match it. -->
              <td class="dt-wide" :class="entry.replicated ? 'written' : 'dim'">
                {{ entry.replicated ? 'yes' : 'no' }}
              </td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>Paths</h2>
        <p v-if="carrying.length === 0" class="dt-empty">No path replicates this flow.</p>
        <table v-else class="dt-table">
          <thead>
            <tr><th>path</th><th>from</th><th>to</th><th>state</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <tr v-for="path in carrying" :key="path.id">
              <td class="mono">
                <RouterLink class="link" :to="pathRoute(path.id)" :title="path.id">
                  {{ shortId(path.id) }}…
                </RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="domainRoute(path.source.node, path.source.domain)">
                  {{ path.source.node }} {{ path.source.domain }}
                </RouterLink>
              </td>
              <td class="mono">
                <RouterLink
                  class="link"
                  :to="domainRoute(path.destination.node, path.destination.domain)"
                >
                  {{ endpointText(path.destination) }}
                </RouterLink>
              </td>
              <td :class="`state-${path.state}`">{{ path.state }}</td>
              <td class="dt-wide">{{ path.reason }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <!-- The reasoning goes in a title and in the comment at the head of this file, not on screen:
           operators are trained and the screen shows data. -->
      <section v-if="definition" class="dt-section">
        <h2 title="Verbatim and read-only">Definition</h2>
        <pre class="dt-raw mono">{{ definition }}</pre>
      </section>
    </template>
  </main>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.written { color: var(--s-paused); }
</style>
