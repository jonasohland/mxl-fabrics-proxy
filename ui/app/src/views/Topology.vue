<script setup lang="ts">
/**
 * The topology view — `ui.md` §7's third item: nodes as vertices, paths as edges, coloured by state.
 *
 * `model/topology.ts` carries what it is for and why it is fleet-wide. What is decided here is how it
 * is drawn, and the decisions are geometry ones:
 *
 * **Hand-rolled SVG, no graph library.** The same reasoning as "no component library" for the grid:
 * every layout property this screen needs is a correctness property — determinism, left-to-right
 * layers, no motion — and a library's defaults are the opposite of all three. What a force-directed
 * layout would give is a picture that settles differently every time it is opened and drifts while
 * the operator is reading it, over data that is polled every three seconds. This costs about eighty
 * lines of arithmetic and it can be tested without a DOM, because the model stops at `(layer, order)`
 * and only the mapping to pixels lives here.
 *
 * **Nothing animates and nothing moves on a poll.** A node's box is at `(layer, order)` and both come
 * out of a pure function of the sorted fleet, so a poll that changes no relationship changes no
 * pixel. A state change repaints a stroke and nothing else.
 *
 * **Colour is `--mark`, not `color`.** `styles/base.css` carries the state palette twice for exactly
 * this: setting `color` on a container repaints every piece of text inside it, so a node box that
 * took its state as `color` would render its own name in red. The stroke reads `var(--mark)` and the
 * text keeps the foreground.
 *
 * **A read view, and the click is selection rather than wiring.** §7a forbids the graph as an editor
 * — a vertex is a node, and no request names a node pair — so a click focuses: it dims what the
 * selection is not about and fills the panel below, where the identifiers become links in the manner
 * of every other screen. The panel is where this view hands off to `describe`.
 */
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import { endpointText } from '@/model/detail'
import { plural, shortId } from '@/model/labels'
import { namespaceOfId } from '@/model/staging'
import type { TopologyEdge, TopologyNode } from '@/model/topology'
import { buildTopology } from '@/model/topology'
import { nodeRoute, pathRoute, requestRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()

/**
 * No read of its own. Both inputs are already on the single poll, which makes this the only screen
 * in the app that adds a view without adding load — `ui.md` §7 calls it "one `GET /v1/paths` away"
 * and it turns out to be nearer than that.
 */
const topology = computed(() => buildTopology(fleet.paths, fleet.nodes))

// -- geometry ---------------------------------------------------------------

const NODE_W = 172
const NODE_H = 40
const GAP_X = 104
const GAP_Y = 22
const MARGIN = 20
/** Room to the right of the last column for a self-loop's arc, so it cannot fall off the canvas. */
const LOOP_ROOM = 60

const columnX = (layer: number) => MARGIN + layer * (NODE_W + GAP_X)
const rowY = (order: number) => MARGIN + order * (NODE_H + GAP_Y)

const positioned = computed(() =>
  topology.value.layers.flat().map((node) => ({
    node,
    x: columnX(node.layer),
    y: rowY(node.order),
  })),
)

const positionOf = computed(() => new Map(positioned.value.map((entry) => [entry.node.name, entry])))

const canvas = computed(() => {
  const rows = Math.max(0, ...topology.value.layers.map((layer) => layer.length))
  const columns = topology.value.layers.length
  return {
    width: columns === 0 ? 0 : columnX(columns - 1) + NODE_W + MARGIN + LOOP_ROOM,
    // Back edges bow below the last row, so the canvas has to hold the bow as well as the boxes.
    height: rows === 0 ? 0 : rowY(rows - 1) + NODE_H + MARGIN + (hasBack.value ? 70 : 0),
  }
})

const hasBack = computed(() => topology.value.edges.some((edge) => edge.back))

interface Drawn {
  edge: TopologyEdge
  d: string
  /** The arrowhead, as an explicit polygon rather than a marker — see the note below. */
  head: string
  labelX: number
  labelY: number
}

/**
 * A point on a cubic bezier, used to put an edge's label **on its own curve**.
 *
 * The obvious placement — the midpoint of the straight line between the two boxes — puts every edge
 * of a crossing pair at almost the same coordinate, so a fan-out of four renders as two legible
 * numbers and two piled on top of each other. Sampled early on the curve instead, the labels
 * separate by where the edges *leave*, which is exactly where a fan-out differs.
 */
function bezierAt(t: number, p: number[], c1: number[], c2: number[], p3: number[]): number[] {
  const u = 1 - t
  const [a, b, c, d] = [u * u * u, 3 * u * u * t, 3 * u * t * t, t * t * t]
  return [
    a * p[0]! + b * c1[0]! + c * c2[0]! + d * p3[0]!,
    a * p[1]! + b * c1[1]! + c * c2[1]! + d * p3[1]!,
  ]
}

/**
 * Where along each curve its label sits, **staggered across a node's fan-out**.
 *
 * Sampling every edge at the same `t` is not enough on its own, and the screenshot is what said so:
 * two edges leaving one node share their start point and separate only slowly, so at any single `t`
 * near the source their labels land within a few pixels of each other. Both failure modes are fixed
 * by one rule — early, so edges *converging* on a target are still far apart, and stepped by the
 * edge's index within its source's fan, so edges *diverging* from a source are spread along their
 * curves rather than stacked across them.
 */
const labelT = (fanIndex: number) => Math.min(0.16 + fanIndex * 0.15, 0.62)

/**
 * The arrowhead is drawn per edge rather than as an SVG `<marker>`.
 *
 * A marker's fill does not inherit the stroke of the path that uses it — `context-stroke` is the
 * feature for that and its support is uneven — so a shared marker means either one arrowhead colour
 * for every state, or nine markers and a lookup. Emitting the triangle beside the curve costs three
 * numbers and lets it carry the same class as its edge, which is what makes the colour a single
 * source of truth.
 */
const drawn = computed<Drawn[]>(() => {
  // How many edges this source has already contributed. `topology.edges` is sorted by `(from, to)`,
  // so a node's fan arrives together and the index is stable across polls for the same fleet.
  const fan = new Map<string, number>()

  return topology.value.edges.flatMap((edge) => {
    const from = positionOf.value.get(edge.from)
    const to = positionOf.value.get(edge.to)
    if (!from || !to) return []

    const fanIndex = fan.get(edge.from) ?? 0
    fan.set(edge.from, fanIndex + 1)

    if (edge.loop) {
      const x = from.x + NODE_W
      const top = from.y + NODE_H * 0.28
      const bottom = from.y + NODE_H * 0.72
      return [{
        edge,
        d: `M ${x} ${top} C ${x + 46} ${top - 16}, ${x + 46} ${bottom + 16}, ${x} ${bottom}`,
        head: `${x},${bottom} ${x + 9},${bottom - 4.5} ${x + 9},${bottom + 4.5}`,
        labelX: x + 30,
        labelY: from.y + NODE_H / 2 + 4,
      }]
    }

    const y1 = from.y + NODE_H / 2
    const y2 = to.y + NODE_H / 2

    if (edge.back) {
      // Only reachable inside a cycle, which the server refuses. Bowed under everything and dashed,
      // so it reads as the anomaly it is rather than as a line somebody has to trace.
      const x1 = from.x
      const x2 = to.x + NODE_W
      const floor = Math.max(y1, y2) + 58
      return [{
        edge,
        d: `M ${x1} ${y1} C ${x1 - 50} ${floor}, ${x2 + 50} ${floor}, ${x2} ${y2}`,
        head: `${x2},${y2} ${x2 + 9},${y2 - 4.5} ${x2 + 9},${y2 + 4.5}`,
        labelX: (x1 + x2) / 2,
        labelY: floor - 6,
      }]
    }

    const x1 = from.x + NODE_W
    const x2 = to.x
    const bend = Math.max(30, (x2 - x1) / 2)
    const [labelX, labelY] = bezierAt(
      labelT(fanIndex), [x1, y1], [x1 + bend, y1], [x2 - bend, y2], [x2, y2],
    )
    return [{
      edge,
      d: `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}`,
      head: `${x2},${y2} ${x2 - 9},${y2 - 4.5} ${x2 - 9},${y2 + 4.5}`,
      labelX: labelX!,
      labelY: labelY! - 5,
    }]
  })
})

/** Thicker for more paths, capped: it is a reading of how much rides the edge, not a measurement. */
const weight = (edge: TopologyEdge) => Math.min(1.2 + edge.paths.length * 0.45, 4)

/** SVG text does not ellipsise, so the budget is taken here and the full name goes in a `<title>`. */
const label = (name: string) => (name.length > 21 ? `${name.slice(0, 20)}…` : name)

// -- focus ------------------------------------------------------------------

/**
 * Two independent ways to narrow what is emphasised, and they compose rather than replacing one
 * another: a **namespace highlight**, which is how a fleet-wide view answers "which of this is mine"
 * without being scoped and cutting a chain in half; and a **selection**, which is the click.
 *
 * Both dim rather than hide. A topology that removed what it was not about would be a different
 * graph on every click, and the operator would lose the shape they were reading.
 */
const highlight = ref('')

type Selection = { kind: 'node'; name: string } | { kind: 'edge'; key: string }

const selected = ref<Selection | undefined>()

/**
 * Resolved against the **current** topology every time, rather than held as an object.
 *
 * The fleet is replaced wholesale on every poll, so a selection that held a `TopologyEdge` would go
 * stale in three seconds and keep a panel open over an edge that no longer exists. Holding the key
 * and re-finding it is the same discipline `model/staging.ts` argues for an edit: the selection is a
 * statement about *which* thing, and the thing itself comes from the freshest read.
 */
const selectedEdge = computed(() => {
  const current = selected.value
  if (current?.kind !== 'edge') return undefined
  return topology.value.edges.find((edge) => edge.key === current.key)
})

const selectedNode = computed(() => {
  const current = selected.value
  if (current?.kind !== 'node') return undefined
  return topology.value.nodes.find((node) => node.name === current.name)
})

/** The edges of the selected node, or the selected edge alone. Empty when nothing is selected. */
const focusEdges = computed(() => {
  if (selectedEdge.value) return [selectedEdge.value]
  const node = selectedNode.value
  if (!node) return []
  return topology.value.edges.filter((edge) => edge.from === node.name || edge.to === node.name)
})

function edgeLit(edge: TopologyEdge): boolean {
  if (highlight.value && !edge.namespaces.includes(highlight.value)) return false
  if (!selected.value) return true
  return focusEdges.value.some((entry) => entry.key === edge.key)
}

function nodeLit(node: TopologyNode): boolean {
  if (selected.value?.kind === 'node') {
    return node.name === selected.value.name ||
      focusEdges.value.some((edge) => edge.from === node.name || edge.to === node.name)
  }
  if (selectedEdge.value) {
    return node.name === selectedEdge.value.from || node.name === selectedEdge.value.to
  }
  if (highlight.value) {
    return topology.value.edges.some(
      (edge) =>
        edge.namespaces.includes(highlight.value) && (edge.from === node.name || edge.to === node.name),
    )
  }
  return true
}

/** Clicking the current selection clears it, so the same target both focuses and releases. */
function pick(next: Selection): void {
  const current = selected.value
  const same =
    current !== undefined &&
    (current.kind === 'node' && next.kind === 'node'
      ? current.name === next.name
      : current.kind === 'edge' && next.kind === 'edge' && current.key === next.key)
  selected.value = same ? undefined : next
}

const nodeTitle = (node: TopologyNode) =>
  `${node.name}\n${plural(node.in, 'path')} in · ${plural(node.out, 'path')} out` +
  (node.relay ? '\nBoth a destination and a source. The middle of a chain.' : '') +
  (node.registered
    ? node.live ? '' : '\nRegistered, with no agent holding the lease.'
    : '\nNot in /v1/nodes, and a path names it.')

const edgeTitle = (edge: TopologyEdge) =>
  `${edge.from} → ${edge.to}\n${plural(edge.paths.length, 'path')} · ` +
  `${plural(edge.flows, 'flow')} · ${edge.namespaces.join(', ') || 'no namespace'}` +
  (edge.loop ? '\nSame node both ends. A loopback.' : '') +
  (edge.back ? '\nPoints backwards. These nodes are in a cycle.' : '')
</script>

<template>
  <main class="page">
    <header class="head">
      <h1>Topology</h1>

      <span class="counts">
        {{ plural(topology.layers.flat().length, 'node') }} ·
        {{ plural(topology.edges.length, 'edge') }} ·
        {{ plural(topology.pathCount, 'path') }}
        <span
          v-if="topology.relays > 0"
          class="relays"
          title="Nodes with a path in and a path out"
        > · {{ topology.relays }} relaying</span>
      </span>

      <!-- Neither should ever be non-zero, so both are counted rather than assumed away — the same
           reason the grid counts unplaced paths. A drawing that quietly straightened a cycle would
           be the one picture that cannot be checked against the fleet. -->
      <span v-if="topology.cyclic.length" class="alarm" :title="topology.cyclic.join(', ')">
        {{ plural(topology.cyclic.length, 'node') }} in a cycle
      </span>
      <span v-if="topology.unregistered.length" class="alarm" :title="topology.unregistered.join(', ')">
        {{ plural(topology.unregistered.length, 'endpoint') }} not registered
      </span>

      <span class="spacer" />

      <!-- A highlight, never a filter. Fleet-wide is the whole point: a chain may cross namespaces —
           hop one in `production` writes into edge-01, hop two in `archive` reads from it — so a
           scoped topology draws two unrelated stubs and loses the thing the view is for. -->
      <label class="pick" title="Dim everything no request of this namespace holds">
        highlight
        <select v-model="highlight">
          <option value="">all namespaces</option>
          <option v-for="entry in fleet.namespaces" :key="entry.name" :value="entry.name">
            {{ entry.name }}
          </option>
        </select>
      </label>
    </header>

    <p v-if="topology.layers.length === 0" class="empty">
      No paths in the fleet.
      <template v-if="topology.isolated.length">
        {{ plural(topology.isolated.length, 'node') }} registered and idle, below.
      </template>
    </p>

    <div v-else class="scroll">
      <!-- A click on the background clears the selection: the graph is the thing, and a focus an
           operator cannot get out of without hunting for the same small target again is a trap. -->
      <svg
        class="graph"
        :width="canvas.width"
        :height="canvas.height"
        :viewBox="`0 0 ${canvas.width} ${canvas.height}`"
        @click="selected = undefined"
      >
        <!-- Edges under the boxes, so a curve arriving at a node passes behind it rather than
             across its name. -->
        <g class="edges">
          <g
            v-for="entry in drawn"
            :key="entry.edge.key"
            class="edge"
            :class="[`mark-${entry.edge.state ?? 'WAITING'}`, {
              dim: !edgeLit(entry.edge),
              on: selectedEdge?.key === entry.edge.key,
              back: entry.edge.back,
            }]"
            @click.stop="pick({ kind: 'edge', key: entry.edge.key })"
          >
            <title>{{ edgeTitle(entry.edge) }}</title>
            <!-- A transparent fat stroke under the visible one: a 1.5px curve is a target an
                 operator has to aim at, and this view's only gesture is clicking edges. -->
            <path class="hit" :d="entry.d" />
            <path class="line" :d="entry.d" :stroke-width="weight(entry.edge)" />
            <polygon class="head" :points="entry.head" />
            <text class="count" :x="entry.labelX" :y="entry.labelY">{{ entry.edge.paths.length }}</text>
          </g>
        </g>

        <g class="nodes">
          <g
            v-for="entry in positioned"
            :key="entry.node.name"
            class="node"
            :class="[`mark-${entry.node.state ?? 'WAITING'}`, {
              dim: !nodeLit(entry.node),
              on: selectedNode?.name === entry.node.name,
              relay: entry.node.relay,
              unregistered: !entry.node.registered,
            }]"
            @click.stop="pick({ kind: 'node', name: entry.node.name })"
          >
            <title>{{ nodeTitle(entry.node) }}</title>
            <rect :x="entry.x" :y="entry.y" :width="NODE_W" :height="NODE_H" rx="3" />
            <!-- The state as a bar down the leading edge rather than as the box's own colour: a
                 filled box would put coloured text on a coloured ground at every state. -->
            <rect class="bar" :x="entry.x" :y="entry.y" width="3" :height="NODE_H" />
            <text class="name" :x="entry.x + 10" :y="entry.y + 17">{{ label(entry.node.name) }}</text>
            <text class="meta" :x="entry.x + 10" :y="entry.y + 31">
              {{ entry.node.in }} in · {{ entry.node.out }} out<template v-if="entry.node.relay"> · relay</template>
            </text>
            <text v-if="entry.node.registered && !entry.node.live" class="lease" :x="entry.x + NODE_W - 10" :y="entry.y + 31">
              no agent
            </text>
          </g>
        </g>
      </svg>
    </div>

    <!-- Registered and idle. Kept out of the graph on purpose: in layer 0 they would read as
         *origins*, and "this archive is the source of something" is a different and wrong sentence
         from "nothing is routed through this archive". -->
    <p v-if="topology.isolated.length" class="idle">
      <span class="label">not routed</span>
      <span class="mono">
        <template v-for="(node, i) in topology.isolated" :key="node.name">
          <span v-if="i">, </span>
          <RouterLink class="link" :to="nodeRoute(node.name)">{{ node.name }}</RouterLink>
        </template>
      </span>
    </p>

    <!-- Where the graph hands off to `describe`. Every identifier here is a link, as everywhere
         else: the graph answers "what is talking to what", and the next question is always about
         one of the things in it. -->
    <section v-if="selectedNode || selectedEdge" class="panel">
      <h2 v-if="selectedNode">
        <RouterLink class="link mono" :to="nodeRoute(selectedNode.name)">{{ selectedNode.name }}</RouterLink>
        <span class="dim">
          {{ plural(selectedNode.in, 'path') }} in · {{ plural(selectedNode.out, 'path') }} out
        </span>
        <span v-if="selectedNode.relay" class="relays" title="Both a destination and a source">relay</span>
      </h2>
      <h2 v-else-if="selectedEdge">
        <RouterLink class="link mono" :to="nodeRoute(selectedEdge.from)">{{ selectedEdge.from }}</RouterLink>
        <span class="dim">→</span>
        <RouterLink class="link mono" :to="nodeRoute(selectedEdge.to)">{{ selectedEdge.to }}</RouterLink>
        <span class="dim">
          {{ plural(selectedEdge.paths.length, 'path') }} · {{ plural(selectedEdge.flows, 'flow') }}
        </span>
      </h2>

      <table class="paths">
        <thead>
          <tr>
            <th>path</th><th>from</th><th>to</th><th>state</th><th>held by</th><th class="wide">reason</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="path in focusEdges.flatMap((edge) => edge.paths)" :key="path.id">
            <td class="mono">
              <RouterLink class="link" :to="pathRoute(path.id)" :title="path.id">
                {{ shortId(path.id) }}…
              </RouterLink>
            </td>
            <td class="mono">
              <RouterLink class="link" :to="`/flows/${path.source.flow}`">
                {{ path.source.node }} {{ path.source.domain }} {{ shortId(path.source.flow) }}…
              </RouterLink>
            </td>
            <td class="mono">{{ endpointText(path.destination) }}</td>
            <td :class="`state-${path.state}`">{{ path.state }}</td>
            <!-- `path.requests[]` is the refcount and the only statement of ownership in the API. -->
            <td class="mono holders">
              <template v-for="(id, i) in path.requests" :key="id">
                <span v-if="i">, </span>
                <RouterLink class="link" :to="requestRoute(id)" :title="`namespace ${namespaceOfId(id)}`">
                  {{ id }}
                </RouterLink>
              </template>
            </td>
            <td class="wide">{{ path.reason }}</td>
          </tr>
        </tbody>
      </table>
    </section>
  </main>
