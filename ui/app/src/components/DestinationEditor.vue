<script setup lang="ts">
/**
 * Naming a destination — `ui.md` §7a's "new column".
 *
 * **This is the step a router matrix does not have, and it is structural rather than incidental: the
 * destination domain does not exist until a request names it.** So the column set is "every
 * destination some request names", plus the one being created — and creating one is real work, which
 * is why it is a control rather than a text box in a corner.
 *
 * Three rules the form is shaped by, all of them from §3 and §10.6 rather than from taste:
 *
 * - **The area is part of the name, not a setting beside it.** The picker is shown even for a node
 *   with exactly one writable area, because omitting the area is omitting half the name. *This
 *   supersedes the `root:` control that could be left out when a node advertised exactly one* —
 *   there is nothing to omit now that the area is the first segment.
 * - **Only areas the node grants `write` on are offered**, and a node with none is shown **disabled
 *   with the reason** rather than hidden. "Where is edge-03?" is the question omission produces, and
 *   the two refusals are different problems: `unknown_area` for a node advertising no areas at all,
 *   `area_not_writable` for one that advertises them without the grant.
 * - **The resolved directory is shown as it is typed.** `area.path` is advertised for diagnostics
 *   and may be absent — guarded — and for an otherwise abstract name it is the strongest affordance
 *   available.
 *
 * The elements field is the one place this UI parses anything, and it is bounded: the area is never
 * typed, so what is split on `/` is a list of elements each validated against the mirror in
 * `model/naming.ts`. Everything else — a name another request is materialising, a domain a scan has
 * taken since the last poll — is the server's, and the dry run is where it is answered.
 *
 * It is a panel rather than a `<dialog>`: `showModal` needs a polyfill under jsdom, and the live
 * suite is the only place this screen's *sequences* are ever tested. A form that could not be
 * driven there would be a form nothing checks.
 */
import { computed, ref, watch } from 'vue'

import type { Area, Destination, Node } from '@/api/types'
import { renderDomain, writableAreas } from '@/api/types'
import type { MatrixColumn } from '@/model/matrix'
import { domainError, parseElements } from '@/model/naming'
import { useFleetStore } from '@/stores/fleet'
import { useStagingStore } from '@/stores/staging'

const props = defineProps<{ namespace: string; columns: MatrixColumn[] }>()
const emit = defineEmits<{ close: [] }>()

const fleet = useFleetStore()
const staging = useStagingStore()

const nodes = computed(() => [...fleet.nodes].sort((a, b) => a.name.localeCompare(b.name)))

/** Which of the two refusals this node would produce, or nothing if it can be a destination. */
function refusal(node: Node): string {
  if (writableAreas(node).length > 0) return ''
  return (node.capabilities?.areas ?? []).length === 0
    ? '  (no areas advertised, unknown_area)'
    : '  (no area it grants writing on, area_not_writable)'
}

const chosen = ref(nodes.value.find((node) => writableAreas(node).length > 0)?.name ?? '')
const area = ref('')
const text = ref('')

const node = computed(() => fleet.nodes.find((entry) => entry.name === chosen.value))
const areas = computed<Area[]>(() => (node.value ? writableAreas(node.value) : []))

// The area belongs to the node it was picked on. Carried across a change of node it would name an
// area the new one does not have, which reads as an empty field rather than as a stale selection.
watch(areas, (list) => {
  if (!list.some((entry) => entry.name === area.value)) area.value = list[0]?.name ?? ''
}, { immediate: true })

const elements = computed(() => parseElements(text.value))

const destination = computed<Destination | undefined>(() =>
  chosen.value && area.value && elements.value.length > 0
    ? { node: chosen.value, domain: { area: area.value, elements: elements.value } }
    : undefined,
)

/** What the fleet will call this domain, and where it lands on the node. */
const resolved = computed(() => {
  const path = areas.value.find((entry) => entry.name === area.value)?.path
  if (elements.value.length === 0) {
    return { name: '', path: path ? `${path}/…` : 'the directory this creates on the node' }
  }
  const joined = elements.value.join('/')
  return { name: `${area.value}/${joined}`, path: path ? `${path}/${joined}` : '' }
})

