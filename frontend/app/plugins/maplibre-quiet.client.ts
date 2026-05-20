// Quiet MapLibre's known-noisy worker warning.
//
// MapLibre GL JS 5.x + OpenFreeMap's "liberty" vector style emit a
// per-tile-feature warning like:
//   "Expected value to be of type number, but found null instead."
// from the worker that parses the style expressions. The warning is the
// SDK observing that a sub-expression evaluated to `null` where the
// schema declared `number` — for OpenFreeMap that happens on a handful
// of optional OSM fields (e.g. some POI elevation/population tags). It
// is informational, repeats on every tile, and pollutes the console for
// the portfolio reviewer.
//
// We override `console.warn` at the page level to filter just this exact
// message. Other warnings (network, Vue, etc) still surface unchanged.
// Limiting the filter to the exact substring keeps the blast radius
// narrow — any new warning shape would still reach the console.
export default defineNuxtPlugin(() => {
  const originalWarn = console.warn
  const SUPPRESSED = 'Expected value to be of type number, but found null instead.'
  console.warn = (...args: unknown[]) => {
    if (args.length && typeof args[0] === 'string' && args[0].includes(SUPPRESSED)) {
      return
    }
    originalWarn.apply(console, args)
  }
})
