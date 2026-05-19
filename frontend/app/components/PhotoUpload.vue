<script setup lang="ts">
// Photo upload (TASK-022).
//
// Per-vehicle photo gallery with R2-backed direct-PUT uploads via presigned
// URLs. The upload is a two-step dance:
//
//   1. POST /api/vehicles/:id/photos:presign → 200 { url, method, headers,
//      key, content_length_max, expires_at, quota_remaining }
//      (manager-only, CSRF; 429 if the daily quota is exhausted)
//   2. Direct PUT to `url` with the file bytes — the backend never sees the
//      raw image, which is the point of presigned URLs (no Go memory
//      pressure for 5 MB uploads, no R2 credentials in the browser).
//
// Why XMLHttpRequest instead of fetch?
//
//   Native fetch does not expose upload progress events — the spec accepts
//   a body but the Streams API has no equivalent of XHR's `upload.onprogress`.
//   For a 5 MB photo on a typical Bangkok 4G connection that is ~6-10s of
//   "is anything happening?". XHR's onprogress lets us show a percentage,
//   which is the cheapest UX win available here. fetch-with-progress
//   workarounds (Response.body ReadableStream) only solve the download
//   side, not upload.
//
// Quota UX
//
//   The backend returns `quota_remaining` AFTER consuming the presign slot
//   (so a fresh response with `quota_remaining: 2` means this is the
//   second-to-last upload of the day). We display `quota_remaining` from the
//   most recent successful presign — not the post-upload value (R2 PUT
//   doesn't return one) — so the count reflects what the backend believes
//   without us second-guessing it. On 429 we show a friendly "try again
//   tomorrow" message rather than the raw error.
//
// List rendering
//
//   GET /api/vehicles/:id/photos returns presigned GET URLs that expire
//   after a window (configured server-side). We don't currently surface
//   `expires_at` to the user — refreshing the gallery re-issues the URLs
//   and the user can't tell, which is exactly what we want.

interface Props {
  vehicleId: string
}

const props = defineProps<Props>()

// Mirrors the photo_handler.go presign response. content_length_max bounds
// the file we will PUT (the backend signs the URL with that limit too, so
// oversized PUTs reject server-side regardless of what the browser sends).
interface PresignResponse {
  url: string
  method: string
  headers: Record<string, string>
  key: string
  content_length_max: number
  expires_at: number
  quota_remaining: number
}

// GET /photos response shape. The backend includes an `expires_at` per
// entry for future expiration UX; we don't render it today.
interface PhotoListEntry {
  key: string
  url: string
  expires_at?: number
}

const api = useApi()
const file = ref<File | null>(null)
const progress = ref(0)
const uploading = ref(false)
const error = ref<string | null>(null)
const photos = ref<PhotoListEntry[]>([])
// `null` = not known yet (first presign hasn't happened). After the first
// successful presign we always have a number.
const quotaRemaining = ref<number | null>(null)

async function fetchPhotos(): Promise<void> {
  try {
    const res = await api<{ photos: PhotoListEntry[] }>(
      `/vehicles/${props.vehicleId}/photos`,
    )
    photos.value = res.photos ?? []
  }
  catch (err: unknown) {
    const e = err as { data?: { message?: string } } | undefined
    error.value = e?.data?.message ?? 'Failed to load photos'
  }
}

function pickFile(e: Event): void {
  const target = e.target as HTMLInputElement
  file.value = target.files?.[0] ?? null
  error.value = null
}

async function upload(): Promise<void> {
  if (!file.value) return
  uploading.value = true
  progress.value = 0
  error.value = null

  try {
    // Step 1: presign. CSRF + cookie auth are handled by useApi's
    // onRequest hook.
    let presign: PresignResponse
    try {
      presign = await api<PresignResponse>(
        `/vehicles/${props.vehicleId}/photos:presign`,
        {
          method: 'POST',
          body: { filename: file.value.name },
        },
      )
    }
    catch (err: unknown) {
      const e = err as { data?: { error?: string, message?: string }, status?: number, statusCode?: number } | undefined
      const status = e?.statusCode ?? e?.status
      if (status === 429 || e?.data?.error === 'quota_exceeded') {
        error.value = 'Daily upload limit reached for this vehicle. Try again tomorrow.'
        return
      }
      throw err
    }

    if (file.value.size > presign.content_length_max) {
      const mb = (presign.content_length_max / 1024 / 1024).toFixed(0)
      error.value = `File too large (max ${mb} MB)`
      return
    }

    // Step 2: direct PUT to R2 with progress. XHR is the only browser API
    // that exposes upload progress — see the script-top comment.
    const f = file.value
    await new Promise<void>((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.open(presign.method, presign.url)
      for (const [k, v] of Object.entries(presign.headers ?? {})) {
        xhr.setRequestHeader(k, v)
      }
      xhr.upload.onprogress = (ev) => {
        if (ev.lengthComputable) {
          progress.value = Math.round((ev.loaded / ev.total) * 100)
        }
      }
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve()
        }
        else {
          reject(new Error(`R2 upload failed: ${xhr.status}`))
        }
      }
      xhr.onerror = () => reject(new Error('R2 upload network error'))
      xhr.send(f)
    })

    // The PUT response doesn't carry a fresh quota_remaining (R2 has no
    // opinion on our app-level quota), so we display the value the backend
    // reported pre-upload. Decrement reflects this upload completing.
    quotaRemaining.value = Math.max(0, presign.quota_remaining - 1)
    await fetchPhotos()
    file.value = null
    // Reset the file input so re-picking the same file fires a fresh
    // change event. The bound `file` ref is enough for the disabled state
    // logic, but the native input value needs an explicit clear to
    // re-trigger `pickFile` for the same filename twice in a row.
    const input = document.querySelector<HTMLInputElement>('#photo-upload-input')
    if (input) input.value = ''
  }
  catch (err: unknown) {
    const e = err as { message?: string, data?: { message?: string } } | undefined
    error.value = e?.message ?? e?.data?.message ?? 'Upload failed'
  }
  finally {
    uploading.value = false
  }
}

onMounted(fetchPhotos)
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle class="text-base">
        Photos
      </CardTitle>
      <CardDescription>
        Up to 3 uploads per vehicle per day, 5 MB each.
        <span v-if="quotaRemaining !== null"> {{ quotaRemaining }} remaining today.</span>
      </CardDescription>
    </CardHeader>
    <CardContent class="space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <input
          id="photo-upload-input"
          type="file"
          accept="image/*"
          :disabled="uploading"
          class="block text-sm file:mr-3 file:rounded-md file:border-0 file:bg-secondary file:px-3 file:py-1.5 file:text-sm file:font-medium hover:file:bg-secondary/80"
          @change="pickFile"
        >
        <Button
          type="button"
          :disabled="!file || uploading"
          @click="upload"
        >
          {{ uploading ? `Uploading ${progress}%` : 'Upload' }}
        </Button>
      </div>

      <p
        v-if="error"
        class="text-sm text-destructive"
        role="alert"
      >
        {{ error }}
      </p>

      <div
        v-if="photos.length"
        class="grid grid-cols-2 gap-2 sm:grid-cols-3"
      >
        <a
          v-for="p in photos"
          :key="p.key"
          :href="p.url"
          target="_blank"
          rel="noopener"
          class="block"
        >
          <img
            :src="p.url"
            :alt="p.key"
            loading="lazy"
            class="aspect-square w-full rounded-md border border-border object-cover"
          >
        </a>
      </div>
      <p
        v-else-if="!error"
        class="text-sm text-muted-foreground"
      >
        No photos yet.
      </p>
    </CardContent>
  </Card>
</template>