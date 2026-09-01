<script setup lang="ts">
/**
 * A `shared` namespace is a ledger, not a board (`ui.md` §7c).
 *
 * The matrix renders **intents**, deduplicated by nothing. This renders **claims** — the triple
 * `(request, source entry, path)` — over the path list the server has already deduplicated. All
 * three of the grid's lies go out with the framing rather than being handled: one path is one row so
 * nothing is double-counted, the counts sum because the rows are the real edges, and "un-lighting
 * stops nothing" becomes visible as a refcount that stays above zero.
 *
 * It is a **read** view and offers no cell-shaped gesture. The desired set here is a population of
 * independently-owned intents, mostly written by something that is not this UI — one request per pod
 * is the case the mode exists for.
 *
 * Nothing in it requires `shared`. Inside an `exclusive` namespace every in-namespace refcount is 1
 * by the rule, so the claims list degenerates into a plain path list — which is exactly what
 * `describe path` gives and is still worth having. One component, both modes.
 */
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import type { Path } from '@/api/types'
import Rectangle from '@/components/Rectangle.vue'
import { plural, shortId, sourceRef } from '@/model/labels'
import { buildLedger, needsReading } from '@/model/ledger'
import { domainRoute, flowRoute, nodeRoute, pathRoute, requestRoute } from '@/router'
import { useFleetStore } from '@/stores/fleet'
import { useNamespaceStore } from '@/stores/namespaces'

const props = defineProps<{ namespace: string }>()

const fleet = useFleetStore()
const namespaces = useNamespaceStore()

const mode = computed(() => namespaces.mode(props.namespace))

const ledger = computed(() => buildLedger(fleet.paths, fleet.requests, props.namespace))

const pathsById = computed(() => new Map(fleet.paths.map((path) => [path.id, path])))

/**
 * Opens summarised, in the landing page's idiom: the counts line, then only the paths that are not
 * `ACTIVE`. Same instinct as `status` naming only what is not active, applied one level down, and
 * the reason this view scales where a grid of the same cardinality does not.
 */
const onlyReading = ref(true)

const groups = computed(() =>
  ledger.value.groups
    .map((group) => ({
      ...group,
      paths: onlyReading.value ? group.paths.filter(needsReading) : group.paths,
    }))
    .filter((group) => group.paths.length > 0),
)

const hidden = computed(() => ledger.value.pathCount - groups.value.reduce((n, g) => n + g.paths.length, 0))

/** Expanded by default where the rectangle is the only thing that can say anything: a parked leg. */
function startsOpen(request: { destinations: { disabled?: boolean }[] }): boolean {
  return request.destinations.some((destination) => destination.disabled === true)
}

const expanded = ref(new Set<string>())

function toggle(id: string, request: { destinations: { disabled?: boolean }[] }) {
  const set = new Set(expanded.value)
  const open = set.has(id) || (!set.has(`!${id}`) && startsOpen(request))
  set.delete(id)
  set.delete(`!${id}`)
  set.add(open ? `!${id}` : id)
  expanded.value = set
}

function isOpen(id: string, request: { destinations: { disabled?: boolean }[] }): boolean {
  if (expanded.value.has(id)) return true
  if (expanded.value.has(`!${id}`)) return false
  return startsOpen(request)
}

function pathTitle(path: Path): string {
  return path.reason ? `${path.state} · ${path.reason}` : path.state
}
</script>

