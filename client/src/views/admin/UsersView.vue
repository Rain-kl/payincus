<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  Check,
  ChevronDown,
  Copy,
  Gift,
  Globe,
  Key,
  Mail,
  MoreHorizontal,
  Plus,
  Search,
  Server,
  ShieldAlert,
  ShieldCheck,
  ShieldOff,
  Trash2,
  UserCheck,
  UserX,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import SkeletonLoader from '@/components/SkeletonLoader.vue'
import UserAvatar from '@/components/UserAvatar.vue'
import VipLevelRulesEditor from '@/components/admin/VipLevelRulesEditor.vue'
import VipBenefitHallSettings from '@/components/admin/VipBenefitHallSettings.vue'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import { normalizeCountryName } from '@/utils/countryDisplay'
import { instancesPath } from '@/utils/app-paths'
import { useTabQuery } from '@/composables/useTabQuery'
import { useReveal } from '@/composables/useReveal'
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

defineOptions({ name: 'AdminUsersView' })

const { t } = useI18n()
const router = useRouter()
const toast = useToast()
const authStore = useAuthStore()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const normalizedCountryLabels = computed(() => ({
  mainlandChina: t('common.countries.cn'),
  hongKong: t('common.countries.hk'),
  macau: t('common.countries.mo'),
  taiwan: t('common.countries.tw')
}))

// Tab 切换
type TabType = 'users' | 'invites' | 'linkedAccounts' | 'userVipLevels' | 'vipBenefits'
const activeTab = useTabQuery<TabType>({
  keys: ['users', 'invites', 'linkedAccounts', 'userVipLevels', 'vipBenefits'],
  fallback: 'users',
  onExternalChange: (tab) => loadTabData(tab)
})

const tabs = computed(() => [
  { key: 'users' as TabType, label: t('nav.users'), badge: userTotal.value },
  { key: 'invites' as TabType, label: t('admin.users.invites'), badge: invitesTotal.value || undefined },
  { key: 'linkedAccounts' as TabType, label: t('admin.users.linkedAccounts') },
  { key: 'userVipLevels' as TabType, label: t('admin.users.userVipLevels') },
  { key: 'vipBenefits' as TabType, label: t('admin.users.vipBenefits') }
])

// 用户数据
const USER_SEARCH_FIELDS = ['username', 'id', 'email']
const USER_SEARCH_FIELD_OPTIONS = [
  { value: 'username', label: 'admin.users.searchFieldUsername' },
  { value: 'id', label: 'admin.users.searchFieldId' },
  { value: 'email', label: 'admin.users.searchFieldEmail' }
]
const users = ref<any[]>([])
const usersLoading = ref(true)
const userSearch = ref('')
const userSearchFields = ref<string[]>([...USER_SEARCH_FIELDS])
const userSearchExact = ref(false)
const userPage = ref(1)
const userPageSize = ref(100)
const userTotal = ref(0)
const userTotalPages = ref(0)

// 邀请码数据
const invites = ref<any[]>([])
const invitesLoading = ref(false)
const invitesPage = ref(1)
const invitesPageSize = ref(100)
const invitesTotal = ref(0)
const invitesTotalPages = ref(1)
const inviteStatusFilter = ref<'all' | 'unused' | 'used'>('all')
const inviteStatusFilterOptions = computed<{ value: 'all' | 'unused' | 'used'; label: string }[]>(() => [
  { value: 'all', label: t('admin.users.inviteFilterAll') },
  { value: 'unused', label: t('admin.users.inviteFilterUnused') },
  { value: 'used', label: t('admin.users.inviteFilterUsed') }
])

// 生成邀请码弹窗
const showInviteModal = ref(false)
const showGenerateModal = ref(false)
const inviteExpireDays = ref(7)
const inviteCount = ref(1)
const newInviteCode = ref('')
const newInviteCodes = ref<string[]>([])
const newInviteExpiresAt = ref('')
const inviteGenerating = ref(false)
const copied = ref(false)

const inviteCountOptions = computed(() => [
  { label: `1 ${t('admin.users.countUnit')}`, value: 1 },
  { label: `5 ${t('admin.users.countUnit')}`, value: 5 },
  { label: `20 ${t('admin.users.countUnit')}`, value: 20 },
  { label: `50 ${t('admin.users.countUnit')}`, value: 50 },
  { label: `100 ${t('admin.users.countUnit')}`, value: 100 }
])

const inviteExpireDaysOptions = computed(() => [
  { label: t('admin.users.day1'), value: 1 },
  { label: t('admin.users.day3'), value: 3 },
  { label: t('admin.users.day7'), value: 7 },
  { label: t('admin.users.day14'), value: 14 },
  { label: t('admin.users.day30'), value: 30 },
  { label: t('admin.users.permanentValid'), value: 0 }
])

// 重置密码弹窗
const showResetPasswordModal = ref(false)
const resetPasswordUser = ref<any>(null)
const resetPasswordForm = ref({ password: '', confirmPassword: '' })
const resetPasswordLoading = ref(false)

// 取消2FA弹窗
const showDisable2FAModal = ref(false)
const disable2FAUser = ref<any>(null)
const disable2FALoading = ref(false)

// 解绑GitHub弹窗
const showUnbindGitHubModal = ref(false)
const unbindGitHubUser = ref<any>(null)
const unbindGitHubLoading = ref(false)

// 登录记录弹窗
const showLoginRecordsModal = ref(false)
const loginRecordsUser = ref<any>(null)
const loginRecords = ref<any[]>([])
const loginRecordsLoading = ref(false)
const loginRecordsPage = ref(1)
const loginRecordsPageSize = ref(100)
const loginRecordsTotal = ref(0)
const loginRecordsTotalPages = ref(1)

// 发送站内信弹窗
const showSendMessageModal = ref(false)
const sendMessageUser = ref<any>(null)
const sendMessageForm = ref({ title: '', content: '' })
const sendMessageLoading = ref(false)

// 用户余额详情弹窗
const showBalanceModal = ref(false)
const balanceUser = ref<any>(null)
const balanceTab = ref('overview')
const balanceInfo = ref({ balance: 0, totalRecharge: 0, totalConsume: 0 })
const balanceInfoLoading = ref(false)
const balanceLogs = ref<any[]>([])
const balanceLogsLoading = ref(false)
const balanceLogsPage = ref(1)
const balanceLogsPageSize = ref(100)
const balanceLogsTotal = ref(0)
const balanceLogsTotalPages = ref(1)
const balanceLogsShowLotteryGiftOnly = ref(false)
const rechargeRecords = ref<any[]>([])
const rechargeRecordsLoading = ref(false)
const rechargeRecordsPage = ref(1)
const rechargeRecordsPageSize = ref(100)
const rechargeRecordsTotal = ref(0)
const rechargeRecordsTotalPages = ref(1)
const adjustBalanceForm = ref({
  type: 'add',
  amount: '',
  reason: ''
})
const adjustBalanceLoading = ref(false)

const balanceTabs = computed(() => [
  { key: 'overview', label: t('admin.users.balanceOverview') },
  { key: 'logs', label: t('admin.users.balanceLogs') },
  { key: 'recharge', label: t('admin.users.rechargeRecords') },
  { key: 'adjust', label: t('admin.users.adjustBalance') }
])

// 用户积分调整弹窗
const showPointsModal = ref(false)
const pointsUser = ref<any>(null)
const adjustPointsForm = ref({
  amount: '',
  reason: ''
})
const adjustPointsLoading = ref(false)

// 关联账号检测
const linkedAccountsData = ref<any>(null)
const linkedAccountsLoading = ref(false)
const linkedAccountsDays = ref(90)
const linkedAccountsExpandedIP = ref(new Set<string>())
const linkedAccountsExpandedEmail = ref(new Set<string>())
const linkedAccountsExpandedUsername = ref(new Set<string>())

const linkedAccountsDaysOptions = computed(() => [
  { label: `30 ${t('admin.users.daysUnit')}`, value: 30 },
  { label: `60 ${t('admin.users.daysUnit')}`, value: 60 },
  { label: `90 ${t('admin.users.daysUnit')}`, value: 90 },
  { label: `180 ${t('admin.users.daysUnit')}`, value: 180 },
  { label: `365 ${t('admin.users.daysUnit')}`, value: 365 }
])

const customerBaseUrl = computed(() => {
  const configuredUrl = import.meta.env.VITE_CUSTOMER_BASE_URL?.trim()
  return (configuredUrl || window.location.origin).replace(/\/+$/, '')
})

// 计算注册链接
const registerLink = computed(() => {
  return `${customerBaseUrl.value}/register/${newInviteCode.value}`
})
const normalizedUserSearchFields = computed(() => {
  return USER_SEARCH_FIELDS.filter(field => userSearchFields.value.includes(field))
})

// 操作菜单状态
const activeActionMenuUserId = ref<number | null>(null)
const actionMenuPosition = ref({ top: 0, right: 0 })

const activeActionUser = computed(() => {
  if (!activeActionMenuUserId.value) return null
  return users.value.find(u => u.id === activeActionMenuUserId.value) || null
})

function toggleActionMenu(userId: number, e?: MouseEvent) {
  if (e) {
    e.stopPropagation()
  }
  if (activeActionMenuUserId.value === userId) {
    activeActionMenuUserId.value = null
    return
  }
  if (e) {
    const btn = (e.currentTarget || e.target) as HTMLElement
    if (btn && typeof btn.getBoundingClientRect === 'function') {
      const rect = btn.getBoundingClientRect()
      const spaceBelow = window.innerHeight - rect.bottom
      const menuHeight = 240
      if (spaceBelow < menuHeight && rect.top > menuHeight) {
        actionMenuPosition.value = {
          top: Math.max(8, rect.top - menuHeight - 4),
          right: Math.max(12, window.innerWidth - rect.right)
        }
      } else {
        actionMenuPosition.value = {
          top: rect.bottom + 4,
          right: Math.max(12, window.innerWidth - rect.right)
        }
      }
    }
  }
  activeActionMenuUserId.value = userId
}

function closeActionMenu() {
  activeActionMenuUserId.value = null
}

function handleGlobalClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (target && !target.closest('.action-menu-container') && !target.closest('.kawaii-menu-panel')) {
    activeActionMenuUserId.value = null
  }
}

onMounted(async () => {
  document.addEventListener('click', handleGlobalClick)
  window.addEventListener('resize', closeActionMenu)
  window.addEventListener('scroll', closeActionMenu, true)
  await loadUsers()
  loadTabData(activeTab.value)
})

onUnmounted(() => {
  document.removeEventListener('click', handleGlobalClick)
  window.removeEventListener('resize', closeActionMenu)
  window.removeEventListener('scroll', closeActionMenu, true)
})

// 搜索防抖
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch([userSearch, userSearchExact, () => normalizedUserSearchFields.value.join(',')], () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    userPage.value = 1
    void loadUsers()
  }, 300)
})

async function loadUsers() {
  usersLoading.value = true
  try {
    const response = await api.users.list({
      page: userPage.value,
      pageSize: userPageSize.value,
      search: userSearch.value,
      searchFields: normalizedUserSearchFields.value.join(','),
      exact: userSearchExact.value
    })
    users.value = (response as any).users || response.items || []
    userTotal.value = response.total || 0
    userTotalPages.value = response.totalPages || 1
  } catch (err: any) {
    toast.error(t('admin.users.loadFailed') + ': ' + err.message)
  } finally {
    usersLoading.value = false
  }
}

async function loadInvites() {
  invitesLoading.value = true
  try {
    const response = await api.auth.getInvites({
      page: invitesPage.value,
      pageSize: invitesPageSize.value,
      ...(inviteStatusFilter.value !== 'all' ? { status: inviteStatusFilter.value } : {})
    })
    invites.value = response.invites || []
    invitesTotal.value = response.total || 0
    invitesTotalPages.value = response.totalPages || 1
    if (invitesPage.value > invitesTotalPages.value && invitesTotalPages.value > 0) {
      invitesPage.value = invitesTotalPages.value
      await loadInvites()
    }
  } finally {
    invitesLoading.value = false
  }
}

