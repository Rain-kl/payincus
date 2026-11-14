<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { HostDiagnosticCheckItem, HostDiagnosticResult } from '@/types/api'
import { Button } from '@/components/ui'
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Database,
  MinusCircle,
  RefreshCw,
  Server,
  ShieldCheck,
  Waypoints,
  X,
  XCircle
} from 'lucide-vue-next'

interface Props {
  show: boolean
  hostName?: string
  loading: boolean
  diagnostic: HostDiagnosticResult | null
  error?: string
  syncingBaseline?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  hostName: '',
  error: '',
  syncingBaseline: false
})

const emit = defineEmits<{
  close: []
  recheck: []
  baselineSync: []
}>()

const { t } = useI18n()

const checkOrder: Array<HostDiagnosticCheckItem['key']> = [
  'agentHeartbeat',
  'reverseTunnel',
  'incusMtls',
  'nodeSecurity',
  'storageBaseline'
]

const orderedChecks = computed<HostDiagnosticCheckItem[]>(() => {
  if (!props.diagnostic) return []
  return checkOrder.map(key => props.diagnostic!.checks[key]).filter(Boolean)
})

const iconByKey: Record<HostDiagnosticCheckItem['key'], typeof Activity> = {
  agentHeartbeat: Activity,
  reverseTunnel: Waypoints,
  incusMtls: Server,
  nodeSecurity: ShieldCheck,
  storageBaseline: Database
}

const bannerTone = computed(() => {
  if (!props.diagnostic) {
    return {
      wrapper: 'bg-[var(--bg-surface-soft)] border-[var(--border-color)] text-[var(--text-primary)]',
      icon: 'text-[var(--accent)]'
    }
  }
  if (props.diagnostic.overallStatus === 'healthy') {
    return {
      wrapper: 'bg-[color-mix(in_srgb,var(--success)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))] text-[var(--success)]',
      icon: 'text-[var(--success)]'
    }
  }
  if (props.diagnostic.overallStatus === 'warning') {
    return {
      wrapper: 'bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] text-[var(--warning)]',
      icon: 'text-[var(--warning)]'
    }
  }
  return {
    wrapper: 'bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] text-[var(--error)]',
    icon: 'text-[var(--error)]'
  }
})

function statusBadgeClass(status: HostDiagnosticCheckItem['status']): string {
  switch (status) {
    case 'pass':
      return 'bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border-[color-mix(in_srgb,var(--success)_28%,var(--bg-surface))]'
    case 'warn':
      return 'bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))]'
    case 'fail':
      return 'bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))]'
    default:
      return 'bg-[var(--bg-surface-soft)] text-[var(--text-tertiary)] border-[var(--border-color)]'
  }
}

function statusText(status: HostDiagnosticCheckItem['status']): string {
  switch (status) {
    case 'pass':
      return t('admin.hosts.diagnostic.statusPass')
    case 'warn':
      return t('admin.hosts.diagnostic.statusWarn')
    case 'fail':
      return t('admin.hosts.diagnostic.statusFail')
    default:
      return t('admin.hosts.diagnostic.statusSkipped')
  }
}

