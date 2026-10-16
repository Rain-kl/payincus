<script setup lang="ts">
import { ref, watch, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import type { HostGpu } from '@/types/api'
import { useToast } from '@/stores/toast'
const props = defineProps<{ hostId: number }>()
const { t } = useI18n()
const toast = useToast()
const cards = ref<HostGpu[]>([])
const loading = ref(false)
const error = ref('')
const saving = ref<number | null>(null)
const drafts = ref<Record<number, { sharingEnabled: boolean; maxInstances: number }>>({})
let generation = 0
let timer: ReturnType<typeof setInterval> | undefined
async function load() {
  const requestGeneration = ++generation
  loading.value = true
  try {
    const data = await api.hosts.getGpus(props.hostId)
    if (requestGeneration !== generation) return
    cards.value = data.gpus
    for (const gpu of data.gpus) if (!drafts.value[gpu.id]) drafts.value[gpu.id] = { sharingEnabled: gpu.sharingEnabled, maxInstances: gpu.maxInstances }
    error.value = ''
  } catch (e: any) { if (requestGeneration === generation) error.value = e.message }
  finally { if (requestGeneration === generation) loading.value = false }
}
async function save(gpu: HostGpu) {
  const draft = drafts.value[gpu.id]
  if (!Number.isSafeInteger(draft.maxInstances) || draft.maxInstances < 1 || draft.maxInstances > 1000) return
  saving.value = gpu.id
  try { await api.hosts.updateGpu(props.hostId, gpu.id, draft); toast.success(t('gpu.saved')); await load() }
  catch (e: any) { toast.error(e.message) }
  finally { saving.value = null }
}
function metric(value: number | null | undefined, unit = '') { return typeof value === 'number' ? `${value.toLocaleString()}${unit}` : t('gpu.unknown') }
watch(() => props.hostId, () => { generation++; cards.value = []; drafts.value = {}; void load() }, { immediate: true })
timer = setInterval(() => { void load() }, 30_000)
onUnmounted(() => { generation++; if (timer) clearInterval(timer) })
</script>
<template>
  <section class="space-y-4">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div><h2 class="text-lg font-semibold text-themed">{{ t('gpu.description') }}</h2><p class="mt-1 text-sm text-themed-muted">{{ t('gpu.notice') }}</p></div>
      <button class="btn-secondary" :disabled="loading" @click="load">{{ t('gpu.refresh') }}</button>
    </div>
    <p v-if="error" class="text-error" role="alert">{{ error }}</p>
    <p v-if="loading && !cards.length" class="text-themed-muted">{{ t('gpu.loading') }}</p>
    <div v-else-if="!cards.length" class="card p-6 text-themed-muted">{{ t('gpu.empty') }}</div>
    <article v-for="gpu in cards" :key="gpu.id" class="card space-y-5 p-5">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0"><h3 class="font-semibold text-themed">{{ gpu.name }}</h3><p class="break-all font-mono text-xs text-themed-muted">{{ gpu.uuid }} · {{ gpu.pciAddress }}</p></div>
        <span :class="gpu.ready ? 'badge-success' : 'badge-warning'">{{ t(gpu.ready ? 'gpu.ready' : 'gpu.unavailable') }}</span>
      </div>
      <p v-if="!gpu.runtimeReady" class="text-sm text-warning">{{ t('gpu.runtimeMissing') }}</p>
      <dl class="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.memory') }}</dt><dd class="mt-1 text-themed">{{ metric(gpu.metrics.memoryUsedMiB) }} / {{ metric(gpu.memoryTotalMiB, ' MiB') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.utilization') }}</dt><dd class="mt-1 text-themed">{{ metric(gpu.metrics.gpuUtilization, '%') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.memoryUtilization') }}</dt><dd class="mt-1 text-themed">{{ metric(gpu.metrics.memoryUtilization, '%') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.temperature') }}</dt><dd class="mt-1 text-themed">{{ metric(gpu.metrics.temperature, ' °C') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.power') }}</dt><dd class="mt-1 text-themed">{{ metric(gpu.metrics.powerDrawWatts) }} / {{ metric(gpu.metrics.powerLimitWatts, ' W') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.driver') }}</dt><dd class="mt-1 text-themed">{{ gpu.metrics.driverVersion || t('gpu.unknown') }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.seats') }}</dt><dd class="mt-1 text-themed">{{ gpu.usedInstances }} / {{ gpu.maxInstances }}</dd></div>
        <div><dt class="text-xs text-themed-muted">{{ t('gpu.updated') }}</dt><dd class="mt-1 text-themed">{{ new Date(gpu.lastSeenAt).toLocaleString() }}</dd></div>
      </dl>
      <form v-if="drafts[gpu.id]" class="flex flex-wrap items-end gap-4 border-t border-themed pt-4" @submit.prevent="save(gpu)">
        <label class="flex items-center gap-2 text-sm text-themed"><input v-model="drafts[gpu.id].sharingEnabled" type="checkbox" />{{ t('gpu.enabled') }}</label>
        <label class="text-sm text-themed"><span class="mb-1 block">{{ t('gpu.limit') }}</span><input v-model.number="drafts[gpu.id].maxInstances" class="input w-32" type="number" min="1" max="1000" required /></label>
        <button class="btn-primary" type="submit" :disabled="saving !== null">{{ t('gpu.save') }}</button>
      </form>
    </article>
  </section>
</template>
