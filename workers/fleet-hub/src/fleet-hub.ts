// FleetHub — the WebSocket fan-out Durable Object for Mini Fleet Tracker.
//
// One global instance (idFromName('global-fleet')) holds the open
// WebSockets of every active dashboard / driver client. The Go API
// publishes events via the gateway's /internal/publish route, which
// forwards an HMAC-signed body to this DO's /publish handler. The DO
// broadcasts the event to every connected WebSocket.
//
// We use the WebSocket Hibernation API: `state.acceptWebSocket(server)`
// instead of `server.accept()`. Hibernation lets the DO be evicted from
// memory between bursts of activity — connections survive, billable
// duration drops to zero, and the runtime re-hydrates the object on the
// next event. The `webSocketMessage` / `webSocketClose` / `webSocketError`
// methods are the wake-up entry points.
//
// Security boundaries:
//   * /publish is HMAC-gated. We require X-Signature == hex-sha256(body).
//     Constant-time comparison guards against timing oracles.
//   * /upgrade is JWT- and Origin-gated. The cookie name and HS256 secret
//     match the Go API's pkg/jwt package; the issuer claim must equal
//     "mini-fleet-tracker".
//   * All other paths return 404.

import { DurableObject } from 'cloudflare:workers'
import { verify, decode } from '@tsndr/cloudflare-worker-jwt'

// DEMO_EXPIRES_AT is the cost-protection kill-switch landed by TASK-030.
// After this instant the DO rejects WS upgrades and /publish posts.
// Even if a residual Container with an old expiry tries to push events
// through, this guard at the DO boundary stops the broadcast. The Date
// object is constructed once at module load.
//
// The const must be edited + redeployed to revive the demo (one of the
// three deployable artefacts that need to change — gateway, DO,
// backend). Friction is the point: revival is a deliberate 5-step
// workflow documented in ARCHITECTURE.md, not a single env-var flip.
export const DEMO_EXPIRES_AT = new Date('2026-05-31T23:59:59+07:00')

// Env is the bindings + vars surface declared in wrangler.toml.
// The interface is exported because tests construct mock envs against
// the same shape.
export interface Env {
  JWT_SECRET: string
  INTERNAL_PUBLISH_SECRET: string
  ALLOWED_ORIGINS: string
  FLEET_HUB: DurableObjectNamespace<FleetHub>
}

// FleetEvent is the wire format published by Go and broadcast verbatim.
// Other event shapes are rejected at /publish time to keep the schema
// honest — adding a new event requires a deliberate code change here AND
// in backend/internal/usecase to match.
export type FleetEvent =
  | {
      type: 'position.update'
      vehicle_id: string
      lat: number
      lng: number
      recorded_at: number
    }
  | {
      type: 'geofence.alert'
      vehicle_id: string
      alert_type: 'enter' | 'exit'
      at: number
    }

// JWT claim shape matches backend/pkg/jwt/jwt.go exactly. Issuer is the
// fixed brand constant; sub is the driver UUID; jti is the per-token
// nonce used by Logout's KV blocklist (the DO does NOT consult the
// blocklist — that work happens at the Go API, which is the only authority
// on revocation. A revoked token will still hold an open WS until it
// naturally expires; acceptable for the demo).
interface FleetClaims {
  iss?: string
  sub?: string
  role?: string
  jti?: string
  iat?: number
  exp?: number
  nbf?: number
}

const ISSUER = 'mini-fleet-tracker'
const AUTH_COOKIE_NAME = 'auth_token'

