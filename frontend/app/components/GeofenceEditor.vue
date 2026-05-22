<script setup lang="ts">
// Geofence editor (TASK-020).
//
// Self-contained editor for a single vehicle's circular geofence. Renders
// three numeric inputs (center lat/lng + radius m), a save button, and a
// preview map with a polygon approximating the configured circle. The
// "Use map center" button copies the map's current pan position into the
// form — the fastest way to place a fence over a chosen Bangkok
// neighbourhood without typing six decimal digits.
//
// Lifecycle
//
//   onMounted     → fetchFence (GET, may 404 → empty state), then setupMap
//                   (instantiate MapLibre map + initial polygon)
//   onBeforeUnmount → remove() the map; the SDK detaches everything
//
// API contract (manager-only, CSRF on PUT via useApi):
//   GET /api/vehicles/:id/geofence → 200 { geofence } | 200 { geofence: null }
//                                    if unset (legacy backends may still
//                                    return 404 — we accept both during the
//                                    deploy window)
//   PUT /api/vehicles/:id/geofence → 200 { geofence }
//
// MapLibre note
//
//   MapLibre has no first-class Circle primitive. We approximate the
//   geofence with a 64-point GeoJSON polygon via circlePolygon() (helper
//   lives in useMaplibre.ts). The approximation is visually identical
//   for fences ≤ 50 km — the backend validation cap.
//
// Validation
//
//   Mirrors the backend bounds (TASK-020 usecase):
//     lat ∈ [-90, 90], lng ∈ [-180, 180], radius_m ∈ [50, 50_000]
//   Client-side validation is a UX nicety; the backend re-validates and
//   returns 400 if these are violated. We surface the client message
//   directly to avoid a network round-trip for obvious typos.
import { toast } from 'vue-sonner'
import type { Map as MlMap } from 'maplibre-gl'
import type { Geofence } from '~~/shared/types/domain'

interface Props {
  vehicleId: string
}

const props = defineProps<Props>()

const api = useApi()
const { ns, styleUrl } = useMaplibre()

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
const map = shallowRef<MlMap | null>(null)
const styleLoaded = ref(false)

const FENCE_SOURCE_ID = 'fleet-fence-circle'
const FENCE_FILL_LAYER_ID = 'fleet-fence-fill'
const FENCE_STROKE_LAYER_ID = 'fleet-fence-stroke'

async function fetchFence(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const res = await api<{ geofence: Geofence | null }>(
      `/vehicles/${props.vehicleId}/geofence`,
    )
    if (res.geofence === null) {
      // New backend contract: 200 with null body == "not configured yet".
      // Keep the Bangkok defaults already on centerLat/centerLng/radiusM so
      // the preview map has somewhere sensible to center on.
      fence.value = null
    }
    else {
      fence.value = res.geofence
      centerLat.value = res.geofence.center_lat
      centerLng.value = res.geofence.center_lng
      radiusM.value = res.geofence.radius_m
    }
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string }, status?: number, statusCode?: number } | undefined
    const status = e?.statusCode ?? e?.status
    if (status === 404) {
      // Legacy backend path — older Go builds returned 404 instead of
      // 200+null. Keep this branch during the deploy window so a returning
      // SPA briefly hitting the old backend (or a fresh SPA briefly hitting
      // the new one) both land in the empty-state UI cleanly.
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
// manager has already dragged the map to the area they want fenced.
function useCurrentMapCenter(): void {
  if (!map.value) return
  const c = map.value.getCenter()
  centerLat.value = c.lat
  centerLng.value = c.lng
}

function setupMap(): void {
  if (!mapEl.value) return
  try {
    map.value = new ns.Map({
      container: mapEl.value,
      style: styleUrl,
      center: [centerLng.value, centerLat.value],
      zoom: 14,
      attributionControl: { compact: true },
    })
    map.value.on('load', () => {
      styleLoaded.value = true
      syncCircle()
    })
    map.value.on('error', (e) => {
      error.value = e?.error?.message ?? 'Map error'
    })
  }
  catch (err: unknown) {
    error.value = err instanceof Error ? err.message : 'Failed to load map'
  }
}

// Reuse the same source/layers across form edits — setData on the
// source is O(1), whereas rebuilding the layer would re-rasterise the
// fill on the next frame.
function syncCircle(): void {
  const m = map.value
  if (!m || !styleLoaded.value) return

  const polygon = circlePolygon(
    { lat: centerLat.value, lng: centerLng.value },
    radiusM.value,
  )

  if (m.getSource(FENCE_SOURCE_ID)) {
    const src = m.getSource(FENCE_SOURCE_ID) as maplibregl.GeoJSONSource
    src.setData(polygon)
    return
  }

  m.addSource(FENCE_SOURCE_ID, { type: 'geojson', data: polygon })
  m.addLayer({
    id: FENCE_FILL_LAYER_ID,
    type: 'fill',
    source: FENCE_SOURCE_ID,
    paint: {
      // Same blue as the history polyline (MapView.vue) for visual
      // consistency. 12% fill opacity keeps the underlying tiles readable.
      'fill-color': '#2563eb',
      'fill-opacity': 0.12,
    },
  })
  m.addLayer({
    id: FENCE_STROKE_LAYER_ID,
    type: 'line',
    source: FENCE_SOURCE_ID,
    paint: {
      'line-color': '#2563eb',
      'line-width': 2,
      'line-opacity': 0.85,
    },
  })
}

watch([centerLat, centerLng, radiusM], () => syncCircle())

onMounted(async () => {
  await fetchFence()
  await nextTick()
  setupMap()
})

onBeforeUnmount(() => {
  if (map.value) {
    map.value.remove()
    map.value = null
  }
})
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle class="text-base">
        Geofence
      </CardTitle>
      <CardDescription>
        Circular boundary; entering / leaving fires a live alert to managers.
      </CardDescription>
    </CardHeader>
    <CardContent class="space-y-3">
      <div v-if="loading" class="text-sm text-muted-foreground">
        Loading…
      </div>
      <template v-else>
        <p v-if="!fence" class="text-sm text-muted-foreground">
          No geofence configured yet.
        </p>

        <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div>
            <Label for="fence-lat">Center latitude</Label>
            <Input id="fence-lat" v-model.number="centerLat" type="number" step="0.000001" />
          </div>
          <div>
            <Label for="fence-lng">Center longitude</Label>
            <Input id="fence-lng" v-model.number="centerLng" type="number" step="0.000001" />
          </div>
          <div>
            <Label for="fence-radius">Radius (m)</Label>
            <Input id="fence-radius" v-model.number="radiusM" type="number" step="10" min="50" max="50000" />
          </div>
        </div>

        <div class="flex gap-2">
          <Button type="button" :disabled="saving" @click="save">
            {{ saving ? 'Saving…' : fence ? 'Update fence' : 'Set fence' }}
          </Button>
          <Button type="button" variant="outline" @click="useCurrentMapCenter">
            Use map center
          </Button>
        </div>

        <p v-if="error" class="text-sm text-destructive" role="alert">
          {{ error }}
        </p>

        <ClientOnly>
          <div ref="mapEl" class="h-[320px] w-full rounded-md border border-border" />
          <template #fallback>
            <div class="h-[320px] w-full rounded-md border border-border bg-muted" />
          </template>
        </ClientOnly>
      </template>
    </CardContent>
  </Card>
</template>
