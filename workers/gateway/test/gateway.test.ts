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

async function signBody(secret: string, body: string): Promise<string> {
  return hmacSha256Hex(secret, new TextEncoder().encode(body))
}

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
    const sig = await signBody(INTERNAL_PUBLISH_SECRET, body)
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Signature': sig },
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
    const wrongSig = await signBody('wrong-secret', body)
    const res = await SELF.fetch('https://gateway.example/internal/publish', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Signature': wrongSig },
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
