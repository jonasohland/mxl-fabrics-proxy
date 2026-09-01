<script setup lang="ts">
/**
 * The pending bar: the staged set, what it would do, and the one button that does it.
 *
 * **This is the confirmation, and it is the only one.** `ui.md` §7a: a dialog that appears
 * mid-gesture gets dismissed reflexively, while a staged change has to be read and applied — so the
 * weight goes here, where it can be specific. "3 of 4 paths stop" is worth reading; "are you sure?"
 * is not, and it is what apply-on-click would need in front of every toggle.
 *
 * Everything of consequence on this bar comes from the batch dry run and the live path list rather
 * than from anything the UI works out: the outcome header the server itself would return, its refusal
 * prose verbatim, and the refcount that decides whether a leg leaving this request stops any media at
 * all. The UI computes none of the expansion, which is exactly what the dry run is for.
 *
 * It is rendered at the application's edge rather than inside a view, because the staged set is
 * fleet-wide: an edit carries its namespace in its request id, and a bar that lived on one screen
 * would discard work the moment the operator navigated.
 */
import { computed } from 'vue'

import { renderDomain } from '@/api/types'
import { plural, selectorLabel, sourceLabel } from '@/model/labels'
import type { Edit } from '@/model/staging'
import { useStagingStore } from '@/stores/staging'

const staging = useStagingStore()

/**
 * What the edit is *about*, in the vocabulary the screen it came from uses.
 *
 * A leg reads as its column and a source reads as the row header's own two facts, so an operator can
 * find the thing the line is talking about without translating.
 */
const subjectOf = (edit: Edit) =>
  edit.target === 'leg'
    ? `${edit.destination.node} ${renderDomain(edit.destination.domain)}`
    : `${sourceLabel(edit.source)} ${selectorLabel(edit.source.select)}`

/**
 * The one line an operator reads before pressing Apply.
 *
 * Media first, because that is the irreversible half: a path stopping is a worker pair torn down and
 * a flag coming back does not bring it with it.
 */
const summary = computed(() => {
  if (staging.previewing) return 'previewing…'
  if (!staging.previewed) return 'preview pending'

  let stops = 0
  let rides = 0
  let hands = 0
  let starts = 0
  let joins = 0
  for (const entry of staging.staged) {
    const radius = staging.radius.get(entry.id)
    if (!radius) continue
    stops += radius.stops
    rides += radius.ridesAlong.length
    hands += radius.handedOff.length
    starts += radius.starting.length
    joins += radius.joining.length
  }

  const parts: string[] = []
  if (stops > 0) parts.push(`${plural(stops, 'path')} stop`)
  // A path leaving one request because another in this set is taking it over. Counted apart from
  // both the stops and the starts, because it is neither: nothing tears down and nothing comes up.
  if (hands > 0) parts.push(`${plural(hands, 'path')} change hands`)
  // A leg leaving a request stops nothing while another request still holds it — the refcount says
  // so at no cost, and it is the difference between a teardown and a bookkeeping change.
  if (rides > 0) parts.push(`${rides} ride${rides === 1 ? 's' : ''} along`)
  if (starts > 0) parts.push(`${plural(starts, 'path')} start`)
  // Not a start: the edge already exists under somebody else's claim, so this adds a reference to it
  // and materialises nothing. Ordinary across namespaces, which partition requests and not
  // destinations.
  if (joins > 0) parts.push(`${joins} joins an existing path`)
  return parts.length ? parts.join(' · ') : 'no path changes'
})

const applyTitle = computed(() => {
  if (staging.applying) return 'Applying…'
  if (staging.blocked) return 'A change was refused. Discard it.'
  if (!staging.previewed) return 'Waiting for the dry run.'
  return 'One POST per request. Nothing is written yet.'
})
</script>

