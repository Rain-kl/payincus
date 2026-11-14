<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useToast } from '@/stores/toast'
import type { InstanceConfigResponse, InstanceConfig, InstanceGpuResponse, HostGpu } from '@/types/api'
import ChangeHostCard from '@/components/instance/ChangeHostCard.vue'

const props = defineProps<{
  instanceId: number
  canEditConfig?: boolean
  canManageGpu?: boolean
  showBootSettings?: boolean
  instanceType?: 'container' | 'vm'
  instanceStatus?: string
  isInstanceOwner?: boolean  // 是否是实例所有者
}>()

const emit = defineEmits<{
  'change-host-task': [taskId: number]
  'gpu-updated': []
}>()

const { t } = useI18n()
const toast = useToast()

const loading = ref(true)
const config = ref<InstanceConfigResponse | null>(null)
const editMode = ref(false)
const saving = ref(false)
const showSwapModal = ref(false)
const swapActionLoading = ref(false)

// 提升进程数相关
const showBoostModal = ref(false)
const boostLoading = ref(false)

// GPU 共享管理相关
const gpuLoading = ref(false)
const gpuSaving = ref(false)
const gpuData = ref<InstanceGpuResponse | null>(null)
const draftGpuShared = ref(false)
const selectedGpuIds = ref<number[]>([])
const gpuRestartRequired = ref(false)
const gpuManagement = computed(() => gpuData.value?.management || null)
const canModifyInstanceGpuState = computed(() => ['running', 'stopped', 'suspended', 'error'].includes(normalizedInstanceStatus.value))
const allocatedGpuIdSet = computed(() => new Set(gpuManagement.value?.allocatedDeviceIds || []))
const hasStaleOrUnmanagedLiveGpu = computed(() => {
  const mgmt = gpuManagement.value
  if (!mgmt?.liveGpuDevices?.length) return false
  if (mgmt.allocatedDeviceIds.length === 0) return true
  return mgmt.liveGpuDevices.some(dev => !dev.hostMatched)
})
const hasAnyGpuState = computed(() => {
  const mgmt = gpuManagement.value
  if (!mgmt) return false
  return mgmt.allocatedDeviceIds.length > 0 || Boolean(mgmt.liveGpuDevices?.length)
})
const hasGpuDraftChanges = computed(() => {
  const mgmt = gpuManagement.value
  if (!mgmt) return false
  const currentShared = mgmt.allocatedDeviceIds.length > 0 && !mgmt.reservationReleased
  if (draftGpuShared.value !== currentShared) return true
  if (!draftGpuShared.value) return hasStaleOrUnmanagedLiveGpu.value
  const currentSorted = [...mgmt.allocatedDeviceIds].sort((a, b) => a - b)
  const draftSorted = [...selectedGpuIds.value].sort((a, b) => a - b)
  return currentSorted.length !== draftSorted.length || currentSorted.some((id, idx) => id !== draftSorted[idx]) || hasStaleOrUnmanagedLiveGpu.value
})

// 进程数上限：LXC=1000, KVM=2000
const processLimit = computed(() => props.instanceType === 'vm' ? 2000 : 1000)
const canChangeHost = computed(() => props.isInstanceOwner === true || props.canEditConfig === true)

// 是否可以显示提升按钮（实例所有者且未达上限）
const canBoostProcesses = computed(() => {
  if (!config.value) return false
  if (!props.isInstanceOwner) return false  // 只有实例所有者可以提升
  const currentLimit = config.value.config.limits_processes ?? 500
  return currentLimit < processLimit.value
})

// Form data for editing - using snake_case to match API
interface ConfigForm {
  limits_read: string | null
  limits_write: string | null
  limits_read_iops: number | null
  limits_write_iops: number | null
  limits_ingress: string | null
  limits_egress: string | null
  limits_processes: number | null
  limits_cpu_priority: number | null
  boot_autostart: boolean | null
  boot_autostart_priority: number | null
  boot_autostart_delay: number | null
  boot_host_shutdown_timeout: number | null
}

const form = ref<ConfigForm>(getDefaultForm())

function getDefaultForm(): ConfigForm {
  return {
    limits_read: null,
    limits_write: null,
    limits_read_iops: null,
    limits_write_iops: null,
    limits_ingress: null,
    limits_egress: null,
    limits_processes: null,
    limits_cpu_priority: null,
    boot_autostart: null,
    boot_autostart_priority: null,
    boot_autostart_delay: null,
    boot_host_shutdown_timeout: null
  }
}

onMounted(async () => {
  await Promise.all([loadConfig(), loadGpuManagement()])
})

watch(() => props.instanceId, async () => {
  await Promise.all([loadConfig(), loadGpuManagement()])
})

