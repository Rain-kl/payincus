<script setup lang="ts">
import GpuSharingFields from '@/components/GpuSharingFields.vue'
import { ref, computed, onMounted, onActivated, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import PackageQuotaReleaseModal from '@/components/PackageQuotaReleaseModal.vue'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import { translateError } from '@/utils/errorHandler'
import type { Package, Host } from '@/types/api'
import { instanceCreatePath, isAdminEntry } from '@/utils/app-paths'
import PackageFormView from './PackageFormView.vue'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select
} from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  Check,
  Copy,
  Edit2,
  Layers,
  Plus,
  Search,
  Sliders,
  Trash2,
  X
} from 'lucide-vue-next'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'MyPackagesView' })

const { t } = useI18n()
const toast = useToast()
const authStore = useAuthStore()
const isAdmin = computed(() => authStore.user?.role === 'admin')
const MAX_PACKAGE_PLAN_PRICE_CENTS = 99999999
const MAX_PACKAGE_PLAN_PRICE = MAX_PACKAGE_PLAN_PRICE_CENTS / 100

const packages = ref<Package[]>([])
const hosts = ref<Host[]>([])
const loading = ref<boolean>(true)
const activeChangingId = ref<number | null>(null)

// 搜索
const searchQuery = ref('')

// 过滤后的套餐（按套餐名、节点名、描述搜索）
const filteredPackages = computed(() => {
  const query = searchQuery.value.toLowerCase().trim()
  if (!query) return packages.value
  return packages.value.filter(pkg => {
    // 按套餐名搜索
    if (pkg.name.toLowerCase().includes(query)) return true
    // 按描述搜索
    if (pkg.description && pkg.description.toLowerCase().includes(query)) return true
    // 按节点名搜索
    const hostNames = getHostNames(pkg as Package & { host_ids?: number[] }).toLowerCase()
    if (hostNames.includes(query)) return true
    return false
  })
})

// 分页配置
const packagesPage = ref(1)
const packagesPageSize = 100
const packagesTotalPages = computed(() => Math.max(1, Math.ceil(filteredPackages.value.length / packagesPageSize)))
const paginatedPackages = computed(() => {
  const start = (packagesPage.value - 1) * packagesPageSize
  return filteredPackages.value.slice(start, start + packagesPageSize)
})

// 监听总页数变化，防止页码越界
watch(packagesTotalPages, (newTotal) => {
  if (packagesPage.value > newTotal) {
    packagesPage.value = Math.max(1, newTotal)
  }
})

// 搜索时重置页码
function onSearchChange() {
  packagesPage.value = 1
}

// 清除搜索
function clearSearch() {
  searchQuery.value = ''
  packagesPage.value = 1
}

// 记录是否已经首次加载
let hasInitialLoad = false

// 配额释放相关
const showQuotaReleaseModal = ref<boolean>(false)
const quotaReleasePackage = ref<Package | null>(null)

function openQuotaReleaseModal(pkg: Package): void {
  quotaReleasePackage.value = pkg
  showQuotaReleaseModal.value = true
}

function handleQuotaReleaseSuccess(): void {
  // 可选：刷新套餐列表以显示最新配额
}

function bytesToGB(bytes: string | null | undefined): number {
  if (!bytes) return 1
  try {
    const b = Number(bytes)
    if (isNaN(b) || b <= 0) return 1
    // 使用浮点运算支持小于1GB的值，保甙6位小数避免精度丢失
    const gb = b / (1024 * 1024 * 1024)
    // 四舍五入到6位小数，避免浮点精度问题
    const rounded = Math.round(gb * 1000000) / 1000000
    return rounded || 1
  } catch {
    return 1
  }
}

// 字节数转换为 Mbps（后端存储的是 bytes 但按 MB 计算为 Mbit）
function bytesToMbps(bytes: string | null | undefined): number {
  if (!bytes || bytes === '0') return 10
  try {
    const mbitMatch = bytes.match(/^(\d+(?:\.\d+)?)\s*Mbit$/i)
    if (mbitMatch) {
      return Math.round(Number(mbitMatch[1])) || 10
    }
    const gbitMatch = bytes.match(/^(\d+(?:\.\d+)?)\s*Gbit$/i)
    if (gbitMatch) {
      return Math.round(Number(gbitMatch[1]) * 1000) || 10
    }

    const b = Number(bytes)
    if (isNaN(b) || b <= 0) return 10
    // 后端计算: bytes / (1024 * 1024) = Mbit
    const mbps = b / (1024 * 1024)
    return Math.round(mbps) || 10
  } catch {
    return 10
  }
}

onMounted(async (): Promise<void> => {
  await Promise.all([loadPackages(), loadHosts()])
  hasInitialLoad = true
})

// 当组件从 KeepAlive 缓存中激活时，重新加载数据
onActivated(async (): Promise<void> => {
  if (hasInitialLoad) {
    await Promise.all([loadPackages(), loadHosts()])
  }
})

async function loadPackages(): Promise<void> {
  loading.value = true
  try {
    if (isAdmin.value) {
      // 管理员查看自己的套餐（传 scope: 'mine'）
      const response = await api.packages.list({ all: true, scope: 'mine' })
      packages.value = (response as { packages?: Package[] }).packages || []
    } else {
      // 普通用户查看自己的套餐
      const response = await api.packages.list({ all: true })
      const allPackages = (response as { packages?: Package[] }).packages || []
      packages.value = allPackages.filter((p: any) => p.isOwn === true)
    }
  } finally {
    loading.value = false
  }
}

async function loadHosts(): Promise<void> {
  try {
    // 管理员加载所有节点（包括官方节点），普通用户只加载自己的节点
    // 传递 pageSize 以获取所有宿主机，避免默认分页限制（默认只返回20条）
    const response = await api.hosts.list(isAdmin.value ? { pageSize: '1000' } : { mine: 'true', pageSize: '1000' })
    hosts.value = (response as { hosts?: Host[] }).hosts || []
  } catch (err) {
    console.error('加载宿主机失败:', err)
  }
}

