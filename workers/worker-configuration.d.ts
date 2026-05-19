// Ambient type for the `env` re-exported from "cloudflare:test" and
// "cloudflare:workers". The vitest-pool-workers types module declares
// `env: Cloudflare.Env`, but the `Cloudflare.Env` shape itself is what
// the developer (us) defines. Production code already declares its own
// `Env` interface per worker; this file extends the `Cloudflare`
// namespace so the test environment sees the right keys.
//
// We use the broadest superset (union of the two workers' bindings) so
// both projects compile against the same global. Per-test typing is
// recovered by importing the worker-specific `Env` directly when needed.
declare namespace Cloudflare {
  interface Env {
    // Shared secrets (both workers)
    JWT_SECRET: string
    INTERNAL_PUBLISH_SECRET: string
    ALLOWED_ORIGINS: string

    // Gateway-only var (left required to avoid optional plumbing in
    // tests; the fleet-hub project ignores it).
    API_UPSTREAM_URL: string

    // The Durable Object binding shared by both projects. We type it as
    // `DurableObjectNamespace<unknown>` here because the cloudflare:test
    // global doesn't know which class it points at; per-test code casts
    // to the concrete FleetHub when calling runInDurableObject.
    FLEET_HUB: DurableObjectNamespace
  }
}
