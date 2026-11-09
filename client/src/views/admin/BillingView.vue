<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, watch, defineAsyncComponent } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api, {
  type FinancialReconciliationItem,
  type FinancialReconciliationItemType,
  type FinancialReconciliationRun,
  type FinancialReconciliationStatus
} from '@/api/admin'
import { useToast } from '@/stores/toast'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import LoadingSpinner from '@/components/LoadingSpinner.vue'
import { RefreshCw } from 'lucide-vue-next'
import BillingOverviewIcon from '@/components/admin/BillingOverviewIcon.vue'
import { calculateDiscountedPrice } from '@/utils/billing'
import { instanceDetailPath } from '@/utils/app-paths'
import { PageHeader, Card, Button, Input, Select, Badge, EmptyState, Pagination } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'

const PaymentProvidersView = defineAsyncComponent(() => import('@/views/admin/PaymentProvidersView.vue'))
const AffReviewView = defineAsyncComponent(() => import('@/views/admin/AffReviewView.vue'))

type BatchPriceUpdateItemStatus = 'ready' | 'unchanged' | 'failed'

interface BatchPriceUpdatePreview {
  summary: {
    selectedCount: number
    validCount: number
    changedCount: number
    unchangedCount: number
    failedCount: number
    totalCharge: number
    totalRefund: number
    netAmount: number
  }
  canSubmit: boolean
  items: Array<{
    id: number
    name: string | null
    user: { id: number; username: string; balance: number } | null
    oldPrice: number | null
    newPrice: number
    billingCycle: number | null
    remainingDays: number
    priceDiff: number
    discountRate: number
    instanceVersion?: number
    status: BatchPriceUpdateItemStatus
    error?: string
  }>
  userImpacts: Array<{
    userId: number
    username: string
    balanceBefore: number
    balanceAfter: number
    totalCharge: number
    totalRefund: number
    netDiff: number
    insufficientBalance: boolean
  }>
}

const router = useRouter()
const route = useRoute()

const { t } = useI18n()
const toast = useToast()

// Tab 切换
type BillingTab = 'overview' | 'instances' | 'records' | 'rechargeRecords' | 'reconciliation' | 'affConversions' | 'paymentProviders'
const billingTabs: BillingTab[] = ['overview', 'instances', 'records', 'rechargeRecords', 'reconciliation', 'affConversions', 'paymentProviders']
function normalizeTab(tab: unknown): BillingTab {
  return typeof tab === 'string' && billingTabs.includes(tab as BillingTab)
    ? tab as BillingTab
    : 'overview'
}

const activeTab = ref<BillingTab>(normalizeTab(route.query.tab))
const hasVisitedPaymentProviders = ref(activeTab.value === 'paymentProviders')
const hasVisitedAffConversions = ref(activeTab.value === 'affConversions')

// 分页大小选项
const pageSizeOptions = [10, 20, 50, 100]

// 宿主机列表（用于筛选）
const hosts = ref<Array<{ id: number; name: string }>>([])

type OverviewIconName = 'wallet' | 'chart' | 'store' | 'cloud' | 'server' | 'pulse' | 'ban' | 'clock' | 'sparkles'

interface BillingRevenueBucket {
  totalAmount: number
  thisMonthAmount: number
  todayAmount: number
}

interface BillingOverview {
  totalRevenue: number
  thisMonthRevenue: number
  lastMonthRevenue: number
  todayRevenue: number
  totalRefunds: number
  netRevenue: number
  paidInstancesCount: number
  activePaidInstancesCount: number
  suspendedCount: number
  expiringCount: number
  revenueMix: {
    direct: BillingRevenueBucket
  }
  recharge: {
    totalAmount: number
    totalCount: number
    thisMonthAmount: number
    thisMonthCount: number
    todayAmount: number
    todayCount: number
  }
  aff: {
    totalCommission: number
    totalOrders: number
    thisMonthCommission: number
    totalConverted: number
    pendingConvertCount: number
  }
}

interface PeriodComparisonCard {
  key: string
  label: string
  revenueAmount: number
  rechargeAmount: number
  rechargeCount: number
  badgeClass: string
  iconClass: string
}

interface RevenueMixRow {
  key: string
  label: string
  directAmount: number
  directRatio: number
}

interface MetricCard {
  key: string
  label: string
  value: number
  format: 'money' | 'count'
  icon: OverviewIconName
  toneClass: string
  surfaceClass: string
  progress?: number
}

// 概览数据
const overview = ref<BillingOverview | null>(null)
const overviewLoading = ref(true)

// 付费实例列表
const instances = ref<any[]>([])
const instancesLoading = ref(false)
const instancesPage = ref(1)
const instancesPageSize = ref(100)
const instancesTotal = ref(0)
const instancesFilter = ref('')
const instancesHostFilter = ref<number | ''>('')
const instancesSearch = ref('')
const showExpiring = ref(false)
const showDateColumns = ref(false)  // 默认隐藏购买日期和到期时间列
const selectedInstanceIds = ref<number[]>([])

// 扣费记录
const records = ref<any[]>([])
const recordsLoading = ref(false)
const recordsPage = ref(1)
const recordsPageSize = ref(100)
const recordsTotal = ref(0)

// 充值记录
const rechargeRecords = ref<any[]>([])
const rechargeRecordsLoading = ref(false)
const rechargeRecordsPage = ref(1)
const rechargeRecordsPageSize = ref(100)
const rechargeRecordsTotal = ref(0)
const rechargeRecordsFilter = ref('')
const syncingRecordId = ref<number | null>(null)

const reconciliationDate = ref(new Date().toISOString().slice(0, 10))
const reconciliation = ref<FinancialReconciliationRun | null>(null)
const reconciliationLoading = ref(false)
const reconciliationRunning = ref(false)
const reconciliationSavingId = ref<number | null>(null)
const reconciliationExporting = ref<string | null>(null)
const reconciliationNotes = ref<Record<number, string>>({})
const reconciliationStatusDrafts = ref<Record<number, FinancialReconciliationStatus>>({})

// 实例操作下拉菜单状态
const activeInstanceMenuId = ref<number | null>(null)
const instanceMenuPosition = ref<{ top: number; right: number }>({ top: 0, right: 0 })

const activeMenuInstance = computed(() => {
  if (!activeInstanceMenuId.value) return null
  return instances.value.find(i => i.id === activeInstanceMenuId.value) || null
})

function toggleInstanceMenu(instanceId: number, e?: Event) {
  if (e) {
    e.stopPropagation()
  }
  if (activeInstanceMenuId.value === instanceId) {
    activeInstanceMenuId.value = null
    return
  }
  if (e) {
    const btn = (e.currentTarget || e.target) as HTMLElement | null
    if (btn && typeof btn.getBoundingClientRect === 'function') {
      const rect = btn.getBoundingClientRect()
      const spaceBelow = window.innerHeight - rect.bottom
      const menuHeight = 220
      if (spaceBelow < menuHeight && rect.top > menuHeight) {
        instanceMenuPosition.value = {
          top: Math.max(8, rect.top - menuHeight - 4),
          right: Math.max(12, window.innerWidth - rect.right)
        }
      } else {
        instanceMenuPosition.value = {
          top: rect.bottom + 4,
          right: Math.max(12, window.innerWidth - rect.right)
        }
      }
    }
  }
  activeInstanceMenuId.value = instanceId
}

function closeInstanceMenu() {
  activeInstanceMenuId.value = null
}

function handleGlobalClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (target && !target.closest('.billing-action-menu-container') && !target.closest('.kawaii-menu-panel')) {
    activeInstanceMenuId.value = null
  }
}

// 操作弹窗
const showActionModal = ref(false)
const actionType = ref<'suspend' | 'unsuspend' | 'extend' | 'deleteRefund' | 'deleteRefundDatabaseOnly' | 'applyDiscount'>('suspend')
const actionTarget = ref<any>(null)
const actionLoading = ref(false)
const actionForm = ref({
  reason: '',
  days: 7,
  freeExtend: false,
  amount: 0,
  refundType: 'remaining' as 'remaining' | 'full',
  affCode: ''
})

// 修改价格弹窗
const showPriceModal = ref(false)
const priceTarget = ref<any>(null)
const priceLoading = ref(false)
const pricePreviewLoading = ref(false)
const pricePreview = ref<{
  oldPrice: number
  newPrice: number
  billingCycle: number
  remainingDays: number
  priceDiff: number
  discountRate: number
  userBalance: number
  instanceVersion: number
} | null>(null)
const priceForm = ref({
  newPrice: 0,
  settleBalance: true
})

// 批量修改价格弹窗
const showBatchPriceModal = ref(false)
const batchPriceLoading = ref(false)
const batchPricePreviewLoading = ref(false)
const batchPricePreview = ref<BatchPriceUpdatePreview | null>(null)
const batchPriceForm = ref({
  newPrice: 0,
  settleBalance: true
})

function isValidPriceInput(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

// 计算实际续费价格（应用 AFF 折扣后）
const actualRenewPrice = computed(() => {
  if (!priceTarget.value) return { old: 0, new: 0 }
  const inst = priceTarget.value
  const discountRate = inst.affDiscountRate || 0
  const oldPrice = inst.billingPrice || 0
  const newPrice = priceForm.value.newPrice
  
  return {
    old: calculateDiscountedPrice(oldPrice, discountRate),
    new: calculateDiscountedPrice(newPrice, discountRate)
  }
})

// 计算价格差价（使用公共方法，确保与后端一致）
const priceDiff = computed(() => {
  return pricePreview.value?.priceDiff || 0
})

// 切换方案弹窗
const showUpgradeModal = ref(false)
const upgradeTarget = ref<any>(null)
const upgradeLoading = ref(false)
const upgradeSubmitting = ref(false)
const upgradeData = ref<{
  currentPlan: {
    id: number
    name: string
    price: number
    billingCycle: number
    monthlyPrice: number
    cpu: number
    memory: number
    disk: number
  } | null
  remainingDays: number
  availablePlans: Array<{
    id: number
    name: string
    description: string | null
    price: number
    billingCycle: number
    monthlyPrice: number
    priceDiff: number
    cpu: number
    memory: number
    disk: number
    canUpgrade?: boolean
    cannotUpgradeReason?: string | null
    resourceWarnings?: string[] | null
  }>
  userBalance: number
}>({
  currentPlan: null,
  remainingDays: 0,
  availablePlans: [],
  userBalance: 0
})
const selectedPlanId = ref<number | null>(null)

// 计算选中方案的补差价
const upgradePriceDiff = computed(() => {
  if (!upgradeData.value.currentPlan || !selectedPlanId.value) return 0
  const newPlan = upgradeData.value.availablePlans.find(p => p.id === selectedPlanId.value)
  if (!newPlan) return 0
  return newPlan.priceDiff
})

const selectedUpgradePlan = computed(() => {
  if (!selectedPlanId.value) return null
  return upgradeData.value.availablePlans.find(p => p.id === selectedPlanId.value) || null
})

const selectedUpgradePlanBlocked = computed(() => selectedUpgradePlan.value?.canUpgrade === false)

const selectedInstanceIdSet = computed(() => new Set(selectedInstanceIds.value))

const selectableInstances = computed(() => {
  return instances.value.filter(inst => inst.status !== 'deleted')
})

const selectedInstances = computed(() => {
  const selectedIds = selectedInstanceIdSet.value
  return instances.value.filter(inst => selectedIds.has(inst.id))
})

const selectedInstancesCount = computed(() => selectedInstanceIds.value.length)

const allCurrentPageSelected = computed(() => {
  return selectableInstances.value.length > 0 &&
    selectableInstances.value.every(inst => selectedInstanceIdSet.value.has(inst.id))
})

const someCurrentPageSelected = computed(() => {
  return selectableInstances.value.some(inst => selectedInstanceIdSet.value.has(inst.id))
})

const selectedPriceBaseline = computed(() => {
  if (selectedInstances.value.length === 0) return 0
  const firstSelected = selectedInstances.value.find(inst => typeof inst.billingPrice === 'number')
  return firstSelected?.billingPrice || 0
})

const batchPriceExpectations = computed(() => {
  return batchPricePreview.value?.items
    .filter(item => typeof item.instanceVersion === 'number')
    .map(item => ({ instanceId: item.id, version: item.instanceVersion! })) || []
})

const batchPricePreviewHasVersionSnapshot = computed(() => {
  if (!batchPricePreview.value) return false
  return batchPricePreview.value.items.every(item => item.status === 'failed' || typeof item.instanceVersion === 'number')
})

const periodComparisonCards = computed<PeriodComparisonCard[]>(() => {
  if (!overview.value) return []

  return [
    {
      key: 'total',
      label: t('admin.billing.overviewPeriods.total'),
      revenueAmount: overview.value.totalRevenue,
      rechargeAmount: overview.value.recharge.totalAmount,
      rechargeCount: overview.value.recharge.totalCount,
      badgeClass: 'bg-themed-tertiary text-themed',
      iconClass: 'text-slate-600 dark:text-slate-300'
    },
    {
      key: 'thisMonth',
      label: t('admin.billing.overviewPeriods.thisMonth'),
      revenueAmount: overview.value.thisMonthRevenue,
      rechargeAmount: overview.value.recharge.thisMonthAmount,
      rechargeCount: overview.value.recharge.thisMonthCount,
      badgeClass: 'bg-themed-tertiary text-themed',
      iconClass: 'text-slate-600 dark:text-slate-300'
    },
    {
      key: 'today',
      label: t('admin.billing.overviewPeriods.today'),
      revenueAmount: overview.value.todayRevenue,
      rechargeAmount: overview.value.recharge.todayAmount,
      rechargeCount: overview.value.recharge.todayCount,
      badgeClass: 'bg-themed-tertiary text-themed',
      iconClass: 'text-slate-600 dark:text-slate-300'
    }
  ]
})

const revenueMixRows = computed<RevenueMixRow[]>(() => {
  if (!overview.value) return []

  const direct = overview.value.revenueMix.direct

  return [
    {
      key: 'total',
      label: t('admin.billing.overviewPeriods.total'),
      directAmount: direct.totalAmount,
      directRatio: direct.totalAmount > 0 ? 100 : 0
    },
    {
      key: 'thisMonth',
      label: t('admin.billing.overviewPeriods.thisMonth'),
      directAmount: direct.thisMonthAmount,
      directRatio: direct.thisMonthAmount > 0 ? 100 : 0
    },
    {
      key: 'today',
      label: t('admin.billing.overviewPeriods.today'),
      directAmount: direct.todayAmount,
      directRatio: direct.todayAmount > 0 ? 100 : 0
    }
  ]
})

const instanceStatusCards = computed<MetricCard[]>(() => {
  if (!overview.value) return []

  return [
    {
      key: 'active',
      label: t('admin.billing.activeInstances'),
      value: overview.value.activePaidInstancesCount,
      format: 'count',
      icon: 'pulse',
      toneClass: 'text-emerald-600 dark:text-emerald-300',
      surfaceClass: 'bg-emerald-500/10 ring-1 ring-emerald-500/20',
      progress: calculateRatio(overview.value.activePaidInstancesCount, overview.value.paidInstancesCount)
    },
    {
      key: 'suspended',
      label: t('admin.billing.suspendedInstances'),
      value: overview.value.suspendedCount,
      format: 'count',
      icon: 'ban',
      toneClass: 'text-rose-600 dark:text-rose-300',
      surfaceClass: 'bg-rose-500/10 ring-1 ring-rose-500/20',
      progress: calculateRatio(overview.value.suspendedCount, overview.value.paidInstancesCount)
    },
    {
      key: 'expiring',
      label: t('admin.billing.expiringInstances'),
      value: overview.value.expiringCount,
      format: 'count',
      icon: 'clock',
      toneClass: 'text-amber-600 dark:text-amber-300',
      surfaceClass: 'bg-amber-500/10 ring-1 ring-amber-500/20',
      progress: calculateRatio(overview.value.expiringCount, overview.value.paidInstancesCount)
    }
  ]
})

const affCards = computed<MetricCard[]>(() => {
  if (!overview.value) return []

  return [
    {
      key: 'aff-total',
      label: t('admin.billing.totalAffCommission'),
      value: overview.value.aff.totalCommission,
      format: 'money',
      icon: 'sparkles',
      toneClass: 'text-slate-700 dark:text-slate-200',
      surfaceClass: 'bg-slate-500/10 ring-1 ring-slate-500/20'
    },
    {
      key: 'aff-month',
      label: t('admin.billing.thisMonthAff'),
      value: overview.value.aff.thisMonthCommission,
      format: 'money',
      icon: 'sparkles',
      toneClass: 'text-slate-700 dark:text-slate-200',
      surfaceClass: 'bg-slate-500/10 ring-1 ring-slate-500/20'
    },
    {
      key: 'aff-converted',
      label: t('admin.billing.affConverted'),
      value: overview.value.aff.totalConverted,
      format: 'money',
      icon: 'wallet',
      toneClass: 'text-slate-700 dark:text-slate-200',
      surfaceClass: 'bg-slate-500/10 ring-1 ring-slate-500/20'
    },
    {
      key: 'aff-pending',
      label: t('admin.billing.affPendingConvert'),
      value: overview.value.aff.pendingConvertCount,
      format: 'count',
      icon: 'clock',
      toneClass: 'text-amber-600 dark:text-amber-300',
      surfaceClass: 'bg-amber-500/10 ring-1 ring-amber-500/20'
    }
  ]
})

const refundRate = computed(() => {
  if (!overview.value) return 0
  return calculateRatio(overview.value.totalRefunds, overview.value.totalRevenue)
})

const activeInstanceShare = computed(() => {
  if (!overview.value) return 0
  return calculateRatio(overview.value.activePaidInstancesCount, overview.value.paidInstancesCount)
})

const monthlyRevenueChange = computed(() => {
  if (!overview.value) return null
  return calculateChange(overview.value.thisMonthRevenue, overview.value.lastMonthRevenue)
})

function loadTabData(tab: BillingTab) {
  if (tab === 'overview' && !overview.value) {
    loadOverview()
  }
  if (tab === 'instances' && instances.value.length === 0) {
    loadInstances()
  }
  if (tab === 'records' && records.value.length === 0) {
    loadRecords()
  }
  if (tab === 'rechargeRecords' && rechargeRecords.value.length === 0) {
    loadRechargeRecords()
  }
  if (tab === 'reconciliation' && !reconciliation.value) {
    loadFinancialReconciliation()
  }
}

onMounted(() => {
  document.addEventListener('click', handleGlobalClick)
  window.addEventListener('resize', closeInstanceMenu)
  window.addEventListener('scroll', closeInstanceMenu, true)
  loadTabData(activeTab.value)
})

onUnmounted(() => {
  document.removeEventListener('click', handleGlobalClick)
  window.removeEventListener('resize', closeInstanceMenu)
  window.removeEventListener('scroll', closeInstanceMenu, true)
})

async function loadOverview() {
  overviewLoading.value = true
  try {
    const res = await api.admin.getBillingOverview()
    overview.value = res.overview
  } catch (err: any) {
    toast.error(t('admin.billing.loadFailed') + ': ' + err.message)
  } finally {
    overviewLoading.value = false
  }
}

async function loadInstances() {
  instancesLoading.value = true
  try {
    const res = await api.admin.getBillingInstances({
      page: instancesPage.value,
      pageSize: instancesPageSize.value,
      status: instancesFilter.value || undefined,
      expiring: showExpiring.value || undefined,
      hostId: instancesHostFilter.value || undefined,
      search: instancesSearch.value.trim() || undefined
    })
    instances.value = res.instances || []
    instancesTotal.value = res.total
    const currentPageSelectableIds = new Set(instances.value.filter(inst => inst.status !== 'deleted').map(inst => inst.id))
    selectedInstanceIds.value = selectedInstanceIds.value.filter(id => currentPageSelectableIds.has(id))
    // 从返回中获取有付费实例的节点列表
    if (res.hosts) {
      hosts.value = res.hosts
    }
  } catch (err: any) {
    toast.error(t('admin.billing.loadInstancesFailed') + ': ' + err.message)
  } finally {
    instancesLoading.value = false
  }
}

async function loadRecords() {
  recordsLoading.value = true
  try {
    const res = await api.admin.getBillingRecords({
      page: recordsPage.value,
      pageSize: recordsPageSize.value
    })
    records.value = res.records || []
    recordsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('admin.billing.loadRecordsFailed') + ': ' + err.message)
  } finally {
    recordsLoading.value = false
  }
}

