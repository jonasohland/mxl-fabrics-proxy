/**
 * The routing matrix (`ui.md` §7a): rows are sources, columns are destinations, a request is a
 * rectangle over them.
 *
 * **The axes are virtual; only the cells are real.** A row is not a domain and a column is not a
 * directory. A row is a *source* — a node, a domain **selector** and a flow **selector** — and
 * `{labels: {role: cameras}}` matches domains that do not exist yet. A column is a *destination*, and
 * a domain a request materialises does not exist until a request names it. Neither axis is a handle
 * on an object, so neither can be derived from inventory: **both are read out of the requests**, and
 * the server materialises them into paths. The cells are the only place real objects appear.
 *
 * Three things follow, and each is a rule the old "a row is a request" framing had to argue for
 * separately:
 *
 * - **A cell with no paths is not an error.** It is an axis that has not materialised, which is the
 *   ordinary state of a pre-provisioned route.
 * - **The count in a cell is load-bearing.** It is the only place an operator learns how much a query
 *   turned into — a row saying `2 fl` and a cell saying `2 paths` are the same fact from two sides.
 * - **A request is sources × destinations**, so its cells cannot be toggled independently. That is an
 *   editing consequence, and it is why the rectangle is carried here as a set of row and column
 *   indices rather than as anything a cell could own on its own.
 *
 * **Parked entries stay on the axes.** Columns come from `destinations[]` and not from paths, so a
 * destination switched off keeps its column: without that the axes would be derived from routes that
 * are currently on, and switching one off would delete the row and the column it lived on. The board
 * would rearrange itself under the pointer, which is the one thing a board must never do (§7a, "Off
 * is a value, not an absence").
 */

import type { Destination, Node, Path, Request, RequestState, Source } from '@/api/types'
import { renderDomain } from '@/api/types'
import { domainSelectorLabel, selectorLabel } from './labels'
import { namespaceOf, namespacesByDestination } from './ledger'
import { endpointKey, ownedPaths } from './ownership'
import { DISPLAY_ORDER, aggregate, displayRank } from './state'

/** One source, as the operator wrote it. Shared by every request that names the same three things. */
export interface MatrixRow {
  key: string
  index: number
  node: string
  /** A representative entry. Every request on this row wrote an identical one, by construction. */
  source: Source
  domain: string
  selector: string
  /** Requests naming this source, sorted. More than one is a row that cannot be extended by a click. */
  requests: string[]
  /** Distinct source flows this row's selectors actually matched — what the query turned into. */
  flows: number
  paths: number
}

/** One destination. `(node, rendered domain)` is its identity; the area is the first element of it. */
export interface MatrixColumn {
  key: string
  index: number
  node: string
  domain: string
  /**
   * A representative entry, kept because the **structured** domain is the form that may be sent and
   * a rendered one must never be split back apart to send it (`ui.md` §5 trap 1).
   */
  destination: Destination
  requests: string[]
  /**
   * No request names this destination: it is on the axis because the operator just named it.
   *
   * The column set is "every destination some request names", so a new one has nowhere to come from
   * until a request names it — and it has to be on the axis first, because naming it *is* how the
   * operator gets a cell to click. It stops being a draft the moment a leg is staged onto it, and
   * nothing about the column changes when that happens: the key is the same, so it does not move.
   */
  draft: boolean
  /**
   * Every entry naming this destination is parked, so the whole column is off.
   *
   * Per *request* rather than per column is where the flag lives, so a domain two requests write into
   * is parked here only when both of them parked it — which is also the condition under which a
   * column-level removal would take nothing live away.
   */
  parked: boolean
  /** How many of the naming requests parked it. Below `requests.length`, the column is still live. */
  parkedRequests: number
  /**
   * Other namespaces landing anything at all in this domain — the `+ archive` fact.
   *
   * Namespaces partition requests, **not destinations**, so two shows deliberately fanning into one
   * archive domain is the arrangement fan-in exists for. It is the fact that changes what emptying a
   * column would mean, and a screen showing one namespace cannot see it any other way. Computed over
   * the whole path list rather than over this namespace's, which is the whole point of it.
   */
  foreignNamespaces: string[]
  /** Rows with a lit cell here. A column is **additive**, and this is the number that says so. */
  sources: number
  paths: number
  /**
   * What the **server** is carrying here, which is not what the grid is drawing.
   *
   * {@link paths} counts the drawn set, so a leg staged for parking drops out of it the moment it is
   * clicked — which is right for the board and wrong for a `×`. Staged dark is not dark: nothing has
   * been written, the media is still moving, and a control that named its cost from the drawn count
   * would say "nothing stops" over paths it is about to stop. Read this for anything that states a
   * cost and {@link paths} for anything that describes the board.
   */
  carrying: number
}

