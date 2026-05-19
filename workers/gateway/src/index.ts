// fleet-worker-gateway — the public edge router for Mini Fleet Tracker.
//
// Responsibilities:
//   * Forward /api/* to the Go API (today: env.API_UPSTREAM_URL; in prod a
//     Container service binding will replace this — TASK-025).
//   * Upgrade /ws/* to the FleetHub Durable Object via its global ID.
//   * HMAC-verify /internal/publish, then relay to the DO. Defence in
//     depth: even if a misconfigured route exposes the Go API to the
//     public internet, the DO will still reject any unsigned publish.
//   * Inject a strict CSP on HTML responses; serve CORS preflights for
//     the SPA at the edge.
//
// Cookies (SameSite=Lax) work without cross-origin CORS in prod because
// the gateway, the Container, and the SPA all share the
// fleet-tracker.jairukchan.com origin. CORS handling is here for the
// localhost:3000 dev case and as defence against accidental
// cross-origin requests.

const CSP_HEADER =
  "default-src 'self'; " +
  "script-src 'self' https://maps.googleapis.com; " +
  "connect-src 'self' wss://fleet-tracker.jairukchan.com https://maps.googleapis.com; " +
  "img-src 'self' data: https:; " +
  "style-src 'self' 'unsafe-inline'; " +
  "frame-ancestors 'none'; " +
  "base-uri 'self';"

// DEMO_EXPIRES_AT is the cost-protection kill-switch landed by TASK-030.
// After this instant the gateway short-circuits to 410 Gone — the
// actual cost saver, because a 410 from the Worker never wakes the
// Container. The Date object is constructed once at module load.
//
// CORS preflights (OPTIONS) are NOT short-circuited: a browser may
// still need to see the allow-origin headers before deciding to issue
// the real request — which will then receive the 410. /healthz is also
// exempted so an external monitor can still probe the upstream's
// liveness after expiration.
//
// The const must be edited + redeployed to revive the demo. The deliberate
// friction is the point of TASK-030 cost-protection layer 2.
export const DEMO_EXPIRES_AT = new Date('2026-05-31T23:59:59+07:00')

// Bindings + vars surface for the gateway worker. Mirrors gateway/wrangler.toml.
export interface Env {
  JWT_SECRET: string
  INTERNAL_PUBLISH_SECRET: string
  API_UPSTREAM_URL: string
  ALLOWED_ORIGINS: string
  FLEET_HUB: DurableObjectNamespace
  // Service binding to the fleet-api Container Worker. Optional so the
  // dev path still type-checks when the binding is absent (wrangler dev
  // without --services); production always has it.
  FLEET_API?: Fetcher
}

// Hop-by-hop headers (RFC 7230 §6.1) must not be forwarded — many of them
// also have specific meaning to fetch/Workers that would break if relayed.
const HOP_BY_HOP_HEADERS = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailers',
  'transfer-encoding',
  'upgrade',
  'host',
])

export default {
  async fetch(req: Request, env: Env, _ctx: ExecutionContext): Promise<Response> {
    const url = new URL(req.url)
    const path = url.pathname

    // CORS preflight is handled at the edge for every route — saves a
    // round-trip to the upstream and gives us one place to enforce the
    // allow-list. Preflights are exempt from the demo-expiry guard
    // below so a browser can still observe the CORS posture and surface
    // a clean 410 on the actual request.
    if (req.method === 'OPTIONS') {
      return handleCORSPreflight(req, env)
    }

    // Demo expiration short-circuit (TASK-030). /healthz forwards
    // through so a monitor can still see the upstream's per-dep status;
    // every other path returns the 410 envelope without touching the
    // Container. This is what makes the cost-protection real: the
    // Worker handles the request itself, the Container never wakes.
    if (new Date() > DEMO_EXPIRES_AT && !path.endsWith('/healthz')) {
      return new Response(
        JSON.stringify({
          error: 'demo_expired',
          message: 'This demo expired on 2026-05-31. See the repo for the source.',
          repo_url: 'https://github.com/ilGentEAcutoO/mini-fleet-tracker',
          expired_at: DEMO_EXPIRES_AT.toISOString(),
        }),
        { status: 410, headers: { 'content-type': 'application/json' } },
      )
    }

    if (path === '/internal/publish') {
      return handleInternalPublish(req, env)
    }

    if (path.startsWith('/api/')) {
      return withCORS(await forwardToUpstream(req, env), req, env)
    }

    if (path.startsWith('/ws/')) {
      return upgradeToHub(req, env)
    }

    return new Response('not found', { status: 404 })
  },
} satisfies ExportedHandler<Env>

