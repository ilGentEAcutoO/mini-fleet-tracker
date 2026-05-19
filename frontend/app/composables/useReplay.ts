// Replay-mode playhead for vehicle position history (TASK-021, Bonus 3).
//
// Wraps a list of recorded `Position` samples in a `requestAnimationFrame`
// driven playhead. Consumers bind `currentMs` to a slider for scrubbing,
// observe `position` for the interpolated marker location, and toggle the
// loop via play/pause/toggle/seek. The composable is intentionally headless
// — it owns no DOM and emits no events; UI lives in `<ReplayTimeline />`
// and the page that mounts it.
//
// Why interpolate?
//
//   The simulator (TASK-019) ticks at ~1 Hz; the dashboard renders at the
//   browser's RAF cadence (~60 Hz). Without interpolation the marker would
//   teleport between recorded samples once per second, which reads as
//   "broken" even though the data is correct. We find the two samples that
//   bracket `currentMs` via binary search on each frame and linearly
//   interpolate lat/lng — visually the marker now glides along the trail
//   instead of jumping.
//
//   Linear (lat, lng) interpolation is geodesically wrong for very long
//   gaps (a great-circle interpolator would be more accurate), but at the
//   scale of a typical sample spacing (tens of metres at 1 Hz) the error is
//   sub-pixel on a city-zoom map. We accept it as the simpler primitive.
//
// Speed multiplier
//
//   `speed` is a ref<1 | 2 | 4> that multiplies the wall-clock delta on
//   each RAF tick. So at 2x, one wall-clock second advances `currentMs` by
//   two seconds of recorded time. The RAF loop itself still runs at the
//   browser's native cadence — we just scale the playhead step.
//
// Lifecycle
//
//   `onScopeDispose` cancels any in-flight RAF when the consumer's effect
//   scope tears down (component unmount or `effectScope` close). The pause
//   path is idempotent so repeated cleanup is safe.
//
// SSR
//
//   Safe to construct on the server (it only registers refs/watchers). The
//   RAF loop only starts when `play()` is called, which the UI guards
//   behind a click handler — i.e. client-only by construction. No SSR
//   guards needed.

import type { Position } from '~~/shared/types/domain'

