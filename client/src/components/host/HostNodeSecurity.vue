<script lang="ts">
import type { NodeSecurityOverview } from '@/api/node-security-types'

// 模块级缓存：切换标签页再切回时直接展示缓存并后台异步刷新
const nodeSecurityCache = new Map<number, NodeSecurityOverview>()
</script>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import api from '@/api/admin'
import DrawerModal from '@/components/DrawerModal.vue'
import { Badge } from '@/components/ui'

const props = defineProps<{ hostId: number }>()
const emit = defineEmits<{ refresh: [] }>()
const overview = ref<NodeSecurityOverview | null>(nodeSecurityCache.get(props.hostId) ?? null)
const initialLoading = ref(!overview.value)
const refreshing = ref(false)
const busy = ref(false)
const error = ref('')
const message = ref('')
const drawer = ref(false)
const action = ref<'identity' | 'policy' | 'recover' | 'lock'>('policy')
const dataset = ref('')
const state = computed(() => overview.value?.state)
const recovering = computed(() => overview.value?.recoveryTask?.status === 'running')
const recoveryStatus = computed(() => ({ running: '后台恢复中', success: '恢复完成', failed: '恢复失败，可重试', cancelled: '恢复已停止', interrupted: '恢复已中断，可重试' })[overview.value?.recoveryTask?.status || 'running'])
const title = computed(() => ({ identity: '核对节点证书指纹', policy: '切换安全模式', recover: '批准节点恢复', lock: '立即锁定节点' })[action.value])
let generation = 0
let loading = false
async function refresh(silent = false) {
  if (loading || busy.value) return
  loading = true
  if (!silent && !overview.value) initialLoading.value = true
  else refreshing.value = true
  const hostId = props.hostId
  const current = generation
  try {
    const result = await api.nodeSecurity.status(hostId)
    if (current === generation) {
      const changed = overview.value?.state?.locked !== result.state?.locked
      overview.value = result
      nodeSecurityCache.set(hostId, result)
      error.value = ''
      if (changed) emit('refresh')
    }
  } catch {
    if (current === generation && !overview.value) {
      error.value = '无法读取节点管理状态，请检查权限和连接'
    }
  } finally {
    loading = false
    if (current === generation) {
      initialLoading.value = false
      refreshing.value = false
    }
  }
}
function open(next: typeof action.value) {
  action.value = next
  dataset.value = ''
  error.value = ''
  message.value = ''
  drawer.value = true
}
function close() {
  if (busy.value) return
  drawer.value = false
}
async function submit() {
  if (busy.value) return
  const hostId = props.hostId
  const current = generation
  const revision = state.value?.revision
  busy.value = true
  error.value = ''
  try {
    if (action.value === 'identity') {
      await api.nodeSecurity.identity(hostId, dataset.value.replaceAll(':', '').toLowerCase().trim())
      if (current !== generation) return
      drawer.value = false
      message.value = '节点证书指纹已保存；后续连接将严格校验。'
    } else if (action.value === 'recover') {
      if (revision === undefined || overview.value?.offline) throw new Error('请先刷新在线状态')
      const accepted = await api.nodeSecurity.recover(hostId, revision)
      if (current !== generation) return
      drawer.value = false
      message.value = `恢复任务 #${accepted.taskId} 已提交，将在后台恢复实例，可查看任务日志。`
    } else {
      if (action.value !== 'lock' && (revision === undefined || overview.value?.offline)) throw new Error('请先刷新在线状态')
      await api.nodeSecurity.action(hostId, action.value, action.value === 'policy' ? { enabled: !state.value?.enabled, revision } : {})
      drawer.value = false
      message.value = '操作完成；授权恢复后自动恢复锁定前运行的实例。'
    }
  } catch {
    if (current === generation) error.value = action.value === 'recover'
      ? '恢复未完成，请查看任务记录并刷新状态后重试。'
      : '节点操作未完成，请刷新状态并检查连接。'
  }
  finally { busy.value = false; await refresh() }
}
watch(() => props.hostId, (hostId) => {
  generation++
  const cached = nodeSecurityCache.get(hostId) ?? null
  overview.value = cached
  initialLoading.value = !cached
  drawer.value = false
  message.value = ''
  error.value = ''
  void refresh(!!cached)
}, { immediate: true })
const timer = setInterval(() => { void refresh(true) }, 5000)
onUnmounted(() => { generation++; clearInterval(timer) })
</script>

