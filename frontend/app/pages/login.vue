<script setup lang="ts">
import { z } from 'zod'
import { useForm } from 'vee-validate'
import { toTypedSchema } from '@vee-validate/zod'

definePageMeta({ layout: 'default' })
useHead({ title: 'Sign in' })

// Zod schema mirrors the Go validator tags on backend/internal/handler/auth_handler.go:
//   email: required,email; password: required,min=8,max=72
// Keep the rules byte-aligned with backend so a successful client-side pass
// implies a successful server-side parse — diverging messages confuse users.
const schema = toTypedSchema(
  z.object({
    email: z.string().email('Please enter a valid email address'),
    password: z
      .string()
      .min(8, 'Password must be at least 8 characters')
      .max(72, 'Password must be at most 72 characters'),
  }),
)

const { handleSubmit, isSubmitting, errors, defineField } = useForm({
  validationSchema: schema,
})
const [email, emailAttrs] = defineField('email')
const [password, passwordAttrs] = defineField('password')

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const formError = ref<string | null>(null)
const demoSubmitting = ref<'manager' | 'driver' | null>(null)

// Quick-fill demo accounts so a reviewer hitting the live URL can land on the
// dashboard in one click instead of scraping creds from the README. These two
// accounts come from `make seed` (manager + driver + 3 vehicles) and are
// documented in README.md as intentionally-checked-in shared demo creds —
// embedding them here is no incremental leak.
const DEMO_CREDS = {
  manager: { email: 'manager@demo.local', password: 'SeedPassword!1', landing: '/dashboard' },
  driver: { email: 'driver@demo.local', password: 'SeedPassword!1', landing: '/driver/report' },
} as const

async function loginAsDemo(role: 'manager' | 'driver') {
  if (demoSubmitting.value || isSubmitting.value) return
  formError.value = null
  demoSubmitting.value = role
  const creds = DEMO_CREDS[role]
  const apiBase = useRuntimeConfig().public.apiBase
  // Two-step raw-fetch login + manual Pinia populate, then SPA nav. Why this
  // shape instead of `auth.login()` + `router.push()`:
  //
  //   1. POST /auth/login responses from the CF Container intermittently arrive
  //      with `content-encoding: zstd` and no terminating Content-Length on the
  //      success path. `r.json()` then hangs on the body stream, so the Pinia
  //      store never sees `user.value = u`. The Set-Cookie headers have already
  //      landed by the time fetch resolves, so we skip reading the login body
  //      entirely.
  //   2. /auth/me's body stream is reliable, so we read the user record from
  //      there and assign it onto the auth store directly. The middleware
  //      reads `fetched` + `user` short-circuits the next push.
  //   3. SPA `router.push` keeps the demo button feeling instant (no full
  //      reload). A `window.location.assign` would bounce off the documented
  //      SSR cookie-validation gap (todos.md / TASK-029 line 160).
  try {
    const loginResp = await fetch(`${apiBase}/auth/login`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: creds.email, password: creds.password }),
    })
    if (!loginResp.ok) {
      formError.value = 'Demo sign-in failed. Please try again.'
      demoSubmitting.value = null
      return
    }
    const meResp = await fetch(`${apiBase}/auth/me`, { credentials: 'include' })
    if (!meResp.ok) {
      formError.value = 'Demo session check failed. Please try again.'
      demoSubmitting.value = null
      return
    }
    const meBody = (await meResp.json()) as { user: typeof auth.user }
    auth.user = meBody.user
    auth.fetched = true
    await router.push(creds.landing)
  }
  catch (err: unknown) {
    const e = err as { message?: string } | undefined
    formError.value = e?.message ?? 'Demo sign-in failed. Please try again.'
    demoSubmitting.value = null
  }
}

