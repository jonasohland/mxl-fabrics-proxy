<script setup lang="ts">
/**
 * `status`'s job: count the fleet, then name **only what is not active** (`ui.md` §7).
 *
 * Not a list. The CLI's answer to "is anything wrong" is two lines rather than a screen to scan.
 * Two things it surfaces that no per-request view can are kept for that reason: nodes registered
 * but not leased, and nodes advertising no writable area — the first thing to check behind a
 * refused request.
 *
 * **Fleet-wide, never namespace-scoped, and it is the one screen the current namespace does not
 * mark.** Health is a fleet fact; scoped — or even tinted — by a namespace, "is anything wrong"
 * starts answering "is anything wrong in the namespace I happen to have selected", which is the
 * wrong answer at 3am (`ui.md` §7b). That is the trap `stores/current.ts` introduces and this is
 * where it would bite, so the rule is written down in both places.
 *
 * **Every count is a link into the table it counts.** That is the whole reason a filter lives in
 * the URL rather than in a component: `12 FAILED` and `/paths?state=FAILED` are the same sentence,
 * and the screen that says "something is wrong" should be one click from the list of what. A count
 * of zero is not a link — there is nothing behind it but an empty table with a filter on.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { PATH_STATES, REQUEST_STATES, writableAreas } from '@/api/types'
import type { RequestState } from '@/api/types'
import { byWorstFirst } from '@/model/state'
import { plural } from '@/model/labels'
import { nodeRoute, nodesRoute, pathsRoute, requestRoute, requestsRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()

function tally<T>(items: readonly T[], stateOf: (item: T) => RequestState) {
  const counts = new Map<RequestState, number>()
  for (const item of items) {
    const state = stateOf(item)
    counts.set(state, (counts.get(state) ?? 0) + 1)
  }
  return counts
}

// Rendered with a floor of zero over the full vocabulary in fixed order: a state nothing is
// currently in must show 0 rather than vanish, or the row shows a gap where it should show a floor.
const requestCounts = computed(() => {
  const counts = tally(fleet.requests, (request) => request.status.state)
  return REQUEST_STATES.map((state) => ({ state, count: counts.get(state) ?? 0 }))
})

const pathCounts = computed(() => {
  const counts = tally(fleet.paths, (path) => path.state)
  return PATH_STATES.map((state) => ({ state, count: counts.get(state) ?? 0 }))
})

const sessions = computed(() => fleet.paths.filter((path) => path.session !== undefined).length)
const leased = computed(() => fleet.nodes.filter((node) => node.live).length)

/**
 * Registered but not leased. Information, not an alarm — the registration is durable and survives
 * the agent being down, and an expired lease is not proof the workers stopped, which is why the
 * server freezes rather than reassigning.
 */
const unleased = computed(() => fleet.nodes.filter((node) => !node.live))

/** A node advertising no writable area is not a destination at all. */
const noWritableArea = computed(() => fleet.nodes.filter((node) => writableAreas(node).length === 0))

/** Only what is not active, worst-first, each with its reason. */
const attention = computed(() =>
  fleet.requests
    .filter((request) => request.status.state !== 'ACTIVE' && request.status.state !== 'DISABLED')
    .sort(byWorstFirst((request) => request.status.state)),
)

const disabledCount = computed(
  () => fleet.requests.filter((request) => request.status.state === 'DISABLED').length,
)
</script>

