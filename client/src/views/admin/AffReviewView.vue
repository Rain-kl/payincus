<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Check,
  X,
  RefreshCw,
  Inbox,
  Loader2,
  ChevronLeft,
  ChevronRight
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Select
} from '@/components/ui'

const { t } = useI18n()
const toast = useToast()

const props = withDefaults(defineProps<{
  embedded?: boolean
}>(), {
  embedded: false
})

// 转化申请列表
interface AffWithdrawal {
  id: number
  userId: number
  username: string
  userAffBalance: number
  amount: number
  status: string
  rejectReason: string | null
  reviewedBy: number | null
  createdAt: string
  reviewedAt: string | null
}

const withdrawals = ref<AffWithdrawal[]>([])
const loading = ref(true)
const page = ref(1)
const pageSize = ref(100)
const total = ref(0)
const statusFilter = ref('')

// 操作状态
const approveLoading = ref<number | null>(null)
const showRejectModal = ref(false)
const rejectTarget = ref<AffWithdrawal | null>(null)
const rejectReason = ref('')
const rejectLoading = ref(false)

const statusOptions = computed(() => [
  { value: '', label: t('aff.all') },
  { value: 'pending', label: t('aff.withdrawalStatus.pending') },
  { value: 'approved', label: t('aff.withdrawalStatus.approved') },
  { value: 'rejected', label: t('aff.withdrawalStatus.rejected') }
])

onMounted(() => {
  loadWithdrawals()
})

async function loadWithdrawals() {
  loading.value = true
  try {
    const res = await api.aff.adminGetWithdrawals({
      page: page.value,
      pageSize: pageSize.value,
      status: statusFilter.value || undefined
    })
    withdrawals.value = (res as any).withdrawals || []
    total.value = (res as any).total || 0
  } catch (err: any) {
    toast.error(t('common.loadFailed') + ': ' + err.message)
  } finally {
    loading.value = false
  }
}

async function approveWithdrawal(w: AffWithdrawal) {
  approveLoading.value = w.id
  try {
    await api.aff.adminApprove(w.id)
    toast.success(t('aff.approveSuccess'))
    loadWithdrawals()
  } catch (err: any) {
    toast.error(t('aff.approveFailed') + ': ' + err.message)
  } finally {
    approveLoading.value = null
  }
}

function openRejectModal(w: AffWithdrawal) {
  rejectTarget.value = w
  rejectReason.value = ''
  showRejectModal.value = true
}

async function submitReject() {
  if (!rejectTarget.value || !rejectReason.value.trim()) {
    toast.error(t('aff.rejectReasonPlaceholder'))
    return
  }
  rejectLoading.value = true
  try {
    await api.aff.adminReject(rejectTarget.value.id, rejectReason.value.trim())
    toast.success(t('aff.rejectSuccess'))
    showRejectModal.value = false
    loadWithdrawals()
  } catch (err: any) {
    toast.error(t('aff.rejectFailed') + ': ' + err.message)
  } finally {
    rejectLoading.value = false
  }
}

function formatDate(dateStr: string | null): string {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleString()
}

function formatMoney(amount: string | number): string {
  const num = typeof amount === 'string' ? parseFloat(amount) : amount
  return `¥${num.toFixed(2)}`
}

function getStatusBadgeVariant(status: string): 'warning' | 'success' | 'error' | 'default' {
  const map: Record<string, 'warning' | 'success' | 'error' | 'default'> = {
    pending: 'warning',
    approved: 'success',
    rejected: 'error'
  }
  return map[status] || 'default'
}


function getStatusName(status: string): string {
  const map: Record<string, string> = {
    pending: t('aff.withdrawalStatus.pending'),
    approved: t('aff.withdrawalStatus.approved'),
    rejected: t('aff.withdrawalStatus.rejected')
  }
  return map[status] || status
}

const totalPages = computed(() => Math.ceil(total.value / pageSize.value))
</script>

