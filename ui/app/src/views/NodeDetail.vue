<script setup lang="ts">
/**
 * `describe node` — what the agent advertises, what it is observing, and every path touching it.
 *
 * Three things this view is careful about, each of which is a way to get a node wrong:
 *
 * - **Registered is not live.** Registration is durable and survives the agent being down; the lease
 *   is separate and TTL'd, and an expired one is *not* proof this node's workers stopped, which is
 *   why the server freezes rather than reassigning. So liveness is `live` and nothing else.
 * - **`last_seen` is when the lease was taken, not the last heartbeat.** A heartbeat renews the TTL
 *   and deliberately writes nothing — rewriting it would advance the store revision several times a
 *   minute per node forever and wake every agent's long poll, where a spurious wakeup is a worker
 *   restart. A healthy node can show it an hour ago, so it is labelled *lease acquired* and carries
 *   no age: an age beside a timestamp is a staleness reading whatever the label says (`ui.md` §5
 *   trap 3).
 * - **A node can be both ends of a path.** Same node, different domain, which is what the loopback
 *   configuration does. `pathsTouching` checks the two ends independently; see its note.
 *
 * The areas block is the one an operator opens this page for behind a refused request: the two
 * grants are the whole of this project's authority over that node's filesystem, and a node granting
 * `write` on nothing is not a destination at all.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { api } from '@/api/client'
import { renderDomain } from '@/api/types'
import EventLog from '@/components/EventLog.vue'
import { byteSize, capFlagsText, grantText, pathsTouching } from '@/model/detail'
import { plural, shortId } from '@/model/labels'
import { domainRoute, flowRoute, nodeRoute, pathRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'
import { useRead } from '@/stores/read'

const props = defineProps<{ node: string }>()

const fleet = useFleetStore()

// There is no `GET /v1/nodes/{node}`: `ui.md` §9.1 lists one, the mux does not register it and it
// returns 404. Fetch the list and filter, which is what `describe node` does — and here the list is
// already on screen from the poll, so it costs nothing at all.
const node = computed(() => fleet.nodes.find((entry) => entry.name === props.node))

const areas = computed(() => node.value?.capabilities?.areas ?? [])
const fabrics = computed(() => node.value?.capabilities?.fabrics ?? [])

/** The observed domains. The one read this view makes that the fleet poll does not. */
const domainRead = useRead(
  (signal) => api.domains(props.node, signal),
  () => props.node,
)

const observed = computed(() => domainRead.data.value?.domains ?? [])
const domainsSettling = computed(() => domainRead.data.value?.settling === true)
const domainsLoading = domainRead.loading
const domainsError = domainRead.error

const touching = computed(() => pathsTouching(props.node, fleet.paths))
</script>

