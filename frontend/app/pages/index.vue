<script setup lang="ts">
definePageMeta({ layout: 'default' })
useHead({ title: 'Mini Fleet Tracker — Live demo' })

const auth = useAuthStore()
const router = useRouter()
const { loginAs, submitting, errorMessage } = useDemoLogin()

// If already authenticated (e.g. browser remembers cookies from a prior visit),
// jump straight to the dashboard. Client-only so SSR returns the landing for
// SEO and the redirect happens during hydration.
//
// Otherwise: auto-trigger manager login so a fresh visitor lands on the
// dashboard without an extra click. The "Try as Driver" button stays
// visible during the ~1s auto-login window for users who want the driver
// surface instead — they can also navigate via /login at any time. Demo
// expiry (2026-05-31) is enforced server-side independent of this flow.
onMounted(async () => {
  if (!auth.fetched) await auth.fetchMe()
  if (auth.isAuthenticated) {
    await router.push(auth.isManager ? '/dashboard' : '/driver/report')
    return
  }
  loginAs('manager')
})
</script>

<template>
  <section class="mx-auto max-w-2xl space-y-8 text-center py-8">
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
