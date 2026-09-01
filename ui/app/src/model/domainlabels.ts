/**
 * Editing one `(node, domain)`'s labels — the second mutation (`ui.md` §3, §10.7).
 *
 * A domain label is annotation, never identity, and that is exactly what makes it dangerous: labels
 * are what a source selector matches, so writing one **joins or removes a domain from a request's
 * expansion** and starts and stops media one level of indirection away from a request. It is easier
 * to do by accident than a request edit is, not harder, which is why every write from this screen is
 * dry-run first and why the preview is the server's own `stopped[]` / `started[]` rather than
 * anything computed here.
 *
 * Three rules this module exists to hold, each of which is silent when broken:
 *
 * - **A patch, never an apply.** An apply *owns the keys it declares* — it removes the ones it
 *   declared last time and no longer does — so a read-modify-write with `apply` would silently adopt,
 *   and later delete, keys someone else's manifest declared. An interactive editor has no declared
 *   set of its own, so it sends `{set, remove}` and merges against nothing.
 * - **The patch is a *diff*, not the map on screen.** Sending every pair back as `set` would be a
 *   read-modify-write in a different spelling: it re-asserts values another writer changed between
 *   the read and the write, and the server's no-write-if-unchanged check is over the *record*, so it
 *   would not save us. Only what the operator actually changed goes on the wire.
 * - **The reserved set is the server's, not the document's.** `ui.md` §3 lists `domain_name` as
 *   reserved and does not list `format` or `media_type`; what is enforced is
 *   `metrics.WorkerLabelNames()` plus `quantile`, which is the opposite on all three. Mirrored from
 *   the code, because it is the list an operator will actually hit.
 */

import type { DomainLabelPatch, DomainLabelResult, Path } from '@/api/types'
import { elementError } from '@/model/naming'

/** `internal/server/userapi.go`'s `maxLabelKeyLength` / `maxLabelValueLength`. */
export const MAX_LABEL_KEY_LEN = 63
export const MAX_LABEL_VALUE_LEN = 253

/**
 * The keys this project sets itself, which a user label may not take —
 * `metrics.WorkerLabelNames()` plus `quantile`.
 *
 * A label rides into every one of a session's metric series, so a user key colliding with one of
 * these would either shadow a dimension the operator needs or invalidate the family at collection
 * time. Refused at write time rather than dropped at scrape time, because a label silently discarded
 * is a label the operator believes is there.
 */
export const RESERVED_LABEL_KEYS: readonly string[] = [
  'direction', 'domain', 'flow_id', 'session', 'namespace', 'format', 'media_type', 'quantile',
]

/** The one conventional key: what an operator calls this domain. Not identity — a relabel moves nothing. */
export const LABEL_NAME = 'name'

/** `metrics.ValidLabelName`, plus this server's bounds and its reserved set. */
export function labelKeyError(key: string): string | undefined {
  if (key === '') return 'a label needs a key'
  if (!/^[a-zA-Z_][a-zA-Z0-9_]*$/.test(key)) {
    return `"${key}" is not a usable Prometheus label name: letters, digits and underscore, not starting with a digit`
  }
  if (key.startsWith('__')) return `"${key}" starts with __, which Prometheus reserves`
  if (RESERVED_LABEL_KEYS.includes(key)) return `"${key}" is reserved for worker metrics`
  if (key.length > MAX_LABEL_KEY_LEN) return `"${key}" is longer than ${MAX_LABEL_KEY_LEN} characters`
  return undefined
}

/**
 * The value, and the one key whose value is constrained.
 *
 * `name` is rendered as the `domain_name` metric label, so its *value* is held to the domain element
 * grammar — the same rule, for the same reason a request's `namespace` value was the only one
 * constrained when it was still a label.
 */
export function labelValueError(key: string, value: string): string | undefined {
  if (value.length > MAX_LABEL_VALUE_LEN) {
    return `the value of "${key}" is longer than ${MAX_LABEL_VALUE_LEN} characters`
  }
  if (key === LABEL_NAME && value !== '') {
    const bad = elementError(value)
    if (bad) return `label "name": ${bad}`
  }
  return undefined
}

/**
 * One row of the editor: a pair as stored, as edited, and whether it is on its way out.
 *
 * `stored` absent is a pair the operator is adding, which is the only difference the diff needs —
 * an added row and an edited row both reach the wire as `set`, and only a row that was there can be
 * removed.
 */
