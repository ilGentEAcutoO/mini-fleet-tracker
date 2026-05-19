<script setup lang="ts">
import { z } from 'zod'
import { useForm } from 'vee-validate'
import { toTypedSchema } from '@vee-validate/zod'
import type { Role } from '~~/shared/types/domain'

definePageMeta({ layout: 'default' })
useHead({ title: 'Create account' })

// Mirrors backend validator tags (auth_handler.go):
//   name: required,min=1,max=200
//   email: required,email
//   password: required,min=8,max=72
//   role: required,oneof=driver manager
const schema = toTypedSchema(
  z.object({
    name: z
      .string()
      .min(1, 'Name is required')
      .max(200, 'Name must be at most 200 characters'),
    email: z.string().email('Please enter a valid email address'),
    password: z
      .string()
      .min(8, 'Password must be at least 8 characters')
      .max(72, 'Password must be at most 72 characters'),
    role: z.enum(['driver', 'manager'], {
      message: 'Choose a role',
    }),
  }),
)

const { handleSubmit, isSubmitting, errors, defineField } = useForm({
  validationSchema: schema,
  initialValues: {
    name: '',
    email: '',
    password: '',
    role: 'driver' as Role,
  },
})
const [name, nameAttrs] = defineField('name')
const [email, emailAttrs] = defineField('email')
const [password, passwordAttrs] = defineField('password')
const [role, roleAttrs] = defineField('role')

const auth = useAuthStore()
const router = useRouter()
const formError = ref<string | null>(null)

const onSubmit = handleSubmit(async (values) => {
  formError.value = null
  try {
    // The auth store chains login() after register() so the cookie is set
    // before we navigate away; otherwise the dashboard guard would bounce
    // back to /login.
    await auth.register(values)
    await router.push('/dashboard')
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    formError.value
      = e?.data?.message ?? 'Registration failed. Please try again.'
  }
})
</script>

<template>
  <section class="mx-auto max-w-md">
    <Card>
      <CardHeader>
        <CardTitle>Create your account</CardTitle>
        <CardDescription>
          Pick driver for the mobile app or manager for the dashboard.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form class="space-y-4" @submit="onSubmit">
          <div class="space-y-1">
            <Label for="name">Name</Label>
            <Input
              id="name"
              v-model="name"
              v-bind="nameAttrs"
              type="text"
              autocomplete="name"
              :aria-invalid="errors.name ? 'true' : undefined"
            />
            <p
              v-if="errors.name"
              class="text-sm text-destructive"
            >
              {{ errors.name }}
            </p>
          </div>
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
              autocomplete="new-password"
              :aria-invalid="errors.password ? 'true' : undefined"
            />
            <p
              v-if="errors.password"
              class="text-sm text-destructive"
            >
              {{ errors.password }}
            </p>
          </div>
          <div class="space-y-1">
            <Label for="role">Role</Label>
            <!--
              Native <select> styled to match the shadcn-vue Input — keeps the
              dependency surface small (no extra shadcn add) and is the most
              accessible option for a 2-item picker. If the design grows past
              a handful of choices, swap for the shadcn Select primitive.
            -->
            <select
              id="role"
              v-model="role"
              v-bind="roleAttrs"
              :aria-invalid="errors.role ? 'true' : undefined"
              class="dark:bg-input/30 border-input focus-visible:border-ring focus-visible:ring-ring/50 aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 aria-invalid:border-destructive dark:aria-invalid:border-destructive/50 h-8 w-full rounded-lg border bg-transparent px-2.5 py-1 text-sm transition-colors focus-visible:ring-3 aria-invalid:ring-3 outline-none disabled:cursor-not-allowed disabled:opacity-50"
            >
              <option value="driver">
                Driver
              </option>
              <option value="manager">
                Manager
              </option>
            </select>
            <p
              v-if="errors.role"
              class="text-sm text-destructive"
            >
              {{ errors.role }}
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
            {{ isSubmitting ? 'Creating account…' : 'Create account' }}
          </Button>
        </form>
        <p class="text-sm text-muted-foreground mt-4 text-center">
          Already have an account?
          <NuxtLink to="/login" class="underline">
            Sign in
          </NuxtLink>
        </p>
      </CardContent>
    </Card>
  </section>
</template>