// --- CORS ---

export function originIsAllowed(origin: string, allowList: string): boolean {
  const list = allowList
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
  return list.includes(origin)
}

function handleCORSPreflight(req: Request, env: Env): Response {
  const origin = req.headers.get('Origin')
  if (!origin || !originIsAllowed(origin, env.ALLOWED_ORIGINS)) {
    return new Response('origin not allowed', { status: 403 })
  }
  // Echoing the validated origin (never `*`) is required for credentialed
  // requests. SameSite=Lax + Access-Control-Allow-Credentials=true is what
  // makes the cookie-based auth work cross-origin during local dev.
  return new Response(null, {
    status: 204,
    headers: {
      'Access-Control-Allow-Origin': origin,
      'Access-Control-Allow-Credentials': 'true',
      'Access-Control-Allow-Methods': 'GET, POST, PUT, PATCH, DELETE, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type, X-CSRF-Token, X-Requested-With',
      'Access-Control-Max-Age': '86400',
      Vary: 'Origin',
    },
  })
}

// withCORS attaches the CORS allow-origin / credentials headers to a
// non-preflight response if the request's Origin is allow-listed. We
// also inject the CSP header on HTML responses here so all the
// "decorate the response" logic stays in one helper.
export function withCORS(resp: Response, req: Request, env: Env): Response {
  // Headers are immutable on a Response constructed by fetch(); clone
  // by spreading into a fresh Headers().
  const headers = new Headers(resp.headers)
  const origin = req.headers.get('Origin')
  if (origin && originIsAllowed(origin, env.ALLOWED_ORIGINS)) {
    headers.set('Access-Control-Allow-Origin', origin)
    headers.set('Access-Control-Allow-Credentials', 'true')
    appendVary(headers, 'Origin')
  }

  // CSP is HTML-only — adding it to a JSON response is harmless but
  // misleading in dev tools, and HTML is the only thing a CSP applies to.
  const contentType = headers.get('Content-Type') ?? ''
  if (contentType.toLowerCase().startsWith('text/html')) {
    headers.set('Content-Security-Policy', CSP_HEADER)
  }

  return new Response(resp.body, {
    status: resp.status,
    statusText: resp.statusText,
    headers,
  })
}

function appendVary(headers: Headers, value: string): void {
  const existing = headers.get('Vary')
  if (!existing) {
    headers.set('Vary', value)
    return
  }
  const parts = existing.split(',').map((s) => s.trim().toLowerCase())
  if (!parts.includes(value.toLowerCase())) {
    headers.set('Vary', `${existing}, ${value}`)
  }
}

// --- /internal/publish ---

// We HMAC-verify on this side AND on the DO side. The double-check costs
// almost nothing (one SubtleCrypto.sign) and means a misconfigured route
// can't accidentally expose the unguarded DO. The gateway uses
// constant-time comparison via timingSafeEqualHex.
async function handleInternalPublish(req: Request, env: Env): Promise<Response> {
  if (req.method !== 'POST') {
    return new Response('method not allowed', { status: 405 })
  }
  const sig = req.headers.get('X-Signature')
  if (!sig) {
    return new Response('missing signature', { status: 401 })
  }

  // Buffer the body so we can both verify and forward without re-reading
  // a consumed stream.
  const bodyBytes = new Uint8Array(await req.arrayBuffer())
  const expected = await hmacSha256Hex(env.INTERNAL_PUBLISH_SECRET, bodyBytes)
  if (!timingSafeEqualHex(sig, expected)) {
    return new Response('bad signature', { status: 401 })
  }

  // Forward to the DO. We rewrite the URL to /publish so the DO router
  // doesn't have to know about the /internal/ prefix.
  const id = env.FLEET_HUB.idFromName('global-fleet')
  const stub = env.FLEET_HUB.get(id)
  const internalReq = new Request('https://fleet-hub.internal/publish', {
    method: 'POST',
    headers: {
      'Content-Type': req.headers.get('Content-Type') ?? 'application/json',
      'X-Signature': sig,
    },
    body: bodyBytes,
  })
  return stub.fetch(internalReq)
}

// --- /ws/* ---

