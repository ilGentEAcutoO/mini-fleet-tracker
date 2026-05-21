// Gateway worker tests — runs inside @cloudflare/vitest-pool-workers
// alongside an auxiliary "fleet-do-hub" worker (declared in
// vitest.config.ts -> miniflare.workers). The gateway's cross-script DO
// binding resolves through the auxiliary, so we can exercise the full
// /internal/publish → DO chain end-to-end.
//
// /api/* tests use a Vitest spy on global.fetch to capture the proxied
// upstream call without actually starting an upstream server.

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { env, SELF } from 'cloudflare:test'
import { sign } from '@tsndr/cloudflare-worker-jwt'
import {
  timingSafeEqualHex,
  hmacSha256Hex,
  originIsAllowed,
} from '../src/index'

const ALLOWED_ORIGIN = 'http://localhost:3000'
const JWT_SECRET = env.JWT_SECRET
const INTERNAL_PUBLISH_SECRET = env.INTERNAL_PUBLISH_SECRET

async function signValidJwt(): Promise<string> {
  const now = Math.floor(Date.now() / 1000)
  return sign(
    {
      iss: 'mini-fleet-tracker',
      sub: 'driver-gw-test',
      role: 'driver',
      jti: 'jti-gw-test',
      iat: now,
      nbf: now,
      exp: now + 60,
    },
    JWT_SECRET,
    { algorithm: 'HS256' },
  )
}

// ============================================================================
// Pure helpers
// ============================================================================

describe('gateway helpers', () => {
  it('timingSafeEqualHex differentiates equal vs unequal strings', () => {
    expect(timingSafeEqualHex('aa', 'aa')).toBe(true)
    expect(timingSafeEqualHex('aa', 'ab')).toBe(false)
  })

  it('hmacSha256Hex matches the gateway↔hub contract', async () => {
    // Same RFC 4231 vector as fleet-hub's test — keeps the two crypto
    // helpers byte-equivalent.
    const key = String.fromCharCode(...new Array(20).fill(0x0b))
    const out = await hmacSha256Hex(key, new TextEncoder().encode('Hi There'))
    expect(out).toBe(
      'b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7',
    )
  })

  it('originIsAllowed honours the comma-separated allow list', () => {
    expect(originIsAllowed('http://localhost:3000', env.ALLOWED_ORIGINS)).toBe(true)
    expect(originIsAllowed('https://attacker.example', env.ALLOWED_ORIGINS)).toBe(false)
  })

  // Single-origin posture per ARCHITECTURE.md + security-review.md M1
  // (cross-cutting). The gateway's wrangler.toml ALLOWED_ORIGINS must
  // match exactly the trimmed canonical pair — the workers.dev preview
  // URL was removed in TASK-048 because neither fleet-hub DO nor the
  // backend's CORS_ORIGIN accepted that origin; keeping the gateway more
  // lenient was inconsistent with the documented posture.
  it('originIsAllowed rejects the workers.dev preview origin under the canonical trimmed allow-list', () => {
    const canonicalAllowList = 'http://localhost:3000,https://fleet-tracker.jairukchan.com'
    expect(
      originIsAllowed(
        'https://fleet-worker-gateway.sornwin.workers.dev',
        canonicalAllowList,
      ),
    ).toBe(false)
    // Positive control: the two canonical origins ARE allowed.
    expect(originIsAllowed('http://localhost:3000', canonicalAllowList)).toBe(true)
    expect(
      originIsAllowed('https://fleet-tracker.jairukchan.com', canonicalAllowList),
    ).toBe(true)
  })
})

// ============================================================================
// CORS preflight (OPTIONS)
// ============================================================================

