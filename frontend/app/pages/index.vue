<script setup lang="ts">
definePageMeta({ layout: 'default' })
useHead({ title: 'Mini Fleet Tracker — Live demo' })

const auth = useAuthStore()
const router = useRouter()
const { loginAs, submitting, errorMessage } = useDemoLogin()
const apiBase = useRuntimeConfig().public.apiBase

// State machine for the landing page render. `checking` shows the splash
// while we decide what to do; `logging-in` keeps the splash up while the
// auto-login round-trip completes; `show-marketing` reveals the static
// landing (buttons + GitHub links) only when auto-login has failed so the
// user has explicit controls to try again or pick a different role.
type LandingState = 'checking' | 'logging-in' | 'show-marketing'
const state = ref<LandingState>('checking')

// Progressive splash status — rotating message every ~1.2s so the splash
// reads as active progress instead of a static "loading" string. The
// recruiter/viewer perceives momentum even though wall-clock is
// dominated by the CF Container cold-start (~3-5s).
const statusMessages = [
  'Warming the backend container…',
  'Authenticating session…',
  'Loading fleet data…',
  'Almost there…',
]
const statusIndex = ref(0)
const statusMessage = computed(() => statusMessages[statusIndex.value])
let statusTimer: ReturnType<typeof setInterval> | null = null

// If already authenticated (e.g. browser remembers cookies from a prior visit),
// jump straight to the dashboard. Client-only so SSR returns the landing for
// SEO and the redirect happens during hydration.
//
// Otherwise: auto-trigger manager login so a fresh visitor lands on the
// dashboard without an extra click. While the auto-login is in flight, the
// splash stays visible (kills the flash-of-marketing-content during the
// ~5–10s cold-start). Only if loginAs fails do we drop down to the
// marketing template so the user sees the buttons + error pill.
//
// Multi-prewarm: fire 3 GET /healthz requests in parallel with mount. CF
// Container with max_instances=1 won't actually scale, but multiple
// in-flight requests can help the scheduler complete the cold-start
// sooner (and at worst is a few harmless extra round-trips). All
// fire-and-forget so they overlap with the auth check.
onMounted(async () => {
  // Multi-prewarm: 3 parallel pings to nudge the container awake.
  for (let i = 0; i < 3; i++) {
    fetch(`${apiBase}/healthz`).catch(() => {})
  }

  // Rotate the splash message every 1.2s. Stops on success (page
  // unmounts after router.push) or on the `show-marketing` transition
  // via onBeforeUnmount cleanup below.
  statusTimer = setInterval(() => {
    if (statusIndex.value < statusMessages.length - 1) {
      statusIndex.value += 1
    }
  }, 1200)

  if (!auth.fetched) await auth.fetchMe()
  if (auth.isAuthenticated) {
    await router.push(auth.isManager ? '/dashboard' : '/driver/report')
    return
  }
  state.value = 'logging-in'
  await loginAs('manager')
  // If loginAs succeeded, the router has navigated away and this page is
  // unmounted — we never get here. If it failed, errorMessage is set and
  // we want the user to see the marketing template with the error pill.
  if (errorMessage.value) {
    state.value = 'show-marketing'
  }
})

onBeforeUnmount(() => {
  if (statusTimer !== null) {
    clearInterval(statusTimer)
    statusTimer = null
  }
})

// Reactive fallback: if errorMessage flips on after a manual click during
// `logging-in`, surface the buttons + error.
watch(errorMessage, (msg) => {
  if (msg && state.value !== 'show-marketing') {
    state.value = 'show-marketing'
  }
})
</script>

<template>
  <section v-if="state !== 'show-marketing'" class="mx-auto max-w-md space-y-6 text-center py-16">
    <div class="flex justify-center">
      <div
        class="h-10 w-10 animate-spin rounded-full border-4 border-muted border-t-primary"
        role="status"
        aria-label="Loading"
      />
    </div>
    <div class="space-y-3">
      <h1 class="text-3xl font-semibold tracking-tight">
        Mini Fleet Tracker
      </h1>
      <p class="text-muted-foreground" aria-live="polite">
        {{ statusMessage }}
      </p>
      <div class="flex justify-center">
        <div class="w-64 h-1 bg-muted rounded-full overflow-hidden">
          <div class="splash-progress h-full bg-foreground origin-left" />
        </div>
      </div>
      <p class="text-xs text-muted-foreground">
        First-load warms the demo backend (~5–10s). Want a different role?
        <NuxtLink to="/login" class="underline">
          /login
        </NuxtLink>
      </p>
    </div>
  </section>

  <section v-else class="mx-auto max-w-2xl space-y-8 text-center py-8">
    <div class="space-y-3">
      <h1 class="text-4xl font-semibold tracking-tight">
        Mini Fleet Tracker
      </h1>
      <p class="text-muted-foreground text-lg">
        Real-time fleet visibility built on Go (Fiber), Vue 3 (Nuxt 4), and
        Cloudflare's edge.
      </p>
      <p class="text-sm text-muted-foreground">
        Demo runs until 2026-05-31 · Shared demo accounts · One click to enter.
      </p>
    </div>

    <div class="mx-auto max-w-sm space-y-3">
      <Button
        type="button"
        class="w-full"
        :disabled="!!submitting"
        data-testid="landing-try-manager"
        @click="loginAs('manager')"
      >
        {{ submitting === 'manager' ? 'Signing in…' : 'Try as Manager → Dashboard' }}
      </Button>
      <Button
        type="button"
        variant="secondary"
        class="w-full"
        :disabled="!!submitting"
        data-testid="landing-try-driver"
        @click="loginAs('driver')"
      >
        {{ submitting === 'driver' ? 'Signing in…' : 'Try as Driver → Report position' }}
      </Button>
      <p
        v-if="errorMessage"
        class="text-sm text-destructive"
        role="alert"
      >
        {{ errorMessage }}
      </p>
      <p class="text-xs text-muted-foreground pt-2">
        First click warms the demo backend (~5–10s). Manual sign-in is at
        <NuxtLink to="/login" class="underline">
          /login
        </NuxtLink>.
      </p>
    </div>

    <div class="text-sm text-muted-foreground pt-4 space-x-4">
      <a
        href="https://github.com/ilGentEAcutoO/mini-fleet-tracker"
        target="_blank"
        rel="noopener noreferrer"
        class="underline"
      >
        Source on GitHub
      </a>
      <span aria-hidden="true">·</span>
      <a
        href="https://github.com/ilGentEAcutoO/mini-fleet-tracker/blob/main/ARCHITECTURE.md"
        target="_blank"
        rel="noopener noreferrer"
        class="underline"
      >
        Architecture
      </a>
    </div>
  </section>
</template>

<style scoped>
/*
 * Splash progress bar — fills over 5s on a single forward run. Pure
 * aesthetics: the actual login may finish faster or slower; the bar
 * provides a sense of progress so the wait reads as "moving" rather
 * than "stuck". Scoped to this component so the keyframe doesn't
 * leak into the global tailwind layer.
 */
@keyframes splash-progress {
  0% {
    transform: scaleX(0);
  }
  100% {
    transform: scaleX(1);
  }
}

.splash-progress {
  animation: splash-progress 5s ease-out forwards;
}
</style>
