<script setup lang="ts">
/**
 * `get nodes` — every node registered with the control plane.
 *
 * Registration is durable and the lease is not: a node here with no lease was not deleted and is
 * not necessarily broken, and its workers may well still be running, which is why the server freezes
 * a path touching it rather than reassigning. So `lease` is a facet rather than an alarm, and the
 * page counts registrations rather than reporting a fleet size.
 *
 * The **areas** column is what this list is for. The two grants are the whole of this project's
 * authority over a node's filesystem (architecture §10.6, §13), neither implies the other and both
 * default false — so "which node will accept a destination" is a question with an answer, and until
 * this page existed the answer took one page-load per node.
 */
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import type { Node } from '@/api/types'
import Facets from '@/components/Facets.vue'
import { grantText, pathsTouching, since } from '@/model/detail'
import type { FilterSpec } from '@/model/filters'
import { plural } from '@/model/labels'
import { namespaceOfId } from '@/model/staging'
import { nodeRoute, pathsRoute } from '@/router'
import { useCurrentStore } from '@/stores/current'
import { useFilters } from '@/stores/filters'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()
const current = useCurrentStore()

const SPEC: FilterSpec<Node> = {
  facets: [
    {
      key: 'lease',
      label: 'lease',
      title: 'Registration is durable; the lease is TTLd and separate',
      valuesOf: (node) => [node.live ? 'leased' : 'none'],
      vocabulary: ['leased', 'none'],
    },
    {
      key: 'grants',
      label: 'grants',
      title: 'A node granting write on nothing is not a destination at all',
      // Every grant the node offers anywhere, so `write` finds a node with one writable area among
      // five. `none` is a value rather than an absence for the same reason `{all: true}` is a
      // selector kind: a row that matched by having nothing would match by omission.
      valuesOf: (node) => {
        const areas = node.capabilities?.areas ?? []
        const grants = new Set<string>()
        for (const area of areas) {
          if (area.read) grants.add('read')
          if (area.write) grants.add('write')
        }
        return grants.size ? [...grants] : ['none']
      },
      vocabulary: ['read', 'write', 'none'],
    },
    {
      key: 'provider',
      label: 'fabric',
      valuesOf: (node) => (node.capabilities?.fabrics ?? []).map((fabric) => fabric.provider),
    },
  ],
  textOf: (node) => [
    node.name,
    node.instance ?? '',
    ...(node.capabilities?.areas ?? []).map((area) => `${area.name} ${area.path ?? ''}`),
    ...(node.capabilities?.fabrics ?? []).map((fabric) => `${fabric.provider} ${fabric.fabric}`),
    node.capabilities?.versions?.replicator ?? '',
  ].join(' '),
}

/** By name. There is no worst-first here: a lease is not a severity and registration order is not one either. */
const sorted = computed(() => [...fleet.nodes].sort((a, b) => a.name.localeCompare(b.name)))

const { rows, groups, text, filtered, toggle, setText, clear } = useFilters(SPEC, () => sorted.value)

const touching = computed(() => {
  const counts = new Map<string, number>()
  for (const node of fleet.nodes) counts.set(node.name, pathsTouching(node.name, fleet.paths).length)
  return counts
})

/**
 * The nodes the current namespace's requests reach, marked and never filtered.
 *
 * Computed off the path list rather than off the requests, because a request names a *selector* and
 * a node only becomes involved once something expanded onto it — which is the same reason the
 * matrix reads its axes out of the requests and its cells out of the paths.
 */
const involved = computed(() => {
  const namespace = current.namespace
  const nodes = new Set<string>()
  if (!namespace) return nodes
  for (const path of fleet.paths) {
    if (!path.requests.some((id) => namespaceOfId(id) === namespace)) continue
    nodes.add(path.source.node)
    nodes.add(path.destination.node)
  }
  return nodes
})
</script>

<template>
  <main class="ls-page">
    <header class="ls-head">
      <h1>Nodes</h1>
      <span class="ls-count-of">{{ rows.length }} of {{ plural(fleet.nodes.length, 'node') }}</span>
    </header>

    <Facets
      :groups="groups"
      :text="text"
      :filtered="filtered"
      @toggle="toggle"
      @update:text="setText"
      @clear="clear"
    />

    <p v-if="fleet.nodes.length === 0" class="ls-empty">
      No nodes registered. Nothing is running an agent against this control plane.
    </p>
    <p v-else-if="rows.length === 0" class="ls-empty">
      No node matches. {{ plural(fleet.nodes.length, 'node') }} filtered out.
    </p>

    <table v-else class="ls-table">
      <thead>
        <tr>
          <th>name</th>
          <th>lease</th>
          <th title="When the lease was taken — not the last heartbeat">acquired</th>
          <th>paths</th>
          <th>fabrics</th>
          <th>version</th>
          <th class="ls-wide">areas</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="node in rows" :key="node.name" :class="{ 'ls-mine': involved.has(node.name) }">
          <td>
            <RouterLink class="link mono" :to="nodeRoute(node.name)">{{ node.name }}</RouterLink>
          </td>

          <!-- Information, not an alarm: an expired lease is not proof this node's workers stopped,
               and the registration behind it is durable. -->
          <td :class="node.live ? 'leased' : 'unleased'">
            {{ node.live ? 'leased' : 'no lease' }}
          </td>

          <!-- No age beside it and none anywhere else: a heartbeat renews the TTL and deliberately
               writes nothing, so a healthy node can show this an hour ago and an age would read as
               staleness whatever the label said (`ui.md` §5 trap 3). -->
          <td class="ls-dim">
            <span v-if="node.last_seen">{{ since(node.last_seen) }} ago</span>
            <span v-else class="ls-faint">·</span>
          </td>

          <td>
            <RouterLink
              v-if="touching.get(node.name)"
              class="link mono"
              :to="pathsRoute({ node: node.name })"
              title="Every path with this node at either end"
            >{{ touching.get(node.name) }}</RouterLink>
            <span v-else class="ls-faint">·</span>
          </td>

          <td class="mono ls-dim">
            {{ (node.capabilities?.fabrics ?? []).map((f) => f.provider).join(' ') || '·' }}
          </td>

          <td class="mono ls-dim">{{ node.capabilities?.versions?.replicator ?? '·' }}</td>

          <td class="ls-wide">
            <span v-if="!node.capabilities?.areas?.length" class="ls-faint">
              no areas — offers no sources and accepts no destinations
            </span>
            <template v-for="(area, i) in node.capabilities?.areas ?? []" :key="area.name">
              <span v-if="i" class="sep">·</span>
              <span class="mono" :title="area.path ?? 'no path advertised'">{{ area.name }}</span>
              <span class="grant ls-faint">{{ grantText(area) }}</span>
            </template>
          </td>
        </tr>
      </tbody>
    </table>
  </main>
</template>

<style scoped>
.leased { color: var(--s-active); }

/* Not red. A node registered and not leased is the ordinary state of an agent that is restarting,
   and the health page is where it becomes a finding. */
.unleased { color: var(--s-establishing); }

/* Declared, never typed. A space written into the template between two elements is a text node
   containing a newline, and Vue's compiler condenses those away — which rendered `bulkread+write`
   on the first live page this table drew. */
.grant { margin-left: 5px; }
.sep { color: var(--fg-faint); margin: 0 6px; }
</style>
