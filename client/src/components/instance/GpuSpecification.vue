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
  <div class="min-w-0 p-3 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] text-center flex flex-col items-center justify-center" data-gpu-spec>
    <div class="mb-1 text-xs font-medium text-[var(--text-secondary)]">{{ t('gpu.specLabel') }}</div>
    <template v-if="configurations.length">
      <div v-for="(configuration, index) in configurations" :key="configuration.hostIds.join(',')">
        <p v-if="index" class="my-1 text-xs text-[var(--text-tertiary)]">{{ t('gpu.or') }}</p>
        <div v-for="device in configuration.devices" :key="`${device.name}:${device.memoryTotalMiB}`" class="flex flex-wrap items-baseline justify-center gap-x-2 gap-y-0.5 text-sm font-semibold leading-snug text-[var(--text-primary)]">
          <span>{{ device.name }}</span>
          <span class="text-xs font-mono font-medium text-[var(--text-secondary)] whitespace-nowrap" :title="t('gpu.perCardMemory')">{{ memory(device.memoryTotalMiB) }}<span v-if="device.count > 1"> ×{{ device.count }}</span></span>
        </div>
      </div>
    </template>
    <p v-else class="text-sm text-[var(--text-secondary)]">{{ t('gpu.unknown') }}</p>
  </div>
</template>
