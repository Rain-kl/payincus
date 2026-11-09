<script setup lang="ts">
import { computed } from 'vue'

export type BadgeVariant = 'default' | 'success' | 'warning' | 'error' | 'info'
export type BadgeSize = 'sm' | 'md' | 'lg'
export type BadgeShape = 'pill' | 'rounded'

export interface BadgeProps {
  variant?: BadgeVariant
  size?: BadgeSize
  shape?: BadgeShape
  dot?: boolean
  pulse?: boolean
}

const props = withDefaults(defineProps<BadgeProps>(), {
  variant: 'default',
  size: 'md',
  shape: 'pill',
  dot: false,
  pulse: false
})

const variantClasses = computed(() => {
  switch (props.variant) {
    case 'success':
      return 'bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))]'
    case 'warning':
      return 'bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))]'
    case 'error':
      return 'bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))]'
    case 'info':
      return 'bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]'
    case 'default':
    default:
      return 'bg-[var(--bg-tertiary)] text-[var(--text-primary)] border border-[var(--border-color)]'
  }
})

const dotColorClasses = computed(() => {
  switch (props.variant) {
    case 'success':
      return 'bg-[var(--success)]'
    case 'warning':
      return 'bg-[var(--warning)]'
    case 'error':
      return 'bg-[var(--error)]'
    case 'info':
      return 'bg-[var(--accent)]'
    case 'default':
    default:
      return 'bg-[var(--text-tertiary)]'
  }
})

const sizeClasses = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'px-2 py-0.5 text-[11px] gap-1'
    case 'lg':
      return 'px-3 py-1 text-sm font-medium gap-2'
    case 'md':
    default:
      return 'px-2.5 py-0.5 text-xs font-medium gap-1.5'
  }
})

const shapeClasses = computed(() => {
  return props.shape === 'rounded' ? 'rounded-md' : 'rounded-full'
})
</script>

<template>
  <span
    class="inline-flex items-center tracking-tight select-none transition-colors duration-150"
    :class="[
      variantClasses,
      sizeClasses,
      shapeClasses
    ]"
  >
    <span
      v-if="dot"
      class="rounded-full shrink-0"
      :class="[
        dotColorClasses,
        size === 'sm' ? 'w-1.5 h-1.5' : 'w-2 h-2',
        pulse ? 'animate-pulse' : ''
      ]"
    />
    <slot />
  </span>
</template>
