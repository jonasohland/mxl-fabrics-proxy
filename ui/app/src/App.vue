<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, watch } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'

import { authRequired, clearToken, token } from '@/api/auth'
import Banners from '@/components/Banners.vue'
import NamespacePicker from '@/components/NamespacePicker.vue'
import PendingBar from '@/components/PendingBar.vue'
import TokenGate from '@/components/TokenGate.vue'
import { healthRoute, nodesRoute, pathsRoute, requestsRoute, topologyRoute } from '@/router'
import { useCurrentStore } from '@/stores/current'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()
const current = useCurrentStore()
const route = useRoute()

/**
 * Whether this browser is carrying a token, which is a different question from whether one is
 * required (`api/auth.ts`) and is why the control below exists at all: the deployment that injects
 * the header upstream shows nothing here, and the one where somebody pasted a fleet-wide secret into
 * a shared workstation shows a way to take it back out.
 */
const held = computed(() => token.value !== '')

// One timer for the whole application, started once here. Every user-API read costs the server a
// full store load and a reconcile, so a per-view poll would multiply that by the number of views.
onMounted(() => fleet.start())
onBeforeUnmount(() => fleet.stop())

/**
 * The URL wins over the picker whenever it names a namespace.
 *
 * Arriving on `/ns/nab` by link, by back button or by typing must leave the picker reading `nab` —
 * a header claiming one namespace above a workspace showing another is worse than either. The flow
 * the other way is the picker's own (`NamespacePicker.vue`), and it is the only direction that
 * navigates. One watcher, in one place, rather than a `router.afterEach` that would put the store
 * in `router.ts`.
 */
watch(
  () => route.params['namespace'],
  (name) => {
    if (typeof name === 'string' && name !== '') current.set(name)
  },
  { immediate: true },
)
</script>

<template>
  <header class="bar">
    <RouterLink to="/" class="brand">mxl-replicator</RouterLink>

    <!-- First, and next to the brand: the workspace is where an operator spends the day, and it was
         the one screen with no control in this bar. It is also the only scoped thing here — the
         picker sets a scope on the workspace and a highlight on everything to the right of it. -->
    <NamespacePicker />

    <!-- The fleet-wide reads, as a group, because that is what they have in common: none of them is
         namespace-scoped and none of them writes. `ui.md` §7's three verbs, in altitude order —
         health counts and names only findings, the three tables list so a name can be found, and
         the topology draws what no table can. -->
    <nav class="nav">
      <RouterLink class="home" :to="healthRoute()" title="Counts, then only what is not active">health</RouterLink>
      <RouterLink :to="nodesRoute()" title="Every node registered, with its areas and grants">nodes</RouterLink>
      <RouterLink :to="pathsRoute()" title="Every edge the fleet is maintaining">paths</RouterLink>
      <RouterLink :to="requestsRoute()" title="Every request, across every namespace">requests</RouterLink>
      <RouterLink :to="topologyRoute()" title="Nodes as vertices, paths as edges">topology</RouterLink>
    </nav>

    <span class="spacer" />

    <!-- Shown only where a token is actually held, which is the one case in which it says something
         — everywhere else it would advertise an auth model this deployment does not use. -->
    <button
      v-if="held"
      type="button"
      class="forget"
      title="Forget the bearer token kept in this browser"
      @click="clearToken"
    >token · forget</button>

    <span v-if="fleet.leader" class="dim mono">leader {{ fleet.leader }}</span>
  </header>

  <Banners />

  <!-- Before `loaded`, not after: the first read is the one that 401s, so a gate that waited for a
       successful poll would sit behind `Loading…` forever. -->
  <TokenGate v-if="authRequired" />
  <RouterView v-else-if="fleet.loaded" />
  <main v-else class="empty">Loading…</main>

  <!-- Outside the router view on purpose: the staged set is fleet-wide — an edit carries its
       namespace in its request id — so a bar that lived on one screen would discard authored work
       the moment the operator navigated. -->
  <PendingBar />
</template>

<style scoped>
.bar {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 8px 14px;
  border-bottom: 1px solid var(--line);
  background: var(--bg-raised);
  flex: 0 0 auto;
}

.brand {
  color: var(--fg);
  text-decoration: none;
  font-weight: 600;
}

.brand:hover { color: var(--accent); }

.nav {
  display: flex;
  gap: 12px;
  align-items: baseline;
}

.nav a {
  color: var(--fg-dim);
  text-decoration: none;
  font-size: 12px;
}

.nav a:hover { color: var(--fg); }

/* A detail view marks its own list: `/nodes/edge-01` lights `nodes`, which is where the operator
   came from and where back goes. That is `router-link-active`'s prefix match doing the right thing
   — except for `/`, which prefixes everything, so health takes the exact match instead. */
.nav a.router-link-active { color: var(--accent); }
.nav a.home.router-link-active:not(.router-link-exact-active) { color: var(--fg-dim); }

/* Text in the bar, not a button-shaped thing: it belongs to the same row of status as the leader
   name beside it, and giving a destructive-ish action a border here would make it the loudest
   control in the header. */
.forget {
  background: none;
  border: 0;
  padding: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
}

.forget:hover { color: var(--fg); }

.empty {
  padding: 24px;
  color: var(--fg-dim);
}

.dim { color: var(--fg-dim); }
</style>