export interface MatrixCell {
  row: number
  column: number
  /** Some request in this namespace names both this source and this destination. */
  lit: boolean
  /** Lit, and every request lighting it has parked this destination. Drawn dark, never blank. */
  parked: boolean
  /**
   * Folded from **this cell's own paths**, never from a request's aggregate — a request's state is a
   * statement about all of its legs and would report another cell's trouble here.
   *
   * Undefined on an unlit cell, and on a lit one whose pairing has materialised nothing that can be
   * attributed to it. Where the whole *source* has matched nothing, the source's own state stands in:
   * that is the "accepted, not yet satisfiable" outcome of §7a, which is a quiet cell state and not
   * an error.
   */
  state: RequestState | undefined
  count: number
  paths: Path[]
  /**
   * What the **server** is carrying on this pairing, which is not what the cell is drawing.
   *
   * {@link count} is the drawn count and is 0 for a leg staged for parking, because the grid stops
   * drawing it the moment it is clicked. That is right for the board and wrong for the cost of a
   * `×`: staged dark is not dark, nothing has been written, and those paths are still up. The two
   * numbers are equal on a screen with nothing staged and differ exactly by what the operator has
   * authored and not yet applied.
   */
  carrying: number
  /**
   * Requests whose rectangle covers this pairing.
   *
   * More than one is the bounded hole §7a names: exclusivity is enforced on *materialised* paths, so
   * two requests with an identical source and destination are both accepted while the selector
   * matches nothing, and it resolves itself into one `INVALID` the moment a producer appears. It
   * cannot persist, and it is the one place sharing markup earns its keep.
   */
  requests: string[]
  /** The accent of the covering rectangle, when it has more than one source. */
  accent: number | undefined
}

/**
 * A node band.
 *
 * **Both axes are grouped by node, because each grouping carries a real resource fact.** One source
 * to five destinations is 5× egress on that source node; twelve sources into one domain is 12×
 * ingress on that destination node, which is the binding direction for an ingest wall since an edge
 * is bounded by what it can take. A grid that makes only one of them legible renders half its
 * requests badly.
 */
export interface Band<T> {
  node: string
  /** The lease. `undefined` means the node is not registered at all, which is a different fact. */
  live: boolean | undefined
  items: T[]
  /** Paths crossing this node in this namespace: egress down the rows, ingress across the columns. */
  paths: number
}

/**
 * One request, as the block it occupies.
 *
 * Its rows need not be adjacent — rows sort by source node — so the rectangle is carried as index
 * sets and drawn with an accent rather than with a border whose geometry would depend on the sort
 * order.
 */
export interface MatrixRequest {
  id: string
  name: string
  request: Request
  rows: number[]
  columns: number[]
  /**
   * Only a request with more than one source gets one: a 1×N rectangle is a row and needs no help
   * being seen as one.
   */
  accent: number | undefined
  paths: number
}

