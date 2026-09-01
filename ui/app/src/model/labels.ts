/**
 * Rendering the things an operator wrote, back to them.
 *
 * A selector is what the ledger's claim lines exist to show (`ui.md` §7c): a path with two claims
 * raises exactly one question — *why do I have two of these?* — and the answer is always the pair of
 * selectors that produced them. `all flows` and `flow 5592a23b…` on adjacent lines turns an
 * invisible interaction into something an operator reads without knowing the rule.
 */

import type { DomainSelector, RequestSpec, Selector, Source } from '@/api/types'
import { renderDomain } from '@/api/types'

/**
 * `1 path`, `2 paths`. Written out rather than spelled `path(s)`, because a control surface an
 * operator reads all day should read like prose and not like a format string.
 */
export function plural(count: number, singular: string, pluralForm = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : pluralForm}`
}

/** IDs are 32 hex characters. Truncating for display is fine; matching is exact. */
export function shortId(id: string | undefined, length = 8): string {
  return (id ?? '').slice(0, length)
}

/**
 * The flow selector, as the operator chose it.
 *
 * Two spellings of "everything" and the difference is worth keeping visible: `all` takes the whole
 * domain, a group hint with no type takes one group of it.
 */
export function selectorLabel(select: Selector): string {
  if (select.flow !== undefined) return `flow ${shortId(select.flow)}…`
  if (select.group_hint !== undefined) {
    const { name, type } = select.group_hint
    return type ? `group ${name} (${type})` : `group ${name}`
  }
  return 'all flows'
}

/**
 * The domain selector.
 *
 * A name is self-contained; a label set is a *standing query* — a domain labelled tomorrow joins it,
 * which is the point and is also the surprise. Rendered differently for that reason.
 */
export function domainSelectorLabel(domain: DomainSelector): string {
  if (domain.name !== undefined) return renderDomain(domain.name)
  const pairs = Object.entries(domain.labels ?? {})
    .map(([key, value]) => `${key}=${value}`)
    .sort()
  return `{${pairs.join(', ')}}`
}

/** `<node>/<domain-selector>` — the line the operator typed, not what it resolved to. */
export function sourceLabel(source: Source): string {
  return `${source.node}/${domainSelectorLabel(source.domain)}`
}

/**
 * `sources[i]`, spelled the way the server's own refusals spell it.
 *
 * `duplicate_source_flow` and `same_endpoint` name their operands by index, so the two spellings
 * should match — an operator reading a refusal and an operator reading a claim line are looking at
 * the same thing.
 */
export function sourceRef(index: number): string {
  return `sources[${index}]`
}

/** `<node> <area>/<elements>` for a destination, which is how a claims group is headed. */
export function destinationLabel(node: string, domain: { area: string; elements: string[] }): string {
  return `${node} ${renderDomain(domain)}`
}

/** The `<namespace>/<name>` a path's refcount list carries. */
export function requestId(spec: Pick<RequestSpec, 'namespace' | 'name'>): string {
  return `${spec.namespace || 'default'}/${spec.name}`
}
