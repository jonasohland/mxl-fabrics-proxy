<script setup lang="ts">
/**
 * `describe session` — the negotiated fabric and interface config, the epoch, and each end.
 *
 * **A session has no identity apart from the path it realises**, which is why there is no
 * `/v1/sessions` and why this view finds its subject by scanning the path list. That is not a
 * missing endpoint to route around: a session is ephemeral and is re-established whenever either end
 * restarts, so the durable thing to hold a link to is the path.
 *
 * Both ends are given the **same** negotiated config and it is pinned for the session's lifetime:
 * the library does no negotiation of its own and requires identical values, so `interface` is one
 * value describing two workers rather than one per side.
 *
 * The epoch is a content hash of the target worker's incarnation, not a counter — it has no ordering,
 * only equality. It changes on every target restart, and that change is what makes the initiator
 * reconnect.
 *
 * What is deliberately not here: `target_info`. The user API never discloses it — it is a set of RDMA
 * rkeys and lives only on the agent API, which the browser is given no route to. That is a property
 * worth preserving rather than a gap to fill.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import type { SessionEndpoint } from '@/api/types'
import { addressText, byteSize, capFlagsText, endpointText, since } from '@/model/detail'
import { domainRoute, flowRoute, nodeRoute, pathRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'

const props = defineProps<{ id: string }>()

const fleet = useFleetStore()

/** Reached through its path, which is the only thing that holds one. */
const path = computed(() => fleet.paths.find((entry) => entry.session?.id === props.id))

const session = computed(() => path.value?.session)

interface End {
  role: 'target' | 'initiator'
  endpoint: SessionEndpoint | undefined
}

/**
 * Target first, which is the order they come up in: the target binds and reports an epoch, then the
 * initiator connects to it. Each is absent until reported — a session in `ESTABLISHING` legitimately
 * has a fabric and an interface config and no endpoints at all.
 */
const ends = computed<End[]>(() => [
  { role: 'target', endpoint: session.value?.target },
  { role: 'initiator', endpoint: session.value?.initiator },
])
</script>

<template>
  <main class="dt-page">
    <h1 class="dt-title">
      <span class="dt-kind">Session</span>
      <span class="mono">{{ props.id }}</span>
      <span v-if="path" :class="`state-${path.state}`">{{ path.state }}</span>
    </h1>

    <p v-if="!path || !session" class="dt-missing">
      No session <span class="mono">{{ props.id }}</span>.
    </p>

    <template v-else>
      <dl class="dt-fields">
        <dt>path</dt>
        <dd class="mono">
          <RouterLink class="link" :to="pathRoute(path.id)">{{ path.id }}</RouterLink>
        </dd>

        <dt>source</dt>
        <dd class="mono">
          <RouterLink class="link" :to="nodeRoute(path.source.node)">{{ path.source.node }}</RouterLink>
          <RouterLink class="link" :to="domainRoute(path.source.node, path.source.domain)">
            {{ path.source.domain }}
          </RouterLink>
          <RouterLink class="link" :to="flowRoute(path.source.flow)">{{ path.source.flow }}</RouterLink>
        </dd>

        <dt>destination</dt>
        <dd class="mono">
          <RouterLink class="link" :to="domainRoute(path.destination.node, path.destination.domain)">
            {{ endpointText(path.destination) }}
          </RouterLink>
        </dd>

        <dt>state</dt>
        <dd>
          <span :class="`state-${path.state}`">{{ path.state }}</span>
          <span v-if="path.reason" class="dim"> · {{ path.reason }}</span>
        </dd>

        <dt>fabric</dt>
        <dd class="mono">{{ session.fabric }}</dd>

        <dt>provider</dt>
        <dd>{{ session.interface.provider }} <span class="dim">(pinned)</span></dd>

        <dt>interface</dt>
        <dd>
          {{ capFlagsText(session.interface.caps_flags) }}
          <span class="dim"> · max message {{ byteSize(session.interface.max_message_size) }}</span>
        </dd>

        <dt>epoch</dt>
        <!-- Absent until the target reports it, which is a step rather than a fault. -->
        <dd :class="session.epoch ? 'mono' : 'dim'">{{ session.epoch ?? 'not reported' }}</dd>
      </dl>

      <section class="dt-section">
        <h2>Workers</h2>
        <table class="dt-table">
          <thead>
            <tr><th>role</th><th>node</th><th>state</th><th>endpoint</th><th>restarts</th><th>up</th><th class="dt-wide">reason</th></tr>
          </thead>
          <tbody>
            <tr v-for="end in ends" :key="end.role">
              <td>{{ end.role }}</td>
              <td class="mono">
                <RouterLink v-if="end.endpoint" class="link" :to="nodeRoute(end.endpoint.node)">
                  {{ end.endpoint.node }}
                </RouterLink>
                <span v-else class="dim">·</span>
              </td>
              <!-- A worker can legitimately sit in `starting` for a minute on a node that is
                   re-establishing in bulk: the agent paces worker starts through a token bucket, and
                   the reason says so. That is rate control working, not a fault. -->
              <td :class="end.endpoint ? `worker-${end.endpoint.state}` : 'dim'">
                {{ end.endpoint?.state ?? 'not running' }}
              </td>
              <td class="mono">{{ addressText(end.endpoint) }}</td>
              <td :class="{ restarts: (end.endpoint?.restarts ?? 0) > 0 }">
                {{ end.endpoint?.restarts ?? '·' }}
              </td>
              <td>{{ (end.endpoint && since(end.endpoint.started_at)) ?? '·' }}</td>
              <td class="dt-wide">{{ end.endpoint?.reason }}</td>
            </tr>
          </tbody>
        </table>
      </section>
    </template>
  </main>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.restarts { color: var(--s-degraded); }

.worker-ready { color: var(--s-active); }
.worker-starting { color: var(--s-establishing); }
.worker-failed { color: var(--s-failed); }

.dt-fields dd .link + .link { margin-left: 10px; }
</style>
