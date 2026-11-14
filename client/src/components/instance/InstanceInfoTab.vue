<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatCpu, formatMemory, formatDisk, formatDate } from '@/utils/formatters'
import type { Instance, PortMapping } from '@/types/api'
import FlagIcon from '@/components/FlagIcon.vue'

const { t } = useI18n()

// SSH 连接帮助弹窗
const showSshHelpModal = ref(false)

interface ResourceStats {
  cpu: { usagePercent: number }
  memory: { usage: number; limit: number; usagePercent: number }
  disk: { usage: number; limit: number; usagePercent: number }
}

interface TrafficData {
  monthlyUsed: string
  monthlyUsedFormatted: string
  monthlyLimit: string | null
  monthlyLimitFormatted: string | null
  trafficStatus: 'NORMAL' | 'WARNING' | 'LIMITED'
  percentage: number
}

interface Props {
  instance: Instance
  stats: ResourceStats
  statsLoading: boolean
  trafficData?: TrafficData | null
  trafficLoading?: boolean
  showPassword: Record<string, boolean>
  instancePassword?: Record<string, string | null>
  canEditConfig?: boolean
  enableResourcePool?: boolean
  copied?: string
  reassignIpv6Loading?: boolean
  lastIpv6ReassignAt?: string | null
  isInstanceOwner?: boolean
}

interface Emits {
  (e: 'toggle-password', instanceId: number): void
  (e: 'copy', text: string, key?: string): void
  (e: 'edit-config'): void
  (e: 'redeem'): void
  (e: 'reassign-ipv6'): void
}

const props = defineProps<Props>()
const emit = defineEmits<Emits>()

const isRunning = computed<boolean>(() => {
  const s = props.instance?.status?.toLowerCase()
  return s === 'running'
})

// 是否可以编辑配置（只有 running 或 stopped 状态才能编辑，且需要权限）
const canEditConfig = computed<boolean>(() => {
  const s = props.instance?.status?.toLowerCase()
  const statusOk = s === 'running' || s === 'stopped'
  // 只有明确传递 true 时才允许编辑，默认 false
  const hasPermission = props.canEditConfig === true
  return statusOk && hasPermission
})

// 获取实例类型的样式类
function getInstanceTypeClass(type: string | undefined | null): string {
  const normalizedType = String(type || 'container').toLowerCase()
  switch (normalizedType) {
    case 'vm':
      return 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300 border border-gray-200 dark:border-gray-700'
    case 'container':
    default:
      return 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300 border border-gray-200 dark:border-gray-700'
  }
}

// 获取实例类型的标签文本
function getInstanceTypeLabel(type: string | undefined | null): string {
  const normalizedType = String(type || 'container').toLowerCase()
  switch (normalizedType) {
    case 'vm':
      return t('common.instanceType.vm')
    case 'container':
    default:
      return t('common.instanceType.container')
  }
}

// 获取网络模式的样式类
function getNetworkModeClass(mode: string | undefined | null): string {
  const normalizedMode = String(mode || 'nat').toLowerCase()
  const neutralChip = 'bg-gray-100 text-gray-700 dark:bg-gray-800 dark:text-gray-300 border border-gray-200 dark:border-gray-700'
  switch (normalizedMode) {
    case 'nat':
      return neutralChip
    case 'nat_ipv6':
      return neutralChip
    case 'nat_ipv6_nat':
      return neutralChip
    case 'ipv6_only':
      return neutralChip
    case 'ipv6_nat':
      return neutralChip
    default:
      return neutralChip
  }
}

// 获取网络模式的标签文本
function getNetworkModeLabel(mode: string | undefined | null): string {
  const normalizedMode = String(mode || 'nat').toLowerCase()
  // 直接使用国际化 key，所有模式的 key 格式统一为 common.networkMode.<mode>
  const key = `common.networkMode.${normalizedMode}`
  const translated = t(key)
  // 如果翻译 key 不存在（返回 key 本身），回退到 nat 标签
  return translated === key ? t('common.networkMode.nat') : translated
}

