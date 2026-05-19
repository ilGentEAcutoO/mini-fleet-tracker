// Pinia setup store for the authenticated session.
//
// Holds the current `Driver` plus a `fetched` latch so the global middleware
// only calls `/auth/me` once per page load. State is reactive so any consumer
// (`<header>` nav, dashboards, route guards) can read user/isAuthenticated/
// isManager without prop-drilling.
//
// All network calls go through `useApi()` from `~/composables/useApi.ts` so
// the SSR cookie forwarding, CSRF injection, and demo-expired interceptor
// apply uniformly.

import { defineStore } from 'pinia'
import type { Driver } from '~~/shared/types/domain'
import type { LoginRequest, RegisterRequest } from '~~/shared/types/api'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<Driver | null>(null)
  // `fetched` flips to true after the first /auth/me round-trip OR after a
  // successful login/register. It prevents repeat /auth/me calls on every
  // subsequent route change.
  const fetched = ref(false)

  const isAuthenticated = computed(() => user.value !== null)
  const isManager = computed(() => user.value?.role === 'manager')

  const api = useApi()

  async function login(payload: LoginRequest): Promise<void> {
    const { user: u } = await api<{ user: Driver }>('/auth/login', {
      method: 'POST',
      body: payload,
    })
    user.value = u
    fetched.value = true
  }

  async function register(payload: RegisterRequest): Promise<void> {
    await api<{ user: Driver }>('/auth/register', {
      method: 'POST',
      body: payload,
    })
    // Auto-login after register for smoother UX — the register endpoint does
    // not set cookies, so we follow up with a login call. Both register and
    // login are rate-limited per-IP (see TASK-007), so the brief double-call
    // is safe.
    await login({ email: payload.email, password: payload.password })
  }

  async function logout(): Promise<void> {
    try {
      await api('/auth/logout', { method: 'POST' })
    } finally {
      // Always clear local state — even if the network call failed (eg
      // already-expired session), the user clicked Sign out and we must
      // reflect that intention in the UI.
      user.value = null
      fetched.value = true
    }
  }

  async function fetchMe(): Promise<void> {
    if (fetched.value) return
    try {
      const { user: u } = await api<{ user: Driver }>('/auth/me')
      user.value = u
    } catch {
      // 401 (no/expired cookie) is the common case for anonymous visitors —
      // swallow and treat as logged-out. Any other error (network, 5xx) is
      // also treated as logged-out so the user lands on /login with a clean
      // form rather than seeing a noisy banner during boot.
      user.value = null
    } finally {
      fetched.value = true
    }
  }

  return {
    user,
    fetched,
    isAuthenticated,
    isManager,
    login,
    register,
    logout,
    fetchMe,
  }
})
