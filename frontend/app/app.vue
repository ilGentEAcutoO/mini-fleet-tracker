<script setup lang="ts">
useHead({
  titleTemplate: (title) =>
    title ? `${title} | Mini Fleet Tracker` : 'Mini Fleet Tracker',
})
</script>

<template>
  <NuxtLayout>
    <NuxtPage />
  </NuxtLayout>
</template>
