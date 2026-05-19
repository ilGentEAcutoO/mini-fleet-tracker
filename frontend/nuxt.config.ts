// https://nuxt.com/docs/api/configuration/nuxt-config
import tailwindcss from '@tailwindcss/vite'

export default defineNuxtConfig({
  compatibilityDate: '2026-05-19',
  devtools: { enabled: true },

  // TypeScript strict mode + typecheck wired into `nuxt typecheck`.
  typescript: {
    strict: true,
    typeCheck: true,
    // MapLibre ships its own types via the published package so no
    // additional `types: [...]` opt-in is needed here.
  },

  modules: ['@pinia/nuxt', 'shadcn-nuxt', '@nuxt/eslint'],

  // shadcn-vue components live under `app/components/ui/` and are owned by
  // the project (copy-pasted from the registry, NOT a node_module). `@` is
  // Nuxt's alias for srcDir, which under Nuxt 4 defaults to `app/`.
  shadcn: {
    prefix: '',
    componentDir: '@/components/ui',
  },

  // Tailwind v4 — installed via the @tailwindcss/vite plugin path (no
  // @nuxtjs/tailwindcss module; the v7 beta of that module exists but the
  // Vite-plugin route is the upstream-recommended path for v4).
  vite: {
    plugins: [tailwindcss()],
  },

  css: ['~/assets/css/tailwind.css'],

  // Single-origin deployment target: the whole site (frontend + /api/* + /ws/*)
  // is fronted by a Cloudflare Worker at fleet-tracker.jairukchan.com. We use
  // the modern `cloudflare_module` preset (NOT cloudflare-pages).
  nitro: {
    preset: 'cloudflare_module',
  },

  runtimeConfig: {
    public: {
      // Populated from NUXT_PUBLIC_* env vars at runtime.
      apiBase: '',
      wsBase: '',
      // OpenFreeMap vector style — overridable via NUXT_PUBLIC_MAP_STYLE.
      mapStyle: 'https://tiles.openfreemap.org/styles/liberty',
    },
  },
})
