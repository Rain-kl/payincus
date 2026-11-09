<script setup lang="ts">
import { ref, onMounted, computed, onUnmounted, nextTick, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import api from '@/api'
import BadgeBatchRewardModal from '@/components/entertainment/BadgeBatchRewardModal.vue'
import { useToast } from '@/stores/toast'
import { translateError } from '@/utils/errorHandler'
import InstanceSelector from '@/components/InstanceSelector.vue'
import BadgeCenter from '@/components/entertainment/BadgeCenter.vue'
import BadgeRewardModal from '@/components/entertainment/BadgeRewardModal.vue'
import VipBenefitHall from '@/components/entertainment/VipBenefitHall.vue'
import type { BadgeOwnership } from '@/types/api'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Select
} from '@/components/ui'
import {
  Activity,
  AlertCircle,
  ArrowUpDown,
  Award,
  CalendarCheck,
  CheckCircle2,
  Coins,
  Cpu,
  Crown,
  Gift,
  HardDrive,
  HelpCircle,
  Layers,
  Sparkles,
  X,
  Zap
} from 'lucide-vue-next'
import {
  confettiPalette,
  prizeColorPalette,
  defaultPrizeColor,
  wheelGradientPalette,
  defaultWheelGradient
} from '@/theme/entertainment-palette'

const { t } = useI18n()
const toast = useToast()
const route = useRoute()

type LotteryBadgeRewardModalState = {
  visible: boolean
  ownership: BadgeOwnership | null
  remainingPoints: number | null
}

const checkinFeatureEnabled = ref(true)

// 主选项卡（福利功能分类）
const mainTab = ref<'vip' | 'lottery' | 'badge' | 'checkin'>('vip')
const badgeInitialTab = ref<'draw' | 'my'>('draw')

// 抽奖子 TAB
const activeTab = ref<'lottery' | 'records' | 'points'>('lottery')

// 签到子 TAB
const checkinTab = ref<'checkin' | 'redeem' | 'pool' | 'logs'>('checkin')

// 积分数据
const points = ref({
  points: 0,
  totalEarned: 0,
  totalSpent: 0,
  convertiblePoints: 0
})
const pointsLoading = ref(true)

// 抽奖活动列表
const lotteries = ref<any[]>([])
const lotteriesLoading = ref(false)
const selectedLottery = ref<any>(null)

// 抽奖状态
const isSpinning = ref(false)
const spinResult = ref<any>(null)
const showResultModal = ref(false)
const lotteryBadgeRewardModal = ref<LotteryBadgeRewardModalState>({
  visible: false,
  ownership: null,
  remainingPoints: null
})
const wheelRotation = ref(0)
const bulbPhase = ref(0) // 灯泡闪烁相位：0 或 1
let bulbTimer: ReturnType<typeof setInterval> | null = null

// 十连抽状态
const isMultiDrawing = ref(false)
const multiDrawResults = ref<any[]>([])
const multiDrawBadgeRewards = ref<BadgeOwnership[]>([])
const showMultiDrawBadgeModal = ref(false)
const showMultiDrawModal = ref(false)
const multiDrawError = ref('')

// 抽奖记录
const records = ref<any[]>([])
const recordsLoading = ref(false)
const recordsPage = ref(1)
const recordsPageSize = ref(20)
const recordsTotal = ref(0)
const recordsTypeFilter = ref('')  // 奖品类型筛选
const recordsPageSizeOptions = [10, 20, 50, 100]

// 积分日志
const pointsLogs = ref<any[]>([])
const pointsLogsLoading = ref(false)
const pointsLogsPage = ref(1)
const pointsLogsPageSize = ref(10)
const pointsLogsTotal = ref(0)

// 兑换状态
const converting = ref(false)

// ==================== 签到系统状态 ====================
// 签到动画状态
const checkinPhase = ref<'idle' | 'shaking' | 'opening' | 'revealing' | 'done'>('idle')
const checkinResult = ref<{ type: string; value: number; bonusPoints: number } | null>(null)
const confettiParticles = ref<Array<{ id: number; x: number; delay: number; color: string; size: number }>>([])

// 签到状态
const checkinLoading = ref(false)
const checkinStatus = ref<{
  enabled: boolean
  hasCheckedIn: boolean
  hasInstances: boolean
  requireInstance: boolean
  minPoints: number
  maxPoints: number
  currentPoints: number
  totalEarned: number
  totalSpent: number
  today: { id: number; dateKey: string; points: number; streakDays: number; createdAt: string } | null
  streakDays: number
  totalCheckins: number
  monthCheckins: number
  recentRecords: Array<{ id: number; dateKey: string; points: number; streakDays: number; createdAt: string }>
  selfOnlyMode: boolean
  consecutiveOthersUse: number
} | null>(null)

// 兑换码输入
const redeemCodeInput = ref('')
const redeemLoading = ref(false)
const redeemSelectedInstance = ref<number | null>(null)

// 资源池数据
const resourcePool = ref({ cpu: 0, memory: 0, disk: 0, traffic: 0 })
const resourcePoolLoading = ref(false)

// 资源池应用表单
const applyForm = ref({ instanceId: null as number | null, resourceType: 'c' as 'c' | 'r' | 'd' | 't', amount: 1 })
const applyLoading = ref(false)

// 可用实例列表（资源池应用）
const poolInstances = ref<Array<{
  id: number
  name: string
  status: string
  cpu: number
  memory: number
  disk: number
  monthlyTrafficLimit: string | null
  isPaid: boolean
  instanceType: 'vm' | 'container'
  host: { id: number; name: string; location: string | null; countryCode: string }
}>>([])

// 资源池日志
const poolLogs = ref<any[]>([])
const poolLogsLoading = ref(false)
const poolLogsPage = ref(1)
const poolLogsPageSize = ref(20)
const poolLogsTotal = ref(0)
const poolLogsFilter = ref({ action: '', resourceType: '' })

onMounted(() => {
  loadPoints()
  loadLotteries()
  
  // 处理 URL 参数跳转
  if (route.query.tab === 'lottery') {
    mainTab.value = 'lottery'
  } else if (route.query.tab === 'checkin' && checkinFeatureEnabled.value) {
    mainTab.value = 'checkin'
    loadCheckinData()
  } else if (route.query.tab === 'badge') {
    mainTab.value = 'badge'
  } else {
    mainTab.value = 'vip'
  }
})

onUnmounted(() => {
  // 清理灯泡闪烁定时器
  if (bulbTimer) {
    clearInterval(bulbTimer)
    bulbTimer = null
  }
})

// 监听主选项卡切换
watch(mainTab, (newTab) => {
  if (newTab === 'checkin' && checkinFeatureEnabled.value) {
    // 切换到签到时始终刷新资源池数据（抽奖可能获得资源）
    loadResourcePool()
    if (!checkinStatus.value) {
      loadCheckinData()
    }
  }
})

// ==================== 签到系统方法 ====================

// 资源类型辅助函数
const getResourceTypeName = (type: string) => {
  const map: Record<string, string> = { c: t('checkin.cpu'), r: t('checkin.memory'), d: t('checkin.disk'), t: t('checkin.traffic'), p: t('checkin.points') }
  return map[type] || type.toUpperCase()
}

const getResourceUnit = (type: string) => {
  const map: Record<string, string> = { c: '%', r: 'MB', d: 'MB', t: 'GB', p: '' }
  return map[type] || ''
}

const getResourceColor = (type: string) => {
  const map: Record<string, string> = { c: 'text-blue-500', r: 'text-purple-500', d: 'text-amber-500', t: 'text-emerald-500', p: 'text-amber-500' }
  return map[type] || 'text-gray-500'
}

const getResourceBgColor = (type: string) => {
  const map: Record<string, string> = { c: 'bg-blue-500/10', r: 'bg-purple-500/10', d: 'bg-amber-500/10', t: 'bg-emerald-500/10', p: 'bg-amber-500/10' }
  return map[type] || 'bg-gray-500/10'
}

const getActionName = (action: string) => {
  const map: Record<string, string> = {
    checkin: t('checkin.actionCheckin'),
    redeem: t('checkin.actionRedeem'),
    admin_grant: t('checkin.actionAdminGrant'),
    system_grant: t('checkin.actionSystemGrant'),
    lottery: t('checkin.actionLottery'),
    apply: t('checkin.actionApply'),
    system_redeem: t('checkin.actionSystemRedeem')
  }
  return map[action] || action
}

// 生成彩色纸屑粒子
const generateConfetti = () => {
  const colors = confettiPalette
  const particles: typeof confettiParticles.value = []
  for (let i = 0; i < 50; i++) {
    particles.push({ id: i, x: Math.random() * 100, delay: Math.random() * 0.5, color: colors[Math.floor(Math.random() * colors.length)], size: Math.random() * 8 + 4 })
  }
  confettiParticles.value = particles
}

// 加载签到相关数据
async function loadCheckinData() {
  await Promise.all([loadCheckinStatus(), loadResourcePool()])
}

// 加载签到状态
async function loadCheckinStatus() {
  try {
    checkinLoading.value = true
    checkinStatus.value = await api.checkin.getStatus()
  } catch (err: any) {
    console.error('Failed to load checkin status:', err)
  } finally {
    checkinLoading.value = false
  }
}

// 加载资源池
async function loadResourcePool() {
  try {
    resourcePoolLoading.value = true
    resourcePool.value = await api.resourcePool.get()
  } catch (err: any) {
    console.error('Failed to load resource pool:', err)
    toast.error(translateError(err))
  } finally {
    resourcePoolLoading.value = false
  }
}

// 加载资源池实例列表
async function loadPoolInstances() {
  try {
    const res = await api.resourcePool.getInstances()
    poolInstances.value = res.instances
  } catch (err: any) {
    console.error('Failed to load pool instances:', err)
  }
}

// 加载资源池日志
async function loadPoolLogs() {
  try {
    poolLogsLoading.value = true
    const res = await api.resourcePool.getLogs({
      action: poolLogsFilter.value.action || undefined,
      resourceType: poolLogsFilter.value.resourceType || undefined,
      limit: poolLogsPageSize.value,
      offset: (poolLogsPage.value - 1) * poolLogsPageSize.value
    })
    poolLogs.value = res.records
    poolLogsTotal.value = res.total
  } catch (err: any) {
    console.error('Failed to load pool logs:', err)
    toast.error(translateError(err))
  } finally {
    poolLogsLoading.value = false
  }
}

