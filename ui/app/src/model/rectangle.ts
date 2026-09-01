/**
 * One request's own sources × destinations grid.
 *
 * A path-first view loses exactly one thing, and it is the thing `disabled` exists to protect: a
 * parked destination produces no path, so it has no claim to appear as, and a ledger built only
 * from `/v1/paths` renders a switched-off leg identically to one that was never written. So each
 * request keeps its own rectangle.
 *
 * **A per-request rectangle is never ambiguous.** Rectangles only overlap each *other*; alone, one
 * is exactly the request's spec. That is why the global grid is what fails in a `shared` namespace
 * and this component does not — and why it is written here once, for the ledger to use now and the
 * matrix to reuse later, rather than as a second spelling of the same thing.
 */

import type { Destination, Path, Request, RequestState, Source } from '@/api/types'
import { renderDomain } from '@/api/types'
import { aggregate } from './state'
import { domainSelectorLabel, selectorLabel } from './labels'
import { endpointKey, ownedPaths } from './ownership'

export interface RectRow {
  index: number
  source: Source
  domain: string
  selector: string
}

export interface RectColumn {
  index: number
  destination: Destination
  node: string
  domain: string
  /** The entry is in the spec and expands to nothing. Drawn dark, never blank. */
  parked: boolean
}

export interface RectCell {
  /** Undefined when this pairing has produced nothing yet — an axis that has not materialised. */
  state: RequestState | undefined
  count: number
  parked: boolean
  paths: Path[]
}

export interface Rectangle {
  rows: RectRow[]
  columns: RectColumn[]
  /** Row-major: `cells[rowIndex][columnIndex]`. */
  cells: RectCell[][]
}

/**
 * Build the rectangle.
 *
 * `pathsById` must come from `GET /v1/paths`, not from the request's own `status.paths[]` — see
 * `ledger.ts`. Ownership is re-checked per path here rather than assumed, so that this renders
 * honestly inside an `exclusive` namespace too, where a request can list a path it does not hold.
 */
export function buildRectangle(request: Request, pathsById: Map<string, Path>): Rectangle {
  const rows: RectRow[] = request.sources.map((source, index) => ({
    index,
    source,
    domain: domainSelectorLabel(source.domain),
    selector: selectorLabel(source.select),
  }))

  const columns: RectColumn[] = request.destinations.map((destination, index) => ({
    index,
    destination,
    node: destination.node,
    domain: renderDomain(destination.domain),
    parked: destination.disabled === true,
  }))

  const cells = rows.map((row) => {
    // The paths this source entry expanded onto, resolved against the authoritative list and
    // filtered to the ones this request actually holds (`model/ownership.ts`).
    const owned = ownedPaths(request, row.index, pathsById)

    return columns.map((column): RectCell => {
      if (column.parked) {
        // A parked leg keeps a lit cell's geometry exactly — two fixed-shape lines — because it *is*
        // a lit cell in every sense except that it is asking for nothing, and a cell that changed
        // shape when switched off would resize its row.
        return { state: 'DISABLED', count: 0, parked: true, paths: [] }
      }

      const endpoint = endpointKey(column.destination)
      const paths = owned.filter((path) => endpointKey(path.destination) === endpoint)
      return {
        state: aggregate(paths.map((path) => path.state)),
        count: paths.length,
        parked: false,
        paths,
      }
    })
  })

  return { rows, columns, cells }
}
