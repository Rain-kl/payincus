<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { useInboxStore } from '@/stores/inbox'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import { formatRelativeTime } from '@/utils/formatters'
import {
  getMessageCategory,
  getMessageRoute,
  canNavigate,
  getAllCategories,
  type MessageCategory
} from '@/utils/inboxHelper'
import type { InboxMessage } from '@/types/api'
import api from '@/api'
import { useReveal } from '@/composables/useReveal'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  PageHeader,
  Pagination,
  type BadgeVariant
} from '@/components/ui'
import {
  ArrowRightLeft,
  Bell,
  Camera,
  Check,
  CheckCheck,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  ExternalLink,
  Eye,
  Gauge,
  LifeBuoy,
  Package,
  RefreshCw,
  Server,
  Shield,
  Trash2,
  X
} from 'lucide-vue-next'

defineOptions({ name: 'InboxView' })

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const inboxStore = useInboxStore()
const toast = useToast()
const authStore = useAuthStore()

// 消息列表与加载状态
const messages = ref<InboxMessage[]>([])
const loading = ref(false)
const refreshing = ref(false)
const loaded = ref(false)

// 分页
const currentPage = ref(1)
const pageSize = ref(20)
const total = ref(0)
const totalPages = computed(() => Math.ceil(total.value / pageSize.value))

// 筛选
const filter = ref<'all' | 'unread'>('all')
const categoryFilter = ref<MessageCategory | 'all'>('all')

// 批量选择与展开交互
const selectedIds = ref<Set<number>>(new Set())
const expandedIds = ref<Set<number>>(new Set())
const activeMessage = ref<InboxMessage | null>(null)
const batchProcessing = ref(false)

// 所有类别（根据用户配额过滤）
const allCategories = getAllCategories()

// 根据用户配额过滤显示的类别
const categories = computed(() => {
  const quota = authStore.quota
  return allCategories.filter(cat => {
    // 好友功能已禁用，隐藏好友分类
    if (cat.key === 'social') {
      return false
    }
    // 如果套餐配额为0，隐藏套餐分类
    if (cat.key === 'package' && quota?.packageLimit === 0) {
      return false
    }
    // 如果套餐配额和节点配额都为0，隐藏配额分类（没有配额警告的意义）
    if (cat.key === 'quota' && quota?.packageLimit === 0 && quota?.hostLimit === 0) {
      return false
    }
    return true
  })
})

// 根据类别筛选后的消息
const filteredMessages = computed(() => {
  if (categoryFilter.value === 'all') return messages.value
  return messages.value.filter(m => getMessageCategory(m.eventType) === categoryFilter.value)
})

// 全选状态
const isAllSelected = computed(() => {
  return filteredMessages.value.length > 0 && filteredMessages.value.every(m => selectedIds.value.has(m.id))
})

const isPartiallySelected = computed(() => {
  return !isAllSelected.value && filteredMessages.value.some(m => selectedIds.value.has(m.id))
})

const selectedCount = computed(() => selectedIds.value.size)
const hasSelected = computed(() => selectedIds.value.size > 0)

// 类别 Badge 变体映射
function getCategoryBadgeVariant(cat: MessageCategory): BadgeVariant {
  switch (cat) {
    case 'instance':
    case 'snapshot':
      return 'info'
    case 'security':
    case 'quota':
    case 'transfer':
      return 'warning'
    case 'ticket':
      return 'info'
    case 'system':
    case 'package':
    default:
      return 'default'
  }
}

// 类别图标映射
function getCategoryIcon(cat: MessageCategory) {
  switch (cat) {
    case 'instance':
      return Server
    case 'snapshot':
      return Camera
    case 'transfer':
      return ArrowRightLeft
    case 'package':
      return Package
    case 'security':
      return Shield
    case 'quota':
      return Gauge
    case 'ticket':
      return LifeBuoy
    case 'system':
    default:
      return Bell
  }
}

// 获取类别名称
function getCategoryLabel(message: InboxMessage): string {
  const category = getMessageCategory(message.eventType)
  return t(`inbox.categories.${category}`)
}

