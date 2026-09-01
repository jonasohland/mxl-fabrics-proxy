<script setup lang="ts">
/**
 * Labelling a domain — `ui.md` §7a's *"see an unnamed domain, name it"*, and the second mutation
 * this UI offers (§3, §10.7).
 *
 * It hangs off the source editor's domain list because that is where the gesture arises: the
 * operator is already looking at a node's domains, choosing which one a request will take flows
 * from, and a domain nobody has labelled is exactly the one they cannot select by label. The CLI's
 * `label domain` verb exists for the same moment.
 *
 * **This writes, and it is the one control on this screen that does not stage.** Everything else
 * here is a request edit, and the staged set works because each edit is dry-run as a POST of one
 * request against the real fleet. A label write is a different record on a different endpoint, and
 * its candidate fleet is *the fleet with this label record replaced* — so it cannot be one of those
 * POSTs, and mixing the two into one batch would give each preview a fleet in which the other had
 * not happened. What replaces the pending bar is this panel: the write's own `stopped[]` and
 * `started[]`, from the server, above the button that commits it. That is the same bargain the bar
 * makes — a real preview instead of a confirmation dialog — rather than an exception to it.
 *
 * Three properties it holds, all of them from §3:
 *
 * - **A patch, never an apply.** An apply owns the keys it declares, so a read-modify-write with one
 *   would adopt and later delete keys someone else's manifest declared. `model/domainlabels.ts`
 *   carries the arithmetic and the reasoning.
 * - **The preview is the server's.** `stopped[]` and `started[]` come back as full `Path` objects,
 *   so which requests lose which legs needs no second read — `path.requests[]` is already on each.
 * - **A key is not renamed in place.** Renaming a label is a remove plus a set, and it moves a
 *   domain out of one standing selector and into another; an editable key box would spell that as a
 *   typo-sized gesture. Stored keys are fixed and the two halves are done separately.
 */
import { computed, onUnmounted, ref, watch } from 'vue'

import { ApiError, api } from '@/api/client'
import type { Domain, DomainLabelPatch } from '@/api/types'
import { renderDomain } from '@/api/types'
import {
  declaredWarning,
  impactOf,
  patchEmpty,
  patchOf,
  rowsError,
  rowsOf,
  type LabelImpact,
  type LabelRow,
} from '@/model/domainlabels'
import { plural } from '@/model/labels'
import { useFleetStore } from '@/stores/fleet'

const props = defineProps<{
  node: string
  domain: Domain
  labels?: Record<string, string> | undefined
  observed: boolean
}>()
const emit = defineEmits<{ close: []; wrote: [] }>()

const fleet = useFleetStore()

const rows = ref<LabelRow[]>(rowsOf(props.labels))
const writing = ref(false)
const failure = ref<string | undefined>(undefined)

const rendered = computed(() => renderDomain(props.domain))

const patch = computed<DomainLabelPatch>(() => patchOf(rows.value))
const nothingToDo = computed(() => patchEmpty(patch.value))
const invalid = computed(() => rowsError(rows.value))

/** What this domain ends up labelled — the set a selector will be matching tomorrow. */
const resulting = computed(() =>
  rows.value
    .filter((row) => !row.removed && row.key !== '')
    .map((row) => `${row.key}=${row.value}`)
    .sort()
    .join(' · '),
)

function toggle(index: number): void {
  const row = rows.value[index]
  if (!row) return
  // A pair the operator added has nothing on the server to keep, so `×` discards the row itself.
  if (row.stored === undefined) rows.value.splice(index, 1)
  else row.removed = !row.removed
}

function addRow(): void {
  rows.value = [...rows.value, { key: '', value: '', removed: false }]
}

// -- the preview ------------------------------------------------------------

const impact = ref<LabelImpact | undefined>(undefined)
const previewing = ref(false)
const refusal = ref<string | undefined>(undefined)

/**
 * Debounced, and keyed on the **patch** rather than on the rows.
 *
 * A dry run costs the server a full store load and two reconciles — one as things are, one as they
 * would be — so it fires on a structurally complete change and not per keystroke. Keying on the
 * patch also means typing a key and deleting it again reaches the same fingerprint and asks nothing.
 */
