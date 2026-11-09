<script setup lang="ts">
import { ref, onMounted, onActivated, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Users,
  UserPlus,
  Clock,
  History,
  Check,
  X,
  Trash2,
  Share2,
  Layers,
  RefreshCw,
  Search,
  Sliders,
  Plus,
  Mail,
  Calendar,
  ChevronRight
} from 'lucide-vue-next'
import api from '@/api'
import type { Package } from '@/types/api'
import UserAvatar from '@/components/UserAvatar.vue'
import DrawerModal from '@/components/DrawerModal.vue'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select,
  type BadgeVariant
} from '@/components/ui'
import { useToast } from '@/stores/toast'
import { translateError } from '@/utils/errorHandler'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'FriendsView' })

// 记录是否已经首次加载
let hasInitialLoad = false

// 类型定义
interface Friend {
  id: number // 好友的用户ID
  friendshipId?: number // friendship 记录的ID
  friendId?: number // 好友的用户ID（与 id 相同，为了向后兼容）
  username: string
  email?: string
  avatarStyle: string
  avatarBadgeId?: string | null
  status?: string
  createdAt: string
  acceptedAt?: string | null
  initiatedByMe?: boolean
  hostCount?: number
  instanceCount?: number
}

interface PendingRequest {
  id: number
  userId: number
  username: string
  email?: string
  avatarStyle: string
  avatarBadgeId?: string | null
  remark: string | null
  createdAt: string
}

interface HistoryRecord {
  id: number
  userId: number
  username: string
  email?: string
  avatarStyle: string
  avatarBadgeId?: string | null
  remark: string | null
  status: 'accepted' | 'rejected' | 'removed'
  createdAt: string
  acceptedAt: string | null
  rejectedAt: string | null
  initiatedByMe: boolean // 是否是我发起的请求
}

interface PackageShare {
  id: number
  packageId: number
  packageName: string
  ownerId: number
  ownerUsername: string
  sharedToId: number
  sharedToUsername: string
  quotaMultiplier: number | null
  maxInstances: number | null
  usage?: {
    instanceCount: number
    totalCpu: number
    totalMemory: number
  }
  createdAt: string
}

const { t } = useI18n()
const toast = useToast()

// 当前激活的 Tab
type TabKey = 'friends' | 'pending' | 'history'
const activeTab = ref<TabKey>('friends')

// 好友列表
const friends = ref<Friend[]>([])
const friendsLoading = ref(true)

// 好友列表分页（9条/页）
const friendsPage = ref(1)
const friendsPageSize = 9

// 待处理请求
const pendingRequests = ref<PendingRequest[]>([])
const pendingLoading = ref(true)

// 待处理请求分页（9条/页）
const pendingPage = ref(1)
const pendingPageSize = 9
const pendingTotalPages = computed(() => Math.max(1, Math.ceil(pendingRequests.value.length / pendingPageSize)))
const paginatedPendingRequests = computed(() => {
  const start = (pendingPage.value - 1) * pendingPageSize
  return pendingRequests.value.slice(start, start + pendingPageSize)
})

// 添加好友弹窗
const showAddFriendModal = ref(false)
const friendUsername = ref('')
const friendRemark = ref('')
const addFriendLoading = ref(false)

// 历史记录
const historyRecords = ref<HistoryRecord[]>([])
const historyLoading = ref(true)
const historyFilter = ref<'all' | 'accepted' | 'rejected' | 'removed'>('all')

// 历史记录分页（10条/页）
const historyPage = ref(1)
const historyPageSize = 10
const historySearch = ref('')

// 搜索和筛选后的历史记录
const filteredHistoryRecords = computed(() => {
  if (!historySearch.value.trim()) return historyRecords.value
  const keyword = historySearch.value.toLowerCase()
  return historyRecords.value.filter(r =>
    r.username.toLowerCase().includes(keyword) ||
    (r.email && r.email.toLowerCase().includes(keyword)) ||
    (r.remark && r.remark.toLowerCase().includes(keyword))
  )
})

const historyTotalPages = computed(() => Math.max(1, Math.ceil(filteredHistoryRecords.value.length / historyPageSize)))
const paginatedHistoryRecords = computed(() => {
  const start = (historyPage.value - 1) * historyPageSize
  return filteredHistoryRecords.value.slice(start, start + historyPageSize)
})

// 好友列表搜索
const friendsSearch = ref('')
const filteredFriends = computed(() => {
  if (!friendsSearch.value.trim()) return friends.value
  const keyword = friendsSearch.value.toLowerCase()
  return friends.value.filter(f =>
    f.username.toLowerCase().includes(keyword) ||
    (f.email && f.email.toLowerCase().includes(keyword))
  )
})

const filteredFriendsTotalPages = computed(() => Math.max(1, Math.ceil(filteredFriends.value.length / friendsPageSize)))
const paginatedFilteredFriends = computed(() => {
  const start = (friendsPage.value - 1) * friendsPageSize
  return filteredFriends.value.slice(start, start + friendsPageSize)
})

// 待处理数量
const pendingCount = computed(() => pendingRequests.value.length)

// ========== 套餐共享相关 ==========
// 选中的好友
const selectedFriend = ref<Friend | null>(null)

// 我的套餐列表
const myPackages = ref<Package[]>([])
const myPackagesLoading = ref(false)

// 好友已获得的共享套餐
const friendShares = ref<PackageShare[]>([])
const friendSharesLoading = ref(false)

