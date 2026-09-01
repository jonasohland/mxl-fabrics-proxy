<script setup lang="ts">
/**
 * Authoring a source — `ui.md` §7a's "new row".
 *
 * **Node, then domain, then a group, then how much of it to take.** Not a flat list of flows: the
 * operator browses to *discover*, but what they mean is the group, and a UUID picker recreates
 * exactly the problem selectors exist to solve. `all` is the default and the one made attractive —
 * omitting `type` is how a camera's video and audio travel together, and it is a **standing**
 * selection, so a flow the producer adds later joins it on its own.
 *
 * Both choices on the way are tagged unions on the wire, and both are rendered as what they are:
 *
 * - **the domain selector** — `{name: {area, elements}}` addresses one domain the operator picked;
 *   `{labels: {…}}` matches every domain on that node carrying all of those pairs, *including ones
 *   labelled tomorrow*. A manifest naming a domain is self-contained; one naming labels depends on a
 *   `kind: domain` document having been applied. The choice is shown rather than defaulted silently,
 *   because a label row is a standing query and that is both the point and the surprise.
 * - **the flow selector** — `{group_hint: {name}}`, `{group_hint: {name, type}}`, `{flow: <uuid>}`,
 *   and `{all: true}` for the whole domain. Two spellings of "everything" and the difference is
 *   worth a sentence: `all` takes the domain, a group hint with no type takes one group of it.
 *
 * Three things the list has to get right, each of which is invisible if it is got wrong:
 *
 * - **Ungrouped flows stay reachable.** A producer that never set the NMOS tag is not a flow you can
 *   decline to replicate, but there is no name for a group selector to match — so it gets a
 *   pseudo-group where only `select flows` is offered, and `{all: true}` over the domain does reach
 *   it.
 * - **Flows this node's own target workers are writing are marked.** A label selector never matches
 *   one — the server drops it as `self_output` — and seeing that here rather than in the response is
 *   the difference between "why did it skip three flows" and "of course it did".
 * - **`select flows` creates one source per flow**, because a selector pins exactly one ID. Whether
 *   that is three requests or one is the operator's choice and it is a real one: one request means
 *   one name, one delete and one aggregate that goes `PARTIAL` when one camera is dark. So the
 *   panel shows the names before it creates them, and *route into* is where the choice is made.
 *
 * **Nothing here writes.** A source added to an existing request stages like every other edit; a new
 * request is authored as a draft and reaches the server through the same dry run and the same Apply.
 * `model/draft.ts` carries why a draft is allowed to be a spec when an edit is not.
 */
import { computed, ref, watch } from 'vue'

import { api } from '@/api/client'
import type { Domain, DomainInfo, DomainSelector, FlowInventory, Node, Request, Selector, Source } from '@/api/types'
import { renderDomain } from '@/api/types'
import LabelEditor from '@/components/LabelEditor.vue'
import { settingsOf } from '@/model/draft'
import { plural, selectorLabel, shortId } from '@/model/labels'
import { namespaceOf } from '@/model/ledger'
import { sourceKey } from '@/model/matrix'
import { requestNameError, slug } from '@/model/naming'
import type { SourcePrefill } from '@/model/unrouted'
import { useFleetStore } from '@/stores/fleet'
import { useStagingStore } from '@/stores/staging'

/**
 * `template` is `duplicate`: the rectangle whose destinations and settings the new request copies.
 *
 * `ui.md` §7a calls duplicating *"probably the most-used control on the screen"* and says exactly
 * what it is for — *"a new camera arrives and it should be routed like the last one: copy the
 * rectangle, **change the selector**, keep the destinations."* That sentence is why a duplicate is
 * this panel rather than a control of its own: a copy that kept the sources as well would be a second
 * request asking for the identical paths, which in an exclusive namespace is `namespace_overlap` by
 * construction and is refused the moment a producer appears. Choosing the new source *is* the
 * operation; the destinations are what comes along.
 *
 * `prefill` is the unrouted strip: *"clicking one starts a new row pre-filled"* (§7a). It sets the
 * first three steps and stops — it is a **starting point, not a shortcut past the panel**. Nothing it
 * chooses is unreachable by hand, nothing is hidden once it lands, and the operator still picks how
 * much of the group to take and what to route it into, so a click on a flow cannot author something
 * they did not read. It is consumed once: switching node away and back is browsing, and re-applying
 * it there would fight the operator for control of the panel.
 */
