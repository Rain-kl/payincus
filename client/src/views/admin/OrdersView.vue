<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import {
  AlertCircle,
  Check,
  Info,
  Loader2,
  RefreshCw,
  RotateCcw,
  Search,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useReveal } from '@/composables/useReveal'
import IconButton from '@/components/IconButton.vue'
import DrawerModal from '@/components/DrawerModal.vue'
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

defineOptions({ name: 'AdminOrdersView' })

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

type OrderSourceType = 'recharge' | 'instance_billing'
type OrderOperationStatus = 'pending_review' | 'confirmed' | 'compensated' | 'closed'

const route = useRoute()

interface OrderOperationCase {
  id: number
  sourceType: OrderSourceType
  sourceId: number
  orderNo: string | null
  userId: number
  status: OrderOperationStatus
  reason: string
  result: string | null
  refundAmount: number | null
  providerSummary: Record<string, any> | null
  createdBy: { id: number; username: string } | null
  updatedBy: { id: number; username: string } | null
  balanceAdjustmentRequestId: number | null
  balanceAdjustmentRequest: BalanceAdjustmentRequest | null
  createdAt: string
  updatedAt: string
}

interface AdminOrderItem {
  id: string
  sourceType: OrderSourceType
  sourceId: number
  orderNo: string
  title: string
  status: string
  rawStatus: string
  amount: number
  actualAmount: number | null
  fee: number
  userId: number
  user: { id: number; username: string; email: string } | null
  provider: { id: number; name: string; type: string } | null
  providerStatusSummary: {
    provider: { id: number; name: string; type: string } | null
    rawStatus: string
    paymentMethod: string | null
    tradeNo: string | null
    callbackAt: string | null
  } | null
  paymentMethod: string | null
  tradeNo: string | null
  failReason: string | null
  instance: {
    id: number
    name: string
    displayName: string | null
    packagePlan?: { id: number; name: string; package?: { id: number; name: string } | null } | null
  } | null
  months: number | null
  periodStart: string | null
  periodEnd: string | null
  remark: string | null
  createdAt: string
  completedAt: string | null
  expiredAt: string | null
  operationCase: OrderOperationCase | null
}

interface BalanceAdjustmentRequest {
  id: number
  userId: number
  user: { id: number; username: string; email: string | null }
  requestedBy: { id: number; username: string }
  reviewedBy: { id: number; username: string } | null
  amount: number
  requestType: string
  status: string
  sourceType: string | null
  sourceId: number | null
  orderNo: string | null
  reason: string
  reviewRemark: string | null
  balanceLogId: number | null
  balanceLog: {
    id: number
    type: string
    amount: number
    balanceBefore: number
    balanceAfter: number
  } | null
  createdAt: string
  updatedAt: string
  reviewedAt: string | null
}

const orders = ref<AdminOrderItem[]>([])
const selectedOrder = ref<AdminOrderItem | null>(null)
const adjustmentRequests = ref<BalanceAdjustmentRequest[]>([])
const loading = ref(false)
const requestLoading = ref(false)
const detailLoading = ref(false)
const error = ref('')
const requestError = ref('')
const page = ref(1)
const pageSize = ref(10)
const total = ref(0)
const requestPage = ref(1)
const requestPageSize = 7
const requestTotal = ref(0)
const requestStatus = ref('pending')
const type = ref('')
const status = ref('')
const userId = ref('')
const keyword = ref('')
const createdFrom = ref('')
const createdTo = ref('')
const actionLoading = ref(false)
const reviewLoadingId = ref<number | null>(null)
const actionMessage = ref('')
const actionError = ref('')
const completeTradeNo = ref('')
const completeActualAmount = ref('')
const failReason = ref('')
const adjustmentAmount = ref('')
const adjustmentRemark = ref('')
const operationStatus = ref<OrderOperationStatus>('pending_review')
const operationReason = ref('')
const operationResult = ref('')
const operationRefundAmount = ref('')
const operationCreateRefundRequest = ref(false)

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))
const requestTotalPages = computed(() => Math.max(1, Math.ceil(requestTotal.value / requestPageSize)))

const typeOptions = [
  { value: '', label: '全部类型' },
  { value: 'recharge', label: '余额充值' },
  { value: 'instance_billing', label: '实例账单' }
]

const statusOptions = computed(() => {
  if (type.value === 'instance_billing') {
    return [
      { value: '', label: '全部状态' },
      { value: 'newPurchase', label: '新购' },
      { value: 'renew', label: '续费' },
      { value: 'upgrade', label: '升级' },
      { value: 'downgrade', label: '降级' },
      { value: 'refund', label: '退款' },
      { value: 'transfer_fee', label: '转移手续费' }
    ]
  }

  return [
    { value: '', label: '全部状态' },
    { value: 'pending', label: '待支付' },
    { value: 'paid', label: '处理中' },
    { value: 'completed', label: '已完成' },
    { value: 'failed', label: '失败' },
    { value: 'cancelled', label: '已取消' },
    { value: 'refunded', label: '已退款' }
  ]
})

const requestStatusOptions = [
  { value: 'pending', label: '待审核' },
  { value: 'approved', label: '已通过' },
  { value: 'rejected', label: '已驳回' },
  { value: '', label: '全部' }
]

const operationStatusOptions: Array<{ value: OrderOperationStatus; label: string }> = [
  { value: 'pending_review', label: '待核查' },
  { value: 'confirmed', label: '已确认' },
  { value: 'compensated', label: '已补偿' },
  { value: 'closed', label: '已关闭' }
]

function formatMoney(value: number | null | undefined): string {
  return `¥${Number(value || 0).toFixed(2)}`
}