watch(() => props.canManageGpu, (enabled) => {
  if (enabled) void loadGpuManagement()
})

function syncGpuDraft(response: InstanceGpuResponse | null): void {
  const mgmt = response?.management
  if (!mgmt) {
    draftGpuShared.value = false
    selectedGpuIds.value = []
    return
  }
  const hasActiveAllocation = mgmt.allocatedDeviceIds.length > 0 && !mgmt.reservationReleased
  draftGpuShared.value = hasActiveAllocation
  selectedGpuIds.value = [...mgmt.allocatedDeviceIds]
}

async function loadGpuManagement(): Promise<void> {
  if (!props.canManageGpu) {
    gpuData.value = null
    return
  }
  gpuLoading.value = true
  try {
    const res = await api.instances.getGpu(props.instanceId)
    gpuData.value = res
    syncGpuDraft(res)
  } catch (err: any) {
    toast.error(err.message || t('common.error'))
  } finally {
    gpuLoading.value = false
  }
}

function gpuCardDisabledReason(card: HostGpu): string | null {
  const heldByCurrent = allocatedGpuIdSet.value.has(card.id)
  const occupiedByCurrent = heldByCurrent && !gpuManagement.value?.reservationReleased ? 1 : 0
  const usedByOthers = Math.max(0, card.usedInstances - occupiedByCurrent)
  if (!card.sharingEnabled && !heldByCurrent) return t('instanceConfig.gpu.sharingDisabled')
  if (!card.ready) return t('instanceConfig.gpu.notReady')
  if (usedByOthers >= card.maxInstances) return t('instanceConfig.gpu.slotsFull')
  return null
}

function isGpuCardSelectable(card: HostGpu): boolean {
  if (selectedGpuIds.value.includes(card.id)) return true
  return gpuCardDisabledReason(card) === null
}

function toggleGpuCard(cardId: number): void {
  if (selectedGpuIds.value.includes(cardId)) {
    selectedGpuIds.value = selectedGpuIds.value.filter(id => id !== cardId)
  } else {
    selectedGpuIds.value = [...selectedGpuIds.value, cardId]
  }
}

async function saveGpuSharing(): Promise<void> {
  if (!props.canManageGpu || gpuSaving.value) return
  if (draftGpuShared.value && selectedGpuIds.value.length === 0) {
    toast.error(t('instanceConfig.gpu.selectRequired'))
    return
  }
  gpuSaving.value = true
  try {
    const res = await api.instances.updateGpu(props.instanceId, {
      gpuShared: draftGpuShared.value,
      gpuDeviceIds: draftGpuShared.value ? selectedGpuIds.value : []
    })
    gpuData.value = res
    syncGpuDraft(res)
    gpuRestartRequired.value = Boolean(res.restartRequired)
    toast.success(t('instanceConfig.gpu.saveSuccess'))
    emit('gpu-updated')
  } catch (err: any) {
    toast.error(err.message || t('common.error'))
  } finally {
    gpuSaving.value = false
  }
}

async function loadConfig(): Promise<void> {
  loading.value = true
  try {
    const response = await api.instances.getConfig(props.instanceId)
    config.value = response as InstanceConfigResponse
    resetForm()
    editMode.value = false
  } catch (err: any) {
    toast.error(err.message || t('common.error'))
  } finally {
    loading.value = false
  }
}

function resetForm(): void {
  if (!config.value) return
  // Check overrides to determine which fields have instance-level values
  const overrides = config.value.overrides
  const cfg = config.value.config
  
  form.value = {
    limits_read: overrides.limits_read ? cfg.limits_read : null,
    limits_write: overrides.limits_write ? cfg.limits_write : null,
    limits_read_iops: overrides.limits_read_iops ? cfg.limits_read_iops : null,
    limits_write_iops: overrides.limits_write_iops ? cfg.limits_write_iops : null,
    limits_ingress: overrides.limits_ingress ? cfg.limits_ingress : null,
    limits_egress: overrides.limits_egress ? cfg.limits_egress : null,
    limits_processes: overrides.limits_processes ? cfg.limits_processes : null,
    limits_cpu_priority: overrides.limits_cpu_priority ? cfg.limits_cpu_priority : null,
    boot_autostart: overrides.boot_autostart ? cfg.boot_autostart : null,
    boot_autostart_priority: overrides.boot_autostart_priority ? cfg.boot_autostart_priority : null,
    boot_autostart_delay: overrides.boot_autostart_delay ? cfg.boot_autostart_delay : null,
    boot_host_shutdown_timeout: overrides.boot_host_shutdown_timeout ? cfg.boot_host_shutdown_timeout : null
  }
}

function beginEdit(): void {
  resetForm()
  // A nullable checkbox must display its effective value until explicitly reset.
  form.value.boot_autostart = config.value?.config.boot_autostart ?? null
  editMode.value = true
}