const props = defineProps<{ namespace: string; template?: Request; prefill?: SourcePrefill }>()
const emit = defineEmits<{ close: [] }>()

const fleet = useFleetStore()
const staging = useStagingStore()

/**
 * The two pseudo-groups. Prefixed with a character an NMOS group name cannot carry, so neither can
 * collide with a real one — NUL is the same byte the model's own keys refuse for the same
 * reason, written as an escape rather than as a literal.
 */
const WHOLE = '\u0000whole'
const UNGROUPED = '\u0000ungrouped'

type Mode = 'all' | 'type' | 'flows'

/** A flow of a matched domain, with what the selector will do about it. */
interface Candidate extends FlowInventory {
  domain: string
  /** This node's own target worker is writing it, and a label selector will skip it (`self_output`). */
  excluded: boolean
}

// -- what the operator has chosen -------------------------------------------

const nodes = computed(() => [...fleet.nodes].sort((a, b) => a.name.localeCompare(b.name)))
const readable = (node: Node) => (node.capabilities?.areas ?? []).some((area) => area.read)

/**
 * Opens on a node this namespace already takes sources from, and falls back to the first with a
 * readable area.
 *
 * The fallback alone is right and useless: sorted by name it lands on whichever node happens to come
 * first, which in an ordinary fleet is an archive — a node with a readable area and nothing on it.
 * The list says so honestly, but "adding another camera to a board that already has some" is the
 * repeated task, and the nodes it comes from are already on screen.
 *
 * A `prefill` outranks both: the operator clicked a specific flow on a specific node, and opening
 * anywhere else would discard the only thing they said.
 */
const chosen = ref(
  props.prefill?.node ??
  nodes.value.find(
    (node) =>
      readable(node) &&
      staging.effectiveRequests.some(
        (request) =>
          namespaceOf(request) === props.namespace &&
          request.sources.some((source) => source.node === node.name),
      ),
  )?.name ??
    nodes.value.find(readable)?.name ??
    nodes.value[0]?.name ??
    '',
)
const domainKind = ref<'name' | 'labels'>('name')
const domain = ref<Domain | undefined>(undefined)
const labels = ref<string[]>([])
const group = ref<string | undefined>(undefined)
const mode = ref<Mode>('all')
const type = ref('')
const flows = ref<string[]>([])
const target = ref('')
/** The strip's click, until it is applied. Cleared on use — see the note on the prop. */
const wanted = ref<SourcePrefill | undefined>(props.prefill)
const typed = ref('')
const touched = ref(false)
const failure = ref<string | undefined>(undefined)

/** The chosen node's domains, and only while this panel is open. */
const domains = ref<DomainInfo[]>([])
const loading = ref(false)

/**
 * A node's domains come from `GET /v1/nodes/{node}/domains`, which reports what the agent observes
 * joined with the label records — so it covers discovered domains, domains this project is
 * replicating into, and labelled domains the node is not currently observing, which is how an
 * operator sees a label applied before the producer came up.
 *
 * **Not part of the poll**: it is per-node and only wanted here.
 *
 * **Cleared before the read, not after it.** Leaving the previous node's domains up until the new
 * ones land shows a list belonging to a node the operator is no longer looking at — and since domain
 * names repeat across nodes (`media/cameras` on both studios), the stale list is not even obviously
 * stale. Empty-while-loading is the honest state.
 *
 * **Sequenced**, because switching nodes twice quickly is an ordinary gesture and the two reads can
 * land out of order: without the guard the *first* node's answer arrives last and empties a list the
 * operator is already working in.
 */
let token = 0

async function load(node: string): Promise<void> {
  const seq = ++token
  domains.value = []
  if (node === '') return
  loading.value = true
  try {
    const list = await api.domains(node)
    if (seq !== token) return
    domains.value = list.domains ?? []
    consumePrefill(node)
  } catch (error) {
    if (seq !== token) return
    failure.value = error instanceof Error ? error.message : String(error)
  } finally {
    if (seq === token) loading.value = false
  }
}

