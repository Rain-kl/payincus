<script setup lang="ts">
import { ref, onMounted, onUnmounted, onActivated, onDeactivated, watch, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useAuthStore } from '@/stores/auth'
import { useConfigStore } from '@/stores/config'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import { useToast } from '@/stores/toast'
import { getLocalizedCountryName } from '@/utils/countryDisplay'
import { formatMemory, formatDisk, formatBytes, getStatusInfo } from '@/utils/formatters'
import { translateError } from '@/utils/errorHandler'
import type { Instance, UserBalance } from '@/types/api'
import FlagIcon from '@/components/FlagIcon.vue'
import DistroIcon from '@/components/icons/DistroIcon.vue'
import InstanceDisplayIcon from '@/components/InstanceDisplayIcon.vue'
import { freeSiteCopy, getFreeSiteBillingCycleLabel } from '@/utils/freeSiteFun'
import { instanceCreatePath, instanceDetailPath, isAdminEntry, transfersPath, walletPath } from '@/utils/app-paths'
import { Button, Card, Badge, PageHeader, EmptyState, Input, Select, Pagination } from '@/components/ui'
import {
  Play,
  Square,
  RotateCw,
  MoreHorizontal,
  Search,
  RefreshCw,
  LayoutList,
  LayoutGrid,
  Copy,
  Plus,
  ChevronDown,
  AlertCircle,
  AlertTriangle,
  X,
  Loader2
} from 'lucide-vue-next'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'InstancesView' })

type InstanceLayoutMode = 'list' | 'card'
const INSTANCE_LAYOUT_STORAGE_KEY = 'incudal.instances.layout'

interface CustomerBillingApi {
  billing: {
    setAutoRenewBatch: (instanceIds: number[], autoRenew: boolean) => Promise<{
      message: string
      successCount: number
      skippedCount: number
      failedCount: number
    }>
    previewBatchRenew: (instanceIds: number[]) => Promise<{ items: typeof batchRenewPreview.value }>
    getUserBalance: () => Promise<{ balance: UserBalance }>
    renewInstancesBatch: (instanceIds: number[], months: number) => Promise<{
      message: string
      successCount: number
      skippedCount: number
      failedCount: number
    }>
    getBatchDestroyInfo: (instanceIds: number[]) => Promise<{ items: typeof batchDestroyPreview.value }>
    destroyInstancesBatch: (instanceIds: number[]) => Promise<{
      message: string
      successCount: number
      skippedCount: number
      failedCount: number
    }>
  }
}

const customerBillingApi = api as typeof api & CustomerBillingApi

const route = useRoute()
const router = useRouter()
const { t, locale } = useI18n()
const authStore = useAuthStore()
const configStore = useConfigStore()
const toast = useToast()
void configStore.loadPublicConfig()

const instances = ref<Instance[]>([])
const loading = ref<boolean>(true)
const listError = ref<string>('')
const actionLoading = ref<Record<number, string>>({})
const batchActionLoading = ref<string>('')

// 搜索和分页
const search = ref<string>('')
const page = ref<number>(1)
const pageSize = ref<number>(30)
const total = ref<number>(0)
const totalPages = ref<number>(0)
const pageSizeOptions = [10, 20, 30, 50, 100]
const countryFilter = ref<string>('')
const availableCountries = ref<string[]>([])
const userSelectedLayout = ref<InstanceLayoutMode | null>(null)
const isDesktopViewport = ref<boolean>(true)

// 批量选择
const selectedIds = ref<Set<number>>(new Set())

// 批量续费
const showBatchRenewModal = ref(false)
const batchRenewLoading = ref(false)
const batchRenewSubmitting = ref(false)
const batchRenewMonths = ref<number>(1)
const batchRenewBalance = ref<UserBalance>({ balance: 0, frozen: 0, totalRecharge: 0, totalConsume: 0 })
const batchRenewPreview = ref<Array<{
  id: number
  name: string
  canRenew: boolean
  autoRenew: boolean
  reason?: string
  daysUntilExpire: number | null
  options: Array<{
    months: number
    price: number
    discountedPrice: number
    expiresAt: string
  }>
}>>([])

// 批量销毁
const showBatchDestroyModal = ref(false)
const batchDestroyLoading = ref(false)
const batchDestroySubmitting = ref(false)
const batchDestroyConfirm = ref('')
const batchDestroyPreview = ref<Array<{
  id: number
  name: string
  canDestroy: boolean
  cannotDestroyReason: string
  isFreeInstance: boolean
  isFirstTime: boolean
  feeWaiverEligible: boolean
  refund: {
    remainingDays: number
    remainingValue: number
    feeRate: number
    feeAmount: number
    refundAmount: number
    destroyCount: number
    maxRefundable: number
  }
  instance: {
    id: number
    name: string
    hostName: string
    planName: string | null
  }
}>>([])

const showResetTrafficModal = ref(false)
const resetTrafficTarget = ref<Instance | null>(null)
const resetTrafficSubmitting = ref(false)

// 筛选条件（从 URL 获取）
const filterUserId = ref<number | null>(null)
const filterUserName = ref<string>('')

const isAllSelected = computed(() => instances.value.length > 0 && selectedIds.value.size === instances.value.length)
const isPartialSelected = computed(() => selectedIds.value.size > 0 && selectedIds.value.size < instances.value.length)
const selectedCount = computed(() => selectedIds.value.size)
const selectedInstances = computed(() => instances.value.filter(instance => selectedIds.value.has(instance.id)))
const selectedRunningCount = computed(() => selectedInstances.value.filter(instance => instance.status?.toLowerCase() === 'running').length)
const selectedStoppedCount = computed(() => selectedInstances.value.filter(instance => instance.status?.toLowerCase() === 'stopped').length)
const selectedBillingActionIds = computed(() =>
  selectedInstances.value
    .filter(instance => !!instance.packagePlanId)
    .map(instance => instance.id)
)
const hasSelectedInstances = computed(() => selectedCount.value > 0)
const isViewingAnotherUsersInstances = computed(() => (
  isAdmin.value &&
  filterUserId.value !== null &&
  filterUserId.value !== (authStore.user?.id || null)
))
const canUseCustomerBillingActions = computed<boolean>(() => !isAdminEntry && !isViewingAnotherUsersInstances.value)
const instanceLayoutMode = computed<InstanceLayoutMode>(() => (
  isDesktopViewport.value
    ? (userSelectedLayout.value || 'list')
    : 'card'
))
const isCardLayout = computed<boolean>(() => instanceLayoutMode.value === 'card')

const batchRenewMonthOptions = computed(() => {
  const options = new Set<number>()
  for (const item of batchRenewPreview.value) {
    if (!item.canRenew) continue
    for (const option of item.options) {
      options.add(option.months)
    }
  }
  return Array.from(options).sort((a, b) => a - b)
})

const batchRenewEligibleItems = computed(() => {
  return batchRenewPreview.value
    .map(item => ({
      ...item,
      selectedOption: item.options.find(option => option.months === batchRenewMonths.value) || null
    }))
    .filter(item => item.selectedOption !== null)
})

const batchRenewIneligibleItems = computed(() => {
  return batchRenewPreview.value.filter(item => !item.canRenew || !item.options.some(option => option.months === batchRenewMonths.value))
})

const batchRenewTotal = computed(() => {
  return batchRenewEligibleItems.value.reduce((sum, item) => sum + Number(item.selectedOption?.discountedPrice || 0), 0)
})

const batchRenewInsufficientBalance = computed(() => batchRenewBalance.value.balance < batchRenewTotal.value)
const batchRenewBalanceAfter = computed(() => batchRenewBalance.value.balance - batchRenewTotal.value)
const batchRenewCanSubmit = computed(() => (
  batchRenewEligibleItems.value.length > 0 &&
  batchRenewMonthOptions.value.length > 0 &&
  !batchRenewInsufficientBalance.value &&
  !batchRenewSubmitting.value
))

const batchDestroyEligibleItems = computed(() => batchDestroyPreview.value.filter(item => item.canDestroy))
const batchDestroyIneligibleItems = computed(() => batchDestroyPreview.value.filter(item => !item.canDestroy))
const batchDestroyTotalRefund = computed(() => batchDestroyEligibleItems.value.reduce((sum, item) => sum + item.refund.refundAmount, 0))
const batchDestroyTotalFee = computed(() => batchDestroyEligibleItems.value.reduce((sum, item) => sum + item.refund.feeAmount, 0))
const batchDestroyCanSubmit = computed(() => batchDestroyEligibleItems.value.length > 0 && batchDestroyConfirm.value === 'DESTROY' && !batchDestroySubmitting.value)
let refreshInterval: ReturnType<typeof setInterval> | null = null

// 监听搜索变化（防抖）
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    page.value = 1
    clearSelection()
    loadInstances()
  }, 300)
})

// 监听路由参数变化（管理员切换查看不同用户的实例）
watch(
  () => route.query.userId,
  async (newUserId, oldUserId) => {
    if (newUserId === oldUserId) return

    if (newUserId) {
      filterUserId.value = parseInt(newUserId as string)
      try {
        const res = await api.users.get(filterUserId.value)
        filterUserName.value = (res as { user?: { username?: string } }).user?.username || ''
      } catch {
        filterUserName.value = ''
      }
    } else {
      filterUserId.value = null
      filterUserName.value = ''
    }

    page.value = 1
    loading.value = true
    await loadInstances()
  },
  { immediate: false }
)

const isAdmin = computed<boolean>(() => authStore.user?.role === 'admin')

function updateViewportState(): void {
  if (typeof window === 'undefined') return
  isDesktopViewport.value = window.innerWidth >= 1024
}

function loadInstanceLayoutPreference(): void {
  if (typeof window === 'undefined') return
  try {
    const stored = localStorage.getItem(INSTANCE_LAYOUT_STORAGE_KEY)
    userSelectedLayout.value = stored === 'list' || stored === 'card' ? stored : 'list'
  } catch {
    userSelectedLayout.value = 'list'
  }
}

function setInstanceLayoutMode(mode: InstanceLayoutMode): void {
  userSelectedLayout.value = mode
  if (typeof window === 'undefined') return
  try {
    localStorage.setItem(INSTANCE_LAYOUT_STORAGE_KEY, mode)
  } catch {
    // ignore storage errors
  }
}

function prepareCreatedInstanceRefresh(): boolean {
  if (!route.query.created) return false

  search.value = ''
  countryFilter.value = ''
  page.value = 1
  selectedIds.value = new Set()
  loading.value = true
  return true
}

function clearCreatedInstanceRouteHint(): void {
  if (!route.query.created) return

  const nextQuery = { ...route.query }
  delete nextQuery.created
  void router.replace({ path: route.path, query: nextQuery })
}

onMounted(async (): Promise<void> => {
  updateViewportState()
  loadInstanceLayoutPreference()
  if (typeof window !== 'undefined') {
    window.addEventListener('resize', updateViewportState)
    window.addEventListener('scroll', closeRowMenu, { passive: true })
  }

  if (route.query.userId) {
    filterUserId.value = parseInt(route.query.userId as string)
    try {
      const res = await api.users.get(filterUserId.value)
      filterUserName.value = (res as { user?: { username?: string } }).user?.username || ''
    } catch {
      // ignore
    }
  } else {
    filterUserId.value = null
    filterUserName.value = ''
  }

  const forceReload = prepareCreatedInstanceRefresh()
  await loadInstances(forceReload)
  clearCreatedInstanceRouteHint()
  refreshInterval = setInterval(loadInstances, 15000)
})