function getHostNames(pkg: Package & { host_ids?: number[] }): string {
  const hostIds = pkg.host_ids || []
  if (hostIds.length === 0) return t('admin.packages.noHostsBound')
  const hostNames = hostIds.map(id => {
    const host = hosts.value.find(h => h.id === id)
    return host ? host.name : `Host #${id}`
  })
  return hostNames.join(', ')
}

function getPackageNetworkModeLabel(pkg: Package): string {
  const mode = (pkg.network_mode || (pkg as { networkMode?: Package['network_mode'] }).networkMode || 'nat').toLowerCase()
  const key = `common.networkMode.${mode}`
  const translated = t(key)
  return translated === key ? mode : translated
}

function getPackageInstanceTypeLabel(pkg: Package): string {
  return pkg.instance_type === 'vm' ? t('common.instanceType.vm') : t('common.instanceType.container')
}

function formatTrafficMultiplier(value: unknown): string {
  const multiplier = Number(value ?? 1)
  if (!Number.isFinite(multiplier)) return '1x'
  return `${Math.round(multiplier * 1000) / 1000}x`
}

function getPackageTrafficMultiplierItems(pkg: Package & { host_ids?: number[] }): Array<{ hostName: string; multiplier: string }> {
  const hostIds = pkg.host_ids || []
  const multipliers = pkg.host_traffic_multipliers || {}

  if (hostIds.length === 0) {
    return [{ hostName: t('admin.packages.noHostsBound'), multiplier: formatTrafficMultiplier(1) }]
  }

  return hostIds.map(id => {
    const host = hosts.value.find(h => h.id === id)
    return {
      hostName: host ? host.name : `Host #${id}`,
      multiplier: formatTrafficMultiplier(multipliers[String(id)] ?? 1)
    }
  })
}

function getPackageTrafficMultiplierLabel(pkg: Package & { host_ids?: number[] }): string {
  const items = getPackageTrafficMultiplierItems(pkg)
  const uniqueMultipliers = [...new Set(items.map(item => item.multiplier))]
  return uniqueMultipliers.join(' / ')
}

function getPackageTrafficMultiplierTitle(pkg: Package & { host_ids?: number[] }): string {
  return getPackageTrafficMultiplierItems(pkg)
    .map(item => `${item.hostName}: ${item.multiplier}`)
    .join(', ')
}

function isPackagePublic(pkg: Package): boolean {
  const globalShared = (pkg as { global_shared?: unknown }).global_shared
  return isPackageActive(pkg) && (globalShared === true || globalShared === 1 || pkg.isGlobalShared === true)
}

function isPackageActive(pkg: Package): boolean {
  const active = (pkg as { active?: unknown }).active
  return active === true || active === 1
}

/**
 * 复制套餐分享链接
 * 链接格式: /instances/create?source=official&package=123
 */
function copyShareLink(pkg: Package): void {
  if (isAdminEntry) return

  const link = `${window.location.origin}${instanceCreatePath()}?source=official&package=${pkg.id}`
  
  navigator.clipboard.writeText(link).then(() => {
    toast.success(t('resources.packages.shareLinkCopied'))
  }).catch(() => {
    toast.error(t('common.copyFailed'))
  })
}

const showPackageDrawer = ref(false)
const editingPackageId = ref<number | null>(null)

// 打开创建套餐抽屉
async function openCreate(): Promise<void> {
  editingPackageId.value = null
  showPackageDrawer.value = true
}

function openEdit(pkg: Package): void {
  editingPackageId.value = pkg.id
  showPackageDrawer.value = true
}

async function deletePackage(pkg: Package): Promise<void> {
  if (!confirm(t('admin.packages.confirmDelete', { name: pkg.name }))) return

  try {
    await api.packages.delete(pkg.id)
    packages.value = packages.value.filter(p => p.id !== pkg.id)
    toast.success(t('admin.packages.packageDeleted'))
  } catch (err: any) {
    toast.error(t('admin.packages.deleteFailed') + ': ' + translateError(err))
  }
}

async function togglePackageActive(pkg: Package): Promise<void> {
  const nextActive = !isPackageActive(pkg)
  const confirmKey = nextActive ? 'admin.packages.confirmActivate' : 'admin.packages.confirmDeactivate'
  if (!confirm(t(confirmKey, { name: pkg.name }))) return

  activeChangingId.value = pkg.id
  try {
    await api.packages.update(pkg.id, { active: nextActive })
    pkg.active = nextActive ? 1 : 0
    toast.success(t(nextActive ? 'admin.packages.packageEnabled' : 'admin.packages.packageDisabled'))
    await loadPackages()
  } catch (err: any) {
    toast.error(t('admin.packages.statusUpdateFailed') + ': ' + translateError(err))
  } finally {
    activeChangingId.value = null
  }
}

// 格式化内存
function formatMemory(mb: number): string {
  if (!mb) return '0'
  if (mb >= 1024) {
    const gb = mb / 1024
    return gb % 1 === 0 ? gb.toFixed(0) + ' GB' : gb.toFixed(1) + ' GB'
  }
  return mb + ' MB'
}

// 格式化磁盘（1024进制）
function formatDisk(mb: number): string {
  if (!mb) return '0'
  if (mb >= 1024) {
    const gb = mb / 1024
    return gb % 1 === 0 ? gb.toFixed(0) + ' GB' : gb.toFixed(1) + ' GB'
  }
  return mb + ' MB'
}

// 方案管理相关
interface PackagePlan {
  gpuShared?: boolean | null
  gpuDeviceIds?: number[]
  id: number
  name: string
  description: string | null
  cpu: number
  memory: number
  disk: number
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  swapSize: number
  trafficLimit: string
  trafficLimitSpeed: string
  price: number
  trafficResetPrice: number | null
  billingCycle: number
  monthlyPrice: number
  isActive: boolean
  isSoldOut: boolean
  sortOrder: number
  slaGuarantee: number | null
}

type PackagePlanStatus = 'active' | 'soldOut' | 'inactive'

