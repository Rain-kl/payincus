<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Send,
  RotateCw,
  Megaphone,
  History,
  Sparkles,
  Clock,
  Users,
  AlertCircle,
  FileText,
  Search,
  X,
  Bold,
  Italic,
  List,
  Code,
  Quote,
  Link2,
  ShieldAlert,
  Bell,
  Mail,
  SendHorizontal,
  CheckCircle2,
  ArrowRight
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import { parseMarkdown } from '@/utils/markdown'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Input,
  Select,
  Pagination,
  Table
} from '@/components/ui'
import type { TableColumn, BadgeVariant } from '@/components/ui'

defineOptions({ name: 'AdminBroadcastView' })

const { t, d } = useI18n()
const toast = useToast()

// 视图 Tab 切换
type ActiveViewTab = 'compose' | 'history'
const activeTab = ref<ActiveViewTab>('compose')

// 表单字段
const title = ref('')
const content = ref('')
const announcementType = ref<'system_broadcast' | 'host_broadcast' | 'admin_message'>('system_broadcast')
const priority = ref<'normal' | 'important' | 'urgent'>('normal')
const sending = ref(false)
const showConfirmModal = ref(false)

// 编辑器模式 (编辑 / Markdown 预览)
type EditorMode = 'write' | 'preview'
const editorMode = ref<EditorMode>('write')

// 推送渠道状态指示
const channels = ref({
  web: true,
  email: false,
  telegram: false,
  popup: false
})

// 历史记录数据
interface AnnouncementItem {
  id: number
  type: 'system_broadcast' | 'host_broadcast' | 'admin_message' | 'host_message'
  title: string
  content: string
  recipientCount: number
  hostId?: number | null
  targetUserId?: number | null
  instanceId?: number | null
  createdAt: string
  sender: { id: number; username: string }
  host?: { id: number; name: string } | null
}

const historyList = ref<AnnouncementItem[]>([])
const historyLoading = ref(false)
const historyPage = ref(1)
const historyPageSize = ref(10)
const historyTotal = ref(0)
const historyTotalPages = ref(0)
const selectedType = ref<string>('all')
const searchKeyword = ref<string>('')

// 详情查看弹窗
const selectedAnnouncement = ref<AnnouncementItem | null>(null)

// 撤回 / 更正弹窗
const showRecallModal = ref(false)
const recallTarget = ref<AnnouncementItem | null>(null)

// 快捷模版预设
interface TemplatePreset {
  id: string
  name: string
  title: string
  type: 'system_broadcast' | 'host_broadcast' | 'admin_message'
  priority: 'normal' | 'important' | 'urgent'
  content: string
}

const templatePresets: TemplatePreset[] = [
  {
    id: 'maintenance',
    name: '例行维护',
    title: '【维护通知】核心系统/节点例行维护预告',
    type: 'host_broadcast',
    priority: 'important',
    content: `### 尊敬的用户：

为了提供更稳定优质的云算力与网络服务，平台计划于 **[请替换维护时间]** 进行例行系统升级与网络维护。

- **维护范围**：核心网关与部分宿主机节点
- **预计耗时**：约 30 分钟
- **业务影响**：维护期间可能会出现短暂网络抖动，实例数据与配置不受任何影响

如遇异常情况，请随时提交工单与运维团队联系。感谢您的支持与理解！`
  },
  {
    id: 'upgrade',
    name: '版本升级',
    title: '【版本发布】PayIncus 平台功能升级通报',
    type: 'system_broadcast',
    priority: 'normal',
    content: `### 全新特性与体验优化发布

本次系统升级主要包含以下关键特性与稳定性改进：

1. **网络与端口**：优化 NAT IPv4 端口分配与实时链路状态探测
2. **交互体验**：全面接入现代化 Apple HIG 设计语言与丝滑微交互
3. **安全加固**：提升会话凭证保护与敏感操作二次验证安全等级

欢迎体验，如有任何使用建议欢迎在工单系统中向我们反馈！`
  },
  {
    id: 'security',
    name: '安全建议',
    title: '【安全建议】关于加强账户与实例安全防护的提醒',
    type: 'system_broadcast',
    priority: 'important',
    content: `### 亲爱的用户：

近期公网自动化扫描与暴力探测较为频繁。为确保您的计算资产与数据安全，我们建议您采取以下安全加固措施：

- **启用两步验证 (2FA)**：请在账户安全设置中绑定 TOTP 动态验证码
- **使用 SSH 密钥认证**：禁用密码暴力探测，配置高强度 SSH Key
- **检查防火墙与暴露端口**：关闭不必要的公网开放端口

PayIncus 团队持续守护您的计算安全。`
  },
  {
    id: 'urgent',
    name: '紧急通告',
    title: '【紧急通告】关于部分上游网络链路波动的处置进展',
    type: 'system_broadcast',
    priority: 'urgent',
    content: `### 紧急网络事件通报

我们监测到部分上游出口运营商链路出现突发异常波动：

- **受影响区域**：部分海外访问节点延迟上升或丢包
- **处置方案**：网络调度中心已介入，并已平滑切换至备用 BGP 高速路由
- **当前状态**：核心链路正在快速收敛恢复中

我们将密切关注网络健康度，最新处置进展将在此保持同步。`
  }
]

