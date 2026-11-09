<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  ArrowRightLeft,
  Ban,
  Check,
  Info,
  RefreshCw,
  Search,
  Send,
  X
} from 'lucide-vue-next'
import api from '@/api'
import { useReveal } from '@/composables/useReveal'
import { Badge, Button, Card, EmptyState, Input, PageHeader, Pagination } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import UserAvatar from '@/components/UserAvatar.vue'
import FlagIcon from '@/components/FlagIcon.vue'
import { useToast } from '@/stores/toast'

const { t } = useI18n()
const toast = useToast()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// Tab state
type TabType = 'sent' | 'received'
const activeTab = ref<TabType>('sent')

// Data
interface TransferSnapshot {
  packageId?: number | null
  packageName?: string | null
  hostId?: number | null
  hostName?: string | null
  hostLocation?: string | null
  hostCountryCode?: string | null
  originalName?: string | null
  cpu?: number
  memory?: number
  disk?: number
  networkMode?: string | null
  portMappingsCount?: number
  snapshotsCount?: number
  backupsCount?: number
  ipv4?: string | null
  ipv6?: string | null
}

interface Transfer {
  id: number
  instanceId: number
  instanceName: string
  instanceStatus: string
  instanceImage: string
  fromUser: { id: number; username: string; email?: string | null; avatarStyle?: string; avatarBadgeId?: string | null } | null
  toUser: { id: number; username: string; email?: string | null; avatarStyle?: string; avatarBadgeId?: string | null } | null
  status: string
  snapshot: TransferSnapshot | null
  remark: string | null
  rejectReason: string | null
  createdAt: string
  acceptedAt: string | null
  rejectedAt: string | null
  cancelledAt: string | null
  canPush?: boolean
}

const transfers = ref<Transfer[]>([])
const loading = ref(true)      // 首次加载
const refreshing = ref(false)  // 后台刷新（不显示骨架屏）
const pendingCount = ref(0)

// Search
const searchQuery = ref('')

// Pagination
const page = ref(1)
const pageSize = ref(10)
const total = ref(0)
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

// Reject modal
const showRejectModal = ref(false)
const rejectingTransferId = ref<number | null>(null)
const rejectReason = ref('')
const rejectLoading = ref(false)

// Action loading
const actionLoading = ref<number | null>(null)
const pushLoading = ref<number | null>(null)

// 配置详情弹窗
const showConfigModal = ref(false)
const selectedTransfer = ref<Transfer | null>(null)

// 备注展开状态
const expandedRemarks = ref<Set<number>>(new Set())

onMounted(async () => {
  await Promise.all([loadTransfers(), loadPendingCount()])
})

