<script setup lang="ts">
/**
 * One request's sources × destinations grid.
 *
 * The ledger needs this because a parked destination produces no path, so it has no claim to appear
 * as, and a view built only from `/v1/paths` would render a switched-off leg identically to one
 * that was never written. The rectangle is where `disabled` is drawable.
 *
 * **Geometry is a correctness property here, not styling** (`ui.md` §7a). A cell is always exactly
 * two lines — a state word and a count — and nothing of variable length ever goes in the box. The
 * reason is prose of any length, cells in a row share a height, and a grid that reflows when one
 * leg starts explaining itself has stopped being a grid. The reason goes in the tooltip, where its
 * length costs nothing.
 */
import { computed } from 'vue'

import type { Path, Request } from '@/api/types'
import { buildRectangle } from '@/model/rectangle'

const props = defineProps<{ request: Request; pathsById: Map<string, Path> }>()

const rect = computed(() => buildRectangle(props.request, props.pathsById))

function line2(cell: { parked: boolean; count: number; state: unknown }): string {
  if (cell.parked) return 'parked'
  if (cell.count === 0) return '·'
  return cell.count === 1 ? '1 path' : `${cell.count} paths`
}

function tooltip(cell: { parked: boolean; paths: Path[] }): string {
  if (cell.parked) return 'Parked. The entry is in the spec and expands to nothing.'
  if (cell.paths.length === 0) return 'No paths yet. The selectors match nothing here.'
  return cell.paths
    .map((path) => `${path.id.slice(0, 8)}… ${path.state}${path.reason ? ` · ${path.reason}` : ''}`)
    .join('\n')
}
</script>

<template>
  <table class="rect">
    <thead>
      <tr>
        <th class="corner"></th>
        <th v-for="column in rect.columns" :key="column.index" :class="{ parked: column.parked }">
          <span class="line">{{ column.node }}</span>
          <span class="line mono">{{ column.domain }}</span>
        </th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="row in rect.rows" :key="row.index">
        <th class="rowhead">
          <span class="line">{{ row.source.node }}</span>
          <span class="line mono dim">{{ row.domain }}</span>
          <span class="line dim">{{ row.selector }}</span>
        </th>
        <td v-for="column in rect.columns" :key="column.index">
          <div
            class="cell"
            :class="{ parked: rect.cells[row.index]![column.index]!.parked }"
            :title="tooltip(rect.cells[row.index]![column.index]!)"
          >
            <span class="line" :class="`state-${rect.cells[row.index]![column.index]!.state ?? 'none'}`">
              {{ rect.cells[row.index]![column.index]!.state ?? '·' }}
            </span>
            <span class="line dim">{{ line2(rect.cells[row.index]![column.index]!) }}</span>
          </div>
        </td>
      </tr>
    </tbody>
  </table>
</template>

<style scoped>
/* Fixed layout, so every column is the same width whatever lands in it.
 *
 * Left to auto-size, a column holding `DISABLED`/`parked` comes out narrower than one holding
 * `ESTABLISHING`/`2 paths`, and the grid develops a ragged edge that tracks its contents. `ui.md`
 * §7a makes that a correctness property rather than styling: a cell's geometry must not depend on
 * its content, because cells in a row share a height and columns share a width, so one cell that
 * changes shape moves the whole grid under the pointer. */
.rect {
  border-collapse: collapse;
  table-layout: fixed;
  font-size: 12px;
}

.corner, .rowhead { width: 26ch; }
thead th:not(.corner) { width: 16ch; }

th, td {
  border: 1px solid var(--line-soft);
  padding: 0;
  vertical-align: top;
}

/* No border and no fill: an empty corner that is drawn reads as a cell with nothing in it. */
.corner { border: none; background: transparent; }

/* Every line starts at the same x. Set explicitly because a cell's contents are otherwise centred
   by inherited table alignment, which makes an applied cell and a parked one look misaligned. */
.line {
  display: block;
  text-align: left;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

thead th, .rowhead, .cell {
  padding: 4px 8px;
  font-weight: 400;
}

thead th { background: var(--bg-raised); }

/* A parked column is drawn, not blanked: an unlit cell says "nobody ever routed this" and a parked
   one says "somebody did and switched it off". Different sentences, read at a glance. */
thead th.parked { color: var(--s-disabled); }

.rowhead {
  background: var(--bg-raised);
  text-align: left;
}

/* The cell fills its box. A row is as tall as its header — three fixed lines — and a cell is two, so
   a cell sized by its own content leaves a strip of every row undrawn and reads as a chip floating in
   the field rather than as the field.
 *
 * Inset rather than `height: 100%`: a percentage height against a table cell has no portable
 * resolution — Firefox resolves it against whatever `height` the `td` declares rather than against the
 * cell's stretched height, which turns the fix into a cell the height of its own padding. The
 * `min-height` keeps the row from depending on content that is now out of flow. */
tbody td { position: relative; min-height: 42px; min-height: calc(2lh + 8px); }
.cell { background: var(--bg-sunken); position: absolute; inset: 0; }
.cell.parked { background: transparent; }

.dim { color: var(--fg-dim); }
.state-none { color: var(--fg-faint); }
</style>