// 执行签到
async function doCheckin() {
  if (
    checkinStatus.value?.hasCheckedIn ||
    checkinStatus.value?.enabled === false ||
    (checkinStatus.value?.requireInstance && !checkinStatus.value?.hasInstances)
  ) return
  try {
    checkinLoading.value = true
    checkinPhase.value = 'shaking'
    await new Promise(resolve => setTimeout(resolve, 1200))
    checkinPhase.value = 'opening'
    await new Promise(resolve => setTimeout(resolve, 600))
    const result = await api.checkin.checkin()
    checkinPhase.value = 'revealing'
    const rewardPoints = result.points ?? result.bonusPoints ?? result.codeValue
    checkinResult.value = { type: 'p', value: rewardPoints, bonusPoints: rewardPoints }
    generateConfetti()
    await new Promise(resolve => setTimeout(resolve, 100))
    await nextTick()
    checkinPhase.value = 'done'
    // 刷新数据
    await Promise.all([loadCheckinStatus(), loadResourcePool(), loadPoints()])
    toast.success(t('checkin.checkinSuccess'))
    // 3秒后自动回到 idle 状态
    setTimeout(() => {
      resetCheckinAnimation()
    }, 3000)
  } catch (err: any) {
    toast.error(translateError(err))
    checkinPhase.value = 'idle'
  } finally {
    checkinLoading.value = false
  }
}

// 重置签到动画
function resetCheckinAnimation() {
  checkinPhase.value = 'idle'
  checkinResult.value = null
  confettiParticles.value = []
}

// 兑换系统兑换码
async function redeemCode() {
  const code = redeemCodeInput.value.trim()
  if (!code) { toast.warning(t('checkin.enterCode')); return }
  if (!redeemSelectedInstance.value) { toast.warning(t('checkin.selectInstance')); return }
  try {
    redeemLoading.value = true
    const result = await api.checkin.redeem(code, redeemSelectedInstance.value)
    toast.success(t('checkin.redeemToInstanceSuccess', { type: getResourceTypeName(result.codeType), value: result.actualAdded, unit: getResourceUnit(result.codeType), instance: result.instanceName }))
    redeemCodeInput.value = ''
    await loadResourcePool()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    redeemLoading.value = false
  }
}

// 应用资源到实例
async function applyResource() {
  if (!applyForm.value.instanceId) { toast.warning(t('checkin.selectInstance')); return }
  if (applyForm.value.amount <= 0) { toast.warning(t('checkin.enterAmount')); return }
  const maxAmount = resourcePool.value[applyForm.value.resourceType === 'c' ? 'cpu' : applyForm.value.resourceType === 'r' ? 'memory' : applyForm.value.resourceType === 'd' ? 'disk' : 'traffic']
  if (applyForm.value.amount > maxAmount) { toast.warning(t('checkin.insufficientPool')); return }
  try {
    applyLoading.value = true
    const result = await api.resourcePool.apply({
      instanceId: applyForm.value.instanceId,
      resourceType: applyForm.value.resourceType,
      amount: applyForm.value.amount
    })
    toast.success(t('checkin.applySuccess', { type: getResourceTypeName(applyForm.value.resourceType), value: applyForm.value.amount, unit: getResourceUnit(applyForm.value.resourceType), instance: result.instanceName }))
    applyForm.value.amount = 1
    await loadResourcePool()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    applyLoading.value = false
  }
}

// 签到子TAB切换
function switchCheckinTab(tab: 'checkin' | 'redeem' | 'pool' | 'logs') {
  checkinTab.value = tab
  if (tab === 'pool' && poolInstances.value.length === 0) loadPoolInstances()
  if (tab === 'redeem' && poolInstances.value.length === 0) loadPoolInstances()
  if (tab === 'logs' && poolLogs.value.length === 0) loadPoolLogs()
}

// 计算资源池日志总页数
const poolLogsTotalPages = computed(() => Math.ceil(poolLogsTotal.value / poolLogsPageSize.value))

// 启动灯泡闪烁
function startBulbBlink() {
  bulbPhase.value = 0
  bulbTimer = setInterval(() => {
    bulbPhase.value = bulbPhase.value === 0 ? 1 : 0
  }, 150) // 每 150ms 切换一次
}

// 停止灯泡闪烁
function stopBulbBlink() {
  if (bulbTimer) {
    clearInterval(bulbTimer)
    bulbTimer = null
  }
  bulbPhase.value = 0
}

async function loadPoints() {
  pointsLoading.value = true
  try {
    const res = await api.entertainment.getPoints()
    points.value = {
      points: res.points,
      totalEarned: res.totalEarned,
      totalSpent: res.totalSpent,
      convertiblePoints: res.convertiblePoints
    }
  } catch (err: any) {
    console.error('Failed to load points:', err)
  } finally {
    pointsLoading.value = false
  }
}

async function loadLotteries() {
  lotteriesLoading.value = true
  try {
    const res = await api.entertainment.getLotteries()
    lotteries.value = res.lotteries || []
    // 如果当前有选中的活动，更新其引用以获取最新数据（如库存）
    if (selectedLottery.value) {
      const updated = lotteries.value.find(l => l.id === selectedLottery.value.id)
      if (updated) {
        selectedLottery.value = updated
      }
    }
    // 如果没有选中的活动，默认选第一个
    if (lotteries.value.length > 0 && !selectedLottery.value) {
      selectedLottery.value = lotteries.value[0]
    }
  } catch (err: any) {
    toast.error(t('entertainment.loadLotteriesFailed') + ': ' + err.message)
  } finally {
    lotteriesLoading.value = false
  }
}

async function loadRecords() {
  recordsLoading.value = true
  try {
    const res = await api.entertainment.getLotteryRecords({
      page: recordsPage.value,
      pageSize: recordsPageSize.value,
      prizeType: recordsTypeFilter.value || undefined
    })
    records.value = res.records || []
    recordsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.loadRecordsFailed') + ': ' + err.message)
  } finally {
    recordsLoading.value = false
  }
}

async function loadPointsLogs() {
  pointsLogsLoading.value = true
  try {
    const res = await api.entertainment.getPointsLogs({
      page: pointsLogsPage.value,
      pageSize: pointsLogsPageSize.value
    })
    pointsLogs.value = res.logs || []
    pointsLogsTotal.value = res.total
  } catch (err: any) {
    toast.error(t('entertainment.loadPointsLogsFailed') + ': ' + err.message)
  } finally {
    pointsLogsLoading.value = false
  }
}

// 兑换积分
async function convertPoints() {
  if (points.value.convertiblePoints <= 0) {
    toast.warning(t('entertainment.noPointsToConvert'))
    return
  }
  converting.value = true
  try {
    const res = await api.entertainment.convertPoints()
    toast.success(t('entertainment.convertSuccess', { points: res.converted }))
    loadPoints()
    if (activeTab.value === 'points') {
      loadPointsLogs()
    }
  } catch (err: any) {
    toast.error(t('entertainment.convertFailed') + ': ' + err.message)
  } finally {
    converting.value = false
  }
}

function openBadgeTab(tab: 'draw' | 'my' = 'my') {
  badgeInitialTab.value = tab
  mainTab.value = 'badge'
}

function openLotteryBadgeRewardModal(ownership: BadgeOwnership, remainingPoints: number | null) {
  if (remainingPoints !== null) {
    points.value.points = remainingPoints
  }
  lotteryBadgeRewardModal.value = {
    visible: true,
    ownership,
    remainingPoints
  }
}

function closeLotteryBadgeRewardModal() {
  lotteryBadgeRewardModal.value = {
    visible: false,
    ownership: null,
    remainingPoints: null
  }
  spinResult.value = null
}

function jumpToMyBadgesFromLotteryReward() {
  closeLotteryBadgeRewardModal()
  openBadgeTab('my')
}

const canSpinAgainFromLotteryBadgeReward = computed(() => {
  if (!selectedLottery.value) return false
  const remainingPoints = lotteryBadgeRewardModal.value.remainingPoints ?? points.value.points
  return !isSpinning.value && remainingPoints >= selectedLottery.value.costPoints
})

async function spinAgainFromLotteryBadgeReward() {
  if (!canSpinAgainFromLotteryBadgeReward.value) return
  closeLotteryBadgeRewardModal()
  await spin()
}

// 抽奖
async function spin() {
  if (!selectedLottery.value) {
    toast.warning(t('entertainment.selectLottery'))
    return
  }
  if (points.value.points < selectedLottery.value.costPoints) {
    toast.warning(t('entertainment.notEnoughPoints'))
    return
  }
  if (isSpinning.value) return

  isSpinning.value = true
  spinResult.value = null
  startBulbBlink() // 开始灯泡闪烁
  
  // 开始旋转动画
  const prizes = selectedLottery.value.prizes || []
  const prizeCount = prizes.length || 8
  const baseRotation = wheelRotation.value
  
  try {
    const res = await api.entertainment.draw(selectedLottery.value.id)
    spinResult.value = {
      prizeType: res.record.prizeType,
      prizeName: res.record.prizeName,
      prizeValue: res.record.prizeValue,
      prizeId: res.record.prizeId,
      badgeOwnership: res.record.badgeOwnership
    }
    
    // 根据后端返回的 prizeId 找到中奖奖品在转盘中的索引
    // 使用 == 而不是 === 来比较，避免类型不一致问题（number vs string）
    const prizeId = res.record.prizeId
    const prizeIndex = prizes.findIndex((p: any) => p.id == prizeId)
    const actualIndex = prizeIndex >= 0 ? prizeIndex : 0
    
    console.log('[Lottery] Prize result:', {
      prizeId,
      prizeIdType: typeof prizeId,
      prizeName: res.record.prizeName,
      prizeIndex,
      actualIndex,
      prizeCount,
      prizes: prizes.map((p: any, i: number) => ({ 
        index: i, 
        id: p.id, 
        idType: typeof p.id,
        name: p.name,
        displayOrder: p.displayOrder
      }))
    })
    
    // 如果 findIndex 返回 -1，说明数据不一致，打印警告
    if (prizeIndex === -1) {
      console.error('[Lottery] WARNING: prizeId not found in prizes array!', {
        searchingFor: prizeId,
        availableIds: prizes.map((p: any) => p.id)
      })
    }
    
    const segmentAngle = 360 / prizeCount
    // 计算目标角度：让该扇区中心对准顶部指针
    // 扇区 N 的中心在 (N + 0.5) * segmentAngle 度位置
    // 要让它对准顶部指针（0度），需要顺时针旋转到 (prizeCount - N - 0.5) * segmentAngle 的绝对角度
    const randomOffset = (Math.random() - 0.5) * segmentAngle * 0.4 // 在扇区中心附近随机偏移，避免停在边界
    const stopAngle = (prizeCount - actualIndex - 0.5) * segmentAngle + randomOffset
    
    // 关键修复：stopAngle 是绝对角度（0-360°），不能简单累加
    // 需要计算一个至少多转 3 圈且最终停在 stopAngle 位置的目标角度
    const minTarget = baseRotation + 1080  // 至少比当前多转 3 圈
    const targetAngle = Math.ceil(minTarget / 360) * 360 + stopAngle
    
    console.log('[Lottery] Angle calculation:', {
      segmentAngle,
      stopAngle,
      targetAngle,
      randomOffset
    })
    
    wheelRotation.value = targetAngle
    
    // 等待动画完成后显示结果
    setTimeout(() => {
      if (res.record.badgeOwnership) {
        openLotteryBadgeRewardModal(res.record.badgeOwnership, res.currentPoints)
      } else {
        showResultModal.value = true
      }
      loadPoints() // 刷新积分
      loadLotteries() // 刷新奖品库存
    }, 2000)
  } catch (err: any) {
    toast.error(t('entertainment.spinFailed') + ': ' + err.message)
    stopBulbBlink()
  } finally {
    setTimeout(() => {
      isSpinning.value = false
      stopBulbBlink() // 停止灯泡闪烁
    }, 2000)
  }
}

