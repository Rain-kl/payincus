<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import api from '@/api'

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

interface TrafficHistoryItem {
  date: string
  rxTotal: string
  txTotal: string
  rxFormatted: string
  txFormatted: string
  total: string
  totalFormatted: string
}

const trafficData = ref<TrafficData | null>(null)
const trafficHistory = ref<TrafficHistoryItem[]>([])
const loading = ref(true)
const historyLoading = ref(true)
const selectedDays = ref<number>(30)

// 悬浮点索引
const hoverIndex = ref<number | null>(null)

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

// 补齐并格式化连续天数
function fillContinuousDays(rawItems: TrafficHistoryItem[], days: number): TrafficHistoryItem[] {
  const itemMap = new Map<string, TrafficHistoryItem>()
  for (const item of rawItems) {
    itemMap.set(item.date, item)
  }

  const result: TrafficHistoryItem[] = []
  const now = new Date()
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(now.getTime() - i * 24 * 60 * 60 * 1000)
    const year = d.getFullYear()
    const month = String(d.getMonth() + 1).padStart(2, '0')
    const day = String(d.getDate()).padStart(2, '0')
    const dateStr = `${year}-${month}-${day}`

    const existing = itemMap.get(dateStr)
    if (existing) {
      result.push(existing)
    } else {
      result.push({
        date: dateStr,
        rxTotal: '0',
        txTotal: '0',
        rxFormatted: '0 B',
        txFormatted: '0 B',
        total: '0',
        totalFormatted: '0 B'
      })
    }
  }
  return result
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
  hoverIndex.value = null
  await loadTrafficHistory(days)
}

