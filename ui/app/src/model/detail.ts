/**
 * The joins and the formatters the six detail views share (`ui.md` §7, `describe`).
 *
 * Everything here is pure and everything here is *read*. A detail view is the one screen in this app
 * that asks "everything known about one thing" rather than "what is routed where", so what it needs
 * is not a model of intent but a handful of joins over the fleet the poll already has — plus the
 * half-dozen renderings the CLI worked out and that a second spelling of would only make different.
 *
 * Where a function mirrors `cmd/mxl-replicator/describe.go` it says so, because a divergence between
 * the two would be silent and an operator moving between them reads the same fleet twice.
 */

import type {
  Area,
  CapFlag,
  Destination,
  DomainName,
  FlowEntry,
  GroupHint,
  Path,
  ProviderPin,
  SessionEndpoint,
  Uint64,
} from '@/api/types'
import { UINT64_MAX, asBigInt, renderDomain } from '@/api/types'

// ---------------------------------------------------------------------------
// Renderings
// ---------------------------------------------------------------------------

/**
 * `max_message_size`, which is a genuine `uint64` — providers do report `UINT64_MAX`, and printing
 * that as a number is less use than saying so (`describe.go`'s `byteSize`).
 *
 * Read through `asBigInt` rather than as a number: the client quotes integers too large for a double
 * before parsing, so the value arrives as a string precisely so that it is not already rounded by
 * the time anything looks at it (`ui.md` §5 trap 2).
 */
export function byteSize(value: Uint64 | undefined): string {
  const bytes = asBigInt(value)
  if (bytes === undefined) return '·'
  if (bytes === UINT64_MAX) return 'unlimited'
  if (bytes >= 1n << 20n) return `${(Number(bytes) / (1 << 20)).toFixed(1)} MiB`
  if (bytes >= 1n << 10n) return `${(Number(bytes) / (1 << 10)).toFixed(1)} KiB`
  return `${bytes} B`
}

/**
 * A coarse age. Seconds matter for a worker that keeps restarting; nothing above an hour needs more
 * than whole hours (`describe.go`'s `since`).
 *
 * `now` is a parameter so this is testable without a clock, and every timestamp on the wire is
 * `omitzero` — absent, not null, not epoch — so an undefined input is the ordinary case rather than
 * a caller's mistake.
 */