// The domain and the flows belong to the node they were chosen on: carried across a change of node
// they name something the new one does not have, which reads as an empty list rather than as a
// stale selection.
watch(chosen, (node) => {
  domain.value = undefined
  labels.value = []
  resetGroup()
  void load(node)
}, { immediate: true })

function resetGroup(): void {
  group.value = undefined
  mode.value = 'all'
  type.value = ''
  flows.value = []
}

// -- the domain step --------------------------------------------------------

/**
 * The domain whose labels are being written, or none.
 *
 * `ui.md` §7a puts labelling here — *"see an unnamed domain, name it"* — and it is the one control
 * in this panel that writes rather than staging: a label is a different record on a different
 * endpoint, previewed by the server's own `stopped[]` / `started[]`. `LabelEditor.vue` carries why
 * it cannot join the staged set.
 */
const labelling = ref<DomainInfo | undefined>(undefined)

const labelPairsOf = (entry: DomainInfo) =>
  Object.entries(entry.labels ?? {}).map(([key, value]) => `${key}=${value}`).sort()

/** The row's own line: what it is labelled, and whether the node is reporting it at all. */
function metaOf(entry: DomainInfo): string {
  const pairs = labelPairsOf(entry)
  if (entry.observed) return pairs.join(' · ')
  return pairs.length ? `${pairs.join(' · ')} · not observed` : 'labelled, not observed'
}

/** The whole set, since the line is bounded — a domain may carry more labels than a row can hold. */
const labelTitle = (entry: DomainInfo) => labelPairsOf(entry).join('\n') || 'no labels'

/** Every label pair on this node's domains, with how many carry it. Equality, ANDed. */
const labelPairs = computed(() => {
  const counts = new Map<string, number>()
  for (const entry of domains.value) {
    for (const [key, value] of Object.entries(entry.labels ?? {})) {
      const pair = `${key}=${value}`
      counts.set(pair, (counts.get(pair) ?? 0) + 1)
    }
  }
  return [...counts].map(([pair, count]) => ({ pair, count })).sort((a, b) => a.pair.localeCompare(b.pair))
})

const selectedLabels = computed(() =>
  Object.fromEntries(labels.value.map((pair) => [pair.slice(0, pair.indexOf('=')), pair.slice(pair.indexOf('=') + 1)])),
)

/** The domains the current selector matches — one for a name, the intersection for labels. */
const matched = computed<DomainInfo[]>(() => {
  if (domainKind.value === 'name') {
    if (!domain.value) return []
    const wanted = renderDomain(domain.value)
    return domains.value.filter((entry) => renderDomain(entry.domain) === wanted)
  }
  const wanted = Object.entries(selectedLabels.value)
  if (wanted.length === 0) return []
  return domains.value.filter((entry) => wanted.every(([key, value]) => (entry.labels ?? {})[key] === value))
})

/** The domain selector this panel would produce — **structured**, always. */
const selector = computed<DomainSelector | undefined>(() => {
  if (domainKind.value === 'name') {
    return domain.value ? { name: { area: domain.value.area, elements: [...domain.value.elements] } } : undefined
  }
  return labels.value.length > 0 ? { labels: { ...selectedLabels.value } } : undefined
})

function pickDomain(entry: DomainInfo): void {
  domain.value = entry.domain
  resetGroup()
}

function toggleLabel(pair: string): void {
  labels.value = labels.value.includes(pair)
    ? labels.value.filter((entry) => entry !== pair)
    : [...labels.value, pair]
  resetGroup()
}

function setKind(kind: 'name' | 'labels'): void {
  domainKind.value = kind
  resetGroup()
}

// -- the group step ---------------------------------------------------------

const candidates = computed<Candidate[]>(() =>
  matched.value.flatMap((entry) =>
    (entry.flows ?? []).map((flow) => ({
      ...flow,
      domain: renderDomain(entry.domain),
      excluded: domainKind.value === 'labels' && flow.replicated === true,
    })),
  ),
)

interface Group {
  name: string
  flows: Candidate[]
  types: string[]
}

