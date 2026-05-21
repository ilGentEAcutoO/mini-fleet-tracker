// FleetHub Durable Object tests — exercised inside the Workers runtime
// via @cloudflare/vitest-pool-workers. The DO is reachable through the
// FLEET_HUB binding declared in fleet-hub/wrangler.toml; SELF routes
// through the worker's default fetch handler which proxies to the DO.
//
// We split into two suites: pure helpers (no Workers runtime needed)
// and integration (HMAC + JWT against the live DO).

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { env, SELF, runInDurableObject } from 'cloudflare:test'
import { sign } from '@tsndr/cloudflare-worker-jwt'
import {
  timingSafeEqualHex,
  hmacSha256Hex,
  isOriginAllowed,
  readCookie,
  verifyJwt,
  isFleetEvent,
  type FleetEvent,
  type FleetHub,
} from '../src/fleet-hub'

// Test secrets are injected via miniflare.bindings in vitest.config.ts.
// We reach into env to keep the values authoritative — if the config
// changes, the tests follow.
const JWT_SECRET = env.JWT_SECRET
const INTERNAL_PUBLISH_SECRET = env.INTERNAL_PUBLISH_SECRET
const ALLOWED_ORIGIN = 'http://localhost:3000'

// Helper: build a valid HS256 JWT matching the Go pkg/jwt claim shape.
async function signValidJwt(opts?: {
  expSecondsFromNow?: number
  iss?: string
  sub?: string
}): Promise<string> {
  const now = Math.floor(Date.now() / 1000)
  const exp = now + (opts?.expSecondsFromNow ?? 60)
  return sign(
    {
      iss: opts?.iss ?? 'mini-fleet-tracker',
      sub: opts?.sub ?? 'driver-uuid-test',
      role: 'driver',
      jti: 'jti-test',
      iat: now,
      nbf: now,
      exp,
    },
    JWT_SECRET,
    { algorithm: 'HS256' },
  )
}

// ============================================================================
// Pure helpers — no runtime bindings needed.
// ============================================================================

describe('timingSafeEqualHex', () => {
  it('returns true for byte-equal inputs', () => {
    expect(timingSafeEqualHex('abc', 'abc')).toBe(true)
  })
  it('returns false for mismatched same-length inputs', () => {
    expect(timingSafeEqualHex('abc', 'abd')).toBe(false)
  })
  it('returns false for length mismatch', () => {
    expect(timingSafeEqualHex('abc', 'abcd')).toBe(false)
  })
  it('handles the 64-char hex length used by SHA-256', () => {
    const a = 'a'.repeat(64)
    const b = 'a'.repeat(63) + 'b'
    expect(timingSafeEqualHex(a, b)).toBe(false)
    expect(timingSafeEqualHex(a, a)).toBe(true)
  })
})

describe('hmacSha256Hex', () => {
  it('produces a 64-char lowercase hex digest', async () => {
    const out = await hmacSha256Hex('secret', new TextEncoder().encode('hello'))
    expect(out).toMatch(/^[0-9a-f]{64}$/)
  })
  it('is deterministic for the same key and message', async () => {
    const msg = new TextEncoder().encode('{"a":1}')
    const a = await hmacSha256Hex('k', msg)
    const b = await hmacSha256Hex('k', msg)
    expect(a).toBe(b)
  })
  it('matches the well-known test vector for HMAC-SHA256', async () => {
    // RFC 4231 test case 1: key=20 bytes of 0x0b, msg="Hi There"
    const key = String.fromCharCode(...new Array(20).fill(0x0b))
    const out = await hmacSha256Hex(key, new TextEncoder().encode('Hi There'))
    expect(out).toBe(
      'b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7',
    )
  })
})

describe('isOriginAllowed', () => {
  it('accepts an exact match', () => {
    expect(isOriginAllowed('http://localhost:3000', 'http://localhost:3000')).toBe(true)
  })
  it('handles a comma-separated list', () => {
    const list = 'http://localhost:3000,https://fleet-tracker.jairukchan.com'
    expect(isOriginAllowed('https://fleet-tracker.jairukchan.com', list)).toBe(true)
  })
  it('rejects an unknown origin', () => {
    expect(isOriginAllowed('https://attacker.example', 'http://localhost:3000')).toBe(false)
  })
  it('ignores blank entries and whitespace', () => {
    expect(isOriginAllowed('http://localhost:3000', '  http://localhost:3000 , , ')).toBe(true)
  })
})