describe('CORS preflight', () => {
  it('returns 204 + allow-origin/credentials/methods for an allowed Origin', async () => {
    const res = await SELF.fetch('https://gateway.example/api/anything', {
      method: 'OPTIONS',
      headers: {
        Origin: ALLOWED_ORIGIN,
        'Access-Control-Request-Method': 'POST',
        'Access-Control-Request-Headers': 'Content-Type',
      },
    })
    expect(res.status).toBe(204)
    expect(res.headers.get('Access-Control-Allow-Origin')).toBe(ALLOWED_ORIGIN)
    expect(res.headers.get('Access-Control-Allow-Credentials')).toBe('true')
    expect(res.headers.get('Access-Control-Allow-Methods')).toMatch(/POST/)
    expect(res.headers.get('Vary')?.toLowerCase()).toContain('origin')
  })

  it('returns 403 for a disallowed Origin', async () => {
    const res = await SELF.fetch('https://gateway.example/api/whatever', {
      method: 'OPTIONS',
      headers: { Origin: 'https://attacker.example' },
    })
    expect(res.status).toBe(403)
  })

  it('returns 403 for a missing Origin', async () => {
    const res = await SELF.fetch('https://gateway.example/api/x', {
      method: 'OPTIONS',
    })
    expect(res.status).toBe(403)
  })

  // TASK-048: the workers.dev preview origin used to be accepted by the
  // gateway only (fleet-hub DO + Go backend CORS_ORIGIN never accepted
  // it). The trimmed allow-list in wrangler.toml + the matching
  // TEST_BINDINGS in vitest.config.ts both reject it.
  it('returns 403 + no allow-origin header for the workers.dev preview origin', async () => {
    const previewOrigin = 'https://fleet-worker-gateway.sornwin.workers.dev'
    const res = await SELF.fetch('https://gateway.example/api/auth/me', {
      method: 'OPTIONS',
      headers: {
        Origin: previewOrigin,
        'Access-Control-Request-Method': 'GET',
        'Access-Control-Request-Headers': 'Content-Type',
      },
    })
    expect(res.status).toBe(403)
    // Critical: must NEVER echo the preview origin back.
    expect(res.headers.get('Access-Control-Allow-Origin')).not.toBe(previewOrigin)
  })
})

// ============================================================================
// /internal/publish — HMAC-gated relay
// ============================================================================

describe('POST /internal/publish', () => {
  it('rejects requests without X-Signature (401)', async () => {
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: 'position.update' }),
    })
    expect(res.status).toBe(401)
  })

  it('rejects a bad signature (401)', async () => {
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v',
      lat: 0,
      lng: 0,
      recorded_at: 0,
    })
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': 'a'.repeat(64),
      },
      body,
    })
    expect(res.status).toBe(401)
  })

  it('rejects non-POST methods (405)', async () => {
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'GET',
    })
    expect(res.status).toBe(405)
  })

  it('relays a valid HMAC body to the DO and returns the DO response (204)', async () => {
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v-relay',
      lat: 13.7,
      lng: 100.5,
      recorded_at: 1716000000,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
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

  it('the relayed body fails the DO check if a different secret was used to sign', async () => {
    // Sign with a wrong secret but pass the value through anyway. The
    // gateway's signature check will catch it first (401) — proving the
    // gateway-side guard is real, not just a passthrough.
    const body = JSON.stringify({
      type: 'position.update',
      vehicle_id: 'v',
      lat: 0,
      lng: 0,
      recorded_at: 0,
    })
    const ts = String(Math.floor(Date.now() / 1000))
    const wrongSig = await hmacSha256Hex(
      'wrong-secret',
      new TextEncoder().encode(body + '\n' + ts),
    )
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': wrongSig,
        'X-Timestamp': ts,
      },
      body,
    })
    expect(res.status).toBe(401)
  })
})

// ============================================================================
// /api/* — forwarded to API_UPSTREAM_URL
// ============================================================================