const groups = computed<Group[]>(() => {
  const all = candidates.value
  const byName = new Map<string, Group>()
  if (selector.value !== undefined) byName.set(WHOLE, { name: WHOLE, flows: all, types: [] })

  for (const flow of all) {
    const key = flow.group_hint?.name ?? UNGROUPED
    let entry = byName.get(key)
    if (!entry) {
      entry = { name: key, flows: [], types: [] }
      byName.set(key, entry)
    }
    entry.flows.push(flow)
    const kind = flow.group_hint?.type
    if (kind && !entry.types.includes(kind)) entry.types.push(kind)
  }
  for (const entry of byName.values()) entry.types.sort()

  // Whole-domain first, ungrouped last, always — stated rather than left to the sentinels' own
  // collation, which would move the day one of them changed.
  const rank = (name: string) => (name === WHOLE ? -1 : name === UNGROUPED ? 1 : 0)
  return [...byName.values()].sort((a, b) => rank(a.name) - rank(b.name) || a.name.localeCompare(b.name))
})

const current = computed(() => groups.value.find((entry) => entry.name === group.value))

const groupLabel = (name: string) =>
  name === WHOLE ? 'everything selected' : name === UNGROUPED ? '(no group hint)' : name

function pickGroup(entry: Group): void {
  group.value = entry.name
  type.value = entry.types[0] ?? ''
  flows.value = []
  // `all: true` has no sub-modes, and a flow carrying no hint has no name for a group selector to
  // match, so the only selector that reaches one is a pinned ID.
  mode.value = entry.name === UNGROUPED ? 'flows' : 'all'
}

/**
 * Apply the strip's click, once, after this node's domains have landed.
 *
 * **It opens on the flow's group rather than pinning the flow**, wherever the flow has one. That is
 * the selector operators actually want — omitting the type is how a camera's video and audio travel
 * together — and it is a *standing* selection, so the next flow the producer publishes into that
 * group joins it without anybody coming back. Pinning the one ID the operator happened to click
 * would author the narrowest possible selector from the broadest possible gesture, and the strip
 * would then re-list its siblings tomorrow.
 *
 * The exception is a flow with no group hint, which has no name for a group selector to match: it is
 * pinned by ID, which is the only selector that reaches one. And a click on the group header names
 * no flow at all, so it opens on the whole domain.
 *
 * Everything here is a click the operator could have made, and every one of them stays visible and
 * changeable afterwards. Silently falls back to as much as it can honestly carry — a domain the node
 * has stopped reporting, or a flow that has gone, leaves the steps before it set and the rest for the
 * operator, rather than guessing at a selector for something that is no longer there.
 */
function consumePrefill(node: string): void {
  const want = wanted.value
  if (!want || want.node !== node) return
  wanted.value = undefined

  // Looked up by *rendered* equality against the server's own list, never reconstructed from the
  // string: the structured domain that ends up in the selector is the one the API handed us, so the
  // UI never becomes the second parser of a domain name (`ui.md` §3).
  const entry = domains.value.find((info) => renderDomain(info.domain) === want.domain)
  if (!entry) return

  domainKind.value = 'name'
  pickDomain(entry)

  const flow = want.flow === undefined
    ? undefined
    : (entry.flows ?? []).find((candidate) => candidate.id === want.flow)

  const wantedGroup = flow === undefined ? WHOLE : (flow.group_hint?.name ?? UNGROUPED)
  const entryGroup = groups.value.find((candidate) => candidate.name === wantedGroup)
  if (!entryGroup) return
  pickGroup(entryGroup)

  if (entryGroup.name === UNGROUPED && flow) flows.value = [flow.id]
}

/** Which modes this group can be taken in, and why not where it cannot. */
function modeOff(kind: Mode): string {
  if (!current.value) return 'Choose a group first.'
  if (current.value.name === WHOLE) {
    return '`all: true` has no sub-modes. Pick a group to narrow it.'
  }
  if (current.value.name === UNGROUPED && kind !== 'flows') {
    return 'These flows carry no group hint, so there is no name for a group selector to match. ' +
      'Pin them by ID, or take the whole domain.'
  }
  if (kind === 'type' && current.value.types.length === 0) return 'None of these flows carries a type.'
  return ''
}

function setMode(kind: Mode): void {
  if (modeOff(kind) !== '') return
  mode.value = kind
  flows.value = []
}

function toggleFlow(id: string): void {
  flows.value = flows.value.includes(id) ? flows.value.filter((entry) => entry !== id) : [...flows.value, id]
}

// -- what it will create ----------------------------------------------------

