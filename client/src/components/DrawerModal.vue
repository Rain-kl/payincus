<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'

const props = withDefaults(
  defineProps<{
    modelValue?: boolean
    show?: boolean
    title?: string
    maxWidth?: string
    closable?: boolean
    closeOnBackdrop?: boolean
    raw?: boolean
    contentClass?: string
    bodyClass?: string
    footerClass?: string
  }>(),
  {
    modelValue: undefined,
    show: undefined,
    title: '',
    maxWidth: 'max-w-md',
    closable: true,
    closeOnBackdrop: true,
    raw: false,
    contentClass: '',
    bodyClass: '',
    footerClass: ''
  }
)

const emit = defineEmits<{
  (e: 'update:modelValue', value: boolean): void
  (e: 'update:show', value: boolean): void
  (e: 'close'): void
}>()

const isOpen = computed(() => {
  if (props.modelValue !== undefined) {
    return props.modelValue
  }
  if (props.show !== undefined) {
    return props.show
  }
  return false
})

function close() {
  emit('update:modelValue', false)
  emit('update:show', false)
  emit('close')
}

function onBackdropClick() {
  if (props.closeOnBackdrop) {
    close()
  }
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape' && isOpen.value && props.closable) {
    close()
  }
}

watch(isOpen, (val) => {
  if (val) {
    window.addEventListener('keydown', onKeydown)
  } else {
    window.removeEventListener('keydown', onKeydown)
  }
})

onMounted(() => {
  if (isOpen.value) {
    window.addEventListener('keydown', onKeydown)
  }
})

onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="isOpen"
        class="modal-overlay"
        @click.self="onBackdropClick"
      >
        <div class="modal-backdrop" @click="onBackdropClick"></div>
        <aside
          class="modal-content"
          :class="[maxWidth, contentClass]"
          tabindex="-1"
          role="dialog"
          aria-modal="true"
        >
          <template v-if="raw">
            <slot />
          </template>

          <template v-else>
            <slot name="header">
              <div v-if="title || closable || $slots['header-content']" class="modal-header">
                <slot name="header-content">
                  <div class="flex items-center gap-2 min-w-0">
                    <slot name="icon" />
                    <h3 v-if="title" class="modal-title truncate">{{ title }}</h3>
                  </div>
                </slot>
                <button
                  v-if="closable"
                  type="button"
                  class="btn btn-ghost btn-sm"
                  aria-label="close"
                  @click="close"
                >
                  <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                  </svg>
                </button>
              </div>
            </slot>

            <div class="modal-body" :class="bodyClass">
              <slot />
            </div>

            <div v-if="$slots.footer" class="modal-footer" :class="footerClass">
              <slot name="footer" />
            </div>
          </template>
        </aside>
      </div>
    </Transition>
  </Teleport>
</template>