onUnmounted(() => {
  if (typeof window !== 'undefined') {
    window.removeEventListener('resize', updateViewportState)
    window.removeEventListener('scroll', closeRowMenu)
  }
  if (refreshInterval) clearInterval(refreshInterval)
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

onDeactivated(() => {
  if (refreshInterval) {
    clearInterval(refreshInterval)
    refreshInterval = null
  }
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

onActivated(async () => {
  updateViewportState()
  if (!refreshInterval) {
    refreshInterval = setInterval(loadInstances, 15000)
  }

  const currentUserId = route.query.userId ? parseInt(route.query.userId as string) : null

  if (currentUserId !== filterUserId.value) {
    if (currentUserId) {
      filterUserId.value = currentUserId
      try {
        const res = await api.users.get(filterUserId.value)
        filterUserName.value = (res as { user?: { username?: string } }).user?.username || ''
      } catch {
        filterUserName.value = ''
      }
    } else {
      filterUserId.value = null
      filterUserName.value = ''
    }
    page.value = 1
  }

  const forceReload = prepareCreatedInstanceRefresh()
  await loadInstances(forceReload)
  clearCreatedInstanceRouteHint()
})

async function loadInstances(force = false): Promise<void> {
  if (
    !force &&
    loading.value === false &&
    (
      Object.keys(actionLoading.value).length > 0 ||
      batchActionLoading.value ||
      batchRenewSubmitting.value ||
      batchDestroySubmitting.value
    )
  ) return

  try {
    listError.value = ''
    const params: { page: number; pageSize: number; search: string; userId?: number } = {
      page: page.value,
      pageSize: pageSize.value,
      search: search.value
    }

    if (filterUserId.value) {
      params.userId = filterUserId.value
    }

    if (countryFilter.value) {
      ;(params as typeof params & { countryCode?: string }).countryCode = countryFilter.value
    }

    const response = await api.instances.list(params)
    const data = response as { instances?: Instance[]; total?: number; totalPages?: number; availableCountries?: string[] }
    const nextInstances = data.instances || []
    instances.value = nextInstances
    total.value = data.total || 0
    totalPages.value = Math.max(1, data.totalPages || 1)
    availableCountries.value = (data.availableCountries || []).map(code => code.toLowerCase())

    if (page.value > totalPages.value) {
      page.value = totalPages.value
      return loadInstances(force)
    }

    const visibleIds = new Set(nextInstances.map(instance => instance.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => visibleIds.has(id)))
  } catch (error) {
    console.error('Failed to load instances:', error)
    listError.value = translateError(error) || t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

function toggleSelectAll(): void {
  if (isAllSelected.value) {
    selectedIds.value = new Set()
  } else {
    selectedIds.value = new Set(instances.value.map(instance => instance.id))
  }
}

function toggleSelect(id: number): void {
  const next = new Set(selectedIds.value)
  if (next.has(id)) {
    next.delete(id)
  } else {
    next.add(id)
  }
  selectedIds.value = next
}

function clearSelection(): void {
  selectedIds.value = new Set()
}

function getCountryLabel(code: string): string {
  return getLocalizedCountryName(code, locale.value, (key, fallback) => t(key, fallback))
}

function handlePaginationChange(payload: { page: number; pageSize: number }): void {
  page.value = payload.page
  pageSize.value = payload.pageSize
  clearSelection()
  void loadInstances()
}

function formatCurrency(value: number | null | undefined): string {
  if (configStore.freeSiteMode) return freeSiteCopy.moneyJustForShow
  return `¥${Number(value || 0).toFixed(2)}`
}

function getBatchRenewMonthsLabel(months: number): string {
  return configStore.freeSiteMode ? getFreeSiteBillingCycleLabel(months) : t('billing.renewMonths', { months })
}

function formatDate(value: string | null | undefined): string {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return formatExpiryDate(date)
}

function clearUserFilter(): void {
  clearSelection()
  page.value = 1
  filterUserId.value = null
  filterUserName.value = ''
  router.replace({ query: {} })
  void loadInstances()
}

function maskIpv4(ip: string | null | undefined): string | null {
  if (!ip) return null
  const parts = ip.split('.')
  if (parts.length === 4) {
    return `${parts[0]}.${parts[1]}.*.*`
  }
  return ip
}

function isIpv4Address(ip: string | null | undefined): boolean {
  if (!ip) return false
  return /^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/.test(ip)
}

function getHostIpv6(instance: Instance): string | null {
  let hostIpv6 = instance.hostNatPublicIpv6 || null
  if (!hostIpv6 && instance.hostIpAddress && instance.hostIpAddress.includes(':')) {
    hostIpv6 = instance.hostIpAddress
  }
  if (!hostIpv6 && instance.hostIpv6Gateway) {
    hostIpv6 = instance.hostIpv6Gateway
  }
  return hostIpv6
}

function getIps(instance: Instance): Array<{ip: string, type: 'ipv4' | 'ipv6'}> {
  const mode = instance.networkMode || instance.network_mode
  const ips: Array<{ip: string, type: 'ipv4' | 'ipv6'}> = []

  const natPublicIp = instance.natPublicIp || instance.host?.nat_public_ip
  const maskedNatIp = isIpv4Address(natPublicIp) ? maskIpv4(natPublicIp) : null
  const maskedPrivateIp = maskIpv4(instance.ipv4)
  const hostIpv6 = getHostIpv6(instance)

  if (mode === 'nat') {
    if (maskedNatIp) ips.push({ ip: maskedNatIp, type: 'ipv4' })
    else if (maskedPrivateIp) ips.push({ ip: maskedPrivateIp, type: 'ipv4' })
  } else if (mode === 'nat_ipv6') {
    if (maskedNatIp) ips.push({ ip: maskedNatIp, type: 'ipv4' })
    else if (maskedPrivateIp) ips.push({ ip: maskedPrivateIp, type: 'ipv4' })
    if (instance.ipv6) ips.push({ ip: instance.ipv6, type: 'ipv6' })
  } else if (mode === 'nat_ipv6_nat') {
    if (maskedNatIp) ips.push({ ip: maskedNatIp, type: 'ipv4' })
    else if (maskedPrivateIp) ips.push({ ip: maskedPrivateIp, type: 'ipv4' })
    if (hostIpv6) ips.push({ ip: hostIpv6, type: 'ipv6' })
  } else if (mode === 'ipv6_nat') {
    if (hostIpv6) ips.push({ ip: hostIpv6, type: 'ipv6' })
  } else if (mode === 'ipv6_only') {
    if (instance.ipv6) ips.push({ ip: instance.ipv6, type: 'ipv6' })
  } else {
    if (maskedPrivateIp) ips.push({ ip: maskedPrivateIp, type: 'ipv4' })
    if (instance.ipv6) ips.push({ ip: instance.ipv6, type: 'ipv6' })
  }

  return ips
}

function getInstanceHostName(instance: Instance): string {
  return (instance as any).host?.name || (instance as any).host || '-'
}

function getInstancePackageName(instance: Instance): string | null {
  return (instance as any).packageName || (instance as any).package?.name || null
}

function getInstancePlanName(instance: Instance): string {
  return getInstancePackageName(instance) || instance.planName || t('instance.freeInstanceLabel')
}

function getInstanceRegionCode(instance: Instance): string {
  return String((instance as any).host?.country_code || (instance as any).hostCountryCode || 'us').toLowerCase()
}

function getInstanceRegionLabel(instance: Instance): string {
  return getCountryLabel(getInstanceRegionCode(instance))
}

function getPrimaryIp(instance: Instance): string {
  return getIps(instance)[0]?.ip || '-'
}

function getPublicIp(instance: Instance): string {
  const natPublicIp = instance.natPublicIp || instance.host?.nat_public_ip
  if (natPublicIp && isIpv4Address(natPublicIp)) return natPublicIp
  if (instance.ipv6) return instance.ipv6
  const hostIpv6 = getHostIpv6(instance)
  if (hostIpv6) return hostIpv6
  return '-'
}

function getPrivateIp(instance: Instance): string {
  return instance.ipv4 || '-'
}

const showActionsDropdown = ref(false)
function toggleActionsDropdown(): void {
  showActionsDropdown.value = !showActionsDropdown.value
}
function closeActionsDropdown(): void {
  showActionsDropdown.value = false
}

const activeRowMenuId = ref<number | null>(null)
const rowMenuPosition = ref<{ top: number; right: number }>({ top: 0, right: 0 })

function toggleRowMenu(id: number, e: Event): void {
  e.stopPropagation()
  if (activeRowMenuId.value === id) {
    closeRowMenu()
    return
  }
  const btn = e.currentTarget as HTMLElement | null
  if (btn) {
    const rect = btn.getBoundingClientRect()
    const spaceBelow = window.innerHeight - rect.bottom
    const menuHeight = 240
    if (spaceBelow < menuHeight && rect.top > menuHeight) {
      rowMenuPosition.value = {
        top: Math.max(8, rect.top - menuHeight - 4),
        right: Math.max(12, window.innerWidth - rect.right)
      }
    } else {
      rowMenuPosition.value = {
        top: rect.bottom + 4,
        right: Math.max(12, window.innerWidth - rect.right)
      }
    }
  }
  activeRowMenuId.value = id
}

function closeRowMenu(): void {
  activeRowMenuId.value = null
}

function getRowMenuInstance(): Instance | null {
  if (activeRowMenuId.value === null) return null
  return instances.value.find(i => i.id === activeRowMenuId.value) || null
}

function formatCpuCores(cpu: number): string {
  const val = cpu / 100
  return Number.isInteger(val) ? String(val) : val.toFixed(1)
}

function getInstanceConfigSummary(instance: Instance): string {
  return `${formatCpuCores(instance.cpu)}${t('instance.mobileCard.cpuCore')} ${formatMemory(instance.memory)} ${formatDisk(instance.disk)}`
}

function getInstanceNetworkMode(instance: Instance): string {
  return String((instance as any).networkMode || instance.network_mode || 'nat')
}

function formatNetworkRate(value: unknown): string | null {
  if (value === null || value === undefined || value === '') return null
  const raw = String(value)
  const unitMatch = raw.trim().match(/^(\d+(?:\.\d+)?)\s*(m|g)?(?:bit|bps)?$/i)
  if (unitMatch) {
    const numeric = Number(unitMatch[1])
    if (!Number.isFinite(numeric) || numeric <= 0) return null
    const mbps = numeric * (unitMatch[2]?.toLowerCase() === 'g' ? 1024 : 1)
    return `${Number.isInteger(mbps) ? mbps.toFixed(0) : mbps.toFixed(1)} Mbps`
  }
  return raw.replace(/Mbit/gi, 'Mbps').replace(/Gbit/gi, 'Gbps')
}

function getInstanceBandwidthSummary(instance: Instance): string {
  const ingress = formatNetworkRate(instance.limitsIngress || (instance as any).limits_ingress)
  const egress = formatNetworkRate(instance.limitsEgress || (instance as any).limits_egress)
  if (ingress && egress && ingress === egress) return ingress
  return [ingress, egress].filter(Boolean).join(' / ') || '-'
}

function getInstanceTrafficLimitLabel(instance: Instance): string {
  const limit = (instance as any).monthlyTrafficLimit
  return limit ? formatBytes(Number(limit)) : t('instance.mobileCard.unlimited')
}

function getInstanceNetworkSummary(instance: Instance): string {
  return `${t('instance.card.bandwidth')} ${getInstanceBandwidthSummary(instance)} ${t('instance.card.traffic')} ${getInstanceTrafficLimitLabel(instance)}${t('instance.card.perMonth')}`
}

function getInstanceTrafficUsage(instance: Instance): string {
  const used = formatBytes(Number((instance as any).monthlyTrafficUsed || 0))
  return `${used} / ${getInstanceTrafficLimitLabel(instance)}`
}

function getInstanceResetTrafficPrice(instance: Instance): string {
  const value = Number((instance as any).trafficResetPrice ?? (instance as any).resetTrafficPrice ?? 0)
  return value > 0 ? formatCurrency(value) : '-'
}

function canResetInstanceTraffic(instance: Instance): boolean {
  if (isAdminEntry) return false
  if (instance.status?.toLowerCase() === 'deleted') return false
  return Number((instance as any).monthlyTrafficUsed || 0) > 0
}

function openResetTrafficModal(instance: Instance): void {
  resetTrafficTarget.value = instance
  showResetTrafficModal.value = true
}

function closeResetTrafficModal(): void {
  if (resetTrafficSubmitting.value) return
  showResetTrafficModal.value = false
  resetTrafficTarget.value = null
}

async function confirmResetTraffic(): Promise<void> {
  const instance = resetTrafficTarget.value
  if (!instance || resetTrafficSubmitting.value) return

  resetTrafficSubmitting.value = true
  actionLoading.value[instance.id] = 'resetTraffic'
  try {
    await api.traffic.resetInstanceTraffic(instance.id)
    toast.success(t('admin.hosts.trafficResetSuccess'))
    ;(instance as any).monthlyTrafficUsed = '0'
    ;(instance as any).trafficStatus = 'NORMAL'
    showResetTrafficModal.value = false
    resetTrafficTarget.value = null
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('admin.hosts.trafficResetFailed') + ': ' + translateError(error))
  } finally {
    resetTrafficSubmitting.value = false
    delete actionLoading.value[instance.id]
  }
}

function getInstanceTrafficPercent(instance: Instance): number | null {
  const limit = Number((instance as any).monthlyTrafficLimit || 0)
  if (limit <= 0) return null
  const used = Number((instance as any).monthlyTrafficUsed || 0)
  return Math.min(100, Math.max(0, Math.round((used / limit) * 100)))
}

function getInstanceTrafficBarColor(instance: Instance): string {
  const percent = getInstanceTrafficPercent(instance)
  if (percent === null) {
    return 'bg-[var(--accent)]'
  }
  if (percent >= 95) return 'bg-[var(--error)]'
  if (percent >= 80) return 'bg-[var(--warning)]'
  return 'bg-[var(--accent)]'
}

async function copyIp(ip: string): Promise<void> {
  if (!ip || ip === '-') return
  try {
    if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(ip)
      toast.success(t('common.copied'))
    }
  } catch {
    // ignore
  }
}

function getInstanceMonthlyPrice(instance: Instance): string {
  const price = Number(instance.billingPrice ?? instance.planPrice ?? 0)
  if (price <= 0) return '-'
  return `${formatCurrency(price)}${t('instance.card.perMonth')}`
}

function canUseInstanceBillingAction(instance: Instance): boolean {
  return canUseCustomerBillingActions.value && !!instance.packagePlanId
}

function canStartInstance(instance: Instance): boolean {
  return instance.status?.toLowerCase() === 'stopped'
}

function canDeleteInstance(instance: Instance): boolean {
  return !instance.packagePlanId && (instance as any).allow_instance_deletion !== false
}

function getStatusBadgeVariant(status?: string): 'default' | 'success' | 'warning' | 'error' | 'info' {
  switch (status?.toLowerCase()) {
    case 'running':
      return 'success'
    case 'stopped':
      return 'default'
    case 'suspended':
    case 'error':
      return 'error'
    case 'creating':
    case 'starting':
    case 'stopping':
    case 'restarting':
      return 'warning'
    default:
      return 'default'
  }
}

function isStatusTransient(status?: string): boolean {
  const s = status?.toLowerCase()
  return s === 'creating' || s === 'starting' || s === 'stopping' || s === 'restarting'
}

function getDistroFromName(name: string): string {
  const lowerName = (name || '').toLowerCase()
  const distros = ['almalinux', 'alma', 'alpine', 'arch', 'centos', 'debian', 'fedora', 'gentoo', 'kali', 'mint', 'opensuse', 'suse', 'oracle', 'redhat', 'rhel', 'rockylinux', 'rocky', 'ubuntu', 'void']
  for (const distro of distros) {
    if (lowerName.includes(distro)) return distro
  }
  return 'linux'
}

function getPaidIconType(instance: Instance): 'pro' | 'prime' | null {
  if (!instance.packagePlanId) return null
  return instance.instanceType === 'vm' ? 'prime' : 'pro'
}

function formatImageName(image: string, imageName?: string | null): string {
  if (imageName) return imageName
  return image?.replace(/^images:/, '') || ''
}

interface InstanceExpiryInfo {
  dateText: string | null
  remainingText: string
  className: string
  title: string | null
}

function formatExpiryDate(date: Date): string {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

function getInstanceExpiryInfo(instance: Instance): InstanceExpiryInfo {
  const expiresAt = instance.expires_at || (instance as any).expiresAt || null
  if (!expiresAt) {
    return {
      dateText: null,
      remainingText: t('instance.freeInstanceLabel'),
      className: 'text-[var(--success)]',
      title: null
    }
  }

  const expires = new Date(expiresAt)
  if (Number.isNaN(expires.getTime())) {
    return {
      dateText: null,
      remainingText: t('instance.freeInstanceLabel'),
      className: 'text-[var(--success)]',
      title: null
    }
  }

  const now = new Date()
  const diff = expires.getTime() - now.getTime()
  const remainingDays = Math.ceil(diff / (1000 * 60 * 60 * 24))
  const dateText = formatExpiryDate(expires)
  const autoRenew = (instance as any).autoRenew === true

  if (autoRenew) {
    return {
      dateText,
      remainingText: t('billing.autoRenewing'),
      className: 'text-[var(--success)]',
      title: expires.toLocaleString()
    }
  }

  if (remainingDays <= 0) {
    return {
      dateText,
      remainingText: t('instance.expiredLabel'),
      className: 'text-[var(--error)]',
      title: expires.toLocaleString()
    }
  }

  if (remainingDays <= 3) {
    return {
      dateText,
      remainingText: `${remainingDays} ${t('common.days')}`,
      className: 'text-[var(--error)]',
      title: expires.toLocaleString()
    }
  }

  if (remainingDays <= 7) {
    return {
      dateText,
      remainingText: `${remainingDays} ${t('common.days')}`,
      className: 'text-[var(--warning)]',
      title: expires.toLocaleString()
    }
  }

  return {
    dateText,
    remainingText: `${remainingDays} ${t('common.days')}`,
    className: 'text-[var(--success)]',
    title: expires.toLocaleString()
  }
}

type InstanceAction = 'start' | 'stop' | 'restart' | 'delete'
type BatchSimpleAction = 'start' | 'stop' | 'restart' | 'sync'
const SIMPLE_BATCH_CONCURRENCY = 5

async function handleAction(instance: Instance, action: InstanceAction): Promise<void> {
  actionLoading.value[instance.id] = action

  try {
    if (action === 'start') {
      await api.instances.start(instance.id)
      toast.success(t('instance.startingInstance', { name: instance.name }))
      const idx = instances.value.findIndex(i => i.id === instance.id)
      if (idx !== -1) {
        (instances.value[idx] as any).status = 'starting'
      }
    }
    else if (action === 'stop') {
      await api.instances.stop(instance.id)
      toast.success(t('instance.stoppedInstance', { name: instance.name }))
      const idx = instances.value.findIndex(i => i.id === instance.id)
      if (idx !== -1) {
        instances.value[idx] = { ...instances.value[idx], status: 'stopped' }
      }
    }
    else if (action === 'restart') {
      await api.instances.restart(instance.id)
      toast.success(t('instance.restartingInstance', { name: instance.name }))
      const idx = instances.value.findIndex(i => i.id === instance.id)
      if (idx !== -1) {
        (instances.value[idx] as any).status = 'restarting'
      }
    }
    else if (action === 'delete') {
      if (!confirm(t('instance.confirmDelete', { name: instance.name }))) {
        delete actionLoading.value[instance.id]
        return
      }
      await api.instances.delete(instance.id)
      instances.value = instances.value.filter(i => i.id !== instance.id)
      total.value = Math.max(0, total.value - 1)
      toast.success(t('instance.deletedInstance', { name: instance.name }))
    }
  } catch (error: any) {
    if (error?.code === 'VERIFICATION_REQUIRED' || error?.message?.includes('Sensitive operation requires verification')) {
      toast.error(t('instance.verificationRequiredHint'), 6000)
    } else {
      toast.error(`${t('instance.actionFailed')}: ${translateError(error)}`)
    }
    await loadInstances(true)
  } finally {
    delete actionLoading.value[instance.id]
  }
}

function openInstanceDetail(instanceId: number): void {
  router.push(instanceDetailPath(instanceId))
}

function getBatchTargets(action: BatchSimpleAction): Instance[] {
  if (action === 'start') {
    return selectedInstances.value.filter(instance => instance.status?.toLowerCase() === 'stopped')
  }
  if (action === 'stop' || action === 'restart') {
    return selectedInstances.value.filter(instance => instance.status?.toLowerCase() === 'running')
  }
  return selectedInstances.value
}

async function runWithConcurrency<T, R>(
  items: T[],
  concurrency: number,
  handler: (item: T) => Promise<R>
): Promise<PromiseSettledResult<R>[]> {
  const results: PromiseSettledResult<R>[] = new Array(items.length)
  let currentIndex = 0

  async function worker(): Promise<void> {
    while (true) {
      const index = currentIndex
      currentIndex += 1
      if (index >= items.length) break
      try {
        const value = await handler(items[index])
        results[index] = { status: 'fulfilled', value }
      } catch (error) {
        results[index] = { status: 'rejected', reason: error }
      }
    }
  }

  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, () => worker()))
  return results
}

function translateBatchReason(reason?: string): string {
  if (!reason) return t('instance.batch.unknownReason')
  if (reason === '实例不存在' || reason === '实例不存在或无权操作' || reason === '无权操作该实例') {
    return t('instance.batchReason.notFoundOrForbidden')
  }
  if (reason === '免费实例无需续费') {
    return t('instance.batchReason.freeNoRenew')
  }
  if (reason === '无法获取实例计费信息') {
    return t('instance.batchReason.billingUnavailable')
  }
  if (reason === '暂无可用续费选项') {
    return t('instance.batchReason.noRenewOptions')
  }
  if (reason === '该实例不支持当前续费时长') {
    return t('instance.batch.unsupportedPeriod')
  }
  if (reason === '免费实例不支持自动续费') {
    return t('instance.batchReason.freeNoAutoRenew')
  }
  if (reason === '已经开启自动续费') {
    return t('instance.batchReason.autoRenewAlreadyOn')
  }
  if (reason === '已经关闭自动续费') {
    return t('instance.batchReason.autoRenewAlreadyOff')
  }
  if (reason === '实例已删除') {
    return t('instance.batchReason.deleted')
  }
  if (reason === '实例正在创建中，无法销毁') {
    return t('instance.batchReason.creating')
  }
  if (reason === '实例已被封停，无法销毁，请先联系管理员解封') {
    return t('instance.batchReason.suspended')
  }
  if (reason === '当前月流量周期无法销毁，已用流量达到或超过 5G') {
    return t('instance.batchReason.destroyTrafficLimit')
  }
  if (reason === '续费失败') {
    return t('instance.batchReason.renewFailed')
  }
  if (reason === '自动续费设置失败') {
    return t('instance.batchReason.autoRenewFailed')
  }
  if (reason === '销毁实例失败') {
    return t('instance.batchReason.destroyFailed')
  }

  const renewWindowMatch = reason.match(/^仅可在到期前\s*(\d+)\s*天内续费$/)
  if (renewWindowMatch) {
    return t('instance.batchReason.renewWindow', { days: renewWindowMatch[1] })
  }

  return reason
}

async function handleBatchSimpleAction(action: BatchSimpleAction): Promise<void> {
  const targets = getBatchTargets(action)
  if (targets.length === 0) {
    toast.warning(t('instance.batch.noEligibleAction'))
    return
  }

  batchActionLoading.value = action
  try {
    const settled = await runWithConcurrency(
      targets,
      SIMPLE_BATCH_CONCURRENCY,
      async (instance) => {
        if (action === 'start') return api.instances.start(instance.id)
        if (action === 'stop') return api.instances.stop(instance.id)
        if (action === 'restart') return api.instances.restart(instance.id)
        return api.instances.syncStatus(instance.id)
      }
    )

    const successCount = settled.filter(item => item.status === 'fulfilled').length
    const failedCount = settled.length - successCount
    const skippedCount = selectedCount.value - targets.length

    if (failedCount > 0) {
      toast.warning(t('instance.batch.partialResult', { success: successCount, failed: failedCount, skipped: skippedCount }))
    } else {
      toast.success(t('instance.batch.successResult', { count: successCount }))
    }

    clearSelection()
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('instance.batch.actionFailed') + ': ' + translateError(error))
  } finally {
    batchActionLoading.value = ''
  }
}

async function handleBatchAutoRenew(autoRenew: boolean): Promise<void> {
  const instanceIds = selectedBillingActionIds.value
  if (instanceIds.length === 0 || !canUseCustomerBillingActions.value) return

  batchActionLoading.value = autoRenew ? 'autoRenewOn' : 'autoRenewOff'
  try {
    const result = await customerBillingApi.billing.setAutoRenewBatch(instanceIds, autoRenew)
    if (result.failedCount > 0) {
      toast.warning(t('instance.batch.partialResult', {
        success: result.successCount,
        failed: result.failedCount,
        skipped: result.skippedCount
      }))
    } else {
      toast.success(t(autoRenew ? 'billing.autoRenewEnabled' : 'billing.autoRenewDisabled'))
    }
    clearSelection()
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('instance.batch.actionFailed') + ': ' + translateError(error))
  } finally {
    batchActionLoading.value = ''
  }
}

async function handleSingleAutoRenew(instance: Instance, autoRenew: boolean): Promise<void> {
  if (!canUseInstanceBillingAction(instance) || !!actionLoading.value[instance.id]) return

  actionLoading.value = {
    ...actionLoading.value,
    [instance.id]: autoRenew ? 'autoRenewOn' : 'autoRenewOff'
  }

  try {
    const result = await customerBillingApi.billing.setAutoRenewBatch([instance.id], autoRenew)
    if (result.failedCount > 0) {
      toast.warning(t('instance.batch.partialResult', {
        success: result.successCount,
        failed: result.failedCount,
        skipped: result.skippedCount
      }))
    } else {
      toast.success(t(autoRenew ? 'billing.autoRenewEnabled' : 'billing.autoRenewDisabled'))
    }
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('instance.batch.actionFailed') + ': ' + translateError(error))
  } finally {
    const next = { ...actionLoading.value }
    delete next[instance.id]
    actionLoading.value = next
  }
}

