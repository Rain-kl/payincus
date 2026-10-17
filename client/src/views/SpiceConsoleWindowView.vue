<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import SpiceConsole from '@/components/instance/SpiceConsole.vue'

const route = useRoute()
const instanceId = computed(() => {
  const value = String(route.params.id || '')
  const id = Number(value)
  return /^[1-9]\d*$/.test(value) && Number.isSafeInteger(id) ? id : null
})
const instanceName = computed(() => typeof route.query.name === 'string' ? route.query.name : '')
</script>

<template>
  <SpiceConsole :visible="true" :instance-id="instanceId" :instance-name="instanceName" standalone />
</template>
