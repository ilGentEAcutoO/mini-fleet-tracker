<template>
  <div class="min-h-screen bg-background text-foreground">
    <header class="border-b border-border">
      <div class="container mx-auto flex h-14 items-center px-4">
        <NuxtLink to="/" class="font-semibold tracking-tight">
          Mini Fleet Tracker
        </NuxtLink>
        <nav class="ml-auto flex gap-4 text-sm">
          <NuxtLink to="/dashboard">Dashboard</NuxtLink>
          <NuxtLink to="/login">Sign in</NuxtLink>
        </nav>
      </div>
    </header>
    <main class="container mx-auto px-4 py-8">
      <slot />
    </main>
  </div>
</template>
