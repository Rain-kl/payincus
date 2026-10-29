<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { NetworkPolicy } from '@/api/egress-types'
const props = defineProps<{ policy: NetworkPolicy | null }>()
const { t } = useI18n()
const state = computed(() => {
  const p = props.policy
  if (!p || !p.revision) return 'idle'
  if (!p.reportedAt || Date.now() - Date.parse(p.reportedAt) > 180_000) return 'pending'
  if (p.status.state === 'error' || p.status.state === 'unsupported') return 'error'
  return p.revision === p.appliedRevision && p.status.state === 'applied' ? 'applied' : 'pending'
})
</script>
<template>
  <div class="space-y-1 text-sm" role="status">
    <span class="badge" :class="state === 'applied' ? 'badge-success' : state === 'error' ? 'badge-error' : 'badge-warning'">{{ t(`egress.states.${state}`) }}</span>
    <p v-if="state === 'error'" class="text-error">{{ t('egress.applyFailed') }}: {{ policy?.status.error }}</p>
    <p v-if="state === 'error' && policy?.status.errorStage" class="text-error">{{ t('egress.errorStage') }}: {{ t(`egress.errorStages.${policy.status.errorStage}`) }}</p>
    <p v-if="state === 'error' && policy?.status.errorDetail" class="text-error">{{ t(`egress.failureReasons.${policy.status.errorDetail}`) }}</p>
    <p v-if="policy?.status.routedFallback" class="text-themed-muted">{{ t('egress.routedFallback') }}</p>
    <p v-if="policy?.status.managedNetworks?.length" class="text-themed-muted">{{ t('egress.managedNetworks') }}: {{ policy.status.managedNetworks.join(', ') }}</p>
  </div>
</template>
