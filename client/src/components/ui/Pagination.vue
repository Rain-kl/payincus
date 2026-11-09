<script setup lang="ts">
import { computed } from 'vue'
import { ChevronLeft, ChevronRight } from 'lucide-vue-next'

export interface PaginationProps {
  page?: number
  pageSize?: number
  total?: number
  showTotal?: boolean
  showSizeChanger?: boolean
  pageSizeOptions?: number[]
}

const props = withDefaults(defineProps<PaginationProps>(), {
  page: 1,
  pageSize: 10,
  total: 0,
  showTotal: true,
  showSizeChanger: false,
  pageSizeOptions: () => [10, 20, 50, 100]
})

const emit = defineEmits<{
  (e: 'update:page', page: number): void
  (e: 'update:pageSize', pageSize: number): void
  (e: 'change', payload: { page: number; pageSize: number }): void
}>()

const totalPages = computed(() => {
  return Math.max(1, Math.ceil(props.total / props.pageSize))
})

const setPage = (p: number) => {
  if (p < 1 || p > totalPages.value || p === props.page) return
  emit('update:page', p)
  emit('change', { page: p, pageSize: props.pageSize })
}

const handlePageSizeChange = (e: Event) => {
  const target = e.target as HTMLSelectElement
  const newSize = Number(target.value)
  emit('update:pageSize', newSize)
  emit('update:page', 1)
  emit('change', { page: 1, pageSize: newSize })
}

// Compute visible page list with ellipses
const pages = computed(() => {
  const current = props.page
  const total = totalPages.value
  const delta = 2

  if (total <= 7) {
    return Array.from({ length: total }, (_, i) => i + 1)
  }

  const range: (number | string)[] = []
  const left = Math.max(2, current - delta)
  const right = Math.min(total - 1, current + delta)

  range.push(1)

  if (left > 2) {
    range.push('...')
  }

  for (let i = left; i <= right; i++) {
    range.push(i)
  }

  if (right < total - 1) {
    range.push('...')
  }

  range.push(total)
  return range
})
</script>

<template>
  <div class="flex flex-wrap items-center justify-between gap-3 text-sm">
    <!-- Total / summary display -->
    <div v-if="showTotal" class="text-xs text-[var(--text-secondary)] select-none">
      <slot name="total" :total="total" :page="page" :total-pages="totalPages">
        共 {{ total }} 条记录 · 第 {{ page }} / {{ totalPages }} 页
      </slot>
    </div>

    <!-- Pager Buttons -->
    <nav class="flex items-center gap-1 select-none" aria-label="分页导航">
      <!-- Prev Button -->
      <button
        type="button"
        class="inline-flex items-center justify-center min-w-[36px] h-9 px-2.5 rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
        :class="page <= 1 ? 'cursor-not-allowed text-[var(--text-tertiary)] bg-[var(--bg-tertiary)] pointer-events-none' : ''"
        :disabled="page <= 1"
        title="上一页"
        aria-label="上一页"
        @click="setPage(page - 1)"
      >
        <ChevronLeft class="w-4 h-4 stroke-[2]" />
      </button>

      <!-- Page Numbers -->
      <template v-for="(item, index) in pages" :key="index">
        <span
          v-if="item === '...'"
          class="inline-flex items-center justify-center min-w-[32px] h-9 text-[var(--text-tertiary)] text-xs select-none"
        >
          •••
        </span>
        <button
          v-else
          type="button"
          class="inline-flex items-center justify-center min-w-[36px] h-9 px-3 rounded-full text-xs font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="item === page
            ? 'bg-[var(--accent)] text-white shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)]'"
          :aria-current="item === page ? 'page' : undefined"
          @click="setPage(item as number)"
        >
          {{ item }}
        </button>
      </template>

      <!-- Next Button -->
      <button
        type="button"
        class="inline-flex items-center justify-center min-w-[36px] h-9 px-2.5 rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
        :class="page >= totalPages ? 'cursor-not-allowed text-[var(--text-tertiary)] bg-[var(--bg-tertiary)] pointer-events-none' : ''"
        :disabled="page >= totalPages"
        title="下一页"
        aria-label="下一页"
        @click="setPage(page + 1)"
      >
        <ChevronRight class="w-4 h-4 stroke-[2]" />
      </button>
    </nav>

    <!-- Optional Page Size Changer -->
    <div v-if="showSizeChanger" class="flex items-center gap-1.5 text-xs text-[var(--text-secondary)]">
      <span>每页</span>
      <select
        :value="pageSize"
        class="h-8 px-2 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)] text-xs cursor-pointer"
        @change="handlePageSizeChange"
      >
        <option v-for="sizeOpt in pageSizeOptions" :key="sizeOpt" :value="sizeOpt">
          {{ sizeOpt }} 条
        </option>
      </select>
    </div>
  </div>
</template>