// 格式化字节数
const formatBytes = (bytes: number): string => {
  if (bytes === 0) return '0 B'
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB'
  if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB'
  return (bytes / 1073741824).toFixed(1) + ' GB'
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

// 计算峰值数值（用于标尺上限）
const chartMaxValue = computed(() => {
  if (trafficHistory.value.length === 0) return 0
  return Math.max(...trafficHistory.value.map(h => Number(h.total) || 0))
})

// 计算 Y 轴整倍数上限 (GB)
const ceilingGb = computed(() => {
  const peakGb = chartMaxValue.value / (1024 * 1024 * 1024)
  if (peakGb <= 0) return 1
  const niceCeilings = [
    0.5, 1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 20, 25, 30, 40, 50, 60, 80, 100, 150, 200, 300, 500, 1000
  ]
  for (const c of niceCeilings) {
    if (c >= peakGb * 1.08) {
      return c
    }
  }
  const factor = Math.pow(10, Math.floor(Math.log10(peakGb)))
  return Math.ceil((peakGb * 1.1) / factor) * factor
})

// Y 轴 5 档刻度 (100%, 75%, 50%, 25%, 0%)
const yAxisTicks = computed(() => {
  const c = ceilingGb.value
  const steps = 4
  const ticks = []
  for (let i = steps; i >= 0; i--) {
    const val = (c / steps) * i
    const label = val === 0 ? '0 GB' : `${Number(val.toFixed(2))} GB`
    ticks.push({
      label,
      valGb: val,
      yPercent: ((steps - i) / steps) * 100
    })
  }
  return ticks
})

// 归一化计算每个点的坐标
const chartPoints = computed(() => {
  const len = trafficHistory.value.length
  if (len === 0) return []
  const cGb = ceilingGb.value

  return trafficHistory.value.map((item, idx) => {
    const x = len <= 1 ? 500 : (idx / (len - 1)) * 1000
    const bytes = Number(item.total) || 0
    const valGb = bytes / (1024 * 1024 * 1024)
    const ratio = Math.max(0, Math.min(1, valGb / cGb))
    const y = 200 - ratio * 200
    return {
      x,
      y,
      item,
      labelDate: item.date?.slice(5) || ''
    }
  })
})

// SVG 折线路径
const linePath = computed(() => {
  const pts = chartPoints.value
  if (pts.length === 0) return ''
  return pts.map((p, idx) => `${idx === 0 ? 'M' : 'L'} ${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(' ')
})

// SVG 面积渐变填充路径
const areaPath = computed(() => {
  const pts = chartPoints.value
  if (pts.length === 0) return ''
  const first = pts[0]
  const last = pts[pts.length - 1]
  const linePart = pts.map((p, idx) => `${idx === 0 ? 'M' : 'L'} ${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(' ')
  return `${linePart} L ${last.x.toFixed(2)} 200 L ${first.x.toFixed(2)} 200 Z`
})

// X 轴日期刻度（4 个均匀点）
const xAxisLabels = computed(() => {
  const len = chartPoints.value.length
  if (len === 0) return []
  if (len <= 4) {
    return chartPoints.value.map((p, idx) => ({ date: p.labelDate, index: idx }))
  }
  const idx1 = 0
  const idx2 = Math.round((len - 1) / 3)
  const idx3 = Math.round(((len - 1) * 2) / 3)
  const idx4 = len - 1
  return [
    { date: chartPoints.value[idx1].labelDate, index: idx1 },
    { date: chartPoints.value[idx2].labelDate, index: idx2 },
    { date: chartPoints.value[idx3].labelDate, index: idx3 },
    { date: chartPoints.value[idx4].labelDate, index: idx4 }
  ]
})

// 悬浮点与对应数据
const hoverPoint = computed(() => {
  if (hoverIndex.value === null) return null
  return chartPoints.value[hoverIndex.value] || null
})

const hoverItem = computed(() => {
  if (hoverIndex.value === null) return null
  return trafficHistory.value[hoverIndex.value] || null
})

function handleMouseMove(e: MouseEvent) {
  const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
  const x = e.clientX - rect.left
  const ratio = Math.max(0, Math.min(1, x / rect.width))
  const count = chartPoints.value.length
  if (count > 0) {
    hoverIndex.value = Math.min(count - 1, Math.max(0, Math.round(ratio * (count - 1))))
  }
}

function handleMouseLeave() {
  hoverIndex.value = null
}

const tooltipStyle = computed(() => {
  if (!hoverPoint.value) return {}
  const leftPercent = hoverPoint.value.x / 10
  const topPercent = Math.max(12, Math.min(78, (hoverPoint.value.y / 200) * 100))
  if (leftPercent > 68) {
    return {
      right: `${Math.max(2, 100 - leftPercent + 2)}%`,
      top: `${topPercent}%`,
      transform: 'translateY(-50%)'
    }
  }
  return {
    left: `${Math.max(2, leftPercent + 2)}%`,
    top: `${topPercent}%`,
    transform: 'translateY(-50%)'
  }
})

// 峰值数据与文案
const peakInfo = computed(() => {
  if (trafficHistory.value.length === 0) return { formatted: '0 GB', date: '' }
  let max = trafficHistory.value[0]
  for (const item of trafficHistory.value) {
    if (Number(item.total) > Number(max.total)) {
      max = item
    }
  }
  const bytes = Number(max.total)
  const gb = bytes / (1024 * 1024 * 1024)
  const formatted = bytes === 0 ? '0 GB' : (gb >= 0.1 ? `${gb.toFixed(1)} GB` : formatBytes(bytes))
  const date = max.date?.slice(5) || ''
  return { formatted, date }
})

const peakText = computed(() => {
  if (!peakInfo.value.date) return peakInfo.value.formatted
  return `${peakInfo.value.formatted}(${peakInfo.value.date})`
})

// 日均用量
const dailyAvgText = computed(() => {
  if (trafficHistory.value.length === 0) return '0 GB'
  const sumBytes = trafficHistory.value.reduce((acc, cur) => acc + (Number(cur.total) || 0), 0)
  const avgBytes = sumBytes / trafficHistory.value.length
  const gb = avgBytes / (1024 * 1024 * 1024)
  if (gb >= 0.05) {
    return `${gb.toFixed(1)} GB`
  }
  return formatBytes(avgBytes)
})
</script>

<template>
  <div class="card p-6">
    <!-- Top Header -->
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 mb-6">
      <div>
        <h3 class="text-base sm:text-lg font-bold text-gray-900 dark:text-gray-100">
          {{ t('traffic.thisPeriod') }}
        </h3>
        <p class="text-xs text-gray-500 dark:text-gray-400 mt-1">
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
            ? 'bg-gray-900 text-white dark:bg-white dark:text-gray-900 shadow-sm'
            : 'border border-gray-200 dark:border-gray-700 text-gray-600 dark:text-gray-400 hover:border-gray-300 dark:hover:border-gray-600'"
          @click="handleDaysChange(d)"
        >
          {{ t(`traffic.days${d}`) }}
        </button>
      </div>
    </div>

    <!-- Usage & Progress -->
    <div v-if="loading && !trafficData" class="animate-pulse space-y-3 mb-6">
      <div class="h-4 bg-gray-200 dark:bg-gray-700 rounded w-1/4"></div>
      <div class="h-1 bg-gray-200 dark:bg-gray-700 rounded"></div>
      <div class="h-3 bg-gray-200 dark:bg-gray-700 rounded w-1/3"></div>
    </div>
    <div v-else-if="trafficData" class="space-y-2 mb-6">
      <div class="flex items-center justify-between text-sm">
        <span class="text-sm font-medium text-gray-600 dark:text-gray-400">{{ t('traffic.used') }}</span>
        <span class="text-base font-bold text-gray-900 dark:text-gray-100">
          {{ usageDisplayText }}
        </span>
      </div>

      <!-- Thin progress bar with indicator dot -->
      <div class="relative w-full h-[3px] bg-gray-100 dark:bg-gray-800 rounded-full my-3">
        <div
          class="absolute left-0 top-0 h-full bg-[#2563eb] rounded-full transition-all duration-300"
          :style="{ width: `${progressPercent}%` }"
        ></div>
        <div
          class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 w-2 h-2 rounded-full bg-[#2563eb] transition-all duration-300 pointer-events-none"
          :style="{ left: `${progressPercent}%` }"
        ></div>
      </div>

      <!-- Reset note -->
      <div class="text-xs text-gray-500 dark:text-gray-400">
        <span v-if="trafficData.trafficStatus === 'LIMITED'" class="text-red-500 mr-2">
          {{ t('traffic.throttledHint') }} ·
        </span>
        {{ resetHintText }}
      </div>
    </div>

    <!-- Area Chart -->
    <div class="flex">
      <!-- Y-axis ticks -->
      <div class="relative w-14 h-48 text-xs text-gray-400 dark:text-gray-500 pr-3 select-none text-right flex-shrink-0">
        <div
          v-for="tick in yAxisTicks"
          :key="tick.valGb"
          class="absolute right-3 transform -translate-y-1/2 leading-none whitespace-nowrap"
          :style="{ top: `${tick.yPercent}%` }"
        >
          {{ tick.label }}
        </div>
      </div>

      <!-- Chart area -->
      <div
        class="relative flex-1 h-48 cursor-crosshair select-none"
        @mousemove="handleMouseMove"
        @mouseleave="handleMouseLeave"
      >
        <!-- Horizontal grid lines -->
        <div class="absolute inset-0 flex flex-col justify-between pointer-events-none">
          <div
            v-for="tick in yAxisTicks"
            :key="tick.valGb"
            class="border-b border-gray-100 dark:border-gray-800/80 w-full"
          ></div>
        </div>

        <!-- SVG Line and Area Chart -->
        <svg
          class="w-full h-full overflow-visible"
          viewBox="0 0 1000 200"
          preserveAspectRatio="none"
        >
          <defs>
            <linearGradient id="trafficAreaGradient" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="#2563eb" stop-opacity="0.18" />
              <stop offset="100%" stop-color="#2563eb" stop-opacity="0.01" />
            </linearGradient>
          </defs>

          <!-- Gradient Area -->
          <path
            v-if="areaPath"
            :d="areaPath"
            fill="url(#trafficAreaGradient)"
          />

          <!-- Line -->
          <path
            v-if="linePath"
            :d="linePath"
            fill="none"
            stroke="#2563eb"
            stroke-width="2.5"
            stroke-linecap="round"
            stroke-linejoin="round"
          />

          <!-- Hover Guideline -->
          <line
            v-if="hoverPoint"
            :x1="hoverPoint.x"
            :y1="0"
            :x2="hoverPoint.x"
            :y2="200"
            stroke="#94a3b8"
            stroke-width="1.2"
            stroke-dasharray="3 3"
          />

          <!-- Hover Dot -->
          <circle
            v-if="hoverPoint"
            :cx="hoverPoint.x"
            :cy="hoverPoint.y"
            r="4.5"
            fill="#2563eb"
            stroke="#ffffff"
            stroke-width="2"
          />
        </svg>

        <!-- Hover Tooltip -->
        <div
          v-if="hoverItem"
          class="absolute z-20 pointer-events-none px-3 py-2 text-xs rounded-lg shadow-xl bg-gray-900 text-white dark:bg-gray-800 dark:text-gray-100 border border-gray-700 whitespace-nowrap transition-all duration-75"
          :style="tooltipStyle"
        >
          <div class="font-semibold text-gray-300 mb-1">{{ hoverItem.date }}</div>
          <div class="flex items-center gap-2">
            <span class="w-2 h-2 rounded-full bg-emerald-400"></span>
            <span>{{ t('traffic.download') }}: {{ hoverItem.rxFormatted }}</span>
          </div>
          <div class="flex items-center gap-2 mt-0.5">
            <span class="w-2 h-2 rounded-full bg-amber-400"></span>
            <span>{{ t('traffic.upload') }}: {{ hoverItem.txFormatted }}</span>
          </div>
          <div class="flex items-center gap-2 mt-1 pt-1 border-t border-gray-700 font-bold text-white">
            <span class="w-2 h-2 rounded-full bg-[#2563eb]"></span>
            <span>{{ t('traffic.total') }}: {{ hoverItem.totalFormatted }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- X-axis date labels -->
    <div class="flex">
      <div class="w-14 flex-shrink-0"></div>
      <div class="relative flex-1 h-5 mt-2 select-none">
        <div
          v-for="label in xAxisLabels"
          :key="label.index"
          class="absolute text-xs text-gray-400 dark:text-gray-500 whitespace-nowrap"
          :class="[
            label.index === 0 ? 'translate-x-0' : label.index === trafficHistory.length - 1 ? '-translate-x-full' : '-translate-x-1/2'
          ]"
          :style="{ left: `${trafficHistory.length === 1 ? 50 : (label.index / (trafficHistory.length - 1)) * 100}%` }"
        >
          {{ label.date }}
        </div>
      </div>
    </div>

    <!-- Bottom Footer Metrics -->
    <div class="flex flex-wrap items-center gap-6 sm:gap-8 pt-4 mt-2 border-t border-gray-100 dark:border-gray-800 text-xs sm:text-sm text-gray-600 dark:text-gray-400">
      <div class="flex items-center gap-2 font-medium text-gray-800 dark:text-gray-200">
        <span class="w-2.5 h-2.5 rounded-full bg-[#2563eb]"></span>
        <span>{{ t('traffic.dailyTotal') }}</span>
      </div>
      <div>
        <span>{{ t('traffic.peak') }} {{ peakText }}</span>
      </div>
      <div>
        <span>{{ t('traffic.dailyAverage') }} {{ dailyAvgText }}</span>
      </div>
    </div>
  </div>
</template>