<template>
  <section v-if="staging.staged.length" class="pending">
    <header class="head">
      <!-- A draft with no edits is one change: `duplicate` and `split` author the request itself,
           and there is no leg edit to count. -->
      <span class="count">{{ plural(staging.changes, 'change') }} staged</span>
      <span class="summary" :class="{ heavy: !staging.previewing && staging.previewed }">
        {{ summary }}
      </span>
      <span class="spacer" />
      <span v-if="staging.applyError" class="failure" :title="staging.applyError">
        {{ staging.applyError }}
      </span>
      <button class="act" :disabled="staging.applying" @click="staging.discardAll()">discard all</button>
      <button class="act go" :disabled="!staging.canApply" :title="applyTitle" @click="staging.apply()">
        {{ staging.applying ? 'applying…' : 'apply' }}
      </button>
    </header>

    <ul class="list">
      <li v-for="entry in staging.staged" :key="entry.id" class="entry">
        <div class="line">
          <span class="mono id">{{ entry.id }}</span>

          <!-- The server's own word for what this write is. The status code cannot tell you: an
               unchanged apply is still a 200, so the header is the honest answer. A commit that has
               become a `DELETE` has no such word and needs none — the emptied spec says it. -->
          <span v-if="entry.deleting" class="outcome deleting"
                title="Nothing left in the spec, so the commit is a DELETE">
            delete
          </span>
          <span v-else class="outcome" :class="staging.previews.get(entry.id)?.outcome">
            {{ staging.previews.get(entry.id)?.outcome ?? '·' }}
          </span>

          <!-- Rendered verbatim: a refusal carries its own fix in the prose, and it is better than
               anything this screen would write. `reason_code` decides only what to highlight. -->
          <span v-if="staging.previews.get(entry.id)?.error" class="failure">
            {{ staging.previews.get(entry.id)?.error }}
          </span>
          <!-- A dry run reconciles a candidate fleet with *one* request changed, so a split previews
               as an overlap it is itself about to resolve: the new request loses the contest for
               being the newer stamp while the update that gives the paths up is staged beside it.
               Said as the hand-off it is, rather than as a refusal it is not. -->
          <span v-else-if="staging.handoffs.get(entry.id)" class="handoff">
            takes {{ plural(staging.handoffs.get(entry.id)!.paths.length, 'path') }} over from
            {{ staging.handoffs.get(entry.id)!.from.join(', ') }} when this set is applied
          </span>
          <span v-else-if="staging.radius.get(entry.id)?.holdsNothing" class="failure">
            holds none of the paths it lists
          </span>
          <span v-else class="effect">
            <!-- The other side of a split: these leave this request and keep running, because
                 something else in this set takes them over. "2 of 2 paths stop" is true of this
                 write alone and false of the set it is in. -->
            <template v-if="(staging.radius.get(entry.id)?.handedOff.length ?? 0) > 0">
              {{ plural(staging.radius.get(entry.id)!.handedOff.length, 'path') }} change hands<template
                v-if="staging.radius.get(entry.id)!.stops > 0"
              >, {{ staging.radius.get(entry.id)!.stops }} stop</template>
            </template>
            <template v-else-if="(staging.radius.get(entry.id)?.stopping.length ?? 0) > 0">
              {{ staging.radius.get(entry.id)!.stops }} of
              {{ plural(staging.radius.get(entry.id)!.stopping.length, 'path') }} stop
            </template>
            <template v-else-if="(staging.radius.get(entry.id)?.starting.length ?? 0) > 0">
              {{ plural(staging.radius.get(entry.id)!.starting.length, 'path') }} start
            </template>
            <template v-else-if="entry.deleting || staging.previews.get(entry.id)">
              no path changes
            </template>
          </span>

          <span class="spacer" />
          <button class="drop" title="Discard every staged change to this request"
                  @click="staging.discardRequest(entry.id)">discard</button>
        </div>

        <div class="legs">
          <!-- A request authored whole — `duplicate` or `split` — has no leg edits to list, because
               the request itself is the change. Its shape is what there is to read. -->
          <span v-if="entry.edits.length === 0 && entry.spec" class="leg add">
            <span class="verb">create</span>
            <span class="mono">
              {{ plural(entry.spec.sources.length, 'source') }} ×
              {{ plural(entry.spec.destinations.length, 'destination') }}
            </span>
          </span>
          <span v-for="edit in entry.edits" :key="subjectOf(edit)" class="leg" :class="staging.verbOf(edit)">
            <span class="verb">{{ staging.verbOf(edit) }}</span>
            <span class="mono">{{ subjectOf(edit) }}</span>
            <button class="x" title="Discard this one change" @click="staging.discard(edit)">×</button>
          </span>
        </div>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.pending {
  flex: 0 0 auto;
  border-top: 1px solid var(--pending);
  background: var(--bg-raised);
  max-height: 34vh;
  overflow: auto;
}

.head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 6px 14px;
  position: sticky;
  top: 0;
  background: var(--bg-raised);
  border-bottom: 1px solid var(--line-soft);
}

.count { color: var(--pending); font-weight: 600; font-size: 12px; }
.summary { color: var(--fg-dim); font-size: 12px; }
.summary.heavy { color: var(--fg); }

.act {
  background: none;
  border: 1px solid var(--line);
  border-radius: 3px;
  color: var(--fg-dim);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  padding: 2px 12px;
}

.act:hover:not(:disabled) { color: var(--fg); border-color: var(--fg-dim); }
.act:disabled { color: var(--fg-faint); border-color: var(--line-soft); cursor: not-allowed; }

.go { border-color: var(--pending); color: var(--pending); }
.go:hover:not(:disabled) { background: var(--pending); color: var(--bg); border-color: var(--pending); }

.list { list-style: none; margin: 0; padding: 0; }

.entry {
  padding: 4px 14px 6px;
  border-bottom: 1px solid var(--line-soft);
}

.line { display: flex; align-items: baseline; gap: 10px; font-size: 12px; }
.spacer { flex: 1; }

.id { color: var(--accent); }

.outcome { color: var(--fg-faint); font-size: 11px; letter-spacing: 0.04em; }
.outcome.created { color: var(--s-new); }
.outcome.updated { color: var(--s-establishing); }
.outcome.unchanged { color: var(--fg-faint); }
.outcome.deleting { color: var(--s-failed); }

.effect { color: var(--fg-dim); }
/* Not a failure and not nothing: the paths keep running and change hands. */
.handoff { color: var(--s-paused); }
.failure { color: var(--s-invalid); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.drop {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  font-size: 11px;
  padding: 0;
}

.drop:hover { color: var(--s-failed); }

.legs { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 3px; }

.leg {
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  border: 1px solid var(--line);
  border-radius: 3px;
  padding: 1px 4px 1px 6px;
  font-size: 11px;
  color: var(--fg-dim);
}

/* Each verb in the colour of what it produces: parking reaches `DISABLED`, and lighting a leg
   reaches a live path. The palette is the same one the grid uses, so the bar and the cell it came
   from read as one statement. */
.leg .verb { letter-spacing: 0.04em; }
.leg.park .verb { color: var(--s-disabled); }
.leg.enable .verb, .leg.add .verb { color: var(--s-new); }
.leg.remove .verb { color: var(--s-failed); }

.x {
  background: none;
  border: 0;
  color: var(--fg-faint);
  cursor: pointer;
  font: inherit;
  line-height: 1;
  padding: 0 2px;
}

.x:hover { color: var(--s-failed); }
</style>
