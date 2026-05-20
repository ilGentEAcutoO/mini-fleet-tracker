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

async function signOut(): Promise<void> {
  await auth.logout()
  await router.push('/login')
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
                @click="signOut"
              >
                Sign out
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
