<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { TrafficHistoryItem } from '@/utils/traffic'

const props = withDefaults(
  defineProps<{
    history: TrafficHistoryItem[]
    loading?: boolean
    chartHeightClass?: string
    idPrefix?: string
    showFooterMetrics?: boolean
    emptyText?: string
  }>(),
  {
    loading: false,
    chartHeightClass: 'h-48',
    idPrefix: 'traffic-chart',
    showFooterMetrics: true
  }
)

const { t } = useI18n()

// 悬浮点索引
const hoverIndex = ref<number | null>(null)

// 渐变 ID
const gradientId = computed(() => `${props.idPrefix}-area-gradient`)

// 格式化字节数
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB'
  if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB'
  return (bytes / 1073741824).toFixed(1) + ' GB'
}

// 计算峰值数值（用于标尺上限）
const chartMaxValue = computed(() => {
  if (props.history.length === 0) return 0
  return Math.max(...props.history.map(h => Number(h.total) || 0))
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
  const len = props.history.length
  if (len === 0) return []
  const cGb = ceilingGb.value

  return props.history.map((item, idx) => {
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
  return props.history[hoverIndex.value] || null
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
  if (props.history.length === 0) return { formatted: '0 GB', date: '' }
  let max = props.history[0]
  for (const item of props.history) {
    if (Number(item.total) > Number(max.total)) {
      max = item
    }
  }
  const bytes = Number(max.total)
  const gb = bytes / (1024 * 1024 * 1024)
  const formatted = bytes === 0 ? '0 GB' : gb >= 0.1 ? `${gb.toFixed(1)} GB` : formatBytes(bytes)
  const date = max.date?.slice(5) || ''
  return { formatted, date }
})

const peakText = computed(() => {
  if (!peakInfo.value.date) return peakInfo.value.formatted
  return `${peakInfo.value.formatted}(${peakInfo.value.date})`
})

// 日均用量
const dailyAvgText = computed(() => {
  if (props.history.length === 0) return '0 GB'
  const sumBytes = props.history.reduce((acc, cur) => acc + (Number(cur.total) || 0), 0)
  const avgBytes = sumBytes / props.history.length
  const gb = avgBytes / (1024 * 1024 * 1024)
  if (gb >= 0.05) {
    return `${gb.toFixed(1)} GB`
  }
  return formatBytes(avgBytes)
})
</script>

<template>
  <div class="w-full">
    <!-- Loading 骨架屏 -->
    <div v-if="loading" class="animate-pulse space-y-4 py-2">
      <div class="flex items-end gap-2" :class="chartHeightClass">
        <div class="w-12 h-full flex flex-col justify-between py-1">
          <div class="h-2.5 bg-[var(--bg-secondary)] rounded w-8"></div>
          <div class="h-2.5 bg-[var(--bg-secondary)] rounded w-6"></div>
          <div class="h-2.5 bg-[var(--bg-secondary)] rounded w-4"></div>
        </div>
        <div class="flex-1 h-full rounded border border-[var(--border-color)] bg-[var(--bg-secondary)]"></div>
      </div>
    </div>

    <!-- 图表区域 -->
    <template v-else-if="history.length > 0">
      <div class="flex">
        <!-- Y 轴刻度 -->
        <div class="relative w-14 text-xs text-[var(--text-tertiary)] pr-3 select-none text-right flex-shrink-0" :class="chartHeightClass">
          <div
            v-for="tick in yAxisTicks"
            :key="tick.valGb"
            class="absolute right-3 transform -translate-y-1/2 leading-none whitespace-nowrap"
            :style="{ top: `${tick.yPercent}%` }"
          >
            {{ tick.label }}
          </div>
        </div>

        <!-- 绘图区域 -->
        <div
          class="relative flex-1 cursor-crosshair select-none"
          :class="chartHeightClass"
          @mousemove="handleMouseMove"
          @mouseleave="handleMouseLeave"
        >
          <!-- 水平网格线 -->
          <div class="absolute inset-0 flex flex-col justify-between pointer-events-none">
            <div
              v-for="tick in yAxisTicks"
              :key="tick.valGb"
              class="border-b border-[var(--border-color)] w-full"
            ></div>
          </div>

          <!-- SVG 折线与面积 -->
          <svg
            class="w-full h-full overflow-visible"
            viewBox="0 0 1000 200"
            preserveAspectRatio="none"
          >
            <defs>
              <linearGradient :id="gradientId" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stop-color="var(--chart-1)" />
                <stop offset="100%" stop-color="var(--bg-surface)" />
              </linearGradient>
            </defs>

            <!-- 面积填充 -->
            <path
              v-if="areaPath"
              :d="areaPath"
              :fill="`url(#${gradientId})`"
            />

            <!-- 折线 -->
            <path
              v-if="linePath"
              :d="linePath"
              fill="none"
              stroke="var(--accent)"
              stroke-width="2.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            />

            <!-- 悬浮参考线 -->
            <line
              v-if="hoverPoint"
              :x1="hoverPoint.x"
              :y1="0"
              :x2="hoverPoint.x"
              :y2="200"
              stroke="var(--border-strong)"
              stroke-width="1.2"
              stroke-dasharray="3 3"
            />

            <!-- 悬浮数据点 -->
            <circle
              v-if="hoverPoint"
              :cx="hoverPoint.x"
              :cy="hoverPoint.y"
              r="4.5"
              fill="var(--accent)"
              stroke="var(--bg-surface)"
              stroke-width="2"
            />
          </svg>

          <!-- 悬停气泡 Tooltip -->
          <div
            v-if="hoverItem"
            class="absolute z-20 pointer-events-none px-3 py-2 text-xs rounded-lg shadow-lg bg-[var(--bg-elevated)] text-[var(--text-primary)] border border-[var(--border-strong)] whitespace-nowrap transition-all duration-75"
            :style="tooltipStyle"
          >
            <div class="font-semibold text-[var(--text-secondary)] mb-1">{{ hoverItem.date }}</div>
            <div class="flex items-center gap-2">
              <span class="w-2 h-2 rounded-full bg-[var(--chart-2)]"></span>
              <span>{{ t('traffic.download') }}: {{ hoverItem.rxFormatted }}</span>
            </div>
            <div class="flex items-center gap-2 mt-0.5">
              <span class="w-2 h-2 rounded-full bg-[var(--chart-3)]"></span>
              <span>{{ t('traffic.upload') }}: {{ hoverItem.txFormatted }}</span>
            </div>
            <div class="flex items-center gap-2 mt-1 pt-1 border-t border-[var(--border-color)] font-bold text-[var(--text-primary)]">
              <span class="w-2 h-2 rounded-full bg-[var(--accent)]"></span>
              <span>{{ t('traffic.total') }}: {{ hoverItem.totalFormatted }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- X 轴日期刻度 -->
      <div class="flex">
        <div class="w-14 flex-shrink-0"></div>
        <div class="relative flex-1 h-5 mt-2 select-none">
          <div
            v-for="label in xAxisLabels"
            :key="label.index"
            class="absolute text-xs text-[var(--text-tertiary)] whitespace-nowrap"
            :class="[
              label.index === 0 ? 'translate-x-0' : label.index === history.length - 1 ? '-translate-x-full' : '-translate-x-1/2'
            ]"
            :style="{ left: `${history.length === 1 ? 50 : (label.index / (history.length - 1)) * 100}%` }"
          >
            {{ label.date }}
          </div>
        </div>
      </div>

      <!-- 底部指标统计 -->
      <div
        v-if="showFooterMetrics"
        class="flex flex-wrap items-center gap-6 sm:gap-8 pt-4 mt-2 border-t border-[var(--border-color)] text-xs sm:text-sm text-[var(--text-secondary)]"
      >
        <div class="flex items-center gap-2 font-medium text-[var(--text-primary)]">
          <span class="w-2.5 h-2.5 rounded-full bg-[var(--accent)]"></span>
          <span>{{ t('traffic.dailyTotal') }}</span>
        </div>
        <div>
          <span>{{ t('traffic.peak') }} {{ peakText }}</span>
        </div>
        <div>
          <span>{{ t('traffic.dailyAverage') }} {{ dailyAvgText }}</span>
        </div>
      </div>
    </template>

    <!-- 无数据状态 -->
    <div v-else class="text-center py-10 text-xs sm:text-sm text-[var(--text-tertiary)]">
      {{ emptyText || t('traffic.noHistoryData') }}
    </div>
  </div>
</template>
