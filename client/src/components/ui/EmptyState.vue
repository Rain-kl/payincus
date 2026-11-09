<script setup lang="ts">
import { Inbox } from 'lucide-vue-next'

export interface EmptyStateProps {
  title?: string
  description?: string
}

withDefaults(defineProps<EmptyStateProps>(), {
  title: '暂无数据',
  description: ''
})
</script>

<template>
  <div class="py-12 md:py-16 px-6 text-center flex flex-col items-center justify-center">
    <!-- Icon Container -->
    <div class="w-14 h-14 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--text-tertiary)] mb-4">
      <slot name="icon">
        <Inbox class="w-7 h-7 stroke-[1.5]" />
      </slot>
    </div>

    <!-- Title -->
    <slot name="title">
      <h4 v-if="title" class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)] mb-1">
        {{ title }}
      </h4>
    </slot>

    <!-- Description -->
    <slot name="description">
      <p v-if="description" class="text-sm text-[var(--text-secondary)] max-w-sm mx-auto leading-relaxed">
        {{ description }}
      </p>
    </slot>

    <!-- Action Buttons -->
    <div v-if="$slots.action || $slots.default" class="mt-6 flex items-center justify-center gap-3">
      <slot name="action">
        <slot />
      </slot>
    </div>
  </div>
</template>
