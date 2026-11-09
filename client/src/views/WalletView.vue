<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter, useRoute } from 'vue-router'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { useConfigStore } from '@/stores/config'
import TermsOfServiceModal from '@/components/TermsOfServiceModal.vue'
import { freeSiteCopy } from '@/utils/freeSiteFun'
import { instanceDetailPath, walletPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import { PageHeader, Card, Button, Input, Badge, EmptyState, Pagination } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  CreditCard,
  Plus,
  Gift,
  Copy,
  Trash2,
  Trophy,
  AlertCircle,
  Coins,
  Share2,
  X,
  Loader2,
  Info
} from 'lucide-vue-next'

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const { t } = useI18n()
const toast = useToast()
const router = useRouter()
const route = useRoute()
const configStore = useConfigStore()

type WalletTab = 'balance' | 'logs' | 'records' | 'aff'
type MetricTone = 'slate' | 'emerald' | 'rose' | 'amber' | 'blue' | 'violet'

interface WalletMetric {
  label: string
  value: string
  tone: MetricTone
}

interface ManualPaymentInfo {
  orderNo: string
  payableAmount: number
  actualAmount: number | null
  providerName?: string | null
  instructions: string
}

// 当前 TAB
const activeTab = ref<WalletTab>('balance')
const walletTabs = computed((): Array<{ key: WalletTab; label: string }> => [
  { key: 'balance', label: configStore.freeSiteMode ? freeSiteCopy.walletBalanceTab : t('wallet.tabs.balance') },
  { key: 'logs', label: configStore.freeSiteMode ? freeSiteCopy.walletLogsTab : t('wallet.tabs.logs') },
  ...(
    configStore.freeSiteMode
      ? []
      : [
          { key: 'records' as WalletTab, label: t('wallet.tabs.records') },
          { key: 'aff' as WalletTab, label: t('aff.title') }
        ]
  )
])

// 余额数据
const balance = ref({
  balance: 0,
  totalRecharge: 0,
  totalConsume: 0,
  destroyedValue: 0
})
const loading = ref(true)

// 余额日志
const logs = ref<any[]>([])
const logsLoading = ref(false)
const logsPage = ref(1)
const logsPageSize = ref(10)
const logsTotal = ref(0)
const showLotteryGiftOnly = ref(false) // 抽奖赠送筛选：false=排除抽奖赠送, true=仅显示抽奖赠送

// 充值弹窗
const showRechargeModal = ref(false)
const rechargeLoading = ref(false)
const providersLoading = ref(false)
const providers = ref<any[]>([])
const selectedProvider = ref<number | null>(null)
const rechargeAmount = ref(10)
// 新增：支付方式选择
const selectedPaymentMethod = ref<string>('')
// 服务条款勾选
const agreedToTerms = ref(false)
const showTermsModal = ref(false)
// 无退款确认
const agreedToNoRefund = ref(false)
const agreedToRechargeNotice = ref(false)
const showManualPaymentModal = ref(false)
const manualPaymentInfo = ref<ManualPaymentInfo | null>(null)
let providersLoadSeq = 0

// 充值记录
const rechargeRecords = ref<any[]>([])
const recordsLoading = ref(false)
const recordsPage = ref(1)
const recordsPageSize = ref(10)
const recordsTotal = ref(0)

// 操作加载状态
const repayLoading = ref<string | null>(null)
const cancelLoading = ref<string | null>(null)

// AFF 推荐计划数据
interface AffStatus {
  activated: boolean
  totalEarnings: number
  totalConverted: number
  currentBalance: number
  totalCodes: number
  totalUsed: number
}
interface AffCode {
  id: number
  code: string
  packagePlanId: number | null
  planName: string | null
  packageName: string | null
  price: number | null
  discountRate: number
  commissionRate: number
  usedCount: number
  totalEarnings: number
  createdAt: string
  isGlobal: boolean
}
interface AffPlan {
  id: number
  name: string
  packageName: string
  price: string
  hasCode: boolean
}
interface AffLog {
  id: number
  type: string
  amount: number
  originalAmount: number | null
  balanceBefore: number
  balanceAfter: number
  instanceId: number | null
  affCodeId: number | null
  affCode: string | null
  remark: string | null
  createdAt: string
}
interface AffWithdrawal {
  id: number
  amount: number
  status: string
  rejectReason: string | null
  createdAt: string
  reviewedAt: string | null
}

const affStatus = ref<AffStatus | null>(null)
const affCodes = ref<AffCode[]>([])
const affPlans = ref<AffPlan[]>([])
const affLogs = ref<AffLog[]>([])
const affWithdrawals = ref<AffWithdrawal[]>([])
const affLoading = ref(false)
const affCodesLoading = ref(false)
const affPlansLoading = ref(false)
const affLogsLoading = ref(false)
const affWithdrawalsLoading = ref(false)
const affLogsPage = ref(1)
const affLogsPageSize = ref(10)
const affLogsTotal = ref(0)
const affWithdrawalsPage = ref(1)
const affWithdrawalsPageSize = ref(10)
const affWithdrawalsTotal = ref(0)

// 创建优惠码弹窗
const showCreateCodeModal = ref(false)
const createCodeLoading = ref(false)
const selectedPlanId = ref<number | null>(null)

// 删除优惠码
const deleteCodeLoading = ref<number | null>(null)

// 转化申请弹窗
const showConvertModal = ref(false)
const convertLoading = ref(false)
const convertAmount = ref<number>(0)

// AFF 榜单弹窗
interface LeaderboardEntry {
  rank: number
  username: string
  totalEarnings: number
  isCurrentUser: boolean
}
const showLeaderboardModal = ref(false)
const leaderboardLoading = ref(false)
const leaderboard = ref<LeaderboardEntry[]>([])

// 充值验证状态
const verifyingRecharge = ref(false)

onMounted(async () => {
  await configStore.loadPublicConfig()
  loadBalance()
  
  // 处理支付回调参数
  if (!configStore.freeSiteMode && route.query.recharge === 'success') {
    const orderNo = route.query.out_trade_no as string
    
    // 清除 URL 参数，避免刷新页面重复处理
    router.replace({ path: walletPath() })
    
    // 切换到充值记录标签页
    switchTab('records')
    
    if (orderNo) {
      // 有订单号，主动查询易支付订单状态
      await verifyRechargeOrder(orderNo)
    } else {
      // 没有订单号，显示等待到账提示
      toast.info(t('wallet.verifyingPayment'))
      // 刷新余额和记录
      loadBalance()
      loadRecords()
    }
  }
})

// 验证充值订单状态
async function verifyRechargeOrder(orderNo: string) {
  verifyingRecharge.value = true
  try {
    toast.info(t('wallet.verifyingPayment'))
    
    const res = await api.billing.verifyRechargeOrder(orderNo)
    
    if (res.verified && res.status === 'completed') {
      // 支付成功，已到账
      toast.success(t('wallet.rechargeSuccess'))
      // 刷新余额
      loadBalance()
    } else if (res.status === 'pending') {
      // 订单待支付，可能回调还没到
      toast.info(t('wallet.paymentProcessing'))
    } else if (res.status === 'failed') {
      // 支付失败或金额异常
      toast.error(res.message || t('wallet.verifyFailed'))
    } else if (res.status === 'cancelled') {
      // 订单已取消
      toast.warning(t('wallet.orderCancelled'))
    } else if (res.message && res.message.includes('不匹配')) {
      // 金额不匹配
      toast.error(t('wallet.amountMismatch'))
    } else {
      // 其他状态
      toast.info(res.message || t('wallet.verifyingPayment'))
    }
    
    // 刷新充值记录
    loadRecords()
  } catch (err: any) {
    console.error('Verify recharge order failed:', err)
    toast.error(t('wallet.verifyFailed'))
    // 仍然刷新记录
    loadRecords()
  } finally {
    verifyingRecharge.value = false
  }
}

async function loadBalance() {
  loading.value = true
  try {
    const res = await api.billing.getUserBalance()
    balance.value = {
      ...balance.value,
      ...res.balance
    }
  } catch (err: any) {
    console.error('Failed to load balance:', err)
  } finally {
    loading.value = false
  }
}

async function loadLogs() {
  logsLoading.value = true
  try {
    const res = await api.billing.getBalanceLogs({
      page: logsPage.value,
      pageSize: logsPageSize.value,
      lotteryGift: showLotteryGiftOnly.value ? 'only' : 'exclude'
    })
    logs.value = res.records || []
    logsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('wallet.loadLogsFailed') + ': ' + (err?.message || ''))
  } finally {
    logsLoading.value = false
  }
}

function resetRechargeProviderSelection() {
  providers.value = []
  selectedProvider.value = null
  selectedPaymentMethod.value = ''
}

async function loadProviders() {
  const loadSeq = ++providersLoadSeq
  providersLoading.value = true
  resetRechargeProviderSelection()
  try {
    const res = await api.billing.getPaymentProviders()
    if (loadSeq !== providersLoadSeq) return

    providers.value = res.providers || []
    if (providers.value.length > 0) {
      selectedProvider.value = providers.value[0].id
      // 自动选中第一个支付方式
      updateDefaultPaymentMethod(providers.value[0])
    }
  } catch (err: any) {
    if (loadSeq === providersLoadSeq) {
      toast.error(t('wallet.loadProvidersFailed') + ': ' + (err?.message || ''))
    }
  } finally {
    if (loadSeq === providersLoadSeq) {
      providersLoading.value = false
    }
  }
}

// 更新默认支付方式
function updateDefaultPaymentMethod(provider: any) {
  if (provider?.type === 'heleket') {
    selectedPaymentMethod.value = ''
  } else if (provider && provider.methods && provider.methods.length > 0) {
    selectedPaymentMethod.value = provider.methods[0]
  } else {
    selectedPaymentMethod.value = ''
  }
}

// 监听支付渠道变化，自动更新默认支付方式
watch(selectedProvider, (newProviderId) => {
  if (newProviderId) {
    const provider = providers.value.find(p => p.id === newProviderId)
    updateDefaultPaymentMethod(provider)
  }
})

async function loadRecords() {
  recordsLoading.value = true
  try {
    const res = await api.billing.getRechargeOrders({
      page: recordsPage.value,
      pageSize: recordsPageSize.value
    })
    rechargeRecords.value = res.records || []
    recordsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('wallet.loadRecordsFailed') + ': ' + (err?.message || ''))
  } finally {
    recordsLoading.value = false
  }
}

