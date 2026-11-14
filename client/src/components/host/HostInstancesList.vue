<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { ChevronDown, RefreshCw, DollarSign, AlertCircle } from 'lucide-vue-next'
import type { InstanceConfigResponse } from '@/types/api'
import { Badge, Button, Card } from '@/components/ui'
import { formatCpu } from '@/utils/formatters'
import UserAvatar from '@/components/UserAvatar.vue'
import InstanceDisplayIcon from '@/components/InstanceDisplayIcon.vue'

interface TrafficData { monthlyUsed: string; monthlyUsedFormatted: string; monthlyLimit: string | null; monthlyLimitFormatted: string | null; percentage: number }
interface Instance {
  id: number
  name: string
  incusId?: string
  incus_id?: string
  status: string
  image: string
  imageName?: string | null
  ipv4?: string | null
  ipv6?: string | null
  network_mode?: string
  networkMode?: string
  cpu: number
  memory: number
  disk: number
  createdAt?: string
  created_at?: string
  updatedAt?: string
  updated_at?: string
  username?: string | null
  userEmail?: string | null
  userAvatarStyle?: string | null
  userAvatarBadgeId?: string | null
  ssh_port?: number | null
  port_limit?: number | null
  snapshot_limit?: number | null
  backup_limit?: number | null
  site_limit?: number | null
  swapSize?: number | null
  packagePlanId?: number | null
  instanceType?: 'container' | 'vm'
  billingPrice?: number | null
  expiresAt?: string | null
  expires_at?: string | null
  suspendReason?: string | null
  suspend_reason?: string | null
  autoRenew?: boolean
  trafficData?: TrafficData | null
  iconBadgeId?: string | null
}
interface StatusInfo { label: string; class: string; dot: string }

const props = defineProps<{
  instances: Instance[]
  selectedIds: Set<number>
  isAllSelected: boolean
  isPartialSelected: boolean
  expandedInstanceId: number | null
  detailConfigs: Record<number, InstanceConfigResponse | null>
  detailConfigErrors: Record<number, string>
  detailLoadingIds: Set<number>
  syncingInstanceIds: Set<number>
  swapActionLoadingIds: Set<number>
  resettingTrafficId: number | null
  recreateLoading: boolean
  recreateTargetId: number | null
  isDark: boolean
  getPaidIconType: (instance: any) => 'pro' | 'prime' | null
  getStatusInfo: (status: string) => StatusInfo
  formatMemory: (mb: number) => string
  formatDisk: (mb: number) => string
  getDisplayIp: (instance: any) => { ipv4: string | null; ipv6: string | null }
  canRecreateInstance: (instance: any) => boolean
  isInstanceExpanded: (id: number) => boolean
  isDetailLoading: (id: number) => boolean
  isInstanceSyncing: (id: number) => boolean
  isSwapActionLoading: (id: number) => boolean
  getDetailConfig: (id: number) => InstanceConfigResponse | null
  formatMoney: (value?: number | null) => string
  formatDateTime: (value?: string | null) => string
  formatBoolean: (value?: boolean | null) => string
  formatBandwidth: (value?: string | null) => string
  formatSwapSize: (sizeMb?: number | null) => string
  getInstanceTypeLabel: (instance: any) => string
  getNetworkModeLabel: (instance: any) => string | null
  getExpiryValue: (instance: any) => string | null
  getSuspendReason: (instance: any) => string | null
  getTrafficSummary: (instance: any) => string
  getTrafficPercentage: (instance: any) => number
  getTrafficBarClass: (instance: any) => string
  getSummaryMeta: (instance: any) => string[]
  getQuotaItems: (instance: any) => Array<{ label: string; value: string | number | null | undefined }>
  formatQuotaValue: (value: string | number | null | undefined) => string
  canToggleSwap: (instance: any) => boolean
  getSwapDisplay: (instance: any) => string
  getExpirySummary: (instance: any) => string
  canExport?: boolean
}>()