</template>

<style scoped>
.page {
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 14px;
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

.counts { color: var(--fg-dim); font-size: 12px; }
.relays { color: var(--s-paused); }
.alarm { color: var(--s-failed); font-size: 12px; }
.empty { color: var(--fg-dim); margin: 0; font-size: 12px; }

.pick { color: var(--fg-dim); font-size: 12px; display: flex; align-items: baseline; gap: 6px; }

.scroll {
  overflow: auto;
  max-height: 60vh;
  border: 1px solid var(--line);
  background: var(--bg-sunken);
}

.graph { display: block; }

/* -- edges ---------------------------------------------------------------- */

.edge { cursor: pointer; }

/* The visible stroke and the arrowhead both take the state's colour from `--mark`, which the
   `mark-*` classes in base.css set. `--mark` and not `color`, so nothing inside a node repaints. */
.edge .line {
  fill: none;
  stroke: var(--mark, var(--fg-faint));
  stroke-linecap: round;
}

.edge .head { fill: var(--mark, var(--fg-faint)); }

/* Invisible, wide, and first: the click target. Without it every edge is a 1.5px line to aim at. */
.edge .hit {
  fill: none;
  stroke: transparent;
  stroke-width: 14;
}

/* A halo in the canvas colour, painted under the glyph: the label sits *on* its own curve, and
   without this the line runs straight through the digits. `paint-order` is what makes the stroke go
   behind the fill rather than thickening it. */
.edge .count {
  fill: var(--fg-faint);
  font-size: 9px;
  text-anchor: middle;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  pointer-events: none;
  paint-order: stroke;
  stroke: var(--bg-sunken);
  stroke-width: 3px;
  stroke-linejoin: round;
}

.edge.back .line { stroke-dasharray: 5 4; }

.edge:hover .line, .edge.on .line { stroke-width: 3; }
.edge:hover .count, .edge.on .count { fill: var(--fg); }

/* Dimmed, never removed: a graph that dropped what it was not about would be a different shape on
   every click, and the operator would lose the picture they were reading. */
.edge.dim { opacity: 0.13; }

/* -- nodes ---------------------------------------------------------------- */

.node { cursor: pointer; }

.node rect {
  fill: var(--bg-raised);
  stroke: var(--line);
  stroke-width: 1;
}

.node .bar { fill: var(--mark, var(--fg-faint)); stroke: none; }

.node .name {
  fill: var(--fg);
  font-size: 11.5px;
  font-weight: 600;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  pointer-events: none;
}

.node .meta, .node .lease {
  fill: var(--fg-dim);
  font-size: 10px;
  pointer-events: none;
}

.node .lease { fill: var(--s-establishing); text-anchor: end; }

/* A relay is the one thing on this screen worth finding at a glance, so it is the one vertex that
   is drawn differently rather than merely labelled. */
.node.relay rect { stroke: var(--fg-dim); }

.node.unregistered rect { stroke: var(--s-failed); stroke-dasharray: 4 3; }

.node:hover rect, .node.on rect { stroke: var(--accent); }
.node.dim { opacity: 0.22; }

/* -- the panel ------------------------------------------------------------ */

.idle {
  display: grid;
  grid-template-columns: 14ch 1fr;
  column-gap: 12px;
  margin: 0;
  color: var(--fg-dim);
  font-size: 12px;
}

.idle .label { color: var(--s-establishing); }

.panel h2 {
  font-size: 12px;
  font-weight: 600;
  margin: 0 0 6px;
  display: flex;
  align-items: baseline;
  gap: 10px;
}

.panel .dim { color: var(--fg-dim); font-weight: 400; }

.paths { border-collapse: collapse; width: 100%; font-size: 12px; }

.paths th {
  text-align: left;
  font-weight: 400;
  color: var(--fg-dim);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  font-size: 10px;
  padding: 0 12px 3px 0;
  border-bottom: 1px solid var(--line);
}

.paths td {
  padding: 3px 12px 3px 0;
  vertical-align: top;
  border-bottom: 1px solid var(--line-soft);
  white-space: nowrap;
}

/* Everything but the reason shrinks to its content, so the sentence explaining a state is not
   separated from it by a hand's width of nothing. */
.paths .wide { width: 100%; white-space: normal; color: var(--fg-dim); }
.paths .holders { color: var(--accent); }
</style>
