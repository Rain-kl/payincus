<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Award,
  Bell,
  CalendarCheck,
  Check,
  Gift,
  Loader2,
  Pencil,
  Plus,
  RotateCw,
  Search,
  Sparkles,
  Trash2,
  Trophy,
  Users,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import { useBadgeStore } from '@/stores/badges'
import { useTabQuery } from '@/composables/useTabQuery'
import { useReveal } from '@/composables/useReveal'
import type { BadgeCatalogItem, BadgeSeriesItem } from '@/types/api'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination
} from '@/components/ui'

defineOptions({ name: 'AdminEntertainmentView' })

const { t } = useI18n()
const toast = useToast()
const badgeStore = useBadgeStore()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 当前 TAB
type EntertainmentTab = 'lotteries' | 'records' | 'users' | 'badges' | 'checkin'
const activeTab = useTabQuery<EntertainmentTab>({
  keys: ['lotteries', 'records', 'users', 'badges', 'checkin'],
  fallback: 'lotteries',
  onExternalChange: (tab) => loadTabData(tab)
})

// 抽奖列表
const lotteries = ref<any[]>([])
const lotteriesLoading = ref(false)
const lotteriesPage = ref(1)
const lotteriesPageSize = ref(100)
const lotteriesTotal = ref(0)

// 中奖记录
const records = ref<any[]>([])
const recordsLoading = ref(false)
const recordsPage = ref(1)
const recordsPageSize = ref(100)
const recordsTotal = ref(0)
const recordsSearch = ref('')
const recordsPrizeType = ref('')

// 用户积分列表
const users = ref<any[]>([])
const usersLoading = ref(false)
const usersPage = ref(1)
const usersPageSize = ref(100)
const usersTotal = ref(0)

// 签到设置与日志
const checkinSettings = ref({
  enabled: true,
  minPoints: 1,
  maxPoints: 500,
  requireInstance: false
})
const checkinSettingsLoading = ref(false)
const savingCheckinSettings = ref(false)
const checkinLogs = ref<any[]>([])
const checkinLogsLoading = ref(false)
const checkinLogsPage = ref(1)
const checkinLogsPageSize = ref(20)
const checkinLogsTotal = ref(0)
const checkinLogsSearch = ref('')
const checkinLogsDateKey = ref('')

// 创建/编辑抽奖弹窗
const showLotteryModal = ref(false)
const editingLottery = ref<any>(null)
const lotteryForm = ref({
  name: '',
  description: '',
  costPoints: 100,
  isActive: true,
  startAt: '',
  endAt: ''
})
const savingLottery = ref(false)

// 奖品管理弹窗
const showPrizesModal = ref(false)
const editingPrizes = ref<any>(null)
const prizes = ref<any[]>([])
const savingPrizes = ref(false)

// 删除确认
const deleteConfirmId = ref<number | null>(null)
const deleting = ref(false)

// 通知配置弹窗
const showNotificationModal = ref(false)
const editingNotification = ref<any>(null)
const notificationForm = ref({
  enabled: true,
  type: 'telegram' as 'telegram' | 'discord' | 'webhook',
  notifyBalance: true,
  notifyInstance: true,
  telegramBotToken: '',
  telegramChatId: '',
  discordWebhookUrl: '',
  webhookUrl: '',
  webhookSecret: ''
})
const savingNotification = ref(false)

// 徽章目录
const badgeSeries = ref<BadgeSeriesItem[]>([])
const badges = ref<BadgeCatalogItem[]>([])
const badgesLoading = ref(false)
const selectedBadgeSeriesId = ref<string>('all')
const showSeriesModal = ref(false)
const showBadgeModal = ref(false)
const editingSeries = ref<BadgeSeriesItem | null>(null)
const editingBadge = ref<BadgeCatalogItem | null>(null)
const deletingSeriesId = ref<string | null>(null)
const deletingBadgeId = ref<string | null>(null)
const savingSeries = ref(false)
const savingBadge = ref(false)

const seriesForm = ref({
  id: '',
  title: '',
  nameZh: '',
  nameEn: '',
  description: '',
  sourceId: '',
  sourceLabel: '',
  displayOrder: 0,
  isActive: true
})

const badgeForm = ref({
  id: '',
  name: '',
  nameEn: '',
  fullLabel: '',
  seriesId: '',
  sourceId: '',
  sourceLabel: '',
  assetUrl: '',
  assetUrlDark: '',
  assetUrlLight: '',
  displayOrder: 0,
  isActive: true
})

const filteredBadges = computed(() => {
  if (selectedBadgeSeriesId.value === 'all') return badges.value
  return badges.value.filter(badge => badge.seriesId === selectedBadgeSeriesId.value)
})

// 顶部仪表盘指标
const activeLotteriesCount = computed(() => lotteries.value.filter(l => l.isActive).length)
const totalDrawsSum = computed(() => lotteries.value.reduce((acc, l) => acc + (Number(l.totalDraws) || 0), 0))

// TAB 切换配置
const tabs = computed<Array<{ key: EntertainmentTab; label: string; icon: any; count?: number }>>(() => [
  {
    key: 'lotteries',
    label: t('entertainment.admin.tabs.lotteries'),
    icon: Sparkles,
    count: lotteriesTotal.value || lotteries.value.length || undefined
  },
  {
    key: 'records',
    label: t('entertainment.admin.tabs.records'),
    icon: Trophy,
    count: recordsTotal.value || undefined
  },
  {
    key: 'users',
    label: t('entertainment.admin.tabs.users'),
    icon: Users,
    count: usersTotal.value || undefined
  },
  {
    key: 'badges',
    label: t('entertainment.admin.tabs.badges'),
    icon: Award,
    count: badges.value.length || undefined
  },
  {
    key: 'checkin',
    label: t('entertainment.admin.tabs.checkin'),
    icon: CalendarCheck
  }
])

const isTabLoading = computed(() => {
  if (activeTab.value === 'lotteries') return lotteriesLoading.value
  if (activeTab.value === 'records') return recordsLoading.value
  if (activeTab.value === 'users') return usersLoading.value
  if (activeTab.value === 'badges') return badgesLoading.value
  if (activeTab.value === 'checkin') return checkinLogsLoading.value || checkinSettingsLoading.value
  return false
})

onMounted(() => {
  loadLotteries()
  loadTabData(activeTab.value)
})

async function loadLotteries() {
  lotteriesLoading.value = true
  try {
    const res = await api.entertainment.adminGetLotteries({
      page: lotteriesPage.value,
      pageSize: lotteriesPageSize.value
    })
    lotteries.value = res.lotteries || []
    lotteriesTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.admin.loadLotteriesFailed') + ': ' + err.message)
  } finally {
    lotteriesLoading.value = false
  }
}

async function loadRecords() {
  recordsLoading.value = true
  try {
    const res = await api.entertainment.adminGetLotteryRecords({
      page: recordsPage.value,
      pageSize: recordsPageSize.value,
      prizeType: recordsPrizeType.value || undefined,
      search: recordsSearch.value || undefined
    })
    records.value = res.records || []
    recordsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.admin.loadRecordsFailed') + ': ' + err.message)
  } finally {
    recordsLoading.value = false
  }
}

function searchRecords() {
  recordsPage.value = 1
  loadRecords()
}

async function loadUsers() {
  usersLoading.value = true
  try {
    const res = await api.entertainment.adminGetUserPoints({
      page: usersPage.value,
      pageSize: usersPageSize.value
    })
    users.value = res.records || []
    usersTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.admin.loadUsersFailed') + ': ' + err.message)
  } finally {
    usersLoading.value = false
  }
}

function loadTabData(tab: EntertainmentTab) {
  if (tab === 'records' && records.value.length === 0) {
    loadRecords()
  } else if (tab === 'users' && users.value.length === 0) {
    loadUsers()
  } else if (tab === 'badges' && badgeSeries.value.length === 0 && badges.value.length === 0) {
    loadBadgeCatalog()
  } else if (tab === 'checkin' && checkinLogs.value.length === 0) {
    loadCheckinAdminData()
  }
}

function switchTab(tab: EntertainmentTab) {
  activeTab.value = tab
  loadTabData(tab)
}

function refreshCurrentTab() {
  if (activeTab.value === 'lotteries') void loadLotteries()
  else if (activeTab.value === 'records') void loadRecords()
  else if (activeTab.value === 'users') void loadUsers()
  else if (activeTab.value === 'badges') void loadBadgeCatalog()
  else if (activeTab.value === 'checkin') void loadCheckinAdminData()
}

async function loadCheckinAdminData() {
  await Promise.all([loadCheckinSettings(), loadCheckinLogs()])
}

async function loadCheckinSettings() {
  checkinSettingsLoading.value = true
  try {
    checkinSettings.value = await api.entertainment.adminGetCheckinSettings()
  } catch (err: any) {
    toast.error(t('entertainment.admin.checkin.loadSettingsFailed') + ': ' + err.message)
  } finally {
    checkinSettingsLoading.value = false
  }
}

async function saveCheckinSettings() {
  if (checkinSettings.value.minPoints < 1 || checkinSettings.value.maxPoints < checkinSettings.value.minPoints) {
    toast.warning(t('entertainment.admin.checkin.invalidRange'))
    return
  }
  savingCheckinSettings.value = true
  try {
    checkinSettings.value = await api.entertainment.adminUpdateCheckinSettings({
      enabled: checkinSettings.value.enabled,
      minPoints: Number(checkinSettings.value.minPoints),
      maxPoints: Number(checkinSettings.value.maxPoints),
      requireInstance: checkinSettings.value.requireInstance
    })
    toast.success(t('entertainment.admin.checkin.saveSuccess'))
  } catch (err: any) {
    toast.error(t('entertainment.admin.checkin.saveFailed') + ': ' + err.message)
  } finally {
    savingCheckinSettings.value = false
  }
}

async function loadCheckinLogs() {
  checkinLogsLoading.value = true
  try {
    const res = await api.entertainment.adminGetCheckinLogs({
      page: checkinLogsPage.value,
      pageSize: checkinLogsPageSize.value,
      search: checkinLogsSearch.value || undefined,
      dateKey: checkinLogsDateKey.value || undefined
    })
    checkinLogs.value = res.records || []
    checkinLogsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.admin.checkin.loadLogsFailed') + ': ' + err.message)
  } finally {
    checkinLogsLoading.value = false
  }
}

function searchCheckinLogs() {
  checkinLogsPage.value = 1
  loadCheckinLogs()
}

