<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { AlertCircle, Info, Loader2, RefreshCw, X } from 'lucide-vue-next'
import api from '@/api'
import { useReveal } from '@/composables/useReveal'
import IconButton from '@/components/IconButton.vue'
import { Badge, Button, Card, EmptyState, PageHeader, Select } from '@/components/ui'

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

type OrderSourceType = 'recharge' | 'instance_billing'

interface OrderItem {
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
  provider: { id: number; name: string; type: string } | null
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
}

const orders = ref<OrderItem[]>([])
const selectedOrder = ref<OrderItem | null>(null)
const loading = ref(false)
const detailLoading = ref(false)
const error = ref('')
const page = ref(1)
const pageSize = ref(10)
const total = ref(0)
const type = ref('')
const status = ref('')

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

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

function formatMoney(value: number | null | undefined): string {
  return `¥${Number(value || 0).toFixed(2)}`
}

function formatTime(value: string | null | undefined): string {
  if (!value) return '-'
  return new Date(value).toLocaleString('zh-CN', { hour12: false })
}

function statusLabel(order: OrderItem): string {
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

function statusBadgeVariant(statusValue: string): 'success' | 'warning' | 'error' | 'default' {
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

function sourceLabel(sourceType: OrderSourceType): string {
  return sourceType === 'recharge' ? '充值订单' : '实例账单'
}

function instanceName(order: OrderItem): string {
  if (!order.instance) return '-'
  return order.instance.displayName || order.instance.name
}

async function loadOrders() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.orders.list({
      page: page.value,
      pageSize: pageSize.value,
      type: type.value || undefined,
      status: status.value || undefined
    })
    orders.value = res.orders as OrderItem[]
    total.value = res.total
  } catch (err: any) {
    error.value = err?.message || '订单加载失败'
  } finally {
    loading.value = false
  }
}

async function openDetail(order: OrderItem) {
  selectedOrder.value = order
  detailLoading.value = true
  try {
    const res = await api.orders.detail(order.sourceType, order.sourceId)
    selectedOrder.value = res.order as OrderItem
  } finally {
    detailLoading.value = false
  }
}

function applyFilters() {
  page.value = 1
  void loadOrders()
}

function onTypeChange(val: string) {
  type.value = val
  status.value = ''
  applyFilters()
}

function onStatusChange(val: string) {
  status.value = val
  applyFilters()
}

function clearFilters() {
  type.value = ''
  status.value = ''
  applyFilters()
}

function goPage(nextPage: number) {
  if (nextPage < 1 || nextPage > totalPages.value || nextPage === page.value) return
  page.value = nextPage
  void loadOrders()
}