// 格式化镜像名称：优先使用 imageName，否则去掉 images: 前缀
function formatImageName(image: string, imageName?: string | null): string {
  if (imageName) return imageName
  return image?.replace(/^images:/, '') || ''
}

// 获取封停原因的显示文本
function getSuspendReasonText(reason: string | null | undefined): string {
  if (!reason) return t('instance.detail.info.suspendReasonDefault')
  if (reason === 'expired') return t('instance.detail.info.suspendReasonExpired')
  return reason
}

// 格式化带宽值，统一换算成 Mbps
function formatBandwidth(bandwidth: string | null | undefined): string {
  const value = String(bandwidth || '').trim()
  if (!value || value === '0') return t('traffic.unlimited')

  const unitMatch = value.match(/^(\d+(?:\.\d+)?)\s*(m|g)?(?:bit|bps)$/i)
  if (unitMatch) {
    const numeric = Number(unitMatch[1])
    if (!Number.isFinite(numeric) || numeric <= 0) return t('traffic.unlimited')
    const multiplier = unitMatch[2]?.toLowerCase() === 'g' ? 1024 : 1
    const mbps = numeric * multiplier
    return `${Number.isInteger(mbps) ? mbps.toFixed(0) : mbps.toFixed(1)} Mbps`
  }

  if (/^\d+$/.test(value)) {
    const bytes = BigInt(value)
    const GB = BigInt(1024 * 1024 * 1024)
    const MB = BigInt(1024 * 1024)

    if (bytes >= GB) {
      const gbps = Number(bytes / GB)
      return `${gbps * 1024} Mbps`
    }
    if (bytes >= MB) {
      return `${Number(bytes / MB)} Mbps`
    }
    return t('traffic.unlimited')
  }

  return value
}

// ===== 网络地址相关 =====
function isIpv4Address(value: string | null | undefined): boolean {
  if (!value) return false
  return /^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/.test(value)
}

function isIpv6Address(value: string | null | undefined): boolean {
  if (!value) return false
  return value.includes(':')
}

const publicIpv4Address = computed<string | null>(() => {
  const candidate = (props.instance as any).nat_public_ip ?? (props.instance as any).natPublicIp ?? (props.instance as any).host?.nat_public_ip ?? null
  return isIpv4Address(candidate) ? candidate : null
})

const hostPublicIpv6 = computed<string | null>(() => {
  const hostIpv6 = (props.instance as any).host_ipv6_address ?? (props.instance as any).hostIpv6Address ?? null
  if (hostIpv6 && isIpv6Address(hostIpv6)) return hostIpv6
  const hostIp = (props.instance as any).host_ip_address ?? (props.instance as any).hostIpAddress ?? (props.instance as any).host?.ip_address ?? null
  if (isIpv6Address(hostIp)) return hostIp
  return null
})

const displayIpv6 = computed<string | null>(() => {
  const mode = props.instance.network_mode
  if (mode === 'nat_ipv6_nat' || mode === 'ipv6_nat') {
    return hostPublicIpv6.value || props.instance.ipv6 || null
  }
  return props.instance.ipv6 || hostPublicIpv6.value || null
})

const networkMode = computed<string>(() => props.instance.network_mode || (props.instance as any)?.networkMode || '')
const isNatMode = computed<boolean>(() => ['nat', 'nat_ipv6', 'nat_ipv6_nat', 'ipv6_nat', 'ipv6_only'].includes(networkMode.value))
const publicSshPort = computed<number | null>(() => {
  const internalPort = props.instance.ssh_port
  if (!internalPort) return null
  if (!['nat', 'nat_ipv6', 'nat_ipv6_nat', 'ipv6_nat'].includes(networkMode.value)) return internalPort
  const mappings = (props.instance as Instance & { port_mappings?: PortMapping[] }).port_mappings || []
  const mapping = mappings.find(item => item.protocol === 'tcp' && (item.privatePort ?? item.private_port) === internalPort)
  return mapping?.publicPort ?? mapping?.public_port ?? null
})

