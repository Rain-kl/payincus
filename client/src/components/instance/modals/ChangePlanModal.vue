<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import { useToast } from '@/stores/toast'
import { useConfigStore } from '@/stores/config'
import api from '@/api'
import type { ChangePlanPreview } from '@/types/api'
import { calculateDailyPrice } from '@/utils/billing'
import { freeSiteCopy, getFreeSiteBillingCycleLabel } from '@/utils/freeSiteFun'
import { walletPath } from '@/utils/app-paths'

const { t } = useI18n()
const themeStore = useThemeStore()
const toast = useToast()
const configStore = useConfigStore()

interface PackagePlanOption {
  id: number
  name: string
  description: string | null
  price: number
  billingCycle: number
  cpu: number
  memory: number
  disk: number
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  swapSize: number
  monthlyTrafficLimit: string | null
  isActive: boolean
  isSoldOut: boolean
}

interface Props {
  show: boolean
  instanceId: number
  instanceName: string
  packageId: number
  currentPlanId: number | null
  currentBillingPrice?: number | null
  currentBillingCycle?: number | null
  instanceType?: 'container' | 'vm'  // 实例类型，用于显示重启提示
}

const props = defineProps<Props>()
const emit = defineEmits<{
  'update:show': [value: boolean]
  'success': []
}>()

// State
const loading = ref(false)
const loadingPreview = ref(false)
const changing = ref(false)
const plans = ref<PackagePlanOption[]>([])
const selectedPlanId = ref<number | null>(null)
const preview = ref<ChangePlanPreview | null>(null)
const userBalance = ref<number>(0)
const error = ref<string>('')
const showRules = ref(false)

// 获取当前方案的日价（用于筛选升级方案）
const currentPlanDailyPrice = computed(() => {
  const currentPlan = plans.value.find(p => p.id === props.currentPlanId)
  if (!currentPlan) return 0
  const currentPrice = props.currentBillingPrice ?? (currentPlan.price / 100)
  const currentCycle = props.currentBillingCycle ?? currentPlan.billingCycle
  // 使用公共方法计算日价
  return calculateDailyPrice(currentPrice, currentCycle)
})

// 可选方案（只显示比当前方案日价更高的升级方案）
const availablePlans = computed(() => {
  return plans.value.filter(p => {
    if (p.id === props.currentPlanId) return false
    // 使用公共方法计算日价
    const dailyPrice = calculateDailyPrice(p.price / 100, p.billingCycle)
    // 只显示日价更高的方案（升级）
    return dailyPrice > currentPlanDailyPrice.value
  })
})

// 是否已是最高方案（没有可升级的方案）
const isHighestPlan = computed(() => {
  return !loading.value && plans.value.length > 0 && availablePlans.value.length === 0
})

// 是否余额不足（仅升级时检查）
const insufficientBalance = computed(() => {
  if (!preview.value || !preview.value.isUpgrade) return false
  return userBalance.value < preview.value.priceDiff
})

// 不能变更的原因文本
const cannotChangeReasonText = computed(() => {
  if (!preview.value || preview.value.canChange) return ''
  
  const reason = preview.value.cannotChangeReason
  if (!reason) return t('billing.cannotChangeUnknown')
  
  if (reason === 'remaining_days_insufficient') {
    return t('billing.cannotChangeRemainingDays', { days: 15 })
  }
  
  if (reason === 'instance_status_invalid') {
    return t('billing.cannotChangeInstanceStatus')
  }

  if (
    reason === 'host_resources_insufficient' ||
    reason === 'host_not_found' ||
    reason === 'host_not_online' ||
    reason === 'cpu_insufficient' ||
    reason === 'memory_insufficient' ||
    reason === 'disk_insufficient'
  ) {
    return preview.value.resourceWarnings?.[0] || t('billing.cannotChangeHostResources')
  }
  
  return t('billing.cannotChangeUnknown')
})

// 格式化金额（分转元 - 用于 plan.price）
function formatPriceCents(cents: number): string {
  return (cents / 100).toFixed(2)
}

// 格式化金额（元 - 用于 preview 计算结果）
function formatMoney(yuan: number): string {
  return yuan.toFixed(2)
}

// 格式化内存
function formatMemory(mb: number): string {
  if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB'
  return mb + ' MB'
}

// 格式化磁盘（1024进制）
function formatDisk(mb: number): string {
  if (mb >= 1024) {
    const gb = mb / 1024
    return gb.toFixed(1) + ' GB'
  }
  return mb + ' MB'
}

// 格式化日期
function formatDate(dateStr: string): string {
  const date = new Date(dateStr)
  return date.toLocaleDateString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit'
  })
}