export class FleetHub extends DurableObject<Env> {
  override async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url)
    switch (url.pathname) {
      case '/publish':
        return this.handlePublish(req)
      case '/upgrade':
        return this.handleUpgrade(req)
      default:
        return new Response('not found', { status: 404 })
    }
  }

  // POST /publish — HMAC-gated event ingestion.
  //
  // The gateway forwards Go's signed body untouched. We verify the HMAC
  // via verifyPublishHMAC which supports two payload formats:
  //   * NEW (TASK-051): HMAC over `body || '\n' || ts`, with a ±30s
  //     window enforced against `X-Timestamp`. Blocks replay of captured
  //     signed requests past the window.
  //   * LEGACY: HMAC over body only. Kept active for ~24h after the
  //     publisher rolls out so any in-flight event from an older
  //     Container instance still lands.
  // The verifier logs whenever the legacy path is taken so operators can
  // watch the rollout drain and flip the flag off once the rate is zero.
  // On success we broadcast the event to every currently-connected WS.
  private async handlePublish(req: Request): Promise<Response> {
    if (req.method !== 'POST') {
      return new Response('method not allowed', { status: 405 })
    }

    // Demo-expiry guard (TASK-030). Belt-and-braces with the gateway:
    // even if a residual Container managed to forward a /publish past
    // the expiry, the DO refuses to fan it out. Returned BEFORE the HMAC
    // check so we don't spend cycles validating a doomed payload.
    if (new Date() > DEMO_EXPIRES_AT) {
      return new Response('demo_expired', { status: 410 })
    }

    const sigHeader = req.headers.get('X-Signature') ?? ''
    if (sigHeader === '') {
      return new Response('missing signature', { status: 401 })
    }

    // arrayBuffer() consumes the body once; we recover the JSON below.
    const bodyBytes = new Uint8Array(await req.arrayBuffer())
    const tsHeader = req.headers.get('X-Timestamp')
    const verdict = await verifyPublishHMAC(
      bodyBytes,
      sigHeader,
      tsHeader,
      this.env.INTERNAL_PUBLISH_SECRET,
    )
    if (!verdict.ok) {
      return new Response('bad signature', { status: 401 })
    }
    if (verdict.mode === 'legacy') {
      // Operational signal — when this rate drops to zero in production
      // logs, TASK-051's rollout is complete and the legacy branch can be
      // removed. Kept at info level: not an error, just a transition.
      console.info('fleet-hub: /publish accepted via legacy body-only HMAC')
    }

    // Parse only after the signature is verified — we don't want to leak
    // parser behaviour to anonymous attackers.
    let event: unknown
    try {
      const text = new TextDecoder().decode(bodyBytes)
      event = JSON.parse(text)
    } catch {
      return new Response('bad json', { status: 400 })
    }

    if (!isFleetEvent(event)) {
      return new Response('unsupported event', { status: 400 })
    }

    // Re-serialise so what we broadcast is the same shape we validated
    // (no leftover unknown fields). Equally important: each socket gets
    // a fresh string — no shared mutable buffer.
    const payload = JSON.stringify(event)
    const sockets = this.ctx.getWebSockets()
    for (const ws of sockets) {
      // Try-catch per socket: a single dead peer must not break fan-out.
      try {
        ws.send(payload)
      } catch {
        // Best-effort close. The runtime will surface the error via the
        // webSocketError hook for observability.
        try {
          ws.close(1011, 'send failed')
        } catch {
          /* ignore */
        }
      }
    }

    return new Response(null, { status: 204 })
  }

  // GET /upgrade — WebSocket upgrade with JWT cookie + Origin verification.
  //
  // We DO NOT trust the path-based subscription channel: every connected
  // client receives every event. Per-vehicle filtering is the SPA's job.
  // This keeps the DO's per-connection state at zero, which is what makes
  // hibernation meaningfully cheap.
  private async handleUpgrade(req: Request): Promise<Response> {
    if (req.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
      return new Response('expected websocket upgrade', { status: 426 })
    }

    // Demo-expiry guard (TASK-030). Reject the upgrade before we burn
    // a JWT verify on a doomed connection. The 426 check above stays
    // first so a non-WS probe still gets the correct hint; everything
    // else falls through to 410.
    if (new Date() > DEMO_EXPIRES_AT) {
      return new Response('demo_expired', { status: 410 })
    }

    // 1. Origin allow-list. Defense against cross-site WebSocket
    // hijacking — the cookie alone wouldn't suffice in a browser context.
    const origin = req.headers.get('Origin')
    if (!origin || !isOriginAllowed(origin, this.env.ALLOWED_ORIGINS)) {
      return new Response('origin not allowed', { status: 403 })
    }

    // 2. Read the auth cookie.
    const token = readCookie(req.headers.get('Cookie'), AUTH_COOKIE_NAME)
    if (!token) {
      return new Response('missing auth cookie', { status: 401 })
    }

    // 3. Verify the JWT.
    const claims = await verifyJwt(token, this.env.JWT_SECRET)
    if (!claims) {
      return new Response('invalid token', { status: 401 })
    }

    // 4. Accept the upgrade. Hibernation-capable.
    const pair = new WebSocketPair()
    const client = pair[0]
    const server = pair[1]
    this.ctx.acceptWebSocket(server)
    // Attach claim metadata so future per-connection logic (e.g. role
    // filtering) can read it without re-verifying the token. The
    // hibernation API persists attachments across eviction.
    server.serializeAttachment({
      sub: claims.sub ?? '',
      role: claims.role ?? '',
      // exp lets us drop sockets whose token aged past the boundary on
      // the next message — useful when TASK-030's DEMO_EXPIRES_AT lands.
      exp: claims.exp ?? 0,
    })

    return new Response(null, { status: 101, webSocket: client })
  }

  // ----- Hibernation hooks -----

  // The client doesn't push application messages today. We only honour a
  // string "ping" so a browser-level keepalive can probe liveness. Other
  // payloads are dropped silently (don't disconnect — be permissive).
  override async webSocketMessage(
    ws: WebSocket,
    message: ArrayBuffer | string,
  ): Promise<void> {
    if (typeof message === 'string' && message === 'ping') {
      try {
        ws.send('pong')
      } catch {
        /* socket gone; webSocketError will fire if relevant */
      }
    }
  }

  // No per-connection state to clean up — the attachment lives on `ws`
  // and disappears with it. Implementing the hook is still required so
  // the runtime can deliver the close to a hibernated DO.
  override async webSocketClose(
    _ws: WebSocket,
    _code: number,
    _reason: string,
    _wasClean: boolean,
  ): Promise<void> {
    // intentionally empty
  }

  override async webSocketError(_ws: WebSocket, _error: unknown): Promise<void> {
    // intentionally empty — runtime logs the error.
  }
}