describe('readCookie', () => {
  it('returns the value for a present key', () => {
    expect(readCookie('foo=bar; auth_token=tk', 'auth_token')).toBe('tk')
  })
  it('returns null when missing', () => {
    expect(readCookie('foo=bar', 'auth_token')).toBeNull()
  })
  it('returns null on empty header', () => {
    expect(readCookie(null, 'auth_token')).toBeNull()
  })
  it('decodes percent-encoded values', () => {
    expect(readCookie('x=a%20b', 'x')).toBe('a b')
  })
})

describe('isFleetEvent', () => {
  it('accepts position.update', () => {
    const e: FleetEvent = {
      type: 'position.update',
      vehicle_id: 'v-1',
      lat: 13.7,
      lng: 100.5,
      recorded_at: 1716000000,
    }
    expect(isFleetEvent(e)).toBe(true)
  })
  it('accepts geofence.alert', () => {
    const e: FleetEvent = {
      type: 'geofence.alert',
      vehicle_id: 'v-1',
      alert_type: 'enter',
      at: 1716000000,
    }
    expect(isFleetEvent(e)).toBe(true)
  })
  it('rejects unknown types', () => {
    expect(isFleetEvent({ type: 'whatever', vehicle_id: 'v' })).toBe(false)
  })
  it('rejects missing fields', () => {
    expect(isFleetEvent({ type: 'position.update', vehicle_id: 'v' })).toBe(false)
  })
  it('rejects non-objects', () => {
    expect(isFleetEvent(null)).toBe(false)
    expect(isFleetEvent('string')).toBe(false)
    expect(isFleetEvent(42)).toBe(false)
  })
})

describe('verifyJwt', () => {
  it('accepts a freshly signed token', async () => {
    const tok = await signValidJwt()
    const claims = await verifyJwt(tok, JWT_SECRET)
    expect(claims).not.toBeNull()
    expect(claims?.sub).toBe('driver-uuid-test')
    expect(claims?.iss).toBe('mini-fleet-tracker')
  })
  it('rejects a token with the wrong issuer', async () => {
    const tok = await signValidJwt({ iss: 'wrong-issuer' })
    const claims = await verifyJwt(tok, JWT_SECRET)
    expect(claims).toBeNull()
  })
  it('rejects a token signed with the wrong secret', async () => {
    const tok = await signValidJwt()
    const claims = await verifyJwt(tok, 'totally-different-secret')
    expect(claims).toBeNull()
  })
  it('rejects an expired token', async () => {
    const tok = await signValidJwt({ expSecondsFromNow: -60 })
    const claims = await verifyJwt(tok, JWT_SECRET)
    expect(claims).toBeNull()
  })
  it('rejects a malformed token', async () => {
    expect(await verifyJwt('not.a.real.token', JWT_SECRET)).toBeNull()
    expect(await verifyJwt('', JWT_SECRET)).toBeNull()
  })
})

// ============================================================================
// Integration — the DO running inside the Workers runtime.
// We hit it through the worker's default fetch handler (SELF), which
// proxies into the DO using idFromName('global-fleet').
// ============================================================================

describe('FleetHub /publish', () => {
  it('rejects a request without an X-Signature header (401)', async () => {
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: 'position.update' }),
    })
    expect(res.status).toBe(401)
  })

  it('rejects a request with the wrong signature (401)', async () => {
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-1',
      lat: 1,
      lng: 2,
      recorded_at: 1,
    })
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': 'deadbeef' + 'aa'.repeat(28), // 64 chars, wrong value
      },
      body,
    })
    expect(res.status).toBe(401)
  })

  it('rejects a method other than POST (405)', async () => {
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'GET',
    })
    expect(res.status).toBe(405)
  })

  it('rejects invalid JSON with a valid signature (400)', async () => {
    const body = 'not json {{'
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(400)
  })

  it('rejects unsupported event types with a valid signature (400)', async () => {
    const body = JSON.stringify({ type: 'unknown.event' })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(400)
  })

  it('accepts a signed position.update and returns 204', async () => {
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-1',
      lat: 13.7,
      lng: 100.5,
      recorded_at: 1716000000,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(204)
  })

  it('accepts a signed geofence.alert and returns 204', async () => {
    const body = JSON.stringify({
      type: 'geofence.alert',
      vehicle_id: 'v-1',
      alert_type: 'exit',
      at: 1716000001,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(204)
  })
})

