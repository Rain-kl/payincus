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
    <!-- Top Page Action Header -->
    <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div class="flex items-start gap-3.5 min-w-0">
        <div class="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] text-[var(--accent)]">
          <Shield class="w-5 h-5 stroke-[1.8]" />
        </div>
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-2.5">
            <h2 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('egress.firewall') }}
            </h2>
            <Badge v-if="dirty" variant="warning" size="sm" dot>
              {{ t('common.unsavedChanges', '未保存') }}
            </Badge>
          </div>
          <p class="mt-1 text-sm text-[var(--text-secondary)] leading-relaxed max-w-2xl">
            {{ t('egress.firewallHint') }}
          </p>
        </div>
      </div>

      <div v-if="form" class="flex items-center gap-2.5 shrink-0 self-start sm:self-center">
        <Button variant="secondary" size="sm" :disabled="busy" @click="load(true)">
          <RotateCw class="w-4 h-4" :class="{ 'animate-spin': busy }" />
          {{ t('egress.reload') }}
        </Button>
        <Button variant="primary" size="sm" :loading="busy" :disabled="busy || !dirty" @click="save">
          {{ t(busy ? 'egress.applying' : 'egress.saveFirewall') }}
        </Button>
      </div>
    </div>

    <!-- Error Feedback -->
    <div
      v-if="error"
      role="alert"
      class="flex items-start gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--error)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] px-4 py-3.5 text-sm text-[var(--error)]"
    >
      <AlertCircle class="w-4 h-4 mt-0.5 shrink-0" />
      <span class="break-words">{{ error }}</span>
    </div>

    <!-- Loading Skeleton -->
    <div v-if="!form && !error" class="grid grid-cols-1 lg:grid-cols-12 gap-6">
      <Card padding="md" class="lg:col-span-4" loading>
        <div class="space-y-4">
          <div class="h-12 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
          <div class="h-11 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
          <div class="h-11 rounded-xl bg-[var(--bg-surface-soft)] animate-pulse" />
        </div>
      </Card>
      <Card padding="md" class="lg:col-span-8" loading>
        <div class="space-y-3">
          <div class="h-16 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse" />
          <div class="h-16 rounded-2xl bg-[var(--bg-surface-soft)] animate-pulse" />
        </div>
      </Card>
    </div>

    <!-- 2-Column Policy Layout -->
    <div v-if="form" class="grid grid-cols-1 lg:grid-cols-12 gap-6 items-start" @input="dirty = true" @change="dirty = true">
      <!-- Left Sidebar: Default Policy & Sync Status -->
      <div class="lg:col-span-4 space-y-5">
        <Card padding="md">
          <div class="space-y-5">
            <!-- Apple-style Switch Row -->
            <label class="flex items-center justify-between gap-4 min-h-[44px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3 cursor-pointer transition-colors hover:border-[var(--border-hover)]">
              <span class="text-sm font-medium text-[var(--text-primary)] select-none">
                {{ t('egress.enableFirewall') }}
              </span>
              <span class="relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors duration-200" :class="form.enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border border-[var(--border-color)]'">
                <input v-model="form.enabled" type="checkbox" class="sr-only" />
                <span
                  class="inline-block h-4 w-4 transform rounded-full bg-white shadow-sm transition-transform duration-200"
                  :class="form.enabled ? 'translate-x-6' : 'translate-x-1'"
                />
              </span>
            </label>

            <div class="space-y-4">
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

            <div class="rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] p-3.5 text-xs text-[var(--text-secondary)] leading-relaxed">
              {{ t('egress.aclOrder') }}
            </div>

            <div class="pt-3 border-t border-[var(--border-color)]">
              <NetworkPolicyStatus :policy="policy" />
            </div>
          </div>
        </Card>
      </div>

      <!-- Right Main Area: Rules List -->
      <div class="lg:col-span-8">
        <Card padding="none">
          <template #header>
            <div class="flex items-center justify-between gap-3 w-full">
              <div class="flex items-center gap-2.5 min-w-0">
                <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ t('egress.firewall') }}
                </h3>
                <Badge size="sm" class="font-mono tabular-nums">
                  {{ form.rules.length }} / 200
                </Badge>
              </div>
              <Button variant="primary" size="sm" :disabled="form.rules.length >= 200" @click="edit()">
                <Plus class="w-4 h-4" />
                {{ t('egress.addRule') }}
              </Button>
            </div>
          </template>

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

          <div v-else class="divide-y divide-[var(--border-color)]">
            <article
              v-for="(rule, index) in form.rules"
              :key="index"
              class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between px-5 py-4 transition-colors duration-150 hover:bg-[var(--bg-surface-soft)]/70"
              :class="{ 'opacity-55': !rule.enabled }"
            >
              <div class="flex items-start gap-3.5 min-w-0 flex-1">
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
                    <span class="font-mono text-[11px] text-[var(--text-tertiary)] mr-1">#{{ index + 1 }}</span>
                    <Badge size="sm">{{ t(`egress.${rule.direction}`) }}</Badge>
                    <Badge :variant="ruleActionVariant(rule.action)" size="sm">{{ t(`egress.${rule.action}`) }}</Badge>
                    <Badge variant="info" size="sm">{{ rule.protocol ? rule.protocol.toUpperCase() : t('egress.any') }}</Badge>
                  </div>
                  <p class="break-all font-mono text-xs text-[var(--text-primary)] tabular-nums">
                    {{ rule.source || '*' }}:{{ rule.sourcePort || '*' }}
                    <span class="mx-1.5 text-[var(--text-tertiary)]">→</span>
                    {{ rule.destination || '*' }}:{{ rule.destinationPort || '*' }}
                  </p>
                  <p v-if="rule.description" class="text-xs text-[var(--text-secondary)] break-words">
                    {{ rule.description }}
                  </p>
                </div>
              </div>

              <div class="flex items-center gap-1 shrink-0 self-end sm:self-center">
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
        </Card>
      </div>
    </div>

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