function switchTab(tab: 'lottery' | 'records' | 'points') {
  activeTab.value = tab
  if (tab === 'records' && records.value.length === 0) {
    loadRecords()
  } else if (tab === 'points' && pointsLogs.value.length === 0) {
    loadPointsLogs()
  }
}

function selectLottery(lottery: any) {
  selectedLottery.value = lottery
  wheelRotation.value = 0
}

function closeResultModal() {
  showResultModal.value = false
  spinResult.value = null
}

// 十连抽
async function multiDraw() {
  if (!selectedLottery.value) {
    toast.warning(t('entertainment.selectLottery'))
    return
  }
  const totalCost = selectedLottery.value.costPoints * 10
  if (points.value.points < totalCost) {
    toast.warning(t('entertainment.notEnoughPointsForMulti', { required: totalCost, current: points.value.points }))
    return
  }
  if (isMultiDrawing.value || isSpinning.value) return

  isMultiDrawing.value = true
  multiDrawResults.value = []
  multiDrawError.value = ''

  try {
    const res = await api.entertainment.drawMulti(selectedLottery.value.id)
    multiDrawResults.value = res.records || []
    multiDrawBadgeRewards.value = multiDrawResults.value
      .map(result => result.badgeOwnership)
      .filter((ownership): ownership is BadgeOwnership => !!ownership)
    if (res.stopReason) {
      multiDrawError.value = res.stopReason
    }
    
    // 如果没有任何结果，直接显示错误 toast
    if (multiDrawResults.value.length === 0) {
      toast.error(res.stopReason || t('entertainment.multiDrawFailed'))
    } else if (multiDrawBadgeRewards.value.length > 0) {
      showMultiDrawBadgeModal.value = true
    } else {
      showMultiDrawModal.value = true
    }
    
    loadPoints() // 刷新积分
    loadLotteries() // 刷新奖品库存
  } catch (err: any) {
    toast.error(t('entertainment.multiDrawFailed') + ': ' + err.message)
  } finally {
    isMultiDrawing.value = false
  }
}

function closeMultiDrawModal() {
  showMultiDrawModal.value = false
  multiDrawResults.value = []
  multiDrawBadgeRewards.value = []
  showMultiDrawBadgeModal.value = false
  multiDrawError.value = ''
}

function continueToMultiDrawResults() {
  showMultiDrawBadgeModal.value = false
  showMultiDrawModal.value = true
}

// 计算十连抽各类奖品的总值（使用 computed 避免重复计算）
const multiDrawSummary = computed(() => {
  const results = multiDrawResults.value
  return {
    balance: results.filter(r => r.prizeType === 'balance').reduce((sum, r) => sum + r.prizeValue, 0) / 100, // 分转元
    badge: results.filter(r => r.prizeType === 'badge').length,
    instance: results.filter(r => r.prizeType === 'instance').length,
    cpu: results.filter(r => r.prizeType === 'cpu').reduce((sum, r) => sum + r.prizeValue, 0),
    memory: results.filter(r => r.prizeType === 'memory').reduce((sum, r) => sum + r.prizeValue, 0),
    disk: results.filter(r => r.prizeType === 'disk').reduce((sum, r) => sum + r.prizeValue, 0),
    traffic: results.filter(r => r.prizeType === 'traffic').reduce((sum, r) => sum + r.prizeValue, 0),
    points: results.filter(r => r.prizeType === 'points').reduce((sum, r) => sum + r.prizeValue, 0),
    nothing: results.filter(r => r.prizeType === 'nothing').length
  }
})

// 再次十连抽：关闭弹窗后立即再次抽奖
function multiDrawAgain() {
  closeMultiDrawModal()
  multiDraw()
}

