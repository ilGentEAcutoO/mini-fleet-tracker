<script lang="ts" setup>
import type { ToasterProps } from 'vue-sonner'

import {
  CircleCheckIcon,
  InfoIcon,
  Loader2Icon,
  OctagonXIcon,
  TriangleAlertIcon,
  XIcon,
} from 'lucide-vue-next'
import { computed } from 'vue'
import { Toaster as Sonner } from 'vue-sonner'
import { cn } from '@/lib/utils'

const props = defineProps<ToasterProps>()

// Merge caller-supplied toastOptions with our defaults. Done in script so the
// final v-bind on <Sonner> is the single source of truth (avoids the
// "specified more than once" TS warning that the stock shadcn template
// triggers).
const mergedToastOptions = computed<ToasterProps['toastOptions']>(() => ({
  classes: {
    toast: 'rounded-2xl',
    ...(props.toastOptions?.classes ?? {}),
  },
  ...props.toastOptions,
}))

const sonnerProps = computed<ToasterProps>(() => ({
  ...props,
  toastOptions: mergedToastOptions.value,
}))
</script>

<template>
  <Sonner
    :class="cn('toaster group', props.class)"
    :style="{
      '--normal-bg': 'var(--popover)',
      '--normal-text': 'var(--popover-foreground)',
      '--normal-border': 'var(--border)',
      '--border-radius': 'var(--radius)',
    }"
    v-bind="sonnerProps"
  >
    <template #success-icon>
      <CircleCheckIcon class="size-4" />
    </template>
    <template #info-icon>
      <InfoIcon class="size-4" />
    </template>
    <template #warning-icon>
      <TriangleAlertIcon class="size-4" />
    </template>
    <template #error-icon>
      <OctagonXIcon class="size-4" />
    </template>
    <template #loading-icon>
      <div>
        <Loader2Icon class="size-4 animate-spin" />
      </div>
    </template>
    <template #close-icon>
      <XIcon class="size-4" />
    </template>
  </Sonner>
</template>