// 重新支付订单
async function repayOrder(orderNo: string) {
  repayLoading.value = orderNo
  try {
    const res = await api.billing.repayRechargeOrder(orderNo)
    if (res.payUrl) {
      toast.success(t('wallet.redirecting'))
      const payUrl = res.payUrl
      setTimeout(() => {
        window.location.href = payUrl
      }, 500)
    } else if (res.manualPayment?.instructions) {
      manualPaymentInfo.value = {
        orderNo: res.order.orderNo,
        payableAmount: res.order.payableAmount,
        actualAmount: res.order.actualAmount,
        providerName: null,
        instructions: res.manualPayment.instructions
      }
      showManualPaymentModal.value = true
    } else {
      toast.error(t('wallet.noPayUrl'))
    }
  } catch (err: any) {
    toast.error(t('wallet.repayFailed') + ': ' + (err?.message || ''))
  } finally {
    repayLoading.value = null
  }
}

// 取消订单
async function cancelOrder(orderNo: string) {
  cancelLoading.value = orderNo
  try {
    await api.billing.cancelRechargeOrder(orderNo)
    toast.success(t('wallet.orderCancelled'))
    loadRecords()
  } catch (err: any) {
    toast.error(t('wallet.cancelFailed') + ': ' + (err?.message || ''))
  } finally {
    cancelLoading.value = null
  }
}

function switchTab(tab: WalletTab) {
  if (configStore.freeSiteMode && (tab === 'records' || tab === 'aff')) {
    activeTab.value = 'balance'
    return
  }

  activeTab.value = tab
  if (tab === 'logs' && logs.value.length === 0) {
    loadLogs()
  } else if (tab === 'records' && rechargeRecords.value.length === 0) {
    loadRecords()
  } else if (tab === 'aff' && affStatus.value === null) {
    loadAffStatus()
  }
}

function openRechargeModal() {
  if (configStore.freeSiteMode) return
  rechargeAmount.value = 10
  agreedToTerms.value = false
  agreedToNoRefund.value = false
  agreedToRechargeNotice.value = false
  loadProviders()
  showRechargeModal.value = true
}

async function createRechargeOrder() {
  if (configStore.freeSiteMode) return

  if (providersLoading.value) {
    toast.info(t('common.loading'))
    return
  }
  if (!selectedProvider.value) {
    toast.error(t('wallet.selectProvider'))
    return
  }
  if (rechargeAmount.value <= 0) {
    toast.error(t('wallet.invalidAmount'))
    return
  }

  rechargeLoading.value = true
  try {
    // 获取选中的支付方式
    const provider = providers.value.find(p => p.id === selectedProvider.value)
    let paymentMethod = ''
    if (provider && provider.type !== 'heleket' && provider.methods && provider.methods.length > 0) {
      // 如果用户选择了特定支付方式，使用该方式；否则使用默认第一个
      paymentMethod = selectedPaymentMethod.value || provider.methods[0]
    }
    
    const res = await api.billing.createRechargeOrder(selectedProvider.value, rechargeAmount.value, paymentMethod)
    
    // 如果有支付链接，跳转支付
    if (res.payUrl) {
      toast.success(t('wallet.redirecting'))
      showRechargeModal.value = false
      const payUrl = res.payUrl
      setTimeout(() => {
        window.location.href = payUrl
      }, 500)
      return
    }

    if (res.manualPayment?.instructions) {
      manualPaymentInfo.value = {
        orderNo: res.order.orderNo,
        payableAmount: res.order.payableAmount,
        actualAmount: res.order.actualAmount,
        providerName: res.provider.name,
        instructions: res.manualPayment.instructions
      }
      showRechargeModal.value = false
      showManualPaymentModal.value = true
      toast.success(t('wallet.manualPaymentCreated'))
      if (activeTab.value === 'records') {
        loadRecords()
      }
      return
    }
    
    // 如果没有支付链接（如人工充值）
    toast.success(t('wallet.orderCreated'))
    showRechargeModal.value = false
    toast.info(`${t('wallet.orderNo')}: ${res.order.orderNo}`)
    
    // 刷新记录
    if (activeTab.value === 'records') {
      loadRecords()
    }
  } catch (err: any) {
    toast.error(t('wallet.createOrderFailed') + ': ' + (err?.message || ''))
  } finally {
    rechargeLoading.value = false
  }
}

function formatMoney(amount: number): string {
  return `¥${amount.toFixed(2)}`
}

function formatDate(dateStr: string): string {
  return new Date(dateStr).toLocaleString()
}

function getLogTypeName(type: string): string {
  const map: Record<string, string> = {
    recharge: t('wallet.logTypes.recharge'),
    consume: t('wallet.logTypes.consume'),
    refund: t('wallet.logTypes.refund'),
    admin_adjust: t('wallet.logTypes.adminAdjust'),
    gift: t('wallet.logTypes.gift'),
    transfer_fee: t('wallet.logTypes.transferFee'),
    transfer_refund: t('wallet.logTypes.transferRefund'),
    invite_generate: '生成邀请码'
  }
  return map[type] || type
}

function getStatusName(status: string): string {
  const map: Record<string, string> = {
    pending: t('wallet.status.pending'),
    paid: t('wallet.status.paid'),
    completed: t('wallet.status.completed'),
    failed: t('wallet.status.failed'),
    cancelled: t('wallet.status.cancelled'),
    refunded: t('wallet.status.refunded')
  }
  return map[status] || status
}

function getPaymentMethodName(method: string): string {
  const map: Record<string, string> = {
    alipay: t('wallet.paymentMethods.alipay'),
    wxpay: t('wallet.paymentMethods.wxpay'),
    qqpay: t('wallet.paymentMethods.qqpay'),
    bank: t('wallet.paymentMethods.bank'),
    jdpay: t('wallet.paymentMethods.jdpay'),
  }
  if (map[method]) {
    return map[method]
  }

  const normalized = method.trim()
  const matched = normalized.match(/^([A-Za-z0-9._-]+)\s*(?:[@:/]\s*([A-Za-z0-9._-]+))?$/)
  if (!matched) {
    return normalized || method
  }

  const currency = matched[1]?.toUpperCase() || normalized
  const network = matched[2]?.toUpperCase()
  return network ? `${currency} (${network})` : currency
}

function getRechargeMethodDisplay(rec: any): string {
  if (rec.actualPaymentMethod) {
    return getPaymentMethodName(rec.actualPaymentMethod)
  }
  if (rec.paymentMethod) {
    return getPaymentMethodName(rec.paymentMethod)
  }
  return ''
}

function getRechargeCreditLabelKey(status: string): string {
  return status === 'completed' ? 'wallet.actualAmount' : 'wallet.estimatedAmount'
}

function getRechargeGatewayStatusText(rec: any): string {
  return rec.status === 'pending' && rec.gatewayStatusDescription ? rec.gatewayStatusDescription : ''
}

// 检查订单是否已过期
function isOrderExpired(expiredAt: string | null): boolean {
  if (!expiredAt) return false
  return new Date(expiredAt) < new Date()
}

function getMetricDotClass(tone: MetricTone): string {
  const map: Record<MetricTone, string> = {
    slate: 'bg-zinc-400 dark:bg-zinc-500',
    emerald: 'bg-[var(--success)]',
    rose: 'bg-[var(--error)]',
    amber: 'bg-[var(--warning)]',
    blue: 'bg-[var(--accent)]',
    violet: 'bg-purple-500'
  }

  return map[tone] || 'bg-zinc-400'
}

function getAmountValueClass(amount: number): string {
  return amount >= 0
    ? 'text-[var(--success)]'
    : 'text-[var(--text-primary)]'
}

function getLogBadgeVariant(type: string): BadgeVariant {
  switch (type) {
    case 'recharge':
      return 'success'
    case 'consume':
      return 'error'
    case 'refund':
    case 'admin_adjust':
    case 'transfer_refund':
      return 'info'
    case 'gift':
      return 'warning'
    case 'transfer_fee':
    case 'invite_generate':
    default:
      return 'default'
  }
}

function getStatusBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'completed':
      return 'success'
    case 'pending':
      return 'warning'
    case 'paid':
      return 'info'
    case 'failed':
      return 'error'
    case 'cancelled':
      return 'default'
    case 'refunded':
      return 'info'
    default:
      return 'default'
  }
}

function getAffLogBadgeVariant(type: string): BadgeVariant {
  switch (type) {
    case 'new_purchase':
      return 'success'
    case 'renew':
      return 'info'
    case 'convert':
    default:
      return 'default'
  }
}

function getWithdrawalBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'approved':
      return 'success'
    case 'pending':
      return 'warning'
    case 'rejected':
      return 'error'
    default:
      return 'default'
  }
}

const logsTotalPages = computed(() => Math.ceil(logsTotal.value / logsPageSize.value))
const recordsTotalPages = computed(() => Math.ceil(recordsTotal.value / recordsPageSize.value))
const selectedProviderInfo = computed(() => providers.value.find(p => p.id === selectedProvider.value))
function getSelectedPaymentMethodForProvider(provider: any): string {
  if (!provider || provider.type === 'heleket') return ''
  if (!provider.methods || provider.methods.length === 0) return ''
  return selectedPaymentMethod.value || provider.methods[0]
}

function getProviderFeeConfig(provider: any, method?: string): { feeRate: number; feeFixed: number } {
  const methodFee = provider?.type === 'yipay' && method && provider?.methodFees ? provider.methodFees[method] : null
  if (methodFee) {
    return {
      feeRate: Number(methodFee.feeRate || 0),
      feeFixed: Number(methodFee.feeFixed || 0)
    }
  }

  return {
    feeRate: Number(provider?.feeRate || 0),
    feeFixed: Number(provider?.feeFixed || 0)
  }
}

const selectedFeeConfig = computed(() => {
  const provider = selectedProviderInfo.value
  return getProviderFeeConfig(provider, getSelectedPaymentMethodForProvider(provider))
})

const selectedRechargeFee = computed(() => {
  const amount = Number(rechargeAmount.value || 0)
  const feeRate = selectedFeeConfig.value.feeRate
  const feeFixed = selectedFeeConfig.value.feeFixed
  return Math.max(0, Math.round((amount * feeRate + feeFixed) * 100) / 100)
})

const selectedFeeIsSurcharge = computed(() => selectedProviderInfo.value?.type === 'yipay')

const selectedPayableAmount = computed(() => {
  const amount = Number(rechargeAmount.value || 0)
  const total = selectedFeeIsSurcharge.value ? amount + selectedRechargeFee.value : amount
  return Math.max(0, Math.round(total * 100) / 100)
})