function formatDate(dateStr: string): string {
  return new Date(dateStr).toLocaleString()
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

function getPointsLogTypeName(type: string): string {
  const map: Record<string, string> = {
    convert: t('entertainment.pointsLogTypes.convert'),
    lottery_win: t('entertainment.pointsLogTypes.lotteryWin'),
    lottery_spend: t('entertainment.pointsLogTypes.lotterySpend'),
    badge_draw_spend: t('entertainment.pointsLogTypes.badgeDrawSpend'),
    badge_select_spend: t('entertainment.pointsLogTypes.badgeSelectSpend'),
    admin_adjust: t('entertainment.pointsLogTypes.adminAdjust'),
    checkin: t('entertainment.pointsLogTypes.checkin'),
    invite_generate: '生成邀请码'
  }
  return map[type] || type
}

function getPrizeColor(type: string): string {
  return prizeColorPalette[type] || defaultPrizeColor
}

// 为转盘奖品区域生成渐变色
function getWheelGradientColor(type: string, index: number): string {
  const colors = wheelGradientPalette[type] || defaultWheelGradient
  // 交替使用深浅色增加对比
  return index % 2 === 0 ? colors[0] : colors[1]
}

const recordsTotalPages = computed(() => Math.ceil(recordsTotal.value / recordsPageSize.value))
const pointsLogsTotalPages = computed(() => Math.ceil(pointsLogsTotal.value / pointsLogsPageSize.value))

// 转盘奖品
const wheelPrizes = computed(() => {
  if (!selectedLottery.value?.prizes) return []
  return selectedLottery.value.prizes
})

// 转盘辅助函数
function polarToCartesian(centerX: number, centerY: number, radius: number, angleInDegrees: number) {
  const angleInRadians = (angleInDegrees - 90) * Math.PI / 180
  return {
    x: centerX + radius * Math.cos(angleInRadians),
    y: centerY + radius * Math.sin(angleInRadians)
  }
}

function getWheelSegmentPath(index: number, total: number): string {
  const centerX = 100
  const centerY = 100
  const radius = 100
  const anglePerSegment = 360 / total
  const startAngle = index * anglePerSegment
  const endAngle = (index + 1) * anglePerSegment
  
  const start = polarToCartesian(centerX, centerY, radius, endAngle)
  const end = polarToCartesian(centerX, centerY, radius, startAngle)
  const largeArcFlag = anglePerSegment > 180 ? 1 : 0
  
  return [
    'M', centerX, centerY,
    'L', start.x, start.y,
    'A', radius, radius, 0, largeArcFlag, 0, end.x, end.y,
    'Z'
  ].join(' ')
}

function getWheelTextTransform(index: number, total: number): string {
  const anglePerSegment = 360 / total
  const angle = index * anglePerSegment + anglePerSegment / 2 - 90
  const radius = 65
  const centerX = 100
  const centerY = 100
  const x = centerX + radius * Math.cos(angle * Math.PI / 180)
  const y = centerY + radius * Math.sin(angle * Math.PI / 180)
  const textRotation = angle + 90
  return `translate(${x}, ${y}) rotate(${textRotation})`
}

const resourceTypeOptions = computed(() => [
  { label: `CPU (${resourcePool.value.cpu}%)`, value: 'c' },
  { label: `${t('checkin.memory')} (${resourcePool.value.memory} MB)`, value: 'r' },
  { label: `${t('checkin.disk')} (${resourcePool.value.disk} MB)`, value: 'd' },
  { label: `${t('checkin.traffic')} (${resourcePool.value.traffic} GB)`, value: 't' }
])

</script>

<template>
  <div class="kawaii-page space-y-6 animate-fade-in pb-12">
    <!-- Apple HIG PageHeader -->
    <PageHeader
      :title="$t('entertainment.title')"
      :description="$t('entertainment.description')"
    />

    <!-- 主选项卡（Apple HIG Segmented Control） -->
    <div class="inline-flex items-center rounded-2xl bg-[var(--bg-surface-soft)] p-1.5 border border-[var(--border-color)] gap-1 flex-wrap">
      <button
        type="button"
        class="inline-flex items-center justify-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98]"
        :class="mainTab === 'vip'
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="mainTab = 'vip'"
      >
        <Crown class="h-4 w-4 text-amber-500" />
        <span>{{ $t('entertainment.mainTabs.vipBenefits') }}</span>
      </button>

      <button
        type="button"
        class="inline-flex items-center justify-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98]"
        :class="mainTab === 'lottery'
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="mainTab = 'lottery'"
      >
        <Sparkles class="h-4 w-4 text-[var(--accent)]" />
        <span>{{ $t('entertainment.mainTabs.lottery') }}</span>
      </button>

      <button
        v-if="checkinFeatureEnabled"
        type="button"
        class="inline-flex items-center justify-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98]"
        :class="mainTab === 'checkin'
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="mainTab = 'checkin'"
      >
        <CalendarCheck class="h-4 w-4 text-emerald-500" />
        <span>{{ $t('entertainment.mainTabs.checkin') }}</span>
      </button>

      <button
        type="button"
        class="inline-flex items-center justify-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98]"
        :class="mainTab === 'badge'
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
        @click="badgeInitialTab = 'draw'; mainTab = 'badge'"
      >
        <Award class="h-4 w-4 text-purple-500" />
        <span>{{ $t('entertainment.mainTabs.badge') }}</span>
      </button>
    </div>

    <!-- VIP权益大厅 -->
    <div v-show="mainTab === 'vip'">
      <VipBenefitHall />
    </div>

    <!-- ==================== 抽奖功能区域 ==================== -->
    <div v-show="mainTab === 'lottery'" class="space-y-6">
      <!-- 积分卡片 -->
      <Card padding="md">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
          <div class="flex items-center gap-4">
            <div class="w-12 h-12 rounded-2xl bg-amber-500/10 border border-amber-500/20 flex items-center justify-center shrink-0">
              <Coins class="w-6 h-6 text-amber-500" />
            </div>
            <div>
              <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('entertainment.currentPoints') }}</div>
              <div class="text-2xl font-bold text-[var(--text-primary)] font-mono tabular-nums tracking-tight">
                {{ points.points.toLocaleString() }}
              </div>
            </div>
          </div>
          <div class="flex items-center gap-2 flex-wrap">
            <!-- 十连抽按钮 -->
            <Button 
              v-if="selectedLottery"
              variant="primary"
              size="sm"
              :disabled="isMultiDrawing || isSpinning || points.points < selectedLottery.costPoints * 10"
              :loading="isMultiDrawing"
              @click="multiDraw"
            >
              <template #icon>
                <Zap class="w-4 h-4" />
              </template>
              {{ $t('entertainment.multiDraw') }}
              <span class="text-xs opacity-80 font-mono">({{ selectedLottery.costPoints * 10 }}{{ $t('entertainment.pointsUnit') }})</span>
            </Button>
            <!-- 兑换积分按钮 -->
            <Button 
              variant="secondary"
              size="sm"
              :disabled="converting || points.convertiblePoints <= 0"
              :loading="converting"
              @click="convertPoints"
            >
              <template #icon>
                <ArrowUpDown class="w-4 h-4" />
              </template>
              {{ $t('entertainment.convertPoints') }}
              <span v-if="points.convertiblePoints > 0" class="ml-1 text-xs text-[var(--accent)] font-semibold font-mono">
                (+{{ points.convertiblePoints }})
              </span>
            </Button>
          </div>
        </div>
      </Card>

      <!-- 子 TAB 切换 -->
      <div class="inline-flex items-center rounded-xl bg-[var(--bg-surface-soft)] p-1 border border-[var(--border-color)] gap-1">
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="activeTab === 'lottery'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchTab('lottery')"
        >
          {{ $t('entertainment.tabs.lottery') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="activeTab === 'records'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchTab('records')"
        >
          {{ $t('entertainment.tabs.records') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="activeTab === 'points'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchTab('points')"
        >
          {{ $t('entertainment.tabs.points') }}
        </button>
      </div>

      <!-- 抽奖 TAB -->
      <div v-show="activeTab === 'lottery'" class="space-y-6">
        <!-- 活动选择 -->
        <div v-if="lotteries.length > 1" class="flex gap-2 flex-wrap">
          <button
            v-for="lottery in lotteries"
            :key="lottery.id"
            type="button"
            class="px-4 py-2 rounded-full border text-sm font-medium transition-all duration-200 active:scale-[0.98]"
            :class="selectedLottery?.id === lottery.id
              ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold shadow-sm'
              : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:border-[var(--border-hover)] hover:text-[var(--text-primary)]'"
            @click="selectLottery(lottery)"
          >
            {{ lottery.name }}
            <span class="text-xs opacity-80 ml-1 font-mono tabular-nums">({{ lottery.costPoints }}{{ $t('entertainment.pointsUnit') }})</span>
          </button>
        </div>

        <!-- 无活动提示 -->
        <Card v-if="!lotteriesLoading && lotteries.length === 0" padding="none">
          <EmptyState :title="$t('entertainment.noActiveLotteries')" />
        </Card>

        <!-- 抽奖转盘 -->
        <div v-else-if="selectedLottery" class="flex flex-col items-center">
          <Card padding="lg" class="w-full flex flex-col items-center">
            <!-- 转盘容器 -->
            <div class="relative w-72 h-72 sm:w-96 sm:h-96 mb-8">
              <!-- 最外层底座阴影 -->
              <div class="absolute inset-[-8px] sm:inset-[-12px] rounded-full bg-gray-300 dark:bg-gray-700/60 shadow-md"></div>
            
              <!-- 外圈金属装饰环 -->
              <div class="absolute inset-[-4px] sm:inset-[-6px] rounded-full bg-amber-400 shadow-sm">
                <!-- 灯泡装饰 - 奇偶交替闪烁 -->
                <div class="absolute inset-0">
                  <div 
                    v-for="i in 16" 
                    :key="i"
                    class="absolute w-2.5 h-2.5 sm:w-3 sm:h-3 rounded-full transition-all duration-100"
                    :class="[
                      isSpinning && (i % 2 === bulbPhase) 
                        ? 'bg-white shadow-md shadow-white/80 scale-110' 
                        : 'bg-amber-200/80 shadow-sm'
                    ]"
                    :style="{
                      left: `calc(50% + ${Math.cos((i - 1) * 22.5 * Math.PI / 180) * 48}% - 5px)`,
                      top: `calc(50% + ${Math.sin((i - 1) * 22.5 * Math.PI / 180) * 48}% - 5px)`
                    }"
                  ></div>
                </div>
              </div>
            
              <!-- 内圈边框 -->
              <div class="absolute inset-1 sm:inset-2 rounded-full bg-gray-200 dark:bg-gray-700 border border-[var(--border-color)]"></div>
            
              <!-- 转盘主体 -->
              <div 
                class="absolute inset-3 sm:inset-4 rounded-full overflow-hidden wheel-spin"
                :style="{ 
                  transform: `rotate(${wheelRotation}deg)`,
                  transitionDuration: isSpinning ? '2s' : '0s',
                  transitionTimingFunction: isSpinning 
                    ? 'cubic-bezier(0.17, 0.67, 0.12, 0.99)'
                    : 'ease-out'
                }"
              >
                <svg viewBox="0 0 200 200" class="w-full h-full drop-shadow-md">
                  <defs>
                    <radialGradient id="wheelGlow" cx="50%" cy="50%" r="50%">
                      <stop offset="0%" stop-color="rgba(255,255,255,0.3)" />
                      <stop offset="100%" stop-color="rgba(255,255,255,0)" />
                    </radialGradient>
                    <filter id="prizeShadow" x="-20%" y="-20%" width="140%" height="140%">
                      <feDropShadow dx="0" dy="1" stdDeviation="1" flood-opacity="0.3" />
                    </filter>
                  </defs>
                
                  <!-- 奖品扇区 -->
                  <g v-for="(prize, idx) in wheelPrizes" :key="prize.id">
                    <path
                      :d="getWheelSegmentPath(Number(idx), wheelPrizes.length)"
                      :fill="getWheelGradientColor(prize.type, Number(idx))"
                      stroke="rgba(255,255,255,0.5)"
                      stroke-width="1"
                    />
                    <path
                      :d="getWheelSegmentPath(Number(idx), wheelPrizes.length)"
                      fill="none"
                      stroke="rgba(255,255,255,0.8)"
                      stroke-width="2"
                    />
                    <text
                      :transform="getWheelTextTransform(Number(idx), wheelPrizes.length)"
                      fill="white"
                      font-size="9"
                      font-weight="600"
                      text-anchor="middle"
                      dominant-baseline="middle"
                      filter="url(#prizeShadow)"
                      style="text-shadow: 0 1px 2px rgba(0,0,0,0.5)"
                    >
                      {{ prize.name.length > 5 ? prize.name.slice(0, 5) + '..' : prize.name }}
                    </text>
                  </g>
                
                  <circle cx="100" cy="100" r="100" fill="url(#wheelGlow)" />
                </svg>
              </div>

              <!-- 指针 -->
              <div class="absolute top-[-2px] sm:top-[-4px] left-1/2 -translate-x-1/2 z-30">
                <div class="relative">
                  <div class="absolute top-1 left-1/2 -translate-x-1/2 w-0 h-0 border-l-[14px] border-r-[14px] border-t-[32px] sm:border-l-[16px] sm:border-r-[16px] sm:border-t-[40px] border-l-transparent border-r-transparent border-t-black/20 blur-sm"></div>
                  <div class="relative w-0 h-0 border-l-[14px] border-r-[14px] border-t-[32px] sm:border-l-[16px] sm:border-r-[16px] sm:border-t-[40px] border-l-transparent border-r-transparent border-t-red-500">
                    <div class="absolute -top-[32px] sm:-top-[40px] left-[-4px] w-0 h-0 border-l-[4px] border-r-[4px] border-t-[20px] sm:border-t-[26px] border-l-transparent border-r-transparent border-t-red-400"></div>
                  </div>
                  <div class="absolute top-[28px] sm:top-[36px] left-1/2 -translate-x-1/2 w-4 h-4 sm:w-5 sm:h-5 rounded-full bg-red-500 shadow-md"></div>
                </div>
              </div>

              <!-- 中心按钮 -->
              <button
                class="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-20 h-20 sm:w-24 sm:h-24 rounded-full z-20 group transition-all duration-200 active:scale-95"
                :class="{ 'animate-pulse-slow': !isSpinning && points.points >= selectedLottery.costPoints }"
                :disabled="isSpinning || points.points < selectedLottery.costPoints"
                @click="spin"
              >
                <!-- 按钮外圈 -->
                <div class="absolute inset-0 rounded-full bg-amber-500 shadow-md group-hover:bg-amber-400 transition-all"></div>
                <!-- 按钮内圈 -->
                <div class="absolute inset-1 rounded-full bg-red-500 group-hover:bg-red-400 transition-all"></div>
                <!-- 按钮高光 -->
                <div class="absolute inset-2 rounded-full bg-white/20"></div>
                <!-- 按钮文字 -->
                <span class="relative z-10 text-white text-base sm:text-lg font-bold drop-shadow-md">
                  <span v-if="isSpinning" class="inline-flex">
                    <span class="animate-bounce" style="animation-delay: 0ms">.</span>
                    <span class="animate-bounce" style="animation-delay: 100ms">.</span>
                    <span class="animate-bounce" style="animation-delay: 200ms">.</span>
                  </span>
                  <span v-else>{{ $t('entertainment.spin') }}</span>
                </span>
                <!-- 禁用遮罩 -->
                <div 
                  v-if="points.points < selectedLottery.costPoints"
                  class="absolute inset-0 rounded-full bg-gray-900/50 cursor-not-allowed"
                ></div>
              </button>
            </div>

            <!-- 消耗提示 -->
            <div class="inline-flex items-center gap-2 px-3 py-1.5 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs text-[var(--text-secondary)] font-medium">
              <Sparkles class="w-3.5 h-3.5 text-amber-500" />
              <span>{{ $t('entertainment.spinCost', { points: selectedLottery.costPoints }) }}</span>
            </div>
          </Card>

          <!-- 奖品列表 -->
          <div class="w-full mt-6">
            <h3 class="text-sm font-semibold tracking-tight text-[var(--text-primary)] mb-3 flex items-center gap-2">
              <Gift class="w-4 h-4 text-[var(--accent)]" />
              {{ $t('entertainment.prizeList') }}
            </h3>
            <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
              <div
                v-for="prize in wheelPrizes"
                :key="prize.id"
                class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-3 text-center shadow-sm hover:border-[var(--border-hover)] transition-all duration-200"
              >
                <div
                  class="w-3 h-3 rounded-full mx-auto mb-2"
                  :style="{ backgroundColor: getPrizeColor(prize.type) }"
                ></div>
                <div class="text-sm text-[var(--text-primary)] font-medium truncate">{{ prize.name }}</div>
                <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ getPrizeTypeName(prize.type) }}</div>
                <!-- 概率和数量 -->
                <div class="text-xs text-[var(--text-tertiary)] mt-2 space-y-0.5">
                  <div>{{ $t('entertainment.probability') }}: <span class="font-mono tabular-nums text-[var(--text-secondary)]">{{ prize.probability }}%</span></div>
                  <div v-if="prize.totalQuantity !== null">
                    {{ $t('entertainment.remaining') }}: <span class="font-mono tabular-nums text-[var(--text-secondary)]">{{ prize.remainQuantity ?? 0 }}/{{ prize.totalQuantity }}</span>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 抽奖记录 TAB -->
      <div v-show="activeTab === 'records'" class="rounded-2xl border border-themed bg-themed-surface shadow-sm overflow-hidden">
        <!-- 筛选栏 -->
        <div class="flex items-center gap-4 p-4 border-b border-themed">
          <div class="flex items-center gap-2">
            <span class="text-sm text-themed-muted">{{ $t('entertainment.prizeType') }}:</span>
            <select 
              v-model="recordsTypeFilter" 
              class="input py-1.5 px-3 min-w-[100px] text-sm rounded-xl"
              @change="recordsPage = 1; loadRecords()"
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
        <div v-if="recordsLoading" class="p-12 text-center text-themed-muted">
          {{ $t('common.loading') }}...
        </div>
        <EmptyState v-else-if="records.length === 0" :title="$t('entertainment.noRecords')" />
        <template v-else>
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="rec in records"
              :key="rec.id"
              class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-themed">{{ rec.lotteryName || '-' }}</p>
                  <p class="mt-1 truncate text-xs text-themed-muted">{{ rec.prizeName || '-' }}</p>
                </div>
                <span
                  class="inline-flex shrink-0 items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium"
                  :style="{
                    backgroundColor: getPrizeColor(rec.prizeType) + '20',
                    color: getPrizeColor(rec.prizeType)
                  }"
                >
                  {{ getPrizeTypeName(rec.prizeType) }}
                </span>
              </div>
              <dl class="mt-4 grid grid-cols-2 gap-3 text-sm">
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('entertainment.value') }}</dt>
                  <dd class="mt-1 font-medium text-themed">
                    <template v-if="rec.prizeType === 'points'">+{{ rec.prizeValue }} {{ $t('entertainment.pointsUnit') }}</template>
                    <template v-else-if="rec.prizeType === 'balance'">+¥{{ (rec.prizeValue / 100).toFixed(2) }}</template>
                    <template v-else-if="rec.prizeType === 'badge'">{{ rec.prizeName || $t('entertainment.prizeTypes.badge') }}</template>
                    <template v-else-if="rec.prizeType === 'instance'">{{ rec.instanceDesc || $t('entertainment.wonInstance') }}</template>
                    <template v-else-if="rec.prizeType === 'cpu'">+{{ rec.prizeValue }}%</template>
                    <template v-else-if="rec.prizeType === 'memory'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'disk'">+{{ rec.prizeValue }}MB</template>
                    <template v-else-if="rec.prizeType === 'traffic'">+{{ rec.prizeValue }}GB</template>
                    <template v-else>-</template>
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('entertainment.time') }}</dt>
                  <dd class="mt-1 text-themed font-mono">{{ formatDate(rec.createdAt) }}</dd>
                </div>
              </dl>
            </div>
          </div>

          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead>
                <tr class="border-b border-themed text-left text-sm text-themed-muted">
                  <th class="w-[24%] px-4 py-3 font-medium">{{ $t('entertainment.lotteryName') }}</th>
                  <th class="w-[24%] px-4 py-3 font-medium">{{ $t('entertainment.prize') }}</th>
                  <th class="w-[16%] px-4 py-3 font-medium">{{ $t('entertainment.prizeType') }}</th>
                  <th class="w-[18%] px-4 py-3 font-medium">{{ $t('entertainment.value') }}</th>
                  <th class="w-[18%] px-4 py-3 font-medium">{{ $t('entertainment.time') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-themed">
                <tr v-for="rec in records" :key="rec.id" class="hover:bg-themed-hover transition-colors">
                  <td class="px-4 py-3 text-sm text-themed">
                    <div class="truncate">{{ rec.lotteryName || '-' }}</div>
                  </td>
                  <td class="px-4 py-3 text-sm text-themed">
                    <div class="truncate">{{ rec.prizeName || '-' }}</div>
                  </td>
                  <td class="px-4 py-3 text-sm">
                    <span
                      class="inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium"
                      :style="{
                        backgroundColor: getPrizeColor(rec.prizeType) + '20',
                        color: getPrizeColor(rec.prizeType)
                      }"
                    >
                      {{ getPrizeTypeName(rec.prizeType) }}
                    </span>
                  </td>
                  <td class="px-4 py-3 text-sm text-themed">
                    <div class="truncate font-mono">
                      <template v-if="rec.prizeType === 'points'">+{{ rec.prizeValue }} {{ $t('entertainment.pointsUnit') }}</template>
                      <template v-else-if="rec.prizeType === 'balance'">+¥{{ (rec.prizeValue / 100).toFixed(2) }}</template>
                      <template v-else-if="rec.prizeType === 'badge'">{{ rec.prizeName || $t('entertainment.prizeTypes.badge') }}</template>
                      <template v-else-if="rec.prizeType === 'instance'">{{ rec.instanceDesc || $t('entertainment.wonInstance') }}</template>
                      <template v-else-if="rec.prizeType === 'cpu'">+{{ rec.prizeValue }}%</template>
                      <template v-else-if="rec.prizeType === 'memory'">+{{ rec.prizeValue }}MB</template>
                      <template v-else-if="rec.prizeType === 'disk'">+{{ rec.prizeValue }}MB</template>
                      <template v-else-if="rec.prizeType === 'traffic'">+{{ rec.prizeValue }}GB</template>
                      <template v-else>-</template>
                    </div>
                  </td>
                  <td class="px-4 py-3 text-sm text-themed-muted font-mono">{{ formatDate(rec.createdAt) }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div class="flex flex-col sm:flex-row items-center justify-between gap-4 p-4 border-t border-themed">
            <!-- 每页条数 -->
            <div class="flex items-center gap-2 text-sm text-themed-muted">
              <span>{{ $t('common.perPage') }}</span>
              <select
                :value="recordsPageSize"
                class="input w-20 py-1 text-sm rounded-xl"
                @change="recordsPageSize = Number(($event.target as HTMLSelectElement).value); recordsPage = 1; loadRecords()"
              >
                <option v-for="size in recordsPageSizeOptions" :key="size" :value="size">{{ size }}</option>
              </select>
              <span>{{ $t('common.totalCount', { count: recordsTotal }) }}</span>
            </div>
            <!-- 分页导航 -->
            <div v-if="recordsTotalPages > 1" class="flex items-center gap-2">
              <button
                class="inline-flex items-center justify-center rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-3 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none"
                :disabled="recordsPage <= 1"
                @click="recordsPage--; loadRecords()"
              >
                {{ $t('common.prevPage') }}
              </button>
              <span class="text-sm text-themed-muted font-mono">{{ recordsPage }} / {{ recordsTotalPages }}</span>
              <button
                class="inline-flex items-center justify-center rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-3 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none"
                :disabled="recordsPage >= recordsTotalPages"
                @click="recordsPage++; loadRecords()"
              >
                {{ $t('common.nextPage') }}
              </button>
            </div>
          </div>
        </template>
      </div>

      <!-- 积分明细 TAB -->
      <div v-show="activeTab === 'points'" class="rounded-2xl border border-themed bg-themed-surface shadow-sm overflow-hidden">
        <div v-if="pointsLogsLoading" class="p-12 text-center text-themed-muted">
          {{ $t('common.loading') }}...
        </div>
        <EmptyState v-else-if="pointsLogs.length === 0" :title="$t('entertainment.noPointsLogs')" />
        <template v-else>
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="log in pointsLogs"
              :key="log.id"
              class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-themed">{{ getPointsLogTypeName(log.type) }}</p>
                  <p class="mt-1 truncate text-xs text-themed-muted">{{ log.remark || '-' }}</p>
                </div>
                <span class="shrink-0 text-sm font-semibold font-mono tabular-nums" :class="log.amount >= 0 ? 'text-green-500' : 'text-red-500'">
                  {{ log.amount >= 0 ? '+' : '' }}{{ log.amount }}
                </span>
              </div>
              <dl class="mt-4 grid grid-cols-2 gap-3 text-sm">
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('entertainment.pointsAfter') }}</dt>
                  <dd class="mt-1 font-medium text-themed font-mono tabular-nums">{{ log.pointsAfter }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('entertainment.time') }}</dt>
                  <dd class="mt-1 text-themed font-mono">{{ formatDate(log.createdAt) }}</dd>
                </div>
              </dl>
            </div>
          </div>

          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead>
                <tr class="border-b border-themed text-left text-sm text-themed-muted">
                  <th class="w-[20%] px-4 py-3 font-medium">{{ $t('entertainment.pointsLogType') }}</th>
                  <th class="w-[16%] px-4 py-3 font-medium">{{ $t('entertainment.pointsChange') }}</th>
                  <th class="w-[16%] px-4 py-3 font-medium">{{ $t('entertainment.pointsAfter') }}</th>
                  <th class="w-[30%] px-4 py-3 font-medium">{{ $t('entertainment.remark') }}</th>
                  <th class="w-[18%] px-4 py-3 font-medium">{{ $t('entertainment.time') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-themed">
                <tr v-for="log in pointsLogs" :key="log.id" class="hover:bg-themed-hover transition-colors">
                  <td class="px-4 py-3 text-sm text-themed font-medium">{{ getPointsLogTypeName(log.type) }}</td>
                  <td class="px-4 py-3 text-sm font-semibold font-mono tabular-nums" :class="log.amount >= 0 ? 'text-green-500' : 'text-red-500'">
                    {{ log.amount >= 0 ? '+' : '' }}{{ log.amount }}
                  </td>
                  <td class="px-4 py-3 text-sm text-themed font-mono tabular-nums">{{ log.pointsAfter }}</td>
                  <td class="px-4 py-3 text-sm text-themed-muted">
                    <div class="truncate">{{ log.remark || '-' }}</div>
                  </td>
                  <td class="px-4 py-3 text-sm text-themed-muted font-mono">{{ formatDate(log.createdAt) }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div v-if="pointsLogsTotalPages > 1" class="grid grid-cols-[1fr_auto_1fr] items-center gap-2 p-4 border-t border-themed sm:flex sm:justify-center">
            <button
              class="btn btn-sm btn-ghost justify-self-start rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-4 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none"
              :disabled="pointsLogsPage <= 1"
              @click="pointsLogsPage--; loadPointsLogs()"
            >
              {{ $t('common.prevPage') }}
            </button>
            <span class="text-sm text-themed-muted justify-self-center font-mono">{{ pointsLogsPage }} / {{ pointsLogsTotalPages }}</span>
            <button
              class="btn btn-sm btn-ghost justify-self-end rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-4 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none"
              :disabled="pointsLogsPage >= pointsLogsTotalPages"
              @click="pointsLogsPage++; loadPointsLogs()"
            >
              {{ $t('common.nextPage') }}
            </button>
          </div>
        </template>
      </div>
    <!-- 抽奖功能区域结束 -->
    </div>

    <div v-if="mainTab === 'badge'">
      <BadgeCenter :initial-tab="badgeInitialTab" @points-updated="loadPoints" />
    </div>

    <!-- ==================== 签到功能区域 ==================== -->
    <div v-show="checkinFeatureEnabled && mainTab === 'checkin'" class="space-y-6">
      <!-- 资源池卡片（总览） -->
      <div class="grid grid-cols-2 md:grid-cols-4 gap-3 sm:gap-4">
        <Card padding="sm" class="hover:border-[var(--border-hover)] transition-all duration-200">
          <div class="flex items-center gap-3">
            <div class="w-10 h-10 rounded-2xl bg-blue-500/10 border border-blue-500/20 flex items-center justify-center shrink-0">
              <Cpu class="w-5 h-5 text-blue-500" />
            </div>
            <div class="min-w-0">
              <div class="text-xs text-[var(--text-secondary)] font-medium">CPU</div>
              <div class="text-base sm:text-lg font-bold text-blue-500 whitespace-nowrap font-mono tabular-nums tracking-tight">
                {{ resourcePool.cpu }}%
              </div>
            </div>
          </div>
        </Card>
        <Card padding="sm" class="hover:border-[var(--border-hover)] transition-all duration-200">
          <div class="flex items-center gap-3">
            <div class="w-10 h-10 rounded-2xl bg-purple-500/10 border border-purple-500/20 flex items-center justify-center shrink-0">
              <Layers class="w-5 h-5 text-purple-500" />
            </div>
            <div class="min-w-0">
              <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.memory') }}</div>
              <div class="text-base sm:text-lg font-bold text-purple-500 whitespace-nowrap font-mono tabular-nums tracking-tight">
                {{ resourcePool.memory }} MB
              </div>
            </div>
          </div>
        </Card>
        <Card padding="sm" class="hover:border-[var(--border-hover)] transition-all duration-200">
          <div class="flex items-center gap-3">
            <div class="w-10 h-10 rounded-2xl bg-amber-500/10 border border-amber-500/20 flex items-center justify-center shrink-0">
              <HardDrive class="w-5 h-5 text-amber-500" />
            </div>
            <div class="min-w-0">
              <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.disk') }}</div>
              <div class="text-base sm:text-lg font-bold text-amber-500 whitespace-nowrap font-mono tabular-nums tracking-tight">
                {{ resourcePool.disk }} MB
              </div>
            </div>
          </div>
        </Card>
        <Card padding="sm" class="hover:border-[var(--border-hover)] transition-all duration-200">
          <div class="flex items-center gap-3">
            <div class="w-10 h-10 rounded-2xl bg-emerald-500/10 border border-emerald-500/20 flex items-center justify-center shrink-0">
              <Activity class="w-5 h-5 text-emerald-500" />
            </div>
            <div class="min-w-0">
              <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.traffic') }}</div>
              <div class="text-base sm:text-lg font-bold text-emerald-500 whitespace-nowrap font-mono tabular-nums tracking-tight">
                {{ resourcePool.traffic }} GB
              </div>
            </div>
          </div>
        </Card>
      </div>

      <!-- 签到子TAB切换 -->
      <div class="inline-flex items-center rounded-xl bg-[var(--bg-surface-soft)] p-1 border border-[var(--border-color)] gap-1 flex-wrap">
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="checkinTab === 'checkin'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchCheckinTab('checkin')"
        >
          {{ $t('checkin.tabCheckin') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="checkinTab === 'redeem'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchCheckinTab('redeem')"
        >
          {{ $t('checkin.tabRedeem') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="checkinTab === 'pool'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchCheckinTab('pool')"
        >
          {{ $t('checkin.tabPool') }}
        </button>
        <button
          type="button"
          class="inline-flex items-center justify-center px-4 py-1.5 rounded-lg text-sm font-medium transition-all duration-200 active:scale-[0.98]"
          :class="checkinTab === 'logs'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
          @click="switchCheckinTab('logs')"
        >
          {{ $t('checkin.tabLogs') }}
        </button>
      </div>

      <!-- 签到子TAB: 签到 -->
      <Card v-show="checkinTab === 'checkin'" padding="md">
        <div v-if="checkinStatus" class="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-6">
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
            <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.currentPoints') }}</div>
            <div class="mt-1 text-xl font-bold text-[var(--text-primary)] font-mono tabular-nums">{{ checkinStatus.currentPoints }}</div>
          </div>
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
            <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.todayRange') }}</div>
            <div class="mt-1 text-xl font-bold text-amber-500 font-mono tabular-nums">{{ checkinStatus.minPoints }}-{{ checkinStatus.maxPoints }}</div>
          </div>
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
            <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.streakDays') }}</div>
            <div class="mt-1 text-xl font-bold text-emerald-500 font-mono tabular-nums">{{ checkinStatus.streakDays }}</div>
          </div>
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
            <div class="text-xs text-[var(--text-secondary)] font-medium">{{ $t('checkin.monthCheckins') }}</div>
            <div class="mt-1 text-xl font-bold text-[var(--accent)] font-mono tabular-nums">{{ checkinStatus.monthCheckins }}</div>
          </div>
        </div>

        <div class="flex flex-col items-center py-6">
          <!-- 签到礼盒动画 -->
          <div class="relative w-48 h-48 mb-6">
            <!-- 纸屑动画 -->
            <div v-if="checkinPhase === 'revealing' || checkinPhase === 'done'" class="absolute inset-0 overflow-hidden pointer-events-none">
              <div
                v-for="particle in confettiParticles"
                :key="particle.id"
                class="absolute animate-confetti"
                :style="{
                  left: `${particle.x}%`,
                  top: '-10%',
                  width: `${particle.size}px`,
                  height: `${particle.size}px`,
                  backgroundColor: particle.color,
                  animationDelay: `${particle.delay}s`,
                  borderRadius: particle.id % 2 === 0 ? '50%' : '2px'
                }"
              ></div>
            </div>
            
            <!-- 礼盒容器 -->
            <div class="w-full h-full flex items-center justify-center">
              <!-- 未签到状态：可点击的礼盒 -->
              <div 
                v-if="checkinPhase === 'idle' || checkinPhase === 'shaking' || checkinPhase === 'opening'"
                :class="[
                  checkinStatus?.hasCheckedIn || checkinStatus?.enabled === false || (checkinStatus?.requireInstance && !checkinStatus?.hasInstances)
                    ? 'cursor-not-allowed opacity-50'
                    : 'cursor-pointer active:scale-95 transition-transform duration-200'
                ]"
                @click="checkinStatus?.hasCheckedIn || checkinStatus?.enabled === false || (checkinStatus?.requireInstance && !checkinStatus?.hasInstances) ? null : doCheckin()"
              >
                <!-- 礼盒图标 -->
                <div 
                  :class="[
                    'w-32 h-32 mx-auto rounded-3xl bg-amber-500 flex items-center justify-center shadow-md transition-all duration-300',
                    {
                      'hover:scale-105 hover:bg-amber-400': checkinPhase === 'idle' && !checkinStatus?.hasCheckedIn && checkinStatus?.enabled !== false && !(checkinStatus?.requireInstance && !checkinStatus?.hasInstances),
                      'animate-shake-box': checkinPhase === 'shaking',
                      'animate-open-box': checkinPhase === 'opening'
                    }
                  ]"
                >
                  <Gift class="w-16 h-16 text-white stroke-[1.75]" />
                </div>
              </div>
              
              <!-- 签到结果显示 -->
              <div v-else-if="checkinPhase === 'revealing' || checkinPhase === 'done'" class="text-center animate-scale-in">
                <div :class="['w-24 h-24 mx-auto mb-4 rounded-2xl border flex items-center justify-center shadow-sm', getResourceBgColor(checkinResult?.type || 'c'), 'border-current/20']">
                  <span :class="['text-3xl font-bold font-mono tracking-tight', getResourceColor(checkinResult?.type || 'c')]">
                    +{{ checkinResult?.value }}{{ getResourceUnit(checkinResult?.type || 'c') }}
                  </span>
                </div>
                <p class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ getResourceTypeName(checkinResult?.type || 'c') }}</p>
                <p class="text-sm text-[var(--text-secondary)] mt-1 font-mono">{{ $t('checkin.pointsAdded', { points: checkinResult?.bonusPoints }) }}</p>
              </div>
            </div>
          </div>

          <!-- 签到提示文字 -->
          <p v-if="checkinPhase === 'idle' && !checkinStatus?.hasCheckedIn && checkinStatus?.enabled !== false && !(checkinStatus?.requireInstance && !checkinStatus?.hasInstances)" class="text-sm text-[var(--text-secondary)] mb-4">
            {{ $t('checkin.clickToCheckin') }}
          </p>

          <!-- 签到状态 -->
          <div v-if="checkinStatus" class="text-center">
            <div v-if="checkinStatus.hasCheckedIn && checkinPhase === 'idle'" class="inline-flex items-center mb-4">
              <Badge variant="success" size="md">
                <CheckCircle2 class="w-3.5 h-3.5 mr-1" />
                <span>{{ $t('checkin.alreadyCheckedIn') }}</span>
              </Badge>
            </div>
            <div v-else-if="checkinStatus.enabled === false && checkinPhase === 'idle'" class="inline-flex items-center mb-4">
              <Badge variant="warning" size="md">
                <AlertCircle class="w-3.5 h-3.5 mr-1" />
                <span>{{ $t('checkin.disabled') }}</span>
              </Badge>
            </div>
            <div v-else-if="checkinStatus.requireInstance && !checkinStatus.hasInstances && checkinPhase === 'idle'" class="inline-flex items-center mb-4">
              <Badge variant="warning" size="md">
                <AlertCircle class="w-3.5 h-3.5 mr-1" />
                <span>{{ $t('checkin.noInstances') }}</span>
              </Badge>
            </div>
          </div>
        </div>
      </Card>

      <!-- 签到子TAB: 兑换码 -->
      <Card v-show="checkinTab === 'redeem'" padding="md">
        <template #header>
          <div class="border-b border-[var(--border-color)] pb-4 mb-5">
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('checkin.redeemSystemCode') }}</h3>
          </div>
        </template>
        <div class="space-y-4 max-w-xl">
          <div>
            <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ $t('checkin.enterCode') }}</label>
            <Input
              v-model="redeemCodeInput"
              type="text"
              :placeholder="$t('checkin.systemCodePlaceholder')"
            />
          </div>
          <!-- 系统码必须选择实例 -->
          <div>
            <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ $t('checkin.targetInstance') }}</label>
            <InstanceSelector
              v-model="redeemSelectedInstance"
              :instances="poolInstances"
              :placeholder="$t('checkin.selectInstance')"
            />
          </div>
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 text-xs text-[var(--text-secondary)] leading-relaxed">
            <p>{{ $t('checkin.systemCodeHint') }}</p>
          </div>
          <Button
            variant="primary"
            block
            :disabled="redeemLoading || !redeemCodeInput.trim() || !redeemSelectedInstance"
            :loading="redeemLoading"
            @click="redeemCode"
          >
            {{ $t('checkin.redeem') }}
          </Button>
        </div>
      </Card>

      <!-- 签到子TAB: 资源池应用 -->
      <Card v-show="checkinTab === 'pool'" padding="md">
        <template #header>
          <div class="border-b border-[var(--border-color)] pb-4 mb-5">
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('checkin.applyToInstance') }}</h3>
          </div>
        </template>
        <div class="space-y-4 max-w-xl">
          <div class="grid gap-4 sm:grid-cols-2">
            <div>
              <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ $t('checkin.resourceType') }}</label>
              <Select
                v-model="applyForm.resourceType"
                :options="resourceTypeOptions"
              />
            </div>
            <div>
              <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ $t('checkin.amount') }}</label>
              <Input
                v-model.number="applyForm.amount"
                type="number"
                :min="1"
              />
            </div>
            <div class="sm:col-span-2">
              <label class="block text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ $t('checkin.targetInstance') }}</label>
              <InstanceSelector
                v-model="applyForm.instanceId"
                :instances="poolInstances"
                :placeholder="$t('checkin.selectInstance')"
              />
            </div>
          </div>
          <!-- KVM 实例限制提示 -->
          <div class="rounded-xl border border-amber-500/20 bg-amber-500/10 p-3 text-xs text-amber-500 space-y-1">
            <p>{{ $t('checkin.kvmHint') }}</p>
          </div>
          <Button
            variant="primary"
            block
            :disabled="applyLoading || !applyForm.instanceId || applyForm.amount <= 0"
            :loading="applyLoading"
            @click="applyResource"
          >
            {{ $t('checkin.apply') }}
          </Button>
        </div>
      </Card>

      <!-- 签到子TAB: 记录 -->
      <div v-show="checkinTab === 'logs'" class="rounded-2xl border border-themed bg-themed-surface shadow-sm overflow-hidden">
        <!-- 筛选 -->
        <div class="flex flex-wrap gap-3 p-4 border-b border-themed">
          <select v-model="poolLogsFilter.action" class="input py-1.5 px-3 text-sm rounded-xl" @change="poolLogsPage = 1; loadPoolLogs()">
            <option value="">{{ $t('checkin.allActions') }}</option>
            <option value="checkin">{{ $t('checkin.actionCheckin') }}</option>
            <option value="redeem">{{ $t('checkin.actionRedeem') }}</option>
            <option value="apply">{{ $t('checkin.actionApply') }}</option>
            <option value="admin_grant">{{ $t('checkin.actionAdminGrant') }}</option>
            <option value="lottery">{{ $t('checkin.actionLottery') }}</option>
            <option value="system_redeem">{{ $t('checkin.actionSystemRedeem') }}</option>
          </select>
          <select v-model="poolLogsFilter.resourceType" class="input py-1.5 px-3 text-sm rounded-xl" @change="poolLogsPage = 1; loadPoolLogs()">
            <option value="">{{ $t('checkin.allResources') }}</option>
            <option value="c">CPU</option>
            <option value="r">{{ $t('checkin.memory') }}</option>
            <option value="d">{{ $t('checkin.disk') }}</option>
            <option value="t">{{ $t('checkin.traffic') }}</option>
          </select>
        </div>
        
        <div v-if="poolLogsLoading" class="p-12 text-center text-themed-muted">{{ $t('common.loading') }}...</div>
        <EmptyState v-else-if="poolLogs.length === 0" :title="$t('checkin.noLogs')" />
        <template v-else>
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="log in poolLogs"
              :key="log.id"
              class="rounded-lg border border-themed bg-themed-surface p-4 shadow-sm"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <p class="truncate text-sm font-medium text-themed">{{ getActionName(log.action) }}</p>
                  <p class="mt-1 truncate text-xs text-themed-muted">{{ log.instance?.name || '-' }}</p>
                </div>
                <span :class="['shrink-0 rounded-full px-2.5 py-0.5 text-xs font-medium', getResourceBgColor(log.resourceType), getResourceColor(log.resourceType)]">
                  {{ getResourceTypeName(log.resourceType) }}
                </span>
              </div>
              <dl class="mt-4 grid grid-cols-2 gap-3 text-sm">
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('checkin.amount') }}</dt>
                  <dd class="mt-1 font-semibold font-mono tabular-nums" :class="log.amount >= 0 ? 'text-emerald-500' : 'text-red-500'">
                    {{ log.amount >= 0 ? '+' : '' }}{{ log.amount }}{{ getResourceUnit(log.resourceType) }}
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-themed-muted">{{ $t('checkin.time') }}</dt>
                  <dd class="mt-1 text-themed font-mono">{{ new Date(log.createdAt).toLocaleString() }}</dd>
                </div>
              </dl>
            </div>
          </div>

          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead>
                <tr class="border-b border-themed text-left text-sm text-themed-muted">
                  <th class="w-[20%] px-4 py-3 font-medium">{{ $t('checkin.action') }}</th>
                  <th class="w-[16%] px-4 py-3 font-medium">{{ $t('checkin.resourceType') }}</th>
                  <th class="w-[16%] px-4 py-3 font-medium">{{ $t('checkin.amount') }}</th>
                  <th class="w-[28%] px-4 py-3 font-medium">{{ $t('checkin.instance') }}</th>
                  <th class="w-[20%] px-4 py-3 font-medium">{{ $t('checkin.time') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-themed">
                <tr v-for="log in poolLogs" :key="log.id" class="hover:bg-themed-hover transition-colors">
                  <td class="px-4 py-3 text-sm text-themed font-medium">{{ getActionName(log.action) }}</td>
                  <td class="px-4 py-3 text-sm">
                    <span :class="['rounded-full px-2.5 py-0.5 text-xs font-medium', getResourceBgColor(log.resourceType), getResourceColor(log.resourceType)]">
                      {{ getResourceTypeName(log.resourceType) }}
                    </span>
                  </td>
                  <td class="px-4 py-3 text-sm font-semibold font-mono tabular-nums" :class="log.amount >= 0 ? 'text-emerald-500' : 'text-red-500'">
                    {{ log.amount >= 0 ? '+' : '' }}{{ log.amount }}{{ getResourceUnit(log.resourceType) }}
                  </td>
                  <td class="px-4 py-3 text-sm text-themed-muted">
                    <div class="truncate">{{ log.instance?.name || '-' }}</div>
                  </td>
                  <td class="px-4 py-3 text-sm text-themed-muted font-mono">{{ new Date(log.createdAt).toLocaleString() }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 分页 -->
          <div v-if="poolLogsTotalPages > 1" class="grid grid-cols-[1fr_auto_1fr] items-center gap-2 p-4 border-t border-themed sm:flex sm:justify-center">
            <button class="btn btn-sm btn-ghost justify-self-start rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-4 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none" :disabled="poolLogsPage <= 1" @click="poolLogsPage--; loadPoolLogs()">
              {{ $t('common.prevPage') }}
            </button>
            <span class="text-sm text-themed-muted justify-self-center font-mono">{{ poolLogsPage }} / {{ poolLogsTotalPages }}</span>
            <button class="btn btn-sm btn-ghost justify-self-end rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] px-4 py-1 text-xs font-medium text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all disabled:opacity-40 disabled:pointer-events-none" :disabled="poolLogsPage >= poolLogsTotalPages" @click="poolLogsPage++; loadPoolLogs()">
              {{ $t('common.nextPage') }}
            </button>
          </div>
        </template>
      </div>
    </div>

    <BadgeRewardModal
      :visible="lotteryBadgeRewardModal.visible"
      mode="draw"
      :ownership="lotteryBadgeRewardModal.ownership"
      :remaining-points="lotteryBadgeRewardModal.remainingPoints"
      :show-primary-action="true"
      :primary-action-label="isSpinning ? $t('common.processing') : $t('entertainment.badges.rewardDrawAgain')"
      :primary-action-disabled="!canSpinAgainFromLotteryBadgeReward"
      @close="closeLotteryBadgeRewardModal"
      @view-mine="jumpToMyBadgesFromLotteryReward"
      @primary-action="spinAgainFromLotteryBadgeReward"
    />

    <!-- 中奖结果弹窗 -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition-opacity duration-200"
        leave-active-class="transition-opacity duration-200"
        enter-from-class="opacity-0"
        leave-to-class="opacity-0"
      >
        <div 
          v-if="showResultModal" 
          class="fixed inset-0 bg-black/50 backdrop-blur-sm flex items-center justify-center z-50 p-4"
          @click.self="closeResultModal"
        >
          <div class="w-full max-w-sm animate-scale-in rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 text-center shadow-xl">
            <div v-if="spinResult?.prizeType === 'nothing'" class="mb-4">
              <div class="w-16 h-16 mx-auto mb-3 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--text-tertiary)]">
                <HelpCircle class="w-8 h-8" />
              </div>
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('entertainment.betterLuckNextTime') }}</h3>
              <p class="text-sm text-[var(--text-secondary)] mt-1">{{ spinResult?.prizeName }}</p>
            </div>
            <div v-else class="mb-4">
              <div class="w-16 h-16 mx-auto mb-3 rounded-2xl bg-amber-500/10 border border-amber-500/20 flex items-center justify-center">
                <Sparkles class="w-8 h-8 text-amber-500" />
              </div>
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('entertainment.congratulations') }}</h3>
              <p class="text-xl font-bold mt-2" :style="{ color: getPrizeColor(spinResult?.prizeType) }">
                {{ spinResult?.prizeName }}
              </p>
              <p class="text-sm text-[var(--text-secondary)] mt-2">
                <template v-if="spinResult?.prizeType === 'points'">
                  {{ $t('entertainment.wonPoints', { points: spinResult.prizeValue }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'balance'">
                  {{ $t('entertainment.wonBalance', { amount: (spinResult.prizeValue / 100).toFixed(2) }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'badge'">
                  {{ $t('entertainment.wonBadge', { badge: spinResult.prizeName }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'instance'">
                  {{ $t('entertainment.wonInstance') }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'cpu'">
                  {{ $t('entertainment.wonCpu', { value: spinResult.prizeValue }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'memory'">
                  {{ $t('entertainment.wonMemory', { value: spinResult.prizeValue }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'disk'">
                  {{ $t('entertainment.wonDisk', { value: spinResult.prizeValue }) }}
                </template>
                <template v-else-if="spinResult?.prizeType === 'traffic'">
                  {{ $t('entertainment.wonTraffic', { value: spinResult.prizeValue }) }}
                </template>
              </p>
            </div>
            <Button variant="primary" block @click="closeResultModal">
              {{ $t('common.confirm') }}
            </Button>
          </div>
        </div>
      </Transition>
    </Teleport>

    <BadgeBatchRewardModal
      :visible="showMultiDrawBadgeModal"
      :badges="multiDrawBadgeRewards"
      :title="$t('entertainment.multiDrawBadgesTitle')"
      :subtitle="$t('entertainment.multiDrawBadgesSubtitle', { count: multiDrawBadgeRewards.length })"
      :confirm-label="$t('entertainment.continueToMultiResults')"
      @close="continueToMultiDrawResults"
    />

    <!-- 十连抽结果弹窗 -->
    <Teleport to="body">
      <Transition
        enter-active-class="transition-opacity duration-200"
        leave-active-class="transition-opacity duration-200"
        enter-from-class="opacity-0"
        leave-to-class="opacity-0"
      >
        <div 
          v-if="showMultiDrawModal" 
          class="fixed inset-0 bg-black/50 backdrop-blur-sm flex items-center justify-center z-50 p-4"
          @click.self="closeMultiDrawModal"
        >
          <div class="w-full max-w-lg animate-scale-in rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-xl">
            <div class="flex items-center justify-between mb-4">
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ $t('entertainment.multiDrawResults') }}</h3>
              <button 
                type="button"
                class="w-8 h-8 rounded-full flex items-center justify-center text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
                @click="closeMultiDrawModal"
              >
                <X class="w-4 h-4" />
              </button>
            </div>
            
            <!-- 结果列表 -->
            <div class="max-h-80 overflow-y-auto space-y-2 mb-4 pr-1">
              <div 
                v-for="(result, idx) in multiDrawResults" 
                :key="result.id"
                class="flex items-center gap-3 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]"
              >
                <span class="text-xs text-[var(--text-tertiary)] font-mono w-6">#{{ idx + 1 }}</span>
                <div 
                  class="w-3 h-3 rounded-full shrink-0"
                  :style="{ backgroundColor: getPrizeColor(result.prizeType) }"
                ></div>
                <span class="text-sm text-[var(--text-primary)] font-medium flex-1 truncate">{{ result.prizeName }}</span>
                <span class="text-xs text-[var(--text-secondary)] font-mono">
                  <template v-if="result.prizeType === 'points'">+{{ result.prizeValue }} {{ $t('entertainment.pointsUnit') }}</template>
                  <template v-else-if="result.prizeType === 'balance'">+¥{{ (result.prizeValue / 100).toFixed(2) }}</template>
                  <template v-else-if="result.prizeType === 'badge'">+1 {{ $t('entertainment.badgeUnit') }}</template>
                  <template v-else-if="result.prizeType === 'instance'">{{ $t('entertainment.wonInstance') }}</template>
                  <template v-else-if="result.prizeType === 'cpu'">+{{ result.prizeValue }}%</template>
                  <template v-else-if="result.prizeType === 'memory'">+{{ result.prizeValue }}MB</template>
                  <template v-else-if="result.prizeType === 'disk'">+{{ result.prizeValue }}MB</template>
                  <template v-else-if="result.prizeType === 'traffic'">+{{ result.prizeValue }}GB</template>
                  <template v-else>-</template>
                </span>
              </div>
            </div>

            <!-- 统计摘要 -->
            <div class="border-t border-[var(--border-color)] pt-4 mb-4">
              <div class="grid grid-cols-2 gap-4 text-sm">
                <div>
                  <span class="text-[var(--text-secondary)]">{{ $t('entertainment.totalDraws') }}:</span>
                  <span class="text-[var(--text-primary)] font-semibold ml-2 font-mono tabular-nums">{{ multiDrawResults.length }}</span>
                </div>
                <div>
                  <span class="text-[var(--text-secondary)]">{{ $t('entertainment.totalPointsSpent') }}:</span>
                  <span class="text-[var(--text-primary)] font-semibold ml-2 font-mono tabular-nums">{{ multiDrawResults.reduce((sum, r) => sum + r.pointsSpent, 0) }}</span>
                </div>
              </div>
              <!-- 奖品统计：显示各类奖品总值 -->
              <div class="flex flex-wrap gap-1.5 mt-3">
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('balance') + '20', color: getPrizeColor('balance') }">
                  {{ $t('entertainment.prizeTypes.balance') }}: {{ multiDrawSummary.balance.toFixed(2) }}
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('badge') + '20', color: getPrizeColor('badge') }">
                  {{ $t('entertainment.prizeTypes.badge') }}: {{ multiDrawSummary.badge }} {{ $t('entertainment.badgeUnit') }}
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('instance') + '20', color: getPrizeColor('instance') }">
                  {{ $t('entertainment.prizeTypes.instance') }}: {{ multiDrawSummary.instance }} {{ $t('entertainment.instanceUnit') }}
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('cpu') + '20', color: getPrizeColor('cpu') }">
                  CPU: {{ multiDrawSummary.cpu }}%
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('memory') + '20', color: getPrizeColor('memory') }">
                  {{ $t('entertainment.prizeTypes.memory') }}: {{ multiDrawSummary.memory }}MB
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('disk') + '20', color: getPrizeColor('disk') }">
                  {{ $t('entertainment.prizeTypes.disk') }}: {{ multiDrawSummary.disk }}MB
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('traffic') + '20', color: getPrizeColor('traffic') }">
                  {{ $t('entertainment.prizeTypes.traffic') }}: {{ multiDrawSummary.traffic }}GB
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('points') + '20', color: getPrizeColor('points') }">
                  {{ $t('entertainment.prizeTypes.points') }}: {{ multiDrawSummary.points }}
                </div>
                <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium" :style="{ backgroundColor: getPrizeColor('nothing') + '20', color: getPrizeColor('nothing') }">
                  {{ $t('entertainment.prizeTypes.nothing') }}: {{ multiDrawSummary.nothing }}
                </div>
              </div>
            </div>

            <!-- 错误提示 -->
            <div v-if="multiDrawError" class="text-sm text-amber-500 bg-amber-500/10 border border-amber-500/20 rounded-xl p-3 mb-4">
              {{ $t('entertainment.multiDrawStopped') }}: {{ multiDrawError }}
            </div>

            <!-- 按钮组: 确认(30%) + 再次十连(70%) -->
            <div class="flex gap-2">
              <Button 
                variant="secondary"
                class="flex-[3]"
                @click="closeMultiDrawModal"
              >
                {{ $t('common.confirm') }}
              </Button>
              <Button 
                variant="primary"
                class="flex-[7]"
                :disabled="isMultiDrawing || !selectedLottery || points.points < selectedLottery.costPoints * 10"
                :loading="isMultiDrawing"
                @click="multiDrawAgain"
              >
                <template #icon>
                  <Zap class="w-4 h-4" />
                </template>
                {{ $t('entertainment.multiDrawAgain') }}
              </Button>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
/* 转盘旋转动画 */
.wheel-spin {
  transition-property: transform;
  will-change: transform;
}

/* 慢速脉冲动画 */
@keyframes pulse-slow {
  0%, 100% {
    transform: translate(-50%, -50%) scale(1);
    box-shadow: 0 0 0 0 rgba(251, 191, 36, 0.4);
  }
  50% {
    transform: translate(-50%, -50%) scale(1.02);
    box-shadow: 0 0 0 10px rgba(251, 191, 36, 0);
  }
}

.animate-pulse-slow {
  animation: pulse-slow 2s ease-in-out infinite;
}

/* 弹窗动画 */
.animate-scale-in {
  animation: scale-in 0.25s cubic-bezier(0.25, 0.1, 0.25, 1);
}

@keyframes scale-in {
  from {
    transform: scale(0.96);
    opacity: 0;
  }
  to {
    transform: scale(1);
    opacity: 1;
  }
}

/* 签到礼盒摇晃动画 */
@keyframes shake-box {
  0%, 100% { 
    transform: rotate(0deg) scale(1); 
  }
  10% { 
    transform: rotate(-8deg) scale(1.03); 
  }
  20% { 
    transform: rotate(8deg) scale(1.05); 
  }
  30% { 
    transform: rotate(-6deg) scale(1.03); 
  }
  40% { 
    transform: rotate(6deg) scale(1.05); 
  }
  50% { 
    transform: rotate(-4deg) scale(1.02); 
  }
  60% { 
    transform: rotate(4deg) scale(1.04); 
  }
  70% { 
    transform: rotate(-2deg) scale(1.02); 
  }
  80% { 
    transform: rotate(2deg) scale(1.01); 
  }
  90% { 
    transform: rotate(-1deg) scale(1); 
  }
}

.animate-shake-box {
  animation: shake-box 0.8s cubic-bezier(0.25, 0.1, 0.25, 1) infinite;
  transform-origin: center bottom;
}

/* 签到礼盒打开动画 */
@keyframes open-box {
  0% { 
    transform: scale(1) rotate(0deg); 
  }
  20% { 
    transform: scale(1.1) rotate(-2deg); 
  }
  40% { 
    transform: scale(1.15) rotate(1deg); 
  }
  60% { 
    transform: scale(1.08) rotate(-1deg); 
  }
  80% { 
    transform: scale(0.96) rotate(0deg); 
  }
  100% { 
    transform: scale(0.85) rotate(0deg); 
    opacity: 0; 
  }
}

.animate-open-box {
  animation: open-box 0.6s cubic-bezier(0.25, 0.1, 0.25, 1) forwards;
  transform-origin: center center;
}

/* 纸屑下落动画 */
@keyframes confetti {
  0% {
    transform: translateY(0) rotate(0deg);
    opacity: 1;
  }
  100% {
    transform: translateY(300px) rotate(720deg);
    opacity: 0;
  }
}

.animate-confetti {
  animation: confetti 2s cubic-bezier(0.25, 0.1, 0.25, 1) forwards;
}

@media (prefers-reduced-motion: reduce) {
  .animate-pulse-slow,
  .animate-shake-box,
  .animate-open-box,
  .animate-confetti,
  .wheel-spin {
    animation: none !important;
    transition: none !important;
  }
}
</style>