async function openSingleRenewModal(instance: Instance): Promise<void> {
  if (!canUseInstanceBillingAction(instance)) return
  selectedIds.value = new Set([instance.id])
  await openBatchRenewModal()
}

function openInstanceTransfer(instance: Instance): void {
  void router.push({ path: transfersPath(), query: { instanceId: String(instance.id) } })
}

async function openBatchRenewModal(): Promise<void> {
  const instanceIds = selectedBillingActionIds.value
  if (instanceIds.length === 0 || !canUseCustomerBillingActions.value) return

  showBatchRenewModal.value = true
  batchRenewLoading.value = true
  batchRenewPreview.value = []
  batchRenewMonths.value = 1

  try {
    const [previewRes, balanceRes] = await Promise.all([
      customerBillingApi.billing.previewBatchRenew(instanceIds),
      customerBillingApi.billing.getUserBalance()
    ])
    batchRenewPreview.value = previewRes.items
    batchRenewBalance.value = balanceRes.balance
    batchRenewMonths.value = batchRenewMonthOptions.value.includes(1)
      ? 1
      : (batchRenewMonthOptions.value[0] || 1)
  } catch (error: any) {
    toast.error(t('instance.batch.previewFailed') + ': ' + translateError(error))
    showBatchRenewModal.value = false
  } finally {
    batchRenewLoading.value = false
  }
}

