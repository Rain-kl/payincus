<script setup lang="ts" generic="T extends Record<string, any> = Record<string, any>">
import EmptyState from './EmptyState.vue'

export interface TableColumn<RowType = any> {
  key: string
  title?: string
  width?: string
  align?: 'left' | 'center' | 'right'
  class?: string
  headerClass?: string
  render?: (value: any, row: RowType, index: number) => any
}

export interface TableProps<RowType = any> {
  columns?: TableColumn<RowType>[]
  data?: RowType[]
  rowKey?: string | ((row: RowType) => string | number)
  loading?: boolean
  loadingRows?: number
  emptyText?: string
  emptyDescription?: string
  fixed?: boolean
  stripe?: boolean
  hover?: boolean
}

const props = withDefaults(defineProps<TableProps<T>>(), {
  columns: () => [],
  data: () => [],
  rowKey: 'id',
  loading: false,
  loadingRows: 5,
  emptyText: '暂无数据',
  emptyDescription: '当前列表暂无任何记录',
  fixed: true,
  stripe: false,
  hover: true
})

const getRowKey = (row: T, index: number): string | number => {
  if (typeof props.rowKey === 'function') {
    return props.rowKey(row)
  }
  return row[props.rowKey] ?? index
}

const getAlignClass = (align?: 'left' | 'center' | 'right') => {
  switch (align) {
    case 'center':
      return 'text-center'
    case 'right':
      return 'text-right'
    case 'left':
    default:
      return 'text-left'
  }
}
</script>

<template>
  <div class="w-full">
    <!-- Responsive Mobile Cards View (when $slots.mobile is provided) -->
    <div v-if="$slots.mobile" class="block md:hidden space-y-3">
      <div v-if="loading" class="space-y-3 animate-pulse">
        <div
          v-for="i in loadingRows"
          :key="i"
          class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-2"
        >
          <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-1/3"></div>
          <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-2/3"></div>
          <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-1/2"></div>
        </div>
      </div>
      <template v-else-if="data && data.length > 0">
        <div v-for="(row, index) in data" :key="getRowKey(row, index)">
          <slot name="mobile" :row="row" :index="index" />
        </div>
      </template>
      <div v-else class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)]">
        <slot name="empty">
          <EmptyState :title="emptyText" :description="emptyDescription" />
        </slot>
      </div>
    </div>

    <!-- Desktop / Standard Table Container -->
    <div
      class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)]"
      :class="[
        $slots.mobile ? 'hidden md:block' : 'block',
        fixed ? 'overflow-hidden' : 'overflow-x-auto'
      ]"
    >
      <table
        class="w-full border-collapse"
        :class="[fixed ? 'table-fixed' : '']"
      >
        <!-- Colgroup for width specification -->
        <colgroup v-if="columns && columns.length > 0">
          <col
            v-for="col in columns"
            :key="col.key"
            :style="col.width ? { width: col.width } : undefined"
          />
        </colgroup>

        <!-- Table Header -->
        <thead class="bg-[var(--bg-tertiary)] border-b border-[var(--border-color)]">
          <slot name="header">
            <tr>
              <th
                v-for="col in columns"
                :key="col.key"
                scope="col"
                class="px-4 py-3 text-xs font-semibold tracking-wider text-[var(--text-secondary)] uppercase select-none"
                :class="[getAlignClass(col.align), col.headerClass || '']"
                :style="col.width ? { width: col.width } : undefined"
              >
                <slot :name="`header-${col.key}`" :column="col">
                  {{ col.title ?? col.key }}
                </slot>
              </th>
            </tr>
          </slot>
        </thead>

        <!-- Table Body -->
        <tbody class="divide-y divide-[var(--border-color)]">
          <!-- Loading State -->
          <template v-if="loading">
            <slot name="loading">
              <tr v-for="rowIndex in loadingRows" :key="`loading-${rowIndex}`" class="animate-pulse">
                <td
                  v-for="col in (columns.length > 0 ? columns : 4)"
                  :key="typeof col === 'object' ? col.key : col"
                  class="px-4 py-4"
                >
                  <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-3/4"></div>
                </td>
              </tr>
            </slot>
          </template>

          <!-- Empty State -->
          <template v-else-if="!data || data.length === 0">
            <tr>
              <td
                :colspan="columns && columns.length > 0 ? columns.length : 1"
                class="px-4 py-12 text-center"
              >
                <slot name="empty">
                  <EmptyState :title="emptyText" :description="emptyDescription" />
                </slot>
              </td>
            </tr>
          </template>

          <!-- Data Rows -->
          <template v-else>
            <tr
              v-for="(row, index) in data"
              :key="getRowKey(row, index)"
              class="transition-colors duration-150"
              :class="[
                hover ? 'hover:bg-[var(--bg-surface-soft)]' : '',
                stripe && index % 2 === 1 ? 'bg-[var(--bg-surface-soft)]' : ''
              ]"
            >
              <td
                v-for="col in columns"
                :key="col.key"
                class="px-4 py-3.5 text-sm text-[var(--text-primary)] align-middle break-words"
                :class="[getAlignClass(col.align), col.class || '']"
              >
                <slot
                  :name="`cell-${col.key}`"
                  :row="row"
                  :column="col"
                  :index="index"
                  :value="row[col.key]"
                >
                  {{ row[col.key] }}
                </slot>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <!-- Bottom Pagination / Footer -->
    <div v-if="$slots.pagination || $slots.footer" class="mt-4 flex flex-col sm:flex-row items-center justify-between gap-4">
      <div v-if="$slots.footer" class="w-full sm:w-auto">
        <slot name="footer" />
      </div>
      <div v-if="$slots.pagination" class="w-full sm:w-auto flex justify-end">
        <slot name="pagination" />
      </div>
    </div>
  </div>
</template>