// upgradeToHub forwards the request to the DO's /upgrade handler. The DO
// is responsible for Origin + JWT verification because those checks need
// to be authoritative at the WebSocket boundary — the gateway is just
// the routing seam.
async function upgradeToHub(req: Request, env: Env): Promise<Response> {
  if (req.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
    return new Response('expected websocket upgrade', { status: 426 })
  }
  const id = env.FLEET_HUB.idFromName('global-fleet')
  const stub = env.FLEET_HUB.get(id)
  // Forward to a synthetic /upgrade path inside the DO. We preserve the
  // original request's headers (Origin, Cookie, Upgrade) so the DO sees
  // the upgrade context unchanged.
  const upstreamUrl = 'https://fleet-hub.internal/upgrade'
  const forwarded = new Request(upstreamUrl, req)
  return stub.fetch(forwarded)
}

// --- /api/* ---

// forwardToUpstream is the dev-mode proxy to the Go API. In production
// (TASK-025) the env binding is replaced with a Container service
// binding and this helper is rewritten to fetch the binding directly.
// The shape (same request method, same body, filtered headers) is kept
// identical so the swap is mechanical.
async function forwardToUpstream(req: Request, env: Env): Promise<Response> {
  // Filter hop-by-hop headers before forwarding either via service
  // binding or HTTP fallback. Both paths preserve method + body + path.
  const forwardedHeaders = new Headers()
  for (const [key, value] of req.headers) {
    if (HOP_BY_HOP_HEADERS.has(key.toLowerCase())) continue
    forwardedHeaders.set(key, value)
  }

  const init: RequestInit = {
    method: req.method,
    headers: forwardedHeaders,
    redirect: 'manual',
  }
  if (req.method !== 'GET' && req.method !== 'HEAD' && req.method !== 'OPTIONS') {
    init.body = req.body
  }

  // Service-binding path: production. env.FLEET_API.fetch goes
  // intra-Cloudflare without DNS or loop-detection issues that hit
  // worker → workers.dev fetches in the same account.
  if (env.FLEET_API) {
    return env.FLEET_API.fetch(new Request(req.url, init))
  }

  // HTTP fallback: dev only. Honour API_UPSTREAM_URL.
  const upstream = new URL(env.API_UPSTREAM_URL)
  const incoming = new URL(req.url)
  upstream.pathname = incoming.pathname
  upstream.search = incoming.search
  return fetch(upstream.toString(), init)
}

// --- crypto helpers (mirrors fleet-hub) ---

// Re-implemented locally instead of imported because the gateway worker
// is a separate bundle. Keeping the two copies byte-identical is enforced
// by the test that signs+sends through the gateway to the DO.
export function timingSafeEqualHex(a: string, b: string): boolean {
  if (a.length !== b.length) return false
  let mismatch = 0
  for (let i = 0; i < a.length; i++) {
    mismatch |= a.charCodeAt(i) ^ b.charCodeAt(i)
  }
  return mismatch === 0
}

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
  const sigBuf = await crypto.subtle.sign('HMAC', key, message)
  const bytes = new Uint8Array(sigBuf)
  const hex = new Array<string>(bytes.length)
  for (let i = 0; i < bytes.length; i++) {
    hex[i] = bytes[i].toString(16).padStart(2, '0')
  }
  return hex.join('')
}

// Exported for the unit test that drives the helpers directly.
export { handleInternalPublish, forwardToUpstream, upgradeToHub }

// FleetHub re-export — used ONLY in tests.
//
// Production: gateway/wrangler.toml binds FLEET_HUB via
// `script_name = "fleet-do-hub"`, which means Cloudflare's runtime
// resolves the class from the separately-deployed DO worker. Any
// `FleetHub` symbol in this bundle is dead code in production — the
// `script_name` lookup takes precedence.
//
// Tests: @cloudflare/vitest-pool-workers cannot host two TypeScript
// scripts in one isolate (Miniflare's auxiliary-workers feature only
// accepts pre-compiled JS). Re-exporting the class here gives the
// vitest config a single bundle that contains both the gateway's
// default export AND the DO class, so we can bind FLEET_HUB to the
// current worker in test mode and avoid the pre-build step.
//
// This is the standard pattern Cloudflare's own examples use when a
// gateway worker tests against an in-process DO.
export { FleetHub } from '../../fleet-hub/src/fleet-hub'