const showPlansModal = ref<boolean>(false)
const plansPackage = ref<Package | null>(null)
const plans = ref<PackagePlan[]>([])
const plansLoading = ref<boolean>(false)
const showPlanFormModal = ref<boolean>(false)
const editingPlan = ref<PackagePlan | null>(null)
const planFormLoading = ref<boolean>(false)

const planForm = ref({
  gpuShared: null as boolean | null,
  gpuDeviceIds: [] as number[],
  name: '',
  description: '',
  cpu: 100,
  memory: 512,
  disk: 5120,
  portLimit: 3,
  snapshotLimit: 3,
  swapSize: 0,
  siteLimit: 1,
  trafficLimit: 1,
  trafficLimitSpeed: 10, // Mbps
  price: 0,
  trafficResetPrice: null as number | null | '',
  billingCycle: 1,  // 计费周期（月），默认月付
  status: 'active' as PackagePlanStatus,
  sortOrder: 0,
  slaGuarantee: null as number | null
})

const planStatusOptions = computed(() => [
  { value: 'active' as PackagePlanStatus, label: t('resources.plans.statusActive'), description: t('resources.plans.statusActiveHint') },
  { value: 'soldOut' as PackagePlanStatus, label: t('resources.plans.statusSoldOut'), description: t('resources.plans.statusSoldOutHint') },
  { value: 'inactive' as PackagePlanStatus, label: t('resources.plans.statusInactive'), description: t('resources.plans.statusInactiveHint') }
])

const selectedPlanStatusOption = computed(() =>
  planStatusOptions.value.find(option => option.value === planForm.value.status) || planStatusOptions.value[0]
)

function getPlanStatus(plan: PackagePlan): PackagePlanStatus {
  if (!plan.isActive) return 'inactive'
  if (plan.isSoldOut) return 'soldOut'
  return 'active'
}

function getPlanStatusLabel(plan: PackagePlan): string {
  const status = getPlanStatus(plan)
  if (status === 'soldOut') return t('resources.plans.statusSoldOut')
  if (status === 'inactive') return t('resources.plans.statusInactive')
  return t('resources.plans.statusActive')
}

function getPlanBadgeVariant(plan: PackagePlan): BadgeVariant {
  const status = getPlanStatus(plan)
  if (status === 'soldOut') return 'warning'
  if (status === 'inactive') return 'default'
  return 'success'
}

// 计费周期选项（单位：月）- 非管理员只能选择月付
const billingCycleOptions = computed(() => {
  // 非管理员只能选择月付
  if (!authStore.isAdmin) {
    return [{ value: 1, label: t('resources.plans.monthly') }]
  }
  // 管理员可以选择所有周期
  return [
    { value: 1, label: t('resources.plans.monthly') },
    { value: 3, label: t('resources.plans.quarterly') },
    { value: 6, label: t('resources.plans.semiAnnual') },
    { value: 12, label: t('resources.plans.yearly') }
  ]
})

function normalizePlanPriceCents(value: unknown): number | null {
  const price = Number(value)
  if (!Number.isFinite(price) || price < 0 || price > MAX_PACKAGE_PLAN_PRICE) {
    return null
  }

  const cents = price * 100
  if (Math.abs(cents - Math.round(cents)) >= 1e-8) {
    return null
  }

  const roundedCents = Math.round(cents)
  return roundedCents <= MAX_PACKAGE_PLAN_PRICE_CENTS ? roundedCents : null
}

function normalizeTrafficResetPriceYuan(value: unknown): number | null {
  const price = Number(value)
  if (!Number.isFinite(price) || price < 0 || price > MAX_PACKAGE_PLAN_PRICE) {
    return null
  }

  const rounded = Math.round(price * 100) / 100
  return Math.abs(price - rounded) < 1e-8 ? rounded : null
}

async function openPlansModal(pkg: Package): Promise<void> {
  plansPackage.value = pkg
  showPlansModal.value = true
  await loadPlans(pkg.id)
}

async function loadPlans(packageId: number): Promise<void> {
  plansLoading.value = true
  try {
    const res = await api.packages.getPlans(packageId)
    plans.value = res.plans || []
  } catch (err) {
    console.error('加载方案列表失败:', err)
    plans.value = []
  } finally {
    plansLoading.value = false
  }
}

function openCreatePlanModal(): void {
  editingPlan.value = null
  planForm.value = {
    gpuShared: null,
    gpuDeviceIds: [],
    name: '',
    description: '',
    cpu: plansPackage.value?.cpu_max || 100,
    memory: plansPackage.value?.memory_max || 512,
    disk: plansPackage.value?.disk_max || 5120,
    portLimit: 3,
    snapshotLimit: 3,
    swapSize: 0,
    siteLimit: 1,
    trafficLimit: 1,
    trafficLimitSpeed: 10, // Mbps
    price: 0,
    trafficResetPrice: null,
    billingCycle: 1,  // 默认月付
    status: 'active',
    sortOrder: plans.value.length,
    slaGuarantee: null
  }
  showPlanFormModal.value = true
}

function openEditPlanModal(plan: PackagePlan): void {
  editingPlan.value = plan
  planForm.value = {
    gpuShared: plan.gpuShared ?? null,
    gpuDeviceIds: plan.gpuDeviceIds ?? [],
    name: plan.name,
    description: plan.description || '',
    cpu: plan.cpu,
    memory: plan.memory,
    disk: plan.disk,
    portLimit: plan.portLimit,
    snapshotLimit: plan.snapshotLimit,
    swapSize: plan.swapSize,
    siteLimit: plan.siteLimit,
    trafficLimit: bytesToGB(plan.trafficLimit),
    trafficLimitSpeed: bytesToMbps(plan.trafficLimitSpeed),
    price: plan.price / 100,
    trafficResetPrice: plan.trafficResetPrice === null || plan.trafficResetPrice === undefined ? null : plan.trafficResetPrice,
    billingCycle: plan.billingCycle,
    status: getPlanStatus(plan),
    sortOrder: plan.sortOrder,
    slaGuarantee: plan.slaGuarantee
  }
  showPlanFormModal.value = true
}