<template>
  <main class="page">
    <header class="head">
      <h1 class="mono">{{ namespace }}</h1>
      <span class="mode" :class="mode">⟨{{ mode }}⟩</span>

      <span class="counts">
        {{ plural(ledger.requests.length, 'request') }} · {{ plural(ledger.pathCount, 'path') }} ·
        <span :class="{ attention: ledger.notActiveCount > 0 }">
          {{ ledger.notActiveCount }} not active
        </span>
      </span>

      <span class="spacer" />

      <label class="filter">
        <input v-model="onlyReading" type="checkbox" />
        only what needs reading
      </label>
    </header>

    <section class="claims">
      <h2>Claims</h2>

      <p v-if="groups.length === 0" class="dim">
        <template v-if="ledger.pathCount === 0">No paths.</template>
        <template v-else>Nothing needs reading.</template>
      </p>

      <div v-for="group in groups" :key="group.key" class="group">
        <!-- Every identifier on this screen is a link into `describe`, and each one keeps its grid
             track: an anchor is a grid item exactly as the span it replaced was. `.link` inherits
             its colour at rest, so the state colours that *are* the information stay the only
             colours on the row (`base.css`). -->
        <div class="group-head">
          <RouterLink class="mono col dst-node link" :to="nodeRoute(group.node)">
            {{ group.node }}
          </RouterLink>
          <RouterLink class="mono col dst-domain link" :to="domainRoute(group.node, group.domain)">
            {{ group.domain }}
          </RouterLink>
          <span class="dim col count">{{ plural(group.paths.length, 'path') }}</span>
          <!-- Namespaces partition requests, not destinations. Another namespace writing into this
               domain is ordinary fan-in, and it is the fact that changes what emptying this would
               mean — dropping every claim here would not empty the domain. A view of one namespace
               cannot see it any other way. -->
          <span
            v-if="group.foreignNamespaces.length"
            class="col foreign"
            :title="`also written into by: ${group.foreignNamespaces.join(', ')}`"
          >
            + {{ group.foreignNamespaces.join(', ') }}
          </span>
        </div>

        <div
          v-for="entry in group.paths"
          :key="entry.path.id"
          class="path"
          :class="[`mark-${entry.path.state}`, { attention: entry.path.state !== 'ACTIVE' }]"
        >
          <div class="path-line">
            <span class="arrow">←</span>
            <RouterLink
              class="mono col node link"
              :to="nodeRoute(entry.path.source.node)"
              :title="entry.path.source.node"
            >
              {{ entry.path.source.node }}
            </RouterLink>
            <RouterLink
              class="mono col domain link"
              :to="domainRoute(entry.path.source.node, entry.path.source.domain)"
              :title="entry.path.source.domain"
            >
              {{ entry.path.source.domain }}
            </RouterLink>
            <RouterLink
              class="mono col flow link"
              :to="flowRoute(entry.path.source.flow)"
              :title="entry.path.source.flow"
            >
              {{ shortId(entry.path.source.flow) }}
            </RouterLink>
            <!-- The state word is the way into the path itself: it is what the operator is already
                 looking at when they decide they need the session behind it. -->
            <RouterLink
              class="col state state-link"
              :class="`state-${entry.path.state}`"
              :to="pathRoute(entry.path.id)"
              :title="pathTitle(entry.path)"
            >
              {{ entry.path.state }}
            </RouterLink>
            <span class="col held">held by {{ entry.heldBy }}</span>
            <span class="reason">{{ entry.path.reason }}</span>
          </div>

          <!-- Every claim carries the selector that made it. A path with two claims raises exactly
               one question — why do I have two of these — and this is the whole answer. The wrapper
               draws the rule that ties them to the path above. -->
          <div class="claims-list">
            <div v-for="claim in entry.claims" :key="claim.requestId" class="claim">
              <RouterLink class="mono col name link" :to="requestRoute(claim.requestId)" :title="claim.requestId">
                {{ claim.requestId }}
              </RouterLink>
              <span class="dim col ref">
                {{ claim.sourceIndex >= 0 ? sourceRef(claim.sourceIndex) : '·' }}
              </span>
              <span class="selector">{{ claim.selector }}</span>
            </div>
          </div>
        </div>
      </div>

      <p v-if="hidden > 0" class="dim hidden-note">{{ plural(hidden, 'path') }} hidden</p>
    </section>

    <section class="requests">
      <h2>Requests</h2>

      <div v-for="entry in ledger.requests" :key="entry.request.id" class="request">
        <div class="request-line">
          <button class="disclosure" @click="toggle(entry.request.id, entry.request)">
            {{ isOpen(entry.request.id, entry.request) ? '▾' : '▸' }}
          </button>
          <RouterLink class="mono col name link" :to="requestRoute(entry.request.id)" :title="entry.request.id">
            {{ entry.request.name }}
          </RouterLink>
          <span class="col state" :class="`state-${entry.request.status.state}`">
            {{ entry.request.status.state }}
          </span>
          <span class="col tally">
            {{ plural(entry.paths, 'path') }} · {{ entry.sole }} sole · {{ entry.shared }} shared
          </span>
          <!-- No sole paths while holding some: nothing is broken and nothing is doubled, but
               somebody wrote an intent entirely subsumed by another. It has no other symptom, so
               the label stays; what it means is training, and lives in the tooltip. -->
          <span v-if="entry.ridesAlong" class="rides" title="No sole paths. Deleting it stops nothing.">
            rides along
          </span>
          <!-- Server prose of any length in the one track that absorbs the slack, so it takes the
               same clipping every other item on this row has. Left unclipped it wrapped and grew the
               row, and only in some engines: 38px in Chrome against 19px in Firefox for one reason
               string. `.col`'s own note is the rule — a row that wraps stops being a row. -->
          <span v-else-if="entry.paths === 0" class="col dim">{{ entry.request.status.reason }}</span>
        </div>

        <div v-if="isOpen(entry.request.id, entry.request)" class="rect-wrap">
          <Rectangle :request="entry.request" :paths-by-id="pathsById" />
        </div>
      </div>
    </section>
  </main>