function formattedCheckedAt(iso?: string): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (!Number.isFinite(date.getTime())) return iso
  return date.toLocaleString()
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div v-if="show" class="modal-overlay">
        <div class="modal-backdrop" @click="!loading && !syncingBaseline && emit('close')"></div>
        <div class="modal-content max-w-xl">
          <div class="modal-header">
            <div class="flex items-center gap-3 min-w-0">
              <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_25%,var(--bg-surface))]">
                <Activity class="w-5 h-5 stroke-[2]" />
              </div>
              <div class="min-w-0">
                <h3 class="modal-title">{{ t('admin.hosts.diagnostic.title') }}</h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5 truncate">{{ hostName || diagnostic?.hostName }}</p>
              </div>
            </div>
            <button
              type="button"
              class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95"
              :disabled="loading || syncingBaseline"
              @click="emit('close')"
            >
              <X class="w-4 h-4" />
            </button>
          </div>

          <div class="modal-body space-y-4">
            <div v-if="loading" class="space-y-2.5">
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 flex items-center gap-3">
                <RefreshCw class="w-4 h-4 text-[var(--accent)] animate-spin shrink-0" />
                <span class="text-sm text-[var(--text-primary)] font-medium">{{ t('admin.hosts.diagnostic.running') }}</span>
              </div>
              <div
                v-for="idx in 5"
                :key="idx"
                class="h-14 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/60 animate-pulse"
              ></div>
            </div>

            <div
              v-else-if="error"
              class="rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-4 text-sm text-[var(--error)] flex items-start gap-3"
            >
              <XCircle class="w-5 h-5 shrink-0 mt-0.5" />
              <div class="space-y-1">
                <div class="font-medium">{{ t('admin.hosts.diagnostic.requestError') }}</div>
                <div class="text-xs opacity-90 break-words">{{ error }}</div>
              </div>
            </div>

            <template v-else-if="diagnostic">
              <div class="rounded-2xl border p-3.5 flex items-start justify-between gap-3" :class="bannerTone.wrapper">
                <div class="flex items-start gap-2.5 min-w-0">
                  <CheckCircle2 v-if="diagnostic.overallStatus === 'healthy'" class="w-5 h-5 shrink-0 mt-0.5" :class="bannerTone.icon" />
                  <AlertTriangle v-else-if="diagnostic.overallStatus === 'warning'" class="w-5 h-5 shrink-0 mt-0.5" :class="bannerTone.icon" />
                  <XCircle v-else class="w-5 h-5 shrink-0 mt-0.5" :class="bannerTone.icon" />
                  <div class="min-w-0">
                    <div class="text-sm font-semibold">{{ diagnostic.summary }}</div>
                    <div class="text-xs opacity-80 mt-0.5">
                      {{ t('admin.hosts.diagnostic.checkedAt') }} {{ formattedCheckedAt(diagnostic.checkedAt) }}
                    </div>
                  </div>
                </div>
              </div>

              <div class="space-y-2.5">
                <div
                  v-for="item in orderedChecks"
                  :key="item.key"
                  class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/60 p-3.5 transition-colors"
                >
                  <div class="flex items-center justify-between gap-2">
                    <div class="flex items-center gap-2 min-w-0">
                      <component :is="iconByKey[item.key]" class="w-4 h-4 text-[var(--text-secondary)] shrink-0" />
                      <span class="text-sm font-medium text-[var(--text-primary)] truncate">{{ item.label }}</span>
                    </div>
                    <div class="flex items-center gap-2 shrink-0">
                      <span
                        v-if="typeof item.latencyMs === 'number'"
                        class="text-xs font-mono tabular-nums text-[var(--text-secondary)]"
                      >
                        {{ item.latencyMs }} ms
                      </span>
                      <span
                        class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium border"
                        :class="statusBadgeClass(item.status)"
                      >
                        <CheckCircle2 v-if="item.status === 'pass'" class="w-3 h-3" />
                        <AlertTriangle v-else-if="item.status === 'warn'" class="w-3 h-3" />
                        <XCircle v-else-if="item.status === 'fail'" class="w-3 h-3" />
                        <MinusCircle v-else class="w-3 h-3" />
                        {{ statusText(item.status) }}
                      </span>
                    </div>
                  </div>
                  <p class="mt-1.5 text-xs text-[var(--text-secondary)] leading-relaxed break-words">
                    {{ item.detail }}
                  </p>
                </div>
              </div>
            </template>
          </div>

          <div class="modal-footer flex flex-wrap items-center justify-end gap-2.5 px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface)]">
            <Button
              variant="secondary"
              :disabled="loading || syncingBaseline"
              @click="emit('close')"
            >
              {{ t('common.close') }}
            </Button>
            <Button
              v-if="diagnostic?.needsBaselineSync"
              variant="secondary"
              :disabled="loading || syncingBaseline"
              :loading="syncingBaseline"
              @click="emit('baselineSync')"
            >
              {{ t('admin.hosts.diagnostic.syncBaselineNow') }}
            </Button>
            <Button
              variant="primary"
              :disabled="syncingBaseline"
              :loading="loading"
              @click="emit('recheck')"
            >
              {{ t('admin.hosts.diagnostic.recheck') }}
            </Button>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
