<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Badge } from '@/components/ui'
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
const badgeVariant = computed(() => {
  if (state.value === 'applied') return 'success'
  if (state.value === 'error') return 'error'
  if (state.value === 'pending') return 'warning'
  return 'default'
})
</script>
<template>
  <div class="space-y-2 text-sm" role="status">
    <div class="flex flex-wrap items-center gap-2.5">
      <Badge :variant="badgeVariant" dot :pulse="state === 'pending'">
        {{ t(`egress.states.${state}`) }}
      </Badge>
      <span v-if="policy?.status.managedNetworks?.length" class="text-xs text-[var(--text-secondary)]">
        {{ t('egress.managedNetworks') }}:
        <span class="font-mono text-[var(--text-primary)]">{{ policy.status.managedNetworks.join(', ') }}</span>
      </span>
    </div>
    <p v-if="state === 'error'" class="text-xs text-[var(--error)] leading-relaxed">
      {{ t('egress.applyFailed') }}: {{ policy?.status.error }}
    </p>
    <p v-if="state === 'error' && policy?.status.errorStage" class="text-xs text-[var(--error)]">
      {{ t('egress.errorStage') }}: {{ t(`egress.errorStages.${policy.status.errorStage}`) }}
    </p>
    <p v-if="state === 'error' && policy?.status.errorDetail" class="text-xs text-[var(--error)]">
      {{ t(`egress.failureReasons.${policy.status.errorDetail}`) }}
    </p>
    <p v-if="policy?.status.routedFallback" class="text-xs text-[var(--text-tertiary)] leading-relaxed">
      {{ t('egress.routedFallback') }}
    </p>
  </div>
</template>