async function loadRechargeRecords() {
  rechargeRecordsLoading.value = true
  try {
    const res = await api.admin.getRechargeRecords({
      page: rechargeRecordsPage.value,
      pageSize: rechargeRecordsPageSize.value,
      status: rechargeRecordsFilter.value || undefined
    })
    rechargeRecords.value = res.records || []
    rechargeRecordsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('admin.billing.loadRechargeRecordsFailed') + ': ' + err.message)
  } finally {
    rechargeRecordsLoading.value = false
  }
}

async function loadFinancialReconciliation() {
  reconciliationLoading.value = true
  try {
    const res = await api.admin.getFinancialReconciliation({ date: reconciliationDate.value })
    reconciliation.value = res.run
    syncReconciliationDrafts()
  } catch (err: any) {
    toast.error('加载财务对账失败: ' + err.message)
  } finally {
    reconciliationLoading.value = false
  }
}

async function runFinancialReconciliation() {
  if (reconciliationRunning.value) return
  reconciliationRunning.value = true
  try {
    const res = await api.admin.runFinancialReconciliation(reconciliationDate.value)
    reconciliation.value = res.run
    syncReconciliationDrafts()
    toast.success('财务对账已完成')
  } catch (err: any) {
    toast.error('运行财务对账失败: ' + err.message)
  } finally {
    reconciliationRunning.value = false
  }
}

function syncReconciliationDrafts() {
  const notes: Record<number, string> = {}
  const statuses: Record<number, FinancialReconciliationStatus> = {}
  for (const item of reconciliation.value?.items || []) {
    notes[item.id] = item.note || ''
    statuses[item.id] = item.status
  }
  reconciliationNotes.value = notes
  reconciliationStatusDrafts.value = statuses
}

async function saveReconciliationItem(item: FinancialReconciliationItem) {
  if (reconciliationSavingId.value) return
  reconciliationSavingId.value = item.id
  try {
    const res = await api.admin.updateFinancialReconciliationItem(item.id, {
      status: reconciliationStatusDrafts.value[item.id] || item.status,
      note: reconciliationNotes.value[item.id] || null
    })
    if (reconciliation.value) {
      reconciliation.value.status = res.runStatus
      const index = reconciliation.value.items.findIndex(current => current.id === item.id)
      if (index >= 0) reconciliation.value.items[index] = res.item
    }
    syncReconciliationDrafts()
    toast.success('差异项已更新')
  } catch (err: any) {
    toast.error('更新差异项失败: ' + err.message)
  } finally {
    reconciliationSavingId.value = null
  }
}

async function exportFinancialReconciliation(type: 'orders' | 'balance_logs' | 'adjustments') {
  if (reconciliationExporting.value) return
  reconciliationExporting.value = type
  try {
    const blob = await api.admin.exportFinancialReconciliationCsv(reconciliationDate.value, type)
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `payincus-reconciliation-${reconciliationDate.value}-${type}.csv`
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
  } catch (err: any) {
    toast.error('导出失败: ' + err.message)
  } finally {
    reconciliationExporting.value = null
  }
}

async function syncRechargeRecord(record: any) {
  if (syncingRecordId.value) return
  
  syncingRecordId.value = record.id
  try {
    const res = await api.admin.syncRechargeRecord(record.id)
    if (res.synced) {
      toast.success(t('admin.billing.syncSuccess'))
      // 并行刷新充值记录和概览
      await Promise.all([loadRechargeRecords(), loadOverview()])
    } else {
      toast.info(res.message)
    }
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    syncingRecordId.value = null
  }
}

function switchTab(tab: BillingTab) {
  activeTab.value = tab
  if (tab === 'paymentProviders') {
    hasVisitedPaymentProviders.value = true
  }
  if (tab === 'affConversions') {
    hasVisitedAffConversions.value = true
  }
  router.replace({
    path: route.path,
    query: tab === 'overview'
      ? { ...route.query, tab: undefined }
      : { ...route.query, tab }
  })
  loadTabData(tab)
}

watch(
  () => route.query.tab,
  (tab) => {
    const nextTab = normalizeTab(tab)
    if (nextTab !== activeTab.value) {
      switchTab(nextTab)
    }
  }
)

function openActionModal(type: 'suspend' | 'unsuspend' | 'extend' | 'deleteRefund' | 'deleteRefundDatabaseOnly' | 'applyDiscount', instance: any) {
  closeInstanceMenu()
  actionType.value = type
  actionTarget.value = instance
  actionForm.value = {
    reason: '',
    days: 7,
    freeExtend: false,
    amount: 0,
    refundType: 'remaining',
    affCode: ''
  }
  showActionModal.value = true
}

async function executeAction() {
  if (!actionTarget.value) return
  
  actionLoading.value = true
  try {
    const id = actionTarget.value.id
    
    switch (actionType.value) {
      case 'suspend':
        await api.admin.suspendInstance(id, actionForm.value.reason)
        toast.success(t('admin.billing.suspendSuccess'))
        break
      case 'unsuspend':
        await api.admin.unsuspendInstance(id)
        toast.success(t('admin.billing.unsuspendSuccess'))
        break
      case 'extend':
        await api.admin.extendInstance(id, actionForm.value.days, actionForm.value.reason, actionForm.value.freeExtend)
        toast.success(t('admin.billing.extendSuccess'))
        break
      case 'deleteRefund':
        if (!actionForm.value.reason.trim()) {
          toast.error(t('admin.billing.deleteRefundReasonRequired'))
          actionLoading.value = false
          return
        }
        {
          const result = await api.admin.deleteAndRefundInstance(id, actionForm.value.refundType, actionForm.value.reason)
          toast.success(result.message || t('admin.billing.deleteRefundSuccess'))
        }
        break
      case 'deleteRefundDatabaseOnly':
        if (!actionForm.value.reason.trim()) {
          toast.error(t('admin.billing.deleteRefundReasonRequired'))
          actionLoading.value = false
          return
        }
        {
          const result = await api.admin.deleteAndRefundInstance(id, actionForm.value.refundType, actionForm.value.reason, true)
          toast.success(result.message || t('admin.billing.deleteRefundDatabaseOnlySuccess'))
        }
        break
      case 'applyDiscount':
        if (!actionForm.value.affCode.trim()) {
          toast.error(t('admin.billing.affCodeRequired'))
          actionLoading.value = false
          return
        }
        await api.admin.applyAffCode(id, actionForm.value.affCode.trim())
        toast.success(t('admin.billing.applyDiscountSuccess'))
        break
    }
    
    showActionModal.value = false
    // 并行刷新实例列表和概览
    await Promise.all([loadInstances(), loadOverview()])
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    actionLoading.value = false
  }
}

// 打开修改价格弹窗
function openPriceModal(instance: any) {
  closeInstanceMenu()
  priceTarget.value = instance
  pricePreviewRequestId += 1
  priceForm.value = {
    newPrice: instance.billingPrice || 0,
    settleBalance: true
  }
  pricePreview.value = null
  showPriceModal.value = true
}

let pricePreviewTimer: ReturnType<typeof setTimeout> | null = null
let pricePreviewRequestId = 0

async function loadPricePreview() {
  if (!showPriceModal.value || !priceTarget.value) return
  if (!isValidPriceInput(priceForm.value.newPrice)) {
    pricePreviewRequestId += 1
    pricePreview.value = null
    pricePreviewLoading.value = false
    return
  }

  const requestId = ++pricePreviewRequestId
  pricePreviewLoading.value = true
  try {
    const res = await api.admin.previewInstancePriceUpdate(
      priceTarget.value.id,
      priceForm.value.newPrice,
      priceForm.value.settleBalance
    )
    if (requestId !== pricePreviewRequestId) return
    pricePreview.value = res
  } catch (err: any) {
    if (requestId !== pricePreviewRequestId) return
    pricePreview.value = null
    toast.error(err.message)
  } finally {
    if (requestId === pricePreviewRequestId) {
      pricePreviewLoading.value = false
    }
  }
}

watch(
  () => [showPriceModal.value, priceTarget.value?.id, priceForm.value.newPrice, priceForm.value.settleBalance],
  ([visible]) => {
    if (pricePreviewTimer) {
      clearTimeout(pricePreviewTimer)
      pricePreviewTimer = null
    }

    if (!visible || !priceTarget.value) {
      if (!visible) {
        pricePreviewRequestId += 1
        pricePreviewLoading.value = false
      }
      if (!visible) {
        pricePreview.value = null
      }
      return
    }

    pricePreviewTimer = setTimeout(() => {
      loadPricePreview()
    }, 150)
  }
)

function toggleSelectAllCurrentPage() {
  const selected = new Set(selectedInstanceIds.value)
  if (allCurrentPageSelected.value) {
    for (const inst of selectableInstances.value) {
      selected.delete(inst.id)
    }
  } else {
    for (const inst of selectableInstances.value) {
      selected.add(inst.id)
    }
  }
  selectedInstanceIds.value = Array.from(selected)
}

function openBatchPriceModal() {
  if (selectedInstanceIds.value.length === 0) {
    toast.warning(t('admin.billing.batchPriceNoSelection'))
    return
  }

  batchPricePreviewRequestId += 1
  batchPriceForm.value = {
    newPrice: selectedPriceBaseline.value,
    settleBalance: true
  }
  batchPricePreview.value = null
  showBatchPriceModal.value = true
}

let batchPricePreviewTimer: ReturnType<typeof setTimeout> | null = null
let batchPricePreviewRequestId = 0

async function loadBatchPricePreview() {
  if (!showBatchPriceModal.value || selectedInstanceIds.value.length === 0) return
  if (!isValidPriceInput(batchPriceForm.value.newPrice)) {
    batchPricePreviewRequestId += 1
    batchPricePreview.value = null
    batchPricePreviewLoading.value = false
    return
  }

  const requestId = ++batchPricePreviewRequestId
  batchPricePreviewLoading.value = true
  try {
    const res = await api.admin.previewBatchInstancePriceUpdate(
      selectedInstanceIds.value,
      batchPriceForm.value.newPrice,
      batchPriceForm.value.settleBalance
    )
    if (requestId !== batchPricePreviewRequestId) return
    batchPricePreview.value = res
  } catch (err: any) {
    if (requestId !== batchPricePreviewRequestId) return
    batchPricePreview.value = err?.preview || null
    toast.error(err.message)
  } finally {
    if (requestId === batchPricePreviewRequestId) {
      batchPricePreviewLoading.value = false
    }
  }
}

