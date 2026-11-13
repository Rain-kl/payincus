<script setup lang="ts">
import { ref, computed, watch, onUnmounted, toRaw } from 'vue'
import { useI18n } from 'vue-i18n'
import { Shield, Plus, RotateCw, Pencil, Trash2, AlertCircle } from 'lucide-vue-next'
import api from '@/api'
import { Badge, Button, Card, EmptyState, Input, Select } from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'
import NetworkPolicyStatus from './NetworkPolicyStatus.vue'
import type { FirewallPolicy, FirewallRule, NetworkPolicy } from '@/api/egress-types'

const props = defineProps<{ hostId: number }>()
const { t } = useI18n()
const policy = ref<NetworkPolicy | null>(null)
const form = ref<FirewallPolicy | null>(null)
const error = ref('')
const busy = ref(false)
const dirty = ref(false)
const showRule = ref(false)
const editing = ref(-1)
const formRevision = ref(0)
const emptyRule = (): FirewallRule => ({ direction: 'egress', action: 'drop', protocol: 'tcp', source: '', destination: '', sourcePort: '', destinationPort: '', description: '', enabled: true })
const draft = ref(emptyRule())
let generation = 0

const defaultActionOptions = computed(() => [
  { value: 'allow', label: t('egress.allow') },
  { value: 'drop', label: t('egress.drop') },
  { value: 'reject', label: t('egress.reject') }
])
const directionOptions = computed(() => [
  { value: 'egress', label: t('egress.egress') },
  { value: 'ingress', label: t('egress.ingress') }
])
const ruleActionOptions = computed(() => [
  { value: 'drop', label: t('egress.drop') },
  { value: 'reject', label: t('egress.reject') },
  { value: 'allow', label: t('egress.allow') }
])
const protocolOptions = computed(() => [
  { value: '', label: t('egress.any') },
  { value: 'tcp', label: 'TCP' },
  { value: 'udp', label: 'UDP' },
  { value: 'icmp4', label: 'ICMPv4' },
  { value: 'icmp6', label: 'ICMPv6' }
])

function ruleActionVariant(action: string): 'success' | 'warning' | 'error' {
  if (action === 'allow') return 'success'
  if (action === 'reject') return 'warning'
  return 'error'
}

async function load(reset = false) {
  const current = generation
  try {
    const response = await api.network.policy(props.hostId)
    if (current !== generation) return
    policy.value = response.policy
    if (!form.value || reset) {
      form.value = structuredClone(response.policy.firewall)
      formRevision.value = response.policy.revision
      dirty.value = false
    }
  } catch (e: any) {
    if (current === generation) error.value = e.message
  }
}

function edit(index = -1) {
  editing.value = index
  draft.value = index < 0 ? emptyRule() : structuredClone(toRaw(form.value!.rules[index]))
  showRule.value = true
}

function acceptRule() {
  if (!form.value) return
  if (editing.value < 0) form.value.rules.push(structuredClone(toRaw(draft.value)))
  else form.value.rules[editing.value] = structuredClone(toRaw(draft.value))
  showRule.value = false
  dirty.value = true
}

function remove(index: number) {
  form.value?.rules.splice(index, 1)
  dirty.value = true
}

async function save() {
  if (!form.value) return
  busy.value = true
  error.value = ''
  try {
    await api.network.updateFirewall(props.hostId, formRevision.value, form.value)
    await load(true)
  } catch (e: any) {
    error.value = e.message
    await load(Number.isSafeInteger(e.revision))
  } finally {
    busy.value = false
  }
}

watch(() => props.hostId, () => {
  generation++
  policy.value = null
  form.value = null
  error.value = ''
  showRule.value = false
  void load()
}, { immediate: true })

const timer = setInterval(() => { void load() }, 15_000)
onUnmounted(() => { generation++; clearInterval(timer) })
</script>

