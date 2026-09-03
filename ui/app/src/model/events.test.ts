import { describe, expect, it } from 'vitest'

import type { Event, EventList } from '@/api/types'
import {
  agedOutNote,
  countSpan,
  entries,
  eventKey,
  eventSubject,
  eventTime,
  severityClass,
  spanText,
  subjectKey,
} from './events'

/**
 * Each block here pins one of the five decisions `docs/open-items.md` §2.11 says a renderer that
 * treats an event list as an ordinary list gets wrong. They are unit tests rather than live ones
 * because every one of them is a property of the *shape* of an entry, and the live suite cannot make
 * a fleet flap forty-seven times on demand.
 */

function event(overrides: Partial<Event> = {}): Event {
  return {
    seq: 1,
    kind: 'path_state_changed',
    severity: 'info',
    at: '2026-09-02T12:04:11Z',
    message: 'first grain received',
    ...overrides,
  }
}

describe('a coalesced entry is one row', () => {
  // The rule the whole ring bound depends on: forty-seven identical worker exits are one row that
  // says *flapping*, and expanding them re-creates the churn the ring exists to compress in the one
  // place nothing can bound it.
  it('renders the count and the span rather than repeating the row', () => {
    const flapping = event({
      kind: 'worker_exited',
      severity: 'error',
      count: 47,
      first_at: '2026-09-02T12:41:04Z',
      at: '2026-09-02T12:47:22Z',
    })

    expect(countSpan(flapping)).toBe('×47 over 6m')
  })

  // `first_at` is set only when `count` is above one, so an ordinary entry says one thing and
  // carries one timestamp — and must not grow a `×1`.
  it('says nothing at all for a single occurrence', () => {
    expect(countSpan(event())).toBeUndefined()
    expect(countSpan(event({ count: 1 }))).toBeUndefined()
  })

  // A count without a first_at is not a reason to drop the count.
  it('keeps the count when the span cannot be computed', () => {
    expect(countSpan(event({ count: 3 }))).toBe('×3')
  })

  it('rounds a span the way an age is rounded', () => {
    expect(spanText('2026-09-02T12:00:00Z', '2026-09-02T12:00:09Z')).toBe('9s')
    expect(spanText('2026-09-02T12:00:00Z', '2026-09-02T13:30:00Z')).toBe('1h')
    expect(spanText(undefined, '2026-09-02T12:00:00Z')).toBeUndefined()
    expect(spanText('nonsense', '2026-09-02T12:00:00Z')).toBeUndefined()
  })
})

describe('severity is not the state vocabulary', () => {
  /*
   * The trap this exists for: designed behaviour never warns. An idle teardown is `PAUSED` and
   * `info`, and a renderer that coloured the row from the state would paint a routine event in the
   * palette of a fault — which is the board-full-of-false-faults §11 avoids twice over, once by
   * giving an idle source `PAUSED` rather than a failure and once by giving a parked leg `DISABLED`.
   */
  it('gives an info row the info class even when its state is a loud one', () => {
    const teardown = event({ severity: 'info', state: 'PAUSED', message: 'idle teardown' })
    expect(severityClass(teardown.severity)).toBe('ev-info')

    const invalid = event({ severity: 'info', state: 'INVALID' })
    expect(severityClass(invalid.severity)).toBe('ev-info')
  })

  it('maps the three severities and nothing else', () => {
    expect(severityClass('error')).toBe('ev-error')
    expect(severityClass('warn')).toBe('ev-warn')
    expect(severityClass('info')).toBe('ev-info')
    // A severity this UI has not heard of reads as quiet rather than as a fault: inventing an alarm
    // for a word from a newer server is the wrong direction to be wrong in.
    expect(severityClass(undefined)).toBe('ev-info')
  })
})

