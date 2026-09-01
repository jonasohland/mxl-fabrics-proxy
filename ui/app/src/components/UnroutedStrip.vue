<script setup lang="ts">
/**
 * The unrouted-sources strip — `ui.md` §7a's *"see what exists, route it"*.
 *
 * The matrix draws intent, so it can only ever show routes somebody has already written: a camera
 * nobody wrote a source for has no row, and a row that does not exist cannot be dark. This is the
 * other half — inventory this namespace is not carrying — and it is the only place in the product
 * where routing *starts* from what is there rather than from a name the operator already knows.
 *
 * `model/unrouted.ts` carries what "unrouted" is computed from and the two filters that make it
 * honest. What is decided here is how it is read:
 *
 * - **Grouped by `(node, domain)`**, because that is the unit a source is written against — a source
 *   pins a node and selects a domain. It also makes "route this whole domain" one gesture instead of
 *   one per flow, which for a camera that has just come up is the whole job.
 * - **The attention filter defaults on**, in the ledger's idiom. Fleet inventory is unbounded and
 *   most of it on a working board is accounted for; the two accounted-for kinds stay reachable
 *   behind the toggle rather than being dropped, because a chain `A→B→C` is written by routing this
 *   project's own output onward and that entry has to be findable somewhere.
 * - **Nothing here writes, and nothing here stages.** A click opens the source editor pre-filled,
 *   which is a panel the operator then reads and applies — the same one `+ source` opens. A strip
 *   entry that staged a request on the click would author a selector nobody had seen.
 *
 * The read is `GET /v1/flows`, and it is the fifth read of this screen — the one `ui.md` §7a's last
 * gap counts. It rides the fleet poll's clock rather than starting a timer (`stores/read.ts`), and
 * only while this component is mounted, so the landing page and the detail views do not pay for it.
 */
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import { api } from '@/api/client'
import type { DomainName } from '@/api/types'
import { plural, shortId } from '@/model/labels'
import type { SourcePrefill, UnroutedFlow } from '@/model/unrouted'
import { buildUnrouted } from '@/model/unrouted'
import { domainRoute, flowRoute, nodeRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'
import { useRead } from '@/stores/read'

const props = defineProps<{ namespace: string }>()
const emit = defineEmits<{ route: [SourcePrefill] }>()

const fleet = useFleetStore()

// No subject: the fleet's inventory is not namespace-scoped and neither is this read — only the
// question asked of it is. A constant means one read on mount and one per poll thereafter, which is
// what `useRead` is for; re-reading on a namespace switch would fetch the identical body.
const read = useRead((signal) => api.flows({}, signal), () => 'flows')

const unclaimedOnly = ref(true)

const strip = computed(() =>
  buildUnrouted(read.data.value?.flows ?? [], fleet.paths, props.namespace, {
    unclaimedOnly: unclaimedOnly.value,
  }),
)

/** How many the filter is holding back. Named, so the toggle says what turning it off would show. */
const hidden = computed(() => strip.value.elsewhere + strip.value.replicated)

const groupText = (flow: UnroutedFlow) =>
  flow.group === undefined
    ? '(no group hint)'
    : flow.group.type
      ? `${flow.group.name} (${flow.group.type})`
      : flow.group.name

/**
 * Why this entry is listed but is not work. Empty for the ones that are.
 *
 * `replicated` wins where both apply: it is the stronger explanation, and it is the one that changes
 * what routing the entry *means* — a label selector cannot reach it at all.
 */
function mark(flow: UnroutedFlow): string {
  if (flow.replicated) return 'written here by this project'
  if (flow.elsewhere.length) return `routed by ${flow.elsewhere.join(', ')}`
  return ''
}

function markTitle(flow: UnroutedFlow): string {
  if (flow.replicated) {
    return 'Written by one of this node\'s own target workers. A label selector never matches it, ' +
      'so routing it onward means naming its domain.'
  }
  if (flow.elsewhere.length) {
    return `Already a source in ${flow.elsewhere.join(', ')}. Routing it here as well is legal and ` +
      'doubles egress on this node.'
  }
  return ''
}

function routeFlow(flow: UnroutedFlow): void {
  emit('route', { node: flow.node, domain: flow.domain, flow: flow.flow })
}

function routeDomain(node: string, domain: DomainName): void {
  emit('route', { node, domain })
}

const flowTitle = (flow: UnroutedFlow) =>
  `${flow.flow}\n${flow.producing ? 'Producing.' : 'Idle.'}` +
  (flow.group
    ? '\nroute opens the source editor on this flow\'s group.'
    : '\nNo group hint, so there is no name for a group selector to match. route opens the editor ' +
      'with this flow pinned by ID.')
</script>

<template>
  <section class="strip">
    <header class="strip-head">
      <h2 title="Fleet inventory this namespace is not carrying">
        Unrouted
      </h2>

      <span class="counts">
        <span :class="{ work: strip.unclaimed > 0 }">{{ strip.unclaimed }} unclaimed</span>
        <span class="dim"> · {{ strip.routed }} routed here</span>
      </span>

      <!-- A silent cap reads as "nothing else was unrouted", which is exactly what the API's own
           `excluded_dropped` exists to prevent. Same treatment. -->
      <span v-if="strip.dropped > 0" class="dropped">
        {{ plural(strip.dropped, 'more') }} not shown
      </span>

      <span class="spacer" />

      <label
        class="filter"
        title="Hides entries routed by another namespace or written here by this project"
      >
        <input v-model="unclaimedOnly" type="checkbox" />
        only unclaimed<span v-if="hidden > 0" class="dim"> ({{ hidden }} hidden)</span>
      </label>
    </header>

    <p v-if="read.error.value" class="fail">{{ read.error.value.message }}</p>
    <p v-else-if="read.loading.value" class="empty">Reading the fleet's flows…</p>
    <p v-else-if="strip.domains.length === 0" class="empty">
      <template v-if="unclaimedOnly && hidden > 0">
        Nothing unclaimed. {{ plural(hidden, 'entry', 'entries') }} accounted for, hidden by the
        filter.
      </template>
      <template v-else-if="strip.routed > 0">
        Every flow the fleet reports is a source of something in {{ namespace }}.
      </template>
      <template v-else>
        No flows in the fleet's inventory.
      </template>
    </p>

    <!-- `v-else` on a wrapper rather than beside the `v-for`: the two on one element is a Vue
         warning and the `v-for` wins, so the list would render under every branch above. -->
    <template v-else>
      <div v-for="group in strip.domains" :key="group.key" class="group">
        <div class="group-head">
          <RouterLink class="mono node link" :to="nodeRoute(group.node)">{{ group.node }}</RouterLink>
          <RouterLink class="mono link" :to="domainRoute(group.node, group.domain)">
            {{ group.domain }}
          </RouterLink>
          <span class="dim tally">{{ plural(group.flows.length, 'flow') }}</span>
          <!-- Next to what it is about, not at the far edge: the rows below put their own control at
               a fixed column for the same reason, and a header whose control sat somewhere else
               would be the only thing on the strip that moved with the window. -->
          <button
            type="button"
            class="route"
            :title="`Open the source editor on ${group.node} ${group.domain} with the whole domain selected. Nothing is written.`"
            @click="routeDomain(group.node, group.domain)"
          >route domain</button>
        </div>

        <div v-for="flow in group.flows" :key="flow.key" class="row" :class="{ marked: !flow.unclaimed }">
          <span class="dot" :class="{ on: flow.producing }"></span>
          <RouterLink class="mono link flow" :to="flowRoute(flow.flow)" :title="flowTitle(flow)">
            {{ shortId(flow.flow) }}…
          </RouterLink>
          <span class="hint" :title="groupText(flow)">{{ groupText(flow) }}</span>
          <span class="producing" :class="flow.producing ? 'state-ACTIVE' : 'dim'">
            {{ flow.producing ? 'producing' : 'idle' }}
          </span>
          <button type="button" class="route" :title="flowTitle(flow)" @click="routeFlow(flow)">
            route
          </button>
          <!-- Last, and rendered unconditionally: it is the only thing on the row whose length may
               vary, so it is the only thing that may sit in the track that depends on content. -->
          <span class="mark" :title="markTitle(flow)">{{ mark(flow) }}</span>
        </div>
      </div>
    </template>
  </section>
</template>

<style scoped>
h2 {
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-dim);
  margin: 0;
}

