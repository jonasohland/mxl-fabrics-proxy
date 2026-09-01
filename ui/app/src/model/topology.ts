/**
 * The topology view (`ui.md` §7, item 3) — nodes as vertices, paths as edges, coloured by state.
 *
 * It exists for two facts that are **obvious in a graph and invisible in a table**, and both are
 * about a node's position rather than about any one path:
 *
 * - **Chains.** `A→B→C` is written by naming, on the second hop, the domain the first hop
 *   materialised (§3). In `/v1/paths` that is two unrelated rows; here it is a node with an edge in
 *   and an edge out, and there is nowhere else in the product it can be seen at all.
 * - **Fan-in.** Twelve sources into one node is 12× ingress on it, which is the binding direction for
 *   an ingest wall — an edge is bounded by what it can take. The matrix's column header counts
 *   sources within one namespace; this counts them across the fleet.
 *
 * ## Fleet-wide, and that is the load-bearing decision
 *
 * Every other namespace-aware screen in this app scopes to one. This one must not, and the reason is
 * the first fact above: **a chain may cross namespaces.** Hop one in `production` writes into
 * `edge-01`, hop two in `archive` reads from it — namespaces partition requests, not nodes or
 * destinations (§7b) — so a namespace-scoped topology draws two unrelated stubs and destroys exactly
 * the thing the view is for. It follows the landing page's rule rather than the workspace's: the
 * subject is the fleet, and a namespace is a **highlight** over it rather than a filter through it.
 *
 * ## A read view, not an editor
 *
 * §7a settles this and the reasoning is not about effort: **a graph wants concrete endpoints to
 * wire, and the whole point of a selector is that a source is a query whose match set changes
 * underneath you.** A vertex here is a node, and no request names a node pair — it names a source
 * selector and a destination domain, which the server expands. Wiring two vertices would be authoring
 * something the model cannot express, so nothing here writes and nothing here stages.
 *
 * ## Layout is a correctness property, exactly as the grid's geometry is
 *
 * This screen polls every three seconds. A layout that depended on iteration order, insertion order
 * or anything non-deterministic would rearrange the fleet under the operator on every tick, which
 * is the one thing a board must never do — the same rule `model/matrix.ts` states for the grid,
 * arriving here in a form where it is much easier to get wrong. So: layers from longest-path
 * assignment over sorted input, within-layer order from barycentre sweeps with the **name** as
 * tie-break, and no randomness, no animation and no force simulation anywhere. Same fleet, same
 * picture, every poll.
 *
 * Positions stop at `(layer, order)`. Turning those into pixels is the view's job, which keeps
 * everything decided here testable without a DOM.
 */

import type { Path, PathState, RequestState } from '@/api/types'
import { namespaceOfId } from './staging'
import { aggregate } from './state'

export interface TopologyNode {
  name: string
  /** The lease. `undefined` means the node is **not registered at all**, which is a third state. */
  live: boolean | undefined
  registered: boolean
  /** Column. Origins are 0; every edge of an acyclic graph goes strictly rightwards. */
  layer: number
  /** Slot within the column, after the crossing-reduction sweeps. */
  order: number
  /** Paths arriving and leaving. A self-loop counts in both, because it is both. */
  in: number
  out: number
  /**
   * Both a destination and a source: the middle of a chain, and the whole reason this view exists.
   * A self-loop makes one too — the loopback configuration is a chain with one host in it.
   */
  relay: boolean
  /** Folded over every path touching it, in or out. Undefined when nothing does. */
  state: RequestState | undefined
  /** Nothing routed touches it. Registered and idle is a fact, not a fault — see {@link Topology}. */
  isolated: boolean
}

export interface TopologyEdge {
  key: string
  from: string
  to: string
  paths: Path[]
  state: RequestState | undefined
  /** Distinct source flows, which is not the path count: one flow to two domains is two paths. */
  flows: number
  /** Namespaces holding at least one of these paths, sorted. Fan-in across shows lives here. */
  namespaces: string[]
  /** Same node both ends — the loopback, drawn as an arc rather than as a line to nowhere. */
  loop: boolean
  /**
   * Points leftwards or sideways, which in a longest-path layering can only happen inside a cycle.
   *
   * The server refuses a `loop` (§4, reason codes), so this should be empty. It is computed and
   * counted rather than assumed away, for the reason `Matrix.unplaced` is: a view that silently drew
   * a cycle as a straight line would be lying about the one arrangement that cannot work.
   */
  back: boolean
}

