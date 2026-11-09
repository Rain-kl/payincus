<script setup lang="ts">
import { computed, ref, useId } from 'vue'
import { XCircle, AlertCircle } from 'lucide-vue-next'

export type InputSize = 'sm' | 'md' | 'lg'

export interface InputProps {
  modelValue?: string | number
  type?: string
  placeholder?: string
  disabled?: boolean
  readonly?: boolean
  clearable?: boolean
  error?: string
  hint?: string
  label?: string
  id?: string
  name?: string
  autocomplete?: string
  maxlength?: number
  size?: InputSize
  block?: boolean
}

const props = withDefaults(defineProps<InputProps>(), {
  modelValue: '',
  type: 'text',
  placeholder: '',
  disabled: false,
  readonly: false,
  clearable: false,
  error: '',
  hint: '',
  label: '',
  id: '',
  name: '',
  autocomplete: 'off',
  size: 'md',
  block: true
})

const emit = defineEmits<{
  (e: 'update:modelValue', value: string | number): void
  (e: 'clear'): void
  (e: 'focus', event: FocusEvent): void
  (e: 'blur', event: FocusEvent): void
  (e: 'change', event: Event): void
  (e: 'enter', event: KeyboardEvent): void
}>()

const generatedId = useId()
const inputId = computed(() => props.id || generatedId)
const inputRef = ref<HTMLInputElement | null>(null)

const handleInput = (event: Event) => {
  const target = event.target as HTMLInputElement
  emit('update:modelValue', target.value)
}

const handleClear = () => {
  emit('update:modelValue', '')
  emit('clear')
  inputRef.value?.focus()
}

const handleKeyDown = (event: KeyboardEvent) => {
  if (event.key === 'Enter') {
    emit('enter', event)
  }
}

const handleWrapperClick = () => {
  if (!props.disabled && !props.readonly) {
    inputRef.value?.focus()
  }
}

const sizeClasses = computed(() => {
  switch (props.size) {
    case 'sm':
      return 'min-h-[36px] px-3 text-xs'
    case 'lg':
      return 'min-h-[50px] px-4 text-base'
    case 'md':
    default:
      return 'min-h-[44px] px-3.5 text-sm'
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

defineExpose({
  focus: () => inputRef.value?.focus(),
  blur: () => inputRef.value?.blur(),
  inputRef
})
</script>

<template>
  <div class="flex flex-col gap-1.5" :class="block ? 'w-full' : 'inline-flex'">
    <!-- Top Label -->
    <label
      v-if="label || $slots.label"
      :for="inputId"
      class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center justify-between"
    >
      <slot name="label">{{ label }}</slot>
    </label>

    <!-- Input Box Wrapper -->
    <div
      class="group relative flex items-center rounded-xl bg-[var(--bg-surface-soft)] border transition-all duration-200 cursor-text"
      :class="[
        sizeClasses,
        wrapperBorderClasses
      ]"
      @click="handleWrapperClick"
    >
      <!-- Prefix Slot -->
      <div v-if="$slots.prefix" class="mr-2 flex items-center text-[var(--text-tertiary)] shrink-0">
        <slot name="prefix" />
      </div>

      <!-- Native Input -->
      <input
        :id="inputId"
        ref="inputRef"
        :name="name || undefined"
        :type="type"
        :value="modelValue"
        :placeholder="placeholder"
        :disabled="disabled"
        :readonly="readonly"
        :autocomplete="autocomplete"
        :maxlength="maxlength"
        class="ui-native-input w-full bg-transparent text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none disabled:cursor-not-allowed border-0 p-0 text-inherit"
        @input="handleInput"
        @focus="emit('focus', $event)"
        @blur="emit('blur', $event)"
        @change="emit('change', $event)"
        @keydown="handleKeyDown"
      />

      <!-- Clear Button -->
      <button
        v-if="clearable && modelValue && !disabled && !readonly"
        type="button"
        class="ml-1.5 text-[var(--text-tertiary)] hover:text-[var(--text-secondary)] transition-colors focus:outline-none shrink-0"
        title="清除"
        aria-label="清除输入"
        @click.stop="handleClear"
      >
        <XCircle class="w-4 h-4 fill-[var(--bg-surface)] stroke-[var(--text-tertiary)]" />
      </button>

      <!-- Suffix Slot -->
      <div v-if="$slots.suffix" class="ml-2 flex items-center text-[var(--text-tertiary)] shrink-0">
        <slot name="suffix" />
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
.ui-native-input {
  border: none !important;
  outline: none !important;
  box-shadow: none !important;
  background: transparent !important;
  background-color: transparent !important;
  border-radius: 0 !important;
  padding: 0 !important;
  -webkit-appearance: none;
}
.ui-native-input::-webkit-outer-spin-button,
.ui-native-input::-webkit-inner-spin-button {
  -webkit-appearance: none;
  margin: 0;
}
.ui-native-input[type='number'] {
  -moz-appearance: textfield;
}
</style>
