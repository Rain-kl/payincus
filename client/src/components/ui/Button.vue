<script setup lang="ts">
import { computed } from 'vue'
import { Loader2 } from 'lucide-vue-next'

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type ButtonSize = 'sm' | 'md' | 'lg'

export interface ButtonProps {
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
  disabled?: boolean
  block?: boolean
  type?: 'button' | 'submit' | 'reset'
}

const props = withDefaults(defineProps<ButtonProps>(), {
  variant: 'primary',
  size: 'md',
  loading: false,
  disabled: false,
  block: false,
  type: 'button'
})

const emit = defineEmits<{
  (e: 'click', event: MouseEvent): void
}>()

const handleClick = (event: MouseEvent) => {
  if (props.disabled || props.loading) {
    event.preventDefault()
    return
  }
  emit('click', event)
}

const variantClasses = computed(() => {
  if (props.disabled) {
    return 'border-0'
  }
  switch (props.variant) {
    case 'primary':
      return 'bg-[var(--accent)] text-white hover:bg-[var(--accent-strong)] border-0 focus-visible:ring-[var(--accent)]'
    case 'secondary':
      return 'bg-[var(--btn-secondary-bg)] text-[var(--text-primary)] border-0 hover:bg-[var(--btn-secondary-hover-bg)] focus-visible:ring-[var(--accent)]'
    case 'ghost':
      return 'bg-transparent text-[var(--text-secondary)] border-0 hover:bg-[var(--btn-secondary-bg)] hover:text-[var(--text-primary)] focus-visible:ring-[var(--accent)]'
    case 'danger':
      return 'bg-[var(--btn-danger-bg)] text-[var(--error)] border-0 hover:bg-[var(--btn-danger-hover-bg)] focus-visible:ring-[var(--error)]'
    default:
      return 'bg-[var(--accent)] text-white hover:bg-[var(--accent-strong)] border-0 focus-visible:ring-[var(--accent)]'
  }
})

const sizeClasses = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'min-h-[36px] px-3.5 py-1 text-xs gap-1.5'
    case 'lg':
      return 'min-h-[50px] px-6 py-2.5 text-base gap-2.5'
    case 'md':
    default:
      return 'min-h-[44px] px-5 py-2 text-sm gap-2'
  }
})

const stateClasses = computed(() => {
  if (props.disabled) {
    return 'cursor-not-allowed text-[var(--text-tertiary)] bg-[var(--btn-secondary-bg)] opacity-60 pointer-events-none'
  }
  if (props.loading) {
    return 'cursor-wait pointer-events-none'
  }
  return 'active:scale-[0.98]'
})
</script>

<template>
  <button
    :type="type"
    :disabled="disabled || loading"
    :aria-busy="loading ? 'true' : undefined"
    class="inline-flex items-center justify-center font-medium rounded-full transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2"
    :class="[
      variantClasses,
      sizeClasses,
      stateClasses,
      block ? 'w-full' : ''
    ]"
    @click="handleClick"
  >
    <Loader2 v-if="loading" class="animate-spin shrink-0" :class="size === 'sm' ? 'w-3.5 h-3.5' : size === 'lg' ? 'w-5 h-5' : 'w-4 h-4'" />
    <slot v-else name="icon" />
    <slot name="prefix" />
    <slot />
    <slot name="suffix" />
  </button>
</template>
