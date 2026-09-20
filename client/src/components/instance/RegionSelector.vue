<script setup lang="ts">
/**
 * 地区选择器组件
 * 用于创建实例时选择国家/地区（横向卡片单选风格）
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import FlagIcon from '@/components/FlagIcon.vue'
import { getLocalizedCountryName } from '@/utils/countryDisplay'

export interface Region {
  code: string      // 国家代码，如 'hk', 'de'
  name: string      // 国家名称
  packageCount: number  // 套餐数量
  packageIds: number[]  // 该地区包含的套餐 ID 列表
}

interface Props {
  regions: Region[]
  selectedRegion: string | null  // null 表示"全部"
  loading?: boolean
  title?: string
  packageCountLabel?: string
  packageCountLabelKey?: string
  stepNumber?: number
}

const props = withDefaults(defineProps<Props>(), {
  loading: false,
  title: undefined,
  packageCountLabel: undefined,
  packageCountLabelKey: undefined,
  stepNumber: 1
})

const emit = defineEmits<{
  select: [region: string | null]
}>()

const { t, locale } = useI18n()
const themeStore = useThemeStore()

// 计算总套餐数
const totalPackageCount = computed(() => {
  return props.regions.reduce((sum, r) => sum + r.packageCount, 0)
})

// 是否选中"全部"
const isAllSelected = computed(() => props.selectedRegion === null)

// 选择地区
function selectRegion(code: string | null) {
  emit('select', code)
}

function getRegionLabel(code: string): string {
  return getLocalizedCountryName(code, locale.value, (key, fallback) => t(key, fallback))
}

function getPackageCountLabel(count: number): string {
  if (props.packageCountLabelKey) {
    return t(props.packageCountLabelKey, { count })
  }

  if (props.packageCountLabel) {
    return props.packageCountLabel.replace('{count}', String(count))
  }

  return t('instance.selector.packageCount', { count })
}
</script>

<template>
  <div class="card p-5">
    <!-- 标题栏 -->
    <div class="flex items-center gap-2 mb-4">
      <span
        class="w-6 h-6 rounded-full text-sm font-bold flex items-center justify-center"
        :class="themeStore.isDark ? 'bg-white text-gray-900' : 'bg-gray-900 text-white'"
      >{{ props.stepNumber }}</span>
      <h2
        class="text-sm font-medium"
        :class="themeStore.isDark ? 'text-gray-300' : 'text-gray-700'"
      >
        {{ props.title || t('instance.selector.selectRegion') }}
      </h2>
    </div>

    <!-- 加载状态 -->
    <div v-if="loading" class="flex items-center justify-center py-8">
      <svg class="w-6 h-6 animate-spin text-gray-500" fill="none" viewBox="0 0 24 24">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
      </svg>
    </div>

    <!-- 无数据 -->
    <div v-else-if="regions.length === 0" class="text-center py-8 text-themed-muted text-sm">
      {{ t('instance.selector.noRegions') }}
    </div>

    <!-- 区域横向长方形卡片网格 -->
    <div
      v-else
      class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-2.5"
      role="radiogroup"
    >
      <!-- "全部"选项 -->
      <button
        type="button"
        role="radio"
        :aria-checked="isAllSelected"
        :class="[
          'group relative flex items-center gap-3 px-3.5 py-2.5 rounded-xl border text-left transition-all duration-150 cursor-pointer select-none',
          isAllSelected
            ? 'border-primary-500 dark:border-primary-400 bg-primary-500/10 dark:bg-primary-500/15 ring-1 ring-primary-500/40 dark:ring-primary-400/40'
            : 'border-themed bg-themed-surface hover:border-themed-strong hover:bg-themed-secondary/50'
        ]"
        @click="selectRegion(null)"
      >
        <!-- 单选框圆点 -->
        <span
          class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0 transition-colors"
          :class="isAllSelected
            ? 'border-primary-500 dark:border-primary-400'
            : 'border-themed-strong bg-transparent group-hover:border-primary-500/60'"
        >
          <span
            v-if="isAllSelected"
            class="w-2 h-2 rounded-full bg-primary-500 dark:bg-primary-400"
          ></span>
        </span>

        <!-- 图标容器 -->
        <div class="w-9 h-7 rounded overflow-hidden flex items-center justify-center shrink-0 border border-themed bg-themed-secondary shadow-sm">
          <svg class="w-4 h-4 text-primary-500 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3.055 11H5a2 2 0 012 2v1a2 2 0 002 2 2 2 0 012 2v2.945M8 3.935V5.5A2.5 2.5 0 0010.5 8h.5a2 2 0 012 2 2 2 0 104 0 2 2 0 012-2h1.064M15 20.488V18a2 2 0 012-2h3.064M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
        </div>

        <!-- 文本信息 -->
        <div class="min-w-0 flex-1 flex flex-col justify-center">
          <div
            class="text-sm font-semibold truncate leading-snug"
            :class="isAllSelected ? 'text-primary-600 dark:text-primary-300' : 'text-themed-primary'"
          >
            {{ t('instance.selector.allRegions') }}
          </div>
          <div class="text-xs text-themed-muted truncate leading-normal">
            ALL · {{ getPackageCountLabel(totalPackageCount) }}
          </div>
        </div>
      </button>

      <!-- 各国家/地区卡片 -->
      <button
        v-for="region in regions"
        :key="region.code"
        type="button"
        role="radio"
        :aria-checked="selectedRegion === region.code"
        :class="[
          'group relative flex items-center gap-3 px-3.5 py-2.5 rounded-xl border text-left transition-all duration-150 cursor-pointer select-none',
          selectedRegion === region.code
            ? 'border-primary-500 dark:border-primary-400 bg-primary-500/10 dark:bg-primary-500/15 ring-1 ring-primary-500/40 dark:ring-primary-400/40'
            : 'border-themed bg-themed-surface hover:border-themed-strong hover:bg-themed-secondary/50'
        ]"
        @click="selectRegion(region.code)"
      >
        <!-- 单选框圆点 -->
        <span
          class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0 transition-colors"
          :class="selectedRegion === region.code
            ? 'border-primary-500 dark:border-primary-400'
            : 'border-themed-strong bg-transparent group-hover:border-primary-500/60'"
        >
          <span
            v-if="selectedRegion === region.code"
            class="w-2 h-2 rounded-full bg-primary-500 dark:bg-primary-400"
          ></span>
        </span>

        <!-- 国旗容器 -->
        <div class="w-9 h-7 rounded overflow-hidden flex items-center justify-center shrink-0 border border-themed bg-themed-secondary shadow-sm">
          <FlagIcon :code="region.code" size="lg" class="scale-105" />
        </div>

        <!-- 文本信息 -->
        <div class="min-w-0 flex-1 flex flex-col justify-center">
          <div
            class="text-sm font-semibold truncate leading-snug"
            :class="selectedRegion === region.code ? 'text-primary-600 dark:text-primary-300' : 'text-themed-primary'"
          >
            {{ getRegionLabel(region.code) }}
          </div>
          <div class="text-xs text-themed-muted truncate leading-normal">
            {{ region.code.toUpperCase() }} · {{ getPackageCountLabel(region.packageCount) }}
          </div>
        </div>
      </button>
    </div>
  </div>
</template>