// 加载消息列表
async function loadMessages(silent = false) {
  if (!silent) {
    loading.value = true
  } else {
    refreshing.value = true
  }
  try {
    const res = await api.inbox.list({
      page: currentPage.value,
      pageSize: pageSize.value,
      isRead: filter.value === 'unread' ? false : undefined
    })
    messages.value = res.messages
    total.value = res.total
    selectedIds.value.clear()
  } catch (err) {
    console.error('Failed to load inbox messages:', err)
    toast.error(t('common.error'))
  } finally {
    loading.value = false
    refreshing.value = false
    loaded.value = true
  }
}

// 标记单条已读
async function markAsRead(message: InboxMessage) {
  if (message.isRead) return
  try {
    await api.inbox.markAsRead(message.id)
    message.isRead = true
    inboxStore.decrementUnread()
  } catch {
    // 静默失败
  }
}

// 全部标记已读
async function markAllAsRead() {
  try {
    const res = await api.inbox.markAllAsRead()
    messages.value.forEach(m => (m.isRead = true))
    inboxStore.clearUnread()
    selectedIds.value.clear()
    toast.success(t('inbox.markedAllRead'))
    if (res.count > 0) {
      loadMessages(true)
    }
  } catch {
    toast.error(t('common.error'))
  }
}

// 批量标记已读
async function markSelectedAsRead(): Promise<void> {
  if (selectedIds.value.size === 0 || batchProcessing.value) return
  batchProcessing.value = true
  try {
    const unreadSelected = filteredMessages.value.filter(m => selectedIds.value.has(m.id) && !m.isRead)
    if (unreadSelected.length > 0) {
      await Promise.all(unreadSelected.map(m => api.inbox.markAsRead(m.id)))
      unreadSelected.forEach(m => {
        m.isRead = true
        inboxStore.decrementUnread()
      })
    }
    toast.success(t('inbox.markedRead'))
    selectedIds.value.clear()
  } catch {
    toast.error(t('common.error'))
  } finally {
    batchProcessing.value = false
  }
}

// 删除单条消息
async function deleteMessage(message: InboxMessage) {
  if (!confirm(t('inbox.deleteConfirm'))) return
  try {
    await api.inbox.delete(message.id)
    messages.value = messages.value.filter(m => m.id !== message.id)
    total.value = Math.max(0, total.value - 1)
    if (!message.isRead) {
      inboxStore.decrementUnread()
    }
    selectedIds.value.delete(message.id)
    expandedIds.value.delete(message.id)
    toast.success(t('inbox.deleted'))
  } catch {
    toast.error(t('common.error'))
  }
}

// 批量删除已选
async function deleteSelected(): Promise<void> {
  if (selectedIds.value.size === 0 || batchProcessing.value) return
  if (!confirm(t('inbox.deleteConfirm'))) return
  batchProcessing.value = true
  try {
    const toDeleteIds = Array.from(selectedIds.value)
    await Promise.all(toDeleteIds.map(id => api.inbox.delete(id)))
    const deletedUnreadCount = messages.value.filter(m => selectedIds.value.has(m.id) && !m.isRead).length
    messages.value = messages.value.filter(m => !selectedIds.value.has(m.id))
    total.value = Math.max(0, total.value - toDeleteIds.length)
    if (deletedUnreadCount > 0) {
      inboxStore.refresh()
    }
    selectedIds.value.clear()
    toast.success(t('inbox.deleted'))
  } catch {
    toast.error(t('common.error'))
  } finally {
    batchProcessing.value = false
  }
}

// 清空已读
async function deleteReadMessages() {
  if (!confirm(t('inbox.clearConfirm'))) return
  try {
    await api.inbox.deleteRead()
    selectedIds.value.clear()
    expandedIds.value.clear()
    loadMessages(true)
    toast.success(t('inbox.cleared'))
  } catch {
    toast.error(t('common.error'))
  }
}

