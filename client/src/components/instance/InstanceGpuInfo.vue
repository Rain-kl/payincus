<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import type { InstanceGpuInfo } from '@/types/api'
const props = defineProps<{ instanceId: number }>()
const { t } = useI18n()
const gpu = ref<InstanceGpuInfo | null>(null)
const error = ref('')
let generation = 0
watch(() => props.instanceId, async id => {
  const current = ++generation
  gpu.value = null; error.value = ''
  try { const data = await api.instances.getGpu(id); if (current === generation) gpu.value = data.gpu }
  catch (e: any) { if (current === generation) error.value = e.message }
}, { immediate: true })
</script>
<template>
  <div v-if="gpu" class="card space-y-2 p-5">
    <h3 class="font-semibold text-themed">{{ t('gpu.shared') }} · {{ gpu.name }}</h3>
    <p class="text-sm text-themed-secondary">{{ t('gpu.hostMemory') }}: {{ gpu.memoryTotalMiB }} MiB</p>
    <p class="text-sm text-themed-muted">{{ t('gpu.notice') }}</p>
    <p v-if="gpu.reservationReleased" class="text-sm text-warning">{{ t('gpu.released') }}</p>
  </div>
  <p v-else-if="error" class="text-sm text-error" role="alert">{{ error }}</p>
</template>
