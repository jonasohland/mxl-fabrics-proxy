/**
 * The ledger's joins (`ui.md` §7c).
 *
 * The matrix renders **intents**, deduplicated by nothing. This renders **claims** — the triple
 * `(request, source entry, path)` — over the path list the server has already deduplicated, which
 * is the same deduplication the fleet is doing. That inversion is what makes the view honest where
 * a grid is not: one path is one row so nothing is double-counted, the counts sum because the rows
 * are the real edges, and "un-lighting stops nothing" becomes visible as a refcount that stays
 * above zero.
 *
 * **Ownership comes from `path.requests[]` and from nothing else.** A request's own `status.paths[]`
 * is not an ownership statement — the loser of a namespace overlap lists the contested path with
 * the incumbent's state, so a request carrying nothing can report `{"ACTIVE": 1}` (`ui.md` §5 trap
 * 14). That trap does not fire in a `shared` namespace, where no overlap rule runs, but this view is
 * meant to serve both modes, so the authority is used either way.
 */

import type { DomainName, Path, Request, Source } from '@/api/types'
import { renderDomain } from '@/api/types'
import { selectorLabel } from './labels'
import { endpointKey } from './ownership'

/** One request's claim on one path, through one of its source entries. */
export interface Claim {
  requestId: string
  request: Request
  /** The index into `request.sources`, or -1 if the join could not attribute it. */
  sourceIndex: number
  source: Source | undefined
  /** What the operator wrote — the whole answer to "why are there two of these". */
  selector: string
}

export interface ClaimedPath {
  path: Path
  /** Claims from requests **in this namespace**, ordered by request id. */
  claims: Claim[]
  /**
   * `path.requests.length` — the refcount across every namespace, since namespaces partition
   * requests and not destinations. This is the number that decides whether deleting a claim stops
   * anything.
   */
  heldBy: number
  /** Holders outside this namespace. The `+archive` fact, at claim-line altitude. */
  foreign: string[]
}

export interface DestinationGroup {
  key: string
  node: string
  domain: DomainName
  paths: ClaimedPath[]
  /**
   * Other namespaces writing into this same domain — the `+ archive` fact.
   *
   * **Not the same question as {@link ClaimedPath.foreign}, and it is the wider one.** That is
   * another namespace holding *this path*; this is another namespace landing anything at all in
   * this domain, which is ordinary fan-in and is invisible from inside one namespace. It is the
   * fact that changes what emptying this destination would mean: two shows deliberately fanning
   * into one archive domain is the arrangement fan-in exists for, so dropping every claim here
   * would not empty the domain.
   *
   * Computed over the whole path list rather than over this namespace's claims, which is why
   * {@link buildLedger} takes every path rather than a pre-filtered set.
   */
  foreignNamespaces: string[]
}

export interface RequestTally {
  request: Request
  paths: number
  /**
   * Paths no one else holds — the ones that stop if this request is deleted. The whole cancellation
   * preview, standing rather than summoned by a dialog.
   */
  sole: number
  shared: number
  /**
   * Zero sole paths while holding some: this request is carrying nothing. Nothing is broken and
   * nothing is doubled — refcounting is working exactly as designed — but somebody wrote an intent
   * entirely subsumed by another, which in an adapter-populated namespace is usually a bug in the
   * thing writing the requests. It has no other symptom anywhere in the product.
   */
  ridesAlong: boolean
}

export interface Ledger {
  groups: DestinationGroup[]
  requests: RequestTally[]
  pathCount: number
  notActiveCount: number
}

/** The namespace a request belongs to, defaulting an unset one exactly as the server does. */
export function namespaceOf(request: Request): string {
  return request.namespace || 'default'
}

/**
 * Which source entry produced this path.
 *
 * `status.sources[]` carries each entry's own path IDs, which is the only honest way to attribute a
 * path to a source: a label selector expands over several domains, so a path's own source address
 * does not say which entry asked for it.
 *
 * The fallback matters for robustness rather than for the common case. `ui.md` §7c says the list is
 * absent for a single-source request; against a server built from this tree it is always present
 * (verified 2026-09-01) — the Go doc comment on `RequestStatus.Sources` says "always present and
 * always the full list", and it is the one that is right. Keeping the fallback costs a line and
 * means an older server, or a request whose paths the join misses, still attributes rather than
 * rendering an unlabelled claim.
 */
export function sourceIndexFor(request: Request, pathId: string): number {
  const sources = request.status.sources ?? []
  const index = sources.findIndex((entry) => entry.paths?.includes(pathId))
  if (index >= 0) return index
  return request.sources.length === 1 ? 0 : -1
}