// 切换选择
function toggleSelectAll(): void {
  if (isAllSelected.value) {
    selectedIds.value.clear()
  } else {
    filteredMessages.value.forEach(m => selectedIds.value.add(m.id))
  }
}

function toggleSelect(id: number): void {
  if (selectedIds.value.has(id)) {
    selectedIds.value.delete(id)
  } else {
    selectedIds.value.add(id)
  }
}

// 展开/收起单条正文
function toggleExpand(id: number): void {
  if (expandedIds.value.has(id)) {
    expandedIds.value.delete(id)
  } else {
    expandedIds.value.add(id)
  }
}

// 打开详情模态框
async function openDetail(message: InboxMessage): Promise<void> {
  activeMessage.value = message
  if (!message.isRead) {
    await markAsRead(message)
  }
}

function closeDetail(): void {
  activeMessage.value = null
}

async function handleNavigateFromModal(): Promise<void> {
  if (!activeMessage.value) return
  const msg = activeMessage.value
  closeDetail()
  const targetRoute = getMessageRoute(msg)
  if (targetRoute) {
    router.push(targetRoute)
  }
}

async function handleDeleteFromModal(): Promise<void> {
  if (!activeMessage.value) return
  const msg = activeMessage.value
  closeDetail()
  await deleteMessage(msg)
}

// 切换已读/未读状态筛选
function setFilter(newFilter: 'all' | 'unread') {
  if (filter.value === newFilter) return
  filter.value = newFilter
  currentPage.value = 1
  selectedIds.value.clear()
  loadMessages()
}

// 切换类别筛选
function setCategoryFilter(category: MessageCategory | 'all') {
  categoryFilter.value = category
  selectedIds.value.clear()
}

// 点击消息行
async function handleMessageClick(message: InboxMessage) {
  await markAsRead(message)
  const targetRoute = getMessageRoute(message)
  if (targetRoute) {
    router.push(targetRoute)
  } else {
    openDetail(message)
  }
}

// 分页变化
function onPageChange(payload: { page: number; pageSize: number }) {
  if (payload.page === currentPage.value) return
  currentPage.value = payload.page
  pageSize.value = payload.pageSize
  selectedIds.value.clear()
  loadMessages(true)
}

// 格式化时间
function formatTime(dateStr: string): string {
  return formatRelativeTime(dateStr, t)
}

onMounted(() => {
  loadMessages()
})

