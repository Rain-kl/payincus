<script setup lang="ts">
import { ref, onMounted, watch, onBeforeUnmount, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import { useToast } from '@/stores/toast'
import { buildAbsoluteApiUrl } from '@/utils/api-url'
import api from '@/api'

const { t } = useI18n()
const themeStore = useThemeStore()
const toast = useToast()

interface Props {
  hostId: number
}

const props = defineProps<Props>()

// connecting: 正在建立 SSE；online: 已连接；offline: Agent 不可用
type LogState = 'connecting' | 'online' | 'offline'
const state = ref<LogState>('connecting')
const lines = ref<string[]>([])
const MAX_LINES = 500

let eventSource: EventSource | null = null
let retryTimer: ReturnType<typeof setTimeout> | null = null
let closed = false

const logsPanel = ref<HTMLElement | null>(null)
const stickToBottom = ref(true)

onMounted(() => {
  connect()
})

onBeforeUnmount(() => {
  closed = true
  closeStream()
})

watch(() => props.hostId, () => {
  lines.value = []
  closeStream()
  connect()
})

function closeStream(): void {
  if (retryTimer) {
    clearTimeout(retryTimer)
    retryTimer = null
  }
  if (eventSource) {
    eventSource.close()
    eventSource = null
  }
}

async function connect(): Promise<void> {
  if (closed) return
  closeStream()
  state.value = 'connecting'

  let ticket: string
  try {
    const res = await api.hosts.createAgentLogsTicket(props.hostId)
    ticket = res.ticket
  } catch (err: unknown) {
    state.value = 'offline'
    const error = err as { message?: string }
    toast.error(t('host.agentLogs.failed') + ': ' + (error?.message || String(err)))
    scheduleRetry()
    return
  }

  const url = buildAbsoluteApiUrl(
    `/hosts/${props.hostId}/agent/logs/stream?ticket=${encodeURIComponent(ticket)}`
  )
  const es = new EventSource(url, { withCredentials: true })
  eventSource = es

  es.addEventListener('status', (ev) => {
    try {
      const payload = JSON.parse((ev as MessageEvent).data) as { online?: boolean }
      state.value = payload.online ? 'online' : 'offline'
    } catch {
      // 忽略畸形状态帧
    }
  })

  es.addEventListener('log', (ev) => {
    let line: string
    try {
      line = JSON.parse((ev as MessageEvent).data) as string
    } catch {
      line = String((ev as MessageEvent).data ?? '')
    }
    appendLine(line)
  })

  es.addEventListener('end', () => {
    es.close()
    eventSource = null
    state.value = 'offline'
    scheduleRetry()
  })

  es.onopen = () => {
    state.value = 'online'
  }

  es.onerror = () => {
    // EventSource 会自行重连同一 URL，但票据是一次性的，必须关闭后重新换取
    es.close()
    eventSource = null
    state.value = 'offline'
    scheduleRetry()
  }
}

function appendLine(line: string): void {
  const text = line.trim()
  if (!text) return
  lines.value.push(text)
  if (lines.value.length > MAX_LINES) {
    lines.value.splice(0, lines.value.length - MAX_LINES)
  }
  if (stickToBottom.value) {
    nextTick(() => {
      const el = logsPanel.value
      if (el) el.scrollTop = el.scrollHeight
    })
  }
}

function scheduleRetry(): void {
  if (closed || retryTimer) return
  retryTimer = setTimeout(() => {
    retryTimer = null
    connect()
  }, 5000)
}

function onLogsScroll(): void {
  const el = logsPanel.value
  if (!el) return
  stickToBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

function reconnectNow(): void {
  lines.value = []
  connect()
}
</script>

<template>
  <div class="card p-6">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-lg font-medium" :class="themeStore.isDark ? 'text-white' : 'text-gray-900'">
        {{ t('host.agentLogs.title') }}
      </h3>
      <div class="flex items-center gap-3">
        <button class="btn-ghost btn-sm" :disabled="state === 'connecting'" @click="reconnectNow">
          {{ t('host.agentLogs.refresh') }}
        </button>
        <span
          class="px-3 py-1 text-sm rounded-full"
          :class="[
            state === 'online'
              ? 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-400'
              : state === 'connecting'
                ? 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400'
                : 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-400'
          ]"
        >
          {{ state === 'online' ? t('host.agentLogs.online') : state === 'connecting' ? t('common.loading') : t('host.agentLogs.offline') }}
        </span>
      </div>
    </div>

    <div
      ref="logsPanel"
      class="font-mono text-xs whitespace-pre-wrap break-all max-h-[65vh] overflow-y-auto rounded-lg p-4 border"
      :class="themeStore.isDark ? 'bg-gray-900 border-gray-800 text-gray-300' : 'bg-gray-50 border-gray-200 text-gray-800'"
      @scroll="onLogsScroll"
    >
      <div v-if="state === 'connecting' && lines.length === 0" class="flex items-center justify-center gap-2 py-10 text-gray-500">
        <div class="animate-spin rounded-full h-5 w-5 border-b-2 border-blue-500"></div>
        <span>{{ t('common.loading') }}</span>
      </div>
      <p v-else-if="lines.length === 0" class="text-center py-10" :class="themeStore.isDark ? 'text-gray-400' : 'text-gray-500'">
        {{ state === 'offline' ? t('host.agentLogs.offline') : t('host.agentLogs.empty') }}
      </p>
      <template v-else>
        <div v-for="(line, index) in lines" :key="index">{{ line }}</div>
      </template>
    </div>

    <div v-if="state === 'offline'" class="flex items-center justify-between text-xs mt-3"
      :class="themeStore.isDark ? 'text-gray-400' : 'text-gray-500'">
      <span>{{ t('host.agentLogs.emptyWaitingAgent') }}</span>
      <span v-if="state === 'offline'" class="text-gray-400">{{ t('host.agentLogs.autoRetry') }}</span>
    </div>
  </div>
</template>
