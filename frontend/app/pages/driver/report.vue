<script setup lang="ts">
// Driver-only manual position-report page.
//
// Three submission paths:
//   1. Type lat/lng manually
//   2. Click "Use my location" → navigator.geolocation.getCurrentPosition
//      fills the form (the driver still has to submit; this matches the
//      "confirm before send" pattern most fleet apps use)
//   3. Future: TASK-019 sim CLI / driver mobile (out of scope)
//
// Role: drivers only. The global middleware guarantees auth; we also
// redirect managers back to /dashboard so the form never even renders for
// them. The backend's position handler will reject managers with 403 even
// if they bypassed the redirect.
//
// Vehicle picker: GET /api/vehicles is a manager-only endpoint, so a
// driver cannot list "the vehicle assigned to me" via the public API. We
// expose a plain text input for `vehicle_id` (UUID). The backend's
// PositionUsecase.Write checks driver-ownership: driver_id on the vehicle
// row must match the authenticated driver, otherwise it returns
// ErrForbidden → 403. Documented inline so the next reader doesn't ship
// a "fetch and pre-fill" patch only to discover the 403.
//
// Schema mirrors backend/internal/handler/position_handler.go validators:
//   vehicle_id:  required
//   lat:         required, -90..90
//   lng:         required, -180..180
//   speed_kmh:   optional, 0..500
//   recorded_at: required, unix-ms

import { z } from 'zod'
import { useForm } from 'vee-validate'
import { toTypedSchema } from '@vee-validate/zod'
import { toast } from 'vue-sonner'
import type { PositionWriteRequest } from '~~/shared/types/api'

definePageMeta({ layout: 'default' })
useHead({ title: 'Report position' })

const auth = useAuthStore()
const api = useApi()

// Role guard for managers. The auth state is already populated by the
// global middleware (which awaited fetchMe before allowing the navigation),
// so `auth.user` is guaranteed non-null here.
if (import.meta.client && auth.user && auth.user.role !== 'driver') {
  await navigateTo('/dashboard')
}

const schema = toTypedSchema(
  z.object({
    vehicle_id: z
      .string()
      .min(1, 'Vehicle ID is required'),
    lat: z
      .number({ message: 'Latitude must be a number' })
      .min(-90, 'Latitude must be between -90 and 90')
      .max(90, 'Latitude must be between -90 and 90'),
    lng: z
      .number({ message: 'Longitude must be a number' })
      .min(-180, 'Longitude must be between -180 and 180')
      .max(180, 'Longitude must be between -180 and 180'),
    speed_kmh: z
      .number()
      .min(0, 'Speed cannot be negative')
      .max(500, 'Speed must be at most 500 km/h')
      .optional(),
  }),
)

const { handleSubmit, isSubmitting, errors, defineField, resetForm } = useForm({
  validationSchema: schema,
  initialValues: {
    vehicle_id: '',
    lat: undefined as number | undefined,
    lng: undefined as number | undefined,
    speed_kmh: undefined as number | undefined,
  },
})
const [vehicleId, vehicleIdAttrs] = defineField('vehicle_id')
const [lat, latAttrs] = defineField('lat')
const [lng, lngAttrs] = defineField('lng')
const [speedKmh, speedKmhAttrs] = defineField('speed_kmh')

const submitError = ref<string | null>(null)
const geoBusy = ref(false)

function useMyLocation(): void {
  if (typeof navigator === 'undefined' || !navigator.geolocation) {
    toast.error('Geolocation is not available in this browser')
    return
  }
  geoBusy.value = true
  navigator.geolocation.getCurrentPosition(
    (pos) => {
      lat.value = pos.coords.latitude
      lng.value = pos.coords.longitude
      // GPS speed is meters/second when available; convert to km/h. Some
      // platforms (desktop) return null for speed entirely.
      if (typeof pos.coords.speed === 'number' && !Number.isNaN(pos.coords.speed) && pos.coords.speed >= 0) {
        speedKmh.value = Math.round(pos.coords.speed * 3.6 * 10) / 10
      }
      geoBusy.value = false
      toast.success('Location captured. Review and submit.')
    },
    (err) => {
      geoBusy.value = false
      // err.code: 1=permission, 2=unavailable, 3=timeout
      const msg = err.code === 1
        ? 'Location permission denied'
        : err.code === 3
          ? 'Location request timed out'
          : 'Could not get your location'
      toast.error(msg)
    },
    { enableHighAccuracy: true, timeout: 10_000, maximumAge: 0 },
  )
}

