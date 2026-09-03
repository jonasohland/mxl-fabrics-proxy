<script setup lang="ts">
/**
 * The filter row above an index table: a text box and a chip per facet value.
 *
 * Chips rather than a multi-select, because the counts are half of what the control says — an
 * operator opening `/paths` wants to see that two of ninety are `FAILED` before deciding to click,
 * and a closed dropdown hides exactly that. The count on a chip is what the table *would* hold, so
 * a chip reading 0 is a question already answered.
 *
 * It owns no state. The selection lives in the URL (`model/filters.ts`) and the view writes it
 * there; this only renders and emits, so there is no second copy to go stale against the address
 * bar when an operator edits it by hand or arrives on a link.
 */
import type { FacetGroup } from '@/model/filters'

defineProps<{
  groups: readonly FacetGroup[]
  text: string
  /** Shown only when something is actually filtered — a dead "clear" is noise on every table. */
  filtered: boolean
}>()

const emit = defineEmits<{
  toggle: [key: string, value: string]
  'update:text': [value: string]
  clear: []
}>()

function onInput(event: Event) {
  emit('update:text', (event.target as HTMLInputElement).value)
}
</script>

<template>
  <div class="ls-facets">
    <input
      class="ls-search"
      type="search"
      placeholder="filter"
      :value="text"
      @input="onInput"
    />

    <!-- A facet with one option constrains nothing: every row already has that value, so clicking
         it can only be a no-op that looks like a filter. Dropped rather than disabled, because
         unlike a control that is *off for a reason* there is nothing here to explain. -->
    <div v-for="group in groups" v-show="group.options.length > 1" :key="group.key" class="ls-facet">
      <span class="ls-facet-label" :title="group.title">{{ group.label }}</span>
      <button
        v-for="option in group.options"
        :key="option.value"
        class="ls-chip"
        :class="{ on: option.on }"
        type="button"
        @click="emit('toggle', group.key, option.value)"
      >
        <span class="ls-chip-value" :class="group.chipClass?.(option.value)">{{ option.value }}</span>
        <span class="ls-chip-count">{{ option.count }}</span>
      </button>
    </div>

    <button v-if="filtered" class="ls-clear" type="button" @click="emit('clear')">clear</button>
  </div>
</template>
