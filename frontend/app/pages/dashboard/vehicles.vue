<script setup lang="ts">
// Manager-only vehicle CRUD.
//
// Scope choices (deliberate, documented for next-iteration cleanup):
//
//   - Create + Delete are wired end-to-end via shadcn Dialog + the existing
//     vehicle CRUD endpoints (POST /api/vehicles, DELETE /api/vehicles/:id).
//   - Update is wired (PATCH /api/vehicles/:id) — same Dialog, populates the
//     form from the row. Plate + model + driver_id all editable.
//   - Driver assignment is a MANUAL driver_id (UUID) input — there is no
//     /api/auth/drivers list endpoint today and adding one would be a
//     backend change outside this task's surface. A future iteration can
//     swap the Input for a Select once that endpoint exists. The current
//     UX is fine for the demo because the dashboard manager has the driver
//     UUIDs from the registration flow / seed script.
//
// Role guard: the global middleware already enforces auth; a non-manager
// landing here will hit a 403 on the very first /api/vehicles GET. We also
// belt-and-braces redirect to /dashboard so drivers don't see a broken
// surface — the redirect is the friendly path, the backend 403 is the
// hard gate.
//
// Optimistic refresh model: every mutation refetches the list via
// `fetchVehicles()`. The fleet is tiny in the demo (single-digit vehicles)
// so the round-trip is sub-100ms; a smarter local-merge cache would be
// premature optimization.

import { z } from 'zod'
import { useForm } from 'vee-validate'
import { toTypedSchema } from '@vee-validate/zod'
import { toast } from 'vue-sonner'
import type { Vehicle } from '~~/shared/types/domain'
import type {
  VehicleCreateRequest,
  VehicleUpdateRequest,
} from '~~/shared/types/api'

definePageMeta({ layout: 'default' })
useHead({ title: 'Vehicles' })

const auth = useAuthStore()
const api = useApi()

// Belt-and-braces role guard. The global middleware enforces auth; this
// extra check kicks drivers back to /dashboard rather than letting the
// backend's 403 surface as an ugly error pill on this page.
if (import.meta.client && auth.user && !auth.isManager) {
  await navigateTo('/dashboard')
}

const vehicles = ref<Vehicle[]>([])
const loading = ref(false)
const loadError = ref<string | null>(null)

async function fetchVehicles(): Promise<void> {
  loading.value = true
  loadError.value = null
  try {
    const res = await api<{ vehicles: Vehicle[] }>('/vehicles')
    vehicles.value = res.vehicles
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    loadError.value = e?.data?.message ?? 'Failed to load vehicles'
  }
  finally {
    loading.value = false
  }
}

onMounted(fetchVehicles)

// ----- Create / edit dialog -----
//
// One dialog, two modes: `editing === null` is a create; otherwise we're
// editing the in-`editing` vehicle. The form is the same shape in both
// modes (plate + model + driver_id) — only the submit handler branches.

const dialogOpen = ref(false)
const editing = ref<Vehicle | null>(null)
const submitError = ref<string | null>(null)

// Zod schema mirrors the Go validator on backend/internal/handler/vehicle_handler.go:
//   plate_number: required,min=1,max=50
//   model:        omitempty,max=100
//   driver_id:    omitempty,uuid4|uuid (validator/v10's uuid validator)
// We accept empty strings for optional fields and only validate UUID when
// the field is non-empty — matches the backend's omitempty semantics.
const schema = toTypedSchema(
  z.object({
    plate_number: z
      .string()
      .min(1, 'Plate number is required')
      .max(50, 'Plate number must be at most 50 characters'),
    model: z
      .string()
      .max(100, 'Model must be at most 100 characters')
      .optional(),
    driver_id: z
      .string()
      .optional()
      .refine(
        (v) => !v || /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(v),
        { message: 'Driver ID must be a UUID (or leave empty to unassign)' },
      ),
  }),
)

const { handleSubmit, isSubmitting, errors, defineField, resetForm, setValues } = useForm({
  validationSchema: schema,
  initialValues: { plate_number: '', model: '', driver_id: '' },
})
const [plateNumber, plateNumberAttrs] = defineField('plate_number')
const [model, modelAttrs] = defineField('model')
const [driverId, driverIdAttrs] = defineField('driver_id')

function openCreate(): void {
  editing.value = null
  submitError.value = null
  resetForm({ values: { plate_number: '', model: '', driver_id: '' } })
  dialogOpen.value = true
}

function openEdit(v: Vehicle): void {
  editing.value = v
  submitError.value = null
  // setValues (not resetForm) so dirty-tracking sees this as the new baseline.
  setValues({
    plate_number: v.plate_number,
    model: v.model ?? '',
    driver_id: v.driver_id ?? '',
  })
  dialogOpen.value = true
}

const onSubmit = handleSubmit(async (values) => {
  submitError.value = null
  try {
    if (editing.value === null) {
      // Create. Strip empty optionals so the backend's omitempty validators
      // treat them as absent rather than `""`.
      const payload: VehicleCreateRequest = {
        plate_number: values.plate_number,
        ...(values.model ? { model: values.model } : {}),
        ...(values.driver_id ? { driver_id: values.driver_id } : {}),
      }
      await api('/vehicles', { method: 'POST', body: payload })
      toast.success('Vehicle created')
    }
    else {
      // Update. PATCH semantics: only send the keys we actually want to
      // change. For this UX we always send all three (the form holds the
      // full current state); the backend's `omitnil` validator + Patch
      // semantics tolerate that.
      const payload: VehicleUpdateRequest = {
        plate_number: values.plate_number,
        model: values.model ?? '',
        driver_id: values.driver_id ?? '',
      }
      await api(`/vehicles/${editing.value.id}`, {
        method: 'PATCH',
        body: payload,
      })
      toast.success('Vehicle updated')
    }
    dialogOpen.value = false
    await fetchVehicles()
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    submitError.value = e?.data?.message ?? 'Save failed. Please try again.'
  }
})