watch(
  () => [showBatchPriceModal.value, selectedInstanceIds.value.join(','), batchPriceForm.value.newPrice, batchPriceForm.value.settleBalance],
  ([visible]) => {
    if (batchPricePreviewTimer) {
      clearTimeout(batchPricePreviewTimer)
      batchPricePreviewTimer = null
    }

    if (!visible || selectedInstanceIds.value.length === 0) {
      if (!visible) {
        batchPricePreviewRequestId += 1
        batchPricePreviewLoading.value = false
      }
      if (!visible) {
        batchPricePreview.value = null
      }
      return
    }

    batchPricePreviewTimer = setTimeout(() => {
      loadBatchPricePreview()
    }, 150)
  }
)

async function submitBatchUpdatePrice() {
  if (selectedInstanceIds.value.length === 0) return

  batchPriceLoading.value = true
  try {
    const res = await api.admin.updateBatchInstancePrice(
      selectedInstanceIds.value,
      batchPriceForm.value.newPrice,
      batchPriceForm.value.settleBalance,
      batchPriceExpectations.value
    )
    toast.success(res.message)
    batchPricePreview.value = res.preview
    showBatchPriceModal.value = false
    selectedInstanceIds.value = []
    await Promise.all([loadInstances(), loadOverview()])
  } catch (err: any) {
    batchPricePreview.value = err?.preview || batchPricePreview.value
    toast.error(err.message)
  } finally {
    batchPriceLoading.value = false
  }
}

// 提交修改价格
async function submitUpdatePrice() {
  if (!priceTarget.value) return
  
  priceLoading.value = true
  try {
    const res = await api.admin.updateInstancePrice(
      priceTarget.value.id,
      priceForm.value.newPrice,
      priceForm.value.settleBalance,
      pricePreview.value?.instanceVersion
    )
    toast.success(res.message)
    showPriceModal.value = false
    // 并行刷新实例列表和概览
    await Promise.all([loadInstances(), loadOverview()])
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    priceLoading.value = false
  }
}

// 打开切换方案弹窗
async function openUpgradeModal(instance: any) {
  closeInstanceMenu()
  upgradeTarget.value = instance
  selectedPlanId.value = null
  upgradeData.value = { currentPlan: null, remainingDays: 0, availablePlans: [], userBalance: 0 }
  showUpgradeModal.value = true
  upgradeLoading.value = true
  
  try {
    const res = await api.admin.getAvailablePlans(instance.id)
    upgradeData.value = res
  } catch (err: any) {
    toast.error(err.message)
    showUpgradeModal.value = false
  } finally {
    upgradeLoading.value = false
  }
}

// 执行升级
async function executeUpgrade() {
  if (!upgradeTarget.value || !selectedPlanId.value) return
  if (selectedUpgradePlanBlocked.value) {
    toast.error(selectedUpgradePlan.value?.resourceWarnings?.[0] || '实例所在节点资源不足，无法升级方案')
    return
  }

  upgradeSubmitting.value = true
  try {
    const res = await api.admin.upgradePlan(upgradeTarget.value.id, selectedPlanId.value)
    toast.success(res.message)
    showUpgradeModal.value = false
    // 并行刷新实例列表和概览
    await Promise.all([loadInstances(), loadOverview()])
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    upgradeSubmitting.value = false
  }
}

// 格式化计费周期
function formatBillingCycle(cycle: number): string {
  const map: Record<number, string> = {
    1: t('admin.billing.monthly'),
    3: t('admin.billing.quarterly'),
    6: t('admin.billing.semiAnnual'),
    12: t('admin.billing.yearly')
  }
  return map[cycle] || `${cycle} ${t('admin.billing.months')}`
}

function formatDate(dateStr: string | null): string {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleString()
}

const currencyFormatter = new Intl.NumberFormat('zh-CN', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

const countFormatter = new Intl.NumberFormat('zh-CN')

function formatMoney(amount: number): string {
  return `¥${currencyFormatter.format(amount || 0)}`
}

function formatCount(value: number): string {
  return countFormatter.format(value || 0)
}

function formatPercent(value: number): string {
  const rounded = value >= 10 ? value.toFixed(0) : value.toFixed(1)
  return `${rounded}%`
}

function formatSignedPercent(value: number | null): string {
  if (value === null) return '--'
  const rounded = Math.abs(value) >= 10 ? value.toFixed(0) : value.toFixed(1)
  return `${value > 0 ? '+' : ''}${rounded}%`
}

function getReconciliationStatusLabel(status: FinancialReconciliationStatus): string {
  const labels: Record<FinancialReconciliationStatus, string> = {
    normal: '正常',
    discrepancy: '差异',
    confirmed: '已确认',
    ignored: '已忽略'
  }
  return labels[status] || status
}

function getReconciliationItemTypeLabel(type: FinancialReconciliationItemType): string {
  const labels: Record<FinancialReconciliationItemType, string> = {
    recharge_missing_balance_log: '成功充值未入账',
    orphan_balance_log: '流水缺少来源',
    delivered_instance_missing_billing: '交付缺少扣费',
    approved_adjustment_missing_balance_log: '审批缺少流水'
  }
  return labels[type] || type
}

function getReconciliationExportLabel(type: string): string {
  const labels: Record<string, string> = {
    orders: '订单',
    balance_logs: '余额流水',
    adjustments: '调账审批'
  }
  return labels[type] || type
}

function getSummaryMoney(key: 'recharge' | 'balanceLogs' | 'instanceBilling' | 'approvedAdjustments'): number {
  return Number(reconciliation.value?.summary?.[key]?.amount || 0)
}

function getSummaryCount(key: 'recharge' | 'balanceLogs' | 'instanceBilling' | 'approvedAdjustments'): number {
  return Number(reconciliation.value?.summary?.[key]?.count || 0)
}

function calculateRatio(part: number, total: number): number {
  if (total <= 0) return 0
  return Math.min(100, Math.max(0, (part / total) * 100))
}

function calculateChange(current: number, previous: number): number | null {
  if (previous === 0) {
    return current === 0 ? 0 : null
  }
  return ((current - previous) / previous) * 100
}

// 获取扣费记录类型标签
function getRecordTypeLabel(type: string): string {
  const labels: Record<string, string> = {
    newPurchase: t('admin.billing.recordTypes.newPurchase'),
    renew: t('admin.billing.recordTypes.renew'),
    upgrade: t('admin.billing.recordTypes.upgrade'),
    downgrade: t('admin.billing.recordTypes.downgrade'),
    refund: t('admin.billing.recordTypes.refund'),
    transfer_fee: t('admin.billing.recordTypes.transfer_fee')
  }
  return labels[type] || type
}

function getInstanceStatusBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'running':
      return 'success'
    case 'stopped':
      return 'warning'
    case 'suspended':
      return 'error'
    case 'creating':
      return 'info'
    default:
      return 'default'
  }
}

function getRechargeStatusBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'completed':
      return 'success'
    case 'pending':
      return 'warning'
    case 'failed':
      return 'error'
    case 'cancelled':
    default:
      return 'default'
  }
}

function getReconciliationStatusBadgeVariant(status: FinancialReconciliationStatus): BadgeVariant {
  switch (status) {
    case 'normal':
      return 'success'
    case 'discrepancy':
      return 'error'
    case 'confirmed':
      return 'info'
    case 'ignored':
    default:
      return 'default'
  }
}

function getBatchPriceItemBadgeVariant(status: BatchPriceUpdateItemStatus): BadgeVariant {
  switch (status) {
    case 'ready':
      return 'info'
    case 'failed':
      return 'error'
    case 'unchanged':
    default:
      return 'default'
  }
}

function getBatchPriceItemStatusLabel(status: BatchPriceUpdateItemStatus): string {
  const map: Record<BatchPriceUpdateItemStatus, string> = {
    ready: t('admin.billing.batchPriceStatusReady'),
    unchanged: t('admin.billing.batchPriceStatusUnchanged'),
    failed: t('admin.billing.batchPriceStatusFailed')
  }
  return map[status]
}

function formatPriceDiff(value: number): string {
  if (value > 0) return `+${formatMoney(value)}`
  if (value < 0) return `-${formatMoney(Math.abs(value))}`
  return formatMoney(0)
}

// 获取支付渠道显示文本（渠道名 - 支付方式）
function getPayChannelDisplay(rec: any): string {
  const providerName = rec.provider?.displayName || rec.provider?.name || ''
  const methodCode = rec.payMethod || ''
  const methodKey = methodCode ? `wallet.paymentMethods.${methodCode}` : ''
  const methodLabel = methodKey ? t(methodKey) : ''
  // 如果翻译不存在（返回原 key），则使用原始值
  const method = methodLabel === methodKey ? methodCode : methodLabel
  
  if (providerName && method) {
    return `${method} - ${providerName}`
  } else if (providerName) {
    return providerName
  } else if (method) {
    return method
  }
  return '-'
}

function getRechargePaymentDetailsDisplay(rec: any): string {
  if (rec.actualPaymentMethod) {
    const methodKey = `wallet.paymentMethods.${rec.actualPaymentMethod}`
    const translated = t(methodKey)
    return translated === methodKey ? rec.actualPaymentMethod : translated
  }
  return '-'
}

function getRechargeCreditLabelKey(status: string): string {
  return status === 'completed' ? 'admin.billing.actualAmount' : 'admin.billing.estimatedAmount'
}

function getRechargeGatewayStatusText(rec: any): string {
  return rec.status === 'pending' && rec.gatewayStatusDescription ? rec.gatewayStatusDescription : ''
}

function copyToClipboard(text: string) {
  window.navigator.clipboard.writeText(text)
  toast.success(t('common.copied'))
}
</script>

