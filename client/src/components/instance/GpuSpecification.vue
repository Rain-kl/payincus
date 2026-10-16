<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GpuConfiguration } from '@/types/api'

const props = defineProps<{ configurations?: GpuConfiguration[]; hostId?: number | null }>()
const { t } = useI18n()
const configurations = computed(() => (props.configurations || []).filter(config => !props.hostId || config.hostIds.includes(props.hostId)))
function memory(mib: number): string {
  return mib >= 1024 ? `${(mib / 1024).toFixed(1).replace(/\.0$/, '')} GiB` : `${mib} MiB`
}
</script>

<template>
  <div class="min-w-0 text-center" data-gpu-spec>
    <div class="mb-0.5 text-xs text-themed-muted">{{ t('gpu.specLabel') }}</div>
    <template v-if="configurations.length">
      <div v-for="(configuration, index) in configurations" :key="configuration.hostIds.join(',')">
        <p v-if="index" class="my-1 text-xs text-themed-muted">{{ t('gpu.or') }}</p>
        <div v-for="device in configuration.devices" :key="`${device.name}:${device.memoryTotalMiB}`" class="break-words text-sm font-semibold leading-snug text-themed">
          <div>{{ device.name }}</div>
          <div class="text-xs font-medium text-themed-secondary" :title="t('gpu.perCardMemory')">{{ memory(device.memoryTotalMiB) }}<span v-if="device.count > 1"> ×{{ device.count }}</span></div>
        </div>
      </div>
    </template>
    <p v-else class="text-sm text-themed-secondary">{{ t('gpu.unknown') }}</p>
  </div>
</template>
