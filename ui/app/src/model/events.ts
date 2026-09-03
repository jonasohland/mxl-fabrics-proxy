/**
 * Rendering an event log, mirroring `cmd/mxl-replicator/events.go`.
 *
 * The CLI worked this out first and its answers are right, so this is a translation rather than a
 * second design — the same reason `model/detail.ts` exists beside `describe.go`. An operator moving
 * between `mxl-replicator events path <id>` and this screen is reading one log twice, and a
 * divergence between the two would be silent.
 *
 * **Everything here is a consequence of a decision in architecture §12.1 that is invisible from the
 * JSON**, which is why it is a module with tests rather than five expressions inside a template.
 * `docs/open-items.md` §2.11 enumerates them; each one below says which it is discharging. The
 * common failure is treating the response as an ordinary list, and every one of these is a way that
 * goes wrong.
 */

import type { Event, EventList, EventSeverity } from '@/api/types'

/**
 * Which object's log to fetch. A closed union rather than three optional props, because it is what
 * makes "only a path has worker logs" structural: there is no way to spell a request with a tail.
 */
export type EventSubject =
  | { kind: 'path'; id: string }
  | { kind: 'request'; namespace: string; name: string }
  | { kind: 'node'; node: string }

/** The identity `stores/read.ts` watches — a change here means a different object, not a refresh. */
export function subjectKey(subject: EventSubject): string {
  switch (subject.kind) {
    case 'path':
      return `path ${subject.id}`
    case 'request':
      return `request ${subject.namespace}/${subject.name}`
    case 'node':
      return `node ${subject.node}`
  }
}

/**
 * The short column: what kind of thing this is, in the vocabulary an operator reads.
 *
 * `state` when there is one, because the state *is* the news on a transition — and it is a field
 * rather than something dug out of the message precisely so that a renderer gets its badge without
 * parsing English. Otherwise the kind with its underscores opened up. Mirrors `eventSubject`.
 */
export function eventSubject(event: Event): string {
  if (event.state) return event.state
  return event.kind.replaceAll('_', ' ')
}

/**
 * How many times, over how long — `×47 over 6m`, or nothing at all.
 *
 * **This is the one row-shaping rule that must not be got wrong** (`docs/open-items.md` §2.11): a
 * coalesced entry is *one* row. `count` is how many occurrences it stands for, and expanding it into
 * that many rows re-creates the flapping the ring exists to compress, in the one place the ring can
 * no longer bound anything.
 *
 * `first_at` is set only when `count` is above one, so an ordinary entry says one thing and carries
 * one timestamp.
 */
export function countSpan(event: Event): string | undefined {
  const count = event.count ?? 0
  if (count <= 1) return undefined

  const span = spanText(event.first_at, event.at)
  return span ? `×${count} over ${span}` : `×${count}`
}

/**
 * A coarse duration between two stamps on **one** ring's entry.
 *
 * Safe where a cross-host comparison would not be: both stamps come from the single emitter that
 * coalesced the run, so the difference is a duration rather than a claim about two clocks. Nothing
 * else in this module subtracts two timestamps.
 *
 * Rounded the way `since()` rounds, not the way Go's `Duration.String()` prints — `6m` rather than
 * `6m0s`. A second spelling of a duration is worth less than not having a Go-format reimplementation
 * to keep honest.
 */
export function spanText(from: string | undefined, to: string | undefined): string | undefined {
  if (!from || !to) return undefined

  const start = Date.parse(from)
  const end = Date.parse(to)
  if (Number.isNaN(start) || Number.isNaN(end)) return undefined

  const seconds = Math.max(0, Math.round((end - start) / 1000))
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

/**
 * The wall clock, `15:04:05` local, as the CLI prints it.
 *
 * **Display only**, and it is the whole of what a timestamp is for here. An entry is stamped by
 * whoever emitted it, so a merged request view interleaves two agents' clocks and a leader's — TAI
 * correctness is a deployment assumption about *offsets*, not about ordering across hosts. Nothing
 * in this UI sorts on these, computes an interval across two of them, or draws them on an axis.
 */
export function eventTime(at: string | undefined): string {
  if (!at) return ''
  const stamp = Date.parse(at)
  if (Number.isNaN(stamp)) return ''

  const time = new Date(stamp)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${pad(time.getHours())}:${pad(time.getMinutes())}:${pad(time.getSeconds())}`
}

/**
 * The severity class, and **the only thing that may colour a row**.
 *
 * `severity` is not the state vocabulary and a row must never be coloured from `event.state`.
 * Designed behaviour never warns — a long-idle teardown, a start waiting for a permit, a producer
 * stopping — so an entry whose state is `PAUSED` is routinely `info`, and colouring from the state
 * would turn a board of ordinary fleet activity into a board of problems. That is the mistake §11
 * avoids twice, once by giving an idle source `PAUSED` rather than a fault and once by giving a
 * parked leg `DISABLED`.
 *
 * The state badge keeps its own `state-*` colour, on its own element. Two classes on two elements
 * rather than one decision, so the rule is structural rather than remembered.
 */
export function severityClass(severity: EventSeverity | undefined): string {
  switch (severity) {
    case 'error':
      return 'ev-error'
    case 'warn':
      return 'ev-warn'
    default:
      return 'ev-info'
  }
}

/**
 * What the ring's own `dropped` counter has to say, or nothing.
 *
 * **Not the same loss as an `events_dropped` entry**, and the two are deliberately worded apart:
 * this is history that aged out of a full ring — it happened and was recorded and is now gone — and
 * that one is entries an agent never managed to report at all. Only the second means something was
 * never seen, and saying them the same way loses exactly that.
 */
export function agedOutNote(list: EventList | undefined): string | undefined {
  const dropped = list?.dropped ?? 0
  if (dropped <= 0) return undefined
  return `${dropped} older ${dropped === 1 ? 'entry has' : 'entries have'} aged out of this ring`
}

/**
 * The entries, in the order the server returned them, oldest first.
 *
 * **It does not sort, and that is a decision rather than an omission.** Within one ring the order is
 * the sequence; across the rings a request's view merges, the server has already ordered by time
 * (`Recorder.Merge`), which is the best available and is not causal. Re-sorting here would either
 * repeat that or — worse — assert an ordering across hosts that the design says is not there.
 *
 * Newest *last*, as a log file reads and as the CLI prints, so a reader's eye ends on what is
 * happening now.
 */
export function entries(list: EventList | undefined): Event[] {
  return list?.events ?? []
}

/**
 * A stable key for the row.
 *
 * `seq` alone is not it: a merged request view interleaves several rings whose sequences are
 * independent, so two entries can genuinely share one number. Nothing here is a cursor — the pair is
 * only for the renderer's list reconciliation.
 */
export function eventKey(event: Event, index: number): string {
  return `${event.seq}-${index}`
}
