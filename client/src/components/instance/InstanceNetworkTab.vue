<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import type { Instance } from '@/types/api'

const { t } = useI18n()

interface PortMapping {
  id: number
  protocol: 'tcp' | 'udp'
  publicPort?: number
  privatePort?: number
  public_port?: number
  private_port?: number
  remark?: string | null
}

// 兼容两种命名格式
function getPublicPort(m: PortMapping): number {
  return m.publicPort ?? m.public_port ?? 0
}

function getPrivatePort(m: PortMapping): number {
  return m.privatePort ?? m.private_port ?? 0
}

interface Props {
  instance: Instance
  copied: string
  canManagePorts?: boolean  // AUTH004: 节点所有者不能管理端口映射
  deletePortsLoading?: boolean  // 批量删除加载状态
}

interface Emits {
  (e: 'copy', text: string, key?: string): void
  (e: 'add-port'): void
  (e: 'delete-port', portId: number): void
  (e: 'delete-ports', portIds: number[]): void
}

const props = defineProps<Props>()
const emit = defineEmits<Emits>()
const themeStore = useThemeStore()

function isIpv4Address(value: string | null | undefined): boolean {
  if (!value) return false
  return /^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/.test(value)
}

const publicIpv4Address = computed<string | null>(() => {
  const candidate = (props.instance as any).nat_public_ip ?? props.instance.natPublicIp ?? props.instance.host?.nat_public_ip ?? null
  return isIpv4Address(candidate) ? candidate : null
})

// 端口配额状态（如果未特别指定，放行判断交由后端）
const portLimit = computed<number | null>(() => {
  const limit = (props.instance as any)?.port_limit
  return limit === undefined ? null : limit
})

// 最终显示的公网 IPv6 地址（根据网络模式决定优先级）
const networkMode = computed<string>(() => props.instance.network_mode || (props.instance as any)?.networkMode || '')
const isIpv6OnlyInstance = computed<boolean>(() => networkMode.value === 'ipv6_only')
const hasPortQuota = computed<boolean>(() => portLimit.value !== 0) // 如果设置为0则没有配额，否则认为有配额（哪怕null）
const portQuotaUsed = computed<number>(() => portMappings.value.length)
const portQuotaRemaining = computed<number>(() => {
  if (portLimit.value === null) return 999 // fallback unlimited representation
  return Math.max(0, portLimit.value - portQuotaUsed.value)
})
const isPortQuotaFull = computed<boolean>(() => portLimit.value !== null && portQuotaRemaining.value <= 0)
const canMutateNetwork = computed<boolean>(() => props.canManagePorts !== false)
const canAddPorts = computed<boolean>(() => {
  if (!canMutateNetwork.value) return false
  if (isIpv6OnlyInstance.value) return false
  return true
})

// 协议筛选
const protocolFilter = ref<'both' | 'tcp' | 'udp'>('both')

// 分页
const portPage = ref<number>(1)
const portPageSize = ref<number>(30)
const pageSizeOptions = [10, 20, 30, 50, 100]

// 多选
const selectedPorts = ref<Set<number>>(new Set())
const isSelectAll = computed(() => {
  if (paginatedPorts.value.length === 0) return false
  return paginatedPorts.value.every(m => selectedPorts.value.has(m.id))
})
const isPartialSelect = computed(() => {
  if (paginatedPorts.value.length === 0) return false
  const selectedInPage = paginatedPorts.value.filter(m => selectedPorts.value.has(m.id)).length
  return selectedInPage > 0 && selectedInPage < paginatedPorts.value.length
})
const hasSelection = computed(() => selectedPorts.value.size > 0)

const portMappings = computed<PortMapping[]>(() => {
  return (props.instance as any)?.port_mappings || []
})

// 根据协议筛选后的映射
const filteredPortMappings = computed<PortMapping[]>(() => {
  if (protocolFilter.value === 'both') return portMappings.value
  return portMappings.value.filter(m => m.protocol === protocolFilter.value)
})

