<script setup lang="ts">
/**
 * `describe request` — the stored spec, the per-source breakdown, the per-path breakdown, and the
 * exclusions. Where "2 of 3 active" and "studio-b is dark" live.
 *
 * Four things this view holds that a list cannot:
 *
 * - **The per-source breakdown leads.** A request's expansion is the cross product of its sources
 *   and its enabled destinations, so a fan-in folds twelve cameras to one word and that word cannot
 *   say which one is dark. `status.sources[]` is the same set sliced the way an operator asks about
 *   it, and it is joined against the *spec* so that a source which expanded to nothing still gets a
 *   row — that is exactly the source somebody is looking for.
 * - **A parked destination gets a row like any other.** This is the page an operator opens to decide
 *   whether to switch one back on, so what is off has to be as legible here as what is on. `off` is
 *   a value, not an absence.
 * - **The state is the server's, never recomputed.** `Compute` also folds *leg failures* — pairings
 *   refused during validation, which produce no path at all — so a request whose every materialised
 *   path is `ACTIVE` beside one invalid pairing is `PARTIAL` on the wire and would fold to `ACTIVE`
 *   here. Those failures are absent from `status.paths[]` and there is nothing to recompute them
 *   from (`ui-plan.md` §4).
 * - **A request can list a path it does not hold.** `ui.md` §5 trap 14, and this is the one screen
 *   where it is worth *rendering* rather than merely defending against: the loser of a namespace
 *   overlap goes `INVALID` and still lists the contested path with the incumbent's state, so a
 *   request carrying nothing shows `{"ACTIVE": 1}` in its own counts. `/v1/paths` is the authority,
 *   and each row says which of the two it is.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { REQUEST_STATES, renderDomain } from '@/api/types'
import type { Path, PathStatus, SourceStatus } from '@/api/types'
import { endpointText, providerText, since } from '@/model/detail'
import { domainSelectorLabel, plural, selectorLabel, shortId, sourceRef } from '@/model/labels'
import { countsInOrder } from '@/model/state'
import { domainRoute, flowRoute, namespaceRoute, nodeRoute, pathRoute, requestRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'

const props = defineProps<{ namespace: string; name: string }>()

const fleet = useFleetStore()

/** The identity is the pair, never the name alone — two shows both naming a request `wall` is how a
 *  view keyed on one silently merges them. */
const id = computed(() => `${props.namespace}/${props.name}`)

const request = computed(() => fleet.requests.find((entry) => entry.id === id.value))

const status = computed(() => request.value?.status)

const counts = computed(() => countsInOrder(status.value?.counts, REQUEST_STATES))

const labels = computed(() =>
  Object.entries(request.value?.labels ?? {}).sort(([a], [b]) => a.localeCompare(b)),
)

/** The spec's sources, each joined to its own slice of the status. Driven by the spec, so a source
 *  that matched nothing is a row rather than a gap. */
const sources = computed(() =>
  (request.value?.sources ?? []).map((source, index) => ({
    index,
    source,
    row: (status.value?.sources ?? [])[index] as SourceStatus | undefined,
  })),
)

const pathsById = computed(() => new Map(fleet.paths.map((path) => [path.id, path])))

/**
 * The paths this request lists, each with the answer to *does it actually hold this one*.
 *
 * `held` is `path.requests[]` and nothing else. A path missing from `/v1/paths` entirely is a
 * different case again and is left as such: the two reads land together, so it means the path went
 * away between the reconcile that built this status and the one that built that list.
 */
interface PathRow {
  listed: PathStatus
  live: Path | undefined
  held: boolean
  /** The other holders, which is what makes a shared leg legible without a second read. */
  others: string[]
}

const pathRows = computed<PathRow[]>(() =>
  (status.value?.paths ?? []).map((listed) => {
    const live = pathsById.value.get(listed.id)
    const holders = live?.requests ?? []
    return {
      listed,
      live,
      held: holders.includes(id.value),
      others: holders.filter((holder) => holder !== id.value),
    }
  }),
)

const notHeld = computed(() => pathRows.value.filter((row) => row.live && !row.held).length)
</script>

