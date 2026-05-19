<script setup lang="ts">
// Geofence alert toaster (TASK-020).
//
// Watches `useFleetStore().alerts` (a bounded ring populated by the WS
// dispatcher in stores/fleet.ts) and fires a Sonner toast per new entry.
// Renders no visible DOM — the toast layer (mounted in layouts/default.vue)
// is the actual surface; this component is just the bridge.
//
// De-dup strategy
//
//   The alerts array is a ring buffer kept at 50 entries (ALERT_RING_SIZE
//   in fleet.ts). New alerts arrive at index 0 via `[new, ...prev.slice(0,
//   49)]`. The watcher fires on every array replacement, so without a seen
//   set we would re-toast every existing entry on each new push. Tracking
//   composite keys (vehicle_id + alert_type + at) sidesteps that — the `at`
//   timestamp is unique per backend emit so two enter events 1s apart on
//   the same vehicle each produce their own toast, as expected.
//
//   The Set grows unbounded across a session but is naturally capped: each
//   entry is a short string, and the dashboard is a short-lived surface
//   (managers log out / refresh more often than they'd accumulate 10k+
//   alerts). If that ever changes, we can cap the Set by mirroring the
//   ring's 50-entry bound.
//
// Why a "sr-only" span instead of `null`?
//
//   Vue 3 single-file components require a non-empty <template>. An
//   sr-only span is invisible to sighted users but readable by screen
//   readers — gives the bridge component a stable semantic anchor in the
//   accessibility tree without bloating the visual DOM.

import { toast } from 'vue-sonner'

const fleet = useFleetStore()

interface AlertEntry {
  vehicle_id: string
  alert_type: 'enter' | 'exit'
  at: number
}

// Composite key: a single `at` ms collides only if the backend emits two
// alerts of the same type for the same vehicle at the same millisecond,
// which the usecase prevents via the previous-state comparison.
const seen = new Set<string>()

function key(a: AlertEntry): string {
  return `${a.vehicle_id}|${a.alert_type}|${a.at}`
}

watch(
  () => fleet.alerts,
  (list) => {
    for (const a of list) {
      const k = key(a)
      if (seen.has(k)) continue
      seen.add(k)
      const verb = a.alert_type === 'enter' ? 'entered' : 'left'
      toast.info(`Vehicle ${a.vehicle_id} ${verb} its geofence`, {
        duration: 5000,
      })
    }
  },
  // shallow watch is fine — fleet.ts replaces the array reference on every
  // new alert, so we never need deep tracking inside the entries.
  { deep: false },
)
</script>

<template>
  <span class="sr-only">Geofence alerts</span>
</template>