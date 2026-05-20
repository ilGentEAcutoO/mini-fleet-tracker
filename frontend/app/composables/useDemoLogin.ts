// Shared demo-login plumbing for the landing page and the /login form's quick
// buttons. See `frontend/app/pages/login.vue` for the full rationale on why we
// skip the response-body parse on POST /auth/login and use a follow-up /auth/me
// call to populate the Pinia store before SPA-navigating.

import type { Driver } from '~~/shared/types/domain'

const DEMO_CREDS = {
  manager: { email: 'manager@demo.local', password: 'SeedPassword!1', landing: '/dashboard' },
  driver: { email: 'driver@demo.local', password: 'SeedPassword!1', landing: '/driver/report' },
} as const

export type DemoRole = keyof typeof DEMO_CREDS

export function useDemoLogin() {
  const auth = useAuthStore()
  const router = useRouter()
  const submitting = ref<DemoRole | null>(null)
  const errorMessage = ref<string | null>(null)
  const apiBase = useRuntimeConfig().public.apiBase

  async function loginAs(role: DemoRole) {
    if (submitting.value) return
    errorMessage.value = null
    submitting.value = role
    const creds = DEMO_CREDS[role]
    try {
      const loginResp = await fetch(`${apiBase}/auth/login`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: creds.email, password: creds.password }),
      })
      if (!loginResp.ok) {
        errorMessage.value = 'Demo sign-in failed. Please try again.'
        submitting.value = null
        return
      }
      const meResp = await fetch(`${apiBase}/auth/me`, { credentials: 'include' })
      if (!meResp.ok) {
        errorMessage.value = 'Demo session check failed. Please try again.'
        submitting.value = null
        return
      }
      const meBody = (await meResp.json()) as { user: Driver }
      auth.user = meBody.user
      auth.fetched = true
      await router.push(creds.landing)
    }
    catch (err: unknown) {
      const e = err as { message?: string } | undefined
      errorMessage.value = e?.message ?? 'Demo sign-in failed. Please try again.'
      submitting.value = null
    }
  }

  return { loginAs, submitting, errorMessage }
}
