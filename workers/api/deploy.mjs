#!/usr/bin/env node
// Deploy wrapper for the fleet-api Container Worker.
//
// Why: wrangler's TOML parser does not interpolate environment variables,
// so we cannot put `${GIT_COMMIT}` directly into wrangler.toml's
// `image_vars`. This wrapper reads the current short SHA from git,
// splices it into a temporary deploy config, and invokes wrangler with
// `--config` pointing at the temp file. The committed wrangler.toml
// keeps its "dev" sentinel — if a healthz response ever shows
// "commit":"dev" in production, someone ran `wrangler deploy` directly
// and bypassed this wrapper.
//
// Why spawnSync with arg array (not exec): avoids shell interpolation
// for the path values we pass through, even though those values are not
// user-controlled. Defense-in-depth pattern enforced by the project's
// security hook.

import { execFileSync, spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync, rmSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..')
const wranglerBin = join(repoRoot, 'workers', 'node_modules', 'wrangler', 'bin', 'wrangler.js')
const srcPath = join(here, 'wrangler.toml')
const tmpPath = join(here, '.wrangler.deploy.toml')

const sha = execFileSync('git', ['rev-parse', '--short', 'HEAD'], { cwd: repoRoot })
  .toString()
  .trim()

console.log(`[deploy.mjs] GIT_COMMIT=${sha}`)

const src = readFileSync(srcPath, 'utf8')
const patched = src.replace(
  /^image_vars = \{ GIT_COMMIT = "[^"]*" \}$/m,
  `image_vars = { GIT_COMMIT = "${sha}" }`,
)

if (patched === src) {
  throw new Error(
    `[deploy.mjs] failed to splice GIT_COMMIT into ${srcPath} — the ` +
      `image_vars line did not match the expected pattern. Did the ` +
      `wrangler.toml layout change?`,
  )
}

writeFileSync(tmpPath, patched)
try {
  const result = spawnSync(
    process.execPath,
    [wranglerBin, 'deploy', '--config', tmpPath],
    { stdio: 'inherit', cwd: here },
  )
  if (result.status !== 0) {
    throw new Error(`[deploy.mjs] wrangler deploy exited with code ${result.status}`)
  }
} finally {
  rmSync(tmpPath, { force: true })
}
