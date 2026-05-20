<script setup lang="ts">
// Dashboard home — the live-fleet surface.
//
// Owns three pieces:
//   1. <MapView> bound to fleet.positions (live, WS-driven)
//   2. <LiveBadge> reflecting fleet.status (Live / Reconnecting / Offline)
//   3. A manager-only vehicle list sidebar (the driver dashboard is the same
//      map without the sidebar — drivers don't need to see fleet roster
//      from this page)
//
// Lifecycle: this page is the long-lived owner that calls fleet.connect()
// on mount and fleet.disconnect() on unmount. Per the fleet store doc
// comment, that pattern guards against premature teardown — short-lived
// children must NOT be the first consumer of useFleetStore().
//
// API contract: backend wraps list responses as `{ vehicles: [...] }`
// (see backend/internal/handler/vehicle_handler.go:vehicleListBody), so the
// fetch unwraps once and stores the bare array.

import type { Vehicle, Position } from '~~/shared/types/domain'

definePageMeta({ layout: 'default' })
useHead({ title: 'Dashboard' })

const auth = useAuthStore()
const fleet = useFleetStore()
const api = useApi()

const vehicles = ref<Vehicle[]>([])
const loadingVehicles = ref(false)
const vehicleError = ref<string | null>(null)

async function fetchVehicles(): Promise<void> {
  // Drivers don't see the fleet roster on this page — the backend returns
  // 403 for non-managers so we short-circuit to avoid a guaranteed 403 in
  // the network panel.
  if (!auth.isManager) return
  loadingVehicles.value = true
  vehicleError.value = null
  try {
    const res = await api<{ vehicles: Vehicle[] }>('/vehicles')
    vehicles.value = res.vehicles
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    vehicleError.value = e?.data?.message ?? 'Failed to load vehicles'
  }
  finally {
    loadingVehicles.value = false
  }
}

// Client-side playback of pre-recorded routes.
//
// The CF Container backend doesn't run a continuous position simulator (cost-
// protection — the demo expires 2026-05-31 with a budget under $5). Instead we
// pre-seed ~60 positions per vehicle covering the last hour and play them back
// in the browser: each tick interpolates between the two timestamps bracketing
// `now() % span` and pushes the result into the fleet store. Result is smooth
// movement on the map without any ongoing backend cost.
//
// The WS connection stays open so any real driver POST (e.g. from cmd/sim or
// /driver/report) overrides the playback for that vehicle.
const playbackTracks = ref(new Map<string, Position[]>())
let playbackHandle: ReturnType<typeof setInterval> | null = null

async function loadPlaybackTracks(): Promise<void> {
  if (!auth.isManager) return
  const since = Date.now() - 60 * 60 * 1000
  await Promise.all(vehicles.value.map(async (v) => {
    try {
      const res = await api<{ positions: Position[] }>(
        `/vehicles/${v.id}/positions?from=${since}&limit=500`,
      )
      // Backend returns DESC (newest first); reverse for chronological playback.
      const sorted = [...res.positions].reverse()
      if (sorted.length > 0) playbackTracks.value.set(v.id, sorted)
    }
    catch {
      // Per-vehicle fetch failures are non-fatal — other vehicles still animate.
    }
  }))
}

function tickPlayback(): void {
  const now = Date.now()
  for (const [vehicleId, track] of playbackTracks.value.entries()) {
    if (track.length === 0) continue
    const first = track[0]!.recorded_at
    const last = track[track.length - 1]!.recorded_at
    const span = last - first
    if (span <= 0) {
      fleet.positions.set(vehicleId, track[0]!)
      continue
    }
    // Map current wall time into the track via modulo: track loops every `span`.
    const elapsed = ((now - first) % span + span) % span
    const target = first + elapsed
    // Binary-ish linear scan for the bracketing pair. Cheap for ~60 points.
    let i = 0
    while (i < track.length - 1 && track[i + 1]!.recorded_at <= target) i++
    const a = track[i]!
    const b = track[Math.min(i + 1, track.length - 1)]!
    if (b.recorded_at <= a.recorded_at) {
      fleet.positions.set(vehicleId, a)
      continue
    }
    const t = (target - a.recorded_at) / (b.recorded_at - a.recorded_at)
    fleet.positions.set(vehicleId, {
      id: 0,
      vehicle_id: vehicleId,
      lat: a.lat + (b.lat - a.lat) * t,
      lng: a.lng + (b.lng - a.lng) * t,
      speed_kmh: a.speed_kmh,
      recorded_at: now,
      created_at: now,
    })
  }
}