export function since(at: string | undefined, now: number = Date.now()): string | undefined {
  if (!at) return undefined
  const then = Date.parse(at)
  if (Number.isNaN(then)) return undefined

  const seconds = Math.max(0, Math.floor((now - then) / 1000))
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

/**
 * A provider pin, in preference order.
 *
 * `>` rather than a comma because the order is the whole of what the list says, and a pin is
 * honoured or the request fails — never substituted. Nothing here should read as a fallback.
 */
export function providerText(pin: ProviderPin | undefined): string {
  if (pin === undefined) return ''
  return (Array.isArray(pin) ? pin : [pin]).join(' > ')
}

/**
 * An area's two grants, spelled as the flag spells them so the output and the input read alike.
 *
 * Neither implies the other and both default false, so "no grants" is a real answer: an area
 * advertised and granted nothing is visible to discovery and usable for nothing.
 */
export function grantText(area: Area): string {
  if (area.read && area.write) return 'read+write'
  if (area.write) return 'write'
  if (area.read) return 'read'
  return 'no grants'
}

export function capFlagsText(flags: CapFlag[] | undefined): string {
  return flags?.length ? flags.join(', ') : 'no capabilities'
}

export function groupHintText(hint: GroupHint | undefined): string {
  if (!hint) return ''
  return hint.type ? `${hint.name} (${hint.type})` : hint.name
}

/** `<node> <area>/<elements>`, the way a destination is headed everywhere else in this app. */
export function endpointText(destination: Destination): string {
  return `${destination.node} ${renderDomain(destination.domain)}`
}

/** `host:port`, or nothing. An endpoint in `ESTABLISHING` legitimately has neither. */
export function addressText(endpoint: SessionEndpoint | undefined): string {
  if (!endpoint?.address) return '·'
  return endpoint.service ? `${endpoint.address}:${endpoint.service}` : endpoint.address
}

/**
 * The handful of NMOS fields worth a line of summary, out of the verbatim `flow_def`
 * (`describe.go`'s `describeDefinition`).
 *
 * **Decoded for display only.** The definition is arbitrary content including fields nothing in this
 * tree models, the destination flow must reproduce it exactly, and the session identity hashes those
 * bytes — so a re-serialisation that reordered keys would read as a different flow and rebuild a
 * healthy session (`ui.md` §5 trap 10). Nothing here goes back over the wire.
 */
export function describeDefinition(def: unknown): string {
  if (typeof def !== 'object' || def === null) return ''
  const fields = def as Record<string, unknown>

  const parts: string[] = []

  const label = fields['label']
  if (typeof label === 'string' && label !== '') parts.push(`"${label}"`)

  const mediaType = fields['media_type']
  const format = fields['format']
  if (typeof mediaType === 'string' && mediaType !== '') parts.push(mediaType)
  else if (typeof format === 'string' && format !== '') {
    parts.push(format.replace(/^urn:x-nmos:format:/, ''))
  }

  const width = Number(fields['frame_width'])
  const height = Number(fields['frame_height'])
  if (width > 0 && height > 0) parts.push(`${width}x${height}`)

  const rate = fields['grain_rate']
  if (typeof rate === 'object' && rate !== null) {
    const { numerator, denominator } = rate as Record<string, unknown>
    const n = Number(numerator)
    const d = Number(denominator) || 1
    if (n > 0) parts.push(`${(n / d).toFixed(3)} Hz`)
  }

  return parts.join(', ')
}

// ---------------------------------------------------------------------------
// Joins over the path list
// ---------------------------------------------------------------------------

/** Which end of a path a node is. A node can legitimately be both, on one path. */
export type Role = 'initiator' | 'target'

export interface PathRole {
  role: Role
  path: Path
  /** The other end, rendered — what the operator wants beside the role. */
  peer: string
}

/**
 * Every path touching a node, with this node's role in each.
 *
 * **Both ends are checked independently, and that is the whole point of this function.** A node can
 * be *both* ends of a path — same node, different domain — which is what the loopback configuration
 * does and what `edge-01` does in the `ui.md` §9 fixture. A `switch` that matched the source first
 * would hide half of what that node is running, and it is a live-run bug rather than one a unit test
 * would have found (`ui.md` §7).
 */
export function pathsTouching(node: string, paths: readonly Path[]): PathRole[] {
  const rows: PathRole[] = []
  for (const path of paths) {
    if (path.source.node === node) {
      rows.push({ role: 'initiator', path, peer: endpointText(path.destination) })
    }
    if (path.destination.node === node) {
      rows.push({ role: 'target', path, peer: `${path.source.node} ${path.source.domain}` })
    }
  }
  return rows
}

/**
 * Every path replicating a flow.
 *
 * Matched on the source address only: after replication the same UUID exists at the destination too,
 * and a path whose *destination* carries the ID is the one writing it rather than one carrying it.
 */
export function pathsCarrying(flow: string, paths: readonly Path[]): Path[] {
  return paths.filter((path) => path.source.flow === flow)
}

/**
 * Every path touching one `(node, domain)`, with the domain's role in each.
 *
 * The reason this exists beside {@link pathsTouching} is `ui.md` §3's one-kind rule: a domain
 * replication writes into is discovered, observed and listed like any other, so "what lands here"
 * and "what leaves here" are two answers about one place rather than two kinds of object. A domain
 * view that showed only outgoing paths would render a destination domain as if nothing touched it.
 */
export function pathsAtDomain(
  node: string,
  domain: DomainName,
  paths: readonly Path[],
): PathRole[] {
  const rows: PathRole[] = []
  for (const path of paths) {
    if (path.source.node === node && path.source.domain === domain) {
      rows.push({ role: 'initiator', path, peer: endpointText(path.destination) })
    }
    if (path.destination.node === node && renderDomain(path.destination.domain) === domain) {
      rows.push({ role: 'target', path, peer: `${path.source.node} ${path.source.domain}` })
    }
  }
  return rows
}

/** Every location a flow ID exists. The multiplicity *is* the answer — after replication it is two. */
export function locationsOf(flow: string, entries: readonly FlowEntry[]): FlowEntry[] {
  return entries.filter((entry) => entry.id === flow)
}

/** The first group hint any location reports. A flow's hint is a property of the media, not a place. */
export function firstGroupHint(entries: readonly FlowEntry[]): GroupHint | undefined {
  return entries.find((entry) => entry.group_hint !== undefined)?.group_hint
}
