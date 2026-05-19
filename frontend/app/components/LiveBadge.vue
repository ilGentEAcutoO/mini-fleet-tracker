<script setup lang="ts">
// Live-channel status pill. Renders a dot + label that mirrors the current
// `useFleetStore().status` reactive state. Three visual variants:
//
//   open                          → "Live"          green, pulsing dot
//   connecting | reconnecting     → "Reconnecting…" amber, pulsing dot
//   closed | idle                 → "Offline"       slate, static dot
//
// The pulse animation is a Tailwind `animate-ping` halo behind the dot — it
// communicates "transient state, please wait" without an explicit spinner.
// Static for the terminal state because the user shouldn't expect movement
// after a manual disconnect or after autoReconnect's retries exhaust.
//
// Lives at the component root (not inside a sub-folder) so the dashboard
// page imports it via Nuxt's auto-import as `<LiveBadge />`. Single-file,
// presentational, no props — it reads the store directly.

const fleet = useFleetStore()

interface BadgeVariant {
  label: string
  color: string
  pulse: boolean
}

// `switch` on a discriminated union of literal strings gives us exhaustive
// checking under TypeScript strict — if a new ConnectionState lands without
// a branch, the compiler flags the missing case via the `never` fallthrough.
const variant = computed<BadgeVariant>(() => {
  switch (fleet.status) {
    case 'open':
      return { label: 'Live', color: 'bg-emerald-500', pulse: true }
    case 'connecting':
    case 'reconnecting':
      return { label: 'Reconnecting…', color: 'bg-amber-500', pulse: true }
    case 'closed':
    case 'idle':
      return { label: 'Offline', color: 'bg-slate-400', pulse: false }
  }
  // Unreachable under strict typing; compiler-pleaser fallback so the
  // computed always returns a BadgeVariant value.
  return { label: 'Offline', color: 'bg-slate-400', pulse: false }
})
</script>

<template>
  <span
    class="inline-flex items-center gap-2 rounded-full bg-muted px-3 py-1 text-xs font-medium"
    role="status"
    :aria-label="`Connection status: ${variant.label}`"
  >
    <span class="relative flex h-2 w-2">
      <span
        v-if="variant.pulse"
        :class="[
          'absolute inline-flex h-full w-full rounded-full opacity-75 animate-ping',
          variant.color,
        ]"
      />
      <span
        :class="['relative inline-flex h-2 w-2 rounded-full', variant.color]"
      />
    </span>
    {{ variant.label }}
  </span>
</template>