/** The requests this panel can add a row to: everything in this namespace, drafts included. */
const targets = computed(() =>
  staging.effectiveRequests
    .filter((request) => namespaceOf(request) === props.namespace)
    .sort((a, b) => a.name.localeCompare(b.name)),
)

const joining = computed(() => targets.value.find((request) => request.id === target.value))

/**
 * A name as soon as there is something to name it after, kept editable, because the operator owns it
 * — names end up in the manifest and in `delete` commands.
 */
const suggested = computed(() => {
  const entry = current.value
  let base = 'source'
  if (entry?.name === WHOLE) {
    base = domainKind.value === 'name' && domain.value
      ? slug(domain.value.elements.join('-'))
      : slug(labels.value[0] ?? 'all')
  } else if (entry && entry.name !== UNGROUPED) {
    base = slug(entry.name)
  }
  if (mode.value === 'type' && type.value) base += `-${slug(type.value)}`
  return base || 'source'
})

const name = computed({
  get: () => (touched.value ? typed.value : suggested.value),
  set: (value: string) => {
    touched.value = true
    typed.value = value
  },
})

interface Planned {
  source: Source
  /** What it takes, in the vocabulary of the step it came from. */
  label: string
  /** Only used when a new request is being created; ignored when joining an existing one. */
  name: string
}

const planned = computed<Planned[]>(() => {
  const entry = current.value
  const domainSelector = selector.value
  if (!entry || !domainSelector) return []

  const make = (select: Selector, label: string, suffix = ''): Planned => ({
    source: { node: chosen.value, domain: structuredClone(domainSelector), select },
    label,
    name: suffix ? `${name.value}-${suffix}` : name.value,
  })

  if (entry.name === WHOLE) {
    const count = entry.flows.filter((flow) => !flow.excluded).length
    return [make({ all: true }, `the whole domain, ${plural(count, 'flow')} so far`)]
  }
  if (mode.value === 'all') {
    return [make({ group_hint: { name: entry.name } }, `every flow of the group, ${plural(entry.flows.length, 'flow')} so far`)]
  }
  if (mode.value === 'type') {
    if (!type.value) return []
    const count = entry.flows.filter((flow) => flow.group_hint?.type === type.value).length
    return [make({ group_hint: { name: entry.name, type: type.value } }, `${type.value}, ${plural(count, 'flow')}`)]
  }

  const picked = entry.flows.filter((flow) => flows.value.includes(flow.id))
  const types = picked.map((flow) => flow.group_hint?.type ?? '')
  return picked.map((flow) => {
    const kind = flow.group_hint?.type
    // Suffixed by type where that is unambiguous within the selection, by id where it is not.
    const unique = kind !== undefined && types.filter((entry) => entry === kind).length === 1
    return make(
      { flow: flow.id },
      `${kind ?? 'no type'} ${shortId(flow.id)}…`,
      picked.length > 1 ? (unique ? slug(kind) : shortId(flow.id)) : '',
    )
  })
})

const namePrefix = computed(() => mode.value === 'flows' && flows.value.length > 1)

const blocked = computed<string | undefined>(() => {
  if (planned.value.length === 0) {
    if (!selector.value) return domainKind.value === 'name' ? 'Choose a domain.' : 'Choose one or more labels.'
    if (mode.value === 'flows' && flows.value.length === 0) return 'Select at least one flow.'
    return 'Choose a group.'
  }
  if (joining.value) {
    for (const entry of planned.value) {
      const key = sourceKey(entry.source)
      if (joining.value.sources.some((source) => sourceKey(source) === key)) {
        return `${joining.value.name} already has this exact source.`
      }
    }
    return undefined
  }

  // Names are scoped to the namespace, so the check is against this namespace and not the fleet:
  // checking everything would refuse `cam1` in `archive` because `nab` has one, which is exactly
  // what the partition exists to allow.
  const taken = new Set(targets.value.map((request) => request.name))
  const seen = new Set<string>()
  for (const entry of planned.value) {
    const bad = requestNameError(entry.name)
    if (bad) return bad
    // A POST is create-or-**update** with no create-only mode and no 409, so a name that exists is
    // not a refusal the server will make — it is a silent overwrite of somebody's running request.
    if (taken.has(entry.name)) return `${entry.name} already exists in ${props.namespace}. Applying would replace it.`
    if (seen.has(entry.name)) return `${entry.name} would be created twice`
    seen.add(entry.name)
  }
  return undefined
})