function gbToBytes(gb: string | number): string {
  if (gb === '' || gb === null || gb === undefined) return ''
  try {
    const gbNum = typeof gb === 'string' ? parseFloat(gb) : gb
    if (isNaN(gbNum) || gbNum <= 0) return ''
    // 使用普通数学运算支持小数，然后转为BigInt字符串
    const bytes = Math.floor(gbNum * 1024 * 1024 * 1024)
    return bytes.toString()
  } catch {
    return ''
  }
}

// Mbps 转换为字节数（后端按 bytes / (1024*1024) = Mbit 计算）
function mbpsToBytes(mbps: string | number): string {
  if (mbps === '' || mbps === null || mbps === undefined) return ''
  try {
    const mbpsNum = typeof mbps === 'string' ? parseFloat(mbps) : mbps
    if (isNaN(mbpsNum) || mbpsNum <= 0) return ''
    // 后端计算: bytes / (1024 * 1024) = Mbit，所以 bytes = Mbit * 1024 * 1024
    const bytes = Math.floor(mbpsNum * 1024 * 1024)
    return bytes.toString()
  } catch {
    return ''
  }
}

async function savePlan(): Promise<void> {
  if (!plansPackage.value || !planForm.value.name.trim()) return

  const priceCents = normalizePlanPriceCents(planForm.value.price)
  if (priceCents === null) {
    toast.error(t('resources.plans.priceRangeError', { max: MAX_PACKAGE_PLAN_PRICE.toFixed(2) }))
    return
  }
  const trafficResetPriceYuan = planForm.value.trafficResetPrice === null || planForm.value.trafficResetPrice === ''
    ? null
    : normalizeTrafficResetPriceYuan(planForm.value.trafficResetPrice)
  const hasTrafficResetPriceOverride = planForm.value.trafficResetPrice !== null && planForm.value.trafficResetPrice !== ''
  if (hasTrafficResetPriceOverride && trafficResetPriceYuan === null) {
    toast.error(`流量重置价格必须在 0-${MAX_PACKAGE_PLAN_PRICE.toFixed(2)} 元之间，且最多两位小数`)
    return
  }
  planForm.value.price = priceCents / 100

  planFormLoading.value = true
  try {
    const data = {
      gpuShared: planForm.value.gpuShared,
      gpuDeviceIds: planForm.value.gpuDeviceIds,
      name: planForm.value.name.trim(),
      description: planForm.value.description.trim() || undefined,
      cpu: planForm.value.cpu,
      memory: planForm.value.memory,
      disk: planForm.value.disk,
      portLimit: planForm.value.portLimit,
      snapshotLimit: planForm.value.snapshotLimit,
      backupLimit: 0,
      siteLimit: planForm.value.siteLimit,
      swapSize: planForm.value.swapSize,
      trafficLimit: gbToBytes(planForm.value.trafficLimit) || '0',
      trafficLimitSpeed: mbpsToBytes(planForm.value.trafficLimitSpeed) || '0',
      price: priceCents,
      trafficResetPrice: trafficResetPriceYuan,
      billingCycle: planForm.value.billingCycle,
      isActive: planForm.value.status !== 'inactive',
      isSoldOut: planForm.value.status === 'soldOut',
      sortOrder: planForm.value.sortOrder,
      slaGuarantee: planForm.value.slaGuarantee ?? undefined
    }

    if (editingPlan.value) {
      await api.packages.updatePlan(plansPackage.value.id, editingPlan.value.id, data)
      toast.success(t('resources.plans.updateSuccess'))
    } else {
      await api.packages.createPlan(plansPackage.value.id, data)
      toast.success(t('resources.plans.createSuccess'))
    }
    showPlanFormModal.value = false
    await loadPlans(plansPackage.value.id)
  } catch (err: any) {
    toast.error(translateError(err) || t('resources.plans.saveFailed'))
  } finally {
    planFormLoading.value = false
  }
}

async function deletePlan(plan: PackagePlan): Promise<void> {
  if (!plansPackage.value) return
  if (!confirm(t('resources.plans.confirmDelete', { name: plan.name }))) return

  try {
    await api.packages.deletePlan(plansPackage.value.id, plan.id)
    toast.success(t('resources.plans.deleteSuccess'))
    await loadPlans(plansPackage.value.id)
  } catch (err: any) {
    toast.error(err.message || t('resources.plans.deleteFailed'))
  }
}

function formatPrice(cents: number): string {
  return (cents / 100).toFixed(2)
}

function getBillingCycleLabel(months: number): string {
  const opt = billingCycleOptions.value.find(o => o.value === months)
  return opt ? opt.label : `${months} ${t('resources.plans.months')}`
}
</script>

