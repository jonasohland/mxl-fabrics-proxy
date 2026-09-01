<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import { RouterLink, RouterView } from 'vue-router'

import Banners from '@/components/Banners.vue'
import NamespacePicker from '@/components/NamespacePicker.vue'
import PendingBar from '@/components/PendingBar.vue'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()

// One timer for the whole application, started once here. Every user-API read costs the server a
// full store load and a reconcile, so a per-view poll would multiply that by the number of views.
onMounted(() => fleet.start())
onBeforeUnmount(() => fleet.stop())
</script>

<template>
  <header class="bar">
    <RouterLink to="/" class="brand">mxl-replicator</RouterLink>
    <!-- Beside the brand rather than beside the namespace picker: both are fleet-wide, and the
         picker drives the workspace, which is the one screen that is scoped to one namespace. -->
    <RouterLink
      to="/topology"
      class="nav"
      title="Nodes as vertices, paths as edges"
    >topology</RouterLink>
    <NamespacePicker />
    <span class="spacer" />
    <span v-if="fleet.leader" class="dim mono">leader {{ fleet.leader }}</span>
  </header>

  <Banners />

  <RouterView v-if="fleet.loaded" />
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
  color: var(--fg-dim);
  text-decoration: none;
  font-size: 12px;
}

.nav:hover { color: var(--fg); }
.nav.router-link-active { color: var(--accent); }

.empty {
  padding: 24px;
  color: var(--fg-dim);
}

.dim { color: var(--fg-dim); }
</style>