// IPv6 重新分配冷却
const ipv6ReassignCooldown = computed(() => {
  if (!props.lastIpv6ReassignAt) {
    return { inCooldown: false, remainingHours: 0 }
  }
  const cooldownMs = 24 * 60 * 60 * 1000
  const lastReassign = new Date(props.lastIpv6ReassignAt).getTime()
  const timeSinceLastReassign = Date.now() - lastReassign
  if (timeSinceLastReassign < cooldownMs) {
    const remainingHours = Math.ceil((cooldownMs - timeSinceLastReassign) / (60 * 60 * 1000))
    return { inCooldown: true, remainingHours }
  }
  return { inCooldown: false, remainingHours: 0 }
})

const canReassignIpv6 = computed(() => {
  return props.instance.status === 'stopped' && !ipv6ReassignCooldown.value.inCooldown
})
</script>

<template>
  <div class="space-y-4">
    <!-- Top 4 Metric Cards -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <!-- CPU -->
      <div class="card p-4 flex flex-col justify-between">
        <div class="flex items-center justify-between text-xs text-themed-muted">
          <span class="flex items-center gap-1.5 font-medium">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 3v2m6-2v2M9 19v2m6-2v2M5 9H3m2 6H3m18-6h-2m2 6h-2M7 19h10a2 2 0 002-2V7a2 2 0 00-2-2H7a2 2 0 00-2 2v10a2 2 0 002 2zM9 9h6v6H9V9z" />
            </svg>
            {{ t('instance.detail.info.cpu') }}
          </span>
          <span
            class="font-medium tabular-nums"
            :class="stats.cpu.usagePercent > 80 ? 'text-red-500' : stats.cpu.usagePercent > 60 ? 'text-yellow-500' : 'text-green-600 dark:text-green-400'"
          >
            {{ stats.cpu.usagePercent }}%
          </span>
        </div>
        <div class="mt-2 text-2xl font-bold text-themed tabular-nums">
          {{ stats.cpu.usagePercent }}%
        </div>
        <div class="mt-3 h-1 rounded-full overflow-hidden bg-themed/20">
          <div
            class="h-full rounded-full transition-all duration-500"
            :class="stats.cpu.usagePercent > 80 ? 'bg-red-500' : stats.cpu.usagePercent > 60 ? 'bg-yellow-500' : 'bg-green-500'"
            :style="{ width: Math.min(Math.max(stats.cpu.usagePercent, 0), 100) + '%' }"
          ></div>
        </div>
        <div class="mt-2 text-xs text-themed-muted">
          {{ formatCpu(instance.cpu) }}
        </div>
      </div>

      <!-- Memory -->
      <div class="card p-4 flex flex-col justify-between">
        <div class="flex items-center justify-between text-xs text-themed-muted">
          <span class="flex items-center gap-1.5 font-medium">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 7v10c0 2.21 3.582 4 8 4s8-1.79 8-4V7M4 7c0 2.21 3.582 4 8 4s8-1.79 8-4M4 7c0-2.21 3.582-4 8-4s8 1.79 8 4m0 5c0 2.21-3.582 4-8 4s-8-1.79-8-4" />
            </svg>
            {{ t('instance.detail.info.memory') }}
          </span>
          <span
            v-if="stats.memory.usagePercent > 0"
            class="font-medium tabular-nums"
            :class="stats.memory.usagePercent > 80 ? 'text-red-500' : stats.memory.usagePercent > 60 ? 'text-yellow-500' : 'text-green-600 dark:text-green-400'"
          >
            {{ stats.memory.usagePercent }}%
          </span>
        </div>
        <div class="mt-2 text-2xl font-bold text-themed tabular-nums">
          {{ stats.memory.usage > 0 ? formatMemory(stats.memory.usage) : formatMemory(stats.memory.limit > 0 ? stats.memory.limit : instance.memory) }}
        </div>
        <div class="mt-3 h-1 rounded-full overflow-hidden bg-themed/20">
          <div
            class="h-full rounded-full transition-all duration-500"
            :class="stats.memory.usagePercent > 80 ? 'bg-red-500' : stats.memory.usagePercent > 60 ? 'bg-yellow-500' : 'bg-green-500'"
            :style="{ width: Math.min(Math.max(stats.memory.usagePercent, 0), 100) + '%' }"
          ></div>
        </div>
        <div class="mt-2 text-xs text-themed-muted truncate" :title="t('instance.detail.info.includesCache')">
          / {{ formatMemory(stats.memory.limit > 0 ? stats.memory.limit : instance.memory) }} · {{ t('instance.detail.info.includesCache') }}
        </div>
      </div>

      <!-- Disk -->
      <div class="card p-4 flex flex-col justify-between">
        <div class="flex items-center justify-between text-xs text-themed-muted">
          <span class="flex items-center gap-1.5 font-medium">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 7v10c0 2.21 3.582 4 8 4s8-1.79 8-4V7M4 7c0 2.21 3.582 4 8 4s8-1.79 8-4M4 7c0-2.21 3.582-4 8-4s8 1.79 8 4m0 5c0 2.21-3.582 4-8 4s-8-1.79-8-4" />
            </svg>
            {{ t('instance.detail.info.disk') }}
          </span>
          <span
            v-if="stats.disk.usagePercent > 0"
            class="font-medium tabular-nums"
            :class="stats.disk.usagePercent > 80 ? 'text-red-500' : stats.disk.usagePercent > 60 ? 'text-yellow-500' : 'text-purple-500'"
          >
            {{ stats.disk.usagePercent }}%
          </span>
        </div>
        <div class="mt-2 text-2xl font-bold text-themed tabular-nums">
          {{ stats.disk.usage > 0 ? formatDisk(stats.disk.usage) : formatDisk(stats.disk.limit > 0 ? stats.disk.limit : instance.disk) }}
        </div>
        <div class="mt-3 h-1 rounded-full overflow-hidden bg-themed/20">
          <div
            class="h-full rounded-full transition-all duration-500"
            :class="stats.disk.usagePercent > 80 ? 'bg-red-500' : stats.disk.usagePercent > 60 ? 'bg-yellow-500' : 'bg-purple-500'"
            :style="{ width: Math.min(Math.max(stats.disk.usagePercent, 0), 100) + '%' }"
          ></div>
        </div>
        <div class="mt-2 text-xs text-themed-muted">
          / {{ formatDisk(stats.disk.limit > 0 ? stats.disk.limit : instance.disk) }}
        </div>
      </div>

      <!-- Network / Traffic Summary -->
      <div class="card p-4 flex flex-col justify-between">
        <div class="flex items-center justify-between text-xs text-themed-muted gap-2">
          <span class="flex items-center gap-1.5 font-medium truncate">
            <svg class="w-3.5 h-3.5 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 12a9 9 0 01-9 9m9-9a9 9 0 00-9-9m9 9H3m9 9a9 9 0 01-9-9m9 9c1.657 0 3-4.03 3-9s-1.343-9-3-9m0 18c-1.657 0-3-4.03-3-9s1.343-9 3-9" />
            </svg>
            {{ t('instance.detail.tabs.network') }}
          </span>
          <span class="text-themed-muted font-medium truncate">
            {{ getNetworkModeLabel(instance.network_mode) }}
          </span>
        </div>
        <div class="mt-2 text-2xl font-bold text-themed tabular-nums truncate">
          <template v-if="trafficLoading">
            <span class="text-sm font-normal text-themed-secondary">{{ t('common.loading') }}</span>
          </template>
          <template v-else-if="trafficData">
            {{ trafficData.monthlyUsedFormatted }}
          </template>
          <template v-else>
            {{ formatBandwidth(instance.limitsEgress) }}
          </template>
        </div>
        <div class="mt-3 h-1 rounded-full overflow-hidden bg-themed/20">
          <div
            class="h-full rounded-full transition-all duration-500 bg-blue-500"
            :style="{ width: trafficData ? Math.min(Math.max(trafficData.percentage || 0, 0), 100) + '%' : '100%' }"
          ></div>
        </div>
        <div class="mt-2 text-xs text-themed-muted truncate">
          ↑ {{ formatBandwidth(instance.limitsEgress) }} · ↓ {{ formatBandwidth(instance.limitsIngress) }}
        </div>
      </div>
    </div>

    <!-- Bottom 2-Column Info Grid -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <!-- Left Card: Basic & Network Connection Info -->
      <div class="card p-5">
        <h2 class="text-themed font-semibold text-sm mb-4">
          {{ t('instance.detail.info.title') }}
        </h2>
        <dl class="space-y-3 text-sm">
          <!-- NAT Mode Addresses -->
          <template v-if="isNatMode">
            <!-- Private IPv4 -->
            <div class="flex justify-between items-center gap-3">
              <dt class="text-themed-muted shrink-0">{{ t('instance.detail.network.privateIpv4') }}</dt>
              <dd v-if="instance.ipv4" class="flex items-center gap-2 min-w-0">
                <code class="font-mono text-xs px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-700 dark:text-gray-300">{{ instance.ipv4 }}</code>
                <button class="text-themed-muted hover:text-themed transition-colors shrink-0" :title="t('common.copy')" @click="emit('copy', instance.ipv4, 'ipv4')">
                  <svg v-if="copied !== 'ipv4'" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" /></svg>
                  <svg v-else class="w-3.5 h-3.5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" /></svg>
                </button>
              </dd>
              <dd v-else class="text-themed-muted">-</dd>
            </div>
            <!-- Public IPv4 -->
            <div v-if="publicIpv4Address" class="flex justify-between items-center gap-3">
              <dt class="text-themed-muted shrink-0">{{ t('instance.detail.network.publicIpv4') }}</dt>
              <dd class="flex items-center gap-2 min-w-0">
                <code class="font-mono text-xs px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-700 dark:text-gray-300">{{ publicIpv4Address }}</code>
                <button class="text-themed-muted hover:text-themed transition-colors shrink-0" :title="t('common.copy')" @click="emit('copy', publicIpv4Address, 'nat_public_ip')">
                  <svg v-if="copied !== 'nat_public_ip'" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" /></svg>
                  <svg v-else class="w-3.5 h-3.5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" /></svg>
                </button>
              </dd>
            </div>
            <!-- Public IPv6 -->
            <div v-if="['nat_ipv6', 'nat_ipv6_nat', 'ipv6_nat', 'ipv6_only'].includes(networkMode)" class="flex justify-between items-center gap-3">
              <dt class="text-themed-muted shrink-0">{{ t('instance.detail.network.publicIpv6') }}</dt>
              <dd v-if="displayIpv6" class="flex items-center gap-2 min-w-0">
                <button
                  v-if="isInstanceOwner !== false && instance.network_mode === 'nat_ipv6'"
                  class="btn btn-secondary btn-xs flex-shrink-0"
                  :disabled="!canReassignIpv6 || reassignIpv6Loading"
                  :title="ipv6ReassignCooldown.inCooldown
                    ? t('instance.detail.network.reassignIpv6Cooldown', { hours: ipv6ReassignCooldown.remainingHours })
                    : (instance.status !== 'stopped'
                      ? t('instance.detail.network.reassignIpv6StopRequired')
                      : t('instance.detail.network.reassignIpv6'))"
                  @click="emit('reassign-ipv6')"
                >
                  <span v-if="reassignIpv6Loading" class="flex items-center gap-1">
                    <svg class="w-3 h-3 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" /><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" /></svg>
                  </span>
                  <span v-else-if="ipv6ReassignCooldown.inCooldown">{{ t('instance.detail.network.reassignIpv6CooldownShort', { hours: ipv6ReassignCooldown.remainingHours }) }}</span>
                  <span v-else>{{ t('instance.detail.network.reassignIpv6') }}</span>
                </button>
                <code class="font-mono text-xs px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-800/50 text-gray-600 dark:text-gray-400 truncate max-w-[200px] sm:max-w-[260px]" :title="displayIpv6 || undefined">{{ displayIpv6 }}</code>
                <button class="text-themed-muted hover:text-themed transition-colors flex-shrink-0" :title="t('common.copy')" @click="emit('copy', displayIpv6 || '', 'ipv6')">
                  <svg v-if="copied !== 'ipv6'" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" /></svg>
                  <svg v-else class="w-3.5 h-3.5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" /></svg>
                </button>
              </dd>
              <dd v-else class="text-themed-muted">-</dd>
            </div>
          </template>
          <!-- Non-NAT Mode Addresses -->
          <template v-else>
            <div class="flex justify-between items-center gap-3">
              <dt class="text-themed-muted shrink-0">{{ t('instance.detail.network.publicIpv4') }}</dt>
              <dd v-if="instance.ipv4" class="flex items-center gap-2 min-w-0">
                <code class="font-mono text-xs px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-800 text-gray-700 dark:text-gray-300">{{ instance.ipv4 }}</code>
                <button class="text-themed-muted hover:text-themed transition-colors shrink-0" :title="t('common.copy')" @click="emit('copy', instance.ipv4, 'ipv4')">
                  <svg v-if="copied !== 'ipv4'" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" /></svg>
                  <svg v-else class="w-3.5 h-3.5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" /></svg>
                </button>
              </dd>
              <dd v-else class="text-themed-muted">-</dd>
            </div>
            <div v-if="instance.ipv6" class="flex justify-between items-center gap-3">
              <dt class="text-themed-muted shrink-0">{{ t('instance.detail.network.publicIpv6') }}</dt>
              <dd class="flex items-center gap-2 min-w-0">
                <code class="font-mono text-xs px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-800/50 text-gray-600 dark:text-gray-400 truncate max-w-[200px] sm:max-w-[260px]" :title="instance.ipv6">{{ instance.ipv6 }}</code>
                <button class="text-themed-muted hover:text-themed transition-colors flex-shrink-0" :title="t('common.copy')" @click="emit('copy', instance.ipv6, 'ipv6')">
                  <svg v-if="copied !== 'ipv6'" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" /></svg>
                  <svg v-else class="w-3.5 h-3.5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" /></svg>
                </button>
              </dd>
            </div>
          </template>

          <div v-if="instance.ssh_port" class="flex justify-between items-center gap-3 pt-2 border-t border-themed/15">
            <dt class="text-themed-muted">{{ t('instance.detail.info.sshPort') }}</dt>
            <dd class="flex items-center gap-2 text-themed font-medium">
              <span class="font-mono">{{ publicSshPort ?? '-' }}</span>
              <button
                class="text-themed-muted hover:text-themed transition-colors"
                :title="t('instance.detail.info.sshHelpTitle')"
                @click="showSshHelpModal = true"
              >
                <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8.228 9c.549-1.165 2.03-2 3.772-2 2.21 0 4 1.343 4 3 0 1.4-1.278 2.575-3.006 2.907-.542.104-.994.54-.994 1.093m0 3h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                </svg>
              </button>
              <button
                class="text-xs text-themed-muted hover:text-themed transition-colors"
                :disabled="publicSshPort === null"
                @click="emit('copy', publicSshPort?.toString() || '')"
              >
                {{ t('instance.detail.info.copy') }}
              </button>
            </dd>
          </div>

          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted">{{ t('instance.detail.info.rootPassword') }}</dt>
            <dd class="flex items-center gap-2 text-themed font-medium">
              <span
                :id="'pwd-' + instance.id"
                class="font-mono"
              >{{ showPassword[instance.id] && instancePassword?.[instance.id] ? instancePassword[instance.id] : '••••••••' }}</span>
              <button
                class="text-xs text-themed-muted hover:text-themed transition-colors"
                @click="emit('toggle-password', instance.id)"
              >
                {{ showPassword[instance.id] ? t('instance.detail.info.hide') : t('instance.detail.info.show') }}
              </button>
              <button
                v-if="showPassword[instance.id] && instancePassword?.[instance.id]"
                class="text-xs text-themed-muted hover:text-themed transition-colors"
                @click="emit('copy', instancePassword[instance.id] || '')"
              >
                {{ t('instance.detail.info.copy') }}
              </button>
            </dd>
          </div>

          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted">{{ t('instance.detail.info.instanceId') }}</dt>
            <dd class="font-mono text-xs text-themed-muted">
              #{{ instance.id }}
              <template v-if="instance.incus_id">
                <span class="text-themed-secondary mx-1">/</span>
                <span>{{ instance.incus_id }}</span>
              </template>
            </dd>
          </div>

          <!-- 封停信息 -->
          <div v-if="instance.status === 'suspended'" class="p-3 rounded-xl bg-rose-50 border border-rose-200 dark:bg-rose-500/15 dark:border-rose-500/30">
            <div class="flex items-center gap-2 mb-1">
              <svg class="w-4 h-4 text-rose-600 dark:text-rose-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
              </svg>
              <span class="text-sm font-medium text-rose-600 dark:text-rose-400">{{ t('instance.detail.info.suspended') }}</span>
            </div>
            <p class="text-xs text-rose-700 dark:text-rose-300">
              <span class="font-medium">{{ t('instance.detail.info.suspendReasonLabel') }}:</span>
              {{ getSuspendReasonText(instance.suspend_reason) }}
            </p>
            <p v-if="instance.suspended_at" class="text-xs mt-1 text-rose-600/70 dark:text-rose-400/70">
              {{ t('instance.detail.info.suspendedAt') }}: {{ formatDate(instance.suspended_at) }}
            </p>
            <p class="text-xs mt-2 text-rose-600/80 dark:text-rose-400/80">
              {{ t('instance.detail.info.suspendTip') }}
            </p>
          </div>
        </dl>
      </div>

      <!-- Right Card: Allocated Resources & Configuration -->
      <div class="card p-5">
        <div class="flex items-center justify-between mb-4">
          <h2 class="text-themed font-semibold text-sm">
            {{ t('instance.detail.info.resourceUsage') }}
          </h2>
          <div class="flex items-center gap-2">
            <button
              v-if="props.enableResourcePool"
              class="btn btn-secondary btn-sm"
              @click="emit('redeem')"
            >
              <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v13m0-13V6a2 2 0 112 2h-2zm0 0V5.5A2.5 2.5 0 109.5 8H12zm-7 4h14M5 12a2 2 0 110-4h14a2 2 0 110 4M5 12v7a2 2 0 002 2h10a2 2 0 002-2v-7" />
              </svg>
              <span>{{ t('instance.detail.info.redeem') }}</span>
            </button>
            <button
              class="btn btn-secondary btn-sm"
              :disabled="!canEditConfig"
              :title="canEditConfig ? '' : t('instance.detail.info.cannotEditConfig')"
              @click="canEditConfig && emit('edit-config')"
            >
              <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z" />
              </svg>
              <span>{{ t('common.edit') }}</span>
            </button>
            <div
              v-if="isRunning && statsLoading"
              class="w-3.5 h-3.5 border-2 border-sakura-400 border-t-transparent rounded-full animate-spin"
            ></div>
          </div>
        </div>

        <dl class="space-y-3 text-sm">
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.image') }}</dt>
            <dd class="text-themed font-medium text-right truncate" :title="formatImageName(instance.image, (instance as any).imageName)">
              {{ formatImageName(instance.image, (instance as any).imageName) }}
            </dd>
          </div>
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.host') }}</dt>
            <dd class="flex items-center gap-2 text-themed font-medium">
              <FlagIcon v-if="(instance as any).host?.country_code || (instance as any).hostCountryCode" :code="(instance as any).host?.country_code || (instance as any).hostCountryCode || 'us'" size="sm" />
              <span class="uppercase">{{ (instance as any).host?.name || (instance as any).host || '-' }}</span>
            </dd>
          </div>
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.instanceMode') }}</dt>
            <dd class="flex flex-wrap items-center gap-2">
              <span
                :class="[
                  'text-xs px-1.5 py-0.5 rounded-lg',
                  getInstanceTypeClass((instance as any).instance_type)
                ]"
              >
                {{ getInstanceTypeLabel((instance as any).instance_type) }}
              </span>
              <span
                :class="[
                  'text-xs px-1.5 py-0.5 rounded-lg',
                  getNetworkModeClass(instance.network_mode)
                ]"
              >
                {{ getNetworkModeLabel(instance.network_mode) }}
              </span>
            </dd>
          </div>
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.resourceUsage') }}</dt>
            <dd class="text-themed font-medium tabular-nums">
              {{ formatCpu(instance.cpu) }} · {{ formatMemory(instance.memory) }} · {{ formatDisk(instance.disk) }}
            </dd>
          </div>
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.ingressLimit') }} / {{ t('instance.detail.info.egressLimit') }}</dt>
            <dd class="text-themed font-medium tabular-nums">
              ↓ {{ formatBandwidth(instance.limitsIngress) }} / ↑ {{ formatBandwidth(instance.limitsEgress) }}
            </dd>
          </div>
          <div class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.createdAt') }}</dt>
            <dd class="text-xs text-themed-muted tabular-nums">
              {{ formatDate(instance.created_at) }}
            </dd>
          </div>
          <div v-if="instance.expires_at" class="flex justify-between items-center gap-3">
            <dt class="text-themed-muted shrink-0">{{ t('instance.detail.info.expiresAt') }}</dt>
            <dd
              class="text-xs tabular-nums"
              :class="new Date(instance.expires_at) <= new Date() ? 'text-rose-600 dark:text-rose-400' : 'text-themed-muted'"
            >
              {{ formatDate(instance.expires_at) }}
            </dd>
          </div>
        </dl>
      </div>
    </div>

    <!-- SSH 连接帮助弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showSshHelpModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showSshHelpModal = false"></div>
          <div class="modal-content max-w-md">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('instance.detail.info.sshHelpTitle') }}</h3>
              <button class="text-themed-muted hover:text-themed" @click="showSshHelpModal = false">
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <div class="modal-body space-y-4">
              <!-- IPv4 连接说明 -->
              <div class="p-3 rounded-lg bg-themed/30 border border-themed/30">
                <div class="flex items-center gap-2 mb-2">
                  <span class="text-xs font-medium px-2 py-0.5 rounded-lg bg-themed/50 text-themed border border-themed/30">IPv4</span>
                </div>
                <p class="text-sm text-themed">
                  {{ t('instance.detail.info.sshHelpIpv4') }}
                </p>
              </div>
              <!-- IPv6 连接说明（仅在 nat_ipv6 模式下显示） -->
              <div
                v-if="instance.network_mode === 'nat_ipv6'"
                class="p-3 rounded-lg bg-themed/30 border border-themed/30"
              >
                <div class="flex items-center gap-2 mb-2">
                  <span class="text-xs font-medium px-2 py-0.5 rounded-lg bg-themed/50 text-themed border border-themed/30">IPv6</span>
                </div>
                <p class="text-sm text-themed">
                  {{ t('instance.detail.info.sshHelpIpv6') }}
                </p>
              </div>
            </div>
            <div class="modal-footer">
              <button class="btn-primary" @click="showSshHelpModal = false">
                {{ t('common.gotIt') }}
              </button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
