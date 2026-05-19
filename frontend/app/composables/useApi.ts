// Centralised $fetch wrapper.
//
// Lives in `composables/` (not `utils/`) because it is a stateful factory:
// it reads `useRuntimeConfig()`, mounts request/response hooks, and on SSR
// reaches into the request context via `useRequestHeaders`. Pure helpers
// belong in `utils/`; anything that touches Nuxt composables belongs here.
//
// Responsibilities:
//   1. Send credentials (httpOnly session cookie set by the Go API) on every
//      request — same-origin in prod, dev relies on the CORS_ORIGIN config in
//      backend/.env (http://localhost:3000).
//   2. On SSR, forward the incoming Cookie header so server-side $fetch calls
//      reuse the user's session. On the client the browser handles cookies.
//   3. Inject X-CSRF-Token on mutating verbs (POST/PUT/PATCH/DELETE) using the
//      double-submit pattern — the backend sets a non-httpOnly `csrf_token`
//      cookie at login; we read it via document.cookie and echo it as a
//      header.
//   4. Short-circuit 410 demo_expired responses to /expired (TASK-030 wires
//      both backend and gateway to emit these; the frontend just reacts).
//
// Usage:
//   const api = useApi()
//   const res = await api<MeResponse>('/auth/me')

export const useApi = () => {
  const config = useRuntimeConfig()

  return $fetch.create({
    baseURL: config.public.apiBase,
    credentials: 'include',

    onRequest({ options }) {
      // SSR: forward the user's Cookie header so server-side fetches see the
      // session. On the client, the browser already sends cookies for us.
      if (import.meta.server) {
        const headers = useRequestHeaders(['cookie'])
        if (headers.cookie) {
          const h = new Headers(options.headers)
          h.set('cookie', headers.cookie)
          options.headers = h
        }
      }

      // CSRF (double-submit): only on mutating methods, only on the client.
      const method = (options.method ?? 'GET').toString().toUpperCase()
      if (
        import.meta.client &&
        ['POST', 'PUT', 'PATCH', 'DELETE'].includes(method)
      ) {
        const csrf = readCsrfToken()
        if (csrf) {
          const h = new Headers(options.headers)
          h.set('X-CSRF-Token', csrf)
          options.headers = h
        }
      }
    },

    onResponseError({ response }) {
      // Demo-expired short-circuit (TASK-030). Both backend (Go) and gateway
      // (Worker) return 410 Gone with `{ error: "demo_expired", ... }` after
      // 2026-05-31. Redirect to /expired unless we're already there to avoid
      // a loop.
      const body = response._data as { error?: string } | undefined
      if (response.status === 410 && body?.error === 'demo_expired') {
        if (import.meta.client && useRoute().path !== '/expired') {
          // navigateTo returns a Promise but we deliberately don't await it
          // here — onResponseError is a fire-and-forget hook.
          void navigateTo('/expired')
        }
      }
    },
  })
}

function readCsrfToken(): string | null {
  if (import.meta.server) return null
  const match = document.cookie.match(/(?:^|;\s*)csrf_token=([^;]+)/)
  return match ? decodeURIComponent(match[1]!) : null
}