// 打开创建抽奖弹窗
function openCreateLotteryModal() {
  editingLottery.value = null
  lotteryForm.value = {
    name: '',
    description: '',
    costPoints: 100,
    isActive: true,
    startAt: '',
    endAt: ''
  }
  showLotteryModal.value = true
}

// 打开编辑抽奖弹窗
function openEditLotteryModal(lottery: any) {
  editingLottery.value = lottery
  lotteryForm.value = {
    name: lottery.name,
    description: lottery.description || '',
    costPoints: lottery.costPoints,
    isActive: lottery.isActive,
    startAt: lottery.startAt ? lottery.startAt.split('T')[0] : '',
    endAt: lottery.endAt ? lottery.endAt.split('T')[0] : ''
  }
  showLotteryModal.value = true
}

// 保存抽奖
async function saveLottery() {
  if (!lotteryForm.value.name.trim()) {
    toast.warning(t('entertainment.admin.enterName'))
    return
  }
  if (lotteryForm.value.costPoints <= 0) {
    toast.warning(t('entertainment.admin.invalidCostPoints'))
    return
  }

  savingLottery.value = true
  try {
    const data = {
      name: lotteryForm.value.name.trim(),
      description: lotteryForm.value.description.trim() || undefined,
      costPoints: lotteryForm.value.costPoints,
      isActive: lotteryForm.value.isActive,
      startAt: lotteryForm.value.startAt || undefined,
      endAt: lotteryForm.value.endAt || undefined
    }

    if (editingLottery.value) {
      await api.entertainment.adminUpdateLottery(editingLottery.value.id, data)
      toast.success(t('entertainment.admin.updateSuccess'))
    } else {
      await api.entertainment.adminCreateLottery(data)
      toast.success(t('entertainment.admin.createSuccess'))
    }
    showLotteryModal.value = false
    loadLotteries()
  } catch (err: any) {
    toast.error(t('entertainment.admin.saveFailed') + ': ' + err.message)
  } finally {
    savingLottery.value = false
  }
}

// 删除抽奖
async function deleteLottery(id: number) {
  deleting.value = true
  try {
    await api.entertainment.adminDeleteLottery(id)
    toast.success(t('entertainment.admin.deleteSuccess'))
    deleteConfirmId.value = null
    loadLotteries()
  } catch (err: any) {
    toast.error(t('entertainment.admin.deleteFailed') + ': ' + err.message)
  } finally {
    deleting.value = false
  }
}

// 打开奖品管理弹窗
async function openPrizesModal(lottery: any) {
  editingPrizes.value = lottery
  prizes.value = lottery.prizes?.map((p: any) => ({ ...p, replenishAmount: 0 })) || []
  showPrizesModal.value = true
}

// 添加奖品
function addPrize() {
  prizes.value.push({
    id: 0,
    name: '',
    type: 'nothing',
    value: 0,
    probability: 10,
    totalQuantity: null,
    displayOrder: prizes.value.length,
    instanceDesc: ''
  })
}

// 删除奖品
function removePrize(index: number) {
  prizes.value.splice(index, 1)
}

// 保存奖品
async function savePrizes() {
  for (const prize of prizes.value) {
    if (!prize.name.trim()) {
      toast.warning(t('entertainment.admin.enterPrizeName'))
      return
    }
    if (prize.probability <= 0) {
      toast.warning(t('entertainment.admin.invalidProbability'))
      return
    }
  }

  savingPrizes.value = true
  try {
    const lotteryId = editingPrizes.value.id
    const existingPrizes = editingPrizes.value.prizes || []
    const existingIds = existingPrizes.map((p: any) => p.id)
    const newPrizeIds = prizes.value.filter(p => p.id).map(p => p.id)

    for (const existingId of existingIds) {
      if (!newPrizeIds.includes(existingId)) {
        await api.entertainment.adminDeletePrize(existingId)
      }
    }

    for (let i = 0; i < prizes.value.length; i++) {
      const prize = prizes.value[i]
      let newRemainQuantity = prize.remainQuantity
      if (prize.replenishAmount && prize.replenishAmount > 0) {
        newRemainQuantity = (prize.remainQuantity || 0) + prize.replenishAmount
      }

      const prizeData = {
        name: prize.name.trim(),
        type: prize.type,
        value: prize.value || 0,
        probability: prize.probability,
        totalQuantity: prize.totalQuantity || undefined,
        remainQuantity: newRemainQuantity,
        displayOrder: i,
        instanceDesc: prize.type === 'instance' ? prize.instanceDesc : undefined
      }

      if (prize.id && existingIds.includes(prize.id)) {
        await api.entertainment.adminUpdatePrize(prize.id, prizeData)
      } else {
        await api.entertainment.adminCreatePrize(lotteryId, prizeData)
      }
    }

    toast.success(t('entertainment.admin.savePrizesSuccess'))
    showPrizesModal.value = false
    loadLotteries()
  } catch (err: any) {
    toast.error(t('entertainment.admin.savePrizesFailed') + ': ' + err.message)
  } finally {
    savingPrizes.value = false
  }
}

function formatDate(dateStr?: string | null): string {
  if (!dateStr) return '-'
  try {
    return new Date(dateStr).toLocaleString()
  } catch {
    return String(dateStr)
  }
}

function getPrizeTypeName(type: string): string {
  const map: Record<string, string> = {
    nothing: t('entertainment.prizeTypes.nothing'),
    points: t('entertainment.prizeTypes.points'),
    balance: t('entertainment.prizeTypes.balance'),
    badge: t('entertainment.prizeTypes.badge'),
    instance: t('entertainment.prizeTypes.instance'),
    cpu: t('entertainment.prizeTypes.cpu'),
    memory: t('entertainment.prizeTypes.memory'),
    disk: t('entertainment.prizeTypes.disk'),
    traffic: t('entertainment.prizeTypes.traffic')
  }
  return map[type] || type
}

// 打开通知配置弹窗
function openNotificationModal(lottery: any) {
  editingNotification.value = lottery
  const config = lottery.notificationConfig
  if (config) {
    notificationForm.value = {
      enabled: config.enabled,
      type: config.type || 'telegram',
      notifyBalance: config.notifyBalance,
      notifyInstance: config.notifyInstance,
      telegramBotToken: config.config?.botToken || '',
      telegramChatId: config.config?.chatId || '',
      discordWebhookUrl: config.config?.webhookUrl || '',
      webhookUrl: config.config?.url || '',
      webhookSecret: config.config?.secret || ''
    }
  } else {
    notificationForm.value = {
      enabled: true,
      type: 'telegram',
      notifyBalance: true,
      notifyInstance: true,
      telegramBotToken: '',
      telegramChatId: '',
      discordWebhookUrl: '',
      webhookUrl: '',
      webhookSecret: ''
    }
  }
  showNotificationModal.value = true
}

// 保存通知配置
async function saveNotification() {
  const form = notificationForm.value

  let config: Record<string, string> = {}
  if (form.type === 'telegram') {
    if (!form.telegramBotToken || !form.telegramChatId) {
      toast.warning(t('entertainment.admin.notification.fillTelegram'))
      return
    }
    config = { botToken: form.telegramBotToken, chatId: form.telegramChatId }
  } else if (form.type === 'discord') {
    if (!form.discordWebhookUrl) {
      toast.warning(t('entertainment.admin.notification.fillDiscord'))
      return
    }
    config = { webhookUrl: form.discordWebhookUrl }
  } else if (form.type === 'webhook') {
    if (!form.webhookUrl) {
      toast.warning(t('entertainment.admin.notification.fillWebhook'))
      return
    }
    config = { url: form.webhookUrl, secret: form.webhookSecret }
  }

  savingNotification.value = true
  try {
    await api.entertainment.adminUpdateNotification(editingNotification.value.id, {
      enabled: form.enabled,
      type: form.type,
      config,
      notifyBalance: form.notifyBalance,
      notifyInstance: form.notifyInstance
    })
    toast.success(t('entertainment.admin.notification.saveSuccess'))
    showNotificationModal.value = false
    loadLotteries()
  } catch (err: any) {
    toast.error(t('entertainment.admin.notification.saveFailed') + ': ' + err.message)
  } finally {
    savingNotification.value = false
  }
}

async function loadBadgeCatalog() {
  badgesLoading.value = true
  try {
    const res = await api.entertainment.adminGetBadgeCatalog()
    badgeSeries.value = res.series || []
    badges.value = res.badges || []
  } catch (err: any) {
    toast.error(t('entertainment.admin.badgeCatalog.loadFailed') + ': ' + err.message)
  } finally {
    badgesLoading.value = false
  }
}

function openCreateSeriesModal() {
  editingSeries.value = null
  seriesForm.value = {
    id: '',
    title: '',
    nameZh: '',
    nameEn: '',
    description: '',
    sourceId: '',
    sourceLabel: '',
    displayOrder: (badgeSeries.value.length + 1) * 100,
    isActive: true
  }
  showSeriesModal.value = true
}

function openEditSeriesModal(series: BadgeSeriesItem) {
  editingSeries.value = series
  seriesForm.value = {
    id: series.id,
    title: series.title,
    nameZh: series.nameZh,
    nameEn: series.nameEn || '',
    description: series.description,
    sourceId: series.sourceId || '',
    sourceLabel: series.sourceLabel || '',
    displayOrder: series.displayOrder,
    isActive: series.isActive
  }
  showSeriesModal.value = true
}

async function saveSeries() {
  const form = seriesForm.value
  if (!form.id.trim() || !form.title.trim() || !form.nameZh.trim() || !form.description.trim()) {
    toast.warning(t('entertainment.admin.badgeCatalog.fillSeriesRequired'))
    return
  }

  savingSeries.value = true
  try {
    const data = {
      title: form.title.trim(),
      nameZh: form.nameZh.trim(),
      nameEn: form.nameEn.trim() || null,
      description: form.description.trim(),
      sourceId: form.sourceId.trim() || null,
      sourceLabel: form.sourceLabel.trim() || null,
      displayOrder: Number(form.displayOrder) || 0,
      isActive: form.isActive
    }
    if (editingSeries.value) {
      await api.entertainment.adminUpdateBadgeSeries(editingSeries.value.id, data)
      toast.success(t('entertainment.admin.badgeCatalog.seriesUpdated'))
    } else {
      await api.entertainment.adminCreateBadgeSeries({ id: form.id.trim(), ...data })
      toast.success(t('entertainment.admin.badgeCatalog.seriesCreated'))
    }
    showSeriesModal.value = false
    await loadBadgeCatalog()
    await badgeStore.loadCatalog(true)
  } catch (err: any) {
    toast.error(t('entertainment.admin.badgeCatalog.saveSeriesFailed') + ': ' + err.message)
  } finally {
    savingSeries.value = false
  }
}