// 路由变化时刷新
watch(
  () => route.path,
  () => {
    if (route.path === '/inbox') {
      inboxStore.refresh()
    }
  }
)
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in max-w-5xl mx-auto">
    <!-- Header -->
    <PageHeader
      data-reveal
      :title="t('inbox.title')"
      :description="t('inbox.description')"
    >
      <template #actions>
        <Button
          variant="ghost"
          size="sm"
          :loading="refreshing"
          @click="loadMessages(true)"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" :class="{ 'animate-spin': refreshing }" />
          </template>
          {{ t('common.refresh') }}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          @click="deleteReadMessages"
        >
          <template #icon>
            <Trash2 class="w-4 h-4" />
          </template>
          {{ t('inbox.clearRead') }}
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="markAllAsRead"
        >
          <template #icon>
            <CheckCheck class="w-4 h-4" />
          </template>
          {{ t('inbox.markAllRead') }}
        </Button>
      </template>
    </PageHeader>

    <!-- 筛选工具栏 -->
    <div data-reveal class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <!-- 状态筛选：Apple 胶囊分段控件 -->
      <div class="inline-flex items-center p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] self-start shrink-0">
        <button
          type="button"
          class="inline-flex items-center justify-center rounded-full px-4 py-1.5 text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
          :class="filter === 'all'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="setFilter('all')"
        >
          {{ t('inbox.all') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center gap-1.5 justify-center rounded-full px-4 py-1.5 text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
          :class="filter === 'unread'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="setFilter('unread')"
        >
          <span>{{ t('inbox.unread') }}</span>
          <span
            v-if="inboxStore.unreadCount > 0"
            class="inline-flex min-w-[18px] h-4 items-center justify-center rounded-full bg-[var(--accent)] px-1.5 font-mono text-[10px] font-semibold text-white"
          >
            {{ inboxStore.unreadCount }}
          </span>
        </button>
      </div>

      <!-- 类别筛选：横向滑动胶囊药丸按钮 -->
      <div class="flex items-center gap-1.5 overflow-x-auto py-0.5 min-w-0">
        <button
          type="button"
          class="inline-flex items-center rounded-full px-3 py-1 text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95 shrink-0"
          :class="categoryFilter === 'all'
            ? 'bg-[var(--accent)] text-white shadow-sm'
            : 'bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)] hover:border-[var(--border-hover)]'"
          @click="setCategoryFilter('all')"
        >
          {{ t('inbox.allCategories') }}
        </button>
        <button
          v-for="cat in categories"
          :key="cat.key"
          type="button"
          class="inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95 shrink-0"
          :class="categoryFilter === cat.key
            ? 'bg-[var(--accent)] text-white shadow-sm'
            : 'bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)] hover:border-[var(--border-hover)]'"
          @click="setCategoryFilter(cat.key)"
        >
          <component :is="getCategoryIcon(cat.key)" class="w-3 h-3 stroke-[2]" />
          <span>{{ t(cat.config.label) }}</span>
        </button>
      </div>
    </div>

    <!-- 消息卡片容器 -->
    <Card data-reveal padding="none" class="shadow-sm border border-[var(--border-color)] bg-[var(--bg-surface)] rounded-2xl overflow-hidden">
      <!-- 批量操作栏 / 列表表头 -->
      <div
        v-if="hasSelected"
        class="px-4 py-3 md:px-6 bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))] border-b border-[var(--border-color)] flex flex-wrap items-center justify-between gap-3 transition-colors duration-200"
      >
        <div class="flex items-center gap-3">
          <label class="relative inline-flex items-center justify-center cursor-pointer select-none">
            <input
              type="checkbox"
              :checked="isAllSelected || isPartiallySelected"
              class="peer sr-only"
              @change="toggleSelectAll"
            >
            <div
              class="w-4 h-4 rounded-md border flex items-center justify-center transition-all duration-200"
              :class="[
                isAllSelected || isPartiallySelected
                  ? 'bg-[var(--accent)] border-[var(--accent)] text-white'
                  : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-transparent hover:border-[var(--border-hover)]'
              ]"
            >
              <Check v-if="isAllSelected" class="w-3 h-3 stroke-[3]" />
              <span v-else-if="isPartiallySelected" class="w-2 h-0.5 bg-white rounded-full"></span>
            </div>
          </label>
          <span class="text-xs font-medium text-[var(--text-primary)]">
            已选择 {{ selectedCount }} 项
          </span>
        </div>

        <div class="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            :loading="batchProcessing"
            @click="markSelectedAsRead"
          >
            <template #icon>
              <Check class="w-3.5 h-3.5" />
            </template>
            {{ t('inbox.markRead') }}
          </Button>
          <Button
            variant="danger"
            size="sm"
            :loading="batchProcessing"
            @click="deleteSelected"
          >
            <template #icon>
              <Trash2 class="w-3.5 h-3.5" />
            </template>
            {{ t('common.delete') }}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            @click="selectedIds.clear()"
          >
            {{ t('common.cancel') }}
          </Button>
        </div>
      </div>

      <!-- 常规列表表头（未选中时） -->
      <div
        v-else
        class="px-4 py-3 md:px-6 bg-[var(--bg-surface)] border-b border-[var(--border-color)] flex items-center justify-between gap-3 text-xs text-[var(--text-secondary)] select-none"
      >
        <div class="flex items-center gap-3">
          <label class="relative inline-flex items-center justify-center cursor-pointer select-none">
            <input
              type="checkbox"
              :checked="false"
              class="peer sr-only"
              @change="toggleSelectAll"
            >
            <div class="w-4 h-4 rounded-md border border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:border-[var(--border-hover)] transition-all duration-200 flex items-center justify-center">
            </div>
          </label>
          <span>{{ t('inbox.all') }} {{ total }} {{ t('common.items') }}</span>
          <span v-if="inboxStore.unreadCount > 0" class="text-[var(--accent)] font-medium">
            · {{ inboxStore.unreadCount }} {{ t('inbox.unread') }}
          </span>
        </div>
        <div class="text-[11px] text-[var(--text-tertiary)] hidden sm:block">
          点击通知可查看详情或跳转处理
        </div>
      </div>

      <!-- 加载中骨架状态 -->
      <div v-if="loading && !loaded" class="divide-y divide-[var(--border-color)]">
        <div v-for="i in 5" :key="i" class="p-4 md:px-6 md:py-4 flex items-center gap-3.5 animate-pulse">
          <div class="w-4 h-4 rounded-md bg-[var(--bg-surface-soft)] shrink-0"></div>
          <div class="w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] shrink-0"></div>
          <div class="flex-1 space-y-2 min-w-0">
            <div class="flex items-center gap-2">
              <div class="h-4 w-12 rounded-full bg-[var(--bg-surface-soft)]"></div>
              <div class="h-4 w-40 rounded-md bg-[var(--bg-surface-soft)]"></div>
            </div>
            <div class="h-3.5 w-3/4 rounded-md bg-[var(--bg-surface-soft)]"></div>
          </div>
          <div class="h-3.5 w-16 rounded-md bg-[var(--bg-surface-soft)] shrink-0"></div>
        </div>
      </div>

      <!-- 空状态：无通知 -->
      <EmptyState
        v-else-if="messages.length === 0"
        :title="filter === 'unread' ? t('inbox.noUnread') : t('inbox.noMessages')"
        :description="filter === 'unread' ? '' : t('inbox.description')"
      >
        <template #action v-if="filter === 'unread'">
          <Button variant="secondary" size="sm" @click="setFilter('all')">
            {{ t('inbox.all') }}
          </Button>
        </template>
      </EmptyState>

      <!-- 消息列表 -->
      <div v-else class="divide-y divide-[var(--border-color)]">
        <!-- 类别筛选结果为空 -->
        <EmptyState
          v-if="filteredMessages.length === 0"
          :title="t('inbox.noCategoryMessages')"
        >
          <template #action>
            <Button variant="secondary" size="sm" @click="setCategoryFilter('all')">
              {{ t('inbox.allCategories') }}
            </Button>
          </template>
        </EmptyState>

        <!-- 消息行 -->
        <div
          v-for="message in filteredMessages"
          :key="message.id"
          class="group relative flex items-start gap-3.5 px-4 py-3.5 md:px-6 md:py-4 transition-colors duration-200"
          :class="[
            !message.isRead
              ? 'bg-[color-mix(in_srgb,var(--accent)_3%,var(--bg-surface))] hover:bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))]'
              : 'hover:bg-[var(--bg-surface-soft)]',
            selectedIds.has(message.id) ? 'bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))]' : ''
          ]"
        >
          <!-- 复选框 -->
          <div class="pt-1 shrink-0" @click.stop>
            <label class="relative inline-flex items-center justify-center cursor-pointer select-none">
              <input
                type="checkbox"
                :checked="selectedIds.has(message.id)"
                class="peer sr-only"
                @change="toggleSelect(message.id)"
              >
              <div
                class="w-4 h-4 rounded-md border flex items-center justify-center transition-all duration-200"
                :class="[
                  selectedIds.has(message.id)
                    ? 'bg-[var(--accent)] border-[var(--accent)] text-white'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-transparent hover:border-[var(--border-hover)]'
                ]"
              >
                <Check class="w-3 h-3 stroke-[3]" :class="selectedIds.has(message.id) ? 'opacity-100' : 'opacity-0'" />
              </div>
            </label>
          </div>

          <!-- 类别圆标与未读小红点 -->
          <div class="relative shrink-0 pt-0.5">
            <div class="w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--text-secondary)] transition-transform duration-200 group-hover:scale-105">
              <component :is="getCategoryIcon(getMessageCategory(message.eventType))" class="w-4 h-4 stroke-[2]" />
            </div>
            <span
              v-if="!message.isRead"
              class="absolute -top-0.5 -right-0.5 w-2.5 h-2.5 rounded-full bg-[var(--accent)] ring-2 ring-[var(--bg-surface)]"
            ></span>
          </div>

          <!-- 消息主要内容区域 -->
          <div
            class="min-w-0 flex-1 cursor-pointer"
            @click="handleMessageClick(message)"
          >
            <div class="flex flex-wrap items-center gap-2">
              <Badge :variant="getCategoryBadgeVariant(getMessageCategory(message.eventType))" size="sm">
                {{ getCategoryLabel(message) }}
              </Badge>
              <h3
                class="text-sm tracking-tight text-[var(--text-primary)] transition-colors truncate"
                :class="!message.isRead ? 'font-semibold' : 'font-normal text-[var(--text-secondary)]'"
              >
                {{ message.title }}
              </h3>
              <span class="text-xs text-[var(--text-tertiary)] font-mono shrink-0">
                {{ formatTime(message.createdAt) }}
              </span>
              <ExternalLink
                v-if="canNavigate(message)"
                class="w-3.5 h-3.5 text-[var(--accent)] shrink-0 transition-transform group-hover:translate-x-0.5"
              />
            </div>

            <!-- 正文预览或完整展开 -->
            <p
              class="mt-1 text-xs md:text-sm text-[var(--text-secondary)] leading-relaxed whitespace-pre-line"
              :class="expandedIds.has(message.id) ? '' : 'line-clamp-2'"
            >
              {{ message.content }}
            </p>

            <!-- 展开/收起长文本按钮 -->
            <button
              v-if="message.content && message.content.length > 80"
              type="button"
              class="mt-1 inline-flex items-center gap-1 text-[11px] font-medium text-[var(--accent)] hover:underline"
              @click.stop="toggleExpand(message.id)"
            >
              <span>{{ expandedIds.has(message.id) ? '收起' : '展开' }}</span>
              <ChevronUp v-if="expandedIds.has(message.id)" class="w-3 h-3" />
              <ChevronDown v-else class="w-3 h-3" />
            </button>
          </div>

          <!-- 右侧操作栏 -->
          <div class="flex items-center gap-1 shrink-0 pt-0.5" @click.stop>
            <!-- 查看详情 -->
            <button
              type="button"
              class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
              :title="t('common.detail')"
              :aria-label="t('common.detail')"
              @click="openDetail(message)"
            >
              <Eye class="w-3.5 h-3.5 stroke-[2]" />
            </button>

            <!-- 标记单条已读 -->
            <button
              v-if="!message.isRead"
              type="button"
              class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-tertiary)] hover:text-[var(--accent)] hover:bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95"
              :title="t('inbox.markRead')"
              :aria-label="t('inbox.markRead')"
              @click="markAsRead(message)"
            >
              <Check class="w-3.5 h-3.5 stroke-[2]" />
            </button>

            <!-- 快速跳转 -->
            <button
              v-if="canNavigate(message)"
              type="button"
              class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-tertiary)] hover:text-[var(--accent)] hover:bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95"
              :title="t('common.detail')"
              :aria-label="t('common.detail')"
              @click="handleMessageClick(message)"
            >
              <ChevronRight class="w-3.5 h-3.5 stroke-[2]" />
            </button>

            <!-- 删除 -->
            <button
              type="button"
              class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-tertiary)] hover:text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] transition-all duration-200 active:scale-95"
              :title="t('common.delete')"
              :aria-label="t('common.delete')"
              @click="deleteMessage(message)"
            >
              <Trash2 class="w-3.5 h-3.5 stroke-[2]" />
            </button>
          </div>
        </div>
      </div>

      <!-- 分页栏 -->
      <div
        v-if="totalPages > 1 && categoryFilter === 'all'"
        class="px-4 py-3 md:px-6 border-t border-[var(--border-color)] bg-[var(--bg-surface)]"
      >
        <Pagination
          v-model:page="currentPage"
          :page-size="pageSize"
          :total="total"
          @change="onPageChange"
        />
      </div>
      <!-- 类别筛选时显示计数 -->
      <div
        v-else-if="categoryFilter !== 'all' && filteredMessages.length > 0"
        class="px-4 py-3 md:px-6 border-t border-[var(--border-color)] bg-[var(--bg-surface)] text-xs text-[var(--text-secondary)] font-mono"
      >
        {{ t('inbox.currentPageFiltered', { count: filteredMessages.length }) }}
      </div>
    </Card>

    <!-- 消息详情模态框 (Apple Modal Sheet) -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="opacity-0"
        enter-to-class="opacity-100"
        leave-active-class="transition duration-150 ease-in"
        leave-from-class="opacity-100"
        leave-to-class="opacity-0"
      >
        <div
          v-if="activeMessage"
          class="fixed inset-0 z-50 flex items-center justify-center p-4 sm:p-6"
          role="dialog"
          aria-modal="true"
          :aria-label="activeMessage.title"
        >
          <!-- 背景遮罩磨砂 -->
          <div
            class="absolute inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
            @click="closeDetail"
          ></div>

          <!-- 对话框主体 -->
          <div
            class="relative w-full max-w-lg bg-[var(--bg-surface)] text-[var(--text-primary)] border border-[var(--border-color)] rounded-2xl shadow-[0_8px_24px_rgba(0,0,0,0.12)] overflow-hidden flex flex-col max-h-[85vh] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
            @click.stop
          >
            <!-- 模态框顶栏 -->
            <div class="flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)]">
              <div class="flex items-center gap-2">
                <Badge :variant="getCategoryBadgeVariant(getMessageCategory(activeMessage.eventType))" size="sm">
                  {{ getCategoryLabel(activeMessage) }}
                </Badge>
                <span class="text-xs text-[var(--text-tertiary)] font-mono">
                  {{ formatTime(activeMessage.createdAt) }}
                </span>
              </div>
              <button
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95"
                :title="t('common.close')"
                :aria-label="t('common.close')"
                @click="closeDetail"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>

            <!-- 模态框正文 -->
            <div class="p-6 overflow-y-auto space-y-4">
              <h2 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ activeMessage.title }}
              </h2>
              <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-secondary)] whitespace-pre-line leading-relaxed">
                {{ activeMessage.content }}
              </div>

              <!-- 关联上下文数据 -->
              <div
                v-if="activeMessage.data && Object.keys(activeMessage.data).length > 0"
                class="space-y-1.5 text-xs text-[var(--text-tertiary)]"
              >
                <div class="font-medium text-[var(--text-secondary)]">
                  {{ t('common.detail') }}
                </div>
                <div class="grid grid-cols-2 gap-2 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                  <div
                    v-for="(val, key) in activeMessage.data"
                    :key="key"
                    class="truncate font-mono"
                  >
                    <span class="text-[var(--text-tertiary)]">{{ key }}:</span>
                    <span class="ml-1 text-[var(--text-primary)]">{{ String(val) }}</span>
                  </div>
                </div>
              </div>
            </div>

            <!-- 模态框底栏操作 -->
            <div class="flex items-center justify-between px-6 py-3.5 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
              <Button
                variant="ghost"
                size="sm"
                @click="handleDeleteFromModal"
              >
                <template #icon>
                  <Trash2 class="w-3.5 h-3.5 text-[var(--error)]" />
                </template>
                <span class="text-[var(--error)]">{{ t('common.delete') }}</span>
              </Button>

              <div class="flex items-center gap-2">
                <Button
                  v-if="canNavigate(activeMessage)"
                  variant="primary"
                  size="sm"
                  @click="handleNavigateFromModal"
                >
                  <template #icon>
                    <ExternalLink class="w-3.5 h-3.5" />
                  </template>
                  {{ t('common.detail') }}
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  @click="closeDetail"
                >
                  {{ t('common.close') }}
                </Button>
              </div>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  * {
    transition: none !important;
    animation: none !important;
  }
}
</style>