</template>

<style scoped>
.page {
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 18px;
  overflow: auto;
}

.head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}

h1 { font-size: 15px; margin: 0; }

.mode { color: var(--fg-dim); font-size: 12px; }
.mode.exclusive { color: var(--accent); }

.counts { color: var(--fg-dim); font-size: 12px; }
.attention { color: var(--s-establishing); }

h2 {
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--fg-dim);
  margin: 0 0 8px;
}

.group {
  margin-bottom: 16px;
  border-left: 2px solid var(--line);
  padding-left: 14px;
}

/* Shares `.path-line`'s tracks, with the arrow column left empty, so the destination node sits
 * exactly above the source nodes it receives from. A header whose stops are *nearly* those of the
 * rows beneath it reads as a mistake; either it lines up or it is plainly somewhere else. */
.group-head {
  display: grid;
  grid-template-columns: 2ch 11ch 20ch 10ch 1fr;
  column-gap: 12px;
  align-items: baseline;
  padding: 2px 0 5px 8px;
  margin: 0 0 2px -8px;
  border-bottom: 1px solid var(--line);
}

.group-head .dst-node { grid-column: 2; }

.dst-node, .dst-domain { font-weight: 600; }
.count { font-size: 12px; }
.foreign { color: var(--s-paused); font-size: 12px; }

/* Each path is a block: its own row plus the claims holding it. The gutter is reserved on every
   one and filled only where there is something to mark, so the accent cannot change the layout it
   appears in. */
.path {
  padding: 4px 0 4px 8px;
  margin-left: -8px;
}

.path + .path { border-top: 1px solid var(--line-soft); }

/* Marked in the path's **own state colour**, so the gutter can be read as a distribution rather
   than as a row of identical alarms. PAUSED comes up calm and blue where FAILED comes up red, which
   is the distinction §11 exists to preserve — a path nobody is producing into has a different owner
   from a broken one, and folding them into one warning colour destroys that.
   A path held by several requests is *not* marked: in a shared namespace that is refcounting
   working as designed, not a condition to address. */
