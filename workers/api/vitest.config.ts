// fleet-api Container shim — standalone vitest config.
//
// The repo-level vitest config (workers/vitest.config.ts) wires the
// fleet-hub DO and gateway worker into @cloudflare/vitest-pool-workers.
// The api worker is a thin Container shim and its only testable surface
// is the pure error-log sanitiser (TASK-058), so we run it under the
// plain Node environment to keep CI fast and free of CF-runtime startup
// cost. Run with:
//
//   pnpm exec vitest run --config api/vitest.config.ts
//
// from the workers/ directory.
//
// This config deliberately does NOT touch workers/vitest.config.ts (held
// by another teammate under the File Lock Registry).

import { defineConfig } from 'vitest/config'

// Anchor the project root to this config file's directory. Combined
// with the explicit `--root` flag at the call site, this lets vitest
// discover the `test/**` files no matter where the command is invoked
// from (we invoke it from workers/ as
//   pnpm exec vitest run --config api/vitest.config.ts).
const HERE = import.meta.dirname

export default defineConfig({
  root: HERE,
  test: {
    name: 'api',
    include: ['test/**/*.test.ts'],
    environment: 'node',
  },
})