function loadTabData(tab: string) {
  if (tab === 'invites' && invites.value.length === 0) {
    void loadInvites()
  }
  if (tab === 'linkedAccounts' && !linkedAccountsData.value) {
    void loadLinkedAccounts()
  }
}

function switchTab(tab: TabType) {
  activeTab.value = tab
  loadTabData(tab)
}

function openGenerateModal() {
  inviteExpireDays.value = 7
  inviteCount.value = 1
  showGenerateModal.value = true
}

async function generateInvite() {
  inviteGenerating.value = true
  try {
    const payload = {
      ...(inviteExpireDays.value > 0 ? { expiresInDays: inviteExpireDays.value } : {}),
      count: inviteCount.value
    }
    const response = await api.auth.generateInvite(payload)

    if (response.codes) {
      newInviteCodes.value = response.codes
      newInviteCode.value = ''
    } else {
      newInviteCode.value = response.code || ''
      newInviteCodes.value = []
    }
    newInviteExpiresAt.value = response.expiresAt || ''

    showGenerateModal.value = false
    showInviteModal.value = true
    if (activeTab.value === 'invites') {
      void loadInvites()
    }
  } catch (err: any) {
    toast.error(t('admin.users.generateFailed') + ': ' + err.message)
  } finally {
    inviteGenerating.value = false
  }
}

async function deleteInvite(invite: any) {
  if (!confirm(t('admin.users.confirmDeleteInvite', { code: invite.code }))) return

  try {
    await api.auth.deleteInvite(invite.id)
    if (invites.value.length === 1 && invitesPage.value > 1) {
      invitesPage.value = invitesPage.value - 1
    }
    await loadInvites()
    toast.success(t('admin.users.inviteDeleted'))
  } catch (err: any) {
    toast.error(t('admin.users.deleteFailed') + ': ' + err.message)
  }
}

async function copyCode() {
  const textToCopy = newInviteCodes.value.length > 0
    ? newInviteCodes.value.join('\n')
    : newInviteCode.value

  try {
    await navigator.clipboard.writeText(textToCopy)
    copied.value = true
    setTimeout(() => { copied.value = false }, 2000)
  } catch {
    const input = document.createElement('textarea')
    input.value = textToCopy
    document.body.appendChild(input)
    input.select()
    document.execCommand('copy')
    document.body.removeChild(input)
    copied.value = true
    setTimeout(() => { copied.value = false }, 2000)
  }
}

function copyLink() {
  navigator.clipboard.writeText(registerLink.value).then(() => {
    copied.value = true
    setTimeout(() => { copied.value = false }, 2000)
  })
}

// 切换用户状态（封禁/解封）
async function toggleUserStatus(user: any) {
  closeActionMenu()
  const newStatus = user.status === 'active' ? 'banned' : 'active'

  const confirmMsg = newStatus === 'banned'
    ? t('admin.users.confirmBan', { name: user.username })
    : t('admin.users.confirmUnban', { name: user.username })

  if (!confirm(confirmMsg)) return

  try {
    await api.users.updateStatus(user.id, newStatus)
    const idx = users.value.findIndex(u => u.id === user.id)
    if (idx !== -1) {
      users.value[idx] = { ...users.value[idx], status: newStatus }
    }
    toast.success(newStatus === 'banned' ? t('admin.users.userBanned') : t('admin.users.userUnbanned'))
  } catch (err: any) {
    toast.error(err.message)
    await loadUsers()
  }
}

// 切换用户管理员角色
async function toggleUserRole(user: any) {
  closeActionMenu()
  const newRole = user.role === 'admin' ? 'user' : 'admin'

  if (newRole === 'admin' && user.status !== 'active') {
    toast.error(t('admin.users.onlyActiveCanBeAdmin'))
    return
  }

  const confirmMsg = newRole === 'admin'
    ? t('admin.users.confirmPromoteAdmin', { name: user.username })
    : t('admin.users.confirmDemoteAdmin', { name: user.username })

  if (!confirm(confirmMsg)) return

  try {
    await api.users.updateRole(user.id, newRole)
    const idx = users.value.findIndex(u => u.id === user.id)
    if (idx !== -1) {
      users.value[idx] = { ...users.value[idx], role: newRole }
    }
    toast.success(newRole === 'admin' ? t('admin.users.userPromotedAdmin') : t('admin.users.userDemotedAdmin'))
  } catch (err: any) {
    toast.error(err.details || err.message)
    await loadUsers()
  }
}

// 查看用户实例
function viewUserInstances(user: any) {
  closeActionMenu()
  void router.push({ path: instancesPath(), query: { userId: user.id } })
}

// 打开重置密码确认弹窗
function openResetPasswordModal(user: any) {
  closeActionMenu()
  resetPasswordUser.value = user
  resetPasswordForm.value = { password: '', confirmPassword: '' }
  showResetPasswordModal.value = true
}

// 重置密码
async function resetPassword() {
  if (!resetPasswordForm.value.password || resetPasswordForm.value.password !== resetPasswordForm.value.confirmPassword) {
    toast.error(t('auth.passwordMismatch'))
    return
  }

  resetPasswordLoading.value = true

  try {
    await api.users.resetPassword(resetPasswordUser.value.id, resetPasswordForm.value.password)
    resetPasswordForm.value = { password: '', confirmPassword: '' }
    showResetPasswordModal.value = false
    toast.success(t('admin.users.passwordResetSuccess'))
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    resetPasswordLoading.value = false
  }
}

// 打开取消2FA弹窗
function openDisable2FAModal(user: any) {
  closeActionMenu()
  disable2FAUser.value = user
  showDisable2FAModal.value = true
}

// 取消2FA
async function disable2FA() {
  disable2FALoading.value = true

  try {
    await api.users.disable2FA(disable2FAUser.value.id)
    showDisable2FAModal.value = false
    toast.success(t('admin.users.twoFADisabled'))
    await loadUsers()
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    disable2FALoading.value = false
  }
}

// 打开解绑GitHub弹窗
function openUnbindGitHubModal(user: any) {
  closeActionMenu()
  unbindGitHubUser.value = user
  showUnbindGitHubModal.value = true
}

// 解绑GitHub
async function unbindGitHub() {
  unbindGitHubLoading.value = true

  try {
    await api.users.unbindOAuth(unbindGitHubUser.value.id, 'github')
    showUnbindGitHubModal.value = false
    toast.success(t('admin.users.githubUnbound'))
    await loadUsers()
  } catch (err: any) {
    toast.error(err.message)
  } finally {
    unbindGitHubLoading.value = false
  }
}

// 打开登录记录弹窗
async function openLoginRecordsModal(user: any) {
  loginRecordsUser.value = user
  loginRecordsPage.value = 1
  loginRecords.value = []
  showLoginRecordsModal.value = true
  await loadLoginRecords()
}

// 加载登录记录
async function loadLoginRecords() {
  if (!loginRecordsUser.value) return

  loginRecordsLoading.value = true
  try {
    const response = await api.users.getLoginRecords(loginRecordsUser.value.id, {
      page: loginRecordsPage.value,
      pageSize: loginRecordsPageSize.value
    })
    loginRecords.value = response.records || []
    loginRecordsTotal.value = response.total || 0
    loginRecordsTotalPages.value = response.totalPages || 1
  } catch (err: any) {
    toast.error(t('admin.users.loadLoginRecordsFailed') + ': ' + err.message)
  } finally {
    loginRecordsLoading.value = false
  }
}

// 加载关联账号检测数据
async function loadLinkedAccounts() {
  linkedAccountsLoading.value = true
  try {
    const response = await api.users.detectLinkedAccounts(linkedAccountsDays.value)
    linkedAccountsData.value = response
    linkedAccountsExpandedIP.value = new Set()
    linkedAccountsExpandedEmail.value = new Set()
    linkedAccountsExpandedUsername.value = new Set()
  } catch (err: any) {
    toast.error(t('admin.users.loadLinkedAccountsFailed') + ': ' + err.message)
  } finally {
    linkedAccountsLoading.value = false
  }
}

// 切换IP组展开状态
function toggleIPGroup(ip: string) {
  if (linkedAccountsExpandedIP.value.has(ip)) {
    linkedAccountsExpandedIP.value.delete(ip)
  } else {
    linkedAccountsExpandedIP.value.add(ip)
  }
}

// 切换邮箱组展开状态
function toggleEmailGroup(pattern: string) {
  if (linkedAccountsExpandedEmail.value.has(pattern)) {
    linkedAccountsExpandedEmail.value.delete(pattern)
  } else {
    linkedAccountsExpandedEmail.value.add(pattern)
  }
}

// 切换用户名组展开状态
function toggleUsernameGroup(pattern: string) {
  if (linkedAccountsExpandedUsername.value.has(pattern)) {
    linkedAccountsExpandedUsername.value.delete(pattern)
  } else {
    linkedAccountsExpandedUsername.value.add(pattern)
  }
}

// 打开发送站内信弹窗
function openSendMessageModal(user: any) {
  closeActionMenu()
  sendMessageUser.value = user
  sendMessageForm.value = { title: '', content: '' }
  showSendMessageModal.value = true
}

// 发送站内信
async function sendMessage() {
  if (!sendMessageUser.value) return

  if (!sendMessageForm.value.title.trim()) {
    toast.error(t('admin.users.messageTitleRequired'))
    return
  }
  if (!sendMessageForm.value.content.trim()) {
    toast.error(t('admin.users.messageContentRequired'))
    return
  }

  sendMessageLoading.value = true
  try {
    await api.inbox.sendToUser(sendMessageUser.value.id, {
      title: sendMessageForm.value.title.trim(),
      content: sendMessageForm.value.content.trim()
    })
    toast.success(t('admin.users.messageSent'))
    showSendMessageModal.value = false
    sendMessageForm.value = { title: '', content: '' }
    sendMessageUser.value = null
  } catch (err: any) {
    toast.error(t('admin.users.messageSendFailed') + ': ' + err.message)
  } finally {
    sendMessageLoading.value = false
  }
}

// 打开用户余额详情弹窗
async function openBalanceModal(user: any) {
  balanceUser.value = user
  balanceTab.value = 'overview'
  balanceInfo.value = { balance: user.balance || 0, totalRecharge: 0, totalConsume: 0 }
  balanceLogs.value = []
  balanceLogsPage.value = 1
  balanceLogsShowLotteryGiftOnly.value = false
  rechargeRecords.value = []
  rechargeRecordsPage.value = 1
  adjustBalanceForm.value = { type: 'add', amount: '', reason: '' }
  showBalanceModal.value = true
  await loadBalanceInfo()
}

// 加载用户余额信息
async function loadBalanceInfo() {
  if (!balanceUser.value) return
  balanceInfoLoading.value = true
  try {
    const res = await api.admin.getUserBalance(balanceUser.value.id)
    balanceInfo.value = {
      balance: res.balance,
      totalRecharge: res.totalRecharge,
      totalConsume: res.totalConsume
    }
  } catch (err: any) {
    toast.error(t('admin.users.loadBalanceFailed') + ': ' + err.message)
  } finally {
    balanceInfoLoading.value = false
  }
}

// 加载余额明细
async function loadBalanceLogs() {
  if (!balanceUser.value) return
  balanceLogsLoading.value = true
  try {
    const res = await api.admin.getUserBalanceLogs(balanceUser.value.id, {
      page: balanceLogsPage.value,
      pageSize: balanceLogsPageSize.value,
      lotteryGift: balanceLogsShowLotteryGiftOnly.value ? 'only' : 'exclude'
    })
    balanceLogs.value = res.logs || []
    balanceLogsTotal.value = res.total || 0
    balanceLogsTotalPages.value = Math.ceil(res.total / balanceLogsPageSize.value) || 1
  } catch (err: any) {
    toast.error(t('admin.users.loadBalanceLogsFailed') + ': ' + err.message)
  } finally {
    balanceLogsLoading.value = false
  }
}

