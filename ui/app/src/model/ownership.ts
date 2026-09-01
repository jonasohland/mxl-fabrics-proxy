/**
 * Who holds a path, and which pairing it belongs to.
 *
 * Both grids in this app — one request's rectangle and the whole namespace's matrix — ask the same
 * two questions of a path: *did this request actually get it*, and *which cell is it in*. The answers
 * live here once rather than in each renderer, because the first of them is `ui.md` §5 trap 14 and
 * getting it wrong is silent:
 *
 * > A request can report a path it does not hold. The loser of a namespace overlap goes `INVALID` /
 * > `namespace_overlap` and **still lists the contested path with the incumbent's state**, so a
 * > request carrying nothing shows `{"ACTIVE": 1}` in its own counts.
 *
 * `/v1/paths` → `path.requests[]` is the only statement of ownership in the API. A cell drawn from a
 * request's own `status.paths[]` without the cross-check shows another request's media as this
 * one's — in the one situation where the operator most needs to know they are not carrying it.
 */

import type { Destination, Domain, Path, Request } from '@/api/types'
import { renderDomain } from '@/api/types'

/**
 * `(node, rendered domain)`, separated by NUL — written as an **escape**, never as a literal byte,
 * or the source stops being text to every tool that reads it. It is the one byte neither a node name
 * nor a domain can contain, so the key is injective with nothing quoted.
 *
 * The *rendered* domain is right here even though a structured one is what may be sent: two
 * destinations are the same column exactly when they name the same place, and `fast/ingest` under
 * one area is a different string from `bulk/ingest` under another, so the rendering is already
 * injective over what the wire accepts.
 */
export function destinationKey(node: string, domain: Domain): string {
  return `${node}\u0000${renderDomain(domain)}`
}

export function endpointKey(destination: Destination): string {
  return destinationKey(destination.node, destination.domain)
}

/**
 * `(node, domain, flow)` — a flow's address, which is the one thing a flow ID on its own is not.
 *
 * The same UUID legitimately exists on two nodes after replication, and may exist twice on one host
 * in two domains, so the domain component is mandatory and **nothing may be keyed on the ID alone**
 * (`ui.md` §5 trap 4). Same NUL separator as {@link destinationKey}, and same escape rule: written
 * as `\u0000`, never as the literal byte, or the source stops being text to every tool that reads it.
 *
 * Takes the three parts rather than either wire struct, because the two spell the ID differently —
 * `path.source` is a `FlowAddress` carrying `flow`, and an entry from `/v1/flows` is a `FlowEntry`
 * carrying `id`. Naming the parts at each call site makes that mismatch a compile error rather than
 * an `undefined` inside a key that then matches nothing, silently.
 */
export function flowAddressKey(node: string, domain: string, flow: string): string {
  return `${node}\u0000${domain}\u0000${flow}`
}

/**
 * The paths one source entry of one request expanded onto, **filtered to the ones it holds**.
 *
 * `status.sources[i].paths` carries the IDs, which is the only honest way to attribute a path to a
 * source: a label selector expands over several domains, so a path's own source address does not say
 * which entry asked for it.
 *
 * The fallback covers a single-source request arriving without the breakdown. `ui.md` §7c says it is
 * `omitempty` and absent in that case; against a server built from this tree it is always present
 * and always the full list (verified 2026-09-01, and the Go doc comment on `RequestStatus.Sources`
 * agrees). It costs one line and means an older server still attributes rather than drawing a dark
 * row over live media.
 */
export function ownedPaths(
  request: Request,
  sourceIndex: number,
  pathsById: Map<string, Path>,
): Path[] {
  const breakdown = request.status.sources ?? []
  const ids = breakdown[sourceIndex]?.paths
    ?? (breakdown.length === 0 && request.sources.length === 1
      ? request.status.paths.map((path) => path.id)
      : [])

  return ids
    .map((id) => pathsById.get(id))
    .filter((path): path is Path => path !== undefined && path.requests.includes(request.id))
}