function add(): void {
  failure.value = blocked.value
  if (failure.value !== undefined) return

  if (joining.value) {
    for (const entry of planned.value) staging.addSource(joining.value.id, entry.source)
    emit('close')
    return
  }
  for (const entry of planned.value) {
    staging.createDraft(
      props.namespace,
      entry.name,
      [entry.source],
      // Copied whole: a parked leg stays parked and a per-destination `provider` override comes with
      // it, because the operator asked for a copy of the request rather than for a list of places.
      (props.template?.destinations ?? []).map((destination) => ({ ...destination })),
      props.template ? settingsOf(props.template) : {},
    )
  }
  emit('close')
}
</script>

<template>
  <div class="ed-scrim" @click.self="emit('close')">
    <section class="ed-panel">
      <header class="ed-head">
        <h2>{{ template ? 'duplicate' : 'new source' }}</h2>
        <span v-if="template" class="ed-label">
          routed like <span class="mono">{{ template.name }}</span>, with a source you choose
        </span>
        <span v-else class="ed-label">a node, a domain selector and a flow selector</span>
        <span class="spacer" />
        <button class="ed-x" title="Close" @click="emit('close')">×</button>
      </header>

      <div class="ed-body">
        <label class="ed-field">
          <span class="ed-label">node</span>
          <!-- A node with no readable area offers no sources at all: the grants are the whole of
               this project's authority over a node's filesystem, and `read` is the half that
               matters here. Disabled with the reason rather than hidden. -->
          <select v-model="chosen" class="mono">
            <option v-for="entry in nodes" :key="entry.name" :value="entry.name" :disabled="!readable(entry)">
              {{ entry.name }}{{ readable(entry) ? (entry.live ? '' : '  (no agent)') : '  (no readable area)' }}
            </option>
          </select>
        </label>

        <section class="ed-step">
          <div class="ed-step-head">
            <span>which domains</span>
            <div class="ed-modes">
              <label class="ed-mode" title="One domain, by name">
                <input type="radio" :checked="domainKind === 'name'" @change="setKind('name')" />
                this one
              </label>
              <label
                class="ed-mode"
                title="Every domain on this node carrying all of these pairs, including ones labelled later"
              >
                <input type="radio" :checked="domainKind === 'labels'" @change="setKind('labels')" />
                anything labelled
              </label>
            </div>
          </div>

          <div class="ed-list">
            <p v-if="loading" class="ed-empty">reading {{ chosen }}'s domains…</p>

            <template v-else-if="domainKind === 'name'">
              <p v-if="domains.length === 0" class="ed-empty">
                No domains observed or labelled on this node.
              </p>
              <div v-for="entry in domains" :key="renderDomain(entry.domain)" class="ed-row">
                <button
                  type="button"
                  class="ed-item"
                  :class="{ on: domain && renderDomain(entry.domain) === renderDomain(domain) }"
                  @click="pickDomain(entry)"
                >
                  <span class="ed-dot" :class="{ on: (entry.flows ?? []).some((flow) => flow.producing) }"></span>
                  <span class="ed-name mono">{{ renderDomain(entry.domain) }}</span>
                  <!-- Its labels, and its `name` label among them — that is what an operator called
                       this domain. A labelled-but-unobserved one is how they see a label applied
                       before the producer came up: information, not an error. -->
                  <span class="ed-meta ed-labels" :title="labelTitle(entry)">{{ metaOf(entry) }}</span>
                  <span class="ed-count">{{ plural((entry.flows ?? []).length, 'flow') }}</span>
                </button>
                <!-- A sibling of the row's button, never a child: a `<button>` inside a `<button>`
                     is invalid, and the inner click would pick the domain on its way to labelling
                     it. Spelled as a word because it opens a panel rather than acting on the click. -->
                <button
                  type="button"
                  class="ed-tag"
                  title="Write this domain's labels"
                  @click="labelling = entry"
                >label</button>
              </div>
            </template>

            <template v-else>
              <p v-if="labelPairs.length === 0" class="ed-empty">
                No labels on this node's domains. The CLI writes them with
                <code>label domain</code>.
              </p>
              <button
                v-for="entry in labelPairs"
                :key="entry.pair"
                type="button"
                class="ed-item"
                :class="{ on: labels.includes(entry.pair) }"
                @click="toggleLabel(entry.pair)"
              >
                <span class="ed-check">{{ labels.includes(entry.pair) ? '✓' : '' }}</span>
                <span class="ed-name mono">{{ entry.pair }}</span>
                <span class="ed-meta"></span>
                <span class="ed-count">{{ plural(entry.count, 'domain') }}</span>
              </button>
              <p class="ed-empty">
                <template v-if="labels.length">
                  Matches {{ plural(matched.length, 'domain') }}:
                  {{ matched.map((entry) => renderDomain(entry.domain)).join(', ') || '·' }}. A domain
                  labelled later joins them.
                </template>
                <template v-else>
                  Pick one or more labels. They are ANDed, and an empty set is refused.
                </template>
              </p>
            </template>
          </div>
        </section>

        <section class="ed-step">
          <div class="ed-step-head"><span>which group</span></div>
          <div class="ed-list">
            <p v-if="groups.length === 0" class="ed-empty">Choose a domain above.</p>
            <button
              v-for="entry in groups"
              :key="entry.name"
              type="button"
              class="ed-item"
              :class="{ on: group === entry.name }"
              @click="pickGroup(entry)"
            >
              <span
                class="ed-dot"
                :class="{ on: entry.flows.some((flow) => flow.producing && !flow.excluded) }"
              ></span>
              <span class="ed-name">{{ groupLabel(entry.name) }}</span>
              <span class="ed-meta">{{ entry.name === WHOLE ? 'all: true' : (entry.types.join(' + ') || '·') }}</span>
              <span class="ed-count">
                {{ plural(entry.flows.filter((flow) => !flow.excluded).length, 'flow') }}
                <!-- A label selector never matches this node's own output — the server drops it as
                     `self_output` — and seeing it here is the difference between "why did it skip
                     three flows" and "of course it did". -->
                <template v-if="entry.flows.some((flow) => flow.excluded)">
                  · {{ entry.flows.filter((flow) => flow.excluded).length }} excluded
                </template>
              </span>
            </button>
          </div>
        </section>

        <section class="ed-step">
          <div class="ed-step-head">
            <span>how much of it</span>
            <div class="ed-modes">
              <label
                v-for="kind in (['all', 'type', 'flows'] as const)"
                :key="kind"
                class="ed-mode"
                :class="{ off: modeOff(kind) !== '' }"
                :title="modeOff(kind) || {
                  all: 'Every flow of this group, including ones the producer adds later',
                  type: 'One type of this group',
                  flows: 'Pin flows by ID. One source per flow.',
                }[kind]"
              >
                <input
                  type="radio"
                  :checked="mode === kind"
                  :disabled="modeOff(kind) !== ''"
                  @change="setMode(kind)"
                />
                {{ kind === 'type' ? 'select type' : kind === 'flows' ? 'select flows' : 'all' }}
              </label>
            </div>
          </div>

          <div class="ed-list">
            <p v-if="!current" class="ed-empty">Choose a group above.</p>

            <!-- The empty box is the point, so it says so rather than being left blank: `all` is a
                 standing selection and there is nothing to pick. -->
            <p v-else-if="current.name === WHOLE" class="ed-empty">
              Every flow in
              {{ domainKind === 'name' && domain ? renderDomain(domain) : 'every matching domain' }},
              {{ plural(current.flows.filter((flow) => !flow.excluded).length, 'flow') }} right now.
              <b>Nothing to select.</b> A flow the producer adds later joins it.
            </p>

            <p v-else-if="mode === 'all'" class="ed-empty">
              Every flow whose group hint is "{{ current.name }}",
              {{ plural(current.flows.length, 'flow') }} right now<template v-if="current.types.length">
                ({{ current.types.join(', ') }})</template>. <b>Nothing to select.</b> A flow the
              producer adds later joins it.
            </p>

            <template v-else-if="mode === 'type'">
              <p v-if="current.types.length === 0" class="ed-empty">None of these flows carries a type.</p>
              <button
                v-for="kind in current.types"
                :key="kind"
                type="button"
                class="ed-item"
                :class="{ on: type === kind }"
                @click="type = kind"
              >
                <span class="ed-dot" :class="{ on: current.flows.some((flow) => flow.group_hint?.type === kind && flow.producing) }"></span>
                <span class="ed-name">{{ kind }}</span>
                <span class="ed-meta"></span>
                <span class="ed-count">
                  {{ plural(current.flows.filter((flow) => flow.group_hint?.type === kind).length, 'flow') }}
                </span>
              </button>
            </template>

            <template v-else>
              <button
                v-for="flow in current.flows"
                :key="flow.id"
                type="button"
                class="ed-item"
                :class="{ on: flows.includes(flow.id) }"
                @click="toggleFlow(flow.id)"
              >
                <span class="ed-check">{{ flows.includes(flow.id) ? '✓' : '' }}</span>
                <span class="ed-name mono">{{ flow.id }}</span>
                <span class="ed-meta">{{ flow.excluded ? 'this node writes it' : (flow.group_hint?.type ?? '(no type)') }}</span>
                <span class="ed-count">{{ flow.producing ? 'producing' : 'idle' }}</span>
              </button>
            </template>
          </div>
        </section>

        <label class="ed-field">
          <span class="ed-label">route into</span>
          <!-- Shown and disabled with the reason rather than omitted, as everywhere else: a
               duplicate always creates a request, because its destinations are the ones it is
               copying and an existing request already has its own. -->
          <select
            v-model="target"
            :disabled="template !== undefined"
            :title="template ? 'A duplicate always creates a request' : ''"
          >
            <option value="">a new request</option>
            <option v-for="request in targets" :key="request.id" :value="request.id">
              {{ request.name }}
              ({{ plural(request.sources.length, 'source') }},
              {{ plural(request.destinations.length, 'destination') }}){{ staging.isDraft(request.id) ? '  (draft)' : '' }}
            </option>
          </select>
        </label>

        <label v-if="!joining" class="ed-field">
          <span class="ed-label">{{ namePrefix ? 'name prefix' : 'name' }}</span>
          <input v-model="name" class="mono" spellcheck="false" autocomplete="off" />
        </label>

        <!-- Only once there is something to describe: "creates 0 requests: —" is a sentence about
             nothing, and the line below already says what is missing. -->
        <p v-if="planned.length" class="ed-note">
          <template v-if="joining">
            <b>Adds {{ plural(planned.length, 'source') }} to {{ joining.name }}:</b>
            {{ planned.map((entry) => entry.label).join(', ') || '·' }}.
            <!-- A request is its sources against its destinations, so a new row lights every one of
                 its columns at once. Said before it commits, from the model rather than the dry run. -->
            <template v-if="joining.destinations.length">
              It lights {{ plural(joining.destinations.length, 'cell') }} on each.
            </template>
          </template>
          <template v-else>
            <b>Creates {{ plural(planned.length, 'request') }}:</b>
            {{ planned.map((entry) => `${entry.name} (${entry.label})`).join(', ') || '·' }}.
            <template v-if="template">
              Each gets {{ template.name }}'s {{ plural(template.destinations.length, 'destination') }}<template
                v-if="template.destinations.some((entry) => entry.disabled)"
              >, including the parked ones, which stay parked</template>, and its request-level
              settings.
            </template>
            <template v-else>
              A new request has no destinations yet. Its row goes on the axis.
            </template>
          </template>
        </p>

        <p v-if="failure" class="ed-fail">{{ failure }}</p>
        <p v-else-if="blocked" class="ed-note">{{ blocked }}</p>
        <p v-else class="ed-note">
          Selector: <span class="mono">{{ selectorLabel(planned[0]!.source.select) }}</span>. Nothing
          is written until Apply.
        </p>
      </div>

      <footer class="ed-foot">
        <span class="spacer" />
        <button class="ed-act" @click="emit('close')">cancel</button>
        <button class="ed-act ed-go" :disabled="blocked !== undefined" @click="add()">add</button>
      </footer>
    </section>

    <!-- Over this panel rather than instead of it: the operator is mid-choice, and a label written
         here changes the list they are choosing from — which is why the write reloads it. -->
    <LabelEditor
      v-if="labelling"
      :node="chosen"
      :domain="labelling.domain"
      :labels="labelling.labels"
      :observed="labelling.observed"
      @wrote="load(chosen)"
      @close="labelling = undefined"
    />
  </div>
</template>