.strip-head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  margin-bottom: 8px;
}

.counts, .dropped, .filter { font-size: 12px; }
.counts { color: var(--fg-dim); }

/* The number an operator is reading. Coloured only when there is something to read — a zero in the
   attention colour is a standing alarm about nothing. */
.work { color: var(--s-establishing); }
.dropped { color: var(--s-failed); }

.filter {
  color: var(--fg-dim);
  cursor: pointer;
  display: flex;
  align-items: baseline;
  gap: 5px;
}

.group { margin-bottom: 10px; }

.group-head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  padding: 2px 0;
  border-bottom: 1px solid var(--line-soft);
  font-size: 12px;
}

.node { font-weight: 600; }
.tally { font-variant-numeric: tabular-nums; }

/* A grid with fixed tracks, never flex: under flex every item is content-sized, so a longer group
   hint would move the mark and the button on every row that has one.
   The **control comes before the mark**, which is the shape the rectangle rows above already use —
   `duplicate` then the server's reason. Both orders satisfy "only the last track may depend on
   content", and this one is the readable half: with the attention filter on every mark is empty by
   definition, so a mark track between the two would be a fixed void in the default view, with
   `route` stranded the width of it away from the flow it routes. Found by screenshotting it. */
.row {
  display: grid;
  grid-template-columns: 1ch 11ch 34ch 10ch 7ch 1fr;
  column-gap: 10px;
  align-items: baseline;
  padding: 2px 0;
  font-size: 12px;
}

/* Listed, and not work. Dimmed rather than hidden — the filter is what hides them, and this is what
   keeps them legible as a different kind of thing when it is off. */
.row.marked { opacity: 0.62; }

.dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--fg-faint);
  align-self: center;
}

.dot.on { background: var(--s-active); }

.hint, .mark, .flow { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.hint { color: var(--fg); }
.mark { color: var(--s-paused); }
.producing { font-size: 11px; }

.route {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 11px;
  padding: 0;
  text-align: left;
}

.route:hover { color: var(--accent); }
.route:focus-visible { outline: 1px solid var(--accent); }

.empty, .fail { font-size: 12px; margin: 0; }
.empty { color: var(--fg-dim); }
.fail { color: var(--s-failed); }
.dim { color: var(--fg-dim); }
</style>