const selectedCreditAmount = computed(() => {
  const amount = Number(rechargeAmount.value || 0)
  const total = selectedFeeIsSurcharge.value ? amount : amount - selectedRechargeFee.value
  return Math.max(0, Math.round(total * 100) / 100)
})
const affLogsTotalPages = computed(() => Math.ceil(affLogsTotal.value / affLogsPageSize.value))
const affWithdrawalsTotalPages = computed(() => Math.ceil(affWithdrawalsTotal.value / affWithdrawalsPageSize.value))
const logsPositiveAmount = computed(() => logs.value.filter(log => log.amount > 0).reduce((sum, log) => sum + log.amount, 0))
const logsNegativeAmount = computed(() => logs.value.filter(log => log.amount < 0).reduce((sum, log) => sum + Math.abs(log.amount), 0))
const pendingRechargeCount = computed(() => rechargeRecords.value.filter(rec => rec.status === 'pending' && !isOrderExpired(rec.expiredAt)).length)
const completedRechargeCount = computed(() => rechargeRecords.value.filter(rec => ['paid', 'completed'].includes(rec.status)).length)
const expiredRechargeCount = computed(() => rechargeRecords.value.filter(rec => rec.status === 'pending' && isOrderExpired(rec.expiredAt)).length)
const affPendingWithdrawalCount = computed(() => affWithdrawals.value.filter(withdrawal => withdrawal.status === 'pending').length)
const balanceMetrics = computed<WalletMetric[]>(() => [
  { label: configStore.freeSiteMode ? freeSiteCopy.walletTotalRecharge : t('wallet.totalRecharge'), value: formatMoney(balance.value.totalRecharge), tone: 'emerald' },
  { label: configStore.freeSiteMode ? freeSiteCopy.walletTotalConsume : t('wallet.totalConsume'), value: formatMoney(balance.value.totalConsume), tone: 'rose' },
  { label: configStore.freeSiteMode ? freeSiteCopy.walletDestroyedValue : t('wallet.totalDestroyedValue'), value: formatMoney(balance.value.destroyedValue), tone: 'slate' }
])
const logMetrics = computed<WalletMetric[]>(() => [
  { label: configStore.freeSiteMode ? freeSiteCopy.walletTotalRecharge : t('wallet.totalRecharge'), value: formatMoney(logsPositiveAmount.value), tone: 'emerald' },
  { label: configStore.freeSiteMode ? freeSiteCopy.walletTotalConsume : t('wallet.totalConsume'), value: formatMoney(logsNegativeAmount.value), tone: 'rose' }
])
const recordMetrics = computed<WalletMetric[]>(() => [
  { label: t('wallet.status.pending'), value: pendingRechargeCount.value.toLocaleString(), tone: 'amber' },
  { label: t('wallet.status.completed'), value: completedRechargeCount.value.toLocaleString(), tone: 'emerald' },
  { label: t('wallet.orderExpired'), value: expiredRechargeCount.value.toLocaleString(), tone: 'slate' }
])
const affMetrics = computed<WalletMetric[]>(() => {
  if (!affStatus.value) return []
  return [
    { label: t('aff.totalEarnings'), value: formatMoney(affStatus.value.totalEarnings), tone: 'emerald' },
    { label: t('aff.myCodes'), value: affStatus.value.totalCodes.toLocaleString(), tone: 'blue' },
    { label: t('aff.usedCount'), value: affStatus.value.totalUsed.toLocaleString(), tone: 'amber' },
    { label: t('aff.withdrawalStatus.pending'), value: affPendingWithdrawalCount.value.toLocaleString(), tone: 'violet' }
  ]
})

// AFF 加载函数
async function loadAffStatus() {
  affLoading.value = true
  try {
    const res = await api.aff.getStatus()
    affStatus.value = res as AffStatus
    // 如果已激活，加载优惠码列表
    if (affStatus.value?.activated) {
      loadAffCodes()
      loadAffLogs()
      loadAffWithdrawals()
    }
  } catch (err: any) {
    console.error('Failed to load AFF status:', err)
  } finally {
    affLoading.value = false
  }
}

async function loadAffCodes() {
  affCodesLoading.value = true
  try {
    const res = await api.aff.getCodes()
    affCodes.value = (res as any).codes || []
  } catch (err: any) {
    console.error('Failed to load AFF codes:', err)
  } finally {
    affCodesLoading.value = false
  }
}

async function loadAffPlans() {
  affPlansLoading.value = true
  try {
    const res = await api.aff.getAvailablePlans()
    affPlans.value = (res as any).plans || []
  } catch (err: any) {
    console.error('Failed to load AFF plans:', err)
  } finally {
    affPlansLoading.value = false
  }
}

async function loadAffLogs() {
  affLogsLoading.value = true
  try {
    const res = await api.aff.getLogs({
      page: affLogsPage.value,
      pageSize: affLogsPageSize.value
    })
    affLogs.value = (res as any).logs || []
    affLogsTotal.value = (res as any).total || 0
  } catch (err: any) {
    console.error('Failed to load AFF logs:', err)
  } finally {
    affLogsLoading.value = false
  }
}

async function loadAffWithdrawals() {
  affWithdrawalsLoading.value = true
  try {
    const res = await api.aff.getWithdrawals({
      page: affWithdrawalsPage.value,
      pageSize: affWithdrawalsPageSize.value
    })
    affWithdrawals.value = (res as any).withdrawals || []
    affWithdrawalsTotal.value = (res as any).total || 0
  } catch (err: any) {
    console.error('Failed to load AFF withdrawals:', err)
  } finally {
    affWithdrawalsLoading.value = false
  }
}

// 打开创建优惠码弹窗
function openCreateCodeModal() {
  selectedPlanId.value = null
  loadAffPlans()
  showCreateCodeModal.value = true
}

// 创建优惠码
async function createAffCode() {
  if (selectedPlanId.value === null) return
  createCodeLoading.value = true
  try {
    // selectedPlanId = -1 表示全局码
    if (selectedPlanId.value === -1) {
      await api.aff.createCode(undefined)
    } else {
      await api.aff.createCode(selectedPlanId.value)
    }
    toast.success(t('aff.createSuccess'))
    showCreateCodeModal.value = false
    loadAffCodes()
    loadAffPlans() // 刷新全局码状态
  } catch (err: any) {
    toast.error(t('aff.createFailed') + ': ' + (err?.message || ''))
  } finally {
    createCodeLoading.value = false
  }
}

// 打开转化申请弹窗
function openConvertModal() {
  convertAmount.value = affStatus.value?.currentBalance || 0
  showConvertModal.value = true
}

// 打开 AFF 榜单弹窗
async function openLeaderboardModal() {
  showLeaderboardModal.value = true
  leaderboardLoading.value = true
  try {
    const res = await api.aff.getLeaderboard()
    leaderboard.value = res.leaderboard
  } catch (err: any) {
    toast.error(t('aff.leaderboard.loadFailed') + ': ' + (err?.message || ''))
  } finally {
    leaderboardLoading.value = false
  }
}

// 获取榜单排名样式
function getRankStyle(rank: number): string {
  if (rank === 1) return 'bg-amber-100 text-amber-800 dark:bg-amber-500/20 dark:text-amber-300'
  if (rank === 2) return 'bg-zinc-200 text-zinc-800 dark:bg-zinc-700/40 dark:text-zinc-200'
  if (rank === 3) return 'bg-orange-100 text-orange-800 dark:bg-orange-500/20 dark:text-orange-300'
  return 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)]'
}

// 提交转化申请
async function submitConvert() {
  if (convertAmount.value < 0.1) {
    toast.error(t('aff.convertModal.invalidAmount'))
    return
  }
  convertLoading.value = true
  try {
    await api.aff.createConvert(convertAmount.value)
    toast.success(t('aff.convertModal.success'))
    showConvertModal.value = false
    // 刷新 AFF 状态和转化记录
    loadAffStatus()
    loadAffWithdrawals()
    // 刷新账户余额（转化成功后已转入主余额）
    loadBalance()
    // 切换到账户余额标签页，让用户看到更新后的余额
    switchTab('balance')
  } catch (err: any) {
    toast.error(t('aff.convertModal.failed') + ': ' + (err?.message || ''))
  } finally {
    convertLoading.value = false
  }
}

// AFF 日志类型名称
function getAffLogTypeName(type: string): string {
  const map: Record<string, string> = {
    new_purchase: t('aff.logType.new_purchase'),
    renew: t('aff.logType.renew'),
    convert: t('aff.logType.convert')
  }
  return map[type] || type
}

// AFF 转化状态名称
function getWithdrawalStatusName(status: string): string {
  const map: Record<string, string> = {
    pending: t('aff.withdrawalStatus.pending'),
    approved: t('aff.withdrawalStatus.approved'),
    rejected: t('aff.withdrawalStatus.rejected')
  }
  return map[status] || status
}

// 复制优惠码
async function copyCode(code: string) {
  try {
    await navigator.clipboard.writeText(code)
    toast.success(t('common.copied'))
  } catch {
    toast.error(t('common.copyFailed'))
  }
}

// 删除优惠码
async function deleteAffCode(codeId: number, code: string) {
  if (!confirm(t('aff.deleteCodeConfirm', { code }))) {
    return
  }
  
  deleteCodeLoading.value = codeId
  try {
    await api.aff.deleteCode(codeId)
    toast.success(t('aff.deleteCodeSuccess'))
    loadAffCodes()
  } catch (err: any) {
    toast.error(t('aff.deleteCodeFailed') + ': ' + (err?.message || ''))
  } finally {
    deleteCodeLoading.value = null
  }
}

// 格式化金额：限制两位小数
function formatAmount() {
  if (rechargeAmount.value) {
    rechargeAmount.value = Math.round(Number(rechargeAmount.value) * 100) / 100
  }
}
</script>