// ===== Helpers (exported for tests) =====

// Constant-time string comparison. Returns false immediately on length
// mismatch (length isn't secret — the hex digest is fixed-width 64 chars)
// and otherwise OR-accumulates the xor of every char pair. JS strings are
// UTF-16; for hex inputs every code unit is < 128 so charCodeAt is safe.
export function timingSafeEqualHex(a: string, b: string): boolean {
  if (a.length !== b.length) return false
  let mismatch = 0
  for (let i = 0; i < a.length; i++) {
    mismatch |= a.charCodeAt(i) ^ b.charCodeAt(i)
  }
  return mismatch === 0
}

// verifyPublishHMAC enforces the TASK-051 replay-protection contract on
// /internal/publish. The new envelope is `HMAC-SHA256(body || '\n' || ts,
// secret)` with `X-Timestamp` carrying the unix-seconds string. We accept
// it only when `|now - ts| <= 30s`. If no timestamp is present (or if
// the new check declined), we fall through to the legacy body-only
// signature so events from a pre-rollout publisher still land.
//
// Bytes-identical to the gateway's verifier (workers/gateway/src/index.ts).
// Both ends MUST agree on:
//   * separator '\n' (UTF-8 single byte 0x0a)
//   * lowercase hex output from hmacSha256Hex
//   * constant-time compare via timingSafeEqualHex
// Diverging on any of these silently breaks the verifier without a clear
// failure mode at deploy time — a regression caught only by integration
// tests that drive the publisher through the gateway end-to-end.
//
// The `mode` field on a successful verdict lets the caller log when the
// legacy path was taken so the rollout drain is observable. Logging is
// the caller's job — this helper stays pure for testability.
export async function verifyPublishHMAC(
  body: Uint8Array,
  signature: string,
  ts: string | null,
  secret: string,
): Promise<{ ok: true; mode: 'new' | 'legacy' } | { ok: false; mode: 'new' }> {
  // Try the new format first when a timestamp is present. Parse defensively
  // — a non-numeric header is treated as missing and we fall through to
  // legacy. Number.isFinite rejects NaN, +Inf, -Inf which all coerce from
  // string forms that would otherwise pass `!Number.isNaN`.
  if (ts !== null) {
    const tsNum = Number.parseInt(ts, 10)
    if (Number.isFinite(tsNum)) {
      const nowSec = Date.now() / 1000
      const skew = Math.abs(nowSec - tsNum)
      if (skew <= 30) {
        // Build `body + '\n' + ts` as a single byte buffer. Encoding ts
        // separately as UTF-8 keeps the byte layout identical to a
        // Go-side `append(body, '\n'); append(body, []byte(ts))`.
        const tsBytes = new TextEncoder().encode('\n' + ts)
        const combined = new Uint8Array(body.length + tsBytes.length)
        combined.set(body, 0)
        combined.set(tsBytes, body.length)
        const expected = await hmacSha256Hex(secret, combined)
        if (timingSafeEqualHex(signature, expected)) {
          return { ok: true, mode: 'new' }
        }
      }
    }
  }

  // Legacy fallback: HMAC over body only. This is the contract that
  // shipped before TASK-051 — we keep it active during rollout so
  // unsigned-timestamp events from an older Container instance still
  // verify. Once the publisher upgrade is complete and the legacy log
  // line stops appearing in prod, this branch can be removed.
  const expectedLegacy = await hmacSha256Hex(secret, body)
  if (timingSafeEqualHex(signature, expectedLegacy)) {
    return { ok: true, mode: 'legacy' }
  }

  return { ok: false, mode: 'new' }
}