async function deleteSeries(id: string) {
  try {
    await api.entertainment.adminDeleteBadgeSeries(id)
    toast.success(t('entertainment.admin.badgeCatalog.seriesDeleted'))
    deletingSeriesId.value = null
    if (selectedBadgeSeriesId.value === id) selectedBadgeSeriesId.value = 'all'
    await loadBadgeCatalog()
    await badgeStore.loadCatalog(true)
  } catch (err: any) {
    toast.error(t('entertainment.admin.badgeCatalog.deleteSeriesFailed') + ': ' + err.message)
  }
}

function openCreateBadgeModal(seriesId?: string) {
  editingBadge.value = null
  const targetSeriesId = seriesId && seriesId !== 'all'
    ? seriesId
    : badgeSeries.value[0]?.id || ''
  badgeForm.value = {
    id: '',
    name: '',
    nameEn: '',
    fullLabel: '',
    seriesId: targetSeriesId,
    sourceId: '',
    sourceLabel: '',
    assetUrl: '',
    assetUrlDark: '',
    assetUrlLight: '',
    displayOrder: (badges.value.filter(badge => badge.seriesId === targetSeriesId).length + 1) * 10,
    isActive: true
  }
  showBadgeModal.value = true
}

function openEditBadgeModal(badge: BadgeCatalogItem) {
  editingBadge.value = badge
  badgeForm.value = {
    id: badge.id,
    name: badge.name,
    nameEn: badge.nameEn || '',
    fullLabel: badge.fullLabel,
    seriesId: badge.seriesId,
    sourceId: badge.sourceId || '',
    sourceLabel: badge.sourceLabel || '',
    assetUrl: badge.assetUrl,
    assetUrlDark: badge.assetUrlDark || '',
    assetUrlLight: badge.assetUrlLight || '',
    displayOrder: badge.displayOrder || 0,
    isActive: badge.isActive ?? true
  }
  showBadgeModal.value = true
}

async function saveBadge() {
  const form = badgeForm.value
  if (!form.id.trim() || !form.name.trim() || !form.fullLabel.trim() || !form.seriesId.trim() || !form.assetUrl.trim()) {
    toast.warning(t('entertainment.admin.badgeCatalog.fillBadgeRequired'))
    return
  }

  savingBadge.value = true
  try {
    const data = {
      name: form.name.trim(),
      nameEn: form.nameEn.trim() || null,
      fullLabel: form.fullLabel.trim(),
      seriesId: form.seriesId.trim(),
      sourceId: form.sourceId.trim() || null,
      sourceLabel: form.sourceLabel.trim() || null,
      assetUrl: form.assetUrl.trim(),
      assetUrlDark: form.assetUrlDark.trim() || null,
      assetUrlLight: form.assetUrlLight.trim() || null,
      displayOrder: Number(form.displayOrder) || 0,
      isActive: form.isActive
    }
    if (editingBadge.value) {
      await api.entertainment.adminUpdateBadge(editingBadge.value.id, data)
      toast.success(t('entertainment.admin.badgeCatalog.badgeUpdated'))
    } else {
      await api.entertainment.adminCreateBadge({ id: form.id.trim(), ...data })
      toast.success(t('entertainment.admin.badgeCatalog.badgeCreated'))
    }
    showBadgeModal.value = false
    await loadBadgeCatalog()
    await badgeStore.loadCatalog(true)
  } catch (err: any) {
    toast.error(t('entertainment.admin.badgeCatalog.saveBadgeFailed') + ': ' + err.message)
  } finally {
    savingBadge.value = false
  }
}

async function deleteBadge(id: string) {
  try {
    await api.entertainment.adminDeleteBadge(id)
    toast.success(t('entertainment.admin.badgeCatalog.badgeDeleted'))
    deletingBadgeId.value = null
    await loadBadgeCatalog()
    await badgeStore.loadCatalog(true)
  } catch (err: any) {
    toast.error(t('entertainment.admin.badgeCatalog.deleteBadgeFailed') + ': ' + err.message)
  }
}

