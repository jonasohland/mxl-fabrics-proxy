<script setup lang="ts">
/**
 * The routing matrix (`ui.md` §7a) — the reason to build a UI at all, and where the operator lives.
 *
 * Rows are sources, columns are destinations, a request is a **rectangle** over them. Both axes are
 * read out of the requests rather than out of inventory, because both are things the operator
 * *writes*: a row is a pair of selectors and a column is a domain that does not exist until a
 * request names it. Only the cells hold real objects.
 *
 * **The cell is the gesture, and it parks rather than deletes.** A click on a live leg switches it
 * off with the entry kept — which is §7a's default for clearing a cell ("drop the destination from
 * the request, which clears that column across the whole rectangle"), with the text left in the spec
 * so the column survives being switched off. Nothing here writes: every click stages, the staged set
 * is dry-run as a batch, and the bar at the foot of the screen is the confirmation.
 *
 * Two clicks this screen deliberately declines to interpret, and both are conditions the model
 * already knows rather than accidents:
 *
 * - **An unlit cell on a row that is a source of two requests.** A row is a query and is shared by
 *   every request naming the same three things, so there is no one request for a new leg to join.
 * - **A lit cell two requests name.** Bounded — exclusivity is enforced on materialised paths, so it
 *   holds only while the selector matches nothing — and which of the two legs a click means is not
 *   decidable from the grid.
 *
 * Two things this screen must not become, both from §7a:
 *
 * - **An exclusive crosspoint.** An operator trained on an SDI router expects the second click in a
 *   column to displace the first. It will not: a destination domain holds flows from many requests
 *   and fan-in is the supported way to land several sources in one domain. The column header counts
 *   its sources for exactly that reason, and no cell is drawn as a latched button on an exclusive bus.
 * - **A grid whose geometry depends on its content.** A cell is a state word and a count, both
 *   fixed-shape; cells in a row share a height and columns share a width, so one cell that reflows
 *   moves the whole grid under the pointer at the moment the operator is clicking in it. Reasons go
 *   in tooltips. Nothing conditional resizes anything — controls that do not apply are hidden, never
 *   omitted.
 */
import { computed, ref } from 'vue'

import type { Destination, Path, Request, Source } from '@/api/types'
import DestinationEditor from '@/components/DestinationEditor.vue'
import SourceEditor from '@/components/SourceEditor.vue'
import SplitEditor from '@/components/SplitEditor.vue'
import UnroutedStrip from '@/components/UnroutedStrip.vue'
import { plural } from '@/model/labels'
import type { Band, MatrixCell, MatrixColumn, MatrixRequest, MatrixRow } from '@/model/matrix'
import { buildMatrix, sourceKey } from '@/model/matrix'
import { endpointKey } from '@/model/ownership'
import type { LegEdit, SourceEdit, Verb, Want } from '@/model/staging'
import type { SourcePrefill } from '@/model/unrouted'
import { useFleetStore } from '@/stores/fleet'
import { useNamespaceStore } from '@/stores/namespaces'
import { useStagingStore } from '@/stores/staging'

const props = defineProps<{ namespace: string }>()

const fleet = useFleetStore()
const namespaces = useNamespaceStore()
const staging = useStagingStore()

const mode = computed(() => namespaces.mode(props.namespace))

/**
 * Built from the **staged** request list, not the server's.
 *
 * That is what makes the rectangle's own rule render itself rather than be explained: a request is
 * sources × destinations and has no notches, so staging a park on one leg darkens that column across
 * every row of its rectangle, in the grid, before anything is written. The paths underneath stay the
 * server's own — they exist until an apply stops them — which is why a staged cell says `staged` and
 * never borrows a state it has not reached.
 */
const matrix = computed(() =>
  buildMatrix(
    fleet.paths,
    staging.effectiveRequests,
    fleet.nodes,
    props.namespace,
    // The server's own list, for the one question that needs the difference: whether an undrawn path
    // is the server disagreeing with itself or the operator having staged the leg away.
    fleet.requests,
    // Destinations the operator has named and nothing routes yet. They have to be on the axis before
    // anything routes them, because naming one *is* how you get a cell to click.
    staging.columnsIn(props.namespace),
  ),
)

/**
 * Which panel is open, if any. One at a time.
 *
 * `duplicate` is the source editor with a rectangle to copy, not a panel of its own: §7a's duplicate
 * is *"copy the rectangle, change the selector, keep the destinations"*, and choosing the new
 * selector is the operation. A copy that kept the sources too would be a second request asking for
 * the identical paths, which exclusivity refuses the moment a producer appears.
 */
const editor = ref<'source' | 'destination' | 'split' | undefined>(undefined)

/** The rectangle a duplicate copies, and the (request, source) a split takes out. */
const template = ref<Request | undefined>(undefined)
const splitting = ref<{ request: Request; source: Source } | undefined>(undefined)

/**
 * The strip's click. It opens the same panel `+ source` does, with its first steps already made.
 *
 * A third way into one editor rather than a fourth panel, because *"see an unrouted flow, route it"*
 * and *"write a row"* are the same act arrived at from two directions — one of them knowing what it
 * is starting from. Anything the strip decided stays visible and changeable in the panel, so the
 * click can never author a selector nobody read.
 */
const prefill = ref<SourcePrefill | undefined>(undefined)

function openSource(copy?: Request): void {
  template.value = copy
  prefill.value = undefined
  editor.value = 'source'
}

function openFromStrip(wanted: SourcePrefill): void {
  prefill.value = wanted
  template.value = undefined
  editor.value = 'source'
}

function closeEditor(): void {
  editor.value = undefined
  template.value = undefined
  splitting.value = undefined
  prefill.value = undefined
}

/**
 * A row of a request nobody has created yet.
 *
 * Marked with colour and a title rather than with anything that takes up space — a badge that
 * appeared on a draft row would resize the header, and a table shares heights across a row.
 */
const rowIsDraft = (row: MatrixRow) => row.requests.length === 1 && staging.isDraft(row.requests[0]!)

const draftCount = computed(() => staging.draftsIn(props.namespace).length)

/** The name half of a rendered `<namespace>/<name>`. The whole grid is one namespace. */
const nameOf = (id: string) => id.slice(id.indexOf('/') + 1)

