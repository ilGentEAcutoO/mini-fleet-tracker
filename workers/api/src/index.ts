// fleet-api — Cloudflare Container Worker wrapping the Go Fiber backend.
//
// The Worker's responsibility is minimal: hand every request off to the
// Container instance bound at env.FLEET_API. The Container class extends
// the @cloudflare/containers Container base — that base handles
// hibernation, the HTTP bridge to localhost:8080 inside the container,
// and lifecycle hooks.
//
// envVars are populated at instance construction from the union of
// wrangler.toml [vars] and the secrets uploaded via `wrangler secret put`.
// Anything we reference in the Go backend's config.Load() must appear
// here either as a [vars] entry (non-sensitive) or as a wrangler secret
// (sensitive — never in this file or wrangler.toml).
import { Container } from '@cloudflare/containers'
import { safeContainerErrorMessage } from './error-log'

interface Env {
  FLEET_API: DurableObjectNamespace
  // [vars] from wrangler.toml
  APP_ENV: string
  PORT: string
  LOG_LEVEL: string
  CORS_ORIGIN: string
  CF_ACCOUNT_ID: string
  D1_DATABASE_ID: string
  KV_SESSIONS_NAMESPACE_ID: string
  KV_RATELIMITS_NAMESPACE_ID: string
  KV_QUOTAS_NAMESPACE_ID: string
  R2_ENDPOINT: string
  R2_BUCKET_NAME: string
  DO_PUBLISH_URL: string
  // secrets from `wrangler secret put`
  JWT_SECRET: string
  INTERNAL_PUBLISH_SECRET: string
  CF_API_TOKEN: string
  R2_ACCESS_KEY_ID: string
  R2_SECRET_ACCESS_KEY: string
}

export class FleetAPI extends Container<Env> {
  // The Go Fiber server listens on $PORT (default 8080).
  defaultPort = 8080

  // Hibernate the container after 30 seconds of inactivity. Cold start
  // pays a small first-request latency in exchange for $0 idle cost
  // outside of the demo window. Combined with max_instances=1 + the
  // hard expiration on 2026-05-31, total spend stays bounded.
  sleepAfter = '30s'

  override envVars = (() => {
    const e = this.env
    return {
      APP_ENV: e.APP_ENV,
      PORT: e.PORT,
      LOG_LEVEL: e.LOG_LEVEL,
      CORS_ORIGIN: e.CORS_ORIGIN,
      CF_ACCOUNT_ID: e.CF_ACCOUNT_ID,
      D1_DATABASE_ID: e.D1_DATABASE_ID,
      KV_SESSIONS_NAMESPACE_ID: e.KV_SESSIONS_NAMESPACE_ID,
      KV_RATELIMITS_NAMESPACE_ID: e.KV_RATELIMITS_NAMESPACE_ID,
      KV_QUOTAS_NAMESPACE_ID: e.KV_QUOTAS_NAMESPACE_ID,
      R2_ENDPOINT: e.R2_ENDPOINT,
      R2_BUCKET_NAME: e.R2_BUCKET_NAME,
      DO_PUBLISH_URL: e.DO_PUBLISH_URL,
      JWT_SECRET: e.JWT_SECRET,
      INTERNAL_PUBLISH_SECRET: e.INTERNAL_PUBLISH_SECRET,
      CF_API_TOKEN: e.CF_API_TOKEN,
      R2_ACCESS_KEY_ID: e.R2_ACCESS_KEY_ID,
      R2_SECRET_ACCESS_KEY: e.R2_SECRET_ACCESS_KEY,
    }
  })()

  override onStart() {
    console.log('FleetAPI container started')
  }

  override onError(err: unknown) {
    // TASK-058 (security-review.md Workers M2): log only the message,
    // never the full error object. See safeContainerErrorMessage for
    // rationale (avoids leaking err.cause / err.stack into Workers
    // logs if application code wraps user-controlled input).
    console.error(
      'FleetAPI container error:',
      safeContainerErrorMessage(err),
    )
  }
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    // Single-instance routing: every request maps to the same Container
    // instance keyed by the literal "global". A future iteration could
    // shard by tenant; this demo is single-tenant.
    const container = env.FLEET_API.get(env.FLEET_API.idFromName('global'))
    return container.fetch(request)
  },
}