function formatTime(value: string | null | undefined): string {
  if (!value) return '-'
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

function statusLabel(order: AdminOrderItem): string {
  const labels: Record<string, string> = {
    completed: '已完成',
    pending: '待处理',
    paid: '处理中',
    failed: '失败',
    cancelled: '已取消',
    refunded: '已退款',
    newPurchase: '新购',
    renew: '续费',
    upgrade: '升级',
    downgrade: '降级',
    refund: '退款',
    transfer_fee: '转移手续费'
  }
  return labels[order.status] || order.rawStatus || order.status
}

function statusBadgeVariant(statusValue: string): BadgeVariant {
  switch (statusValue) {
    case 'completed':
    case 'newPurchase':
    case 'renew':
      return 'success'
    case 'pending':
    case 'paid':
    case 'upgrade':
    case 'downgrade':
      return 'warning'
    case 'failed':
      return 'error'
    case 'cancelled':
    case 'refunded':
    case 'refund':
    case 'transfer_fee':
    default:
      return 'default'
  }
}

function operationStatusLabel(statusValue: string | null | undefined): string {
  const labels: Record<string, string> = {
    pending_review: '待核查',
    confirmed: '已确认',
    compensated: '已补偿',
    closed: '已关闭'
  }
  return statusValue ? labels[statusValue] || statusValue : '未登记'
}

function operationStatusBadgeVariant(statusValue: string | null | undefined): BadgeVariant {
  switch (statusValue) {
    case 'pending_review':
      return 'warning'
    case 'confirmed':
      return 'info'
    case 'compensated':
      return 'success'
    case 'closed':
    default:
      return 'default'
  }
}

function sourceLabel(sourceType: OrderSourceType): string {
  return sourceType === 'recharge' ? '充值订单' : '实例账单'
}

function adjustmentTypeLabel(requestType: string): string {
  return requestType === 'refund' ? '退款补偿' : '人工调账'
}

function adjustmentStatusLabel(statusValue: string): string {
  const labels: Record<string, string> = {
    pending: '待审核',
    approved: '已通过',
    rejected: '已驳回'
  }
  return labels[statusValue] || statusValue
}

function adjustmentStatusBadgeVariant(statusValue: string): BadgeVariant {
  switch (statusValue) {
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

const canCompleteRecharge = computed(() => {
  return selectedOrder.value?.sourceType === 'recharge' && ['pending', 'paid'].includes(selectedOrder.value.rawStatus)
})

const canFailRecharge = computed(() => {
  return selectedOrder.value?.sourceType === 'recharge' && ['pending', 'paid'].includes(selectedOrder.value.rawStatus)
})

const canAdjustBalance = computed(() => {
  return Boolean(selectedOrder.value?.userId)
})

function instanceName(order: AdminOrderItem): string {
  if (!order.instance) return '-'
  return order.instance.displayName || order.instance.name
}

function parsedUserId(): number | undefined {
  const trimmed = userId.value.trim()
  if (!/^[1-9]\d*$/.test(trimmed)) return undefined
  return Number(trimmed)
}

function syncOperationForm(order: AdminOrderItem) {
  const operationCase = order.operationCase
  operationStatus.value = operationCase?.status || 'pending_review'
  operationReason.value = operationCase?.reason || (order.sourceType === 'recharge'
    ? `充值订单 ${order.orderNo} 待核查`
    : `实例账单 ${order.orderNo} 待核查`)
  operationResult.value = operationCase?.result || ''
  operationRefundAmount.value = operationCase?.refundAmount ? String(operationCase.refundAmount) : ''
  operationCreateRefundRequest.value = false
}

const hasPendingRefundRequest = computed(() => {
  const request = selectedOrder.value?.operationCase?.balanceAdjustmentRequest
  return request?.requestType === 'refund' && request.status === 'pending'
})

async function loadOrders() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.orders.list({
      page: page.value,
      pageSize: pageSize.value,
      type: type.value || undefined,
      status: status.value || undefined,
      userId: parsedUserId(),
      keyword: keyword.value.trim() || undefined,
      createdFrom: createdFrom.value || undefined,
      createdTo: createdTo.value || undefined
    })
    orders.value = res.orders as AdminOrderItem[]
    total.value = res.total
  } catch (err: any) {
    error.value = err?.message || '订单加载失败'
  } finally {
    loading.value = false
  }
}

async function loadAdjustmentRequests() {
  requestLoading.value = true
  requestError.value = ''
  try {
    const res = await api.admin.getBalanceAdjustmentRequests({
      page: requestPage.value,
      pageSize: requestPageSize,
      status: requestStatus.value || undefined
    })
    adjustmentRequests.value = res.requests as BalanceAdjustmentRequest[]
    requestTotal.value = res.total
  } catch (err: any) {
    requestError.value = err?.message || '调账审批任务加载失败'
  } finally {
    requestLoading.value = false
  }
}

async function openDetail(order: AdminOrderItem) {
  selectedOrder.value = order
  actionMessage.value = ''
  actionError.value = ''
  completeTradeNo.value = order.tradeNo || ''
  completeActualAmount.value = order.actualAmount ? String(order.actualAmount) : ''
  failReason.value = order.failReason || ''
  adjustmentAmount.value = ''
  adjustmentRemark.value = order.sourceType === 'recharge'
    ? `订单 ${order.orderNo} 人工处理`
    : `账单 ${order.orderNo} 人工处理`
  syncOperationForm(order)
  detailLoading.value = true
  try {
    const res = await api.orders.detail(order.sourceType, order.sourceId)
    selectedOrder.value = res.order as AdminOrderItem
    completeTradeNo.value = selectedOrder.value.tradeNo || ''
    completeActualAmount.value = selectedOrder.value.actualAmount ? String(selectedOrder.value.actualAmount) : ''
    syncOperationForm(selectedOrder.value)
  } finally {
    detailLoading.value = false
  }
}

function parseActionAmount(value: string): number | null {
  const trimmed = value.trim()
  if (!trimmed) return null
  if (!/^-?\d+(\.\d{1,2})?$/.test(trimmed)) return null
  const parsed = Number(trimmed)
  return Number.isFinite(parsed) && parsed !== 0 ? parsed : null
}

function parseOptionalPositiveAmount(value: string): number | undefined | null {
  const trimmed = value.trim()
  if (!trimmed) return undefined
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) return null
  const parsed = Number(trimmed)
  return Number.isFinite(parsed) && parsed > 0 ? parsed : null
}