// ----- Delete -----
//
// `window.confirm` is the lightest path; a custom AlertDialog would be a
// stricter pattern but for a manager-facing destructive action on a single
// row, the native browser confirm is acceptable and keeps the dependency
// surface small. Swap for AlertDialog if the brief gets stricter.

const deletingId = ref<string | null>(null)

async function onDelete(v: Vehicle): Promise<void> {
  if (!confirm(`Delete vehicle ${v.plate_number}? This cannot be undone.`)) return
  deletingId.value = v.id
  try {
    await api(`/vehicles/${v.id}`, { method: 'DELETE' })
    toast.success(`Deleted ${v.plate_number}`)
    await fetchVehicles()
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    toast.error(e?.data?.message ?? 'Delete failed')
  }
  finally {
    deletingId.value = null
  }
}
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight">
          Vehicles
        </h1>
        <p class="text-sm text-muted-foreground">
          Create, edit, and remove vehicles in the fleet.
        </p>
      </div>
      <Button @click="openCreate">
        Add vehicle
      </Button>
    </div>

    <Card>
      <CardContent>
        <p v-if="loading" class="text-sm text-muted-foreground">
          Loading…
        </p>
        <p
          v-else-if="loadError"
          class="text-sm text-destructive"
          role="alert"
        >
          {{ loadError }}
        </p>
        <Table v-else>
          <TableHeader>
            <TableRow>
              <TableHead>Plate</TableHead>
              <TableHead>Model</TableHead>
              <TableHead>Driver ID</TableHead>
              <TableHead class="w-[160px] text-right">
                Actions
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow v-if="vehicles.length === 0">
              <TableCell
                colspan="4"
                class="text-center text-sm text-muted-foreground"
              >
                No vehicles yet. Click <span class="font-medium">Add vehicle</span> to create one.
              </TableCell>
            </TableRow>
            <TableRow v-for="v in vehicles" :key="v.id">
              <TableCell class="font-medium">
                {{ v.plate_number }}
              </TableCell>
              <TableCell>
                <span v-if="v.model">{{ v.model }}</span>
                <span v-else class="text-muted-foreground">—</span>
              </TableCell>
              <TableCell class="font-mono text-xs">
                <span v-if="v.driver_id">{{ v.driver_id }}</span>
                <span v-else class="text-muted-foreground">unassigned</span>
              </TableCell>
              <TableCell class="text-right space-x-1">
                <Button
                  variant="outline"
                  size="sm"
                  @click="openEdit(v)"
                >
                  Edit
                </Button>
                <Button
                  variant="destructive"
                  size="sm"
                  :disabled="deletingId === v.id"
                  @click="onDelete(v)"
                >
                  {{ deletingId === v.id ? 'Deleting…' : 'Delete' }}
                </Button>
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </CardContent>
    </Card>

    <Dialog v-model:open="dialogOpen">
      <DialogContent>
        <DialogHeader>
          <DialogTitle>
            {{ editing ? 'Edit vehicle' : 'Add vehicle' }}
          </DialogTitle>
          <DialogDescription>
            {{
              editing
                ? `Updating ${editing.plate_number}.`
                : 'Create a new vehicle in the fleet.'
            }}
          </DialogDescription>
        </DialogHeader>
        <form class="space-y-4" @submit="onSubmit">
          <div class="space-y-1">
            <Label for="plate_number">Plate number</Label>
            <Input
              id="plate_number"
              v-model="plateNumber"
              v-bind="plateNumberAttrs"
              type="text"
              autocomplete="off"
              :aria-invalid="errors.plate_number ? 'true' : undefined"
            />
            <p v-if="errors.plate_number" class="text-sm text-destructive">
              {{ errors.plate_number }}
            </p>
          </div>
          <div class="space-y-1">
            <Label for="model">Model (optional)</Label>
            <Input
              id="model"
              v-model="model"
              v-bind="modelAttrs"
              type="text"
              autocomplete="off"
              :aria-invalid="errors.model ? 'true' : undefined"
            />
            <p v-if="errors.model" class="text-sm text-destructive">
              {{ errors.model }}
            </p>
          </div>
          <div class="space-y-1">
            <Label for="driver_id">Driver ID (optional)</Label>
            <Input
              id="driver_id"
              v-model="driverId"
              v-bind="driverIdAttrs"
              type="text"
              autocomplete="off"
              placeholder="UUID — leave empty to unassign"
              :aria-invalid="errors.driver_id ? 'true' : undefined"
            />
            <p v-if="errors.driver_id" class="text-sm text-destructive">
              {{ errors.driver_id }}
            </p>
            <p class="text-xs text-muted-foreground">
              Paste a driver's UUID. A driver-list picker lands when the
              backend exposes <code>/api/auth/drivers</code>.
            </p>
          </div>
          <p
            v-if="submitError"
            class="text-sm text-destructive"
            role="alert"
          >
            {{ submitError }}
          </p>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              :disabled="isSubmitting"
              @click="dialogOpen = false"
            >
              Cancel
            </Button>
            <Button
              type="submit"
              :disabled="isSubmitting"
            >
              {{ isSubmitting ? 'Saving…' : editing ? 'Save changes' : 'Create' }}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  </section>
</template>