<template>
  <div ref="revealRoot" class="max-w-7xl mx-auto space-y-6 sm:space-y-8 animate-fade-in">
    <!-- 页面顶栏 -->
    <PageHeader
      data-reveal
      :title="configStore.freeSiteMode ? freeSiteCopy.walletBalanceTab : t('wallet.title')"
      :description="configStore.freeSiteMode ? freeSiteCopy.walletDescription : t('wallet.description')"
    >
      <template v-if="!configStore.freeSiteMode" #actions>
        <Button variant="primary" @click="openRechargeModal">
          <template #icon>
            <Plus class="w-4 h-4" />
          </template>
          <span>{{ t('wallet.recharge') }}</span>
        </Button>
      </template>
    </PageHeader>

    <!-- Apple 风格分段导航 (Segmented Tabs) -->
    <div data-reveal class="flex items-center overflow-x-auto p-1.5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] w-fit max-w-full tab-scroll-hide shadow-[0_2px_8px_rgba(0,0,0,0.02)]">
      <div class="flex items-center gap-1 shrink-0" role="tablist" :aria-label="t('wallet.title')">
        <button
          v-for="tab in walletTabs"
          :key="tab.key"
          type="button"
          role="tab"
          :aria-selected="activeTab === tab.key"
          class="whitespace-nowrap rounded-xl px-4 py-2 text-sm font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="activeTab === tab.key
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="switchTab(tab.key)"
        >
          {{ tab.label }}
        </button>
      </div>
    </div>

    <!-- 账户余额 TAB -->
    <div v-show="activeTab === 'balance'" class="space-y-6">
      <Card data-reveal padding="md">
        <div v-if="loading" class="animate-pulse space-y-6">
          <div class="h-3 w-24 rounded bg-[var(--bg-surface-soft)]" />
          <div class="h-16 w-64 rounded bg-[var(--bg-surface-soft)]" />
          <div class="grid gap-4 sm:grid-cols-3">
            <div v-for="i in 3" :key="i" class="h-28 rounded-2xl bg-[var(--bg-surface-soft)]" />
          </div>
        </div>
        <div v-else>
          <div class="flex flex-col gap-6 lg:flex-row lg:items-start lg:justify-between">
            <div class="max-w-3xl">
              <div class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.walletCurrentBalance : t('wallet.currentBalance') }}
              </div>
              <div class="mt-3 font-mono text-5xl sm:text-6xl font-semibold figure-hero text-[var(--text-primary)] tabular-nums">
                {{ formatMoney(balance.balance) }}
              </div>
              <p class="mt-3 max-w-2xl text-sm leading-relaxed text-[var(--text-secondary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.walletDescription : t('wallet.description') }}
              </p>
            </div>
            <div v-if="!configStore.freeSiteMode" class="shrink-0">
              <Button variant="primary" size="md" @click="openRechargeModal">
                <template #icon>
                  <Plus class="w-4 h-4" />
                </template>
                <span>{{ t('wallet.recharge') }}</span>
              </Button>
            </div>
          </div>

          <div class="mt-8 grid gap-4 sm:grid-cols-3">
            <Card
              v-for="metric in balanceMetrics"
              :key="metric.label"
              hoverable
              padding="sm"
              class="group"
            >
              <div class="flex items-center gap-2">
                <span class="h-2 w-2 rounded-full" :class="getMetricDotClass(metric.tone)" />
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ metric.label }}
                </span>
              </div>
              <div class="mt-3 font-mono text-2xl sm:text-3xl font-semibold figure-stat text-[var(--text-primary)] tabular-nums">
                {{ metric.value }}
              </div>
            </Card>
          </div>
        </div>
      </Card>
    </div>

    <!-- 余额明细 TAB -->
    <div v-show="activeTab === 'logs'" class="space-y-6">
      <Card data-reveal padding="md">
        <div class="flex flex-col gap-6">
          <div class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <div class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.walletLogsTab : t('wallet.tabs.logs') }}
              </div>
              <div class="mt-3 font-mono text-4xl sm:text-5xl font-semibold figure-hero text-[var(--text-primary)] tabular-nums">
                {{ logsTotal.toLocaleString() }}
              </div>
              <div class="mt-1.5 text-xs text-[var(--text-secondary)]">
                {{ t('common.total') }} {{ logsTotal }} {{ t('common.items') }}
              </div>
            </div>

            <Button
              :variant="showLotteryGiftOnly ? 'primary' : 'secondary'"
              size="sm"
              @click="showLotteryGiftOnly = !showLotteryGiftOnly; logsPage = 1; loadLogs()"
            >
              <template #icon>
                <Gift class="h-4 w-4" />
              </template>
              <span>{{ showLotteryGiftOnly ? t('wallet.showingLotteryGift') : t('wallet.showLotteryGift') }}</span>
            </Button>
          </div>

          <div class="grid gap-4 sm:grid-cols-2">
            <Card
              v-for="metric in logMetrics"
              :key="metric.label"
              hoverable
              padding="sm"
              class="group"
            >
              <div class="flex items-center gap-2">
                <span class="h-2 w-2 rounded-full" :class="getMetricDotClass(metric.tone)" />
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ metric.label }}
                </span>
              </div>
              <div class="mt-3 font-mono text-2xl font-semibold figure-stat text-[var(--text-primary)] tabular-nums">
                {{ metric.value }}
              </div>
            </Card>
          </div>
        </div>
      </Card>

      <Card data-reveal padding="none">
        <template #header>
          <div class="px-6 py-4 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-1 w-full">
            <div>
              <h3 class="font-semibold text-lg text-[var(--text-primary)]">
                {{ configStore.freeSiteMode ? freeSiteCopy.walletLogsTab : t('wallet.tabs.logs') }}
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                {{ showLotteryGiftOnly ? t('wallet.showingLotteryGift') : (configStore.freeSiteMode ? freeSiteCopy.walletLogsDescription : t('wallet.description')) }}
              </p>
            </div>
          </div>
        </template>

        <div v-if="logsLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
          <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
          <span>{{ t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="logs.length === 0"
          :title="t('wallet.noLogs')"
          :description="showLotteryGiftOnly ? t('wallet.showingLotteryGift') : (configStore.freeSiteMode ? freeSiteCopy.walletLogsDescription : t('wallet.description'))"
        />
        <div v-else>
          <!-- 表头 (桌面端) -->
          <div class="hidden grid-cols-[minmax(0,1fr)_130px_130px_180px] gap-4 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-6 py-3 text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] lg:grid">
            <div>{{ t('wallet.instanceOrRemark') }}</div>
            <div>{{ t('wallet.amount') }}</div>
            <div>{{ t('wallet.balanceAfter') }}</div>
            <div>{{ t('wallet.time') }}</div>
          </div>

          <!-- 行数据 -->
          <div class="divide-y divide-[var(--border-color)]">
            <div
              v-for="log in logs"
              :key="log.id"
              class="grid gap-3 px-5 py-4 transition-colors hover:bg-[var(--bg-surface-soft)] sm:px-6 lg:grid-cols-[minmax(0,1fr)_130px_130px_180px] lg:items-center lg:py-3.5"
            >
              <div class="min-w-0">
                <!-- 移动端视图 -->
                <div class="lg:hidden">
                  <div class="flex flex-wrap items-center gap-2">
                    <Badge :variant="getLogBadgeVariant(log.type)">
                      {{ getLogTypeName(log.type) }}
                    </Badge>
                    <button
                      v-if="log.instanceId"
                      class="text-sm font-medium text-[var(--accent)] transition-colors hover:underline"
                      @click="router.push(instanceDetailPath(log.instanceId))"
                    >
                      #{{ log.instanceId }}
                    </button>
                  </div>
                  <div class="mt-2 truncate text-sm text-[var(--text-secondary)]">
                    {{ log.remark || '-' }}
                  </div>
                </div>

                <!-- 桌面端视图 -->
                <div class="hidden min-w-0 items-center gap-2.5 lg:flex">
                  <Badge :variant="getLogBadgeVariant(log.type)" class="shrink-0">
                    {{ getLogTypeName(log.type) }}
                  </Badge>
                  <button
                    v-if="log.instanceId"
                    class="shrink-0 text-sm font-medium text-[var(--accent)] transition-colors hover:underline"
                    @click="router.push(instanceDetailPath(log.instanceId))"
                  >
                    #{{ log.instanceId }}
                  </button>
                  <span class="min-w-0 truncate text-sm text-[var(--text-secondary)]" :title="log.remark || undefined">
                    {{ log.remark || '-' }}
                  </span>
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.amount') }}
                </div>
                <div class="font-mono text-sm font-semibold tabular-nums" :class="getAmountValueClass(log.amount)">
                  {{ log.amount >= 0 ? '+' : '-' }}{{ formatMoney(Math.abs(log.amount)) }}
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.balanceAfter') }}
                </div>
                <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                  {{ formatMoney(log.balanceAfter) }}
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.time') }}
                </div>
                <div class="text-sm text-[var(--text-secondary)]">
                  {{ formatDate(log.createdAt) }}
                </div>
              </div>
            </div>
          </div>

          <!-- 分页器 -->
          <div v-if="logsTotalPages > 1" class="border-t border-[var(--border-color)] px-5 py-4 sm:px-6">
            <Pagination
              :page="logsPage"
              :page-size="logsPageSize"
              :total="logsTotal"
              @update:page="(p) => { logsPage = p; loadLogs() }"
            >
              <template #total>
                <span>{{ t('common.total') }} {{ logsTotal }} {{ t('common.items') }}</span>
              </template>
            </Pagination>
          </div>
        </div>
      </Card>
    </div>

    <!-- 充值记录 TAB -->
    <div v-if="!configStore.freeSiteMode" v-show="activeTab === 'records'" class="space-y-6">
      <Card data-reveal padding="md">
        <div class="flex flex-col gap-6">
          <div class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <div class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
                {{ t('wallet.tabs.records') }}
              </div>
              <div class="mt-3 font-mono text-4xl sm:text-5xl font-semibold figure-hero text-[var(--text-primary)] tabular-nums">
                {{ recordsTotal.toLocaleString() }}
              </div>
              <div class="mt-1.5 text-xs text-[var(--text-secondary)]">
                {{ t('common.total') }} {{ recordsTotal }} {{ t('common.items') }}
              </div>
              <div v-if="verifyingRecharge" class="mt-3">
                <Badge variant="info" pulse>
                  {{ t('wallet.verifyingPayment') }}
                </Badge>
              </div>
            </div>

            <Button variant="primary" size="md" @click="openRechargeModal">
              <template #icon>
                <Plus class="h-4 w-4" />
              </template>
              <span>{{ t('wallet.recharge') }}</span>
            </Button>
          </div>

          <div class="grid gap-4 sm:grid-cols-3">
            <Card
              v-for="metric in recordMetrics"
              :key="metric.label"
              hoverable
              padding="sm"
              class="group"
            >
              <div class="flex items-center gap-2">
                <span class="h-2 w-2 rounded-full" :class="getMetricDotClass(metric.tone)" />
                <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ metric.label }}
                </span>
              </div>
              <div class="mt-3 font-mono text-2xl font-semibold figure-stat text-[var(--text-primary)] tabular-nums">
                {{ metric.value }}
              </div>
            </Card>
          </div>
        </div>
      </Card>

      <Card data-reveal padding="none">
        <template #header>
          <div class="px-6 py-4 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-1 w-full">
            <div>
              <h3 class="font-semibold text-lg text-[var(--text-primary)]">{{ t('wallet.tabs.records') }}</h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('wallet.description') }}</p>
            </div>
          </div>
        </template>

        <div v-if="recordsLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
          <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
          <span>{{ t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="rechargeRecords.length === 0"
          :title="t('wallet.noRecords')"
          :description="t('wallet.description')"
        />
        <div v-else>
          <!-- 表头 (桌面端) -->
          <div class="hidden grid-cols-[minmax(0,1.2fr)_120px_160px_120px_180px_auto] gap-4 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-6 py-3 text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] lg:grid">
            <div>{{ t('wallet.orderNo') }}</div>
            <div>{{ t('wallet.amount') }}</div>
            <div>{{ t('wallet.paymentChannel') }}</div>
            <div>{{ t('wallet.statusLabel') }}</div>
            <div>{{ t('wallet.time') }}</div>
            <div class="text-right">{{ t('common.actions') }}</div>
          </div>

          <!-- 行数据 -->
          <div class="divide-y divide-[var(--border-color)]">
            <div
              v-for="rec in rechargeRecords"
              :key="rec.id"
              class="grid gap-3 px-5 py-4 transition-colors hover:bg-[var(--bg-surface-soft)] sm:px-6 lg:grid-cols-[minmax(0,1.2fr)_120px_160px_120px_180px_auto] lg:items-center lg:py-3.5"
            >
              <div class="min-w-0">
                <!-- 移动端视图 -->
                <div class="lg:hidden">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="truncate font-mono text-sm font-semibold text-[var(--text-primary)]">{{ rec.orderNo }}</span>
                  </div>
                  <div class="mt-1 text-sm text-[var(--text-secondary)]">
                    {{ rec.provider?.name || '-' }}
                    <span v-if="getRechargeMethodDisplay(rec)">· {{ getRechargeMethodDisplay(rec) }}</span>
                  </div>
                  <div v-if="rec.paymentUuid || rec.paymentTxid || rec.completedAt" class="mt-1 break-all font-mono text-xs text-[var(--text-tertiary)]">
                    <span v-if="rec.paymentUuid">{{ t('wallet.paymentUuid') }} {{ rec.paymentUuid }}</span>
                    <span v-if="rec.paymentUuid && (rec.paymentTxid || rec.completedAt)"> · </span>
                    <span v-if="rec.paymentTxid">{{ t('wallet.paymentTxid') }} {{ rec.paymentTxid }}</span>
                    <span v-if="rec.paymentTxid && rec.completedAt"> · </span>
                    <span v-if="rec.completedAt">{{ t('wallet.completedAt') }} {{ formatDate(rec.completedAt) }}</span>
                  </div>
                </div>

                <!-- 桌面端视图 -->
                <div class="hidden lg:block min-w-0">
                  <span class="block truncate font-mono text-sm font-semibold text-[var(--text-primary)]" :title="rec.orderNo">
                    {{ rec.orderNo }}
                  </span>
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.amount') }}
                </div>
                <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                  {{ formatMoney(rec.amount) }}
                </div>
                <div v-if="rec.actualAmount !== null" class="mt-0.5 font-mono text-xs text-[var(--text-secondary)]">
                  {{ t(getRechargeCreditLabelKey(rec.status)) }} {{ formatMoney(rec.actualAmount) }}
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block min-w-0">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.paymentChannel') }}
                </div>
                <div class="text-sm text-[var(--text-secondary)] lg:truncate">
                  {{ rec.provider?.name || '-' }}<span v-if="getRechargeMethodDisplay(rec)"> · {{ getRechargeMethodDisplay(rec) }}</span>
                </div>
                <div v-if="rec.paymentUuid" class="mt-0.5 hidden truncate font-mono text-xs text-[var(--text-tertiary)] lg:block" :title="rec.paymentUuid">
                  {{ t('wallet.paymentUuid') }} {{ rec.paymentUuid }}
                </div>
                <div v-if="rec.paymentTxid" class="mt-0.5 hidden truncate font-mono text-xs text-[var(--text-tertiary)] lg:block" :title="rec.paymentTxid">
                  {{ t('wallet.paymentTxid') }} {{ rec.paymentTxid }}
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.statusLabel') }}
                </div>
                <Badge :variant="getStatusBadgeVariant(rec.status)">
                  {{ getStatusName(rec.status) }}
                </Badge>
                <div v-if="getRechargeGatewayStatusText(rec)" class="mt-1 text-xs text-[var(--text-secondary)]">
                  {{ getRechargeGatewayStatusText(rec) }}
                </div>
              </div>

              <div class="flex items-center justify-between gap-3 lg:block">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                  {{ t('wallet.time') }}
                </div>
                <div class="text-sm text-[var(--text-secondary)]">
                  {{ formatDate(rec.createdAt) }}
                </div>
                <div v-if="rec.completedAt" class="mt-0.5 hidden text-xs text-[var(--text-tertiary)] lg:block">
                  {{ t('wallet.completedAt') }} {{ formatDate(rec.completedAt) }}
                </div>
              </div>

              <div class="flex flex-wrap items-center gap-2 lg:justify-end">
                <template v-if="rec.status === 'pending' && !isOrderExpired(rec.expiredAt)">
                  <Button
                    size="sm"
                    variant="primary"
                    :loading="repayLoading === rec.orderNo"
                    @click="repayOrder(rec.orderNo)"
                  >
                    {{ repayLoading === rec.orderNo ? t('common.processing') : t('wallet.pay') }}
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    class="text-[var(--error)] hover:bg-[var(--error)]/10"
                    :loading="cancelLoading === rec.orderNo"
                    @click="cancelOrder(rec.orderNo)"
                  >
                    {{ cancelLoading === rec.orderNo ? t('common.processing') : t('wallet.void') }}
                  </Button>
                </template>
                <template v-else-if="rec.status === 'pending' && isOrderExpired(rec.expiredAt)">
                  <Badge variant="error">
                    {{ t('wallet.orderExpired') }}
                  </Badge>
                </template>
                <template v-else>
                  <span class="text-sm text-[var(--text-tertiary)]">-</span>
                </template>
              </div>
            </div>
          </div>

          <!-- 分页器 -->
          <div v-if="recordsTotalPages > 1" class="border-t border-[var(--border-color)] px-5 py-4 sm:px-6">
            <Pagination
              :page="recordsPage"
              :page-size="recordsPageSize"
              :total="recordsTotal"
              @update:page="(p) => { recordsPage = p; loadRecords() }"
            >
              <template #total>
                <span>{{ t('common.total') }} {{ recordsTotal }} {{ t('common.items') }}</span>
              </template>
            </Pagination>
          </div>
        </div>
      </Card>
    </div>

    <!-- 推荐计划 TAB -->
    <div v-if="!configStore.freeSiteMode" v-show="activeTab === 'aff'" class="space-y-6">
      <div v-if="affLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
        <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
        <span>{{ t('common.loading') }}...</span>
      </div>

      <!-- 未激活 -->
      <Card v-else-if="affStatus && !affStatus.activated" data-reveal padding="lg" class="text-center">
        <div class="mx-auto mb-4 flex h-16 w-16 items-center justify-center rounded-2xl bg-[var(--bg-surface-soft)] text-[var(--accent)] border border-[var(--border-color)]">
          <Share2 class="h-8 w-8 stroke-[1.8]" />
        </div>
        <h3 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">{{ t('aff.notActivated') }}</h3>
        <p class="mx-auto mt-2 max-w-xl text-sm leading-relaxed text-[var(--text-secondary)]">{{ t('aff.activateHint') }}</p>
        <div class="mt-6 flex justify-center">
          <Button variant="primary" @click="switchTab('balance'); openRechargeModal()">
            {{ t('aff.goRecharge') }}
          </Button>
        </div>
      </Card>

      <!-- 已激活 -->
      <template v-else-if="affStatus && affStatus.activated">
        <Card data-reveal padding="md">
          <div class="flex flex-col gap-6">
            <div class="flex flex-col gap-6 lg:flex-row lg:items-start lg:justify-between">
              <div class="min-w-0 flex-1">
                <div class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
                  {{ t('aff.affBalance') }}
                </div>
                <div class="mt-3 font-mono text-5xl sm:text-6xl font-semibold figure-hero text-[var(--text-primary)] tabular-nums">
                  {{ formatMoney(affStatus.currentBalance) }}
                </div>
                <p class="mt-3 max-w-2xl text-sm leading-relaxed text-[var(--text-secondary)]">
                  {{ t('aff.balanceHint') }}
                </p>
              </div>

              <div class="flex flex-wrap items-center gap-3">
                <Button variant="secondary" @click="openLeaderboardModal">
                  <template #icon>
                    <Trophy class="h-4 w-4" />
                  </template>
                  <span>{{ t('aff.leaderboard.title') }}</span>
                </Button>
                <Button
                  variant="primary"
                  :disabled="affStatus.currentBalance < 0.1"
                  @click="openConvertModal"
                >
                  <template #icon>
                    <Coins class="h-4 w-4" />
                  </template>
                  <span>{{ t('aff.convert') }}</span>
                </Button>
              </div>
            </div>

            <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
              <Card
                v-for="metric in affMetrics"
                :key="metric.label"
                hoverable
                padding="sm"
                class="group"
              >
                <div class="flex items-center gap-2">
                  <span class="h-2 w-2 rounded-full" :class="getMetricDotClass(metric.tone)" />
                  <span class="text-xs font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                    {{ metric.label }}
                  </span>
                </div>
                <div class="mt-3 font-mono text-2xl font-semibold figure-stat text-[var(--text-primary)] tabular-nums">
                  {{ metric.value }}
                </div>
              </Card>
            </div>
          </div>
        </Card>

        <!-- 我的优惠码卡片 -->
        <Card data-reveal padding="none">
          <template #header>
            <div class="flex flex-col gap-3 px-6 py-4 sm:flex-row sm:items-center sm:justify-between w-full">
              <div>
                <h3 class="font-semibold text-lg text-[var(--text-primary)]">{{ t('aff.myCodes') }}</h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('common.total') }} {{ affStatus.totalCodes }} {{ t('common.items') }}
                </p>
              </div>
              <Button size="sm" variant="primary" @click="openCreateCodeModal">
                <template #icon>
                  <Plus class="h-4 w-4" />
                </template>
                <span>{{ t('aff.createCode') }}</span>
              </Button>
            </div>
          </template>

          <div v-if="affCodesLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
            <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
            <span>{{ t('common.loading') }}...</span>
          </div>
          <EmptyState
            v-else-if="affCodes.length === 0"
            :title="t('aff.noCodes')"
          />
          <div v-else>
            <div class="hidden grid-cols-[minmax(0,1.5fr)_minmax(0,1.25fr)_150px_100px_120px_auto] gap-4 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-6 py-3 text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] lg:grid">
              <div>{{ t('aff.code') }}</div>
              <div>{{ t('aff.plan') }}</div>
              <div>{{ t('aff.discount') }}/{{ t('aff.commission') }}</div>
              <div>{{ t('aff.usedCount') }}</div>
              <div>{{ t('aff.earnings') }}</div>
              <div class="text-right"></div>
            </div>

            <div class="divide-y divide-[var(--border-color)]">
              <div
                v-for="code in affCodes"
                :key="code.id"
                class="grid gap-3 px-5 py-4 transition-colors hover:bg-[var(--bg-surface-soft)] sm:px-6 lg:grid-cols-[minmax(0,1.5fr)_minmax(0,1.25fr)_150px_100px_120px_auto] lg:items-center lg:py-3.5"
              >
                <div class="min-w-0">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-mono text-sm font-semibold text-[var(--text-primary)]">{{ code.code }}</span>
                    <button
                      type="button"
                      class="text-[var(--text-secondary)] transition-colors hover:text-[var(--text-primary)] active:scale-95"
                      :title="t('common.copy')"
                      @click="copyCode(code.code)"
                    >
                      <Copy class="h-4 w-4" />
                    </button>
                    <Badge v-if="code.isGlobal" variant="info">
                      {{ t('aff.globalCodeBadge') }}
                    </Badge>
                  </div>
                </div>

                <div class="text-sm text-[var(--text-secondary)] lg:truncate">
                  {{ code.isGlobal ? t('aff.globalCode') : `${code.packageName} / ${code.planName}` }}
                </div>

                <div class="flex items-center justify-between gap-3 lg:block">
                  <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                    {{ t('aff.discount') }}/{{ t('aff.commission') }}
                  </div>
                  <div class="font-mono text-sm text-[var(--text-secondary)] tabular-nums">
                    {{ (code.discountRate * 100).toFixed(0) }}% / {{ (code.commissionRate * 100).toFixed(0) }}%
                  </div>
                </div>

                <div class="flex items-center justify-between gap-3 lg:block">
                  <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                    {{ t('aff.usedCount') }}
                  </div>
                  <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                    {{ code.usedCount.toLocaleString() }}
                  </div>
                </div>

                <div class="flex items-center justify-between gap-3 lg:block">
                  <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                    {{ t('aff.earnings') }}
                  </div>
                  <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                    {{ formatMoney(code.totalEarnings) }}
                  </div>
                </div>

                <div class="flex justify-start lg:justify-end">
                  <button
                    v-if="code.usedCount === 0"
                    type="button"
                    class="rounded-full p-2 text-[var(--error)] transition-colors hover:bg-[var(--error)]/10 active:scale-95"
                    :disabled="deleteCodeLoading === code.id"
                    :title="t('common.delete')"
                    @click="deleteAffCode(code.id, code.code)"
                  >
                    <Trash2 v-if="deleteCodeLoading !== code.id" class="h-4 w-4" />
                    <Loader2 v-else class="h-4 w-4 animate-spin" />
                  </button>
                </div>
              </div>
            </div>
          </div>
        </Card>

        <!-- 收益明细与提现记录 -->
        <div class="grid gap-6 xl:grid-cols-[minmax(0,1.5fr)_minmax(20rem,1fr)]">
          <!-- 收益明细 -->
          <Card data-reveal padding="none">
            <template #header>
              <div class="px-6 py-4">
                <h3 class="font-semibold text-lg text-[var(--text-primary)]">{{ t('aff.earningsLog') }}</h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('common.total') }} {{ affLogsTotal }} {{ t('common.items') }}</p>
              </div>
            </template>

            <div v-if="affLogsLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
              <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
              <span>{{ t('common.loading') }}...</span>
            </div>
            <EmptyState
              v-else-if="affLogs.length === 0"
              :title="t('aff.noLogs')"
            />
            <div v-else>
              <div class="hidden grid-cols-[minmax(0,1fr)_120px_120px_180px] gap-4 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-6 py-3 text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] lg:grid">
                <div>{{ t('wallet.instanceOrRemark') }}</div>
                <div>{{ t('wallet.amount') }}</div>
                <div>{{ t('wallet.balanceAfter') }}</div>
                <div>{{ t('wallet.time') }}</div>
              </div>

              <div class="divide-y divide-[var(--border-color)]">
                <div
                  v-for="log in affLogs"
                  :key="log.id"
                  class="grid gap-3 px-5 py-4 transition-colors hover:bg-[var(--bg-surface-soft)] sm:px-6 lg:grid-cols-[minmax(0,1fr)_120px_120px_180px] lg:items-center lg:py-3.5"
                >
                  <div class="min-w-0">
                    <div class="lg:hidden">
                      <div class="flex flex-wrap items-center gap-2">
                        <Badge :variant="getAffLogBadgeVariant(log.type)">
                          {{ getAffLogTypeName(log.type) }}
                        </Badge>
                        <button
                          v-if="log.instanceId"
                          class="text-sm font-medium text-[var(--accent)] transition-colors hover:underline"
                          @click="router.push(instanceDetailPath(log.instanceId))"
                        >
                          #{{ log.instanceId }}
                        </button>
                        <span v-else-if="log.affCode" class="font-mono text-sm text-[var(--text-secondary)]">
                          {{ log.affCode }}
                        </span>
                      </div>
                      <div class="mt-2 truncate text-sm text-[var(--text-secondary)]">
                        {{ log.remark || '-' }}
                      </div>
                    </div>

                    <div class="hidden min-w-0 items-center gap-2.5 lg:flex">
                      <Badge :variant="getAffLogBadgeVariant(log.type)" class="shrink-0">
                        {{ getAffLogTypeName(log.type) }}
                      </Badge>
                      <button
                        v-if="log.instanceId"
                        class="shrink-0 text-sm font-medium text-[var(--accent)] transition-colors hover:underline"
                        @click="router.push(instanceDetailPath(log.instanceId))"
                      >
                        #{{ log.instanceId }}
                      </button>
                      <span v-else-if="log.affCode" class="shrink-0 font-mono text-sm text-[var(--text-secondary)]">
                        {{ log.affCode }}
                      </span>
                      <span class="min-w-0 truncate text-sm text-[var(--text-secondary)]" :title="log.remark || undefined">
                        {{ log.remark || '-' }}
                      </span>
                    </div>
                  </div>

                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('wallet.amount') }}
                    </div>
                    <div class="font-mono text-sm font-semibold tabular-nums" :class="getAmountValueClass(log.amount)">
                      {{ log.amount >= 0 ? '+' : '-' }}{{ formatMoney(Math.abs(log.amount)) }}
                    </div>
                  </div>

                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('wallet.balanceAfter') }}
                    </div>
                    <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                      {{ formatMoney(log.balanceAfter) }}
                    </div>
                  </div>

                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('wallet.time') }}
                    </div>
                    <div class="text-sm text-[var(--text-secondary)]">
                      {{ formatDate(log.createdAt) }}
                    </div>
                  </div>
                </div>
              </div>

              <!-- 分页器 -->
              <div v-if="affLogsTotalPages > 1" class="border-t border-[var(--border-color)] px-5 py-4 sm:px-6">
                <Pagination
                  :page="affLogsPage"
                  :page-size="affLogsPageSize"
                  :total="affLogsTotal"
                  @update:page="(p) => { affLogsPage = p; loadAffLogs() }"
                >
                  <template #total>
                    <span>{{ t('common.total') }} {{ affLogsTotal }} {{ t('common.items') }}</span>
                  </template>
                </Pagination>
              </div>
            </div>
          </Card>

          <!-- 提现记录 -->
          <Card data-reveal padding="none">
            <template #header>
              <div class="px-6 py-4">
                <h3 class="font-semibold text-lg text-[var(--text-primary)]">{{ t('aff.withdrawals') }}</h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('common.total') }} {{ affWithdrawalsTotal }} {{ t('common.items') }}</p>
              </div>
            </template>

            <div v-if="affWithdrawalsLoading" class="p-12 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
              <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
              <span>{{ t('common.loading') }}...</span>
            </div>
            <EmptyState
              v-else-if="affWithdrawals.length === 0"
              :title="t('aff.noWithdrawals')"
            />
            <div v-else>
              <div class="hidden grid-cols-[110px_100px_160px_minmax(0,1fr)] gap-4 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-6 py-3 text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] lg:grid">
                <div>{{ t('wallet.amount') }}</div>
                <div>{{ t('aff.status') }}</div>
                <div>{{ t('aff.requestTime') }}</div>
                <div>{{ t('aff.rejectReason') }}</div>
              </div>

              <div class="divide-y divide-[var(--border-color)]">
                <div
                  v-for="w in affWithdrawals"
                  :key="w.id"
                  class="grid gap-3 px-5 py-4 transition-colors hover:bg-[var(--bg-surface-soft)] sm:px-6 lg:grid-cols-[110px_100px_160px_minmax(0,1fr)] lg:items-center lg:py-3.5"
                >
                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('wallet.amount') }}
                    </div>
                    <div class="font-mono text-sm font-medium text-[var(--text-primary)] tabular-nums">
                      {{ formatMoney(w.amount) }}
                    </div>
                  </div>

                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('aff.status') }}
                    </div>
                    <Badge :variant="getWithdrawalBadgeVariant(w.status)">
                      {{ getWithdrawalStatusName(w.status) }}
                    </Badge>
                  </div>

                  <div class="flex items-center justify-between gap-3 lg:block">
                    <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)] lg:hidden">
                      {{ t('aff.requestTime') }}
                    </div>
                    <div class="text-sm text-[var(--text-secondary)]">
                      {{ formatDate(w.createdAt) }}
                    </div>
                  </div>

                  <div class="text-sm text-[var(--text-secondary)] lg:truncate">
                    {{ w.rejectReason || '-' }}
                  </div>
                </div>
              </div>

              <!-- 分页器 -->
              <div v-if="affWithdrawalsTotalPages > 1" class="border-t border-[var(--border-color)] px-5 py-4 sm:px-6">
                <Pagination
                  :page="affWithdrawalsPage"
                  :page-size="affWithdrawalsPageSize"
                  :total="affWithdrawalsTotal"
                  @update:page="(p) => { affWithdrawalsPage = p; loadAffWithdrawals() }"
                >
                  <template #total>
                    <span>{{ t('common.total') }} {{ affWithdrawalsTotal }} {{ t('common.items') }}</span>
                  </template>
                </Pagination>
              </div>
            </div>
          </Card>
        </div>
      </template>
    </div>

    <!-- 充值弹窗 -->
    <DrawerModal
      v-if="!configStore.freeSiteMode"
      :show="showRechargeModal"
      max-width="max-w-md"
      raw
      @close="showRechargeModal = false"
    >
      <!-- 头部 -->
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]">
            <CreditCard class="h-5 w-5 stroke-[1.8]" />
          </div>
          <div>
            <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('wallet.recharge') }}</h3>
            <p class="text-xs text-[var(--text-secondary)]">{{ t('wallet.description') }}</p>
          </div>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showRechargeModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-5">
        <!-- 支付渠道选择 -->
        <div>
          <label class="mb-2 block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.paymentMethod') }}</label>
          <div v-if="providersLoading" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-center text-sm text-[var(--text-secondary)]">
            <span class="inline-flex items-center justify-center gap-2">
              <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
              {{ t('common.loading') }}
            </span>
          </div>
          <div v-else-if="providers.length === 0" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-center text-sm text-[var(--text-secondary)]">
            {{ t('wallet.noProviders') }}
          </div>
          <div v-else class="grid grid-cols-2 gap-3">
            <button
              v-for="p in providers"
              :key="p.id"
              type="button"
              class="flex items-center justify-center gap-2 rounded-xl border p-4 text-sm font-medium transition-all duration-200 active:scale-95"
              :class="selectedProvider === p.id
                ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold shadow-sm'
                : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-primary)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]'"
              @click="selectedProvider = p.id; selectedPaymentMethod = ''"
            >
              {{ p.name }}
            </button>
          </div>
        </div>

        <!-- 支付方式选择 (仅当所选渠道支持多种方式时显示) -->
        <div v-if="selectedProviderInfo && selectedProviderInfo.type !== 'heleket' && selectedProviderInfo.methods && selectedProviderInfo.methods.length > 1">
          <label class="mb-2 block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.paymentMethodType') }}</label>
          <div class="grid grid-cols-2 gap-3">
            <button
              v-for="method in selectedProviderInfo.methods"
              :key="method"
              type="button"
              class="p-3.5 rounded-xl border text-sm font-medium transition-all duration-200 flex items-center justify-center gap-2.5 active:scale-95"
              :class="[
                selectedPaymentMethod === method || (!selectedPaymentMethod && selectedProviderInfo.methods[0] === method)
                  ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold shadow-sm'
                  : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-primary)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]'
              ]"
              @click="selectedPaymentMethod = method"
            >
              <!-- 支付宝图标 -->
              <svg v-if="method === 'alipay'" class="w-5 h-5 text-blue-500 shrink-0" viewBox="0 0 24 24" fill="currentColor">
                <path d="M21.422 15.358c-3.83-1.153-6.055-1.84-7.358-2.22a15.503 15.503 0 001.07-3.122h-4.067v-1.2h4.89V7.69h-4.89V5.5H9.333v2.19H4.5v1.126h4.833v1.2H5.266v1.124h7.09a12.14 12.14 0 01-.671 1.883c-1.627-.396-3.177-.586-4.384-.513-3.407.206-5.136 2.051-5.26 3.708-.153 2.038 1.57 3.728 4.49 3.782 2.448.045 4.793-1.083 6.51-3.058.756.39 1.603.831 2.542 1.323.939.492 1.97 1.033 3.114 1.632h5.25v-3.514c-.167-.053-.324-.095-.525-.025zM7.028 18.93c-1.86-.143-2.66-1.062-2.583-1.95.076-.882.973-1.712 2.526-1.806.984-.06 2.243.13 3.685.508a7.016 7.016 0 01-3.628 3.248z" />
              </svg>
              <!-- 微信支付图标 -->
              <svg v-else-if="method === 'wxpay'" class="w-5 h-5 text-emerald-500 shrink-0" viewBox="0 0 24 24" fill="currentColor">
                <path d="M8.691 2.188C3.891 2.188 0 5.476 0 9.53c0 2.212 1.17 4.203 3.002 5.55a.59.59 0 01.213.665l-.39 1.48c-.019.07-.048.141-.048.213 0 .163.13.295.29.295a.326.326 0 00.167-.054l1.903-1.114a.864.864 0 01.717-.098 10.16 10.16 0 002.837.403c.276 0 .543-.027.811-.05-.857-2.578.157-4.972 1.932-6.446 1.703-1.415 3.882-1.98 5.853-1.838-.576-3.583-4.196-6.348-8.596-6.348zM5.785 5.991c.642 0 1.162.529 1.162 1.18a1.17 1.17 0 01-1.162 1.178A1.17 1.17 0 014.623 7.17c0-.651.52-1.18 1.162-1.18zm5.813 0c.642 0 1.162.529 1.162 1.18a1.17 1.17 0 01-1.162 1.178 1.17 1.17 0 01-1.162-1.178c0-.651.52-1.18 1.162-1.18zm5.34 2.867c-1.797-.052-3.746.512-5.28 1.786-1.72 1.428-2.687 3.72-1.78 6.22.942 2.453 3.666 4.229 6.884 4.229.826 0 1.622-.12 2.361-.336a.722.722 0 01.598.082l1.584.926a.272.272 0 00.14.045c.133 0 .241-.108.241-.241 0-.06-.023-.12-.038-.177l-.327-1.233a.582.582 0 01-.023-.156.49.49 0 01.201-.398C23.024 18.48 24 16.82 24 14.98c0-3.21-2.931-5.837-6.656-6.088l-.105-.001-.301-.033zm-2.746 2.769a.96.96 0 110 1.92.96.96 0 010-1.92zm4.93 0a.96.96 0 110 1.92.96.96 0 010-1.92z" />
              </svg>
              <!-- 其他支付方式 -->
              <CreditCard v-else class="w-5 h-5 text-[var(--text-secondary)] shrink-0" />
              <span>{{ getPaymentMethodName(method) }}</span>
            </button>
          </div>
        </div>

        <div v-else-if="selectedProviderInfo?.type === 'heleket'" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-xs text-[var(--text-secondary)]">
          {{ t('wallet.heleketSelectionHint') }}
        </div>

        <!-- 充值金额 -->
        <div>
          <label class="mb-2 block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.amountLabel') }}</label>
          <div class="mb-3 grid grid-cols-4 gap-2">
            <button
              v-for="amt in [10, 50, 100, 200]"
              :key="amt"
              type="button"
              class="rounded-xl py-2.5 font-mono text-sm font-semibold tabular-nums border transition-all duration-200 active:scale-95"
              :class="Number(rechargeAmount) === amt
                ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-bold'
                : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-primary)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface)]'"
              @click="rechargeAmount = amt"
            >
              ¥{{ amt }}
            </button>
          </div>
          <Input
            :model-value="rechargeAmount"
            type="number"
            size="lg"
            placeholder="0"
            @update:model-value="(val) => rechargeAmount = Number(val)"
            @blur="formatAmount"
          >
            <template #prefix>
              <span class="font-semibold text-base text-[var(--text-secondary)]">¥</span>
            </template>
          </Input>
          <div v-if="selectedProviderInfo" class="mt-2 flex items-center gap-1.5 text-xs text-[var(--text-secondary)]">
            <Info class="h-3.5 w-3.5 shrink-0" />
            <span>
              {{ t('wallet.amountRange') }}:
              {{ selectedProviderInfo.minAmount.toFixed(2) }} -
              {{ selectedProviderInfo.maxAmount ? selectedProviderInfo.maxAmount.toFixed(2) : t('common.unlimited') }}
            </span>
          </div>
        </div>

        <!-- 费用预览 -->
        <div
          v-if="selectedProviderInfo && selectedRechargeFee > 0"
          class="rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3.5 text-sm text-[var(--warning)]"
        >
          <div class="flex items-start gap-2.5">
            <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
            <div class="min-w-0 space-y-1">
              <div>
                {{ t('wallet.feeNote') }}:
                <span v-if="selectedFeeConfig.feeRate > 0" class="font-mono font-medium tabular-nums">{{ (selectedFeeConfig.feeRate * 100).toFixed(2) }}%</span>
                <span v-if="selectedFeeConfig.feeRate > 0 && selectedFeeConfig.feeFixed > 0"> + </span>
                <span v-if="selectedFeeConfig.feeFixed > 0" class="font-mono font-medium tabular-nums">¥{{ selectedFeeConfig.feeFixed.toFixed(2) }}</span>
                <span class="font-mono font-medium tabular-nums"> = ¥{{ selectedRechargeFee.toFixed(2) }}</span>
              </div>
              <div>
                {{ t('wallet.payableAmount') }}:
                <span class="font-mono font-semibold tabular-nums">¥{{ selectedPayableAmount.toFixed(2) }}</span>
                <span class="text-[var(--text-secondary)]"> / {{ t('wallet.actualAmount') }} <span class="font-mono tabular-nums">¥{{ selectedCreditAmount.toFixed(2) }}</span></span>
              </div>
            </div>
          </div>
        </div>

        <!-- 确认复选框组 -->
        <div class="space-y-2.5">
          <!-- 无退款确认 -->
          <label for="agree-no-refund" class="flex items-start gap-2.5 cursor-pointer">
            <input
              id="agree-no-refund"
              v-model="agreedToNoRefund"
              type="checkbox"
              class="mt-0.5 h-4 w-4 shrink-0 rounded border-[var(--border-color)] text-[var(--error)] focus:ring-[var(--error)] bg-[var(--bg-surface-soft)]"
            />
            <span class="text-xs text-[var(--error)] leading-relaxed">
              {{ t('wallet.noRefundNotice') }}
            </span>
          </label>

          <!-- 充值须知确认 -->
          <label for="agree-recharge-notice" class="flex items-start gap-2.5 cursor-pointer">
            <input
              id="agree-recharge-notice"
              v-model="agreedToRechargeNotice"
              type="checkbox"
              class="mt-0.5 h-4 w-4 shrink-0 rounded border-[var(--border-color)] text-[var(--error)] focus:ring-[var(--error)] bg-[var(--bg-surface-soft)]"
            />
            <span class="text-xs text-[var(--error)] leading-relaxed">
              {{ t('wallet.rechargeNotice') }}
            </span>
          </label>

          <!-- 服务条款勾选 -->
          <label for="agree-terms-recharge" class="flex items-start gap-2.5 cursor-pointer">
            <input
              id="agree-terms-recharge"
              v-model="agreedToTerms"
              type="checkbox"
              class="mt-0.5 h-4 w-4 shrink-0 rounded border-[var(--border-color)] text-[var(--error)] focus:ring-[var(--error)] bg-[var(--bg-surface-soft)]"
            />
            <span class="text-xs text-[var(--error)] leading-relaxed">
              {{ t('auth.tos.agreePrefix') }}
              <button
                type="button"
                class="text-[var(--error)] hover:underline font-semibold"
                @click.prevent="showTermsModal = true"
              >
                {{ t('auth.tos.termsLink') }}
              </button>
            </span>
          </label>
        </div>
      </div>
      
      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showRechargeModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          class="min-w-[120px]"
          :disabled="providersLoading || rechargeLoading || !selectedProvider || rechargeAmount <= 0 || !agreedToNoRefund || !agreedToRechargeNotice || !agreedToTerms"
          :loading="rechargeLoading"
          @click="createRechargeOrder"
        >
          <template #icon>
            <CreditCard class="w-4 h-4" />
          </template>
          <span>{{ providersLoading ? t('common.loading') : (rechargeLoading ? t('common.processing') : t('wallet.pay')) }}</span>
        </Button>
      </div>
    </DrawerModal>

    <!-- 人工充值付款说明 -->
    <DrawerModal
      v-if="manualPaymentInfo"
      :show="showManualPaymentModal"
      max-width="max-w-lg"
      raw
      @close="showManualPaymentModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div>
          <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('wallet.manualPaymentTitle') }}</h3>
          <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('wallet.manualPaymentDesc') }}</p>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showManualPaymentModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-4">
        <div class="grid gap-3 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-sm sm:grid-cols-2">
          <div>
            <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.orderNo') }}</div>
            <div class="mt-1 break-all font-mono font-semibold text-[var(--text-primary)]">{{ manualPaymentInfo.orderNo }}</div>
          </div>
          <div>
            <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.payableAmount') }}</div>
            <div class="mt-1 font-mono font-semibold text-[var(--text-primary)] tabular-nums">¥{{ manualPaymentInfo.payableAmount.toFixed(2) }}</div>
          </div>
          <div v-if="manualPaymentInfo.providerName">
            <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.paymentChannel') }}</div>
            <div class="mt-1 font-semibold text-[var(--text-primary)]">{{ manualPaymentInfo.providerName }}</div>
          </div>
          <div>
            <div class="text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('wallet.actualAmount') }}</div>
            <div class="mt-1 font-mono font-semibold text-[var(--text-primary)] tabular-nums">¥{{ (manualPaymentInfo.actualAmount ?? manualPaymentInfo.payableAmount).toFixed(2) }}</div>
          </div>
        </div>

        <div>
          <div class="mb-2 text-sm font-semibold text-[var(--text-primary)]">{{ t('wallet.manualPaymentInstructions') }}</div>
          <pre class="max-h-80 whitespace-pre-wrap break-words rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 font-mono text-xs leading-relaxed text-[var(--text-primary)]">{{ manualPaymentInfo.instructions }}</pre>
        </div>

        <div class="rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3.5 text-xs text-[var(--warning)] leading-relaxed">
          {{ t('wallet.manualPaymentPendingHint') }}
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end">
        <Button variant="primary" @click="showManualPaymentModal = false">{{ t('common.confirm') }}</Button>
      </div>
    </DrawerModal>

    <!-- 创建优惠码弹窗 -->
    <DrawerModal
      :show="showCreateCodeModal"
      max-width="max-w-md"
      raw
      @close="showCreateCodeModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('aff.createCode') }}</h3>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showCreateCodeModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-4">
        <div>
          <label class="block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-1.5">{{ t('aff.selectPlan') }}</label>
          <p class="text-xs text-[var(--text-secondary)] mb-3">{{ t('aff.selectPlanHint') }}</p>

          <div v-if="affPlansLoading" class="p-6 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
            <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
            <span>{{ t('common.loading') }}...</span>
          </div>
          <div v-else-if="affPlans.length === 0" class="p-6 text-center text-sm text-[var(--text-secondary)]">
            {{ t('aff.noCodes') }}
          </div>
          <div v-else class="max-h-64 space-y-2 overflow-y-auto pr-1">
            <!-- 全局优惠码选项 -->
            <label
              class="flex cursor-pointer items-center gap-3 rounded-xl border p-3.5 transition-all duration-200"
              :class="[
                selectedPlanId === -1
                  ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] shadow-sm'
                  : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]'
              ]"
            >
              <input
                v-model="selectedPlanId"
                type="radio"
                :value="-1"
                class="h-4 w-4 text-[var(--accent)] focus:ring-[var(--accent)]"
              />
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="font-medium text-sm text-[var(--text-primary)]">{{ t('aff.globalCode') }}</span>
                  <Badge variant="info" size="sm">{{ t('aff.globalCodeBadge') }}</Badge>
                </div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('aff.globalCodeHint') }}</div>
              </div>
            </label>

            <!-- 分隔线 -->
            <div v-if="affPlans.length > 0" class="relative py-2">
              <div class="absolute inset-0 flex items-center">
                <div class="w-full border-t border-[var(--border-color)]"></div>
              </div>
              <div class="relative flex justify-center text-xs">
                <span class="bg-[var(--bg-surface)] px-2 text-[var(--text-tertiary)]">{{ t('aff.orSelectPlan') }}</span>
              </div>
            </div>

            <!-- 方案专有码选项 -->
            <label
              v-for="plan in affPlans"
              :key="plan.id"
              class="flex cursor-pointer items-center gap-3 rounded-xl border p-3.5 transition-all duration-200"
              :class="[
                plan.hasCode
                  ? 'cursor-not-allowed border-[var(--border-color)] bg-[var(--bg-surface-soft)] opacity-50'
                  : selectedPlanId === plan.id
                    ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]'
              ]"
            >
              <input
                v-model="selectedPlanId"
                type="radio"
                :value="plan.id"
                :disabled="plan.hasCode"
                class="h-4 w-4 text-[var(--accent)] focus:ring-[var(--accent)]"
              />
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="font-medium text-sm text-[var(--text-primary)]">{{ plan.name }}</span>
                  <span v-if="plan.hasCode" class="text-xs text-[var(--text-tertiary)]">({{ t('aff.alreadyCreated') }})</span>
                </div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ plan.packageName }} · <span class="font-mono tabular-nums">¥{{ (parseFloat(plan.price) / 100).toFixed(2) }}</span>
                </div>
              </div>
            </label>
          </div>
        </div>

        <!-- 固定折扣说明 -->
        <div class="border-t border-[var(--border-color)] pt-4">
          <div class="flex items-center justify-between">
            <div>
              <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('aff.fixedRate') }}</div>
              <p class="mt-0.5 text-xs text-[var(--text-secondary)]">{{ t('aff.fixedRateHint') }}</p>
            </div>
            <div class="text-right">
              <div class="font-mono text-2xl font-bold text-[var(--text-primary)] tabular-nums">5%</div>
              <div class="text-xs text-[var(--text-secondary)]">{{ t('aff.discount') }} 5% / {{ t('aff.commission') }} 5%</div>
            </div>
          </div>
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showCreateCodeModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          :disabled="createCodeLoading || selectedPlanId === null"
          :loading="createCodeLoading"
          @click="createAffCode"
        >
          {{ createCodeLoading ? t('common.processing') : t('common.create') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- 转化申请弹窗 -->
    <DrawerModal
      :show="showConvertModal"
      max-width="max-w-md"
      raw
      @close="showConvertModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('aff.convertModal.title') }}</h3>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showConvertModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-4">
        <div>
          <label class="mb-1 block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('aff.convertModal.currentBalance') }}</label>
          <div class="font-mono text-3xl font-bold text-[var(--text-primary)] tabular-nums">
            ¥{{ affStatus?.currentBalance.toFixed(2) || '0.00' }}
          </div>
        </div>

        <div>
          <label class="mb-1.5 block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">{{ t('aff.convertModal.amount') }}</label>
          <Input
            :model-value="convertAmount"
            type="number"
            size="lg"
            placeholder="0.00"
            @update:model-value="(val) => convertAmount = Number(val)"
          >
            <template #prefix>
              <span class="font-semibold text-base text-[var(--text-secondary)]">¥</span>
            </template>
          </Input>
          <p class="mt-1.5 text-xs text-[var(--text-secondary)]">{{ t('aff.convertModal.minAmount') }}</p>
        </div>

        <div class="rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3.5 text-xs text-[var(--warning)] leading-relaxed">
          <div class="flex items-start gap-2">
            <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
            <span>{{ t('aff.convertModal.hint') }}</span>
          </div>
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showConvertModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          :disabled="convertLoading || convertAmount < 0.1 || convertAmount > (affStatus?.currentBalance || 0)"
          :loading="convertLoading"
          @click="submitConvert"
        >
          {{ convertLoading ? t('common.processing') : t('aff.convertModal.submit') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- AFF 榜单弹窗 -->
    <DrawerModal
      :show="showLeaderboardModal"
      max-width="max-w-md"
      raw
      @close="showLeaderboardModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div class="flex items-center gap-2">
          <Trophy class="w-5 h-5 text-[var(--warning)]" />
          <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('aff.leaderboard.title') }}</h3>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showLeaderboardModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-0">
        <div v-if="leaderboardLoading" class="p-8 text-center text-sm text-[var(--text-secondary)] flex items-center justify-center gap-2">
          <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
          <span>{{ t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="leaderboard.length === 0"
          :title="t('aff.leaderboard.empty')"
        />
        <div v-else class="divide-y divide-[var(--border-color)] max-h-96 overflow-y-auto">
          <div
            v-for="(entry, index) in leaderboard"
            :key="entry.rank"
            class="flex items-center justify-between px-6 py-3.5 transition-colors"
            :class="[
              entry.isCurrentUser ? 'bg-[color-mix(in_srgb,var(--accent)_10%,var(--bg-surface))]' : 'hover:bg-[var(--bg-surface-soft)]',
            ]"
            :style="{ animationDelay: `${index * 40}ms` }"
          >
            <div class="flex items-center gap-3">
              <div
                class="flex h-8 w-8 items-center justify-center rounded-full font-mono text-sm font-bold tabular-nums"
                :class="getRankStyle(entry.rank)"
              >
                <span>{{ entry.rank }}</span>
              </div>
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">
                  {{ entry.username }}
                </div>
                <div v-if="entry.isCurrentUser" class="text-xs font-medium text-[var(--accent)]">
                  {{ t('aff.leaderboard.you') }}
                </div>
              </div>
            </div>
            <div class="text-right">
              <span class="font-mono font-semibold text-[var(--success)] tabular-nums">¥{{ entry.totalEarnings.toFixed(2) }}</span>
            </div>
          </div>
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4">
        <Button variant="primary" block @click="showLeaderboardModal = false">
          {{ t('common.close') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- 服务条款弹窗 -->
    <TermsOfServiceModal
      :show="showTermsModal"
      @close="showTermsModal = false"
    />
  </div>
</template>

<style scoped>
.figure-hero {
  letter-spacing: -0.04em;
  font-feature-settings: 'tnum';
}

.figure-stat {
  letter-spacing: -0.03em;
  font-feature-settings: 'tnum';
}

.tab-scroll-hide {
  scrollbar-width: none;
  -ms-overflow-style: none;
}

.tab-scroll-hide::-webkit-scrollbar {
  display: none;
}
</style>