// 插入 Markdown 语法辅助
function insertMarkdown(prefix: string, suffix = '') {
  const textarea = document.getElementById('content-textarea') as HTMLTextAreaElement | null
  if (!textarea) {
    content.value += `${prefix}${suffix}`
    return
  }
  const start = textarea.selectionStart
  const end = textarea.selectionEnd
  const selected = content.value.substring(start, end)
  const replacement = `${prefix}${selected || '文本'}${suffix}`
  content.value = content.value.substring(0, start) + replacement + content.value.substring(end)
  textarea.focus()
  setTimeout(() => {
    textarea.setSelectionRange(start + prefix.length, start + replacement.length - suffix.length)
  }, 0)
}

// 应用预设模版
function applyTemplate(tpl: TemplatePreset) {
  title.value = tpl.title
  content.value = tpl.content
  announcementType.value = tpl.type
  priority.value = tpl.priority
  editorMode.value = 'write'
  toast.info(`已填入模版「${tpl.name}」`)
}

// 清空表单
function clearForm() {
  title.value = ''
  content.value = ''
  announcementType.value = 'system_broadcast'
  priority.value = 'normal'
  editorMode.value = 'write'
}

// 预览 HTML
const previewHtml = computed(() => {
  return parseMarkdown(content.value || '*暂无内容预览，请在编辑区域输入正文内容*')
})

// 历史广播表格列定义
const historyColumns: TableColumn<AnnouncementItem>[] = [
  { key: 'type', title: '类型', width: '130px', align: 'left' },
  { key: 'title', title: '标题与摘要', align: 'left' },
  { key: 'sender', title: '发布人', width: '130px', align: 'left' },
  { key: 'recipientCount', title: '触达人数', width: '110px', align: 'center' },
  { key: 'createdAt', title: '发布时间', width: '170px', align: 'left' },
  { key: 'actions', title: '操作', width: '130px', align: 'right' }
]

// 过滤后的历史记录（支持按关键字搜索）
const filteredHistoryList = computed(() => {
  if (!searchKeyword.value.trim()) {
    return historyList.value
  }
  const kw = searchKeyword.value.trim().toLowerCase()
  return historyList.value.filter(item =>
    item.title.toLowerCase().includes(kw) ||
    item.content.toLowerCase().includes(kw) ||
    item.sender.username.toLowerCase().includes(kw)
  )
})

// 获取公告类型 Badge 变体
const getTypeBadgeVariant = (type: string): BadgeVariant => {
  switch (type) {
    case 'system_broadcast':
      return 'info'
    case 'host_broadcast':
      return 'warning'
    case 'admin_message':
      return 'success'
    case 'host_message':
      return 'default'
    default:
      return 'default'
  }
}

// 获取类型本地化文案
const getTypeName = (type: string): string => {
  switch (type) {
    case 'system_broadcast':
      return t('admin.broadcast.types.system_broadcast')
    case 'host_broadcast':
      return t('admin.broadcast.types.host_broadcast')
    case 'admin_message':
      return t('admin.broadcast.types.admin_message')
    case 'host_message':
      return t('admin.broadcast.types.host_message')
    default:
      return type
  }
}

// 加载历史广播
async function loadHistory(page = 1) {
  historyLoading.value = true
  try {
    const result = await api.announcements.list({
      page,
      pageSize: historyPageSize.value,
      type: selectedType.value === 'all' ? undefined : (selectedType.value as any)
    })
    historyList.value = result.items
    historyPage.value = result.page
    historyTotal.value = result.total
    historyTotalPages.value = result.totalPages
  } catch (_err: any) {
    toast.error(t('common.loadFailed'))
  } finally {
    historyLoading.value = false
  }
}

// 切换类型过滤
function handleTypeFilterChange(type: string) {
  selectedType.value = type
  historyPage.value = 1
  loadHistory(1)
}

// 分页变化
function handlePageChange(newPage: number) {
  historyPage.value = newPage
  loadHistory(newPage)
}

// 详情查看
function viewDetail(item: AnnouncementItem) {
  selectedAnnouncement.value = item
}

function closeDetail() {
  selectedAnnouncement.value = null
}

