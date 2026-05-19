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

const onSubmit = handleSubmit(async (values) => {
  formError.value = null
  try {
    await auth.login(values)
    // Honour `?redirect=` from the global middleware so a deep link survives
    // the login bounce. Validate it starts with `/` to prevent open-redirect
    // to an external attacker-controlled URL.
    const raw = route.query.redirect
    const redirect =
      typeof raw === 'string' && raw.startsWith('/') ? raw : '/dashboard'
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
