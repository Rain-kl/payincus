<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { Archive, Check, RefreshCw } from 'lucide-vue-next'
import api from '@/api/admin'
import type { HostInstanceBackupTask } from '@/api/host-instance-backup-types'
import { Button, Card, Input } from '@/components/ui'

const props = defineProps<{ hostId: number }>()
const selected = ref(false)
const directory = ref('')
const tasks = ref<HostInstanceBackupTask[]>([])
const loading = ref(false)
const submitting = ref(false)
const loaded = ref(false)
const error = ref('')
const message = ref('')
const pending = ref<{ id: string; directory: string } | null>(null)
const running = computed(() => tasks.value.some(task => task.status === 'running'))
const pathValid = computed(() => /^\/[^\u0000\r\n]+$/.test(directory.value.trim()))
const statusText: Record<string, string> = { running: '备份中', pending: '等待备份', success: '备份完成', partial: '部分失败', failed: '备份失败', interrupted: '已中断' }
let generation = 0
let timer: ReturnType<typeof setInterval> | undefined
let fetching = false

async function refresh() {
  if (fetching) return
  const current = generation
  const hostId = props.hostId
  fetching = true
  loading.value = !loaded.value
  try {
    const result = await api.hosts.getInstanceBackupTasks(hostId)
    if (current !== generation) return
    tasks.value = result.tasks
    loaded.value = true
    error.value = ''
  } catch (err) {
    if (current === generation) error.value = err instanceof Error ? err.message : '读取备份任务失败'
  } finally {
    if (current === generation) { fetching = false; loading.value = false }
  }
}

async function startBackup() {
  if (!pathValid.value || submitting.value || running.value || !loaded.value) return
  const current = generation
  const hostId = props.hostId
  const path = directory.value.trim()
  if (!pending.value || pending.value.directory !== path) pending.value = { id: crypto.randomUUID().replaceAll('-', ''), directory: path }
  submitting.value = true
  error.value = ''
  message.value = ''
  try {
    const task = await api.hosts.startInstanceBackup(hostId, pending.value)
    if (current !== generation) return
    tasks.value = [task, ...tasks.value.filter(item => item.id !== task.id)]
    pending.value = null
    message.value = '备份任务已启动，离开页面后仍会继续执行。'
  } catch (err) {
    if (current === generation) error.value = err instanceof Error ? err.message : '启动备份任务失败'
  } finally {
    if (current === generation) submitting.value = false
  }
}

function completed(task: HostInstanceBackupTask) { return task.items.filter(item => ['success', 'failed', 'interrupted'].includes(item.status)).length }
function formatDate(value: string) { return new Date(value).toLocaleString() }
function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KiB`
  if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MiB`
  return `${(bytes / 1024 ** 3).toFixed(1)} GiB`
}

watch(() => props.hostId, () => {
  generation++
  fetching = false
  loaded.value = false
  submitting.value = false
  tasks.value = []
  directory.value = ''
  pending.value = null
  message.value = ''
  error.value = ''
  selected.value = false
  clearInterval(timer)
  void refresh()
  timer = setInterval(() => { void refresh() }, 5000)
}, { immediate: true })
onUnmounted(() => { generation++; clearInterval(timer) })
</script>

<template>
  <section class="space-y-6" aria-label="实例备份">
    <div class="space-y-3">
      <h3 class="text-lg font-semibold tracking-tight text-themed">备份方式</h3>
      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3" role="radiogroup" aria-label="选择备份方式">
        <button
          type="button" role="radio" :aria-checked="selected"
          class="group rounded-2xl text-left min-h-[44px] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] motion-reduce:transition-none"
          :class="selected ? 'ring-2 ring-[var(--accent)]' : 'hover:opacity-80'"
          @click="selected = true"
        >
          <Card padding="sm">
            <div class="flex items-start justify-between gap-4">
              <Archive class="h-6 w-6 text-accent transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] group-hover:scale-105 motion-reduce:transition-none" aria-hidden="true" />
              <Check v-if="selected" class="h-5 w-5 text-accent" aria-hidden="true" />
            </div>
            <div class="mt-5 text-base font-semibold tracking-tight text-themed">手动备份</div>
            <p class="mt-2 text-sm text-themed-muted">立即备份当前宿主机的全部实例。</p>
          </Card>
        </button>
      </div>
    </div>

    <Card v-if="selected" title="备份设置" subtitle="实例保持当前运行状态，备份文件保存到宿主机目录。">
      <form class="space-y-5" @submit.prevent="startBackup">
        <Input v-model="directory" label="备份存储位置" placeholder="例如 /mnt/backup" :disabled="submitting || running" :maxlength="4096" hint="填写已存在的宿主机绝对目录，也支持已挂载的 NAS。每次任务会创建独立子目录。" />
        <p class="text-sm text-themed-muted">备份保存实例磁盘与配置；实例挂载的宿主机目录需单独备份。</p>
        <div class="flex items-center gap-3 flex-wrap">
          <Button type="submit" :loading="submitting" :disabled="!pathValid || running || !loaded">{{ running ? '备份任务进行中' : '开始备份' }}</Button>
          <span class="text-sm text-themed-muted">备份任务在节点后台执行，可随时回来查看进度。</span>
        </div>
      </form>
    </Card>

    <p v-if="error" role="alert" class="text-sm text-[var(--error)] break-words">{{ error }}</p>
    <p v-if="message" role="status" class="text-sm text-themed">{{ message }}</p>

    <Card title="备份任务" :loading="loading">
      <template #extra><Button variant="secondary" :disabled="loading" @click="refresh"><RefreshCw class="h-4 w-4" aria-hidden="true" />刷新</Button></template>
      <p v-if="loaded && tasks.length === 0" class="py-8 text-center text-sm text-themed-muted">暂无备份任务。选择手动备份后开始第一次备份。</p>
      <div v-else class="space-y-5" aria-live="polite">
        <article v-for="task in tasks" :key="task.id" class="rounded-xl bg-[var(--bg-surface-soft)] p-4 space-y-3">
          <div class="flex items-start justify-between gap-3 flex-wrap">
            <div><div class="font-medium text-themed">{{ statusText[task.status] || task.status }}</div><div class="mt-1 text-xs text-themed-muted">{{ formatDate(task.createdAt) }}</div></div>
            <div class="text-sm text-themed-muted">{{ completed(task) }} / {{ task.items.length }} 个实例</div>
          </div>
          <p class="text-xs font-mono text-themed-muted break-all">{{ task.directory }}</p>
          <p v-if="task.error" class="text-sm text-[var(--error)] break-words">{{ task.error }}</p>
          <div v-if="task.status === 'running' && task.items.length === 0" class="text-sm text-themed-muted">正在读取实例列表并准备备份目录…</div>
          <ul v-if="task.items.length" class="divide-y divide-[var(--border-color)]">
            <li v-for="item in task.items" :key="`${item.project}/${item.name}`" class="py-3 text-sm space-y-1">
              <div class="flex justify-between gap-3 flex-wrap"><span class="font-medium text-themed break-all">{{ item.project }} / {{ item.name }}</span><span class="text-themed-muted">{{ statusText[item.status] || item.status }}<template v-if="item.status === 'success'"> · {{ formatSize(item.bytes) }}</template></span></div>
              <div v-if="item.file" class="font-mono text-xs text-themed-muted break-all">{{ item.file }}</div>
              <div v-if="item.error" class="text-[var(--error)] break-words">{{ item.error }}</div>
            </li>
          </ul>
        </article>
      </div>
    </Card>
  </section>
</template>
