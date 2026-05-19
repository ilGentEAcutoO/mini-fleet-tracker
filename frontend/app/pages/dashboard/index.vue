<script setup lang="ts">
// Placeholder for TASK-017. For TASK-009 (auth flow) we only need to prove
// that:
//   1. the global middleware redirects unauthenticated users to /login
//   2. an authenticated user can read their session via useAuthStore
//   3. logout clears the session and bounces to /login
//
// Map, vehicle list, and history all arrive in later tasks.

definePageMeta({ layout: 'default' })
useHead({ title: 'Dashboard' })

const auth = useAuthStore()
const router = useRouter()

async function signOut(): Promise<void> {
  await auth.logout()
  await router.push('/login')
}
</script>

<template>
  <section class="space-y-4">
    <h1 class="text-2xl font-semibold tracking-tight">
      Dashboard
    </h1>
    <p class="text-muted-foreground">
      Signed in as
      <span class="font-medium">{{ auth.user?.email }}</span>
      (role: {{ auth.user?.role }}).
    </p>
    <p class="text-sm text-muted-foreground">
      Live map, vehicle list, and history come in later tasks.
    </p>
    <Button variant="outline" @click="signOut">
      Sign out
    </Button>
  </section>
</template>
