<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import {
  PACKAGE_NONE_SENTINEL,
  type ImportPackageOption,
  type ImportPackagePlanOption
} from '@/api/host-transfer'
import { Input, Select } from '@/components/ui'

interface Props {
  packages: ImportPackageOption[]
  loading?: boolean
  loadFailed?: boolean
  instanceType?: 'container' | 'vm' | null
  disabled?: boolean
  packageChoice: string
  planChoice: string
  validDays: string
}

const props = withDefaults(defineProps<Props>(), {
  loading: false,
  loadFailed: false,
  instanceType: null,
  disabled: false
})

const emit = defineEmits<{
  (e: 'update:packageChoice', value: string): void
  (e: 'update:planChoice', value: string): void
  (e: 'update:validDays', value: string): void
  (e: 'retry'): void
}>()

const { t } = useI18n()
const themeStore = useThemeStore()

const filteredPackages = computed(() => {
  if (!props.instanceType) return props.packages
  return props.packages.filter(pkg => pkg.instanceType === props.instanceType)
})

const packageOptions = computed(() => {
  if (props.loading) {
    return [{ value: '', label: t('admin.hosts.importPackageLoading') }]
  }
  if (props.loadFailed && !filteredPackages.value.length) {
    return [{ value: '', label: t('admin.hosts.importPackageLoadFailed') }]
  }
  if (!filteredPackages.value.length) {
    return [{ value: PACKAGE_NONE_SENTINEL, label: t('admin.hosts.importPackageEmpty') }]
  }

  return [
    { value: '', label: t('admin.hosts.importPackagePlaceholder') },
    ...filteredPackages.value.map(pkg => {
      const tags: string[] = []
      if (pkg.boundToHost) tags.push(t('admin.hosts.importPackageBoundTag'))
      tags.push(pkg.instanceType === 'vm' ? 'VM' : 'LXC')
      if (!pkg.isActive) tags.push(t('admin.hosts.importPackageInactiveTag'))
      return {
        value: String(pkg.id),
        label: `${pkg.name} (${tags.join(' · ')})`
      }
    }),
    { value: PACKAGE_NONE_SENTINEL, label: t('admin.hosts.importPackageNoneOption') }
  ]
})

const selectedPackage = computed<ImportPackageOption | null>(() => {
  if (!props.packageChoice || props.packageChoice === PACKAGE_NONE_SENTINEL) return null
  const id = Number(props.packageChoice)
  return filteredPackages.value.find(pkg => pkg.id === id)
    ?? props.packages.find(pkg => pkg.id === id)
    ?? null
})

const planOptions = computed(() => {
  const pkg = selectedPackage.value
  if (!pkg) return []
  return [
    ...pkg.plans.map(plan => {
      const suffix = plan.isActive ? '' : ` (${t('admin.hosts.importPackageInactiveTag')})`
      const cycleText = t('admin.hosts.importPlanMonthsSuffix', { months: plan.billingCycle })
      return {
        value: String(plan.id),
        label: `${plan.name} · ¥${Number(plan.price).toFixed(2)} / ${cycleText}${suffix}`
      }
    }),
    { value: PACKAGE_NONE_SENTINEL, label: t('admin.hosts.importPlanNoneOption') }
  ]
})

const selectedPlan = computed<ImportPackagePlanOption | null>(() => {
  const pkg = selectedPackage.value
  if (!pkg || !props.planChoice || props.planChoice === PACKAGE_NONE_SENTINEL) return null
  const planId = Number(props.planChoice)
  return pkg.plans.find(p => p.id === planId) ?? null
})

const showMissingSelectionWarning = computed(() => {
  return !props.loading && !props.loadFailed && filteredPackages.value.length > 0 && props.packageChoice === ''
})

function handlePackageChange(nextValue: string) {
  emit('update:packageChoice', nextValue)
  if (!nextValue || nextValue === PACKAGE_NONE_SENTINEL) {
    emit('update:planChoice', PACKAGE_NONE_SENTINEL)
    return
  }
  const id = Number(nextValue)
  const pkg = filteredPackages.value.find(p => p.id === id) ?? props.packages.find(p => p.id === id)
  if (!pkg || !pkg.plans.length) {
    emit('update:planChoice', PACKAGE_NONE_SENTINEL)
    return
  }
  const preferredPlan = pkg.plans.find(p => p.isActive) ?? pkg.plans[0]
  emit('update:planChoice', preferredPlan ? String(preferredPlan.id) : PACKAGE_NONE_SENTINEL)
}
</script>

<template>
  <div class="space-y-3">
    <div class="space-y-1.5">
      <div class="flex items-center justify-between gap-2">
        <label class="block text-sm font-medium text-[var(--text-primary)]">
          {{ t('admin.hosts.importPackageLabel') }}
        </label>
        <button
          v-if="props.loadFailed"
          type="button"
          class="text-xs text-[var(--accent)] hover:underline cursor-pointer"
          @click="emit('retry')"
        >
          {{ t('common.retry') }}
        </button>
      </div>
      <Select
        :model-value="props.packageChoice"
        :options="packageOptions"
        :disabled="props.disabled || props.loading"
        @update:model-value="handlePackageChange(String($event))"
      />
      <p
        v-if="showMissingSelectionWarning"
        class="rounded-xl px-3 py-2 text-xs leading-relaxed"
        :class="themeStore.isDark ? 'bg-amber-500/10 text-amber-300' : 'bg-amber-50 text-amber-700'"
      >
        {{ t('admin.hosts.importPackageRequiredWarning') }}
      </p>
      <p v-else class="text-xs text-[var(--text-tertiary)]">
        {{ t('admin.hosts.importPackageHint') }}
      </p>
    </div>

    <div v-if="selectedPackage" class="space-y-1.5">
      <label class="block text-sm font-medium text-[var(--text-primary)]">
        {{ t('admin.hosts.importPlanLabel') }}
      </label>
      <Select
        :model-value="props.planChoice"
        :options="planOptions"
        :disabled="props.disabled"
        @update:model-value="emit('update:planChoice', String($event))"
      />
    </div>

    <div v-if="selectedPackage && selectedPlan" class="space-y-1.5">
      <label class="block text-sm font-medium text-[var(--text-primary)]">
        {{ t('admin.hosts.importValidDaysLabel') }}
      </label>
      <Input
        :model-value="props.validDays"
        type="number"
        min="1"
        max="36500"
        :disabled="props.disabled"
        :placeholder="t('admin.hosts.importValidDaysPlaceholder')"
        @update:model-value="emit('update:validDays', String($event ?? ''))"
      />
      <p class="text-xs text-[var(--text-tertiary)]">
        {{ t('admin.hosts.importValidDaysHint', { months: selectedPlan.billingCycle }) }}
      </p>
    </div>
  </div>
</template>