<template>
  <div class="space-y-6">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('resources.packages.title')"
      :description="t('resources.packages.description')"
    >
      <template #actions>
        <Button variant="primary" @click="openCreate">
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2.5]" />
          </template>
          {{ t('resources.packages.create') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Toolbar / Search Box -->
    <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
      <div class="w-full sm:w-80">
        <Input
          v-model="searchQuery"
          :placeholder="t('resources.packages.searchPlaceholder')"
          size="md"
          clearable
          @input="onSearchChange"
          @clear="clearSearch"
        >
          <template #prefix>
            <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
          </template>
        </Input>
      </div>
    </div>

    <!-- Loading Skeleton -->
    <SkeletonLoader v-if="loading" type="list" />

    <!-- Empty State -->
    <Card v-else-if="filteredPackages.length === 0" padding="none">
      <EmptyState
        :title="searchQuery ? t('resources.packages.noSearchResults') : t('resources.packages.noPackages')"
        :description="searchQuery ? '' : t('resources.packages.noPackagesHint')"
      >
        <template #icon>
          <Layers class="w-7 h-7 stroke-[1.5]" />
        </template>
        <template v-if="searchQuery" #action>
          <Button variant="ghost" size="sm" @click="clearSearch">
            {{ t('resources.packages.clearSearch') }}
          </Button>
        </template>
        <template v-else #action>
          <Button variant="primary" size="sm" @click="openCreate">
            <template #icon>
              <Plus class="w-4 h-4" />
            </template>
            {{ t('resources.packages.create') }}
          </Button>
        </template>
      </EmptyState>
    </Card>

    <!-- Packages Card Table -->
    <template v-else>
      <Card padding="none" class="overflow-hidden">
        <!-- Mobile Cards (Guarded Container) -->
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="pkg in paginatedPackages"
            :key="pkg.id"
            class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
            :class="pkg.active !== 1 ? 'opacity-60' : ''"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="flex min-w-0 items-center gap-2">
                  <span class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ pkg.name }}</span>
                  <Badge
                    v-if="isPackagePublic(pkg)"
                    variant="info"
                    size="sm"
                  >
                    {{ t('resources.packages.publicBadge') }}
                  </Badge>
                </div>
                <p v-if="pkg.description" class="mt-1 line-clamp-2 text-xs text-[var(--text-secondary)]">{{ pkg.description }}</p>
              </div>
              <Badge v-if="pkg.active === 1" variant="success" size="sm">
                {{ t('admin.packages.active') }}
              </Badge>
              <Badge v-else variant="default" size="sm" :title="t('admin.packages.archivedHint')">
                {{ t('admin.packages.inactive') }}
              </Badge>
            </div>

            <dl class="mt-4 grid grid-cols-2 gap-3 text-sm">
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
                <dt class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('resources.packages.networkModeColumn') }}</dt>
                <dd class="mt-1 text-xs font-medium text-[var(--text-primary)]">{{ getPackageNetworkModeLabel(pkg) }}</dd>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
                <dt class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('resources.packages.instanceTypeColumn') }}</dt>
                <dd class="mt-1 text-xs font-medium text-[var(--text-primary)]">{{ getPackageInstanceTypeLabel(pkg) }}</dd>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
                <dt class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('resources.packages.trafficMultiplierColumn') }}</dt>
                <dd class="mt-1 font-mono text-xs font-medium text-[var(--text-primary)]" :title="getPackageTrafficMultiplierTitle(pkg)">{{ getPackageTrafficMultiplierLabel(pkg) }}</dd>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
                <dt class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('resources.packages.instanceColumn') }}</dt>
                <dd class="mt-1 font-mono text-xs font-medium text-[var(--text-primary)]">{{ pkg.instance_count || 0 }}</dd>
              </div>
              <div class="col-span-2 rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
                <dt class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ t('resources.packages.hostColumn') }}</dt>
                <dd class="mt-1 line-clamp-2 text-xs text-[var(--text-secondary)]">{{ getHostNames(pkg) }}</dd>
              </div>
            </dl>

            <!-- Action bar (Guarded) -->
            <div class="mt-4 flex flex-wrap items-center justify-end gap-2 border-t border-themed pt-3">
              <button
                v-if="!isAdminEntry"
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                :title="t('resources.packages.copyShareLink')"
                @click="copyShareLink(pkg)"
              >
                <Copy class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                :title="t('resources.plans.manage')"
                @click="openPlansModal(pkg)"
              >
                <Sliders class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                :title="t('quotaRelease.title')"
                @click="openQuotaReleaseModal(pkg)"
              >
                <Layers class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                :title="t('common.edit')"
                @click="openEdit(pkg)"
              >
                <Edit2 class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="inline-flex items-center justify-center px-3 py-1 text-xs font-medium rounded-full transition-all duration-200 active:scale-95 border"
                :class="isPackageActive(pkg)
                  ? 'text-[var(--warning)] border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,transparent)] hover:bg-[color-mix(in_srgb,var(--warning)_16%,transparent)]'
                  : 'text-[var(--success)] border-[color-mix(in_srgb,var(--success)_30%,transparent)] bg-[color-mix(in_srgb,var(--success)_10%,transparent)] hover:bg-[color-mix(in_srgb,var(--success)_16%,transparent)]'"
                :disabled="activeChangingId === pkg.id"
                :title="isPackageActive(pkg) ? t('admin.packages.deactivate') : t('admin.packages.activate')"
                @click="togglePackageActive(pkg)"
              >
                <span v-if="activeChangingId === pkg.id" class="w-3 h-3 border-2 border-current border-t-transparent rounded-full animate-spin mr-1"></span>
                {{ activeChangingId === pkg.id ? t('common.loading') : (isPackageActive(pkg) ? t('admin.packages.deactivate') : t('admin.packages.activate')) }}
              </button>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,transparent)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                :title="t('common.delete')"
                @click="deletePackage(pkg)"
              >
                <Trash2 class="w-3.5 h-3.5" />
              </button>
            </div>
          </div>
        </div>

        <!-- Desktop Fixed Table (Guarded Container) -->
        <div class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed text-sm">
            <thead>
              <tr class="border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <th class="w-[26%] text-left px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('admin.packages.name') }}</th>
                <th class="w-[8%] text-left px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('admin.packages.status') }}</th>
                <th class="w-[11%] text-left px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('resources.packages.networkModeColumn') }}</th>
                <th class="w-[8%] text-left px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('resources.packages.instanceTypeColumn') }}</th>
                <th class="w-[6%] text-right px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('resources.packages.trafficMultiplierColumn') }}</th>
                <th class="w-[13%] text-left px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('resources.packages.hostColumn') }}</th>
                <th class="w-[8%] text-right px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('resources.packages.instanceColumn') }}</th>
                <th class="w-[20%] text-right px-4 py-3.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="pkg in paginatedPackages"
                :key="pkg.id"
                class="border-b border-[var(--border-color)] transition-colors duration-150 hover:bg-[var(--bg-surface-soft)]"
                :class="pkg.active !== 1 ? 'opacity-60' : ''"
              >
                <!-- 套餐名称 -->
                <td class="px-4 py-3.5">
                  <div class="flex items-center gap-2 min-w-0">
                    <span class="font-semibold truncate text-[var(--text-primary)]">{{ pkg.name }}</span>
                    <Badge
                      v-if="isPackagePublic(pkg)"
                      variant="info"
                      size="sm"
                    >
                      {{ t('resources.packages.publicBadge') }}
                    </Badge>
                  </div>
                  <div v-if="pkg.description" class="text-xs text-[var(--text-tertiary)] mt-0.5 max-w-xs truncate">{{ pkg.description }}</div>
                </td>
                <!-- 状态 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <Badge v-if="pkg.active === 1" variant="success" size="sm">{{ t('admin.packages.active') }}</Badge>
                  <Badge v-else variant="default" size="sm" :title="t('admin.packages.archivedHint')">{{ t('admin.packages.inactive') }}</Badge>
                </td>
                <!-- 网络模式 -->
                <td class="px-4 py-3.5 text-[var(--text-secondary)]">
                  <div class="truncate text-xs font-medium">{{ getPackageNetworkModeLabel(pkg) }}</div>
                </td>
                <!-- 实例类型 -->
                <td class="px-4 py-3.5 text-[var(--text-secondary)]">
                  <div class="truncate text-xs font-medium">{{ getPackageInstanceTypeLabel(pkg) }}</div>
                </td>
                <!-- 流量倍率 -->
                <td
                  class="px-4 py-3.5 text-right font-mono text-xs whitespace-nowrap text-[var(--text-secondary)]"
                  :title="getPackageTrafficMultiplierTitle(pkg)"
                >
                  {{ getPackageTrafficMultiplierLabel(pkg) }}
                </td>
                <!-- 绑定宿主机 -->
                <td class="px-4 py-3.5">
                  <span class="block truncate text-xs text-[var(--text-tertiary)]" :title="getHostNames(pkg)">{{ getHostNames(pkg) }}</span>
                </td>
                <!-- 套餐下实例数 -->
                <td class="px-4 py-3.5 text-right font-mono text-xs whitespace-nowrap text-[var(--text-secondary)]">
                  {{ pkg.instance_count || 0 }}
                </td>
                <!-- 操作 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <div class="flex items-center justify-end gap-1.5">
                    <!-- 分享链接 -->
                    <button
                      v-if="!isAdminEntry"
                      type="button"
                      class="inline-flex items-center justify-center w-7 h-7 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-transparent hover:border-[var(--border-color)] transition-all duration-200 active:scale-95"
                      :title="t('resources.packages.copyShareLink')"
                      @click="copyShareLink(pkg)"
                    >
                      <Copy class="w-3.5 h-3.5" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-7 h-7 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-transparent hover:border-[var(--border-color)] transition-all duration-200 active:scale-95"
                      :title="t('resources.plans.manage')"
                      @click="openPlansModal(pkg)"
                    >
                      <Sliders class="w-3.5 h-3.5" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-7 h-7 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-transparent hover:border-[var(--border-color)] transition-all duration-200 active:scale-95"
                      :title="t('quotaRelease.title')"
                      @click="openQuotaReleaseModal(pkg)"
                    >
                      <Layers class="w-3.5 h-3.5" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-7 h-7 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-transparent hover:border-[var(--border-color)] transition-all duration-200 active:scale-95"
                      :title="t('common.edit')"
                      @click="openEdit(pkg)"
                    >
                      <Edit2 class="w-3.5 h-3.5" />
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center px-2.5 py-1 text-xs font-medium rounded-full transition-all duration-200 active:scale-95 border"
                      :class="isPackageActive(pkg)
                        ? 'text-[var(--warning)] border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,transparent)] hover:bg-[color-mix(in_srgb,var(--warning)_16%,transparent)]'
                        : 'text-[var(--success)] border-[color-mix(in_srgb,var(--success)_30%,transparent)] bg-[color-mix(in_srgb,var(--success)_10%,transparent)] hover:bg-[color-mix(in_srgb,var(--success)_16%,transparent)]'"
                      :disabled="activeChangingId === pkg.id"
                      :title="isPackageActive(pkg) ? t('admin.packages.deactivate') : t('admin.packages.activate')"
                      @click="togglePackageActive(pkg)"
                    >
                      <span v-if="activeChangingId === pkg.id" class="w-3 h-3 border-2 border-current border-t-transparent rounded-full animate-spin mr-1"></span>
                      {{ activeChangingId === pkg.id ? t('common.loading') : (isPackageActive(pkg) ? t('admin.packages.deactivate') : t('admin.packages.activate')) }}
                    </button>
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-7 h-7 rounded-full text-[var(--text-secondary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,transparent)] border border-transparent hover:border-[var(--border-color)] transition-all duration-200 active:scale-95"
                      :title="t('common.delete')"
                      @click="deletePackage(pkg)"
                    >
                      <Trash2 class="w-3.5 h-3.5" />
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 分页控件 -->
        <div v-if="filteredPackages.length > packagesPageSize" class="border-t border-[var(--border-color)] px-4 py-3">
          <Pagination
            v-model:page="packagesPage"
            :total="filteredPackages.length"
            :page-size="packagesPageSize"
          >
            <template #total>
              <span class="text-xs text-[var(--text-secondary)]">
                {{ t('common.total') }} {{ filteredPackages.length }} {{ t('common.items') }}
              </span>
            </template>
          </Pagination>
        </div>
      </Card>
    </template>

    <!-- 配额释放弹窗 -->
    <PackageQuotaReleaseModal
      :visible="showQuotaReleaseModal"
      :package-id="quotaReleasePackage?.id || 0"
      :package-name="quotaReleasePackage?.name || ''"
      @close="showQuotaReleaseModal = false"
      @success="handleQuotaReleaseSuccess"
    />

    <!-- 方案管理弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showPlansModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showPlansModal = false"></div>
          <div class="modal-content max-w-3xl">
            <div class="modal-header flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)]">
              <div class="flex items-center gap-3">
                <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_25%,var(--bg-surface))]">
                  <Sliders class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <div class="flex items-center gap-2">
                    <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('resources.plans.title') }}</h3>
                    <Badge v-if="plans.length > 0" variant="default" size="sm">
                      {{ plans.length }}
                    </Badge>
                  </div>
                  <p v-if="plansPackage" class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ plansPackage.name }} · {{ plansPackage.cpu_max }}% CPU · {{ formatMemory(plansPackage.memory_max) }}
                  </p>
                </div>
              </div>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                @click="showPlansModal = false"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>

            <div class="modal-body flex-1 overflow-y-auto">
              <SkeletonLoader v-if="plansLoading" type="list" />
              <div v-else-if="plans.length === 0" class="py-12">
                <EmptyState
                  :title="t('resources.plans.noPlans')"
                  :description="t('resources.plans.noPlansHint')"
                >
                  <template #icon>
                    <Sliders class="w-7 h-7 stroke-[1.5]" />
                  </template>
                  <template #action>
                    <Button variant="primary" size="sm" @click="openCreatePlanModal">
                      <template #icon>
                        <Plus class="w-4 h-4" />
                      </template>
                      {{ t('resources.plans.add') }}
                    </Button>
                  </template>
                </EmptyState>
              </div>
              <div v-else class="space-y-3 p-6">
                <div
                  v-for="plan in plans"
                  :key="plan.id"
                  class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)] transition-all duration-200 hover:border-[var(--border-hover)]"
                  :class="getPlanStatus(plan) !== 'active' ? 'opacity-70' : ''"
                >
                  <div class="flex items-start justify-between gap-4">
                    <div class="flex-1 min-w-0">
                      <div class="flex items-center gap-2 mb-1">
                        <span class="font-semibold text-sm text-[var(--text-primary)]">{{ plan.name }}</span>
                        <Badge :variant="getPlanBadgeVariant(plan)" size="sm">{{ getPlanStatusLabel(plan) }}</Badge>
                      </div>
                      <p v-if="plan.description" class="text-xs text-[var(--text-secondary)] mb-3">{{ plan.description }}</p>
                      <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs">
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('admin.packages.cpu') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ plan.cpu }}%</span>
                        </div>
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('admin.packages.memory') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ formatMemory(plan.memory) }}</span>
                        </div>
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('admin.packages.disk') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ formatDisk(plan.disk) }}</span>
                        </div>
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('resources.plans.price') }}</span>
                          <span class="font-semibold text-[var(--success)] mt-0.5 block">¥{{ formatPrice(plan.price) }}/{{ getBillingCycleLabel(plan.billingCycle) }}</span>
                        </div>
                      </div>
                      <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs mt-2">
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('resources.plans.portLimit') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ plan.portLimit }}</span>
                        </div>
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('resources.plans.snapshotLimit') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ plan.snapshotLimit }}</span>
                        </div>
                        <div v-if="plansPackage?.instance_type !== 'vm'" class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('resources.plans.swapSize') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ plan.swapSize > 0 ? `${plan.swapSize} MB` : '-' }}</span>
                        </div>
                        <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2 border border-[var(--border-color)]">
                          <span class="text-[11px] uppercase tracking-wide text-[var(--text-tertiary)] block">{{ t('resources.plans.siteLimit') }}</span>
                          <span class="font-mono font-medium text-[var(--text-primary)] mt-0.5 block">{{ plan.siteLimit }}</span>
                        </div>
                      </div>
                      <div v-if="plan.slaGuarantee !== null" class="mt-2 text-xs">
                        <span class="text-[var(--text-secondary)]">SLA保证:</span>
                        <span class="ml-1 font-semibold text-[var(--success)]">{{ plan.slaGuarantee }}%</span>
                      </div>
                    </div>
                    <div class="flex items-center gap-1.5 flex-shrink-0">
                      <button
                        type="button"
                        class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--accent)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                        :title="t('common.edit')"
                        @click="openEditPlanModal(plan)"
                      >
                        <Edit2 class="w-3.5 h-3.5" />
                      </button>
                      <button
                        type="button"
                        class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,transparent)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                        :title="t('common.delete')"
                        @click="deletePlan(plan)"
                      >
                        <Trash2 class="w-3.5 h-3.5" />
                      </button>
                    </div>
                  </div>
                </div>
              </div>
            </div>

            <div class="modal-footer flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
              <Button variant="secondary" @click="showPlansModal = false">{{ t('common.close') }}</Button>
              <Button variant="primary" @click="openCreatePlanModal">
                <template #icon>
                  <Plus class="w-4 h-4 stroke-[2.5]" />
                </template>
                {{ t('resources.plans.add') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 方案表单弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showPlanFormModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showPlanFormModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)]">
              <div class="flex items-center gap-3">
                <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_25%,var(--bg-surface))]">
                  <Sliders class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                    {{ editingPlan ? t('resources.plans.edit') : t('resources.plans.create') }}
                  </h3>
                  <p v-if="plansPackage" class="text-xs text-[var(--text-secondary)] mt-0.5">{{ plansPackage.name }}</p>
                </div>
              </div>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
                @click="showPlanFormModal = false"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>

            <div class="modal-body space-y-4 max-h-[70vh] overflow-y-auto p-6">
              <!-- 基本信息 -->
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <GpuSharingFields
                  v-if="plansPackage"
                  v-model="planForm.gpuShared"
                  v-model:device-ids="planForm.gpuDeviceIds"
                  class="sm:col-span-2"
                  :host-ids="plansPackage.host_ids || []"
                  :allowed="plansPackage.instance_type !== 'vm' && !plansPackage.privileged"
                  nullable
                />
                <div>
                  <Input
                    v-model="planForm.name"
                    :label="t('resources.plans.name') + ' *'"
                    :placeholder="t('resources.plans.namePlaceholder')"
                    size="sm"
                  />
                </div>
                <div>
                  <Input
                    v-model="planForm.description"
                    :label="t('resources.plans.description')"
                    :placeholder="t('resources.plans.descriptionPlaceholder')"
                    size="sm"
                  />
                </div>
              </div>
              <p class="text-xs text-[var(--warning)] -mt-2">{{ t('common.noIncudalHint') }}</p>

              <!-- 资源配置 -->
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4">
                <h4 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">{{ t('resources.plans.resourceConfig') }}</h4>
                <div class="grid grid-cols-2 sm:grid-cols-3 gap-3">
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">CPU (%)</label>
                    <input
                      v-model.number="planForm.cpu"
                      type="number"
                      min="15"
                      max="10000"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('admin.packages.memory') }} (MB)</label>
                    <input
                      v-model.number="planForm.memory"
                      type="number"
                      min="128"
                      max="62144"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('admin.packages.disk') }} (MB)</label>
                    <input
                      v-model.number="planForm.disk"
                      type="number"
                      min="512"
                      max="104857600"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.portLimit') }}</label>
                    <input
                      v-model.number="planForm.portLimit"
                      type="number"
                      min="0"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.snapshotLimit') }}</label>
                    <input
                      v-model.number="planForm.snapshotLimit"
                      type="number"
                      min="0"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div v-if="plansPackage?.instance_type !== 'vm'">
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.swapSize') }} (MB)</label>
                    <input
                      v-model.number="planForm.swapSize"
                      type="number"
                      min="0"
                      max="1048576"
                      step="128"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.siteLimit') }}</label>
                    <input
                      v-model.number="planForm.siteLimit"
                      type="number"
                      min="0"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.trafficLimit') }} (GB) *</label>
                    <input
                      v-model="planForm.trafficLimit"
                      type="number"
                      min="1"
                      max="100000"
                      step="1"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                      required
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.trafficSpeed') }} (Mbps) *</label>
                    <input
                      v-model.number="planForm.trafficLimitSpeed"
                      type="number"
                      min="1"
                      max="10000"
                      step="1"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                      required
                    />
                  </div>
                </div>
              </div>

              <!-- 计费配置 -->
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4">
                <h4 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">{{ t('resources.plans.billingConfig') }}</h4>
                <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">{{ t('resources.plans.price') }} (元)</label>
                    <input
                      v-model.number="planForm.price"
                      type="number"
                      min="0"
                      :max="MAX_PACKAGE_PLAN_PRICE"
                      step="0.01"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                    />
                    <p class="mt-1 text-xs text-[var(--text-tertiary)]">{{ t('resources.plans.priceRangeHint', { max: MAX_PACKAGE_PLAN_PRICE.toFixed(2) }) }}</p>
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">流量重置价格（元）</label>
                    <input
                      v-model.number="planForm.trafficResetPrice"
                      type="number"
                      min="0"
                      :max="MAX_PACKAGE_PLAN_PRICE"
                      step="0.01"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                      placeholder="继承套餐默认"
                    />
                    <p class="mt-1 text-xs text-[var(--text-tertiary)]">单位:元(如填 5 表示 5 元)；留空继承套餐默认价，填写后覆盖。</p>
                  </div>
                  <div>
                    <Select
                      :model-value="planForm.billingCycle"
                      :options="billingCycleOptions"
                      :label="t('resources.plans.billingCycle')"
                      size="sm"
                      @update:model-value="planForm.billingCycle = Number($event)"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1">SLA保证 (%)</label>
                    <input
                      v-model.number="planForm.slaGuarantee"
                      type="number"
                      min="1"
                      max="100"
                      step="0.01"
                      class="w-full rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 py-2 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                      placeholder="如 99.9"
                    />
                  </div>
                </div>
              </div>

              <!-- 状态与排序 -->
              <div class="grid gap-4 sm:grid-cols-3 sm:items-start">
                <div class="min-w-0 sm:col-span-2">
                  <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5">{{ t('resources.plans.status') }}</label>
                  <div class="h-10 rounded-full border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-1 flex items-center gap-1">
                    <button
                      v-for="option in planStatusOptions"
                      :key="option.value"
                      type="button"
                      class="flex-1 h-full min-w-0 flex items-center justify-center gap-1.5 rounded-full px-2 text-xs font-medium transition-all duration-200 active:scale-95"
                      :class="planForm.status === option.value
                        ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold border border-[var(--border-color)]'
                        : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                      :aria-pressed="planForm.status === option.value"
                      @click="planForm.status = option.value"
                    >
                      <span
                        class="h-2 w-2 flex-shrink-0 rounded-full"
                        :class="option.value === 'active' ? 'bg-[var(--success)]' : option.value === 'soldOut' ? 'bg-[var(--warning)]' : 'bg-[var(--text-tertiary)]'"
                      ></span>
                      <span class="truncate">{{ option.label }}</span>
                    </button>
                  </div>
                  <p class="mt-2 min-h-[2rem] text-xs leading-5 text-[var(--text-tertiary)]">
                    {{ selectedPlanStatusOption.description }}
                  </p>
                </div>
                <div class="min-w-0">
                  <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5">{{ t('resources.plans.sortOrder') }}</label>
                  <input
                    v-model.number="planForm.sortOrder"
                    type="number"
                    min="0"
                    class="w-full h-10 rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-3 text-center text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 focus:border-[var(--accent)]"
                  />
                </div>
              </div>
            </div>

            <div class="modal-footer flex items-center justify-end gap-3 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
              <Button variant="secondary" @click="showPlanFormModal = false">{{ t('common.cancel') }}</Button>
              <Button
                variant="primary"
                :disabled="!planForm.name.trim() || planFormLoading"
                :loading="planFormLoading"
                @click="savePlan"
              >
                <template #icon>
                  <Check class="w-4 h-4 stroke-[2.5]" />
                </template>
                {{ t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 套餐创建/编辑抽屉 -->
    <PackageFormView
      v-model:show="showPackageDrawer"
      :package-id="editingPackageId"
      @saved="loadPackages"
    />
  </div>
</template>

<style scoped>
/* Linear finish: uppercase micro-label table headers (consistent with MyHostsView) */
thead th {
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

@media (prefers-reduced-motion: reduce) {
  * {
    animation-duration: 0.001ms !important;
    transition-duration: 0.001ms !important;
  }
}
</style>