const cellAt = (row: number, column: number) => matrix.value.cells[row]![column]!

/** `--acc-N`, and only for a rectangle with more than one source: a 1×N rectangle is a row. */
const accentClass = (accent: number | undefined) => (accent === undefined ? '' : `acc-${accent}`)

const rowAccent = (row: MatrixRow) =>
  accentClass(matrix.value.requests.find((entry) => entry.rows.includes(row.index) && entry.accent !== undefined)?.accent)

/**
 * The rectangle badge, shown on a row that is one source of several.
 *
 * Rendered unconditionally and hidden with `visibility`, because a badge that appears when a second
 * source joins a request would resize the row header — and a table shares heights across a row.
 */
function badge(row: MatrixRow): { text: string; shown: boolean } {
  const sources = Math.max(
    ...matrix.value.requests
      .filter((entry) => entry.rows.includes(row.index))
      .map((entry) => entry.rows.length),
    1,
  )
  return { text: `⧉${sources} srcs`, shown: sources > 1 }
}

/** Registered but not leased is information, not an alarm — and not registered at all is a third. */
function lease(band: Band<unknown>): { text: string; shown: boolean } {
  if (band.live === undefined) return { text: 'not registered', shown: true }
  return { text: 'no agent', shown: !band.live }
}

// -- the gesture ------------------------------------------------------------

/**
 * What a click on this cell would stage, or why the screen declines to interpret it.
 *
 * The whole of the ambiguity is in `owners`. A **lit** cell is a leg of the requests whose rectangle
 * covers it; an **unlit** one would become a leg of whichever request owns the row. Either way, one
 * owner is a click and more than one is a question the grid has no way to ask — §7a's "one click the
 * screen declines to interpret", which is a rendered condition here rather than an accident.
 */
type Target =
  | { ok: true; request: string; want: Want; verb: Verb; destination: Destination; also: number }
  | { ok: false; reason: string }

function target(row: MatrixRow, cell: MatrixCell): Target {
  const column = matrix.value.columns[cell.column]!
  const owners = cell.lit ? cell.requests : row.requests

  if (owners.length !== 1) {
    return {
      ok: false,
      reason: cell.lit
        ? `${owners.length} requests name this pairing. Which of their legs a click means is not decidable here.`
        : `This source is a row of ${owners.length} requests. No single request for a new leg to join.`,
    }
  }

  const id = owners[0]!
  const rectangle = matrix.value.requests.find((entry) => entry.id === id)!
  const want: Want = cell.lit && !cell.parked ? 'off' : 'on'

  return {
    ok: true,
    request: id,
    want,
    verb: cell.lit ? (cell.parked ? 'enable' : 'park') : 'add',
    destination: column.destination,
    // A rectangle has no notches: the entry is per *destination*, so the click takes out — or lights
    // — that column across every row of the request. Said before it commits, which the grid can do
    // from the model rather than from the dry run.
    also: rectangle.rows.length - 1,
  }
}

/** The staged leg this cell is showing, if any. Keyed on the request, which the cell carries. */
function stagedLeg(cell: MatrixCell): LegEdit | undefined {
  const key = matrix.value.columns[cell.column]!.key
  return staging.pending.find(
    (edit): edit is LegEdit =>
      edit.target === 'leg' && edit.column === key && cell.requests.includes(edit.request),
  )
}

/**
 * A cell that exists because its **row** is staged.
 *
 * A source arriving lights every column of its request at once — that is the rectangle rule, and it
 * is why the source editor can say "lights 3 cells" before it commits. Those cells are as unwritten
 * as a staged leg is, so they must read the same way: `staged`, and never the request's own state or
 * a path count. Found by a live run, where such a cell reported `ESTABLISHING` — a state belonging
 * to the request's *other*, applied, source.
 */
function stagedSource(row: MatrixRow, cell: MatrixCell): SourceEdit | undefined {
  return staging.pending.find(
    (edit): edit is SourceEdit =>
      edit.target === 'source' &&
      edit.want === 'in' &&
      sourceKey(edit.source) === row.key &&
      cell.requests.includes(edit.request),
  )
}

/** Whichever of the two is showing. The leg first: it is the more specific claim on this cell. */
function stagedAt(row: MatrixRow, cell: MatrixCell): LegEdit | SourceEdit | undefined {
  return stagedLeg(cell) ?? stagedSource(row, cell)
}

/**
 * Whether this cell is drawn for something the server does not have, and the word for what it is.
 *
 * Three ways to be unwritten and they arrive from three directions: a staged **leg**, a staged
 * **source** whose row lights every column of its request at once, and a cell of a **draft**, where
 * the request itself has never been created. The third is why this asks about the request rather than
 * hunting for an edit — `duplicate` and `split` author a request's destinations with it, so those
 * cells have no edit of their own and would otherwise read as the draft's synthesised state.
 *
 * A cell two requests light is unwritten only if **both** are: one real claim means real paths.
 */
function unwritten(row: MatrixRow, cell: MatrixCell): string | undefined {
  const staged = stagedAt(row, cell)
  if (staged) return staging.verbOf(staged)
  if (cell.lit && cell.requests.every((id) => staging.isDraft(id))) {
    // A parked entry in a request that does not exist is a leg authored *off*, not one switched off.
    return cell.parked ? 'parked' : 'add'
  }
  return undefined
}

function click(row: MatrixRow, cell: MatrixCell): void {
  // A staged leg undoes rather than reversing: the effective grid already shows the staged world, so
  // the "opposite" click would read as parking a leg that is only staged to exist.
  const leg = stagedLeg(cell)
  if (leg) {
    staging.discardLeg(leg.request, leg.column)
    return
  }
  // A cell of a staged *row* has nothing of its own to toggle — the leg is the request's and already
  // applies to its other sources — so the click is declined rather than reinterpreted as parking the
  // whole column. The row's `×` is what discards it, and the tooltip says so.
  if (stagedSource(row, cell)) return

  const wanted = target(row, cell)
  if (!wanted.ok) return
  staging.set(wanted.request, matrix.value.columns[cell.column]!.key, wanted.want, wanted.destination)
}

