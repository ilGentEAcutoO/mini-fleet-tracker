<script setup lang="ts">
// Geofence editor (TASK-020).
//
// Self-contained editor for a single vehicle's circular geofence. Renders
// three numeric inputs (center lat/lng + radius m), a save button, and a
// preview map with a google.maps.Circle synced to the form. The "Use map
// center" button copies the map's current pan position into the form, which
// is the fastest way to place a fence over a chosen Bangkok neighbourhood
// without typing six decimal digits.
//
// Lifecycle
//
//   onMounted     → fetchFence (GET, may 404 → empty state), then setupMap
//                   (load Maps SDK + instantiate map + initial circle)
//   onBeforeUnmount → detach circle, drop map ref so the SDK GC's cleanly
//
// API contract (manager-only, CSRF on PUT via useApi):
//   GET /api/vehicles/:id/geofence → 200 { geofence } | 404 if unset
//   PUT /api/vehicles/:id/geofence → 200 { geofence }
//
// Maps SDK note
//
//   google.maps.Circle is part of the `maps` library (same import as Map +
//   Polyline). Loading `maps` once via useGoogleMaps().load() is enough —
//   the importLibrary call inside setupMap is a hot-cache hit. Circle is
//   the legacy class (still supported in `weekly`); AdvancedMarkerElement
//   does not have a circle equivalent in 2026-05, so the legacy class is
//   the correct choice here.
//
// Validation
//
//   Mirrors the backend bounds (TASK-020 usecase):
//     lat ∈ [-90, 90], lng ∈ [-180, 180], radius_m ∈ [50, 50_000]
//   Client-side validation is a UX nicety; the backend re-validates and
//   returns 400 if these are violated. We surface the client message
//   directly to avoid a network round-trip for obvious typos.

import { toast } from 'vue-sonner'
import type { Geofence } from '~~/shared/types/domain'

interface Props {
  vehicleId: string
}

const props = defineProps<Props>()

const api = useApi()
const { load } = useGoogleMaps()

const fence = ref<Geofence | null>(null)
// Bangkok defaults — match MapView.vue's empty-state center so an
// unconfigured fence drops over the same area as the live dashboard.
const centerLat = ref(13.7563)
const centerLng = ref(100.5018)
const radiusM = ref(500)
const loading = ref(true)
const saving = ref(false)
const error = ref<string | null>(null)

const mapEl = ref<HTMLElement | null>(null)
// shallowRef for the SDK objects — same reasoning as MapView.vue: the SDK's
// internal state is intentionally opaque and Vue's deep proxy would warn
// about frozen prototypes if we used a plain ref.
const map = shallowRef<google.maps.Map | null>(null)
const circle = shallowRef<google.maps.Circle | null>(null)

async function fetchFence(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const res = await api<{ geofence: Geofence }>(
      `/vehicles/${props.vehicleId}/geofence`,
    )
    fence.value = res.geofence
    centerLat.value = res.geofence.center_lat
    centerLng.value = res.geofence.center_lng
    radiusM.value = res.geofence.radius_m
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string }, status?: number, statusCode?: number } | undefined
    const status = e?.statusCode ?? e?.status
    if (status === 404) {
      // 404 is the "not configured yet" path — empty state, no error pill.
      fence.value = null
    }
    else {
      error.value = e?.data?.message ?? 'Failed to load geofence'
    }
  }
  finally {
    loading.value = false
  }
}

async function save(): Promise<void> {
  if (centerLat.value < -90 || centerLat.value > 90) {
    error.value = 'Latitude must be between -90 and 90'
    return
  }
  if (centerLng.value < -180 || centerLng.value > 180) {
    error.value = 'Longitude must be between -180 and 180'
    return
  }
  if (radiusM.value < 50 || radiusM.value > 50_000) {
    error.value = 'Radius must be between 50 m and 50 km'
    return
  }
  saving.value = true
  error.value = null
  try {
    const res = await api<{ geofence: Geofence }>(
      `/vehicles/${props.vehicleId}/geofence`,
      {
        method: 'PUT',
        body: {
          center_lat: centerLat.value,
          center_lng: centerLng.value,
          radius_m: radiusM.value,
        },
      },
    )
    fence.value = res.geofence
    toast.success('Geofence saved')
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    error.value = e?.data?.message ?? 'Failed to save geofence'
  }
  finally {
    saving.value = false
  }
}