const onSubmit = handleSubmit(async (values) => {
  formError.value = null
  try {
    await auth.login(values)
    // Honour `?redirect=` from the global middleware so a deep link survives
    // the login bounce. Resolve the candidate against the current origin via
    // the URL parser and only accept it when the resolved origin matches —
    // this rejects open-redirect vectors that loose `startsWith('/')` checks
    // miss, including:
    //   * protocol-relative paths  `//evil.com/path`  (parses to evil.com)
    //   * backslash variants       `/\evil.com/path`  (Chrome normalizes \\)
    //   * full external URLs       `https://evil.com`
    // SSR-safe: the `onSubmit` handler only fires after a user click, by
    // which time we're guaranteed to be on the client and `location.origin`
    // is defined.
    const raw = route.query.redirect
    let redirect = '/dashboard'
    if (typeof raw === 'string' && raw.startsWith('/')) {
      try {
        const candidate = new URL(raw, location.origin)
        if (candidate.origin === location.origin) {
          redirect = candidate.pathname + candidate.search + candidate.hash
        }
      }
      catch {
        // URL constructor threw — keep the safe default.
      }
    }
    await router.push(redirect)
  }
  catch (err: unknown) {
    // The backend returns { error, message, request_id? } — surface `message`
    // for actionable copy; fall back to a generic line if the shape was lost
    // (network failure, CORS preflight reject, etc).
    const e = err as { data?: { message?: string } } | undefined
    formError.value
      = e?.data?.message ?? 'Sign-in failed. Please try again.'
  }
})
</script>

<template>
  <section class="mx-auto max-w-md">
    <Card>
      <CardHeader>
        <CardTitle>Sign in</CardTitle>
        <CardDescription>
          Use your manager or driver credentials.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div class="space-y-2 mb-4">
          <Button
            type="button"
            variant="secondary"
            class="w-full"
            :disabled="!!demoSubmitting || isSubmitting"
            data-testid="try-demo-manager"
            @click="loginAsDemo('manager')"
          >
            {{ demoSubmitting === 'manager' ? 'Signing in…' : 'Try as Manager (demo)' }}
          </Button>
          <Button
            type="button"
            variant="secondary"
            class="w-full"
            :disabled="!!demoSubmitting || isSubmitting"
            data-testid="try-demo-driver"
            @click="loginAsDemo('driver')"
          >
            {{ demoSubmitting === 'driver' ? 'Signing in…' : 'Try as Driver (demo)' }}
          </Button>
        </div>
        <div class="relative my-4">
          <div class="absolute inset-0 flex items-center" aria-hidden="true">
            <span class="w-full border-t" />
          </div>
          <div class="relative flex justify-center text-xs uppercase">
            <span class="bg-card px-2 text-muted-foreground">
              or sign in manually
            </span>
          </div>
        </div>
        <form class="space-y-4" @submit="onSubmit">
          <div class="space-y-1">
            <Label for="email">Email</Label>
            <Input
              id="email"
              v-model="email"
              v-bind="emailAttrs"
              type="email"
              autocomplete="email"
              :aria-invalid="errors.email ? 'true' : undefined"
            />
            <p
              v-if="errors.email"
              class="text-sm text-destructive"
            >
              {{ errors.email }}
            </p>
          </div>
          <div class="space-y-1">
            <Label for="password">Password</Label>
            <Input
              id="password"
              v-model="password"
              v-bind="passwordAttrs"
              type="password"
              autocomplete="current-password"
              :aria-invalid="errors.password ? 'true' : undefined"
            />
            <p
              v-if="errors.password"
              class="text-sm text-destructive"
            >
              {{ errors.password }}
            </p>
          </div>
          <p
            v-if="formError"
            class="text-sm text-destructive"
            role="alert"
          >
            {{ formError }}
          </p>
          <Button
            type="submit"
            :disabled="isSubmitting"
            class="w-full"
          >
            {{ isSubmitting ? 'Signing in…' : 'Sign in' }}
          </Button>
        </form>
        <p class="text-sm text-muted-foreground mt-4 text-center">
          No account?
          <NuxtLink to="/register" class="underline">
            Register
          </NuxtLink>
        </p>
      </CardContent>
    </Card>
  </section>
</template>