async function saveConfig(): Promise<void> {
  if (!props.canEditConfig || saving.value || !config.value) return
  const fields = {
    limits_read: 'limitsRead', limits_write: 'limitsWrite', limits_read_iops: 'limitsReadIops', limits_write_iops: 'limitsWriteIops',
    limits_ingress: 'limitsIngress', limits_egress: 'limitsEgress', limits_processes: 'limitsProcesses', limits_cpu_priority: 'limitsCpuPriority',
    boot_autostart: 'bootAutostart', boot_autostart_priority: 'bootAutostartPriority', boot_autostart_delay: 'bootAutostartDelay', boot_host_shutdown_timeout: 'bootHostShutdownTimeout'
  } as const
  const payload: Record<string, string | number | boolean | null> = {}
  for (const field of Object.keys(fields) as (keyof ConfigForm)[]) {
    if (ioLimitMode.value === 'throughput' && field.endsWith('_iops')) continue
    if (ioLimitMode.value === 'iops' && ['limits_read', 'limits_write'].includes(field)) continue
    if (props.instanceType === 'vm' && field === 'limits_processes') continue
    const value = form.value[field] === '' ? null : form.value[field]
    const previous = config.value.overrides[field] ? config.value.config[field] : null
    if (field === 'boot_autostart' && value === config.value.config[field] && previous === null) continue
    if (value !== previous) payload[fields[field]] = value
  }
  if (!Object.keys(payload).length) { editMode.value = false; return }
  saving.value = true
  try {
    await api.instances.updateInstanceConfig(props.instanceId, payload)
    toast.success(t('common.success'))
    editMode.value = false
    await loadConfig()
  } catch (err: any) {
    toast.error(err.message || t('common.error'))
  } finally { saving.value = false }
}

function resetField(field: keyof ConfigForm): void {
  (form.value as any)[field] = null
}

function isOverridden(field: keyof InstanceConfig): boolean {
  if (!config.value) return false
  return config.value.overrides[field] === true
}

function getEffectiveValue(field: keyof InstanceConfig): any {
  if (!config.value) return '-'
  return config.value.config[field]
}

// 格式化网络带宽值（将 Mbit/Gbit 转换为 Mbps/Gbps）
function formatNetworkValue(field: keyof InstanceConfig): string {
  const value = getEffectiveValue(field)
  if (!value || value === '-') return '-'
  return String(value).replace(/Mbit/gi, 'Mbps').replace(/Gbit/gi, 'Gbps')
}

function getPackageDefault(field: keyof InstanceConfig): any {
  if (!config.value) return '-'
  return config.value.packageDefaults[field]
}

const ioLimitMode = computed<'throughput' | 'iops'>(() => config.value?.ioLimitMode || 'throughput')
const normalizedInstanceStatus = computed<string>(() => props.instanceStatus?.toLowerCase() || '')
const canEnableSwap = computed<boolean>(() => {
  if (!config.value?.swap.available || config.value.swap.enabled) return false
  if (!props.isInstanceOwner) return false
  if (!['running', 'stopped'].includes(normalizedInstanceStatus.value)) return false
  if (config.value.swap.requiresRunning && normalizedInstanceStatus.value !== 'running') return false
  return true
})
const canDisableSwap = computed<boolean>(() => {
  if (!config.value?.swap.enabled) return false
  if (!props.isInstanceOwner) return false
  if (config.value.swap.kind !== 'container') return false
  if (!['running', 'stopped'].includes(normalizedInstanceStatus.value)) return false
  return true
})
const swapAction = computed<'enable' | 'disable'>(() => config.value?.swap.enabled ? 'disable' : 'enable')

function formatSwapSize(sizeMb: number): string {
  if (sizeMb >= 1024) {
    const sizeGb = sizeMb / 1024
    return sizeGb % 1 === 0 ? `${sizeGb.toFixed(0)} GB` : `${sizeGb.toFixed(1)} GB`
  }
  return `${sizeMb} MB`
}

async function submitSwapAction(): Promise<void> {
  if (!config.value) return
  swapActionLoading.value = true
  try {
    if (config.value.swap.enabled) {
      await api.instances.disableSwap(props.instanceId)
      toast.success(t('instanceConfig.swap.disableSuccess'))
    } else {
      await api.instances.enableSwap(props.instanceId)
      toast.success(t('instanceConfig.swap.enableSuccess'))
    }
    showSwapModal.value = false
    await loadConfig()
  } catch (err: any) {
    toast.error(err.message || t(`instanceConfig.swap.${swapAction.value}Failed`))
  } finally {
    swapActionLoading.value = false
  }
}