<template>
  <section class="card p-5 space-y-4">
    <!-- 首次无缓存加载中：仅显示骨架动画 -->
    <div v-if="initialLoading && !overview" class="space-y-4 animate-pulse" aria-busy="true" aria-label="正在加载节点安全状态">
      <div class="flex items-start justify-between gap-4">
        <div class="space-y-2">
          <div class="h-5 w-32 rounded-md bg-[var(--bg-surface-soft)]"></div>
          <div class="h-4 w-64 rounded-md bg-[var(--bg-surface-soft)]"></div>
        </div>
        <div class="flex items-center gap-2">
          <div class="h-6 w-16 rounded-full bg-[var(--bg-surface-soft)]"></div>
          <div class="h-8 w-16 rounded-full bg-[var(--bg-surface-soft)]"></div>
        </div>
      </div>
      <div class="h-16 w-full rounded-xl bg-[var(--bg-surface-soft)]"></div>
      <div class="flex flex-wrap gap-2">
        <div class="h-8 w-28 rounded-full bg-[var(--bg-surface-soft)]"></div>
        <div class="h-8 w-24 rounded-full bg-[var(--bg-surface-soft)]"></div>
        <div class="h-8 w-24 rounded-full bg-[var(--bg-surface-soft)]"></div>
      </div>
      <div class="h-4 w-3/4 rounded-md bg-[var(--bg-surface-soft)]"></div>
    </div>

    <template v-else>
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 class="text-lg font-semibold text-themed">节点安全模式</h3>
          <p class="text-sm text-themed-muted mt-1">所有受管 CLI 调用上报审计；安全模式默认关闭。</p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <Badge :variant="!state ? 'default' : state.locked ? 'error' : 'success'" dot size="sm">
            API：{{ !state ? '未知' : state.locked ? '已锁定' : '未锁定' }}
          </Badge>
          <Badge v-if="overview?.offline" variant="warning" dot size="sm">节点离线，显示上次上报</Badge>
          <button class="btn-secondary btn-sm" :disabled="busy || refreshing" @click="refresh(false)">
            {{ refreshing ? '刷新中…' : '刷新' }}
          </button>
        </div>
      </div>

      <p v-if="error" class="text-sm text-[var(--error)]" role="alert">{{ error }}</p>
      <p v-if="message" class="text-sm text-[var(--success)]" role="status">{{ message }}</p>

      <div v-if="overview?.recoveryTask" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-2 text-sm text-themed" role="status">
        <p class="font-medium">恢复任务 #{{ overview.recoveryTask.id }}：{{ recoveryStatus }}</p>
        <p v-if="overview.recoveryTask.error" class="text-xs text-[var(--error)]">{{ overview.recoveryTask.error }}</p>
        <RouterLink class="btn-secondary btn-sm inline-flex" :to="{ path: '/admin/tasks', query: { task: overview.recoveryTask.id } }">查看恢复任务日志</RouterLink>
      </div>

      <!-- 安全模式开关 -->
      <div class="flex items-center justify-between gap-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3">
        <div class="min-w-0">
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium text-[var(--text-primary)]">安全模式</span>
            <Badge :variant="state?.enabled ? 'info' : 'default'" size="sm">
              {{ state?.enabled ? '已开启' : '未开启' }}
            </Badge>
          </div>
          <p v-if="state?.reason" class="mt-0.5 text-xs text-[var(--text-secondary)]">原因：{{ state.reason }}</p>
          <p v-else class="mt-0.5 text-xs text-[var(--text-tertiary)]">开启后将拦截异常受管操作并按策略触发锁定</p>
        </div>
        <button
          type="button"
          role="switch"
          :aria-checked="!!state?.enabled"
          aria-label="切换安全模式"
          :disabled="busy || !state || overview?.offline"
          class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer items-center rounded-full border-0 transition-colors duration-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] focus-visible:ring-offset-2 disabled:opacity-50 disabled:cursor-not-allowed"
          :class="state?.enabled ? 'bg-[var(--accent)]' : 'bg-[var(--border-strong)]'"
          @click="open('policy')"
        >
          <span
            class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow transition-transform duration-200"
            :class="state?.enabled ? 'translate-x-5' : 'translate-x-0.5'"
          />
        </button>
      </div>

      <div class="flex flex-wrap gap-2">
        <button class="btn-secondary btn-sm" :disabled="busy" @click="open('identity')">核对证书指纹</button>
        <button class="btn-secondary btn-sm" :disabled="busy || overview?.offline" @click="open('lock')">立即锁定</button>
        <button class="btn-primary btn-sm" :disabled="busy || recovering || (!state?.locked && !state?.resumeInstances?.length) || overview?.offline" @click="open('recover')">批准恢复</button>
      </div>

      <p class="text-xs text-themed-muted">锁定将停止本节点实例并尝试卸载已注册的加密存储；可能中断业务。存储动作失败时 API 仍保持锁定。</p>
      <p class="text-xs text-themed-muted break-all">节点证书 SHA256：{{ overview?.serverFingerprint || '尚未保存，请先通过可信终端核对' }}</p>
      <div v-for="result in state?.results || []" :key="result.target" class="rounded-lg border border-themed p-3 text-sm text-themed">
        {{ result.target }}：{{ result.detail === 'not_configured' ? '未配置加密存储，附加锁定不适用' : result.success ? '完成' : '失败，请检查占用或存储状态' }}
      </div>
      <details class="text-sm text-themed">
        <summary class="cursor-pointer select-none">最近 CLI 审计与告警（最多 100 条）</summary>
        <div v-for="event in overview?.events || []" :key="event.eventId" class="border-b border-themed py-2 break-words">
          {{ new Date(event.occurredAt).toLocaleString() }} · UID {{ event.uid }} · PID {{ event.pid }} · {{ event.command }} · {{ event.blocked ? '已拒绝' : '已记录' }}
        </div>
      </details>
    </template>
    <DrawerModal :show="drawer" :title="title" :closable="!busy" :close-on-backdrop="!busy" @close="close">
      <form class="space-y-4" @submit.prevent="submit">
        <p class="text-sm text-themed-muted">请确认本次管理操作。存储磁盘的加密与解锁请在“存储”页面管理。</p>
        <p v-if="action === 'lock'" class="text-sm text-themed">此操作将中断本节点实例，之后需要人工恢复。</p>
        <p v-if="action === 'recover'" class="text-sm text-themed">请先确认告警原因已处理，并在“存储”页面解密相关磁盘。批准后创建后台任务，自动恢复锁定前运行的实例。</p>
        <label v-if="action === 'identity'" class="block text-sm text-themed">
          从节点 TUI 可信终端核对服务端证书 SHA256；更换证书时必须重新核对
          <input v-model="dataset" class="input mt-1 w-full" required autocomplete="off" />
        </label>
        <p v-if="message" class="text-sm text-themed">{{ message }}</p>
        <p v-if="error" class="text-sm text-themed" role="alert">{{ error }}</p>
        <div class="flex justify-end gap-2">
          <button type="button" class="btn-secondary" :disabled="busy" @click="close">关闭</button>
          <button type="submit" class="btn-primary" :disabled="busy">{{ busy ? '正在处理…' : '确认操作' }}</button>
        </div>
      </form>
    </DrawerModal>
  </section>
</template>
