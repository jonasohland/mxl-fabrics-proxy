<script setup lang="ts">
/**
 * What *happened* to one object — the pane `describe` prints under a status (architecture §12.1).
 *
 * **One component for all three subjects, and that is the structural decision here.** Everything
 * this renders is a consequence of a decision in §12.1 that is invisible from the JSON — a coalesced
 * entry is one row, `has_log` is a marker rather than content, `severity` is not the state
 * vocabulary, the cursor is not a timestamp, and two kinds of loss are worded apart — so three panes
 * would be three chances to get each of them wrong. `docs/open-items.md` §2.11 is the list, and it
 * is a specification for this file.
 *
 * **It does not start a timer.** The reads behind it are the first user-API reads that are
 * O(response) rather than O(fleet) — one `Get` on one key, no `Compute` — so a faster poll would for
 * once be affordable. It still rides `stores/read.ts` and the fleet clock, because `ui.md` §2's
 * one-timer rule is worth more than three seconds of latency on a diagnostic pane, and a second
 * cadence is the kind of thing that gets copied to the next component that cannot afford it. The
 * affordability is recorded here rather than spent.
 *
 * **The whole ring, every read.** No `?since=` cursor — see `api/client.ts` for why an incremental
 * poll cannot be made correct without reimplementing the coalescing key in TypeScript.
 */
import { computed, ref } from 'vue'

import { api } from '@/api/client'
import type { Event, LogTail } from '@/api/types'
import {
  agedOutNote,
  countSpan,
  entries,
  eventKey,
  eventSubject,
  eventTime,
  severityClass,
  subjectKey,
} from '@/model/events'
import type { EventSubject } from '@/model/events'
import { useRead } from '@/stores/read'

const props = defineProps<{ subject: EventSubject }>()

const read = useRead(
  (signal) => {
    const subject = props.subject
    switch (subject.kind) {
      case 'path':
        return api.pathEvents(subject.id, signal)
      case 'request':
        return api.requestEvents(subject.namespace, subject.name, signal)
      case 'node':
        return api.nodeEvents(subject.node, signal)
    }
  },
  () => subjectKey(props.subject),
)

const rows = computed(() => entries(read.data.value))
const agedOut = computed(() => agedOutNote(read.data.value))

/**
 * The tail behind a `has_log` marker, fetched **on the click and never on the poll**.
 *
 * That split is the entire reason the event carries a marker rather than the bytes (§12.2): the
 * list a UI polls has to stay cheap exactly when things are failing, which is when it is polled
 * hardest. A pane that fetched every tail as it rendered would undo the decision from the far end,
 * and it would do it silently — the screen would look identical.
 */
const tail = ref<LogTail | undefined>(undefined)
const tailError = ref<string | undefined>(undefined)
const tailLoading = ref(false)

async function openLog(): Promise<void> {
  if (props.subject.kind !== 'path') return

  tailLoading.value = true
  tailError.value = undefined
  try {
    const fetched = await api.pathLogs(props.subject.id)
    // A 404 is the ordinary answer once a tail has aged out of the store while the marker it belongs
    // to is still in the ring. Said plainly rather than raised: nothing is broken.
    tail.value = fetched
    if (!fetched) tailError.value = 'No worker log is stored for this path any more.'
  } catch (error) {
    tailError.value = error instanceof Error ? error.message : String(error)
  } finally {
    tailLoading.value = false
  }
}

function closeLog(): void {
  tail.value = undefined
  tailError.value = undefined
}

/** Only a path has worker logs, and the union in `model/events.ts` is what makes that structural. */
const canFetchLog = computed(() => props.subject.kind === 'path')

function hasLog(event: Event): boolean {
  return event.has_log === true && canFetchLog.value
}
</script>

