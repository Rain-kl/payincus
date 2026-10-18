<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import api, { type OperationTask, type OperationTaskLog } from '@/api/admin'
import DrawerModal from '@/components/DrawerModal.vue'
import { buildApiUrl } from '@/utils/api-url'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const items = ref<OperationTask[]>([])
const total = ref(0)
const page = ref(1)
const search = ref('')
const status = ref('')
const kind = ref('')
const loading = ref(false)
const listError = ref('')
const selected = ref<OperationTask | null>(null)
const logs = ref<OperationTaskLog[]>([])
const drawer = ref(false)
const connection = ref('connecting')
const streamError = ref('')
const follow = ref(true)
const logPanel = ref<HTMLElement | null>(null)
const now = ref(Date.now())
const stopTarget = ref<OperationTask | null>(null)
const stopping = ref(false)
const stopError = ref('')
let clockTimer: ReturnType<typeof setInterval> | undefined
let disposed = false
let refreshTimer: ReturnType<typeof setInterval> | undefined
let controller: AbortController | undefined
let session = 0
const pages = computed(() => Math.max(1, Math.ceil(total.value / 20)))
const key = (name: string) => `operationTasks.${name}`
const badge = (value: string) => ({ running: 'badge-warning', success: 'badge-success', failed: 'badge-error', interrupted: 'badge-error' }[value] || 'badge-default')
const date = (value: string | null) => value ? new Date(value).toLocaleString() : '—'
function duration(task: OperationTask) {
  const end = task.finishedAt ? new Date(task.finishedAt).getTime() : task.status === 'running' ? now.value : NaN
  if (!Number.isFinite(end)) return '—'
  const seconds = Math.max(0, Math.floor((end - new Date(task.createdAt).getTime()) / 1000))
  return `${Math.floor(seconds / 3600).toString().padStart(2, '0')}:${Math.floor(seconds / 60 % 60).toString().padStart(2, '0')}:${(seconds % 60).toString().padStart(2, '0')}`
}
function askStop(task: OperationTask) { stopError.value = ''; stopTarget.value = task }
async function confirmStop() {
  if (!stopTarget.value || stopping.value) return
  stopping.value = true; stopError.value = ''
  try {
    await api.operationTasks.stop(stopTarget.value.id)
    stopTarget.value = null
    await load()
  } catch (error) {
    stopError.value = error && typeof error === 'object' && 'message' in error && typeof error.message === 'string' ? error.message : t(key('stopFailed'))
  }
  finally { stopping.value = false }
}
async function load() {
  if (loading.value) return
  loading.value = true
  try {
    const data = await api.operationTasks.list({ page: page.value, search: search.value || undefined, status: status.value || undefined, kind: kind.value || undefined })
    if (disposed) return
    items.value = data.items; total.value = data.total; listError.value = ''
  } catch (error) { if (!disposed) listError.value = error instanceof Error ? error.message : t(key('loadFailed')) }
  finally { loading.value = false }
}
async function filter() { page.value = 1; await load() }
function open(task: OperationTask) { void router.replace({ query: { ...route.query, task: task.id } }) }
function close() { const query = { ...route.query }; delete query.task; void router.replace({ query }) }
function stop() { session++; controller?.abort(); controller = undefined }
async function scroll() { if (follow.value) { await nextTick(); logPanel.value?.scrollTo({ top: logPanel.value.scrollHeight }) } }
async function stream(id: number) {
  stop()
  const ownSession = session
  const abort = new AbortController()
  controller = abort
  logs.value = []
  selected.value = items.value.find(item => item.id === id) || null
  drawer.value = true
  connection.value = 'connecting'; streamError.value = ''
  let cursor = 0
  while (!abort.signal.aborted && ownSession === session) {
    try {
      // Refresh access tokens through the standard API interceptor before each SSE connection.
      await api.operationTasks.list({ page: 1 })
      if (abort.signal.aborted) break
      const token = localStorage.getItem('token')
      const response = await fetch(buildApiUrl(`/admin/tasks/${id}/logs/stream?after=${cursor}`), {
        headers: { Authorization: `Bearer ${token || ''}` }, signal: abort.signal, credentials: 'include'
      })
      if (!response.ok || !response.body) {
        if ([400, 401, 403, 404].includes(response.status)) { connection.value = 'loadFailed'; streamError.value = `${t(key('loadFailed'))} (HTTP ${response.status})`; break }
        throw new Error(`HTTP ${response.status}`)
      }
      connection.value = 'connected'; streamError.value = ''
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let ended = false
      try {
        while (!abort.signal.aborted) {
          const result = await reader.read()
          if (result.done) break
          buffer += decoder.decode(result.value, { stream: true }).replace(/\r\n/g, '\n')
          if (buffer.length > 1024 * 1024) throw new Error('Invalid log stream')
          let split: number
          while ((split = buffer.indexOf('\n\n')) >= 0) {
            const block = buffer.slice(0, split); buffer = buffer.slice(split + 2)
            const event = block.split('\n').find(line => line.startsWith('event:'))?.slice(6).trim()
            const payload = block.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
            if (!payload) continue
            const data = JSON.parse(payload)
            if (ownSession !== session) return
            if (event === 'log' && data.id > cursor) { logs.value.push(data); cursor = data.id }
            if (event === 'task') selected.value = data
            if (event === 'stream-error') throw new Error(data.message)
            if (event === 'end') { ended = true; connection.value = 'complete' }
          }
          await scroll()
        }
      } finally { await reader.cancel().catch(() => {}); reader.releaseLock() }
      if (ended) { void load(); break }
    } catch (error) {
      if (abort.signal.aborted || ownSession !== session) return
      streamError.value = error instanceof Error ? error.message : t(key('loadFailed'))
    }
    if (abort.signal.aborted || ownSession !== session) break
    connection.value = 'reconnecting'
    await new Promise<void>(resolve => {
      const done = () => { clearTimeout(timer); abort.signal.removeEventListener('abort', done); resolve() }
      const timer = setTimeout(done, 2000)
      abort.signal.addEventListener('abort', done, { once: true })
    })
  }
}
function download() {
  const content = logs.value.map(log => `${date(log.createdAt)} [${log.source}] [${log.level}] ${log.message}`).join('\n')
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain;charset=utf-8' }))
  const anchor = document.createElement('a')
  anchor.href = url; anchor.download = `task-${route.query.task}.log`; anchor.click()
  URL.revokeObjectURL(url)
}
watch(() => route.query.task, value => {
  const id = Number(value)
  if (Number.isSafeInteger(id) && id > 0) void stream(id)
  else { stop(); drawer.value = false; selected.value = null }
}, { immediate: true })
watch(drawer, value => { if (!value && route.query.task) close() })
watch(follow, () => { void scroll() })
onMounted(() => { void load(); refreshTimer = setInterval(() => { void load() }, 5000); clockTimer = setInterval(() => { now.value = Date.now() }, 1000) })
onUnmounted(() => { disposed = true; stop(); clearInterval(refreshTimer); clearInterval(clockTimer) })
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-start justify-between gap-4">
      <div><h1 class="page-title">{{ t(key('title')) }}</h1><p class="mt-2 text-themed-secondary">{{ t(key('description')) }}</p></div>
      <button class="btn-secondary" :disabled="loading" @click="load">{{ t(key('refresh')) }}</button>
    </div>
    <form class="card p-4 flex flex-wrap gap-3" @submit.prevent="filter">
      <input v-model="search" class="input flex-1" :aria-label="t(key('instance'))" :placeholder="t(key('instance'))" maxlength="100" />
      <select v-model="status" class="input w-auto" :aria-label="t(key('allStatuses'))" @change="filter"><option value="">{{ t(key('allStatuses')) }}</option><option v-for="value in ['running', 'success', 'failed', 'cancelled', 'interrupted']" :key="value" :value="value">{{ t(key(value)) }}</option></select>
      <select v-model="kind" class="input w-auto" :aria-label="t(key('allKinds'))" @change="filter"><option value="">{{ t(key('allKinds')) }}</option><option value="create">{{ t(key('create')) }}</option><option value="destroy">{{ t(key('destroy')) }}</option></select>
      <button class="btn-primary" type="submit">{{ t(key('search')) }}</button>
    </form>
    <p v-if="listError" role="alert" class="text-error">{{ listError }}</p>
    <div v-if="!items.length" class="card p-10 text-center text-themed-muted">{{ t(key('empty')) }}</div>
    <div v-else>
      <div class="space-y-3 lg:hidden">
        <div v-for="task in items" :key="task.id" class="card p-4 w-full text-left space-y-2">
          <div class="flex justify-between gap-2"><button class="font-medium break-all text-left hover:underline" @click="open(task)">{{ task.instanceName }}</button><span class="badge shrink-0" :class="badge(task.status)">{{ t(key(task.status)) }}</span></div>
          <div class="text-sm text-themed-secondary">#{{ task.id }} · {{ t(key(task.kind)) }} · {{ task.hostName || task.hostId }}</div>
          <div class="text-xs text-themed-muted">{{ date(task.createdAt) }}</div>
          <div class="text-xs text-themed-secondary">{{ t(key('duration')) }}: <span class="font-mono tabular-nums">{{ duration(task) }}</span></div>
          <span v-if="task.stopping" class="text-warning text-sm">{{ t(key('stopping')) }}</span>
          <button v-else-if="task.canStop" class="btn-danger" @click="askStop(task)">{{ t(key('stopTask')) }}</button>
          <p v-if="task.error" class="text-sm text-error break-words">{{ task.error }}</p>
        </div>
      </div>
      <div class="card hidden lg:block overflow-hidden">
        <table class="w-full table-fixed text-sm">
          <thead class="bg-themed-tertiary text-themed-secondary"><tr><th class="p-4 text-left w-[25%]">{{ t(key('instance')) }}</th><th class="p-4 text-left w-[8%]">{{ t(key('allKinds')) }}</th><th class="p-4 text-left w-[12%]">{{ t(key('allStatuses')) }}</th><th class="p-4 text-left w-[16%]">{{ t(key('host')) }}</th><th class="p-4 text-left w-[17%]">{{ t(key('created')) }}</th><th class="p-4 text-left w-[12%]">{{ t(key('duration')) }}</th><th class="p-4 w-[10%]"><span class="sr-only">{{ t(key('actions')) }}</span></th></tr></thead>
          <tbody><tr v-for="task in items" :key="task.id" class="border-t border-themed">
            <td class="p-4 break-all"><button class="text-left font-medium hover:underline" @click="open(task)">{{ task.instanceName }}</button><div class="text-xs text-themed-muted mt-1">#{{ task.id }}</div><div v-if="task.error" class="text-error mt-1 break-words">{{ task.error }}</div></td>
            <td class="p-4">{{ t(key(task.kind)) }}</td><td class="p-4"><span class="badge" :class="badge(task.status)">{{ t(key(task.status)) }}</span><div v-if="task.stopping" class="text-warning text-xs mt-1">{{ t(key('stopping')) }}</div></td>
            <td class="p-4 break-words">{{ task.hostName || task.hostId }}</td><td class="p-4 break-words">{{ date(task.createdAt) }}</td><td class="p-4 font-mono tabular-nums">{{ duration(task) }}</td>
            <td class="p-2"><div class="flex flex-wrap justify-end gap-1">
              <button class="inline-flex items-center justify-center p-2 text-themed-secondary transition-colors hover:text-themed focus-visible:outline focus-visible:outline-2 focus-visible:outline-[var(--accent)]" :title="t(key('view'))" :aria-label="t(key('view'))" @click="open(task)"><svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><path stroke-linecap="round" d="M6 5.5h12M6 10h12M6 14.5h8M6 19h12" /></svg></button>
              <button v-if="task.status === 'running'" class="inline-flex items-center justify-center p-2 text-error transition-colors hover:text-themed disabled:cursor-not-allowed disabled:text-themed-muted focus-visible:outline focus-visible:outline-2 focus-visible:outline-[var(--accent)]" :disabled="!task.canStop || task.stopping" :title="t(key(task.stopping ? 'stopping' : 'stopTask'))" :aria-label="t(key('stopTask'))" @click="askStop(task)"><svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><rect x="6.5" y="6.5" width="11" height="11" rx="1.5" /></svg></button>
            </div></td>
          </tr></tbody>
        </table>
      </div>
    </div>
    <div class="flex items-center justify-end gap-4"><button class="btn-secondary" :disabled="page <= 1 || loading" @click="page--; load()">{{ t(key('previous')) }}</button><span class="text-sm text-themed-muted">{{ page }} / {{ pages }}</span><button class="btn-secondary" :disabled="page >= pages || loading" @click="page++; load()">{{ t(key('next')) }}</button></div>
    <DrawerModal v-model="drawer" :title="t(key('detail'))" max-width="max-w-4xl">
      <div class="space-y-4">
        <div v-if="selected" class="space-y-2">
          <div class="font-semibold text-lg break-all">#{{ selected.id }} · {{ selected.instanceName }}</div>
          <div class="flex flex-wrap gap-3 items-center"><span class="badge" :class="badge(selected.status)">{{ t(key(selected.status)) }}</span><span>{{ t(key(selected.kind)) }} · {{ t(key('host')) }} #{{ selected.hostId }}</span></div>
          <div class="text-sm text-themed-secondary">{{ t(key('created')) }}: {{ date(selected.createdAt) }}<br />{{ t(key('finished')) }}: {{ date(selected.finishedAt) }}</div>
          <div class="text-sm text-themed-secondary">{{ t(key('duration')) }}: <span class="font-mono tabular-nums">{{ duration(selected) }}</span></div>
          <button v-if="selected.canStop" class="btn-danger" @click="askStop(selected)">{{ t(key('stopTask')) }}</button>
          <p v-if="selected.stopping" class="text-warning">{{ t(key('stopping')) }}</p>
          <p v-if="selected.error" role="alert" class="text-error break-words">{{ selected.error }}</p>
          <p v-if="selected.status === 'interrupted'" class="text-warning">{{ t(key('interruptedHint')) }}</p>
        </div>
        <div class="flex flex-wrap gap-3 items-center justify-between text-sm"><span role="status" class="text-themed-secondary">{{ t(key(connection)) }}</span><label class="flex gap-2 items-center"><input v-model="follow" type="checkbox" />{{ t(key('follow')) }}</label><button class="btn-secondary" :disabled="!logs.length" @click="download">{{ t(key('download')) }}</button></div>
        <p v-if="streamError" role="alert" class="text-error text-sm break-words">{{ streamError }}</p>
        <div ref="logPanel" class="task-logs border border-themed rounded-lg p-3 bg-themed-tertiary font-mono text-xs" role="log" :aria-label="t(key('view'))">
          <p v-if="!logs.length" class="text-themed-muted">{{ t(key('waiting')) }}</p>
          <div v-for="log in logs" :key="log.id" class="mb-2 whitespace-pre-wrap break-all" :class="log.level === 'error' ? 'text-error' : log.level === 'warn' ? 'text-warning' : 'text-themed'"><span class="text-themed-muted">{{ date(log.createdAt) }} [{{ t(key(log.source)) }}]</span> {{ log.message }}</div>
        </div>
      </div>
    </DrawerModal>
    <DrawerModal :model-value="Boolean(stopTarget)" :title="t(key('stopTask'))" :closable="!stopping" :close-on-backdrop="!stopping" @update:model-value="value => { if (!value && !stopping) stopTarget = null }">
      <p class="break-all font-medium">{{ stopTarget?.instanceName }}</p>
      <p class="mt-3 text-themed-secondary">{{ t(key('stopHint')) }}</p>
      <p v-if="stopError" role="alert" class="mt-3 text-error">{{ stopError }}</p>
      <template #footer><button class="btn-secondary" :disabled="stopping" @click="stopTarget = null">{{ t(key('cancel')) }}</button><button class="btn-primary" :disabled="stopping" @click="confirmStop">{{ t(key(stopping ? 'stopping' : 'confirmStop')) }}</button></template>
    </DrawerModal>
  </div>
</template>

<style scoped>
.task-logs { height: 55vh; overflow-y: auto; }
</style>