// 共享操作
const shareLoading = ref(false)
const unshareLoading = ref<number | null>(null)

// 编辑配额弹窗
const showEditQuotaModal = ref(false)
const editingShare = ref<PackageShare | null>(null)
const editQuotaMultiplier = ref<string | number>('')
const editMaxInstances = ref<string | number>('')
const editQuotaLoading = ref(false)

// 添加共享弹窗
const showAddShareModal = ref(false)
const selectedPackageId = ref<string | number>('')
const newQuotaMultiplier = ref<string | number>('')
const newMaxInstances = ref<string | number>('')
const addShareLoading = ref(false)

// 可共享套餐搜索
const packageSearch = ref('')

// Tabs 定义
const tabs = computed(() => [
  {
    key: 'friends' as const,
    label: t('friends.friendsList'),
    badge: friends.value.length,
    badgeVariant: 'default' as BadgeVariant,
    icon: Users
  },
  {
    key: 'pending' as const,
    label: t('friends.pendingRequests'),
    badge: pendingCount.value > 0 ? pendingCount.value : undefined,
    badgeVariant: (pendingCount.value > 0 ? 'warning' : 'default') as BadgeVariant,
    icon: Clock
  },
  {
    key: 'history' as const,
    label: t('friends.historyRequests'),
    badge: historyRecords.value.length > 0 ? historyRecords.value.length : undefined,
    badgeVariant: 'default' as BadgeVariant,
    icon: History
  }
])

// 历史记录筛选下拉选项
const historyFilterOptions = computed(() => [
  { label: t('friends.filterAll'), value: 'all' },
  { label: t('friends.filterAccepted'), value: 'accepted' },
  { label: t('friends.filterRejected'), value: 'rejected' }
])

// 选中好友
function selectFriend(friend: Friend) {
  selectedFriend.value = friend
  loadFriendShares()
}

// 取消选中
function deselectFriend() {
  selectedFriend.value = null
  friendShares.value = []
}

// 加载我的套餐
async function loadMyPackages() {
  myPackagesLoading.value = true
  try {
    const res = await api.packages.list()
    myPackages.value = res.packages || []
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    myPackagesLoading.value = false
  }
}

// 加载共享给当前好友的套餐
async function loadFriendShares() {
  if (!selectedFriend.value) return

  friendSharesLoading.value = true
  try {
    const res = await api.packages.getShared()
    friendShares.value = (res.shares || []).filter(
      s => s.sharedToId === selectedFriend.value?.id
    )
  } catch (err) {
    console.error('加载共享列表失败:', err)
    friendShares.value = []
  } finally {
    friendSharesLoading.value = false
  }
}

// 打开添加共享弹窗
function openAddShareModal(packageId?: number) {
  selectedPackageId.value = packageId !== undefined ? packageId : ''
  newQuotaMultiplier.value = ''
  newMaxInstances.value = ''
  showAddShareModal.value = true
}

// 可以共享的套餐（只返回用户自己拥有的套餐，排除已共享给当前好友的）
const availablePackages = computed(() => {
  const sharedIds = new Set(friendShares.value.map(s => s.packageId))
  return myPackages.value.filter(p =>
    (p.isOwn === true) && !sharedIds.has(p.id)
  )
})

// 添加共享的套餐下拉选项
const packageSelectOptions = computed(() => [
  { label: t('friends.selectPackagePlaceholder'), value: '' },
  ...availablePackages.value.map(pkg => ({
    label: `${pkg.name} (${pkg.cpu_max}% CPU, ${formatMemory(pkg.memory_max)})`,
    value: pkg.id
  }))
])

// 搜索过滤后的可共享套餐
const filteredAvailablePackages = computed(() => {
  if (!packageSearch.value.trim()) return availablePackages.value
  const keyword = packageSearch.value.toLowerCase()
  return availablePackages.value.filter(p =>
    p.name.toLowerCase().includes(keyword) ||
    (p.description && p.description.toLowerCase().includes(keyword))
  )
})

// 刷新套餐共享数据
async function refreshShareData() {
  await Promise.all([
    loadMyPackages(),
    selectedFriend.value ? loadFriendShares() : Promise.resolve()
  ])
  toast.success(t('common.refresh') + ' ' + t('common.success'))
}

// 全局刷新所有数据
async function refreshAllData() {
  await Promise.all([
    loadFriends(),
    loadPendingRequests(),
    loadHistoryRecords(),
    loadMyPackages(),
    selectedFriend.value ? loadFriendShares() : Promise.resolve()
  ])
  toast.success(t('common.refresh') + ' ' + t('common.success'))
}

// 添加共享
async function addShare() {
  const pkgId = Number(selectedPackageId.value)
  if (!selectedFriend.value || !pkgId) return

  const qm = newQuotaMultiplier.value === '' ? null : Number(newQuotaMultiplier.value)
  const quotaMultiplier = (qm !== null && !Number.isNaN(qm)) ? qm : null

  const mi = newMaxInstances.value === '' ? null : Number(newMaxInstances.value)
  const maxInstances = (mi !== null && !Number.isNaN(mi)) ? mi : null

  addShareLoading.value = true
  try {
    await api.packages.share(
      pkgId,
      selectedFriend.value.id,
      quotaMultiplier,
      maxInstances
    )
    toast.success(t('friends.shareAdded'))
    showAddShareModal.value = false
    loadFriendShares()
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    addShareLoading.value = false
  }
}