export interface Matrix {
  rows: MatrixRow[]
  columns: MatrixColumn[]
  rowBands: Band<MatrixRow>[]
  columnBands: Band<MatrixColumn>[]
  /** Row-major: `cells[rowIndex][columnIndex]`. */
  cells: MatrixCell[][]
  requests: MatrixRequest[]
  /**
   * Paths this namespace's requests hold **through a leg the board draws**. The cells sum to this.
   *
   * On the server's own request list that is simply every path they hold. It differs only while
   * something is staged: a leg parked or removed but not applied is still carrying its paths, and
   * they are deliberately drawn nowhere — so they are not counted here either, and the pending bar
   * is what says how many of them stop.
   */
  pathCount: number
  notActiveCount: number
  /**
   * Held paths that landed in no cell.
   *
   * There is no arrangement that should produce one, so it is counted rather than assumed away: a
   * grid that silently drops an edge is exactly the failure this whole model is built to avoid, and a
   * number on the screen is cheaper than believing the join.
   */
  unplaced: number
}

/** How many distinct accents there are. Matches `--acc-0…5` in `styles/base.css`. */
export const ACCENTS = 6

/**
 * A source's identity, over the three things that make one: the node, the domain selector and the
 * flow selector.
 *
 * Two requests naming the same three things are the **same row** — that is what makes a row a query
 * rather than a request's property, and it is what lets a fan-in rectangle and a single-source
 * request share a row honestly. JSON over sorted label pairs rather than a hand-rolled spelling,
 * because a label value is an arbitrary string and any separator could appear inside one.
 */
export function sourceKey(source: Source): string {
  const domain = source.domain.name !== undefined
    ? `name:${renderDomain(source.domain.name)}`
    : `labels:${JSON.stringify(Object.entries(source.domain.labels ?? {}).sort())}`

  const select = source.select.flow !== undefined
    ? `flow:${source.select.flow}`
    : source.select.group_hint !== undefined
      ? `hint:${JSON.stringify([source.select.group_hint.name, source.select.group_hint.type ?? null])}`
      : 'all'

  return `${source.node}\u0000${domain}\u0000${select}`
}

interface RowBuild {
  key: string
  node: string
  source: Source
  requests: string[]
  flows: Set<string>
  paths: Set<string>
}

interface ColumnBuild {
  key: string
  node: string
  domain: string
  destination: Destination
  requests: string[]
  parkedRequests: number
  paths: Set<string>
  sources: Set<number>
  /** The server's own, for the controls that state a cost — see {@link MatrixColumn.carrying}. */
  carrying: Set<string>
}

/** The worst of a set of states by display order, which is worst-first. */
function worstDisplayed(states: readonly RequestState[]): RequestState | undefined {
  let best: RequestState | undefined
  let bestRank = DISPLAY_ORDER.length + 1
  for (const state of states) {
    const rank = displayRank(state)
    if (rank < bestRank) {
      best = state
      bestRank = rank
    }
  }
  return best
}

/**
 * Build the matrix for one namespace.
 *
 * `paths` is the **whole** fleet's path list, not this namespace's: ownership comes from
 * `path.requests[]`, and the cross-namespace fact on a column header is a question about every other
 * namespace, which a filtered list cannot answer. `nodes` supplies the bands' liveness and nothing
 * else — a node registered but not leased is information, not an alarm, and the grid must not blank
 * itself over one.
 *
 * `requests` may be a **staged** list — the specs as the operator has authored them, which is what
 * the grid draws — and `stored` is then the server's own. Only one thing needs the difference, and
 * it is {@link Matrix.unplaced}: the alarm asks whether the *server* handed us a path we cannot
 * place, so a leg the operator has parked or removed but not yet applied must not trip it. Left
 * defaulted the two are the same list and the question collapses, which is what every read-only
 * caller wants.
 *
 * `extra` are destinations the operator has named and nothing routes yet. They are the one axis
 * input that does not come from a request, and they have to be: a column is a domain that does not
 * exist until a request names it, so without somewhere to put a newly named one there is no cell to
 * click and no way to name the first destination of anything. One that some request *does* name is
 * ignored here rather than duplicated — the request is the authority on its own column.
 */