export interface Topology {
  nodes: TopologyNode[]
  /** Column-major, each already in draw order. Isolated nodes are **not** in here. */
  layers: TopologyNode[][]
  edges: TopologyEdge[]
  /**
   * Registered nodes no path touches.
   *
   * Kept out of the layered graph and listed separately: put in layer 0 they would read as
   * *origins*, and "this archive is a source of something" is a different and wrong sentence from
   * "nothing is routed through this archive".
   */
  isolated: TopologyNode[]
  /** Nodes with an edge in and an edge out. Non-zero means there is a chain to look at. */
  relays: number
  /** Nodes that appear as a path endpoint and are not in `/v1/nodes`. Should be none. */
  unregistered: string[]
  /** Nodes the layering could not order because they sit in a cycle. Should be none. */
  cyclic: string[]
  pathCount: number
}

/** `from → to`. Node names cannot contain NUL, so the key is injective with nothing quoted. */
function edgeKey(from: string, to: string): string {
  return `${from}\u0000${to}`
}

interface EdgeBuild {
  from: string
  to: string
  paths: Path[]
  flows: Set<string>
  namespaces: Set<string>
}

/**
 * Build the fleet's topology.
 *
 * `nodes` supplies liveness and the isolated vertices — a fleet's registered set is not derivable
 * from its paths, and a node with nothing routed through it is exactly the kind of thing an operator
 * looks at this screen to notice. Paths supply everything else.
 */
export function buildTopology(
  paths: Path[],
  nodes: { name: string; live: boolean }[],
): Topology {
  const liveByNode = new Map(nodes.map((node) => [node.name, node.live]))

  // Vertices are the union: a path endpoint that is not a registered node should not exist, and if
  // one does the graph must draw it rather than dropping the edge that names it.
  const names = new Set<string>(nodes.map((node) => node.name))
  for (const path of paths) {
    names.add(path.source.node)
    names.add(path.destination.node)
  }

  const edgesByKey = new Map<string, EdgeBuild>()
  const inCount = new Map<string, number>()
  const outCount = new Map<string, number>()
  const touching = new Map<string, PathState[]>()

  const bump = (map: Map<string, number>, key: string) => map.set(key, (map.get(key) ?? 0) + 1)
  const touch = (name: string, state: PathState) => {
    const list = touching.get(name)
    if (list) list.push(state)
    else touching.set(name, [state])
  }

  for (const path of paths) {
    const from = path.source.node
    const to = path.destination.node
    const key = edgeKey(from, to)

    let edge = edgesByKey.get(key)
    if (!edge) {
      edge = { from, to, paths: [], flows: new Set(), namespaces: new Set() }
      edgesByKey.set(key, edge)
    }
    edge.paths.push(path)
    edge.flows.add(path.source.flow)
    for (const id of path.requests) edge.namespaces.add(namespaceOfId(id))

    bump(outCount, from)
    bump(inCount, to)
    touch(from, path.state)
    // A loopback touches one node once as far as its own state fold is concerned; counting it twice
    // would weight a self-edge higher than any other in the node's aggregate for no reason.
    if (to !== from) touch(to, path.state)
  }

  const sorted = [...names].sort((a, b) => a.localeCompare(b))
  const successors = new Map<string, string[]>()
  for (const name of sorted) successors.set(name, [])
  for (const edge of edgesByKey.values()) {
    if (edge.from === edge.to) continue
    successors.get(edge.from)!.push(edge.to)
  }
  for (const list of successors.values()) list.sort((a, b) => a.localeCompare(b))

  const isolatedNames = sorted.filter(
    (name) => (inCount.get(name) ?? 0) === 0 && (outCount.get(name) ?? 0) === 0,
  )
  const placed = sorted.filter((name) => !isolatedNames.includes(name))

  const { layer, cyclic } = assignLayers(placed, successors)
  const layerLists = orderWithinLayers(placed, layer, successors)

  const make = (name: string, index: number, layerIndex: number): TopologyNode => ({
    name,
    live: liveByNode.get(name),
    registered: liveByNode.has(name),
    layer: layerIndex,
    order: index,
    in: inCount.get(name) ?? 0,
    out: outCount.get(name) ?? 0,
    relay: (inCount.get(name) ?? 0) > 0 && (outCount.get(name) ?? 0) > 0,
    state: aggregate(touching.get(name) ?? []),
    isolated: (inCount.get(name) ?? 0) === 0 && (outCount.get(name) ?? 0) === 0,
  })

  const layers = layerLists.map((names_, index) =>
    names_.map((name, order) => make(name, order, index)),
  )
  const byName = new Map(layers.flat().map((node) => [node.name, node]))

  const isolated = isolatedNames.map((name, index) => make(name, index, 0))

  const edges: TopologyEdge[] = [...edgesByKey.values()]
    .map((build) => {
      const from = byName.get(build.from)
      const to = byName.get(build.to)
      return {
        key: edgeKey(build.from, build.to),
        from: build.from,
        to: build.to,
        paths: build.paths,
        state: aggregate(build.paths.map((path) => path.state)),
        flows: build.flows.size,
        namespaces: [...build.namespaces].sort(),
        loop: build.from === build.to,
        back:
          build.from !== build.to &&
          from !== undefined &&
          to !== undefined &&
          to.layer <= from.layer,
      }
    })
    .sort((a, b) => a.from.localeCompare(b.from) || a.to.localeCompare(b.to))

  return {
    nodes: [...layers.flat(), ...isolated],
    layers,
    edges,
    isolated,
    relays: layers.flat().filter((node) => node.relay).length,
    unregistered: sorted.filter((name) => !liveByNode.has(name)),
    cyclic,
    pathCount: paths.length,
  }
}