// 加载充值记录
async function loadRechargeRecords() {
  if (!balanceUser.value) return
  rechargeRecordsLoading.value = true
  try {
    const res = await api.admin.getRechargeOrders({
      page: rechargeRecordsPage.value,
      pageSize: rechargeRecordsPageSize.value,
      userId: balanceUser.value.id
    })
    rechargeRecords.value = res.records || []
    rechargeRecordsTotal.value = res.total || 0
    rechargeRecordsTotalPages.value = Math.ceil(res.total / rechargeRecordsPageSize.value) || 1
  } catch (err: any) {
    toast.error(t('admin.users.loadRechargeRecordsFailed') + ': ' + err.message)
  } finally {
    rechargeRecordsLoading.value = false
  }
}

// 切换余额弹窗TAB
function switchBalanceTab(tab: string) {
  balanceTab.value = tab
  if (tab === 'logs' && balanceLogs.value.length === 0) {
    void loadBalanceLogs()
  } else if (tab === 'recharge' && rechargeRecords.value.length === 0) {
    void loadRechargeRecords()
  }
}

// 提交调整余额
async function submitAdjustBalance() {
  if (!balanceUser.value) return

  const amount = parseFloat(adjustBalanceForm.value.amount)
  if (isNaN(amount) || amount <= 0) {
    toast.error(t('admin.users.invalidAmount'))
    return
  }

  if (!adjustBalanceForm.value.reason.trim()) {
    toast.error(t('admin.users.reasonRequired'))
    return
  }

  const finalAmount = adjustBalanceForm.value.type === 'add' ? amount : -amount

  adjustBalanceLoading.value = true
  try {
    await api.admin.adjustUserBalance(
      balanceUser.value.id,
      finalAmount,
      adjustBalanceForm.value.reason.trim()
    )
    toast.success(t('admin.users.balanceAdjusted'))
    adjustBalanceForm.value = { type: 'add', amount: '', reason: '' }
    await loadBalanceInfo()
    if (balanceLogs.value.length > 0) {
      await loadBalanceLogs()
    }
    await loadUsers()
    balanceTab.value = 'overview'
  } catch (err: any) {
    toast.error(t('admin.users.balanceAdjustFailed') + ': ' + err.message)
  } finally {
    adjustBalanceLoading.value = false
  }
}

// 打开积分调整弹窗
function openPointsModal(user: any) {
  pointsUser.value = user
  adjustPointsForm.value = { amount: '', reason: '' }
  showPointsModal.value = true
}

// 提交调整积分
async function submitAdjustPoints() {
  if (!pointsUser.value) return

  const amount = parseInt(adjustPointsForm.value.amount)
  if (isNaN(amount) || amount === 0) {
    toast.error(t('admin.users.invalidPointsAmount'))
    return
  }

  if (!adjustPointsForm.value.reason.trim()) {
    toast.error(t('admin.users.reasonRequired'))
    return
  }

  adjustPointsLoading.value = true
  try {
    await api.entertainment.adminAdjustPoints(
      pointsUser.value.id,
      amount,
      adjustPointsForm.value.reason.trim()
    )
    toast.success(t('admin.users.pointsAdjusted'))
    showPointsModal.value = false
    await loadUsers()
  } catch (err: any) {
    toast.error(t('admin.users.pointsAdjustFailed') + ': ' + err.message)
  } finally {
    adjustPointsLoading.value = false
  }
}

// 获取余额记录类型标签
function getBalanceLogTypeLabel(type: string): string {
  const labels: Record<string, string> = {
    recharge: t('admin.users.balanceType.recharge'),
    consume: t('admin.users.balanceType.consume'),
    refund: t('admin.users.balanceType.refund'),
    admin_adjust: t('admin.users.balanceType.admin_adjust'),
    gift: t('admin.users.balanceType.gift'),
    transfer_fee: t('admin.users.balanceType.transfer_fee'),
    transfer_refund: t('admin.users.balanceType.transfer_refund')
  }
  return labels[type] || type
}

function getRechargeBadgeVariant(status: string): 'success' | 'warning' | 'error' | 'default' {
  switch (status) {
    case 'completed':
      return 'success'
    case 'pending':
      return 'warning'
    case 'failed':
      return 'error'
    default:
      return 'default'
  }
}

function formatLocation(record: any): string {
  const parts = [
    normalizeCountryName(record.country, normalizedCountryLabels.value),
    record.region,
    record.city
  ].filter(Boolean)
  return parts.length > 0 ? parts.join(', ') : '-'
}

function formatDevice(userAgent: string): string {
  if (!userAgent) return '-'
  if (userAgent.includes('Windows')) return 'Windows'
  if (userAgent.includes('Mac OS')) return 'macOS'
  if (userAgent.includes('Linux') && !userAgent.includes('Android')) return 'Linux'
  if (userAgent.includes('Android')) return 'Android'
  if (userAgent.includes('iPhone')) return 'iPhone'
  if (userAgent.includes('iPad')) return 'iPad'
  return '-'
}

function formatBrowser(userAgent: string): string {
  if (!userAgent) return '-'
  if (userAgent.includes('Edg')) return 'Edge'
  if (userAgent.includes('Chrome')) return 'Chrome'
  if (userAgent.includes('Firefox')) return 'Firefox'
  if (userAgent.includes('Safari') && !userAgent.includes('Chrome')) return 'Safari'
  if (userAgent.includes('Opera') || userAgent.includes('OPR')) return 'Opera'
  return '-'
}

function handleUserPageSizeChange() {
  userPage.value = 1
  void loadUsers()
}

function handleInvitesPageSizeChange() {
  invitesPage.value = 1
  void loadInvites()
}

function handleInviteStatusFilterChange(status: 'all' | 'unused' | 'used') {
  if (inviteStatusFilter.value === status) return
  inviteStatusFilter.value = status
  invitesPage.value = 1
  void loadInvites()
}

function getInviteStatus(invite: any): { label: string; variant: 'success' | 'error' | 'warning' } {
  if (invite.used_by || invite.usedBy) {
    return { label: t('admin.users.inviteUsed'), variant: 'success' }
  }
  if (invite.expires_at || invite.expiresAt) {
    const expiry = new Date(invite.expires_at || invite.expiresAt)
    if (expiry < new Date()) {
      return { label: t('admin.users.inviteExpired'), variant: 'error' }
    }
  }
  return { label: t('admin.users.inviteUnused'), variant: 'warning' }
}