function closeBatchRenewModal(force = false): void {
  if (batchRenewSubmitting.value && !force) return
  showBatchRenewModal.value = false
  batchRenewPreview.value = []
  batchRenewMonths.value = 1
}

async function confirmBatchRenew(): Promise<void> {
  if (!batchRenewCanSubmit.value || !canUseCustomerBillingActions.value) return
  const instanceIds = selectedBillingActionIds.value
  if (instanceIds.length === 0) return

  batchRenewSubmitting.value = true
  try {
    const result = await customerBillingApi.billing.renewInstancesBatch(instanceIds, batchRenewMonths.value)
    if (result.failedCount > 0) {
      toast.warning(t('instance.batch.partialResult', {
        success: result.successCount,
        failed: result.failedCount,
        skipped: result.skippedCount
      }))
    } else {
      toast.success(t('instance.batch.successResult', { count: result.successCount }))
    }
    closeBatchRenewModal(true)
    clearSelection()
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('instance.batch.actionFailed') + ': ' + translateError(error))
  } finally {
    batchRenewSubmitting.value = false
  }
}

async function openBatchDestroyModal(): Promise<void> {
  if (selectedCount.value === 0 || !canUseCustomerBillingActions.value) return

  showBatchDestroyModal.value = true
  batchDestroyLoading.value = true
  batchDestroyConfirm.value = ''
  batchDestroyPreview.value = []

  try {
    const result = await customerBillingApi.billing.getBatchDestroyInfo(Array.from(selectedIds.value))
    batchDestroyPreview.value = result.items
  } catch (error: any) {
    toast.error(t('instance.batch.previewFailed') + ': ' + translateError(error))
    showBatchDestroyModal.value = false
  } finally {
    batchDestroyLoading.value = false
  }
}

function closeBatchDestroyModal(force = false): void {
  if (batchDestroySubmitting.value && !force) return
  showBatchDestroyModal.value = false
  batchDestroyConfirm.value = ''
  batchDestroyPreview.value = []
}

async function confirmBatchDestroy(): Promise<void> {
  if (!batchDestroyCanSubmit.value || !canUseCustomerBillingActions.value) return

  batchDestroySubmitting.value = true
  try {
    const result = await customerBillingApi.billing.destroyInstancesBatch(Array.from(selectedIds.value))
    if (result.failedCount > 0) {
      toast.warning(t('instance.batch.partialResult', {
        success: result.successCount,
        failed: result.failedCount,
        skipped: result.skippedCount
      }))
    } else {
      toast.success(t('instance.batch.successResult', { count: result.successCount }))
    }
    closeBatchDestroyModal(true)
    clearSelection()
    await loadInstances(true)
  } catch (error: any) {
    toast.error(t('instance.batch.actionFailed') + ': ' + translateError(error))
  } finally {
    batchDestroySubmitting.value = false
  }
}
</script>