onMounted(async () => {
  fleet.connect()
  await fetchVehicles()
  await loadPlaybackTracks()
  tickPlayback()
  // 3s cadence — slow enough to be cheap, fast enough to feel alive.
  playbackHandle = setInterval(tickPlayback, 3000)
})

onBeforeUnmount(() => {
  if (playbackHandle) {
    clearInterval(playbackHandle)
    playbackHandle = null
  }
  fleet.disconnect()
})

// Map center: most recent live position, or Bangkok (the demo dataset's
// area) for the cold-start empty state. The Map iterates in insertion
// order, so `.at(-1)` is whichever vehicle last broadcast — a reasonable
// "follow the action" default for an empty dashboard.
const center = computed(() => {
  const latest = Array.from(fleet.positions.values()).at(-1)
  return latest ? { lat: latest.lat, lng: latest.lng } : { lat: 13.7563, lng: 100.5018 }
})

// vehicle_id → plate_number for marker popups. Recomputed when the vehicle
// roster changes; MapView watches this map and updates open popups without
// rebuilding markers.
const vehicleLabels = computed(() => {
  const out = new Map<string, string>()
  for (const v of vehicles.value) out.set(v.id, v.plate_number)
  return out
})

function formatTimestamp(ms: number): string {
  const d = new Date(ms)
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}
</script>

<template>
  <div class="grid gap-4 lg:grid-cols-[1fr_320px]">
    <section class="space-y-3">
      <div class="flex items-center justify-between">
        <h1 class="text-2xl font-semibold tracking-tight">
          Live fleet
        </h1>
        <LiveBadge />
      </div>
      <MapView
        :positions="fleet.positions"
        :labels="vehicleLabels"
        :center="center"
        class-name="h-[560px] w-full rounded-md border border-border"
      />
    </section>

    <aside v-if="auth.isManager" class="space-y-3">
      <Card>
        <CardHeader>
          <CardTitle class="text-base">
            Vehicles
          </CardTitle>
          <CardDescription>{{ vehicles.length }} in fleet</CardDescription>
        </CardHeader>
        <CardContent>
          <p
            v-if="loadingVehicles"
            class="text-sm text-muted-foreground"
          >
            Loading…
          </p>
          <p
            v-else-if="vehicleError"
            class="text-sm text-destructive"
            role="alert"
          >
            {{ vehicleError }}
          </p>
          <p
            v-else-if="vehicles.length === 0"
            class="text-sm text-muted-foreground"
          >
            No vehicles yet.
          </p>
          <ul v-else class="space-y-3">
            <li
              v-for="v in vehicles"
              :key="v.id"
              class="text-sm border-b border-border/60 pb-2 last:border-b-0 last:pb-0"
            >
              <div class="flex items-center justify-between">
                <span class="truncate font-medium">
                  {{ v.plate_number }}
                  <span
                    v-if="v.model"
                    class="text-muted-foreground font-normal"
                  > · {{ v.model }}</span>
                </span>
                <span
                  v-if="fleet.positions.has(v.id)"
                  class="text-xs text-emerald-600 shrink-0 ml-2"
                >live</span>
              </div>
              <div
                v-if="fleet.positions.get(v.id) as Position | undefined"
                class="text-xs text-muted-foreground tabular-nums mt-0.5"
              >
                <span>{{ fleet.positions.get(v.id)!.lat.toFixed(4) }}, {{ fleet.positions.get(v.id)!.lng.toFixed(4) }}</span>
                <span class="ml-2 opacity-70">@ {{ formatTimestamp(fleet.positions.get(v.id)!.recorded_at) }}</span>
              </div>
              <div
                v-else
                class="text-xs text-muted-foreground/70 mt-0.5 italic"
              >
                no position reported
              </div>
            </li>
          </ul>
          <Button
            as-child
            variant="outline"
            class="mt-3 w-full"
          >
            <NuxtLink to="/dashboard/vehicles">
              Manage vehicles
            </NuxtLink>
          </Button>
        </CardContent>
      </Card>
    </aside>
  </div>
</template>