<template>
  <main class="page">
    <section class="counts">
      <div class="group">
        <h2>
          <RouterLink class="link" :to="requestsRoute()">Requests</RouterLink>
          <span class="dim">{{ fleet.requests.length }}</span>
        </h2>
        <ul>
          <li v-for="entry in requestCounts" :key="entry.state" :class="{ zero: entry.count === 0 }">
            <component
              :is="entry.count ? RouterLink : 'span'"
              class="link row"
              :to="entry.count ? requestsRoute({ state: entry.state }) : undefined"
            >
              <span :class="`state-${entry.state}`">{{ entry.state }}</span>
              <span class="mono">{{ entry.count }}</span>
            </component>
          </li>
        </ul>
      </div>

      <div class="group">
        <h2>
          <RouterLink class="link" :to="pathsRoute()">Paths</RouterLink>
          <span class="dim">{{ fleet.paths.length }}</span>
        </h2>
        <ul>
          <li v-for="entry in pathCounts" :key="entry.state" :class="{ zero: entry.count === 0 }">
            <component
              :is="entry.count ? RouterLink : 'span'"
              class="link row"
              :to="entry.count ? pathsRoute({ state: entry.state }) : undefined"
            >
              <span :class="`state-${entry.state}`">{{ entry.state }}</span>
              <span class="mono">{{ entry.count }}</span>
            </component>
          </li>
        </ul>
      </div>

      <div class="group">
        <h2>Fleet</h2>
        <ul>
          <li>
            <RouterLink class="link row" :to="nodesRoute()">
              <span>nodes registered</span><span class="mono">{{ fleet.nodes.length }}</span>
            </RouterLink>
          </li>
          <li>
            <RouterLink class="link row" :to="nodesRoute({ lease: 'leased' })">
              <span>agents leased</span><span class="mono">{{ leased }}</span>
            </RouterLink>
          </li>
          <!-- Sessions are counted off the path list and have no page of their own: every session
               is some path's session, so the paths table filtered to the ones running *is* the
               session index (`ui.md` §7 keeps the two apart at the detail level, which it still
               does). -->
          <li>
            <RouterLink class="link row" :to="pathsRoute({ session: 'running' })">
              <span>sessions</span><span class="mono">{{ sessions }}</span>
            </RouterLink>
          </li>
          <li><span class="row"><span>namespaces</span><span class="mono">{{ fleet.namespaces.length }}</span></span></li>
        </ul>
      </div>
    </section>

    <!-- Two facts no per-request view can surface. Labels only: what they imply is training. -->
    <section v-if="unleased.length || noWritableArea.length" class="notes">
      <p v-if="unleased.length" title="Registered, with no agent holding the lease">
        <span class="label">not leased</span>
        <span class="mono">
          <template v-for="(node, i) in unleased" :key="node.name">
            <span v-if="i">, </span>
            <RouterLink class="link" :to="nodeRoute(node.name)">{{ node.name }}</RouterLink>
          </template>
        </span>
      </p>
      <p v-if="noWritableArea.length" title="Advertises no area it grants writing on">
        <span class="label">no writable area</span>
        <span class="mono">
          <template v-for="(node, i) in noWritableArea" :key="node.name">
            <span v-if="i">, </span>
            <RouterLink class="link" :to="nodeRoute(node.name)">{{ node.name }}</RouterLink>
          </template>
        </span>
      </p>
    </section>

    <section class="attention">
      <h2>Not active <span class="dim">{{ attention.length }}</span></h2>

      <p v-if="attention.length === 0" class="ok">Everything is active.</p>

      <table v-else>
        <tbody>
          <tr v-for="request in attention" :key="request.id">
            <!-- To the request, not to the namespace it is in. This row *is* the finding, and the
                 next question is always "why" — which is the per-source breakdown and the reason on
                 each path, one click away rather than a namespace to search. -->
            <td class="id">
              <RouterLink :to="requestRoute(request.id)" class="mono">{{ request.id }}</RouterLink>
            </td>
            <td class="state" :class="`state-${request.status.state}`">{{ request.status.state }}</td>
            <td class="reason">{{ request.status.reason }}</td>
          </tr>
        </tbody>
      </table>

      <!-- Parked intent stays countable without being loud: it does not belong in the list of
           things that are wrong, but a fleet with fifteen parked legs is a fact an operator should
           be able to see without hunting for it. -->
      <p v-if="disabledCount" class="dim" title="Every destination parked">
        {{ plural(disabledCount, 'request') }} disabled
      </p>
    </section>
  </main>
</template>

<style scoped>
.page {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 20px;
  overflow: auto;
}

.counts {
  display: flex;
  gap: 32px;
  flex-wrap: wrap;
}

/* Flex rather than inline: Vue's template compiler condenses whitespace containing a newline away
   entirely, so a heading whose two parts sit on two lines of the template would render them run
   together. The gap is declared rather than typed. */
h2 {
  display: flex;
  align-items: baseline;
  gap: 6px;
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-dim);
  margin: 0 0 8px;
}

.group ul {
  list-style: none;
  margin: 0;
  padding: 0;
  min-width: 190px;
}

.group li { padding: 2px 0; }

/* The flex lives on the row rather than the `li`, because the row is a link and a link has to be
   the thing with the width — a count in the far column that was not inside the anchor would be
   beside the target rather than in it. */
.group .row {
  display: flex;
  justify-content: space-between;
  gap: 20px;
}

.group li.zero { color: var(--fg-faint); }
.group li.zero span { color: inherit !important; }

/* A count of zero is a `span`, so this only ever reaches the ones with something behind them. */
.group h2 .link { color: inherit; }

.dim { color: var(--fg-dim); font-weight: 400; }
.ok { color: var(--s-active); }

.notes p {
  display: grid;
  grid-template-columns: 20ch 1fr;
  column-gap: 12px;
  margin: 0 0 3px;
  color: var(--fg-dim);
}

.notes .label { color: var(--s-establishing); }

.attention table {
  border-collapse: collapse;
  width: 100%;
}

.attention td {
  padding: 3px 12px 3px 0;
  vertical-align: top;
  border-bottom: 1px solid var(--line-soft);
}

/* The id and state columns shrink to their content and the reason absorbs the slack. Without this
   the auto layout hands the spare width to the state column, which puts a hand's width of nothing
   between a state and the sentence explaining it. */
.attention .id,
.attention .state { width: 1%; white-space: nowrap; }
.attention .reason { width: 100%; }
.attention .id a { color: var(--accent); text-decoration: none; }
.attention .id a:hover { text-decoration: underline; }
.attention .reason { color: var(--fg-dim); }
</style>