describe('FleetHub /upgrade', () => {
  it('rejects a request that is not a WebSocket upgrade (426)', async () => {
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Origin: ALLOWED_ORIGIN,
        Cookie: 'auth_token=tk',
      },
    })
    expect(res.status).toBe(426)
  })

  it('rejects a disallowed Origin (403)', async () => {
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: 'https://attacker.example',
        Cookie: 'auth_token=tk',
      },
    })
    expect(res.status).toBe(403)
  })

  it('rejects a missing auth cookie (401)', async () => {
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
      },
    })
    expect(res.status).toBe(401)
  })

  it('rejects an invalid JWT (401)', async () => {
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: 'auth_token=not.a.jwt',
      },
    })
    expect(res.status).toBe(401)
  })

  it('accepts a valid JWT + allowed Origin and broadcasts to the socket (101 + message)', async () => {
    const tok = await signValidJwt()
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${tok}`,
      },
    })
    expect(res.status).toBe(101)
    expect(res.webSocket).not.toBeNull()

    const ws = res.webSocket!
    ws.accept()

    // Capture the first frame the DO sends down this socket.
    const received = new Promise<string>((resolve) => {
      ws.addEventListener('message', (ev: MessageEvent) => {
        resolve(typeof ev.data === 'string' ? ev.data : '[binary]')
      })
    })

    // Drive a /publish through the same DO. The same SELF + DO instance
    // means the broadcast arrives back over our socket.
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-broadcast',
      lat: 1.1,
      lng: 2.2,
      recorded_at: 99,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const pubRes = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(pubRes.status).toBe(204)

    const msg = await received
    const parsed = JSON.parse(msg)
    expect(parsed.type).toBe('position.update')
    expect(parsed.vehicle_id).toBe('v-broadcast')

    ws.close(1000, 'test done')
  })

  it('attaches claims metadata to the accepted websocket', async () => {
    const tok = await signValidJwt({ sub: 'driver-attach' })
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${tok}`,
      },
    })
    expect(res.status).toBe(101)
    expect(res.webSocket).not.toBeNull()
    res.webSocket!.accept()

    // Reach into the DO to assert the attachment landed. runInDurableObject
    // runs our callback in the DO's context with access to ctx.getWebSockets().
    // The global `cloudflare:test` env declares FLEET_HUB as a generic
    // DurableObjectNamespace; cast the namespace to the concrete class so
    // runInDurableObject's stub type matches its callback signature.
    const ns = env.FLEET_HUB as unknown as DurableObjectNamespace<FleetHub>
    const stub = ns.get(ns.idFromName('global-fleet'))
    const attachments = await runInDurableObject<FleetHub, Array<unknown>>(
      stub,
      async (_instance, state) => {
        return state.getWebSockets().map((s) => s.deserializeAttachment())
      },
    )
    // At least one of the attachments should have our test subject.
    const subs = attachments
      .filter((a): a is { sub: string } => typeof a === 'object' && a !== null && 'sub' in a)
      .map((a) => a.sub)
    expect(subs).toContain('driver-attach')

    res.webSocket!.close(1000, 'test done')
  })
})

describe('FleetHub default route', () => {
  it('returns 404 for unknown paths', async () => {
    const res = await SELF.fetch('https://test.example/who-knows', { method: 'GET' })
    expect(res.status).toBe(404)
  })
})

// ============================================================================
// Demo expiration (TASK-030)
// ============================================================================
//
// The DO consults `new Date()` at request time. vi.setSystemTime moves
// the runtime clock so we can drive both "before" and "after" the
// 2026-05-31T23:59:59+07:00 cutoff. The DEMO_EXPIRES_AT module constant
// was constructed at import time before the fake clock applies, so its
// value is real and stable.

