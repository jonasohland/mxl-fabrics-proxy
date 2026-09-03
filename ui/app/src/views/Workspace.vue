<script setup lang="ts">
/**
 * One namespace, and the mode decides which screens it has.
 *
 * **The matrix is a matrix only over an `exclusive` namespace.** Two requests can otherwise expand
 * onto one path, and then a cell stops meaning what it looks like: two cells are one session and one
 * worker pair, the cell counts stop summing to what lands on the node, and clearing either one
 * cancels a request and stops nothing while going dark exactly as if it had. That last one is the
 * dangerous one — nothing breaks, so the operator believes they tore it down and does not come back
 * when the egress is still there.
 *
 * A `shared` namespace is **not the matrix greyed out**. Read-only was the cheapest thing that was
 * not wrong to click; it was never right to look at, because the grid still draws two lit cells for
 * one path. It gets the ledger, where a claim rather than an intent is the object.
 *
 * The ledger is offered over **both** modes, which `ui.md` §7c argues for directly: nothing in it
 * requires `shared`, and inside an exclusive namespace the claims list degenerates into the plain
 * path list `describe path` gives — still worth having beside the grid. One component, both modes.
 *
 * The grid choice is **shown and disabled** in a shared namespace rather than omitted, with the
 * reason on it. Omission produces "where is the grid?", which is a worse question than a control
 * that says why it is off — the same reason the destination editor lists a node with no writable
 * area rather than hiding it.
 *
 * **Which screen is in the URL, and the tabs are links.** It was a local `ref`, which made
 * `/ns/nab` name the namespace and not the screen: the claims view of an exclusive namespace could
 * be reached but not linked, and a browser's back button walked over it without stopping. The
 * choice still survives a namespace switch — the picker carries the view across (`NamespacePicker`)
 * — but it now survives a reload and a paste as well.
 */
import { computed, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import Ledger from './Ledger.vue'
import Matrix from './Matrix.vue'
import type { WorkspaceView } from '@/router'
import { claimsRoute, gridRoute } from '@/router'
import { useNamespaceStore } from '@/stores/namespaces'

/** `view` is absent on bare `/ns/:namespace`, which asks for whichever screen the mode allows. */
const props = defineProps<{ namespace: string; view?: string }>()

const router = useRouter()
const namespaces = useNamespaceStore()
const mode = computed(() => namespaces.mode(props.namespace))

const asked = computed<WorkspaceView | undefined>(() =>
  props.view === 'grid' || props.view === 'claims' ? props.view : undefined)

/** The grid is only ever a grid here; anywhere else the choice resolves to the ledger. */
const view = computed<WorkspaceView>(() =>
  mode.value === 'exclusive' ? asked.value ?? 'grid' : 'claims')

/**
 * A URL asking for the grid of a shared namespace is corrected rather than rendered.
 *
 * The alternative is an address bar that says `grid` beside a ledger, which is worse than either
 * screen — the whole point of putting the view in the URL is that the URL names what is on screen.
 * Safe to do on arrival because the namespace list lands in the same poll as everything else and
 * `App.vue` renders no route until the first one has: this cannot fire against a mode it has not
 * read yet. `replace`, so the back button does not land on the URL that was just corrected.
 */
watch(
  [() => props.namespace, asked, mode],
  () => {
    if (asked.value === 'grid' && mode.value !== 'exclusive') {
      void router.replace(claimsRoute(props.namespace))
    }
  },
  { immediate: true },
)
</script>

<template>
  <nav class="views">
    <RouterLink
      v-if="mode === 'exclusive'"
      class="view"
      :class="{ on: view === 'grid' }"
      :to="gridRoute(namespace)"
      title="Sources down, destinations across, a request as a rectangle"
    >
      grid
    </RouterLink>
    <!-- Shown and disabled with the reason, never omitted: "where is the grid?" is a worse question
         than a control that says why it is off. A `<span>` rather than a link, because there is
         nothing to navigate to — a disabled anchor is still an anchor. -->
    <span
      v-else
      class="view off"
      title="Not available in a shared namespace: two requests may hold one path, so a cell would stop meaning what it looks like"
    >
      grid
    </span>

    <RouterLink
      class="view"
      :class="{ on: view === 'claims' }"
      :to="claimsRoute(namespace)"
      title="One row per path, with the claims on it"
    >
      claims
    </RouterLink>
  </nav>

  <Matrix v-if="view === 'grid'" :namespace="namespace" />
  <Ledger v-else :namespace="namespace" />
</template>

<style scoped>
.views {
  display: flex;
  gap: 2px;
  padding: 6px 16px 0;
  flex: 0 0 auto;
}

.view {
  background: none;
  border: 1px solid transparent;
  border-bottom: none;
  border-radius: 3px 3px 0 0;
  color: var(--fg-dim);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  padding: 3px 12px;
  text-decoration: none;
}

.view.on {
  background: var(--bg-raised);
  border-color: var(--line);
  color: var(--fg);
}

.view.off { color: var(--fg-faint); cursor: not-allowed; }
</style>
