<script setup lang="ts">
// Default layout — chrome around every page.
//
// Header shows:
//   - Brand link (always)
//   - Authenticated nav: Dashboard, Vehicles (manager-only), Report (driver-only)
//   - Sign-out button (authenticated)
//   - Sign-in / Register links (anonymous)
//
// NuxtLink's built-in `active` class is `router-link-active`; we use it
// (via the `:to` href match) for the underline. The `exactActiveClass`
// makes the trailing-slash-vs-exact distinction explicit.
//
// Sign-out lives in the header (not on the dashboard page) so it's a
// consistent affordance across all authenticated routes.

const auth = useAuthStore()
const router = useRouter()

// `signingOut` doubles as a click guard (prevents double-click → second
// /auth/logout call with an already-blocklisted JTI → 401) and as the
// flag that swaps the button label/spinner. Logout always settles
// (auth.logout swallows network errors), so the finally clears the
// flag in both the happy path and any unexpected throw from the
// router.
const signingOut = ref(false)

async function signOut(): Promise<void> {
  if (signingOut.value) return
  signingOut.value = true
  try {
    await auth.logout()
    await router.push('/login')
  }
  finally {
    signingOut.value = false
  }
}
</script>

<template>
  <div class="min-h-screen bg-background text-foreground">
    <DemoBanner />
    <header class="border-b border-border">
      <div class="container mx-auto flex h-14 items-center gap-6 px-4">
        <NuxtLink to="/" class="font-semibold tracking-tight">
          Mini Fleet Tracker
        </NuxtLink>
        <ClientOnly>
          <nav
            v-if="auth.isAuthenticated"
            class="hidden md:flex items-center gap-4 text-sm"
          >
            <NuxtLink
              to="/dashboard"
              active-class="text-foreground font-medium"
              class="text-muted-foreground hover:text-foreground transition-colors"
            >
              Dashboard
            </NuxtLink>
            <NuxtLink
              v-if="auth.isManager"
              to="/dashboard/vehicles"
              active-class="text-foreground font-medium"
              class="text-muted-foreground hover:text-foreground transition-colors"
            >
              Vehicles
            </NuxtLink>
            <NuxtLink
              v-if="auth.user?.role === 'driver'"
              to="/driver/report"
              active-class="text-foreground font-medium"
              class="text-muted-foreground hover:text-foreground transition-colors"
            >
              Report
            </NuxtLink>
          </nav>
        </ClientOnly>
        <ClientOnly>
          <template #fallback>
            <div class="ml-auto flex items-center gap-3 text-sm">
              <NuxtLink to="/login" class="text-muted-foreground hover:text-foreground">
                Sign in
              </NuxtLink>
              <NuxtLink
                to="/register"
                class="text-muted-foreground hover:text-foreground"
              >
                Register
              </NuxtLink>
            </div>
          </template>
          <div class="ml-auto flex items-center gap-3 text-sm">
            <template v-if="auth.isAuthenticated">
              <span
                class="hidden sm:inline text-xs text-muted-foreground"
                :title="auth.user?.email"
              >
                {{ auth.user?.email }}
              </span>
              <Button
                variant="outline"
                size="sm"
                :disabled="signingOut"
                @click="signOut"
              >
                <span v-if="signingOut" class="inline-flex items-center gap-1.5">
                  <svg
                    class="h-3 w-3 animate-spin"
                    viewBox="0 0 24 24"
                    fill="none"
                    aria-hidden="true"
                  >
                    <circle
                      cx="12"
                      cy="12"
                      r="10"
                      stroke="currentColor"
                      stroke-width="3"
                      stroke-linecap="round"
                      stroke-dasharray="30 70"
                    />
                  </svg>
                  Signing out…
                </span>
                <span v-else>Sign out</span>
              </Button>
            </template>
            <template v-else>
              <NuxtLink to="/login" class="text-muted-foreground hover:text-foreground">
                Sign in
              </NuxtLink>
              <NuxtLink
                to="/register"
                class="text-muted-foreground hover:text-foreground"
              >
                Register
              </NuxtLink>
            </template>
          </div>
        </ClientOnly>
      </div>
    </header>
    <main class="container mx-auto px-4 py-8">
      <slot />
    </main>
    <ClientOnly>
      <Sonner position="top-right" :rich-colors="true" />
    </ClientOnly>
  </div>
</template>
