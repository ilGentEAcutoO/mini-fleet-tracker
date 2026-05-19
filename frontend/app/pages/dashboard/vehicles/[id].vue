<script setup lang="ts">
// Manager-only vehicle history view (TASK-018).
//
// Fetches the vehicle metadata + position history for the selected window
// and renders a polyline on a map. The window defaults to the last 24
// hours so the page is useful on first load; the manager can shrink or
// widen it via two datetime-local pickers.
//
// Backend contract (see backend/internal/handler/vehicle_handler.go):
//   GET /api/vehicles/:id/positions?from=&to=&limit=
//     -> { vehicle_id, positions: Position[], count }
//   - manager-only (driver = 403)
//   - 404 if the vehicle id does not exist
//   - limit silently clamped at 5000
//   - ordering: DESC by recorded_at — we reverse on receive for the
//     polyline so the line is drawn oldest-to-newest (and so the marker
//     parent of MapView can later read positions[0] as "start of trail")
//
// The map's marker layer is fed an empty Map because the history view's
// visual is the polyline, not the live-tick markers. A future iteration
// could pin the start/end of the trail — not in scope for TASK-018.

import type { Position, Vehicle } from '~~/shared/types/domain'

definePageMeta({ layout: 'default' })

const route = useRoute()
const auth = useAuthStore()
const api = useApi()

// Route param is always a string; the cast is safe because vehicle IDs are
// UUIDs and the router never produces an array form for non-catchall params.
const vehicleId = route.params.id as string

useHead({ title: 'Vehicle history' })

// Belt-and-braces role guard. The backend will 403 a driver anyway, but
// kicking them back to /dashboard avoids the error pill and matches the
// same pattern used on the vehicles index page.
if (import.meta.client && auth.user && !auth.isManager) {
  await navigateTo('/dashboard')
}

const vehicle = ref<Vehicle | null>(null)
const positions = ref<Position[]>([])
const vehicleLoading = ref(false)
const historyLoading = ref(false)
const error = ref<string | null>(null)

// ---- Date range ----
//
// <input type="datetime-local"> wants local-time strings shaped as
// `YYYY-MM-DDTHH:MM` (no timezone, no seconds). new Date(str) parses that
// as local time, and Date.prototype.getTime() returns unix-ms — so the
// round-trip lines up with the backend's unix-ms expectation without
// extra timezone juggling on the client.

function toLocalDatetimeInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const dayMs = 24 * 60 * 60 * 1000
// Lazy init so SSR + first client paint use the same fixed instant — Date.now()
// during template setup is fine because the page renders inside ClientOnly
// for the map; the form fields themselves are SSR-safe but the value choice
// is irrelevant before hydration.
const initialNow = new Date()
const fromInput = ref(toLocalDatetimeInput(new Date(initialNow.getTime() - dayMs)))
const toInput = ref(toLocalDatetimeInput(initialNow))

async function fetchVehicle(): Promise<void> {
  vehicleLoading.value = true
  try {
    const res = await api<{ vehicle: Vehicle }>(`/vehicles/${vehicleId}`)
    vehicle.value = res.vehicle
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string }, status?: number, statusCode?: number } | undefined
    const status = e?.statusCode ?? e?.status
    if (status === 404) {
      error.value = 'Vehicle not found.'
    }
    else {
      error.value = e?.data?.message ?? 'Failed to load vehicle.'
    }
  }
  finally {
    vehicleLoading.value = false
  }
}

async function fetchHistory(): Promise<void> {
  historyLoading.value = true
  error.value = null
  try {
    const fromMs = new Date(fromInput.value).getTime()
    const toMs = new Date(toInput.value).getTime()
    if (Number.isNaN(fromMs) || Number.isNaN(toMs)) {
      error.value = 'Pick valid from and to times.'
      return
    }
    if (fromMs > toMs) {
      error.value = 'The "from" time must be before "to".'
      return
    }
    const res = await api<{ vehicle_id: string, positions: Position[], count: number }>(
      `/vehicles/${vehicleId}/positions`,
      // limit=5000 is the backend cap; passing it explicitly documents the
      // contract at the call-site and lets a future iteration tighten the
      // window without rediscovering the bound.
      { query: { from: fromMs, to: toMs, limit: 5000 } },
    )
    // The server returns DESC (newest-first). The polyline reads the array
    // in order, so we reverse to chronological — otherwise the line is
    // drawn end-to-start, which works mathematically but obscures the
    // mental model.
    positions.value = res.positions.slice().reverse()
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string }, status?: number, statusCode?: number } | undefined
    const status = e?.statusCode ?? e?.status
    if (status === 404) {
      error.value = 'Vehicle not found.'
    }
    else {
      error.value = e?.data?.message ?? 'Failed to load history.'
    }
  }
  finally {
    historyLoading.value = false
  }
}

onMounted(() => {
  void fetchVehicle()
  void fetchHistory()
})

// MapView takes a Map for `positions` (live markers) — we don't render any
// markers on the history view, so we pass an empty Map. The polyline does
// all the work.
const emptyMarkers = new Map<string, Position>()

// Center on the first point of the trail (oldest) so a long route is
// visible from start; falls back to Bangkok when the window is empty.
const center = computed(() => {
  const first = positions.value[0]
  return first ? { lat: first.lat, lng: first.lng } : { lat: 13.7563, lng: 100.5018 }
})
</script>

<template>
  <section class="space-y-4">
    <div>
      <NuxtLink
        to="/dashboard/vehicles"
        class="text-sm text-muted-foreground hover:underline"
      >
        &larr; Vehicles
      </NuxtLink>
      <h1 class="text-2xl font-semibold tracking-tight mt-2">
        <span v-if="vehicle">
          {{ vehicle.plate_number }}
          <span
            v-if="vehicle.model"
            class="text-muted-foreground font-normal"
          > &middot; {{ vehicle.model }}</span>
        </span>
        <span v-else-if="vehicleLoading" class="text-muted-foreground">
          Loading vehicle&hellip;
        </span>
        <span v-else class="text-muted-foreground">
          Unknown vehicle
        </span>
      </h1>
      <p
        v-if="vehicle?.driver_id"
        class="text-sm text-muted-foreground font-mono mt-1"
      >
        Driver: {{ vehicle.driver_id }}
      </p>
    </div>

    <Card>
      <CardHeader>
        <CardTitle class="text-base">
          History window
        </CardTitle>
        <CardDescription>
          Trail rendered for the selected range. Defaults to the last 24 hours.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          class="flex flex-wrap items-end gap-3"
          @submit.prevent="fetchHistory"
        >
          <div class="space-y-1">
            <Label for="from">From</Label>
            <Input
              id="from"
              v-model="fromInput"
              type="datetime-local"
            />
          </div>
          <div class="space-y-1">
            <Label for="to">To</Label>
            <Input
              id="to"
              v-model="toInput"
              type="datetime-local"
            />
          </div>
          <Button
            type="submit"
            :disabled="historyLoading"
          >
            {{ historyLoading ? 'Loading…' : 'Apply' }}
          </Button>
          <p class="text-sm text-muted-foreground">
            {{ positions.length }} point{{ positions.length === 1 ? '' : 's' }}
          </p>
        </form>
        <p
          v-if="error"
          class="text-sm text-destructive mt-2"
          role="alert"
        >
          {{ error }}
        </p>
      </CardContent>
    </Card>

    <MapView
      :positions="emptyMarkers"
      :path="positions"
      :center="center"
      class-name="h-[560px] w-full rounded-md border border-border"
    />
  </section>
</template>
