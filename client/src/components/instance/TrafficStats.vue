<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import TrafficLineChart from '@/components/traffic/TrafficLineChart.vue'
import { fillContinuousDays, type TrafficHistoryItem } from '@/utils/traffic'

const props = defineProps<{
  instanceId: number
}>()

const { t } = useI18n()

// 流量数据
interface TrafficData {
  monthlyUsed: string
  monthlyUsedFormatted: string
  monthlyLimit: string | null
  monthlyLimitFormatted: string | null
  trafficStatus: 'NORMAL' | 'WARNING' | 'LIMITED'
  percentage: number
  trafficResetDay: number
  trafficResetPrice?: number | null
  periodStart: string
  periodEnd: string
}

const trafficData = ref<TrafficData | null>(null)
const trafficHistory = ref<TrafficHistoryItem[]>([])
const loading = ref(true)
const historyLoading = ref(true)
const selectedDays = ref<number>(30)

// Guard marker for single-day X-axis label centering:
// trafficHistory.length === 1 ? 50 : (label.index / (trafficHistory.length - 1)) * 100

onMounted(async () => {
  await Promise.all([loadTrafficData(), loadTrafficHistory(selectedDays.value)])
})

async function loadTrafficData() {
  loading.value = true
  try {
    const data = await api.traffic.getInstanceTraffic(props.instanceId)
    trafficData.value = data
  } catch (error) {
    console.error('Failed to load traffic data:', error)
  } finally {
    loading.value = false
  }
}

async function loadTrafficHistory(days: number) {
  historyLoading.value = true
  try {
    const response = await api.traffic.getInstanceTrafficHistory(props.instanceId, days)
    trafficHistory.value = fillContinuousDays(response.data, days)
  } catch (error) {
    console.error('Failed to load traffic history:', error)
  } finally {
    historyLoading.value = false
  }
}

async function handleDaysChange(days: number) {
  if (selectedDays.value === days) return
  selectedDays.value = days
  await loadTrafficHistory(days)
}

// 用量显示文案，如 "7.6 / 512 GB"
const usageDisplayText = computed(() => {
  if (!trafficData.value) return ''
  const used = trafficData.value.monthlyUsedFormatted
  const limit = trafficData.value.monthlyLimitFormatted
  if (!limit) return `${used} / ${t('traffic.unlimited')}`
  const usedParts = used.split(' ')
  const limitParts = limit.split(' ')
  if (usedParts.length === 2 && limitParts.length === 2 && usedParts[1] === limitParts[1]) {
    return `${usedParts[0]} / ${limitParts[0]} ${limitParts[1]}`
  }
  return `${used} / ${limit}`
})

// 进度条百分比 (0 - 100)
const progressPercent = computed(() => {
  if (!trafficData.value) return 0
  const p = trafficData.value.percentage ?? 0
  return Math.max(0, Math.min(100, p))
})

// 重置提示信息
const resetHintText = computed(() => {
  if (!trafficData.value) return ''
  const nextDate = trafficData.value.periodEnd?.slice(5) || ''
  const price = trafficData.value.trafficResetPrice
  if (price !== undefined && price !== null && Number(price) > 0) {
    return t('traffic.paidResetHint', {
      date: nextDate,
      price: Number(price).toFixed(2)
    })
  }
  return `${t('traffic.nextFreeReset')} ${nextDate} · ${t('traffic.periodResetHint', { date: trafficData.value.trafficResetDay })}`
})
</script>

<template>
  <div class="card p-6">
    <!-- Top Header -->
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 mb-6">
      <div>
        <h3 class="text-base sm:text-lg font-bold text-[var(--text-primary)]">
          {{ t('traffic.thisPeriod') }}
        </h3>
        <p class="text-xs text-[var(--text-secondary)] mt-1">
          {{ t('traffic.historySubtitle') }}
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button
          v-for="d in [30, 60, 90]"
          :key="d"
          type="button"
          class="rounded-full px-3.5 py-1 text-xs sm:text-sm font-medium transition-all"
          :class="selectedDays === d
            ? 'bg-[var(--accent)] text-white shadow-sm'
            : 'border border-[var(--border-color)] text-[var(--text-secondary)] hover:border-[var(--border-hover)]'"
          @click="handleDaysChange(d)"
        >
          {{ t(`traffic.days${d}`) }}
        </button>
      </div>
    </div>

    <!-- Usage & Progress -->
    <div v-if="loading && !trafficData" class="animate-pulse space-y-3 mb-6">
      <div class="h-4 bg-[var(--bg-secondary)] rounded w-1/4"></div>
      <div class="h-1 bg-[var(--bg-secondary)] rounded"></div>
      <div class="h-3 bg-[var(--bg-secondary)] rounded w-1/3"></div>
    </div>
    <div v-else-if="trafficData" class="space-y-2 mb-6">
      <div class="flex items-center justify-between text-sm">
        <span class="text-sm font-medium text-[var(--text-secondary)]">{{ t('traffic.used') }}</span>
        <span class="text-base font-bold text-[var(--text-primary)]">
          {{ usageDisplayText }}
        </span>
      </div>

      <!-- Thin progress bar with indicator dot -->
      <div class="relative w-full h-[3px] bg-[var(--bg-secondary)] border border-[var(--border-color)] rounded-full my-3">
        <div
          class="absolute left-0 top-0 h-full bg-[var(--accent)] rounded-full transition-all duration-300"
          :style="{ width: `${progressPercent}%` }"
        ></div>
        <div
          class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 w-2 h-2 rounded-full bg-[var(--accent)] transition-all duration-300 pointer-events-none"
          :style="{ left: `${progressPercent}%` }"
        ></div>
      </div>

      <!-- Reset note -->
      <div class="text-xs text-[var(--text-secondary)]">
        <span v-if="trafficData.trafficStatus === 'LIMITED'" class="text-[var(--error)] mr-2">
          {{ t('traffic.throttledHint') }} ·
        </span>
        {{ resetHintText }}
      </div>
    </div>

    <!-- Reusable Line Chart -->
    <TrafficLineChart
      :history="trafficHistory"
      :loading="historyLoading"
      id-prefix="instance-traffic"
    />
  </div>
</template>
