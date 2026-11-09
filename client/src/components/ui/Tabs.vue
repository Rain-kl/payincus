<script setup lang="ts">
import { computed } from 'vue'

export type TabsSize = 'sm' | 'md' | 'lg'

export interface TabItem {
  key: string | number
  label: string
  icon?: any
  badge?: string | number
  disabled?: boolean
}

export interface TabsProps {
  modelValue?: string | number
  tabs?: Array<TabItem | string | number>
  size?: TabsSize
  block?: boolean
  ariaLabel?: string
}

const props = withDefaults(defineProps<TabsProps>(), {
  modelValue: '',
  tabs: () => [],
  size: 'md',
  block: false,
  ariaLabel: 'Tabs'
})

const emit = defineEmits<{
  (e: 'update:modelValue', value: string | number): void
  (e: 'change', value: string | number): void
}>()

const normalizedTabs = computed<TabItem[]>(() => {
  return props.tabs.map(tab => {
    if (typeof tab === 'object' && tab !== null) {
      return tab as TabItem
    }
    return {
      key: tab,
      label: String(tab)
    }
  })
})

const handleSelect = (key: string | number, disabled?: boolean) => {
  if (disabled) return
  emit('update:modelValue', key)
  emit('change', key)
}

const sizeClasses = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'px-3 py-1.5 text-xs'
    case 'lg':
      return 'px-5 py-2.5 text-base'
    case 'md':
    default:
      return 'px-4 py-2 text-sm'
  }
})
</script>

<template>
  <div
    class="flex items-center overflow-x-auto p-1.5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] max-w-full shadow-[0_2px_8px_rgba(0,0,0,0.02)]"
    :class="block ? 'w-full' : 'w-fit'"
  >
    <div
      class="flex items-center gap-1 shrink-0"
      :class="block ? 'w-full justify-between' : ''"
      role="tablist"
      :aria-label="ariaLabel"
    >
      <button
        v-for="tab in normalizedTabs"
        :key="String(tab.key)"
        type="button"
        role="tab"
        :aria-selected="modelValue === tab.key"
        :disabled="tab.disabled"
        class="whitespace-nowrap rounded-xl font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] flex items-center justify-center gap-2 select-none"
        :class="[
          sizeClasses,
          block ? 'flex-1' : '',
          tab.disabled
            ? 'opacity-40 cursor-not-allowed'
            : modelValue === tab.key
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/50'
        ]"
        @click="handleSelect(tab.key, tab.disabled)"
      >
        <component :is="tab.icon" v-if="tab.icon" class="w-4 h-4 shrink-0" />
        <slot name="tab" :tab="tab" :active="modelValue === tab.key">
          <span>{{ tab.label }}</span>
        </slot>
        <span
          v-if="tab.badge !== undefined"
          class="ml-1 px-1.5 py-0.5 rounded-full text-xs font-semibold leading-none transition-colors"
          :class="modelValue === tab.key
            ? 'bg-[var(--accent)] text-white'
            : 'bg-[var(--bg-tertiary)] text-[var(--text-secondary)]'"
        >
          {{ tab.badge }}
        </span>
      </button>
    </div>
  </div>
</template>