/** Shown and declined, never inert and silent: the title is where the reason goes. */
function declined(row: MatrixRow, cell: MatrixCell): boolean {
  if (stagedLeg(cell)) return false
  return stagedSource(row, cell) !== undefined || !target(row, cell).ok
}

// -- the × controls ---------------------------------------------------------

/**
 * **`×` says what it costs, and never looks safe while it moves media.**
 *
 * It used to be offered only over a leg that was dark **on the server**, which made a small target in
 * the corner of a cell safe by construction. It also made deleting a live leg park, *apply*, then `×`
 * — an apply cycle in the middle of one intent, for the sole purpose of reaching a control. The
 * staged set is what makes that unnecessary: `gone` is a state like any other, it supersedes a park
 * staged on the same leg, and the batch dry run reports what stops before anything is written. So the
 * gate goes and the honesty moves into the title, which is where the row's `×` has always kept it.
 *
 * That leaves **one** rule on all three: a `×` names the paths it stops, in the title, in the
 * vocabulary of the thing it is attached to. `storedParked` survives for exactly that — a leg the
 * operator has only *staged* for parking is still carrying media, so it must not be described as
 * parked and safe.
 *
 * The ambiguity rules are untouched. They are about what a click can *mean*, not about what it costs:
 * a cell two requests light still has no single leg to remove.
 */
const storedById = computed(() => new Map(fleet.requests.map((request) => [request.id, request])))

/** Dark on the **server**, which is the only kind of dark that means "carrying nothing". */
function storedParked(request: string, column: string): boolean {
  const entry = storedById.value
    .get(request)
    ?.destinations.find((destination) => endpointKey(destination) === column)
  return entry?.disabled === true
}

/** The cell's `×`: takes the leg out of the one request the operator is looking at. */
function legRemovable(cell: MatrixCell): boolean {
  return cell.lit && cell.requests.length === 1
}

/**
 * The column's `×`: takes the destination out of **every** request that names it.
 *
 * Two `×`s on this side because "remove this destination" has two honest meanings — a domain several
 * requests write into is ordinary fan-in, and one control that guessed between them would be wrong
 * half the time, with the wrong half a bulk teardown.
 */
function columnRemovable(column: MatrixColumn): boolean {
  return column.draft || column.requests.length > 0
}

/**
 * Whether removing this much would leave a spec with nothing in it, which commits as a DELETE.
 *
 * Read off the **effective** spec rather than the stored one, because several removals can now be
 * staged before anything is applied: measured against the server, taking the last two destinations of
 * a three-destination request would report neither click as the one that empties it.
 */
const effectiveById = computed(
  () => new Map(staging.effectiveRequests.map((request) => [request.id, request])),
)

function wouldEmpty(request: string, dropping: { legs?: number; sources?: number }): boolean {
  const current = effectiveById.value.get(request)
  if (!current) return false
  return current.destinations.length === (dropping.legs ?? 0) ||
    current.sources.length === (dropping.sources ?? 0)
}

function removeLeg(cell: MatrixCell): void {
  const column = matrix.value.columns[cell.column]!
  staging.set(cell.requests[0]!, column.key, 'gone', column.destination)
}

function removeColumn(column: MatrixColumn): void {
  if (column.draft) {
    staging.discardColumn(props.namespace, column.key)
    return
  }
  for (const request of column.requests) staging.set(request, column.key, 'gone', column.destination)
}

/** The row's `×`. Ambiguous on a row two requests share, exactly as the cell's click is. */
function rowRemovable(row: MatrixRow): boolean {
  return row.requests.length === 1
}

// -- split ------------------------------------------------------------------

/**
 * `⧉→` — take this source out into a request of its own, keeping the destinations.
 *
 * The same ambiguity rule as the `×`: a row two requests share has no single request to split from.
 * Beyond that it wants a rectangle with something left behind — splitting the only source out of a
 * request is a rename, and renaming is not a thing the API can do (`(namespace, name)` is the ID, so
 * a changed name is a *new* request and the old one stays, still running).
 */
function splittable(row: MatrixRow): boolean {
  if (row.requests.length !== 1) return false
  const request = requestOf(row.requests[0]!)
  return request !== undefined && !staging.isDraft(request.id) && request.sources.length > 1
}

const requestOf = (id: string) =>
  matrix.value.requests.find((entry) => entry.id === id)?.request

function splitTitle(row: MatrixRow): string {
  if (row.requests.length !== 1) {
    return `This source is in ${plural(row.requests.length, 'request')}. Which one to split it out ` +
      `of is not decidable here.`
  }
  const request = requestOf(row.requests[0]!)
  if (request === undefined) return ''
  if (staging.isDraft(request.id)) return `${request.name} has not been created.`
  if (request.sources.length === 1) {
    return `${request.name} has one source, so there is nothing to keep behind.`
  }
  return `Split this source out of ${request.name} into a request of its own, keeping its ` +
    `${plural(request.destinations.length, 'destination')}.`
}

function openSplit(row: MatrixRow): void {
  if (!splittable(row)) return
  const request = requestOf(row.requests[0]!)!
  splitting.value = { request, source: row.source }
  editor.value = 'split'
}

/** `duplicate` is offered on the rectangle, because request-level things belong to the rectangle. */
function duplicateTitle(entry: MatrixRequest): string {
  if (staging.isDraft(entry.id)) return `${entry.name} has not been created.`
  return `A new request with ${entry.name}'s ` +
    `${plural(entry.request.destinations.length, 'destination')} and its settings, and a source you ` +
    `choose.`
}

function removeRow(row: MatrixRow): void {
  if (!rowRemovable(row)) return
  staging.removeSource(row.requests[0]!, row.source)
}

function legTitle(cell: MatrixCell): string {
  const id = cell.requests[0] ?? ''
  const request = nameOf(id)
  if (id && staging.isDraft(id)) {
    return `Drop this destination from ${request}, which has not been created. Nothing is written, ` +
      `so nothing stops.`
  }
  const empty = wouldEmpty(id, { legs: 1 })
    ? ` It is the last destination of ${request}, so applying deletes the request.`
    : ''
  // Parked on the *server* is the only kind that is carrying nothing. A leg the operator has staged
  // for parking still has its paths up, and a `×` over it stops them.
  if (storedParked(id, matrix.value.columns[cell.column]!.key)) {
    return `Remove this leg from ${request}. It is parked, so nothing stops.${empty}`
  }
  return cell.count > 0
    ? `Remove this leg from ${request}. It is carrying ${plural(cell.count, 'path')}, which stop.${empty}`
    : `Remove this leg from ${request}. It is carrying nothing.${empty}`
}

