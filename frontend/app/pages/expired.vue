<script setup lang="ts">
// Demo-expiration landing page (TASK-030 will configure the backend/Worker
// to redirect here after 2026-05-31). Lives in the public-routes allow-list
// in `middleware/auth.global.ts` so anonymous visitors hit by the 410
// interceptor always reach this page — never the /login bounce.

definePageMeta({ layout: 'default' })
useHead({ title: 'Demo expired' })
</script>

<template>
  <section class="mx-auto max-w-2xl text-center space-y-4">
    <h1 class="text-3xl font-semibold tracking-tight">
      Demo expired
    </h1>
    <p class="text-muted-foreground">
      The live Mini Fleet Tracker demo ran until 2026-05-31. The backend is
      offline, but the full source — Go, TypeScript, and the architecture
      decisions behind it — is still available on GitHub.
    </p>
    <p>
      <a
        href="https://github.com/ilGentEAcutoO/mini-fleet-tracker"
        class="underline"
      >
        github.com/ilGentEAcutoO/mini-fleet-tracker
      </a>
    </p>
  </section>
</template>