/**
 * Longest-path layering: a node sits one column right of its furthest-right predecessor.
 *
 * Kahn's algorithm over a name-sorted queue, so the traversal order is fixed by the input rather
 * than by a `Map`'s insertion history. Self-loops are excluded from the in-degree — a node whose only
 * incoming edge is its own would otherwise never reach zero and would be reported as a cycle, which
 * is exactly wrong for the loopback configuration the design supports.
 *
 * Anything left when the queue drains is in a genuine cycle. The server refuses those, so this is a
 * defensive branch rather than a feature: they are given a column after their resolved predecessors
 * and **named**, because a graph that quietly straightened a cycle would be the one drawing that
 * cannot be checked against the fleet.
 */
function assignLayers(
  names: string[],
  successors: Map<string, string[]>,
): { layer: Map<string, number>; cyclic: string[] } {
  const indegree = new Map<string, number>(names.map((name) => [name, 0]))
  for (const name of names) {
    for (const next of successors.get(name) ?? []) {
      if (indegree.has(next)) indegree.set(next, indegree.get(next)! + 1)
    }
  }

  const layer = new Map<string, number>(names.map((name) => [name, 0]))
  const queue = names.filter((name) => indegree.get(name) === 0)
  const settled = new Set<string>()

  while (queue.length > 0) {
    const name = queue.shift()!
    settled.add(name)
    for (const next of successors.get(name) ?? []) {
      if (!indegree.has(next)) continue
      layer.set(next, Math.max(layer.get(next) ?? 0, layer.get(name)! + 1))
      const remaining = indegree.get(next)! - 1
      indegree.set(next, remaining)
      // Kept sorted on insertion rather than sorted at the end: the queue is what decides traversal
      // order, and a queue in discovery order makes the layout depend on iteration.
      if (remaining === 0) {
        queue.push(next)
        queue.sort((a, b) => a.localeCompare(b))
      }
    }
  }

  const cyclic = names.filter((name) => !settled.has(name)).sort((a, b) => a.localeCompare(b))
  for (const name of cyclic) {
    let deepest = -1
    for (const [other, list] of successors) {
      if (settled.has(other) && list.includes(name)) deepest = Math.max(deepest, layer.get(other)!)
    }
    layer.set(name, deepest + 1)
  }

  return { layer, cyclic }
}

/**
 * Order each column so the edges between them cross as little as possible.
 *
 * Two barycentre sweeps — forward, placing a node at the mean slot of its predecessors, then
 * backward at the mean slot of its successors. It is the standard first move and it is cheap; what
 * matters more than the crossing count is that **the tie-break is the node's name**, so two nodes
 * with the same barycentre never swap between polls. An ordering that depended on anything else
 * would shuffle the fleet on the screen every three seconds.
 */
function orderWithinLayers(
  names: string[],
  layer: Map<string, number>,
  successors: Map<string, string[]>,
): string[][] {
  const depth = Math.max(-1, ...names.map((name) => layer.get(name) ?? 0)) + 1
  const layers: string[][] = Array.from({ length: depth }, () => [])
  for (const name of names) layers[layer.get(name) ?? 0]!.push(name)
  for (const list of layers) list.sort((a, b) => a.localeCompare(b))

  const predecessors = new Map<string, string[]>(names.map((name) => [name, []]))
  for (const [name, list] of successors) {
    for (const next of list) predecessors.get(next)?.push(name)
  }

  const slots = () => {
    const index = new Map<string, number>()
    for (const list of layers) list.forEach((name, i) => index.set(name, i))
    return index
  }

  const sweep = (indices: number[], neighbours: Map<string, string[]>) => {
    for (const i of indices) {
      const index = slots()
      const list = layers[i]!
      const key = new Map<string, number>()
      for (const name of list) {
        const near = (neighbours.get(name) ?? [])
          .map((other) => index.get(other))
          .filter((value): value is number => value !== undefined)
        // A node with no neighbour in the adjacent column keeps its current slot rather than being
        // pulled to zero, which would drag every unconnected vertex to the top of its column.
        key.set(name, near.length === 0 ? (index.get(name) ?? 0) : near.reduce((a, b) => a + b, 0) / near.length)
      }
      list.sort((a, b) => key.get(a)! - key.get(b)! || a.localeCompare(b))
    }
  }

  sweep([...layers.keys()].slice(1), predecessors)
  sweep([...layers.keys()].slice(0, -1).reverse(), successors)

  return layers
}