// 将历史公告载入编辑器以重新编辑/发布
function reuseAsTemplate(item: AnnouncementItem) {
  title.value = item.title
  content.value = item.content
  activeTab.value = 'compose'
  editorMode.value = 'write'
  closeDetail()
  toast.success('已载入编辑器，可在此基础上修改后重新发布')
}

// 触发撤回确认弹窗
function openRecallModal(item: AnnouncementItem) {
  recallTarget.value = item
  showRecallModal.value = true
}

function closeRecallModal() {
  showRecallModal.value = false
  recallTarget.value = null
}

// 撤回 / 发布更正说明
function proceedToCorrection() {
  if (!recallTarget.value) return
  const item = recallTarget.value
  title.value = `【更正说明】关于此前「${item.title}」的补充通报`
  content.value = `### 关于此前广播公告的更正说明\n\n针对此前于 ${d(new Date(item.createdAt), 'short')} 发布的公告（原标题：${item.title}），现做如下更正与补充说明：\n\n- **更正内容**：[请在此填写最新更正事实]\n- **后续安排**：[请在此说明后续动作]\n\n因历史站内信已即时送达用户收件箱，特发此说明通告。`
  priority.value = 'important'
  closeRecallModal()
  closeDetail()
  activeTab.value = 'compose'
  editorMode.value = 'write'
  toast.info('已生成更正公告草稿，请补充后发送')
}

// 预检验并打开发送确认对话框
function handlePreSend() {
  if (!title.value.trim()) {
    toast.error(t('admin.broadcast.titleRequired'))
    return
  }
  if (!content.value.trim()) {
    toast.error(t('admin.broadcast.contentRequired'))
    return
  }
  if (title.value.length > 200) {
    toast.error(t('admin.broadcast.titleTooLong'))
    return
  }
  if (content.value.length > 5000) {
    toast.error(t('admin.broadcast.contentTooLong'))
    return
  }
  showConfirmModal.value = true
}

// 执行全站广播发送
async function confirmSendBroadcast() {
  sending.value = true
  try {
    const result = await api.inbox.broadcast({
      title: title.value.trim(),
      content: content.value.trim()
    })
    toast.success(t('admin.broadcast.sendSuccess', { count: result.count }))
    clearForm()
    showConfirmModal.value = false
    // 刷新历史记录
    loadHistory(1)
  } catch (err: any) {
    toast.error(t('admin.broadcast.sendFailed') + ': ' + (err?.message || String(err)))
  } finally {
    sending.value = false
  }
}

onMounted(() => {
  loadHistory(1)
})
</script>