function columnTitle(column: MatrixColumn): string {
  if (column.draft) {
    return `Discard this destination. Nothing routes it yet, so nothing is written.`
  }
  const scope = `Remove ${column.node} ${column.domain} from ` +
    `${plural(column.requests.length, 'request')}.`
  return column.paths > 0
    ? `${scope} ${plural(column.paths, 'path')} land here, which stop.`
    : `${scope} Nothing lands here, so nothing stops.`
}

function rowTitle(row: MatrixRow): string {
  if (!rowRemovable(row)) {
    return `This source is in ${plural(row.requests.length, 'request')}. Which one to remove it ` +
      `from is not decidable here.`
  }
  const request = nameOf(row.requests[0]!)
  // A row that is only staged is undone rather than removed: staging the opposite state against the
  // same target replaces it, and an edit that asks for what the request already says is not a change
  // — so the `×` cancels the add and leaves nothing behind.
  if (staging.pending.some(
    (edit) => edit.target === 'source' && edit.want === 'in' && edit.request === row.requests[0]! &&
      sourceKey(edit.source) === row.key,
  )) {
    return `Discard this staged source. It has not been written, so nothing stops.`
  }
  if (rowIsDraft(row)) {
    // Nothing on the server to stop: the request has never been created, so this discards text.
    const rectangle = matrix.value.requests.find((entry) => entry.id === row.requests[0]!)
    return rectangle && rectangle.rows.length === 1
      ? `Discard this source. It is the only one of ${request}, so the whole draft goes with it.`
      : `Discard this source from ${request}, which has not been created. Nothing stops.`
  }
  if (wouldEmpty(row.requests[0]!, { sources: 1 })) {
    return `Remove this source from ${request}. It is its last, so applying deletes the request ` +
      `and ${plural(row.paths, 'path')} stop.`
  }
  // The one × that moves media: there is no flag on a source, so there is no dark state to require
  // first. It says so rather than being made to look safe.
  return row.paths > 0
    ? `Remove this source from ${request}. It is carrying ${plural(row.paths, 'path')}, which stop.`
    : `Remove this source from ${request}. It is carrying nothing.`
}

/** Second line of a cell. Fixed-shape, always — nothing of variable length goes in the box. */
function countLine(row: MatrixRow, cell: MatrixCell): string {
  // Never the request's own state and never a path count: the leg has not been written, so a cell
  // that borrowed either would show `ACTIVE` for something that does not exist yet. What the change
  // costs is on the pending bar, where its length costs nothing.
  const word = unwritten(row, cell)
  if (word) return word
  if (cell.parked) return 'parked'
  return cell.count === 0 ? '·' : plural(cell.count, 'path')
}

/** First line. `staged` is a word about the *screen*, so it is not one of the nine states. */
function stateLine(row: MatrixRow, cell: MatrixCell): string {
  if (unwritten(row, cell)) return 'staged'
  return cell.state ?? '·'
}

function stateClass(row: MatrixRow, cell: MatrixCell): string {
  return unwritten(row, cell) ? 'staged-word' : `state-${cell.state ?? 'none'}`
}

/**
 * Everything of variable length, where its length costs nothing.
 *
 * Server-authored prose is rendered verbatim — a reason is diagnostic data rather than UI copy, and
 * it is better than anything this screen would write.
 */
function cellTitle(row: MatrixRow, cell: MatrixCell): string {
  const lines: string[] = []

  if (cell.lit) {
    const held = cell.requests.map(nameOf).join(', ')
    lines.push(
      cell.requests.length > 1 ? `${held} · ${cell.requests.length} requests name this pairing` : held,
    )

    if (cell.requests.every((id) => staging.isDraft(id))) {
      lines.push(
        cell.parked
          ? 'Authored off. It will be created parked.'
          : 'Authored, not created. Nothing is written until Apply.',
      )
    } else if (cell.parked) {
      lines.push('Parked. The entry is in the spec and expands to nothing.')
    } else if (cell.paths.length === 0) {
      lines.push('No paths yet. The selectors match nothing here.')
    } else {
      for (const path of cell.paths) lines.push(pathLine(path))
    }
  }

  lines.push(gestureLine(row, cell))
  return lines.join('\n')
}

/**
 * What clicking would do, in the one place on this screen where length costs nothing.
 *
 * A control that is merely inert teaches nothing, so a cell the screen declines to interpret says why
 * rather than being silent — the same reason the grid control over a shared namespace is shown and
 * disabled rather than omitted.
 */
function gestureLine(row: MatrixRow, cell: MatrixCell): string {
  const leg = stagedLeg(cell)
  if (leg) return `Staged: ${staging.verbOf(leg)}. Click to undo. Nothing is written until Apply.`

  if (stagedSource(row, cell)) {
    return 'Here because its source is staged. Nothing to toggle until it is applied. The row\'s × ' +
      'discards it.'
  }

  const wanted = target(row, cell)
  if (!wanted.ok) return wanted.reason

  const also = wanted.also > 0
    ? ` Also ${wanted.want === 'off' ? 'parks' : 'lights'} ${plural(wanted.also, 'other cell')} of this request.`
    : ''

  if (wanted.verb === 'park') {
    return `Click to park this leg. It stops the media on it and keeps the entry in the spec.${also}`
  }
  return `Click to switch this leg on. Staged, and nothing is written until Apply.${also}`
}

const pathLine = (path: Path) =>
  `${path.id.slice(0, 8)}… ${path.source.flow.slice(0, 8)}… ${path.state}` +
  (path.reason ? ` · ${path.reason}` : '')
</script>

