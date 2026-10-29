<script setup lang="ts">
/**
 * 表格操作图标按钮（AGENTS.md §2.13 / THEME.md §5）
 * 扁平、无边框、无默认底色，hover 才浮出浅底；label 同时作为 hover hint 与无障碍名。
 */
defineProps<{ label: string; variant?: 'default' | 'danger'; disabled?: boolean }>()
</script>
<template>
  <button
    type="button"
    :class="['icon-btn', variant === 'danger' && 'icon-btn-danger']"
    :title="label"
    :aria-label="label"
    :disabled="disabled"
  >
    <slot />
  </button>
</template>
