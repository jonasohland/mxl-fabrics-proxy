/**
 * Unrouted sources (`ui.md` §7a) — what exists in the fleet that this namespace is not carrying.
 *
 * **A matrix shows only what someone has already asked for**, so "camera 5 is going nowhere" is
 * invisible on it by construction: a flow nobody wrote a source for has no row, and a row that does
 * not exist cannot be dark. That is the discovery loop the grid leaves open, and closing it is what
 * turns the workspace from an editor of existing intent into a place where new intent starts.
 *
 * ## What "unrouted" is computed from, and what it deliberately is not
 *
 * The obvious reading — *no request's selector matches this flow* — would need selector evaluation
 * in the browser: label sets ANDed over a node's domains, group hints with and without a type,
 * pinned IDs, and the `self_output` exclusion on top. That is `reconcile.Compute`'s job, the server
 * already does it on every read, and reimplementing it here would produce a second expansion engine
 * whose disagreements with the real one are silent (`ui.md` §3: *"let the server be the authority"*).
 *
 * So the question asked here is the one the API answers directly: **is there a path carrying this
 * flow as its source?** `path.source` is a `FlowAddress` and an inventory entry is the same triple,
 * so the join is a set membership test over `flowAddressKey` and nothing is guessed.
 *
 * The two readings differ in exactly one place and it is worth knowing which way: a request whose
 * selector matches a flow but whose every destination is **parked** expands to no path, so this
 * counts the flow as unrouted. That is the more useful answer of the two — the strip's question is
 * *is this going anywhere*, and a parked route is going nowhere — but it is not the same sentence,
 * which is why nothing here claims "no request selects it".
 *
 * ## The two filters, both load-bearing (§7a, §7b)
 *
 * - **Namespace-scoped, with a note on entries another namespace routes.** Neither plain reading
 *   works. Fleet-wide, a flow another namespace routes vanishes from the strip — and since the strip
 *   is also the flow browser, it becomes undiscoverable in the one view where routing is built.
 *   Namespace-scoped and *silent*, it shows the flow as untouched, the operator routes it, no
 *   `namespace_overlap` fires because the duplicate is in another namespace, and the strip has just
 *   talked them into doubling egress on the source node. So: scoped, and {@link UnroutedFlow.elsewhere}
 *   names who else has it.
 * - **`replicated: true` is this project's own output.** Legitimately a source — that is how a chain
 *   `A→B→C` is written, the second hop naming the domain the first materialised — but it is not
 *   *unrouted*, it is the far end of something already routed. Marked rather than hidden, because
 *   hiding it would make a chain unwritable from the one screen that is for writing routes.
 *
 * Both marks are the same shape: an entry the strip lists but does not count as work.
 */

import type { DomainName, FlowEntry, GroupHint, Path } from '@/api/types'
import { flowAddressKey } from './ownership'
import { namespaceOfId } from './staging'

/** One inventory entry, with what accounts for it. */
export interface UnroutedFlow {
  /** `(node, domain, flow)`. Never the ID alone — the same UUID exists in two places on purpose. */
  key: string
  node: string
  /**
   * Rendered — it identifies a domain that already exists, which is the only kind inventory has.
   *
   * Kept in the branded {@link DomainName} rather than widened to `string`, so it can be handed to a
   * route or a lookup but can never be passed where a structured `Domain` is wanted. The brand is
   * what makes "never split a rendered domain back apart to send it" a type error.
   */
  domain: DomainName
  flow: string
  group?: GroupHint
  producing: boolean
  /**
   * One of this node's own target workers is writing it. A label selector will never match it (the
   * server drops it as `self_output`), so routing it onward means naming its domain.
   */
  replicated: boolean
  /**
   * Namespaces already carrying it as a source. Never contains the namespace being built for — a
   * flow this one routes is not in the strip at all.
   */
  elsewhere: string[]
  /** Nothing accounts for it: nobody routes it and this project did not write it. The actual work. */
  unclaimed: boolean
}

/**
 * One `(node, domain)`, because that is the unit a source is written against.
 *
 * A source pins a node and selects a domain, so the group header is the thing the editor opens on —
 * and "route this whole domain" is one gesture rather than one per flow.
 */
export interface UnroutedDomain {
  key: string
  node: string
  domain: DomainName
  flows: UnroutedFlow[]
  unclaimed: number
}

export interface Unrouted {
  domains: UnroutedDomain[]
  /** Entries listed, after the filter and before the cap. */
  count: number
  /** Of those, the ones nothing accounts for. The number an operator is actually reading. */
  unclaimed: number
  /** Listed but accounted for, by the two marks above. Counted so the filter can say what it hides. */
  elsewhere: number
  replicated: number
  /** Inventory entries this namespace already carries. The strip is the complement of this. */
  routed: number
  /**
   * Entries the cap discarded.
   *
   * A silent cap reads as "nothing else was unrouted", which is the failure mode the API's own
   * `excluded_dropped` exists to prevent — so this is reported for the same reason and rendered
   * whenever it is non-zero.
   */
  dropped: number
}