.path.attention {
  box-shadow: inset 2px 0 0 var(--mark);
  background: color-mix(in srgb, var(--mark) 4%, transparent);
}

.path-line {
  display: grid;
  grid-template-columns: 2ch 11ch 20ch 10ch 13ch 11ch 1fr;
  column-gap: 12px;
  align-items: baseline;
}

/* The state word links to its path, and hovering it must not repaint it: the colour *is* the
   reading, and `.link:hover`'s accent would trade it for a hint that the row is clickable. Underline
   only, and the state keeps its own colour. */
.state-link { color: inherit; text-decoration: none; }
.state-link:hover { text-decoration: underline; }

/* Clipped rather than wrapped: a row that wraps stops being a row. */
.col {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.arrow { color: var(--fg-faint); }
.state {
  font-size: 11px;
  letter-spacing: 0.04em;
  border-left: 2px solid currentColor;
  padding-left: 7px;
}
.held { color: var(--fg-dim); font-size: 12px; font-variant-numeric: tabular-nums; }
.reason { color: var(--fg-faint); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

/* The rule that ties a path's claims to it — drawn rather than implied by indentation alone, which
   is the difference between a list and a tree. It also carries the indent the claims used to hold
   themselves, so the columns below still line up with the path row above. */
.claims-list {
  margin-left: 1ch;
  border-left: 1px solid var(--line);
  padding-left: calc(4ch + 11px);
}

/* Claims share the path row's column stops rather than inventing their own.
 *
 * The tracks are chosen so `sources[i]` lands under the flow and the selector under the state: the
 * path reaches its flow column at `2ch + 11ch + 20ch + 3×gap`, and a claim reaches its second track
 * at the same distance once `.claims-list`'s inset is counted.
 *
 * **The font size must match `.path-line`'s**, because a `ch` resolves against the element that
 * uses it — a 12px claim row would compute narrower tracks and drift out of alignment by a few
 * pixels per column. Hierarchy comes from the rule, the indent and the colour instead. */
.claim {
  display: grid;
  grid-template-columns: calc(28ch + 12px) 10ch 1fr;
  column-gap: 12px;
  padding: 1px 0;
  position: relative;
}

/* A tick from the rule to each claim, so a claim is attached rather than merely near. */
.claim::before {
  content: '';
  position: absolute;
  left: calc(-4ch - 11px);
  top: 0.62em;
  width: calc(3ch);
  border-top: 1px solid var(--line);
}

.claim .name { color: var(--accent); }
.claim .ref { font-size: 12px; }
.claim .selector { color: var(--fg-dim); font-size: 12px; }

.hidden-note { margin: 4px 0 0; font-size: 12px; }

.request { padding: 2px 0; border-bottom: 1px solid var(--line-soft); }

.request-line {
  display: grid;
  grid-template-columns: 2ch 26ch 13ch 30ch 1fr;
  column-gap: 12px;
  align-items: baseline;
}

.tally { color: var(--fg-dim); font-size: 12px; font-variant-numeric: tabular-nums; }
.rides { color: var(--s-paused); font-size: 12px; }

.disclosure {
  background: none;
  border: none;
  color: var(--fg-dim);
  cursor: pointer;
  padding: 0;
  width: 14px;
  font: inherit;
}

/* Tied to its request the same way a claim is tied to its path, so an expanded rectangle reads as
   belonging to the row above rather than floating between two of them. */
.rect-wrap {
  margin: 4px 0 8px 1ch;
  border-left: 1px solid var(--line);
  padding: 2px 0 2px calc(2ch + 11px);
}

button.link {
  background: none;
  border: none;
  color: var(--accent);
  cursor: pointer;
  padding: 0;
  font: inherit;
}

.filter { color: var(--fg-dim); font-size: 12px; display: flex; gap: 5px; align-items: center; }
.dim { color: var(--fg-dim); }
.ok { color: var(--s-active); }
.spacer { flex: 1; }
</style>