// 提升进程数限制
async function boostProcesses(): Promise<void> {
  boostLoading.value = true
  try {
    await api.instances.boostProcesses(props.instanceId)
    toast.success(t('instanceConfig.boostProcesses.success', { limit: processLimit.value }))
    showBoostModal.value = false
    await loadConfig()
  } catch (err: any) {
    toast.error(err.message || t('instanceConfig.boostProcesses.failed'))
  } finally {
    boostLoading.value = false
  }
}
</script>

<template>
  <div class="space-y-6">
    <!-- Loading -->
    <div v-if="loading" class="card shadow-pop p-8 text-center">
      <div class="loading-spinner w-8 h-8 mx-auto"></div>
      <p class="text-themed-muted mt-4">{{ t('common.loading') }}</p>
    </div>

    <template v-else-if="config">
      <!-- Header -->
      <div class="flex items-center justify-between">
        <h2 class="text-lg font-semibold text-themed">
          {{ t('instanceConfig.title') }}
        </h2>
        <div v-if="props.canEditConfig" class="flex gap-2">
          <button v-if="!editMode" class="btn-primary btn-sm" :disabled="!['running', 'stopped'].includes(normalizedInstanceStatus)" @click="beginEdit">{{ t('common.edit') }}</button>
          <template v-else>
            <button class="btn-secondary btn-sm" :disabled="saving" @click="editMode = false; resetForm()">{{ t('common.cancel') }}</button>
            <button class="btn-primary btn-sm" :disabled="saving" @click="saveConfig">{{ t(saving ? 'common.saving' : 'common.save') }}</button>
          </template>
        </div>
      </div>
      <p v-if="editMode" class="text-sm text-themed-muted">{{ t('instanceConfig.applyHint') }}</p>

      <ChangeHostCard
        :instance-id="props.instanceId"
        :can-change-host="canChangeHost"
        :instance-status="props.instanceStatus"
        @task-created="emit('change-host-task', $event)"
      />

      <!-- Shared GPU Management Section (Admin / Host Owner) -->
      <div v-if="props.canManageGpu" class="card shadow-pop p-6 space-y-4">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div>
            <div class="flex flex-wrap items-center gap-2">
              <h3 class="text-themed font-semibold text-sm">
                {{ t('instanceConfig.sections.gpu') }}
              </h3>
              <span
                v-if="gpuManagement?.reservationReleased"
                class="badge-warning px-2.5 py-0.5 rounded-xl text-xs font-medium"
              >
                {{ t('instanceConfig.gpu.releasedBadge') }}
              </span>
              <span
                v-else-if="(gpuManagement?.allocatedDeviceIds.length || 0) > 0"
                class="badge-success px-2.5 py-0.5 rounded-xl text-xs font-medium"
              >
                {{ t('instanceConfig.gpu.enabledBadge', { count: gpuManagement?.allocatedDeviceIds.length || 0 }) }}
              </span>
              <span
                v-else
                class="badge-muted px-2.5 py-0.5 rounded-xl text-xs font-medium"
              >
                {{ t('instanceConfig.gpu.disabledBadge') }}
              </span>
            </div>
            <p class="text-xs text-themed-muted mt-1">
              {{ t('instanceConfig.gpu.subtitle') }}
            </p>
          </div>
          <button
            type="button"
            class="btn-secondary btn-sm"
            :disabled="gpuLoading || gpuSaving"
            @click="loadGpuManagement"
          >
            {{ t('gpu.refresh') }}
          </button>
        </div>

        <div v-if="gpuLoading && !gpuManagement" class="text-xs text-themed-muted py-2">
          {{ t('instanceConfig.gpu.loading') }}
        </div>

        <template v-else-if="gpuManagement">
          <div
            v-if="hasStaleOrUnmanagedLiveGpu"
            class="rounded-xl border border-warning/30 bg-warning/5 p-3 text-xs text-themed space-y-1.5"
          >
            <p class="font-medium text-warning">{{ t('instanceConfig.gpu.staleLiveWarning') }}</p>
            <div v-if="gpuManagement.liveGpuDevices?.length" class="flex flex-wrap items-center gap-2 text-themed-secondary">
              <span>{{ t('instanceConfig.gpu.liveDevicesLabel') }}</span>
              <span
                v-for="dev in gpuManagement.liveGpuDevices"
                :key="dev.name"
                class="inline-flex items-center gap-1 rounded-lg bg-[var(--bg-surface)] px-2 py-0.5 font-mono text-xs border border-[var(--border-default)]"
              >
                <span>{{ dev.name }}</span>
                <span v-if="dev.pci">· {{ dev.pci }}</span>
                <span :class="dev.hostMatched ? 'text-success' : 'text-error'">
                  ({{ dev.hostMatched ? t('instanceConfig.gpu.hostMatched') : t('instanceConfig.gpu.hostMissing') }})
                </span>
              </span>
            </div>
          </div>

          <p v-if="gpuManagement.liveSyncError" class="text-xs text-warning">
            {{ t('instanceConfig.gpu.liveSyncError', { error: gpuManagement.liveSyncError }) }}
          </p>

          <p v-if="!gpuManagement.supported" class="text-xs text-warning">
            {{ t('gpu.containerOnly') }}
          </p>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
            <label
              class="flex items-start gap-3 rounded-xl border p-3.5 cursor-pointer transition-colors"
              :class="!draftGpuShared ? 'border-accent-primary bg-accent-primary/5' : 'border-[var(--border-default)] hover:bg-[var(--bg-surface-soft)]'"
            >
              <input
                v-model="draftGpuShared"
                type="radio"
                :value="false"
                :disabled="gpuSaving"
                class="mt-1"
              />
              <div class="min-w-0">
                <div class="text-sm font-medium text-themed">{{ t('instanceConfig.gpu.modeDisable') }}</div>
                <div class="text-xs text-themed-muted mt-0.5">{{ t('instanceConfig.gpu.modeDisableDesc') }}</div>
              </div>
            </label>

            <label
              class="flex items-start gap-3 rounded-xl border p-3.5 transition-colors"
              :class="[
                gpuManagement.supported ? 'cursor-pointer' : 'opacity-60 cursor-not-allowed',
                draftGpuShared ? 'border-accent-primary bg-accent-primary/5' : 'border-[var(--border-default)] hover:bg-[var(--bg-surface-soft)]'
              ]"
            >
              <input
                v-model="draftGpuShared"
                type="radio"
                :value="true"
                :disabled="gpuSaving || !gpuManagement.supported"
                class="mt-1"
              />
              <div class="min-w-0">
                <div class="text-sm font-medium text-themed">{{ t('instanceConfig.gpu.modeEnable') }}</div>
                <div class="text-xs text-themed-muted mt-0.5">{{ t('instanceConfig.gpu.modeEnableDesc') }}</div>
              </div>
            </label>
          </div>

          <div v-if="draftGpuShared" class="space-y-3">
            <div v-if="!gpuManagement.hostGpus.length" class="rounded-xl border border-[var(--border-default)] bg-[var(--bg-surface-soft)] p-3 text-xs text-themed-muted">
              {{ t('instanceConfig.gpu.noHostGpus') }}
            </div>
            <div v-else class="space-y-2">
              <label
                v-for="card in gpuManagement.hostGpus"
                :key="card.id"
                class="flex flex-wrap items-center justify-between gap-3 rounded-xl border p-3 transition-colors"
                :class="[
                  selectedGpuIds.includes(card.id) ? 'border-accent-primary bg-accent-primary/5' : 'border-[var(--border-default)]',
                  isGpuCardSelectable(card) ? 'cursor-pointer hover:bg-[var(--bg-surface-soft)]' : 'opacity-60 cursor-not-allowed'
                ]"
              >
                <div class="flex items-start gap-3 min-w-0">
                  <input
                    type="checkbox"
                    :checked="selectedGpuIds.includes(card.id)"
                    :disabled="gpuSaving || !isGpuCardSelectable(card)"
                    class="mt-1"
                    @change="toggleGpuCard(card.id)"
                  />
                  <div class="min-w-0">
                    <div class="text-sm font-medium text-themed break-words">
                      {{ card.name }} · {{ Math.round(card.memoryTotalMiB / 1024 * 10) / 10 }} GiB
                    </div>
                    <div class="font-mono text-xs text-themed-muted break-all">
                      {{ card.pciAddress }} · {{ card.uuid }}
                    </div>
                  </div>
                </div>
                <div class="flex flex-wrap items-center gap-2 text-xs">
                  <span class="text-themed-secondary">
                    {{ t('gpu.seats') }}: {{ card.usedInstances }} / {{ card.maxInstances }}
                  </span>
                  <span
                    :class="gpuCardDisabledReason(card) ? 'badge-warning' : 'badge-success'"
                    class="px-2 py-0.5 rounded-lg"
                  >
                    {{ gpuCardDisabledReason(card) || t('gpu.ready') }}
                  </span>
                </div>
              </label>
              <p class="text-xs text-themed-muted">{{ t('gpu.notice') }} {{ t('gpu.bundleNotice') }}</p>
            </div>
          </div>

          <p v-if="gpuRestartRequired" class="rounded-xl border border-warning/30 bg-warning/5 p-3 text-xs text-warning">
            {{ t('instanceConfig.gpu.restartNotice') }}
          </p>
          <p v-else-if="normalizedInstanceStatus === 'running'" class="text-xs text-themed-muted">
            {{ t('instanceConfig.gpu.runningNotice') }}
          </p>

          <div class="flex flex-wrap items-center justify-between gap-3 pt-1">
            <p v-if="draftGpuShared && selectedGpuIds.length === 0" class="text-xs text-warning">
              {{ t('instanceConfig.gpu.selectRequired') }}
            </p>
            <span v-else></span>
            <button
              type="button"
              class="btn-primary btn-sm"
              :disabled="gpuSaving || !canModifyInstanceGpuState || !hasGpuDraftChanges || (draftGpuShared && selectedGpuIds.length === 0)"
              @click="saveGpuSharing"
            >
              {{
                gpuSaving
                  ? t('instanceConfig.gpu.saving')
                  : (!draftGpuShared && hasAnyGpuState ? t('instanceConfig.gpu.clearButton') : t('instanceConfig.gpu.saveButton'))
              }}
            </button>
          </div>
        </template>
      </div>

      <!-- SWAP Section -->
      <div v-if="config.swap.available || config.swap.enabled" class="card shadow-pop p-6">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h3 class="text-themed font-semibold text-sm mb-1">
              {{ t('instanceConfig.sections.swap') }}
            </h3>
            <p class="text-sm text-themed-muted">
              {{ t('instanceConfig.swap.size') }}: {{ formatSwapSize(config.swap.sizeMb) }}
            </p>
            <p class="text-xs text-themed-muted mt-2">
              {{ config.swap.requiresRunning ? t('instanceConfig.swap.vmHint') : t('instanceConfig.swap.containerHint') }}
            </p>
            <p class="text-xs mt-1 text-themed-accent">
              {{ t('instanceConfig.swap.toggleHint') }}
            </p>
          </div>
          <div class="flex flex-col items-end gap-2">
            <span
              :class="config.swap.enabled ? 'badge-success' : 'badge-muted'"
              class="px-2.5 py-1 rounded-xl text-xs font-medium"
            >
              {{ config.swap.enabled ? t('instanceConfig.swap.enabled') : t('instanceConfig.swap.disabled') }}
            </span>
            <button
              v-if="!config.swap.enabled && props.isInstanceOwner"
              class="btn-primary btn-sm"
              :disabled="!canEnableSwap"
              @click="showSwapModal = true"
            >
              {{ t('instanceConfig.swap.enableButton') }}
            </button>
            <button
              v-else-if="config.swap.enabled && props.isInstanceOwner"
              class="btn-secondary btn-sm"
              :disabled="!canDisableSwap"
              @click="showSwapModal = true"
            >
              {{ t('instanceConfig.swap.disableButton') }}
            </button>
            <p
              v-if="!config.swap.enabled && props.isInstanceOwner && config.swap.requiresRunning && normalizedInstanceStatus !== 'running'"
              class="text-xs text-themed-muted text-right max-w-48"
            >
              {{ t('instanceConfig.swap.runningRequired') }}
            </p>
          </div>
        </div>
      </div>

      <!-- Storage I/O Section -->
      <div class="card shadow-pop p-6">
        <h3 class="text-themed font-semibold text-sm mb-4">
          {{ t('instanceConfig.sections.storageIO') }}
        </h3>
        <div v-if="ioLimitMode === 'throughput'" class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <!-- Read Limit -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsRead') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model="form.limits_read" :disabled="saving" type="text" class="input flex-1" :placeholder="String(getPackageDefault('limits_read'))" />
                <button v-if="form.limits_read !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_read')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_read') }}</span>
                <span v-if="isOverridden('limits_read')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Write Limit -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsWrite') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model="form.limits_write" :disabled="saving" type="text" class="input flex-1" :placeholder="String(getPackageDefault('limits_write'))" />
                <button v-if="form.limits_write !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_write')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_write') }}</span>
                <span v-if="isOverridden('limits_write')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>
        </div>

        <div v-else class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <!-- Read IOPS -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsReadIops') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.limits_read_iops" :disabled="saving" type="number" class="input flex-1" :placeholder="String(getPackageDefault('limits_read_iops'))" />
                <button v-if="form.limits_read_iops !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_read_iops')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_read_iops') }}</span>
                <span v-if="isOverridden('limits_read_iops')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Write IOPS -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsWriteIops') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.limits_write_iops" :disabled="saving" type="number" class="input flex-1" :placeholder="String(getPackageDefault('limits_write_iops'))" />
                <button v-if="form.limits_write_iops !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_write_iops')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_write_iops') }}</span>
                <span v-if="isOverridden('limits_write_iops')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>
        </div>
      </div>

      <!-- Network Limits Section -->
      <div class="card shadow-pop p-6">
        <h3 class="text-themed font-semibold text-sm mb-4">
          {{ t('instanceConfig.sections.networkLimits') }}
        </h3>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <!-- Ingress -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsIngress') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model="form.limits_ingress" :disabled="saving" type="text" class="input flex-1" :placeholder="String(getPackageDefault('limits_ingress'))" />
                <button v-if="form.limits_ingress !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_ingress')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ formatNetworkValue('limits_ingress') }}</span>
                <span v-if="isOverridden('limits_ingress')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Egress -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsEgress') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model="form.limits_egress" :disabled="saving" type="text" class="input flex-1" :placeholder="String(getPackageDefault('limits_egress'))" />
                <button v-if="form.limits_egress !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_egress')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ formatNetworkValue('limits_egress') }}</span>
                <span v-if="isOverridden('limits_egress')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>
        </div>
      </div>

      <!-- Process & Scheduling Section -->
      <div class="card shadow-pop p-6">
        <h3 class="text-themed font-semibold text-sm mb-4">
          {{ t('instanceConfig.sections.processScheduling') }}
        </h3>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <!-- Processes -->
          <div v-if="props.instanceType !== 'vm'">
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsProcesses') }}</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.limits_processes" :disabled="saving" type="number" class="input flex-1" :placeholder="String(getPackageDefault('limits_processes'))" />
                <button v-if="form.limits_processes !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_processes')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_processes') }}</span>
                <span v-if="isOverridden('limits_processes')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
                <!-- 提升进程数按钮 -->
                <button
                  v-if="canBoostProcesses && !editMode"
                  class="ml-2 px-2 py-1 text-xs rounded-xl bg-accent-primary/10 text-accent-primary hover:bg-accent-primary/20 transition-colors flex items-center gap-1 border-2 border-accent-primary/30"
                  @click="showBoostModal = true"
                >
                  <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 7h8m0 0v8m0-8l-8 8-4-4-6 6" />
                  </svg>
                  {{ t('instanceConfig.boostProcesses.button') }}
                </button>
              </div>
            </template>
          </div>

          <!-- CPU Priority -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.limitsCpuPriority') }} (0-10)</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.limits_cpu_priority" :disabled="saving" type="number" min="0" max="10" class="input flex-1" :placeholder="String(getPackageDefault('limits_cpu_priority'))" />
                <button v-if="form.limits_cpu_priority !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('limits_cpu_priority')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('limits_cpu_priority') }}</span>
                <span v-if="isOverridden('limits_cpu_priority')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>
        </div>
      </div>

      <!-- Boot Settings Section -->
      <div v-if="props.showBootSettings !== false" class="card shadow-pop p-6">
        <h3 class="text-themed font-semibold text-sm mb-4">
          {{ t('instanceConfig.sections.bootSettings') }}
        </h3>
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <!-- Autostart -->
          <div class="md:col-span-2">
            <template v-if="editMode">
              <label class="flex items-center gap-2 cursor-pointer">
                <input :disabled="saving" :checked="form.boot_autostart ?? config.config.boot_autostart" type="checkbox" class="w-4 h-4 rounded" @change="form.boot_autostart = ($event.target as HTMLInputElement).checked" />
                <span class="text-sm text-themed-secondary">{{ t('packageForm.fields.bootAutostart') }}</span>
                <button v-if="form.boot_autostart !== null" :disabled="saving" class="btn-ghost btn-sm ml-2" :title="t('instanceConfig.resetToDefault')" @click="resetField('boot_autostart')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </label>
            </template>
            <template v-else>
              <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.bootAutostart') }}</label>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('boot_autostart') ? t('common.yes') : t('common.no') }}</span>
                <span v-if="isOverridden('boot_autostart')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Autostart Priority -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.bootAutostartPriority') }} (0-100)</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.boot_autostart_priority" :disabled="saving" type="number" min="0" max="100" class="input flex-1" :placeholder="String(getPackageDefault('boot_autostart_priority'))" />
                <button v-if="form.boot_autostart_priority !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('boot_autostart_priority')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('boot_autostart_priority') }}</span>
                <span v-if="isOverridden('boot_autostart_priority')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Autostart Delay -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.bootAutostartDelay') }} ({{ t('packageForm.units.seconds') }})</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.boot_autostart_delay" :disabled="saving" type="number" min="0" max="600" class="input flex-1" :placeholder="String(getPackageDefault('boot_autostart_delay'))" />
                <button v-if="form.boot_autostart_delay !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('boot_autostart_delay')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('boot_autostart_delay') }}</span>
                <span v-if="isOverridden('boot_autostart_delay')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>

          <!-- Shutdown Timeout -->
          <div>
            <label class="block text-xs text-themed-muted mb-1">{{ t('packageForm.fields.bootHostShutdownTimeout') }} ({{ t('packageForm.units.seconds') }})</label>
            <template v-if="editMode">
              <div class="flex gap-2">
                <input v-model.number="form.boot_host_shutdown_timeout" :disabled="saving" type="number" min="0" max="600" class="input flex-1" :placeholder="String(getPackageDefault('boot_host_shutdown_timeout'))" />
                <button v-if="form.boot_host_shutdown_timeout !== null" :disabled="saving" class="btn-ghost btn-sm" :title="t('instanceConfig.resetToDefault')" @click="resetField('boot_host_shutdown_timeout')">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" /></svg>
                </button>
              </div>
            </template>
            <template v-else>
              <div class="flex items-center gap-2">
                <span class="text-themed font-medium">{{ getEffectiveValue('boot_host_shutdown_timeout') }}</span>
                <span v-if="isOverridden('boot_host_shutdown_timeout')" class="badge-info text-xs">{{ t('instanceConfig.overridden') }}</span>
              </div>
            </template>
          </div>
        </div>
      </div>
    </template>

    <!-- 提升进程数确认弹窗 -->
    <!-- 提升进程数确认弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showBoostModal" class="modal-overlay" @click.self="showBoostModal = false">
          <!-- 背景遮罩 -->
          <div class="modal-backdrop" @click="showBoostModal = false"></div>
          <!-- 弹窗内容 -->
          <div class="modal-content max-w-md">
            <!-- 头部 -->
            <div class="modal-header">
              <div class="flex items-center gap-3">
                <div class="w-10 h-10 rounded-xl bg-accent-primary/10 flex items-center justify-center border-2 border-accent-primary/30">
                  <svg class="w-5 h-5 text-accent-primary" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 7h8m0 0v8m0-8l-8 8-4-4-6 6" />
                  </svg>
                </div>
                <div>
                  <h3 class="modal-title">
                    {{ t('instanceConfig.boostProcesses.title') }}
                  </h3>
                </div>
              </div>
              <button class="p-1 rounded hover:bg-gray-500/20" @click="showBoostModal = false">
                <svg class="w-5 h-5 text-themed-muted" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <!-- 内容 -->
            <div class="modal-body">
              <p class="text-sm leading-relaxed text-themed-muted">
                {{ t('instanceConfig.boostProcesses.confirm', {
                  type: instanceType === 'vm' ? 'KVM' : 'LXC',
                  limit: processLimit
                }) }}
              </p>
              <div class="mt-4 p-3 rounded-xl bg-accent-primary/10 border-2 border-accent-primary/30">
                <div class="flex items-center gap-2 text-sm text-accent-primary">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                  </svg>
                  {{ t('instanceConfig.boostProcesses.hint') }}
                </div>
              </div>
            </div>
            <!-- 底部按钮 -->
            <div class="modal-footer">
              <button
                class="btn-secondary"
                :disabled="boostLoading"
                @click="showBoostModal = false"
              >
                {{ t('common.cancel') }}
              </button>
              <button
                class="btn-primary flex items-center gap-2"
                :disabled="boostLoading"
                @click="boostProcesses"
              >
                <svg v-if="boostLoading" class="w-4 h-4 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
                  <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z" />
                </svg>
                {{ boostLoading ? t('common.processing') : t('common.confirm') }}
              </button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- SWAP 操作确认弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showSwapModal" class="modal-overlay" @click.self="showSwapModal = false">
          <div class="modal-backdrop" @click="showSwapModal = false"></div>
          <div class="modal-content max-w-md">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ t(`instanceConfig.swap.${swapAction}ConfirmTitle`) }}
              </h3>
              <button class="p-1 rounded hover:bg-gray-500/20" @click="showSwapModal = false">
                <svg class="w-5 h-5 text-themed-muted" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <div class="modal-body space-y-3">
              <p class="text-sm leading-relaxed text-themed-muted">
                {{ t(`instanceConfig.swap.${swapAction}ConfirmText`, { size: formatSwapSize(config?.swap.sizeMb || 0) }) }}
              </p>
              <div class="p-3 rounded-xl bg-accent-secondary/10 text-accent-secondary border-2 border-accent-secondary/30">
                <p class="text-sm">{{ t('instanceConfig.swap.toggleHint') }}</p>
              </div>
            </div>
            <div class="modal-footer">
              <button
                class="btn-secondary"
                :disabled="swapActionLoading"
                @click="showSwapModal = false"
              >
                {{ t('common.cancel') }}
              </button>
              <button
                class="flex items-center gap-2"
                :class="swapAction === 'enable' ? 'btn-primary' : 'btn-secondary'"
                :disabled="swapActionLoading"
                @click="submitSwapAction"
              >
                <svg v-if="swapActionLoading" class="w-4 h-4 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
                  <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                </svg>
                {{ swapActionLoading ? t('common.processing') : t('common.confirm') }}
              </button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
