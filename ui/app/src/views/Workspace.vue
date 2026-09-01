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
 */
import { computed, ref } from 'vue'

import Ledger from './Ledger.vue'
import Matrix from './Matrix.vue'
import { useNamespaceStore } from '@/stores/namespaces'

const props = defineProps<{ namespace: string }>()

const namespaces = useNamespaceStore()
const mode = computed(() => namespaces.mode(props.namespace))

type View = 'grid' | 'claims'

const chosen = ref<View>('grid')

/**
 * The grid is only ever a grid here; anywhere else the choice resolves to the ledger.
 *
 * The *choice* survives a namespace switch and only its resolution changes, so moving through a
 * shared namespace and back does not silently change which screen the operator is reading.
 */
const view = computed<View>(() => (mode.value === 'exclusive' ? chosen.value : 'claims'))
</script>

<template>
  <nav class="views">
    <button
      class="view"
      :class="{ on: view === 'grid' }"
      :disabled="mode !== 'exclusive'"
      :title="
        mode === 'exclusive'
          ? 'Sources down, destinations across, a request as a rectangle'
          : 'Not available in a shared namespace'
      "
      @click="chosen = 'grid'"
    >
      grid
    </button>
    <button
      class="view"
      :class="{ on: view === 'claims' }"
      title="One row per path, with the claims on it"
      @click="chosen = 'claims'"
    >
      claims
    </button>
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
}

.view.on {
  background: var(--bg-raised);
  border-color: var(--line);
  color: var(--fg);
}

.view:disabled { color: var(--fg-faint); cursor: not-allowed; }
</style>