const onSubmit = handleSubmit(async (values) => {
  submitError.value = null
  // recorded_at is "now" — the backend rejects values too far in the past
  // and the demo doesn't have a real GPS time-of-fix to pass through, so
  // stamping client-time at submit is fine. The clock skew between client
  // and server is sub-second for the demo scope.
  const payload: PositionWriteRequest = {
    vehicle_id: values.vehicle_id,
    lat: values.lat,
    lng: values.lng,
    ...(typeof values.speed_kmh === 'number' ? { speed_kmh: values.speed_kmh } : {}),
    recorded_at: Date.now(),
  }
  try {
    await api('/positions', { method: 'POST', body: payload })
    toast.success('Position reported')
    // Preserve vehicle_id between reports — a real driver submits many
    // positions for the same vehicle over a shift; resetting it every
    // time would be annoying. Clear lat/lng/speed so accidental
    // double-submits show as obviously-blank.
    resetForm({
      values: {
        vehicle_id: values.vehicle_id,
        lat: undefined,
        lng: undefined,
        speed_kmh: undefined,
      },
    })
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    submitError.value = e?.data?.message ?? 'Submit failed. Please try again.'
  }
})
</script>

<template>
  <section class="mx-auto max-w-xl space-y-4">
    <div>
      <h1 class="text-2xl font-semibold tracking-tight">
        Report position
      </h1>
      <p class="text-sm text-muted-foreground">
        Submit a position for the vehicle assigned to you. Use the
        browser's location, or type coordinates manually.
      </p>
    </div>

    <Card>
      <CardContent>
        <form class="space-y-4" @submit="onSubmit">
          <div class="space-y-1">
            <Label for="vehicle_id">Vehicle ID</Label>
            <Input
              id="vehicle_id"
              v-model="vehicleId"
              v-bind="vehicleIdAttrs"
              type="text"
              autocomplete="off"
              placeholder="UUID of the vehicle assigned to you"
              :aria-invalid="errors.vehicle_id ? 'true' : undefined"
            />
            <p v-if="errors.vehicle_id" class="text-sm text-destructive">
              {{ errors.vehicle_id }}
            </p>
          </div>

          <div class="grid grid-cols-2 gap-3">
            <div class="space-y-1">
              <Label for="lat">Latitude</Label>
              <Input
                id="lat"
                v-model.number="lat"
                v-bind="latAttrs"
                type="number"
                step="any"
                inputmode="decimal"
                :aria-invalid="errors.lat ? 'true' : undefined"
              />
              <p v-if="errors.lat" class="text-sm text-destructive">
                {{ errors.lat }}
              </p>
            </div>
            <div class="space-y-1">
              <Label for="lng">Longitude</Label>
              <Input
                id="lng"
                v-model.number="lng"
                v-bind="lngAttrs"
                type="number"
                step="any"
                inputmode="decimal"
                :aria-invalid="errors.lng ? 'true' : undefined"
              />
              <p v-if="errors.lng" class="text-sm text-destructive">
                {{ errors.lng }}
              </p>
            </div>
          </div>

          <div class="space-y-1">
            <Label for="speed_kmh">Speed (km/h, optional)</Label>
            <Input
              id="speed_kmh"
              v-model.number="speedKmh"
              v-bind="speedKmhAttrs"
              type="number"
              step="any"
              inputmode="decimal"
              :aria-invalid="errors.speed_kmh ? 'true' : undefined"
            />
            <p v-if="errors.speed_kmh" class="text-sm text-destructive">
              {{ errors.speed_kmh }}
            </p>
          </div>

          <div class="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              :disabled="geoBusy"
              @click="useMyLocation"
            >
              {{ geoBusy ? 'Locating…' : 'Use my location' }}
            </Button>
            <Button
              type="submit"
              :disabled="isSubmitting"
              class="flex-1"
            >
              {{ isSubmitting ? 'Submitting…' : 'Submit position' }}
            </Button>
          </div>

          <p
            v-if="submitError"
            class="text-sm text-destructive"
            role="alert"
          >
            {{ submitError }}
          </p>
        </form>
      </CardContent>
    </Card>
  </section>
</template>
