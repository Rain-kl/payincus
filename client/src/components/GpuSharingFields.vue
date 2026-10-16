<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import type { HostGpu } from '@/types/api'
const props = defineProps<{ hostIds: number[]; modelValue: boolean | null; deviceIds: number[]; nullable?: boolean; allowed?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [boolean | null]; 'update:deviceIds': [number[]] }>()
const { t } = useI18n()
const cards = ref<HostGpu[]>([])
const error = ref('')
const loading = ref(false)
let generation = 0
watch(() => props.hostIds.join(','), async () => {
  const current = ++generation
  cards.value = []; error.value = ''; loading.value = true
  try {
    const responses = await Promise.all(props.hostIds.map(id => api.hosts.getGpus(id)))
    if (current === generation) cards.value = responses.flatMap(data => data.gpus).filter(gpu => gpu.sharingEnabled || props.deviceIds.includes(gpu.id))
  } catch (e: any) { if (current === generation) error.value = e.message }
  finally { if (current === generation) loading.value = false }
}, { immediate: true })
function mode(event: Event) { const value = (event.target as HTMLSelectElement).value; emit('update:modelValue', value === 'inherit' ? null : value === 'true') }
function toggle(id: number, event: Event) { emit('update:deviceIds', (event.target as HTMLInputElement).checked ? [...props.deviceIds, id] : props.deviceIds.filter(value => value !== id)) }
</script>
<template>
  <fieldset class="space-y-3 rounded-lg border border-themed p-4">
    <legend class="px-1 text-sm font-semibold text-themed">{{ t('gpu.shared') }}</legend>
    <select v-if="nullable" class="input" :value="modelValue === null ? 'inherit' : String(modelValue)" @change="mode">
      <option value="inherit">{{ t('gpu.inherit') }}</option><option value="false">{{ t('gpu.disabled') }}</option><option value="true" :disabled="allowed === false">{{ t('gpu.enabled') }}</option>
    </select>
    <label v-else class="flex items-center gap-2 text-sm text-themed"><input type="checkbox" :checked="modelValue === true" :disabled="allowed === false" @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked)" />{{ t('gpu.enabled') }}</label>
    <p v-if="allowed === false" class="text-xs text-warning">{{ t('gpu.containerOnly') }}</p>
    <p class="text-xs text-themed-muted">{{ t('gpu.notice') }}</p>
    <p class="text-xs text-themed-muted">{{ t('gpu.bundleNotice') }}</p>
    <template v-if="modelValue === true">
      <p class="text-sm font-medium text-themed">{{ t('gpu.select') }}</p>
      <p v-if="loading" class="text-sm text-themed-muted">{{ t('gpu.loading') }}</p>
      <p v-else-if="!cards.length" class="text-sm text-themed-muted">{{ t('gpu.none') }}</p>
      <p v-if="error" class="text-sm text-error" role="alert">{{ error }}</p>
      <label v-for="gpu in cards" :key="gpu.id" class="flex items-start gap-2 text-sm text-themed">
        <input type="checkbox" :checked="deviceIds.includes(gpu.id)" @change="toggle(gpu.id, $event)" />
        <span>{{ gpu.name }} · {{ gpu.pciAddress }} · Host #{{ gpu.hostId }}<span class="block text-xs text-themed-muted">{{ gpu.memoryTotalMiB }} MiB · {{ t('gpu.seats') }} {{ gpu.usedInstances }}/{{ gpu.maxInstances }} · {{ t(gpu.ready ? 'gpu.ready' : 'gpu.unavailable') }}</span></span>
      </label>
    </template>
  </fieldset>
</template>