export function buildMatrix(
  paths: Path[],
  requests: Request[],
  nodes: Node[],
  namespace: string,
  stored: Request[] = requests,
  extra: Destination[] = [],
): Matrix {
  const inNamespace = requests
    .filter((request) => namespaceOf(request) === namespace)
    .sort((a, b) => a.name.localeCompare(b.name))

  const byId = new Map(inNamespace.map((request) => [request.id, request]))
  const storedById = new Map(stored.map((request) => [request.id, request]))
  const pathsById = new Map(paths.map((path) => [path.id, path]))
  const liveByNode = new Map(nodes.map((node) => [node.name, node.live]))
  const writers = namespacesByDestination(paths)

  // -- the axes, read out of the requests and not out of the fleet ----------

  const rowsByKey = new Map<string, RowBuild>()
  const columnsByKey = new Map<string, ColumnBuild>()

  for (const request of inNamespace) {
    for (const source of request.sources) {
      const key = sourceKey(source)
      let row = rowsByKey.get(key)
      if (!row) {
        row = { key, node: source.node, source, requests: [], flows: new Set(), paths: new Set() }
        rowsByKey.set(key, row)
      }
      if (!row.requests.includes(request.id)) row.requests.push(request.id)
    }

    for (const destination of request.destinations) {
      const key = endpointKey(destination)
      let column = columnsByKey.get(key)
      if (!column) {
        column = {
          key,
          node: destination.node,
          domain: renderDomain(destination.domain),
          destination,
          requests: [],
          parkedRequests: 0,
          paths: new Set(),
          sources: new Set(),
          carrying: new Set(),
        }
        columnsByKey.set(key, column)
      }
      if (!column.requests.includes(request.id)) {
        column.requests.push(request.id)
        // Per request, so a destination two requests write into is only off when both parked it.
        // A request naming one destination twice is not constructible in the manifest and would be
        // counted once here either way.
        if (destination.disabled === true) column.parkedRequests++
      }
    }
  }

  for (const destination of extra) {
    const key = endpointKey(destination)
    if (columnsByKey.has(key)) continue
    columnsByKey.set(key, {
      key,
      node: destination.node,
      domain: renderDomain(destination.domain),
      destination,
      requests: [],
      parkedRequests: 0,
      paths: new Set(),
      sources: new Set(),
      carrying: new Set(),
    })
  }

  const rowBuilds = [...rowsByKey.values()].sort(
    (a, b) =>
      a.node.localeCompare(b.node) ||
      domainSelectorLabel(a.source.domain).localeCompare(domainSelectorLabel(b.source.domain)) ||
      selectorLabel(a.source.select).localeCompare(selectorLabel(b.source.select)),
  )
  const columnBuilds = [...columnsByKey.values()].sort(
    (a, b) => a.node.localeCompare(b.node) || a.domain.localeCompare(b.domain),
  )

  const rowIndex = new Map(rowBuilds.map((row, index) => [row.key, index]))
  const columnIndex = new Map(columnBuilds.map((column, index) => [column.key, index]))

  // -- the cells, which are the only real objects on the screen -------------

  const cells: MatrixCell[][] = rowBuilds.map((_, row) =>
    columnBuilds.map((__, column) => ({
      row,
      column,
      lit: false,
      parked: false,
      state: undefined,
      count: 0,
      paths: [],
      carrying: 0,
      requests: [],
      accent: undefined,
    })),
  )

  // Filled while the cells are, and read once each cell knows whether it materialised anything: a
  // pairing whose whole *source* has matched nothing is the "accepted, not yet satisfiable" outcome
  // and the source's own state is the honest word for it.
  const pending: RequestState[][][] = rowBuilds.map(() => columnBuilds.map(() => []))

  /**
   * What the **server** holds per cell, accumulated beside the drawn set rather than derived from it.
   *
   * It has to be gathered before the parked check below, because that is exactly where the two part
   * company: a destination the operator has staged for parking is skipped for drawing and is still
   * carrying its paths. Ids rather than a counter, because two requests can light one cell and the
   * path underneath is one path.
   */
  const carrying: Set<string>[][] = rowBuilds.map(() => columnBuilds.map(() => new Set<string>()))

  const rectangles: MatrixRequest[] = []
  let accentNext = 0
  const held = new Set<string>()
  const placed = new Set<string>()
  const unaccounted = new Set<string>()

  for (const request of inNamespace) {
    const accent = request.sources.length > 1 ? accentNext++ % ACCENTS : undefined
    const rows: number[] = []
    const columns: number[] = []
    let requestPaths = 0

    // Destinations this request draws, and the ones the *server* has it holding paths through. On a
    // read-only list the two are the same set; they differ exactly by what the operator has staged.
    const drawable = drawableKeys(request)
    const authored = drawableKeys(storedById.get(request.id) ?? request)

    for (const [index, source] of request.sources.entries()) {
      const row = rowIndex.get(sourceKey(source))!
      if (!rows.includes(row)) rows.push(row)

      const owned = ownedPaths(request, index, pathsById)
      const sourceState = request.status.sources?.[index]?.state ?? request.status.state
      for (const path of owned) {
        // The row's own two facts are about the *query* — what this source matched — so they count
        // every path it holds, whatever the board is currently drawing.
        rowBuilds[row]!.flows.add(path.source.flow)
        rowBuilds[row]!.paths.add(path.id)

        const destination = endpointKey(path.destination)
        if (drawable.has(destination)) {
          // What the **board** accounts for, and therefore what the cells sum to. A leg parked or
          // removed but not yet applied is still carrying this path and the grid deliberately does
          // not draw it, so it is not in this count either — what it will do is the pending bar's to
          // say. On the server's own list the distinction does not arise: a parked destination
          // produces no path at all.
          held.add(path.id)
          requestPaths++
        } else if (!authored.has(destination)) {
          // The server handed us a path through a destination its **own stored spec** does not name.
          // No arrangement should produce one, so it is counted rather than assumed away.
          unaccounted.add(path.id)
        }
      }

      for (const destination of request.destinations) {
        const column = columnIndex.get(endpointKey(destination))!
        if (!columns.includes(column)) columns.push(column)

        const cell = cells[row]![column]!
        cell.lit = true
        if (!cell.requests.includes(request.id)) cell.requests.push(request.id)
        cell.accent ??= accent

        // Before the skip, deliberately. A `×` names the paths it stops, and a leg the operator has
        // only staged for parking is still carrying every one of them.
        const key = endpointKey(destination)
        for (const path of owned) {
          if (endpointKey(path.destination) !== key) continue
          carrying[row]![column]!.add(path.id)
          columnBuilds[column]!.carrying.add(path.id)
        }

        if (destination.disabled === true) continue

        // Counted as a source of this column because it is authored *and* on — a column reads as
        // additive, and a parked leg is not a source into that domain right now. A fully parked
        // column therefore counts none rather than claiming sources it does not have.
        columnBuilds[column]!.sources.add(row)

        const legs = owned.filter((path) => endpointKey(path.destination) === endpointKey(destination))
        for (const path of legs) {
          if (cell.paths.some((existing) => existing.id === path.id)) continue
          cell.paths.push(path)
          placed.add(path.id)
          columnBuilds[column]!.paths.add(path.id)
        }
        if (owned.length === 0) pending[row]![column]!.push(sourceState)
      }
    }

    rectangles.push({
      id: request.id,
      name: request.name,
      request,
      rows,
      columns,
      accent,
      paths: requestPaths,
    })
  }

  for (const row of cells) {
    for (const cell of row) {
      if (!cell.lit) continue

      // Above the parked check on purpose: a cell drawn dark because the operator staged a park is
      // the one case where the two counts differ, and it is the case a `×` must not describe as
      // safe. Skipping it here would zero exactly the number that has to be right.
      cell.carrying = carrying[cell.row]![cell.column]!.size

      // Parked only when *every* request lighting the cell parked its entry: one live claim keeps
      // the leg running and a cell drawn dark over it would be a lie about media that is flowing.
      const parked = cell.requests.every((id) =>
        byId.get(id)!.destinations.some(
          (destination) =>
            endpointKey(destination) === columnBuilds[cell.column]!.key &&
            destination.disabled === true,
        ),
      )

      if (parked) {
        cell.parked = true
        cell.state = 'DISABLED'
        continue
      }

      cell.count = cell.paths.length
      if (cell.count > 0) {
        cell.state = aggregate(cell.paths.map((path) => path.state))
      } else {
        const waiting = pending[cell.row]![cell.column]!
        // Only when every request lighting the cell is waiting on its source. Otherwise one of them
        // has legs elsewhere and this pairing is silent for a reason the API does not carry — a leg
        // refused during validation produces no path and appears in no list, so the honest cell says
        // nothing rather than borrowing another leg's word for it.
        cell.state = waiting.length === cell.requests.length ? worstDisplayed(waiting) : undefined
      }
    }
  }

  // -- the axes' own facts --------------------------------------------------

  const rows: MatrixRow[] = rowBuilds.map((build, index) => ({
    key: build.key,
    index,
    node: build.node,
    source: build.source,
    domain: domainSelectorLabel(build.source.domain),
    selector: selectorLabel(build.source.select),
    requests: [...build.requests].sort(),
    flows: build.flows.size,
    paths: build.paths.size,
  }))

  const columns: MatrixColumn[] = columnBuilds.map((build, index) => ({
    key: build.key,
    index,
    node: build.node,
    domain: build.domain,
    destination: build.destination,
    requests: [...build.requests].sort(),
    draft: build.requests.length === 0,
    // Guarded on there being a request to have parked it: a column nothing names yet has parked
    // none of nothing, and drawing it dark would say "somebody routed this and switched it off"
    // about a destination nobody has routed at all.
    parked: build.requests.length > 0 && build.parkedRequests === build.requests.length,
    parkedRequests: build.parkedRequests,
    // Keyed exactly as the path list is, which is what lets one namespace see a fact about all of
    // them without a second read.
    foreignNamespaces: [...(writers.get(build.key) ?? [])]
      .filter((name) => name !== namespace)
      .sort(),
    sources: build.sources.size,
    paths: build.paths.size,
    carrying: build.carrying.size,
  }))

  const heldPaths = paths.filter((path) => held.has(path.id))

  return {
    rows,
    columns,
    rowBands: band(rows, liveByNode, (row) => row.paths),
    columnBands: band(columns, liveByNode, (column) => column.paths),
    cells,
    requests: rectangles,
    pathCount: held.size,
    notActiveCount: heldPaths.filter((path) => path.state !== 'ACTIVE').length,
    // Two ways to lose an edge, both of which should be impossible: the join dropped one it had a
    // row and a column for, or the server reported one through a destination its own spec does not
    // name. Neither is what a staged edit produces.
    unplaced: [...held].filter((id) => !placed.has(id)).length + unaccounted.size,
  }
}

/** The destinations a request draws: named, and not parked. */
function drawableKeys(request: Request): Set<string> {
  return new Set(
    request.destinations
      .filter((destination) => destination.disabled !== true)
      .map((destination) => endpointKey(destination)),
  )
}

/** Group an axis by node, preserving the axis's own order. */
function band<T extends { node: string }>(
  items: T[],
  live: Map<string, boolean>,
  paths: (item: T) => number,
): Band<T>[] {
  const bands: Band<T>[] = []
  for (const item of items) {
    let current = bands[bands.length - 1]
    if (!current || current.node !== item.node) {
      current = { node: item.node, live: live.get(item.node), items: [], paths: 0 }
      bands.push(current)
    }
    current.items.push(item)
    current.paths += paths(item)
  }
  return bands
}
