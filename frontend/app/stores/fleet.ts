// Pinia setup store for the live-tracking realtime channel.
//
// Owns:
//   - the WebSocket lifecycle (connect / disconnect / auto-reconnect / heartbeat)
//   - the per-vehicle latest `Position` map consumed by MapView
//   - a bounded ring of recent geofence alerts for the dashboard toaster
//
// The WS frames are the discriminated union `FleetMsg` from shared/types/domain.
// We parse every incoming frame with a zod schema at the seam so a malformed
// broadcast cannot poison downstream reactivity — it gets logged and dropped.
//
// Lifetime model: Pinia keeps the store instance alive across navigations, but
// `useFleetStore` here registers `onScopeDispose(disconnect)` against the
// FIRST consumer's effect scope (e.g. the layout or the dashboard page that
// calls `connect()`). When that consumer unmounts, the WS is torn down. If a
// later consumer wants the WS again it must call `connect()` itself. This
// keeps idle pages from holding open a CF DO connection.
//
// Design notes are inline; the trickier ones are documented at the call sites.

import { defineStore, skipHydrate } from 'pinia'
import { useWebSocket } from '@vueuse/core'
import { z } from 'zod'
import type { Position, FleetMsg } from '~~/shared/types/domain'

// Wire-format schemas mirror the discriminated union in
// shared/types/domain.d.ts byte-for-byte. We parse at the seam (any incoming
// WS message) so malformed broadcasts are isolated rather than poisoning
// downstream reactivity.
const positionUpdateSchema = z.object({
  type: z.literal('position.update'),
  vehicle_id: z.string().min(1),
  lat: z.number().min(-90).max(90),
  lng: z.number().min(-180).max(180),
  recorded_at: z.number().int().positive(),
})

const geofenceAlertSchema = z.object({
  type: z.literal('geofence.alert'),
  vehicle_id: z.string().min(1),
  alert_type: z.union([z.literal('enter'), z.literal('exit')]),
  at: z.number().int().positive(),
})

const fleetMsgSchema = z.discriminatedUnion('type', [
  positionUpdateSchema,
  geofenceAlertSchema,
])

export type ConnectionState =
  | 'idle'
  | 'connecting'
  | 'open'
  | 'reconnecting'
  | 'closed'

export interface GeofenceAlertEntry {
  vehicle_id: string
  alert_type: 'enter' | 'exit'
  at: number
}

// 25s — comfortably under Cloudflare's 100s WebSocket idle timeout. We drive
// our own heartbeat instead of VueUse's because the server's pong response is
// the literal string 'pong' (see workers/fleet-hub) and we want the watcher
// below to short-circuit on it without going through zod parse.
const HEARTBEAT_INTERVAL_MS = 25_000

// Cap the alert ring at 50 entries — the dashboard renders the most recent
// few in a toaster; older entries roll off. Cheaper than a circular buffer
// and the array shape plays nicely with Vue's reactivity.
const ALERT_RING_SIZE = 50

