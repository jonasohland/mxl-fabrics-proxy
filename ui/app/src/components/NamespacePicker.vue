<script setup lang="ts">
/**
 * The picker sets **the current namespace**, and that is its only rule.
 *
 * It used to be a navigation control with a null option: `⟨fleet health⟩` was how an operator left a
 * namespace. Health is a page now (`views/Health.vue`), so leaving has a destination of its own and
 * the picker becomes what its label always claimed. On a namespace-scoped route the current
 * namespace *is* the URL, so setting it navigates; on a fleet-wide one it only re-marks, and must
 * not yank an operator off the list they are reading onto a grid they did not ask for. That looks
 * like two behaviours and is one.
 *
 * It **shows every namespace's mode, always**. The mode is not a preference to be discovered on
 * hover: it decides which of two screens the workspace is, and a `shared` namespace is not the
 * matrix greyed out.
 *
 * Navigation preserves the view. Switching namespace from a claims screen lands on the next
 * namespace's claims — which is the behaviour the local `ref` used to give, kept now that the view
 * lives in the URL, so that moving through a shared namespace and back does not silently change
 * which screen is being read.
 *
 * **And the link beside it is the way in, which the rule above otherwise leaves missing.** Not
 * navigating from a fleet-wide screen is right — an operator reading the path list must not be
 * thrown onto a grid by asking which rows are theirs — but on its own it left the workspace with no
 * entry point at all from four of the five screens in the bar: the select would mark rows and offer
 * nothing onward. So the picker is a *pair*: the select says which namespace, the link goes there.
 *
 * It goes to the **bare** namespace route on purpose, which is the one that means *whichever screen
 * this namespace has*. A link that named `grid` would be wrong for half the namespaces in the list
 * and would read as a third tab beside the two the workspace already has.
 */
import { computed } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { plural } from '@/model/labels'
import type { WorkspaceView } from '@/router'
import { namespaceRoute } from '@/router'
import { useCurrentStore } from '@/stores/current'
import { useNamespaceStore } from '@/stores/namespaces'

const route = useRoute()
const router = useRouter()
const namespaces = useNamespaceStore()
const current = useCurrentStore()

/** Whether the screen in front of the operator is about one namespace. */
const scoped = computed(() => typeof route.params['namespace'] === 'string')

const view = computed<WorkspaceView | undefined>(() => {
  const value = route.params['view']
  return value === 'grid' || value === 'claims' ? value : undefined
})

function go(event: Event) {
  const name = (event.target as HTMLSelectElement).value
  current.set(name)

  // Only from a scoped screen, and always to the workspace: the route in front of the operator may
  // be a request detail, and the same request name in another namespace is a different request or
  // no request at all (`ui.md` §5 trap 8). The namespace's own screen is the honest landing point.
  if (name && scoped.value) void router.push(namespaceRoute(name, view.value))
}
</script>

<template>
  <span class="picker">
    <!-- `for` rather than a wrapping `<label>`: the group holds a link now, and an anchor inside a
         label is both invalid and unclickable — the label would swallow the click and focus the
         select instead. -->
    <label class="label" for="ns-picker">namespace</label>
    <select id="ns-picker" :value="current.namespace" @change="go">
      <!-- A real state and not a missing one: *nothing marked*. Disabled while the screen is about
           one namespace, because there is no such thing as being on a namespace's workspace and in
           no namespace — shown with the reason rather than omitted, as everything off in this app
           is. -->
      <option value="" :disabled="scoped" title="Mark nothing on the fleet-wide views">⟨none⟩</option>
      <option v-for="entry in namespaces.all" :key="entry.name" :value="entry.name">
        {{ entry.name }} ⟨{{ entry.paths ?? 'shared' }}⟩ · {{ plural(entry.requests, 'request') }}
      </option>
    </select>

    <!-- Shown and disabled with the reason rather than omitted, as everything off in this app is: a
         control that vanishes when nothing is picked produces "where did that go?", which is a worse
         question than one that says why it is off. -->
    <RouterLink
      v-if="current.namespace"
      class="open"
      :to="namespaceRoute(current.namespace)"
      title="This namespace's own screen — the grid, or the claims list, whichever its mode has"
    >workspace</RouterLink>
    <span v-else class="open off" title="Pick a namespace to open its workspace">workspace</span>
  </span>
</template>

<style scoped>
.picker {
  display: flex;
  align-items: center;
  gap: 6px;
}

/* The way into the workspace, and the reason the select does not navigate on a fleet-wide screen.
   Styled as the bar's other links are, so it reads as navigation rather than as a form control that
   happens to sit next to one. */
.open {
  color: var(--fg-dim);
  text-decoration: none;
  font-size: 12px;
}

.open:hover { color: var(--fg); }
.open.router-link-active { color: var(--accent); }
.open.off { color: var(--fg-faint); cursor: not-allowed; }

.label {
  color: var(--fg-dim);
  font-size: 12px;
}

select {
  background: var(--bg-sunken);
  color: var(--fg);
  border: 1px solid var(--line);
  border-radius: 3px;
  padding: 3px 6px;
  font: inherit;
}
</style>