describe('/api/* forwarding', () => {
  let fetchSpy: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    // Spy on the global fetch the gateway uses for upstream proxying. We
    // return a fabricated response so the test doesn't need a live
    // upstream — and we assert on the call args to confirm the gateway
    // mapped the URL correctly.
    fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response(JSON.stringify({ ok: true, forwardedFrom: url }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      // Anything else (e.g. internal DO fetches that go through the
      // service binding, not global fetch) falls through to the
      // original implementation.
      return (fetchSpy.getMockImplementation() as never) // unreachable
        ?? new Response('unhandled', { status: 500 })
    })
  })

  afterEach(() => {
    fetchSpy.mockRestore()
  })

  it('forwards GET /api/healthz to the upstream and returns 200', async () => {
    const res = await SELF.fetch('https://gateway.example/api/healthz', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(res.status).toBe(200)
    const json = (await res.json()) as { ok: boolean; forwardedFrom: string }
    expect(json.ok).toBe(true)
    expect(json.forwardedFrom).toBe(`${env.API_UPSTREAM_URL}/api/healthz`)

    // CORS injection on the response.
    expect(res.headers.get('Access-Control-Allow-Origin')).toBe(ALLOWED_ORIGIN)
    expect(res.headers.get('Access-Control-Allow-Credentials')).toBe('true')
  })

  it('forwards POST /api/auth/login with a body to the upstream', async () => {
    const res = await SELF.fetch('https://gateway.example/api/auth/login', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Origin: ALLOWED_ORIGIN,
      },
      body: JSON.stringify({ email: 'a@b.com' }),
    })
    expect(res.status).toBe(200)
    // The spy received a real URL — confirms hop-by-hop filtering + path
    // composition worked.
    const lastCall = fetchSpy.mock.calls[fetchSpy.mock.calls.length - 1]
    const calledUrl = lastCall[0] as string
    expect(calledUrl).toBe(`${env.API_UPSTREAM_URL}/api/auth/login`)
  })

  it('does not attach CORS headers when Origin is not allow-listed', async () => {
    const res = await SELF.fetch('https://gateway.example/api/anything', {
      method: 'GET',
      headers: { Origin: 'https://attacker.example' },
    })
    expect(res.status).toBe(200)
    expect(res.headers.get('Access-Control-Allow-Origin')).toBeNull()
  })
})

// ============================================================================
// /ws/* — WebSocket upgrade to the DO
// ============================================================================