// Copy the map's current pan position into the form. Useful when the
// manager has already dragged the map to the area they want fenced — one
// click avoids typing two six-decimal coordinates.
function useCurrentMapCenter(): void {
  if (!map.value) return
  const c = map.value.getCenter()
  if (!c) return
  centerLat.value = c.lat()
  centerLng.value = c.lng()
}

async function setupMap(): Promise<void> {
  if (!mapEl.value) return
  try {
    await load()
    const { Map: GMap } = (await google.maps.importLibrary(
      'maps',
    )) as google.maps.MapsLibrary
    map.value = new GMap(mapEl.value, {
      center: { lat: centerLat.value, lng: centerLng.value },
      zoom: 14,
      disableDefaultUI: false,
      mapTypeControl: false,
      streetViewControl: false,
    })
    await syncCircle()
  }
  catch (err: unknown) {
    error.value = err instanceof Error ? err.message : 'Failed to load map'
  }
}

// Reuse the same Circle instance across form edits — setCenter / setRadius
// are O(1) on the SDK side, whereas constructing a fresh Circle each time
// re-rasterises the fill on the next frame.
async function syncCircle(): Promise<void> {
  if (!map.value) return
  const { Circle } = (await google.maps.importLibrary(
    'maps',
  )) as google.maps.MapsLibrary
  if (!circle.value) {
    circle.value = new Circle({
      map: map.value,
      center: { lat: centerLat.value, lng: centerLng.value },
      radius: radiusM.value,
      // Same blue as the history polyline (MapView.vue) for visual
      // consistency. 12% fill opacity keeps the underlying tiles readable.
      strokeColor: '#2563eb',
      strokeOpacity: 0.85,
      strokeWeight: 2,
      fillColor: '#2563eb',
      fillOpacity: 0.12,
    })
  }
  else {
    circle.value.setCenter({ lat: centerLat.value, lng: centerLng.value })
    circle.value.setRadius(radiusM.value)
  }
}

// Cheap to watch each scalar separately — Vue batches three watcher fires
// into one tick anyway, and the body is idempotent.
watch([centerLat, centerLng, radiusM], () => {
  if (map.value) void syncCircle()
})

onMounted(async () => {
  await fetchFence()
  // Wait one tick so <ClientOnly>'s default slot has rendered the mapEl
  // ref before we try to attach a Map to it.
  await nextTick()
  await setupMap()
})

onBeforeUnmount(() => {
  if (circle.value) {
    circle.value.setMap(null)
    circle.value = null
  }
  map.value = null
})
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle class="text-base">
        Geofence
      </CardTitle>
      <CardDescription>
        Circular boundary; entering or leaving fires a live alert to managers.
      </CardDescription>
    </CardHeader>
    <CardContent class="space-y-3">
      <p v-if="loading" class="text-sm text-muted-foreground">
        Loading…
      </p>
      <template v-else>
        <p
          v-if="!fence"
          class="text-sm text-muted-foreground"
        >
          No geofence configured yet.
        </p>

        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div class="space-y-1">
            <Label for="fence-lat">Center latitude</Label>
            <Input
              id="fence-lat"
              v-model.number="centerLat"
              type="number"
              step="0.000001"
            />
          </div>
          <div class="space-y-1">
            <Label for="fence-lng">Center longitude</Label>
            <Input
              id="fence-lng"
              v-model.number="centerLng"
              type="number"
              step="0.000001"
            />
          </div>
          <div class="space-y-1">
            <Label for="fence-radius">Radius (m)</Label>
            <Input
              id="fence-radius"
              v-model.number="radiusM"
              type="number"
              step="10"
              min="50"
              max="50000"
            />
          </div>
        </div>

        <div class="flex flex-wrap gap-2">
          <Button
            type="button"
            :disabled="saving"
            @click="save"
          >
            {{ saving ? 'Saving…' : fence ? 'Update fence' : 'Set fence' }}
          </Button>
          <Button
            type="button"
            variant="outline"
            @click="useCurrentMapCenter"
          >
            Use map center
          </Button>
        </div>

        <p
          v-if="error"
          class="text-sm text-destructive"
          role="alert"
        >
          {{ error }}
        </p>

        <ClientOnly>
          <div
            ref="mapEl"
            class="h-[320px] w-full rounded-md border border-border"
          />
          <template #fallback>
            <div class="h-[320px] w-full rounded-md border border-border bg-muted" />
          </template>
        </ClientOnly>
      </template>
    </CardContent>
  </Card>
</template>