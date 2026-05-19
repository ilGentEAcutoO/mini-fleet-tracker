// useMaplibre — central tile-source + lazy SDK loader for the project.
//
// MapLibre GL JS is bundled at build time (no async script tag), but
// importing the maplibregl namespace from a `<script setup>` block in
// every consumer would defeat tree-shaking. Centralising the import
// here gives us one spot to swap providers (OpenFreeMap → MapTiler →
// self-hosted, etc.) without touching every map consumer.
//
// Tiles default to OpenFreeMap's "liberty" style — vector tiles, hosted
// for free on Cloudflare's edge, no API key, attribution baked into the
// style JSON. The style URL can be overridden via NUXT_PUBLIC_MAP_STYLE
// for projects that want to point at their own tile server.
import maplibregl from 'maplibre-gl'
import 'maplibre-gl/dist/maplibre-gl.css'

export interface MaplibreHandle {
  ns: typeof maplibregl
  styleUrl: string
}

export const useMaplibre = (): MaplibreHandle => {
  const config = useRuntimeConfig()
  const styleUrl = (config.public.mapStyle as string)
    || 'https://tiles.openfreemap.org/styles/liberty'
  return { ns: maplibregl, styleUrl }
}

// circlePolygon builds a GeoJSON Polygon approximating a circle. MapLibre
// has no circle primitive, but a 64-point polygon is visually identical
// for fences ≤ 50km. Latitude scaling converts metres-radius to degrees
// using the local meridian length; longitude scaling additionally
// divides by cos(latitude) so the projected circle stays round at the
// edges of mid-latitude regions like Bangkok.
export function circlePolygon(
  center: { lat: number; lng: number },
  radiusMeters: number,
  points = 64,
): GeoJSON.Feature<GeoJSON.Polygon> {
  const coords: [number, number][] = []
  const radLat = (center.lat * Math.PI) / 180
  // 1 deg lat = 111_320 m; longitude shrinks by cos(lat) toward the poles.
  const latPerMetre = 1 / 111_320
  const lngPerMetre = 1 / (111_320 * Math.cos(radLat))
  for (let i = 0; i <= points; i++) {
    const angle = (i / points) * 2 * Math.PI
    const dy = radiusMeters * Math.sin(angle) * latPerMetre
    const dx = radiusMeters * Math.cos(angle) * lngPerMetre
    coords.push([center.lng + dx, center.lat + dy])
  }
  return {
    type: 'Feature',
    geometry: { type: 'Polygon', coordinates: [coords] },
    properties: {},
  }
}
