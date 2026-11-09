<script setup lang="ts">
import { computed, useId } from 'vue'
import { ChevronDown, AlertCircle } from 'lucide-vue-next'

export type SelectSize = 'sm' | 'md' | 'lg'

export interface SelectOption {
  label: string
  value: string | number | boolean
  disabled?: boolean
}

export interface SelectProps {
  modelValue?: string | number | boolean
  options?: Array<SelectOption | string | number>
  placeholder?: string
  disabled?: boolean
  error?: string
  hint?: string
  label?: string
  id?: string
  name?: string
  size?: SelectSize
  block?: boolean
}

const props = withDefaults(defineProps<SelectProps>(), {
  modelValue: '',
  options: () => [],
  placeholder: '',
  disabled: false,
  error: '',
  hint: '',
  label: '',
  id: '',
  name: '',
  size: 'md',
  block: true
})

const emit = defineEmits<{
  (e: 'update:modelValue', value: any): void
  (e: 'change', value: any): void
  (e: 'focus', event: FocusEvent): void
  (e: 'blur', event: FocusEvent): void
}>()

const generatedId = useId()
const selectId = computed(() => props.id || generatedId)

const normalizedOptions = computed<SelectOption[]>(() => {
  return props.options.map(opt => {
    if (typeof opt === 'object' && opt !== null) {
      return opt as SelectOption
    }
    return {
      label: String(opt),
      value: opt
    }
  })
})

const handleChange = (event: Event) => {
  const target = event.target as HTMLSelectElement
  emit('update:modelValue', target.value)
  emit('change', target.value)
}

const sizeClasses = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'min-h-[36px] pl-3 pr-8 text-xs'
    case 'lg':
      return 'min-h-[50px] pl-4 pr-10 text-base'
    case 'md':
    default:
      return 'min-h-[44px] pl-3.5 pr-9 text-sm'
  }
})

const wrapperBorderClasses = computed(() => {
  if (props.error) {
    return 'border-[var(--error)] focus-within:border-[var(--error)] focus-within:ring-2 focus-within:ring-[var(--error)]/20'
  }
  if (props.disabled) {
    return 'border-[var(--border-color)] bg-[var(--bg-tertiary)] opacity-60 cursor-not-allowed'
  }
  return 'border-[var(--border-color)] hover:border-[var(--border-hover)] focus-within:border-[var(--accent)] focus-within:ring-2 focus-within:ring-[var(--accent)]/20'
})
</script>

<template>
  <div class="flex flex-col gap-1.5" :class="block ? 'w-full' : 'inline-flex'">
    <!-- Top Label -->
    <label
      v-if="label || $slots.label"
      :for="selectId"
      class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between"
    >
      <slot name="label">{{ label }}</slot>
    </label>

    <!-- Select Box Wrapper -->
    <div
      class="group relative flex items-center rounded-xl bg-[var(--bg-surface-soft)] border transition-all duration-200"
      :class="[
        sizeClasses,
        wrapperBorderClasses
      ]"
    >
      <!-- Prefix Slot -->
      <div v-if="$slots.prefix" class="mr-2 flex items-center text-[var(--text-tertiary)] shrink-0">
        <slot name="prefix" />
      </div>

      <!-- Native Select -->
      <select
        :id="selectId"
        :name="name || undefined"
        :value="modelValue"
        :disabled="disabled"
        class="ui-native-select w-full bg-transparent text-[var(--text-primary)] focus:outline-none disabled:cursor-not-allowed border-0 p-0 text-inherit cursor-pointer appearance-none"
        @change="handleChange"
        @focus="emit('focus', $event)"
        @blur="emit('blur', $event)"
      >
        <option v-if="placeholder" value="" disabled :selected="!modelValue">
          {{ placeholder }}
        </option>
        <slot>
          <option
            v-for="opt in normalizedOptions"
            :key="String(opt.value)"
            :value="opt.value"
            :disabled="opt.disabled"
            class="bg-[var(--bg-surface)] text-[var(--text-primary)]"
          >
            {{ opt.label }}
          </option>
        </slot>
      </select>

      <!-- Chevron Arrow Icon -->
      <div class="pointer-events-none absolute right-3 flex items-center text-[var(--text-secondary)] group-hover:text-[var(--text-primary)] transition-colors">
        <ChevronDown class="w-4 h-4 stroke-[2]" />
      </div>
    </div>

    <!-- Error Message -->
    <div v-if="error" class="flex items-center gap-1 text-xs text-[var(--error)] font-medium mt-0.5">
      <AlertCircle class="w-3.5 h-3.5 shrink-0" />
      <span>{{ error }}</span>
    </div>

    <!-- Helper Hint -->
    <p v-else-if="hint || $slots.hint" class="text-xs text-[var(--text-tertiary)] mt-0.5">
      <slot name="hint">{{ hint }}</slot>
    </p>
  </div>
</template>

<style scoped>
.ui-native-select {
  border: none !important;
  outline: none !important;
  box-shadow: none !important;
  background: transparent !important;
  background-color: transparent !important;
  border-radius: 0 !important;
  padding: 0 !important;
  -webkit-appearance: none;
  -moz-appearance: none;
  appearance: none;
}
</style>
