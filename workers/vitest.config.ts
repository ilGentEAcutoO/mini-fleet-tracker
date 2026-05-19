// Root vitest config for the workers monorepo.
//
// We split the test surface into two Vitest "projects" — one for the
// FleetHub Durable Object worker, one for the gateway worker. Each
// project loads its own wrangler.toml so the bindings (DO classes,
// vars) match production exactly.
//
// The gateway project does NOT use a cross-script DO binding in tests
// (the production wrangler.toml uses `script_name = "fleet-do-hub"` to
// point at a separately-deployed worker). Instead we pass an auxiliary
// worker via `miniflare.workers` so a single Miniflare instance hosts
// both scripts. The gateway can then resolve env.FLEET_HUB the same way
// it does in prod.
//
// References:
//   * https://developers.cloudflare.com/workers/testing/vitest-integration/
//   * @cloudflare/vitest-pool-workers v0.16.x — uses cloudflareTest plugin
//     + defineConfig (the v3 defineWorkersProject API was removed).

import { cloudflareTest } from '@cloudflare/vitest-pool-workers'
import { defineConfig } from 'vitest/config'

// Absolute path is needed so Miniflare's auxiliary-workers feature can
// load the DO worker source from disk. `import.meta.dirname` is the
// ESM-native equivalent of __dirname; Node 20.11+ exposes it natively.
const HERE = import.meta.dirname
const FLEET_HUB_ENTRY = `${HERE}/fleet-hub/src/fleet-hub.ts`

// Shared miniflare.bindings for every project. wrangler.toml does not
// declare secrets, so we inject them here so verifyJwt / handlePublish
// have deterministic values to compare against.
const TEST_BINDINGS = {
  JWT_SECRET: 'test-jwt-secret-32-byte-random-hex',
  INTERNAL_PUBLISH_SECRET: 'test-internal-publish-secret',
  ALLOWED_ORIGINS: 'http://localhost:3000,https://fleet-tracker.jairukchan.com',
}

export default defineConfig({
  test: {
    projects: [
      // --- FleetHub DO project -------------------------------------------------
      {
        plugins: [
          cloudflareTest({
            main: FLEET_HUB_ENTRY,
            wrangler: {
              configPath: `${HERE}/fleet-hub/wrangler.toml`,
            },
            miniflare: {
              bindings: { ...TEST_BINDINGS },
            },
          }),
        ],
        test: {
          name: 'fleet-hub',
          include: ['fleet-hub/test/**/*.test.ts'],
        },
      },

      // --- Gateway project -----------------------------------------------------
      // The gateway's wrangler.toml declares a cross-script DO binding
      // (script_name = "fleet-do-hub"). In production that resolves to
      // the separately-deployed DO worker. For tests we re-host the
      // FleetHub class inside the gateway's own bundle (see the
      // re-export at the bottom of gateway/src/index.ts) and override
      // the DO binding here so it resolves to the current worker. This
      // avoids needing a pre-compiled auxiliary worker (Miniflare's
      // `workers` array only accepts JS, not TS — vitest's Vite
      // transform only runs on `main`).
      {
        plugins: [
          cloudflareTest({
            wrangler: {
              configPath: `${HERE}/gateway/wrangler.toml`,
            },
            miniflare: {
              bindings: {
                ...TEST_BINDINGS,
                API_UPSTREAM_URL: 'http://upstream.example.com',
              },
              // Override the cross-script DO binding declared in
              // wrangler.toml. Omitting `scriptName` here binds FLEET_HUB
              // to the FleetHub class in the current worker bundle — the
              // one re-exported by gateway/src/index.ts for this purpose.
              durableObjects: {
                FLEET_HUB: { className: 'FleetHub' },
              },
            },
          }),
        ],
        test: {
          name: 'gateway',
          include: ['gateway/test/**/*.test.ts'],
        },
      },
    ],
  },
})
