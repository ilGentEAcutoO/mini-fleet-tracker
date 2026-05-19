<script setup lang="ts">
// Replay-mode UI shell for the vehicle history page (TASK-021, Bonus 3).
//
// Stateless wrapper around `useReplay()`. Renders the playback controls
// (play/pause button, speed selector, current-time readout) and a native
// `<input type="range">` scrubber, then exposes the interpolated
// `position` via a scoped slot so the page-owner can render a marker on
// their own MapView without this component reaching into the SDK.
//
// Why a native <input type="range"> instead of a shadcn-vue Slider?
//
//   The shadcn-vue Slider primitive would pull `radix-vue` Slider into
//   the bundle, and `pnpm-lock.yaml` is a shared file across the three
//   parallel bonus tasks (TASK-020 geofence, TASK-021 replay, TASK-022 R2
//   upload). Adding a new dep here would race with the other two
//   teammates' lockfile updates. The native `<input type="range">` styled
//   with `accent-primary` gets us a serviceable scrubber with zero
//   lockfile churn — and is fully accessible by default (keyboard arrow
//   keys + screen-reader announcements come for free).
//
// Slot contract
//
//   The default slot receives `{ position, playing, currentMs, boundsMs }`.
//   Consumers typically render a `<MapView :positions="markers" />`
//   inside the slot where `markers` is a 1-entry Map keyed by vehicle id
//   pointing at `position`. The component does NOT render a map itself —
//   the page-owner already has one for the history polyline, and we want
//   the replay marker to ride on the same map instance.
//
// defineExpose for refs
//
//   `defineExpose` lets a parent grab `position` via a `ref` on the
//   component (`const replayRef = ref<InstanceType<typeof
//   ReplayTimeline>>()`). This is the alternative integration path for
//   pages that prefer template refs over scoped slots — both work.

import type { Position } from '~~/shared/types/domain'

interface Props {
  positions: Position[]
}
const props = defineProps<Props>()

// `() => props.positions` keeps the composable's `toValue` happy and
// preserves prop reactivity. Passing `props.positions` directly would
// snapshot the array at mount time and miss subsequent re-fetches when
// the manager changes the date range.
// Only the controls bound in the template are destructured here. `play`
// and `pause` are also exposed by the composable, but the template uses
// `toggle` for the play/pause button (one click handler covers both) and
// `seek` for the scrubber, so the rest is intentionally elided.
const {
  toggle,
  seek,
  playing,
  currentMs,
  speed,
  position,
  boundsMs,
} = useReplay(() => props.positions)

const min = computed(() => boundsMs.value.from)
const max = computed(() => boundsMs.value.to)
// Two-point minimum: with a single sample there is no segment to animate
// over. We still render the control row so the layout doesn't jump when
// data arrives, but disable the inputs and explain why.
const hasData = computed(() => props.positions.length >= 2)

function fmtTime(ms: number): string {
  if (!ms) return '—'
  return new Date(ms).toLocaleTimeString()
}

function onScrub(e: Event) {
  const target = e.target as HTMLInputElement
  seek(Number(target.value))
}

// Exposed for parents that prefer the template-ref integration path over
// the scoped slot. Both produce the same interpolated `position` ref.
defineExpose({ position, playing, currentMs })
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-center gap-2 flex-wrap">
      <Button
        type="button"
        :disabled="!hasData"
        variant="outline"
        @click="toggle"
      >
        {{ playing ? 'Pause' : 'Play' }}
      </Button>
      <label class="sr-only" for="replay-speed">Playback speed</label>
      <!-- Native <select> mirrors the project's pattern from register.vue
           (role picker) and pages/dashboard/vehicles/index.vue (driver
           picker): for a small fixed list of options, the browser native
           control is more accessible than a custom popover and avoids a
           lockfile change. -->
      <select
        id="replay-speed"
        v-model.number="speed"
        :disabled="!hasData"
        class="h-8 rounded-md border border-input bg-background px-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
      >
        <option :value="1">
          1x
        </option>
        <option :value="2">
          2x
        </option>
        <option :value="4">
          4x
        </option>
      </select>
      <span class="text-sm text-muted-foreground tabular-nums">
        {{ fmtTime(currentMs) }}
      </span>
    </div>
    <input
      type="range"
      :min="min"
      :max="max"
      :value="currentMs"
      step="1000"
      :disabled="!hasData"
      aria-label="Replay timeline scrubber"
      class="w-full accent-primary disabled:cursor-not-allowed disabled:opacity-50"
      @input="onScrub"
    >
    <p v-if="!hasData" class="text-sm text-muted-foreground">
      Replay needs at least two recorded points. Adjust the date range above.
    </p>
    <slot
      :position="position"
      :playing="playing"
      :current-ms="currentMs"
      :bounds-ms="boundsMs"
    />
  </div>
</template>
