<script setup lang="ts">
/**
 * Splitting one source out of a request — `ui.md` §7a consequence 2's second answer.
 *
 * *"Clearing a cell in a multi-source request is ambiguous, and the UI must ask. Two real operations:
 * drop the destination from the request, which clears that column across the whole rectangle; or
 * split the source out into a request of its own, keeping the others."* The first is the cell click
 * and has been since the gestures landed — parking is that operation with the entry kept. This is the
 * second, and §7a is explicit about why it cannot be a mode of the click: **it creates a name**, a
 * second lifecycle and a second thing to delete later, so it must be an explicit choice with the new
 * name visible.
 *
 * Which is the whole of what this panel is: a name, and a statement of what the two requests become.
 * Everything it does is staged, and both halves land in the bar together — the new request created,
 * the old one updated — so the dry run reports each before either happens.
 *
 * **The one thing worth saying out loud is the preview.** A dry run reconciles a candidate fleet with
 * *one* request changed, so the new request is measured against a fleet where the original still
 * holds the contested paths: it loses (newer stamp, `ui.md` §7b) and reports `namespace_overlap`. The
 * bar recognises that case and calls it a hand-off rather than a refusal, because the update that
 * gives the paths up is staged beside it — but an operator reading the *panel* should not have to
 * discover that from the bar.
 */
import { computed, ref } from 'vue'

import type { Request, Source } from '@/api/types'
import { plural, selectorLabel, sourceLabel } from '@/model/labels'
import { requestNameError, slug } from '@/model/naming'
import { useStagingStore } from '@/stores/staging'

const props = defineProps<{ namespace: string; request: Request; source: Source }>()
const emit = defineEmits<{ close: [] }>()

const staging = useStagingStore()

/**
 * Named after the source it carries, because that is what distinguishes it from the request it came
 * out of. Suggested and editable: names end up in the manifest and in `delete` commands.
 */
const suggested = computed(() => {
  const select = props.source.select
  const base = select.group_hint?.name ?? (select.flow !== undefined ? select.flow.slice(0, 8) : props.source.node)
  return slug(`${props.request.name}-${base}`) || `${props.request.name}-split`
})

const typed = ref('')
const touched = ref(false)

const name = computed({
  get: () => (touched.value ? typed.value : suggested.value),
  set: (value: string) => {
    touched.value = true
    typed.value = value
  },
})

/** Everything in this namespace, drafts included: a name is unique within its matrix, not the fleet. */
const taken = computed(
  () =>
    new Set(
      staging.effectiveRequests
        .filter((request) => (request.namespace || 'default') === props.namespace)
        .map((request) => request.name),
    ),
)

const blocked = computed<string | undefined>(() => {
  const bad = requestNameError(name.value)
  if (bad) return bad
  // A POST is create-or-**update** with no create-only mode and no 409, so a name that exists is not
  // a refusal the server will make — it is a silent overwrite of somebody's running request.
  if (taken.value.has(name.value)) {
    return `${name.value} already exists in ${props.namespace}. Applying would replace it.`
  }
  return undefined
})

const remaining = computed(() => props.request.sources.length - 1)
const parked = computed(() => props.request.destinations.filter((entry) => entry.disabled === true).length)

function apply(): void {
  if (blocked.value !== undefined) return
  staging.split(props.request, props.source, name.value)
  emit('close')
}
</script>

<template>
  <div class="ed-scrim" @click.self="emit('close')">
    <section class="ed-panel">
      <header class="ed-head">
        <h2>split a source out</h2>
        <span class="ed-label mono">{{ request.name }}</span>
        <span class="spacer" />
        <button class="ed-x" title="Close" @click="emit('close')">×</button>
      </header>

      <div class="ed-body">
        <!-- `{{ ' ' }}` rather than a newline between the two: Vue condenses a whitespace-only text
             node that contains one, so two adjacent elements written on separate lines render with
             nothing between them. It read as `{role=cameras}group Studio B:Camera 1`. -->
        <p class="ed-note">
          <b class="mono">{{ sourceLabel(source) }}</b>{{ ' ' }}
          <span class="mono">{{ selectorLabel(source.select) }}</span>
        </p>

        <label class="ed-field">
          <span class="ed-label">new name</span>
          <input v-model="name" class="mono" spellcheck="false" autocomplete="off" />
        </label>

        <p class="ed-note">
          <b>Two staged changes.</b>{{ ' ' }}
          <span class="mono">{{ name || '·' }}</span> is created with this source and
          {{ plural(request.destinations.length, 'destination') }} copied from
          <span class="mono">{{ request.name }}</span><template v-if="parked > 0">,
            {{ parked }} of them parked and staying parked</template>. Then
          <span class="mono">{{ request.name }}</span> gives the source up and keeps
          {{ plural(remaining, 'source') }}.
        </p>

        <!-- The rectangle's own settings come with it. A split that dropped a `provider` pin would
             move those legs onto another fabric, which is a performance cliff whose symptom looks
             like a source problem; dropped labels change what `apply --prune` deletes. -->
        <p v-if="request.provider || request.labels || request.sched_prio !== undefined || request.idle_teardown_ms"
           class="ed-note">
          Its provider, labels and scheduling settings are copied too.
        </p>

        <p class="ed-note">
          The paths change hands rather than stopping. Until both are applied the preview reports
          an overlap between them, which is that hand-off.
        </p>

        <p v-if="blocked" class="ed-fail">{{ blocked }}</p>
      </div>

      <footer class="ed-foot">
        <span class="spacer" />
        <button class="ed-act" @click="emit('close')">cancel</button>
        <button class="ed-act ed-go" :disabled="blocked !== undefined" @click="apply()">split</button>
      </footer>
    </section>
  </div>
</template>
