<script setup lang="ts">
import { ArrowLeft } from 'lucide-vue-next'

export interface PageHeaderProps {
  title?: string
  description?: string
  back?: boolean
  backLabel?: string
}

withDefaults(defineProps<PageHeaderProps>(), {
  title: '',
  description: '',
  back: false,
  backLabel: '返回'
})

const emit = defineEmits<{
  (e: 'back'): void
}>()
</script>

<template>
  <header class="mb-6 md:mb-8 space-y-4">
    <div class="flex flex-col md:flex-row md:items-center md:justify-between gap-4">
      <!-- Title & Back Left Section -->
      <div class="flex items-start md:items-center gap-3 min-w-0">
        <!-- Back Button -->
        <slot name="back">
          <button
            v-if="back"
            type="button"
            class="mt-0.5 md:mt-0 inline-flex items-center justify-center w-9 h-9 rounded-full bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] shrink-0"
            :title="backLabel"
            :aria-label="backLabel"
            @click="emit('back')"
          >
            <ArrowLeft class="w-4 h-4 stroke-[2]" />
          </button>
        </slot>

        <!-- Title & Description -->
        <div class="min-w-0 flex-1">
          <slot name="title">
            <h1 v-if="title" class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)] truncate">
              {{ title }}
            </h1>
          </slot>
          <slot name="description">
            <p v-if="description" class="text-sm text-[var(--text-secondary)] mt-1 max-w-2xl leading-relaxed">
              {{ description }}
            </p>
          </slot>
        </div>
      </div>

      <!-- Action Toolbar Right Section -->
      <div v-if="$slots.actions || $slots.extra" class="flex items-center gap-3 shrink-0 flex-wrap">
        <slot name="actions">
          <slot name="extra" />
        </slot>
      </div>
    </div>

    <!-- Optional Bottom Slot (e.g. tabs, filters) -->
    <div v-if="$slots.bottom" class="pt-2">
      <slot name="bottom" />
    </div>
  </header>
</template>