// HMAC-SHA256(message, secret) → lowercase hex string. Uses SubtleCrypto,
// the Web Crypto API exposed by the Workers runtime — no third-party
// dependency. Matches the byte-exact output of Go's
// hmac.New(sha256.New, ...) + hex.EncodeToString.
export async function hmacSha256Hex(
  secret: string,
  message: Uint8Array,
): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  )
  // SubtleCrypto.sign requires a BufferSource. Passing the Uint8Array's
  // backing buffer with offset/length keeps us zero-copy.
  const sigBuf = await crypto.subtle.sign('HMAC', key, message)
  return bufferToHex(sigBuf)
}

function bufferToHex(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf)
  const hex = new Array<string>(bytes.length)
  for (let i = 0; i < bytes.length; i++) {
    hex[i] = bytes[i].toString(16).padStart(2, '0')
  }
  return hex.join('')
}

// Origin allow-list. The env var is a comma-separated list of exact-match
// origins (scheme + host + optional port). We trim each entry and ignore
// blanks so a trailing comma in the var doesn't blow up.
export function isOriginAllowed(origin: string, allowList: string): boolean {
  const list = allowList
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
  return list.includes(origin)
}

// readCookie does case-sensitive key match. The Cookie header is the
// only thing a browser controls verbatim, so we accept the conventional
// `name=value; ...` layout and ignore attributes.
export function readCookie(cookieHeader: string | null, name: string): string | null {
  if (!cookieHeader) return null
  const parts = cookieHeader.split(';')
  for (const raw of parts) {
    const part = raw.trim()
    const eq = part.indexOf('=')
    if (eq === -1) continue
    const k = part.slice(0, eq)
    const v = part.slice(eq + 1)
    if (k === name) return decodeURIComponent(v)
  }
  return null
}

// verifyJwt returns claims on success, null on any failure. We deliberately
// collapse all failure modes (signature mismatch, expired, malformed,
// wrong issuer) into a single null — callers map every miss to HTTP 401
// and we don't want to leak which check failed via response timing or
// error text.
export async function verifyJwt(
  token: string,
  secret: string,
): Promise<FleetClaims | null> {
  try {
    // @tsndr/cloudflare-worker-jwt's verify() returns false on a bad
    // signature and throws on a malformed token. It also validates `exp`
    // and `nbf` by default. We additionally check the issuer ourselves
    // below — the library doesn't expose an issuer option.
    const ok = await verify(token, secret, { algorithm: 'HS256' })
    if (!ok) return null
    const decoded = decode<FleetClaims>(token)
    const claims = decoded.payload
    if (!claims) return null
    if (claims.iss !== ISSUER) return null
    return claims
  } catch {
    return null
  }
}

// Runtime type guard for the union type. We only validate the discriminant
// and the required keys — extra fields are dropped by the JSON.stringify
// re-serialisation in handlePublish.
export function isFleetEvent(v: unknown): v is FleetEvent {
  if (typeof v !== 'object' || v === null) return false
  const obj = v as Record<string, unknown>
  if (obj.type === 'position.update') {
    return (
      typeof obj.vehicle_id === 'string' &&
      typeof obj.lat === 'number' &&
      typeof obj.lng === 'number' &&
      typeof obj.recorded_at === 'number'
    )
  }
  if (obj.type === 'geofence.alert') {
    return (
      typeof obj.vehicle_id === 'string' &&
      (obj.alert_type === 'enter' || obj.alert_type === 'exit') &&
      typeof obj.at === 'number'
    )
  }
  return false
}

// The Durable Object exports the class. The DO worker itself also
// exports a default fetch handler so that vitest-pool-workers and the
// `wrangler dev` test harness can route through SELF. Production traffic
// always arrives via the gateway worker's idFromName lookup, so this
// fallback is exercised only in tests.
export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const id = env.FLEET_HUB.idFromName('global-fleet')
    const stub = env.FLEET_HUB.get(id)
    return stub.fetch(req)
  },
} satisfies ExportedHandler<Env>
