<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import GpuSpecification from './GpuSpecification.vue'
import type { InstanceGpuInfo } from '@/types/api'
const props = defineProps<{ instanceId: number; refreshKey?: number }>()
const { t } = useI18n()
const gpu = ref<InstanceGpuInfo | null>(null)
const error = ref('')
let generation = 0
watch(() => [props.instanceId, props.refreshKey] as const, async ([id]) => {
  const current = ++generation
  gpu.value = null; error.value = ''
  try { const data = await api.instances.getGpu(id); if (current === generation) gpu.value = data.gpu }
  catch (e: any) { if (current === generation) error.value = e.message }
}, { immediate: true })
</script>
<template>
  <div v-if="gpu" class="card p-4 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
    <div class="space-y-1">
      <div class="flex items-center gap-2">
        <h3 class="text-sm font-semibold text-themed">{{ t('gpu.shared') }}</h3>
        <p v-if="gpu.reservationReleased" class="text-xs text-warning">{{ t('gpu.released') }}</p>
      </div>
      <p class="text-xs text-themed-muted">{{ t('gpu.notice') }}</p>
    </div>
    <div class="shrink-0">
      <GpuSpecification :configurations="[{ hostIds: [], devices: gpu.devices || [{ name: gpu.name, memoryTotalMiB: gpu.memoryTotalMiB, count: 1 }], count: gpu.count || 1 }]" />
    </div>
  </div>
  <p v-else-if="error" class="text-sm text-error" role="alert">{{ error }}</p>
</template>
