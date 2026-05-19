// Singleton loader for the Google Maps JavaScript API.
//
// Lives in `composables/` because it reads `useRuntimeConfig()` to pick up the
// `NUXT_PUBLIC_GOOGLE_MAPS_KEY` env var. The actual loading state is held in a
// module-level promise so multiple components that mount `<MapView>` in the
// same page share one SDK load — the Maps JS API can only be bootstrapped
// once per page, and `setOptions()` will warn (and noop) if called twice.
//
// We use the v2.x functional API from `@googlemaps/js-api-loader` (the v1.x
// `Loader` class is deprecated and throws in v2.x). The first call to
// `importLibrary()` actually fetches the SDK; subsequent calls reuse the
// in-memory module map.
//
// Usage:
//   const { load } = useGoogleMaps()
//   await load()
//   const { Map } = await google.maps.importLibrary('maps') as google.maps.MapsLibrary
//
// SSR note: `setOptions` / `importLibrary` touch `document` and `window`, so
// callers must guard with `<ClientOnly>` or `import.meta.client`. The
// composable itself is safe to instantiate on the server (it just stores a
// closure); it only blows up if `load()` is actually called server-side, and
// in that case we surface a clear error instead of a `document is not defined`
// crash from the SDK.

import { importLibrary, setOptions } from '@googlemaps/js-api-loader'

// Module-level singleton — survives across component mounts in the same page.
// Reset only via the test-only `__reset` helper.
let loaderPromise: Promise<typeof google.maps> | null = null

export const useGoogleMaps = () => {
  const config = useRuntimeConfig()

  const load = (): Promise<typeof google.maps> => {
    if (loaderPromise) return loaderPromise

    if (import.meta.server) {
      // The Maps JS SDK is browser-only — fail fast with a clear message
      // instead of letting the SDK crash on `document.createElement`.
      loaderPromise = Promise.reject(
        new Error('Google Maps cannot be loaded during SSR — wrap in <ClientOnly>'),
      )
      return loaderPromise
    }

    const key = config.public.googleMapsKey
    if (!key) {
      loaderPromise = Promise.reject(
        new Error('Google Maps API key not configured (NUXT_PUBLIC_GOOGLE_MAPS_KEY)'),
      )
      return loaderPromise
    }

    loaderPromise = (async () => {
      // setOptions must be called before the first importLibrary; calling it
      // twice on the same page logs a warning (the SDK ignores the second
      // call) — the singleton above prevents that path.
      setOptions({ key, v: 'weekly' })
      // Bootstrap the SDK by importing the `maps` library. Subsequent
      // importLibrary calls in <MapView> for `marker` re-use the same global
      // `google.maps` namespace, so this also serves as the "is the API
      // global ready?" signal.
      await importLibrary('maps')
      return google.maps
    })()

    return loaderPromise
  }

  // Test-only escape hatch — production code should never reset the loader
  // since `setOptions` cannot run twice on the same page anyway. Exposed for
  // unit tests that swap module-level state between cases.
  const __reset = () => {
    loaderPromise = null
  }

  return { load, __reset }
}