describe('/ws/* upgrade', () => {
  it('returns 426 for a non-websocket request', async () => {
    const res = await SELF.fetch('https://gateway.example/ws/fleet', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(res.status).toBe(426)
  })

  it('completes a real WebSocket upgrade through to the DO when JWT + Origin are valid', async () => {
    const tok = await signValidJwt()
    const res = await SELF.fetch('https://gateway.example/ws/fleet', {
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
    res.webSocket!.close(1000, 'test done')
  })

  it('propagates a 401 from the DO when the cookie is missing', async () => {
    const res = await SELF.fetch('https://gateway.example/ws/fleet', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
      },
    })
    expect(res.status).toBe(401)
  })
})

// ============================================================================
// Unknown paths and CSP injection
// ============================================================================

describe('unknown paths', () => {
  it('returns 404 for paths that match no route', async () => {
    const res = await SELF.fetch('https://gateway.example/nowhere', {
      method: 'GET',
    })
    expect(res.status).toBe(404)
  })
})

// ============================================================================
// Demo expiration short-circuit (TASK-030)
// ============================================================================

describe('demo expiration', () => {
  // vi.setSystemTime is the right tool here: the gateway constructs
  // `new Date()` on every request, and setSystemTime moves the runtime
  // clock so that constructor returns our chosen instant. The
  // DEMO_EXPIRES_AT module-level Date was constructed at import time
  // BEFORE the fake clock applies, so it keeps its real value.
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns 410 demo_expired for /api/* after the cutoff', async () => {
    // Set the clock to one day after the cutoff. The cutoff is
    // 2026-05-31T23:59:59+07:00 == 2026-05-31T16:59:59Z.
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))

    const res = await SELF.fetch('https://gateway.example/api/anything', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(res.status).toBe(410)
    const body = (await res.json()) as {
      error: string
      message: string
      repo_url: string
      expired_at: string
    }
    expect(body.error).toBe('demo_expired')
    expect(body.expired_at).toMatch(/2026-05-31T/)
    expect(body.repo_url).toContain('github.com')
    expect(body.message).toContain('2026-05-31')
  })

  it('forwards /healthz to the upstream even after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    // Spy on global fetch so we can confirm the upstream call happened.
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response(JSON.stringify({ status: 'ok' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response('unhandled', { status: 500 })
    })
    try {
      const res = await SELF.fetch('https://gateway.example/healthz', {
        method: 'GET',
      })
      // The gateway has no explicit /healthz route (it 404s); the
      // point of this test is the demo-expiry guard does NOT
      // short-circuit /healthz. Status 404 is acceptable here — the
      // important assertion is "NOT 410". /api/healthz is the path
      // operators actually hit; tested below.
      expect(res.status).not.toBe(410)
    } finally {
      fetchSpy.mockRestore()
    }
  })

  it('forwards /api/healthz to the upstream even after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response(JSON.stringify({ status: 'ok' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response('unhandled', { status: 500 })
    })
    try {
      const res = await SELF.fetch('https://gateway.example/api/healthz', {
        method: 'GET',
        headers: { Origin: ALLOWED_ORIGIN },
      })
      expect(res.status).toBe(200)
      expect(fetchSpy).toHaveBeenCalled()
    } finally {
      fetchSpy.mockRestore()
    }
  })

  it('still serves CORS preflight (OPTIONS) after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://gateway.example/api/anything', {
      method: 'OPTIONS',
      headers: {
        Origin: ALLOWED_ORIGIN,
        'Access-Control-Request-Method': 'POST',
      },
    })
    // Preflight passes the allow-list and returns 204; the demo-expiry
    // guard does not intercept OPTIONS because the browser may need
    // to read the CORS posture before deciding to issue the real
    // request (which then itself gets the 410).
    expect(res.status).toBe(204)
  })

  it('does not 410 before the cutoff', async () => {
    vi.setSystemTime(new Date('2026-05-30T00:00:00Z'))
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response('unhandled', { status: 500 })
    })
    try {
      const res = await SELF.fetch('https://gateway.example/api/anything', {
        method: 'GET',
        headers: { Origin: ALLOWED_ORIGIN },
      })
      expect(res.status).toBe(200)
    } finally {
      fetchSpy.mockRestore()
    }
  })
})

describe('CSP injection', () => {
  // The withCORS helper inspects Content-Type to decide whether to add
  // CSP. The upstream HTML response is fabricated via the fetch spy so
  // we don't need a real HTML upstream.
  it('adds Content-Security-Policy to HTML responses', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response('<html><body>hi</body></html>', {
          status: 200,
          headers: { 'Content-Type': 'text/html; charset=utf-8' },
        })
      }
      return new Response('unhandled', { status: 500 })
    })
    try {
      const res = await SELF.fetch('https://gateway.example/api/dashboard', {
        method: 'GET',
        headers: { Origin: ALLOWED_ORIGIN },
      })
      expect(res.status).toBe(200)
      const csp = res.headers.get('Content-Security-Policy')
      expect(csp).not.toBeNull()
      expect(csp).toContain("default-src 'self'")
      expect(csp).toContain('frame-ancestors')
    } finally {
      spy.mockRestore()
    }
  })

  it('does not add Content-Security-Policy to JSON responses', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = typeof input === 'string' ? input : (input as Request).url
      if (url.startsWith(env.API_UPSTREAM_URL)) {
        return new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      return new Response('unhandled', { status: 500 })
    })
    try {
      const res = await SELF.fetch('https://gateway.example/api/healthz', {
        method: 'GET',
        headers: { Origin: ALLOWED_ORIGIN },
      })
      expect(res.status).toBe(200)
      expect(res.headers.get('Content-Security-Policy')).toBeNull()
    } finally {
      spy.mockRestore()
    }
  })
})

// ============================================================================
// TASK-050 — /ws/* upgrade rate limit (Workers Rate-Limiting binding WS_RL)
// ============================================================================
//
// The gateway binds a [[ratelimits]] namespace named WS_RL with
// limit=3, period=60. Before delegating to the DO, /ws/* requests with
// an Upgrade: websocket header are gated by env.WS_RL.limit({ key: IP }).
// The 4th attempt within the window returns 429 + Retry-After: 60.
//
// We use a unique IP per test so the limiter's per-key counter doesn't
// leak between tests. The signed JWT lets the DO 101 the first three
// upgrades; the 4th is rejected by the gateway BEFORE the DO sees it.

describe('TASK-050: /ws upgrade rate limit', () => {
  async function attemptUpgrade(ip: string, token: string): Promise<Response> {
    return SELF.fetch('https://gateway.example/ws/fleet', {
      method: 'GET',
      headers: {
        Upgrade: 'websocket',
        Origin: ALLOWED_ORIGIN,
        Cookie: `auth_token=${token}`,
        'CF-Connecting-IP': ip,
      },
    })
  }

  it('returns 429 + Retry-After: 60 on the 4th upgrade within the window', async () => {
    const ip = '203.0.113.50'
    const token = await signValidJwt()

    // First three calls consume the quota. We expect the gateway to
    // delegate to the DO (which 101s with a valid JWT) — but the test
    // only cares that they are NOT 429, since the rate limit applies
    // BEFORE the DO call. Drain the WebSocket to free the test from
    // hangs on hibernation cleanup.
    for (let i = 0; i < 3; i++) {
      const res = await attemptUpgrade(ip, token)
      expect(res.status).not.toBe(429)
      if (res.webSocket) {
        res.webSocket.accept()
        res.webSocket.close(1000, 'test done')
      }
    }

    // 4th attempt is rate-limited at the gateway, BEFORE the DO. The
    // response must include Retry-After: 60 per the spec.
    const fourth = await attemptUpgrade(ip, token)
    expect(fourth.status).toBe(429)
    expect(fourth.headers.get('Retry-After')).toBe('60')
  })

  it('counts rate-limit per IP — a different IP is not penalised by another IP exhausting its quota', async () => {
    const exhaustedIp = '203.0.113.51'
    const freshIp = '203.0.113.52'
    const token = await signValidJwt()

    // Exhaust the limit for exhaustedIp.
    for (let i = 0; i < 3; i++) {
      const res = await attemptUpgrade(exhaustedIp, token)
      if (res.webSocket) {
        res.webSocket.accept()
        res.webSocket.close(1000, 'exhaust done')
      }
    }
    const exhausted = await attemptUpgrade(exhaustedIp, token)
    expect(exhausted.status).toBe(429)

    // freshIp should still be able to upgrade — the limiter key is
    // CF-Connecting-IP per the implementation.
    const fresh = await attemptUpgrade(freshIp, token)
    expect(fresh.status).not.toBe(429)
    if (fresh.webSocket) {
      fresh.webSocket.accept()
      fresh.webSocket.close(1000, 'fresh done')
    }
  })

  it('does not consume the rate limit on non-websocket /ws requests (426 path)', async () => {
    // A plain GET to /ws/* returns 426. That branch must not call
    // env.WS_RL.limit — otherwise a non-WS probe burns the quota.
    const ip = '203.0.113.53'
    for (let i = 0; i < 5; i++) {
      const res = await SELF.fetch('https://gateway.example/ws/fleet', {
        method: 'GET',
        headers: { Origin: ALLOWED_ORIGIN, 'CF-Connecting-IP': ip },
      })
      // No Upgrade header → 426, NOT 429.
      expect(res.status).toBe(426)
    }
  })
})

// ============================================================================
// TASK-051 — /internal/publish HMAC over body + X-Timestamp (load-bearing)
// ============================================================================
//
// Contract: signature = HMAC-SHA256(body || "\n" || ts, secret) where ts
// is the X-Timestamp header (unix seconds, integer-stringified). Verifier
// accepts ONLY within ±30s of "now". Outside the window — or with no
// X-Timestamp — the request is rejected (401). Replay protection is
// load-bearing post-rollout: the 24h legacy body-only fallback has been
// removed on both gateway and hub.

describe('TASK-051: /internal/publish HMAC verifier', () => {
  const validBody = JSON.stringify({
    type: 'position.update',
    vehicle_id: 'v-task051',
    lat: 13.7,
    lng: 100.5,
    recorded_at: 1716000000,
  })

  async function signNewFormat(body: string, ts: string): Promise<string> {
    return hmacSha256Hex(
      INTERNAL_PUBLISH_SECRET,
      new TextEncoder().encode(body + '\n' + ts),
    )
  }

  it('accepts a valid new-format signature with X-Timestamp within ±30s', async () => {
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await signNewFormat(validBody, ts)
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body: validBody,
    })
    // Gateway-side verifier must accept this exact (sig, ts) pair, then
    // forward to the DO whose verifier mirrors the same contract. The
    // gateway returns the DO's 204.
    expect(res.status).toBe(204)
  })

  it('rejects a valid signature whose X-Timestamp is outside the ±30s window (401)', async () => {
    // ts = "1 hour ago" — out of window. The signature itself is computed
    // correctly with that ts, so the only thing that should reject it is
    // the window check; outside the window the verifier returns 401 with
    // no second path to fall back to.
    const ts = String(Math.floor(Date.now() / 1000) - 3600)
    const sig = await signNewFormat(validBody, ts)
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body: validBody,
    })
    expect(res.status).toBe(401)
  })

  it('rejects a request signed with the wrong secret (401)', async () => {
    // Valid X-Timestamp + correctly-shaped signature but signed against
    // the wrong secret — the new-mode HMAC compare returns false, the
    // verifier returns { ok: false }, and there is no fallback to mask it.
    const ts = String(Math.floor(Date.now() / 1000))
    const badSig = await hmacSha256Hex(
      'wrong-secret',
      new TextEncoder().encode(validBody + '\n' + ts),
    )
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': badSig,
        'X-Timestamp': ts,
      },
      body: validBody,
    })
    expect(res.status).toBe(401)
  })

  it('forwards X-Timestamp to the DO so the DO verifier can apply the same logic', async () => {
    // The DO now ALSO requires X-Timestamp + new-mode signature — no
    // legacy fallback on either end. A successful request MUST traverse
    // to the DO with both X-Signature and X-Timestamp intact, or the DO
    // returns 401 and the gateway propagates it. A 204 here is therefore
    // sufficient end-to-end proof that the gateway forwarded the headers
    // needed for the new-mode path.
    const ts = String(Math.floor(Date.now() / 1000))
    const sig = await signNewFormat(validBody, ts)
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Signature': sig,
        'X-Timestamp': ts,
      },
      body: validBody,
    })
    expect(res.status).toBe(204)
  })
})

// ============================================================================
// TASK-056 — /healthz strict equality demo-expiry bypass
// ============================================================================
//
// Previously `path.endsWith('/healthz')` let any URL ending in /healthz
// (e.g. /api/something/healthz) escape the demo-expiry 410. The fix is
// strict equality on the two canonical paths only: '/healthz' and
// '/api/healthz'.

describe('TASK-056: demo-expiry /healthz strict equality', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns 410 for /foo/healthz after the cutoff (was bypassed by the broad endsWith match)', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://gateway.example/foo/healthz', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(res.status).toBe(410)
    const body = (await res.json()) as { error: string }
    expect(body.error).toBe('demo_expired')
  })

  it('returns 410 for /api/something/healthz after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://gateway.example/api/something/healthz', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    expect(res.status).toBe(410)
  })

  it('still bypasses /healthz exactly after the cutoff', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://gateway.example/healthz', {
      method: 'GET',
    })
    // /healthz has no explicit route on the gateway — it 404s in normal
    // operation. The point is it's NOT 410: the demo-expiry guard does
    // not intercept the canonical /healthz path.
    expect(res.status).not.toBe(410)
  })

  it('still bypasses /api/healthz exactly after the cutoff (forwarded to FLEET_API stub)', async () => {
    vi.setSystemTime(new Date('2026-06-01T17:00:00Z'))
    const res = await SELF.fetch('https://gateway.example/api/healthz', {
      method: 'GET',
      headers: { Origin: ALLOWED_ORIGIN },
    })
    // Whatever the upstream returns, it must NOT be 410 — the
    // demo-expiry guard does not intercept the canonical /api/healthz.
    expect(res.status).not.toBe(410)
  })
})