<template>
  <main class="page">
    <header class="head">
      <h1 class="mono">{{ namespace }}</h1>
      <span class="mode" :class="mode">⟨{{ mode }}⟩</span>

      <span class="counts">
        <!-- Drafts counted apart from requests, and spliced in without a space of its own: the board
             draws both, but a request the fleet does not have is not one an operator would find with
             `get requests`, and a count that merged them would disagree with the CLI by however many
             are authored. -->
        {{ plural(matrix.requests.length - draftCount, 'request')
        }}<span v-if="draftCount > 0" class="draft-word"> · {{ draftCount }} draft</span> ·
        {{ plural(matrix.pathCount, 'path') }} ·
        <span :class="{ attention: matrix.notActiveCount > 0 }">
          {{ matrix.notActiveCount }} not active
        </span>
      </span>

      <!-- No arrangement should produce one, so it is counted rather than assumed away: a grid that
           silently drops an edge is the failure this whole model exists to avoid. -->
      <span v-if="matrix.unplaced > 0" class="unplaced" title="Held paths this grid could not attribute to a row">
        {{ plural(matrix.unplaced, 'path') }} unplaced
      </span>

      <span class="spacer" />

      <!-- The two axes are things the operator *writes*, so each gets a control that writes one.
           Neither commits anything: a source becomes a row and a destination becomes a column, and
           the cell between them is still the gesture that routes them. -->
      <button class="new" title="A node, a domain selector and a flow selector"
              @click="openSource()">+ source</button>
      <button class="new" title="A node, an area it grants writing on, and a name"
              @click="editor = 'destination'">+ destination</button>
    </header>

    <p v-if="matrix.rows.length === 0 && matrix.columns.length === 0" class="dim empty">
      No requests in this namespace. <b>+ source</b> writes a row, <b>+ destination</b> writes a
      column.
    </p>

    <!-- Rendered with columns and no rows too: naming the first destination of an empty namespace
         has to put something on screen, or the control reads as having done nothing. -->
    <div v-else class="scroll">
      <table class="matrix">
        <!-- The widths are declared here rather than on the header cells, because under
             `table-layout: fixed` the first row decides them — and the first row is the node bands,
             whose colspans would hand a two-column band's width to whichever columns it covers. A
             column that is wider because of what sits above it is the ragged edge this rule exists
             to prevent. -->
        <colgroup>
          <col class="axis" />
          <col v-for="column in matrix.columns" :key="column.key" />
        </colgroup>

        <thead>
          <!-- The column bands. Twelve sources into one domain is 12× ingress on that node, which is
               the binding direction for an ingest wall — an edge is bounded by what it can take. -->
          <tr class="col-band">
            <th class="corner"></th>
            <th v-for="band in matrix.columnBands" :key="band.node" :colspan="band.items.length" class="band-head">
              <span class="band-label">
                <span class="mono node">{{ band.node }}</span>
                <span class="dim">{{ plural(band.paths, 'path') }} in</span>
                <span class="lease" :style="{ visibility: lease(band).shown ? 'visible' : 'hidden' }">
                  {{ lease(band).text }}
                </span>
              </span>
            </th>
          </tr>

          <tr class="col-head">
            <th class="corner"></th>
            <th
              v-for="column in matrix.columns"
              :key="column.key"
              :class="{ parked: column.parked, draft: column.draft }"
              :title="column.node + ' ' + column.domain +
                (column.draft ? ' · named here, routed by nothing yet. Nothing is written.' : '')"
            >
              <span class="line dest">
                <span class="mono">{{ column.domain }}</span>
                <!-- In flow, so it is rendered unconditionally and toggled with `visibility`: a
                     control that appeared when the last cell of a column went dark would resize
                     every column beside it, and a table shares widths down a column. -->
                <button
                  type="button"
                  class="drop-column"
                  :style="{ visibility: columnRemovable(column) ? 'visible' : 'hidden' }"
                  :title="columnTitle(column)"
                  @click="removeColumn(column)"
                >×</button>
              </span>
              <span class="line sub">
                <!-- A column is additive, and this is the number that says so. -->
                <span class="dim">{{ plural(column.sources, 'source') }}</span>
                <!-- Namespaces partition requests, not destinations. Another namespace writing here
                     is ordinary fan-in and is what makes "emptying this column empties the domain"
                     false — and a view of one namespace cannot see it any other way. -->
                <span
                  class="foreign"
                  :style="{ visibility: column.foreignNamespaces.length ? 'visible' : 'hidden' }"
                  :title="`also written into by: ${column.foreignNamespaces.join(', ')}`"
                >
                  + {{ column.foreignNamespaces.join(', ') }}
                </span>
              </span>
            </th>
          </tr>
        </thead>

        <tbody>
          <template v-for="band in matrix.rowBands" :key="band.node">
            <!-- The row bands. One source to five destinations is 5× egress on that source node. -->
            <tr class="band">
              <th class="band-head" :colspan="matrix.columns.length + 1">
                <span class="band-label">
                  <span class="mono node">{{ band.node }}</span>
                  <span class="dim">{{ plural(band.items.length, 'source') }}</span>
                  <span class="dim">{{ plural(band.paths, 'path') }} out</span>
                  <span class="lease" :style="{ visibility: lease(band).shown ? 'visible' : 'hidden' }">
                    {{ lease(band).text }}
                  </span>
                </span>
              </th>
            </tr>

            <tr v-for="row in band.items" :key="row.key">
              <!-- Exactly three lines, always. What a row *is*: who wants it, which domains, which
                   flows — and how much the query turned into. Request-level settings belong to the
                   rectangle and not here, because a row can be a source of more than one request. -->
              <th class="rowhead" :class="[rowAccent(row), { draft: rowIsDraft(row) }]">
                <span class="line names">
                  <span
                    class="mono name"
                    :title="rowIsDraft(row)
                      ? row.requests.join(', ') + ' · not created yet'
                      : row.requests.join(', ')"
                  >
                    {{ row.requests.map(nameOf).join(', ') }}
                  </span>
                  <span class="badge" :style="{ visibility: badge(row).shown ? 'visible' : 'hidden' }">
                    {{ badge(row).text }}
                  </span>
                  <!-- The one × that is allowed to move media, because a source has no parked state
                       to be put into first. Always rendered — a row always has a source to remove —
                       and `aria-disabled` where the row is shared, exactly as the cell is. -->
                  <!-- Kept together at the end of the line: they are the row's two controls and the
                       `×` reads as misplaced with something after it. -->
                  <span class="controls">
                    <button
                      type="button"
                      class="drop-row"
                      :aria-disabled="!rowRemovable(row)"
                      :title="rowTitle(row)"
                      @click="removeRow(row)"
                    >×</button>
                    <!-- The other answer to clearing one cell of a rectangle. In flow, so it is
                         rendered unconditionally and toggled with `visibility` — a control that
                         appeared when a request grew a second source would resize every row of the
                         header, and a table shares heights across a row.
                         A word rather than a glyph, and the same for `duplicate`: those two create a
                         name, which is the thing that makes them different in kind from the `×`s. -->
                    <button
                      type="button"
                      class="split-row"
                      :style="{ visibility: splittable(row) ? 'visible' : 'hidden' }"
                      :title="splitTitle(row)"
                      @click="openSplit(row)"
                    >split</button>
                  </span>
                </span>
                <span class="line mono dim" :title="row.domain">{{ row.domain }}</span>
                <span class="line sub">
                  <span class="dim selector" :title="row.selector">{{ row.selector }}</span>
                  <span class="dim flows" title="Distinct source flows this selector matched">
                    {{ row.flows }} fl
                  </span>
                </span>
              </th>

              <!-- A button, and one on every cell: an unlit cell is a click too, and a cell the
                   screen declines to interpret is `aria-disabled` with the reason in its title rather
                   than inert and silent. `disabled` would be the wrong spelling — it takes the
                   element out of the tab order and hides the tooltip that says why. -->
              <td v-for="column in matrix.columns" :key="column.key" class="cellbox">
                <button
                  type="button"
                  class="cell"
                  :class="[
                    accentClass(cellAt(row.index, column.index).accent),
                    {
                      lit: cellAt(row.index, column.index).lit,
                      parked: cellAt(row.index, column.index).parked,
                      duo: cellAt(row.index, column.index).requests.length > 1,
                      staged: unwritten(row, cellAt(row.index, column.index)) !== undefined,
                    },
                  ]"
                  :aria-disabled="declined(row, cellAt(row.index, column.index))"
                  :title="cellTitle(row, cellAt(row.index, column.index))"
                  @click="click(row, cellAt(row.index, column.index))"
                >
                  <template v-if="cellAt(row.index, column.index).lit">
                    <span class="line" :class="stateClass(row, cellAt(row.index, column.index))">
                      {{ stateLine(row, cellAt(row.index, column.index)) }}
                    </span>
                    <span class="line dim">{{ countLine(row, cellAt(row.index, column.index)) }}</span>
                  </template>
                  <span v-else class="line unlit">·</span>
                </button>

                <!-- A **sibling** of the cell, absolutely positioned over its corner, never a child:
                     a `<button>` inside a `<button>` is invalid, and the inner click bubbles, so a
                     nested × would park the leg on its way to removing it. Being out of flow then
                     buys the geometry rule for free — a control that took up space inside the cell
                     would resize every row it appeared in. -->
                <button
                  v-if="legRemovable(cellAt(row.index, column.index))"
                  type="button"
                  class="drop-leg"
                  :title="legTitle(cellAt(row.index, column.index))"
                  @click="removeLeg(cellAt(row.index, column.index))"
                >×</button>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <!-- The rectangle's own line. `PARTIAL` is the rectangle's word and it never appears on a path;
         the state is the server's own, never the local fold, because the server also folds leg
         failures that produce no path and there is nothing here to recompute them from. -->
    <section v-if="matrix.requests.length" class="requests">
      <h2>Requests</h2>
      <div v-for="entry in matrix.requests" :key="entry.id" class="request">
        <span class="swatch" :class="accentClass(entry.accent)"></span>
        <span class="mono col name" :title="entry.id">{{ entry.name }}</span>
        <!-- A draft has no state to report and the vocabulary has no word for *not written*, so it
             says what it is instead of borrowing one. `WAITING` is what the synthesised status
             carries and nothing renders it — this is the check that keeps that true. -->
        <span
          v-if="staging.isDraft(entry.id)"
          class="col state draft-word"
          title="Authored here, not created"
        >draft</span>
        <span v-else class="col state" :class="`state-${entry.request.status.state}`">
          {{ entry.request.status.state }}
        </span>
        <span class="col shape dim">
          {{ plural(entry.rows.length, 'source') }} × {{ plural(entry.columns.length, 'destination') }}
        </span>
        <span class="col tally dim">{{ plural(entry.paths, 'path') }}</span>
        <!-- §7a: request-level things belong to the rectangle rather than to a row, because a row
             can be a source of more than one request. This is the only one of them so far. -->
        <button
          type="button"
          class="dup"
          :aria-disabled="staging.isDraft(entry.id)"
          :title="duplicateTitle(entry)"
          @click="staging.isDraft(entry.id) ? undefined : openSource(entry.request)"
        >duplicate</button>
        <span class="reason">{{ entry.request.status.reason }}</span>
      </div>
    </section>

    <!-- The other half of the axes: the grid draws intent, and this is the inventory that intent has
         not reached. It sits below the rectangles because it is where the *next* one starts, and it
         is namespace-scoped with a note on entries another namespace routes — neither plain reading
         works (`ui.md` §7b). -->
    <UnroutedStrip :namespace="namespace" @route="openFromStrip" />

    <SourceEditor
      v-if="editor === 'source'"
      :namespace="namespace"
      :template="template"
      :prefill="prefill"
      @close="closeEditor()"
    />
    <DestinationEditor
      v-if="editor === 'destination'"
      :namespace="namespace"
      :columns="matrix.columns"
      @close="closeEditor()"
    />
    <SplitEditor
      v-if="editor === 'split' && splitting"
      :namespace="namespace"
      :request="splitting.request"
      :source="splitting.source"
      @close="closeEditor()"
    />
  </main>