/**
 * The refusals decidable from what is on screen.
 *
 * Nesting is the one destination-name collision that survives — `fast/studio-a` against an existing
 * `fast/studio-a/cam1`, within one area, because two areas are two directory trees. Two domains
 * sharing a name under different areas is unconstructible now that the area is in the name, so
 * `fast/ingest` and `bulk/ingest` are simply two columns.
 */
const error = computed<string | undefined>(() => {
  if (!chosen.value) return 'no node selected'
  if (areas.value.length === 0) {
    return `${chosen.value} advertises no area it grants writing on`
  }
  if (text.value.trim() === '') return undefined // nothing typed yet is not yet a mistake
  const domain = { area: area.value, elements: elements.value }
  const bad = domainError(domain)
  if (bad) return bad

  for (const column of props.columns) {
    if (column.node !== chosen.value) continue
    const other = column.destination.domain
    if (other.area !== domain.area) continue
    const [inner, outer] = other.elements.length < domain.elements.length
      ? [domain.elements, other.elements]
      : [other.elements, domain.elements]
    if (inner.length !== outer.length && outer.every((element, i) => element === inner[i])) {
      return `"${renderDomain(domain)}" nests with "${column.domain}", which ${chosen.value} is ` +
        `already a destination for (domain_name_in_use)`
    }
  }
  return undefined
})

const ready = computed(() => destination.value !== undefined && error.value === undefined)

/** Already a column is not an error: it is the operator arriving at one that exists. */
const existing = computed(() =>
  destination.value !== undefined &&
  props.columns.some(
    (column) => column.node === destination.value!.node && column.domain === resolved.value.name,
  ),
)

function add(): void {
  if (!ready.value || !destination.value) return
  if (!existing.value) staging.addColumn(props.namespace, destination.value)
  emit('close')
}
</script>

<template>
  <div class="ed-scrim" @click.self="emit('close')">
    <section class="ed-panel dest-editor">
      <header class="ed-head">
        <h2>new destination</h2>
        <span class="ed-label">a domain a request will materialise on a node</span>
        <span class="spacer" />
        <button class="ed-x" title="Close" @click="emit('close')">×</button>
      </header>

      <div class="ed-body">
        <label class="ed-field">
          <span class="ed-label">node</span>
          <!-- Disabled rather than omitted, with which refusal it would be: "where is edge-03?" is
               the question omission produces, and the two codes are different problems. -->
          <select v-model="chosen" class="mono">
            <option
              v-for="entry in nodes"
              :key="entry.name"
              :value="entry.name"
              :disabled="refusal(entry) !== ''"
            >{{ entry.name }}{{ refusal(entry) }}{{ entry.live ? '' : '  (no agent)' }}</option>
          </select>
        </label>

        <label class="ed-field">
          <span class="ed-label">area</span>
          <!-- Always shown, even for a node with one: the area is the first segment of the domain's
               name, so defaulting it invisibly is omitting half the name. -->
          <select v-model="area" class="mono">
            <option v-for="entry in areas" :key="entry.name" :value="entry.name">
              {{ entry.name }}{{ entry.read ? '' : '  (write-only)' }}
            </option>
          </select>
        </label>

        <label class="ed-field">
          <span class="ed-label">name</span>
          <input
            v-model="text"
            class="mono"
            placeholder="studio-a/cam1"
            spellcheck="false"
            autocomplete="off"
          />
        </label>

        <p class="ed-resolved">
          <b class="mono">{{ resolved.name || '·' }}</b>
          <span v-if="resolved.path" class="ed-label mono">→ {{ resolved.path }}</span>
        </p>

        <p v-if="error" class="ed-fail">{{ error }}</p>
        <p v-else-if="existing" class="ed-note">
          Already a column here. Adding changes nothing.
        </p>
        <p v-else class="ed-note">
          Naming a column writes nothing. It goes on the axis.
        </p>
      </div>

      <footer class="ed-foot">
        <span class="spacer" />
        <button class="ed-act" @click="emit('close')">cancel</button>
        <button class="ed-act ed-go" :disabled="!ready" @click="add()">add</button>
      </footer>
    </section>
  </div>
</template>