async function refreshSelectedOrder() {
  if (!selectedOrder.value) return
  const current = selectedOrder.value
  const res = await api.orders.detail(current.sourceType, current.sourceId)
  selectedOrder.value = res.order as AdminOrderItem
  await loadOrders()
}

async function completeRecharge() {
  if (!selectedOrder.value || !canCompleteRecharge.value) return
  const actualAmount = parseOptionalPositiveAmount(completeActualAmount.value)
  if (actualAmount === null) {
    actionError.value = '实际入账金额必须是大于 0 的金额，最多两位小数'
    return
  }

  actionLoading.value = true
  actionMessage.value = ''
  actionError.value = ''
  try {
    const result = await api.admin.completeRechargeOrder(
      selectedOrder.value.orderNo,
      completeTradeNo.value.trim() || undefined,
      actualAmount
    )
    actionMessage.value = result.message || '订单已手动完成'
    await refreshSelectedOrder()
  } catch (err: any) {
    actionError.value = err?.message || '手动完成失败'
  } finally {
    actionLoading.value = false
  }
}

async function failRecharge() {
  if (!selectedOrder.value || !canFailRecharge.value) return
  const reason = failReason.value.trim()
  if (!reason) {
    actionError.value = '请填写失败原因'
    return
  }

  actionLoading.value = true
  actionMessage.value = ''
  actionError.value = ''
  try {
    const result = await api.admin.failRechargeOrder(selectedOrder.value.orderNo, reason)
    actionMessage.value = result.message || '订单已标记失败'
    await refreshSelectedOrder()
  } catch (err: any) {
    actionError.value = err?.message || '标记失败失败'
  } finally {
    actionLoading.value = false
  }
}

async function adjustBalance() {
  if (!selectedOrder.value || !canAdjustBalance.value) return
  const amount = parseActionAmount(adjustmentAmount.value)
  const remark = adjustmentRemark.value.trim()
  if (amount === null) {
    actionError.value = '调账金额必须是非 0 金额，最多两位小数；退款/补款填正数，扣款填负数'
    return
  }
  if (!remark) {
    actionError.value = '请填写调账原因'
    return
  }

  actionLoading.value = true
  actionMessage.value = ''
  actionError.value = ''
  try {
    const result = await api.admin.createBalanceAdjustmentRequest(selectedOrder.value.userId, {
      amount,
      reason: remark,
      requestType: amount > 0 ? 'refund' : 'manual_adjust',
      sourceType: selectedOrder.value.sourceType,
      sourceId: selectedOrder.value.sourceId,
      orderNo: selectedOrder.value.orderNo
    })
    actionMessage.value = `${result.message}：#${result.request.id}`
    adjustmentAmount.value = ''
    requestStatus.value = 'pending'
    requestPage.value = 1
    await loadAdjustmentRequests()
    await refreshSelectedOrder()
  } catch (err: any) {
    actionError.value = err?.message || '调账申请提交失败'
  } finally {
    actionLoading.value = false
  }
}

async function saveOperationCase() {
  if (!selectedOrder.value) return
  const reason = operationReason.value.trim()
  const result = operationResult.value.trim()
  if (!reason) {
    actionError.value = '请填写运营处理原因'
    return
  }
  const refundAmount = parseOptionalPositiveAmount(operationRefundAmount.value)
  if (operationCreateRefundRequest.value && refundAmount === undefined) {
    actionError.value = '勾选退款登记时必须填写退款金额'
    return
  }
  if (refundAmount === null) {
    actionError.value = '退款金额必须是大于 0 的金额，最多两位小数'
    return
  }
  if (operationCreateRefundRequest.value && hasPendingRefundRequest.value) {
    actionError.value = '该订单已有待审核退款审批，不能重复登记'
    return
  }

  actionLoading.value = true
  actionMessage.value = ''
  actionError.value = ''
  try {
    const response = await api.orders.recordOperation(selectedOrder.value.sourceType, selectedOrder.value.sourceId, {
      status: operationStatus.value,
      reason,
      result: result || undefined,
      refundAmount,
      createRefundRequest: operationCreateRefundRequest.value
    })
    actionMessage.value = response.message || '订单运营处理记录已保存'
    operationCreateRefundRequest.value = false
    requestStatus.value = 'pending'
    requestPage.value = 1
    await loadAdjustmentRequests()
    await refreshSelectedOrder()
  } catch (err: any) {
    actionError.value = err?.message || '订单运营处理保存失败'
  } finally {
    actionLoading.value = false
  }
}

async function approveAdjustmentRequest(request: BalanceAdjustmentRequest) {
  if (request.status !== 'pending') return
  reviewLoadingId.value = request.id
  requestError.value = ''
  try {
    await api.admin.approveBalanceAdjustmentRequest(request.id, `订单中心审批通过 #${request.id}`)
    await loadAdjustmentRequests()
    await loadOrders()
  } catch (err: any) {
    requestError.value = err?.message || '审批通过失败'
  } finally {
    reviewLoadingId.value = null
  }
}

async function rejectAdjustmentRequest(request: BalanceAdjustmentRequest) {
  if (request.status !== 'pending') return
  reviewLoadingId.value = request.id
  requestError.value = ''
  try {
    await api.admin.rejectBalanceAdjustmentRequest(request.id, `订单中心驳回 #${request.id}`)
    await loadAdjustmentRequests()
  } catch (err: any) {
    requestError.value = err?.message || '驳回失败'
  } finally {
    reviewLoadingId.value = null
  }
}