export const useFleetStore = defineStore('fleet', () => {
  const config = useRuntimeConfig()

  // Latest position per vehicle. `shallowReactive` on a Map gives us
  // reactivity on .set / .delete operations (those mutate the Map's internal
  // size + structure, which Vue tracks). We never mutate `Position` records
  // in place — every WS frame produces a fresh object — so the lack of deep
  // reactivity is by design and saves CPU at WS-frame rate.
  const positions = shallowReactive(new Map<string, Position>())

  // Bounded alert ring — newest first. `ref` (not shallowRef) so the array
  // replace below is reactive; the alert entries themselves are immutable
  // after creation.
  const alerts = ref<GeofenceAlertEntry[]>([])

  const status = ref<ConnectionState>('idle')
  const lastError = ref<string | null>(null)

  // `useWebSocket` handle. Held in a module-scoped variable instead of a ref
  // because (a) we don't want it to be reactive — the handle's internal state
  // is already reactive via `ws.data` / `ws.status` — and (b) Pinia would try
  // to serialise a non-plain value on SSR if it landed in a ref.
  let ws: ReturnType<typeof useWebSocket> | null = null
  let heartbeatHandle: ReturnType<typeof setInterval> | null = null

  function buildUrl(): string {
    const base = config.public.wsBase as string
    if (!base) throw new Error('NUXT_PUBLIC_WS_BASE not configured')
    return base.replace(/\/$/, '') + '/fleet'
  }

  function startHeartbeat(sock: WebSocket) {
    stopHeartbeat()
    heartbeatHandle = setInterval(() => {
      // Guard against a closed socket between intervals — onDisconnected
      // calls stopHeartbeat, but a race during the close handshake could
      // still see readyState !== OPEN here.
      if (sock.readyState === WebSocket.OPEN) {
        sock.send('ping')
      }
    }, HEARTBEAT_INTERVAL_MS)
  }

  function stopHeartbeat() {
    if (heartbeatHandle) {
      clearInterval(heartbeatHandle)
      heartbeatHandle = null
    }
  }

  function connect() {
    if (ws) return
    status.value = 'connecting'
    lastError.value = null

    try {
      ws = useWebSocket(buildUrl(), {
        autoReconnect: {
          retries: -1, // unlimited — the WS is the live channel and we want to
          // recover from transient drops without user action.
          // Exponential backoff capped at 30s. VueUse v14.3 accepts a function
          // `(retries: number) => number` for `delay`. Cap the exponent at 6
          // so 2^6 = 64s * 1000ms is bounded before the min(…, 30_000) clamp.
          delay: (attempt) =>
            Math.min(1_000 * 2 ** Math.min(attempt, 6), 30_000),
          onFailed() {
            status.value = 'closed'
            lastError.value = 'Reconnect failed'
          },
        },
        // We drive our own heartbeat (see HEARTBEAT_INTERVAL_MS) because the
        // server pong response is the literal string 'pong' — handled below
        // in the watcher's short-circuit.
        heartbeat: false,
        immediate: true,
        onConnected(socket) {
          status.value = 'open'
          startHeartbeat(socket)
        },
        onDisconnected() {
          // VueUse will attempt auto-reconnect itself; surface that as
          // 'reconnecting' until either onConnected (back to 'open') or
          // onFailed (terminal 'closed') resolves the state.
          status.value = 'reconnecting'
          stopHeartbeat()
        },
        onError(_socket, event) {
          // Native WebSocket errors don't expose a meaningful message — the
          // spec deliberately keeps them opaque to avoid leaking server state.
          // We log what we have and let the caller decide whether to show
          // anything in the UI.
          lastError.value =
            (event as Event & { message?: string })?.message ?? 'WebSocket error'
        },
      })

      // Parse + dispatch incoming messages. The `watch` on `ws.data` fires
      // for every frame the underlying WebSocket emits; the shallowRef shape
      // means we only see new values (not deep mutations of old ones), which
      // is what we want here.
      watch(ws.data, (raw) => {
        if (raw === null || raw === undefined) return
        if (typeof raw !== 'string') return
        // Server heartbeat response — short-circuit before JSON parse.
        if (raw === 'pong') return

        let parsed: FleetMsg
        try {
          parsed = fleetMsgSchema.parse(JSON.parse(raw)) as FleetMsg
        } catch (e) {
          // Don't poison the store on malformed frames — log and move on.
          // A future observability hook could surface these to ops dashboards.
          console.warn('[fleet] invalid WS message', e, raw)
          return
        }

        if (parsed.type === 'position.update') {
          // Synthesize a Position record from the wire shape. The DB row is
          // the source of truth (the Go API writes it before publishing the
          // WS frame); we don't have the DB-assigned id/created_at here and
          // MapView doesn't need them — it reads lat/lng/recorded_at only.
          // `id: 0` is a deliberate sentinel; if a future consumer needs the
          // real id, the history API is the place to fetch it from.
          positions.set(parsed.vehicle_id, {
            id: 0,
            vehicle_id: parsed.vehicle_id,
            lat: parsed.lat,
            lng: parsed.lng,
            recorded_at: parsed.recorded_at,
            created_at: parsed.recorded_at,
          } satisfies Position)
        } else if (parsed.type === 'geofence.alert') {
          // Bounded ring — newest first, drop tail beyond ALERT_RING_SIZE.
          alerts.value = [
            {
              vehicle_id: parsed.vehicle_id,
              alert_type: parsed.alert_type,
              at: parsed.at,
            },
            ...alerts.value.slice(0, ALERT_RING_SIZE - 1),
          ]
        }
      })
    } catch (e: unknown) {
      status.value = 'closed'
      lastError.value = e instanceof Error ? e.message : 'connect failed'
    }
  }

  function disconnect() {
    stopHeartbeat()
    if (ws) {
      ws.close()
      ws = null
    }
    status.value = 'closed'
  }

  // Tear down the WS when the first consumer's effect scope dies (component
  // unmount, page navigation away if the caller is page-scoped).
  //
  // Caveat: Pinia keeps the store alive across navigations, but
  // `onScopeDispose` here mirrors the lifecycle of the FIRST consumer that
  // resolves the store. If that consumer is short-lived (e.g. a child
  // component that calls `useFleetStore` and unmounts before the parent), we
  // would disconnect prematurely. The recommended pattern is to call
  // `connect()` from a long-lived owner (layout, or the route that needs the
  // live channel) — see `dashboard/index.vue` in TASK-017.
  onScopeDispose(disconnect)

  return {
    // skipHydrate on the positions Map: Pinia's hydration serialises state
    // on SSR; a Map serialises to `{}` and would be clobbered on the client
    // side. We always hydrate empty on the client and let the WS fill it.
    // (Pinia 3's `skipHydrate<T>(obj: T): T` preserves the input type, so no
    // cast is needed here.)
    positions: skipHydrate(positions),
    alerts,
    status,
    lastError,
    connect,
    disconnect,
  }
})