// 取消共享
async function removeShare(share: PackageShare) {
  if (!selectedFriend.value) return
  if (!confirm(t('friends.confirmRemoveShare', { package: share.packageName }))) return

  unshareLoading.value = share.id
  try {
    await api.packages.unshare(share.packageId, selectedFriend.value.id)
    toast.success(t('friends.shareRemoved'))
    loadFriendShares()
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    unshareLoading.value = null
  }
}

// 打开编辑配额弹窗
function openEditQuotaModal(share: PackageShare) {
  editingShare.value = share
  editQuotaMultiplier.value = share.quotaMultiplier !== null && share.quotaMultiplier !== undefined ? share.quotaMultiplier : ''
  editMaxInstances.value = share.maxInstances !== null && share.maxInstances !== undefined ? share.maxInstances : ''
  showEditQuotaModal.value = true
}

// 保存配额编辑
async function saveQuotaEdit() {
  if (!editingShare.value) return

  const qm = editQuotaMultiplier.value === '' ? null : Number(editQuotaMultiplier.value)
  const quotaMultiplier = (qm !== null && !Number.isNaN(qm)) ? qm : null

  const mi = editMaxInstances.value === '' ? null : Number(editMaxInstances.value)
  const maxInstances = (mi !== null && !Number.isNaN(mi)) ? mi : null

  editQuotaLoading.value = true
  try {
    await api.packages.updateShareQuota(
      editingShare.value.packageId,
      editingShare.value.id,
      quotaMultiplier,
      maxInstances
    )
    toast.success(t('friends.quotaUpdated'))
    showEditQuotaModal.value = false
    loadFriendShares()
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    editQuotaLoading.value = false
  }
}

// 格式化内存
function formatMemory(mb: number): string {
  if (mb >= 1024) {
    return `${(mb / 1024).toFixed(1)} GB`
  }
  return `${mb} MB`
}

// 加载好友列表
async function loadFriends() {
  friendsLoading.value = true
  try {
    const res = await api.friends.list()
    friends.value = res.friends || []
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    friendsLoading.value = false
  }
}

// 加载待处理请求
async function loadPendingRequests() {
  pendingLoading.value = true
  try {
    const res = await api.friends.getPendingRequests()
    pendingRequests.value = res.requests || []
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    pendingLoading.value = false
  }
}

// 加载历史记录
async function loadHistoryRecords() {
  historyLoading.value = true
  try {
    const res = await api.friends.getHistory(historyFilter.value)
    historyRecords.value = res.history || []
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    historyLoading.value = false
  }
}

// 发送好友请求
async function sendFriendRequest() {
  if (!friendUsername.value.trim()) return

  addFriendLoading.value = true
  try {
    await api.friends.sendRequest(friendUsername.value.trim(), friendRemark.value.trim() || undefined)
    toast.success(t('friends.requestSent'))
    showAddFriendModal.value = false
    friendUsername.value = ''
    friendRemark.value = ''
  } catch (err) {
    toast.error(translateError(err))
  } finally {
    addFriendLoading.value = false
  }
}

// 接受好友请求
async function acceptRequest(request: PendingRequest) {
  try {
    await api.friends.accept(request.id)
    toast.success(t('friends.requestAccepted'))
    pendingRequests.value = pendingRequests.value.filter(r => r.id !== request.id)
    loadFriends()
  } catch (err) {
    toast.error(translateError(err))
  }
}

// 拒绝好友请求
async function rejectRequest(request: PendingRequest) {
  try {
    await api.friends.reject(request.id)
    toast.success(t('friends.requestRejected'))
    pendingRequests.value = pendingRequests.value.filter(r => r.id !== request.id)
    loadHistoryRecords()
  } catch (err) {
    toast.error(translateError(err))
  }
}

// 删除好友
async function removeFriend(friend: Friend) {
  if (!confirm(t('friends.confirmRemove', { name: friend.username }))) return

  try {
    const friendshipId = friend.friendshipId || friend.id
    await api.friends.remove(friendshipId)
    toast.success(t('friends.friendRemoved'))
    friends.value = friends.value.filter(f => f.id !== friend.id)
    if (selectedFriend.value?.id === friend.id) {
      deselectFriend()
    }
  } catch (err) {
    toast.error(translateError(err))
  }
}

// 格式化时间
function formatDate(dateStr: string | null | undefined) {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleDateString()
}

// 切换 tab
function switchTab(tab: TabKey) {
  activeTab.value = tab
}

onMounted(async () => {
  await Promise.all([loadFriends(), loadPendingRequests(), loadHistoryRecords(), loadMyPackages()])
  hasInitialLoad = true
})

onActivated(async () => {
  if (hasInitialLoad) {
    await Promise.all([loadFriends(), loadPendingRequests(), loadMyPackages()])
  }
})
</script>