function getSeriesTitle(seriesId: string): string {
  return badgeSeries.value.find(series => series.id === seriesId)?.title || seriesId
}
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- 页面标题 -->
    <PageHeader
      :title="t('entertainment.admin.title')"
      :description="t('entertainment.admin.description')"
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            :loading="isTabLoading"
            @click="refreshCurrentTab"
          >
            <template #icon>
              <RotateCw class="w-4 h-4" :class="{ 'animate-spin': isTabLoading }" />
            </template>
            {{ t('common.refresh') || '刷新' }}
          </Button>

          <Button
            v-if="activeTab === 'lotteries'"
            variant="primary"
            size="sm"
            @click="openCreateLotteryModal"
          >
            <template #icon>
              <Plus class="w-4 h-4" />
            </template>
            {{ t('entertainment.admin.createLottery') }}
          </Button>

          <template v-else-if="activeTab === 'badges'">
            <Button variant="secondary" size="sm" @click="openCreateSeriesModal">
              <template #icon>
                <Plus class="w-4 h-4" />
              </template>
              {{ t('entertainment.admin.badgeCatalog.addSeries') }}
            </Button>
            <Button
              variant="primary"
              size="sm"
              :disabled="badgeSeries.length === 0"
              @click="openCreateBadgeModal(selectedBadgeSeriesId)"
            >
              <template #icon>
                <Sparkles class="w-4 h-4" />
              </template>
              {{ t('entertainment.admin.badgeCatalog.addBadge') }}
            </Button>
          </template>
        </div>
      </template>
    </PageHeader>

    <!-- 概览指标卡 -->
    <div data-reveal class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('entertainment.admin.active') }} {{ t('entertainment.admin.tabs.lotteries') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Sparkles class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ activeLotteriesCount }} <span class="text-xs font-normal text-[var(--text-tertiary)]">/ {{ lotteriesTotal || lotteries.length }}</span>
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('entertainment.admin.totalDraws') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Trophy class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
          {{ totalDrawsSum.toLocaleString() }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('entertainment.admin.tabs.users') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Users class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--warning)]">
          {{ (usersTotal || users.length).toLocaleString() }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('entertainment.admin.badgeCatalog.badges.title') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Award class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ badges.length }} <span class="text-xs font-normal text-[var(--text-tertiary)]">{{ t('entertainment.admin.badgeCatalog.series.title') }}: {{ badgeSeries.length }}</span>
        </div>
      </Card>
    </div>

    <!-- Apple 风格分段控制器 (Segmented Tab Control) -->
    <div data-reveal class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full">
      <button
        v-for="tab in tabs"
        :key="tab.key"
        type="button"
        class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none cursor-pointer"
        :class="activeTab === tab.key
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="switchTab(tab.key)"
      >
        <component :is="tab.icon" class="w-4 h-4 stroke-[1.8]" />
        <span>{{ tab.label }}</span>
        <Badge
          v-if="tab.count !== undefined"
          size="sm"
          :variant="activeTab === tab.key ? 'info' : 'default'"
        >
          {{ tab.count }}
        </Badge>
      </button>
    </div>

    <!-- 1. 签到设置与日志 TAB -->
    <div v-show="activeTab === 'checkin'" class="space-y-6">
      <!-- 签到设置卡片 -->
      <Card data-reveal padding="md">
        <div class="flex items-center justify-between mb-4 pb-3 border-b border-[var(--border-color)]">
          <div>
            <h3 class="text-base font-semibold text-[var(--text-primary)]">
              {{ $t('entertainment.admin.tabs.checkin') }}
            </h3>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">
              {{ $t('entertainment.admin.description') }}
            </p>
          </div>
          <Badge :variant="checkinSettings.enabled ? 'success' : 'default'" size="sm">
            {{ checkinSettings.enabled ? $t('common.active') : $t('common.inactive') }}
          </Badge>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4 items-end">
          <div>
            <span class="block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-1.5">
              {{ $t('entertainment.admin.checkin.minPoints') }}
            </span>
            <Input
              v-model.number="checkinSettings.minPoints"
              type="number"
              :min="1"
              size="md"
              placeholder="1"
            />
          </div>
          <div>
            <span class="block text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-1.5">
              {{ $t('entertainment.admin.checkin.maxPoints') }}
            </span>
            <Input
              v-model.number="checkinSettings.maxPoints"
              type="number"
              :min="1"
              size="md"
              placeholder="500"
            />
          </div>
          <div class="space-y-2">
            <label class="flex items-center gap-2.5 cursor-pointer select-none">
              <input
                v-model="checkinSettings.enabled"
                type="checkbox"
                class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
              />
              <span class="text-xs font-medium text-[var(--text-primary)]">{{ $t('entertainment.admin.checkin.enabled') }}</span>
            </label>
            <label class="flex items-center gap-2.5 cursor-pointer select-none">
              <input
                v-model="checkinSettings.requireInstance"
                type="checkbox"
                class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
              />
              <span class="text-xs font-medium text-[var(--text-primary)]">{{ $t('entertainment.admin.checkin.requireInstance') }}</span>
            </label>
          </div>
          <div class="flex justify-end">
            <Button
              variant="primary"
              size="md"
              block
              :loading="savingCheckinSettings"
              :disabled="checkinSettingsLoading"
              @click="saveCheckinSettings"
            >
              <template #icon>
                <Check class="w-4 h-4" />
              </template>
              {{ savingCheckinSettings ? $t('common.saving') : $t('common.save') }}
            </Button>
          </div>
        </div>
      </Card>

      <!-- 签到日志卡片 -->
      <Card data-reveal padding="none" class="overflow-hidden">
        <!-- 搜索与筛选工具栏 -->
        <div class="p-4 border-b border-[var(--border-color)] flex flex-wrap gap-3 items-center justify-between">
          <div class="flex flex-wrap items-center gap-3 flex-1">
            <div class="w-full sm:w-64">
              <Input
                v-model="checkinLogsSearch"
                size="sm"
                :placeholder="$t('entertainment.admin.searchUser')"
                @enter="searchCheckinLogs"
              >
                <template #prefix>
                  <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>
            <div class="w-full sm:w-44">
              <input
                v-model="checkinLogsDateKey"
                type="date"
                class="w-full h-8 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                @change="searchCheckinLogs"
              />
            </div>
            <Button variant="secondary" size="sm" @click="searchCheckinLogs">
              <template #icon>
                <Search class="w-3.5 h-3.5" />
              </template>
              {{ $t('common.search') }}
            </Button>
          </div>
        </div>

        <div v-if="checkinLogsLoading" class="p-12 text-center text-[var(--text-secondary)]">
          <Loader2 class="w-6 h-6 animate-spin mx-auto mb-2 text-[var(--accent)]" />
          <span>{{ $t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="checkinLogs.length === 0"
          :title="$t('entertainment.admin.checkin.noLogs')"
        />
        <template v-else>
          <!-- 移动端卡片 (极度关键守卫) -->
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="record in checkinLogs"
              :key="record.id"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">
                    {{ record.username }} <span class="font-mono text-xs text-[var(--text-tertiary)]">#{{ record.userId }}</span>
                  </div>
                  <div class="mt-1 font-mono text-xs text-[var(--text-secondary)]">{{ record.dateKey }}</div>
                </div>
                <div class="shrink-0 font-mono text-sm font-semibold text-[var(--warning)]">+{{ record.points }}</div>
              </div>
              <div class="mt-3 grid grid-cols-2 gap-3 text-xs pt-3 border-t border-[var(--border-color)]">
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.checkin.streakDays') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-primary)]">{{ record.streakDays }}</div>
                </div>
                <div>
                  <div class="text-[var(--text-secondary)]">IP</div>
                  <div class="mt-1 break-all font-mono font-medium text-[var(--text-secondary)]">{{ record.ipAddress || '-' }}</div>
                </div>
                <div class="col-span-2">
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.checkin.time') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-secondary)]">{{ formatDate(record.createdAt) }}</div>
                </div>
              </div>
            </div>
          </div>

          <!-- PC 端定宽表格 (极度关键守卫) -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                <tr>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[22%]">{{ $t('entertainment.admin.user') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[14%]">{{ $t('entertainment.admin.checkin.date') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.checkin.points') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.checkin.streakDays') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[18%]">IP</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[22%]">{{ $t('entertainment.admin.checkin.time') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr v-for="record in checkinLogs" :key="record.id" class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150">
                  <td class="truncate px-4 py-3 text-sm text-[var(--text-primary)]">
                    <span class="font-medium">{{ record.username }}</span>
                    <span class="ml-1.5 font-mono text-xs text-[var(--text-tertiary)]">#{{ record.userId }}</span>
                  </td>
                  <td class="truncate px-4 py-3 text-sm font-mono text-[var(--text-primary)] tabular-nums">{{ record.dateKey }}</td>
                  <td class="px-4 py-3 text-sm font-mono font-medium text-[var(--warning)] tabular-nums">+{{ record.points }}</td>
                  <td class="px-4 py-3 text-sm font-mono text-[var(--text-primary)] tabular-nums">{{ record.streakDays }}</td>
                  <td class="truncate px-4 py-3 text-sm font-mono text-[var(--text-secondary)] tabular-nums">{{ record.ipAddress || '-' }}</td>
                  <td class="truncate px-4 py-3 text-sm font-mono text-[var(--text-secondary)] tabular-nums">{{ formatDate(record.createdAt) }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div v-if="checkinLogsTotal > checkinLogsPageSize" class="p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between">
            <span class="text-xs text-[var(--text-secondary)]">
              {{ $t('common.total') || '共' }} <span class="font-mono tabular-nums text-[var(--text-primary)] font-medium">{{ checkinLogsTotal }}</span> {{ $t('common.items') || '条' }}
            </span>
            <Pagination
              :page="checkinLogsPage"
              :page-size="checkinLogsPageSize"
              :total="checkinLogsTotal"
              :show-total="false"
              @update:page="val => { checkinLogsPage = val; loadCheckinLogs() }"
            />
          </div>
        </template>
      </Card>
    </div>

    <!-- 2. 抽奖活动 TAB -->
    <div v-show="activeTab === 'lotteries'" class="space-y-6">
      <Card data-reveal padding="none" class="overflow-hidden">
        <div v-if="lotteriesLoading" class="p-12 text-center text-[var(--text-secondary)]">
          <Loader2 class="w-6 h-6 animate-spin mx-auto mb-2 text-[var(--accent)]" />
          <span>{{ $t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="lotteries.length === 0"
          :title="$t('entertainment.admin.noLotteries')"
        >
          <template #action>
            <Button variant="primary" size="sm" @click="openCreateLotteryModal">
              <template #icon>
                <Plus class="w-4 h-4" />
              </template>
              {{ $t('entertainment.admin.createLottery') }}
            </Button>
          </template>
        </EmptyState>
        <template v-else>
          <!-- 移动端卡片 (极度关键守卫) -->
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="lottery in lotteries"
              :key="lottery.id"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ lottery.name }}</div>
                  <div v-if="lottery.description" class="mt-1 line-clamp-2 text-xs text-[var(--text-secondary)]">
                    {{ lottery.description }}
                  </div>
                </div>
                <Badge :variant="lottery.isActive ? 'success' : 'default'" size="sm" class="shrink-0">
                  {{ lottery.isActive ? $t('entertainment.admin.active') : $t('entertainment.admin.inactive') }}
                </Badge>
              </div>
              <div class="mt-3 grid grid-cols-3 gap-3 text-xs pt-3 border-t border-[var(--border-color)]">
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.costPoints') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-primary)]">{{ lottery.costPoints }}</div>
                </div>
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.prizes') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-primary)]">{{ lottery.prizesCount || lottery.prizes?.length || 0 }}</div>
                </div>
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.totalDraws') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-primary)]">{{ lottery.totalDraws }}</div>
                </div>
              </div>
              <div class="mt-4 flex flex-wrap gap-2 pt-3 border-t border-[var(--border-color)]">
                <Button variant="secondary" size="sm" :title="$t('entertainment.admin.managePrizes')" @click="openPrizesModal(lottery)">
                  <template #icon>
                    <Gift class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('entertainment.admin.managePrizes') }}
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  :title="$t('entertainment.admin.notification.title')"
                  @click="openNotificationModal(lottery)"
                >
                  <template #icon>
                    <Bell class="w-3.5 h-3.5" :class="lottery.notificationConfig?.enabled ? 'text-[var(--success)]' : ''" />
                  </template>
                  {{ $t('entertainment.admin.notification.title') }}
                </Button>
                <Button variant="secondary" size="sm" @click="openEditLotteryModal(lottery)">
                  <template #icon>
                    <Pencil class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('common.edit') }}
                </Button>
                <Button
                  v-if="deleteConfirmId !== lottery.id"
                  variant="ghost"
                  size="sm"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                  @click="deleteConfirmId = lottery.id"
                >
                  <template #icon>
                    <Trash2 class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('common.delete') }}
                </Button>
                <template v-else>
                  <Button
                    variant="danger"
                    size="sm"
                    :disabled="deleting"
                    @click="deleteLottery(lottery.id)"
                  >
                    {{ $t('common.confirm') }}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    @click="deleteConfirmId = null"
                  >
                    {{ $t('common.cancel') }}
                  </Button>
                </template>
              </div>
            </div>
          </div>

          <!-- PC 端定宽表格 (极度关键守卫) -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                <tr>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[30%]">{{ $t('entertainment.admin.lotteryName') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.costPoints') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[10%]">{{ $t('entertainment.admin.prizes') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.totalDraws') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.status') }}</th>
                  <th class="text-right text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[24%]">{{ $t('common.actions') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr v-for="lottery in lotteries" :key="lottery.id" class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150">
                  <td class="px-4 py-3.5">
                    <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ lottery.name }}</div>
                    <div v-if="lottery.description" class="truncate text-xs text-[var(--text-secondary)] mt-0.5">
                      {{ lottery.description }}
                    </div>
                  </td>
                  <td class="px-4 py-3.5 text-sm font-mono text-[var(--text-primary)] tabular-nums">{{ lottery.costPoints }}</td>
                  <td class="px-4 py-3.5 text-sm font-mono text-[var(--text-primary)] tabular-nums">{{ lottery.prizesCount || lottery.prizes?.length || 0 }}</td>
                  <td class="px-4 py-3.5 text-sm font-mono text-[var(--text-primary)] tabular-nums">{{ lottery.totalDraws }}</td>
                  <td class="px-4 py-3.5">
                    <Badge :variant="lottery.isActive ? 'success' : 'default'" size="sm">
                      {{ lottery.isActive ? $t('entertainment.admin.active') : $t('entertainment.admin.inactive') }}
                    </Badge>
                  </td>
                  <td class="px-4 py-3.5 text-right">
                    <div class="flex items-center justify-end gap-1.5">
                      <Button
                        size="sm"
                        variant="ghost"
                        :title="$t('entertainment.admin.managePrizes')"
                        @click="openPrizesModal(lottery)"
                      >
                        <template #icon>
                          <Gift class="w-3.5 h-3.5" />
                        </template>
                        {{ $t('entertainment.admin.managePrizes') }}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        :title="$t('entertainment.admin.notification.title')"
                        @click="openNotificationModal(lottery)"
                      >
                        <template #icon>
                          <Bell class="w-3.5 h-3.5" :class="lottery.notificationConfig?.enabled ? 'text-[var(--success)]' : ''" />
                        </template>
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        :title="$t('common.edit')"
                        @click="openEditLotteryModal(lottery)"
                      >
                        <template #icon>
                          <Pencil class="w-3.5 h-3.5" />
                        </template>
                      </Button>
                      <div v-if="deleteConfirmId !== lottery.id">
                        <Button
                          size="sm"
                          variant="ghost"
                          class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                          :title="$t('common.delete')"
                          @click="deleteConfirmId = lottery.id"
                        >
                          <template #icon>
                            <Trash2 class="w-3.5 h-3.5" />
                          </template>
                        </Button>
                      </div>
                      <template v-else>
                        <div class="inline-flex items-center gap-1">
                          <Button
                            size="sm"
                            variant="danger"
                            :disabled="deleting"
                            @click="deleteLottery(lottery.id)"
                          >
                            {{ $t('common.confirm') }}
                          </Button>
                          <Button
                            size="sm"
                            variant="secondary"
                            @click="deleteConfirmId = null"
                          >
                            {{ $t('common.cancel') }}
                          </Button>
                        </div>
                      </template>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div v-if="lotteriesTotal > lotteriesPageSize" class="p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between">
            <span class="text-xs text-[var(--text-secondary)]">
              {{ $t('common.total') || '共' }} <span class="font-mono tabular-nums text-[var(--text-primary)] font-medium">{{ lotteriesTotal }}</span> {{ $t('common.items') || '条' }}
            </span>
            <Pagination
              :page="lotteriesPage"
              :page-size="lotteriesPageSize"
              :total="lotteriesTotal"
              :show-total="false"
              @update:page="val => { lotteriesPage = val; loadLotteries() }"
            />
          </div>
        </template>
      </Card>
    </div>

    <!-- 3. 中奖记录 TAB -->
    <div v-show="activeTab === 'records'" class="space-y-6">
      <Card data-reveal padding="none" class="overflow-hidden">
        <!-- 搜索和筛选 -->
        <div class="p-4 border-b border-[var(--border-color)] flex flex-wrap gap-3 items-center justify-between">
          <div class="flex flex-wrap items-center gap-3">
            <div class="w-full sm:w-56">
              <Input
                v-model="recordsSearch"
                size="sm"
                :placeholder="$t('entertainment.admin.searchUser')"
                @enter="searchRecords"
              >
                <template #prefix>
                  <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>
            <Button variant="primary" size="sm" @click="searchRecords">
              <template #icon>
                <Search class="w-3.5 h-3.5" />
              </template>
              {{ $t('common.search') }}
            </Button>

            <div class="flex items-center gap-2 ml-0 sm:ml-2">
              <span class="text-xs text-[var(--text-secondary)]">{{ $t('entertainment.admin.prizeType') }}:</span>
              <select
                v-model="recordsPrizeType"
                class="h-8 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 text-xs text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                @change="searchRecords"
              >
                <option value="">{{ $t('common.all') }}</option>
                <option value="nothing">{{ $t('entertainment.prizeTypes.nothing') }}</option>
                <option value="points">{{ $t('entertainment.prizeTypes.points') }}</option>
                <option value="balance">{{ $t('entertainment.prizeTypes.balance') }}</option>
                <option value="badge">{{ $t('entertainment.prizeTypes.badge') }}</option>
                <option value="instance">{{ $t('entertainment.prizeTypes.instance') }}</option>
                <option value="cpu">{{ $t('entertainment.prizeTypes.cpu') }}</option>
                <option value="memory">{{ $t('entertainment.prizeTypes.memory') }}</option>
                <option value="disk">{{ $t('entertainment.prizeTypes.disk') }}</option>
                <option value="traffic">{{ $t('entertainment.prizeTypes.traffic') }}</option>
              </select>
            </div>
          </div>
        </div>

        <div v-if="recordsLoading" class="p-12 text-center text-[var(--text-secondary)]">
          <Loader2 class="w-6 h-6 animate-spin mx-auto mb-2 text-[var(--accent)]" />
          <span>{{ $t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="records.length === 0"
          :title="$t('entertainment.admin.noRecords')"
        />
        <template v-else>
          <!-- 移动端卡片 (极度关键守卫) -->
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="rec in records"
              :key="rec.id"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ rec.username || rec.userId }}</div>
                  <div class="mt-1 truncate text-xs text-[var(--text-secondary)]">{{ rec.lotteryName || '-' }}</div>
                </div>
                <Badge variant="info" size="sm" class="shrink-0">{{ getPrizeTypeName(rec.prizeType) }}</Badge>
              </div>
              <div class="mt-3 grid grid-cols-2 gap-3 text-xs pt-3 border-t border-[var(--border-color)]">
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.prize') }}</div>
                  <div class="mt-1 font-medium text-[var(--text-primary)]">{{ rec.prizeName || '-' }}</div>
                </div>
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.value') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--text-primary)]">
                    <template v-if="rec.prizeType === 'points'">+{{ rec.prizeValue }}</template>
                    <template v-else-if="rec.prizeType === 'balance'">+¥{{ (rec.prizeValue / 100).toFixed(2) }}</template>
                    <template v-else-if="rec.prizeType === 'badge'">{{ rec.prizeName || $t('entertainment.prizeTypes.badge') }}</template>
                    <template v-else-if="rec.prizeType === 'instance'">{{ rec.instanceDesc || $t('entertainment.wonInstance') }}</template>
                    <template v-else-if="rec.prizeType === 'cpu'">+{{ rec.prizeValue }}%</template>
                    <template v-else-if="rec.prizeType === 'memory'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'disk'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'traffic'">+{{ rec.prizeValue }}GB</template>
                    <template v-else>-</template>
                  </div>
                </div>
                <div class="col-span-2">
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.time') }}</div>
                  <div class="mt-1 font-mono text-[var(--text-secondary)]">{{ formatDate(rec.createdAt) }}</div>
                </div>
              </div>
            </div>
          </div>

          <!-- PC 端定宽表格 (极度关键守卫) -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                <tr>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[18%]">{{ $t('entertainment.admin.user') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[20%]">{{ $t('entertainment.admin.lotteryName') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[20%]">{{ $t('entertainment.admin.prize') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[13%]">{{ $t('entertainment.admin.prizeType') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[14%]">{{ $t('entertainment.admin.value') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[15%]">{{ $t('entertainment.time') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr v-for="rec in records" :key="rec.id" class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150">
                  <td class="truncate px-4 py-3.5 text-sm font-medium text-[var(--text-primary)]">{{ rec.username || rec.userId }}</td>
                  <td class="truncate px-4 py-3.5 text-sm text-[var(--text-secondary)]">{{ rec.lotteryName || '-' }}</td>
                  <td class="truncate px-4 py-3.5 text-sm text-[var(--text-primary)] font-medium">{{ rec.prizeName || '-' }}</td>
                  <td class="px-4 py-3.5 text-sm">
                    <Badge variant="info" size="sm">{{ getPrizeTypeName(rec.prizeType) }}</Badge>
                  </td>
                  <td class="truncate px-4 py-3.5 text-sm font-mono text-[var(--text-primary)] tabular-nums">
                    <template v-if="rec.prizeType === 'points'">+{{ rec.prizeValue }}</template>
                    <template v-else-if="rec.prizeType === 'balance'">+¥{{ (rec.prizeValue / 100).toFixed(2) }}</template>
                    <template v-else-if="rec.prizeType === 'badge'">{{ rec.prizeName || $t('entertainment.prizeTypes.badge') }}</template>
                    <template v-else-if="rec.prizeType === 'instance'">{{ rec.instanceDesc || $t('entertainment.wonInstance') }}</template>
                    <template v-else-if="rec.prizeType === 'cpu'">+{{ rec.prizeValue }}%</template>
                    <template v-else-if="rec.prizeType === 'memory'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'disk'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'traffic'">+{{ rec.prizeValue }}GB</template>
                    <template v-else>-</template>
                  </td>
                  <td class="truncate px-4 py-3.5 text-sm font-mono text-[var(--text-secondary)] tabular-nums">{{ formatDate(rec.createdAt) }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页与每页条数 -->
          <div class="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-3 p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
            <div class="flex items-center gap-2">
              <span class="text-xs text-[var(--text-secondary)]">{{ $t('common.perPage') }}:</span>
              <select 
                v-model.number="recordsPageSize" 
                class="h-8 rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] px-2.5 text-xs text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                @change="recordsPage = 1; loadRecords()"
              >
                <option :value="10">10</option>
                <option :value="20">20</option>
                <option :value="50">50</option>
                <option :value="100">100</option>
              </select>
              <span class="text-xs text-[var(--text-secondary)] ml-2">
                {{ $t('common.total') || '共' }} <span class="font-mono tabular-nums text-[var(--text-primary)] font-medium">{{ recordsTotal }}</span> {{ $t('common.items') || '条' }}
              </span>
            </div>
            <Pagination
              :page="recordsPage"
              :page-size="recordsPageSize"
              :total="recordsTotal"
              :show-total="false"
              @update:page="val => { recordsPage = val; loadRecords() }"
            />
          </div>
        </template>
      </Card>
    </div>

    <!-- 4. 用户积分 TAB -->
    <div v-show="activeTab === 'users'" class="space-y-6">
      <Card data-reveal padding="none" class="overflow-hidden">
        <div v-if="usersLoading" class="p-12 text-center text-[var(--text-secondary)]">
          <Loader2 class="w-6 h-6 animate-spin mx-auto mb-2 text-[var(--accent)]" />
          <span>{{ $t('common.loading') }}...</span>
        </div>
        <EmptyState
          v-else-if="users.length === 0"
          :title="$t('entertainment.admin.noUsers')"
        />
        <template v-else>
          <!-- 移动端卡片 (极度关键守卫) -->
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="user in users"
              :key="user.userId"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ user.username || user.userId }}</div>
                  <div class="mt-1 text-xs text-[var(--text-secondary)]">{{ $t('entertainment.admin.currentPoints') }}</div>
                </div>
                <div class="shrink-0 font-mono text-sm font-semibold text-[var(--warning)]">{{ user.points?.toLocaleString() }}</div>
              </div>
              <div class="mt-3 grid grid-cols-2 gap-3 text-xs pt-3 border-t border-[var(--border-color)]">
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.totalEarned') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--success)]">+{{ user.totalEarned?.toLocaleString() }}</div>
                </div>
                <div>
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.totalSpent') }}</div>
                  <div class="mt-1 font-mono font-medium text-[var(--error)]">-{{ user.totalSpent?.toLocaleString() }}</div>
                </div>
                <div class="col-span-2">
                  <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.lastConvertedAt') }}</div>
                  <div class="mt-1 font-mono text-[var(--text-secondary)]">{{ user.lastConvertedAt ? formatDate(user.lastConvertedAt) : '-' }}</div>
                </div>
              </div>
            </div>
          </div>

          <!-- PC 端定宽表格 (极度关键守卫) -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                <tr>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[28%]">{{ $t('entertainment.admin.user') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[16%]">{{ $t('entertainment.admin.currentPoints') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[16%]">{{ $t('entertainment.admin.totalEarned') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[16%]">{{ $t('entertainment.admin.totalSpent') }}</th>
                  <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[24%]">{{ $t('entertainment.admin.lastConvertedAt') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr v-for="user in users" :key="user.userId" class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150">
                  <td class="truncate px-4 py-3.5 text-sm font-medium text-[var(--text-primary)]">{{ user.username || user.userId }}</td>
                  <td class="px-4 py-3.5 text-sm font-mono font-semibold text-[var(--warning)] tabular-nums">{{ user.points?.toLocaleString() }}</td>
                  <td class="px-4 py-3.5 text-sm font-mono text-[var(--success)] tabular-nums">+{{ user.totalEarned?.toLocaleString() }}</td>
                  <td class="px-4 py-3.5 text-sm font-mono text-[var(--error)] tabular-nums">-{{ user.totalSpent?.toLocaleString() }}</td>
                  <td class="truncate px-4 py-3.5 text-sm font-mono text-[var(--text-secondary)] tabular-nums">
                    {{ user.lastConvertedAt ? formatDate(user.lastConvertedAt) : '-' }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div v-if="usersTotal > usersPageSize" class="p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex items-center justify-between">
            <span class="text-xs text-[var(--text-secondary)]">
              {{ $t('common.total') || '共' }} <span class="font-mono tabular-nums text-[var(--text-primary)] font-medium">{{ usersTotal }}</span> {{ $t('common.items') || '条' }}
            </span>
            <Pagination
              :page="usersPage"
              :page-size="usersPageSize"
              :total="usersTotal"
              :show-total="false"
              @update:page="val => { usersPage = val; loadUsers() }"
            />
          </div>
        </template>
      </Card>
    </div>

    <!-- 5. 徽章管理 TAB -->
    <div v-show="activeTab === 'badges'" class="space-y-6">
      <div v-if="badgesLoading" class="p-12 text-center text-[var(--text-secondary)]">
        <Loader2 class="w-6 h-6 animate-spin mx-auto mb-2 text-[var(--accent)]" />
        <span>{{ $t('common.loading') }}...</span>
      </div>
      <template v-else>
        <div class="grid grid-cols-1 xl:grid-cols-[360px_1fr] gap-6">
          <!-- 系列列表 Card -->
          <Card data-reveal padding="none" class="overflow-hidden">
            <div class="p-4 border-b border-[var(--border-color)] flex items-center justify-between">
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ $t('entertainment.admin.badgeCatalog.series.title') }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ $t('entertainment.admin.badgeCatalog.series.description') }}</div>
              </div>
              <Button variant="secondary" size="sm" @click="openCreateSeriesModal">
                <template #icon>
                  <Plus class="w-3.5 h-3.5" />
                </template>
                {{ $t('entertainment.admin.badgeCatalog.series.add') }}
              </Button>
            </div>
            <div class="divide-y divide-[var(--border-color)]">
              <!-- 全部系列按钮 -->
              <button
                type="button"
                class="w-full px-4 py-3 text-left flex items-center justify-between transition-colors select-none cursor-pointer"
                :class="selectedBadgeSeriesId === 'all' ? 'bg-[var(--bg-surface-soft)] font-semibold' : 'hover:bg-[var(--bg-surface-soft)]/50'"
                @click="selectedBadgeSeriesId = 'all'"
              >
                <span class="text-sm text-[var(--text-primary)]">{{ $t('entertainment.admin.badgeCatalog.series.all') }}</span>
                <Badge size="sm" :variant="selectedBadgeSeriesId === 'all' ? 'info' : 'default'">
                  {{ badges.length }}
                </Badge>
              </button>

              <!-- 系列项目 -->
              <div
                v-for="series in badgeSeries"
                :key="series.id"
                class="p-4 transition-colors"
                :class="selectedBadgeSeriesId === series.id ? 'bg-[var(--bg-surface-soft)]' : 'hover:bg-[var(--bg-surface-soft)]/40'"
              >
                <div class="flex items-start justify-between gap-3">
                  <button
                    type="button"
                    class="min-w-0 text-left flex-1 cursor-pointer select-none"
                    @click="selectedBadgeSeriesId = series.id"
                  >
                    <div class="flex items-center gap-2">
                      <span class="text-sm font-semibold text-[var(--text-primary)] truncate">{{ series.nameZh }}</span>
                      <Badge size="sm" :variant="series.isActive ? 'success' : 'default'">
                        {{ series.isActive ? $t('common.active') : $t('common.inactive') }}
                      </Badge>
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] mt-1 truncate">{{ series.title }}</div>
                    <div class="text-xs text-[var(--text-tertiary)] mt-1 font-mono">
                      {{ $t('entertainment.admin.badgeCatalog.series.enabledCount', { active: series.activeBadgeCount || 0, total: series.badgeCount || 0 }) }}
                    </div>
                  </button>
                  <div class="flex items-center gap-1">
                    <Button size="sm" variant="ghost" @click="openEditSeriesModal(series)">
                      <template #icon>
                        <Pencil class="w-3.5 h-3.5" />
                      </template>
                      {{ $t('common.edit') }}
                    </Button>
                    <template v-if="deletingSeriesId === series.id">
                      <Button size="sm" variant="danger" @click="deleteSeries(series.id)">
                        {{ $t('common.confirm') }}
                      </Button>
                      <Button size="sm" variant="secondary" @click="deletingSeriesId = null">
                        {{ $t('common.cancel') }}
                      </Button>
                    </template>
                    <Button
                      v-else
                      size="sm"
                      variant="ghost"
                      class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                      @click="deletingSeriesId = series.id"
                    >
                      <template #icon>
                        <Trash2 class="w-3.5 h-3.5" />
                      </template>
                      {{ $t('common.delete') }}
                    </Button>
                  </div>
                </div>
              </div>
              <div v-if="badgeSeries.length === 0" class="p-8 text-center text-xs text-[var(--text-secondary)]">
                {{ $t('entertainment.admin.badgeCatalog.series.empty') }}
              </div>
            </div>
          </Card>

          <!-- 徽章列表 Card -->
          <Card data-reveal padding="none" class="overflow-hidden">
            <div class="p-4 border-b border-[var(--border-color)] flex flex-col sm:flex-row sm:items-center justify-between gap-3">
              <div>
                <div class="text-sm font-semibold text-[var(--text-primary)]">{{ $t('entertainment.admin.badgeCatalog.badges.title') }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ $t('entertainment.admin.badgeCatalog.badges.currentFilter', { name: selectedBadgeSeriesId === 'all' ? $t('entertainment.admin.badgeCatalog.series.all') : getSeriesTitle(selectedBadgeSeriesId) }) }}
                </div>
              </div>
              <Button
                variant="primary"
                size="sm"
                :disabled="badgeSeries.length === 0"
                @click="openCreateBadgeModal(selectedBadgeSeriesId)"
              >
                <template #icon>
                  <Plus class="w-3.5 h-3.5" />
                </template>
                {{ $t('entertainment.admin.badgeCatalog.addBadge') }}
              </Button>
            </div>

            <EmptyState
              v-if="filteredBadges.length === 0"
              :title="$t('entertainment.admin.badgeCatalog.badges.empty')"
            />
            <template v-else>
              <!-- 移动端卡片 (极度关键守卫) -->
              <div class="space-y-3 p-4 lg:hidden">
                <div
                  v-for="badge in filteredBadges"
                  :key="badge.id"
                  class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
                >
                  <div class="flex items-start gap-3">
                    <img :src="badge.assetUrlLight || badge.assetUrl" :alt="badge.fullLabel" class="h-11 w-11 shrink-0 rounded-xl object-contain bg-[var(--bg-surface-soft)] p-1 border border-[var(--border-color)]" loading="lazy" />
                    <div class="min-w-0 flex-1">
                      <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ badge.name }}</div>
                      <div class="mt-0.5 truncate text-xs text-[var(--text-secondary)]">{{ badge.fullLabel }}</div>
                      <div class="mt-0.5 break-all font-mono text-[11px] text-[var(--text-tertiary)]">{{ badge.id }}</div>
                    </div>
                    <Badge :variant="badge.isActive && badge.seriesIsActive ? 'success' : 'default'" size="sm" class="shrink-0">
                      {{ badge.isActive && badge.seriesIsActive ? $t('entertainment.admin.badgeCatalog.badges.drawable') : $t('entertainment.admin.badgeCatalog.badges.notDrawable') }}
                    </Badge>
                  </div>
                  <div class="mt-3 space-y-2 text-xs pt-3 border-t border-[var(--border-color)]">
                    <div class="flex justify-between gap-3">
                      <span class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.badgeCatalog.badges.tableSeries') }}</span>
                      <span class="min-w-0 truncate font-medium text-[var(--text-primary)]">{{ badge.seriesNameZh || badge.seriesTitle }}</span>
                    </div>
                    <div>
                      <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.badgeCatalog.badges.tableAssetUrl') }}</div>
                      <div class="mt-1 break-all font-mono text-[11px] text-[var(--text-secondary)]">{{ badge.assetUrl }}</div>
                    </div>
                    <div>
                      <div class="text-[var(--text-secondary)]">{{ $t('entertainment.admin.badgeCatalog.badges.tableUsage') }}</div>
                      <div class="mt-1 font-mono text-[var(--text-primary)]">
                        {{ $t('entertainment.admin.badgeCatalog.badges.usage', { ownership: badge.ownershipCount || 0, avatar: badge.avatarUseCount || 0, instance: badge.instanceUseCount || 0 }) }}
                      </div>
                    </div>
                  </div>
                  <div class="mt-4 flex flex-wrap gap-2 pt-3 border-t border-[var(--border-color)]">
                    <Button size="sm" variant="secondary" @click="openEditBadgeModal(badge)">
                      <template #icon>
                        <Pencil class="w-3.5 h-3.5" />
                      </template>
                      {{ $t('common.edit') }}
                    </Button>
                    <template v-if="deletingBadgeId === badge.id">
                      <Button size="sm" variant="danger" @click="deleteBadge(badge.id)">
                        {{ $t('common.confirm') }}
                      </Button>
                      <Button size="sm" variant="secondary" @click="deletingBadgeId = null">
                        {{ $t('common.cancel') }}
                      </Button>
                    </template>
                    <Button
                      v-else
                      size="sm"
                      variant="ghost"
                      class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                      @click="deletingBadgeId = badge.id"
                    >
                      <template #icon>
                        <Trash2 class="w-3.5 h-3.5" />
                      </template>
                      {{ $t('common.delete') }}
                    </Button>
                  </div>
                </div>
              </div>

              <!-- PC 端定宽表格 (极度关键守卫) -->
              <div class="hidden overflow-hidden lg:block">
                <table class="w-full table-fixed">
                  <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                    <tr>
                      <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[26%]">{{ $t('entertainment.admin.badgeCatalog.badges.tableBadge') }}</th>
                      <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[15%]">{{ $t('entertainment.admin.badgeCatalog.badges.tableSeries') }}</th>
                      <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[20%]">{{ $t('entertainment.admin.badgeCatalog.badges.tableAssetUrl') }}</th>
                      <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('entertainment.admin.badgeCatalog.badges.tableStatus') }}</th>
                      <th class="text-left text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[15%]">{{ $t('entertainment.admin.badgeCatalog.badges.tableUsage') }}</th>
                      <th class="text-right text-xs font-semibold text-[var(--text-secondary)] uppercase tracking-wider px-4 py-3 select-none w-[12%]">{{ $t('common.actions') }}</th>
                    </tr>
                  </thead>
                  <tbody class="divide-y divide-[var(--border-color)]">
                    <tr v-for="badge in filteredBadges" :key="badge.id" class="hover:bg-[var(--bg-surface-soft)]/60 transition-colors duration-150">
                      <td class="px-4 py-3">
                        <div class="flex items-center gap-3">
                          <img :src="badge.assetUrlLight || badge.assetUrl" :alt="badge.fullLabel" class="h-10 w-10 shrink-0 rounded-xl object-contain bg-[var(--bg-surface-soft)] p-1 border border-[var(--border-color)]" loading="lazy" />
                          <div class="min-w-0">
                            <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ badge.name }}</div>
                            <div class="truncate text-xs text-[var(--text-secondary)]">{{ badge.fullLabel }}</div>
                            <div class="truncate font-mono text-[11px] text-[var(--text-tertiary)]">{{ badge.id }}</div>
                          </div>
                        </div>
                      </td>
                      <td class="truncate px-4 py-3 text-sm text-[var(--text-primary)] font-medium">{{ badge.seriesNameZh || badge.seriesTitle }}</td>
                      <td class="truncate px-4 py-3 font-mono text-xs text-[var(--text-secondary)]">{{ badge.assetUrl }}</td>
                      <td class="px-4 py-3">
                        <Badge :variant="badge.isActive && badge.seriesIsActive ? 'success' : 'default'" size="sm">
                          {{ badge.isActive && badge.seriesIsActive ? $t('entertainment.admin.badgeCatalog.badges.drawable') : $t('entertainment.admin.badgeCatalog.badges.notDrawable') }}
                        </Badge>
                      </td>
                      <td class="px-4 py-3 font-mono text-xs text-[var(--text-secondary)]">
                        {{ $t('entertainment.admin.badgeCatalog.badges.usage', { ownership: badge.ownershipCount || 0, avatar: badge.avatarUseCount || 0, instance: badge.instanceUseCount || 0 }) }}
                      </td>
                      <td class="px-4 py-3 text-right">
                        <div class="flex items-center justify-end gap-1.5">
                          <Button
                            size="sm"
                            variant="ghost"
                            :title="$t('common.edit')"
                            @click="openEditBadgeModal(badge)"
                          >
                            <template #icon>
                              <Pencil class="w-3.5 h-3.5" />
                            </template>
                          </Button>
                          <div v-if="deletingBadgeId !== badge.id">
                            <Button
                              size="sm"
                              variant="ghost"
                              class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                              :title="$t('common.delete')"
                              @click="deletingBadgeId = badge.id"
                            >
                              <template #icon>
                                <Trash2 class="w-3.5 h-3.5" />
                              </template>
                            </Button>
                          </div>
                          <template v-else>
                            <div class="inline-flex items-center gap-1">
                              <Button
                                size="sm"
                                variant="danger"
                                @click="deleteBadge(badge.id)"
                              >
                                {{ $t('common.confirm') }}
                              </Button>
                              <Button
                                size="sm"
                                variant="secondary"
                                @click="deletingBadgeId = null"
                              >
                                {{ $t('common.cancel') }}
                              </Button>
                            </div>
                          </template>
                        </div>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </template>
          </Card>
        </div>
      </template>
    </div>

    <!-- 1. 创建/编辑抽奖弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="showLotteryModal"
          class="modal-overlay"
          @click.self="showLotteryModal = false"
        >
          <div class="modal-backdrop" @click="showLotteryModal = false"></div>
          <div class="modal-content max-w-xl">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ editingLottery ? $t('entertainment.admin.editLottery') : $t('entertainment.admin.createLottery') }}
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors select-none cursor-pointer"
                @click="showLotteryModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <div class="modal-body space-y-4">
              <div>
                <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.lotteryName') }}</label>
                <Input
                  v-model="lotteryForm.name"
                  :placeholder="$t('entertainment.admin.enterLotteryName')"
                />
              </div>

              <div>
                <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.lotteryDesc') }}</label>
                <textarea
                  v-model="lotteryForm.description"
                  class="w-full h-20 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-2 text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all resize-none"
                  :placeholder="$t('entertainment.admin.enterDescription')"
                ></textarea>
              </div>

              <div>
                <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.costPoints') }}</label>
                <Input
                  v-model.number="lotteryForm.costPoints"
                  type="number"
                  :min="1"
                />
              </div>

              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.startAt') }}</label>
                  <input
                    v-model="lotteryForm.startAt"
                    type="date"
                    class="w-full h-10 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.endAt') }}</label>
                  <input
                    v-model="lotteryForm.endAt"
                    type="date"
                    class="w-full h-10 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                  />
                </div>
              </div>

              <div class="pt-2">
                <label class="flex items-center gap-2.5 cursor-pointer select-none">
                  <input
                    id="isActive"
                    v-model="lotteryForm.isActive"
                    type="checkbox"
                    class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
                  />
                  <span class="text-sm font-medium text-[var(--text-primary)]">{{ $t('entertainment.admin.isActive') }}</span>
                </label>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showLotteryModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button variant="primary" :loading="savingLottery" @click="saveLottery">
                {{ savingLottery ? $t('common.saving') : $t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 2. 奖品管理弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="showPrizesModal"
          class="modal-overlay"
          @click.self="showPrizesModal = false"
        >
          <div class="modal-backdrop" @click="showPrizesModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header">
              <h3 class="modal-title truncate">
                {{ $t('entertainment.admin.managePrizes') }} - {{ editingPrizes?.name }}
              </h3>
              <div class="flex items-center gap-2">
                <Button variant="primary" size="sm" @click="addPrize">
                  <template #icon>
                    <Plus class="w-3.5 h-3.5" />
                  </template>
                  {{ $t('entertainment.admin.addPrize') }}
                </Button>
                <button
                  type="button"
                  class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors select-none cursor-pointer"
                  @click="showPrizesModal = false"
                >
                  <X class="w-4 h-4" />
                </button>
              </div>
            </div>

            <div class="modal-body space-y-4">
              <div
                v-for="(prize, index) in prizes"
                :key="index"
                class="p-4 border border-[var(--border-color)] rounded-2xl bg-[var(--bg-surface-soft)] space-y-3"
              >
                <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.prizeName') }}</label>
                    <Input v-model="prize.name" size="sm" />
                  </div>
                  <div>
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.prizeType') }}</label>
                    <select
                      v-model="prize.type"
                      class="w-full h-8 px-3 rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] text-xs text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                    >
                      <option value="nothing">{{ $t('entertainment.prizeTypes.nothing') }}</option>
                      <option value="points">{{ $t('entertainment.prizeTypes.points') }}</option>
                      <option value="balance">{{ $t('entertainment.prizeTypes.balance') }}</option>
                      <option value="badge">{{ $t('entertainment.prizeTypes.badge') }}</option>
                      <option value="instance">{{ $t('entertainment.prizeTypes.instance') }}</option>
                      <option value="cpu">{{ $t('entertainment.prizeTypes.cpu') }}</option>
                      <option value="memory">{{ $t('entertainment.prizeTypes.memory') }}</option>
                      <option value="disk">{{ $t('entertainment.prizeTypes.disk') }}</option>
                      <option value="traffic">{{ $t('entertainment.prizeTypes.traffic') }}</option>
                    </select>
                  </div>
                </div>

                <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div v-if="prize.type !== 'nothing' && prize.type !== 'badge'">
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">
                      {{ prize.type === 'balance' ? $t('entertainment.admin.balanceValue') : $t('entertainment.admin.prizeValue') }}
                      <span v-if="prize.type === 'cpu'" class="text-[var(--text-tertiary)]">(%)</span>
                      <span v-else-if="prize.type === 'memory' || prize.type === 'disk'" class="text-[var(--text-tertiary)]">(MB)</span>
                      <span v-else-if="prize.type === 'traffic'" class="text-[var(--text-tertiary)]">(GB)</span>
                    </label>
                    <Input
                      v-model.number="prize.value"
                      type="number"
                      size="sm"
                      :placeholder="prize.type === 'balance' ? $t('entertainment.admin.balanceCents') : 
                        prize.type === 'cpu' ? $t('entertainment.admin.cpuPercent') :
                        prize.type === 'memory' ? $t('entertainment.admin.memoryMB') :
                        prize.type === 'disk' ? $t('entertainment.admin.diskMB') :
                        prize.type === 'traffic' ? $t('entertainment.admin.trafficGB') : ''"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.probability') }}</label>
                    <Input
                      v-model.number="prize.probability"
                      type="number"
                      size="sm"
                      :min="0.01"
                      :step="0.01"
                    />
                  </div>
                  <div>
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.quantity') }}</label>
                    <Input
                      v-model.number="prize.totalQuantity"
                      type="number"
                      size="sm"
                      :placeholder="$t('entertainment.admin.unlimited')"
                      :disabled="prize.type === 'points' || prize.type === 'nothing'"
                      :title="(prize.type === 'points' || prize.type === 'nothing') ? $t('entertainment.admin.noQuantityForType') : ''"
                    />
                  </div>
                  <!-- 补充库存（仅对有数量限制的非 points/nothing 奖品显示） -->
                  <div v-if="prize.id && prize.type !== 'points' && prize.type !== 'nothing' && prize.totalQuantity">
                    <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">
                      {{ $t('entertainment.admin.replenish') }}
                      <span class="text-[var(--text-tertiary)]">({{ $t('entertainment.admin.remaining') }}: {{ prize.remainQuantity ?? 0 }})</span>
                    </label>
                    <Input
                      v-model.number="prize.replenishAmount"
                      type="number"
                      size="sm"
                      :min="0"
                      :placeholder="$t('entertainment.admin.replenishPlaceholder')"
                    />
                  </div>
                </div>

                <div v-if="prize.type === 'instance'">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.instanceDesc') }}</label>
                  <Input
                    v-model="prize.instanceDesc"
                    size="sm"
                    :placeholder="$t('entertainment.admin.instanceDescPlaceholder')"
                  />
                </div>

                <div class="flex justify-end pt-2 border-t border-[var(--border-color)]">
                  <Button
                    variant="ghost"
                    size="sm"
                    class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                    @click="removePrize(index)"
                  >
                    <template #icon>
                      <Trash2 class="w-3.5 h-3.5" />
                    </template>
                    {{ $t('common.delete') }}
                  </Button>
                </div>
              </div>

              <div v-if="prizes.length === 0" class="py-8">
                <EmptyState :title="$t('entertainment.admin.noPrizes')" />
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showPrizesModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button variant="primary" :loading="savingPrizes" @click="savePrizes">
                {{ savingPrizes ? $t('common.saving') : $t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 3. 通知配置弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="showNotificationModal"
          class="modal-overlay"
          @click.self="showNotificationModal = false"
        >
          <div class="modal-backdrop" @click="showNotificationModal = false"></div>
          <div class="modal-content max-w-xl">
            <div class="modal-header">
              <h3 class="modal-title truncate">
                {{ $t('entertainment.admin.notification.title') }} - {{ editingNotification?.name }}
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors select-none cursor-pointer"
                @click="showNotificationModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <div class="modal-body space-y-4">
              <!-- 启用开关 -->
              <div class="flex items-center justify-between p-3 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                <div>
                  <div class="text-sm font-semibold text-[var(--text-primary)]">{{ $t('entertainment.admin.notification.enabled') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ $t('entertainment.admin.notification.title') }}</div>
                </div>
                <label class="relative inline-flex items-center cursor-pointer select-none">
                  <input v-model="notificationForm.enabled" type="checkbox" class="sr-only peer">
                  <div class="w-11 h-6 bg-[var(--bg-tertiary)] peer-focus:outline-none rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-[var(--accent)]"></div>
                </label>
              </div>

              <!-- 通知方式选择 -->
              <div>
                <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.notification.type') }}</label>
                <select
                  v-model="notificationForm.type"
                  class="w-full h-10 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                >
                  <option value="telegram">Telegram</option>
                  <option value="discord">Discord</option>
                  <option value="webhook">Webhook</option>
                </select>
              </div>

              <!-- Telegram 配置 -->
              <template v-if="notificationForm.type === 'telegram'">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">Bot Token</label>
                  <Input
                    v-model="notificationForm.telegramBotToken"
                    placeholder="123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">Chat ID</label>
                  <Input
                    v-model="notificationForm.telegramChatId"
                    placeholder="-1001234567890"
                  />
                </div>
              </template>

              <!-- Discord 配置 -->
              <template v-if="notificationForm.type === 'discord'">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">Webhook URL</label>
                  <Input
                    v-model="notificationForm.discordWebhookUrl"
                    placeholder="https://discord.com/api/webhooks/..."
                  />
                </div>
              </template>

              <!-- Webhook 配置 -->
              <template v-if="notificationForm.type === 'webhook'">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">Webhook URL</label>
                  <Input
                    v-model="notificationForm.webhookUrl"
                    placeholder="https://your-server.com/webhook"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.notification.secret') }}</label>
                  <Input
                    v-model="notificationForm.webhookSecret"
                    :placeholder="$t('entertainment.admin.notification.secretPlaceholder')"
                  />
                </div>
              </template>

              <!-- 通知条件 -->
              <div class="border-t border-[var(--border-color)] pt-4">
                <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-2">{{ $t('entertainment.admin.notification.conditions') }}</label>
                <div class="space-y-2">
                  <label class="flex items-center gap-2.5 cursor-pointer select-none">
                    <input
                      id="notifyBalance"
                      v-model="notificationForm.notifyBalance"
                      type="checkbox"
                      class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
                    />
                    <span class="text-sm text-[var(--text-primary)]">{{ $t('entertainment.admin.notification.notifyBalance') }}</span>
                  </label>
                  <label class="flex items-center gap-2.5 cursor-pointer select-none">
                    <input
                      id="notifyInstance"
                      v-model="notificationForm.notifyInstance"
                      type="checkbox"
                      class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
                    />
                    <span class="text-sm text-[var(--text-primary)]">{{ $t('entertainment.admin.notification.notifyInstance') }}</span>
                  </label>
                </div>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showNotificationModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button variant="primary" :loading="savingNotification" @click="saveNotification">
                {{ savingNotification ? $t('common.saving') : $t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 4. 系列编辑弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="showSeriesModal"
          class="modal-overlay"
          @click.self="showSeriesModal = false"
        >
          <div class="modal-backdrop" @click="showSeriesModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ editingSeries ? $t('entertainment.admin.badgeCatalog.series.editTitle') : $t('entertainment.admin.badgeCatalog.series.createTitle') }}
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors select-none cursor-pointer"
                @click="showSeriesModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <div class="modal-body space-y-4">
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.id') }}</label>
                  <Input
                    v-model="seriesForm.id"
                    :disabled="!!editingSeries"
                    placeholder="supreme"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.sort') }}</label>
                  <Input
                    v-model.number="seriesForm.displayOrder"
                    type="number"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.nameZh') }}</label>
                  <Input v-model="seriesForm.nameZh" />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.nameEn') }}</label>
                  <Input v-model="seriesForm.nameEn" />
                </div>
                <div class="sm:col-span-2">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.titleLabel') }}</label>
                  <Input
                    v-model="seriesForm.title"
                    :placeholder="$t('entertainment.admin.badgeCatalog.series.titlePlaceholder')"
                  />
                </div>
                <div class="sm:col-span-2">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.descriptionLabel') }}</label>
                  <textarea
                    v-model="seriesForm.description"
                    class="w-full h-20 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] px-3 py-2 text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all resize-none"
                  ></textarea>
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.sourceId') }}</label>
                  <Input v-model="seriesForm.sourceId" />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.series.sourceLabel') }}</label>
                  <Input v-model="seriesForm.sourceLabel" />
                </div>
                <div class="sm:col-span-2 pt-2">
                  <label class="flex items-center gap-2.5 cursor-pointer select-none">
                    <input
                      v-model="seriesForm.isActive"
                      type="checkbox"
                      class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
                    />
                    <span class="text-sm font-medium text-[var(--text-primary)]">{{ $t('entertainment.admin.badgeCatalog.series.enable') }}</span>
                  </label>
                </div>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showSeriesModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button variant="primary" :loading="savingSeries" @click="saveSeries">
                {{ savingSeries ? $t('common.saving') : $t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>

    <!-- 5. 徽章编辑弹窗 -->
    <Teleport to="body">
      <Transition name="modal">
        <div
          v-if="showBadgeModal"
          class="modal-overlay"
          @click.self="showBadgeModal = false"
        >
          <div class="modal-backdrop" @click="showBadgeModal = false"></div>
          <div class="modal-content max-w-2xl">
            <div class="modal-header">
              <h3 class="modal-title">
                {{ editingBadge ? $t('entertainment.admin.badgeCatalog.badges.editTitle') : $t('entertainment.admin.badgeCatalog.badges.createTitle') }}
              </h3>
              <button
                type="button"
                class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors select-none cursor-pointer"
                @click="showBadgeModal = false"
              >
                <X class="w-4 h-4" />
              </button>
            </div>

            <div class="modal-body space-y-4">
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.id') }}</label>
                  <Input
                    v-model="badgeForm.id"
                    :disabled="!!editingBadge"
                    placeholder="elite"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.series') }}</label>
                  <select
                    v-model="badgeForm.seriesId"
                    class="w-full h-10 px-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-sm text-[var(--text-primary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 transition-all cursor-pointer"
                  >
                    <option v-for="series in badgeSeries" :key="series.id" :value="series.id">
                      {{ series.nameZh }} · {{ series.id }}
                    </option>
                  </select>
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.name') }}</label>
                  <Input v-model="badgeForm.name" />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.nameEn') }}</label>
                  <Input v-model="badgeForm.nameEn" />
                </div>
                <div class="sm:col-span-2">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.fullLabel') }}</label>
                  <Input
                    v-model="badgeForm.fullLabel"
                    :placeholder="$t('entertainment.admin.badgeCatalog.badges.fullLabelPlaceholder')"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.sourceId') }}</label>
                  <Input v-model="badgeForm.sourceId" />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.sourceLabel') }}</label>
                  <Input v-model="badgeForm.sourceLabel" />
                </div>
                <div class="sm:col-span-2">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.assetUrl') }}</label>
                  <Input
                    v-model="badgeForm.assetUrl"
                    :placeholder="$t('entertainment.admin.badgeCatalog.badges.assetUrlPlaceholder')"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.assetUrlDark') }}</label>
                  <Input
                    v-model="badgeForm.assetUrlDark"
                    placeholder="/badges/dark/elite.svg"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.assetUrlLight') }}</label>
                  <Input
                    v-model="badgeForm.assetUrlLight"
                    placeholder="/badges/light/elite.svg"
                  />
                </div>
                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">{{ $t('entertainment.admin.badgeCatalog.badges.sort') }}</label>
                  <Input
                    v-model.number="badgeForm.displayOrder"
                    type="number"
                  />
                </div>
                <div class="flex items-end pb-3">
                  <label class="flex items-center gap-2.5 cursor-pointer select-none">
                    <input
                      v-model="badgeForm.isActive"
                      type="checkbox"
                      class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer"
                    />
                    <span class="text-sm font-medium text-[var(--text-primary)]">{{ $t('entertainment.admin.badgeCatalog.badges.enable') }}</span>
                  </label>
                </div>
              </div>

              <!-- 实时预览 Card -->
              <div class="mt-4 rounded-2xl border border-[var(--border-color)] p-4 bg-[var(--bg-surface-soft)]">
                <div class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">
                  {{ $t('entertainment.admin.badgeCatalog.badges.preview') }}
                </div>
                <div class="flex items-center gap-4">
                  <img
                    v-if="badgeForm.assetUrl"
                    :src="badgeForm.assetUrlLight || badgeForm.assetUrl"
                    :alt="badgeForm.fullLabel || badgeForm.id"
                    class="w-16 h-16 object-contain rounded-2xl bg-[var(--bg-surface)] p-2 border border-[var(--border-color)] shadow-sm"
                    loading="lazy"
                  />
                  <div>
                    <div class="text-base font-semibold text-[var(--text-primary)]">
                      {{ badgeForm.name || $t('entertainment.admin.badgeCatalog.badges.previewName') }}
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] mt-0.5">
                      {{ badgeForm.fullLabel || $t('entertainment.admin.badgeCatalog.badges.previewLabel') }}
                    </div>
                    <div class="font-mono text-[11px] text-[var(--text-tertiary)] mt-1">
                      {{ badgeForm.id || 'badge-id' }}
                    </div>
                  </div>
                </div>
              </div>
            </div>

            <div class="modal-footer">
              <Button variant="secondary" @click="showBadgeModal = false">
                {{ $t('common.cancel') }}
              </Button>
              <Button variant="primary" :loading="savingBadge" @click="saveBadge">
                {{ savingBadge ? $t('common.saving') : $t('common.save') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
