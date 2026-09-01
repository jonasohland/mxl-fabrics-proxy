<script setup lang="ts">
/**
 * The three conditions that change how everything below should be read.
 *
 * They are separate banners rather than one status line because they send an operator to three
 * different places, and two of them are not errors.
 */
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()

function ago(at: number | undefined): string {
  if (at === undefined) return 'never'
  const seconds = Math.round((Date.now() - at) / 1000)
  if (seconds < 60) return `${seconds}s ago`
  return `${Math.round(seconds / 60)}m ago`
}
</script>

<template>
  <!-- Not an error. The server has not run its first reconcile — it just started, or an HA leader
       changed — so the state below is real but not yet being acted on. -->
  <div v-if="fleet.settling" class="banner banner-note" title="No reconcile has run yet">
    <strong>Settling</strong>
  </div>

  <!-- The server is fine, its store is not. A different place to look from an unreachable server,
       which is why it is not folded into the stale banner. -->
  <div v-else-if="fleet.storeUnreachable" class="banner banner-bad" title="The server is running but cannot reach its store">
    <strong>Store unreachable</strong>
    <span class="detail">last read {{ ago(fleet.lastGoodAt) }}</span>
  </div>

  <!-- A failed poll changes nothing on screen: the last good read stays rendered. An unreachable
       server must not look like an empty fleet. -->
  <div v-else-if="fleet.stale" class="banner banner-warn">
    <strong>Stale</strong>
    <span class="detail">last read {{ ago(fleet.lastGoodAt) }} · {{ fleet.lastError?.message }}</span>
  </div>
</template>

<style scoped>
.banner {
  padding: 8px 14px;
  border-bottom: 1px solid var(--line);
  font-size: 12px;
}

.banner-note { background: #1b2530; color: var(--fg); }
.banner-warn { background: #2e2413; color: #f0d9a8; }
.banner-bad  { background: #2e1a1a; color: #f0c0c0; }

.detail {
  color: var(--fg-dim);
  margin-left: 6px;
}
</style>