function claimFor(request: Request, path: Path): Claim {
  const sourceIndex = sourceIndexFor(request, path.id)
  const source = sourceIndex >= 0 ? request.sources[sourceIndex] : undefined
  return {
    requestId: request.id,
    request,
    sourceIndex,
    source,
    selector: source ? selectorLabel(source.select) : 'unattributed',
  }
}

/**
 * Groups by `(node, rendered domain)` — the key `ownership.ts` defines and the matrix's columns use,
 * so that the map below can be read by a view that is not this one.
 */
const groupKey = (path: Path) => endpointKey(path.destination)

/** The namespace half of a rendered `<namespace>/<name>`. Neither half can contain the separator. */
const namespaceOfId = (id: string) => id.slice(0, id.indexOf('/'))

/**
 * Which namespaces write into each destination domain, over the **whole** fleet.
 *
 * Built before the namespace filter, because the question is precisely about what a view of one
 * namespace cannot otherwise see.
 */
export function namespacesByDestination(paths: Path[]): Map<string, Set<string>> {
  const map = new Map<string, Set<string>>()
  for (const path of paths) {
    const key = groupKey(path)
    let set = map.get(key)
    if (!set) map.set(key, (set = new Set<string>()))
    for (const id of path.requests ?? []) set.add(namespaceOfId(id))
  }
  return map
}

/**
 * Build the ledger for one namespace.
 *
 * Group by destination domain, then by source flow. That is the axis fan-in runs along, it is the
 * binding resource direction on the ingress side — twelve sources into one domain is 12× ingress on
 * that node — and it is what puts two claims on one flow on adjacent lines, where the diagnosis is a
 * two-second read rather than a hunt.
 */
export function buildLedger(paths: Path[], requests: Request[], namespace: string): Ledger {
  const inNamespace = requests.filter((request) => namespaceOf(request) === namespace)
  const byId = new Map(inNamespace.map((request) => [request.id, request]))

  const claimed: ClaimedPath[] = []
  for (const path of paths) {
    const holders = path.requests ?? []
    const claims = holders
      .filter((id) => byId.has(id))
      .sort()
      .map((id) => claimFor(byId.get(id)!, path))

    // A path held entirely by other namespaces is not this namespace's business, and listing it
    // would put edges on screen that no request here asked for.
    if (claims.length === 0) continue

    claimed.push({
      path,
      claims,
      heldBy: holders.length,
      foreign: holders.filter((id) => !byId.has(id)),
    })
  }

  const groups = new Map<string, DestinationGroup>()
  for (const entry of claimed) {
    const key = groupKey(entry.path)
    let group = groups.get(key)
    if (!group) {
      group = {
        key,
        node: entry.path.destination.node,
        domain: renderDomain(entry.path.destination.domain),
        paths: [],
        foreignNamespaces: [],
      }
      groups.set(key, group)
    }
    group.paths.push(entry)
  }

  const writers = namespacesByDestination(paths)
  for (const group of groups.values()) {
    group.paths.sort(bySourceAddress)
    group.foreignNamespaces = [...(writers.get(group.key) ?? [])]
      .filter((name) => name !== namespace)
      .sort()
  }

  const tallies = inNamespace
    .map((request) => tally(request, claimed))
    .sort((a, b) => a.request.id.localeCompare(b.request.id))

  return {
    groups: [...groups.values()].sort(
      (a, b) => a.node.localeCompare(b.node) || a.domain.localeCompare(b.domain),
    ),
    requests: tallies,
    pathCount: claimed.length,
    notActiveCount: claimed.filter(needsReading).length,
  }
}

function bySourceAddress(a: ClaimedPath, b: ClaimedPath): number {
  return (
    a.path.source.node.localeCompare(b.path.source.node) ||
    a.path.source.domain.localeCompare(b.path.source.domain) ||
    a.path.source.flow.localeCompare(b.path.source.flow)
  )
}

function tally(request: Request, claimed: ClaimedPath[]): RequestTally {
  const held = claimed.filter((entry) => entry.claims.some((claim) => claim.requestId === request.id))
  const sole = held.filter((entry) => entry.heldBy === 1).length
  return {
    request,
    paths: held.length,
    sole,
    shared: held.length - sole,
    ridesAlong: held.length > 0 && sole === 0,
  }
}

/**
 * Keep only what an operator should look at first.
 *
 * **State alone.** A path held by several requests is not a condition to address: in a `shared`
 * namespace it is refcounting working exactly as designed, and it is the arrangement the mode
 * exists for — one request per pod means several pods asking for one flow, routinely. Flagging it
 * would make an ordinary population read as a list of faults.
 *
 * An adapter-populated namespace can hold thousands of requests, so the ledger opens summarised —
 * the same instinct as `status` naming only what is not active, applied one level down, and the
 * reason this view scales where a grid of the same cardinality does not.
 */
export function needsReading(entry: ClaimedPath): boolean {
  return entry.path.state !== 'ACTIVE'
}