<template>
  <section class="space-y-6">
    <!-- Header + Sync Status -->
    <Card padding="md">
      <template #header>
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between w-full">
          <div class="flex items-start gap-3">
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--accent)]">
              <Shield class="w-5 h-5 stroke-[1.8]" />
            </div>
            <div class="min-w-0">
              <h2 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                {{ t('egress.firewall') }}
              </h2>
              <p class="mt-0.5 text-sm text-[var(--text-secondary)] leading-relaxed">
                {{ t('egress.firewallHint') }}
              </p>
            </div>
          </div>
          <Button variant="secondary" size="sm" :disabled="busy" @click="load(true)">
            <RotateCw class="w-4 h-4" :class="{ 'animate-spin': busy }" />
            {{ t('egress.reload') }}
          </Button>
        </div>
      </template>

      <NetworkPolicyStatus :policy="policy" />
    </Card>

    <!-- Error Feedback -->
    <div
      v-if="error"
      role="alert"
      class="flex items-start gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--error)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] px-4 py-3 text-sm text-[var(--error)]"
    >
      <AlertCircle class="w-4 h-4 mt-0.5 shrink-0" />
      <span class="break-words">{{ error }}</span>
    </div>

    <!-- Loading Skeleton -->
    <Card v-if="!form && !error" padding="md" loading>
      <div class="space-y-4">
        <div class="h-11 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="h-11 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
          <div class="h-11 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
        </div>
        <div class="h-24 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse" />
      </div>
    </Card>

    <!-- Policy & Rules Form -->
    <Card v-if="form" padding="md">
      <div class="space-y-6" @input="dirty = true" @change="dirty = true">
        <!-- Enable Toggle -->
        <label class="flex items-center justify-between gap-4 min-h-[44px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3 cursor-pointer transition-colors hover:border-[var(--border-hover)]">
          <span class="text-sm font-medium text-[var(--text-primary)] select-none">{{ t('egress.enableFirewall') }}</span>
          <input
            v-model="form.enabled"
            type="checkbox"
            class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
          />
        </label>

        <!-- Default Actions -->
        <div class="grid gap-4 sm:grid-cols-2">
          <Select
            v-model="form.ingressAction"
            :label="t('egress.defaultIngress')"
            :options="defaultActionOptions"
          />
          <Select
            v-model="form.egressAction"
            :label="t('egress.defaultEgress')"
            :options="defaultActionOptions"
          />
        </div>

        <p class="text-xs text-[var(--text-tertiary)] leading-relaxed">
          {{ t('egress.aclOrder') }}
        </p>

        <!-- Rules Section -->
        <div class="space-y-3">
          <div class="flex items-center justify-between gap-3">
            <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
              {{ t('egress.firewall') }} ({{ form.rules.length }}/200)
            </span>
            <Button variant="secondary" size="sm" :disabled="form.rules.length >= 200" @click="edit()">
              <Plus class="w-4 h-4" />
              {{ t('egress.addRule') }}
            </Button>
          </div>

          <EmptyState
            v-if="form.rules.length === 0"
            :title="t('egress.addRule')"
            :description="t('egress.aclOrder')"
          >
            <template #icon>
              <Shield class="w-7 h-7 stroke-[1.5]" />
            </template>
            <template #action>
              <Button variant="secondary" size="sm" @click="edit()">
                <Plus class="w-4 h-4" />
                {{ t('egress.addRule') }}
              </Button>
            </template>
          </EmptyState>

          <div v-else class="space-y-2.5">
            <article
              v-for="(rule, index) in form.rules"
              :key="index"
              class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3.5 transition-all duration-200 hover:border-[var(--border-hover)]"
              :class="{ 'opacity-60': !rule.enabled }"
            >
              <div class="flex items-start gap-3 min-w-0 flex-1">
                <label class="flex items-center justify-center min-h-[44px] min-w-[24px] sm:min-h-[24px] cursor-pointer pt-0.5">
                  <input
                    v-model="rule.enabled"
                    type="checkbox"
                    :aria-label="t('egress.enableRule')"
                    class="h-4 w-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] cursor-pointer"
                  />
                </label>
                <div class="min-w-0 flex-1 space-y-1.5">
                  <div class="flex flex-wrap items-center gap-1.5">
                    <Badge size="sm">{{ t(`egress.${rule.direction}`) }}</Badge>
                    <Badge :variant="ruleActionVariant(rule.action)" size="sm">{{ t(`egress.${rule.action}`) }}</Badge>
                    <Badge variant="info" size="sm">{{ rule.protocol ? rule.protocol.toUpperCase() : t('egress.any') }}</Badge>
                  </div>
                  <p class="break-all font-mono text-xs text-[var(--text-primary)] tabular-nums">
                    {{ rule.source || '*' }}:{{ rule.sourcePort || '*' }} → {{ rule.destination || '*' }}:{{ rule.destinationPort || '*' }}
                  </p>
                  <p v-if="rule.description" class="text-xs text-[var(--text-secondary)] break-words">
                    {{ rule.description }}
                  </p>
                </div>
              </div>

              <div class="flex items-center gap-1.5 shrink-0 self-end sm:self-center">
                <Button variant="ghost" size="sm" @click="edit(index)">
                  <Pencil class="w-3.5 h-3.5" />
                  {{ t('egress.editRule') }}
                </Button>
                <Button variant="ghost" size="sm" class="text-[var(--error)]" @click="remove(index)">
                  <Trash2 class="w-3.5 h-3.5" />
                  {{ t('common.delete') }}
                </Button>
              </div>
            </article>
          </div>
        </div>

        <!-- Save Bar -->
        <div class="flex flex-wrap items-center justify-end gap-3 pt-4 border-t border-[var(--border-color)]">
          <Button variant="secondary" :disabled="busy" @click="load(true)">
            {{ t('egress.reload') }}
          </Button>
          <Button variant="primary" :loading="busy" :disabled="busy || !dirty" @click="save">
            {{ t(busy ? 'egress.applying' : 'egress.saveFirewall') }}
          </Button>
        </div>
      </div>
    </Card>

    <!-- Rule Drawer Modal -->
    <DrawerModal :show="showRule" :title="editing < 0 ? t('egress.addRule') : t('egress.editRule')" @close="showRule = false">
      <form class="space-y-4" @submit.prevent="acceptRule">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Select v-model="draft.direction" :label="t('egress.direction')" :options="directionOptions" />
          <Select v-model="draft.action" :label="t('egress.action')" :options="ruleActionOptions" />
        </div>
        <Select v-model="draft.protocol" :label="t('egress.protocol')" :options="protocolOptions" />
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Input v-model="draft.source" :label="t('egress.source')" placeholder="192.0.2.0/24,198.51.100.1" class="font-mono" />
          <Input v-model="draft.destination" :label="t('egress.destination')" placeholder="203.0.113.0/24" class="font-mono" />
        </div>
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Input v-model="draft.sourcePort" :label="t('egress.sourcePort')" placeholder="1000-2000" class="font-mono" />
          <Input v-model="draft.destinationPort" :label="t('egress.destinationPort')" placeholder="25,465,587" class="font-mono" />
        </div>
        <Input v-model="draft.description" :label="t('egress.description')" :maxlength="200" />
        <div class="flex items-center justify-end gap-3 pt-4 border-t border-[var(--border-color)]">
          <Button type="button" variant="secondary" @click="showRule = false">{{ t('common.cancel') }}</Button>
          <Button type="submit" variant="primary">{{ t('common.confirm') }}</Button>
        </div>
      </form>
    </DrawerModal>
  </section>
</template>
