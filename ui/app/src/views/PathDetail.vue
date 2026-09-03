<script setup lang="ts">
/**
 * `describe path` — the edge, its state, its **refcount**, and a link to the session realising it.
 *
 * A path is the deduplicated edge `(src node, src domain, flow) → (dst node, dst domain)`: derived,
 * recomputed every reconcile, and refcounted. `requests[]` *is* the refcount — N requests whose
 * selectors expand onto this edge share one path, one session and one worker pair, and the path goes
 * away when the last of them is cancelled. That list is therefore the whole answer to *what happens
 * if I delete that request*, which is why it is a field here rather than a footnote.
 *
 * **This is not the session, and the seam is deliberate.** They are 1:1 in practice and separate
 * layers on purpose: a path outlives any particular session, which is re-established whenever either
 * end restarts. Collapsing the two views would quietly assert that a path dies when its workers do,
 * which is the opposite of what the design guarantees — so the session is summarised here and
 * described there.
 *
 * There is no `GET /v1/paths/{id}`: fetch the list and match `id` **exactly**. A shortened ID is
 * fine to display and never to match on.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import EventLog from '@/components/EventLog.vue'
import { endpointText, since } from '@/model/detail'
import { plural } from '@/model/labels'
import { domainRoute, flowRoute, nodeRoute, requestRoute, sessionRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'

const props = defineProps<{ id: string }>()

const fleet = useFleetStore()

const path = computed(() => fleet.paths.find((entry) => entry.id === props.id))

const session = computed(() => path.value?.session)

const holders = computed(() => path.value?.requests ?? [])
</script>

<template>
  <main class="dt-page">
    <h1 class="dt-title">
      <span class="dt-kind">Path</span>
      <span class="mono">{{ props.id }}</span>
      <span v-if="path" :class="`state-${path.state}`">{{ path.state }}</span>
    </h1>

    <!-- A path is derived: one that is not here was not deleted, it stopped being computed, which
         happens the moment the last request naming it is cancelled or parked. Not an error surface,
         and not an explained one either: the sentence is the whole answer, and a tooltip teaching
         the derivation rule to somebody who followed a stale link is training nobody asked for. -->
    <p v-if="!path" class="dt-missing">
      No path <span class="mono">{{ props.id }}</span>.
    </p>

    <template v-else>
      <dl class="dt-fields">
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
          <span v-if="path.destination.disabled" class="dt-parked"> · parked</span>
        </dd>

        <dt>state</dt>
        <dd>
          <span :class="`state-${path.state}`">{{ path.state }}</span>
          <span v-if="path.reason" class="dim"> · {{ path.reason }}</span>
        </dd>

        <template v-if="path.reason_code">
          <dt>reason code</dt>
          <dd class="mono dim">{{ path.reason_code }}</dd>
        </template>

        <!-- The refcount. One holder means deleting it stops this; more than one means it keeps
             running, which is the whole cancellation preview and it needs no dialog to ask for. -->
        <dt>requests</dt>
        <dd>
          <template v-for="(holder, i) in holders" :key="holder">
            <span v-if="i">, </span>
            <RouterLink class="link mono" :to="requestRoute(holder)">{{ holder }}</RouterLink>
          </template>
          <span
            class="dim"
            :title="holders.length > 1
              ? 'Cancelling any one of them leaves this leg running'
              : 'Cancelling it stops this leg'"
          >
            · refcount {{ holders.length }}
          </span>
        </dd>
      </dl>

      <section class="dt-section">
        <h2>Session</h2>

        <!-- Absent on a path in WAITING, and after an idle teardown. Neither is a fault: PAUSED with
             no session is not a contradiction. -->
        <p v-if="!session" class="dt-empty">No session.</p>

        <dl v-else class="dt-fields">
          <dt>id</dt>
          <dd class="mono">
            <RouterLink class="link" :to="sessionRoute(session.id)">{{ session.id }}</RouterLink>
          </dd>

          <dt>fabric</dt>
          <dd class="mono">{{ session.fabric }} / {{ session.interface.provider }}</dd>

          <dt>target</dt>
          <dd v-if="!session.target" class="dim">not running</dd>
          <dd v-else>
            <span class="mono">{{ session.target.node }}</span>
            <span class="dim"> · {{ session.target.state }}</span>
            <span v-if="session.target.started_at" class="dim"> · up {{ since(session.target.started_at) }}</span>
            <span v-if="session.target.restarts" class="restarts"> · {{ plural(session.target.restarts, 'restart') }}</span>
          </dd>

          <dt>initiator</dt>
          <dd v-if="!session.initiator" class="dim">not running</dd>
          <dd v-else>
            <span class="mono">{{ session.initiator.node }}</span>
            <span class="dim"> · {{ session.initiator.state }}</span>
            <span v-if="session.initiator.started_at" class="dim"> · up {{ since(session.initiator.started_at) }}</span>
            <span v-if="session.initiator.restarts" class="restarts"> · {{ plural(session.initiator.restarts, 'restart') }}</span>
          </dd>
        </dl>
      </section>

      <!-- The path is the unit of retention (§12.1), so this is the log the whole feature is
           anchored on: a state and a reason say what is true now, and a path that flapped for ten
           minutes and is ACTIVE again says nothing at all about the ten minutes. It is also the only
           subject with worker log tails, which is why the pane's subject union is a union. -->
      <EventLog :subject="{ kind: 'path', id: props.id }" />
    </template>
  </main>
</template>

<style scoped>
.dim { color: var(--fg-dim); }
.restarts { color: var(--s-degraded); }

/* Three identifiers on one line, and each is its own link. Spaced rather than run together, so the
   node, the domain and the flow read as the triple they are. */
.dt-fields dd .link + .link { margin-left: 10px; }
</style>