<template>
  <div class="space-y-6">
    <!-- Apple HIG PageHeader -->
    <PageHeader
      :title="t('friends.title')"
      :description="t('friends.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :title="t('common.refresh')"
          @click="refreshAllData"
        >
          <template #icon>
            <RefreshCw class="w-4 h-4" />
          </template>
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="showAddFriendModal = true"
        >
          <template #icon>
            <UserPlus class="w-4 h-4" />
          </template>
          <span>{{ t('friends.addFriend') }}</span>
        </Button>
      </template>
    </PageHeader>

    <!-- Apple 风格分段控制栏 (Segmented Control Tabs) -->
    <div class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
      <button
        v-for="tab in tabs"
        :key="tab.key"
        type="button"
        class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none"
        :class="activeTab === tab.key
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="switchTab(tab.key)"
      >
        <component
          :is="tab.icon"
          class="w-4 h-4 shrink-0"
          :class="activeTab === tab.key ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
        />
        <span>{{ tab.label }}</span>
        <Badge
          v-if="tab.badge !== undefined"
          size="sm"
          :variant="tab.badgeVariant || (activeTab === tab.key ? 'info' : 'default')"
        >
          {{ tab.badge }}
        </Badge>
      </button>
    </div>

    <!-- Tab 动画过渡 -->
    <Transition name="tab" mode="out-in">
      <div :key="activeTab">
        <!-- 好友列表 Tab -->
        <div v-if="activeTab === 'friends'" class="space-y-4">
          <!-- 工具栏 -->
          <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-4">
            <div class="w-full sm:w-80 md:w-96">
              <Input
                v-model="friendsSearch"
                size="sm"
                :placeholder="t('common.searchPlaceholder')"
                clearable
                @update:model-value="friendsPage = 1"
              >
                <template #prefix>
                  <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>
            <div class="text-xs text-[var(--text-secondary)] flex items-center gap-2 shrink-0 self-end sm:self-auto">
              <span>{{ t('friends.friendsList') }}: {{ filteredFriends.length }}</span>
            </div>
          </div>

          <!-- 加载中 -->
          <div v-if="friendsLoading" class="grid grid-cols-1 lg:grid-cols-3 gap-6">
            <div class="space-y-3">
              <div v-for="i in 3" :key="i" class="h-16 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse border border-[var(--border-color)]"></div>
            </div>
            <div class="lg:col-span-2 h-64 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse border border-[var(--border-color)]"></div>
          </div>

          <!-- 无好友 -->
          <Card v-else-if="friends.length === 0" padding="lg">
            <EmptyState :title="t('friends.noFriends')" :description="t('friends.noFriendsHint')">
              <template #icon>
                <Users class="w-7 h-7 stroke-[1.5] text-[var(--accent)]" />
              </template>
              <template #action>
                <Button variant="primary" @click="showAddFriendModal = true">
                  <template #icon>
                    <UserPlus class="w-4 h-4" />
                  </template>
                  <span>{{ t('friends.addFriend') }}</span>
                </Button>
              </template>
            </EmptyState>
          </Card>

          <!-- 搜索无结果 -->
          <Card v-else-if="filteredFriends.length === 0" padding="lg">
            <EmptyState :title="t('friends.noSearchResult')" :description="t('friends.noFriendsHint')">
              <template #icon>
                <Search class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
              </template>
            </EmptyState>
          </Card>

          <!-- 主从双栏布局 -->
          <div v-else class="flex flex-col lg:flex-row gap-6 items-start">
            <!-- 左侧：好友列表 -->
            <div class="w-full lg:w-80 xl:w-96 shrink-0 space-y-3">
              <div class="space-y-2">
                <div
                  v-for="friend in paginatedFilteredFriends"
                  :key="friend.id"
                  class="group relative rounded-2xl border p-3.5 transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] cursor-pointer select-none active:scale-[0.98]"
                  :class="selectedFriend?.id === friend.id
                    ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))] shadow-sm ring-1 ring-[var(--accent)]'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:shadow-[0_4px_12px_rgba(0,0,0,0.04)]'"
                  @click="selectFriend(friend)"
                >
                  <div class="flex items-center gap-3">
                    <UserAvatar
                      :username="friend.username || ''"
                      :email="friend.email"
                      :avatar-style="friend.avatarStyle || ''"
                      :badge-id="friend.avatarBadgeId || null"
                      :size="40"
                      class="shrink-0"
                    />
                    <div class="min-w-0 flex-1">
                      <div class="flex items-center justify-between gap-1">
                        <span class="font-semibold text-sm text-[var(--text-primary)] truncate">
                          {{ friend.username }}
                        </span>
                        <ChevronRight
                          class="w-4 h-4 text-[var(--text-tertiary)] transition-transform duration-200"
                          :class="selectedFriend?.id === friend.id ? 'text-[var(--accent)] translate-x-0.5' : 'group-hover:translate-x-0.5'"
                        />
                      </div>
                      <p class="text-xs text-[var(--text-secondary)] truncate mt-0.5">
                        {{ friend.email || (t('friends.addedOn') + ' ' + formatDate(friend.createdAt)) }}
                      </p>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 好友列表分页 -->
              <div v-if="filteredFriendsTotalPages > 1" class="pt-2">
                <Pagination
                  v-model:page="friendsPage"
                  :page-size="friendsPageSize"
                  :total="filteredFriends.length"
                  :show-size-changer="false"
                />
              </div>
            </div>

            <!-- 右侧：套餐管理面板 -->
            <div class="flex-1 min-w-0 w-full space-y-5">
              <!-- 未选择好友 -->
              <Card v-if="!selectedFriend" padding="lg">
                <EmptyState
                  :title="t('friends.selectFriendHint')"
                  :description="t('friends.selectFriendDesc')"
                >
                  <template #icon>
                    <Share2 class="w-7 h-7 stroke-[1.5] text-[var(--accent)]" />
                  </template>
                </EmptyState>
              </Card>

              <!-- 已选择好友 -->
              <template v-else>
                <!-- 好友信息横幅卡片 -->
                <Card padding="sm">
                  <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
                    <div class="flex items-center gap-3.5 min-w-0">
                      <UserAvatar
                        :username="selectedFriend.username || ''"
                        :email="selectedFriend.email"
                        :avatar-style="selectedFriend.avatarStyle || ''"
                        :badge-id="selectedFriend.avatarBadgeId || null"
                        :size="48"
                        class="shrink-0 ring-2 ring-[var(--border-color)] rounded-full"
                      />
                      <div class="min-w-0 flex-1">
                        <div class="flex items-center gap-2 flex-wrap">
                          <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] truncate">
                            {{ selectedFriend.username }}
                          </h3>
                          <Badge size="sm" variant="info">
                            {{ t('friends.sharedPackages') }}: {{ friendShares.length }}
                          </Badge>
                        </div>
                        <div class="flex items-center gap-3 mt-1 text-xs text-[var(--text-secondary)] flex-wrap">
                          <span v-if="selectedFriend.email" class="flex items-center gap-1 truncate">
                            <Mail class="w-3.5 h-3.5 shrink-0" />
                            {{ selectedFriend.email }}
                          </span>
                          <span class="flex items-center gap-1">
                            <Calendar class="w-3.5 h-3.5 shrink-0" />
                            {{ t('friends.addedOn') }} {{ formatDate(selectedFriend.createdAt) }}
                          </span>
                        </div>
                      </div>
                    </div>

                    <div class="flex items-center gap-2 shrink-0 self-end sm:self-center">
                      <Button
                        variant="ghost"
                        size="sm"
                        :title="t('common.refresh')"
                        @click="refreshShareData"
                      >
                        <template #icon>
                          <RefreshCw class="w-4 h-4" />
                        </template>
                      </Button>
                      <Button
                        variant="danger"
                        size="sm"
                        @click="removeFriend(selectedFriend)"
                      >
                        <template #icon>
                          <Trash2 class="w-4 h-4" />
                        </template>
                        <span>{{ t('friends.removeFriend') }}</span>
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        class="lg:hidden"
                        @click="deselectFriend"
                      >
                        <template #icon>
                          <X class="w-4 h-4" />
                        </template>
                      </Button>
                    </div>
                  </div>
                </Card>

                <!-- 已共享的套餐 -->
                <Card padding="md">
                  <template #header>
                    <div class="flex items-center justify-between w-full">
                      <div class="flex items-center gap-2">
                        <h3 class="font-semibold text-base text-[var(--text-primary)]">
                          {{ t('friends.sharedPackages') }}
                        </h3>
                        <Badge size="sm" variant="default">{{ friendShares.length }}</Badge>
                      </div>
                      <Button
                        v-if="availablePackages.length > 0"
                        variant="primary"
                        size="sm"
                        @click="openAddShareModal()"
                      >
                        <template #icon>
                          <Plus class="w-4 h-4" />
                        </template>
                        <span>{{ t('friends.addShare') }}</span>
                      </Button>
                    </div>
                  </template>

                  <div v-if="friendSharesLoading" class="space-y-3">
                    <div v-for="i in 2" :key="i" class="h-20 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse border border-[var(--border-color)]"></div>
                  </div>

                  <EmptyState
                    v-else-if="friendShares.length === 0"
                    :title="t('friends.noSharedPackages')"
                    description=""
                  >
                    <template #icon>
                      <Layers class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
                    </template>
                    <template v-if="availablePackages.length > 0" #action>
                      <Button variant="primary" size="sm" @click="openAddShareModal()">
                        <template #icon>
                          <Plus class="w-4 h-4" />
                        </template>
                        <span>{{ t('friends.addFirstShare') }}</span>
                      </Button>
                    </template>
                  </EmptyState>

                  <div v-else class="space-y-3">
                    <div
                      v-for="share in friendShares"
                      :key="share.id"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 transition-colors duration-200"
                    >
                      <div class="min-w-0 flex-1">
                        <div class="flex items-center gap-2">
                          <span class="font-semibold text-sm text-[var(--text-primary)] truncate">
                            {{ share.packageName }}
                          </span>
                        </div>
                        <div class="flex flex-wrap items-center gap-2 mt-2">
                          <Badge v-if="share.quotaMultiplier !== null" size="sm" variant="default">
                            {{ t('friends.quotaMultiplier') }}: {{ share.quotaMultiplier }}x
                          </Badge>
                          <Badge v-if="share.maxInstances !== null" size="sm" variant="default">
                            {{ t('friends.maxInstances') }}: {{ share.maxInstances }}
                          </Badge>
                          <Badge v-if="share.usage" size="sm" variant="info">
                            {{ t('friends.currentUsage') }}: {{ share.usage.instanceCount }} {{ t('friends.instances') }}
                          </Badge>
                        </div>
                      </div>

                      <div class="flex items-center gap-2 shrink-0 self-end sm:self-center">
                        <Button
                          variant="secondary"
                          size="sm"
                          :title="t('friends.editQuota')"
                          @click="openEditQuotaModal(share)"
                        >
                          <template #icon>
                            <Sliders class="w-3.5 h-3.5" />
                          </template>
                          <span>{{ t('friends.editQuota') }}</span>
                        </Button>
                        <Button
                          variant="danger"
                          size="sm"
                          :loading="unshareLoading === share.id"
                          :title="t('friends.removeShare')"
                          @click="removeShare(share)"
                        >
                          <template #icon>
                            <Trash2 class="w-3.5 h-3.5" />
                          </template>
                          <span>{{ t('friends.removeShare') }}</span>
                        </Button>
                      </div>
                    </div>
                  </div>
                </Card>

                <!-- 可共享的套餐 -->
                <Card v-if="availablePackages.length > 0" padding="md">
                  <template #header>
                    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
                      <div class="flex items-center gap-2">
                        <h3 class="font-semibold text-base text-[var(--text-primary)]">
                          {{ t('friends.availablePackages') }}
                        </h3>
                        <Badge size="sm" variant="default">{{ filteredAvailablePackages.length }}</Badge>
                      </div>
                      <div v-if="availablePackages.length > 3" class="w-full sm:w-48">
                        <Input
                          v-model="packageSearch"
                          size="sm"
                          :placeholder="t('common.searchPlaceholder')"
                          clearable
                        >
                          <template #prefix>
                            <Search class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                          </template>
                        </Input>
                      </div>
                    </div>
                  </template>

                  <div v-if="filteredAvailablePackages.length === 0" class="py-6 text-center text-sm text-[var(--text-secondary)]">
                    {{ t('friends.noPackageSearchResult') }}
                  </div>

                  <div v-else class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                    <div
                      v-for="pkg in filteredAvailablePackages"
                      :key="pkg.id"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 flex items-center justify-between gap-3 hover:border-[var(--border-hover)] transition-colors duration-200"
                    >
                      <div class="min-w-0 flex-1">
                        <p class="font-semibold text-sm text-[var(--text-primary)] truncate">
                          {{ pkg.name }}
                        </p>
                        <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                          {{ pkg.cpu_max }}% CPU · {{ formatMemory(pkg.memory_max) }}
                        </p>
                      </div>
                      <Button
                        variant="secondary"
                        size="sm"
                        :disabled="shareLoading"
                        @click="openAddShareModal(pkg.id)"
                      >
                        <template #icon>
                          <Plus class="w-3.5 h-3.5" />
                        </template>
                        <span>{{ t('friends.addShare') }}</span>
                      </Button>
                    </div>
                  </div>
                </Card>

                <!-- 用户没有套餐时的提示 -->
                <Card v-else-if="myPackages.length === 0 && friendShares.length === 0" padding="md">
                  <EmptyState
                    :title="t('friends.noPackagesToShare')"
                    :description="t('friends.createPackageFirst')"
                  >
                    <template #icon>
                      <Layers class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
                    </template>
                  </EmptyState>
                </Card>
              </template>
            </div>
          </div>
        </div>

        <!-- 待处理请求 Tab -->
        <div v-if="activeTab === 'pending'" class="space-y-4">
          <div v-if="pendingLoading" class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            <div v-for="i in 3" :key="i" class="h-44 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse border border-[var(--border-color)]"></div>
          </div>

          <Card v-else-if="pendingRequests.length === 0" padding="lg">
            <EmptyState :title="t('friends.noPendingRequests')" description="">
              <template #icon>
                <Clock class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
              </template>
            </EmptyState>
          </Card>

          <template v-else>
            <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              <Card
                v-for="request in paginatedPendingRequests"
                :key="request.id"
                padding="sm"
              >
                <div class="flex items-start gap-3.5">
                  <UserAvatar
                    :username="request.username || ''"
                    :email="request.email"
                    :avatar-style="request.avatarStyle || ''"
                    :badge-id="request.avatarBadgeId || null"
                    :size="44"
                    class="shrink-0 ring-1 ring-[var(--border-color)] rounded-full"
                  />
                  <div class="min-w-0 flex-1">
                    <h4 class="font-semibold text-base text-[var(--text-primary)] truncate">
                      {{ request.username }}
                    </h4>
                    <p v-if="request.email" class="text-xs text-[var(--text-secondary)] truncate mt-0.5">
                      {{ request.email }}
                    </p>
                    <div class="flex items-center gap-1 text-xs text-[var(--text-tertiary)] mt-1">
                      <Clock class="w-3.5 h-3.5 shrink-0" />
                      <span>{{ t('friends.requestedOn') }} {{ formatDate(request.createdAt) }}</span>
                    </div>
                  </div>
                </div>

                <div v-if="request.remark" class="mt-3 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs text-[var(--text-secondary)]">
                  <span class="font-medium text-[var(--text-primary)]">{{ t('friends.remark') }}:</span>
                  <span class="break-words ml-1">{{ request.remark }}</span>
                </div>

                <div class="flex items-center gap-2.5 mt-4 pt-3.5 border-t border-[var(--border-color)]">
                  <Button
                    variant="primary"
                    size="sm"
                    block
                    @click="acceptRequest(request)"
                  >
                    <template #icon>
                      <Check class="w-4 h-4" />
                    </template>
                    <span>{{ t('friends.accept') }}</span>
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    block
                    @click="rejectRequest(request)"
                  >
                    <template #icon>
                      <X class="w-4 h-4" />
                    </template>
                    <span>{{ t('friends.reject') }}</span>
                  </Button>
                </div>
              </Card>
            </div>

            <!-- 分页 -->
            <div v-if="pendingTotalPages > 1" class="pt-2">
              <Pagination
                v-model:page="pendingPage"
                :page-size="pendingPageSize"
                :total="pendingRequests.length"
              />
            </div>
          </template>
        </div>

        <!-- 历史记录 Tab -->
        <div v-if="activeTab === 'history'" class="space-y-4">
          <!-- 搜索和筛选工具栏 -->
          <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3">
            <div class="w-full sm:w-80">
              <Input
                v-model="historySearch"
                size="sm"
                :placeholder="t('common.searchPlaceholder')"
                clearable
                @update:model-value="historyPage = 1"
              >
                <template #prefix>
                  <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>
            <div class="flex items-center gap-2 self-end sm:self-auto">
              <span class="text-xs text-[var(--text-secondary)] shrink-0">{{ t('common.filter') }}:</span>
              <Select
                v-model="historyFilter"
                size="sm"
                :options="historyFilterOptions"
                :block="false"
                class="w-36"
                @change="loadHistoryRecords(); historyPage = 1"
              />
            </div>
          </div>

          <div v-if="historyLoading" class="space-y-3">
            <div v-for="i in 4" :key="i" class="h-20 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse border border-[var(--border-color)]"></div>
          </div>

          <Card v-else-if="historyRecords.length === 0" padding="lg">
            <EmptyState :title="t('friends.noHistoryRecords')" :description="t('friends.noHistoryRecordsHint')">
              <template #icon>
                <History class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
              </template>
            </EmptyState>
          </Card>

          <Card v-else-if="filteredHistoryRecords.length === 0" padding="lg">
            <EmptyState :title="t('friends.noHistorySearchResult')">
              <template #icon>
                <Search class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
              </template>
            </EmptyState>
          </Card>

          <div v-else class="space-y-3">
            <div
              v-for="record in paginatedHistoryRecords"
              :key="record.id"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.04)] transition-all duration-200"
            >
              <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
                <div class="flex items-start gap-3.5 min-w-0 flex-1">
                  <UserAvatar
                    :username="record.username || ''"
                    :email="record.email"
                    :avatar-style="record.avatarStyle || ''"
                    :badge-id="record.avatarBadgeId || null"
                    :size="42"
                    class="shrink-0 ring-1 ring-[var(--border-color)] rounded-full"
                  />
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-2 flex-wrap">
                      <span class="font-semibold text-sm text-[var(--text-primary)] truncate">
                        {{ record.initiatedByMe ? t('friends.sentTo', { username: record.username }) : record.username }}
                      </span>
                      <Badge
                        size="sm"
                        :variant="record.status === 'accepted' ? 'success' : record.status === 'rejected' ? 'error' : 'default'"
                        dot
                      >
                        {{ record.status === 'accepted' ? t('friends.statusAccepted') : record.status === 'rejected' ? t('friends.statusRejected') : record.status }}
                      </Badge>
                    </div>
                    <div class="flex items-center gap-3 mt-1 text-xs text-[var(--text-secondary)] flex-wrap">
                      <span v-if="record.email" class="truncate">{{ record.email }}</span>
                      <span class="flex items-center gap-1">
                        <Calendar class="w-3.5 h-3.5 shrink-0" />
                        {{ record.initiatedByMe ? t('friends.sentOn') : t('friends.requestedOn') }} {{ formatDate(record.createdAt) }}
                      </span>
                      <span v-if="record.acceptedAt || record.rejectedAt" class="flex items-center gap-1 text-[var(--text-tertiary)]">
                        <Clock class="w-3.5 h-3.5 shrink-0" />
                        {{ t('friends.processedOn') }} {{ formatDate(record.status === 'accepted' ? record.acceptedAt : record.rejectedAt) }}
                      </span>
                    </div>
                    <div v-if="record.remark" class="mt-2 text-xs text-[var(--text-secondary)] bg-[var(--bg-surface-soft)] rounded-lg px-2.5 py-1.5 border border-[var(--border-color)] inline-block max-w-full truncate">
                      <span class="text-[var(--text-tertiary)]">{{ t('friends.remark') }}:</span> {{ record.remark }}
                    </div>
                  </div>
                </div>
              </div>
            </div>

            <!-- 历史记录分页 -->
            <div v-if="historyTotalPages > 1" class="pt-2">
              <Pagination
                v-model:page="historyPage"
                :page-size="historyPageSize"
                :total="filteredHistoryRecords.length"
              />
            </div>
          </div>
        </div>
      </div>
    </Transition>

    <!-- 添加好友弹窗 -->
    <DrawerModal
      :show="showAddFriendModal"
      max-width="max-w-md"
      raw
      @close="showAddFriendModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]">
            <UserPlus class="h-5 w-5 stroke-[1.8]" />
          </div>
          <div>
            <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('friends.addFriend') }}</h3>
            <p class="text-xs text-[var(--text-secondary)]">{{ t('friends.usernameHint') }}</p>
          </div>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showAddFriendModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <form class="modal-body p-6 space-y-4" @submit.prevent="sendFriendRequest">
        <Input
          v-model="friendUsername"
          :label="t('friends.username')"
          :placeholder="t('friends.usernamePlaceholder')"
          :hint="t('friends.usernameHint')"
          required
        />
        <div class="flex flex-col gap-1.5">
          <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
            {{ t('friends.remark') }}
          </label>
          <textarea
            v-model="friendRemark"
            rows="3"
            class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] hover:border-[var(--border-hover)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] focus:border-[var(--accent)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200"
            :placeholder="t('friends.remarkPlaceholder')"
          ></textarea>
          <p class="text-xs text-[var(--text-tertiary)] mt-0.5">{{ t('friends.remarkHint') }}</p>
        </div>
      </form>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showAddFriendModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          :disabled="!friendUsername.trim() || addFriendLoading"
          :loading="addFriendLoading"
          @click="sendFriendRequest"
        >
          {{ t('friends.sendRequest') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- 添加共享弹窗 -->
    <DrawerModal
      :show="showAddShareModal"
      max-width="max-w-md"
      raw
      @close="showAddShareModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]">
            <Share2 class="h-5 w-5 stroke-[1.8]" />
          </div>
          <div>
            <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('friends.addShareTitle') }}</h3>
            <p class="text-xs text-[var(--text-secondary)]">{{ selectedFriend ? t('friends.sharedTo') + ': ' + selectedFriend.username : '' }}</p>
          </div>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showAddShareModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-4">
        <!-- 目标好友预览 -->
        <div class="p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <div class="flex items-center gap-3">
            <UserAvatar
              v-if="selectedFriend"
              :username="selectedFriend.username || ''"
              :email="selectedFriend.email"
              :avatar-style="selectedFriend.avatarStyle || ''"
              :badge-id="selectedFriend.avatarBadgeId || null"
              :size="36"
            />
            <div>
              <div class="text-sm font-semibold text-[var(--text-primary)]">
                {{ selectedFriend?.username }}
              </div>
              <div class="text-xs text-[var(--text-secondary)]">{{ t('friends.shareToFriend') }}</div>
            </div>
          </div>
        </div>

        <!-- 选择套餐 -->
        <div>
          <Select
            v-model="selectedPackageId"
            :label="t('friends.selectPackage')"
            :options="packageSelectOptions"
          />
        </div>

        <!-- 配额限制 -->
        <div class="grid grid-cols-2 gap-4">
          <div>
            <Input
              v-model="newQuotaMultiplier"
              type="number"
              :label="t('friends.quotaMultiplier')"
              :hint="t('friends.quotaMultiplierHint')"
              :placeholder="t('friends.noLimit')"
            >
              <template #suffix>
                <span class="text-xs text-[var(--text-tertiary)] font-medium">×</span>
              </template>
            </Input>
          </div>
          <div>
            <Input
              v-model="newMaxInstances"
              type="number"
              :label="t('friends.maxInstances')"
              :hint="t('friends.maxInstancesHint')"
              :placeholder="t('friends.noLimit')"
            />
          </div>
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showAddShareModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          :disabled="selectedPackageId === '' || addShareLoading"
          :loading="addShareLoading"
          @click="addShare"
        >
          {{ t('friends.confirmShare') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- 编辑配额弹窗 -->
    <DrawerModal
      :show="showEditQuotaModal"
      max-width="max-w-md"
      raw
      @close="showEditQuotaModal = false"
    >
      <div class="modal-header border-b border-[var(--border-color)] px-6 py-4 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]">
            <Sliders class="h-5 w-5 stroke-[1.8]" />
          </div>
          <div>
            <h3 class="text-base sm:text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('friends.editQuotaTitle') }}</h3>
            <p class="text-xs text-[var(--text-secondary)]">{{ editingShare?.packageName }}</p>
          </div>
        </div>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] border border-[var(--border-color)] transition-all duration-200 active:scale-95"
          @click="showEditQuotaModal = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <div class="modal-body p-6 space-y-4">
        <!-- 套餐信息 -->
        <div class="p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <div class="text-sm font-semibold text-[var(--text-primary)]">
            {{ editingShare?.packageName }}
          </div>
          <div class="text-xs text-[var(--text-secondary)] mt-1">
            {{ t('friends.sharedTo') }}: {{ selectedFriend?.username }}
          </div>
        </div>

        <!-- 配额限制 -->
        <div class="grid grid-cols-2 gap-4">
          <div>
            <Input
              v-model="editQuotaMultiplier"
              type="number"
              :label="t('friends.quotaMultiplier')"
              :hint="t('friends.quotaMultiplierHint')"
              :placeholder="t('friends.noLimit')"
            >
              <template #suffix>
                <span class="text-xs text-[var(--text-tertiary)] font-medium">×</span>
              </template>
            </Input>
          </div>
          <div>
            <Input
              v-model="editMaxInstances"
              type="number"
              :label="t('friends.maxInstances')"
              :hint="t('friends.maxInstancesHint')"
              :placeholder="t('friends.noLimit')"
            />
          </div>
        </div>
      </div>

      <div class="modal-footer border-t border-[var(--border-color)] px-6 py-4 flex items-center justify-end gap-3">
        <Button variant="secondary" @click="showEditQuotaModal = false">{{ t('common.cancel') }}</Button>
        <Button
          variant="primary"
          :disabled="editQuotaLoading"
          :loading="editQuotaLoading"
          @click="saveQuotaEdit"
        >
          {{ t('common.save') }}
        </Button>
      </div>
    </DrawerModal>
  </div>
</template>

<style scoped>
.tab-enter-active,
.tab-leave-active {
  transition: opacity 0.2s cubic-bezier(0.25, 0.1, 0.25, 1), transform 0.2s cubic-bezier(0.25, 0.1, 0.25, 1);
}
.tab-enter-from {
  opacity: 0;
  transform: translateY(4px);
}
.tab-leave-to {
  opacity: 0;
  transform: translateY(-4px);
}
</style>