const emit = defineEmits<{
  (e: 'toggle-select', id: number): void
  (e: 'toggle-select-all'): void
  (e: 'toggle-expand', instance: any): void
  (e: 'retry-detail', instanceId: number): void
  (e: 'sync-instance', instance: any): void
  (e: 'toggle-swap', instance: any): void
  (e: 'open-single-config', instanceId: number): void
  (e: 'open-price', instance: any): void
  (e: 'open-bind-package', instance: any): void
  (e: 'open-reset-traffic', instance: any): void
  (e: 'open-recreate', instance: any): void
  (e: 'open-export', instance: any): void
  (e: 'open-delete', instanceId: number): void
  (e: 'open-detail', instanceId: number): void
}>()

const { t } = useI18n()

function isSelected(id: number): boolean { return props.selectedIds.has(id) }
function getDetailError(id: number): string { return props.detailConfigErrors[id] || '' }

function statusBadgeVariant(status: string): 'success' | 'warning' | 'error' | 'default' {
  const s = status?.toLowerCase()
  if (s === 'running') return 'success'
  if (s === 'suspended' || s === 'error') return 'error'
  if (s === 'starting' || s === 'stopping' || s === 'restarting' || s === 'creating') return 'warning'
  return 'default'
}

function isStatusPulsing(status: string): boolean {
  const s = status?.toLowerCase()
  return s === 'starting' || s === 'stopping' || s === 'restarting' || s === 'creating'
}
</script>