<template>
  <div class="kawaii-page space-y-6 animate-fade-in">
    <!-- 页面头部 -->
    <PageHeader
      :title="$t('admin.billing.title')"
      :description="$t('admin.billing.description')"
    />

    <!-- Tab 切换 (Apple 风格分段胶囊控制) -->
    <div class="mb-6 flex items-center overflow-x-auto p-1.5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] w-fit max-w-full shadow-[0_2px_8px_rgba(0,0,0,0.02)]">
      <div class="flex items-center gap-1 shrink-0" role="tablist" :aria-label="$t('admin.billing.title')">
        <button
          v-for="tab in billingTabs"
          :key="tab"
          type="button"
          role="tab"
          :aria-selected="activeTab === tab"
          class="whitespace-nowrap rounded-xl px-4 py-2 text-sm font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="activeTab === tab
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="switchTab(tab)"
        >
          {{ $t(`admin.billing.tabs.${tab}`) }}
        </button>
      </div>
    </div>

    <!-- 概览 Tab -->
    <div v-show="activeTab === 'overview'">
      <div v-if="overviewLoading" class="space-y-6">
        <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <Card v-for="i in 4" :key="`overview-kpi-${i}`" class="h-36 animate-pulse" padding="md">
            <div class="h-4 w-20 rounded-md bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-5 h-8 w-36 rounded-md bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-4 h-3 w-28 rounded-md bg-[var(--bg-surface-soft)]"></div>
          </Card>
        </div>
        <div class="grid gap-5 xl:grid-cols-[minmax(0,1.35fr)_minmax(320px,0.65fr)]">
          <Card class="h-80 animate-pulse" padding="md">
            <div class="h-5 w-28 rounded-md bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-6 space-y-4">
              <div v-for="i in 3" :key="`overview-mix-${i}`" class="h-16 rounded-xl bg-[var(--bg-surface-soft)]"></div>
            </div>
          </Card>
          <Card class="h-80 animate-pulse" padding="md">
            <div class="h-5 w-28 rounded-md bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-6 h-24 rounded-xl bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-5 space-y-3">
              <div v-for="i in 3" :key="`overview-health-${i}`" class="h-12 rounded-xl bg-[var(--bg-surface-soft)]"></div>
            </div>
          </Card>
        </div>
        <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
          <Card v-for="i in 4" :key="`overview-aff-${i}`" class="h-28 animate-pulse" padding="md">
            <div class="h-4 w-24 rounded-md bg-[var(--bg-surface-soft)]"></div>
            <div class="mt-5 h-7 w-32 rounded-md bg-[var(--bg-surface-soft)]"></div>
          </Card>
        </div>
      </div>

      <template v-else-if="overview">
        <div class="space-y-6">
          <section class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <Card class="min-w-0 xl:col-span-2" padding="md">
              <div class="flex items-start justify-between gap-4">
                <div class="min-w-0">
                  <p class="text-xs font-semibold tracking-wide uppercase text-[var(--text-secondary)]">{{ $t('admin.billing.netRevenueLabel') }}</p>
                  <p class="mt-3 break-words font-mono text-3xl font-semibold leading-tight tracking-normal tabular-nums text-[var(--text-primary)] md:text-4xl">
                    {{ formatMoney(overview.netRevenue) }}
                  </p>
                </div>
                <div class="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="wallet" class="h-5 w-5" />
                </div>
              </div>

              <div class="mt-6 grid gap-4 sm:grid-cols-2">
                <div class="min-w-0 border-l-2 border-[var(--border-color)] pl-3.5">
                  <p class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.todayRevenue') }}</p>
                  <p class="mt-1 break-words font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.todayRevenue) }}</p>
                  <p class="mt-1 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.todayRecharge') }} {{ formatMoney(overview.recharge.todayAmount) }}</p>
                </div>
                <div class="min-w-0 border-l-2 border-[var(--border-color)] pl-3.5">
                  <p class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.thisMonthRevenue') }}</p>
                  <p class="mt-1 break-words font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.thisMonthRevenue) }}</p>
                  <p class="mt-1 text-xs font-medium" :class="monthlyRevenueChange !== null && monthlyRevenueChange < 0 ? 'text-[var(--error)]' : 'text-[var(--success)]'">
                    {{ $t('admin.billing.thisMonthVsLastMonth') }} {{ formatSignedPercent(monthlyRevenueChange) }}
                  </p>
                </div>
              </div>
            </Card>

            <Card class="min-w-0" padding="md">
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="text-xs font-semibold tracking-wide uppercase text-[var(--text-secondary)]">{{ $t('admin.billing.totalRevenue') }}</p>
                  <p class="mt-2 break-words font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.totalRevenue) }}</p>
                </div>
                <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="chart" class="h-5 w-5" />
                </div>
              </div>
              <p class="mt-3 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.todayRevenue') }} {{ formatMoney(overview.todayRevenue) }}</p>
            </Card>

            <Card class="min-w-0" padding="md">
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="text-xs font-semibold tracking-wide uppercase text-[var(--text-secondary)]">{{ $t('admin.billing.totalRecharge') }}</p>
                  <p class="mt-2 break-words font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.recharge.totalAmount) }}</p>
                </div>
                <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="wallet" class="h-5 w-5" />
                </div>
              </div>
              <p class="mt-3 text-xs text-[var(--text-secondary)]">{{ formatCount(overview.recharge.totalCount) }} {{ $t('admin.billing.orders') }}</p>
            </Card>

            <Card class="min-w-0" padding="md">
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="text-xs font-semibold tracking-wide uppercase text-[var(--text-secondary)]">{{ $t('admin.billing.totalRefunds') }}</p>
                  <p class="mt-2 break-words font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.totalRefunds) }}</p>
                </div>
                <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="ban" class="h-5 w-5" />
                </div>
              </div>
              <p class="mt-3 text-xs text-[var(--text-secondary)]">{{ formatPercent(refundRate) }}</p>
            </Card>
          </section>

          <section>
            <div class="mb-3 flex items-center justify-between gap-4">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.rechargeLabel') }} / {{ $t('admin.billing.revenueLabel') }}</h2>
                <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.overviewPeriods.total') }} · {{ $t('admin.billing.overviewPeriods.thisMonth') }} · {{ $t('admin.billing.overviewPeriods.today') }}</p>
              </div>
            </div>

            <div class="grid gap-4 lg:grid-cols-3">
              <Card
                v-for="period in periodComparisonCards"
                :key="period.key"
                class="min-w-0"
                padding="md"
              >
                <div class="flex items-start justify-between gap-4">
                  <div class="min-w-0">
                    <Badge variant="default" size="sm">
                      {{ period.label }}
                    </Badge>
                    <p class="mt-3 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.rechargeLabel') }} / {{ $t('admin.billing.revenueLabel') }}</p>
                  </div>
                  <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)]">
                    <BillingOverviewIcon name="wallet" class="h-5 w-5" />
                  </div>
                </div>

                <dl class="mt-5 divide-y divide-[var(--border-color)]">
                  <div class="flex items-center justify-between gap-4 pb-3">
                    <dt class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.rechargeLabel') }}</dt>
                    <dd class="min-w-0 text-right">
                      <div class="break-words font-mono text-lg font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(period.rechargeAmount) }}</div>
                      <div class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ formatCount(period.rechargeCount) }} {{ $t('admin.billing.orders') }}</div>
                    </dd>
                  </div>
                  <div class="flex items-center justify-between gap-4 pt-3">
                    <dt class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.revenueLabel') }}</dt>
                    <dd class="min-w-0 break-words text-right font-mono text-lg font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(period.revenueAmount) }}</dd>
                  </div>
                </dl>
              </Card>
            </div>
          </section>

          <div class="grid gap-5 xl:grid-cols-[minmax(0,1.35fr)_minmax(320px,0.65fr)]">
            <Card class="min-w-0" padding="md">
              <div class="flex flex-col gap-3 border-b border-[var(--border-color)] pb-4 sm:flex-row sm:items-start sm:justify-between">
                <div class="min-w-0">
                  <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.revenueBreakdownTitle') }}</h2>
                  <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.revenueLabel') }}</p>
                </div>
                <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="chart" class="h-5 w-5" />
                </div>
              </div>

              <div class="mt-5 grid gap-4 sm:grid-cols-2">
                <div class="min-w-0 border-l-2 border-[var(--accent)] pl-3.5">
                  <p class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.revenueLabel') }}</p>
                  <p class="mt-1 break-words font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(overview.revenueMix.direct.totalAmount) }}</p>
                </div>
              </div>

              <div class="mt-5 divide-y divide-[var(--border-color)]">
                <div v-for="row in revenueMixRows" :key="row.key" class="py-3.5 first:pt-0 last:pb-0">
                  <div class="flex items-center justify-between gap-3">
                    <p class="text-sm font-medium text-[var(--text-primary)]">{{ row.label }}</p>
                    <span class="break-words text-right font-mono font-semibold text-[var(--text-primary)]">{{ formatMoney(row.directAmount) }}</span>
                  </div>
                </div>
              </div>
            </Card>

            <Card class="min-w-0" padding="md">
              <div class="flex items-start justify-between gap-4 border-b border-[var(--border-color)] pb-4">
                <div class="min-w-0">
                  <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.instanceHealthTitle') }}</h2>
                  <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ formatCount(overview.activePaidInstancesCount) }} / {{ formatCount(overview.paidInstancesCount) }}</p>
                </div>
                <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))]">
                  <BillingOverviewIcon name="server" class="h-5 w-5" />
                </div>
              </div>

              <div class="mt-5">
                <div class="flex items-end justify-between gap-4">
                  <div class="min-w-0">
                    <p class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.paidInstances') }}</p>
                    <p class="mt-1 break-words font-mono text-3xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatCount(overview.paidInstancesCount) }}</p>
                  </div>
                  <div class="rounded-full bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))] px-2.5 py-0.5 text-xs font-semibold text-[var(--success)]">
                    {{ formatPercent(activeInstanceShare) }}
                  </div>
                </div>

                <div class="mt-4 h-2 overflow-hidden rounded-full bg-[var(--bg-surface-soft)]">
                  <div class="h-full rounded-full bg-[var(--success)] transition-all duration-500" :style="{ width: `${activeInstanceShare}%` }"></div>
                </div>
              </div>

              <div class="mt-5 divide-y divide-[var(--border-color)]">
                <div
                  v-for="card in instanceStatusCards"
                  :key="card.key"
                  class="py-3.5 first:pt-0 last:pb-0"
                >
                  <div class="flex items-center justify-between gap-4">
                    <div class="flex min-w-0 items-center gap-3">
                      <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl" :class="card.surfaceClass">
                        <BillingOverviewIcon :name="card.icon" class="h-4 w-4" :class="card.toneClass" />
                      </div>
                      <div class="min-w-0">
                        <p class="text-sm font-medium text-[var(--text-primary)]">{{ card.label }}</p>
                        <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ formatPercent(card.progress || 0) }}</p>
                      </div>
                    </div>
                    <div class="shrink-0 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatCount(card.value) }}</div>
                  </div>
                  <div class="mt-2.5 h-1.5 overflow-hidden rounded-full bg-[var(--bg-surface-soft)]">
                    <div class="h-full rounded-full bg-current transition-all duration-500" :class="card.toneClass" :style="{ width: `${card.progress || 0}%` }"></div>
                  </div>
                </div>
              </div>
            </Card>
          </div>

          <section>
            <div class="mb-3 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
              <div>
                <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.affOverviewTitle') }}</h2>
                <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ formatCount(overview.aff.totalOrders) }} {{ $t('admin.billing.orders') }}</p>
              </div>
            </div>

            <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
              <Card
                v-for="card in affCards"
                :key="card.key"
                class="min-w-0"
                padding="md"
              >
                <div class="flex items-start justify-between gap-3">
                  <div class="min-w-0">
                    <p class="text-xs font-semibold tracking-wide uppercase text-[var(--text-secondary)]">{{ card.label }}</p>
                    <p class="mt-2 break-words font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">
                      {{ card.format === 'money' ? formatMoney(card.value) : formatCount(card.value) }}
                    </p>
                  </div>
                  <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl" :class="card.surfaceClass">
                    <BillingOverviewIcon :name="card.icon" class="h-5 w-5" :class="card.toneClass" />
                  </div>
                </div>
              </Card>
            </div>
          </section>
        </div>
      </template>

      <Card v-else padding="none">
        <EmptyState :title="$t('common.noData')" />
      </Card>
    </div>

    <!-- 实例 Tab -->
    <div v-show="activeTab === 'instances'">
      <!-- 筛选 -->
      <Card class="mb-5" padding="md">
        <div class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.tabs.instances') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.totalCount', { count: instancesTotal }) }}</p>
          </div>
          <div class="flex flex-wrap items-center gap-3">
            <div class="w-40">
              <Select v-model="instancesFilter" size="sm" @change="instancesPage = 1; loadInstances()">
                <option value="">{{ $t('admin.billing.allStatus') }}</option>
                <option value="running">{{ $t('instance.status.running') }}</option>
                <option value="stopped">{{ $t('instance.status.stopped') }}</option>
                <option value="suspended">{{ $t('instance.status.suspended') }}</option>
              </Select>
            </div>
            <div class="w-48">
              <Select
                :model-value="instancesHostFilter"
                size="sm"
                @update:model-value="val => instancesHostFilter = val === '' ? '' : Number(val)"
                @change="instancesPage = 1; loadInstances()"
              >
                <option value="">{{ $t('admin.billing.allHosts') }}</option>
                <option v-for="host in hosts" :key="host.id" :value="host.id">{{ host.name }}</option>
              </Select>
            </div>
            <label class="inline-flex items-center gap-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-1.5 text-xs font-medium cursor-pointer transition-colors hover:bg-[var(--bg-surface)]">
              <input v-model="showExpiring" type="checkbox" class="checkbox rounded-md" @change="instancesPage = 1; loadInstances()" />
              <span>{{ $t('admin.billing.showExpiring') }}</span>
            </label>
            <label class="inline-flex items-center gap-2 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-1.5 text-xs font-medium cursor-pointer transition-colors hover:bg-[var(--bg-surface)]">
              <input v-model="showDateColumns" type="checkbox" class="checkbox rounded-md" />
              <span>{{ $t('admin.billing.showDateColumns') }}</span>
            </label>
          </div>
        </div>

        <div class="mt-4 flex flex-col gap-3 sm:flex-row sm:items-center">
          <div class="flex-1">
            <Input
              v-model="instancesSearch"
              clearable
              :placeholder="$t('admin.billing.searchPlaceholder')"
              @enter="instancesPage = 1; loadInstances()"
            />
          </div>
          <Button
            variant="primary"
            @click="instancesPage = 1; loadInstances()"
          >
            {{ $t('common.search') }}
          </Button>
        </div>
      </Card>

      <SkeletonLoader v-if="instancesLoading" :count="3" />

      <Card v-else-if="instances.length > 0" padding="none">
        <div class="flex flex-col gap-3 border-b border-[var(--border-color)] px-6 py-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <div class="text-sm font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.tabs.instances') }}</div>
            <div class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.totalCount', { count: instancesTotal }) }}</div>
          </div>
          <div class="flex flex-wrap items-center gap-3 sm:justify-end">
            <div class="hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-1 text-xs text-[var(--text-secondary)] md:block">
              {{ selectedInstancesCount > 0 ? $t('admin.billing.batchSelectedCount', { count: selectedInstancesCount }) : (showExpiring ? $t('admin.billing.showExpiring') : $t('admin.billing.allStatus')) }}
            </div>
            <Button
              variant="primary"
              size="sm"
              :disabled="selectedInstancesCount === 0"
              @click="openBatchPriceModal"
            >
              {{ $t('admin.billing.batchUpdatePrice') }}
            </Button>
          </div>
        </div>
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="inst in instances"
            :key="inst.id"
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)] transition-all"
            :class="inst.status === 'deleted' ? 'opacity-60' : ''"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-start gap-3">
                <input
                  v-model="selectedInstanceIds"
                  type="checkbox"
                  class="checkbox mt-1 rounded-md"
                  :value="inst.id"
                  :disabled="inst.status === 'deleted'"
                  :aria-label="$t('admin.billing.selectInstance')"
                />
                <div class="min-w-0">
                  <button
                    class="truncate text-left text-sm font-semibold text-[var(--text-primary)] hover:text-[var(--accent)] transition-colors"
                    @click="router.push(instanceDetailPath(inst.id))"
                  >
                    {{ inst.name }}
                  </button>
                  <div class="mt-1 flex flex-wrap items-center gap-2 text-xs text-[var(--text-secondary)]">
                    <button class="font-mono text-[var(--text-primary)] hover:text-[var(--accent)] transition-colors" @click="router.push(instanceDetailPath(inst.id))">
                      #{{ inst.id }}
                    </button>
                    <span v-if="inst.incusId" class="font-mono" :title="inst.incusId">{{ inst.incusId?.slice(0, 8) }}</span>
                  </div>
                </div>
              </div>
              <div class="flex shrink-0 flex-col items-end gap-1.5">
                <Badge :variant="getInstanceStatusBadgeVariant(inst.status)">{{ $t('instance.status.' + inst.status) }}</Badge>
                <Badge v-if="inst.autoRenew" variant="info" size="sm">{{ $t('admin.billing.autoRenew') }}</Badge>
              </div>
            </div>

            <div class="mt-3 flex flex-wrap gap-2">
              <button
                class="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-medium bg-[var(--bg-surface-soft)] text-[var(--text-primary)] border border-[var(--border-color)] hover:bg-[var(--bg-surface)] active:scale-95 transition-all"
                @click="router.push(instanceDetailPath(inst.id))"
              >
                {{ inst.instanceTypeLabel }}
              </button>
            </div>

            <div class="mt-4 grid grid-cols-2 gap-3 text-sm">
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.user') }}</div>
                <div class="mt-1 min-w-0">
                  <div class="truncate font-medium text-[var(--text-primary)]">{{ inst.user?.username || '-' }}</div>
                  <div class="truncate text-xs text-[var(--text-secondary)]">#{{ inst.user?.id }} {{ inst.user?.email || '' }}</div>
                </div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.package') }}</div>
                <div class="mt-1 truncate font-medium text-[var(--text-primary)]">{{ inst.package?.name || '-' }}</div>
                <div class="truncate text-xs text-[var(--text-secondary)]">{{ inst.packagePlan?.name || '-' }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.host') }}</div>
                <div class="mt-1 truncate font-medium text-[var(--text-primary)]">{{ inst.host?.name || '-' }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.price') }}</div>
                <button
                  v-if="inst.billingPrice"
                  class="mt-1 font-semibold text-[var(--accent)] hover:underline active:scale-95 transition-all"
                  @click="openPriceModal(inst)"
                >
                  {{ formatMoney(inst.billingPrice) }}
                </button>
                <span v-else class="mt-1 block text-[var(--text-secondary)]">-</span>
                <div class="text-xs text-[var(--text-secondary)]">{{ inst.billingCycle ? $t('admin.billing.cycleMonths', { months: inst.billingCycle }) : '-' }}</div>
              </div>
              <div v-if="showDateColumns" class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.purchaseDate') }}</div>
                <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatDate(inst.createdAt) }}</div>
              </div>
              <div v-if="showDateColumns" class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.expiresAt') }}</div>
                <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatDate(inst.expiresAt) }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.remainingDays') }}</div>
                <div class="mt-1">
                  <span
                    v-if="inst.remainingDays !== null"
                    :class="[
                      'font-semibold text-xs',
                      inst.remainingDays <= 0 ? 'text-[var(--error)]' :
                      inst.remainingDays <= 7 ? 'text-[var(--warning)]' : 'text-[var(--success)]'
                    ]"
                  >
                    {{ inst.remainingDays <= 0 ? $t('admin.billing.expired') : inst.remainingDays + ' ' + $t('admin.billing.days') }}
                  </span>
                  <span v-else class="text-[var(--text-secondary)]">-</span>
                </div>
              </div>
            </div>

            <div class="mt-4 flex flex-wrap justify-end gap-1 border-t border-[var(--border-color)] pt-3">
              <div class="relative inline-block text-left billing-action-menu-container">
                <button
                  type="button"
                  class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all"
                  :class="{ 'text-[var(--text-primary)] bg-[var(--bg-surface-soft)]': activeInstanceMenuId === inst.id }"
                  :title="$t('common.actions')"
                  @click.stop="toggleInstanceMenu(inst.id, $event)"
                >
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 12h.01M12 12h.01M19 12h.01M6 12a1 1 0 11-2 0 1 1 0 012 0zm7 0a1 1 0 11-2 0 1 1 0 012 0zm7 0a1 1 0 11-2 0 1 1 0 012 0z" />
                  </svg>
                </button>
              </div>
            </div>
          </div>
        </div>

        <div class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed text-sm">
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr>
                <th class="w-11 p-3 whitespace-nowrap">
                  <input
                    type="checkbox"
                    class="checkbox rounded-md"
                    :checked="allCurrentPageSelected"
                    :indeterminate="someCurrentPageSelected && !allCurrentPageSelected"
                    :disabled="selectableInstances.length === 0"
                    :aria-label="$t('admin.billing.selectAllCurrentPage')"
                    @change="toggleSelectAllCurrentPage"
                  />
                </th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.instanceType') }}</th>
                <th class="text-left p-3 whitespace-nowrap">ID</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.instanceName') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.instanceStatus') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.user') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.package') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.plan') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.host') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.price') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.cycle') }}</th>
                <th v-if="showDateColumns" class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.purchaseDate') }}</th>
                <th v-if="showDateColumns" class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.expiresAt') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.remainingDays') }}</th>
                <th class="text-right p-3 whitespace-nowrap">{{ $t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr v-for="inst in instances" :key="inst.id" class="border-t border-[var(--border-color)] hover:bg-[var(--bg-surface-soft)]/50 transition-colors">
                <td class="p-3 whitespace-nowrap">
                  <input
                    v-model="selectedInstanceIds"
                    type="checkbox"
                    class="checkbox rounded-md"
                    :value="inst.id"
                    :disabled="inst.status === 'deleted'"
                    :aria-label="$t('admin.billing.selectInstance')"
                  />
                </td>
                <!-- 实例类型图标 -->
                <td class="p-3 whitespace-nowrap">
                  <span 
                    class="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-full text-xs font-medium cursor-pointer bg-[var(--bg-surface-soft)] text-[var(--text-primary)] border border-[var(--border-color)] hover:bg-[var(--bg-surface)] active:scale-95 transition-all"
                    @click="router.push(instanceDetailPath(inst.id))"
                  >
                    <svg v-if="inst.instanceTypeLabel === 'PRIME'" class="w-3 h-3 text-[var(--accent)]" fill="currentColor" viewBox="0 0 20 20">
                      <path d="M9.049 2.927c.3-.921 1.603-.921 1.902 0l1.07 3.292a1 1 0 00.95.69h3.462c.969 0 1.371 1.24.588 1.81l-2.8 2.034a1 1 0 00-.364 1.118l1.07 3.292c.3.921-.755 1.688-1.54 1.118l-2.8-2.034a1 1 0 00-1.175 0l-2.8 2.034c-.784.57-1.838-.197-1.539-1.118l1.07-3.292a1 1 0 00-.364-1.118L2.98 8.72c-.783-.57-.38-1.81.588-1.81h3.461a1 1 0 00.951-.69l1.07-3.292z" />
                    </svg>
                    <svg v-else class="w-3 h-3 text-[var(--text-secondary)]" fill="currentColor" viewBox="0 0 20 20">
                      <path fill-rule="evenodd" d="M11.3 1.046A1 1 0 0112 2v5h4a1 1 0 01.82 1.573l-7 10A1 1 0 018 18v-5H4a1 1 0 01-.82-1.573l7-10a1 1 0 011.12-.38z" clip-rule="evenodd" />
                    </svg>
                    {{ inst.instanceTypeLabel }}
                  </span>
                </td>
                <!-- ID -->
                <td class="p-3 whitespace-nowrap">
                  <div class="flex flex-col gap-0.5">
                    <span 
                      class="text-[var(--text-primary)] hover:text-[var(--accent)] cursor-pointer font-mono text-xs font-medium transition-colors"
                      @click="router.push(instanceDetailPath(inst.id))"
                    >#{{ inst.id }}</span>
                    <span class="text-[var(--text-secondary)] font-mono text-xs" :title="inst.incusId">{{ inst.incusId?.slice(0, 8) }}</span>
                  </div>
                </td>
                <!-- 实例名 -->
                <td class="p-3">
                  <span 
                    class="block truncate text-[var(--text-primary)] font-medium hover:text-[var(--accent)] cursor-pointer transition-colors"
                    :title="inst.name"
                    @click="router.push(instanceDetailPath(inst.id))"
                  >{{ inst.name }}</span>
                </td>
                <!-- 状态 -->
                <td class="p-3 whitespace-nowrap">
                  <Badge :variant="getInstanceStatusBadgeVariant(inst.status)">{{ $t('instance.status.' + inst.status) }}</Badge>
                  <Badge v-if="inst.autoRenew" variant="info" size="sm" class="ml-1">{{ $t('admin.billing.autoRenew') }}</Badge>
                </td>
                <!-- 用户 -->
                <td class="p-3 whitespace-nowrap">
                  <div class="flex flex-col">
                    <span class="truncate text-[var(--text-primary)] font-medium">{{ inst.user?.username || '-' }}</span>
                    <div class="flex items-center gap-1 text-xs text-[var(--text-secondary)]">
                      <span>#{{ inst.user?.id }}</span>
                      <span v-if="inst.user?.email" class="truncate max-w-32" :title="inst.user.email">{{ inst.user.email }}</span>
                    </div>
                  </div>
                </td>
                <!-- 套餐名 -->
                <td class="p-3 truncate text-[var(--text-secondary)]" :title="inst.package?.name">{{ inst.package?.name || '-' }}</td>
                <!-- 方案名 -->
                <td class="p-3 truncate text-[var(--text-secondary)]" :title="inst.packagePlan?.name">{{ inst.packagePlan?.name || '-' }}</td>
                <!-- 宿主机名 -->
                <td class="p-3 truncate text-[var(--text-secondary)]" :title="inst.host?.name">{{ inst.host?.name || '-' }}</td>
                <!-- 价格（可点击修改） -->
                <td class="p-3 whitespace-nowrap">
                  <button
                    v-if="inst.billingPrice"
                    class="text-[var(--accent)] hover:underline cursor-pointer font-semibold font-mono"
                    @click="openPriceModal(inst)"
                  >
                    {{ formatMoney(inst.billingPrice) }}
                  </button>
                  <span v-else class="text-[var(--text-secondary)]">-</span>
                </td>
                <!-- 周期 -->
                <td class="p-3 whitespace-nowrap text-[var(--text-secondary)]">{{ inst.billingCycle ? $t('admin.billing.cycleMonths', { months: inst.billingCycle }) : '-' }}</td>
                <!-- 购买日期 -->
                <td v-if="showDateColumns" class="p-3 whitespace-nowrap text-[var(--text-secondary)] text-xs">{{ formatDate(inst.createdAt) }}</td>
                <!-- 到期时间 -->
                <td v-if="showDateColumns" class="p-3 whitespace-nowrap text-[var(--text-secondary)] text-xs">{{ formatDate(inst.expiresAt) }}</td>
                <!-- 剩余天数 -->
                <td class="p-3 whitespace-nowrap">
                  <span 
                    v-if="inst.remainingDays !== null"
                    :class="[
                      'font-semibold text-xs',
                      inst.remainingDays <= 0 ? 'text-[var(--error)]' :
                      inst.remainingDays <= 7 ? 'text-[var(--warning)]' : 'text-[var(--success)]'
                    ]"
                  >
                    {{ inst.remainingDays <= 0 ? $t('admin.billing.expired') : inst.remainingDays + ' ' + $t('admin.billing.days') }}
                  </span>
                  <span v-else class="text-[var(--text-secondary)]">-</span>
                </td>
                <!-- 操作 -->
                <td class="p-3 text-right whitespace-nowrap">
                  <div class="relative inline-block text-left billing-action-menu-container">
                    <button
                      type="button"
                      class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all"
                      :class="{ 'text-[var(--text-primary)] bg-[var(--bg-surface-soft)]': activeInstanceMenuId === inst.id }"
                      :title="$t('common.actions')"
                      @click.stop="toggleInstanceMenu(inst.id, $event)"
                    >
                      <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 12h.01M12 12h.01M19 12h.01M6 12a1 1 0 11-2 0 1 1 0 012 0zm7 0a1 1 0 11-2 0 1 1 0 012 0zm7 0a1 1 0 11-2 0 1 1 0 012 0z" />
                      </svg>
                    </button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>

      <Card v-else padding="none">
        <EmptyState
          :title="$t('admin.billing.noInstances')"
          :description="$t('admin.billing.searchPlaceholder')"
        />
      </Card>

      <!-- 分页 -->
      <Card v-if="instances.length > 0" class="mt-4" padding="sm">
        <Pagination
          :page="instancesPage"
          :page-size="instancesPageSize"
          :total="instancesTotal"
          :show-size-changer="true"
          :page-size-options="pageSizeOptions"
          @update:page="instancesPage = $event; loadInstances()"
          @update:page-size="instancesPageSize = $event; instancesPage = 1; loadInstances()"
        />
      </Card>
    </div>

    <!-- 实例操作下拉菜单 (Teleport 挂载到 body) -->
    <Teleport to="body">
      <div 
        v-if="activeMenuInstance" 
        class="fixed inset-0 z-40" 
        @click.stop="closeInstanceMenu"
      ></div>
      <div
        v-if="activeMenuInstance"
        class="fixed z-50 min-w-[10rem] rounded-2xl p-1.5 border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] backdrop-blur-md transition-all animate-fade-in"
        :style="{ top: `${instanceMenuPosition.top}px`, right: `${instanceMenuPosition.right}px` }"
        @click.stop
      >
        <template v-for="inst in [activeMenuInstance]" :key="inst.id">
          <!-- 解除封停 -->
          <button
            v-if="inst.status === 'suspended'"
            type="button"
            class="w-full flex items-center px-3 py-2 text-xs font-medium rounded-xl text-left transition-colors cursor-pointer text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
            @click="openActionModal('unsuspend', inst)"
          >
            {{ $t('admin.billing.unsuspend') }}
          </button>

          <!-- 延期 -->
          <button
            type="button"
            class="w-full flex items-center px-3 py-2 text-xs font-medium rounded-xl text-left transition-colors cursor-pointer text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
            @click="openActionModal('extend', inst)"
          >
            {{ $t('admin.billing.extend') }}
          </button>

          <!-- 切换方案 -->
          <button
            v-if="inst.remainingDays > 0"
            type="button"
            class="w-full flex items-center px-3 py-2 text-xs font-medium rounded-xl text-left transition-colors cursor-pointer text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
            @click="openUpgradeModal(inst)"
          >
            {{ $t('admin.billing.upgradePlan') }}
          </button>

          <!-- 应用折扣 -->
          <button
            v-if="!inst.hasAffBinding"
            type="button"
            class="w-full flex items-center px-3 py-2 text-xs font-medium rounded-xl text-left transition-colors cursor-pointer text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
            @click="openActionModal('applyDiscount', inst)"
          >
            {{ $t('admin.billing.applyDiscount') }}
          </button>

          <!-- 删除退款 -->
          <button
            type="button"
            class="w-full flex items-center px-3 py-2 text-xs font-medium rounded-xl text-left transition-colors cursor-pointer text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] active:scale-[0.98]"
            @click="openActionModal('deleteRefund', inst)"
          >
            {{ $t('admin.billing.deleteRefund') }}
          </button>
        </template>
      </div>
    </Teleport>

    <!-- 记录 Tab -->
    <div v-show="activeTab === 'records'">
      <SkeletonLoader v-if="recordsLoading" :count="5" />

      <Card v-else-if="records.length > 0" padding="none">
        <div class="flex items-center justify-between gap-4 border-b border-[var(--border-color)] px-6 py-4">
          <div>
            <div class="text-sm font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.tabs.records') }}</div>
            <div class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.totalCount', { count: recordsTotal }) }}</div>
          </div>
          <div class="hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-1 text-xs text-[var(--text-secondary)] md:block">
            {{ $t('admin.billing.recordType') }} / {{ $t('admin.billing.amount') }}
          </div>
        </div>
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="rec in records"
            :key="rec.id"
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)]"
          >
            <div class="flex items-start justify-between gap-3">
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ getRecordTypeLabel(rec.type) }}</div>
                <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatDate(rec.createdAt) }}</div>
              </div>
              <div class="shrink-0 text-sm font-semibold font-mono" :class="rec.amount >= 0 ? 'text-[var(--success)]' : 'text-[var(--error)]'">
                {{ formatMoney(rec.amount) }}
              </div>
            </div>
            <div class="mt-4 grid grid-cols-2 gap-3 text-sm">
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.instance') }}</div>
                <div class="mt-1 truncate font-medium text-[var(--text-primary)]">{{ rec.instance?.name || '-' }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.user') }}</div>
                <div class="mt-1 truncate font-medium text-[var(--text-primary)]">{{ rec.user?.username || '-' }}</div>
              </div>
              <div class="col-span-2 rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.remark') }}</div>
                <div class="mt-1 break-words text-xs text-[var(--text-secondary)] leading-relaxed">{{ rec.remark || '-' }}</div>
              </div>
            </div>
          </div>
        </div>

        <div class="hidden lg:block">
          <table class="w-full table-fixed text-sm">
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr>
                <th class="w-[16%] text-left p-3">{{ $t('admin.billing.recordType') }}</th>
                <th class="w-[12%] text-left p-3">{{ $t('admin.billing.amount') }}</th>
                <th class="w-[18%] text-left p-3">{{ $t('admin.billing.instance') }}</th>
                <th class="w-[14%] text-left p-3">{{ $t('admin.billing.user') }}</th>
                <th class="w-[24%] text-left p-3">{{ $t('admin.billing.remark') }}</th>
                <th class="w-[16%] text-left p-3">{{ $t('admin.billing.time') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr v-for="rec in records" :key="rec.id" class="border-t border-[var(--border-color)] hover:bg-[var(--bg-surface-soft)]/50 transition-colors">
                <td class="p-3 whitespace-nowrap font-medium text-[var(--text-primary)]">{{ getRecordTypeLabel(rec.type) }}</td>
                <td class="p-3 whitespace-nowrap font-mono font-semibold" :class="rec.amount >= 0 ? 'text-[var(--success)]' : 'text-[var(--error)]'">
                  {{ formatMoney(rec.amount) }}
                </td>
                <td class="p-3 truncate text-[var(--text-secondary)]" :title="rec.instance?.name">{{ rec.instance?.name || '-' }}</td>
                <td class="p-3 truncate text-[var(--text-secondary)]" :title="rec.user?.username">{{ rec.user?.username || '-' }}</td>
                <td class="p-3 text-[var(--text-secondary)] truncate text-xs" :title="rec.remark">{{ rec.remark || '-' }}</td>
                <td class="p-3 text-[var(--text-secondary)] whitespace-nowrap text-xs font-mono">{{ formatDate(rec.createdAt) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>

      <Card v-else padding="none">
        <EmptyState
          :title="$t('admin.billing.noRecords')"
          :description="$t('admin.billing.tabs.records')"
        />
      </Card>

      <!-- 分页 -->
      <Card v-if="records.length > 0" class="mt-4" padding="sm">
        <Pagination
          :page="recordsPage"
          :page-size="recordsPageSize"
          :total="recordsTotal"
          :show-size-changer="true"
          :page-size-options="pageSizeOptions"
          @update:page="recordsPage = $event; loadRecords()"
          @update:page-size="recordsPageSize = $event; recordsPage = 1; loadRecords()"
        />
      </Card>
    </div>

    <!-- 充值记录 Tab -->
    <div v-show="activeTab === 'rechargeRecords'">
      <!-- 筛选 -->
      <Card class="mb-5" padding="md">
        <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.tabs.rechargeRecords') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.totalCount', { count: rechargeRecordsTotal }) }}</p>
          </div>
          <div class="w-48">
            <Select v-model="rechargeRecordsFilter" size="sm" @change="rechargeRecordsPage = 1; loadRechargeRecords()">
              <option value="">{{ $t('admin.billing.allStatus') }}</option>
              <option value="pending">{{ $t('admin.billing.rechargeStatus.pending') }}</option>
              <option value="completed">{{ $t('admin.billing.rechargeStatus.completed') }}</option>
              <option value="cancelled">{{ $t('admin.billing.rechargeStatus.cancelled') }}</option>
              <option value="failed">{{ $t('admin.billing.rechargeStatus.failed') }}</option>
            </Select>
          </div>
        </div>
      </Card>

      <SkeletonLoader v-if="rechargeRecordsLoading" :count="5" />

      <Card v-else-if="rechargeRecords.length > 0" padding="none">
        <div class="flex items-center justify-between gap-4 border-b border-[var(--border-color)] px-6 py-4">
          <div>
            <div class="text-sm font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('admin.billing.tabs.rechargeRecords') }}</div>
            <div class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ $t('admin.billing.totalCount', { count: rechargeRecordsTotal }) }}</div>
          </div>
          <div class="hidden rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-1 text-xs text-[var(--text-secondary)] md:block">
            {{ rechargeRecordsFilter ? $t(`admin.billing.rechargeStatus.${rechargeRecordsFilter}`) : $t('admin.billing.allStatus') }}
          </div>
        </div>
        <div class="divide-y divide-[var(--border-color)] md:hidden">
          <div v-for="rec in rechargeRecords" :key="rec.id" class="space-y-3 p-4">
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="truncate font-mono text-xs text-[var(--text-primary)]">
                  <span class="cursor-pointer hover:text-[var(--accent)] transition-colors" @click="copyToClipboard(rec.orderNo)">{{ rec.orderNo }}</span>
                </div>
                <div class="mt-1 text-sm font-semibold text-[var(--text-primary)]">{{ rec.user?.username || '-' }}</div>
                <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatDate(rec.createdAt) }}</div>
              </div>
              <Badge :variant="getRechargeStatusBadgeVariant(rec.status)">{{ $t(`admin.billing.rechargeStatus.${rec.status}`) }}</Badge>
            </div>

            <div class="grid grid-cols-2 gap-3 text-sm">
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs uppercase tracking-wide text-[var(--text-secondary)]">{{ $t('admin.billing.amount') }}</div>
                <div class="mt-1 font-semibold font-mono text-[var(--text-primary)]">{{ formatMoney(rec.amount) }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)]/60 border border-[var(--border-color)]/60 p-2.5">
                <div class="text-xs uppercase tracking-wide text-[var(--text-secondary)]">{{ $t('admin.billing.creditAmount') }}</div>
                <div class="mt-1 font-semibold font-mono text-[var(--text-primary)]">{{ rec.actualAmount !== null ? formatMoney(rec.actualAmount) : '-' }}</div>
                <div v-if="rec.actualAmount !== null" class="mt-1 text-xs text-[var(--text-secondary)]">
                  {{ $t(getRechargeCreditLabelKey(rec.status)) }}
                </div>
              </div>
            </div>

            <div class="space-y-1 text-xs text-[var(--text-secondary)]">
              <div>{{ $t('admin.billing.payChannel') }}: {{ getPayChannelDisplay(rec) }}</div>
              <div>{{ $t('admin.billing.paymentDetails') }}: {{ getRechargePaymentDetailsDisplay(rec) }}</div>
              <div v-if="getRechargeGatewayStatusText(rec)">{{ getRechargeGatewayStatusText(rec) }}</div>
              <div v-if="rec.paymentUuid" class="truncate font-mono" :title="rec.paymentUuid">
                {{ $t('admin.billing.paymentUuid') }} {{ rec.paymentUuid }}
              </div>
              <div v-if="rec.paymentTxid" class="truncate font-mono" :title="rec.paymentTxid">
                {{ $t('admin.billing.paymentTxid') }} {{ rec.paymentTxid }}
              </div>
              <div v-if="rec.tradeNo" class="truncate font-mono" :title="rec.tradeNo">
                {{ $t('admin.billing.tradeNo') }} {{ rec.tradeNo }}
              </div>
            </div>

            <div class="flex justify-end gap-2">
              <Button
                v-if="rec.status === 'pending'"
                size="sm"
                variant="ghost"
                :disabled="syncingRecordId === rec.id"
                :loading="syncingRecordId === rec.id"
                @click="syncRechargeRecord(rec)"
              >
                {{ syncingRecordId === rec.id ? $t('common.syncing') : $t('admin.billing.sync') }}
              </Button>
            </div>
          </div>
        </div>

        <div class="hidden overflow-hidden md:block">
          <table class="w-full table-fixed text-sm">
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.rechargeOrderNo') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.user') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.amount') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.creditAmount') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.payChannel') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.paymentDetails') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.rechargeStatusLabel') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.tradeNo') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('admin.billing.time') }}</th>
                <th class="text-left p-3 whitespace-nowrap">{{ $t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr v-for="rec in rechargeRecords" :key="rec.id" class="border-t border-[var(--border-color)] hover:bg-[var(--bg-surface-soft)]/50 transition-colors">
                <td class="p-3 font-mono text-xs whitespace-nowrap">
                  <span class="cursor-pointer hover:text-[var(--accent)] transition-colors" @click="copyToClipboard(rec.orderNo)">{{ rec.orderNo }}</span>
                </td>
                <td class="p-3 whitespace-nowrap font-medium text-[var(--text-primary)]">{{ rec.user?.username || '-' }}</td>
                <td class="p-3 whitespace-nowrap font-mono font-semibold text-[var(--text-primary)]">{{ formatMoney(rec.amount) }}</td>
                <td class="p-3 whitespace-nowrap font-mono">
                  <div class="font-semibold text-[var(--text-primary)]">{{ rec.actualAmount !== null ? formatMoney(rec.actualAmount) : '-' }}</div>
                  <div v-if="rec.actualAmount !== null" class="mt-0.5 text-xs text-[var(--text-secondary)]">
                    {{ $t(getRechargeCreditLabelKey(rec.status)) }}
                  </div>
                </td>
                <td class="p-3 whitespace-nowrap text-xs text-[var(--text-secondary)]">{{ getPayChannelDisplay(rec) }}</td>
                <td class="p-3 text-xs text-[var(--text-secondary)] max-w-[220px]">
                  <div>{{ getRechargePaymentDetailsDisplay(rec) }}</div>
                  <div v-if="getRechargeGatewayStatusText(rec)" class="mt-1">{{ getRechargeGatewayStatusText(rec) }}</div>
                  <div v-if="rec.paymentUuid" class="mt-1 truncate font-mono" :title="rec.paymentUuid">
                    {{ $t('admin.billing.paymentUuid') }} {{ rec.paymentUuid }}
                  </div>
                  <div v-if="rec.paymentTxid" class="mt-1 truncate font-mono" :title="rec.paymentTxid">
                    {{ $t('admin.billing.paymentTxid') }} {{ rec.paymentTxid }}
                  </div>
                </td>
                <td class="p-3 whitespace-nowrap">
                  <Badge :variant="getRechargeStatusBadgeVariant(rec.status)">{{ $t(`admin.billing.rechargeStatus.${rec.status}`) }}</Badge>
                </td>
                <td class="p-3 font-mono text-xs whitespace-nowrap text-[var(--text-secondary)] max-w-[150px] truncate" :title="rec.tradeNo || ''">
                  <span 
                    v-if="rec.tradeNo" 
                    class="cursor-pointer hover:text-[var(--accent)] transition-colors"
                    @click="copyToClipboard(rec.tradeNo)"
                  >{{ rec.tradeNo }}</span>
                  <span v-else>-</span>
                </td>
                <td class="p-3 text-[var(--text-secondary)] whitespace-nowrap text-xs font-mono">{{ formatDate(rec.createdAt) }}</td>
                <td class="p-3 whitespace-nowrap">
                  <div class="flex items-center gap-2">
                    <Button
                      v-if="rec.status === 'pending'"
                      size="sm"
                      variant="ghost"
                      :disabled="syncingRecordId === rec.id"
                      :loading="syncingRecordId === rec.id"
                      @click="syncRechargeRecord(rec)"
                    >
                      <template #icon>
                        <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': syncingRecordId === rec.id }" />
                      </template>
                      {{ syncingRecordId === rec.id ? $t('common.syncing') : $t('admin.billing.sync') }}
                    </Button>
                    <span v-if="rec.status !== 'pending'" class="text-[var(--text-tertiary)]">-</span>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>

      <Card v-else padding="none">
        <EmptyState
          :title="$t('admin.billing.noRechargeRecords')"
          :description="$t('admin.billing.tabs.rechargeRecords')"
        />
      </Card>

      <!-- 分页 -->
      <Card v-if="rechargeRecords.length > 0" class="mt-4" padding="sm">
        <Pagination
          :page="rechargeRecordsPage"
          :page-size="rechargeRecordsPageSize"
          :total="rechargeRecordsTotal"
          :show-size-changer="true"
          :page-size-options="pageSizeOptions"
          @update:page="rechargeRecordsPage = $event; loadRechargeRecords()"
          @update:page-size="rechargeRecordsPageSize = $event; rechargeRecordsPage = 1; loadRechargeRecords()"
        />
      </Card>
    </div>

    <!-- 财务对账 Tab -->
    <div v-show="activeTab === 'reconciliation'" class="space-y-6">
      <Card padding="md">
        <div class="flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
          <div>
            <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">日对账工作台</h2>
            <p class="mt-1 text-sm text-[var(--text-secondary)]">按业务日汇总充值、余额流水、实例扣费和退款审批。</p>
          </div>
          <div class="flex flex-col gap-3 sm:flex-row sm:items-end">
            <div class="min-w-[180px]">
              <Input
                v-model="reconciliationDate"
                type="date"
                label="对账日期"
                @change="loadFinancialReconciliation"
              />
            </div>
            <Button variant="ghost" :disabled="reconciliationLoading" @click="loadFinancialReconciliation">
              刷新
            </Button>
            <Button variant="primary" :disabled="reconciliationRunning" :loading="reconciliationRunning" @click="runFinancialReconciliation">
              {{ reconciliationRunning ? '对账中...' : '运行对账' }}
            </Button>
          </div>
        </div>
      </Card>

      <div v-if="reconciliationLoading" class="grid gap-4 md:grid-cols-4">
        <Card v-for="i in 4" :key="`reconciliation-loading-${i}`" class="h-28 animate-pulse" padding="sm">
          <div class="h-4 w-20 rounded-md bg-[var(--bg-surface-soft)]"></div>
          <div class="mt-5 h-7 w-28 rounded-md bg-[var(--bg-surface-soft)]"></div>
        </Card>
      </div>

      <template v-else-if="reconciliation">
        <div class="grid gap-4 sm:grid-cols-2 md:grid-cols-5">
          <Card padding="sm">
            <div class="text-xs text-[var(--text-secondary)]">对账状态</div>
            <div class="mt-2.5">
              <Badge :variant="getReconciliationStatusBadgeVariant(reconciliation.status)">
                {{ getReconciliationStatusLabel(reconciliation.status) }}
              </Badge>
            </div>
            <div class="mt-3 text-xs text-[var(--text-tertiary)] font-mono">{{ reconciliation.date }}</div>
          </Card>
          <Card padding="sm">
            <div class="text-xs text-[var(--text-secondary)]">成功充值</div>
            <div class="mt-2 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(getSummaryMoney('recharge')) }}</div>
            <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatCount(getSummaryCount('recharge')) }} 笔</div>
          </Card>
          <Card padding="sm">
            <div class="text-xs text-[var(--text-secondary)]">余额流水</div>
            <div class="mt-2 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(getSummaryMoney('balanceLogs')) }}</div>
            <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatCount(getSummaryCount('balanceLogs')) }} 条</div>
          </Card>
          <Card padding="sm">
            <div class="text-xs text-[var(--text-secondary)]">实例扣费</div>
            <div class="mt-2 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatMoney(getSummaryMoney('instanceBilling')) }}</div>
            <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ formatCount(getSummaryCount('instanceBilling')) }} 条</div>
          </Card>
          <Card padding="sm">
            <div class="text-xs text-[var(--text-secondary)]">差异项</div>
            <div class="mt-2 font-mono text-xl font-semibold tabular-nums text-[var(--text-primary)]">{{ formatCount(reconciliation.summary?.discrepancies?.total || 0) }}</div>
            <div class="mt-1 text-xs text-[var(--text-secondary)]">重跑不会重复写入</div>
          </Card>
        </div>

        <Card padding="md">
          <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <div>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">CSV 导出</h3>
              <p class="mt-1 text-sm text-[var(--text-secondary)]">导出内容已脱敏，不包含回调原文、支付密钥、token 或密码。</p>
            </div>
            <div class="flex flex-wrap gap-2">
              <Button
                v-for="type in ['orders', 'balance_logs', 'adjustments']"
                :key="type"
                size="sm"
                variant="secondary"
                :disabled="!!reconciliationExporting"
                :loading="reconciliationExporting === type"
                @click="exportFinancialReconciliation(type as 'orders' | 'balance_logs' | 'adjustments')"
              >
                {{ reconciliationExporting === type ? '导出中...' : `导出${getReconciliationExportLabel(type)}` }}
              </Button>
            </div>
          </div>
        </Card>

        <Card padding="none">
          <div class="border-b border-[var(--border-color)] px-6 py-4">
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">差异处理</h3>
            <p class="mt-1 text-sm text-[var(--text-secondary)]">差异项可确认或忽略，备注和处理人会保留。</p>
          </div>
          <div v-if="reconciliation.items.length === 0" class="p-8 text-center">
            <EmptyState
              title="没有发现差异"
              description="当前日期对账结果正常。"
            />
          </div>
          <div v-else class="divide-y divide-[var(--border-color)]">
            <div v-for="item in reconciliation.items" :key="item.id" class="grid gap-4 p-6 xl:grid-cols-[minmax(0,1fr)_320px]">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <Badge :variant="getReconciliationStatusBadgeVariant(item.status)">
                    {{ getReconciliationStatusLabel(item.status) }}
                  </Badge>
                  <Badge variant="default">
                    {{ getReconciliationItemTypeLabel(item.itemType) }}
                  </Badge>
                  <span class="text-xs text-[var(--text-tertiary)] font-mono">#{{ item.id }}</span>
                </div>
                <div class="mt-3 text-sm font-semibold text-[var(--text-primary)]">{{ item.title }}</div>
                <div class="mt-2 grid gap-2 text-sm text-[var(--text-secondary)] sm:grid-cols-2 lg:grid-cols-4">
                  <div>用户：{{ item.user?.username || item.userId || '-' }}</div>
                  <div>来源：{{ item.sourceType }} {{ item.sourceId || '' }}</div>
                  <div>金额：{{ item.amount !== null ? formatMoney(item.amount) : '-' }}</div>
                  <div>处理人：{{ item.handledBy?.username || '-' }}</div>
                </div>
                <pre class="mt-3 max-h-28 overflow-auto rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-xs text-[var(--text-secondary)] font-mono leading-relaxed">{{ item.detail }}</pre>
              </div>
              <div class="space-y-3">
                <Select
                  v-model="reconciliationStatusDrafts[item.id]"
                  label="处理状态"
                  size="sm"
                >
                  <option value="discrepancy">差异</option>
                  <option value="confirmed">已确认</option>
                  <option value="ignored">已忽略</option>
                </Select>
                <div class="flex flex-col gap-1.5">
                  <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">处理备注</label>
                  <textarea
                    v-model="reconciliationNotes[item.id]"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all resize-y"
                    rows="3"
                    maxlength="500"
                    placeholder="填写处理结论或忽略原因"
                  ></textarea>
                </div>
                <Button
                  variant="primary"
                  block
                  :disabled="reconciliationSavingId === item.id"
                  :loading="reconciliationSavingId === item.id"
                  @click="saveReconciliationItem(item)"
                >
                  {{ reconciliationSavingId === item.id ? '保存中...' : '保存处理结果' }}
                </Button>
              </div>
            </div>
          </div>
        </Card>
      </template>

      <Card v-else padding="none">
        <EmptyState
          title="还没有生成这一天的对账结果"
          description="点击“运行对账”生成日对账任务。"
        />
      </Card>
    </div>

    <!-- 支付渠道 Tab -->
    <div v-show="activeTab === 'paymentProviders'">
      <PaymentProvidersView v-if="hasVisitedPaymentProviders" embedded />
    </div>

    <!-- AFF 转化 Tab -->
    <div v-show="activeTab === 'affConversions'">
      <AffReviewView v-if="hasVisitedAffConversions" embedded />
    </div>

    <!-- 操作弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showActionModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showActionModal = false"></div>
          <div class="modal-content max-w-md">
            <div class="modal-header">
              <h3 class="modal-title">{{ $t(`admin.billing.${actionType}Title`) }}</h3>
              <button
                type="button"
                class="p-1 rounded-lg text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showActionModal = false"
              >
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            
            <div class="modal-body space-y-4">
              <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm">
                <span class="text-[var(--text-secondary)]">{{ $t('admin.billing.targetInstance') }}:</span>
                <strong class="ml-1 text-[var(--text-primary)]">{{ actionTarget?.name }}</strong>
              </div>

              <!-- 封停/解封 -->
              <template v-if="actionType === 'suspend'">
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.reason') }}</label>
                  <textarea
                    v-model="actionForm.reason"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all resize-y"
                    rows="3"
                    :placeholder="$t('admin.billing.reasonPlaceholder')"
                  ></textarea>
                </div>
              </template>

              <!-- 延期 -->
              <template v-else-if="actionType === 'extend'">
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.extendDays') }}</label>
                  <input
                    v-model.number="actionForm.days"
                    type="number"
                    min="1"
                    max="365"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all font-mono"
                  />
                </div>
                <div class="flex items-center gap-2">
                  <input
                    id="freeExtend"
                    v-model="actionForm.freeExtend"
                    type="checkbox"
                    class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                  />
                  <label for="freeExtend" class="text-sm text-[var(--text-secondary)] cursor-pointer">{{ $t('admin.billing.freeExtend') }}</label>
                </div>
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.reason') }}</label>
                  <input
                    v-model="actionForm.reason"
                    type="text"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all"
                    :placeholder="$t('admin.billing.reasonPlaceholder')"
                  />
                </div>
              </template>

              <!-- 删除退款 -->
              <template v-else-if="actionType === 'deleteRefund'">
                <div class="p-3 bg-[var(--error-soft)] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_20%,transparent)] rounded-xl text-sm">
                  {{ $t('admin.billing.deleteRefundWarning') }}
                </div>
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.refundTypeLabel') }} *</label>
                  <div class="flex flex-col gap-2">
                    <label class="flex items-center gap-2 cursor-pointer p-2.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:bg-[var(--bg-surface)] transition-colors">
                      <input v-model="actionForm.refundType" type="radio" value="remaining" class="h-4 w-4 border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]" />
                      <span class="text-sm text-[var(--text-primary)]">{{ $t('admin.billing.refundTypeRemaining') }}</span>
                    </label>
                    <label class="flex items-center gap-2 cursor-pointer p-2.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:bg-[var(--bg-surface)] transition-colors">
                      <input v-model="actionForm.refundType" type="radio" value="full" class="h-4 w-4 border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]" />
                      <span class="text-sm text-[var(--text-primary)]">{{ $t('admin.billing.refundTypeFull') }}</span>
                    </label>
                  </div>
                </div>
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.reason') }} *</label>
                  <textarea
                    v-model="actionForm.reason"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all resize-y"
                    rows="3"
                    :placeholder="$t('admin.billing.deleteRefundReasonPlaceholder')"
                  ></textarea>
                </div>
              </template>

              <!-- 应用折扣 -->
              <template v-if="actionType === 'applyDiscount'">
                <div class="p-3 bg-[var(--bg-surface-soft)] rounded-xl border border-[var(--border-color)]">
                  <p class="text-sm text-[var(--text-secondary)]">
                    {{ $t('admin.billing.applyDiscountHint') }}
                  </p>
                </div>
                <div class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.affCodeLabel') }} *</label>
                  <input 
                    v-model="actionForm.affCode" 
                    type="text" 
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all uppercase" 
                    :placeholder="$t('admin.billing.affCodePlaceholder')"
                  />
                </div>
              </template>
            </div>
            
            <div class="modal-footer flex flex-row items-center justify-end gap-2">
              <Button variant="ghost" @click="showActionModal = false">{{ $t('common.cancel') }}</Button>
              <!-- 删除并退款模式下显示仅数据库删除按钮 -->
              <Button
                v-if="actionType === 'deleteRefund'"
                variant="secondary"
                class="!text-[var(--warning)] hover:!bg-[var(--warning-soft)]"
                :disabled="actionLoading"
                :loading="actionLoading"
                @click="actionType = 'deleteRefundDatabaseOnly'; executeAction()"
              >
                {{ actionLoading ? $t('common.processing') : $t('admin.billing.databaseOnlyDelete') }}
              </Button>
              <Button
                :variant="actionType === 'suspend' || actionType === 'deleteRefund' || actionType === 'deleteRefundDatabaseOnly' ? 'danger' : 'primary'"
                :disabled="actionLoading"
                :loading="actionLoading"
                @click="executeAction"
              >
                {{ actionLoading ? $t('common.processing') : $t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 批量修改价格弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showBatchPriceModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showBatchPriceModal = false"></div>
          <div class="modal-content max-w-3xl max-h-[90vh] overflow-hidden flex flex-col">
            <div class="modal-header">
              <h3 class="modal-title">{{ $t('admin.billing.batchUpdatePrice') }}</h3>
              <button
                type="button"
                class="p-1 rounded-lg text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showBatchPriceModal = false"
              >
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>

            <div class="modal-body space-y-4 overflow-y-auto flex-1">
              <div class="grid gap-3 sm:grid-cols-3">
                <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5">
                  <div class="text-xs font-medium text-[var(--text-tertiary)] uppercase tracking-wider">{{ $t('admin.billing.batchSelected') }}</div>
                  <div class="mt-1 text-lg font-bold font-mono text-[var(--text-primary)]">{{ selectedInstancesCount }}</div>
                </div>
                <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5">
                  <div class="text-xs font-medium text-[var(--text-tertiary)] uppercase tracking-wider">{{ $t('admin.billing.batchPreviewChanged') }}</div>
                  <div class="mt-1 text-lg font-bold font-mono text-[var(--text-primary)]">{{ batchPricePreview?.summary.changedCount || 0 }}</div>
                </div>
                <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5">
                  <div class="text-xs font-medium text-[var(--text-tertiary)] uppercase tracking-wider">{{ $t('admin.billing.batchPreviewFailed') }}</div>
                  <div
                    class="mt-1 text-lg font-bold font-mono"
                    :class="(batchPricePreview?.summary.failedCount || 0) > 0 ? 'text-[var(--error)]' : 'text-[var(--text-primary)]'"
                  >
                    {{ batchPricePreview?.summary.failedCount || 0 }}
                  </div>
                </div>
              </div>

              <div class="space-y-1.5">
                <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.newPrice') }} *</label>
                <div class="relative">
                  <span class="absolute left-3.5 top-1/2 -translate-y-1/2 text-[var(--text-tertiary)] font-mono text-sm">¥</span>
                  <input
                    v-model.number="batchPriceForm.newPrice"
                    type="number"
                    step="0.01"
                    min="0"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] pl-8 pr-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all font-mono"
                    :placeholder="$t('admin.billing.enterNewPrice')"
                  />
                </div>
                <p class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.batchPriceHint') }}</p>
              </div>

              <div class="flex items-center gap-3">
                <input
                  id="batchSettleBalance"
                  v-model="batchPriceForm.settleBalance"
                  type="checkbox"
                  class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                />
                <label for="batchSettleBalance" class="text-sm text-[var(--text-secondary)] cursor-pointer">
                  {{ $t('admin.billing.settleBalance') }}
                </label>
              </div>

              <div v-if="batchPricePreviewLoading" class="p-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-sm text-[var(--text-tertiary)]">
                {{ $t('common.loading') }}
              </div>

              <template v-else-if="batchPricePreview">
                <div
                  class="rounded-xl border p-3.5"
                  :class="batchPricePreview.canSubmit ? 'border-[color-mix(in_srgb,var(--accent)_20%,transparent)] bg-[var(--accent-soft)]' : 'border-[color-mix(in_srgb,var(--error)_20%,transparent)] bg-[var(--error-soft)]'"
                >
                  <div class="grid gap-3 sm:grid-cols-3">
                    <div>
                      <div class="text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.batchTotalCharge') }}</div>
                      <div class="mt-1 text-base font-bold font-mono text-[var(--error)]">{{ formatMoney(batchPricePreview.summary.totalCharge) }}</div>
                    </div>
                    <div>
                      <div class="text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.batchTotalRefund') }}</div>
                      <div class="mt-1 text-base font-bold font-mono text-[var(--success)]">{{ formatMoney(batchPricePreview.summary.totalRefund) }}</div>
                    </div>
                    <div>
                      <div class="text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.batchNetAmount') }}</div>
                      <div class="mt-1 text-base font-bold font-mono text-[var(--text-primary)]">{{ formatPriceDiff(batchPricePreview.summary.netAmount) }}</div>
                    </div>
                  </div>
                  <p v-if="!batchPricePreview.canSubmit" class="mt-3 text-sm font-medium text-[var(--error)]">
                    {{ $t('admin.billing.batchPreviewBlocked') }}
                  </p>
                </div>

                <div v-if="batchPricePreview.userImpacts.length > 0" class="rounded-xl border border-[var(--border-color)] overflow-hidden">
                  <div class="bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ $t('admin.billing.batchUserImpact') }}</div>
                  <div class="max-h-48 overflow-y-auto">
                    <div
                      v-for="impact in batchPricePreview.userImpacts"
                      :key="impact.userId"
                      class="grid grid-cols-2 gap-2 border-t border-[var(--border-color)] px-3.5 py-2.5 text-sm md:grid-cols-4"
                    >
                      <div>
                        <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.user') }}</div>
                        <div class="truncate font-medium text-[var(--text-primary)]" :title="impact.username">{{ impact.username }}</div>
                      </div>
                      <div>
                        <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.userBalance') }}</div>
                        <div class="font-mono text-xs text-[var(--text-secondary)]">{{ formatMoney(impact.balanceBefore) }}</div>
                      </div>
                      <div>
                        <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.priceDifference') }}</div>
                        <div class="font-mono text-xs font-medium" :class="impact.netDiff > 0 ? 'text-[var(--error)]' : impact.netDiff < 0 ? 'text-[var(--success)]' : 'text-[var(--text-secondary)]'">{{ formatPriceDiff(impact.netDiff) }}</div>
                      </div>
                      <div>
                        <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.userBalance') }}</div>
                        <div class="font-mono text-xs" :class="impact.insufficientBalance ? 'text-[var(--error)] font-medium' : 'text-[var(--text-primary)]'">{{ formatMoney(impact.balanceAfter) }}</div>
                      </div>
                    </div>
                  </div>
                </div>

                <div class="rounded-xl border border-[var(--border-color)] overflow-hidden">
                  <div class="bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ $t('admin.billing.batchPreviewDetails') }}</div>
                  <div class="max-h-64 overflow-y-auto">
                    <div class="space-y-2 p-3 lg:hidden">
                      <div
                        v-for="item in batchPricePreview.items"
                        :key="item.id"
                        class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3 text-sm"
                      >
                        <div class="flex items-start justify-between gap-3">
                          <div class="min-w-0">
                            <div class="truncate font-medium text-[var(--text-primary)]" :title="item.name || ''">{{ item.name || '-' }}</div>
                            <div class="text-xs text-[var(--text-tertiary)] font-mono">#{{ item.id }} · {{ item.user?.username || '-' }}</div>
                          </div>
                          <Badge :variant="getBatchPriceItemBadgeVariant(item.status)" size="sm">
                            {{ item.error || getBatchPriceItemStatusLabel(item.status) }}
                          </Badge>
                        </div>
                        <div class="mt-3 grid grid-cols-3 gap-2 text-right">
                          <div>
                            <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.currentPrice') }}</div>
                            <div class="font-mono text-xs text-[var(--text-primary)]">{{ item.oldPrice !== null ? formatMoney(item.oldPrice) : '-' }}</div>
                          </div>
                          <div>
                            <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.newPrice') }}</div>
                            <div class="font-mono text-xs text-[var(--text-primary)]">{{ formatMoney(item.newPrice) }}</div>
                          </div>
                          <div>
                            <div class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.priceDifference') }}</div>
                            <div class="font-mono text-xs" :class="item.priceDiff > 0 ? 'text-[var(--error)]' : item.priceDiff < 0 ? 'text-[var(--success)]' : 'text-[var(--text-tertiary)]'">
                              {{ formatPriceDiff(item.priceDiff) }}
                            </div>
                          </div>
                        </div>
                      </div>
                    </div>

                    <table class="hidden w-full table-fixed text-sm lg:table">
                      <thead class="bg-[var(--bg-surface-soft)]">
                        <tr>
                          <th class="w-[24%] text-left p-2.5">{{ $t('admin.billing.instance') }}</th>
                          <th class="w-[16%] text-left p-2.5">{{ $t('admin.billing.user') }}</th>
                          <th class="w-[14%] text-right p-2.5">{{ $t('admin.billing.currentPrice') }}</th>
                          <th class="w-[14%] text-right p-2.5">{{ $t('admin.billing.newPrice') }}</th>
                          <th class="w-[14%] text-right p-2.5">{{ $t('admin.billing.priceDifference') }}</th>
                          <th class="w-[18%] text-left p-2.5">{{ $t('admin.billing.result') }}</th>
                        </tr>
                      </thead>
                      <tbody class="divide-y divide-[var(--border-color)]">
                        <tr v-for="item in batchPricePreview.items" :key="item.id" class="hover:bg-[var(--bg-surface-soft)]/50 transition-colors">
                          <td class="p-2.5">
                            <div class="truncate font-medium text-[var(--text-primary)]" :title="item.name || ''">{{ item.name || '-' }}</div>
                            <div class="text-xs text-[var(--text-tertiary)] font-mono">#{{ item.id }}</div>
                          </td>
                          <td class="p-2.5 text-[var(--text-secondary)] truncate" :title="item.user?.username || ''">{{ item.user?.username || '-' }}</td>
                          <td class="p-2.5 text-right font-mono">{{ item.oldPrice !== null ? formatMoney(item.oldPrice) : '-' }}</td>
                          <td class="p-2.5 text-right font-mono">{{ formatMoney(item.newPrice) }}</td>
                          <td class="p-2.5 text-right font-mono" :class="item.priceDiff > 0 ? 'text-[var(--error)]' : item.priceDiff < 0 ? 'text-[var(--success)]' : 'text-[var(--text-secondary)]'">
                            {{ formatPriceDiff(item.priceDiff) }}
                          </td>
                          <td class="p-2.5">
                            <Badge :variant="getBatchPriceItemBadgeVariant(item.status)" size="sm">
                              {{ item.error || getBatchPriceItemStatusLabel(item.status) }}
                            </Badge>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>
              </template>

              <div v-else class="p-3.5 rounded-xl border border-[var(--border-color)] text-sm text-[var(--text-tertiary)]">
                {{ $t('admin.billing.batchPreviewWaiting') }}
              </div>

              <div v-if="!batchPriceForm.settleBalance" class="p-3.5 bg-[var(--warning-soft)] rounded-xl border border-[color-mix(in_srgb,var(--warning)_20%,transparent)]">
                <p class="text-sm text-[var(--warning)]">
                  {{ $t('admin.billing.noSettleHint') }}
                </p>
              </div>
            </div>

            <div class="modal-footer flex flex-row items-center justify-end gap-2">
              <Button variant="ghost" :disabled="batchPriceLoading" @click="showBatchPriceModal = false">{{ $t('common.cancel') }}</Button>
              <Button
                variant="primary"
                :disabled="batchPriceLoading || batchPricePreviewLoading || !isValidPriceInput(batchPriceForm.newPrice) || !batchPricePreview?.canSubmit || !batchPricePreviewHasVersionSnapshot"
                :loading="batchPriceLoading"
                @click="submitBatchUpdatePrice"
              >
                {{ batchPriceLoading ? $t('common.processing') : $t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 修改价格弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showPriceModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showPriceModal = false"></div>
          <div class="modal-content max-w-md">
            <div class="modal-header">
              <h3 class="modal-title">{{ $t('admin.billing.updatePrice') }}</h3>
              <button
                type="button"
                class="p-1 rounded-lg text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showPriceModal = false"
              >
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            
            <div class="modal-body space-y-4">
              <!-- 实例信息 -->
              <div class="p-3.5 bg-[var(--bg-surface-soft)] rounded-xl border border-[var(--border-color)] space-y-2">
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.instanceName') }}:</span>
                  <span class="font-medium text-[var(--text-primary)]">{{ priceTarget?.name }}</span>
                </div>
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.owner') }}:</span>
                  <span class="text-[var(--text-secondary)]">{{ priceTarget?.user?.username }}</span>
                </div>
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.currentPrice') }}:</span>
                  <span class="font-mono font-medium text-[var(--text-primary)]">¥{{ (priceTarget?.billingPrice || 0).toFixed(2) }}</span>
                </div>
                <!-- 优惠码状态 -->
                <div v-if="priceTarget?.hasAffBinding && priceTarget?.affDiscountRate > 0" class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.affDiscount') }}:</span>
                  <span class="text-[var(--success)] font-medium font-mono">-{{ (priceTarget.affDiscountRate * 100).toFixed(0) }}%</span>
                </div>
                <!-- 实际续费价（如果有折扣） -->
                <div v-if="priceTarget?.hasAffBinding && priceTarget?.affDiscountRate > 0" class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.actualRenewPrice') }}:</span>
                  <span class="font-mono font-medium text-[var(--success)]">¥{{ actualRenewPrice.old.toFixed(2) }}</span>
                </div>
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.cycle') }}:</span>
                  <span class="text-[var(--text-secondary)]">{{ formatBillingCycle(priceTarget?.billingCycle || 1) }}</span>
                </div>
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.remainingDays') }}:</span>
                  <span :class="priceTarget?.remainingDays <= 7 ? 'text-[var(--warning)] font-medium' : 'text-[var(--text-secondary)]'">
                    {{ Math.ceil(pricePreview?.remainingDays || priceTarget?.remainingDays || 0) }} {{ $t('admin.billing.days') }}
                  </span>
                </div>
                <div class="flex justify-between text-sm">
                  <span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.userBalance') }}:</span>
                  <span class="font-mono text-[var(--text-secondary)]">¥{{ (priceTarget?.user?.balance || 0).toFixed(2) }}</span>
                </div>
              </div>
              
              <!-- 优惠码提示 -->
              <div v-if="priceTarget?.hasAffBinding && priceTarget?.affDiscountRate > 0" class="p-3 bg-[var(--accent-soft)] rounded-xl border border-[color-mix(in_srgb,var(--accent)_20%,transparent)]">
                <p class="text-sm text-[var(--accent)]">
                  {{ $t('admin.billing.affAppliedHint', { discount: (priceTarget.affDiscountRate * 100).toFixed(0) }) }}
                </p>
              </div>
              
              <!-- 新价格输入 -->
              <div class="space-y-1.5">
                <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.newPrice') }} *</label>
                <div class="relative">
                  <span class="absolute left-3.5 top-1/2 -translate-y-1/2 text-[var(--text-tertiary)] font-mono text-sm">¥</span>
                  <input 
                    v-model.number="priceForm.newPrice" 
                    type="number" 
                    step="0.01" 
                    min="0" 
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] pl-8 pr-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all font-mono" 
                    :placeholder="$t('admin.billing.enterNewPrice')"
                  />
                </div>
                <p class="text-xs text-[var(--text-tertiary)]">{{ $t('admin.billing.priceHint') }}</p>
                <!-- 新价格的实际续费价（如果有折扣） -->
                <p v-if="priceTarget?.hasAffBinding && priceTarget?.affDiscountRate > 0 && priceForm.newPrice > 0" class="text-xs text-[var(--success)] mt-1 font-mono">
                  → {{ $t('admin.billing.newActualPrice') }}: ¥{{ actualRenewPrice.new.toFixed(2) }}
                </p>
              </div>
              
              <!-- 是否结算差价 -->
              <div class="flex items-center gap-3">
                <input 
                  id="settleBalance" 
                  v-model="priceForm.settleBalance" 
                  type="checkbox"
                  class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                />
                <label for="settleBalance" class="text-sm text-[var(--text-secondary)] cursor-pointer">
                  {{ $t('admin.billing.settleBalance') }}
                </label>
              </div>
              
              <!-- 差价计算结果 -->
              <div v-if="pricePreviewLoading" class="p-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-sm text-[var(--text-tertiary)]">
                {{ $t('common.loading') }}
              </div>

              <div
                v-else-if="priceForm.settleBalance && (pricePreview?.remainingDays || 0) > 0 && priceForm.newPrice !== priceTarget?.billingPrice"
                class="p-3.5 rounded-xl border"
                :class="priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0) ? 'bg-[var(--error-soft)] border-[color-mix(in_srgb,var(--error)_20%,transparent)]' : 'bg-[var(--accent-soft)] border-[color-mix(in_srgb,var(--accent)_20%,transparent)]'"
              >
                <div class="flex justify-between items-center">
                  <span class="text-sm font-medium" :class="priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0) ? 'text-[var(--error)]' : 'text-[var(--accent)]'">
                    {{ priceDiff > 0 ? $t('admin.billing.needPay') : $t('admin.billing.willRefund') }}:
                  </span>
                  <span class="text-xl font-bold font-mono" :class="priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0) ? 'text-[var(--error)]' : 'text-[var(--accent)]'">
                    ¥{{ Math.abs(priceDiff).toFixed(2) }}
                  </span>
                </div>
                <p v-if="priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0)" class="text-xs text-[var(--error)] mt-2 font-medium">
                  {{ $t('admin.billing.insufficientBalance') }}
                </p>
                <p v-else class="text-xs mt-1" :class="priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0) ? 'text-[var(--error)]' : 'text-[var(--accent)]'">
                  {{ $t('admin.billing.priceDiffHint', { days: Math.ceil(pricePreview?.remainingDays || 0) }) }}
                </p>
              </div>
              
              <!-- 不结算差价提示 -->
              <div v-if="!priceForm.settleBalance" class="p-3.5 bg-[var(--warning-soft)] rounded-xl border border-[color-mix(in_srgb,var(--warning)_20%,transparent)]">
                <p class="text-sm text-[var(--warning)]">
                  {{ $t('admin.billing.noSettleHint') }}
                </p>
              </div>
            </div>
            
            <div class="modal-footer flex flex-row items-center justify-end gap-2">
              <Button variant="ghost" :disabled="priceLoading" @click="showPriceModal = false">{{ $t('common.cancel') }}</Button>
              <Button
                variant="primary"
                :disabled="priceLoading || pricePreviewLoading || !isValidPriceInput(priceForm.newPrice) || (priceForm.settleBalance && priceDiff > 0 && priceDiff > (pricePreview?.userBalance || priceTarget?.user?.balance || 0))"
                :loading="priceLoading"
                @click="submitUpdatePrice"
              >
                {{ priceLoading ? $t('common.processing') : $t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 切换方案弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showUpgradeModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showUpgradeModal = false"></div>
          <div class="modal-content max-w-lg">
            <div class="modal-header">
              <h3 class="modal-title">{{ $t('admin.billing.upgradePlanTitle') }}</h3>
              <button
                type="button"
                class="p-1 rounded-lg text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showUpgradeModal = false"
              >
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            
            <div class="modal-body space-y-4">
              <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm">
                <span class="text-[var(--text-secondary)]">{{ $t('admin.billing.targetInstance') }}:</span>
                <strong class="ml-1 text-[var(--text-primary)]">{{ upgradeTarget?.name }}</strong>
              </div>

              <!-- 加载中 -->
              <div v-if="upgradeLoading" class="py-8 text-center">
                <LoadingSpinner size="xl" color="primary" class="mx-auto" />
                <p class="mt-2 text-sm text-[var(--text-tertiary)]">{{ $t('common.loading') }}</p>
              </div>

              <template v-else-if="upgradeData.currentPlan">
                <!-- 当前方案信息 -->
                <div class="p-3.5 bg-[var(--bg-surface-soft)] rounded-xl border border-[var(--border-color)]">
                  <h4 class="text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider mb-2.5">{{ $t('admin.billing.currentPlan') }}</h4>
                  <div class="grid grid-cols-2 gap-2.5 text-sm">
                    <div><span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.planName') }}:</span> <strong class="ml-1 text-[var(--text-primary)]">{{ upgradeData.currentPlan.name }}</strong></div>
                    <div><span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.monthlyPrice') }}:</span> <strong class="ml-1 text-[var(--text-primary)] font-mono">¥{{ upgradeData.currentPlan.monthlyPrice.toFixed(2) }}</strong></div>
                    <div><span class="text-[var(--text-tertiary)]">CPU:</span> <strong class="ml-1 text-[var(--text-primary)] font-mono">{{ upgradeData.currentPlan.cpu }}%</strong></div>
                    <div><span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.memoryLabel') }}:</span> <strong class="ml-1 text-[var(--text-primary)] font-mono">{{ upgradeData.currentPlan.memory }}MB</strong></div>
                    <div><span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.diskLabel') }}:</span> <strong class="ml-1 text-[var(--text-primary)] font-mono">{{ upgradeData.currentPlan.disk }}MB</strong></div>
                    <div><span class="text-[var(--text-tertiary)]">{{ $t('admin.billing.remainingDays') }}:</span> <strong class="ml-1 text-[var(--text-primary)] font-mono">{{ Math.ceil(upgradeData.remainingDays) }} {{ $t('admin.billing.days') }}</strong></div>
                  </div>
                </div>

                <!-- 可升级方案列表 -->
                <div v-if="upgradeData.availablePlans.length > 0" class="space-y-1.5">
                  <label class="block text-xs font-medium text-[var(--text-secondary)]">{{ $t('admin.billing.selectNewPlan') }} *</label>
                  <div class="space-y-2 max-h-48 overflow-y-auto pr-1">
                    <label
                      v-for="plan in upgradeData.availablePlans"
                      :key="plan.id"
                      class="flex items-start gap-3 p-3 rounded-xl border transition-all"
                      :class="[
                        plan.canUpgrade === false
                          ? 'cursor-not-allowed opacity-60 border-[var(--error)]/30 bg-[var(--error-soft)]/50'
                          : 'cursor-pointer hover:bg-[var(--bg-surface-soft)]',
                        selectedPlanId === plan.id
                          ? 'border-[var(--accent)] bg-[var(--accent-soft)]'
                          : 'border-[var(--border-color)] bg-[var(--bg-surface)]'
                      ]"
                    >
                      <input
                        v-model="selectedPlanId"
                        type="radio"
                        :value="plan.id"
                        class="h-4 w-4 mt-1 border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
                        :disabled="plan.canUpgrade === false"
                      />
                      <div class="flex-1 min-w-0">
                        <div class="font-medium text-sm text-[var(--text-primary)]">{{ plan.name }}</div>
                        <div class="text-xs text-[var(--text-secondary)] font-mono">
                          ¥{{ plan.price.toFixed(2) }} / {{ formatBillingCycle(plan.billingCycle) }}
                          (¥{{ plan.monthlyPrice.toFixed(2) }}/{{ $t('admin.billing.month') }})
                        </div>
                        <div class="text-xs text-[var(--text-tertiary)] mt-1 font-mono">
                          CPU {{ plan.cpu }}% | {{ plan.memory }}MB RAM | {{ plan.disk }}MB {{ $t('admin.billing.diskLabel') }}
                        </div>
                        <div v-if="plan.canUpgrade === false && plan.resourceWarnings?.length" class="mt-2 text-xs text-[var(--error)] font-medium">
                          {{ plan.resourceWarnings[0] }}
                        </div>
                      </div>
                    </label>
                  </div>
                </div>
                <div v-else class="p-4 text-center text-sm text-[var(--text-tertiary)] bg-[var(--bg-surface-soft)] rounded-xl border border-[var(--border-color)]">
                  {{ $t('admin.billing.noAvailablePlans') }}
                </div>

                <!-- 差价计算结果 -->
                <div
                  v-if="selectedPlanId"
                  class="p-3.5 rounded-xl border"
                  :class="upgradePriceDiff > upgradeData.userBalance ? 'bg-[var(--error-soft)] border-[color-mix(in_srgb,var(--error)_20%,transparent)]' : 'bg-[var(--accent-soft)] border-[color-mix(in_srgb,var(--accent)_20%,transparent)]'"
                >
                  <div class="flex justify-between items-center">
                    <span class="text-sm font-medium" :class="upgradePriceDiff > upgradeData.userBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'">{{ $t('admin.billing.priceDifference') }}:</span>
                    <span class="text-xl font-bold font-mono" :class="upgradePriceDiff > upgradeData.userBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'">¥{{ upgradePriceDiff.toFixed(2) }}</span>
                  </div>
                  <div class="flex justify-between items-center mt-1 text-sm">
                    <span :class="upgradePriceDiff > upgradeData.userBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'">{{ $t('admin.billing.userBalance') }}:</span>
                    <span class="font-medium font-mono" :class="upgradePriceDiff > upgradeData.userBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'">¥{{ upgradeData.userBalance.toFixed(2) }}</span>
                  </div>
                  <p v-if="upgradePriceDiff > upgradeData.userBalance" class="text-xs text-[var(--error)] mt-2 font-medium">
                    {{ $t('admin.billing.insufficientBalance') }}
                  </p>
                  <p v-else-if="selectedUpgradePlanBlocked" class="text-xs text-[var(--error)] mt-2 font-medium">
                    {{ selectedUpgradePlan?.resourceWarnings?.[0] || '实例所在节点资源不足，无法升级方案' }}
                  </p>
                  <p v-else class="text-xs mt-1" :class="upgradePriceDiff > upgradeData.userBalance ? 'text-[var(--error)]' : 'text-[var(--accent)]'">
                    {{ $t('admin.billing.priceDifferenceHint') }}
                  </p>
                </div>
              </template>
            </div>
            
            <div class="modal-footer flex flex-row items-center justify-end gap-2">
              <Button variant="ghost" :disabled="upgradeSubmitting" @click="showUpgradeModal = false">{{ $t('common.cancel') }}</Button>
              <Button
                variant="primary"
                :disabled="upgradeSubmitting || !selectedPlanId || selectedUpgradePlanBlocked || upgradeData.availablePlans.length === 0"
                :loading="upgradeSubmitting"
                @click="executeUpgrade"
              >
                {{ upgradeSubmitting ? $t('common.processing') : $t('admin.billing.confirmUpgrade') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
/* Nimbus console: mono, uppercased column headers on the locked billing data tables */
thead th {
  font-family: 'JetBrains Mono', ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--kawaii-faint);
}

/* Tabular figures keep amounts, ids, and dates vertically aligned across rows */
tbody td {
  font-variant-numeric: tabular-nums;
}

@media (prefers-reduced-motion: reduce) {
  *,
  ::before,
  ::after {
    animation-duration: 0.001ms !important;
    transition-duration: 0.001ms !important;
  }
}
</style>