describe('demo expiration', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('rejects POST /publish with 410 after the cutoff (no HMAC needed)', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    // Build a perfectly valid signed body; the guard fires BEFORE the
    // HMAC check, so the 410 is the only possible outcome.
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-after',
      lat: 1,
      lng: 2,
      recorded_at: 99,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(410)
    expect(await res.text()).toBe('demo_expired')
  })

  it('rejects WebSocket upgrade with 410 after the cutoff (no JWT needed)', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    // signValidJwt() under fake timers signs against the fake `now` —
    // the token's iat/exp end up nonsense for the real verifier but
    // it doesn't matter: the demo-expiry guard short-circuits before
    // verifyJwt runs.
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: 'auth_token=anything-the-guard-fires-first',
      },
    })
    expect(res.status).toBe(410)
    expect(await res.text()).toBe('demo_expired')
  })

  it('still answers 426 for non-WebSocket /upgrade requests after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    // The 426 check comes BEFORE the demo-expiry check — a probe
    // that's not even a WS upgrade gets the existing hint, not a
    // misleading 410.
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Origin: ALLOWED_ORIGIN,
        Cookie: 'auth_token=tk',
      },
    })
    expect(res.status).toBe(426)
  })

  it('does not 410 publishes before the cutoff', async () => {
    vi.setSystemTime(new Date('2026-05-30T00:00:00Z'))
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-before',
      lat: 1,
      lng: 2,
      recorded_at: 99,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(204)
  })

  it('still answers 405 for the wrong method on /publish after the cutoff', async () => {
    // /publish's method check happens BEFORE the demo-expiry guard so
    // a GET against /publish after the cutoff returns the existing
    // 405, not 410. Keeps the response shapes deterministic for
    // observability tooling that filters on status code.
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'GET',
    })
    expect(res.status).toBe(405)
  })
})

// ============================================================================
// TASK-051 — HMAC replay protection (DO side, load-bearing).
//
// The signed envelope is HMAC-SHA256(body || '\n' || ts, secret) with an
// X-Timestamp header and a ±30s window. Outside the window — or with no
// X-Timestamp — the DO rejects with 401. The 24h legacy body-only
// fallback has been removed: replay protection is now load-bearing.
//
// Bytes-identical contract with the gateway verifier:
//   * separator is the single byte '\n' (0x0a)
//   * UTF-8 encoding for both body and ts
//   * lowercase hex output
//   * constant-time compare via timingSafeEqualHex
// ============================================================================

describe('FleetHub /publish — HMAC replay protection (TASK-051)', () => {
  // Sign the new envelope: HMAC over `body + '\n' + ts` (UTF-8).
  async function signBodyAndTs(
    secret: string,
    body: string,
    ts: string,
  ): Promise<string> {
    return hmacSha256Hex(secret, new TextEncoder().encode(body + '\n' + ts))
  }

  it('accepts the new format inside the ±30s window (204)', async () => {
    // The DO checks `Math.abs(Date.now()/1000 - ts) <= 30`. Stamp now.
    const ts = String(Math.floor(Date.now() / 1000))
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-new',
      lat: 13.7,
      lng: 100.5,
      recorded_at: 1716000000,
    })
    const sig = await signBodyAndTs(INTERNAL_PUBLISH_SECRET, body, ts)
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(204)
  })

  it('rejects the new format outside the ±30s window (401)', async () => {
    // 5 minutes in the past — well outside ±30s. Even with a valid signature
    // over `body || \n || ts` for that ts, the DO refuses because the window
    // closed. There is no second path to fall back to — outside-window
    // requests are unconditionally 401.
    const ts = String(Math.floor(Date.now() / 1000) - 300)
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-stale',
      lat: 1,
      lng: 2,
      recorded_at: 1,
    })
    const sig = await signBodyAndTs(INTERNAL_PUBLISH_SECRET, body, ts)
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(401)
  })

  it('rejects an unsigned-timestamp body-only request (401)', async () => {
    // Garbage signature, no X-Timestamp header, valid body. The verifier
    // rejects when X-Timestamp is missing (no fallback path), so the DO
    // must respond 401.
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-bad',
      lat: 1,
      lng: 2,
      recorded_at: 1,
    })
    const res = await SELF.fetch('https://test.example/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': 'deadbeef'.repeat(8), // 64 chars, wrong
      },
      body,
    })
    expect(res.status).toBe(401)
  })
})