const paginatedPorts = computed(() => {
  const start = (portPage.value - 1) * portPageSize.value
  return filteredPortMappings.value.slice(start, start + portPageSize.value)
})

const portTotalPages = computed<number>(() => {
  return Math.ceil(filteredPortMappings.value.length / portPageSize.value) || 1
})

// 筛选或每页数量变化时重置页码和选中
watch([protocolFilter, portPageSize], () => {
  portPage.value = 1
  selectedPorts.value.clear()
})

// 翻页时清空选中（避免删除不可见的端口）
watch(portPage, () => {
  selectedPorts.value.clear()
})

// 切换全选
function toggleSelectAll() {
  if (!canMutateNetwork.value) return
  if (isSelectAll.value) {
    // 取消当前页的选中
    paginatedPorts.value.forEach(m => selectedPorts.value.delete(m.id))
  } else {
    // 选中当前页
    paginatedPorts.value.forEach(m => selectedPorts.value.add(m.id))
  }
}

// 切换单个选中
function toggleSelect(id: number) {
  if (!canMutateNetwork.value) return
  if (selectedPorts.value.has(id)) {
    selectedPorts.value.delete(id)
  } else {
    selectedPorts.value.add(id)
  }
}

// 批量删除
function handleBatchDelete() {
  if (!canMutateNetwork.value) return
  if (selectedPorts.value.size === 0) return
  emit('delete-ports', Array.from(selectedPorts.value))
}

// 删除成功后清空选中
watch(() => props.deletePortsLoading, (loading, prevLoading) => {
  if (prevLoading && !loading) {
    selectedPorts.value.clear()
  }
})

// 自动调整页码（当删除导致当前页超出范围时）
watch(portTotalPages, (newTotal) => {
  if (portPage.value > newTotal) {
    portPage.value = Math.max(1, newTotal)
  }
})

// 当端口映射数据变化时，清理不存在的选中项
watch(portMappings, (newMappings) => {
  const existingIds = new Set(newMappings.map(m => m.id))
  const toRemove: number[] = []
  selectedPorts.value.forEach(id => {
    if (!existingIds.has(id)) {
      toRemove.push(id)
    }
  })
  toRemove.forEach(id => selectedPorts.value.delete(id))
}, { deep: true })

</script>