function formatDate(dateStr: string | null | undefined): string {
  if (!dateStr) return '-'
  const date = new Date(dateStr)
  return date.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function formatRegisteredAge(createdAt: string | null | undefined): string {
  if (!createdAt) return '-'
  const createdDate = new Date(createdAt)
  if (Number.isNaN(createdDate.getTime())) return '-'

  const today = new Date()
  const msPerDay = 24 * 60 * 60 * 1000
  const todayDay = Date.UTC(today.getFullYear(), today.getMonth(), today.getDate()) / msPerDay
  const createdDay = Date.UTC(createdDate.getFullYear(), createdDate.getMonth(), createdDate.getDate()) / msPerDay
  const days = Math.max(0, Math.floor(todayDay - createdDay))

  return days === 0
    ? t('admin.users.registeredNew')
    : t('admin.users.registeredDays', { days })
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- 页面头部 -->
    <PageHeader
      data-reveal
      :title="t('admin.users.title')"
      :description="t('admin.users.description')"
    >
      <template #actions>
        <Button
          variant="primary"
          @click="openGenerateModal"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2]" />
          </template>
          {{ t('admin.users.generateInvite') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Tab 切换 -->
    <div data-reveal class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
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
        <span>{{ tab.label }}</span>
        <Badge
          v-if="tab.badge !== undefined"
          size="sm"
          :variant="activeTab === tab.key ? 'info' : 'default'"
        >
          {{ tab.badge }}
        </Badge>
      </button>
    </div>

    <!-- 用户管理 Tab -->
    <div v-if="activeTab === 'users'" class="space-y-5">
      <!-- 搜索与筛选 Card -->
      <Card data-reveal padding="sm">
        <div class="flex flex-col gap-4 xl:flex-row xl:items-center xl:justify-between">
          <div class="w-full xl:max-w-sm">
            <Input
              v-model="userSearch"
              type="text"
              :placeholder="t('admin.users.searchPlaceholder')"
              clearable
            >
              <template #prefix>
                <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>
          <div class="flex flex-wrap items-center gap-4">
            <div class="flex flex-wrap items-center gap-3">
              <span class="text-xs font-semibold text-[var(--text-secondary)] whitespace-nowrap">{{ t('admin.users.searchRange') }}</span>
              <label
                v-for="field in USER_SEARCH_FIELD_OPTIONS"
                :key="field.value"
                class="inline-flex items-center gap-1.5 text-xs text-[var(--text-secondary)] whitespace-nowrap cursor-pointer select-none transition-colors hover:text-[var(--text-primary)]"
              >
                <input
                  v-model="userSearchFields"
                  :value="field.value"
                  type="checkbox"
                  class="h-4 w-4 shrink-0 rounded border border-[var(--border-color)] bg-[var(--bg-surface-soft)] accent-[var(--accent)] cursor-pointer disabled:cursor-not-allowed disabled:opacity-50"
                  :disabled="normalizedUserSearchFields.length === 1 && normalizedUserSearchFields.includes(field.value)"
                />
                <span>{{ t(field.label) }}</span>
              </label>
            </div>
            <label class="inline-flex items-center gap-1.5 text-xs text-[var(--text-secondary)] whitespace-nowrap cursor-pointer select-none transition-colors hover:text-[var(--text-primary)]">
              <input
                v-model="userSearchExact"
                type="checkbox"
                class="h-4 w-4 shrink-0 rounded border border-[var(--border-color)] bg-[var(--bg-surface-soft)] accent-[var(--accent)] cursor-pointer"
              />
              <span>{{ t('admin.users.exactMatch') }}</span>
            </label>
          </div>
        </div>
      </Card>

      <SkeletonLoader v-if="usersLoading" type="list" />

      <Card v-else-if="users.length === 0" padding="none">
        <EmptyState
          :title="userSearch ? t('admin.users.noMatchingUsers') : t('admin.users.noUsers')"
        />
      </Card>

      <!-- 用户列表 -->
      <template v-else>
        <!-- 移动端卡片视图 -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="user in users"
            :key="user.id"
            padding="sm"
            class="relative transition-all duration-200"
            :class="{ 'z-20 ring-1 ring-[var(--accent)]': activeActionMenuUserId === user.id }"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-center gap-3">
                <UserAvatar
                  :username="user.username"
                  :email="user.email"
                  :avatar-style="user.avatarStyle || ''"
                  :badge-id="user.avatarBadgeId || null"
                  :size="40"
                />
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ user.username }}</div>
                  <div class="truncate text-xs text-[var(--text-secondary)]">{{ user.email || t('admin.users.noEmail') }}</div>
                  <div class="mt-0.5 font-mono tabular-nums text-xs text-[var(--text-tertiary)]">#{{ user.id }}</div>
                </div>
              </div>
              <div class="flex shrink-0 flex-col items-end gap-1.5">
                <Badge size="sm" :variant="user.role === 'admin' ? 'info' : 'default'">
                  {{ user.role === 'admin' ? t('admin.users.admin') : t('admin.users.user') }}
                </Badge>
                <Badge size="sm" :variant="user.status === 'active' ? 'success' : 'error'" :dot="true">
                  {{ user.status === 'active' ? t('admin.users.active') : t('admin.users.banned') }}
                </Badge>
              </div>
            </div>

            <div class="mt-4 grid gap-2.5 text-xs text-[var(--text-secondary)] sm:grid-cols-2">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.balance') }}</div>
                <button
                  class="mt-1 text-left font-mono tabular-nums text-sm font-semibold hover:text-[var(--accent)] transition-colors"
                  :class="user.balance > 0 ? 'text-[var(--success)]' : 'text-[var(--text-secondary)]'"
                  :title="t('admin.users.viewBalance')"
                  @click="openBalanceModal(user)"
                >
                  ¥{{ (user.balance || 0).toFixed(2) }}
                </button>
                <div v-if="user.totalConsume > 0" class="mt-1 text-[11px] text-[var(--text-tertiary)] truncate">
                  {{ t('admin.users.consumed') }} ¥{{ (user.totalConsume || 0).toFixed(2) }}
                </div>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.points') }}</div>
                <button
                  class="mt-1 text-left font-mono tabular-nums text-sm font-semibold hover:text-[var(--accent)] transition-colors"
                  :class="user.points > 0 ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'"
                  :title="t('admin.users.adjustPoints')"
                  @click="openPointsModal(user)"
                >
                  {{ user.points || 0 }}
                </button>
                <div v-if="(user.totalEarnedPoints - user.points) > 0" class="mt-1 text-[11px] text-[var(--text-tertiary)] truncate">
                  {{ t('admin.users.spent') }} {{ user.totalEarnedPoints - user.points }}
                </div>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.allInstances') }}</div>
                <div class="mt-1 flex items-center gap-2 text-[var(--text-primary)] font-medium">
                  <span>{{ user.instanceCount || 0 }} {{ t('admin.users.instances') }}</span>
                  <button
                    v-if="user.instanceCount > 0"
                    type="button"
                    class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors cursor-pointer inline-flex items-center justify-center p-0.5"
                    :title="t('admin.users.viewInstances')"
                    @click="viewUserInstances(user)"
                  >
                    <Server class="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>

              <button
                class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5 text-left sm:col-span-2 transition-colors hover:border-[var(--border-hover)]"
                :title="t('admin.users.viewLoginRecords')"
                @click="openLoginRecordsModal(user)"
              >
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.userActivity') }}</div>
                <div class="mt-1 text-[var(--text-primary)] font-medium">
                  {{ formatRegisteredAge(user.createdAt) }}
                  <template v-if="user.lastLogin"> · {{ formatDate(user.lastLogin.createdAt) }}</template>
                </div>
                <div class="mt-1 font-mono text-[11px] text-[var(--text-tertiary)] truncate">{{ user.lastLogin?.ip || t('admin.users.noLoginRecord') }}</div>
              </button>
            </div>

            <div class="mt-3.5 flex items-center justify-end gap-1.5 border-t border-[var(--border-color)] pt-3">
              <Button
                size="sm"
                variant="ghost"
                class="!min-h-0 h-8 w-8 !p-0 text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
                :title="t('admin.users.viewInstances')"
                @click="viewUserInstances(user)"
              >
                <Server class="w-4 h-4" />
              </Button>

              <Button
                v-if="user.role !== 'admin'"
                size="sm"
                variant="ghost"
                class="!min-h-0 h-8 w-8 !p-0"
                :class="user.status === 'active' ? 'text-[var(--text-secondary)] hover:text-[var(--error)]' : 'text-[var(--warning)] hover:text-[var(--success)]'"
                :title="user.status === 'active' ? t('admin.users.ban') : t('admin.users.unban')"
                @click="toggleUserStatus(user)"
              >
                <UserX v-if="user.status === 'active'" class="w-4 h-4" />
                <UserCheck v-else class="w-4 h-4" />
              </Button>

              <div class="relative inline-block text-left action-menu-container">
                <Button
                  size="sm"
                  variant="ghost"
                  class="!min-h-0 h-8 w-8 !p-0 text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
                  :class="{ 'bg-[var(--bg-surface-soft)] text-[var(--text-primary)]': activeActionMenuUserId === user.id }"
                  :title="t('admin.users.moreActions')"
                  @click.stop="toggleActionMenu(user.id, $event)"
                >
                  <MoreHorizontal class="w-4 h-4" />
                </Button>
              </div>
            </div>
          </Card>
        </div>

        <!-- 桌面端表格视图 -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[6%]" />
              <col class="w-[22%]" />
              <col class="w-[8%]" />
              <col class="w-[8%]" />
              <col class="w-[12%]" />
              <col class="w-[10%]" />
              <col class="w-[10%]" />
              <col class="w-[14%]" />
              <col class="w-[10%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">ID</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.userInfo') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.role') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.status') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.balance') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.points') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.allInstances') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.userActivity') }}</th>
                <th class="text-right text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="user in users"
                :key="user.id"
                class="hover:bg-[var(--bg-surface-soft)] transition-colors duration-150"
              >
                <!-- 用户 ID -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <span class="text-xs font-mono tabular-nums text-[var(--text-tertiary)]">#{{ user.id }}</span>
                </td>

                <!-- 用户信息 -->
                <td class="px-4 py-3.5">
                  <div class="flex items-center gap-3 min-w-0">
                    <UserAvatar
                      :username="user.username"
                      :email="user.email"
                      :avatar-style="user.avatarStyle || ''"
                      :badge-id="user.avatarBadgeId || null"
                      :size="36"
                    />
                    <div class="min-w-0 flex-1">
                      <div class="text-sm font-medium text-[var(--text-primary)] truncate">{{ user.username }}</div>
                      <div class="text-xs text-[var(--text-tertiary)] truncate">{{ user.email || t('admin.users.noEmail') }}</div>
                    </div>
                  </div>
                </td>

                <!-- 角色 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <Badge size="sm" :variant="user.role === 'admin' ? 'info' : 'default'">
                    {{ user.role === 'admin' ? t('admin.users.admin') : t('admin.users.user') }}
                  </Badge>
                </td>

                <!-- 状态 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <Badge size="sm" :variant="user.status === 'active' ? 'success' : 'error'" :dot="true">
                    {{ user.status === 'active' ? t('admin.users.active') : t('admin.users.banned') }}
                  </Badge>
                </td>

                <!-- 余额 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <div class="flex flex-col gap-0.5">
                    <button
                      class="text-sm font-mono tabular-nums font-semibold hover:text-[var(--accent)] transition-colors text-left"
                      :class="user.balance > 0 ? 'text-[var(--success)]' : 'text-[var(--text-secondary)]'"
                      :title="t('admin.users.viewBalance')"
                      @click="openBalanceModal(user)"
                    >
                      ¥{{ (user.balance || 0).toFixed(2) }}
                    </button>
                    <span
                      v-if="user.totalConsume > 0"
                      class="text-xs text-[var(--text-tertiary)] truncate"
                      :title="t('admin.users.totalConsumed')"
                    >
                      {{ t('admin.users.consumed') }} ¥{{ (user.totalConsume || 0).toFixed(2) }}
                    </span>
                  </div>
                </td>

                <!-- 积分 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <div class="flex flex-col gap-0.5">
                    <button
                      class="text-sm font-mono tabular-nums font-semibold hover:text-[var(--accent)] transition-colors text-left"
                      :class="user.points > 0 ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'"
                      :title="t('admin.users.adjustPoints')"
                      @click="openPointsModal(user)"
                    >
                      {{ user.points || 0 }}
                    </button>
                    <span
                      v-if="(user.totalEarnedPoints - user.points) > 0"
                      class="text-xs text-[var(--text-tertiary)] truncate"
                      :title="t('admin.users.spentPoints')"
                    >
                      {{ t('admin.users.spent') }} {{ user.totalEarnedPoints - user.points }}
                    </span>
                  </div>
                </td>

                <!-- 所有实例 -->
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <div v-if="user.quota" class="flex items-center gap-1.5">
                    <span class="text-sm text-[var(--text-secondary)]">{{ user.instanceCount || 0 }} {{ t('admin.users.instances') }}</span>
                    <button
                      v-if="user.instanceCount > 0"
                      type="button"
                      class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors cursor-pointer inline-flex items-center justify-center p-0.5"
                      :title="t('admin.users.viewInstances')"
                      @click="viewUserInstances(user)"
                    >
                      <Server class="w-3.5 h-3.5" />
                    </button>
                  </div>
                  <span v-else class="text-sm text-[var(--text-tertiary)]">-</span>
                </td>

                <!-- 用户活动 -->
                <td class="px-4 py-3.5 align-middle">
                  <button
                    v-if="user.lastLogin"
                    class="flex min-h-[38px] w-full flex-col justify-center rounded-xl px-2 py-1 text-left transition-colors hover:bg-[var(--bg-surface-soft)]"
                    :title="t('admin.users.viewLoginRecords')"
                    @click="openLoginRecordsModal(user)"
                  >
                    <div class="flex items-center gap-1.5 text-xs text-[var(--text-secondary)] truncate">
                      <span :title="t('admin.users.createdAt') + ' ' + formatDate(user.createdAt)">
                        {{ formatRegisteredAge(user.createdAt) }}
                      </span>
                      <span class="text-[var(--text-tertiary)]">·</span>
                      <span class="truncate">{{ formatDate(user.lastLogin.createdAt) }}</span>
                    </div>
                    <div class="text-xs font-mono text-[var(--text-tertiary)] truncate max-w-[200px]" :title="user.lastLogin.ip">
                      {{ user.lastLogin.ip }}
                    </div>
                  </button>
                  <button
                    v-else
                    class="flex min-h-[38px] w-full flex-col justify-center rounded-xl px-2 py-1 text-left transition-colors hover:bg-[var(--bg-surface-soft)]"
                    :title="t('admin.users.viewLoginRecords')"
                    @click="openLoginRecordsModal(user)"
                  >
                    <span class="text-xs text-[var(--text-secondary)]" :title="t('admin.users.createdAt') + ' ' + formatDate(user.createdAt)">
                      {{ formatRegisteredAge(user.createdAt) }}
                    </span>
                    <span class="text-xs text-[var(--text-tertiary)]">{{ t('admin.users.noLoginRecord') }}</span>
                  </button>
                </td>

                <!-- 操作 -->
                <td class="px-4 py-3.5 text-right whitespace-nowrap">
                  <div class="inline-flex items-center justify-end gap-1">
                    <Button
                      size="sm"
                      variant="ghost"
                      class="!min-h-0 h-8 w-8 !p-0 text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
                      :title="t('admin.users.viewInstances')"
                      @click="viewUserInstances(user)"
                    >
                      <Server class="w-4 h-4" />
                    </Button>

                    <Button
                      v-if="user.role !== 'admin'"
                      size="sm"
                      variant="ghost"
                      class="!min-h-0 h-8 w-8 !p-0"
                      :class="user.status === 'active' ? 'text-[var(--text-secondary)] hover:text-[var(--error)]' : 'text-[var(--warning)] hover:text-[var(--success)]'"
                      :title="user.status === 'active' ? t('admin.users.ban') : t('admin.users.unban')"
                      @click="toggleUserStatus(user)"
                    >
                      <UserX v-if="user.status === 'active'" class="w-4 h-4" />
                      <UserCheck v-else class="w-4 h-4" />
                    </Button>

                    <div class="relative inline-block text-left action-menu-container">
                      <Button
                        size="sm"
                        variant="ghost"
                        class="!min-h-0 h-8 w-8 !p-0 text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
                        :class="{ 'bg-[var(--bg-surface-soft)] text-[var(--text-primary)]': activeActionMenuUserId === user.id }"
                        :title="t('admin.users.moreActions')"
                        @click.stop="toggleActionMenu(user.id, $event)"
                      >
                        <MoreHorizontal class="w-4 h-4" />
                      </Button>
                    </div>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 底部分页 -->
        <div v-if="userTotal > 0" class="pt-2 px-1">
          <Pagination
            :page="userPage"
            :page-size="userPageSize"
            :total="userTotal"
            :show-size-changer="true"
            :page-size-options="[10, 30, 50, 100]"
            @update:page="(p) => { userPage = p; void loadUsers() }"
            @update:page-size="(ps) => { userPageSize = ps; handleUserPageSizeChange() }"
          />
        </div>
      </template>
    </div>

    <!-- 邀请码列表 Tab -->
    <div v-if="activeTab === 'invites'" class="space-y-5">
      <!-- 状态筛选栏 -->
      <div class="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-3">
        <div>
          <h3 class="text-base font-semibold text-[var(--text-primary)] tracking-tight">
            {{ t('admin.users.invites') }}
          </h3>
          <p class="text-xs text-[var(--text-secondary)] mt-0.5">
            {{ t('admin.users.description') }}
          </p>
        </div>
        <div class="inline-flex items-center gap-1 rounded-full p-1 bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <button
            v-for="option in inviteStatusFilterOptions"
            :key="option.value"
            type="button"
            :disabled="invitesLoading"
            class="px-3.5 py-1.5 rounded-full text-xs font-medium transition-all duration-200 active:scale-95 disabled:cursor-not-allowed disabled:opacity-50 select-none"
            :class="inviteStatusFilter === option.value
              ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="handleInviteStatusFilterChange(option.value)"
          >
            {{ option.label }}
          </button>
        </div>
      </div>

      <SkeletonLoader v-if="invitesLoading" type="list" />

      <Card v-else-if="invites.length === 0" padding="none">
        <EmptyState
          :title="inviteStatusFilter === 'all' ? t('admin.users.noInvites') : t('admin.users.noMatchingInvites')"
        />
      </Card>

      <template v-else>
        <!-- 移动端邀请码列表 -->
        <div class="space-y-3 lg:hidden">
          <Card
            v-for="invite in invites"
            :key="invite.id"
            padding="sm"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-mono tabular-nums text-sm font-semibold text-[var(--text-primary)] break-all">{{ invite.code }}</div>
                <div class="mt-2">
                  <Badge size="sm" :variant="getInviteStatus(invite).variant">
                    {{ getInviteStatus(invite).label }}
                  </Badge>
                </div>
              </div>
              <Button
                size="sm"
                variant="ghost"
                class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,transparent)] shrink-0"
                @click="deleteInvite(invite)"
              >
                <template #icon>
                  <Trash2 class="w-3.5 h-3.5" />
                </template>
                {{ t('admin.users.deleteInvite') }}
              </Button>
            </div>

            <div class="mt-4 grid grid-cols-1 gap-3 text-xs sm:grid-cols-2">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.createdBy') }}</div>
                <div v-if="invite.createdByUsername" class="mt-1 flex min-w-0 items-center gap-2">
                  <UserAvatar
                    v-if="invite.createdByAvatarStyle"
                    :username="invite.createdByUsername"
                    :email="invite.createdByEmail"
                    :avatar-style="invite.createdByAvatarStyle"
                    :badge-id="invite.createdByAvatarBadgeId || null"
                    :size="24"
                  />
                  <span class="truncate text-[var(--text-primary)] font-medium">{{ invite.createdByUsername }}</span>
                </div>
                <span v-else class="mt-1 block text-[var(--text-tertiary)]">-</span>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.usedBy') }}</div>
                <div v-if="invite.usedByUsername" class="mt-1 flex min-w-0 items-center gap-2">
                  <UserAvatar
                    v-if="invite.usedByAvatarStyle"
                    :username="invite.usedByUsername"
                    :email="invite.usedByEmail"
                    :avatar-style="invite.usedByAvatarStyle"
                    :badge-id="invite.usedByAvatarBadgeId || null"
                    :size="24"
                  />
                  <span class="truncate text-[var(--text-primary)] font-medium">{{ invite.usedByUsername }}</span>
                </div>
                <span v-else class="mt-1 block text-[var(--text-tertiary)]">-</span>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.createdAt') }}</div>
                <div class="mt-1 text-[var(--text-secondary)] font-medium">{{ formatDate(invite.createdAt) }}</div>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5">
                <div class="text-[11px] text-[var(--text-tertiary)] uppercase tracking-wide">{{ t('admin.users.usedExpireAt') }}</div>
                <div class="mt-1 text-[var(--text-secondary)] font-medium">
                  <span v-if="invite.usedAt">{{ formatDate(invite.usedAt) }}</span>
                  <span v-else-if="invite.expiresAt">{{ formatDate(invite.expiresAt) }}</span>
                  <span v-else class="text-[var(--text-tertiary)]">{{ t('admin.users.permanent') }}</span>
                </div>
              </div>
            </div>
          </Card>
        </div>

        <!-- 桌面端邀请码表格 -->
        <div class="card hidden overflow-hidden lg:block">
          <table class="w-full table-fixed">
            <colgroup>
              <col class="w-[20%]" />
              <col class="w-[16%]" />
              <col class="w-[12%]" />
              <col class="w-[16%]" />
              <col class="w-[14%]" />
              <col class="w-[14%]" />
              <col class="w-[8%]" />
            </colgroup>
            <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
              <tr>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.inviteCode') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.createdBy') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.inviteStatus') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.usedBy') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.createdAt') }}</th>
                <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('admin.users.usedExpireAt') }}</th>
                <th class="text-right text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none">{{ t('common.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-[var(--border-color)]">
              <tr
                v-for="invite in invites"
                :key="invite.id"
                class="hover:bg-[var(--bg-surface-soft)] transition-colors duration-150"
              >
                <td class="px-4 py-3.5 font-mono tabular-nums text-sm font-semibold text-[var(--text-primary)] truncate" :title="invite.code">
                  {{ invite.code }}
                </td>
                <td class="px-4 py-3.5 text-sm">
                  <div v-if="invite.createdByUsername" class="flex items-center gap-2">
                    <UserAvatar
                      v-if="invite.createdByAvatarStyle"
                      :username="invite.createdByUsername"
                      :email="invite.createdByEmail"
                      :avatar-style="invite.createdByAvatarStyle"
                      :badge-id="invite.createdByAvatarBadgeId || null"
                      :size="24"
                    />
                    <span class="truncate text-[var(--text-primary)] font-medium">{{ invite.createdByUsername }}</span>
                  </div>
                  <span v-else class="text-[var(--text-tertiary)]">-</span>
                </td>
                <td class="px-4 py-3.5 whitespace-nowrap">
                  <Badge size="sm" :variant="getInviteStatus(invite).variant">
                    {{ getInviteStatus(invite).label }}
                  </Badge>
                </td>
                <td class="px-4 py-3.5 text-sm">
                  <div v-if="invite.usedByUsername" class="flex items-center gap-2">
                    <UserAvatar
                      v-if="invite.usedByAvatarStyle"
                      :username="invite.usedByUsername"
                      :email="invite.usedByEmail"
                      :avatar-style="invite.usedByAvatarStyle"
                      :badge-id="invite.usedByAvatarBadgeId || null"
                      :size="24"
                    />
                    <span class="truncate text-[var(--text-primary)] font-medium">{{ invite.usedByUsername }}</span>
                  </div>
                  <span v-else class="text-[var(--text-tertiary)]">-</span>
                </td>
                <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)] whitespace-nowrap">
                  {{ formatDate(invite.createdAt) }}
                </td>
                <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)] whitespace-nowrap">
                  <span v-if="invite.usedAt">{{ formatDate(invite.usedAt) }}</span>
                  <span v-else-if="invite.expiresAt">{{ formatDate(invite.expiresAt) }}</span>
                  <span v-else class="text-[var(--text-tertiary)]">{{ t('admin.users.permanent') }}</span>
                </td>
                <td class="px-4 py-3.5 text-right whitespace-nowrap">
                  <Button
                    size="sm"
                    variant="ghost"
                    class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,transparent)] !min-h-0 h-8 px-2.5 text-xs"
                    @click="deleteInvite(invite)"
                  >
                    <template #icon>
                      <Trash2 class="w-3.5 h-3.5" />
                    </template>
                    {{ t('admin.users.deleteInvite') }}
                  </Button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 邀请码底部分页 -->
        <div v-if="invitesTotal > 0" class="pt-2 px-1">
          <Pagination
            :page="invitesPage"
            :page-size="invitesPageSize"
            :total="invitesTotal"
            :show-size-changer="true"
            :page-size-options="[10, 30, 50, 100]"
            @update:page="(p) => { invitesPage = p; void loadInvites() }"
            @update:page-size="(ps) => { invitesPageSize = ps; handleInvitesPageSizeChange() }"
          />
        </div>
      </template>
    </div>

    <!-- 关联账号检测 Tab -->
    <div v-if="activeTab === 'linkedAccounts'" class="space-y-5">
      <!-- 控制栏 Card -->
      <Card data-reveal padding="sm">
        <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3 sm:gap-4">
          <div class="flex items-center gap-3">
            <span class="text-xs font-semibold text-[var(--text-secondary)] whitespace-nowrap">{{ t('admin.users.detectDays') }}</span>
            <div class="w-44">
              <Select
                :model-value="linkedAccountsDays"
                :options="linkedAccountsDaysOptions"
                size="sm"
                @update:model-value="(val) => { linkedAccountsDays = Number(val) }"
              />
            </div>
          </div>
          <Button
            variant="primary"
            size="sm"
            :loading="linkedAccountsLoading"
            @click="loadLinkedAccounts"
          >
            <template #icon>
              <Search class="w-4 h-4" />
            </template>
            {{ linkedAccountsLoading ? t('admin.users.detecting') : t('admin.users.startDetect') }}
          </Button>
        </div>
      </Card>

      <!-- 加载中 -->
      <Card v-if="linkedAccountsLoading" padding="lg" class="text-center">
        <div class="animate-spin w-8 h-8 mx-auto text-[var(--accent)] mb-3">
          <svg fill="none" viewBox="0 0 24 24">
            <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
            <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
          </svg>
        </div>
        <p class="text-sm text-[var(--text-secondary)]">{{ t('admin.users.detectingHint') }}</p>
      </Card>

      <!-- 未检测 -->
      <Card v-else-if="!linkedAccountsData" padding="lg">
        <EmptyState
          :title="t('admin.users.clickToDetect')"
        >
          <template #icon>
            <Search class="w-7 h-7 text-[var(--text-tertiary)] stroke-[1.5]" />
          </template>
        </EmptyState>
      </Card>

      <!-- 检测结果 -->
      <template v-else>
        <!-- 摘要信息 Card -->
        <Card padding="sm" class="space-y-4">
          <div class="flex flex-wrap items-center gap-6 text-xs text-[var(--text-secondary)]">
            <div class="flex items-center gap-2">
              <span class="text-[var(--text-tertiary)]">{{ t('admin.users.detectTime') }}:</span>
              <span class="font-medium text-[var(--text-primary)]">{{ formatDate(linkedAccountsData.detectedAt) }}</span>
            </div>
            <div class="flex items-center gap-2">
              <span class="text-[var(--text-tertiary)]">{{ t('admin.users.detectDuration') }}:</span>
              <span class="font-mono font-medium text-[var(--text-primary)]">{{ linkedAccountsData.durationMs }}ms</span>
            </div>
            <div class="flex items-center gap-2">
              <span class="text-[var(--text-tertiary)]">{{ t('admin.users.detectRange') }}:</span>
              <span class="font-medium text-[var(--text-primary)]">{{ linkedAccountsData.days }} {{ t('admin.users.daysUnit') }}</span>
            </div>
          </div>
          <div class="flex flex-wrap gap-3 pt-3 border-t border-[var(--border-color)]">
            <Badge variant="warning" size="md">
              {{ linkedAccountsData.summary.ipGroups }} {{ t('admin.users.ipGroupCount') }}
            </Badge>
            <Badge variant="info" size="md">
              {{ linkedAccountsData.summary.emailGroups }} {{ t('admin.users.emailGroupCount') }}
            </Badge>
            <Badge variant="default" size="md">
              {{ linkedAccountsData.summary.usernameGroups }} {{ t('admin.users.usernameGroupCount') }}
            </Badge>
          </div>
        </Card>

        <!-- IP 关联组 -->
        <div v-if="linkedAccountsData.ipGroups.length > 0" class="space-y-3">
          <h3 class="text-sm font-semibold text-[var(--text-primary)] flex items-center gap-2 tracking-tight">
            <Globe class="w-4 h-4 text-[var(--accent)]" />
            <span>{{ t('admin.users.ipLinkedGroups') }} ({{ linkedAccountsData.ipGroups.length }})</span>
          </h3>
          <div class="space-y-2.5">
            <Card
              v-for="group in linkedAccountsData.ipGroups"
              :key="group.ip"
              padding="none"
              class="overflow-hidden"
            >
              <button
                type="button"
                class="w-full px-4 py-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors text-left select-none"
                @click="toggleIPGroup(group.ip)"
              >
                <div class="flex items-center gap-3">
                  <span class="font-mono tabular-nums text-sm font-semibold text-[var(--text-primary)]">{{ group.ip }}</span>
                  <Badge variant="warning" size="sm">{{ group.userCount }} {{ t('admin.users.usersCount') }}</Badge>
                  <span class="text-xs text-[var(--text-tertiary)]">{{ group.totalLogins }} {{ t('admin.users.loginsCount') }}</span>
                </div>
                <ChevronDown
                  class="w-4 h-4 text-[var(--text-tertiary)] transition-transform duration-200"
                  :class="{ 'rotate-180': linkedAccountsExpandedIP.has(group.ip) }"
                />
              </button>
              <div v-if="linkedAccountsExpandedIP.has(group.ip)" class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <div class="divide-y divide-[var(--border-color)]">
                  <div
                    v-for="user in group.users"
                    :key="user.id"
                    class="px-4 py-2.5 flex flex-col sm:flex-row sm:items-center justify-between gap-1 sm:gap-3 text-sm hover:bg-[var(--bg-surface)] transition-colors"
                  >
                    <div class="flex flex-wrap items-center gap-2.5">
                      <span class="font-mono tabular-nums text-xs text-[var(--text-tertiary)]">#{{ user.id }}</span>
                      <span class="font-medium text-xs text-[var(--text-primary)]">{{ user.username }}</span>
                      <span v-if="user.email" class="text-xs text-[var(--text-secondary)] truncate max-w-[200px]">{{ user.email }}</span>
                      <Badge size="sm" :variant="user.status === 'active' ? 'success' : 'error'">
                        {{ user.status === 'active' ? t('admin.users.active') : t('admin.users.banned') }}
                      </Badge>
                    </div>
                    <div class="text-xs text-[var(--text-tertiary)] whitespace-nowrap">
                      {{ user.loginCount }} {{ t('admin.users.loginsCount') }} · {{ t('admin.users.lastLoginAt') }} {{ formatDate(user.lastLogin) }}
                    </div>
                  </div>
                </div>
              </div>
            </Card>
          </div>
        </div>

        <!-- 邮箱相似组 -->
        <div v-if="linkedAccountsData.emailGroups.length > 0" class="space-y-3">
          <h3 class="text-sm font-semibold text-[var(--text-primary)] flex items-center gap-2 tracking-tight">
            <Mail class="w-4 h-4 text-[var(--accent)]" />
            <span>{{ t('admin.users.emailSimilarGroups') }} ({{ linkedAccountsData.emailGroups.length }})</span>
          </h3>
          <div class="space-y-2.5">
            <Card
              v-for="group in linkedAccountsData.emailGroups"
              :key="group.pattern"
              padding="none"
              class="overflow-hidden"
            >
              <button
                type="button"
                class="w-full px-4 py-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors text-left select-none"
                @click="toggleEmailGroup(group.pattern)"
              >
                <div class="flex items-center gap-3">
                  <span class="font-mono text-sm font-semibold text-[var(--text-primary)]">{{ group.pattern }}*</span>
                  <Badge variant="info" size="sm">{{ group.userCount }} {{ t('admin.users.usersCount') }}</Badge>
                </div>
                <ChevronDown
                  class="w-4 h-4 text-[var(--text-tertiary)] transition-transform duration-200"
                  :class="{ 'rotate-180': linkedAccountsExpandedEmail.has(group.pattern) }"
                />
              </button>
              <div v-if="linkedAccountsExpandedEmail.has(group.pattern)" class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <div class="divide-y divide-[var(--border-color)]">
                  <div
                    v-for="user in group.users"
                    :key="user.id"
                    class="px-4 py-2.5 flex flex-col sm:flex-row sm:items-center justify-between gap-1 sm:gap-3 text-sm hover:bg-[var(--bg-surface)] transition-colors"
                  >
                    <div class="flex flex-wrap items-center gap-2.5">
                      <span class="font-mono tabular-nums text-xs text-[var(--text-tertiary)]">#{{ user.id }}</span>
                      <span class="font-medium text-xs text-[var(--text-primary)]">{{ user.username }}</span>
                      <span class="text-xs text-[var(--text-secondary)] truncate max-w-[200px]">{{ user.email }}</span>
                      <Badge size="sm" :variant="user.status === 'active' ? 'success' : 'error'">
                        {{ user.status === 'active' ? t('admin.users.active') : t('admin.users.banned') }}
                      </Badge>
                    </div>
                    <div class="text-xs text-[var(--text-tertiary)] whitespace-nowrap">
                      {{ t('admin.users.createdAt') }} {{ formatDate(user.createdAt) }}
                    </div>
                  </div>
                </div>
              </div>
            </Card>
          </div>
        </div>

        <!-- 用户名相似组 -->
        <div v-if="linkedAccountsData.usernameGroups.length > 0" class="space-y-3">
          <h3 class="text-sm font-semibold text-[var(--text-primary)] flex items-center gap-2 tracking-tight">
            <UserCheck class="w-4 h-4 text-[var(--accent)]" />
            <span>{{ t('admin.users.usernameSimilarGroups') }} ({{ linkedAccountsData.usernameGroups.length }})</span>
          </h3>
          <div class="space-y-2.5">
            <Card
              v-for="group in linkedAccountsData.usernameGroups"
              :key="group.pattern"
              padding="none"
              class="overflow-hidden"
            >
              <button
                type="button"
                class="w-full px-4 py-3 flex items-center justify-between hover:bg-[var(--bg-surface-soft)] transition-colors text-left select-none"
                @click="toggleUsernameGroup(group.pattern)"
              >
                <div class="flex items-center gap-3">
                  <span class="font-mono text-sm font-semibold text-[var(--text-primary)]">{{ group.pattern }}*</span>
                  <Badge variant="default" size="sm">{{ group.userCount }} {{ t('admin.users.usersCount') }}</Badge>
                </div>
                <ChevronDown
                  class="w-4 h-4 text-[var(--text-tertiary)] transition-transform duration-200"
                  :class="{ 'rotate-180': linkedAccountsExpandedUsername.has(group.pattern) }"
                />
              </button>
              <div v-if="linkedAccountsExpandedUsername.has(group.pattern)" class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <div class="divide-y divide-[var(--border-color)]">
                  <div
                    v-for="user in group.users"
                    :key="user.id"
                    class="px-4 py-2.5 flex flex-col sm:flex-row sm:items-center justify-between gap-1 sm:gap-3 text-sm hover:bg-[var(--bg-surface)] transition-colors"
                  >
                    <div class="flex flex-wrap items-center gap-2.5">
                      <span class="font-mono tabular-nums text-xs text-[var(--text-tertiary)]">#{{ user.id }}</span>
                      <span class="font-medium text-xs text-[var(--text-primary)]">{{ user.username }}</span>
                      <span v-if="user.email" class="text-xs text-[var(--text-secondary)] truncate max-w-[200px]">{{ user.email }}</span>
                      <Badge size="sm" :variant="user.status === 'active' ? 'success' : 'error'">
                        {{ user.status === 'active' ? t('admin.users.active') : t('admin.users.banned') }}
                      </Badge>
                    </div>
                    <div class="text-xs text-[var(--text-tertiary)] whitespace-nowrap">
                      {{ t('admin.users.createdAt') }} {{ formatDate(user.createdAt) }}
                    </div>
                  </div>
                </div>
              </div>
            </Card>
          </div>
        </div>

        <!-- 无检测结果 -->
        <Card
          v-if="linkedAccountsData.ipGroups.length === 0 && linkedAccountsData.emailGroups.length === 0 && linkedAccountsData.usernameGroups.length === 0"
          padding="lg"
        >
          <EmptyState
            :title="t('admin.users.noLinkedAccounts')"
          >
            <template #icon>
              <Check class="w-7 h-7 text-[var(--success)] stroke-[2]" />
            </template>
          </EmptyState>
        </Card>
      </template>
    </div>

    <!-- VIP 等级与权益设置 -->
    <VipLevelRulesEditor v-if="activeTab === 'userVipLevels'" type="user" />
    <VipBenefitHallSettings v-if="activeTab === 'vipBenefits'" />

    <!-- 生成邀请码配置弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showGenerateModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showGenerateModal = false"></div>
          <div class="modal-content">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.generateInviteTitle') }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showGenerateModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-4">
              <div>
                <Select
                  v-model.number="inviteCount"
                  :options="inviteCountOptions"
                  :label="t('admin.users.inviteCount')"
                />
              </div>
              <div>
                <Select
                  v-model.number="inviteExpireDays"
                  :options="inviteExpireDaysOptions"
                  :label="t('admin.users.expireDays')"
                  :hint="t('admin.users.expireHint')"
                />
              </div>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showGenerateModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                :loading="inviteGenerating"
                @click="generateInvite"
              >
                {{ inviteGenerating ? t('admin.users.generating') : t('admin.users.generate') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 邀请码生成结果弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showInviteModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showInviteModal = false"></div>
          <div class="modal-content" :class="{ 'max-w-lg': newInviteCodes.length > 0 }">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ t('admin.users.inviteGenerated') }}
                <span v-if="newInviteCodes.length > 0" class="text-[var(--text-secondary)] font-normal text-xs ml-2">
                  ({{ newInviteCodes.length }} {{ t('admin.users.countUnit') }})
                </span>
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showInviteModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-4">
              <!-- 单个邀请码 -->
              <template v-if="newInviteCode">
                <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 font-mono tabular-nums text-lg text-center text-[var(--text-primary)] tracking-wider">
                  {{ newInviteCode }}
                </div>
              </template>
              <!-- 批量邀请码 -->
              <template v-else-if="newInviteCodes.length > 0">
                <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 font-mono tabular-nums text-xs text-[var(--text-primary)] max-h-64 overflow-y-auto space-y-1">
                  <div
                    v-for="(code, index) in newInviteCodes"
                    :key="code"
                    class="py-1.5 px-2.5 hover:bg-[var(--bg-surface)] rounded-lg flex items-center justify-between"
                  >
                    <span>{{ index + 1 }}. {{ code }}</span>
                  </div>
                </div>
              </template>

              <div class="text-center text-xs text-[var(--text-secondary)]">
                <span v-if="newInviteExpiresAt">{{ t('admin.users.validUntil') }}: {{ formatDate(newInviteExpiresAt) }}</span>
                <span v-else>{{ t('admin.users.permanentValid') }}</span>
              </div>

              <div class="flex gap-2.5">
                <Button
                  variant="primary"
                  class="flex-1"
                  @click="copyCode"
                >
                  <template #icon>
                    <Copy class="w-4 h-4" />
                  </template>
                  {{ copied ? t('admin.users.copied') : (newInviteCodes.length > 0 ? t('admin.users.copyAllCodes') : t('admin.users.copyCode')) }}
                </Button>
                <Button
                  v-if="newInviteCode"
                  variant="secondary"
                  class="flex-1"
                  @click="copyLink"
                >
                  {{ t('admin.users.copyLink') }}
                </Button>
              </div>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 重置密码确认弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showResetPasswordModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showResetPasswordModal = false"></div>
          <div class="modal-content">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.resetPassword') }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showResetPasswordModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-4">
              <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
                {{ t('admin.users.resetPasswordConfirm', { name: resetPasswordUser?.username }) }}
              </p>
              <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.users.resetPasswordHint') }}</p>
              <div class="space-y-3 pt-1">
                <div>
                  <Input
                    v-model="resetPasswordForm.password"
                    type="password"
                    autocomplete="new-password"
                    :label="t('auth.newPassword')"
                  />
                </div>
                <div>
                  <Input
                    v-model="resetPasswordForm.confirmPassword"
                    type="password"
                    autocomplete="new-password"
                    :label="t('auth.confirmPassword')"
                  />
                </div>
              </div>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showResetPasswordModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                :loading="resetPasswordLoading"
                @click="resetPassword"
              >
                {{ resetPasswordLoading ? t('admin.users.resetting') : t('admin.users.confirmResetPassword') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 取消2FA确认弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showDisable2FAModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showDisable2FAModal = false"></div>
          <div class="modal-content">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.disable2FA') }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showDisable2FAModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-3">
              <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
                {{ t('admin.users.disable2FAConfirm', { name: disable2FAUser?.username }) }}
              </p>
              <p class="text-xs text-[var(--error)]">{{ t('admin.users.disable2FAWarning') }}</p>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showDisable2FAModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="danger"
                :loading="disable2FALoading"
                @click="disable2FA"
              >
                {{ disable2FALoading ? t('admin.system.saving') : t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 解绑GitHub确认弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showUnbindGitHubModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showUnbindGitHubModal = false"></div>
          <div class="modal-content">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.unbindGitHub') }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showUnbindGitHubModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-3">
              <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
                {{ t('admin.users.unbindGitHubConfirm', { name: unbindGitHubUser?.username }) }}
              </p>
              <p class="text-xs text-[var(--error)]">{{ t('admin.users.unbindGitHubWarning') }}</p>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showUnbindGitHubModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="danger"
                :loading="unbindGitHubLoading"
                @click="unbindGitHub"
              >
                {{ unbindGitHubLoading ? t('admin.system.saving') : t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 登录记录弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showLoginRecordsModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showLoginRecordsModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.loginRecords') }} - {{ loginRecordsUser?.username }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showLoginRecordsModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body">
              <div v-if="loginRecordsLoading" class="flex items-center justify-center py-12">
                <div class="animate-spin w-6 h-6 text-[var(--accent)]">
                  <svg fill="none" viewBox="0 0 24 24">
                    <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                    <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
                  </svg>
                </div>
              </div>
              <div v-else-if="loginRecords.length === 0" class="py-8">
                <EmptyState :title="t('admin.users.noLoginRecords')" />
              </div>
              <div v-else class="space-y-2 max-h-96 overflow-y-auto pr-1">
                <div
                  v-for="record in loginRecords"
                  :key="record.id"
                  class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs"
                >
                  <div class="flex items-start justify-between gap-4">
                    <div class="min-w-0 flex-1 space-y-1">
                      <div class="font-mono tabular-nums font-semibold text-[var(--text-primary)]">{{ record.ip }}</div>
                      <div v-if="record.country" class="text-[11px] text-[var(--text-secondary)]">
                        {{ formatLocation(record) }}
                        <span v-if="record.isp" class="text-[var(--text-tertiary)]"> · {{ record.isp }}</span>
                      </div>
                      <div v-else-if="record.isp" class="text-[11px] text-[var(--text-secondary)]">{{ record.isp }}</div>
                      <div v-if="record.userAgent" class="text-[11px] text-[var(--text-tertiary)]">
                        {{ formatDevice(record.userAgent) }} · {{ formatBrowser(record.userAgent) }}
                      </div>
                    </div>
                    <div class="text-[11px] text-[var(--text-tertiary)] whitespace-nowrap flex-shrink-0">
                      {{ formatDate(record.createdAt) }}
                    </div>
                  </div>
                </div>
              </div>

              <!-- 分页 -->
              <div v-if="loginRecords.length > 0" class="flex items-center justify-between text-xs text-[var(--text-secondary)] mt-4 pt-4 border-t border-[var(--border-color)]">
                <span>{{ t('admin.users.totalRecords', { count: loginRecordsTotal }) }}</span>
                <div v-if="loginRecordsTotalPages > 1" class="flex items-center gap-2">
                  <Button
                    size="sm"
                    variant="ghost"
                    :disabled="loginRecordsPage <= 1"
                    @click="loginRecordsPage--; void loadLoginRecords()"
                  >
                    {{ t('admin.users.prevPage') }}
                  </Button>
                  <span class="font-mono tabular-nums">{{ loginRecordsPage }} / {{ loginRecordsTotalPages }}</span>
                  <Button
                    size="sm"
                    variant="ghost"
                    :disabled="loginRecordsPage >= loginRecordsTotalPages"
                    @click="loginRecordsPage++; void loadLoginRecords()"
                  >
                    {{ t('admin.users.nextPage') }}
                  </Button>
                </div>
              </div>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showLoginRecordsModal = false">
                {{ t('common.close') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 发送站内信弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showSendMessageModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showSendMessageModal = false"></div>
          <div class="modal-content max-w-lg">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.sendMessageTo', { username: sendMessageUser?.username }) }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showSendMessageModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-4">
              <div>
                <Input
                  v-model="sendMessageForm.title"
                  type="text"
                  :label="t('admin.users.messageTitle')"
                  :placeholder="t('admin.users.messageTitlePlaceholder')"
                  :maxlength="200"
                  :hint="`${sendMessageForm.title.length}/200`"
                />
              </div>
              <div class="flex flex-col gap-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">
                  {{ t('admin.users.messageContent') }}
                </label>
                <textarea
                  v-model="sendMessageForm.content"
                  rows="4"
                  maxlength="5000"
                  :placeholder="t('admin.users.messageContentPlaceholder')"
                  class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200"
                ></textarea>
                <p class="text-xs text-[var(--text-tertiary)] mt-0.5">{{ sendMessageForm.content.length }}/5000</p>
              </div>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showSendMessageModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                :loading="sendMessageLoading"
                :disabled="!sendMessageForm.title.trim() || !sendMessageForm.content.trim()"
                @click="sendMessage"
              >
                {{ sendMessageLoading ? t('common.sending') : t('common.send') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 用户余额详情弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showBalanceModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showBalanceModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header">
              <h3 class="modal-title">{{ t('admin.users.balanceDetails') }} - {{ balanceUser?.username }}</h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showBalanceModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <!-- TAB 切换 -->
            <div class="flex items-center gap-1 border-b border-[var(--border-color)] px-4 py-2 bg-[var(--bg-surface-soft)] overflow-x-auto">
              <button
                v-for="bt in balanceTabs"
                :key="bt.key"
                type="button"
                class="px-3.5 py-1.5 rounded-full text-xs font-medium transition-all duration-200 active:scale-95 whitespace-nowrap select-none"
                :class="balanceTab === bt.key
                  ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
                  : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                @click="switchBalanceTab(bt.key)"
              >
                {{ bt.label }}
              </button>
            </div>

            <div class="modal-body">
              <!-- 余额概览 TAB -->
              <div v-if="balanceTab === 'overview'">
                <div v-if="balanceInfoLoading" class="flex items-center justify-center py-12">
                  <div class="animate-spin w-6 h-6 text-[var(--accent)]">
                    <svg fill="none" viewBox="0 0 24 24">
                      <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                      <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
                    </svg>
                  </div>
                </div>
                <div v-else class="grid grid-cols-1 sm:grid-cols-3 gap-3">
                  <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-center">
                    <div class="font-mono text-2xl font-semibold tabular-nums text-[var(--success)]">
                      ¥{{ balanceInfo.balance.toFixed(2) }}
                    </div>
                    <div class="mt-2 text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                      {{ t('admin.users.currentBalance') }}
                    </div>
                  </div>
                  <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-center">
                    <div class="font-mono text-2xl font-semibold tabular-nums text-[var(--text-primary)]">
                      ¥{{ balanceInfo.totalRecharge.toFixed(2) }}
                    </div>
                    <div class="mt-2 text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                      {{ t('admin.users.totalRecharge') }}
                    </div>
                  </div>
                  <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-center">
                    <div class="font-mono text-2xl font-semibold tabular-nums text-[var(--warning)]">
                      ¥{{ balanceInfo.totalConsume.toFixed(2) }}
                    </div>
                    <div class="mt-2 text-[11px] font-medium uppercase tracking-wider text-[var(--text-secondary)]">
                      {{ t('admin.users.totalConsume') }}
                    </div>
                  </div>
                </div>
              </div>

              <!-- 余额明细 TAB -->
              <div v-if="balanceTab === 'logs'">
                <div class="flex justify-end mb-3">
                  <Button
                    size="sm"
                    :variant="balanceLogsShowLotteryGiftOnly ? 'primary' : 'secondary'"
                    @click="balanceLogsShowLotteryGiftOnly = !balanceLogsShowLotteryGiftOnly; balanceLogsPage = 1; void loadBalanceLogs()"
                  >
                    <template #icon>
                      <Gift class="w-3.5 h-3.5" />
                    </template>
                    {{ balanceLogsShowLotteryGiftOnly ? t('wallet.showingLotteryGift') : t('wallet.showLotteryGift') }}
                  </Button>
                </div>
                <div v-if="balanceLogsLoading" class="flex items-center justify-center py-12">
                  <div class="animate-spin w-6 h-6 text-[var(--accent)]">
                    <svg fill="none" viewBox="0 0 24 24">
                      <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                      <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
                    </svg>
                  </div>
                </div>
                <div v-else-if="balanceLogs.length === 0" class="py-8">
                  <EmptyState :title="t('admin.users.noBalanceLogs')" />
                </div>
                <div v-else>
                  <div class="space-y-2 max-h-80 overflow-y-auto pr-1">
                    <div
                      v-for="log in balanceLogs"
                      :key="log.id"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs flex items-center justify-between"
                    >
                      <div class="min-w-0 flex-1">
                        <div class="flex items-center gap-2">
                          <Badge size="sm" :variant="log.amount > 0 ? 'success' : 'error'">
                            {{ getBalanceLogTypeLabel(log.type) }}
                          </Badge>
                          <span class="font-mono tabular-nums font-semibold" :class="log.amount > 0 ? 'text-[var(--success)]' : 'text-[var(--error)]'">
                            {{ log.amount > 0 ? '+' : '' }}¥{{ log.amount.toFixed(2) }}
                          </span>
                        </div>
                        <div v-if="log.remark" class="text-xs text-[var(--text-secondary)] mt-1 truncate">{{ log.remark }}</div>
                      </div>
                      <div class="text-xs text-[var(--text-tertiary)] whitespace-nowrap ml-4">{{ formatDate(log.createdAt) }}</div>
                    </div>
                  </div>
                  <!-- 分页 -->
                  <div v-if="balanceLogsTotalPages > 1" class="flex items-center justify-between text-xs text-[var(--text-secondary)] mt-4 pt-4 border-t border-[var(--border-color)]">
                    <span>{{ t('admin.users.totalRecords', { count: balanceLogsTotal }) }}</span>
                    <div class="flex items-center gap-2">
                      <Button
                        size="sm"
                        variant="ghost"
                        :disabled="balanceLogsPage <= 1"
                        @click="balanceLogsPage--; void loadBalanceLogs()"
                      >
                        {{ t('admin.users.prevPage') }}
                      </Button>
                      <span class="font-mono tabular-nums">{{ balanceLogsPage }} / {{ balanceLogsTotalPages }}</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        :disabled="balanceLogsPage >= balanceLogsTotalPages"
                        @click="balanceLogsPage++; void loadBalanceLogs()"
                      >
                        {{ t('admin.users.nextPage') }}
                      </Button>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 充值记录 TAB -->
              <div v-if="balanceTab === 'recharge'">
                <div v-if="rechargeRecordsLoading" class="flex items-center justify-center py-12">
                  <div class="animate-spin w-6 h-6 text-[var(--accent)]">
                    <svg fill="none" viewBox="0 0 24 24">
                      <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                      <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
                    </svg>
                  </div>
                </div>
                <div v-else-if="rechargeRecords.length === 0" class="py-8">
                  <EmptyState :title="t('admin.users.noRechargeRecords')" />
                </div>
                <div v-else>
                  <div class="space-y-2 max-h-80 overflow-y-auto pr-1">
                    <div
                      v-for="record in rechargeRecords"
                      :key="record.id"
                      class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs flex items-center justify-between"
                    >
                      <div class="min-w-0 flex-1">
                        <div class="flex items-center gap-2">
                          <span class="font-mono tabular-nums font-semibold text-[var(--text-primary)]">¥{{ record.amount.toFixed(2) }}</span>
                          <Badge size="sm" :variant="getRechargeBadgeVariant(record.status)">
                            {{ t('wallet.status.' + record.status) }}
                          </Badge>
                        </div>
                        <div class="text-xs text-[var(--text-secondary)] mt-1 truncate">
                          {{ record.orderNo }}
                          <span v-if="record.provider"> · {{ record.provider.name }}</span>
                        </div>
                      </div>
                      <div class="text-xs text-[var(--text-tertiary)] whitespace-nowrap ml-4">{{ formatDate(record.createdAt) }}</div>
                    </div>
                  </div>
                  <!-- 分页 -->
                  <div v-if="rechargeRecordsTotalPages > 1" class="flex items-center justify-between text-xs text-[var(--text-secondary)] mt-4 pt-4 border-t border-[var(--border-color)]">
                    <span>{{ t('admin.users.totalRecords', { count: rechargeRecordsTotal }) }}</span>
                    <div class="flex items-center gap-2">
                      <Button
                        size="sm"
                        variant="ghost"
                        :disabled="rechargeRecordsPage <= 1"
                        @click="rechargeRecordsPage--; void loadRechargeRecords()"
                      >
                        {{ t('admin.users.prevPage') }}
                      </Button>
                      <span class="font-mono tabular-nums">{{ rechargeRecordsPage }} / {{ rechargeRecordsTotalPages }}</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        :disabled="rechargeRecordsPage >= rechargeRecordsTotalPages"
                        @click="rechargeRecordsPage++; void loadRechargeRecords()"
                      >
                        {{ t('admin.users.nextPage') }}
                      </Button>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 调整余额 TAB -->
              <div v-if="balanceTab === 'adjust'" class="space-y-4">
                <!-- 调整类型 -->
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-2">{{ t('admin.users.adjustType') }}</label>
                  <div class="grid grid-cols-2 gap-3">
                    <label
                      class="flex items-center gap-2.5 p-3 rounded-xl border cursor-pointer transition-all active:scale-[0.98]"
                      :class="adjustBalanceForm.type === 'add'
                        ? 'border-[var(--accent)] bg-[var(--bg-surface)] ring-1 ring-[var(--accent)] shadow-sm'
                        : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:border-[var(--border-hover)]'"
                    >
                      <input
                        v-model="adjustBalanceForm.type"
                        type="radio"
                        value="add"
                        class="sr-only"
                      />
                      <div
                        class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0 transition-colors"
                        :class="adjustBalanceForm.type === 'add' ? 'border-[var(--accent)] bg-[var(--accent)]' : 'border-[var(--border-color)]'"
                      >
                        <div v-if="adjustBalanceForm.type === 'add'" class="w-1.5 h-1.5 rounded-full bg-white" />
                      </div>
                      <span class="text-xs font-medium text-[var(--success)]">{{ t('admin.users.addBalance') }}</span>
                    </label>

                    <label
                      class="flex items-center gap-2.5 p-3 rounded-xl border cursor-pointer transition-all active:scale-[0.98]"
                      :class="adjustBalanceForm.type === 'deduct'
                        ? 'border-[var(--error)] bg-[var(--bg-surface)] ring-1 ring-[var(--error)] shadow-sm'
                        : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:border-[var(--border-hover)]'"
                    >
                      <input
                        v-model="adjustBalanceForm.type"
                        type="radio"
                        value="deduct"
                        class="sr-only"
                      />
                      <div
                        class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0 transition-colors"
                        :class="adjustBalanceForm.type === 'deduct' ? 'border-[var(--error)] bg-[var(--error)]' : 'border-[var(--border-color)]'"
                      >
                        <div v-if="adjustBalanceForm.type === 'deduct'" class="w-1.5 h-1.5 rounded-full bg-white" />
                      </div>
                      <span class="text-xs font-medium text-[var(--error)]">{{ t('admin.users.deductBalance') }}</span>
                    </label>
                  </div>
                </div>

                <!-- 金额 -->
                <div>
                  <Input
                    v-model="adjustBalanceForm.amount"
                    type="number"
                    :label="t('admin.users.amount')"
                    :placeholder="t('admin.users.amountPlaceholder')"
                  >
                    <template #prefix>
                      <span class="font-mono text-xs text-[var(--text-tertiary)]">¥</span>
                    </template>
                  </Input>
                </div>

                <!-- 理由 -->
                <div class="flex flex-col gap-1.5">
                  <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">
                    {{ t('admin.users.adjustReason') }} <span class="text-[var(--error)]">*</span>
                  </label>
                  <textarea
                    v-model="adjustBalanceForm.reason"
                    rows="3"
                    maxlength="500"
                    :placeholder="t('admin.users.adjustReasonPlaceholder')"
                    class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200"
                  ></textarea>
                  <p class="text-xs text-[var(--text-tertiary)] mt-0.5">{{ adjustBalanceForm.reason.length }}/500</p>
                </div>

                <!-- 提交按钮 -->
                <div class="pt-2">
                  <Button
                    variant="primary"
                    block
                    :loading="adjustBalanceLoading"
                    :disabled="!adjustBalanceForm.amount || !adjustBalanceForm.reason.trim()"
                    @click="submitAdjustBalance"
                  >
                    {{ adjustBalanceLoading ? t('common.submitting') : t('common.confirm') }}
                  </Button>
                </div>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showBalanceModal = false">
                {{ t('common.close') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 积分调整弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div v-if="showPointsModal" class="modal-overlay">
          <div class="modal-backdrop" @click="showPointsModal = false"></div>
          <div class="modal-content max-w-lg">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ t('admin.users.adjustPoints') }} - {{ pointsUser?.username }}
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="showPointsModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            <div class="modal-body space-y-4">
              <!-- 当前积分 -->
              <div class="flex items-center justify-between p-3.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <span class="text-xs text-[var(--text-secondary)] font-medium">{{ t('admin.users.currentPoints') }}</span>
                <span class="font-mono tabular-nums text-lg font-semibold text-[var(--text-primary)]">{{ pointsUser?.points || 0 }}</span>
              </div>
              <!-- 调整数量 -->
              <div>
                <Input
                  v-model="adjustPointsForm.amount"
                  type="number"
                  :label="t('admin.users.pointsAmount')"
                  :hint="t('admin.users.pointsAmountHint')"
                  :placeholder="t('admin.users.pointsAmountPlaceholder')"
                />
              </div>
              <!-- 理由 -->
              <div class="flex flex-col gap-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">
                  {{ t('admin.users.adjustReason') }} <span class="text-[var(--error)]">*</span>
                </label>
                <textarea
                  v-model="adjustPointsForm.reason"
                  rows="3"
                  :placeholder="t('admin.users.adjustReasonPlaceholder')"
                  class="w-full rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200"
                ></textarea>
              </div>
            </div>
            <div class="modal-footer">
              <Button variant="secondary" @click="showPointsModal = false">
                {{ t('common.cancel') }}
              </Button>
              <Button
                variant="primary"
                :loading="adjustPointsLoading"
                :disabled="!adjustPointsForm.amount || !adjustPointsForm.reason.trim()"
                @click="submitAdjustPoints"
              >
                {{ adjustPointsLoading ? t('common.submitting') : t('common.confirm') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 统一操作下拉菜单 (Teleport 挂载到 body) -->
    <Teleport to="body">
      <div
        v-if="activeActionUser"
        class="fixed inset-0 z-40"
        @click.stop="closeActionMenu"
      ></div>
      <div
        v-if="activeActionUser"
        class="kawaii-menu-panel fixed z-50 min-w-[10.5rem] rounded-2xl p-1.5 border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-lg backdrop-blur-md"
        :style="{ top: `${actionMenuPosition.top}px`, right: `${actionMenuPosition.right}px` }"
        @click.stop
      >
        <!-- 发送站内信 -->
        <button
          v-if="activeActionUser.id !== authStore.user?.id"
          type="button"
          class="kawaii-menu-item w-full flex items-center gap-2.5 px-3 py-2 text-xs text-left rounded-xl transition-all cursor-pointer text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
          :title="t('admin.users.sendMessage')"
          @click="closeActionMenu(); openSendMessageModal(activeActionUser)"
        >
          <Mail class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
          <span class="truncate font-medium">{{ t('admin.users.sendMessage') }}</span>
        </button>

        <!-- 重置密码 -->
        <button
          type="button"
          class="kawaii-menu-item w-full flex items-center gap-2.5 px-3 py-2 text-xs text-left rounded-xl transition-all cursor-pointer text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
          :title="t('admin.users.resetPassword')"
          @click="closeActionMenu(); openResetPasswordModal(activeActionUser)"
        >
          <Key class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
          <span class="truncate font-medium">{{ t('admin.users.resetPassword') }}</span>
        </button>

        <!-- 管理员角色 -->
        <button
          v-if="activeActionUser.id !== authStore.user?.id"
          type="button"
          class="kawaii-menu-item w-full flex items-center gap-2.5 px-3 py-2 text-xs text-left rounded-xl transition-all cursor-pointer hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
          :class="activeActionUser.role === 'admin' ? 'text-[var(--warning)]' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          :title="activeActionUser.role === 'admin' ? t('admin.users.demoteAdmin') : t('admin.users.promoteAdmin')"
          @click="closeActionMenu(); toggleUserRole(activeActionUser)"
        >
          <ShieldOff v-if="activeActionUser.role === 'admin'" class="w-3.5 h-3.5 text-[var(--warning)] shrink-0" />
          <ShieldCheck v-else class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
          <span class="truncate font-medium">{{ activeActionUser.role === 'admin' ? t('admin.users.demoteAdmin') : t('admin.users.promoteAdmin') }}</span>
        </button>

        <!-- 取消2FA -->
        <button
          v-if="activeActionUser.twoFAEnabled"
          type="button"
          class="kawaii-menu-item w-full flex items-center gap-2.5 px-3 py-2 text-xs text-left text-[var(--warning)] rounded-xl transition-all cursor-pointer hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
          :title="t('admin.users.disable2FA')"
          @click="closeActionMenu(); openDisable2FAModal(activeActionUser)"
        >
          <ShieldAlert class="w-3.5 h-3.5 shrink-0" />
          <span class="truncate font-medium">{{ t('admin.users.disable2FA') }}</span>
        </button>

        <!-- 解绑GitHub -->
        <button
          v-if="activeActionUser.hasGithubBinding"
          type="button"
          class="kawaii-menu-item w-full flex items-center gap-2.5 px-3 py-2 text-xs text-left rounded-xl transition-all cursor-pointer text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-[0.98]"
          :title="t('admin.users.unbindGitHub')"
          @click="closeActionMenu(); openUnbindGitHubModal(activeActionUser)"
        >
          <Globe class="w-3.5 h-3.5 text-[var(--text-tertiary)] shrink-0" />
          <span class="truncate font-medium">{{ t('admin.users.unbindGitHub') }}</span>
        </button>
      </div>
    </Teleport>
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