<template>
  <main class="dt-page">
    <h1 class="dt-title">
      <span class="dt-kind">Node</span>
      <span class="mono">{{ node?.name ?? props.node }}</span>
      <span v-if="node" :class="node.live ? 'live' : 'unleased'">
        {{ node.live ? 'leased' : 'no lease' }}
      </span>
    </h1>

    <p v-if="!node" class="dt-missing">
      No node <span class="mono">{{ props.node }}</span> is registered.
    </p>

    <template v-else>
      <dl class="dt-fields">
        <dt>liveness</dt>
        <dd>
          <template v-if="node.live">leased by <span class="mono">{{ node.instance }}</span></template>
          <!-- Information, not an alarm: registration is durable and survives the agent being down,
               and an expired lease is not proof this node's workers stopped. -->
          <span v-else class="unleased" title="No agent holds this node's lease">
            no lease
          </span>
        </dd>

        <template v-if="node.registered_at">
          <dt>registered</dt>
          <dd class="mono">{{ node.registered_at }}</dd>
        </template>

        <!-- Never an age, and never a health indicator: a heartbeat renews the lease and writes
             nothing, so this is as old as the last time the agent (re)acquired it. -->
        <template v-if="node.last_seen">
          <dt>lease acquired</dt>
          <dd class="mono">{{ node.last_seen }}</dd>
        </template>

        <dt>versions</dt>
        <dd>
          replicator {{ node.capabilities.versions.replicator }},
          protocol {{ node.capabilities.versions.protocol }}
          <!-- The mxl version is the non-obvious one: target_info is produced by one node's
               mxl-fabrics and consumed by another's, so a pair straddling a version boundary is a
               compatibility concern neither agent can see alone. -->
          <span v-if="node.capabilities.versions.mxl || node.capabilities.versions.libfabric" class="dim">
            · worker {{ node.capabilities.versions.proxy ?? '·' }},
            mxl {{ node.capabilities.versions.mxl ?? '·' }},
            libfabric {{ node.capabilities.versions.libfabric ?? '·' }}
          </span>
        </dd>

        <dt>sched_prio</dt>
        <dd>{{ node.capabilities.sched_prio }}</dd>

        <template v-if="node.capabilities.port_range">
          <dt>port range</dt>
          <dd class="mono">{{ node.capabilities.port_range }} (inbound)</dd>
        </template>
      </dl>

      <section class="dt-section">
        <h2>Areas</h2>
        <p v-if="areas.length === 0" class="dt-empty">None advertised.</p>
        <table v-else class="dt-table">
          <thead>
            <tr><th>area</th><th>grants</th><th class="dt-wide">path</th></tr>
          </thead>
          <tbody>
            <tr v-for="area in areas" :key="area.name">
              <td class="mono">{{ area.name }}</td>
              <td :class="{ dim: !area.read && !area.write }">{{ grantText(area) }}</td>
              <!-- Advertised for diagnostics only and may be absent. Guarded rather than assumed. -->
              <td class="dt-wide mono">{{ area.path ?? '·' }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>Fabric attachments</h2>
        <p v-if="fabrics.length === 0" class="dt-empty">None.</p>
        <table v-else class="dt-table">
          <thead>
            <tr>
              <th>provider</th><th>fabric</th><th>address</th><th>device</th>
              <th>max message</th><th class="dt-wide">capabilities</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(fabric, i) in fabrics" :key="`${fabric.provider}-${fabric.fabric}-${i}`">
              <td class="mono">{{ fabric.provider }}</td>
              <td class="mono">{{ fabric.fabric }}</td>
              <td class="mono">{{ fabric.address || '·' }}</td>
              <!-- Not the netdev name for verbs or efa, but it is what an operator matches against
                   `fi_info` when an attachment does not come up. -->
              <td class="mono dim">{{ fabric.device || '·' }}</td>
              <td>{{ byteSize(fabric.max_message_size) }}</td>
              <td class="dt-wide">{{ capFlagsText(fabric.caps_flags) }}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>Domains <span class="dim">observed</span></h2>

        <p v-if="domainsError" class="dt-read-error">{{ domainsError.message }}</p>
        <p v-else-if="domainsLoading" class="dt-empty">Loading…</p>
        <!-- Said explicitly rather than rendering every label with nothing observed beside it,
             which looks exactly like the labels having been lost. -->
        <p v-else-if="domainsSettling" class="dt-empty">Still settling.</p>
        <p v-else-if="observed.length === 0" class="dt-empty">None observed.</p>

        <table v-else class="dt-table">
          <thead>
            <tr><th>domain</th><th>flows</th><th class="dt-wide">labels</th></tr>
          </thead>
          <tbody>
            <!-- A domain replication writes into is listed like any other: a domain is a place, not
                 a direction, and there is exactly one kind of it. -->
            <tr v-for="info in observed" :key="renderDomain(info.domain)">
              <td class="mono">
                <RouterLink class="link" :to="domainRoute(props.node, info.domain)">
                  {{ renderDomain(info.domain) }}
                </RouterLink>
                <span v-if="!info.observed" class="dim"> · labelled, not observed</span>
              </td>
              <td>{{ info.flows?.length ?? 0 }}</td>
              <td class="dt-wide">
                {{ Object.entries(info.labels ?? {}).map(([k, v]) => `${k}=${v}`).sort().join(' · ') }}
              </td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="dt-section">
        <h2>Paths <span class="dim">{{ plural(touching.length, 'leg') }}</span></h2>
        <p v-if="touching.length === 0" class="dt-empty">No paths touch this node.</p>
        <table v-else class="dt-table">
          <thead>
            <tr><th>role</th><th>path</th><th>flow</th><th>peer</th><th>state</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <!-- Keyed on the pair, not on the path: a node that is both ends of one path has two
                 rows here, and that is the answer rather than a duplicate to collapse. -->
            <tr v-for="row in touching" :key="`${row.role}-${row.path.id}`">
              <td>{{ row.role }}</td>
              <td class="mono">
                <RouterLink class="link" :to="pathRoute(row.path.id)" :title="row.path.id">
                  {{ shortId(row.path.id) }}…
                </RouterLink>
              </td>
              <td class="mono">
                <RouterLink class="link" :to="flowRoute(row.path.source.flow)" :title="row.path.source.flow">
                  {{ shortId(row.path.source.flow) }}…
                </RouterLink>
              </td>
              <td class="mono">
                <RouterLink
                  class="link"
                  :to="row.role === 'initiator'
                    ? nodeRoute(row.path.destination.node)
                    : nodeRoute(row.path.source.node)"
                >
                  {{ row.peer }}
                </RouterLink>
              </td>
              <td :class="`state-${row.path.state}`">{{ row.path.state }}</td>
              <td class="dt-wide">{{ row.path.reason }}</td>
            </tr>
          </tbody>
        </table>
      </section>
    </template>

    <!-- **Outside the `v-else`, deliberately.** The endpoint answers for a node with no registration
         at all, because a node's log outlives its paths and its lease — after a deregistration it is
         the only place left that says what happened, which is exactly the page an operator lands on
         holding a name that is no longer in `/v1/nodes`.

         It is also the log that answers "why did every path on edge-01 re-establish at 12:04" in one
         line instead of in fifty identical path entries. -->
    <EventLog :subject="{ kind: 'node', node: props.node }" />
  </main>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.live { color: var(--s-active); font-size: 12px; }
.unleased { color: var(--s-establishing); font-size: 12px; }
</style>
