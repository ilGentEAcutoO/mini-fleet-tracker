// Global route guard.
//
// Pattern: an explicit allow-list of public routes — any path NOT on the list
// requires an authenticated session. This is safer than the opposite (default
// public + opt-in protected via `definePageMeta({ middleware: 'auth' })`)
// because forgetting to mark a new page protected leaks data, while
// forgetting to mark a new page public only annoys an anonymous visitor with
// a /login redirect.
//
// `/expired` is in the allow-list because the api.ts interceptor sends users
// there on a 410 demo_expired response — even an unauthenticated viewer hit
// by that response must reach the page, not bounce to /login.

export default defineNuxtRouteMiddleware(async (to) => {
  const publicRoutes = new Set(['/', '/login', '/register', '/expired'])
  if (publicRoutes.has(to.path)) return

  const auth = useAuthStore()
  await auth.fetchMe()

  if (!auth.isAuthenticated) {
    return navigateTo({
      path: '/login',
      query: { redirect: to.fullPath },
    })
  }
})
