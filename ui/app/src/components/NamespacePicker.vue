<script setup lang="ts">
/**
 * The picker chooses what the whole workspace is about — and **shows every namespace's mode,
 * always**.
 *
 * The mode is not a preference to be discovered on hover: it decides which of two screens the
 * workspace is. A `shared` namespace is not the matrix greyed out, because the grid is as wrong to
 * *read* in that mode as it is to click.
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { plural } from '@/model/labels'
import { useNamespaceStore } from '@/stores/namespaces'

const route = useRoute()
const router = useRouter()
const namespaces = useNamespaceStore()

const current = computed(() => (route.params['namespace'] as string | undefined) ?? '')

function go(event: Event) {
  const name = (event.target as HTMLSelectElement).value
  if (name) void router.push({ name: 'workspace', params: { namespace: name } })
  else void router.push({ name: 'landing' })
}
</script>

<template>
  <label class="picker">
    <span class="label">namespace</span>
    <select :value="current" @change="go">
      <option value="">⟨fleet health⟩</option>
      <option v-for="entry in namespaces.all" :key="entry.name" :value="entry.name">
        {{ entry.name }} ⟨{{ entry.paths ?? 'shared' }}⟩ · {{ plural(entry.requests, 'request') }}
      </option>
    </select>
  </label>
</template>

<style scoped>
.picker {
  display: flex;
  align-items: center;
  gap: 6px;
}

.label {
  color: var(--fg-dim);
  font-size: 12px;
}

select {
  background: var(--bg-sunken);
  color: var(--fg);
  border: 1px solid var(--line);
  border-radius: 3px;
  padding: 3px 6px;
  font: inherit;
}
</style>