<template>
  <div class="space-y-6 animate-fade-in pb-12">
    <!-- Apple HIG PageHeader -->
    <PageHeader
      :title="$t('instance.title')"
      :description="filterUserId && filterUserName ? undefined : $t('instance.manageDesc')"
    >
      <template v-if="filterUserId && filterUserName" #description>
        <div class="flex items-center gap-2 mt-1">
          <span class="text-sm text-[var(--text-secondary)]">
            {{ $t('instance.userInstances', { name: filterUserName }) }}
          </span>
          <Button
            variant="ghost"
            size="sm"
            class="h-6 px-2 text-xs text-[var(--accent)] hover:underline"
            @click="clearUserFilter"
          >
            {{ $t('instance.clearFilter') }}
          </Button>
        </div>
      </template>

      <template #actions>
        <!-- Create Instance Button -->
        <RouterLink :to="instanceCreatePath()">
          <Button variant="primary" size="md">
            <template #icon>
              <Plus class="w-4 h-4" />
            </template>
            {{ configStore.freeSiteMode ? freeSiteCopy.instanceCreate : $t('instance.create') }}
          </Button>
        </RouterLink>

        <!-- Refresh Button -->
        <Button
          variant="secondary"
          size="md"
          :loading="loading"
          @click="loadInstances(true)"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
          </template>
          {{ $t('common.refresh') }}
        </Button>

        <!-- Layout Switcher (Desktop) -->
        <div class="hidden sm:inline-flex items-center rounded-full border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-0.5 shadow-sm">
          <button
            type="button"
            class="inline-flex items-center justify-center w-8 h-8 rounded-full transition-all duration-200 active:scale-95"
            :class="instanceLayoutMode === 'list'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
              : 'text-[var(--text-tertiary)] hover:text-[var(--text-primary)]'"
            :title="$t('instance.listLayout')"
            @click="setInstanceLayoutMode('list')"
          >
            <LayoutList class="w-4 h-4" />
          </button>
          <button
            type="button"
            class="inline-flex items-center justify-center w-8 h-8 rounded-full transition-all duration-200 active:scale-95"
            :class="instanceLayoutMode === 'card'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
              : 'text-[var(--text-tertiary)] hover:text-[var(--text-primary)]'"
            :title="$t('instance.cardLayout')"
            @click="setInstanceLayoutMode('card')"
          >
            <LayoutGrid class="w-4 h-4" />
          </button>
        </div>
      </template>
    </PageHeader>

    <!-- Toolbar: Search & Filters & Batch Dropdown -->
    <Card padding="sm">
      <div class="flex flex-col md:flex-row md:items-center md:justify-between gap-3">
        <!-- Search & Country Filters -->
        <div class="flex flex-1 flex-wrap items-center gap-2.5">
          <div class="w-full sm:w-64">
            <Input
              v-model="search"
              size="sm"
              clearable
              :placeholder="$t('common.search')"
            >
              <template #prefix>
                <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>

          <div v-if="availableCountries.length > 0" class="w-36">
            <Select
              v-model="countryFilter"
              size="sm"
              :options="[
                { label: $t('common.all'), value: '' },
                ...availableCountries.map(code => ({
                  label: getCountryLabel(code),
                  value: code
                }))
              ]"
              @change="page = 1; clearSelection(); loadInstances()"
            />
          </div>

          <div class="text-xs text-[var(--text-tertiary)] tabular-nums hidden lg:inline">
            {{ $t('instance.totalCount', { count: total }) }}
          </div>
        </div>

        <!-- Actions Dropdown -->
        <div class="flex items-center gap-2 shrink-0">
          <div class="relative">
            <Button
              variant="secondary"
              size="sm"
              @click="toggleActionsDropdown"
            >
              <span>{{ $t('instance.actionsDropdown') }}</span>
              <Badge
                v-if="hasSelectedInstances"
                variant="info"
                size="sm"
                class="ml-1"
              >
                {{ selectedCount }}
              </Badge>
              <ChevronDown class="w-3.5 h-3.5 ml-1 transition-transform" :class="{ 'rotate-180': showActionsDropdown }" />
            </Button>

            <div v-if="showActionsDropdown" class="fixed inset-0 z-30" @click="closeActionsDropdown"></div>
            <div
              v-if="showActionsDropdown"
              class="absolute right-0 mt-2 w-52 rounded-2xl bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] border border-[var(--border-color)] p-1.5 z-40 text-xs backdrop-blur-md"
            >
              <button
                type="button"
                class="w-full flex items-center justify-between px-3 py-2 rounded-xl text-left hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                :disabled="selectedStoppedCount === 0 || !!batchActionLoading"
                @click="closeActionsDropdown(); handleBatchSimpleAction('start')"
              >
                <span class="flex items-center gap-2">
                  <Play class="w-3.5 h-3.5 text-[var(--success)] fill-current" />
                  <span>{{ $t('instance.batch.start') }}</span>
                </span>
                <span class="text-[var(--text-tertiary)] font-mono">({{ selectedStoppedCount }})</span>
              </button>
              <button
                type="button"
                class="w-full flex items-center justify-between px-3 py-2 rounded-xl text-left hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                :disabled="selectedRunningCount === 0 || !!batchActionLoading"
                @click="closeActionsDropdown(); handleBatchSimpleAction('stop')"
              >
                <span class="flex items-center gap-2">
                  <Square class="w-3.5 h-3.5 text-[var(--warning)] fill-current" />
                  <span>{{ $t('instance.batch.stop') }}</span>
                </span>
                <span class="text-[var(--text-tertiary)] font-mono">({{ selectedRunningCount }})</span>
              </button>
              <button
                type="button"
                class="w-full flex items-center justify-between px-3 py-2 rounded-xl text-left hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                :disabled="selectedRunningCount === 0 || !!batchActionLoading"
                @click="closeActionsDropdown(); handleBatchSimpleAction('restart')"
              >
                <span class="flex items-center gap-2">
                  <RotateCw class="w-3.5 h-3.5 text-[var(--accent)]" />
                  <span>{{ $t('instance.batch.restart') }}</span>
                </span>
                <span class="text-[var(--text-tertiary)] font-mono">({{ selectedRunningCount }})</span>
              </button>
              <button
                type="button"
                class="w-full flex items-center gap-2 px-3 py-2 rounded-xl text-left hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                :disabled="!hasSelectedInstances || !!batchActionLoading"
                @click="closeActionsDropdown(); handleBatchSimpleAction('sync')"
              >
                <RefreshCw class="w-3.5 h-3.5 text-[var(--text-secondary)]" />
                <span>{{ $t('instance.batch.sync') }}</span>
              </button>
              <template v-if="!isAdminEntry">
                <div class="border-t border-[var(--border-color)] my-1"></div>
                <button
                  type="button"
                  class="w-full text-left px-3 py-2 rounded-xl hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                  :disabled="selectedBillingActionIds.length === 0 || isViewingAnotherUsersInstances || !!batchActionLoading"
                  @click="closeActionsDropdown(); handleBatchAutoRenew(true)"
                >
                  {{ $t('instance.batch.autoRenewOn') }}
                </button>
                <button
                  type="button"
                  class="w-full text-left px-3 py-2 rounded-xl hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                  :disabled="selectedBillingActionIds.length === 0 || isViewingAnotherUsersInstances || !!batchActionLoading"
                  @click="closeActionsDropdown(); handleBatchAutoRenew(false)"
                >
                  {{ $t('instance.batch.autoRenewOff') }}
                </button>
                <button
                  type="button"
                  class="w-full text-left px-3 py-2 rounded-xl hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--text-primary)]"
                  :disabled="selectedBillingActionIds.length === 0 || isViewingAnotherUsersInstances || !!batchActionLoading"
                  @click="closeActionsDropdown(); openBatchRenewModal()"
                >
                  {{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchRenewAction : $t('instance.batch.renew') }}
                </button>
                <button
                  type="button"
                  class="w-full text-left px-3 py-2 rounded-xl hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] transition-colors disabled:opacity-40 disabled:cursor-not-allowed text-[var(--error)]"
                  :disabled="!hasSelectedInstances || isViewingAnotherUsersInstances || !!batchActionLoading"
                  @click="closeActionsDropdown(); openBatchDestroyModal()"
                >
                  {{ $t('instance.batch.destroy') }}
                </button>
              </template>
            </div>
          </div>
        </div>
      </div>
    </Card>

    <!-- Loading Skeleton -->
    <SkeletonLoader
      v-if="loading"
      :type="isCardLayout ? 'grid' : 'table'"
      :rows="isCardLayout ? 3 : 8"
      :cols="isDesktopViewport ? 3 : 1"
    />

    <!-- Load Error -->
    <Card v-else-if="listError" padding="lg" class="text-center">
      <div class="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)]">
        <AlertCircle class="h-7 w-7 stroke-[1.5]" />
      </div>
      <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] mb-1">
        {{ $t('common.loadFailed') }}
      </h3>
      <p class="text-sm text-[var(--text-secondary)] mb-6 max-w-sm mx-auto leading-relaxed">
        {{ listError }}
      </p>
      <Button variant="primary" size="md" @click="loadInstances(true)">
        {{ $t('common.retry') }}
      </Button>
    </Card>

    <!-- Empty State -->
    <Card v-else-if="instances.length === 0" padding="none">
      <EmptyState
        :title="search ? $t('instance.noMatchingInstances') : $t('instance.noInstances')"
        :description="search ? $t('instance.tryOtherKeywords') : $t('instance.createFirstInstance')"
      >
        <template v-if="!search" #action>
          <RouterLink :to="instanceCreatePath()">
            <Button variant="primary" size="md">
              <template #icon>
                <Plus class="w-4 h-4" />
              </template>
              {{ configStore.freeSiteMode ? freeSiteCopy.instanceCreateFirst : $t('instance.create') }}
            </Button>
          </RouterLink>
        </template>
      </EmptyState>
    </Card>

    <!-- Instances Listing -->
    <template v-else>
      <!-- Desktop Table View -->
      <div v-if="instanceLayoutMode === 'list'" class="hidden overflow-hidden sm:block card">
        <table class="w-full table-fixed">
          <thead>
            <tr class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              <th class="w-[4%] px-3 py-3 text-center">
                <div class="flex items-center justify-center">
                  <button
                    type="button"
                    class="relative flex h-4 w-4 items-center justify-center rounded border border-[var(--border-strong)] bg-[var(--bg-surface)] text-[var(--accent)] transition-colors active:scale-95"
                    @click="toggleSelectAll"
                  >
                    <span v-if="isAllSelected" class="h-2 w-2 rounded-sm bg-current"></span>
                    <span v-else-if="isPartialSelected" class="h-0.5 w-2 rounded-full bg-current"></span>
                  </button>
                </div>
              </th>
              <th class="w-[22%] px-3 py-3">{{ $t('instance.name') }}</th>
              <th class="w-[11%] px-3 py-3 whitespace-nowrap">{{ $t('instance.statusLabel') }}</th>
              <th class="w-[15%] px-3 py-3 whitespace-nowrap">{{ $t('instance.publicIp') }}</th>
              <th class="w-[16%] px-3 py-3 whitespace-nowrap">{{ $t('instance.config') }}</th>
              <th class="w-[14%] px-3 py-3 whitespace-nowrap">{{ $t('instance.trafficLabel') }}</th>
              <th class="w-[11%] px-3 py-3 whitespace-nowrap">{{ $t('instance.expireAt') }}</th>
              <th class="w-[7%] px-2 py-3 text-center whitespace-nowrap">
                <span class="sr-only">{{ $t('common.actions') }}</span>
              </th>
            </tr>
          </thead>
          <TransitionGroup
            tag="tbody"
            name="instance-table-order"
            class="divide-y divide-[var(--border-color)]"
          >
            <tr
              v-for="instance in instances"
              :key="instance.id"
              class="group cursor-pointer transition-colors duration-150 hover:bg-[var(--bg-surface-soft)]"
              :class="[
                selectedIds.has(instance.id) ? 'bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))]' : 'bg-[var(--bg-surface)]'
              ]"
              @click="openInstanceDetail(instance.id)"
            >
              <!-- Checkbox -->
              <td class="w-[4%] px-3 py-3 text-center" @click.stop>
                <div class="flex items-center justify-center">
                  <input
                    type="checkbox"
                    class="w-4 h-4 rounded border-[var(--border-strong)] accent-[var(--accent)] cursor-pointer"
                    :checked="selectedIds.has(instance.id)"
                    @change="toggleSelect(instance.id)"
                  />
                </div>
              </td>

              <!-- Name & OS -->
              <td class="w-[22%] px-3 py-3">
                <div class="flex items-center gap-2.5 min-w-0">
                  <div class="w-8 h-8 rounded-xl flex items-center justify-center shrink-0 overflow-hidden bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                    <InstanceDisplayIcon
                      v-if="instance.iconBadgeId || getPaidIconType(instance)"
                      :badge-id="instance.iconBadgeId"
                      :fallback-icon="getPaidIconType(instance)"
                      :alt="instance.name"
                      :size="28"
                    />
                    <DistroIcon
                      v-else
                      :distro="getDistroFromName(instance.image)"
                      :size="24"
                    />
                  </div>
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-1.5 min-w-0">
                      <span
                        class="font-semibold text-sm text-[var(--text-primary)] hover:text-[var(--accent)] truncate transition-colors"
                        :title="instance.name"
                      >
                        {{ instance.name }}
                      </span>
                      <Badge v-if="!instance.packagePlanId" variant="default" size="sm" class="shrink-0">
                        {{ $t('instance.alwaysFree') }}
                      </Badge>
                    </div>
                    <div class="text-xs text-[var(--text-tertiary)] truncate mt-0.5" :title="formatImageName(instance.image, (instance as any).imageName)">
                      {{ formatImageName(instance.image, (instance as any).imageName) }}
                    </div>
                  </div>
                </div>
              </td>

              <!-- Status -->
              <td class="w-[11%] px-3 py-3 whitespace-nowrap">
                <Badge
                  :variant="getStatusBadgeVariant(instance.status)"
                  size="sm"
                  dot
                  :pulse="isStatusTransient(instance.status)"
                >
                  {{ getStatusInfo(instance.status, t).label }}
                </Badge>
              </td>

              <!-- IP -->
              <td class="w-[15%] px-3 py-3 font-mono text-xs">
                <div class="space-y-0.5 min-w-0">
                  <div class="flex items-center gap-1.5 min-w-0 text-[var(--text-primary)]">
                    <span class="truncate" :title="getPublicIp(instance)">{{ getPublicIp(instance) }}</span>
                    <button
                      v-if="getPublicIp(instance) && getPublicIp(instance) !== '-'"
                      type="button"
                      class="shrink-0 p-0.5 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] rounded transition-colors"
                      :title="$t('common.copy')"
                      @click.stop="copyIp(getPublicIp(instance))"
                    >
                      <Copy class="w-3 h-3" />
                    </button>
                  </div>
                  <div v-if="getPrivateIp(instance) && getPrivateIp(instance) !== '-'" class="text-[var(--text-tertiary)] text-[11px] truncate" :title="getPrivateIp(instance)">
                    {{ getPrivateIp(instance) }}
                  </div>
                </div>
              </td>

              <!-- Config -->
              <td class="w-[16%] px-3 py-3 text-xs">
                <div class="space-y-0.5 min-w-0">
                  <div class="font-medium text-[var(--text-primary)] truncate" :title="getInstancePlanName(instance)">
                    {{ getInstancePlanName(instance) }}
                  </div>
                  <div class="text-[var(--text-tertiary)] text-[11px] font-mono truncate">
                    {{ formatCpuCores(instance.cpu) }}{{ $t('instance.mobileCard.cpuCore') }} · {{ formatMemory(instance.memory) }} · {{ formatDisk(instance.disk) }}
                  </div>
                </div>
              </td>

              <!-- Traffic -->
              <td class="w-[14%] px-3 py-3 text-xs">
                <div class="space-y-1 min-w-0">
                  <div class="flex items-center justify-between text-[11px] font-mono">
                    <span class="text-[var(--text-secondary)] truncate">{{ getInstanceTrafficUsage(instance) }}</span>
                    <span v-if="getInstanceTrafficPercent(instance) !== null" class="text-[var(--text-tertiary)] ml-1">
                      {{ getInstanceTrafficPercent(instance) }}%
                    </span>
                  </div>
                  <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                    <div
                      class="h-full rounded-full transition-all duration-300"
                      :class="getInstanceTrafficBarColor(instance)"
                      :style="{ width: `${getInstanceTrafficPercent(instance) ?? 100}%` }"
                    ></div>
                  </div>
                </div>
              </td>

              <!-- Expiry -->
              <td class="w-[11%] px-3 py-3 text-xs">
                <div class="space-y-0.5 min-w-0" :title="getInstanceExpiryInfo(instance).title || ''">
                  <div class="font-medium text-[11px]" :class="getInstanceExpiryInfo(instance).className">
                    {{ getInstanceExpiryInfo(instance).remainingText }}
                  </div>
                  <div v-if="getInstanceExpiryInfo(instance).dateText" class="font-mono text-[10px] text-[var(--text-tertiary)] truncate">
                    {{ getInstanceExpiryInfo(instance).dateText }}
                  </div>
                  <div v-if="isAdmin && (instance as any).username" class="text-[10px] text-[var(--text-tertiary)] truncate" :title="(instance as any).username">
                    {{ (instance as any).username }}
                  </div>
                </div>
              </td>

              <!-- Actions -->
              <td class="w-[7%] px-2 py-3 text-center" @click.stop>
                <div class="inline-flex items-center justify-center gap-1">
                  <button
                    v-if="canStartInstance(instance)"
                    type="button"
                    :disabled="!!actionLoading[instance.id]"
                    class="p-1 rounded-full text-[var(--success)] hover:bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95 disabled:opacity-50"
                    :title="$t('instance.actions.start')"
                    @click.stop="handleAction(instance, 'start')"
                  >
                    <Loader2 v-if="actionLoading[instance.id] === 'start'" class="w-3.5 h-3.5 animate-spin" />
                    <Play v-else class="w-3.5 h-3.5 fill-current" />
                  </button>
                  <button
                    v-else-if="instance.status?.toLowerCase() === 'running'"
                    type="button"
                    :disabled="!!actionLoading[instance.id]"
                    class="p-1 rounded-full text-[var(--warning)] hover:bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95 disabled:opacity-50"
                    :title="$t('instance.actions.restart')"
                    @click.stop="handleAction(instance, 'restart')"
                  >
                    <Loader2 v-if="actionLoading[instance.id] === 'restart'" class="w-3.5 h-3.5 animate-spin" />
                    <RotateCw v-else class="w-3.5 h-3.5" />
                  </button>
                  <button
                    v-if="instance.status?.toLowerCase() === 'running'"
                    type="button"
                    :disabled="!!actionLoading[instance.id]"
                    class="p-1 rounded-full text-[var(--text-tertiary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95 disabled:opacity-50"
                    :title="$t('instance.actions.stop')"
                    @click.stop="handleAction(instance, 'stop')"
                  >
                    <Loader2 v-if="actionLoading[instance.id] === 'stop'" class="w-3.5 h-3.5 animate-spin" />
                    <Square v-else class="w-3.5 h-3.5 fill-current" />
                  </button>

                  <button
                    type="button"
                    class="p-1 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
                    :title="$t('instance.actions.more') || $t('instance.details')"
                    @click.stop="toggleRowMenu(instance.id, $event)"
                  >
                    <MoreHorizontal class="w-4 h-4" />
                  </button>
                </div>
              </td>
            </tr>
          </TransitionGroup>
        </table>
      </div>

      <!-- Desktop Card View -->
      <TransitionGroup
        v-else-if="instanceLayoutMode === 'card'"
        name="instance-card-order"
        tag="div"
        class="hidden sm:grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5"
      >
        <Card
          v-for="instance in instances"
          :key="instance.id"
          padding="none"
          class="group transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] flex flex-col justify-between"
          :class="[
            selectedIds.has(instance.id) ? 'ring-2 ring-[var(--accent)] border-[var(--accent)]' : ''
          ]"
        >
          <div class="p-5 space-y-4">
            <!-- Card Top: Checkbox, Icon, Name, Badges & Status -->
            <div class="flex items-start justify-between gap-3">
              <div class="flex items-start gap-3 min-w-0 flex-1">
                <!-- Checkbox -->
                <div class="pt-0.5 shrink-0" @click.stop>
                  <input
                    type="checkbox"
                    class="w-4 h-4 rounded border-[var(--border-strong)] accent-[var(--accent)] cursor-pointer"
                    :checked="selectedIds.has(instance.id)"
                    @change="toggleSelect(instance.id)"
                  />
                </div>

                <!-- Icon -->
                <div
                  class="w-10 h-10 rounded-xl flex items-center justify-center shrink-0 overflow-hidden bg-[var(--bg-surface-soft)] border border-[var(--border-color)] cursor-pointer"
                  @click="openInstanceDetail(instance.id)"
                >
                  <InstanceDisplayIcon
                    v-if="instance.iconBadgeId || getPaidIconType(instance)"
                    :badge-id="instance.iconBadgeId"
                    :fallback-icon="getPaidIconType(instance)"
                    :alt="instance.name"
                    :size="32"
                  />
                  <DistroIcon
                    v-else
                    :distro="getDistroFromName(instance.image)"
                    :size="28"
                  />
                </div>

                <!-- Name & Tags -->
                <div class="min-w-0 flex-1">
                  <h3
                    class="font-semibold text-base text-[var(--text-primary)] hover:text-[var(--accent)] truncate transition-colors cursor-pointer"
                    :title="instance.name"
                    @click="openInstanceDetail(instance.id)"
                  >
                    {{ instance.name }}
                  </h3>
                  <div class="mt-1 flex flex-wrap items-center gap-1.5">
                    <Badge variant="default" size="sm">
                      {{ (instance as any).instanceType === 'vm' ? $t('common.instanceType.vm') : $t('common.instanceType.container') }}
                    </Badge>
                    <Badge variant="default" size="sm" :title="getInstanceNetworkSummary(instance)">
                      {{ $t('common.networkMode.' + getInstanceNetworkMode(instance)) }}
                    </Badge>
                    <span class="inline-flex items-center gap-1 text-xs text-[var(--text-tertiary)] truncate" :title="getInstanceRegionLabel(instance)">
                      <FlagIcon :code="getInstanceRegionCode(instance)" size="xs" />
                      <span>{{ getInstanceHostName(instance) }}</span>
                    </span>
                  </div>
                </div>
              </div>

              <!-- Status Badge -->
              <Badge
                :variant="getStatusBadgeVariant(instance.status)"
                size="sm"
                dot
                :pulse="isStatusTransient(instance.status)"
                class="shrink-0"
              >
                {{ getStatusInfo(instance.status, t).label }}
              </Badge>
            </div>

            <!-- IP Strip -->
            <div
              class="flex items-center justify-between gap-2 px-3 py-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs cursor-pointer"
              @click="openInstanceDetail(instance.id)"
            >
              <div class="flex items-center gap-2 min-w-0 font-mono">
                <span class="text-[10px] font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">IP</span>
                <span class="text-[var(--text-primary)] truncate font-medium" :title="getPrimaryIp(instance)">
                  {{ getPrimaryIp(instance) }}
                </span>
              </div>
              <button
                v-if="getPrimaryIp(instance) && getPrimaryIp(instance) !== '-'"
                type="button"
                class="shrink-0 p-1 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] rounded transition-colors"
                :title="$t('common.copy')"
                @click.stop="copyIp(getPrimaryIp(instance))"
              >
                <Copy class="w-3.5 h-3.5" />
              </button>
            </div>

            <!-- Hardware Specs Ribbon -->
            <div
              class="grid grid-cols-3 gap-2 text-center cursor-pointer"
              :title="getInstanceConfigSummary(instance)"
              @click="openInstanceDetail(instance.id)"
            >
              <div class="p-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[10px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">CPU</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatCpuCores(instance.cpu) }} {{ $t('instance.mobileCard.cpuCore') }}
                </div>
              </div>
              <div class="p-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[10px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">RAM</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatMemory(instance.memory) }}
                </div>
              </div>
              <div class="p-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[10px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">DISK</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatDisk(instance.disk) }}
                </div>
              </div>
            </div>

            <!-- Traffic Progress Bar -->
            <div class="space-y-1.5 cursor-pointer" @click="openInstanceDetail(instance.id)">
              <div class="flex items-center justify-between text-xs">
                <span class="text-[var(--text-tertiary)] text-[11px]">{{ $t('instance.trafficLabel') }}</span>
                <span class="font-mono text-[var(--text-secondary)] tabular-nums text-[11px]">
                  {{ getInstanceTrafficUsage(instance) }}
                </span>
              </div>
              <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div
                  class="h-full rounded-full transition-all duration-300"
                  :class="getInstanceTrafficBarColor(instance)"
                  :style="{ width: `${getInstanceTrafficPercent(instance) ?? 100}%` }"
                ></div>
              </div>
            </div>

            <!-- Billing & Expiry Info -->
            <div class="flex items-center justify-between pt-2 border-t border-[var(--border-color)] text-xs">
              <div class="flex items-center gap-1.5 min-w-0" :title="getInstanceExpiryInfo(instance).title || ''">
                <span class="text-[var(--text-tertiary)] text-[11px]">{{ $t('instance.expireAt') }}:</span>
                <span class="font-medium text-[11px] truncate" :class="getInstanceExpiryInfo(instance).className">
                  {{ getInstanceExpiryInfo(instance).remainingText || getInstanceExpiryInfo(instance).dateText || '-' }}
                </span>
              </div>
              <div class="flex items-center gap-2.5 shrink-0">
                <span class="font-mono font-medium text-xs text-[var(--text-primary)]">{{ getInstanceMonthlyPrice(instance) }}</span>
                <div v-if="canUseInstanceBillingAction(instance)" class="flex items-center gap-1" :title="$t('instance.card.autoRenew')">
                  <button
                    type="button"
                    class="relative inline-flex h-4 w-7 items-center rounded-full transition-colors disabled:cursor-not-allowed disabled:opacity-50"
                    :class="(instance as any).autoRenew ? 'bg-[var(--accent)]' : 'bg-[var(--border-strong)]'"
                    :disabled="!!actionLoading[instance.id]"
                    :aria-pressed="!!(instance as any).autoRenew"
                    @click.stop="handleSingleAutoRenew(instance, !(instance as any).autoRenew)"
                  >
                    <span
                      class="inline-block h-3 w-3 rounded-full bg-white shadow-sm transition-transform"
                      :class="(instance as any).autoRenew ? 'translate-x-3.5' : 'translate-x-0.5'"
                    ></span>
                  </button>
                </div>
              </div>
            </div>
          </div>

          <!-- Card Footer Actions Bar -->
          <div class="px-5 py-3 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between gap-2">
            <!-- Power Buttons -->
            <div class="flex items-center gap-1">
              <Button
                v-if="canStartInstance(instance)"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--success)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'start'"
                :title="$t('instance.actions.start')"
                @click.stop="handleAction(instance, 'start')"
              >
                <template #icon>
                  <Play class="w-3.5 h-3.5 fill-current" />
                </template>
              </Button>
              <Button
                v-if="instance.status?.toLowerCase() === 'running'"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--warning)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'restart'"
                :title="$t('instance.actions.restart')"
                @click.stop="handleAction(instance, 'restart')"
              >
                <template #icon>
                  <RotateCw class="w-3.5 h-3.5" />
                </template>
              </Button>
              <Button
                v-if="instance.status?.toLowerCase() === 'running'"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--text-tertiary)] hover:text-[var(--error)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'stop'"
                :title="$t('instance.actions.stop')"
                @click.stop="handleAction(instance, 'stop')"
              >
                <template #icon>
                  <Square class="w-3.5 h-3.5 fill-current" />
                </template>
              </Button>
            </div>

            <!-- Action Buttons -->
            <div class="flex items-center gap-1.5">
              <Button
                variant="secondary"
                size="sm"
                class="h-8 px-2.5 text-xs"
                @click.stop="openInstanceTransfer(instance)"
              >
                {{ $t('instance.card.push') }}
              </Button>
              <Button
                v-if="canUseInstanceBillingAction(instance)"
                variant="secondary"
                size="sm"
                class="h-8 px-2.5 text-xs"
                :disabled="!!actionLoading[instance.id]"
                @click.stop="openSingleRenewModal(instance)"
              >
                {{ $t('instance.card.renew') }}
              </Button>
              <Button
                variant="primary"
                size="sm"
                class="h-8 px-3 text-xs"
                @click.stop="openInstanceDetail(instance.id)"
              >
                {{ $t('instance.card.manage') }}
              </Button>
            </div>
          </div>
        </Card>
      </TransitionGroup>

      <!-- Mobile Stack View -->
      <TransitionGroup name="instance-stack-order" tag="div" class="space-y-3 sm:hidden">
        <Card
          v-for="instance in instances"
          :key="instance.id"
          padding="none"
          class="overflow-hidden"
          :class="[
            selectedIds.has(instance.id) ? 'ring-2 ring-[var(--accent)] border-[var(--accent)]' : ''
          ]"
        >
          <div class="p-4 space-y-3">
            <!-- Header: Checkbox + Icon + Name & Status -->
            <div class="flex items-start gap-3">
              <div class="pt-0.5 shrink-0" @click.stop>
                <input
                  type="checkbox"
                  class="w-4 h-4 rounded border-[var(--border-strong)] accent-[var(--accent)] cursor-pointer"
                  :checked="selectedIds.has(instance.id)"
                  @change="toggleSelect(instance.id)"
                />
              </div>
              <div
                class="w-10 h-10 rounded-xl flex items-center justify-center shrink-0 overflow-hidden bg-[var(--bg-surface-soft)] border border-[var(--border-color)] cursor-pointer"
                @click="openInstanceDetail(instance.id)"
              >
                <InstanceDisplayIcon
                  v-if="instance.iconBadgeId || getPaidIconType(instance)"
                  :badge-id="instance.iconBadgeId"
                  :fallback-icon="getPaidIconType(instance)"
                  :alt="instance.name"
                  :size="32"
                />
                <DistroIcon
                  v-else
                  :distro="getDistroFromName(instance.image)"
                  :size="28"
                />
              </div>
              <div class="flex-1 min-w-0 cursor-pointer" @click="openInstanceDetail(instance.id)">
                <div class="flex items-center justify-between gap-2">
                  <h3 class="font-semibold text-sm text-[var(--text-primary)] truncate">
                    {{ instance.name }}
                  </h3>
                  <Badge
                    :variant="getStatusBadgeVariant(instance.status)"
                    size="sm"
                    dot
                    :pulse="isStatusTransient(instance.status)"
                  >
                    {{ getStatusInfo(instance.status, t).label }}
                  </Badge>
                </div>
                <div class="mt-1 flex flex-wrap items-center gap-1.5">
                  <Badge variant="default" size="sm">
                    {{ (instance as any).instanceType === 'vm' ? $t('common.instanceType.vm') : $t('common.instanceType.container') }}
                  </Badge>
                  <Badge variant="default" size="sm">
                    {{ $t('common.networkMode.' + getInstanceNetworkMode(instance)) }}
                  </Badge>
                  <span class="text-xs text-[var(--text-tertiary)] truncate max-w-[120px]">
                    {{ getInstancePlanName(instance) }}
                  </span>
                </div>
              </div>
            </div>

            <!-- IP row -->
            <div
              class="flex items-center justify-between gap-2 px-3 py-1.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs cursor-pointer font-mono"
              @click="openInstanceDetail(instance.id)"
            >
              <span class="text-[var(--text-primary)] truncate font-medium" :title="getPrimaryIp(instance)">
                {{ getPrimaryIp(instance) }}
              </span>
              <button
                v-if="getPrimaryIp(instance) && getPrimaryIp(instance) !== '-'"
                type="button"
                class="shrink-0 p-1 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] rounded transition-colors"
                :title="$t('common.copy')"
                @click.stop="copyIp(getPrimaryIp(instance))"
              >
                <Copy class="w-3.5 h-3.5" />
              </button>
            </div>

            <!-- Hardware Specs -->
            <div
              class="grid grid-cols-3 gap-1.5 text-center cursor-pointer"
              :title="getInstanceConfigSummary(instance)"
              @click="openInstanceDetail(instance.id)"
            >
              <div class="p-1.5 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[9px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">CPU</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatCpuCores(instance.cpu) }} {{ $t('instance.mobileCard.cpuCore') }}
                </div>
              </div>
              <div class="p-1.5 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[9px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">RAM</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatMemory(instance.memory) }}
                </div>
              </div>
              <div class="p-1.5 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div class="text-[9px] uppercase font-semibold tracking-wider text-[var(--text-tertiary)]">DISK</div>
                <div class="mt-0.5 text-xs font-mono font-semibold text-[var(--text-primary)] tabular-nums">
                  {{ formatDisk(instance.disk) }}
                </div>
              </div>
            </div>

            <!-- Traffic -->
            <div class="space-y-1 cursor-pointer" @click="openInstanceDetail(instance.id)">
              <div class="flex items-center justify-between text-xs">
                <span class="text-[var(--text-tertiary)] text-[11px]">{{ $t('instance.mobileCard.traffic') }}</span>
                <span class="font-mono text-[var(--text-secondary)] tabular-nums text-[11px]">
                  {{ getInstanceTrafficUsage(instance) }}
                </span>
              </div>
              <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div
                  class="h-full rounded-full transition-all duration-300"
                  :class="getInstanceTrafficBarColor(instance)"
                  :style="{ width: `${getInstanceTrafficPercent(instance) ?? 100}%` }"
                ></div>
              </div>
            </div>

            <!-- Expiry & Host -->
            <div class="flex items-center justify-between text-xs pt-1 border-t border-[var(--border-color)]">
              <span class="text-[var(--text-tertiary)] flex items-center gap-1" :title="getInstanceRegionLabel(instance)">
                <FlagIcon :code="getInstanceRegionCode(instance)" size="xs" />
                <span>{{ getInstanceHostName(instance) }}</span>
              </span>
              <span class="font-medium" :class="getInstanceExpiryInfo(instance).className">
                {{ getInstanceExpiryInfo(instance).remainingText }}
              </span>
            </div>
          </div>

          <!-- Mobile Card Footer -->
          <div class="flex items-center justify-between px-3 py-2 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
            <div class="flex items-center gap-1">
              <Button
                v-if="canStartInstance(instance)"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--success)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'start'"
                @click.stop="handleAction(instance, 'start')"
              >
                <template #icon>
                  <Play class="w-3.5 h-3.5 fill-current" />
                </template>
              </Button>
              <Button
                v-if="instance.status?.toLowerCase() === 'running'"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--warning)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'restart'"
                @click.stop="handleAction(instance, 'restart')"
              >
                <template #icon>
                  <RotateCw class="w-3.5 h-3.5" />
                </template>
              </Button>
              <Button
                v-if="instance.status?.toLowerCase() === 'running'"
                variant="ghost"
                size="sm"
                class="h-8 px-2 text-xs text-[var(--text-tertiary)] hover:text-[var(--error)]"
                :disabled="!!actionLoading[instance.id]"
                :loading="actionLoading[instance.id] === 'stop'"
                @click.stop="handleAction(instance, 'stop')"
              >
                <template #icon>
                  <Square class="w-3.5 h-3.5 fill-current" />
                </template>
              </Button>
            </div>

            <div class="flex items-center gap-1.5">
              <Button
                variant="secondary"
                size="sm"
                class="h-8 px-2 text-xs"
                @click.stop="openInstanceTransfer(instance)"
              >
                {{ $t('instance.card.push') }}
              </Button>
              <Button
                variant="primary"
                size="sm"
                class="h-8 px-3 text-xs"
                @click.stop="openInstanceDetail(instance.id)"
              >
                {{ $t('instance.card.manage') }}
              </Button>
            </div>
          </div>
        </Card>
      </TransitionGroup>

      <!-- Pagination -->
      <Card padding="sm">
        <Pagination
          v-model:page="page"
          v-model:pageSize="pageSize"
          :total="total"
          :show-size-changer="true"
          :page-size-options="pageSizeOptions"
          @change="handlePaginationChange"
        >
          <template #total="{ total: tCount, page: pNum, totalPages: pTotal }">
            <span class="font-mono text-xs text-[var(--text-secondary)]">
              {{ $t('instance.pageInfo', {
                page: pNum,
                totalPages: pTotal || 1,
                start: tCount > 0 ? (pNum - 1) * pageSize + 1 : 0,
                end: Math.min(pNum * pageSize, tCount),
                total: tCount
              }) }}
            </span>
          </template>
        </Pagination>
      </Card>
    </template>

    <!-- Floating Capsule Selection Bar -->
    <Transition
      enter-active-class="transition duration-300 ease-out"
      enter-from-class="transform translate-y-6 opacity-0"
      enter-to-class="transform translate-y-0 opacity-100"
      leave-active-class="transition duration-200 ease-in"
      leave-from-class="transform translate-y-0 opacity-100"
      leave-to-class="transform translate-y-6 opacity-0"
    >
      <div
        v-if="hasSelectedInstances"
        class="fixed bottom-6 left-1/2 -translate-x-1/2 z-40 flex items-center gap-2 px-4 py-2.5 rounded-full bg-[var(--bg-surface)] text-[var(--text-primary)] border border-[var(--border-color)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] backdrop-blur-md select-none text-xs"
      >
        <span class="font-medium pr-1">
          {{ $t('instance.batch.selectedCount', { count: selectedCount }) }}
        </span>
        <div class="h-3.5 w-px bg-[var(--border-color)]"></div>
        <Button
          v-if="selectedStoppedCount > 0"
          variant="ghost"
          size="sm"
          :disabled="!!batchActionLoading"
          class="h-7 px-2.5 text-xs text-[var(--success)]"
          @click="handleBatchSimpleAction('start')"
        >
          <Play class="w-3.5 h-3.5 fill-current" />
          <span>{{ $t('instance.batch.start') }} ({{ selectedStoppedCount }})</span>
        </Button>
        <Button
          v-if="selectedRunningCount > 0"
          variant="ghost"
          size="sm"
          :disabled="!!batchActionLoading"
          class="h-7 px-2.5 text-xs text-[var(--warning)]"
          @click="handleBatchSimpleAction('restart')"
        >
          <RotateCw class="w-3.5 h-3.5" />
          <span>{{ $t('instance.batch.restart') }} ({{ selectedRunningCount }})</span>
        </Button>
        <Button
          v-if="selectedRunningCount > 0"
          variant="ghost"
          size="sm"
          :disabled="!!batchActionLoading"
          class="h-7 px-2.5 text-xs text-[var(--error)]"
          @click="handleBatchSimpleAction('stop')"
        >
          <Square class="w-3.5 h-3.5 fill-current" />
          <span>{{ $t('instance.batch.stop') }} ({{ selectedRunningCount }})</span>
        </Button>
        <template v-if="!isAdminEntry && selectedBillingActionIds.length > 0">
          <Button
            variant="ghost"
            size="sm"
            :disabled="!!batchActionLoading"
            class="h-7 px-2.5 text-xs text-[var(--accent)]"
            @click="openBatchRenewModal()"
          >
            <span>{{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchRenewAction : $t('instance.batch.renew') }}</span>
          </Button>
        </template>
        <div class="h-3.5 w-px bg-[var(--border-color)]"></div>
        <Button
          variant="ghost"
          size="sm"
          class="h-7 px-2 text-xs text-[var(--text-tertiary)] hover:text-[var(--text-primary)]"
          @click="clearSelection"
        >
          {{ $t('instance.batch.clear') }}
        </Button>
      </div>
    </Transition>

    <!-- Row Menu Teleport -->
    <Teleport to="body">
      <div
        v-if="activeRowMenuId !== null"
        class="fixed inset-0 z-40"
        @click.stop="closeRowMenu"
      ></div>
      <div
        v-if="activeRowMenuId !== null"
        class="fixed z-50 w-44 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] p-1.5 text-left text-xs select-none backdrop-blur-md"
        :style="{ top: `${rowMenuPosition.top}px`, right: `${rowMenuPosition.right}px` }"
        @click.stop
      >
        <button
          type="button"
          class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
          @click.stop="const id = activeRowMenuId; closeRowMenu(); if (id) openInstanceDetail(id)"
        >
          {{ $t('instance.details') }}
        </button>

        <template v-if="getRowMenuInstance()">
          <div class="border-t border-[var(--border-color)] my-1"></div>

          <button
            v-if="canStartInstance(getRowMenuInstance()!)"
            type="button"
            :disabled="!!actionLoading[getRowMenuInstance()!.id]"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--success)] hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-50"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); handleAction(inst, 'start')"
          >
            {{ $t('instance.actions.start') }}
          </button>

          <button
            v-if="getRowMenuInstance()!.status?.toLowerCase() === 'running'"
            type="button"
            :disabled="!!actionLoading[getRowMenuInstance()!.id]"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--warning)] hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-50"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); handleAction(inst, 'stop')"
          >
            {{ $t('instance.actions.stop') }}
          </button>

          <button
            v-if="getRowMenuInstance()!.status?.toLowerCase() === 'running'"
            type="button"
            :disabled="!!actionLoading[getRowMenuInstance()!.id]"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--accent)] hover:bg-[var(--bg-surface-soft)] transition-colors disabled:opacity-50"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); handleAction(inst, 'restart')"
          >
            {{ $t('instance.actions.restart') }}
          </button>

          <button
            v-if="canUseInstanceBillingAction(getRowMenuInstance()!)"
            type="button"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); openSingleRenewModal(inst)"
          >
            {{ $t('instance.card.renew') }}
          </button>

          <button
            type="button"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); openInstanceTransfer(inst)"
          >
            {{ $t('instance.card.push') }}
          </button>

          <button
            v-if="canResetInstanceTraffic(getRowMenuInstance()!)"
            type="button"
            class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
            @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); openResetTrafficModal(inst)"
          >
            {{ $t('admin.hosts.resetTraffic') }}
          </button>

          <template v-if="canDeleteInstance(getRowMenuInstance()!)">
            <div class="border-t border-[var(--border-color)] my-1"></div>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 rounded-xl text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] transition-colors"
              @click.stop="const inst = getRowMenuInstance()!; closeRowMenu(); handleAction(inst, 'delete')"
            >
              {{ $t('instance.actions.delete') }}
            </button>
          </template>
        </template>
      </div>
    </Teleport>

    <!-- Reset Traffic Modal -->
    <Teleport to="body">
      <div
        v-if="showResetTrafficModal && resetTrafficTarget"
        class="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm flex items-center justify-center p-4"
        @click.self="closeResetTrafficModal()"
      >
        <div class="bg-[var(--bg-surface)] text-[var(--text-primary)] rounded-2xl border border-[var(--border-color)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] w-full max-w-md overflow-hidden animate-scale-in">
          <div class="flex items-center justify-between p-5 border-b border-[var(--border-color)]">
            <div>
              <h3 class="font-semibold text-lg text-[var(--text-primary)]">
                {{ $t('admin.hosts.resetTrafficTitle') }}
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ $t('admin.hosts.resetTrafficDesc', { instance: resetTrafficTarget.name }) }}
              </p>
            </div>
            <button
              type="button"
              class="p-1 rounded-full text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
              :disabled="resetTrafficSubmitting"
              @click="closeResetTrafficModal()"
            >
              <X class="w-5 h-5" />
            </button>
          </div>

          <div class="p-5 space-y-4 text-sm">
            <div class="space-y-2 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
              <div class="flex items-center justify-between">
                <span class="text-[var(--text-secondary)]">{{ $t('instance.trafficLabel') }}</span>
                <span class="font-mono font-medium">{{ getInstanceTrafficUsage(resetTrafficTarget) }}</span>
              </div>
              <div class="flex items-center justify-between">
                <span class="text-[var(--text-secondary)]">{{ $t('instance.card.trafficReset') }}</span>
                <span class="font-mono font-medium">{{ getInstanceResetTrafficPrice(resetTrafficTarget) }}</span>
              </div>
            </div>

            <p class="text-xs text-[var(--warning)] leading-relaxed">
              {{ $t('admin.hosts.resetTrafficWarning') }}
            </p>
          </div>

          <div class="p-5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-end gap-2.5">
            <Button
              variant="secondary"
              size="sm"
              :disabled="resetTrafficSubmitting"
              @click="closeResetTrafficModal()"
            >
              {{ $t('common.cancel') }}
            </Button>
            <Button
              variant="primary"
              size="sm"
              :loading="resetTrafficSubmitting"
              @click="confirmResetTraffic()"
            >
              {{ $t('admin.hosts.resetTraffic') }}
            </Button>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- Batch Renew Modal -->
    <Teleport to="body">
      <div
        v-if="!isAdminEntry && showBatchRenewModal"
        class="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm flex items-center justify-center p-4"
        @click.self="closeBatchRenewModal()"
      >
        <div class="bg-[var(--bg-surface)] text-[var(--text-primary)] rounded-2xl border border-[var(--border-color)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] w-full max-w-2xl overflow-hidden flex flex-col max-h-[90vh] animate-scale-in">
          <div class="flex items-center justify-between p-5 border-b border-[var(--border-color)] shrink-0">
            <div>
              <h3 class="font-semibold text-lg text-[var(--text-primary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchRenewTitle : $t('instance.batch.renewTitle') }}
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchRenewDescription : $t('instance.batch.renewDescription') }}
              </p>
            </div>
            <button
              type="button"
              class="p-1 rounded-full text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
              @click="closeBatchRenewModal()"
            >
              <X class="w-5 h-5" />
            </button>
          </div>

          <div class="p-5 space-y-4 overflow-y-auto flex-1">
            <div v-if="batchRenewLoading" class="flex items-center justify-center py-12">
              <Loader2 class="w-8 h-8 animate-spin text-[var(--accent)]" />
            </div>

            <template v-else>
              <!-- Summary Cards -->
              <div class="grid grid-cols-2 md:grid-cols-4 gap-2.5">
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.selectedCount', { count: selectedCount }) }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--text-primary)]">{{ selectedCount }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.eligibleCount') }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--success)]">{{ batchRenewEligibleItems.length }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchTotalAmount : $t('instance.batch.totalAmount') }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--text-primary)]">{{ formatCurrency(batchRenewTotal) }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchBalanceAfter : $t('billing.balanceAfterRenew') }}</div>
                  <div
                    class="mt-1 text-xl font-semibold font-mono tabular-nums"
                    :class="batchRenewInsufficientBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'"
                  >
                    {{ formatCurrency(batchRenewBalanceAfter) }}
                  </div>
                </div>
              </div>

              <!-- Duration selector -->
              <div v-if="batchRenewMonthOptions.length > 0" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
                <div class="text-xs font-medium text-[var(--text-primary)]">{{ $t('instance.batch.selectedMonths') }}</div>
                <div class="mt-2.5 flex flex-wrap gap-2">
                  <button
                    v-for="months in batchRenewMonthOptions"
                    :key="months"
                    type="button"
                    class="px-3.5 py-1.5 rounded-full text-xs font-medium border transition-all duration-200 active:scale-95"
                    :class="months === batchRenewMonths
                      ? 'border-[var(--accent)] bg-[var(--accent)] text-white'
                      : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="batchRenewMonths = months"
                  >
                    {{ getBatchRenewMonthsLabel(months) }}
                  </button>
                </div>
              </div>
              <div
                v-else
                class="rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] p-3 text-xs text-[var(--warning)]"
              >
                {{ $t('instance.batch.renewEmpty') }}
              </div>

              <!-- Insufficient Balance Warning -->
              <div
                v-if="batchRenewInsufficientBalance"
                class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-3 text-xs text-[var(--error)]"
              >
                <AlertCircle class="w-4 h-4 shrink-0" />
                <span>
                  {{ $t('billing.insufficientBalance') }}
                  <RouterLink :to="walletPath()" class="text-[var(--accent)] hover:underline ml-1 font-medium">{{ $t('billing.goRecharge') }}</RouterLink>
                </span>
              </div>

              <!-- Eligible list -->
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5">
                <div class="flex items-center justify-between mb-2.5">
                  <h4 class="text-xs font-medium text-[var(--text-primary)]">
                    {{ $t('instance.batch.eligibleList', { count: batchRenewEligibleItems.length }) }}
                  </h4>
                  <span class="text-[11px] text-[var(--text-tertiary)]">
                    {{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchCurrentBalance : $t('billing.currentBalance') }}: {{ formatCurrency(batchRenewBalance.balance) }}
                  </span>
                </div>
                <div v-if="batchRenewEligibleItems.length > 0" class="space-y-2 max-h-48 overflow-y-auto pr-1">
                  <div
                    v-for="item in batchRenewEligibleItems"
                    :key="item.id"
                    class="flex items-center justify-between p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs"
                  >
                    <div class="min-w-0 flex-1 pr-3">
                      <div class="font-medium text-[var(--text-primary)] truncate">{{ item.name }}</div>
                      <div class="text-[11px] text-[var(--text-tertiary)] mt-0.5">
                        {{ item.autoRenew ? $t('billing.autoRenewEnabled') : $t('billing.autoRenewDisabled') }}
                      </div>
                    </div>
                    <div class="text-right shrink-0 font-mono">
                      <div class="font-semibold text-[var(--success)]">{{ formatCurrency(item.selectedOption?.discountedPrice) }}</div>
                      <div class="text-[10px] text-[var(--text-tertiary)]">{{ formatDate(item.selectedOption?.expiresAt) }}</div>
                    </div>
                  </div>
                </div>
                <div v-else class="text-xs text-[var(--text-tertiary)] py-3 text-center">
                  {{ $t('instance.batch.renewEmpty') }}
                </div>
              </div>

              <!-- Ineligible items list -->
              <div v-if="batchRenewIneligibleItems.length > 0" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5">
                <h4 class="text-xs font-medium text-[var(--text-primary)] mb-2">
                  {{ $t('instance.batch.skippedList', { count: batchRenewIneligibleItems.length }) }}
                </h4>
                <div class="space-y-1.5 max-h-36 overflow-y-auto pr-1">
                  <div
                    v-for="item in batchRenewIneligibleItems"
                    :key="`renew-skip-${item.id}`"
                    class="flex items-center justify-between p-2 rounded-lg bg-[var(--bg-surface-soft)] text-xs"
                  >
                    <span class="text-[var(--text-primary)] truncate">{{ item.name }}</span>
                    <span class="text-[var(--error)] text-[11px] shrink-0 ml-2">
                      {{ translateBatchReason(item.reason || (item.canRenew ? '该实例不支持当前续费时长' : undefined)) }}
                    </span>
                  </div>
                </div>
              </div>
            </template>
          </div>

          <div class="p-5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between gap-3 shrink-0">
            <span class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.currentPageOnly') }}</span>
            <div class="flex items-center gap-2">
              <Button
                variant="secondary"
                size="sm"
                :disabled="batchRenewSubmitting"
                @click="closeBatchRenewModal()"
              >
                {{ $t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                size="sm"
                :disabled="!batchRenewCanSubmit"
                :loading="batchRenewSubmitting"
                @click="confirmBatchRenew"
              >
                {{ configStore.freeSiteMode ? freeSiteCopy.instanceBatchRenewAction : $t('instance.batch.renew') }}
              </Button>
            </div>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- Batch Destroy Modal -->
    <Teleport to="body">
      <div
        v-if="!isAdminEntry && showBatchDestroyModal"
        class="fixed inset-0 z-50 bg-black/40 backdrop-blur-sm flex items-center justify-center p-4"
        @click.self="closeBatchDestroyModal()"
      >
        <div class="bg-[var(--bg-surface)] text-[var(--text-primary)] rounded-2xl border border-[var(--border-color)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] w-full max-w-2xl overflow-hidden flex flex-col max-h-[90vh] animate-scale-in">
          <div class="flex items-center justify-between p-5 border-b border-[var(--border-color)] shrink-0">
            <div>
              <h3 class="font-semibold text-lg text-[var(--text-primary)]">
                {{ $t('instance.batch.destroyTitle') }}
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ $t('instance.batch.destroyDescription') }}
              </p>
            </div>
            <button
              type="button"
              class="p-1 rounded-full text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
              @click="closeBatchDestroyModal()"
            >
              <X class="w-5 h-5" />
            </button>
          </div>

          <div class="p-5 space-y-4 overflow-y-auto flex-1">
            <div v-if="batchDestroyLoading" class="flex items-center justify-center py-12">
              <Loader2 class="w-8 h-8 animate-spin text-[var(--error)]" />
            </div>

            <template v-else>
              <!-- Warning banner -->
              <div class="flex items-start gap-2.5 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-3.5 text-xs text-[var(--error)]">
                <AlertTriangle class="w-4 h-4 shrink-0 mt-0.5" />
                <p class="font-medium leading-relaxed">
                  {{ $t('instance.destroy.warning') }}
                </p>
              </div>

              <!-- Stats Grid -->
              <div class="grid grid-cols-2 md:grid-cols-4 gap-2.5">
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.selectedCount', { count: selectedCount }) }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--text-primary)]">{{ selectedCount }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.eligibleCount') }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--success)]">{{ batchDestroyEligibleItems.length }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.refundTotal') }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--success)]">{{ formatCurrency(batchDestroyTotalRefund) }}</div>
                </div>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3">
                  <div class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.feeTotal') }}</div>
                  <div class="mt-1 text-xl font-semibold font-mono tabular-nums text-[var(--text-primary)]">{{ formatCurrency(batchDestroyTotalFee) }}</div>
                </div>
              </div>

              <!-- Eligible list -->
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5">
                <h4 class="text-xs font-medium text-[var(--text-primary)] mb-2.5">
                  {{ $t('instance.batch.eligibleList', { count: batchDestroyEligibleItems.length }) }}
                </h4>
                <div v-if="batchDestroyEligibleItems.length > 0" class="space-y-2 max-h-48 overflow-y-auto pr-1">
                  <div
                    v-for="item in batchDestroyEligibleItems"
                    :key="item.id"
                    class="flex items-center justify-between p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs"
                  >
                    <div class="min-w-0 flex-1 pr-3">
                      <div class="font-medium text-[var(--text-primary)] truncate">{{ item.name }}</div>
                      <div class="text-[11px] text-[var(--text-tertiary)] mt-0.5">
                        {{ item.instance.hostName }} · {{ item.instance.planName || $t('billing.freeInstance') }}
                      </div>
                    </div>
                    <div class="text-right shrink-0 font-mono">
                      <div class="font-semibold text-[var(--success)]">{{ formatCurrency(item.refund.refundAmount) }}</div>
                      <div class="text-[10px] text-[var(--text-tertiary)]">{{ $t('instance.destroy.feeAmount') }} {{ formatCurrency(item.refund.feeAmount) }}</div>
                    </div>
                  </div>
                </div>
                <div v-else class="text-xs text-[var(--text-tertiary)] py-3 text-center">
                  {{ $t('instance.batch.destroyEmpty') }}
                </div>
              </div>

              <!-- Ineligible items list -->
              <div v-if="batchDestroyIneligibleItems.length > 0" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3.5">
                <h4 class="text-xs font-medium text-[var(--text-primary)] mb-2">
                  {{ $t('instance.batch.skippedList', { count: batchDestroyIneligibleItems.length }) }}
                </h4>
                <div class="space-y-1.5 max-h-36 overflow-y-auto pr-1">
                  <div
                    v-for="item in batchDestroyIneligibleItems"
                    :key="`destroy-skip-${item.id}`"
                    class="flex items-center justify-between p-2 rounded-lg bg-[var(--bg-surface-soft)] text-xs"
                  >
                    <span class="text-[var(--text-primary)] truncate">{{ item.name }}</span>
                    <span class="text-[var(--error)] text-[11px] shrink-0 ml-2">
                      {{ item.cannotDestroyReason ? translateBatchReason(item.cannotDestroyReason) : $t('instance.destroy.cannotDestroy') }}
                    </span>
                  </div>
                </div>
              </div>

              <!-- Confirmation input -->
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-1.5">
                <label class="block text-xs font-medium text-[var(--text-primary)]">
                  {{ $t('instance.batch.confirmHint') }}
                </label>
                <Input
                  v-model="batchDestroyConfirm"
                  size="sm"
                  :placeholder="$t('instance.batch.confirmPlaceholder')"
                />
              </div>
            </template>
          </div>

          <div class="p-5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between gap-3 shrink-0">
            <span class="text-[11px] text-[var(--text-tertiary)]">{{ $t('instance.batch.currentPageOnly') }}</span>
            <div class="flex items-center gap-2">
              <Button
                variant="secondary"
                size="sm"
                :disabled="batchDestroySubmitting"
                @click="closeBatchDestroyModal()"
              >
                {{ $t('common.cancel') }}
              </Button>
              <Button
                variant="danger"
                size="sm"
                :disabled="!batchDestroyCanSubmit"
                :loading="batchDestroySubmitting"
                @click="confirmBatchDestroy"
              >
                {{ $t('instance.batch.destroy') }}
              </Button>
            </div>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.instance-table-order-move,
.instance-stack-order-move,
.instance-card-order-move {
  transition:
    transform 400ms cubic-bezier(0.25, 0.1, 0.25, 1),
    opacity 400ms cubic-bezier(0.25, 0.1, 0.25, 1);
}

@media (prefers-reduced-motion: reduce) {
  .instance-table-order-move,
  .instance-stack-order-move,
  .instance-card-order-move {
    transition-duration: 1ms;
  }
}
</style>
