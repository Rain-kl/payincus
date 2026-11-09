<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import { useConfigStore } from '@/stores/config'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import UserAvatar from '@/components/UserAvatar.vue'
import InstanceDisplayIcon from '@/components/InstanceDisplayIcon.vue'
import InstanceSelector from '@/components/InstanceSelector.vue'
import TicketImageLightbox from '@/components/tickets/TicketImageLightbox.vue'
import TicketImageUploader from '@/components/tickets/TicketImageUploader.vue'
import TicketInstanceOwnerCard from '@/components/tickets/TicketInstanceOwnerCard.vue'
import { ticketsPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import { Button, Card, Badge, PageHeader, EmptyState, Input, Select, Pagination } from '@/components/ui'
import type { SelectOption } from '@/components/ui'
import {
  Plus,
  Search,
  MessageSquare,
  Send,
  Server,
  User,
  RotateCcw,
  Trash2,
  ExternalLink,
  ChevronRight
} from 'lucide-vue-next'
import type {
  Ticket,
  TicketMessage,
  TicketMessageAttachment,
  TicketStatus,
  TicketPriority,
  TicketCategory,
  TicketObjectLinkType,
  TicketSupportContext,
  InstanceWithDetails
} from '@/types/api'

const { t } = useI18n()
const toast = useToast()
const authStore = useAuthStore()
const configStore = useConfigStore()
const route = useRoute()
const router = useRouter()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// Tab state
type TabType = 'my' | 'host'
const activeTab = ref<TabType>('my')

// View state
type ViewMode = 'list' | 'detail' | 'create'
const viewMode = ref<ViewMode>('list')

// List data
const tickets = ref<Ticket[]>([])
const loading = ref(true)
const refreshing = ref(false)
const pendingCount = ref({ userTickets: 0, hostTickets: 0, total: 0, isHostOwner: false })

// Pagination
const page = ref(1)
const pageSize = ref(100)
const pageSizeOptions = [10, 20, 50, 100]
const total = ref(0)

// Search
const searchQuery = ref('')
let searchDebounceTimer: ReturnType<typeof setTimeout> | null = null

// Filters - 默认筛选活跃工单（排除已关闭）
type StatusFilterType = TicketStatus | 'active' | 'all'
type HostTicketSourceType = 'all' | 'user' | 'official'
type TicketQueueFilter = 'all' | 'pending' | 'due_soon' | 'overdue' | 'waiting_user' | 'waiting_internal'
const statusFilter = ref<StatusFilterType>('active')
const hostFilter = ref<number | ''>('')
const hostTicketSourceFilter = ref<HostTicketSourceType>('all')
const queueFilter = ref<TicketQueueFilter>('all')

// Detail data
const selectedTicket = ref<Ticket | null>(null)
const messages = ref<TicketMessage[]>([])
const messagesLoading = ref(false)
const isOwner = ref(false)
const isCreator = ref(false)
const supportContext = ref<TicketSupportContext | null>(null)
const supportContextLoading = ref(false)
const internalNoteContent = ref('')
const internalNoteSubmitting = ref(false)
const notifyContent = ref('')
const notifySubmitting = ref(false)
const linkForm = ref({
  objectType: 'instance' as TicketObjectLinkType,
  objectId: ''
})
const linkSubmitting = ref(false)

// Messages pagination
const messagesPage = ref(1)
const messagesPageSize = ref(100)
const messagesTotal = ref(0)
const messagesTotalPages = computed(() => Math.ceil(messagesTotal.value / messagesPageSize.value))
const messagesLoadingMore = ref(false)

// Reply
const replyContent = ref('')
const replying = ref(false)
const replyAttachments = ref<File[]>([])

// Create form
const createForm = ref({
  instanceId: null as number | null,
  subject: '',
  category: 'general' as TicketCategory,
  priority: 'normal' as TicketPriority,
  content: ''
})
const creating = ref(false)
const createAttachments = ref<File[]>([])
const availableInstances = ref<InstanceWithDetails[]>([])
const instancesLoading = ref(false)
const attachmentObjectUrls = ref<Record<number, string>>({})
const attachmentLoadingIds = ref<Record<number, boolean>>({})
const lightboxOpen = ref(false)
const lightboxStartIndex = ref(0)

// 将实例列表转换为 InstanceSelector 组件需要的格式
const formattedInstances = computed(() => {
  return availableInstances.value.map(inst => ({
    id: inst.id,
    name: inst.name,
    status: inst.status,
    instanceType: (inst.instanceType || 'container') as 'vm' | 'container',
    host: {
      id: inst.host?.id ?? 0,
      name: inst.host?.name ?? '',
      location: inst.host?.location ?? null,
      countryCode: inst.host?.country_code ?? 'us'
    }
  }))
})

// Statuses and priorities
const statuses: TicketStatus[] = ['open', 'in_progress', 'resolved', 'closed']
const priorities: TicketPriority[] = ['low', 'normal', 'high', 'urgent']
const categories: TicketCategory[] = ['general', 'billing', 'technical', 'abuse']

const categoryOptions = computed<SelectOption[]>(() => {
  return categories.map(cat => ({
    label: t(`tickets.category.${cat}`),
    value: cat
  }))
})

const priorityOptions = computed<SelectOption[]>(() => {
  return priorities.map(p => ({
    label: t(`tickets.priority.${p}`),
    value: p
  }))
})

function getStatusBadgeVariant(status: TicketStatus): 'warning' | 'info' | 'success' | 'default' {
  switch (status) {
    case 'open':
      return 'warning'
    case 'in_progress':
      return 'info'
    case 'resolved':
      return 'success'
    case 'closed':
    default:
      return 'default'
  }
}

function getPriorityBadgeVariant(priority: TicketPriority): 'error' | 'warning' | 'default' {
  switch (priority) {
    case 'urgent':
      return 'error'
    case 'high':
      return 'warning'
    case 'normal':
    case 'low':
    default:
      return 'default'
  }
}

function getSlaBadgeVariant(slaStatus?: string | null): 'error' | 'warning' | 'success' | 'default' {
  switch (slaStatus) {
    case 'overdue':
      return 'error'
    case 'due_soon':
      return 'warning'
    case 'met':
      return 'success'
    default:
      return 'default'
  }
}

const ticketLinkTypes: TicketObjectLinkType[] = ['recharge_record', 'order_operation_case', 'instance', 'host']

const lightboxImages = computed(() => {
  return messages.value.flatMap(message =>
    message.attachments
      .map(attachment => {
        const src = attachmentObjectUrls.value[attachment.id]
        if (!src) return null
        return {
          id: attachment.id,
          src,
          alt: attachment.originalName || attachment.filename
        }
      })
      .filter((item): item is { id: number; src: string; alt: string } => item !== null)
  )
})

function revokeAttachmentUrls(): void {
  for (const url of Object.values(attachmentObjectUrls.value)) {
    URL.revokeObjectURL(url)
  }
  attachmentObjectUrls.value = {}
  attachmentLoadingIds.value = {}
}

async function ensureAttachmentObjectUrl(attachment: TicketMessageAttachment): Promise<void> {
  if (attachmentObjectUrls.value[attachment.id] || attachmentLoadingIds.value[attachment.id]) {
    return
  }

  attachmentLoadingIds.value = {
    ...attachmentLoadingIds.value,
    [attachment.id]: true
  }

  try {
    const blob = await api.tickets.getAttachmentContent(attachment.id)
    attachmentObjectUrls.value = {
      ...attachmentObjectUrls.value,
      [attachment.id]: URL.createObjectURL(blob)
    }
  } catch (error) {
    console.error('Failed to load ticket attachment:', error)
  } finally {
    const nextLoadingIds = { ...attachmentLoadingIds.value }
    delete nextLoadingIds[attachment.id]
    attachmentLoadingIds.value = nextLoadingIds
  }
}

async function ensureMessageAttachmentUrls(messageList: TicketMessage[]): Promise<void> {
  await Promise.all(messageList.flatMap(message => message.attachments.map(attachment => ensureAttachmentObjectUrl(attachment))))
}

function openAttachmentLightbox(attachmentId: number): void {
  const index = lightboxImages.value.findIndex(image => image.id === attachmentId)
  if (index === -1) return
  lightboxStartIndex.value = index
  lightboxOpen.value = true
}

onBeforeUnmount(() => {
  revokeAttachmentUrls()
})

// 管理员或拥有节点的用户可以查看收到的工单
const isHostOwner = computed(() => authStore.isAdmin || pendingCount.value.isHostOwner)

const hostEmptyState = computed(() => {
  if (activeTab.value !== 'host') {
    return {
      title: t('tickets.noTickets'),
      hint: t('tickets.noTicketsHint')
    }
  }

  if (!authStore.isAdmin) {
    return {
      title: t('tickets.noHostTickets'),
      hint: t('tickets.noHostTicketsHint')
    }
  }

  switch (hostTicketSourceFilter.value) {
    case 'user':
      return {
        title: t('tickets.noUserTickets'),
        hint: t('tickets.noUserTicketsHint')
      }
    case 'official':
      return {
        title: t('tickets.noOfficialTickets'),
        hint: t('tickets.noOfficialTicketsHint')
      }
    default:
      return {
        title: t('tickets.noHostTickets'),
        hint: t('tickets.noHostTicketsHint')
      }
  }
})

const routeStateReady = ref(false)

function getDefaultTab(): TabType {
  return authStore.isAdmin ? 'host' : 'my'
}

function normalizeTab(tab: unknown): TabType {
  return tab === 'host' && isHostOwner.value ? 'host' : 'my'
}

function buildTicketsQuery(options: {
  tab?: TabType
  ticketId?: number | null
  compose?: boolean
}) {
  const query: Record<string, string> = {}
  const targetTab = options.tab ?? activeTab.value

  if (targetTab !== getDefaultTab()) {
    query.tab = targetTab
  }

  if (options.ticketId) {
    query.ticket = String(options.ticketId)
  }

  if (options.compose) {
    query.compose = '1'
  }

  return query
}

function resetDetailState() {
  selectedTicket.value = null
  messages.value = []
  replyContent.value = ''
  replyAttachments.value = []
  messagesPage.value = 1
  messagesTotal.value = 0
  isOwner.value = false
  isCreator.value = false
  supportContext.value = null
  supportContextLoading.value = false
  internalNoteContent.value = ''
  notifyContent.value = ''
  linkForm.value = {
    objectType: 'instance',
    objectId: ''
  }
  lightboxOpen.value = false
  revokeAttachmentUrls()
}

function resetCreateForm() {
  createForm.value = {
    instanceId: null,
    subject: '',
    category: 'general',
    priority: 'normal',
    content: ''
  }
  createAttachments.value = []
}

async function navigateToList(replace = false): Promise<void> {
  const location = {
    path: ticketsPath(),
    query: buildTicketsQuery({ tab: activeTab.value })
  }

  if (replace) {
    await router.replace(location)
  } else {
    await router.push(location)
  }
}

async function openTicketDetail(ticketId: number): Promise<void> {
  viewMode.value = 'detail'
  messagesLoading.value = true
  supportContext.value = null
  messagesPage.value = 1
  revokeAttachmentUrls()
  messages.value = []
  selectedTicket.value = selectedTicket.value?.id === ticketId ? selectedTicket.value : ({ id: ticketId } as Ticket)

  try {
    const [detailRes, messagesRes] = await Promise.all([
      api.tickets.get(ticketId),
      api.tickets.getMessages(ticketId, { page: 1, pageSize: messagesPageSize.value })
    ])
    selectedTicket.value = detailRes.ticket
    isOwner.value = detailRes.isOwner
    isCreator.value = detailRes.isCreator
    messages.value = messagesRes.messages
    messagesTotal.value = messagesRes.total
    await ensureMessageAttachmentUrls(messagesRes.messages)
    if (authStore.isAdmin) {
      await loadSupportContext(ticketId)
    }
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
    await navigateToList(true)
  } finally {
    messagesLoading.value = false
  }
}

async function loadSupportContext(ticketId = selectedTicket.value?.id): Promise<void> {
  if (!authStore.isAdmin || !ticketId) return
  supportContextLoading.value = true
  try {
    supportContext.value = await api.tickets.getSupportContext(ticketId)
    selectedTicket.value = supportContext.value.ticket
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    supportContextLoading.value = false
  }
}

async function openCreateView(): Promise<void> {
  viewMode.value = 'create'
  resetCreateForm()
  if (availableInstances.value.length === 0) {
    await loadAvailableInstances()
  }
}

async function syncViewFromRoute(): Promise<void> {
  const targetTab = normalizeTab(route.query.tab ?? getDefaultTab())
  if (activeTab.value !== targetTab) {
    activeTab.value = targetTab
    await loadTickets(true)
  }

  const ticketIdParam = route.query.ticket
  const compose = route.query.compose === '1'
  const ticketId = typeof ticketIdParam === 'string' ? Number(ticketIdParam) : NaN

  if (Number.isInteger(ticketId) && ticketId > 0) {
    if (viewMode.value !== 'detail' || selectedTicket.value?.id !== ticketId) {
      await openTicketDetail(ticketId)
    }
    return
  }

  if (compose && !authStore.isAdmin) {
    if (viewMode.value !== 'create') {
      await openCreateView()
    }
    return
  }

  if (viewMode.value !== 'list') {
    viewMode.value = 'list'
    resetDetailState()
  }
}

// 管理员默认显示"收到的工单"标签，拥有节点的普通用户也显示但默认显示"我的工单"
onMounted(async () => {
  await loadPendingCount()
  activeTab.value = normalizeTab(route.query.tab ?? getDefaultTab())
  await loadTickets()
  routeStateReady.value = true
  await syncViewFromRoute()
})

watch(
  () => [route.query.tab, route.query.ticket, route.query.compose],
  async () => {
    if (!routeStateReady.value) return
    await syncViewFromRoute()
  }
)

async function loadTickets(silent = false) {
  if (!silent) loading.value = true
  else refreshing.value = true

  try {
    const params: Record<string, unknown> = {
      page: page.value,
      pageSize: pageSize.value
    }

    // 状态筛选：'active' 传递给后端处理，'all' 不传 status
    if (statusFilter.value === 'active') {
      params.status = 'active'
    } else if (statusFilter.value !== 'all') {
      params.status = statusFilter.value
    }

    // 搜索
    if (searchQuery.value.trim()) {
      params.search = searchQuery.value.trim()
    }

    if (hostFilter.value) params.hostId = hostFilter.value
    if (activeTab.value === 'host' && authStore.isAdmin && hostTicketSourceFilter.value !== 'all') {
      params.sourceType = hostTicketSourceFilter.value
    }
    if (activeTab.value === 'host' && authStore.isAdmin && queueFilter.value !== 'all') {
      params.queue = queueFilter.value
    }

    let response
    if (activeTab.value === 'my') {
      response = await api.tickets.list(params as { status?: TicketStatus | 'active'; search?: string; page?: number; pageSize?: number })
    } else {
      response = await api.tickets.getMyHostTickets(params as { status?: TicketStatus | 'active'; hostId?: number; search?: string; page?: number; pageSize?: number })
    }

    tickets.value = response.tickets
    total.value = response.total
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

// 搜索防抖处理
function handleSearchInput() {
  if (searchDebounceTimer) {
    clearTimeout(searchDebounceTimer)
  }
  searchDebounceTimer = setTimeout(() => {
    page.value = 1
    loadTickets(true)
  }, 300)
}

// 分页大小变更
function handlePageSizeChange(newSize: number) {
  pageSize.value = newSize
  page.value = 1
  loadTickets(true)
}

async function loadPendingCount() {
  try {
    pendingCount.value = await api.tickets.getPendingCount()
  } catch (error) {
    console.error('Failed to load pending count:', error)
  }
}

async function switchTab(tab: TabType) {
  page.value = 1
  statusFilter.value = 'active'
  hostFilter.value = ''
  hostTicketSourceFilter.value = 'all'
  queueFilter.value = 'all'
  searchQuery.value = ''
  await router.replace({
    path: ticketsPath(),
    query: buildTicketsQuery({ tab })
  })
}

function handlePageChange(newPage: number) {
  page.value = newPage
  loadTickets(true)
}

watch([statusFilter, hostFilter, hostTicketSourceFilter, queueFilter], () => {
  page.value = 1
  loadTickets(true)
})

watch(hostTicketSourceFilter, (value) => {
  if (value === 'user' && hostFilter.value) {
    hostFilter.value = ''
  }
})

// Detail view
async function viewTicket(ticket: Ticket) {
  await router.push({
    path: ticketsPath(),
    query: buildTicketsQuery({ tab: activeTab.value, ticketId: ticket.id })
  })
}

// Load more messages
async function loadMoreMessages() {
  if (!selectedTicket.value || messagesLoadingMore.value) return
  if (messagesPage.value >= messagesTotalPages.value) return

  messagesLoadingMore.value = true
  try {
    messagesPage.value++
    const res = await api.tickets.getMessages(selectedTicket.value.id, {
      page: messagesPage.value,
      pageSize: messagesPageSize.value
    })
    messages.value.push(...res.messages)
    await ensureMessageAttachmentUrls(res.messages)
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
    messagesPage.value--
  } finally {
    messagesLoadingMore.value = false
  }
}

async function backToList() {
  await navigateToList()
}

// Reply
async function handleReply() {
  if (!selectedTicket.value) return
  if (!replyContent.value.trim() && replyAttachments.value.length === 0) return

  replying.value = true
  try {
    const res = await api.tickets.reply(selectedTicket.value.id, replyContent.value.trim(), replyAttachments.value)
    messages.value.push(res.data)
    messagesTotal.value++
    await ensureMessageAttachmentUrls([res.data])
    replyContent.value = ''
    replyAttachments.value = []
    toast.success(t('tickets.replySuccess'))
    if (authStore.isAdmin) {
      await loadSupportContext()
    }
    // Scroll to bottom
    await nextTick()
    const container = document.getElementById('messages-container')
    if (container) container.scrollTop = container.scrollHeight
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    replying.value = false
  }
}

// Status update
async function updateStatus(status: TicketStatus) {
  if (!selectedTicket.value) return

  try {
    await api.tickets.updateStatus(selectedTicket.value.id, status)
    selectedTicket.value.status = status
    toast.success(t('tickets.statusUpdated'))
    loadPendingCount()
    loadTickets(true)
    if (authStore.isAdmin) {
      await loadSupportContext()
    }
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  }
}

// Close ticket
async function closeTicket() {
  if (!selectedTicket.value) return
  if (!confirm(t('tickets.confirmCloseHint'))) return

  try {
    await api.tickets.close(selectedTicket.value.id)
    selectedTicket.value.status = 'closed'
    toast.success(t('tickets.closeSuccess'))
    loadPendingCount()
    loadTickets(true)
    if (authStore.isAdmin) {
      await loadSupportContext()
    }
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  }
}

// Reopen own closed ticket
async function reopenTicket() {
  if (!selectedTicket.value || !isCreator.value || selectedTicket.value.status !== 'closed') return

  try {
    await api.tickets.updateStatus(selectedTicket.value.id, 'open')
    selectedTicket.value.status = 'open'
    toast.success(t('tickets.reopenSuccess'))
    loadPendingCount()
    loadTickets(true)
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  }
}

// Delete message (仅管理员)
async function confirmDeleteMessage(message: TicketMessage) {
  if (!selectedTicket.value) return
  if (!confirm(t('tickets.confirmDeleteMessage'))) return

  try {
    await api.tickets.deleteMessage(selectedTicket.value.id, message.id)
    // 从列表中移除已删除的消息
    messages.value = messages.value.filter(m => m.id !== message.id)
    messagesTotal.value--
    toast.success(t('tickets.deleteMessageSuccess'))
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  }
}

async function submitInternalNote() {
  if (!selectedTicket.value || !internalNoteContent.value.trim()) return
  internalNoteSubmitting.value = true
  try {
    await api.tickets.createInternalNote(selectedTicket.value.id, internalNoteContent.value.trim())
    internalNoteContent.value = ''
    toast.success(t('tickets.support.noteSaved'))
    await loadSupportContext()
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    internalNoteSubmitting.value = false
  }
}

async function submitNotifyUser() {
  if (!selectedTicket.value || !notifyContent.value.trim()) return
  notifySubmitting.value = true
  try {
    await api.tickets.notifyUser(selectedTicket.value.id, notifyContent.value.trim())
    notifyContent.value = ''
    toast.success(t('tickets.support.noticeSent'))
    await loadSupportContext()
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    notifySubmitting.value = false
  }
}

async function submitLinkObject() {
  if (!selectedTicket.value) return
  const objectId = Number(linkForm.value.objectId)
  if (!Number.isInteger(objectId) || objectId <= 0) {
    toast.error(t('tickets.support.invalidObjectId'))
    return
  }
  linkSubmitting.value = true
  try {
    await api.tickets.linkObject(selectedTicket.value.id, linkForm.value.objectType, objectId)
    linkForm.value.objectId = ''
    toast.success(t('tickets.support.linkSaved'))
    await loadSupportContext()
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    linkSubmitting.value = false
  }
}

function openAdminPath(path: string | null) {
  if (!path) return
  router.push(path)
}

function displayValue(item: Record<string, unknown>, key: string, fallback = '-'): string {
  const value = item[key]
  if (value === null || value === undefined || value === '') return fallback
  return String(value)
}

function displayDateValue(item: Record<string, unknown>, key: string): string {
  const value = item[key]
  return typeof value === 'string' && value ? formatDateShort(value) : '-'
}

// Create ticket
function openCreateForm() {
  if (!configStore.ticketEnabled) return
  router.push({
    path: ticketsPath(),
    query: buildTicketsQuery({ tab: activeTab.value, compose: true })
  })
}

async function loadAvailableInstances() {
  instancesLoading.value = true
  try {
    const response = await api.instances.list({ pageSize: 100 }) as unknown as { instances: InstanceWithDetails[] }
    availableInstances.value = response.instances || []
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    instancesLoading.value = false
  }
}

async function submitCreate() {
  if (!configStore.ticketEnabled) return
  if (createForm.value.subject.trim().length < 2) {
    toast.error(t('tickets.subject') + ' ' + t('common.error'))
    return
  }
  if (createAttachments.value.length === 0 && createForm.value.content.trim().length < 10) {
    toast.error(t('tickets.contentTooShort'))
    return
  }

  creating.value = true
  try {
    const res = await api.tickets.create({
      instanceId: createForm.value.instanceId || undefined,
      subject: createForm.value.subject.trim(),
      category: createForm.value.category,
      priority: createForm.value.priority,
      content: createForm.value.content.trim(),
      attachments: createAttachments.value
    })
    toast.success(t('tickets.createSuccess'))
    activeTab.value = 'my'
    await Promise.all([loadTickets(), loadPendingCount()])
    await router.replace({
      path: ticketsPath(),
      query: buildTicketsQuery({ tab: 'my', ticketId: res.ticket.id })
    })
  } catch (error: any) {
    toast.error(error?.message || t('common.error'))
  } finally {
    creating.value = false
  }
}

function formatDate(dateString: string) {
  const date = new Date(dateString)
  return date.toLocaleString()
}

function formatDateShort(dateString: string) {
  const date = new Date(dateString)
  const now = new Date()
  const diff = now.getTime() - date.getTime()
  const days = Math.floor(diff / (1000 * 60 * 60 * 24))

  if (days === 0) {
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  } else if (days < 7) {
    return t('inbox.daysAgo', { n: days })
  } else {
    return date.toLocaleDateString()
  }
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 pb-12 animate-fade-in max-w-6xl mx-auto px-4 sm:px-6 md:px-8">
    <!-- Header -->
    <PageHeader
      :title="viewMode === 'detail' ? t('tickets.ticketDetails') : viewMode === 'create' ? t('tickets.newTicket') : t('tickets.title')"
      :description="t('tickets.subtitle')"
      :back="viewMode !== 'list'"
      :back-label="t('common.back')"
      @back="backToList"
    >
      <template #actions>
        <!-- 管理员不显示"创建工单"按钮 -->
        <Button
          v-if="viewMode === 'list' && !authStore.isAdmin && configStore.ticketEnabled"
          variant="primary"
          @click="openCreateForm"
        >
          <template #icon>
            <Plus class="w-4 h-4" />
          </template>
          {{ t('tickets.createTicket') }}
        </Button>
      </template>
    </PageHeader>

    <!-- List View -->
    <template v-if="viewMode === 'list'">
      <!-- Tabs -->
      <div v-if="!authStore.isAdmin || isHostOwner" data-reveal class="flex items-center gap-2 mb-4">
        <div class="inline-flex items-center gap-1 p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <!-- 普通用户显示"我的工单"标签，管理员不显示 -->
          <button
            v-if="!authStore.isAdmin"
            type="button"
            class="inline-flex items-center gap-2 px-4 py-1.5 text-xs font-medium rounded-full transition-all duration-200 active:scale-95"
            :class="activeTab === 'my'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('my')"
          >
            <span>{{ t('tickets.myTickets') }}</span>
            <span
              v-if="pendingCount.userTickets > 0"
              class="inline-flex items-center justify-center px-1.5 py-0.5 text-[10px] font-bold rounded-full bg-[var(--accent)] text-white min-w-[18px] h-[18px]"
            >
              {{ pendingCount.userTickets }}
            </span>
          </button>
          <button
            v-if="isHostOwner"
            type="button"
            class="inline-flex items-center gap-2 px-4 py-1.5 text-xs font-medium rounded-full transition-all duration-200 active:scale-95"
            :class="activeTab === 'host'
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('host')"
          >
            <span>{{ t('tickets.hostTickets') }}</span>
            <span
              v-if="pendingCount.hostTickets > 0"
              class="inline-flex items-center justify-center px-1.5 py-0.5 text-[10px] font-bold rounded-full bg-[var(--warning)] text-white min-w-[18px] h-[18px]"
            >
              {{ pendingCount.hostTickets }}
            </span>
          </button>
        </div>
      </div>

      <!-- Filters & Search Row -->
      <div data-reveal class="flex flex-col sm:flex-row sm:flex-wrap sm:items-center justify-between gap-3 mb-6">
        <div class="flex flex-wrap items-center gap-2.5">
          <!-- Status Filter - Apple 胶囊分段组件 -->
          <div class="inline-flex items-center gap-1 p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
            <button
              type="button"
              :class="[
                'px-3.5 py-1.5 text-xs font-medium rounded-full transition-all duration-200 whitespace-nowrap active:scale-95',
                statusFilter === 'active'
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'
              ]"
              @click="statusFilter = 'active'"
            >
              {{ t('tickets.activeStatus') }}
            </button>
            <button
              v-for="status in statuses"
              :key="status"
              type="button"
              :class="[
                'px-3.5 py-1.5 text-xs font-medium rounded-full transition-all duration-200 whitespace-nowrap active:scale-95',
                statusFilter === status
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'
              ]"
              @click="statusFilter = status"
            >
              {{ t(`tickets.status.${status}`) }}
            </button>
            <button
              type="button"
              :class="[
                'px-3.5 py-1.5 text-xs font-medium rounded-full transition-all duration-200 whitespace-nowrap active:scale-95',
                statusFilter === 'all'
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'
              ]"
              @click="statusFilter = 'all'"
            >
              {{ t('tickets.allStatus') }}
            </button>
          </div>

          <!-- Source Filter (Host tab & Admin) -->
          <div v-if="activeTab === 'host' && authStore.isAdmin" class="inline-flex items-center gap-1 p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
            <button
              v-for="sourceType in ['all', 'user', 'official']"
              :key="sourceType"
              type="button"
              :class="[
                'px-3.5 py-1.5 text-xs font-medium rounded-full transition-all duration-200 whitespace-nowrap active:scale-95',
                hostTicketSourceFilter === sourceType
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'
              ]"
              @click="hostTicketSourceFilter = sourceType as HostTicketSourceType"
            >
              {{ t(`tickets.sourceFilter.${sourceType}`) }}
            </button>
          </div>

          <!-- Queue Filter (Host tab & Admin) -->
          <div v-if="activeTab === 'host' && authStore.isAdmin" class="inline-flex items-center gap-1 p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
            <button
              v-for="queue in ['all', 'pending', 'due_soon', 'overdue', 'waiting_user', 'waiting_internal']"
              :key="queue"
              type="button"
              :class="[
                'px-3.5 py-1.5 text-xs font-medium rounded-full transition-all duration-200 whitespace-nowrap active:scale-95',
                queueFilter === queue
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'
              ]"
              @click="queueFilter = queue as TicketQueueFilter"
            >
              {{ t(`tickets.support.queue.${queue}`) }}
            </button>
          </div>
        </div>

        <!-- Search Box -->
        <div class="w-full sm:w-64">
          <Input
            v-model="searchQuery"
            size="sm"
            clearable
            :placeholder="t('tickets.searchPlaceholder')"
            @input="handleSearchInput"
            @clear="searchQuery = ''; handleSearchInput()"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
      </div>

      <!-- Loading State -->
      <template v-if="loading">
        <SkeletonLoader type="list" :count="5" />
      </template>

      <!-- Empty State -->
      <template v-else-if="tickets.length === 0">
        <Card padding="none">
          <EmptyState
            :title="activeTab === 'my' ? t('tickets.noTickets') : hostEmptyState.title"
            :description="activeTab === 'my' ? t('tickets.noTicketsHint') : hostEmptyState.hint"
          >
            <template #action>
              <Button
                v-if="activeTab === 'my' && !authStore.isAdmin && configStore.ticketEnabled"
                variant="primary"
                size="sm"
                @click="openCreateForm"
              >
                <template #icon>
                  <Plus class="w-4 h-4" />
                </template>
                {{ t('tickets.createTicket') }}
              </Button>
            </template>
          </EmptyState>
        </Card>
      </template>

      <!-- Ticket List -->
      <template v-else>
        <div class="space-y-3">
          <Card
            v-for="ticket in tickets"
            :key="ticket.id"
            hoverable
            padding="none"
            class="group cursor-pointer border transition-all duration-300"
            :class="ticket.needsReply ? 'border-[var(--warning)] ring-1 ring-[var(--warning)]/30' : ''"
            @click="viewTicket(ticket)"
          >
            <div class="p-4 sm:p-5 flex flex-col sm:flex-row sm:items-center justify-between gap-4">
              <div class="flex items-start gap-3.5 min-w-0 flex-1">
                <!-- User avatar (for host tab) -->
                <UserAvatar
                  v-if="activeTab === 'host' && ticket.user"
                  :username="ticket.user.username"
                  :avatar-style="ticket.user.avatarStyle"
                  :badge-id="ticket.user.avatarBadgeId || null"
                  :size="36"
                  class="shrink-0 mt-0.5"
                />

                <div class="min-w-0 flex-1">
                  <!-- Badges & Ticket ID -->
                  <div class="flex flex-wrap items-center gap-1.5 mb-1.5">
                    <Badge v-if="ticket.needsReply" variant="warning" size="sm" dot>
                      {{ t('tickets.needsReply') }}
                    </Badge>
                    <Badge :variant="getStatusBadgeVariant(ticket.status)" size="sm" dot>
                      {{ t(`tickets.status.${ticket.status}`) }}
                    </Badge>
                    <Badge :variant="getPriorityBadgeVariant(ticket.priority)" size="sm">
                      {{ t(`tickets.priority.${ticket.priority}`) }}
                    </Badge>
                    <Badge v-if="authStore.isAdmin && ticket.slaStatus" :variant="getSlaBadgeVariant(ticket.slaStatus)" size="sm" dot>
                      {{ t(`tickets.support.slaStatus.${ticket.slaStatus}`) }}
                    </Badge>
                    <span class="text-xs text-[var(--text-tertiary)] font-mono ml-1">
                      #{{ ticket.id }}
                    </span>
                  </div>

                  <!-- Subject -->
                  <h3 class="text-sm sm:text-base font-semibold tracking-tight text-[var(--text-primary)] group-hover:text-[var(--accent)] transition-colors truncate">
                    {{ ticket.subject }}
                  </h3>

                  <!-- Metadata -->
                  <div class="flex flex-wrap items-center gap-2 sm:gap-3 mt-1.5 text-xs text-[var(--text-secondary)]">
                    <span v-if="activeTab === 'host' && ticket.user" class="inline-flex items-center gap-1">
                      <User class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                      <span>{{ ticket.user.username }}</span>
                    </span>
                    <span v-if="ticket.host" class="inline-flex items-center gap-1">
                      <Server class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                      <span>{{ ticket.host.name }}</span>
                    </span>
                    <span v-if="ticket.instance" class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)]">
                      <InstanceDisplayIcon
                        v-if="ticket.instance.iconBadgeId"
                        :badge-id="ticket.instance.iconBadgeId"
                        :alt="ticket.instance.name"
                        :size="14"
                      />
                      <span class="font-medium">{{ ticket.instance.name }}</span>
                    </span>
                  </div>
                </div>
              </div>

              <!-- Right Meta Info -->
              <div class="flex sm:flex-col items-center sm:items-end justify-between sm:justify-center gap-2 shrink-0 border-t sm:border-t-0 pt-2 sm:pt-0 border-[var(--border-color)]">
                <span class="font-mono text-xs text-[var(--text-tertiary)]">
                  {{ formatDateShort(ticket.createdAt) }}
                </span>
                <div class="flex items-center gap-2">
                  <span v-if="ticket.messageCount" class="inline-flex items-center gap-1 font-mono text-xs text-[var(--text-secondary)] bg-[var(--bg-surface-soft)] px-2 py-0.5 rounded-full border border-[var(--border-color)]">
                    <MessageSquare class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                    <span>{{ ticket.messageCount }}</span>
                  </span>
                  <ChevronRight class="w-4 h-4 text-[var(--text-tertiary)] group-hover:translate-x-1 group-hover:text-[var(--accent)] transition-all" />
                </div>
              </div>
            </div>
          </Card>
        </div>

        <!-- Pagination -->
        <div class="mt-6">
          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            :show-size-changer="true"
            :page-size-options="pageSizeOptions"
            @update:page="handlePageChange"
            @update:page-size="handlePageSizeChange"
          >
            <template #total="{ total: totalCount }">
              {{ t('tickets.totalCount', { count: totalCount }) }}
            </template>
          </Pagination>
        </div>
      </template>
    </template>

    <!-- Detail View -->
    <template v-else-if="viewMode === 'detail' && selectedTicket">
      <!-- Ticket Header Info Card -->
      <Card padding="none" class="mb-6">
        <div class="p-5 sm:p-6">
          <div class="flex flex-col md:flex-row md:items-start justify-between gap-4">
            <div class="min-w-0 flex-1">
              <!-- Badges -->
              <div class="flex flex-wrap items-center gap-2 mb-2">
                <Badge :variant="getStatusBadgeVariant(selectedTicket.status)" size="sm" dot>
                  {{ t(`tickets.status.${selectedTicket.status}`) }}
                </Badge>
                <Badge :variant="getPriorityBadgeVariant(selectedTicket.priority)" size="sm">
                  {{ t(`tickets.priority.${selectedTicket.priority}`) }}
                </Badge>
                <Badge variant="default" size="sm">
                  {{ t(`tickets.category.${selectedTicket.category}`) }}
                </Badge>
                <Badge v-if="authStore.isAdmin && selectedTicket.slaStatus" :variant="getSlaBadgeVariant(selectedTicket.slaStatus)" size="sm" dot>
                  {{ t(`tickets.support.slaStatus.${selectedTicket.slaStatus}`) }}
                </Badge>
              </div>

              <!-- Subject & ID -->
              <div class="flex items-center gap-3 mb-3">
                <h2 class="text-xl sm:text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ selectedTicket.subject }}
                </h2>
                <span class="text-xs text-[var(--text-tertiary)] font-mono">#{{ selectedTicket.id }}</span>
              </div>

              <!-- Meta info -->
              <div class="flex flex-wrap items-center gap-4 text-xs sm:text-sm text-[var(--text-secondary)]">
                <span v-if="selectedTicket.user" class="inline-flex items-center gap-2">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.from') }}:</span>
                  <UserAvatar
                    :username="selectedTicket.user.username"
                    :avatar-style="selectedTicket.user.avatarStyle"
                    :badge-id="selectedTicket.user.avatarBadgeId || null"
                    :size="22"
                  />
                  <span class="font-medium text-[var(--text-primary)]">{{ selectedTicket.user.username }}</span>
                </span>
                <span v-if="selectedTicket.host" class="inline-flex items-center gap-1.5">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.host') }}:</span>
                  <span class="font-medium text-[var(--text-primary)]">{{ selectedTicket.host.name }}</span>
                </span>
                <span v-if="selectedTicket.instance" class="inline-flex items-center gap-1.5">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.instance') }}:</span>
                  <InstanceDisplayIcon
                    v-if="selectedTicket.instance.iconBadgeId"
                    :badge-id="selectedTicket.instance.iconBadgeId"
                    :alt="selectedTicket.instance.name"
                    :size="20"
                  />
                  <span class="font-medium text-[var(--text-primary)]">{{ selectedTicket.instance.name }}</span>
                </span>
                <span class="inline-flex items-center gap-1">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.createdAt') }}:</span>
                  <span class="font-mono">{{ formatDate(selectedTicket.createdAt) }}</span>
                </span>
                <span v-if="authStore.isAdmin && selectedTicket.firstResponseDueAt" class="inline-flex items-center gap-1">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.support.firstResponseDue') }}:</span>
                  <span class="font-mono text-[var(--warning)]">{{ formatDate(selectedTicket.firstResponseDueAt) }}</span>
                </span>
                <span v-if="authStore.isAdmin && selectedTicket.resolutionDueAt" class="inline-flex items-center gap-1">
                  <span class="text-[var(--text-tertiary)]">{{ t('tickets.support.resolutionDue') }}:</span>
                  <span class="font-mono text-[var(--warning)]">{{ formatDate(selectedTicket.resolutionDueAt) }}</span>
                </span>
              </div>
            </div>

            <!-- Actions -->
            <div class="flex items-center gap-2 shrink-0 flex-wrap">
              <template v-if="isOwner && selectedTicket.status !== 'closed'">
                <Button v-if="selectedTicket.status === 'open'" variant="secondary" size="sm" @click="updateStatus('in_progress')">
                  {{ t('tickets.markInProgress') }}
                </Button>
                <Button v-if="selectedTicket.status !== 'resolved'" variant="secondary" size="sm" @click="updateStatus('resolved')">
                  {{ t('tickets.markResolved') }}
                </Button>
                <Button variant="danger" size="sm" @click="closeTicket">
                  {{ t('tickets.close') }}
                </Button>
              </template>
              <template v-else-if="isCreator && selectedTicket.status !== 'closed'">
                <Button variant="danger" size="sm" @click="closeTicket">
                  {{ t('tickets.close') }}
                </Button>
              </template>
              <template v-else-if="isCreator && selectedTicket.status === 'closed'">
                <Button variant="secondary" size="sm" @click="reopenTicket">
                  <template #icon>
                    <RotateCcw class="w-3.5 h-3.5" />
                  </template>
                  {{ t('tickets.reopen') }}
                </Button>
              </template>
            </div>
          </div>

          <!-- Instance owner card if applicable -->
          <div v-if="isOwner && selectedTicket.instance" class="mt-4 pt-4 border-t border-[var(--border-color)]">
            <TicketInstanceOwnerCard :instance-summary="selectedTicket.instance" />
          </div>
        </div>
      </Card>

      <!-- Admin Support Success Center -->
      <Card v-if="authStore.isAdmin" padding="none" class="mb-6 bg-[var(--bg-surface-soft)]">
        <div class="p-4 sm:p-6">
          <div v-if="supportContextLoading" class="py-4">
            <SkeletonLoader type="list" :count="2" />
          </div>
          <div v-else-if="supportContext" class="grid gap-4 lg:grid-cols-[1.2fr_1fr]">
            <!-- User Context -->
            <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 sm:p-5 shadow-sm">
              <div class="flex items-center justify-between gap-3 mb-3">
                <h3 class="font-semibold text-sm sm:text-base text-[var(--text-primary)]">
                  {{ t('tickets.support.userContext') }}
                </h3>
                <button
                  type="button"
                  class="text-xs text-[var(--accent)] hover:underline inline-flex items-center gap-1 font-medium transition-colors"
                  @click="openAdminPath(supportContext.quickActions.userPath)"
                >
                  <span>{{ t('tickets.support.openUser') }}</span>
                  <ExternalLink class="w-3 h-3" />
                </button>
              </div>

              <div v-if="supportContext.userContext" class="grid gap-3 sm:grid-cols-2 text-xs sm:text-sm">
                <div>
                  <p class="text-[var(--text-tertiary)]">{{ t('tickets.support.account') }}</p>
                  <p class="font-medium text-[var(--text-primary)] mt-0.5">
                    {{ supportContext.userContext.username }} · {{ supportContext.userContext.status }}
                  </p>
                  <p class="text-[var(--text-tertiary)]">{{ supportContext.userContext.emailMasked || '-' }}</p>
                </div>
                <div>
                  <p class="text-[var(--text-tertiary)]">{{ t('tickets.support.balance') }}</p>
                  <p class="font-semibold text-[var(--text-primary)] font-mono mt-0.5">
                    ¥{{ supportContext.userContext.balance || '0.00' }}
                  </p>
                  <p class="text-[var(--text-tertiary)]">
                    {{ t('tickets.support.registeredAt') }} <span class="font-mono">{{ formatDateShort(supportContext.userContext.createdAt) }}</span>
                  </p>
                </div>
                <div class="sm:col-span-2 grid grid-cols-2 sm:grid-cols-4 gap-2 pt-2">
                  <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-center">
                    <p class="text-[11px] text-[var(--text-secondary)]">{{ t('tickets.support.instances') }}</p>
                    <p class="font-semibold font-mono text-[var(--text-primary)] text-sm mt-0.5">{{ supportContext.userContext.counts.instances }}</p>
                  </div>
                  <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-center">
                    <p class="text-[11px] text-[var(--text-secondary)]">{{ t('tickets.support.tickets') }}</p>
                    <p class="font-semibold font-mono text-[var(--text-primary)] text-sm mt-0.5">{{ supportContext.userContext.counts.ticketsCreated }}</p>
                  </div>
                  <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-center">
                    <p class="text-[11px] text-[var(--text-secondary)]">{{ t('tickets.support.orders') }}</p>
                    <p class="font-semibold font-mono text-[var(--text-primary)] text-sm mt-0.5">{{ supportContext.userContext.counts.rechargeRecords }}</p>
                  </div>
                  <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-center">
                    <p class="text-[11px] text-[var(--text-secondary)]">{{ t('tickets.support.ledger') }}</p>
                    <p class="font-semibold font-mono text-[var(--text-primary)] text-sm mt-0.5">{{ supportContext.userContext.counts.balanceLogs }}</p>
                  </div>
                </div>
              </div>

              <div class="mt-4 grid gap-3 md:grid-cols-2 pt-3 border-t border-[var(--border-color)]">
                <div>
                  <h4 class="text-xs font-semibold text-[var(--text-primary)] mb-2">{{ t('tickets.support.recentOrders') }}</h4>
                  <div class="space-y-2">
                    <div
                      v-for="order in supportContext.recentOrders"
                      :key="displayValue(order, 'id')"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5 text-xs"
                    >
                      <div class="flex justify-between gap-2">
                        <span class="font-medium text-[var(--text-primary)] font-mono">{{ displayValue(order, 'orderNo') }}</span>
                        <span class="text-[var(--text-secondary)]">{{ displayValue(order, 'status') }}</span>
                      </div>
                      <p class="text-[var(--text-secondary)] mt-1">
                        <span class="font-mono">¥{{ displayValue(order, 'amount') }}</span> · <span class="font-mono">{{ displayDateValue(order, 'createdAt') }}</span>
                      </p>
                    </div>
                    <p v-if="supportContext.recentOrders.length === 0" class="text-xs text-[var(--text-tertiary)] py-1">{{ t('common.noData') }}</p>
                  </div>
                </div>
                <div>
                  <h4 class="text-xs font-semibold text-[var(--text-primary)] mb-2">{{ t('tickets.support.recentInstances') }}</h4>
                  <div class="space-y-2">
                    <div
                      v-for="instance in supportContext.recentInstances"
                      :key="displayValue(instance, 'id')"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5 text-xs"
                    >
                      <div class="flex justify-between gap-2">
                        <span class="font-medium text-[var(--text-primary)]">{{ displayValue(instance, 'name') }}</span>
                        <span class="text-[var(--text-secondary)]">{{ displayValue(instance, 'status') }}</span>
                      </div>
                      <p class="text-[var(--text-secondary)] mt-1">
                        <span class="font-mono">#{{ displayValue(instance, 'id') }}</span> · <span class="font-mono">{{ displayDateValue(instance, 'createdAt') }}</span>
                      </p>
                    </div>
                    <p v-if="supportContext.recentInstances.length === 0" class="text-xs text-[var(--text-tertiary)] py-1">{{ t('common.noData') }}</p>
                  </div>
                </div>
              </div>
            </div>

            <!-- Quick actions & Links column -->
            <div class="space-y-4">
              <!-- Quick actions -->
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 sm:p-5 shadow-sm">
                <h3 class="font-semibold text-sm sm:text-base text-[var(--text-primary)] mb-3">
                  {{ t('tickets.support.quickActions') }}
                </h3>
                <div class="grid grid-cols-2 gap-2">
                  <Button variant="secondary" size="sm" @click="openAdminPath(supportContext.quickActions.balanceAdjustmentPath)">
                    {{ t('tickets.support.adjustment') }}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    :disabled="!supportContext.quickActions.instancePath"
                    @click="openAdminPath(supportContext.quickActions.instancePath)"
                  >
                    {{ t('tickets.support.openInstance') }}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    :disabled="!supportContext.quickActions.hostPath"
                    class="col-span-2"
                    @click="openAdminPath(supportContext.quickActions.hostPath)"
                  >
                    {{ t('tickets.support.openHost') }}
                  </Button>
                </div>
                <div class="mt-4 space-y-2 pt-3 border-t border-[var(--border-color)]">
                  <textarea
                    v-model="notifyContent"
                    rows="2"
                    :placeholder="t('tickets.support.notifyPlaceholder')"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-none transition-all"
                  />
                  <Button
                    variant="primary"
                    size="sm"
                    block
                    :loading="notifySubmitting"
                    :disabled="notifySubmitting || !notifyContent.trim()"
                    @click="submitNotifyUser"
                  >
                    {{ notifySubmitting ? t('common.sending') : t('tickets.support.sendNotice') }}
                  </Button>
                </div>
              </div>

              <!-- Linked Objects -->
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 sm:p-5 shadow-sm">
                <h3 class="font-semibold text-sm sm:text-base text-[var(--text-primary)] mb-3">
                  {{ t('tickets.support.linkedObjects') }}
                </h3>
                <div class="flex gap-2">
                  <select
                    v-model="linkForm.objectType"
                    class="h-9 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 cursor-pointer w-32 shrink-0"
                  >
                    <option v-for="type in ticketLinkTypes" :key="type" :value="type">
                      {{ t(`tickets.support.linkType.${type}`) }}
                    </option>
                  </select>
                  <input
                    v-model="linkForm.objectId"
                    type="number"
                    min="1"
                    class="h-9 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 flex-1 min-w-0"
                    :placeholder="t('tickets.support.objectId')"
                  />
                  <Button
                    variant="primary"
                    size="sm"
                    :loading="linkSubmitting"
                    :disabled="linkSubmitting"
                    @click="submitLinkObject"
                  >
                    {{ t('tickets.support.addLink') }}
                  </Button>
                </div>
                <div class="mt-3 space-y-2">
                  <div
                    v-for="link in supportContext.links"
                    :key="link.id"
                    class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-2 text-xs flex items-center justify-between"
                  >
                    <span class="font-medium text-[var(--text-primary)]">{{ t(`tickets.support.linkType.${link.objectType}`) }}</span>
                    <span class="text-[var(--text-secondary)] font-mono"> · {{ link.objectLabel || `#${link.objectId}` }}</span>
                  </div>
                  <p v-if="supportContext.links.length === 0" class="text-xs text-[var(--text-tertiary)] py-1">{{ t('common.noData') }}</p>
                </div>
              </div>
            </div>

            <!-- Knowledge, Internal Notes, Timeline (3 cols) -->
            <div class="lg:col-span-2 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 sm:p-5 shadow-sm">
              <div class="grid gap-6 lg:grid-cols-3">
                <!-- Knowledge -->
                <div>
                  <h3 class="font-semibold text-sm text-[var(--text-primary)] mb-3">
                    {{ t('tickets.support.knowledge') }}
                  </h3>
                  <div class="space-y-3">
                    <div
                      v-for="item in supportContext.knowledgeSuggestions"
                      :key="item.title"
                      class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-xs"
                    >
                      <p class="font-semibold text-[var(--text-primary)]">{{ item.title }}</p>
                      <ul class="mt-2 space-y-1 text-[var(--text-secondary)]">
                        <li v-for="step in item.steps" :key="step">• {{ step }}</li>
                      </ul>
                    </div>
                  </div>
                </div>

                <!-- Internal Notes (tickets.support.internalNotes) -->
                <div>
                  <h3 class="font-semibold text-sm text-[var(--text-primary)] mb-3">
                    {{ t('tickets.support.internalNotes') }}
                  </h3>
                  <textarea
                    v-model="internalNoteContent"
                    rows="3"
                    :placeholder="t('tickets.support.notePlaceholder')"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-2.5 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-none transition-all"
                  />
                  <Button
                    variant="primary"
                    size="sm"
                    block
                    class="mt-2"
                    :loading="internalNoteSubmitting"
                    :disabled="internalNoteSubmitting || !internalNoteContent.trim()"
                    @click="submitInternalNote"
                  >
                    {{ internalNoteSubmitting ? t('common.saving') : t('tickets.support.saveNote') }}
                  </Button>
                  <div class="mt-3 max-h-48 overflow-y-auto space-y-2 pr-1">
                    <div
                      v-for="note in supportContext.internalNotes"
                      :key="note.id"
                      class="rounded-xl bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--warning)_25%,var(--bg-surface))] p-2.5 text-xs"
                    >
                      <p class="text-[var(--text-primary)] whitespace-pre-wrap leading-relaxed">{{ note.content }}</p>
                      <p class="mt-1.5 text-[11px] text-[var(--text-secondary)] flex items-center justify-between">
                        <span class="font-medium">{{ note.actorUsername }}</span>
                        <span class="font-mono text-[var(--text-tertiary)]">{{ formatDateShort(note.createdAt) }}</span>
                      </p>
                    </div>
                    <p v-if="supportContext.internalNotes.length === 0" class="text-xs text-[var(--text-tertiary)] py-1">{{ t('common.noData') }}</p>
                  </div>
                </div>

                <!-- Timeline (tickets.support.timeline) -->
                <div>
                  <h3 class="font-semibold text-sm text-[var(--text-primary)] mb-3">
                    {{ t('tickets.support.timeline') }}
                  </h3>
                  <div class="max-h-80 overflow-y-auto space-y-2 pr-1">
                    <div
                      v-for="item in supportContext.timeline"
                      :key="`${item.type}-${item.id}-${item.createdAt}`"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5 text-xs"
                    >
                      <div class="flex justify-between items-start gap-2">
                        <span class="font-semibold text-[var(--text-primary)]">{{ item.title }}</span>
                        <span class="text-[11px] text-[var(--text-tertiary)] font-mono shrink-0">{{ formatDateShort(item.createdAt) }}</span>
                      </div>
                      <p class="text-[11px] text-[var(--text-secondary)] mt-0.5">{{ item.actor }}</p>
                      <p class="mt-1 text-[var(--text-secondary)] line-clamp-2 leading-relaxed">{{ item.content }}</p>
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </Card>

      <!-- Messages Stream & Reply Box -->
      <Card padding="none" class="overflow-hidden">
        <!-- Messages Header -->
        <div class="p-4 sm:p-5 border-b border-[var(--border-color)] flex items-center justify-between">
          <div class="flex items-center gap-2">
            <MessageSquare class="w-4 h-4 text-[var(--text-tertiary)]" />
            <h3 class="font-semibold text-sm sm:text-base text-[var(--text-primary)]">
              {{ t('tickets.ticketDetails') }}
            </h3>
            <span v-if="messagesTotal > 0" class="text-xs font-mono text-[var(--text-tertiary)]">
              ({{ messagesTotal }})
            </span>
          </div>

          <!-- Load more messages button -->
          <Button
            v-if="messagesTotalPages > 1 && messagesPage < messagesTotalPages"
            variant="secondary"
            size="sm"
            :loading="messagesLoadingMore"
            @click="loadMoreMessages"
          >
            {{ t('tickets.loadMoreMessages') }} ({{ messagesTotal - messages.length }} {{ t('tickets.remaining') }})
          </Button>
        </div>

        <!-- Messages scroll container -->
        <div id="messages-container" class="max-h-[520px] overflow-y-auto p-4 sm:p-6 space-y-6">
          <template v-if="messagesLoading">
            <SkeletonLoader type="list" :count="3" />
          </template>
          <template v-else>
            <div
              v-for="message in messages"
              :key="message.id"
              class="flex gap-3"
              :class="message.senderId === authStore.user?.id ? 'flex-row-reverse' : ''"
            >
              <UserAvatar
                :username="message.sender?.username || ''"
                :avatar-style="message.sender?.avatarStyle"
                :badge-id="message.sender?.avatarBadgeId || null"
                :size="34"
                class="shrink-0 mt-0.5"
              />

              <div
                class="flex-1 max-w-[85%] sm:max-w-[75%]"
                :class="message.senderId === authStore.user?.id ? 'text-right' : ''"
              >
                <!-- Message sender header -->
                <div
                  class="flex items-center gap-2 mb-1.5"
                  :class="message.senderId === authStore.user?.id ? 'justify-end' : ''"
                >
                  <span class="font-medium text-xs text-[var(--text-primary)]">
                    {{ message.sender?.username }}
                  </span>
                  <span
                    v-if="message.isFromOwner"
                    class="text-[11px] font-medium text-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] px-2 py-0.5 rounded-full"
                  >
                    {{ t('tickets.ownerReply') }}
                  </span>
                  <span class="text-[11px] text-[var(--text-tertiary)] font-mono">
                    {{ formatDate(message.createdAt) }}
                  </span>
                  <!-- Admin delete button -->
                  <button
                    v-if="authStore.isAdmin"
                    type="button"
                    class="p-1 text-[var(--text-tertiary)] hover:text-[var(--error)] transition-colors rounded-full hover:bg-[var(--bg-surface-soft)]"
                    :title="t('tickets.deleteMessage')"
                    @click="confirmDeleteMessage(message)"
                  >
                    <Trash2 class="w-3.5 h-3.5 stroke-[1.5]" />
                  </button>
                </div>

                <!-- Message Bubble -->
                <div
                  class="inline-block max-w-full rounded-2xl px-4 py-3 text-sm transition-all duration-200"
                  :class="message.senderId === authStore.user?.id
                    ? 'bg-[var(--accent)] text-white rounded-tr-sm shadow-sm'
                    : 'bg-[var(--bg-surface-soft)] text-[var(--text-primary)] border border-[var(--border-color)] rounded-tl-sm'"
                >
                  <p v-if="message.content" class="whitespace-pre-wrap text-left leading-relaxed">
                    {{ message.content }}
                  </p>

                  <!-- Attachments Grid -->
                  <div
                    v-if="message.attachments.length > 0"
                    class="grid gap-2"
                    :class="[
                      message.content ? 'mt-3 pt-3 border-t' : '',
                      message.senderId === authStore.user?.id ? 'border-white/20' : 'border-[var(--border-color)]',
                      message.attachments.length === 1 ? 'grid-cols-1 max-w-xs' : 'grid-cols-2'
                    ]"
                  >
                    <button
                      v-for="attachment in message.attachments"
                      :key="attachment.id"
                      type="button"
                      class="group relative overflow-hidden rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] transition-all hover:opacity-90 active:scale-95"
                      :title="attachment.originalName"
                      @click="openAttachmentLightbox(attachment.id)"
                    >
                      <img
                        v-if="attachmentObjectUrls[attachment.id]"
                        :src="attachmentObjectUrls[attachment.id]"
                        :alt="attachment.originalName"
                        class="h-32 w-full object-cover group-hover:scale-105 transition-transform duration-300"
                      />
                      <div v-else class="flex h-32 w-full items-center justify-center text-xs text-[var(--text-tertiary)]">
                        {{ attachmentLoadingIds[attachment.id] ? t('common.loading') : t('tickets.images.loadFailed') }}
                      </div>
                    </button>
                  </div>
                </div>
              </div>
            </div>
          </template>
        </div>

        <!-- Reply Footer Section -->
        <div class="p-4 sm:p-6 border-t border-[var(--border-color)] bg-[var(--bg-surface)]">
          <template v-if="selectedTicket.status === 'closed'">
            <div class="text-center py-4">
              <p class="text-sm text-[var(--text-secondary)] mb-3">
                {{ t('tickets.ticketClosed') }}
              </p>
              <Button
                v-if="isCreator && selectedTicket.status === 'closed'"
                variant="secondary"
                size="sm"
                @click="reopenTicket"
              >
                <template #icon>
                  <RotateCcw class="w-3.5 h-3.5" />
                </template>
                {{ t('tickets.reopen') }}
              </Button>
            </div>
          </template>
          <template v-else>
            <div class="space-y-3">
              <textarea
                id="ticket-reply-textarea"
                v-model="replyContent"
                :placeholder="t('tickets.replyPlaceholder')"
                rows="3"
                class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-none transition-all duration-200"
              />

              <TicketImageUploader v-model="replyAttachments" :disabled="replying" />

              <div class="flex items-center justify-between pt-2">
                <p class="text-xs text-[var(--text-tertiary)]">
                  {{ replyAttachments.length > 0 ? `${replyAttachments.length} 个附件已添加` : '' }}
                </p>
                <Button
                  variant="primary"
                  :loading="replying"
                  :disabled="replying || (!replyContent.trim() && replyAttachments.length === 0)"
                  @click="handleReply"
                >
                  <template #icon>
                    <Send class="w-4 h-4" />
                  </template>
                  {{ replying ? t('common.sending') : t('tickets.reply') }}
                </Button>
              </div>
            </div>
          </template>
        </div>
      </Card>
    </template>

    <!-- Create View -->
    <template v-else-if="viewMode === 'create'">
      <Card padding="lg" class="max-w-3xl mx-auto shadow-sm">
        <form class="space-y-6" @submit.prevent="submitCreate">
          <!-- Instance -->
          <div>
            <label class="block text-xs font-semibold text-[var(--text-secondary)] tracking-wide mb-2 select-none">
              {{ t('tickets.selectInstance') }}
            </label>
            <InstanceSelector
              v-model="createForm.instanceId"
              :instances="formattedInstances"
              :placeholder="t('tickets.selectInstanceHint')"
            />
            <p v-if="availableInstances.length === 0 && !instancesLoading" class="mt-2 text-xs text-[var(--text-tertiary)]">
              {{ t('tickets.noInstancesHint') }}
            </p>
          </div>

          <!-- Subject -->
          <div>
            <label class="block text-xs font-semibold text-[var(--text-secondary)] tracking-wide mb-2 select-none">
              {{ t('tickets.subject') }} *
            </label>
            <Input
              v-model="createForm.subject"
              type="text"
              required
              :maxlength="200"
              :placeholder="t('tickets.subjectPlaceholder')"
            />
          </div>

          <!-- Category & Priority -->
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label class="block text-xs font-semibold text-[var(--text-secondary)] tracking-wide mb-2 select-none">
                {{ t('tickets.selectCategory') }}
              </label>
              <Select
                v-model="createForm.category"
                :options="categoryOptions"
              />
            </div>
            <div>
              <label class="block text-xs font-semibold text-[var(--text-secondary)] tracking-wide mb-2 select-none">
                {{ t('tickets.selectPriority') }}
              </label>
              <Select
                v-model="createForm.priority"
                :options="priorityOptions"
              />
            </div>
          </div>

          <!-- Content -->
          <div>
            <label class="block text-xs font-semibold text-[var(--text-secondary)] tracking-wide mb-2 select-none">
              {{ t('tickets.content') }}
            </label>
            <textarea
              v-model="createForm.content"
              maxlength="5000"
              rows="6"
              :placeholder="t('tickets.contentPlaceholder')"
              class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-none transition-all duration-200"
            />
          </div>

          <TicketImageUploader v-model="createAttachments" :disabled="creating" />

          <!-- Actions -->
          <div class="flex items-center justify-end gap-3 pt-3 border-t border-[var(--border-color)]">
            <Button type="button" variant="ghost" @click="backToList">
              {{ t('common.cancel') }}
            </Button>
            <Button type="submit" variant="primary" :loading="creating" :disabled="creating">
              {{ creating ? t('common.processing') : t('tickets.createTicket') }}
            </Button>
          </div>
        </form>
      </Card>
    </template>
  </div>

  <!-- Image Lightbox Modal -->
  <TicketImageLightbox
    :show="lightboxOpen"
    :images="lightboxImages"
    :start-index="lightboxStartIndex"
    @close="lightboxOpen = false"
  />
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  * {
    transition: none !important;
    animation: none !important;
  }
}
</style>
