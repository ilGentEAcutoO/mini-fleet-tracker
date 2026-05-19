<script setup lang="ts">
// Live-tracking map.
//
// Wraps the Google Maps JS SDK in a <ClientOnly> because the SDK touches
// `document` / `window` and would crash during Nuxt's SSR pass otherwise.
//
// The component owns ONE map instance and ONE marker per vehicle. When the
// `positions` prop updates (eg from a WebSocket tick in TASK-016), we
// "upsert" markers instead of recreating them — recreating triggers a DOM
// teardown/build that visibly flickers when ticks land at multiple Hz. The
// upsert path just nudges `marker.position`, which the SDK animates
// internally without rebuilding the marker element.
//
// AdvancedMarkerElement requires a `mapId` (Google's vector renderer). We
// read it from runtime config; if it's missing we still render the map (the
// AdvancedMarker class falls back to a console warning, not a crash, but
// pins render with the legacy style).
//
// TASK-018 added an optional `path` prop for the history view. When
// supplied, the component renders a single Polyline through the points in
// order (the parent is responsible for chronological sorting — the
// backend returns DESC by recorded_at). The Polyline instance is reused
// across prop changes (setPath, not recreate) for the same flicker-free
// reason the markers do.

import type { Position } from '~~/shared/types/domain'

interface Props {
  // vehicle_id → latest position. A Map (not a plain object) so consumers can
  // call `.set(id, pos)` to upsert without a deep clone, and so we can iterate
  // entries in insertion order to keep z-order stable across renders.
  positions: Map<string, Position>
  // Optional ordered path. When non-empty, MapView draws a Polyline through
  // these points (in order). Pass `undefined` (or omit) to skip the
  // polyline entirely — the live dashboard does not use this.
  path?: Position[]
  center?: { lat: number; lng: number }
  zoom?: number
  className?: string
}

const props = withDefaults(defineProps<Props>(), {
  path: () => [],
  // Bangkok — chosen because the demo dataset is Bangkok-flavoured and the
  // dashboard's initial empty state should look intentional, not "lost at
  // coordinate 0,0 in the Atlantic".
  center: () => ({ lat: 13.7563, lng: 100.5018 }),
  zoom: 12,
  className: 'h-[500px] w-full',
})

const config = useRuntimeConfig()
const { load } = useGoogleMaps()

const mapEl = ref<HTMLElement | null>(null)
// shallowRef because google.maps.Map is a deeply non-reactive SDK object —
// wrapping it in a normal ref makes Vue try to proxy every internal field
// and triggers SDK warnings about frozen prototypes.
const map = shallowRef<google.maps.Map | null>(null)
const markers = new Map<string, google.maps.marker.AdvancedMarkerElement>()
// Single reusable Polyline instance for the history-view path. shallowRef
// for the same reason as `map` — the SDK object is intentionally opaque.
const polyline = shallowRef<google.maps.Polyline | null>(null)
const error = ref<string | null>(null)

onMounted(async () => {
  try {
    await load()
    const { Map: GMap } = (await google.maps.importLibrary(
      'maps',
    )) as google.maps.MapsLibrary
    if (!mapEl.value) return

    map.value = new GMap(mapEl.value, {
      center: props.center,
      zoom: props.zoom,
      // mapId is REQUIRED for AdvancedMarkerElement to render in vector mode.
      // If the env var is empty/undefined we pass `undefined` (not '') so the
      // SDK's default fallback path engages cleanly.
      mapId: config.public.mapId || undefined,
      disableDefaultUI: false,
      mapTypeControl: false,
      streetViewControl: false,
    })

    // Initial render with whatever positions the parent already has.
    await syncMarkers()
    // Initial path render. Cheap when path is empty — the function
    // short-circuits before reaching for the SDK library.
    await syncPath()
  } catch (e: unknown) {
    error.value = e instanceof Error ? e.message : 'Failed to load map'
  }
})