<template>
  <Card padding="sm">
    <div class="mb-4 flex items-center justify-between gap-3">
      <div class="text-xs font-semibold uppercase tracking-[0.2em] text-[var(--text-tertiary)]">
        {{ t('nav.instances') }}
      </div>
      <Button variant="secondary" size="sm" @click="emit('toggle-select-all')">
        <span class="relative flex h-4 w-4 items-center justify-center rounded border border-[var(--border-color)] bg-[var(--bg-surface)]">
          <span v-if="props.isAllSelected" class="h-2 w-2 rounded-sm bg-[var(--accent)]" />
          <span v-else-if="props.isPartialSelected" class="h-0.5 w-2 rounded-full bg-[var(--accent)]" />
        </span>
        <span class="font-mono tabular-nums">{{ props.selectedIds.size }}/{{ props.instances.length }}</span>
      </Button>
    </div>

    <div class="divide-y divide-[var(--border-color)] -mx-4 -mb-4 border-t border-[var(--border-color)]">
      <div
        v-for="instance in props.instances"
        :key="instance.id"
        class="transition-colors duration-200"
        :class="isSelected(instance.id) ? 'bg-[color-mix(in_srgb,var(--accent)_4%,var(--bg-surface))]' : 'bg-[var(--bg-surface)] hover:bg-[var(--bg-surface-soft)]/50'"
      >
        <div class="px-4 py-4 sm:px-6">
          <div class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
            <!-- Zone 1: Identity, Specs & Owner -->
            <div class="min-w-0 flex-1">
              <div class="flex items-start gap-3">
                <label class="flex items-center justify-center min-h-[36px] min-w-[24px] pt-0.5 cursor-pointer" @click.stop>
                  <input
                    type="checkbox"
                    class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                    :checked="isSelected(instance.id)"
                    @change="emit('toggle-select', instance.id)"
                  />
                </label>

                <div class="min-w-0 flex-1 space-y-1.5">
                  <div class="flex flex-wrap items-center gap-2">
                    <InstanceDisplayIcon
                      v-if="instance.iconBadgeId || props.getPaidIconType(instance)"
                      :badge-id="instance.iconBadgeId"
                      :fallback-icon="props.getPaidIconType(instance)"
                      :alt="instance.name"
                      :size="36"
                    />
                    <button
                      type="button"
                      class="text-left text-base font-semibold tracking-tight text-[var(--text-primary)] hover:text-[var(--accent)] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] rounded cursor-pointer"
                      @click="emit('open-detail', instance.id)"
                    >
                      {{ instance.name }}
                    </button>
                    <Badge
                      :variant="statusBadgeVariant(instance.status)"
                      dot
                      :pulse="isStatusPulsing(instance.status)"
                    >
                      {{ props.getStatusInfo(instance.status).label }}
                    </Badge>
                    <span class="text-xs text-[var(--text-tertiary)]">
                      {{ props.getInstanceTypeLabel(instance) }}
                      <template v-if="props.getNetworkModeLabel(instance)"> · {{ props.getNetworkModeLabel(instance) }}</template>
                    </span>
                  </div>

                  <div class="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-xs text-[var(--text-secondary)]">
                    <span class="font-mono tabular-nums text-[var(--text-tertiary)]">#{{ instance.id }}</span>
                    <span class="text-[var(--border-color)]">·</span>
                    <span class="font-mono">{{ instance.incusId || instance.incus_id || '-' }}</span>
                    <span class="text-[var(--border-color)]">·</span>
                    <span>{{ instance.imageName || instance.image?.replace(/^images:/, '') || '-' }}</span>
                    <template v-if="props.getSummaryMeta(instance).length">
                      <span class="text-[var(--border-color)]">·</span>
                      <span class="font-mono tabular-nums text-[var(--text-primary)]">
                        {{ props.getSummaryMeta(instance).join(' · ') }}
                      </span>
                    </template>
                  </div>

                  <div v-if="instance.username" class="pt-1 flex items-center gap-2.5">
                    <UserAvatar
                      :username="instance.username"
                      :email="instance.userEmail"
                      :avatar-style="instance.userAvatarStyle || ''"
                      :badge-id="instance.userAvatarBadgeId || null"
                      :size="24"
                    />
                    <div class="flex flex-wrap items-center gap-x-2 text-xs min-w-0">
                      <span class="font-medium text-[var(--text-primary)] truncate">{{ instance.username }}</span>
                      <span v-if="instance.userEmail" class="text-[var(--text-tertiary)] truncate">{{ instance.userEmail }}</span>
                    </div>
                  </div>
                </div>
              </div>
            </div>

            <!-- Zone 2: Traffic & Billing Metrics (flat typographic hierarchy, no nested boxes) -->
            <div class="w-full lg:w-[280px] shrink-0 space-y-2.5 pl-9 lg:pl-0">
              <div>
                <div class="flex items-center justify-between text-xs mb-1">
                  <span class="text-[var(--text-tertiary)]">{{ t('admin.hosts.trafficUsage') }}</span>
                  <span class="font-mono tabular-nums text-[var(--text-secondary)]">{{ props.getTrafficSummary(instance) }}</span>
                </div>
                <div class="h-1.5 overflow-hidden rounded-full bg-[var(--bg-tertiary)]">
                  <div
                    class="h-full rounded-full transition-all duration-300"
                    :class="props.getTrafficBarClass(instance)"
                    :style="{ width: `${props.getTrafficPercentage(instance)}%` }"
                  />
                </div>
              </div>

              <div class="grid grid-cols-2 gap-4 text-xs">
                <div class="min-w-0">
                  <div class="text-[var(--text-tertiary)]">{{ t('billing.renewPrice') }}</div>
                  <div class="mt-0.5 font-semibold font-mono tabular-nums text-[var(--text-primary)] truncate">
                    {{ instance.packagePlanId ? props.formatMoney(instance.billingPrice) : '-' }}
                  </div>
                </div>
                <div class="min-w-0">
                  <div class="text-[var(--text-tertiary)]">{{ t('billing.expiresAt') }}</div>
                  <div class="mt-0.5 font-medium text-[var(--text-primary)] truncate">
                    {{ props.getExpirySummary(instance) }}
                  </div>
                </div>
              </div>
            </div>

            <!-- Zone 3: Clean Action Controls -->
            <div class="flex flex-wrap items-center justify-end gap-1.5 shrink-0 pl-9 lg:pl-0" @click.stop>
              <Button
                variant="ghost"
                size="sm"
                :disabled="props.isInstanceSyncing(instance.id)"
                :title="t('admin.hosts.batchSyncStatus')"
                @click="emit('sync-instance', instance)"
              >
                <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': props.isInstanceSyncing(instance.id) }" />
              </Button>
              <Button
                v-if="instance.packagePlanId"
                variant="ghost"
                size="sm"
                :title="t('host.price.editPrice')"
                @click="emit('open-price', instance)"
              >
                <DollarSign class="w-4 h-4" />
              </Button>
              <Button variant="secondary" size="sm" @click="emit('open-single-config', instance.id)">
                {{ t('host.batchConfig.button') }}
              </Button>
              <Button v-if="props.canExport" variant="secondary" size="sm" @click="emit('open-bind-package', instance)">
                {{ t('admin.hosts.bindPackageAction') }}
              </Button>
              <Button v-if="props.canExport" variant="secondary" size="sm" @click="emit('open-export', instance)">
                {{ t('admin.hosts.exportInstance') }}
              </Button>
              <Button variant="primary" size="sm" @click="emit('open-detail', instance.id)">
                {{ t('common.details') }}
              </Button>
              <button
                type="button"
                class="inline-flex h-9 w-9 items-center justify-center rounded-full text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] cursor-pointer"
                :title="props.isInstanceExpanded(instance.id) ? t('common.collapse') : t('common.expand')"
                :aria-expanded="props.isInstanceExpanded(instance.id)"
                @click="emit('toggle-expand', instance)"
              >
                <ChevronDown
                  class="w-4 h-4 transition-transform duration-200"
                  :class="props.isInstanceExpanded(instance.id) ? 'rotate-180' : ''"
                />
              </button>
            </div>
          </div>
        </div>

        <div
          v-if="props.isInstanceExpanded(instance.id)"
          class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-4 sm:px-5"
        >
          <div
            v-if="props.isDetailLoading(instance.id)"
            class="flex items-center gap-3 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-4 py-5 text-[var(--text-secondary)]"
          >
            <RefreshCw class="w-4 h-4 animate-spin text-[var(--accent)]" />
            <span class="text-sm">{{ t('common.loading') }}</span>
          </div>

          <div
            v-else-if="getDetailError(instance.id)"
            class="flex flex-col gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--error)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] px-4 py-4 sm:flex-row sm:items-center sm:justify-between"
          >
            <div class="flex items-start gap-2.5 min-w-0">
              <AlertCircle class="w-4 h-4 mt-0.5 shrink-0 text-[var(--error)]" />
              <div class="min-w-0">
                <div class="text-sm font-medium text-[var(--error)]">{{ t('common.loadFailed') }}</div>
                <div class="mt-1 text-xs text-[var(--error)] opacity-85 break-all">{{ getDetailError(instance.id) }}</div>
              </div>
            </div>
            <Button variant="secondary" size="sm" @click="emit('retry-detail', instance.id)">
              {{ t('common.retry') }}
            </Button>
          </div>

          <div v-else class="grid gap-3 xl:grid-cols-3">
            <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4">
              <div class="mb-4 text-xs font-semibold uppercase tracking-[0.2em] text-[var(--text-tertiary)]">{{ t('common.details') }}</div>
              <dl class="space-y-3 text-sm">
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('instance.detail.info.instanceId') }}</dt><dd class="font-mono tabular-nums text-right text-[var(--text-primary)]">{{ instance.id }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">Incus ID</dt><dd class="font-mono text-right text-[var(--text-primary)] break-all">{{ instance.incusId || instance.incus_id || '-' }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.networkMode') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.getNetworkModeLabel(instance) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.instanceType') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.getInstanceTypeLabel(instance) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('instance.user') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ instance.username || '-' }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('instance.createdAt') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.formatDateTime(instance.createdAt || instance.created_at || null) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('common.updatedAt') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.formatDateTime(instance.updatedAt || instance.updated_at || null) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('billing.expiresAt') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.formatDateTime(props.getExpiryValue(instance)) }}</dd></div>
                <div v-if="props.getSuspendReason(instance)" class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('admin.hosts.suspendReason') }}</dt><dd class="text-right text-[var(--text-primary)] break-all">{{ props.getSuspendReason(instance) }}</dd></div>
              </dl>
            </div>

            <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4">
              <div class="mb-4 text-xs font-semibold uppercase tracking-[0.2em] text-[var(--text-tertiary)]">{{ t('admin.hosts.resources') }}</div>
              <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2.5"><div class="text-[11px] text-[var(--text-tertiary)]">{{ t('admin.hosts.resources') }}</div><div class="mt-1 space-y-1 text-sm font-mono tabular-nums text-[var(--text-primary)]"><div>CPU {{ formatCpu(instance.cpu) }}</div><div>{{ props.formatMemory(instance.memory) }}</div><div>{{ props.formatDisk(instance.disk) }}</div><div>SWAP {{ props.getSwapDisplay(instance) }}</div></div></div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2.5"><div class="text-[11px] text-[var(--text-tertiary)]">{{ t('admin.hosts.trafficUsage') }}</div><div class="mt-1 space-y-1 text-sm font-mono tabular-nums text-[var(--text-primary)]"><div>{{ props.getTrafficSummary(instance) }}</div><div>{{ props.formatBandwidth(props.getDetailConfig(instance.id)?.config.limits_ingress) }}</div><div>{{ props.formatBandwidth(props.getDetailConfig(instance.id)?.config.limits_egress) }}</div></div></div>
                <div v-for="item in props.getQuotaItems(instance)" :key="item.label" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2.5"><div class="text-[11px] text-[var(--text-tertiary)]">{{ item.label }}</div><div class="mt-1 text-sm font-medium font-mono tabular-nums text-[var(--text-primary)]">{{ props.formatQuotaValue(item.value) }}</div></div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3 py-2.5"><div class="text-[11px] text-[var(--text-tertiary)]">IP</div><div class="mt-1 space-y-1 text-sm font-mono text-[var(--text-primary)]"><div>{{ props.getDisplayIp(instance).ipv4 || '-' }}</div><div class="truncate" :title="props.getDisplayIp(instance).ipv6 || ''">{{ props.getDisplayIp(instance).ipv6 || '-' }}</div></div></div>
              </div>
            </div>

            <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4">
              <div class="mb-4 text-xs font-semibold uppercase tracking-[0.2em] text-[var(--text-tertiary)]">{{ t('instanceConfig.title') }}</div>
              <dl class="space-y-3 text-sm">
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">SWAP</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getSwapDisplay(instance) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.ioLimitMode') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.ioLimitMode === 'iops' ? t('packageForm.ioMode.iops') : t('packageForm.ioMode.throughput') }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsRead') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.ioLimitMode === 'iops' ? `${props.getDetailConfig(instance.id)?.config.limits_read_iops ?? '-'} IOPS` : (props.getDetailConfig(instance.id)?.config.limits_read || '-') }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsWrite') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.ioLimitMode === 'iops' ? `${props.getDetailConfig(instance.id)?.config.limits_write_iops ?? '-'} IOPS` : (props.getDetailConfig(instance.id)?.config.limits_write || '-') }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsIngress') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.formatBandwidth(props.getDetailConfig(instance.id)?.config.limits_ingress) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsEgress') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.formatBandwidth(props.getDetailConfig(instance.id)?.config.limits_egress) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsProcesses') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.config.limits_processes ?? '-' }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.limitsCpuPriority') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.config.limits_cpu_priority ?? '-' }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.bootAutostart') }}</dt><dd class="text-right text-[var(--text-primary)]">{{ props.formatBoolean(props.getDetailConfig(instance.id)?.config.boot_autostart) }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.bootAutostartPriority') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.config.boot_autostart_priority ?? '-' }}</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.bootAutostartDelay') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.config.boot_autostart_delay ?? '-' }}s</dd></div>
                <div class="flex items-start justify-between gap-4"><dt class="text-[var(--text-secondary)]">{{ t('packageForm.fields.bootHostShutdownTimeout') }}</dt><dd class="text-right font-mono tabular-nums text-[var(--text-primary)]">{{ props.getDetailConfig(instance.id)?.config.boot_host_shutdown_timeout ?? '-' }}s</dd></div>
              </dl>
            </div>

            <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 xl:col-span-3">
              <div class="mb-4 text-xs font-semibold uppercase tracking-[0.2em] text-[var(--text-tertiary)]">{{ t('common.actions') }}</div>
              <div class="flex flex-wrap gap-2" @click.stop>
                <Button variant="secondary" size="sm" @click="emit('open-single-config', instance.id)">{{ t('host.batchConfig.button') }}</Button>
                <Button variant="secondary" size="sm" :loading="props.isInstanceSyncing(instance.id)" :disabled="props.isInstanceSyncing(instance.id)" @click="emit('sync-instance', instance)">
                  {{ props.isInstanceSyncing(instance.id) ? t('common.syncing') : t('admin.hosts.batchSyncStatus') }}
                </Button>
                <Button
                  v-if="props.getDetailConfig(instance.id)?.swap.available && props.canToggleSwap(instance)"
                  variant="secondary"
                  size="sm"
                  :loading="props.isSwapActionLoading(instance.id)"
                  :disabled="props.isSwapActionLoading(instance.id)"
                  @click="emit('toggle-swap', instance)"
                >
                  {{ props.isSwapActionLoading(instance.id) ? t('common.processing') : (props.getDetailConfig(instance.id)?.swap.enabled ? t('instanceConfig.swap.disableButton') : t('instanceConfig.swap.enableButton')) }}
                </Button>
                <Button
                  v-if="instance.trafficData && Number(instance.trafficData.monthlyUsed) > 0"
                  variant="secondary"
                  size="sm"
                  :disabled="props.resettingTrafficId === instance.id"
                  @click="emit('open-reset-traffic', instance)"
                >
                  {{ t('admin.hosts.resetTraffic') }}
                </Button>
                <Button v-if="instance.packagePlanId" variant="secondary" size="sm" @click="emit('open-price', instance)">
                  {{ t('host.price.editPrice') }}
                </Button>
                <Button v-if="props.canExport" variant="secondary" size="sm" @click="emit('open-bind-package', instance)">
                  {{ t('admin.hosts.bindPackageAction') }}
                </Button>
                <Button
                  v-if="props.canRecreateInstance(instance)"
                  variant="secondary"
                  size="sm"
                  :loading="props.recreateLoading && props.recreateTargetId === instance.id"
                  :disabled="props.recreateLoading"
                  @click="emit('open-recreate', instance)"
                >
                  {{ props.recreateLoading && props.recreateTargetId === instance.id ? t('common.processing') : t('instance.detail.recreate.title') }}
                </Button>
                <Button v-if="props.canExport" variant="secondary" size="sm" @click="emit('open-export', instance)">
                  {{ t('admin.hosts.exportInstance') }}
                </Button>
                <Button variant="danger" size="sm" @click="emit('open-delete', instance.id)">{{ t('common.delete') }}</Button>
                <Button variant="primary" size="sm" @click="emit('open-detail', instance.id)">{{ t('common.details') }}</Button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Card>
</template>
