<script setup lang="ts">
// Live-tracking map.
//
// Wraps MapLibre GL JS in a <ClientOnly> because the SDK touches
// `document` / `window` and would crash during Nuxt's SSR pass. Tiles
// come from OpenFreeMap (free vector tiles, no API key, attribution
// baked into the style). The style URL is overridable via
// NUXT_PUBLIC_MAP_STYLE for projects that want to point at their own
// tile server.
//
// The component owns ONE map instance and ONE marker per vehicle. When
// the `positions` prop updates (eg from a WebSocket tick in TASK-016),
// we "upsert" markers — `marker.setLngLat(...)` nudges the DOM element
// in place instead of recreating it, so high-frequency ticks don't
// flicker.
//
// TASK-018 added an optional `path` prop for the history view. When
// supplied, we render a single LineString GeoJSON source + line layer.
// Updates set new data on the source (no layer rebuild) — same flicker-
// free reasoning as the markers.
import type { Position } from '~~/shared/types/domain'
import type { Map as MlMap, Marker as MlMarker } from 'maplibre-gl'

interface Props {
  // vehicle_id → latest position. A Map (not a plain object) so consumers
  // can call `.set(id, pos)` to upsert without a deep clone, and so we
  // iterate entries in insertion order to keep z-order stable.
  positions: Map<string, Position>
  // Optional ordered path. When non-empty, MapView draws a polyline
  // through these points (in order). Pass `undefined` (or omit) to skip
  // the polyline entirely — the live dashboard does not use this.
  path?: Position[]
  center?: { lat: number; lng: number }
  zoom?: number
  className?: string
}

const props = withDefaults(defineProps<Props>(), {
  path: () => [],
  // Bangkok — the demo dataset is Bangkok-flavoured and the dashboard's
  // initial empty state should look intentional, not "lost at 0,0 in
  // the Atlantic".
  center: () => ({ lat: 13.7563, lng: 100.5018 }),
  zoom: 12,
  className: 'h-[500px] w-full',
})

const { ns, styleUrl } = useMaplibre()

const mapEl = ref<HTMLElement | null>(null)
// shallowRef because MapLibre Map is a deeply non-reactive SDK object —
// wrapping it in a normal ref makes Vue try to proxy every internal
// field and would burn CPU on every render tick.
const map = shallowRef<MlMap | null>(null)
const markers = new Map<string, MlMarker>()
const error = ref<string | null>(null)
// styleLoaded flips true on the map's 'load' event; we defer source
// + layer registration until then so addSource / addLayer don't throw.
const styleLoaded = ref(false)

const PATH_SOURCE_ID = 'fleet-history-path'
const PATH_LAYER_ID = 'fleet-history-line'

onMounted(async () => {
  // <ClientOnly> defers its default slot until ITS own onMounted fires,
  // which sets `mounted.value = true` and schedules a re-render. Our
  // parent onMounted runs before that re-render flushes, so `mapEl` is
  // still null on the first tick. Waiting one nextTick lets the slot
  // render and the template ref bind to the real <div>. Same pattern as
  // GeofenceEditor.vue.
  await nextTick()
  if (!mapEl.value) return
  try {
    map.value = new ns.Map({
      container: mapEl.value,
      style: styleUrl,
      center: [props.center.lng, props.center.lat],
      zoom: props.zoom,
      attributionControl: { compact: true },
    })
    map.value.on('load', () => {
      styleLoaded.value = true
      syncMarkers()
      syncPath()
    })
    map.value.on('error', (e) => {
      error.value = e?.error?.message ?? 'Map error'
    })
  } catch (e: unknown) {
    error.value = e instanceof Error ? e.message : 'Failed to load map'
  }
})

// Watch the entries — Vue's deep-watch on Map keys is fine here. We
// derive the values array so the callback fires whether callers mutate
// in place (`positions.set(id, pos)`) or assign a new Map.
watch(
  () => Array.from(props.positions.values()),
  () => syncMarkers(),
  { deep: false },
)

watch(
  () => props.path,
  () => syncPath(),
  { deep: false },
)

function syncMarkers() {
  const m = map.value
  if (!m || !styleLoaded.value) return

  const seen = new Set<string>()
  for (const [vehicleId, pos] of props.positions.entries()) {
    seen.add(vehicleId)
    const existing = markers.get(vehicleId)
    if (!existing) {
      const marker = new ns.Marker({ color: '#2563eb' })
        .setLngLat([pos.lng, pos.lat])
        .addTo(m)
      // Optional tooltip via popup; left as no-op here so the live
      // dashboard stays uncluttered. Hovering shows the marker only.
      marker.getElement().title = vehicleId
      markers.set(vehicleId, marker)
    } else {
      // Upsert: nudge the existing marker without rebuilding the DOM.
      existing.setLngLat([pos.lng, pos.lat])
    }
  }

  // Reap markers for vehicles that disappeared.
  for (const [vehicleId, marker] of markers.entries()) {
    if (!seen.has(vehicleId)) {
      marker.remove()
      markers.delete(vehicleId)
    }
  }
}

// syncPath reconciles the polyline source data with `props.path`. We
// add the source + layer once (lazy on first non-empty path); on
// subsequent updates we call setData which only updates the GeoJSON,
// not the layer.
function syncPath() {
  const m = map.value
  if (!m || !styleLoaded.value) return

  const path = props.path
  const hasPath = !!path && path.length > 1
  const sourceExists = !!m.getSource(PATH_SOURCE_ID)

  if (!hasPath) {
    if (sourceExists) {
      if (m.getLayer(PATH_LAYER_ID)) m.removeLayer(PATH_LAYER_ID)
      m.removeSource(PATH_SOURCE_ID)
    }
    return
  }

  const geojson: GeoJSON.Feature<GeoJSON.LineString> = {
    type: 'Feature',
    geometry: {
      type: 'LineString',
      coordinates: path.map(p => [p.lng, p.lat]),
    },
    properties: {},
  }

  if (sourceExists) {
    const src = m.getSource(PATH_SOURCE_ID) as maplibregl.GeoJSONSource
    src.setData(geojson)
    return
  }

  m.addSource(PATH_SOURCE_ID, { type: 'geojson', data: geojson })
  m.addLayer({
    id: PATH_LAYER_ID,
    type: 'line',
    source: PATH_SOURCE_ID,
    layout: { 'line-join': 'round', 'line-cap': 'round' },
    // Fleet blue — visible against OpenFreeMap's liberty style without
    // competing with the live marker dots.
    paint: {
      'line-color': '#2563eb',
      'line-width': 3,
      'line-opacity': 0.85,
    },
  })
}

onBeforeUnmount(() => {
  for (const marker of markers.values()) marker.remove()
  markers.clear()
  if (map.value) {
    map.value.remove()
    map.value = null
  }
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