<template>
  <main class="dt-page">
    <h1 class="dt-title">
      <span class="dt-kind">Request</span>
      <span class="mono">{{ id }}</span>
      <span v-if="status" :class="`state-${status.state}`">{{ status.state }}</span>
    </h1>

    <!-- The identity is the pair, so a name that exists in another namespace is not this one. -->
    <p v-if="!request" class="dt-missing">
      No request <span class="mono">{{ id }}</span>.
    </p>

    <template v-else-if="request && status">
      <dl class="dt-fields">
        <dt>namespace</dt>
        <dd class="mono">
          <RouterLink class="link" :to="namespaceRoute(props.namespace)">{{ props.namespace }}</RouterLink>
        </dd>

        <dt>created</dt>
        <dd class="mono">{{ request.created_at }} <span class="dim">({{ since(request.created_at) }} ago)</span></dd>

        <template v-if="request.updated_at">
          <dt>updated</dt>
          <dd class="mono">{{ request.updated_at }} <span class="dim">({{ since(request.updated_at) }} ago)</span></dd>
        </template>

        <!-- A pin is honoured or the request fails; it is never substituted, so nothing here reads
             as a fallback. Landing on tcp when verbs was asked for is a performance cliff whose
             symptom looks like a source problem. -->
        <template v-if="request.provider">
          <dt>provider</dt>
          <dd>{{ providerText(request.provider) }} <span class="dim">(pinned)</span></dd>
        </template>

        <template v-if="request.idle_teardown_ms !== undefined">
          <dt>idle teardown</dt>
          <dd>{{ request.idle_teardown_ms > 0 ? `${request.idle_teardown_ms} ms` : 'never' }}</dd>
        </template>

        <template v-if="request.sched_prio !== undefined && request.sched_prio !== null">
          <dt>sched_prio</dt>
          <dd>{{ request.sched_prio }}</dd>
        </template>

        <!-- Labels ride into worker metrics and, with the namespace, scope `apply --prune` —
             removing one changes what can cancel this request. -->
        <template v-if="labels.length">
          <dt>labels</dt>
          <dd>
            <span v-for="[key, value] in labels" :key="key" class="mono chip">{{ key }}={{ value }}</span>
          </dd>
        </template>

        <dt>state</dt>
        <dd>
          <span :class="`state-${status.state}`">{{ status.state }}</span>
          <span class="dim"> · {{ status.reason }}</span>
        </dd>

        <!-- The full vocabulary in fixed order with a floor of zero: `status.counts` omits zeros, so
             a row built from its keys shows a gap where it should show a floor. -->
        <dt>paths</dt>
        <dd class="tally">
          <span v-for="entry in counts" :key="entry.state" :class="entry.count ? `state-${entry.state}` : 'zero'">
            {{ entry.state }} {{ entry.count }}
          </span>
        </dd>
      </dl>

      <section class="dt-section">
        <h2>Sources <span class="dim">{{ plural(sources.length, 'entry', 'entries') }}</span></h2>
        <table class="dt-table">
          <thead>
            <tr><th></th><th>node</th><th>domain</th><th>select</th><th>state</th><th>paths</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <!-- Spelled `sources[i]` because that is how the server's own refusals spell it:
                 `duplicate_source_flow` and `same_endpoint` name their operands by index. -->
            <tr v-for="entry in sources" :key="entry.index">
              <td class="dim mono">{{ sourceRef(entry.index) }}</td>
              <td class="mono">
                <RouterLink class="link" :to="nodeRoute(entry.source.node)">{{ entry.source.node }}</RouterLink>
              </td>
              <!-- A name is a place; a label set is a standing query, and a domain labelled tomorrow
                   joins it. Rendered differently for that reason. -->
              <td class="mono">
                <RouterLink
                  v-if="entry.source.domain.name"
                  class="link"
                  :to="domainRoute(entry.source.node, entry.source.domain.name)"
                >
                  {{ domainSelectorLabel(entry.source.domain) }}
                </RouterLink>
                <span v-else class="query">{{ domainSelectorLabel(entry.source.domain) }}</span>
              </td>
              <td>{{ selectorLabel(entry.source.select) }}</td>
              <td :class="entry.row ? `state-${entry.row.state}` : 'dim'">{{ entry.row?.state ?? '·' }}</td>
              <td>{{ entry.row?.paths?.length ?? 0 }}</td>
              <td class="dt-wide">{{ entry.row?.reason }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>Destinations <span class="dim">{{ plural(request.destinations.length, 'entry', 'entries') }}</span></h2>
        <table class="dt-table">
          <thead>
            <tr><th>node</th><th>domain</th><th>provider</th><th class="dt-wide">state</th></tr>
          </thead>
          <tbody>
            <tr v-for="dst in request.destinations" :key="endpointText(dst)" :class="{ 'dt-parked': dst.disabled }">
              <td class="mono">
                <RouterLink class="link" :to="nodeRoute(dst.node)">{{ dst.node }}</RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="domainRoute(dst.node, dst.domain)">
                  {{ renderDomain(dst.domain) }}
                </RouterLink>
              </td>
              <!-- Overrides the request-level pin for this destination alone rather than
                   intersecting with it: "verbs here, tcp there" is an ordinary request. -->
              <td>{{ dst.provider ? providerText(dst.provider) : '·' }}</td>
              <!-- Parked, not deleted: the entry stays in the spec and expands to nothing. An apply
                   from a manifest that omits the flag turns the leg back on. -->
              <td class="dt-wide">{{ dst.disabled ? 'parked' : 'enabled' }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>
          Paths <span class="dim">{{ pathRows.length }} listed</span>
          <!-- A tally, not a sentence: a heading holds the heading and a count, and everything of
               variable length is in the title. -->
          <span v-if="notHeld" class="contested" title="Listed with the holder's state. /v1/paths says another request holds it.">
            · {{ notHeld }} not held
          </span>
        </h2>

        <p v-if="pathRows.length === 0" class="dt-empty">No paths.</p>
        <table v-else class="dt-table">
          <thead>
            <tr><th>path</th><th>flow</th><th>destination</th><th>state</th><th>held by</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <tr v-for="row in pathRows" :key="row.listed.id" :class="{ foreign: row.live && !row.held }">
              <td class="mono">
                <RouterLink class="link" :to="pathRoute(row.listed.id)" :title="row.listed.id">
                  {{ shortId(row.listed.id) }}…
                </RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="flowRoute(row.listed.source.flow)" :title="row.listed.source.flow">
                  {{ shortId(row.listed.source.flow) }}…
                </RouterLink>
              </td>
              <td class="mono">{{ endpointText(row.listed.destination) }}</td>
              <td :class="`state-${row.listed.state}`">{{ row.listed.state }}</td>
              <td>
                <template v-if="!row.live"><span class="dim">gone</span></template>
                <template v-else-if="!row.held">
                  <span class="contested">not this one</span>
                  <template v-for="other in row.others" :key="other">
                    {{ ' ' }}
                    <RouterLink class="link mono" :to="requestRoute(other)">{{ other }}</RouterLink>
                  </template>
                </template>
                <!-- The refcount, and the answer to "what happens if I cancel this": a leg another
                     request also names keeps running. -->
                <template v-else-if="row.others.length">
                  <span class="dim">this + </span>
                  <template v-for="other in row.others" :key="other">
                    <RouterLink class="link mono" :to="requestRoute(other)">{{ other }}</RouterLink>
                    {{ ' ' }}
                  </template>
                </template>
                <span v-else class="dim">this alone</span>
              </td>
              <td class="dt-wide">{{ row.listed.reason }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <!-- A path that does not exist has no status to carry a reason, so a flow the expansion
           skipped is invisible in a paths-only rendering. "Did not match the labels" is never listed
           — that set is unbounded and is the ordinary case. -->
      <section v-if="status.excluded?.length || status.excluded_dropped" class="dt-section">
        <h2>Excluded</h2>
        <table class="dt-table">
          <thead>
            <tr><th>node</th><th>domain</th><th>flow</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <tr v-for="ex in status.excluded ?? []" :key="`${ex.node}/${ex.domain}/${ex.flow}`">
              <td class="mono">
                <RouterLink class="link" :to="nodeRoute(ex.node)">{{ ex.node }}</RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="domainRoute(ex.node, ex.domain)">{{ ex.domain }}</RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="flowRoute(ex.flow)" :title="ex.flow">{{ shortId(ex.flow) }}…</RouterLink>
              </td>
              <td class="dt-wide">written by this node</td>
            </tr>
          </tbody>
        </table>
        <!-- A silent cap reads as "nothing else was excluded", which is the one thing this list must
             not say when it is untrue. -->
        <p v-if="status.excluded_dropped" class="dt-note">and {{ status.excluded_dropped }} more</p>
      </section>
    </template>
  </main>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.zero { color: var(--fg-faint); }

.tally {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 12px;
  font-size: 12px;
}

.chip { margin-right: 10px; }

/* A standing query rather than a place: it matches domains that do not exist yet. */
.query { color: var(--s-paused); }

.contested { color: var(--s-invalid); font-size: 12px; }
.foreign td { opacity: 0.75; }
</style>