export function useReplay(positions: MaybeRefOrGetter<Position[]>) {
  // Defensive sort — the caller usually passes a chronologically-ordered
  // array (the vehicle history page already reverses the DESC server
  // response), but a future caller might not, and the bracketing-pair
  // search below assumes monotonic `recorded_at`. `slice()` keeps us out
  // of the source array.
  const sorted = computed(() => {
    return toValue(positions).slice().sort((a, b) => a.recorded_at - b.recorded_at)
  })

  // Boundaries of the playhead range. When the list is empty we collapse
  // to a zero-width range so the slider input still has valid min/max
  // (browsers reject min > max).
  const boundsMs = computed(() => {
    const arr = sorted.value
    if (arr.length === 0) return { from: 0, to: 0 }
    return { from: arr[0]!.recorded_at, to: arr[arr.length - 1]!.recorded_at }
  })

  const currentMs = ref(0)
  const playing = ref(false)
  const speed = ref<1 | 2 | 4>(1)

  // Reset the playhead to the start whenever the input array changes. This
  // covers the dashboard flow: the manager picks a new from/to window,
  // `<ReplayTimeline :positions>` receives a fresh array, and we rewind to
  // the head so play() starts at the new beginning. `immediate: true` so
  // first-mount initialisation runs through the same path.
  watch(() => sorted.value, (s) => {
    currentMs.value = s.length > 0 ? s[0]!.recorded_at : 0
    playing.value = false
  }, { immediate: true })

  // RAF loop bookkeeping. `rafHandle === 0` is the sentinel "no frame
  // pending"; both browser RAF and our cleanup path use that to skip
  // redundant cancels. `lastWallMs === 0` resets each play() so the first
  // tick after un-pause does not advance by a stale delta from before.
  let rafHandle = 0
  let lastWallMs = 0

  function tick(wallMs: number) {
    if (!playing.value) return
    if (lastWallMs === 0) lastWallMs = wallMs
    const wallDelta = wallMs - lastWallMs
    lastWallMs = wallMs
    // Math.min clamps the playhead at the upper bound so we don't overshoot
    // when speed=4 and the user scrubbed close to the end. The follow-up
    // check then transitions to the paused-at-end state.
    currentMs.value = Math.min(
      boundsMs.value.to,
      currentMs.value + wallDelta * speed.value,
    )
    if (currentMs.value >= boundsMs.value.to) {
      playing.value = false
      rafHandle = 0
      return
    }
    rafHandle = requestAnimationFrame(tick)
  }

  function play() {
    if (playing.value) return
    if (sorted.value.length < 2) return // nothing to animate
    // Rewind-on-replay UX: if the playhead is parked at the end (the
    // tick() loop's natural exit), restart from the beginning instead of
    // becoming a no-op. Matches the behaviour of every media player.
    if (currentMs.value >= boundsMs.value.to) {
      currentMs.value = boundsMs.value.from
    }
    playing.value = true
    lastWallMs = 0
    rafHandle = requestAnimationFrame(tick)
  }

  function pause() {
    playing.value = false
    if (rafHandle !== 0) {
      cancelAnimationFrame(rafHandle)
      rafHandle = 0
    }
  }

  function toggle() {
    if (playing.value) pause()
    else play()
  }

  function seek(ms: number) {
    // Seeking always pauses — otherwise the next RAF tick would clobber
    // the user's chosen position with `currentMs + wallDelta * speed`. The
    // user can press play again after dropping the slider.
    pause()
    currentMs.value = Math.max(
      boundsMs.value.from,
      Math.min(boundsMs.value.to, ms),
    )
  }

  // Interpolated position at `currentMs`. Branches:
  //   - empty list  → null (consumer renders no marker)
  //   - single item → that item (degenerate; no segment to interpolate)
  //   - playhead at or past the head/tail → endpoint sample (no
  //     extrapolation; the bounds clamp above prevents this anyway, but
  //     the guard keeps the function correct under direct mutation of
  //     `currentMs` from outside)
  //   - otherwise → binary-search the bracketing pair, linearly
  //     interpolate lat/lng/recorded_at, keep other fields from the
  //     earlier sample (`id`, `vehicle_id`, `speed_kmh`, `created_at`)
  const position = computed<Position | null>(() => {
    const arr = sorted.value
    if (arr.length === 0) return null
    if (arr.length === 1) return arr[0]!
    const t = currentMs.value
    if (t <= arr[0]!.recorded_at) return arr[0]!
    if (t >= arr[arr.length - 1]!.recorded_at) return arr[arr.length - 1]!
    // Binary search for the largest `lo` such that arr[lo].recorded_at <= t.
    // The invariant `hi - lo > 1` exits with `lo` pointing at the lower
    // bracket and `hi` at the upper.
    let lo = 0
    let hi = arr.length - 1
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1
      if (arr[mid]!.recorded_at <= t) lo = mid
      else hi = mid
    }
    const a = arr[lo]!
    const b = arr[hi]!
    const span = b.recorded_at - a.recorded_at
    // `span > 0` guard handles duplicate-timestamp samples — fall back to
    // the earlier point so we don't divide by zero.
    const ratio = span > 0 ? (t - a.recorded_at) / span : 0
    return {
      ...a,
      lat: a.lat + (b.lat - a.lat) * ratio,
      lng: a.lng + (b.lng - a.lng) * ratio,
      recorded_at: t,
    }
  })

  // Cancel any pending RAF when the surrounding effect scope tears down
  // (component unmount, route change, manual scope dispose). pause() is
  // idempotent so the no-op path is cheap.
  onScopeDispose(pause)

  return { play, pause, toggle, seek, playing, currentMs, speed, position, boundsMs }
}