// ============================================================================
// TASK-057 — DO consults JWT blocklist on WS frames.
//
// On /upgrade the DO does a single BLOCKLIST_KV.get(jti) before accepting.
// On every 10th `webSocketMessage` the DO re-checks (cache TTL 60s) and
// closes with code 4001 + reason "token revoked" if the jti now exists in
// the blocklist. Tests mutate the binding's KV store directly via env
// (cloudflare:test exposes BLOCKLIST_KV as a real Miniflare-backed namespace).
// ============================================================================

describe('FleetHub /upgrade — blocklist consult (TASK-057)', () => {
  // Helpers reach into the test env. The KV binding is declared on the
  // FleetHub project in vitest.config.ts (added alongside this task).
  function blocklistKV(): KVNamespace {
    // env is typed loosely by cloudflare:test; cast for the binding we added.
    return (env as unknown as { BLOCKLIST_KV: KVNamespace }).BLOCKLIST_KV
  }

  it('rejects /upgrade when the jti is already in the blocklist (401)', async () => {
    const jti = `revoked-on-upgrade-${Date.now()}`
    await blocklistKV().put(jti, 'revoked')
    const tok = await signValidJwt({ sub: 'driver-blocked' })
    // signValidJwt above hardcodes jti='jti-test'. Re-sign with our jti.
    const now = Math.floor(Date.now() / 1000)
    const tokWithJti = await sign(
      {
        iss: 'mini-fleet-tracker',
        sub: 'driver-blocked',
        role: 'driver',
        jti,
        iat: now,
        nbf: now,
        exp: now + 60,
      },
      JWT_SECRET,
      { algorithm: 'HS256' },
    )
    void tok // unused but kept to mirror existing helper conventions

    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${tokWithJti}`,
      },
    })
    expect(res.status).toBe(401)
    expect(await res.text()).toBe('token revoked')

    // Cleanup so a later test reusing the same jti isn't poisoned.
    await blocklistKV().delete(jti)
  })

  it('accepts /upgrade when the jti is NOT in the blocklist (101)', async () => {
    const now = Math.floor(Date.now() / 1000)
    const tok = await sign(
      {
        iss: 'mini-fleet-tracker',
        sub: 'driver-clean',
        role: 'driver',
        jti: `clean-${Date.now()}`,
        iat: now,
        nbf: now,
        exp: now + 60,
      },
      JWT_SECRET,
      { algorithm: 'HS256' },
    )
    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${tok}`,
      },
    })
    expect(res.status).toBe(101)
    expect(res.webSocket).not.toBeNull()
    // accept() before close() — the test-side WebSocket is in CONNECTING
    // state until either side accepts. Matches the existing pattern in the
    // "broadcasts to the socket" test above.
    res.webSocket!.accept()
    res.webSocket!.close(1000, 'test done')
  })

  it('closes an active WS with code 4001 when the jti is revoked mid-session', async () => {
    // Open a fresh WS, then write to the blocklist, then send a string of
    // pings. Within ~10 frames the DO must consult KV and close 4001.
    const jti = `revoked-mid-${Date.now()}`
    const now = Math.floor(Date.now() / 1000)
    const tok = await sign(
      {
        iss: 'mini-fleet-tracker',
        sub: 'driver-mid',
        role: 'driver',
        jti,
        iat: now,
        nbf: now,
        exp: now + 60,
      },
      JWT_SECRET,
      { algorithm: 'HS256' },
    )

    const res = await SELF.fetch('https://test.example/upgrade', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${tok}`,
      },
    })
    expect(res.status).toBe(101)
    const ws = res.webSocket!
    ws.accept()

    // Revoke after the WS is open.
    await blocklistKV().put(jti, 'revoked')

    // Wait for the close. We send pings to drive webSocketMessage hooks
    // and observe the close event. Race with a timeout so a regression
    // surfaces as a test failure rather than a hang.
    const closed = new Promise<{ code: number; reason: string }>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('no close within 2s')), 2000)
      ws.addEventListener('close', (ev: CloseEvent) => {
        clearTimeout(timeout)
        resolve({ code: ev.code, reason: ev.reason })
      })
    })

    // Send up to 20 pings to be safe — the DO must check at least once
    // within that window regardless of the sample rate.
    for (let i = 0; i < 20; i++) {
      try {
        ws.send('ping')
      } catch {
        break // socket already closed by the DO
      }
    }

    const ev = await closed
    expect(ev.code).toBe(4001)
    expect(ev.reason).toBe('token revoked')

    await blocklistKV().delete(jti)
  })
})