let token = 0
let timer: ReturnType<typeof setTimeout> | undefined

watch(
  () => JSON.stringify(patch.value),
  () => {
    if (timer !== undefined) clearTimeout(timer)
    impact.value = undefined
    refusal.value = undefined
    if (nothingToDo.value || invalid.value) {
      previewing.value = false
      return
    }
    previewing.value = true
    timer = setTimeout(() => void preview(), 300)
  },
  { immediate: true },
)

// A panel closed mid-debounce would otherwise still spend a store load and two reconciles on a
// preview nobody is going to read. The token moves too, so an in-flight one lands on nothing.
onUnmounted(() => {
  token++
  if (timer !== undefined) clearTimeout(timer)
})

async function preview(): Promise<void> {
  const seq = ++token
  const body = { domain: props.domain, patch: patch.value }
  try {
    const result = await api.writeDomainLabels(props.node, body, { dryRun: true })
    if (seq !== token) return
    impact.value = impactOf(result)
  } catch (error) {
    if (seq !== token) return
    // Rendered verbatim: a refusal carries its own fix in the prose, and it is better than anything
    // this panel would write.
    refusal.value = error instanceof ApiError ? error.message : String(error)
  } finally {
    if (seq === token) previewing.value = false
  }
}

const summary = computed(() => {
  if (invalid.value || nothingToDo.value) return ''
  if (previewing.value) return 'previewing…'
  if (!impact.value) return 'preview pending'

  const parts: string[] = []
  // Media first, because that is the irreversible half: a label coming back does not bring the
  // session with it.
  if (impact.value.stopped.length > 0) parts.push(`${plural(impact.value.stopped.length, 'path')} stop`)
  if (impact.value.started.length > 0) parts.push(`${plural(impact.value.started.length, 'path')} start`)
  return parts.length ? parts.join(' · ') : 'no path changes'
})

const warning = computed(() =>
  impact.value ? declaredWarning(patch.value, impact.value.declared) : undefined,
)

/**
 * Every path the write moves, the ones going first.
 *
 * The two lists are disjoint by construction — `stopped` is what the candidate fleet no longer has
 * and `started` is what it gained — so one list with a verb on each line reads as the write's whole
 * effect rather than as two tables to compare.
 */
const moved = computed(() => [
  ...(impact.value?.stopped ?? []).map((path) => ({ path, going: true })),
  ...(impact.value?.started ?? []).map((path) => ({ path, going: false })),
])

const blocked = computed<string | undefined>(() => {
  if (invalid.value) return invalid.value
  if (nothingToDo.value) return 'Nothing changed yet.'
  if (refusal.value) return refusal.value
  if (!impact.value) return 'Waiting for the dry run.'
  return undefined
})

// -- the write --------------------------------------------------------------

async function write(): Promise<void> {
  if (blocked.value !== undefined || writing.value) return
  writing.value = true
  failure.value = undefined
  try {
    await api.writeDomainLabels(props.node, { domain: props.domain, patch: patch.value })
    // The selector this label joins or leaves is a request's, so the grid behind this panel is
    // stale the moment the write lands.
    await fleet.refresh()
    emit('wrote')
    emit('close')
  } catch (error) {
    // 409 is the one refusal that arrives only here: two operators labelling one domain between the
    // same read and the same write, which is what the record's ownership story exists to make
    // visible rather than silent.
    failure.value = error instanceof ApiError ? error.message : String(error)
  } finally {
    writing.value = false
  }
}
</script>