<template>
  <div class="kawaii-page nimbus-view space-y-6 animate-fade-in max-w-6xl mx-auto pb-12">
    <!-- Apple HIG 标准顶栏 -->
    <PageHeader
      :title="t('admin.broadcast.title')"
      :description="t('admin.broadcast.description')"
    >
      <template #actions>
        <!-- 视图分段选择器 (Segmented Control) -->
        <div class="inline-flex p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <button
            type="button"
            class="inline-flex items-center gap-1.5 px-4 py-1.5 rounded-full text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
            :class="activeTab === 'compose'
              ? 'bg-[var(--accent)] text-white shadow-sm'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="activeTab = 'compose'"
          >
            <Megaphone class="w-3.5 h-3.5 stroke-[2]" />
            <span>发布公告</span>
          </button>
          <button
            type="button"
            class="inline-flex items-center gap-1.5 px-4 py-1.5 rounded-full text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-95"
            :class="activeTab === 'history'
              ? 'bg-[var(--accent)] text-white shadow-sm'
              : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="activeTab = 'history'"
          >
            <History class="w-3.5 h-3.5 stroke-[2]" />
            <span>历史广播</span>
            <span
              v-if="historyTotal > 0"
              class="ml-1 px-1.5 py-0.2 rounded-full text-[10px] font-mono tabular-nums leading-none"
              :class="activeTab === 'history' ? 'bg-white/20 text-white' : 'bg-[var(--bg-tertiary)] text-[var(--text-secondary)]'"
            >
              {{ historyTotal }}
            </span>
          </button>
        </div>

        <!-- 刷新数据按钮 -->
        <Button
          variant="secondary"
          size="sm"
          :title="t('common.refresh')"
          @click="loadHistory(historyPage)"
        >
          <template #icon>
            <RotateCw class="w-3.5 h-3.5" :class="historyLoading ? 'animate-spin' : ''" />
          </template>
        </Button>
      </template>
    </PageHeader>

    <!-- TAB 1: 发布公告 (Compose) -->
    <div v-show="activeTab === 'compose'" class="space-y-6">
      <!-- 快捷模版预设横幅 -->
      <Card padding="sm">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <div class="flex items-center gap-2 shrink-0">
            <span class="inline-flex items-center justify-center w-7 h-7 rounded-full bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)]">
              <Sparkles class="w-4 h-4" />
            </span>
            <span class="text-xs font-medium text-[var(--text-primary)]">快捷公告模板：</span>
          </div>
          <div class="flex items-center gap-2 flex-wrap">
            <button
              v-for="tpl in templatePresets"
              :key="tpl.id"
              type="button"
              class="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs font-medium border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:border-[var(--accent)] hover:bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))] transition-all duration-200 active:scale-95"
              @click="applyTemplate(tpl)"
            >
              <FileText class="w-3 h-3 stroke-[2] text-[var(--text-tertiary)]" />
              <span>{{ tpl.name }}</span>
            </button>
          </div>
        </div>
      </Card>

      <!-- 主要发布表单卡片 -->
      <Card padding="md">
        <form class="space-y-6" @submit.prevent="handlePreSend">
          <!-- 渠道与重要性规格栏 -->
          <div class="grid grid-cols-1 md:grid-cols-3 gap-4 pb-4 border-b border-[var(--border-color)]">
            <!-- 分类选择 (Select 组件复用) -->
            <div>
              <Select
                v-model="announcementType"
                label="公告类别"
                :options="[
                  { label: '全站公告 (system_broadcast)', value: 'system_broadcast' },
                  { label: '节点通报 (host_broadcast)', value: 'host_broadcast' },
                  { label: '重要私信 (admin_message)', value: 'admin_message' }
                ]"
                size="md"
              />
            </div>

            <!-- 重要性级别 -->
            <div class="space-y-1.5">
              <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                重要性级别
              </label>
              <div class="flex items-center gap-2">
                <button
                  type="button"
                  class="flex-1 inline-flex items-center justify-center gap-1.5 min-h-[44px] px-2 rounded-xl border text-xs font-medium transition-all duration-200 active:scale-95"
                  :class="priority === 'normal'
                    ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="priority = 'normal'"
                >
                  <Bell class="w-3.5 h-3.5" />
                  <span>普通</span>
                </button>
                <button
                  type="button"
                  class="flex-1 inline-flex items-center justify-center gap-1.5 min-h-[44px] px-2 rounded-xl border text-xs font-medium transition-all duration-200 active:scale-95"
                  :class="priority === 'important'
                    ? 'border-[var(--warning)] bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] font-semibold'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="priority = 'important'"
                >
                  <AlertCircle class="w-3.5 h-3.5" />
                  <span>重要</span>
                </button>
                <button
                  type="button"
                  class="flex-1 inline-flex items-center justify-center gap-1.5 min-h-[44px] px-2 rounded-xl border text-xs font-medium transition-all duration-200 active:scale-95"
                  :class="priority === 'urgent'
                    ? 'border-[var(--error)] bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] font-semibold'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="priority = 'urgent'"
                >
                  <ShieldAlert class="w-3.5 h-3.5" />
                  <span>紧急</span>
                </button>
              </div>
            </div>

            <!-- 推送渠道指示 -->
            <div class="space-y-1.5">
              <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                分发渠道覆盖
              </label>
              <div class="flex items-center gap-1.5 flex-wrap min-h-[44px]">
                <span class="inline-flex items-center gap-1 px-2.5 py-1.5 rounded-xl border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--accent)_10%,var(--bg-surface))] text-[var(--accent)] text-xs font-medium">
                  <CheckCircle2 class="w-3.5 h-3.5" />
                  <span>站内信 (全员)</span>
                </span>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 px-2 py-1.5 rounded-xl border text-xs font-medium transition-all duration-200 active:scale-95"
                  :class="channels.email
                    ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_10%,var(--bg-surface))] text-[var(--accent)]'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-tertiary)]'"
                  title="由后台按用户绑定的有效邮箱投递"
                  @click="channels.email = !channels.email"
                >
                  <Mail class="w-3.5 h-3.5" />
                  <span>邮件</span>
                </button>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 px-2 py-1.5 rounded-xl border text-xs font-medium transition-all duration-200 active:scale-95"
                  :class="channels.telegram
                    ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_10%,var(--bg-surface))] text-[var(--accent)]'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-tertiary)]'"
                  title="面向绑定了 Telegram Bot 的用户投递"
                  @click="channels.telegram = !channels.telegram"
                >
                  <SendHorizontal class="w-3.5 h-3.5" />
                  <span>TG</span>
                </button>
              </div>
            </div>
          </div>

          <!-- 公告标题 (Input 组件复用) -->
          <div>
            <div class="flex items-center justify-between mb-1">
              <label for="broadcast-title-input" class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('admin.broadcast.messageTitle') }}
              </label>
              <span class="font-mono text-xs tabular-nums text-[var(--text-tertiary)]">
                {{ title.length }}/200
              </span>
            </div>
            <Input
              id="broadcast-title-input"
              v-model="title"
              type="text"
              :maxlength="200"
              :placeholder="t('admin.broadcast.titlePlaceholder')"
              clearable
            />
          </div>

          <!-- 正文编辑与 Markdown 实时预览切换 -->
          <div class="space-y-2">
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <label for="content-textarea" class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('admin.broadcast.messageContent') }} (支持 Markdown 语法)
              </label>
              <div class="flex items-center gap-3">
                <span class="font-mono text-xs tabular-nums text-[var(--text-tertiary)]">
                  {{ content.length }}/5000
                </span>
                <!-- 编辑/预览 Tab 切换 -->
                <div class="inline-flex p-0.5 rounded-lg bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                  <button
                    type="button"
                    class="px-2.5 py-1 rounded-md text-xs font-medium transition-all duration-200"
                    :class="editorMode === 'write' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="editorMode = 'write'"
                  >
                    编辑
                  </button>
                  <button
                    type="button"
                    class="px-2.5 py-1 rounded-md text-xs font-medium transition-all duration-200"
                    :class="editorMode === 'preview' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                    @click="editorMode = 'preview'"
                  >
                    实时预览
                  </button>
                </div>
              </div>
            </div>

            <!-- Markdown 快速格式化工具栏 (仅在编辑模式展示) -->
            <div
              v-show="editorMode === 'write'"
              class="flex items-center gap-1 p-1.5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] flex-wrap"
            >
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="加粗"
                @click="insertMarkdown('**', '**')"
              >
                <Bold class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="斜体"
                @click="insertMarkdown('*', '*')"
              >
                <Italic class="w-3.5 h-3.5" />
              </button>
              <span class="h-3 w-px bg-[var(--border-color)] mx-1"></span>
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="无序列表"
                @click="insertMarkdown('- ')"
              >
                <List class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="引用块"
                @click="insertMarkdown('> ')"
              >
                <Quote class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="代码块"
                @click="insertMarkdown('```\n', '\n```')"
              >
                <Code class="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                class="p-1.5 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all"
                title="链接"
                @click="insertMarkdown('[链接描述](', ')')"
              >
                <Link2 class="w-3.5 h-3.5" />
              </button>
            </div>

            <!-- 编辑模式：Textarea -->
            <div v-show="editorMode === 'write'">
              <textarea
                id="content-textarea"
                v-model="content"
                rows="9"
                maxlength="5000"
                :placeholder="t('admin.broadcast.contentPlaceholder')"
                class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] focus:border-transparent transition-all duration-200 resize-none font-sans leading-relaxed"
              ></textarea>
            </div>

            <!-- 预览模式：Markdown 渲染 -->
            <div
              v-show="editorMode === 'preview'"
              class="min-h-[220px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-5 text-sm overflow-y-auto leading-relaxed"
            >
              <div class="prose max-w-none text-sm text-[var(--text-primary)]" v-html="previewHtml"></div>
            </div>
          </div>

          <!-- 底部操作栏 -->
          <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pt-2">
            <!-- 左侧提示与清空 -->
            <div class="flex items-center gap-3">
              <Button
                v-if="title || content"
                variant="ghost"
                size="sm"
                @click="clearForm"
              >
                清空重填
              </Button>
              <span class="text-xs text-[var(--text-tertiary)]">
                {{ t('admin.broadcast.hint') }}
              </span>
            </div>

            <!-- 右侧发送按钮 -->
            <div class="flex items-center gap-3">
              <Button
                type="submit"
                variant="primary"
                size="md"
                :loading="sending"
                :disabled="sending || !title.trim() || !content.trim()"
              >
                <template #icon>
                  <Send class="w-4 h-4 stroke-[2]" />
                </template>
                <span>{{ sending ? t('common.sending') : t('admin.broadcast.send') }}</span>
              </Button>
            </div>
          </div>
        </form>
      </Card>

      <!-- 提示卡片 -->
      <div class="flex gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))] p-4 items-start">
        <span class="inline-flex items-center justify-center w-6 h-6 rounded-full bg-[color-mix(in_srgb,var(--accent)_18%,var(--bg-surface))] text-[var(--accent)] shrink-0 mt-0.5">
          <AlertCircle class="w-4 h-4" />
        </span>
        <div class="text-xs leading-relaxed text-[var(--text-secondary)] space-y-1">
          <p class="font-medium text-[var(--text-primary)]">广播发送机制说明：</p>
          <p>• 广播将即时写入全站状态为「正常」的用户收件箱，并在其客户端站内信中心展示红点徽标。</p>
          <p>• 若需要修改已发出的广播，可在历史列表中点击「查看详情」并使用「以此为模板发布更正说明」。</p>
        </div>
      </div>
    </div>

    <!-- TAB 2: 历史广播 (History) -->
    <div v-show="activeTab === 'history'" class="space-y-4">
      <!-- 过滤与搜索栏 -->
      <Card padding="sm">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <!-- 类型胶囊选择器 -->
          <div class="flex items-center gap-1.5 flex-wrap">
            <button
              type="button"
              class="px-3 py-1 rounded-full text-xs font-medium transition-all duration-200 active:scale-95"
              :class="selectedType === 'all'
                ? 'bg-[var(--accent)] text-white shadow-sm'
                : 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)]'"
              @click="handleTypeFilterChange('all')"
            >
              全部类型
            </button>
            <button
              type="button"
              class="px-3 py-1 rounded-full text-xs font-medium transition-all duration-200 active:scale-95"
              :class="selectedType === 'system_broadcast'
                ? 'bg-[var(--accent)] text-white shadow-sm'
                : 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)]'"
              @click="handleTypeFilterChange('system_broadcast')"
            >
              全站公告
            </button>
            <button
              type="button"
              class="px-3 py-1 rounded-full text-xs font-medium transition-all duration-200 active:scale-95"
              :class="selectedType === 'host_broadcast'
                ? 'bg-[var(--accent)] text-white shadow-sm'
                : 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)]'"
              @click="handleTypeFilterChange('host_broadcast')"
            >
              节点通知
            </button>
            <button
              type="button"
              class="px-3 py-1 rounded-full text-xs font-medium transition-all duration-200 active:scale-95"
              :class="selectedType === 'admin_message'
                ? 'bg-[var(--accent)] text-white shadow-sm'
                : 'bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] border border-[var(--border-color)]'"
              @click="handleTypeFilterChange('admin_message')"
            >
              管理员私信
            </button>
          </div>

          <!-- 搜索输入框 -->
          <div class="w-full sm:w-64">
            <Input
              v-model="searchKeyword"
              size="sm"
              placeholder="搜索标题或内容..."
              clearable
            >
              <template #prefix>
                <Search class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>
        </div>
      </Card>

      <!-- 历史记录表格 (桌面端定宽无溢出 + 移动端自适应卡片) -->
      <Table
        :columns="historyColumns"
        :data="filteredHistoryList"
        :loading="historyLoading"
        :fixed="true"
      >
        <!-- 自定义空状态 (复用 EmptyState 组件) -->
        <template #empty>
          <EmptyState
            title="暂无广播记录"
            description="当前筛选条件下未查询到历史广播与通知记录"
          />
        </template>

        <!-- 单元格：类型 -->
        <template #cell-type="{ row }">
          <Badge :variant="getTypeBadgeVariant(row.type)" size="sm">
            {{ getTypeName(row.type) }}
          </Badge>
        </template>

        <!-- 单元格：标题与摘要 -->
        <template #cell-title="{ row }">
          <div class="min-w-0 pr-2 cursor-pointer group" @click="viewDetail(row)">
            <h4 class="font-medium text-sm text-[var(--text-primary)] group-hover:text-[var(--accent)] transition-colors truncate">
              {{ row.title }}
            </h4>
            <p class="text-xs text-[var(--text-secondary)] line-clamp-1 mt-0.5 max-w-xl">
              {{ row.content }}
            </p>
          </div>
        </template>

        <!-- 单元格：发布人 -->
        <template #cell-sender="{ row }">
          <span class="text-xs text-[var(--text-secondary)] truncate block">
            {{ row.sender?.username || '系统管理员' }}
          </span>
        </template>

        <!-- 单元格：触达人数 -->
        <template #cell-recipientCount="{ row }">
          <span class="inline-flex items-center gap-1 font-mono text-xs tabular-nums text-[var(--text-secondary)]">
            <Users class="w-3 h-3 text-[var(--text-tertiary)]" />
            <span>{{ row.recipientCount }}</span>
          </span>
        </template>

        <!-- 单元格：发布时间 -->
        <template #cell-createdAt="{ row }">
          <span class="inline-flex items-center gap-1 font-mono text-xs tabular-nums text-[var(--text-secondary)]">
            <Clock class="w-3 h-3 text-[var(--text-tertiary)]" />
            <span>{{ d(new Date(row.createdAt), 'short') }}</span>
          </span>
        </template>

        <!-- 单元格：操作 -->
        <template #cell-actions="{ row }">
          <div class="flex items-center justify-end gap-1.5">
            <Button
              variant="secondary"
              size="sm"
              @click="viewDetail(row)"
            >
              详情
            </Button>
          </div>
        </template>

        <!-- 移动端响应式卡片视图 -->
        <template #mobile="{ row }">
          <div
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-3 cursor-pointer transition-all duration-200 active:scale-[0.99] shadow-sm"
            @click="viewDetail(row)"
          >
            <div class="flex items-start justify-between gap-2">
              <Badge :variant="getTypeBadgeVariant(row.type)" size="sm">
                {{ getTypeName(row.type) }}
              </Badge>
              <span class="font-mono text-xs tabular-nums text-[var(--text-tertiary)]">
                {{ d(new Date(row.createdAt), 'short') }}
              </span>
            </div>
            <div>
              <h4 class="font-semibold text-sm text-[var(--text-primary)]">
                {{ row.title }}
              </h4>
              <p class="text-xs text-[var(--text-secondary)] line-clamp-2 mt-1">
                {{ row.content }}
              </p>
            </div>
            <div class="flex items-center justify-between pt-2 border-t border-[var(--border-color)] text-xs text-[var(--text-secondary)]">
              <span class="flex items-center gap-1 font-mono tabular-nums">
                <Users class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
                <span>{{ row.recipientCount }} 人已送达</span>
              </span>
              <span class="text-[var(--accent)] font-medium inline-flex items-center gap-1">
                查看详情
                <ArrowRight class="w-3 h-3" />
              </span>
            </div>
          </div>
        </template>

        <!-- 表格底部：分页器 (复用 Pagination 组件) -->
        <template #pagination>
          <div v-if="historyTotalPages > 1" class="w-full">
            <Pagination
              :page="historyPage"
              :page-size="historyPageSize"
              :total="historyTotal"
              @update:page="handlePageChange"
            />
          </div>
        </template>
      </Table>
    </div>

    <!-- 弹窗 1: 发送确认 Apple HIG 对话框 -->
    <Teleport to="body">
      <div
        v-if="showConfirmModal"
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
      >
        <!-- 毛玻璃半透明遮罩 -->
        <div
          class="fixed inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
          @click="showConfirmModal = false"
        ></div>

        <!-- 弹窗主体卡片 (18px rounded-2xl) -->
        <div class="relative w-full max-w-lg rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-xl transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] space-y-5 animate-scale-in">
          <!-- 头部 -->
          <div class="flex items-start gap-3">
            <span class="inline-flex items-center justify-center w-10 h-10 rounded-full bg-[color-mix(in_srgb,var(--warning)_15%,var(--bg-surface))] text-[var(--warning)] shrink-0">
              <AlertCircle class="w-5 h-5 stroke-[2]" />
            </span>
            <div>
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                确认发布全站广播？
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                此操作将立即向所有状态为「正常」的活跃用户发送站内通知。
              </p>
            </div>
          </div>

          <!-- 内容摘要预览框 -->
          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 space-y-2 text-xs">
            <div class="flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">公告标题：</span>
              <span class="font-medium text-[var(--text-primary)] truncate max-w-[280px]">{{ title }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">类别分类：</span>
              <span class="font-medium text-[var(--text-primary)]">{{ getTypeName(announcementType) }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">重要级别：</span>
              <span class="font-medium" :class="priority === 'urgent' ? 'text-[var(--error)]' : priority === 'important' ? 'text-[var(--warning)]' : 'text-[var(--accent)]'">
                {{ priority === 'urgent' ? '紧急通报' : priority === 'important' ? '重要提醒' : '普通通报' }}
              </span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">推送覆盖：</span>
              <span class="font-medium text-[var(--text-primary)]">全站活跃用户终端</span>
            </div>
          </div>

          <!-- 操作按钮组 -->
          <div class="flex items-center justify-end gap-3 pt-2">
            <Button
              variant="secondary"
              size="md"
              :disabled="sending"
              @click="showConfirmModal = false"
            >
              {{ t('common.cancel') }}
            </Button>
            <Button
              variant="primary"
              size="md"
              :loading="sending"
              @click="confirmSendBroadcast"
            >
              {{ t('common.confirm') }}
            </Button>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- 弹窗 2: 公告详情查看 Apple HIG 模态框 -->
    <Teleport to="body">
      <div
        v-if="selectedAnnouncement"
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
      >
        <!-- 遮罩 -->
        <div
          class="fixed inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
          @click="closeDetail"
        ></div>

        <!-- 弹窗主体 -->
        <div class="relative w-full max-w-2xl max-h-[85vh] flex flex-col rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-2xl transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] animate-scale-in overflow-hidden">
          <!-- 弹窗顶栏 -->
          <div class="flex items-center justify-between px-6 py-4 border-b border-[var(--border-color)] bg-[var(--bg-surface)] shrink-0">
            <div class="flex items-center gap-2.5 min-w-0 pr-4">
              <Badge :variant="getTypeBadgeVariant(selectedAnnouncement.type)" size="sm">
                {{ getTypeName(selectedAnnouncement.type) }}
              </Badge>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)] truncate">
                {{ selectedAnnouncement.title }}
              </h3>
            </div>
            <button
              type="button"
              class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors"
              :title="t('common.close')"
              @click="closeDetail"
            >
              <X class="w-4 h-4" />
            </button>
          </div>

          <!-- 弹窗元信息栏 -->
          <div class="flex flex-wrap items-center gap-4 px-6 py-3 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-xs text-[var(--text-secondary)] shrink-0">
            <span class="inline-flex items-center gap-1.5">
              <span class="text-[var(--text-tertiary)]">发送人:</span>
              <span class="font-medium text-[var(--text-primary)]">{{ selectedAnnouncement.sender?.username || '系统管理员' }}</span>
            </span>
            <span class="inline-flex items-center gap-1 font-mono tabular-nums">
              <Users class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
              <span>{{ t('admin.broadcast.recipients', { count: selectedAnnouncement.recipientCount }) }}</span>
            </span>
            <span class="inline-flex items-center gap-1 font-mono tabular-nums">
              <Clock class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
              <span>{{ d(new Date(selectedAnnouncement.createdAt), 'long') }}</span>
            </span>
          </div>

          <!-- 弹窗正文区域 (Markdown 渲染) -->
          <div class="p-6 overflow-y-auto flex-1 leading-relaxed">
            <div
              class="prose max-w-none text-sm text-[var(--text-primary)]"
              v-html="parseMarkdown(selectedAnnouncement.content)"
            ></div>
          </div>

          <!-- 弹窗底部操作栏 -->
          <div class="flex items-center justify-between px-6 py-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] shrink-0">
            <!-- 撤回 / 更正入口 -->
            <Button
              variant="ghost"
              size="sm"
              @click="openRecallModal(selectedAnnouncement)"
            >
              撤回与更正说明
            </Button>

            <div class="flex items-center gap-3">
              <Button
                variant="secondary"
                size="sm"
                @click="reuseAsTemplate(selectedAnnouncement)"
              >
                以此为模版再次发布
              </Button>
              <Button
                variant="primary"
                size="sm"
                @click="closeDetail"
              >
                {{ t('common.close') }}
              </Button>
            </div>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- 弹窗 3: 撤回与更正说明对话框 -->
    <Teleport to="body">
      <div
        v-if="showRecallModal && recallTarget"
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
      >
        <div
          class="fixed inset-0 bg-black/40 backdrop-blur-sm transition-opacity"
          @click="closeRecallModal"
        ></div>

        <div class="relative w-full max-w-md rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-xl space-y-4 animate-scale-in">
          <div class="flex items-start gap-3">
            <span class="inline-flex items-center justify-center w-10 h-10 rounded-full bg-[color-mix(in_srgb,var(--warning)_15%,var(--bg-surface))] text-[var(--warning)] shrink-0">
              <AlertCircle class="w-5 h-5 stroke-[2]" />
            </span>
            <div>
              <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                广播撤回与更正机制
              </h3>
              <p class="text-xs text-[var(--text-secondary)] mt-1 leading-relaxed">
                全站广播已即时投递至所有在线用户的站内信箱。根据平台不可篡改审计规范，已分发的消息无法从用户终端物理销毁。
              </p>
            </div>
          </div>

          <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs text-[var(--text-secondary)] leading-relaxed">
            <p class="font-medium text-[var(--text-primary)] mb-1">推荐处置方式：</p>
            <p>发布一条包含前序公告关联引用的「更正说明通报」，明确勘误内容或最新处置方案。</p>
          </div>

          <div class="flex items-center justify-end gap-3 pt-2">
            <Button
              variant="secondary"
              size="sm"
              @click="closeRecallModal"
            >
              取消
            </Button>
            <Button
              variant="primary"
              size="sm"
              @click="proceedToCorrection"
            >
              生成并发布更正通报
            </Button>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
@keyframes scaleIn {
  from {
    opacity: 0;
    transform: scale(0.96);
  }
  to {
    opacity: 1;
    transform: scale(1);
  }
}

.animate-scale-in {
  animation: scaleIn 0.2s cubic-bezier(0.25, 0.1, 0.25, 1);
}

@media (prefers-reduced-motion: reduce) {
  .nimbus-view *,
  .nimbus-view *::before,
  .nimbus-view *::after {
    transition-duration: 0.001ms !important;
    animation-duration: 0.001ms !important;
  }
}
</style>