describe('the subject column', () => {
  // The state *is* the news on a transition, and it is a field precisely so a renderer gets it
  // without parsing English out of the message.
  it('leads with the state where there is one', () => {
    expect(eventSubject(event({ state: 'FAILED' }))).toBe('FAILED')
  })

  it('opens up the kind where there is not', () => {
    expect(eventSubject(event({ kind: 'worker_start_queued' }))).toBe('worker start queued')
    expect(eventSubject(event({ kind: 'reconciler_took_over' }))).toBe('reconciler took over')
  })
})

describe('two different losses, worded apart', () => {
  /*
   * `dropped` is history that aged out of a full ring — it happened, it was recorded, it is gone. An
   * `events_dropped` *entry* is something an agent never managed to report at all. Both are expected
   * in a bad hour and only the second means something was never seen, so they must not read alike.
   */
  it('describes the ring’s own eviction as ageing out', () => {
    expect(agedOutNote({ events: [], next: 0, dropped: 12 })).toBe(
      '12 older entries have aged out of this ring',
    )
    expect(agedOutNote({ events: [], next: 0, dropped: 1 })).toBe(
      '1 older entry has aged out of this ring',
    )
  })

  it('says nothing when nothing aged out', () => {
    expect(agedOutNote({ events: [], next: 0 })).toBeUndefined()
    expect(agedOutNote({ events: [], next: 0, dropped: 0 })).toBeUndefined()
    expect(agedOutNote(undefined)).toBeUndefined()
  })

  // The entry keeps its own kind and its own row, so the two are never folded into one sentence.
  it('leaves an events_dropped entry as an ordinary row', () => {
    const lost = event({ kind: 'events_dropped', severity: 'warn', message: '31 entries were lost' })
    expect(eventSubject(lost)).toBe('events dropped')
    expect(severityClass(lost.severity)).toBe('ev-warn')
  })
})

describe('the order is the server’s', () => {
  /*
   * Nothing here sorts. Within one ring the order is the sequence; across the rings a request's view
   * merges, the server has already ordered by time, and re-deriving either would at best repeat it —
   * at worst assert an ordering across hosts that the design says is not there.
   */
  it('returns the entries untouched, oldest first', () => {
    const list: EventList = {
      next: 3,
      events: [event({ seq: 1 }), event({ seq: 2, node: 'edge-01' }), event({ seq: 3 })],
    }
    expect(entries(list).map((e) => e.seq)).toEqual([1, 2, 3])
  })

  it('reads a missing list as empty rather than as an error', () => {
    expect(entries(undefined)).toEqual([])
  })

  // A merged view interleaves independent rings, so two entries can genuinely carry one sequence
  // number. The row key is not a cursor and must not assume otherwise.
  it('keys a row on more than the sequence number', () => {
    const a = event({ seq: 7 })
    const b = event({ seq: 7 })
    expect(eventKey(a, 0)).not.toBe(eventKey(b, 1))
  })
})

describe('timestamps are for display', () => {
  it('renders a wall clock and nothing else', () => {
    // Local time by construction, so this asserts the shape rather than a zone the CI box happens
    // to be in — the point is that it is a clock reading, not a sortable key.
    expect(eventTime('2026-09-02T12:04:11Z')).toMatch(/^\d{2}:\d{2}:\d{2}$/)
  })

  it('renders nothing for an absent or unparseable stamp', () => {
    expect(eventTime(undefined)).toBe('')
    expect(eventTime('not a time')).toBe('')
  })
})

describe('the subject union', () => {
  // The key `stores/read.ts` watches: a change means a different object and the pane blanks, where a
  // tick means the same object read again and what is on screen stays.
  it('is distinct per object and stable per object', () => {
    expect(subjectKey({ kind: 'path', id: 'abc' })).toBe('path abc')
    expect(subjectKey({ kind: 'node', node: 'edge-01' })).toBe('node edge-01')
    expect(subjectKey({ kind: 'request', namespace: 'nab', name: 'wall' })).toBe('request nab/wall')

    // A request and a node that happen to share a name are two subjects.
    expect(subjectKey({ kind: 'node', node: 'wall' })).not.toBe(
      subjectKey({ kind: 'request', namespace: 'default', name: 'wall' }),
    )
  })
})