<template>
  <div class="ed-scrim" @click.self="emit('close')">
    <section class="ed-panel label-editor">
      <header class="ed-head">
        <h2>labels</h2>
        <span class="ed-label mono">{{ node }} {{ rendered }}</span>
        <!-- A label on a domain the node does not report is accepted and inert — a pending record,
             not an error, and how an operator labels a domain before its producer comes up. -->
        <span v-if="!observed" class="ed-label">labelled, not observed</span>
        <span class="spacer" />
        <button class="ed-x" title="Close" @click="emit('close')">×</button>
      </header>

      <div class="ed-body">
        <section class="ed-step">
          <!-- What the pairs mean goes in the title, not on the screen: operators are trained, and
               a heading that explains itself is copy this interface does not carry. -->
          <div
            class="ed-step-head"
            title="Equality, ANDed"
          >
            <span>key / value</span>
          </div>

          <div class="ed-list">
            <p v-if="rows.length === 0" class="ed-empty">No labels on this domain.</p>
            <div
              v-for="(row, index) in rows"
              :key="row.stored === undefined ? `new-${index}` : row.key"
              class="ed-pair"
              :class="{ removed: row.removed, added: row.stored === undefined }"
            >
              <!-- A stored key is fixed: renaming one is a remove plus a set, and it moves the
                   domain out of one standing selector and into another. -->
              <input
                v-model="row.key"
                class="mono"
                :disabled="row.stored !== undefined"
                :title="row.stored !== undefined ? 'A key is not renamed in place. Remove it and add the new one.' : ''"
                placeholder="role"
                spellcheck="false"
                autocomplete="off"
              />
              <input
                v-model="row.value"
                class="mono"
                :disabled="row.removed"
                placeholder="cameras"
                spellcheck="false"
                autocomplete="off"
              />
              <button
                class="ed-x"
                :title="row.removed ? 'Keep this label' : 'Remove this label'"
                @click="toggle(index)"
              >{{ row.removed ? '↺' : '×' }}</button>
            </div>
          </div>

          <div class="ed-more">
            <button class="ed-act" @click="addRow()">+ label</button>
            <span class="ed-label mono">{{ resulting || 'no labels' }}</span>
          </div>
        </section>

        <p class="ed-note">
          <b>{{ summary || '·' }}</b>
          <template v-if="impact && impact.losing.length">
            · loses
            {{ impact.losing.map((entry) => `${entry.id} (${entry.paths})`).join(', ') }}
          </template>
          <template v-if="impact && impact.gaining.length">
            · gains
            {{ impact.gaining.map((entry) => `${entry.id} (${entry.paths})`).join(', ') }}
          </template>
        </p>

        <!-- Every path the write moves. Each line is the edge, its state and who is holding it —
             `path.requests[]` is on the result already, so the refcount costs no second read. -->
        <div v-if="moved.length" class="ed-list">
          <div v-for="entry in moved" :key="entry.path.id" class="ed-path" :class="{ going: entry.going }">
            <span class="ed-verb">{{ entry.going ? 'stops' : 'starts' }}</span>
            <span class="ed-edge mono">
              {{ entry.path.source.node }} {{ entry.path.source.domain }}
              → {{ entry.path.destination.node }} {{ renderDomain(entry.path.destination.domain) }}
            </span>
            <!-- Each state in its own colour, as everywhere else: `PAUSED` comes up calm and blue
                 where `FAILED` comes up red, which is the distinction §11 exists to preserve. -->
            <span class="ed-state" :class="`state-${entry.path.state}`">{{ entry.path.state }}</span>
            <span class="ed-holders mono">{{ entry.path.requests.join(', ') }}</span>
          </div>
        </div>

        <p v-if="warning" class="ed-note">{{ warning }}</p>
        <p v-if="failure" class="ed-fail">{{ failure }}</p>
        <p v-else-if="refusal" class="ed-fail">{{ refusal }}</p>
        <p v-else-if="blocked" class="ed-note">{{ blocked }}</p>
      </div>

      <footer class="ed-foot">
        <span class="spacer" />
        <button class="ed-act" @click="emit('close')">cancel</button>
        <!-- The one button on this screen that writes on the click. The preview above it is what
             the pending bar is for a request edit, and it is read before this is pressed. -->
        <button
          class="ed-act ed-go"
          :disabled="blocked !== undefined || writing"
          :title="blocked ?? 'Writes the labels'"
          @click="write()"
        >{{ writing ? 'writing…' : 'write' }}</button>
      </footer>
    </section>
  </div>
</template>