// 获取账期文本
function getBillingCycleText(months: number): string {
  if (configStore.freeSiteMode) return getFreeSiteBillingCycleLabel(months)
  if (months === 1) return t('billing.billingCycleMonthly')
  if (months === 3) return t('billing.billingCycleQuarterly')
  if (months === 12) return t('billing.billingCycleYearly')
  return `${months}${t('billing.months')}`
}


// 加载方案列表
async function loadPlans() {
  loading.value = true
  error.value = ''
  
  try {
    const [plansRes, balanceRes] = await Promise.all([
      api.billing.getPackagePlans(props.packageId),
      api.billing.getUserBalance()
    ])
    
    plans.value = plansRes.plans as PackagePlanOption[]
    userBalance.value = balanceRes.balance.balance
  } catch (err: any) {
    error.value = err.message || t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

// 加载升降级预览
async function loadPreview() {
  if (!selectedPlanId.value) {
    preview.value = null
    return
  }

  const selectedPlan = plans.value.find(plan => plan.id === selectedPlanId.value)
  if (!selectedPlan || !selectedPlan.isActive || selectedPlan.isSoldOut) {
    preview.value = null
    return
  }
  
  loadingPreview.value = true
  error.value = ''
  
  try {
    const res = await api.billing.getChangePlanPreview(props.instanceId, selectedPlanId.value)
    preview.value = res.preview as ChangePlanPreview
  } catch (err: any) {
    error.value = err.message || t('common.loadFailed')
    preview.value = null
  } finally {
    loadingPreview.value = false
  }
}

// 执行升级
async function handleChangePlan() {
  if (!selectedPlanId.value || !preview.value) return
  if (!preview.value.canChange) return
  if (insufficientBalance.value) return
  
  changing.value = true
  error.value = ''
  
  try {
    const result = await api.billing.changePlan(props.instanceId, selectedPlanId.value)
    
    // 检查是否需要重启实例以应用配置
    if (result.needRestart && result.restartMessage) {
      // 先发出成功事件，然后显示警告 toast
      emit('success')
      // 使用 setTimeout 确保成功 toast 先显示，再显示重启警告
      setTimeout(() => {
        // KVM_RESTART_REQUIRED 是特殊标记，需要显示本地化文本
        const message = result.restartMessage === 'KVM_RESTART_REQUIRED'
          ? t('billing.kvmRestartRequired')
          : result.restartMessage
        toast.warning(message!, 8000) // 警告显示 8 秒
      }, 500)
    } else {
      emit('success')
    }
    
    emit('update:show', false)
  } catch (err: any) {
    error.value = err.message || t('billing.changePlanFailed')
  } finally {
    changing.value = false
  }
}

// 监听选择变化
watch(() => selectedPlanId.value, () => {
  loadPreview()
})

// 监听显示状态
watch(() => props.show, (newVal) => {
  if (newVal) {
    loadPlans()
  } else {
    // 重置状态
    plans.value = []
    selectedPlanId.value = null
    preview.value = null
    userBalance.value = 0
    error.value = ''
    showRules.value = false
  }
})

function handleClose() {
  emit('update:show', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="show"
        class="modal-overlay"
        @click.self="handleClose"
      >
        <!-- Backdrop -->
        <div class="modal-backdrop" @click="handleClose" />

        <!-- Modal -->
        <div class="modal-content max-w-lg">
          <!-- Header -->
          <div class="modal-header sticky top-0 z-10">
            <div class="flex items-center gap-2">
              <h3 class="modal-title">{{ t('billing.changePlanTitle') }}</h3>
              <!-- 查看规则按钮 -->
              <button
                class="text-xs px-2 py-0.5 rounded border border-themed text-themed-muted hover:text-themed hover:bg-themed-hover transition-colors"
                @click="showRules = !showRules"
              >
                {{ showRules ? t('billing.hideRules') : t('billing.viewRules') }}
              </button>
            </div>
            <button
              class="p-1 rounded text-themed-muted hover:text-themed hover:bg-themed-hover transition-colors"
              @click="handleClose"
            >
              <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
              </svg>
            </button>
          </div>

          <!-- Content -->
          <div class="modal-body">
            <!-- Loading -->
            <div v-if="loading" class="flex justify-center py-8">
              <div class="animate-spin rounded-full h-8 w-8 border-b-2 border-accent"></div>
            </div>

            <!-- Error -->
            <div v-else-if="error && !plans.length" class="py-2">
              <div class="p-3 rounded bg-error/10 text-error text-sm">
                {{ error }}
              </div>
            </div>

            <!-- Content -->
            <template v-else>
              <!-- 变更规则面板 -->
              <div
                v-if="showRules"
                class="p-4 rounded border border-themed-hover bg-themed-secondary text-sm"
              >
                <div class="font-medium mb-2 flex items-center gap-1 text-themed">
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                  </svg>
                  {{ t('billing.changePlanRulesTitle') }}
                </div>
                <ul class="list-disc list-inside space-y-1 text-themed-secondary">
                  <li>{{ t('billing.changePlanRule1') }}</li>
                  <li>{{ t('billing.changePlanRule2') }}</li>
                  <li>{{ t('billing.changePlanRule3') }}</li>
                  <li>{{ t('billing.changePlanRule4') }}</li>
                </ul>
              </div>

              <!-- KVM/LXC 重启提示 -->
              <div
                class="p-3 rounded border text-sm flex items-start gap-2"
                :class="instanceType === 'vm'
                  ? 'border-warning/30 bg-warning/10'
                  : 'border-success/30 bg-success/10'"
              >
                <svg
                  class="w-4 h-4 mt-0.5 flex-shrink-0"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                  :class="instanceType === 'vm' ? 'text-warning' : 'text-success'"
                >
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                </svg>
                <span :class="instanceType === 'vm' ? 'text-warning' : 'text-success'">
                  {{ instanceType === 'vm' ? t('billing.kvmRestartHint') : t('billing.lxcInstantHint') }}
                </span>
              </div>

              <!-- 已是最高方案提示 -->
              <div
                v-if="isHighestPlan"
                class="p-4 rounded border border-themed bg-themed-secondary text-sm text-center"
              >
                <div class="font-medium mb-1 text-themed">{{ t('billing.alreadyHighestPlan') }}</div>
                <div class="text-themed-secondary">{{ t('billing.contactForCustomPlan') }}</div>
              </div>

              <!-- 方案列表 -->
              <div v-if="!isHighestPlan">
                <label class="block text-sm font-medium mb-2 text-themed-secondary">
                  {{ t('billing.selectNewPlan') }}
                </label>
                <div class="space-y-2">
                  <button
                    v-for="plan in availablePlans"
                    :key="plan.id"
                    :disabled="!plan.isActive || plan.isSoldOut"
                    class="w-full p-3 rounded border text-left transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    :class="[
                      selectedPlanId === plan.id && !plan.isSoldOut
                        ? 'border-accent bg-accent/10'
                        : 'border-themed hover:border-themed-hover'
                    ]"
                    @click="!plan.isSoldOut && plan.isActive && (selectedPlanId = plan.id)"
                  >
                    <div class="flex justify-between items-start">
                      <div>
                        <div class="font-medium text-themed">{{ plan.name }}</div>
                        <div class="text-xs mt-1 text-themed-muted">
                          {{ plan.cpu }}% CPU · {{ formatMemory(plan.memory) }} · {{ formatDisk(plan.disk) }}
                        </div>
                        <div
                          v-if="props.instanceType !== 'vm' && plan.swapSize > 0"
                          class="text-xs mt-1 text-themed-muted"
                        >
                          {{ t('resources.plans.swapSize') }} · {{ plan.swapSize }} MB
                        </div>
                      </div>
                      <div class="text-right">
                        <div class="font-medium text-themed">¥{{ formatPriceCents(plan.price) }}</div>
                        <div class="text-xs text-themed-muted">{{ getBillingCycleText(plan.billingCycle) }}</div>
                      </div>
                    </div>
                    <!-- 状态标签 -->
                    <div v-if="!plan.isActive || plan.isSoldOut" class="mt-2">
                      <span
                        class="text-xs px-1.5 py-0.5 rounded badge"
                        :class="plan.isSoldOut ? 'badge-error' : 'badge-default'"
                      >
                        {{ plan.isSoldOut ? t('billing.planSoldOut') : t('billing.planInactive') }}
                      </span>
                    </div>
                  </button>
                </div>
              </div>

              <!-- 预览加载中 -->
              <div v-if="loadingPreview" class="flex justify-center py-4">
                <div class="animate-spin rounded-full h-6 w-6 border-b-2 border-accent"></div>
              </div>

              <!-- 预览详情 -->
              <div
                v-else-if="preview"
                class="p-4 rounded border border-themed bg-themed-secondary text-sm space-y-2"
              >
                <!-- 不能变更提示 -->
                <div
                  v-if="!preview.canChange"
                  class="p-3 rounded border border-error/30 bg-error/10 text-sm"
                >
                  <div class="flex items-center gap-1 font-medium text-error">
                    <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
                    </svg>
                    {{ t('billing.cannotChange') }}
                  </div>
                  <div class="mt-1 text-error">{{ cannotChangeReasonText }}</div>
                  <ul v-if="preview.resourceWarnings?.length" class="mt-2 list-disc list-inside space-y-1 text-error">
                    <li v-for="warning in preview.resourceWarnings" :key="warning">{{ warning }}</li>
                  </ul>
                </div>

                <!-- 升级标识 -->
                <div class="flex items-center gap-2">
                  <span class="text-xs px-2 py-0.5 rounded badge badge-success">
                    {{ t('billing.isUpgrade') }}
                  </span>
                </div>

                <div class="flex justify-between">
                  <span class="text-themed-muted">{{ t('billing.remainingDays') }}</span>
                  <span class="text-themed">{{ Math.ceil(preview.remainingDays) }} {{ t('billing.days') }}</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.oldDailyPrice : t('billing.oldDailyPrice') }}</span>
                  <span class="text-themed">¥{{ preview.oldDailyPrice.toFixed(4) }}/{{ t('billing.day') }}</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.remainingValue : t('billing.remainingValue') }}</span>
                  <span class="text-themed">¥{{ formatMoney(preview.remainingValue) }}</span>
                </div>
                <div class="border-t border-themed my-1" />
                <div class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.newDailyPrice : t('billing.newDailyPrice') }}</span>
                  <span class="text-themed">¥{{ preview.newDailyPrice.toFixed(4) }}/{{ t('billing.day') }}</span>
                </div>
                <!-- 新方案剩余费用：有折扣时显示原价+折扣+折后 -->
                <div v-if="preview.discountAmount > 0" class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.newPlanCost : t('billing.newPlanCostOriginal') }}</span>
                  <span class="text-themed">¥{{ formatMoney(preview.newPlanCost + preview.discountAmount) }}</span>
                </div>
                <!-- 折扣信息 -->
                <div v-if="preview.discountAmount > 0" class="flex justify-between">
                  <span class="text-success">{{ t('billing.discountAmount') }} (-{{ (preview.discountRate * 100).toFixed(0) }}%)</span>
                  <span class="text-success">-¥{{ formatMoney(preview.discountAmount) }}</span>
                </div>
                <div v-if="preview.discountAmount > 0" class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.newPlanCost : t('billing.newPlanCostFinal') }}</span>
                  <span class="text-themed">¥{{ formatMoney(preview.newPlanCost) }}</span>
                </div>
                <!-- 无折扣时直接显示新方案剩余费用 -->
                <div v-if="preview.discountAmount <= 0" class="flex justify-between">
                  <span class="text-themed-muted">{{ configStore.freeSiteMode ? freeSiteCopy.newPlanCost : t('billing.newPlanCostOriginal') }}</span>
                  <span class="text-themed">¥{{ formatMoney(preview.newPlanCost) }}</span>
                </div>
                <div class="border-t border-themed my-1" />
                <div class="flex justify-between font-medium">
                  <span class="text-themed-secondary">{{ configStore.freeSiteMode ? freeSiteCopy.needPay : t('billing.needPay') }}</span>
                  <span :class="insufficientBalance ? 'text-error' : 'text-accent'">
                    ¥{{ formatMoney(preview.priceDiff) }}
                  </span>
                </div>
                <div class="flex justify-between">
                  <span class="text-themed-muted">{{ t('billing.newExpiresAt') }}</span>
                  <span class="text-themed">{{ formatDate(preview.newExpiresAt) }}</span>
                </div>

                <!-- 新配置 -->
                <div class="mt-3 pt-3 border-t border-themed text-xs text-themed-muted">
                  {{ t('billing.newConfig') }}:
                  {{ preview.newConfig.cpu }}% CPU ·
                  {{ formatMemory(preview.newConfig.memory) }} ·
                  {{ formatDisk(preview.newConfig.disk) }}
                </div>
              </div>

              <!-- 余额不足提示 -->
              <div
                v-if="preview && insufficientBalance"
                class="p-3 rounded border border-warning/30 bg-warning/10 text-sm text-warning"
              >
                {{ t('billing.insufficientBalance') }}
                <RouterLink :to="walletPath()" class="underline ml-1">{{ t('billing.goRecharge') }}</RouterLink>
              </div>

              <!-- 错误提示 -->
              <div
                v-if="error && plans.length"
                class="p-3 rounded bg-error/10 text-error text-sm"
              >
              {{ error }}
              </div>
            </template>
          </div>

          <!-- Footer -->
          <div class="modal-footer sticky bottom-0">
            <button
              class="btn btn-secondary"
              @click="handleClose"
            >
              {{ t('common.cancel') }}
            </button>
            <button
              :disabled="loading || loadingPreview || changing || !preview ||
                !preview.canChange ||
                insufficientBalance"
              class="btn btn-primary disabled:opacity-50 disabled:cursor-not-allowed"
              @click="handleChangePlan"
            >
              {{ changing ? t('billing.changePlanInProgress') : t('billing.upgrade') }}
            </button>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

