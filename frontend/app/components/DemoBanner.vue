<script setup lang="ts">
// Site-wide demo lifecycle banner.
//
// The visible-warning is for the human visitor, not for the per-request
// expiry path — that's owned by `useApi.ts`'s 410 interceptor +
// `pages/expired.vue`. This component reads the same literal cutoff so
// the policy lives in two places at most (here for the human, the
// backend/Worker constants for the wire).
//
// Visibility rules:
//   * Hidden until the last 7 days of the demo (the cutoff is well-known
//     via the README; an always-on banner would be noisy).
//   * Switches to "ended" state once we're past the cutoff. A visitor
//     who lands on a non-`/expired` page after expiry sees this
//     immediately, even before any API call would redirect them.
//
// The cutoff is constructed at module load. SSR + hydration both see the
// same numerical comparison (no `client-only` wrapper needed); a small
// risk of hydration mismatch exists if the SSR render straddles the
// cutoff to the millisecond, which is a non-issue in practice.

const expiresAt = new Date('2026-05-31T23:59:59+07:00')
const now = new Date()
const msLeft = expiresAt.getTime() - now.getTime()
const daysLeft = Math.floor(msLeft / (24 * 60 * 60 * 1000))

const visible = daysLeft >= 0 && daysLeft <= 7
const expired = msLeft < 0
</script>

<template>
  <div
    v-if="visible || expired"
    class="bg-amber-50 border-b border-amber-200 px-4 py-2 text-center text-sm text-amber-900"
    role="status"
  >
    <template v-if="expired">
      Live demo has ended.
      <a
        href="https://github.com/ilGentEAcutoO/mini-fleet-tracker"
        class="underline font-medium"
      >View source on GitHub</a>.
    </template>
    <template v-else-if="daysLeft === 0">
      Live demo expires <strong>today</strong> at 23:59 GMT+7.
    </template>
    <template v-else>
      Live demo expires in
      <strong>{{ daysLeft }} day{{ daysLeft === 1 ? '' : 's' }}</strong>
      (2026-05-31).
    </template>
  </div>
</template>