<template>
  <section class="dt-section">
    <h2>
      Events
      <!-- History that aged out of a full ring. Deliberately worded apart from an `events_dropped`
           row below, which is entries an agent never managed to report at all: both are expected in
           a bad hour and only the second means something was never seen. -->
      <span v-if="agedOut" class="aged" :title="agedOut">· {{ agedOut }}</span>
    </h2>

    <!-- A read the fleet poll does not make, failing on its own. The rest of the page is still the
         poll's and is still good, so this is a line rather than a state. -->
    <p v-if="read.error.value" class="dt-read-error">{{ read.error.value.message }}</p>
    <p v-else-if="read.loading.value" class="dt-empty">Loading…</p>
    <!-- An object nothing has happened to yet is the ordinary case, not a 404 — and a newly elected
         leader has no baseline, so a quiet log after a takeover is honest rather than broken. -->
    <p v-else-if="rows.length === 0" class="dt-empty">No events recorded.</p>

    <!-- Oldest first, newest last: a log file's order, so the eye ends on what is happening now.
         Nothing here sorts — see `model/events.ts`. -->
    <table v-else class="dt-table ev-table">
      <tbody>
        <tr v-for="(event, index) in rows" :key="eventKey(event, index)" :class="severityClass(event.severity)">
          <!-- Display only. An entry is stamped by whoever emitted it, so a merged request view
               interleaves two agents' clocks and a leader's; nothing reads causality out of this
               column. -->
          <td class="ev-time mono">{{ eventTime(event.at) }}</td>

          <!-- The severity, and the only thing colouring the row. Never derived from `state`. -->
          <td class="ev-severity">{{ event.severity }}</td>

          <!-- The state keeps its own colour on its own element, so the two vocabularies cannot be
               folded into one decision by a later edit. -->
          <td class="ev-subject">
            <span v-if="event.state" :class="`state-${event.state}`">{{ event.state }}</span>
            <span v-else>{{ eventSubject(event) }}</span>
          </td>

          <td class="dt-wide ev-detail">
            <span class="ev-message">{{ event.message }}</span>

            <!-- One row, never `count` rows. `×47 over 6m` is what an operator needs to read — it
                 says *flapping* — and expanding it re-creates the churn the ring exists to compress
                 in the one place nothing can bound it. -->
            <span v-if="countSpan(event)" class="ev-count">{{ countSpan(event) }}</span>

            <!-- A marker, not content: the fetch is this click and nothing else. -->
            <button v-if="hasLog(event)" class="ev-log" :disabled="tailLoading" @click="openLog">
              {{ tailLoading ? 'fetching…' : 'log' }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>

    <p v-if="tailError" class="dt-note">{{ tailError }}</p>

    <div v-if="tail" class="ev-tail">
      <h3>
        <span>Worker log</span>
        <span class="mono dim">{{ tail.node }}</span>
        <span v-if="tail.role" class="dim">{{ tail.role }}</span>
        <span class="mono dim">{{ tail.at }}</span>
        <button class="ev-log" @click="closeLog">close</button>
      </h3>

      <!-- Said out loud, because the alternative is an operator reading a partial log as a complete
           one. The *head* is what went: a worker's fatal line is its last, in both failure shapes. -->
      <p v-if="tail.truncated" class="dt-note">
        Earlier output was discarded to fit the bound — this is the end of a longer log.
      </p>

      <pre class="dt-raw">{{ tail.text }}</pre>
    </div>
  </section>
</template>

<style scoped>
.dim { color: var(--fg-dim); }

/* No header row: four columns of a log read as a log, and a `TIME SEVERITY SUBJECT DETAIL` band on
   top of a six-line pane is furniture rather than information. */
.ev-table td { border-bottom: none; padding-top: 1px; padding-bottom: 1px; }

.ev-time { color: var(--fg-faint); }

/* The severity palette, and the *only* colour carrying a severity. `info` is deliberately quiet:
   designed behaviour — an idle teardown, a queued start, a producer stopping — arrives here, and a
   log where routine activity reads as a finding is the board §11 avoids twice. */
.ev-severity { text-transform: lowercase; }
.ev-info    .ev-severity { color: var(--fg-faint); }
.ev-warn    .ev-severity { color: var(--s-establishing); }
.ev-error   .ev-severity { color: var(--s-failed); }

/* The message is the emitter's own prose and is better than anything this UI would write. */
.ev-message { color: var(--fg); }
.ev-info .ev-message { color: var(--fg-dim); }

.ev-count { color: var(--s-degraded); margin-left: 10px; }

.ev-log {
  background: none;
  border: 1px solid var(--line);
  border-radius: 3px;
  color: var(--fg-dim);
  cursor: pointer;
  font: inherit;
  font-size: 11px;
  padding: 0 6px;
  margin-left: 10px;
}

.ev-log:hover:not(:disabled) { color: var(--fg); border-color: var(--fg-faint); }
.ev-log:disabled { cursor: default; opacity: 0.6; }

.aged { color: var(--fg-faint); font-weight: 400; text-transform: none; letter-spacing: 0; }

.ev-tail { margin-top: 10px; }

.ev-tail h3 {
  display: flex;
  align-items: baseline;
  gap: 10px;
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-dim);
  margin: 0 0 6px;
}

/* Whitespace preserved and wrapped rather than scrolled sideways: a fatal line is often long and a
   horizontal scrollbar is where the useful half of it goes to hide. */
.ev-tail .dt-raw { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