</template>

<style scoped>
.page {
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 16px;
  overflow: auto;
  min-height: 0;
}

.head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}

h1 { font-size: 15px; margin: 0; }

.mode { color: var(--fg-dim); font-size: 12px; }
.mode.exclusive { color: var(--accent); }

.counts { color: var(--fg-dim); font-size: 12px; }

/* The two controls that write an axis. Quiet, because they open a panel rather than doing
   anything — everything they author still has to be applied. */
.new {
  background: none;
  border: 1px solid var(--line);
  border-radius: 3px;
  color: var(--fg-dim);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  padding: 2px 10px;
}

.new:hover { color: var(--fg); border-color: var(--fg-dim); }
.new:focus-visible { outline: 1px solid var(--accent); }

.attention { color: var(--s-establishing); }
.unplaced { color: var(--s-failed); font-size: 12px; }

h2 {
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-dim);
  margin: 0 0 8px;
}

/* The grid scrolls in both directions inside its own box, so the headers can stick to its edges
   rather than to the page's. */
.scroll {
  overflow: auto;
  max-height: 62vh;
  border: 1px solid var(--line);
  background: var(--bg-sunken);
}

/* Fixed layout, so every column is the same width whatever lands in it. Left to auto-size, a column
   holding `DISABLED`/`parked` comes out narrower than one holding `ESTABLISHING`/`2 paths` and the
   grid develops a ragged edge that tracks its contents — which `ui.md` §7a makes a correctness
   property rather than styling.
   `separate` rather than `collapse`, because a collapsed border is owned by the table and does not
   travel with a sticky cell: the header would scroll out from under its own rules. */
