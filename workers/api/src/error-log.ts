// Defense-in-depth helper for the fleet-api Container shim — TASK-058
// (security-review.md Workers M2).
//
// The Container base class's onError lifecycle hook receives an unknown
// from the runtime. The previous implementation passed that value as
// the second arg to console.error, which the Workers log pipeline
// renders by walking every enumerable property of the value — including
// `cause` (where application code commonly wraps request bodies /
// header values) and `stack` (which can carry path fragments and, in
// stitched async stacks, snippets of upstream-frame argument values).
//
// CF Workers logs are accessible to anyone with `Logs:Read` on the
// account; not a secret store but defense in depth says "don't put
// untrusted data there in the first place".
//
// This sanitiser yields ONLY the human-readable message — never the
// full object. The Error.message field is set by the throwing code
// and should already be a vetted, non-secret descriptor. For non-Error
// values we fall back to String(), which renders objects as
// '[object Object]' rather than enumerating their property bag.
export function safeContainerErrorMessage(err: unknown): string {
  if (err instanceof Error) {
    return err.message
  }
  return String(err)
}