export interface LabelRow {
  key: string
  value: string
  /** The value on the server. Absent for a pair being added. */
  stored?: string
  removed: boolean
}

/** The stored record as rows, sorted, so the list does not reorder itself under an edit. */
export function rowsOf(labels: Record<string, string> | undefined): LabelRow[] {
  return Object.entries(labels ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, value]) => ({ key, value, stored: value, removed: false }))
}

/**
 * What changed, as the wire's own two operations.
 *
 * A removed row that was never stored is *nothing* rather than a `remove`: asking the server to
 * delete a key that does not exist is a write it would refuse to make interesting, and it would put
 * the operator's discarded typing on the wire.
 */
export function patchOf(rows: readonly LabelRow[]): DomainLabelPatch {
  const set: Record<string, string> = {}
  const remove: string[] = []

  for (const row of rows) {
    if (row.key === '') continue
    if (row.removed) {
      if (row.stored !== undefined) remove.push(row.key)
      continue
    }
    if (row.stored === undefined || row.stored !== row.value) set[row.key] = row.value
  }

  const patch: DomainLabelPatch = {}
  if (Object.keys(set).length > 0) patch.set = set
  if (remove.length > 0) patch.remove = remove.sort()
  return patch
}

/** A patch that sets and removes nothing is refused by the server, and there is nothing to preview. */
export function patchEmpty(patch: DomainLabelPatch): boolean {
  return Object.keys(patch.set ?? {}).length === 0 && (patch.remove ?? []).length === 0
}

/**
 * The refusals decidable from the rows alone.
 *
 * Everything else — whether a key is one the fleet cares about, what a write would move — needs the
 * fleet and is the dry run's, exactly as `model/naming.ts` draws the line for a request.
 */
export function rowsError(rows: readonly LabelRow[]): string | undefined {
  const seen = new Set<string>()
  for (const row of rows) {
    if (row.key === '' && row.value === '' && row.stored === undefined) continue // an untouched blank row
    const bad = labelKeyError(row.key) ?? labelValueError(row.key, row.value)
    if (bad) return bad
    if (seen.has(row.key)) return `"${row.key}" is written twice`
    seen.add(row.key)
  }
  return undefined
}

/** A request and how many of the write's paths it is on. */
export interface RequestImpact {
  id: string
  paths: number
}

/**
 * Which requests a set of paths belongs to, biggest first.
 *
 * `path.requests[]` is the refcount and it is already on every path the result carries, so this is a
 * renderer rather than a computation — the same property that lets a cancellation preview say what
 * keeps running without a second read.
 */
export function byRequest(paths: readonly Path[]): RequestImpact[] {
  const counts = new Map<string, number>()
  for (const path of paths) {
    for (const id of path.requests) counts.set(id, (counts.get(id) ?? 0) + 1)
  }
  return [...counts]
    .map(([id, count]) => ({ id, paths: count }))
    .sort((a, b) => b.paths - a.paths || a.id.localeCompare(b.id))
}

/** The blast radius of one label write, as the server measured it. */
export interface LabelImpact {
  stopped: Path[]
  started: Path[]
  losing: RequestImpact[]
  gaining: RequestImpact[]
  /** Keys the last `apply` declared, which a patch does not touch — see {@link declaredWarning}. */
  declared: string[]
}

export function impactOf(result: DomainLabelResult): LabelImpact {
  const stopped = result.stopped ?? []
  const started = result.started ?? []
  return {
    stopped,
    started,
    losing: byRequest(stopped),
    gaining: byRequest(started),
    declared: result.declared ?? [],
  }
}

/**
 * The surprise a patch carries, and the same one parking a leg carries: **the file is authoritative**.
 *
 * A patch deliberately does not touch `declared`, which is what makes an interactive edit survive an
 * apply — but the other half of that is that an apply which still declares this key will write its own
 * value back over the edit, and one that has stopped declaring it will remove it. Named here because
 * the operator cannot see the manifest from this screen, and `declared` is the only evidence the API
 * gives that one exists.
 */
export function declaredWarning(patch: DomainLabelPatch, declared: readonly string[]): string | undefined {
  const touched = [...Object.keys(patch.set ?? {}), ...(patch.remove ?? [])]
  const owned = touched.filter((key) => declared.includes(key)).sort()
  if (owned.length === 0) return undefined
  return `${owned.join(', ')} ${owned.length === 1 ? 'was' : 'were'} declared by an apply. The ` +
    'manifest is authoritative and the next apply writes its own value back.'
}