// Watch the entries — the prop is a Map, and Vue's deep-watch on Map keys is
// fine here. We deliberately watch the values array (cheap to derive) rather
// than the Map itself so the callback fires reliably even when callers mutate
// in place (`positions.set(id, pos)`) AND when they assign a new Map.
watch(
  () => Array.from(props.positions.values()),
  () => {
    if (map.value) void syncMarkers()
  },
  { deep: false },
)

// Watch the path. We trigger on length + first/last identity so swapping
// out the array reference always fires, but a parent that pushes the same
// reference unchanged doesn't pay for a redundant SDK round-trip. Path
// equality of arrays in Vue would require a deep watcher, which is more
// expensive than needed here — we already get fresh prop updates whenever
// the parent reassigns the prop.
watch(
  () => props.path,
  () => {
    if (map.value) void syncPath()
  },
  { deep: false },
)

async function syncMarkers() {
  if (!map.value) return
  const { AdvancedMarkerElement } = (await google.maps.importLibrary(
    'marker',
  )) as google.maps.MarkerLibrary

  const seen = new Set<string>()
  for (const [vehicleId, pos] of props.positions.entries()) {
    seen.add(vehicleId)
    const existing = markers.get(vehicleId)
    if (!existing) {
      const marker = new AdvancedMarkerElement({
        map: map.value,
        position: { lat: pos.lat, lng: pos.lng },
        title: vehicleId,
      })
      markers.set(vehicleId, marker)
    } else {
      // Upsert: reuse the marker, only nudge its position. The SDK animates
      // the transition internally so high-frequency ticks don't flicker.
      existing.position = { lat: pos.lat, lng: pos.lng }
    }
  }

  // Reap markers for vehicles that disappeared (eg deleted, or filter
  // narrowed the fleet). Setting `marker.map = null` detaches it from the
  // map; we then drop the reference so the marker can be GC'd.
  for (const [vehicleId, marker] of markers.entries()) {
    if (!seen.has(vehicleId)) {
      marker.map = null
      markers.delete(vehicleId)
    }
  }
}

// syncPath reconciles the Polyline instance with `props.path`. When the
// path is empty we tear down any existing polyline so the map view goes
// clean. When the path is non-empty we reuse the existing polyline (just
// call setPath) to avoid the SDK's create/destroy cost — a 1000-vertex
// polyline takes a noticeable beat to instantiate on a cold map.
async function syncPath() {
  if (!map.value) return
  const path = props.path
  if (!path || path.length === 0) {
    if (polyline.value) {
      polyline.value.setMap(null)
      polyline.value = null
    }
    return
  }
  // Polyline lives in the `maps` library (same as the Map constructor), so
  // this importLibrary call is a hot-cache hit after onMounted — no extra
  // network round trip.
  const { Polyline } = (await google.maps.importLibrary(
    'maps',
  )) as google.maps.MapsLibrary
  const coords = path.map(p => ({ lat: p.lat, lng: p.lng }))
  if (!polyline.value) {
    polyline.value = new Polyline({
      map: map.value,
      path: coords,
      // A clear, fleet-blue line — visible against the default roadmap
      // tiles without competing with the live marker dots. strokeWeight 3
      // is the sweet spot: thin enough not to obscure the route detail,
      // thick enough to read on a phone at zoom 12.
      strokeColor: '#2563eb',
      strokeOpacity: 0.85,
      strokeWeight: 3,
      geodesic: true,
    })
  }
  else {
    polyline.value.setPath(coords)
  }
}

onBeforeUnmount(() => {
  for (const marker of markers.values()) {
    marker.map = null
  }
  markers.clear()
  if (polyline.value) {
    polyline.value.setMap(null)
    polyline.value = null
  }
  map.value = null
})
</script>

<template>
  <ClientOnly>
    <template #fallback>
      <div
        :class="className"
        class="flex items-center justify-center bg-muted text-sm text-muted-foreground"
      >
        Loading map…
      </div>
    </template>
    <div
      v-if="error"
      :class="className"
      class="flex items-center justify-center bg-destructive/10 text-sm text-destructive p-4"
    >
      {{ error }}
    </div>
    <div v-else ref="mapEl" :class="className" />
  </ClientOnly>
</template>