<template>
  <div class="space-y-4">
    <!-- Port Mappings (NAT modes) -->
    <div v-if="['nat', 'nat_ipv6', 'nat_ipv6_nat', 'ipv6_nat', 'ipv6_only'].includes(instance.network_mode || '')" class="card p-5">
      <!-- 头部：标题 + 配额 / 公网 IP -->
      <div class="flex items-start justify-between gap-3">
        <div class="min-w-0">
          <div class="flex items-center gap-2">
            <h2 class="text-sm font-medium text-themed">
              {{ t('instance.detail.network.portMappings') }}
            </h2>
            <span
              v-if="!isIpv6OnlyInstance && hasPortQuota && portLimit !== null"
              class="font-mono text-xs"
              :class="isPortQuotaFull ? 'text-red-500' : 'text-themed-muted'"
            >
              {{ portQuotaUsed }} / {{ portLimit }}
            </span>
          </div>
          <p
            v-if="publicIpv4Address"
            class="text-xs mt-0.5 text-themed-faint font-mono"
          >
            {{ t('instance.detail.network.publicIp') }}: {{ publicIpv4Address }}
          </p>
        </div>

        <div v-if="!isIpv6OnlyInstance" class="flex items-center gap-3 shrink-0">
          <!-- 协议筛选：扁平分段控件，无外框 -->
          <div class="flex items-center gap-1">
            <template
              v-for="(option, index) in (['both', 'tcp', 'udp'] as const)"
              :key="option"
            >
              <span v-if="index > 0" class="text-themed-faint" aria-hidden="true">/</span>
              <button
                class="text-xs font-medium transition-colors"
                :class="protocolFilter === option
                  ? 'text-themed underline underline-offset-4 decoration-1'
                  : 'text-themed-faint hover:text-themed'"
                @click="protocolFilter = option"
              >
                {{ option === 'both' ? t('instance.detail.network.filterBoth') : option.toUpperCase() }}
              </button>
            </template>
          </div>

          <!-- 添加：唯一实心按钮，无外层包裹 -->
          <button
            v-if="canAddPorts && hasPortQuota && !isPortQuotaFull"
            class="btn btn-secondary btn-sm"
            @click="emit('add-port')"
          >
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
            </svg>
            <span>{{ t('instance.detail.network.add') }}</span>
          </button>
        </div>
      </div>

      <template v-if="isIpv6OnlyInstance">
        <p class="mt-4 text-sm leading-6 text-themed-muted">
          {{ t('instance.detail.network.ipv6OnlyPortMappingHint') }}
        </p>
      </template>

      <template v-else-if="filteredPortMappings.length">
        <!-- 批量操作：无背景操作行，挂在列表头上 -->
        <div
          v-if="hasSelection && canMutateNetwork"
          class="mt-4 flex items-center justify-between gap-3 border-t border-themed pt-3"
        >
          <span class="text-sm text-themed-secondary">
            {{ t('instance.detail.network.selectedCount', { count: selectedPorts.size }) }}
          </span>
          <button
            class="btn btn-danger btn-sm"
            :disabled="props.deletePortsLoading"
            @click="handleBatchDelete"
          >
            <svg v-if="props.deletePortsLoading" class="w-3.5 h-3.5 animate-spin" fill="none" viewBox="0 0 24 24">
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
              <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
            </svg>
            <svg v-else class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
            </svg>
            <span>{{ t('instance.detail.network.batchDelete') }}</span>
          </button>
        </div>

        <!-- 列表：无边框行 + 细分隔线，选中用蓝色左边条 -->
        <div
          v-if="canMutateNetwork && paginatedPorts.length > 0"
          class="mt-4 flex items-center justify-between gap-3 pb-2"
        >
          <div class="flex items-center gap-2">
            <button
              class="w-4 h-4 rounded border flex items-center justify-center transition-colors flex-shrink-0"
              :class="[
                isSelectAll || isPartialSelect
                  ? 'bg-blue-600 border-blue-600'
                  : 'border-themed-secondary hover:border-themed'
              ]"
              @click="toggleSelectAll"
            >
              <svg v-if="isSelectAll" class="w-2.5 h-2.5 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 13l4 4L19 7" />
              </svg>
              <svg v-else-if="isPartialSelect" class="w-2.5 h-2.5 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 12h14" />
              </svg>
            </button>
            <span class="text-xs text-themed-faint">
              {{ t('instance.detail.network.selectAll') }}
            </span>
          </div>
        </div>

        <div class="divide-y divide-themed">
          <div
            v-for="m in paginatedPorts"
            :key="m.id"
            class="group flex items-center justify-between gap-3 py-2.5 text-sm pl-1"
            :class="selectedPorts.has(m.id) ? 'border-l-2 border-blue-500 -ml-0.5 pl-1.5' : ''"
          >
            <div class="flex items-center gap-3 flex-1 min-w-0">
              <!-- 多选框 -->
              <button
                v-if="canMutateNetwork"
                class="w-4 h-4 rounded border flex items-center justify-center transition-colors flex-shrink-0"
                :class="[
                  selectedPorts.has(m.id)
                    ? 'bg-blue-600 border-blue-600'
                    : 'border-themed-secondary hover:border-themed'
                ]"
                @click="toggleSelect(m.id)"
              >
                <svg v-if="selectedPorts.has(m.id)" class="w-2.5 h-2.5 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="3" d="M5 13l4 4L19 7" />
                </svg>
              </button>
              <span
                class="font-mono text-[10px] font-medium tracking-wider flex-shrink-0"
                :class="themeStore.isDark ? 'text-gray-500' : 'text-gray-400'"
              >{{ m.protocol.toUpperCase() }}</span>
              <div class="flex items-center gap-2 min-w-0">
                <code
                  class="font-mono text-sm text-themed"
                >{{ getPublicPort(m) }}</code>
                <button
                  class="flex-shrink-0 transition-colors"
                  :class="copied === 'port-' + m.id ? 'text-green-500' : 'text-themed-faint hover:text-themed'"
                  :title="t('common.copy')"
                  @click="emit('copy', String(getPublicPort(m)), 'port-' + m.id)"
                >
                  <svg v-if="copied !== 'port-' + m.id" class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" />
                  </svg>
                  <svg v-else class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
                  </svg>
                </button>
                <span class="text-themed-faint flex-shrink-0">→</span>
                <span class="font-mono text-sm text-themed-faint">{{ getPrivatePort(m) }}</span>
                <span
                  v-if="m.remark"
                  class="text-xs px-1.5 py-0.5 rounded bg-themed-secondary text-themed-secondary truncate max-w-[120px]"
                  :title="m.remark"
                >{{ m.remark }}</span>
              </div>
            </div>
            <button
              v-if="canMutateNetwork"
              class="ml-2 flex-shrink-0 text-themed-faint hover:text-red-500 transition-colors"
              :title="t('common.delete')"
              @click="emit('delete-port', m.id)"
            >
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
              </svg>
            </button>
          </div>
        </div>

        <!-- Pagination -->
        <div class="flex flex-col gap-3 pt-3 border-t border-themed mt-1 sm:flex-row sm:items-center sm:justify-between">
          <div class="flex items-center gap-2">
            <span class="text-xs text-themed-faint">{{ t('instance.detail.network.perPage') }}</span>
            <select
              v-model.number="portPageSize"
              class="text-xs px-2 py-1 rounded border bg-transparent border-themed-secondary text-themed"
            >
              <option v-for="size in pageSizeOptions" :key="size" :value="size">{{ size }}</option>
            </select>
          </div>
          <div class="flex items-center justify-between gap-2 sm:justify-start">
            <button
              :disabled="portPage === 1"
              class="btn-ghost btn-sm"
              @click="portPage = Math.max(1, portPage - 1)"
            >
              {{ t('instance.detail.network.prevPage') }}
            </button>
            <span class="text-sm text-themed-faint">{{ portPage }} / {{ portTotalPages }}</span>
            <button
              :disabled="portPage === portTotalPages"
              class="btn-ghost btn-sm"
              @click="portPage = Math.min(portTotalPages, portPage + 1)"
            >
              {{ t('instance.detail.network.nextPage') }}
            </button>
          </div>
        </div>
      </template>

      <template v-else>
        <div class="mt-4 rounded border border-dashed border-themed-secondary p-6 text-center">
          <template v-if="!hasPortQuota">
            <p class="text-sm font-medium text-yellow-500">{{ t('instance.detail.network.noPortQuota') }}</p>
            <p class="mt-1 text-xs leading-6 text-themed-muted">{{ t('instance.detail.network.allocateQuotaHint') }}</p>
          </template>
          <template v-else-if="portMappings.length > 0 && filteredPortMappings.length === 0">
            <p class="text-sm font-medium text-themed-muted">{{ t('instance.detail.network.noFilterResults') }}</p>
          </template>
          <template v-else>
            <p class="text-sm font-medium text-themed-muted">{{ t('instance.detail.network.noPortMappings') }}</p>
            <p class="mt-1 text-xs leading-6 text-themed-muted">{{ t('instance.detail.network.addPortMapping') }}</p>
            <button
              v-if="canAddPorts && hasPortQuota && !isPortQuotaFull"
              class="btn btn-secondary btn-sm mt-4"
              @click="emit('add-port')"
            >
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
              </svg>
              {{ t('instance.detail.network.add') }}
            </button>
          </template>
        </div>
      </template>
    </div>
  </div>
</template>