onMounted(loadOrders)
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <PageHeader
      data-reveal
      title="订单中心"
      description="查看充值订单、实例新购、续费和退款账单。"
    >
      <template #actions>
        <Button
          variant="secondary"
          :loading="loading"
          class="w-full sm:w-auto"
          @click="loadOrders"
        >
          <template #icon>
            <RefreshCw class="h-4 w-4" />
          </template>
          刷新
        </Button>
      </template>
    </PageHeader>

    <Card data-reveal padding="sm">
      <div class="grid gap-3 md:grid-cols-[180px_180px_auto] items-center">
        <Select
          v-model="type"
          :options="typeOptions"
          @change="onTypeChange"
        />
        <Select
          v-model="status"
          :options="statusOptions"
          @change="onStatusChange"
        />
        <Button variant="secondary" class="justify-self-start" @click="clearFilters">
          重置
        </Button>
      </div>
    </Card>

    <div
      v-if="error"
      class="flex items-center gap-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] p-3 text-sm text-[var(--error)]"
    >
      <AlertCircle class="h-4 w-4 shrink-0" />
      <span>{{ error }}</span>
    </div>

    <section data-reveal class="overflow-hidden rounded-2xl border border-themed bg-themed-surface shadow-sm">
      <div v-if="loading" class="flex items-center justify-center gap-2 p-12 text-sm text-themed-muted">
        <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
        <span>正在加载订单...</span>
      </div>
      <EmptyState
        v-else-if="orders.length === 0"
        title="暂无订单记录"
        description="当前没有符合条件的订单记录"
      />
      <div v-else class="space-y-3 p-4 lg:hidden">
        <div
          v-for="order in orders"
          :key="order.id"
          class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <div class="truncate text-sm font-semibold text-themed" :title="order.title">{{ order.title }}</div>
              <div class="mt-1 truncate font-mono text-xs text-themed-muted" :title="order.orderNo">{{ order.orderNo }}</div>
            </div>
            <Badge :variant="statusBadgeVariant(order.status)">
              {{ statusLabel(order) }}
            </Badge>
          </div>

          <div class="mt-4 grid grid-cols-2 gap-2 text-sm">
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">类型</div>
              <div class="mt-1 truncate text-themed">{{ sourceLabel(order.sourceType) }}</div>
            </div>
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">金额</div>
              <div class="mt-1 font-mono font-semibold tabular-nums text-themed">{{ formatMoney(order.amount) }}</div>
            </div>
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">关联实例</div>
              <div class="mt-1 truncate text-themed" :title="instanceName(order)">{{ instanceName(order) }}</div>
            </div>
            <div class="rounded-lg bg-themed-secondary px-3 py-2">
              <div class="text-[11px] font-medium uppercase tracking-wide text-themed-muted">时间</div>
              <div class="mt-1 text-xs text-themed">{{ formatTime(order.createdAt) }}</div>
            </div>
          </div>

          <div class="mt-4 flex justify-end">
            <Button size="sm" variant="secondary" @click="openDetail(order)">详情</Button>
          </div>
        </div>
      </div>
      <div v-if="orders.length > 0" class="hidden overflow-hidden lg:block">
        <table class="w-full table-fixed divide-y divide-themed">
          <thead class="bg-themed-tertiary">
            <tr class="text-left text-xs font-semibold uppercase tracking-wide text-themed-muted">
              <th class="w-[25%] px-4 py-3">订单</th>
              <th class="w-[12%] px-4 py-3">类型</th>
              <th class="w-[11%] px-4 py-3">金额</th>
              <th class="w-[11%] px-4 py-3">状态</th>
              <th class="w-[17%] px-4 py-3">关联实例</th>
              <th class="w-[16%] px-4 py-3">时间</th>
              <th class="w-[8%] px-4 py-3 text-right">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-themed">
            <tr v-for="order in orders" :key="order.id" class="text-sm transition-colors hover:bg-themed-hover">
              <td class="px-4 py-3">
                <div class="truncate font-medium text-themed" :title="order.title">{{ order.title }}</div>
                <div class="truncate font-mono text-xs text-themed-muted" :title="order.orderNo">{{ order.orderNo }}</div>
              </td>
              <td class="px-4 py-3 text-themed-muted">
                <div class="truncate">{{ sourceLabel(order.sourceType) }}</div>
              </td>
              <td class="px-4 py-3 font-mono font-medium tabular-nums text-themed whitespace-nowrap">{{ formatMoney(order.amount) }}</td>
              <td class="px-4 py-3">
                <Badge :variant="statusBadgeVariant(order.status)">
                  {{ statusLabel(order) }}
                </Badge>
              </td>
              <td class="px-4 py-3 text-themed-muted">
                <div class="truncate" :title="instanceName(order)">{{ instanceName(order) }}</div>
              </td>
              <td class="px-4 py-3 text-themed-muted whitespace-nowrap">{{ formatTime(order.createdAt) }}</td>
              <td class="px-4 py-3 text-right whitespace-nowrap">
                <div class="flex items-center justify-end gap-1">
                  <IconButton :label="$t('common.detail')" @click="openDetail(order)"><Info /></IconButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="flex flex-col items-stretch justify-between gap-3 border-t border-themed px-4 py-3 text-sm text-themed-muted sm:flex-row sm:items-center">
        <span>共 <span class="font-mono tabular-nums text-themed">{{ total }}</span> 条记录</span>
        <div class="grid grid-cols-[1fr_auto_1fr] items-center gap-2 sm:flex">
          <Button size="sm" variant="secondary" class="justify-center" :disabled="page <= 1" @click="goPage(page - 1)">上一页</Button>
          <span class="min-w-[72px] text-center font-mono tabular-nums">{{ page }} / {{ totalPages }}</span>
          <Button size="sm" variant="secondary" class="justify-center" :disabled="page >= totalPages" @click="goPage(page + 1)">下一页</Button>
        </div>
      </div>
    </section>

    <DrawerModal
      v-if="selectedOrder"
      :show="true"
      max-width="max-w-xl"
      raw
      @close="selectedOrder = null"
    >
      <div class="modal-header">
        <div class="min-w-0">
          <h2 class="modal-title truncate">{{ selectedOrder.title }}</h2>
          <p class="mt-0.5 truncate font-mono text-xs text-themed-muted">{{ selectedOrder.orderNo }}</p>
        </div>
        <button class="btn btn-sm btn-ghost gap-1.5" @click="selectedOrder = null">
          <X class="h-4 w-4" />
          关闭
        </button>
      </div>

      <div class="modal-body space-y-4">
        <div v-if="detailLoading" class="flex items-center justify-center gap-2 p-6 text-sm text-themed-muted">
          <Loader2 class="h-4 w-4 animate-spin text-[var(--accent)]" />
          <span>正在加载详情...</span>
        </div>
        <dl v-else class="grid gap-2 text-sm">
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">状态</dt>
            <dd>
              <Badge :variant="statusBadgeVariant(selectedOrder.status)">
                {{ statusLabel(selectedOrder) }}
              </Badge>
            </dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">金额</dt>
            <dd class="font-mono font-medium tabular-nums text-themed">{{ formatMoney(selectedOrder.amount) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">实际到账</dt>
            <dd class="font-mono tabular-nums text-themed">{{ selectedOrder.actualAmount === null ? '-' : formatMoney(selectedOrder.actualAmount) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">手续费</dt>
            <dd class="font-mono tabular-nums text-themed">{{ formatMoney(selectedOrder.fee) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">支付渠道</dt>
            <dd class="text-themed">{{ selectedOrder.provider?.name || selectedOrder.paymentMethod || '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">交易号</dt>
            <dd class="break-all font-mono text-xs text-themed">{{ selectedOrder.tradeNo || '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">关联实例</dt>
            <dd class="text-themed">{{ instanceName(selectedOrder) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">账期</dt>
            <dd class="text-themed">{{ selectedOrder.months ? `${selectedOrder.months} 个月` : '-' }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">开始时间</dt>
            <dd class="text-xs text-themed">{{ formatTime(selectedOrder.periodStart) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">结束时间</dt>
            <dd class="text-xs text-themed">{{ formatTime(selectedOrder.periodEnd) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">创建时间</dt>
            <dd class="text-xs text-themed">{{ formatTime(selectedOrder.createdAt) }}</dd>
          </div>
          <div class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">完成时间</dt>
            <dd class="text-xs text-themed">{{ formatTime(selectedOrder.completedAt) }}</dd>
          </div>
          <div v-if="selectedOrder.failReason" class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">失败原因</dt>
            <dd class="text-xs text-themed">{{ selectedOrder.failReason || '-' }}</dd>
          </div>
          <div v-if="selectedOrder.remark" class="flex items-center justify-between rounded-lg bg-themed-secondary px-3.5 py-2.5">
            <dt class="text-xs uppercase tracking-wider text-themed-muted">备注</dt>
            <dd class="text-xs text-themed">{{ selectedOrder.remark || '-' }}</dd>
          </div>
        </dl>
      </div>

      <div class="modal-footer">
        <button class="btn btn-secondary" @click="selectedOrder = null">关闭</button>
      </div>
    </DrawerModal>
  </div>
</template>
