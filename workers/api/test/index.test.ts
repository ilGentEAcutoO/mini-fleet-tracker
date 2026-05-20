// fleet-api Container Worker tests — pure helpers only.
//
// The api worker is a thin Container shim; it has no business logic
// reachable from the request path (every fetch is delegated to the
// FLEET_API Durable Object). The only logic worth testing in this
// surface is the error-log sanitiser introduced by TASK-058
// (security-review.md Workers M2): the Container's onError hook used
// to dump the entire error object to console.error, which would echo
// any user-controlled fragments wrapped in the error's `cause`/`stack`
// into Workers logs verbatim.
//
// We test the pure helper `safeContainerErrorMessage` directly rather
// than spinning up a CF Containers runtime under vitest-pool-workers
// — the helper is the security boundary, the lifecycle hook is just a
// thin caller.

import { describe, it, expect } from 'vitest'
// Imported from a dedicated module (not src/index.ts) so the test file
// stays in plain Node and never pulls in @cloudflare/containers (which
// requires the cloudflare:workers runtime module unavailable outside
// vitest-pool-workers).
import { safeContainerErrorMessage } from '../src/error-log'

describe('safeContainerErrorMessage (TASK-058)', () => {
  it('returns only the message for a plain Error', () => {
    expect(safeContainerErrorMessage(new Error('boom'))).toBe('boom')
  })

  it('does not include the cause field of an Error', () => {
    // The cause field is where user-controlled fragments (request
    // bodies, header values) most commonly leak into a thrown error
    // when application code does `throw new Error(msg, { cause: req })`.
    // The sanitiser MUST strip it.
    const err = new Error('upstream failed', {
      cause: { sensitiveData: 'jwt-token-abc123', body: 'password=hunter2' },
    })
    const out = safeContainerErrorMessage(err)
    expect(out).toBe('upstream failed')
    expect(out).not.toContain('sensitiveData')
    expect(out).not.toContain('jwt-token-abc123')
    expect(out).not.toContain('password')
    expect(out).not.toContain('hunter2')
  })

  it('does not include the stack of an Error', () => {
    // Stacks can leak path fragments and, in stitched async stacks,
    // captured argument values from upstream frames.
    const err = new Error('container crashed')
    err.stack =
      'Error: container crashed\n    at handler (/srv/secret-path/index.js:42:7)'
    const out = safeContainerErrorMessage(err)
    expect(out).toBe('container crashed')
    expect(out).not.toContain('secret-path')
    expect(out).not.toContain('handler')
  })

  it('falls back to String() for non-Error values without dumping properties', () => {
    // A bare string thrown as an error.
    expect(safeContainerErrorMessage('plain string failure')).toBe(
      'plain string failure',
    )
  })

  it('does not dump arbitrary object properties when given a non-Error object', () => {
    // If the runtime hands us something exotic (a frozen object, a
    // thrown plain object), String() yields '[object Object]' — not
    // the property bag. This is the desired defense-in-depth behaviour.
    const exotic = {
      message: 'pretty',
      secret: 'should-never-appear',
      authCookie: 'session=abc.def.ghi',
    }
    const out = safeContainerErrorMessage(exotic)
    expect(out).not.toContain('should-never-appear')
    expect(out).not.toContain('session=abc.def.ghi')
    expect(out).not.toContain('authCookie')
  })

  it('handles null and undefined safely', () => {
    expect(safeContainerErrorMessage(null)).toBe('null')
    expect(safeContainerErrorMessage(undefined)).toBe('undefined')
  })
})