/**
 * What the source editor opens on when a strip entry is clicked.
 *
 * The domain is the **rendered** name and is used for a lookup by rendered equality against the
 * node's own domain list, exactly as a domain's URL is (`ui-plan.md` §2). Nothing reconstructs an
 * `{area, elements}` from it: the structured domain the editor puts in a selector is always the
 * server's own, so the one thing the design forbids outright — splitting a rendered domain to send
 * it — stays off the page rather than being a rule somebody has to remember.
 *
 * `flow` is a *hint about which group to open on*, not a pin. A flow carrying a group hint opens on
 * its group, because the group is what an operator means; only a flow with no hint opens pinned by
 * ID, because there is no name for a group selector to match.
 */
export interface SourcePrefill {
  node: string
  domain: DomainName
  flow?: string
}

/** How many rows the strip will render before it starts counting instead. */
export const UNROUTED_LIMIT = 200

export interface UnroutedOptions {
  /** Hide the two accounted-for kinds. The attention filter, and it defaults on in the view. */
  unclaimedOnly?: boolean
  limit?: number
}

/**
 * Build the strip for one namespace.
 *
 * `paths` is the **whole** fleet's list rather than this namespace's: the note on an entry another
 * namespace routes is a question about every other namespace, which a filtered list cannot answer —
 * the same reason the matrix's column header needs it (`model/matrix.ts`).
 */
export function buildUnrouted(
  flows: FlowEntry[],
  paths: Path[],
  namespace: string,
  options: UnroutedOptions = {},
): Unrouted {
  const limit = options.limit ?? UNROUTED_LIMIT

  // Which namespaces carry each source address. `path.requests[]` is the only statement of
  // ownership in the API, so the namespaces come from it rather than from a request's own status —
  // the loser of an overlap lists the contested path and holds none of it (`ui.md` §5 trap 14).
  const carriers = new Map<string, Set<string>>()
  for (const path of paths) {
    const key = flowAddressKey(path.source.node, path.source.domain, path.source.flow)
    let namespaces = carriers.get(key)
    if (!namespaces) {
      namespaces = new Set()
      carriers.set(key, namespaces)
    }
    for (const id of path.requests) namespaces.add(namespaceOfId(id))
  }

  let routed = 0
  let unclaimed = 0
  let elsewhereCount = 0
  let replicatedCount = 0

  const listed: UnroutedFlow[] = []

  for (const entry of flows) {
    const key = flowAddressKey(entry.node, entry.domain, entry.id)
    const namespaces = carriers.get(key)

    if (namespaces?.has(namespace)) {
      routed++
      continue
    }

    const replicated = entry.replicated === true
    const elsewhere = [...(namespaces ?? [])].sort()
    const isUnclaimed = !replicated && elsewhere.length === 0

    if (isUnclaimed) unclaimed++
    else if (replicated) replicatedCount++
    else elsewhereCount++

    if (options.unclaimedOnly === true && !isUnclaimed) continue

    listed.push({
      key,
      node: entry.node,
      domain: entry.domain,
      flow: entry.id,
      ...(entry.group_hint ? { group: entry.group_hint } : {}),
      producing: entry.producing,
      replicated,
      elsewhere,
      unclaimed: isUnclaimed,
    })
  }

  // Sorted before the cap, so what the cap keeps is the part worth reading rather than whatever the
  // server's inventory order happened to put first.
  listed.sort(
    (a, b) =>
      a.node.localeCompare(b.node) ||
      a.domain.localeCompare(b.domain) ||
      rank(a) - rank(b) ||
      Number(b.producing) - Number(a.producing) ||
      (a.group?.name ?? '').localeCompare(b.group?.name ?? '') ||
      (a.group?.type ?? '').localeCompare(b.group?.type ?? '') ||
      a.flow.localeCompare(b.flow),
  )

  const kept = listed.slice(0, limit)

  const byDomain = new Map<string, UnroutedDomain>()
  for (const flow of kept) {
    const key = `${flow.node}\u0000${flow.domain}`
    let group = byDomain.get(key)
    if (!group) {
      group = { key, node: flow.node, domain: flow.domain, flows: [], unclaimed: 0 }
      byDomain.set(key, group)
    }
    group.flows.push(flow)
    if (flow.unclaimed) group.unclaimed++
  }

  return {
    domains: [...byDomain.values()],
    count: listed.length,
    unclaimed,
    elsewhere: elsewhereCount,
    replicated: replicatedCount,
    routed,
    dropped: listed.length - kept.length,
  }
}

/** Work first, then the two kinds of already-accounted-for. */
function rank(flow: UnroutedFlow): number {
  if (flow.unclaimed) return 0
  return flow.replicated ? 2 : 1
}