async function loadTransfers(silent = false) {
  if (!silent) loading.value = true
  else refreshing.value = true
  try {
    const response = await api.transfers.list(activeTab.value, {
      page: page.value,
      pageSize: pageSize.value,
      search: searchQuery.value.trim() || undefined
    })
    transfers.value = response.transfers
    total.value = response.total
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

async function loadPendingCount() {
  try {
    const response = await api.transfers.getPendingCount()
    pendingCount.value = response.count
  } catch (error) {
    console.error('Failed to load pending count:', error)
  }
}

async function switchTab(tab: TabType) {
  if (activeTab.value === tab) return
  activeTab.value = tab
  page.value = 1
  // 使用静默刷新，避免 Tab 切换时内容闪烁
  await loadTransfers(true)
}

let searchTimeout: ReturnType<typeof setTimeout> | null = null

function handleSearch() {
  page.value = 1
  if (searchTimeout) {
    clearTimeout(searchTimeout)
    searchTimeout = null
  }
  searchTimeout = setTimeout(() => {
    loadTransfers(true)
    searchTimeout = null
  }, 300)
}

function clearSearch() {
  searchQuery.value = ''
  page.value = 1
  if (searchTimeout) {
    clearTimeout(searchTimeout)
    searchTimeout = null
  }
  loadTransfers(true)
}

function handleSearchEnter() {
  if (searchTimeout) {
    clearTimeout(searchTimeout)
    searchTimeout = null
  }
  page.value = 1
  loadTransfers(true)
}

watch(searchQuery, () => {
  handleSearch()
})

async function handleAccept(transfer: Transfer) {
  if (!confirm(t('transfer.actions.accept') + '?')) return

  actionLoading.value = transfer.id
  try {
    await api.transfers.accept(transfer.id)
    toast.success(t('transfer.messages.acceptSuccess'))
    await Promise.all([loadTransfers(true), loadPendingCount()])
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    actionLoading.value = null
  }
}

function openRejectModal(transfer: Transfer) {
  rejectingTransferId.value = transfer.id
  rejectReason.value = ''
  showRejectModal.value = true
}

async function handleReject() {
  if (!rejectingTransferId.value) return

  rejectLoading.value = true
  try {
    await api.transfers.reject(rejectingTransferId.value, rejectReason.value || undefined)
    toast.success(t('transfer.messages.rejectSuccess'))
    showRejectModal.value = false
    await Promise.all([loadTransfers(true), loadPendingCount()])
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    rejectLoading.value = false
  }
}

async function handleCancel(transfer: Transfer) {
  if (!confirm(t('transfer.actions.cancel') + '?')) return

  actionLoading.value = transfer.id
  try {
    await api.transfers.cancel(transfer.id)
    toast.success(t('transfer.messages.cancelSuccess'))
    await loadTransfers(true)
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    actionLoading.value = null
  }
}

async function handlePush(transfer: Transfer) {
  if (!confirm(t('transfer.actions.push') + '?')) return

  pushLoading.value = transfer.id
  try {
    await api.transfers.push(transfer.id)
    toast.success(t('transfer.messages.pushSuccess'))
    await Promise.all([loadTransfers(true), loadPendingCount()])
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    pushLoading.value = null
  }
}

function getStatusBadgeVariant(status: string): BadgeVariant {
  switch (status) {
    case 'pending':
      return 'warning'
    case 'processing':
      return 'info'
    case 'accepted':
      return 'success'
    case 'rejected':
      return 'error'
    case 'cancelled':
    default:
      return 'default'
  }
}

function formatDate(dateStr: string | null) {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleString()
}

function formatMemory(mb: number) {
  if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`
  return `${mb} MB`
}

function formatDisk(mb: number) {
  if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`
  return `${mb} MB`
}

function openConfigModal(transfer: Transfer) {
  selectedTransfer.value = transfer
  showConfigModal.value = true
}

function toggleRemark(transferId: number) {
  if (expandedRemarks.value.has(transferId)) {
    expandedRemarks.value.delete(transferId)
  } else {
    expandedRemarks.value.add(transferId)
  }
}

function getNetworkModeText(mode: string | null | undefined) {
  if (!mode) return '-'
  return mode === 'nat' ? 'NAT' : (mode === 'routed' ? t('instance.networkMode.routed') : mode)
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- Header -->
    <PageHeader
      data-reveal
      :title="$t('transfer.title')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="refreshing"
          @click="loadTransfers(true)"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': refreshing }" />
          </template>
          {{ $t('common.refresh') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Tabs + Search Card -->
    <Card data-reveal padding="sm">
      <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <!-- Segmented Tab switcher -->
        <div class="inline-flex items-center rounded-xl bg-[var(--bg-surface-soft)] p-1 border border-[var(--border-color)]">
          <button
            type="button"
            class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
            :class="activeTab === 'sent'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('sent')"
          >
            {{ $t('transfer.sentTab') }}
          </button>
          <button
            type="button"
            class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98] gap-1.5"
            :class="activeTab === 'received'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('received')"
          >
            <span>{{ $t('transfer.receivedTab') }}</span>
            <Badge
              v-if="pendingCount > 0"
              variant="warning"
              size="sm"
            >
              {{ pendingCount > 9 ? '9+' : pendingCount }}
            </Badge>
          </button>
        </div>

        <!-- Search input using UI Input -->
        <div class="w-full sm:w-72">
          <Input
            v-model="searchQuery"
            size="sm"
            clearable
            :placeholder="$t('transfer.searchPlaceholder')"
            @enter="handleSearchEnter"
            @clear="clearSearch"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
      </div>
    </Card>

    <!-- Loading -->
    <div v-if="loading">
      <SkeletonLoader type="table" />
    </div>

    <!-- Empty State -->
    <Card v-else-if="transfers.length === 0" data-reveal padding="none">
      <EmptyState
        :title="activeTab === 'sent' ? $t('transfer.noTransfers') : $t('transfer.noPendingTransfers')"
      >
        <template #icon>
          <ArrowRightLeft class="w-7 h-7 stroke-[1.5]" />
        </template>
      </EmptyState>
    </Card>

    <!-- Transfer List -->
    <template v-else>
      <!-- Mobile Cards (Locked container: class="space-y-3 lg:hidden") -->
      <div class="space-y-3 lg:hidden">
        <Card
          v-for="transfer in transfers"
          :key="transfer.id"
          padding="sm"
          class="transition-all duration-300 hover:shadow-[0_8px_24px_rgba(0,0,0,0.06)]"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ transfer.instanceName }}</div>
              <div class="mt-1 text-xs font-mono text-[var(--text-tertiary)] tabular-nums">ID: {{ transfer.instanceId }}</div>
            </div>
            <Badge :variant="getStatusBadgeVariant(transfer.status)">
              {{ $t(`transfer.status.${transfer.status}`) }}
            </Badge>
          </div>

          <div class="mt-4 flex items-center gap-2.5">
            <UserAvatar
              :username="activeTab === 'sent' ? (transfer.toUser?.username || '') : (transfer.fromUser?.username || '')"
              :email="activeTab === 'sent' ? (transfer.toUser?.email || null) : (transfer.fromUser?.email || null)"
              :avatar-style="activeTab === 'sent' ? (transfer.toUser?.avatarStyle || '') : (transfer.fromUser?.avatarStyle || '')"
              :badge-id="activeTab === 'sent' ? (transfer.toUser?.avatarBadgeId || null) : (transfer.fromUser?.avatarBadgeId || null)"
              :size="28"
            />
            <div class="min-w-0">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">
                {{ activeTab === 'sent' ? $t('transfer.detail.toUser') : $t('transfer.detail.fromUser') }}
              </div>
              <div class="truncate text-sm font-medium text-[var(--text-primary)]">
                {{ activeTab === 'sent' ? transfer.toUser?.username : transfer.fromUser?.username }}
              </div>
            </div>
          </div>

          <div class="mt-4 grid grid-cols-2 gap-2 text-sm">
            <button
              v-if="transfer.snapshot"
              type="button"
              class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 text-left border border-[var(--border-color)] hover:border-[var(--border-hover)] transition-all duration-200 active:scale-[0.98]"
              @click="openConfigModal(transfer)"
            >
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.detail.snapshot') }}</div>
              <div class="mt-1 flex min-w-0 items-center gap-1.5 font-medium text-[var(--text-primary)]">
                <FlagIcon :code="transfer.snapshot.hostCountryCode || 'us'" size="xs" />
                <span class="truncate">{{ (transfer.snapshot.hostName || '-').toUpperCase() }}</span>
              </div>
            </button>
            <div v-else class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.detail.snapshot') }}</div>
              <div class="mt-1 text-[var(--text-tertiary)]">-</div>
            </div>

            <div class="rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)]">
              <div class="text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('common.createdAt') }}</div>
              <div class="mt-1 text-xs font-mono text-[var(--text-primary)] tabular-nums">{{ formatDate(transfer.createdAt) }}</div>
            </div>
          </div>

          <div v-if="transfer.remark" class="mt-3 rounded-xl bg-[var(--bg-surface-soft)] p-2.5 border border-[var(--border-color)] text-sm">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.modal.remark') }}</div>
            <div v-if="!expandedRemarks.has(transfer.id)" class="flex items-center gap-1.5">
              <span class="text-[var(--text-secondary)] text-xs">{{ $t('transfer.hasRemark') }}</span>
              <button
                type="button"
                class="text-xs text-[var(--accent)] hover:underline font-medium"
                @click="toggleRemark(transfer.id)"
              >
                {{ $t('common.expand') }}
              </button>
            </div>
            <div v-else>
              <div class="break-words text-xs text-[var(--text-primary)] leading-relaxed">{{ transfer.remark }}</div>
              <button
                type="button"
                class="mt-1 text-xs text-[var(--accent)] hover:underline font-medium"
                @click="toggleRemark(transfer.id)"
              >
                {{ $t('common.collapse') }}
              </button>
            </div>
          </div>

          <div class="mt-4 flex flex-wrap items-center justify-end gap-2">
            <template v-if="activeTab === 'sent'">
              <template v-if="transfer.status === 'pending'">
                <Button
                  v-if="transfer.canPush"
                  size="sm"
                  variant="secondary"
                  :loading="pushLoading === transfer.id"
                  :disabled="actionLoading === transfer.id"
                  @click="handlePush(transfer)"
                >
                  <template #icon>
                    <Send class="w-3.5 h-3.5" />
                  </template>
                  {{ pushLoading === transfer.id ? $t('common.processing') : $t('transfer.actions.push') }}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                  :disabled="actionLoading === transfer.id || pushLoading === transfer.id"
                  @click="handleCancel(transfer)"
                >
                  <template #icon>
                    <Ban class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('transfer.actions.cancel') }}
                </Button>
              </template>
              <div v-else-if="transfer.status === 'accepted'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                {{ $t('transfer.completedAt') }}: {{ formatDate(transfer.acceptedAt) }}
              </div>
              <div v-else-if="transfer.status === 'rejected'" class="min-w-0 text-right text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                <div>{{ $t('transfer.rejectedAt') }}: {{ formatDate(transfer.rejectedAt) }}</div>
                <div v-if="transfer.rejectReason" class="mt-0.5 truncate text-[11px] text-[var(--text-tertiary)]" :title="transfer.rejectReason">
                  {{ transfer.rejectReason }}
                </div>
              </div>
              <div v-else-if="transfer.status === 'cancelled'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                {{ $t('transfer.cancelledAt') }}: {{ formatDate(transfer.cancelledAt) }}
              </div>
            </template>
            <template v-else>
              <template v-if="transfer.status === 'pending'">
                <Button
                  size="sm"
                  variant="primary"
                  :loading="actionLoading === transfer.id"
                  @click="handleAccept(transfer)"
                >
                  <template #icon>
                    <Check class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('transfer.actions.accept') }}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                  :disabled="actionLoading === transfer.id"
                  @click="openRejectModal(transfer)"
                >
                  <template #icon>
                    <X class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('transfer.actions.reject') }}
                </Button>
              </template>
              <div v-else-if="transfer.status === 'accepted'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                {{ $t('transfer.completedAt') }}: {{ formatDate(transfer.acceptedAt) }}
              </div>
              <div v-else-if="transfer.status === 'rejected'" class="min-w-0 text-right text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                <div>{{ $t('transfer.rejectedAt') }}: {{ formatDate(transfer.rejectedAt) }}</div>
                <div v-if="transfer.rejectReason" class="mt-0.5 truncate text-[11px] text-[var(--text-tertiary)]" :title="transfer.rejectReason">
                  {{ transfer.rejectReason }}
                </div>
              </div>
              <div v-else-if="transfer.status === 'cancelled'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                {{ $t('transfer.cancelledAt') }}: {{ formatDate(transfer.cancelledAt) }}
              </div>
            </template>
          </div>
        </Card>

        <!-- Mobile Pagination -->
        <Card v-if="totalPages > 1" padding="sm">
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            @update:page="newPage => { page = newPage; loadTransfers(true) }"
          >
            <template #total="{ total: tTotal, page: cPage, totalPages: tPages }">
              <span>{{ tTotal }} {{ $t('common.total') }} · {{ cPage }} / {{ tPages }}</span>
            </template>
          </Pagination>
        </Card>
      </div>

      <!-- Desktop Table (Locked container: class="card hidden overflow-hidden lg:block") -->
      <div class="card hidden overflow-hidden lg:block">
        <table class="w-full table-fixed">
          <thead>
            <tr class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
              <th class="w-[20%] px-4 py-3 whitespace-nowrap">{{ $t('transfer.detail.instance') }}</th>
              <th class="w-[16%] px-4 py-3 whitespace-nowrap">
                {{ activeTab === 'sent' ? $t('transfer.detail.toUser') : $t('transfer.detail.fromUser') }}
              </th>
              <th class="w-[16%] px-4 py-3 whitespace-nowrap">{{ $t('transfer.detail.snapshot') }}</th>
              <th class="w-[15%] px-4 py-3 whitespace-nowrap">{{ $t('transfer.modal.remark') }}</th>
              <th class="w-[11%] px-4 py-3 whitespace-nowrap">{{ $t('common.status') }}</th>
              <th class="w-[12%] px-4 py-3 whitespace-nowrap">{{ $t('common.createdAt') }}</th>
              <th class="w-[10%] px-4 py-3 text-right whitespace-nowrap">{{ $t('common.actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]">
            <tr
              v-for="transfer in transfers"
              :key="transfer.id"
              class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
            >
              <!-- 实例列：名称 + ID (Locked: class="truncate font-medium") -->
              <td class="px-4 py-3.5">
                <div class="truncate font-medium" :title="transfer.instanceName">{{ transfer.instanceName }}</div>
                <div class="truncate text-xs font-mono text-[var(--text-tertiary)] tabular-nums" :title="String(transfer.instanceId)">
                  ID: {{ transfer.instanceId }}
                </div>
              </td>
              <!-- 发起方/接收方 -->
              <td class="px-4 py-3.5">
                <div class="flex min-w-0 items-center gap-2">
                  <UserAvatar
                    :username="activeTab === 'sent' ? (transfer.toUser?.username || '') : (transfer.fromUser?.username || '')"
                    :email="activeTab === 'sent' ? (transfer.toUser?.email || null) : (transfer.fromUser?.email || null)"
                    :avatar-style="activeTab === 'sent' ? (transfer.toUser?.avatarStyle || '') : (transfer.fromUser?.avatarStyle || '')"
                    :badge-id="activeTab === 'sent' ? (transfer.toUser?.avatarBadgeId || null) : (transfer.fromUser?.avatarBadgeId || null)"
                    :size="26"
                  />
                  <span class="min-w-0 truncate text-sm text-[var(--text-primary)]" :title="activeTab === 'sent' ? transfer.toUser?.username : transfer.fromUser?.username">
                    {{ activeTab === 'sent' ? transfer.toUser?.username : transfer.fromUser?.username }}
                  </span>
                </div>
              </td>
              <!-- 转移时配置：可点击查看详情 -->
              <td class="px-4 py-3.5 text-sm">
                <button
                  v-if="transfer.snapshot"
                  type="button"
                  class="flex min-w-0 items-center gap-1.5 font-medium text-[var(--text-primary)] hover:text-[var(--accent)] transition-colors group cursor-pointer"
                  @click="openConfigModal(transfer)"
                >
                  <FlagIcon :code="transfer.snapshot.hostCountryCode || 'us'" size="xs" />
                  <span class="truncate" :title="(transfer.snapshot.hostName || '-').toUpperCase()">
                    {{ (transfer.snapshot.hostName || '-').toUpperCase() }}
                  </span>
                  <Info class="w-3.5 h-3.5 text-[var(--text-tertiary)] group-hover:text-[var(--accent)] shrink-0 transition-colors" />
                </button>
                <span v-else class="text-[var(--text-tertiary)]">-</span>
              </td>
              <!-- 备注：可展开 -->
              <td class="px-4 py-3.5 text-sm">
                <template v-if="transfer.remark">
                  <div v-if="!expandedRemarks.has(transfer.id)" class="flex items-center gap-1.5">
                    <span class="text-[var(--text-secondary)] text-xs">{{ $t('transfer.hasRemark') }}</span>
                    <button
                      type="button"
                      class="text-xs text-[var(--accent)] hover:underline font-medium"
                      @click="toggleRemark(transfer.id)"
                    >
                      {{ $t('common.expand') }}
                    </button>
                  </div>
                  <div v-else class="max-w-full text-xs">
                    <div class="break-words text-[var(--text-primary)] leading-relaxed">
                      {{ transfer.remark }}
                    </div>
                    <button
                      type="button"
                      class="mt-1 text-xs text-[var(--accent)] hover:underline font-medium"
                      @click="toggleRemark(transfer.id)"
                    >
                      {{ $t('common.collapse') }}
                    </button>
                  </div>
                </template>
                <span v-else class="text-[var(--text-tertiary)] text-xs">-</span>
              </td>
              <!-- 状态 -->
              <td class="px-4 py-3.5 whitespace-nowrap">
                <Badge :variant="getStatusBadgeVariant(transfer.status)">
                  {{ $t(`transfer.status.${transfer.status}`) }}
                </Badge>
              </td>
              <!-- 创建时间 -->
              <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)] whitespace-nowrap font-mono tabular-nums">
                {{ formatDate(transfer.createdAt) }}
              </td>
              <!-- 操作 (Locked: class="flex flex-wrap items-center justify-end gap-2") -->
              <td class="px-4 py-3.5 text-right whitespace-nowrap">
                <div class="flex flex-wrap items-center justify-end gap-2">
                  <!-- Sent tab actions -->
                  <template v-if="activeTab === 'sent'">
                    <template v-if="transfer.status === 'pending'">
                      <Button
                        v-if="transfer.canPush"
                        size="sm"
                        variant="secondary"
                        :loading="pushLoading === transfer.id"
                        :disabled="actionLoading === transfer.id"
                        @click="handlePush(transfer)"
                      >
                        <template #icon>
                          <Send class="w-3.5 h-3.5" />
                        </template>
                        {{ pushLoading === transfer.id ? $t('common.processing') : $t('transfer.actions.push') }}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                        :disabled="actionLoading === transfer.id || pushLoading === transfer.id"
                        @click="handleCancel(transfer)"
                      >
                        <template #icon>
                          <Ban class="w-3.5 h-3.5" />
                        </template>
                        {{ $t('transfer.actions.cancel') }}
                      </Button>
                    </template>
                    <div v-else-if="transfer.status === 'accepted'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                      {{ $t('transfer.completedAt') }}: {{ formatDate(transfer.acceptedAt) }}
                    </div>
                    <div v-else-if="transfer.status === 'rejected'" class="text-xs text-[var(--text-secondary)] text-right font-mono tabular-nums max-w-xs">
                      <div>{{ $t('transfer.rejectedAt') }}: {{ formatDate(transfer.rejectedAt) }}</div>
                      <div v-if="transfer.rejectReason" class="text-[11px] text-[var(--text-tertiary)] mt-0.5 truncate" :title="transfer.rejectReason">
                        {{ transfer.rejectReason }}
                      </div>
                    </div>
                    <div v-else-if="transfer.status === 'cancelled'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                      {{ $t('transfer.cancelledAt') }}: {{ formatDate(transfer.cancelledAt) }}
                    </div>
                  </template>
                  <!-- Received tab actions -->
                  <template v-else>
                    <template v-if="transfer.status === 'pending'">
                      <Button
                        size="sm"
                        variant="primary"
                        :loading="actionLoading === transfer.id"
                        @click="handleAccept(transfer)"
                      >
                        <template #icon>
                          <Check class="w-3.5 h-3.5" />
                        </template>
                        {{ $t('transfer.actions.accept') }}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                        :disabled="actionLoading === transfer.id"
                        @click="openRejectModal(transfer)"
                      >
                        <template #icon>
                          <X class="w-3.5 h-3.5" />
                        </template>
                        {{ $t('transfer.actions.reject') }}
                      </Button>
                    </template>
                    <div v-else-if="transfer.status === 'accepted'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                      {{ $t('transfer.completedAt') }}: {{ formatDate(transfer.acceptedAt) }}
                    </div>
                    <div v-else-if="transfer.status === 'rejected'" class="text-xs text-[var(--text-secondary)] text-right font-mono tabular-nums max-w-xs">
                      <div>{{ $t('transfer.rejectedAt') }}: {{ formatDate(transfer.rejectedAt) }}</div>
                      <div v-if="transfer.rejectReason" class="text-[11px] text-[var(--text-tertiary)] mt-0.5 truncate" :title="transfer.rejectReason">
                        {{ transfer.rejectReason }}
                      </div>
                    </div>
                    <div v-else-if="transfer.status === 'cancelled'" class="text-xs text-[var(--text-secondary)] font-mono tabular-nums">
                      {{ $t('transfer.cancelledAt') }}: {{ formatDate(transfer.cancelledAt) }}
                    </div>
                  </template>
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <!-- Desktop Pagination -->
        <div
          v-if="totalPages > 1"
          class="border-t border-[var(--border-color)] px-4 py-3"
        >
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            @update:page="newPage => { page = newPage; loadTransfers(true) }"
          >
            <template #total="{ total: tTotal, page: cPage, totalPages: tPages }">
              <span>{{ tTotal }} {{ $t('common.total') }} · {{ cPage }} / {{ tPages }}</span>
            </template>
          </Pagination>
        </div>
      </div>
    </template>

    <!-- Reject Modal -->
    <DrawerModal
      :show="showRejectModal"
      max-width="max-w-md"
      raw
      @close="showRejectModal = false"
    >
      <div class="modal-header flex items-center justify-between border-b border-[var(--border-color)] p-4 md:p-6">
        <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('transfer.rejectModal.title') }}</h3>
        <Button size="sm" variant="ghost" class="w-8 h-8 !p-0 rounded-full" @click="showRejectModal = false">
          <X class="w-4 h-4" />
        </Button>
      </div>
      <div class="modal-body p-4 md:p-6 space-y-4">
        <div>
          <label class="mb-1.5 block text-xs font-semibold tracking-wide text-[var(--text-secondary)]">{{ $t('transfer.rejectModal.reason') }}</label>
          <textarea
            v-model="rejectReason"
            class="w-full h-24 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] resize-none transition-all"
            :placeholder="$t('transfer.rejectModal.reasonPlaceholder')"
          ></textarea>
        </div>
      </div>
      <div class="modal-footer flex items-center justify-end gap-2 border-t border-[var(--border-color)] p-4 md:p-6 bg-[var(--bg-surface-soft)]">
        <Button variant="secondary" @click="showRejectModal = false">
          {{ $t('common.cancel') }}
        </Button>
        <Button
          variant="danger"
          :loading="rejectLoading"
          @click="handleReject"
        >
          {{ $t('transfer.actions.reject') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- Config Detail Modal -->
    <DrawerModal
      v-if="selectedTransfer"
      :show="showConfigModal"
      max-width="max-w-lg"
      raw
      @close="showConfigModal = false"
    >
      <div class="modal-header flex items-center justify-between border-b border-[var(--border-color)] p-4 md:p-6">
        <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('transfer.configModal.title') }}</h3>
        <Button size="sm" variant="ghost" class="w-8 h-8 !p-0 rounded-full" @click="showConfigModal = false">
          <X class="w-4 h-4" />
        </Button>
      </div>

      <div class="modal-body p-4 md:p-6 space-y-4">
        <!-- 实例信息 -->
        <div class="grid grid-cols-2 gap-3 text-sm">
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.configModal.instanceName') }}</div>
            <div class="font-medium text-[var(--text-primary)] truncate">{{ selectedTransfer.snapshot?.originalName || selectedTransfer.instanceName }}</div>
          </div>
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('instance.detail.info.instanceId') }}</div>
            <div class="font-mono text-[var(--text-primary)] truncate">{{ selectedTransfer.instanceId }}</div>
          </div>
        </div>

        <!-- 宿主机信息 -->
        <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
          <div class="mb-2 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.configModal.hostInfo') }}</div>
          <div class="flex items-center gap-2">
            <FlagIcon :code="selectedTransfer.snapshot?.hostCountryCode || 'us'" size="sm" />
            <span class="font-medium text-[var(--text-primary)]">{{ (selectedTransfer.snapshot?.hostName || '-').toUpperCase() }}</span>
            <span v-if="selectedTransfer.snapshot?.hostLocation" class="text-[var(--text-secondary)] text-sm">
              ({{ selectedTransfer.snapshot.hostLocation }})
            </span>
          </div>
        </div>

        <!-- 配置信息 -->
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-2.5 text-sm">
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">CPU</div>
            <div class="font-mono font-semibold text-[var(--text-primary)]">{{ selectedTransfer.snapshot?.cpu ?? '-' }}%</div>
          </div>
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('instance.detail.info.memory') }}</div>
            <div class="font-mono font-semibold text-[var(--text-primary)]">{{ selectedTransfer.snapshot?.memory ? formatMemory(selectedTransfer.snapshot.memory) : '-' }}</div>
          </div>
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('instance.detail.info.disk') }}</div>
            <div class="font-mono font-semibold text-[var(--text-primary)]">{{ selectedTransfer.snapshot?.disk ? formatDisk(selectedTransfer.snapshot.disk) : '-' }}</div>
          </div>
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.configModal.networkMode') }}</div>
            <div class="font-medium text-[var(--text-primary)]">{{ getNetworkModeText(selectedTransfer.snapshot?.networkMode) }}</div>
          </div>
        </div>

        <!-- 网络信息 -->
        <div class="grid grid-cols-2 gap-3 text-sm">
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">IPv4</div>
            <div class="font-mono text-xs text-[var(--text-primary)]">{{ selectedTransfer.snapshot?.ipv4 || '-' }}</div>
          </div>
          <div class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)]">
            <div class="mb-1 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">IPv6</div>
            <div class="truncate font-mono text-xs text-[var(--text-primary)]" :title="selectedTransfer.snapshot?.ipv6 || ''">
              {{ selectedTransfer.snapshot?.ipv6 || '-' }}
            </div>
          </div>
        </div>

        <!-- 套餐信息 -->
        <div v-if="selectedTransfer.snapshot?.packageName" class="rounded-xl bg-[var(--bg-surface-soft)] p-3 border border-[var(--border-color)] text-sm">
          <div class="mb-1 text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">{{ $t('transfer.configModal.package') }}</div>
          <div class="text-[var(--text-primary)] font-medium">{{ selectedTransfer.snapshot.packageName }}</div>
        </div>
      </div>

      <div class="modal-footer flex items-center justify-end border-t border-[var(--border-color)] p-4 md:p-6 bg-[var(--bg-surface-soft)]">
        <Button variant="secondary" @click="showConfigModal = false">
          {{ $t('common.close') }}
        </Button>
      </div>
    </DrawerModal>
  </div>
</template>
