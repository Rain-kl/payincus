<script setup lang="ts">
import { computed } from 'vue'

export interface CardProps {
  title?: string
  subtitle?: string
  hoverable?: boolean
  padding?: 'none' | 'sm' | 'md' | 'lg'
  loading?: boolean
}

const props = withDefaults(defineProps<CardProps>(), {
  title: '',
  subtitle: '',
  hoverable: false,
  padding: 'md',
  loading: false
})

const emit = defineEmits<{
  (e: 'click', event: MouseEvent): void
}>()

const paddingClasses = computed(() => {
  switch (props.padding) {
    case 'none':
      return 'p-0'
    case 'sm':
      return 'p-4'
    case 'lg':
      return 'p-8 md:p-12'
    case 'md':
    default:
      return 'p-6 md:p-8'
  }
})

const hoverClasses = computed(() => {
  if (props.hoverable) {
    return 'hover:shadow-[0_8px_24px_rgba(0,0,0,0.08)] hover:border-[var(--border-hover)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] cursor-pointer active:scale-[0.99]'
  }
  return 'transition-colors duration-200'
})

const handleClick = (e: MouseEvent) => {
  if (props.hoverable) {
    emit('click', e)
  }
}
</script>

<template>
  <div
    class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] overflow-hidden"
    :class="[hoverClasses]"
    @click="handleClick"
  >
    <!-- Header slot or Title/Subtitle bar -->
    <div
      v-if="$slots.header || title || $slots.title || subtitle || $slots.extra"
      class="border-b border-[var(--border-color)] flex items-center justify-between gap-4"
      :class="padding === 'none' ? 'p-4' : padding === 'sm' ? 'px-4 py-3' : 'px-6 py-4 md:px-8'"
    >
      <slot name="header">
        <div class="min-w-0 flex-1">
          <slot name="title">
            <h3 v-if="title" class="font-semibold tracking-tight text-lg md:text-xl text-[var(--text-primary)] truncate">
              {{ title }}
            </h3>
          </slot>
          <slot name="subtitle">
            <p v-if="subtitle" class="text-sm text-[var(--text-secondary)] mt-0.5">
              {{ subtitle }}
            </p>
          </slot>
        </div>
        <div v-if="$slots.extra" class="flex items-center gap-2 shrink-0">
          <slot name="extra" />
        </div>
      </slot>
    </div>

    <!-- Body content -->
    <div :class="paddingClasses">
      <!-- Loading Skeleton state -->
      <div v-if="loading" class="animate-pulse space-y-3 py-2">
        <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-3/4"></div>
        <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-full"></div>
        <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-5/6"></div>
      </div>
      <slot v-else />
    </div>

    <!-- Footer slot -->
    <div
      v-if="$slots.footer"
      class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]"
      :class="padding === 'none' ? 'p-4' : padding === 'sm' ? 'px-4 py-3' : 'px-6 py-3.5 md:px-8'"
    >
      <slot name="footer" />
    </div>
  </div>
</template>