.matrix {
  table-layout: fixed;
  border-collapse: separate;
  border-spacing: 0;
  font-size: 12px;
}

col { width: 17ch; }
col.axis { width: 30ch; }

th, td {
  padding: 0;
  vertical-align: top;
  border-right: 1px solid var(--line-soft);
  border-bottom: 1px solid var(--line-soft);
}

/* Both axes stick, so a wide grid keeps saying what a cell is a pairing *of*. The corner outranks
   both because it belongs to each of them. */
.col-band th, .col-head th { position: sticky; z-index: 2; background: var(--bg-raised); }
.col-band th { top: 0; height: 24px; }
.col-head th { top: 24px; }
.rowhead { position: sticky; left: 0; z-index: 3; background: var(--bg-raised); }
.corner { position: sticky; left: 0; z-index: 4; }

.band-head, .corner { background: var(--bg-raised); }

/* The label is a span inside the cell rather than the cell itself: a `th` given `display: flex` stops
   being a table cell, and the bands stack instead of spanning their columns. */
.band-head { padding: 0; text-align: left; }
/* Height pinned, because the second header row sticks *below* this one and has to be told by how
   much. Its content is shorter than the box, so nothing here can push the two apart. */
.col-band .band-head { border-right: 1px solid var(--line); height: 24px; }
.col-band .band-label { padding: 3px 8px; }

/* A row band spans the whole grid, so its label rides the horizontal scroll rather than sliding out
   of view with the column it happens to start over. */
.band .band-head { border-right: none; background: var(--bg); }

.band-label {
  position: sticky;
  left: 0;
  display: flex;
  gap: 10px;
  align-items: baseline;
  padding: 4px 8px;
  width: max-content;
  overflow: hidden;
  white-space: nowrap;
}

.node { font-weight: 600; }
.lease { color: var(--s-establishing); font-size: 11px; }

