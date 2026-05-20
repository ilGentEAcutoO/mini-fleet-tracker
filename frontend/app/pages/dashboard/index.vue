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

// Pull the most-recent persisted position per vehicle so the map renders
// markers on first paint even when no live WS frames have arrived in the
// current session. Without this seed the map looks empty until a driver
// reports — bad first impression for the demo audience.
async function seedLatestPositions(): Promise<void> {
  if (!auth.isManager) return
  const since = Date.now() - 24 * 60 * 60 * 1000
  await Promise.all(vehicles.value.map(async (v) => {
    try {
      const res = await api<{ positions: Position[] }>(
        `/vehicles/${v.id}/positions?from=${since}&limit=1`,
      )
      const latest = res.positions[0]
      // Skip if a WS frame has already populated this vehicle while we were
      // fetching — the WS feed is the live truth.
      if (latest && !fleet.positions.has(latest.vehicle_id)) {
        fleet.positions.set(latest.vehicle_id, latest)
      }
    }
    catch {
      // Per-vehicle failures are non-fatal — the map can still show whatever
      // other vehicles loaded successfully.
    }
  }))
}

onMounted(async () => {
  fleet.connect()
  await fetchVehicles()
  void seedLatestPositions()
})

onBeforeUnmount(() => {
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
          <ul v-else class="space-y-2">
            <li
              v-for="v in vehicles"
              :key="v.id"
              class="flex items-center justify-between text-sm"
            >
              <span class="truncate">
                {{ v.plate_number }}
                <span
                  v-if="v.model"
                  class="text-muted-foreground"
                > · {{ v.model }}</span>
              </span>
              <span
                v-if="fleet.positions.has(v.id)"
                class="text-xs text-emerald-600"
              >live</span>
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