<template>
  <div :class="['animate-fade-in', props.embedded ? 'space-y-4' : 'kawaii-page space-y-6']">
    <!-- 页面头部 -->
    <PageHeader
      v-if="!props.embedded"
      :title="$t('aff.adminTitle')"
      :description="$t('aff.adminDescription')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadWithdrawals"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5 stroke-[2]" :class="loading ? 'animate-spin' : ''" />
          </template>
          {{ $t('common.refresh') }}
        </Button>
      </template>
    </PageHeader>
    <div v-else class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 pb-1">
      <div>
        <h2 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('aff.adminTitle') }}</h2>
        <p class="text-sm text-[var(--text-secondary)] mt-0.5">{{ $t('aff.adminDescription') }}</p>
      </div>
      <Button
        variant="secondary"
        size="sm"
        :loading="loading"
        class="self-start sm:self-auto"
        @click="loadWithdrawals"
      >
        <template #icon>
          <RefreshCw class="w-3.5 h-3.5 stroke-[2]" :class="loading ? 'animate-spin' : ''" />
        </template>
        {{ $t('common.refresh') }}
      </Button>
    </div>

    <!-- 筛选工具栏 -->
    <Card padding="sm">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="flex items-center gap-2">
          <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none shrink-0">
            {{ $t('aff.filterStatus') }}:
          </label>
          <div class="w-36">
            <Select
              v-model="statusFilter"
              size="sm"
              :options="statusOptions"
              @change="page = 1; loadWithdrawals()"
            />
          </div>
        </div>
        <Button
          v-if="props.embedded"
          variant="secondary"
          size="sm"
          :loading="loading"
          class="shrink-0"
          @click="loadWithdrawals"
        >
          <template #icon>
            <RefreshCw class="w-3.5 h-3.5 stroke-[2]" :class="loading ? 'animate-spin' : ''" />
          </template>
          {{ $t('common.refresh') }}
        </Button>
      </div>
    </Card>

    <!-- 列表卡片 -->
    <Card padding="none" class="overflow-hidden">
      <!-- 加载中 -->
      <div v-if="loading" class="flex flex-col items-center justify-center gap-3 py-16 text-center text-[var(--text-secondary)]">
        <Loader2 class="w-7 h-7 animate-spin text-[var(--accent)]" />
        <p class="text-sm">{{ $t('common.loading') }}...</p>
      </div>

      <!-- 空状态 -->
      <EmptyState
        v-else-if="withdrawals.length === 0"
        :title="$t('aff.noRequests')"
      >
        <template #icon>
          <Inbox class="w-7 h-7 stroke-[1.5]" />
        </template>
      </EmptyState>

      <!-- 数据展示 -->
      <template v-else>
        <!-- 移动端卡片列表 -->
        <div class="space-y-3 p-4 lg:hidden">
          <div
            v-for="w in withdrawals"
            :key="w.id"
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm transition-all duration-200 hover:border-[var(--border-hover)]"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-mono text-xs text-[var(--text-tertiary)] tabular-nums">#{{ w.id }}</div>
                <div class="mt-1 truncate text-sm font-semibold text-[var(--text-primary)]">{{ w.username }}</div>
                <div class="mt-0.5 text-xs text-[var(--text-secondary)] tabular-nums">ID: {{ w.userId }}</div>
              </div>
              <Badge :variant="getStatusBadgeVariant(w.status)" :dot="w.status === 'pending'" class="shrink-0">
                {{ getStatusName(w.status) }}
              </Badge>
            </div>

            <div class="mt-3 grid grid-cols-2 gap-2 text-xs">
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)]">{{ $t('aff.requestAmount') }}</div>
                <div class="mt-1 font-mono font-semibold text-[var(--text-primary)] tabular-nums">{{ formatMoney(w.amount) }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)]">{{ $t('aff.userBalance') }}</div>
                <div class="mt-1 font-mono font-medium text-[var(--text-secondary)] tabular-nums">{{ formatMoney(w.userAffBalance) }}</div>
              </div>
              <div class="col-span-2 rounded-xl bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--text-tertiary)]">{{ $t('aff.requestTime') }}</div>
                <div class="mt-1 font-mono font-medium text-[var(--text-secondary)] tabular-nums">{{ formatDate(w.createdAt) }}</div>
              </div>
              <div v-if="w.rejectReason" class="col-span-2 rounded-xl bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_20%,var(--bg-surface))] p-2.5">
                <div class="text-[11px] font-medium uppercase tracking-wider text-[var(--error)]">{{ $t('aff.rejectReason') }}</div>
                <div class="mt-1 break-words text-xs text-[var(--text-primary)]">{{ w.rejectReason }}</div>
              </div>
            </div>

            <div v-if="w.status === 'pending'" class="mt-4 flex items-center gap-2 pt-2 border-t border-[var(--border-color)]">
              <Button
                size="sm"
                variant="primary"
                :loading="approveLoading === w.id"
                :disabled="approveLoading === w.id"
                class="flex-1"
                @click="approveWithdrawal(w)"
              >
                <template #icon>
                  <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                </template>
                {{ approveLoading === w.id ? $t('common.processing') : $t('aff.approve') }}
              </Button>
              <Button
                size="sm"
                variant="danger"
                class="flex-1"
                @click="openRejectModal(w)"
              >
                <template #icon>
                  <X class="w-3.5 h-3.5 stroke-[2.5]" />
                </template>
                {{ $t('aff.reject') }}
              </Button>
            </div>
          </div>
        </div>

        <!-- PC 端定宽表格 -->
        <div class="hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <thead>
              <tr class="border-b border-[var(--border-color)] text-left text-[11px] font-semibold uppercase tracking-wider text-[var(--text-secondary)] bg-[var(--bg-surface-soft)]">
                <th class="w-[7%] px-4 py-3.5">ID</th>
                <th class="w-[16%] px-4 py-3.5">{{ $t('aff.user') }}</th>
                <th class="w-[12%] px-4 py-3.5">{{ $t('aff.requestAmount') }}</th>
                <th class="w-[12%] px-4 py-3.5">{{ $t('aff.userBalance') }}</th>
                <th class="w-[10%] px-4 py-3.5">{{ $t('aff.status') }}</th>
                <th class="w-[15%] px-4 py-3.5">{{ $t('aff.requestTime') }}</th>
                <th class="w-[12%] px-4 py-3.5">{{ $t('aff.rejectReason') }}</th>
                <th class="w-[16%] px-4 py-3.5">{{ $t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)] bg-[var(--bg-surface)]">
              <tr
                v-for="w in withdrawals"
                :key="w.id"
                class="transition-colors hover:bg-[var(--bg-surface-soft)]"
              >
                <td class="px-4 py-3.5 text-xs text-[var(--text-tertiary)] font-mono tabular-nums">#{{ w.id }}</td>
                <td class="px-4 py-3.5">
                  <div class="truncate text-sm font-medium text-[var(--text-primary)]" :title="w.username">{{ w.username }}</div>
                  <div class="text-xs text-[var(--text-secondary)] tabular-nums">ID: {{ w.userId }}</div>
                </td>
                <td class="px-4 py-3.5 text-sm font-semibold font-mono text-[var(--text-primary)] tabular-nums">
                  {{ formatMoney(w.amount) }}
                </td>
                <td class="px-4 py-3.5 text-sm font-mono text-[var(--text-secondary)] tabular-nums">
                  {{ formatMoney(w.userAffBalance) }}
                </td>
                <td class="px-4 py-3.5">
                  <Badge :variant="getStatusBadgeVariant(w.status)" :dot="w.status === 'pending'">
                    {{ getStatusName(w.status) }}
                  </Badge>
                </td>
                <td class="truncate px-4 py-3.5 text-xs font-mono text-[var(--text-secondary)] tabular-nums" :title="formatDate(w.createdAt)">
                  {{ formatDate(w.createdAt) }}
                </td>
                <td class="truncate px-4 py-3.5 text-xs text-[var(--text-secondary)]" :title="w.rejectReason || '-'">
                  {{ w.rejectReason || '-' }}
                </td>
                <td class="px-4 py-3.5">
                  <template v-if="w.status === 'pending'">
                    <div class="flex items-center gap-1.5">
                      <Button
                        size="sm"
                        variant="primary"
                        :loading="approveLoading === w.id"
                        :disabled="approveLoading === w.id"
                        @click="approveWithdrawal(w)"
                      >
                        <template #icon>
                          <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                        </template>
                        {{ approveLoading === w.id ? $t('common.processing') : $t('aff.approve') }}
                      </Button>
                      <Button
                        size="sm"
                        variant="danger"
                        @click="openRejectModal(w)"
                      >
                        <template #icon>
                          <X class="w-3.5 h-3.5 stroke-[2.5]" />
                        </template>
                        {{ $t('aff.reject') }}
                      </Button>
                    </div>
                  </template>
                  <template v-else>
                    <span class="text-xs text-[var(--text-tertiary)] select-none">-</span>
                  </template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 分页 -->
        <div v-if="totalPages > 1" class="flex flex-wrap items-center justify-between gap-3 p-4 border-t border-[var(--border-color)] text-sm">
          <div class="text-xs text-[var(--text-secondary)] select-none">
            {{ $t('common.totalCount', { count: total }) }} · {{ page }} / {{ totalPages }} {{ $t('common.page') }}
          </div>
          <div class="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              :disabled="page <= 1"
              @click="page--; loadWithdrawals()"
            >
              <template #icon>
                <ChevronLeft class="w-4 h-4 stroke-[2]" />
              </template>
              {{ $t('common.prevPage') }}
            </Button>
            <span class="text-xs font-medium text-[var(--text-secondary)] tabular-nums px-2">
              {{ page }} / {{ totalPages }}
            </span>
            <Button
              variant="secondary"
              size="sm"
              :disabled="page >= totalPages"
              @click="page++; loadWithdrawals()"
            >
              {{ $t('common.nextPage') }}
              <template #suffix>
                <ChevronRight class="w-4 h-4 stroke-[2]" />
              </template>
            </Button>
          </div>
        </div>
      </template>
    </Card>

    <!-- 拒绝弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showRejectModal" class="modal-overlay" @click.self="showRejectModal = false">
          <div class="modal-backdrop" @click="showRejectModal = false"></div>
          <div class="modal-content max-w-md">
            <div class="modal-header">
              <h3 class="modal-title">{{ $t('aff.reject') }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showRejectModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <div class="modal-body space-y-4">
              <div v-if="rejectTarget" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-2">
                <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                  <span>{{ $t('aff.user') }}</span>
                  <span class="font-medium text-[var(--text-primary)]">{{ rejectTarget.username }} (ID: {{ rejectTarget.userId }})</span>
                </div>
                <div class="flex items-center justify-between text-xs text-[var(--text-secondary)]">
                  <span>{{ $t('aff.requestAmount') }}</span>
                  <span class="font-semibold font-mono text-[var(--text-primary)] tabular-nums">{{ formatMoney(rejectTarget.amount) }}</span>
                </div>
              </div>

              <div>
                <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-2 select-none">
                  {{ $t('aff.rejectReason') }} <span class="text-[var(--error)]">*</span>
                </label>
                <textarea
                  v-model="rejectReason"
                  rows="3"
                  class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:border-[var(--accent)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-none"
                  :placeholder="$t('aff.rejectReasonPlaceholder')"
                ></textarea>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showRejectModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button
                variant="danger"
                :disabled="rejectLoading || !rejectReason.trim()"
                :loading="rejectLoading"
                @click="submitReject"
              >
                {{ rejectLoading ? $t('common.processing') : $t('aff.reject') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  .animate-fade-in,
  .animate-fade-in *,
  .animate-fade-in *::before,
  .animate-fade-in *::after {
    animation-duration: 0.001ms !important;
    transition-duration: 0.001ms !important;
  }
}
</style>