.col-head .line, .rowhead .line, .cell .line {
  display: block;
  text-align: left;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.col-head th, .rowhead, .cell { padding: 4px 8px; font-weight: 400; }

/* A parked column is drawn, not blanked: an unlit cell says "nobody ever routed this" and a parked
   one says "somebody did and switched it off". Different sentences, read at a glance. */
.col-head th.parked .mono { color: var(--s-disabled); }

/* Authored and not written, in the pending bar's colour so that everything on this screen which has
   not reached the server reads as one statement. Colour and a title only: a badge or a border on a
   draft row would resize the header the moment one was authored, and a table shares heights across a
   row and widths down a column. */
.col-head th.draft .mono, .rowhead.draft .name { color: var(--pending); }

/* A line that carries two facts puts one at each end of the header it is in, so the second never
   runs into the first however long the first is — `group Studio A:Camera 1` and `2 fl` are two
   readings and must not read as one token. Specific enough to beat `.rowhead .line`'s `display:
   block`, which is otherwise the winning rule. */
/* `dest` rather than `head`: this stylesheet already has a `.head` — the page header — whose
   `flex-wrap: wrap` this line would inherit unopposed, since it is declared nowhere here. It did,
   and a hidden `×` wrapped onto a second row and made one column's header a line taller than its
   neighbours. A class-name collision inside one scoped stylesheet, invisible to every test because
   jsdom does no layout. */
.rowhead .line.sub, .rowhead .line.names, .col-head .line.sub, .col-head .line.dest {
  display: flex;
  gap: 8px;
  justify-content: space-between;
  align-items: baseline;
}

.foreign { color: var(--s-paused); }

.rowhead { text-align: left; }
.rowhead .name { color: var(--accent); overflow: hidden; text-overflow: ellipsis; }
.badge { color: var(--fg-faint); font-size: 11px; white-space: nowrap; }
.selector { overflow: hidden; text-overflow: ellipsis; }
.flows { font-variant-numeric: tabular-nums; white-space: nowrap; }

/* A button reset that changes no geometry. Every one of these is a browser default that would
   otherwise resize the cell — a border is layout, and a button's own font is not the table's. The
   padding stays where the shared `.col-head th, .rowhead, .cell` rule put it. */
/* Fills its box rather than being sized by its own content.
 *
 * A row is as tall as its header — three fixed lines — and a cell is two, so a button sized by its
 * content stops a line short of the field it is in: a strip of every row was neither painted in the
 * cell's state nor clickable, and the accent down a cell's leading edge was a stub rather than a rule.
 *
 * **Absolutely positioned rather than `height: 100%`,** which is the obvious fix and is not portable:
 * a percentage height against a table cell has no defined resolution, so it needs a `height` on the
 * `td` to resolve against — and Firefox then resolves against that declared value rather than the
 * cell's stretched one, giving a button the height of its own padding. Measured at 8px of a 61px row
 * in Firefox 153 against the built app. An inset box against the `td`'s padding box has one meaning
 * everywhere. */
.cell {
  position: absolute;
  inset: 0;
  display: block;
  width: 100%;
  margin: 0;
  border: 0;
  border-radius: 0;
  font: inherit;
  font-weight: 400;
  color: inherit;
  text-align: left;
  background: var(--bg-sunken);
  cursor: pointer;
}

/* The `×` lives over the cell's top-right corner, so the two lines reserve the width it occupies.
   Uniform across every cell, lit or not, because a padding that appeared only where the control does
   would be geometry that depends on content — the one thing this grid may not have. */
.cell .line { padding-right: 12px; }

.cell.lit { background: var(--bg); }

/* Hover and focus may change colour and nothing else: a table shares heights across a row and widths
   down a column, so anything that took up space here would move the grid under the pointer at the
   exact moment the operator is clicking in it. */
.cell:hover:not([aria-disabled='true']) { background: var(--bg-raised); }
.cell:focus-visible { outline: 1px solid var(--accent); outline-offset: -1px; }

/* Shown and declined rather than inert: the title says which ambiguity it is, and a cursor that
   refuses is the affordance that sends the operator to read it. */
.cell[aria-disabled='true'] { cursor: not-allowed; }

/* Parked keeps a lit cell's geometry exactly — two fixed-shape lines — because it *is* a lit cell in
   every sense except that it is asking for nothing. A cell that changed shape when switched off
   would resize its row. */
.cell.parked { background: transparent; }

.unlit { color: var(--fg-faint); }
.state-none { color: var(--fg-faint); }

/* The request accent is an inset shadow, never a border: a border is layout, so a row would grow the
   moment a second source joined its request. It marks the rectangle a cell belongs to — rows sort by
   node and a request's rows need not be adjacent, so there is no outline to draw. */
.acc-0 { --acc: var(--acc-0); }
.acc-1 { --acc: var(--acc-1); }
.acc-2 { --acc: var(--acc-2); }
.acc-3 { --acc: var(--acc-3); }
.acc-4 { --acc: var(--acc-4); }
.acc-5 { --acc: var(--acc-5); }

.cell.acc-0, .cell.acc-1, .cell.acc-2, .cell.acc-3, .cell.acc-4, .cell.acc-5,
.rowhead.acc-0, .rowhead.acc-1, .rowhead.acc-2, .rowhead.acc-3, .rowhead.acc-4, .rowhead.acc-5 {
  box-shadow: inset 2px 0 0 var(--acc);
}

/* Two requests naming one pairing. Bounded — exclusivity is enforced on materialised paths, so it
   holds only while the selector matches nothing and resolves into one INVALID the moment a producer
   appears — and it is the one place sharing markup earns its keep. */
.cell.duo { outline: 1px dashed var(--s-invalid); outline-offset: -1px; }

/* A staged cell, in the pending bar's own colour so the two read as one statement. The rectangle
   accent is carried through in the same shadow rather than lost: a staged cell is still a cell of
   whichever request it belongs to, and `--acc` is set on this element by the accent class. Both
   inset, because a border would be layout. */
.cell.staged {
  box-shadow: inset 2px 0 0 var(--acc, transparent), inset 0 0 0 1px var(--pending);
  background: color-mix(in srgb, var(--pending) 7%, var(--bg));
}

.staged-word { color: var(--pending); }

.controls { display: flex; gap: 6px; align-items: baseline; flex: none; }

/* The row's second control. Quiet like the `×`s and next to them, because it is the other answer to
   the same question — and unlike them it creates a name, which is what the panel behind it is for. */
.split-row {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 11px;
  line-height: 1;
  padding: 0 2px;
  flex: none;
}

.split-row:hover { color: var(--accent); }
.split-row:focus-visible { outline: 1px solid var(--accent); }

/* The three `×`s. Small, quiet and reached past something else — which is the whole reason the rule
   above exists: the big click is the destructive one and this is the tidying one. */
.drop-leg, .drop-column, .drop-row {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  line-height: 1;
  padding: 0 3px;
}

/* Never shrink the control and never let the label push it out: only the last track may depend on
   content, so the name ellipsises and the × keeps its corner. */
.drop-leg, .drop-column, .drop-row { flex: none; }
.col-head .line.dest .mono { overflow: hidden; text-overflow: ellipsis; min-width: 0; }

.drop-leg:hover, .drop-column:hover, .drop-row:hover { color: var(--s-failed); }
.drop-leg:focus-visible, .drop-column:focus-visible, .drop-row:focus-visible {
  outline: 1px solid var(--accent);
}

/* The positioning context for both of the boxes above: the `×`, which is over the cell's corner, and
   the cell itself. The `td` has to be the one that carries it because the cell is the button the `×`
   must not be inside.
 *
 * Its whole content is now out of flow, so the row takes its height from the row header alone. That
 * holds by the rule this grid is built on — a header is three fixed lines and a cell is two — and the
 * `min-height` is what keeps it from being load-bearing anyway. `lh` is exact where it is supported
 * and the pixel value covers the rest; neither is ever reached while the rule holds. */
.cellbox {
  position: relative;
  min-height: 42px;
  min-height: calc(2lh + 8px);
}

.drop-leg {
  position: absolute;
  top: 1px;
  right: 1px;
  z-index: 1;
}

/* The row's × is the destructive one, so it is drawn as such rather than as a twin of the other two.
   Declined where the row is shared, with the reason in the title — inert and silent teaches nothing. */
.drop-row[aria-disabled='true'] { color: var(--fg-faint); opacity: 0.35; cursor: not-allowed; }
.drop-row[aria-disabled='true']:hover { color: var(--fg-faint); }

.dim { color: var(--fg-dim); }
.empty { margin: 0; }

.request {
  display: grid;
  grid-template-columns: 2ch 26ch 13ch 26ch 10ch 9ch 1fr;
  column-gap: 12px;
  align-items: baseline;
  padding: 2px 0;
  border-bottom: 1px solid var(--line-soft);
}

.col { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.swatch {
  width: 8px;
  height: 8px;
  border-radius: 2px;
  background: var(--acc, transparent);
  align-self: center;
}

.request .state { font-size: 11px; letter-spacing: 0.04em; }
.draft-word { color: var(--pending); }

/* On the rectangle rather than on a row, because that is where request-level things belong. */
.dup {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 11px;
  padding: 0;
  text-align: left;
}

.dup:hover:not([aria-disabled='true']) { color: var(--accent); }
.dup[aria-disabled='true'] { opacity: 0.35; cursor: not-allowed; }
.request .tally { font-variant-numeric: tabular-nums; }
.reason { color: var(--fg-faint); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