function applyFilters() {
  page.value = 1
  void loadOrders()
}

function clearFilters() {
  type.value = ''
  status.value = ''
  userId.value = ''
  keyword.value = ''
  createdFrom.value = ''
  createdTo.value = ''
  applyFilters()
}

function goPage(nextPage: number) {
  if (nextPage < 1 || nextPage > totalPages.value || nextPage === page.value) return
  page.value = nextPage
  void loadOrders()
}

function applyRequestFilters() {
  requestPage.value = 1
  void loadAdjustmentRequests()
}

function goRequestPage(nextPage: number) {
  if (nextPage < 1 || nextPage > requestTotalPages.value || nextPage === requestPage.value) return
  requestPage.value = nextPage
  void loadAdjustmentRequests()
}

onMounted(() => {
  if (typeof route.query.keyword === 'string') keyword.value = route.query.keyword
  void loadOrders()
  void loadAdjustmentRequests()
})
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- 页面头部 -->
    <PageHeader
      data-reveal
      title="订单中心"
      description="统一查看充值订单、实例新购、续费、升级和退款账单。"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          class="shrink-0"
          @click="loadOrders"
        >
          <template #icon>
            <RefreshCw class="h-4 w-4" />
          </template>
          刷新
        </Button>
      </template>
    </PageHeader>

    <!-- 筛选卡片 -->
    <Card data-reveal padding="sm">
      <div class="flex flex-wrap items-center gap-3">
        <div class="w-full sm:w-44">
          <Select
            v-model="type"
            size="sm"
            :options="typeOptions"
            @change="status = ''; applyFilters()"
          />
        </div>
        <div class="w-full sm:w-44">
          <Select
            v-model="status"
            size="sm"
            :options="statusOptions"
            @change="applyFilters"
          />
        </div>
        <div class="w-full sm:w-36">
          <Input
            v-model="userId"
            size="sm"
            placeholder="用户 ID"
            clearable
            @enter="applyFilters"
          />
        </div>
        <div class="relative w-full sm:min-w-[220px] sm:flex-1">
          <Input
            v-model="keyword"
            size="sm"
            placeholder="订单号、交易号、用户名、实例名"
            clearable
            @enter="applyFilters"
          >
            <template #prefix>
              <Search class="h-4 w-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
        <div class="w-full sm:w-40">
          <Input
            v-model="createdFrom"
            type="date"
            size="sm"
            title="开始日期"
            @change="applyFilters"
          />
        </div>
        <div class="w-full sm:w-40">
          <Input
            v-model="createdTo"
            type="date"
            size="sm"
            title="结束日期"
            @change="applyFilters"
          />
        </div>
        <Button
          variant="secondary"
          size="sm"
          @click="clearFilters"
        >
          <RotateCcw class="h-4 w-4" />
          重置
        </Button>
      </div>
    </Card>

    <!-- 错误提示 -->
    <div
      v-if="error"
      class="flex items-center gap-2 rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-4 text-sm text-[var(--error)] shadow-sm"
    >
      <AlertCircle class="h-4 w-4 shrink-0" />
      <span>{{ error }}</span>
    </div>

    <!-- 订单列表卡片 -->
    <Card data-reveal padding="none" class="overflow-hidden">
      <div v-if="loading" class="flex items-center justify-center gap-2 py-16 text-sm text-themed-muted">
        <Loader2 class="h-5 w-5 animate-spin text-[var(--accent)]" />
        <span>正在加载订单...</span>
      </div>
      <EmptyState
        v-else-if="orders.length === 0"
        title="暂无订单记录"
        description="当前筛选条件下没有找到订单记录"
      />
      <template v-else>
        <!-- 移动端卡片列表 (保持 space-y-3 p-4 lg:hidden 满足守卫测试) -->
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="order in orders"
            :key="order.id"
            class="rounded-2xl border border-themed bg-themed-surface p-4 shadow-sm transition-all duration-200 hover:border-[var(--border-hover)]"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-semibold text-themed truncate" :title="order.title">{{ order.title }}</div>
                <div class="mt-1 break-all font-mono text-xs text-themed-muted">{{ order.orderNo }}</div>
              </div>
              <Badge :variant="statusBadgeVariant(order.status)" class="shrink-0">
                {{ statusLabel(order) }}
              </Badge>
            </div>

            <div class="mt-3 grid gap-2 text-xs text-themed-muted sm:grid-cols-2">
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">用户</div>
                <div class="mt-1 text-themed font-medium">{{ order.user?.username || `#${order.userId}` }}</div>
                <div class="mt-0.5 break-all text-themed-muted">{{ order.user?.email || '-' }}</div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">金额</div>
                <div class="mt-1 font-mono font-semibold tabular-nums text-themed">{{ formatMoney(order.amount) }}</div>
                <div class="mt-0.5 text-themed-muted">{{ sourceLabel(order.sourceType) }}</div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">关联实例</div>
                <div class="mt-1 text-themed truncate" :title="instanceName(order)">{{ instanceName(order) }}</div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">时间</div>
                <div class="mt-1 font-mono tabular-nums text-themed">{{ formatTime(order.createdAt) }}</div>
              </div>
            </div>

            <div class="mt-3 flex items-center justify-between gap-2">
              <div v-if="order.operationCase">
                <Badge :variant="operationStatusBadgeVariant(order.operationCase.status)">
                  {{ operationStatusLabel(order.operationCase.status) }}
                </Badge>
              </div>
              <div v-else />
              <Button size="sm" variant="secondary" @click="openDetail(order)">
                详情
              </Button>
            </div>
          </div>
        </div>

        <!-- PC 端定宽表格 (保持 hidden overflow-hidden lg:block 满足守卫测试) -->
        <div class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed divide-y divide-themed">
            <thead class="bg-themed-tertiary">
              <tr>
                <th class="w-[18%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">订单</th>
                <th class="w-[12%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">用户</th>
                <th class="w-[10%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">类型</th>
                <th class="w-[10%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">金额</th>
                <th class="w-[12%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">状态</th>
                <th class="w-[12%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">关联实例</th>
                <th class="w-[16%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">时间</th>
                <th class="w-[10%] px-4 py-3 text-right text-xs font-semibold uppercase tracking-wide text-themed-muted">操作</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-themed">
              <tr v-for="order in orders" :key="order.id" class="text-sm text-themed transition-colors hover:bg-themed-hover [&>td]:break-words">
                <td class="px-4 py-3">
                  <div class="font-medium text-themed truncate" :title="order.title">{{ order.title }}</div>
                  <div class="font-mono text-xs text-themed-muted truncate" :title="order.orderNo">{{ order.orderNo }}</div>
                </td>
                <td class="px-4 py-3">
                  <div class="text-themed font-medium truncate">{{ order.user?.username || `#${order.userId}` }}</div>
                  <div class="text-xs text-themed-muted truncate" :title="order.user?.email || ''">{{ order.user?.email || '-' }}</div>
                </td>
                <td class="px-4 py-3 text-themed-muted">{{ sourceLabel(order.sourceType) }}</td>
                <td class="px-4 py-3 font-mono font-medium tabular-nums text-themed whitespace-nowrap">{{ formatMoney(order.amount) }}</td>
                <td class="px-4 py-3">
                  <div class="flex flex-wrap items-center gap-1.5">
                    <Badge :variant="statusBadgeVariant(order.status)">{{ statusLabel(order) }}</Badge>
                    <Badge
                      v-if="order.operationCase"
                      :variant="operationStatusBadgeVariant(order.operationCase.status)"
                    >
                      {{ operationStatusLabel(order.operationCase.status) }}
                    </Badge>
                  </div>
                </td>
                <td class="px-4 py-3 text-themed-muted truncate" :title="instanceName(order)">{{ instanceName(order) }}</td>
                <td class="px-4 py-3 font-mono tabular-nums text-themed-muted whitespace-nowrap">{{ formatTime(order.createdAt) }}</td>
                <td class="px-4 py-3 text-right">
                  <div class="flex items-center justify-end gap-1">
                    <IconButton :label="$t('common.detail')" @click="openDetail(order)"><Info class="h-4 w-4" /></IconButton>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>

      <!-- 分页栏 -->
      <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3 border-t border-themed px-4 py-3 text-sm text-themed-muted">
        <span class="text-xs">共 <span class="font-mono tabular-nums text-themed font-medium">{{ total }}</span> 条记录</span>
        <div class="hidden sm:block">
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            :show-total="false"
            @update:page="goPage"
          />
        </div>
        <div class="flex items-center justify-between gap-2 sm:hidden">
          <Button size="sm" variant="secondary" :disabled="page <= 1" @click="goPage(page - 1)">上一页</Button>
          <span class="font-mono text-xs tabular-nums text-themed">{{ page }} / {{ totalPages }}</span>
          <Button size="sm" variant="secondary" :disabled="page >= totalPages" @click="goPage(page + 1)">下一页</Button>
        </div>
      </div>
    </Card>

    <!-- 调账审批区块 -->
    <Card data-reveal padding="none" class="overflow-hidden">
      <div class="flex flex-col gap-3 border-b border-themed p-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h2 class="text-base font-semibold text-themed">调账审批</h2>
          <p class="mt-0.5 text-xs text-themed-muted">订单退款、补款和扣款先进入审批任务，通过后才会写入余额日志。</p>
        </div>
        <div class="flex items-center gap-2">
          <div class="w-32">
            <Select
              v-model="requestStatus"
              size="sm"
              :options="requestStatusOptions"
              @change="applyRequestFilters"
            />
          </div>
          <Button
            variant="secondary"
            size="sm"
            class="shrink-0"
            :loading="requestLoading"
            @click="loadAdjustmentRequests"
          >
            <template #icon>
              <RefreshCw class="h-4 w-4" />
            </template>
            刷新
          </Button>
        </div>
      </div>

      <div
        v-if="requestError"
        class="m-4 flex items-center gap-2 rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-4 text-sm text-[var(--error)] shadow-sm"
      >
        <AlertCircle class="h-4 w-4 shrink-0" />
        <span>{{ requestError }}</span>
      </div>

      <div v-if="requestLoading" class="flex items-center justify-center gap-2 py-12 text-sm text-themed-muted">
        <Loader2 class="h-5 w-5 animate-spin text-[var(--accent)]" />
        <span>正在加载审批任务...</span>
      </div>
      <EmptyState
        v-else-if="adjustmentRequests.length === 0"
        title="暂无调账审批任务"
        description="当前没有需要审核的余额调账申请"
      />
      <template v-else>
        <!-- 移动端审批卡片列表 (保持 space-y-3 p-4 lg:hidden 满足守卫测试) -->
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="request in adjustmentRequests"
            :key="request.id"
            class="rounded-2xl border border-themed bg-themed-surface p-4 shadow-sm"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-semibold text-themed">
                  <span class="font-mono tabular-nums">#{{ request.id }}</span> {{ adjustmentTypeLabel(request.requestType) }}
                </div>
                <div class="mt-1 break-words text-xs text-themed-muted">{{ request.reason }}</div>
              </div>
              <Badge :variant="adjustmentStatusBadgeVariant(request.status)" class="shrink-0">
                {{ adjustmentStatusLabel(request.status) }}
              </Badge>
            </div>

            <div class="mt-3 grid gap-2 text-xs text-themed-muted sm:grid-cols-2">
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">用户</div>
                <div class="mt-1 text-themed font-medium">{{ request.user.username }}</div>
                <div class="mt-0.5 text-themed-muted">申请人 {{ request.requestedBy.username }}</div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">金额</div>
                <div
                  class="mt-1 font-mono font-semibold tabular-nums"
                  :class="request.amount >= 0 ? 'text-[var(--success)]' : 'text-[var(--error)]'"
                >
                  {{ request.amount >= 0 ? '+' : '' }}{{ formatMoney(request.amount) }}
                </div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">来源</div>
                <div class="mt-1 font-mono text-themed">{{ request.orderNo || '-' }}</div>
                <div class="mt-0.5 text-themed-muted">{{ request.sourceType || '-' }}</div>
              </div>
              <div class="rounded-xl bg-themed-secondary p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-themed-faint">时间</div>
                <div class="mt-1 font-mono tabular-nums text-themed">{{ formatTime(request.createdAt) }}</div>
              </div>
            </div>

            <div class="mt-3 flex flex-wrap justify-end gap-2">
              <template v-if="request.status === 'pending'">
                <Button
                  size="sm"
                  variant="danger"
                  :disabled="reviewLoadingId === request.id"
                  @click="rejectAdjustmentRequest(request)"
                >
                  驳回
                </Button>
                <Button
                  size="sm"
                  variant="primary"
                  :disabled="reviewLoadingId === request.id"
                  @click="approveAdjustmentRequest(request)"
                >
                  通过并执行
                </Button>
              </template>
              <span v-else class="text-xs text-themed-muted">审核人：{{ request.reviewedBy?.username || '-' }}</span>
            </div>
          </div>
        </div>

        <!-- PC 端定宽审批表格 (保持 hidden overflow-hidden lg:block 满足守卫测试) -->
        <div class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed divide-y divide-themed">
            <thead class="bg-themed-tertiary">
              <tr>
                <th class="w-[20%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">申请</th>
                <th class="w-[14%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">用户</th>
                <th class="w-[12%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">金额</th>
                <th class="w-[16%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">来源</th>
                <th class="w-[12%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">状态</th>
                <th class="w-[16%] px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">时间</th>
                <th class="w-[10%] px-4 py-3 text-right text-xs font-semibold uppercase tracking-wide text-themed-muted">操作</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-themed">
              <tr v-for="request in adjustmentRequests" :key="request.id" class="text-sm text-themed transition-colors hover:bg-themed-hover [&>td]:break-words">
                <td class="px-4 py-3">
                  <div class="font-medium text-themed"><span class="font-mono tabular-nums">#{{ request.id }}</span> {{ adjustmentTypeLabel(request.requestType) }}</div>
                  <div class="max-w-[260px] truncate text-xs text-themed-muted">{{ request.reason }}</div>
                </td>
                <td class="px-4 py-3">
                  <div class="text-themed font-medium">{{ request.user.username }}</div>
                  <div class="text-xs text-themed-muted">申请人 {{ request.requestedBy.username }}</div>
                </td>
                <td
                  class="px-4 py-3 font-mono font-medium tabular-nums"
                  :class="request.amount >= 0 ? 'text-[var(--success)]' : 'text-[var(--error)]'"
                >
                  {{ request.amount >= 0 ? '+' : '' }}{{ formatMoney(request.amount) }}
                </td>
                <td class="px-4 py-3 text-themed-muted">
                  <div class="font-mono text-themed">{{ request.orderNo || '-' }}</div>
                  <div class="text-xs">{{ request.sourceType || '-' }}</div>
                </td>
                <td class="px-4 py-3">
                  <Badge :variant="adjustmentStatusBadgeVariant(request.status)">
                    {{ adjustmentStatusLabel(request.status) }}
                  </Badge>
                </td>
                <td class="px-4 py-3 font-mono tabular-nums text-themed-muted whitespace-nowrap">{{ formatTime(request.createdAt) }}</td>
                <td class="px-4 py-3 text-right">
                  <div v-if="request.status === 'pending'" class="flex items-center justify-end gap-1">
                    <IconButton :label="$t('admin.orders.rejectAdjustment')" variant="danger" :disabled="reviewLoadingId === request.id" @click="rejectAdjustmentRequest(request)"><X class="h-4 w-4" /></IconButton>
                    <IconButton :label="$t('admin.orders.approveAdjustment')" :disabled="reviewLoadingId === request.id" @click="approveAdjustmentRequest(request)"><Check class="h-4 w-4" /></IconButton>
                  </div>
                  <span v-else class="text-xs text-themed-muted">{{ request.reviewedBy?.username || '-' }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>

      <!-- 审批分页栏 -->
      <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3 border-t border-themed px-4 py-3 text-sm text-themed-muted">
        <span class="text-xs">共 <span class="font-mono tabular-nums text-themed font-medium">{{ requestTotal }}</span> 条审批</span>
        <div class="hidden sm:block">
          <Pagination
            :page="requestPage"
            :page-size="requestPageSize"
            :total="requestTotal"
            :show-total="false"
            @update:page="goRequestPage"
          />
        </div>
        <div class="flex items-center justify-between gap-2 sm:hidden">
          <Button size="sm" variant="secondary" :disabled="requestPage <= 1" @click="goRequestPage(requestPage - 1)">上一页</Button>
          <span class="font-mono text-xs tabular-nums text-themed">{{ requestPage }} / {{ requestTotalPages }}</span>
          <Button size="sm" variant="secondary" :disabled="requestPage >= requestTotalPages" @click="goRequestPage(requestPage + 1)">下一页</Button>
        </div>
      </div>
    </Card>

    <!-- 订单详情与运营操作抽屉 -->
    <DrawerModal
      v-if="selectedOrder"
      :show="true"
      max-width="max-w-3xl"
      raw
      @close="selectedOrder = null"
    >
      <div class="modal-header flex items-center justify-between border-b border-themed px-6 py-4">
        <div class="min-w-0">
          <h3 class="modal-title text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">{{ selectedOrder.title }}</h3>
          <p class="mt-0.5 break-all font-mono text-xs text-[var(--text-secondary)]">{{ selectedOrder.orderNo }}</p>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
          @click="selectedOrder = null"
        >
          <X class="h-4 w-4" />
        </button>
      </div>

      <div class="modal-body flex-1 overflow-y-auto p-5 sm:p-6 space-y-6">
        <div v-if="detailLoading" class="flex items-center justify-center gap-2 py-12 text-sm text-[var(--text-secondary)]">
          <Loader2 class="h-5 w-5 animate-spin text-[var(--accent)]" />
          <span>正在加载详情...</span>
        </div>
        <dl v-else class="grid grid-cols-1 gap-2.5 text-sm sm:grid-cols-2">
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">用户</dt>
            <dd class="text-right text-themed font-medium">{{ selectedOrder.user?.username || `#${selectedOrder.userId}` }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">状态</dt>
            <dd class="text-right">
              <Badge :variant="statusBadgeVariant(selectedOrder.status)">{{ statusLabel(selectedOrder) }}</Badge>
            </dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">金额</dt>
            <dd class="text-right font-mono font-semibold tabular-nums text-themed">{{ formatMoney(selectedOrder.amount) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">实际到账</dt>
            <dd class="text-right font-mono font-medium tabular-nums text-themed">{{ selectedOrder.actualAmount === null ? '-' : formatMoney(selectedOrder.actualAmount) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">手续费</dt>
            <dd class="text-right font-mono tabular-nums text-themed">{{ formatMoney(selectedOrder.fee) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">支付渠道</dt>
            <dd class="text-right text-themed">{{ selectedOrder.provider?.name || selectedOrder.paymentMethod || '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">交易号</dt>
            <dd class="truncate pl-2 text-right font-mono text-themed" :title="selectedOrder.tradeNo || ''">{{ selectedOrder.tradeNo || '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">关联实例</dt>
            <dd class="truncate pl-2 text-right text-themed">{{ instanceName(selectedOrder) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">账期</dt>
            <dd class="text-right text-themed">{{ selectedOrder.months ? `${selectedOrder.months} 个月` : '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">创建时间</dt>
            <dd class="text-right font-mono tabular-nums text-themed">{{ formatTime(selectedOrder.createdAt) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">开始时间</dt>
            <dd class="text-right font-mono tabular-nums text-themed">{{ formatTime(selectedOrder.periodStart) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">结束时间</dt>
            <dd class="text-right font-mono tabular-nums text-themed">{{ formatTime(selectedOrder.periodEnd) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">完成时间</dt>
            <dd class="text-right font-mono tabular-nums text-themed">{{ formatTime(selectedOrder.completedAt) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-xl bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">失败原因</dt>
            <dd class="truncate pl-2 text-right text-themed" :title="selectedOrder.failReason || ''">{{ selectedOrder.failReason || '-' }}</dd>
          </div>
          <div v-if="selectedOrder.providerStatusSummary" class="rounded-xl bg-themed-secondary p-3 sm:col-span-2">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">Provider 摘要</dt>
            <dd class="mt-1.5 space-y-1 text-xs text-themed">
              <div>原始状态：{{ selectedOrder.providerStatusSummary.rawStatus }}</div>
              <div>交易号：<span class="font-mono">{{ selectedOrder.providerStatusSummary.tradeNo || '-' }}</span></div>
              <div>回调时间：<span class="font-mono tabular-nums">{{ formatTime(selectedOrder.providerStatusSummary.callbackAt) }}</span></div>
            </dd>
          </div>
          <div v-if="selectedOrder.remark" class="rounded-xl bg-themed-secondary p-3 sm:col-span-2">
            <dt class="text-xs uppercase tracking-wider text-themed-faint font-medium">备注</dt>
            <dd class="mt-1 break-words text-sm text-themed">{{ selectedOrder.remark }}</dd>
          </div>
        </dl>

        <!-- 操作反馈消息 -->
        <div
          v-if="actionMessage"
          class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] p-3.5 text-sm text-[var(--success)] shadow-sm"
        >
          <Check class="h-4 w-4 shrink-0" />
          <span>{{ actionMessage }}</span>
        </div>
        <div
          v-if="actionError"
          class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-3.5 text-sm text-[var(--error)] shadow-sm"
        >
          <AlertCircle class="h-4 w-4 shrink-0" />
          <span>{{ actionError }}</span>
        </div>

        <!-- 运营处理区块 -->
        <section class="border-t border-themed pt-5">
          <div class="flex items-center justify-between gap-3">
            <div>
              <h3 class="text-base font-semibold text-themed">订单运营处理</h3>
              <p class="mt-0.5 text-xs text-themed-muted">登记争议状态和人工退款申请。退款登记只创建调账审批，不会直接修改余额。</p>
            </div>
            <Badge :variant="operationStatusBadgeVariant(selectedOrder.operationCase?.status)" class="shrink-0">
              {{ operationStatusLabel(selectedOrder.operationCase?.status) }}
            </Badge>
          </div>

          <div v-if="selectedOrder.operationCase" class="mt-4 rounded-xl border border-themed bg-themed-secondary p-3.5 text-xs">
            <div class="grid gap-1.5 text-themed-muted">
              <div>最近处理：{{ selectedOrder.operationCase.updatedBy?.username || selectedOrder.operationCase.createdBy?.username || '-' }} · <span class="font-mono tabular-nums">{{ formatTime(selectedOrder.operationCase.updatedAt) }}</span></div>
              <div>处理原因：<span class="text-themed">{{ selectedOrder.operationCase.reason }}</span></div>
              <div v-if="selectedOrder.operationCase.result">处理结果：<span class="text-themed">{{ selectedOrder.operationCase.result }}</span></div>
              <div v-if="selectedOrder.operationCase.balanceAdjustmentRequest">
                关联审批：
                <span class="text-themed"><span class="font-mono tabular-nums">#{{ selectedOrder.operationCase.balanceAdjustmentRequest.id }}</span> · {{ adjustmentTypeLabel(selectedOrder.operationCase.balanceAdjustmentRequest.requestType) }} · {{ adjustmentStatusLabel(selectedOrder.operationCase.balanceAdjustmentRequest.status) }}</span>
              </div>
            </div>
          </div>

          <div class="mt-4 grid gap-3.5">
            <div class="flex flex-col gap-1.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">争议状态</label>
              <Select
                v-model="operationStatus"
                size="sm"
                :options="operationStatusOptions"
                :disabled="actionLoading"
              />
            </div>

            <div class="flex flex-col gap-1.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">处理原因</label>
              <textarea
                v-model="operationReason"
                rows="3"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200 disabled:opacity-50 disabled:cursor-not-allowed"
                placeholder="写明订单异常、核查依据或退款原因"
                :disabled="actionLoading"
              />
            </div>

            <div class="flex flex-col gap-1.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">处理结果</label>
              <textarea
                v-model="operationResult"
                rows="2"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200 disabled:opacity-50 disabled:cursor-not-allowed"
                placeholder="可选，例如已联系用户、等待 provider 对账、已补偿"
                :disabled="actionLoading"
              />
            </div>

            <label class="flex items-center gap-2.5 text-sm text-themed cursor-pointer select-none">
              <input
                v-model="operationCreateRefundRequest"
                type="checkbox"
                class="h-4 w-4 rounded border-[var(--border-color)] bg-[var(--bg-surface-soft)] accent-[var(--accent)] cursor-pointer"
                :disabled="actionLoading || hasPendingRefundRequest"
              />
              <span class="font-medium">同时登记退款审批</span>
            </label>

            <div class="flex flex-col gap-1.5">
              <Input
                v-model="operationRefundAmount"
                size="sm"
                label="退款金额"
                placeholder="勾选退款登记时必填，例如 10.00"
                :disabled="actionLoading || !operationCreateRefundRequest"
              />
            </div>

            <p v-if="hasPendingRefundRequest" class="text-xs text-[var(--warning)]">
              该订单已有待审核退款审批，需处理完成后才能再次登记。
            </p>

            <div>
              <Button
                variant="primary"
                size="sm"
                :loading="actionLoading"
                @click="saveOperationCase"
              >
                保存运营处理
              </Button>
            </div>
          </div>
        </section>

        <!-- 充值订单处理区块 -->
        <section v-if="selectedOrder.sourceType === 'recharge'" class="border-t border-themed pt-5">
          <h3 class="text-base font-semibold text-themed">充值订单处理</h3>
          <p class="mt-0.5 text-xs text-themed-muted">仅待支付或处理中订单允许人工完成或标记失败，入账仍走原有充值完成逻辑。</p>

          <div class="mt-4 grid gap-3.5">
            <Input
              v-model="completeTradeNo"
              size="sm"
              label="交易号"
              placeholder="可选，第三方交易号或线下凭证号"
              :disabled="!canCompleteRecharge || actionLoading"
            />

            <Input
              v-model="completeActualAmount"
              size="sm"
              label="实际入账金额"
              placeholder="留空则按订单金额入账"
              :disabled="!canCompleteRecharge || actionLoading"
            />

            <div>
              <Button
                variant="primary"
                size="sm"
                :disabled="!canCompleteRecharge || actionLoading"
                :loading="actionLoading"
                @click="completeRecharge"
              >
                手动完成并入账
              </Button>
            </div>

            <div class="flex flex-col gap-1.5 pt-2">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">失败原因</label>
              <textarea
                v-model="failReason"
                rows="3"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200 disabled:opacity-50 disabled:cursor-not-allowed"
                placeholder="标记失败时必须填写原因"
                :disabled="!canFailRecharge || actionLoading"
              />
            </div>

            <div>
              <Button
                variant="danger"
                size="sm"
                :disabled="!canFailRecharge || actionLoading"
                :loading="actionLoading"
                @click="failRecharge"
              >
                标记失败
              </Button>
            </div>
          </div>
        </section>

        <!-- 人工调账 / 退款审批区块 -->
        <section class="border-t border-themed pt-5">
          <h3 class="text-base font-semibold text-themed">人工调账 / 退款审批</h3>
          <p class="mt-0.5 text-xs text-themed-muted">用于补款、退款或扣款。正数增加用户余额，负数扣减用户余额；提交后进入审批任务，通过后才会写入余额日志。</p>

          <div class="mt-4 grid gap-3.5">
            <Input
              v-model="adjustmentAmount"
              size="sm"
              label="调账金额"
              placeholder="例如 10.00 或 -5.00"
              :disabled="actionLoading"
            />

            <div class="flex flex-col gap-1.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">调账原因</label>
              <textarea
                v-model="adjustmentRemark"
                rows="3"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200 disabled:opacity-50 disabled:cursor-not-allowed"
                placeholder="必须写明订单、原因和处理结论"
                :disabled="actionLoading"
              />
            </div>

            <div>
              <Button
                variant="primary"
                size="sm"
                :loading="actionLoading"
                @click="adjustBalance"
              >
                提交调账审批
              </Button>
            </div>
          </div>
        </section>
      </div>

      <div class="modal-footer flex items-center justify-end border-t border-themed px-6 py-4">
        <Button variant="secondary" size="sm" @click="selectedOrder = null">
          关闭
        </Button>
      </div>
    </DrawerModal>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  *,
  ::before,
  ::after {
    animation-duration: 0.001ms !important;
    transition-duration: 0.001ms !important;
  }
}
</style>
