<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useToast } from '@/stores/toast'
import { translateError } from '@/utils/errorHandler'
import { helpPath, mailPath } from '@/utils/app-paths'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  Table,
  EmptyState,
  Input
} from '@/components/ui'
import type { TableColumn, BadgeVariant } from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'
import {
  ExternalLink,
  FileText,
  RefreshCw,
  Plus,
  Edit3,
  Trash2,
  Key,
  Copy,
  Check,
  Eye,
  EyeOff,
  AlertTriangle
} from 'lucide-vue-next'

interface MailAccountItem {
  id: number
  email: string
  username: string
  displayName: string | null
  diskLimitMb: number
  diskUsedMb: number
  isAdmin: boolean
  createdAt: string
}

interface MailDomainDetail {
  id: number
  domain: string
  status: string
  diskLimitMb?: number
  diskUsedMb?: number
  verifiedAt: string | null
  createdAt: string
  adminUsername: string | null
  adminPasswordConfigured: boolean
  sourceCode: string
  accounts?: MailAccountItem[]
}

interface DnsRecordItem {
  type: string
  record: string
  value: string
  prio?: number
}

interface DnsConfigData {
  verified: boolean
  txtRecord?: string
  dnsRecords?: DnsRecordItem[]
  mxRecords?: string[]
  spfRecord?: string
  dkimRecord?: string
  cnameRecords?: Array<{ record: string; value: string }>
}

const route = useRoute()
const router = useRouter()
const { t } = useI18n()
const toast = useToast()

// TAB 状态
const activeTab = ref<'accounts' | 'dns' | 'settings'>('accounts')

// 加载状态
const loading = ref(true)
const actionLoading = ref('')

// 域名数据
const domain = ref<MailDomainDetail | null>(null)
const dnsConfig = ref<DnsConfigData | null>(null)
const adminPassword = ref<string | null>(null)

const domainId = computed(() => parseInt(route.params.id as string))

// 域名下的邮箱账户列表
const domainAccounts = computed<MailAccountItem[]>(() => {
  return domain.value?.accounts || []
})

// 表格列定义
const accountColumns = computed<TableColumn<MailAccountItem>[]>(() => [
  { key: 'email', title: t('mail.emailAddress'), width: '30%' },
  { key: 'displayName', title: t('mail.displayName'), width: '22%' },
  { key: 'diskLimitMb', title: t('mail.diskLimit'), width: '16%' },
  { key: 'isAdmin', title: t('profile.account.role'), width: '14%' },
  { key: 'actions', title: t('common.actions'), width: '18%', align: 'right' }
])

// 过滤后的 DNS 记录（排除验证记录，因为它单独显示）
const filteredDnsRecords = computed<DnsRecordItem[]>(() => {
  if (!dnsConfig.value?.dnsRecords) return []
  return dnsConfig.value.dnsRecords.filter(
    (r) => r.record !== 'workspace-verification'
  )
})

onMounted(async () => {
  await loadDomain()
})

async function loadDomain() {
  loading.value = true
  try {
    const res = await api.mail.getDomain(domainId.value)
    domain.value = res.domain
    adminPassword.value = null
    showPassword.value = false
  } catch (err: any) {
    toast.error(translateError(err))
    router.push(mailPath())
  } finally {
    loading.value = false
  }
}

async function loadDnsConfig() {
  try {
    const res = await api.mail.getDomainDns(domainId.value)
    dnsConfig.value = res
  } catch (err: any) {
    toast.error(translateError(err))
  }
}

async function verifyDomain() {
  actionLoading.value = 'verify'
  try {
    const res = await api.mail.verifyDomain(domainId.value)
    if (res.verified) {
      toast.success(t('mail.domainVerified'))
      await loadDomain()
    } else {
      toast.info(t('mail.domainNotVerified'))
    }
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// 删除域名
async function deleteDomain() {
  if (!confirm(t('mail.deleteDomainConfirm', { domain: domain.value?.domain }))) return

  actionLoading.value = 'delete-domain'
  try {
    await api.mail.deleteDomain(domainId.value)
    toast.success(t('mail.domainDeleted'))
    router.push(mailPath())
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// 复制状态
const copiedKey = ref<string>('')
const showPassword = ref(false)
const passwordLoading = ref(false)

// Webmail 登录地址
const webmailUrl = computed(() => {
  if (!domain.value?.sourceCode) return ''
  const code = domain.value.sourceCode
  // us -> us2, 其他 -> {code}1
  const subdomain = code === 'us' ? 'us2' : `${code}1`
  return `https://${subdomain}.workspace.org/`
})

function copyValue(text: string, key: string) {
  navigator.clipboard.writeText(text)
  copiedKey.value = key
  toast.success(t('common.copied'))
  setTimeout(() => {
    if (copiedKey.value === key) copiedKey.value = ''
  }, 2000)
}

async function loadAdminPassword() {
  if (adminPassword.value || !domain.value?.adminPasswordConfigured) return adminPassword.value
  passwordLoading.value = true
  try {
    const res = await api.mail.getDomainAdminPassword(domainId.value)
    adminPassword.value = res.adminPassword
    return adminPassword.value
  } catch (err: any) {
    toast.error(translateError(err))
    return null
  } finally {
    passwordLoading.value = false
  }
}

async function toggleAdminPassword() {
  if (showPassword.value) {
    showPassword.value = false
    return
  }
  const password = await loadAdminPassword()
  if (password) showPassword.value = true
}

async function copyAdminPassword() {
  const password = await loadAdminPassword()
  if (!password) return
  copyValue(password, 'admin-password')
}

// DNS 记录格式化函数
function formatHostRecord(record: string): string {
  if (record === '@') return '@'
  return record.replace(/\.$/, '')
}

function formatRecordValue(record: { type: string; value: string; prio?: number }): string {
  if (record.type === 'MX' && record.prio !== undefined) {
    return `${record.prio} ${record.value}`
  }
  return record.value
}

function getDnsRecordDescription(record: { type: string; value: string; record?: string }): string {
  switch (record.type) {
    case 'MX': return t('mail.dnsHint.mx')
    case 'CNAME': return t('mail.dnsHint.cname')
    case 'TXT':
      if (record.value?.toLowerCase().includes('spf')) return t('mail.dnsHint.spf')
      if (record.value?.toLowerCase().includes('dkim')) return t('mail.dnsHint.dkim')
      if (record.record === '_dmarc') return t('mail.dnsHint.dmarc')
      return t('mail.dnsHint.txt')
    default: return ''
  }
}

function getStatusBadgeVariant(status?: string): BadgeVariant {
  switch (status) {
    case 'verified': return 'success'
    case 'pending': return 'warning'
    case 'suspended': return 'error'
    default: return 'default'
  }
}

function getDnsBadgeVariant(type?: string): BadgeVariant {
  switch (type?.toUpperCase()) {
    case 'MX': return 'info'
    case 'TXT': return 'success'
    case 'CNAME': return 'warning'
    default: return 'default'
  }
}

function formatDate(dateStr?: string | null): string {
  if (!dateStr) return '—'
  return new Date(dateStr).toLocaleDateString()
}

function formatDiskMb(mb?: number | null): string {
  if (!mb || mb === 0) return '—'
  if (mb >= 1024) {
    const gb = mb / 1024
    return `${Number.isInteger(gb) ? gb : gb.toFixed(1)} GB`
  }
  return `${mb} MB`
}

// 切换到 DNS 配置 TAB 时加载配置
function switchTab(tab: 'accounts' | 'dns' | 'settings') {
  activeTab.value = tab
  if (tab === 'dns' && !dnsConfig.value) {
    void loadDnsConfig()
  }
}

// ==================== 账户管理弹窗逻辑 ====================

// 创建账户
const showCreateAccountModal = ref(false)
const createAccountForm = ref({
  username: '',
  password: '',
  displayName: '',
  diskLimitMb: 1024,
  isAdmin: false
})

function openCreateAccountModal() {
  createAccountForm.value = {
    username: '',
    password: '',
    displayName: '',
    diskLimitMb: 1024,
    isAdmin: false
  }
  showCreateAccountModal.value = true
}

async function handleCreateAccount() {
  const username = createAccountForm.value.username.trim()
  const password = createAccountForm.value.password.trim()
  if (!username || !password) {
    toast.error(t('mail.accountFieldsRequired'))
    return
  }
  if (password.length < 8) {
    toast.error(t('mail.passwordMinLength'))
    return
  }

  actionLoading.value = 'create-account'
  try {
    await api.mail.createAccount(domainId.value, {
      username,
      password,
      displayName: createAccountForm.value.displayName.trim() || undefined,
      diskLimitMb: createAccountForm.value.diskLimitMb ? Number(createAccountForm.value.diskLimitMb) : undefined,
      isAdmin: createAccountForm.value.isAdmin
    })
    toast.success(t('mail.accountCreated'))
    showCreateAccountModal.value = false
    await loadDomain()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// 编辑账户
const showEditAccountModal = ref(false)
const editingAccount = ref<MailAccountItem | null>(null)
const editAccountForm = ref({
  displayName: '',
  diskLimitMb: 1024
})

function openEditAccountModal(account: MailAccountItem) {
  editingAccount.value = account
  editAccountForm.value = {
    displayName: account.displayName || '',
    diskLimitMb: account.diskLimitMb || 1024
  }
  showEditAccountModal.value = true
}

async function handleUpdateAccount() {
  if (!editingAccount.value) return

  actionLoading.value = 'update-account'
  try {
    await api.mail.updateAccount(domainId.value, editingAccount.value.id, {
      displayName: editAccountForm.value.displayName.trim() || undefined,
      diskLimitMb: editAccountForm.value.diskLimitMb ? Number(editAccountForm.value.diskLimitMb) : undefined
    })
    toast.success(t('mail.accountUpdated'))
    showEditAccountModal.value = false
    await loadDomain()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// 重置密码
const showResetPasswordModal = ref(false)
const resettingAccount = ref<MailAccountItem | null>(null)
const resetPasswordForm = ref({
  password: ''
})

function openResetPasswordModal(account: MailAccountItem) {
  resettingAccount.value = account
  resetPasswordForm.value = {
    password: ''
  }
  showResetPasswordModal.value = true
}

async function handleResetPassword() {
  if (!resettingAccount.value) return
  const password = resetPasswordForm.value.password.trim()
  if (!password) {
    toast.error(t('mail.accountFieldsRequired'))
    return
  }
  if (password.length < 8) {
    toast.error(t('mail.passwordMinLength'))
    return
  }

  actionLoading.value = 'reset-password'
  try {
    await api.mail.resetAccountPassword(domainId.value, resettingAccount.value.id, password)
    toast.success(t('mail.passwordReset'))
    showResetPasswordModal.value = false
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}

// 删除账户
async function deleteAccount(account: MailAccountItem) {
  if (!confirm(t('mail.deleteAccountConfirm', { email: account.email }))) return

  actionLoading.value = `delete-account-${account.id}`
  try {
    await api.mail.deleteAccount(domainId.value, account.id)
    toast.success(t('mail.accountDeleted'))
    await loadDomain()
  } catch (err: any) {
    toast.error(translateError(err))
  } finally {
    actionLoading.value = ''
  }
}
</script>

<template>
  <div class="space-y-6">
    <!-- 页面顶栏 -->
    <PageHeader
      back
      :back-label="t('common.back')"
      @back="router.push(mailPath())"
    >
      <template #title>
        <div v-if="loading" class="h-8 bg-[var(--bg-surface-soft)] rounded-xl animate-pulse w-56"></div>
        <div v-else-if="domain" class="flex flex-wrap items-center gap-3">
          <h1 class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ domain.domain }}
          </h1>
          <Badge
            :variant="getStatusBadgeVariant(domain.status)"
            size="md"
            dot
            :pulse="domain.status === 'pending'"
          >
            {{ t('mail.domainStatus.' + domain.status) }}
          </Badge>
        </div>
      </template>

      <template #description>
        <p class="text-sm text-[var(--text-secondary)] mt-1 max-w-2xl leading-relaxed">
          {{ t('mail.description') }}
        </p>
      </template>

      <!-- 选项卡切换 -->
      <template #bottom>
        <div class="inline-flex p-1 bg-[var(--bg-surface-soft)] rounded-full border border-[var(--border-color)]">
          <button
            type="button"
            class="px-4 py-1.5 text-xs sm:text-sm font-medium rounded-full transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
            :class="activeTab === 'accounts' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('accounts')"
          >
            {{ t('mail.tabs.accounts') }}
          </button>
          <button
            type="button"
            class="px-4 py-1.5 text-xs sm:text-sm font-medium rounded-full transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
            :class="activeTab === 'dns' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('dns')"
          >
            {{ t('mail.tabs.dns') }}
          </button>
          <button
            type="button"
            class="px-4 py-1.5 text-xs sm:text-sm font-medium rounded-full transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
            :class="activeTab === 'settings' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
            @click="switchTab('settings')"
          >
            {{ t('mail.tabs.settings') }}
          </button>
        </div>
      </template>
    </PageHeader>

    <!-- 加载骨架屏 -->
    <div v-if="loading" class="space-y-6">
      <Card loading />
      <Card loading />
    </div>

    <template v-else-if="domain">
      <!-- 邮箱账户 TAB -->
      <div v-show="activeTab === 'accounts'" class="space-y-6">
        <!-- DNS 未完成提示横幅 -->
        <div
          v-if="domain.status !== 'verified'"
          class="rounded-2xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--border-color))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-4 sm:p-5 flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4 transition-all"
        >
          <div class="flex items-center gap-3 min-w-0">
            <div class="w-9 h-9 rounded-xl bg-[color-mix(in_srgb,var(--warning)_18%,var(--bg-surface))] flex items-center justify-center shrink-0">
              <AlertTriangle class="w-5 h-5 text-[var(--warning)]" />
            </div>
            <p class="text-sm font-medium text-[var(--text-primary)] leading-relaxed">
              {{ t('mail.completeDnsFirst') }}
            </p>
          </div>
          <Button
            variant="secondary"
            size="sm"
            class="shrink-0"
            @click="switchTab('dns')"
          >
            {{ t('mail.goDnsConfig') }}
          </Button>
        </div>

        <!-- 管理员账号卡片 -->
        <Card
          v-if="domain.adminUsername"
          :title="t('mail.adminAccount')"
          :subtitle="t('mail.adminAccountDesc')"
          padding="md"
        >
          <template #extra v-if="webmailUrl">
            <a
              :href="webmailUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)] transition-all border border-[var(--border-color)] active:scale-95"
            >
              <span>Webmail</span>
              <ExternalLink class="w-3.5 h-3.5" />
            </a>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-3 gap-4 sm:gap-6">
            <!-- 邮箱地址 -->
            <div class="space-y-1.5">
              <div class="text-xs font-medium text-[var(--text-secondary)]">{{ t('mail.emailAddress') }}</div>
              <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] truncate">{{ domain.adminUsername }}</code>
                <button
                  type="button"
                  class="p-1 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                  :title="t('common.copy')"
                  @click="copyValue(domain.adminUsername, 'admin-email')"
                >
                  <Check v-if="copiedKey === 'admin-email'" class="w-4 h-4 text-[var(--success)]" />
                  <Copy v-else class="w-4 h-4" />
                </button>
              </div>
            </div>

            <!-- 密码 -->
            <div class="space-y-1.5">
              <div class="text-xs font-medium text-[var(--text-secondary)]">{{ t('mail.password') }}</div>
              <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] truncate">
                  {{ domain.adminPasswordConfigured ? (showPassword ? adminPassword : '••••••••') : '—' }}
                </code>
                <div class="flex items-center gap-1 shrink-0">
                  <button
                    type="button"
                    class="p-1 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors disabled:opacity-40 active:scale-95"
                    :disabled="passwordLoading || !domain.adminPasswordConfigured"
                    :title="showPassword ? t('common.hide') : t('common.show')"
                    @click="toggleAdminPassword"
                  >
                    <EyeOff v-if="showPassword" class="w-4 h-4" />
                    <Eye v-else class="w-4 h-4" />
                  </button>
                  <button
                    type="button"
                    class="p-1 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors disabled:opacity-40 active:scale-95"
                    :disabled="passwordLoading || !domain.adminPasswordConfigured"
                    :title="t('common.copy')"
                    @click="copyAdminPassword"
                  >
                    <Check v-if="copiedKey === 'admin-password'" class="w-4 h-4 text-[var(--success)]" />
                    <Copy v-else class="w-4 h-4" />
                  </button>
                </div>
              </div>
            </div>

            <!-- 登录地址 -->
            <div class="space-y-1.5">
              <div class="text-xs font-medium text-[var(--text-secondary)]">{{ t('mail.webmailUrl') }}</div>
              <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                <a
                  :href="webmailUrl"
                  target="_blank"
                  rel="noopener noreferrer"
                  class="text-xs sm:text-sm font-mono text-[var(--accent)] hover:underline truncate"
                >
                  {{ webmailUrl }}
                </a>
                <button
                  type="button"
                  class="p-1 rounded-lg text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                  :title="t('common.copy')"
                  @click="copyValue(webmailUrl, 'webmail-url')"
                >
                  <Check v-if="copiedKey === 'webmail-url'" class="w-4 h-4 text-[var(--success)]" />
                  <Copy v-else class="w-4 h-4" />
                </button>
              </div>
            </div>
          </div>

          <!-- 帮助文档链接 -->
          <template #footer>
            <div class="flex items-center justify-between">
              <router-link
                :to="`${helpPath()}/mail`"
                class="text-xs sm:text-sm text-[var(--accent)] hover:underline inline-flex items-center gap-1.5 font-medium transition-colors"
              >
                <FileText class="w-4 h-4 shrink-0" />
                {{ t('mail.helpDoc') }}
              </router-link>
            </div>
          </template>
        </Card>

        <!-- 无管理员账号提示 -->
        <Card v-else padding="md">
          <EmptyState
            :title="t('mail.noAdminAccount')"
            :description="t('mail.description')"
          />
        </Card>

        <!-- 用户账户列表卡片 -->
        <Card padding="none">
          <template #header>
            <div class="flex items-center justify-between gap-4 w-full">
              <div class="min-w-0">
                <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)] truncate">
                  {{ t('mail.tabs.accounts') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('mail.accountsDescription') }}
                </p>
              </div>
              <Button
                variant="primary"
                size="sm"
                :disabled="domain.status !== 'verified'"
                @click="openCreateAccountModal"
              >
                <template #icon>
                  <Plus class="w-4 h-4" />
                </template>
                {{ t('mail.createAccount') }}
              </Button>
            </div>
          </template>

          <Table
            :columns="accountColumns"
            :data="domainAccounts"
            row-key="id"
            :empty-text="t('mail.noAccounts')"
            :empty-description="domain.status !== 'verified' ? t('mail.verifyFirst') : ''"
          >
            <!-- 邮箱地址单元格 -->
            <template #cell-email="{ row }">
              <div class="flex items-center gap-1.5">
                <code class="font-mono text-xs sm:text-sm text-[var(--text-primary)]">{{ row.email }}</code>
                <button
                  type="button"
                  class="p-1 rounded text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                  :title="t('common.copy')"
                  @click="copyValue(row.email, `account-${row.id}-email`)"
                >
                  <Check v-if="copiedKey === `account-${row.id}-email`" class="w-3.5 h-3.5 text-[var(--success)]" />
                  <Copy v-else class="w-3.5 h-3.5" />
                </button>
              </div>
            </template>

            <!-- 显示名称单元格 -->
            <template #cell-displayName="{ row }">
              <span class="text-sm text-[var(--text-primary)]">{{ row.displayName || '—' }}</span>
            </template>

            <!-- 空间配额单元格 -->
            <template #cell-diskLimitMb="{ row }">
              <span class="text-sm text-[var(--text-secondary)]">{{ formatDiskMb(row.diskLimitMb) }}</span>
            </template>

            <!-- 角色单元格 -->
            <template #cell-isAdmin="{ row }">
              <Badge :variant="row.isAdmin ? 'info' : 'default'" size="sm">
                {{ row.isAdmin ? t('mail.admin') : t('common.user') }}
              </Badge>
            </template>

            <!-- 操作列单元格 -->
            <template #cell-actions="{ row }">
              <div class="flex items-center justify-end gap-1">
                <Button
                  variant="ghost"
                  size="sm"
                  class="h-8 px-2 text-xs"
                  :title="t('mail.editAccount')"
                  @click="openEditAccountModal(row)"
                >
                  <Edit3 class="w-3.5 h-3.5" />
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  class="h-8 px-2 text-xs"
                  :title="t('mail.resetPassword')"
                  @click="openResetPasswordModal(row)"
                >
                  <Key class="w-3.5 h-3.5" />
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  class="h-8 px-2 text-xs text-[var(--error)] hover:text-[var(--error)]"
                  :title="t('common.delete')"
                  :loading="actionLoading === `delete-account-${row.id}`"
                  @click="deleteAccount(row)"
                >
                  <Trash2 class="w-3.5 h-3.5" />
                </Button>
              </div>
            </template>

            <!-- 移动端卡片视图 -->
            <template #mobile="{ row }">
              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-3">
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0">
                    <div class="font-mono text-sm font-semibold text-[var(--text-primary)] truncate">{{ row.email }}</div>
                    <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ row.displayName || '—' }}</div>
                  </div>
                  <Badge :variant="row.isAdmin ? 'info' : 'default'" size="sm">
                    {{ row.isAdmin ? t('mail.admin') : t('common.user') }}
                  </Badge>
                </div>

                <div class="flex items-center justify-between text-xs text-[var(--text-secondary)] pt-2 border-t border-[var(--border-color)]">
                  <div>{{ t('mail.diskLimit') }}: {{ formatDiskMb(row.diskLimitMb) }}</div>
                  <div>{{ formatDate(row.createdAt) }}</div>
                </div>

                <div class="flex items-center justify-end gap-2 pt-2 border-t border-[var(--border-color)]">
                  <Button variant="secondary" size="sm" class="text-xs" @click="openEditAccountModal(row)">
                    {{ t('common.edit') }}
                  </Button>
                  <Button variant="secondary" size="sm" class="text-xs" @click="openResetPasswordModal(row)">
                    {{ t('mail.resetPassword') }}
                  </Button>
                  <Button
                    variant="danger"
                    size="sm"
                    class="text-xs"
                    :loading="actionLoading === `delete-account-${row.id}`"
                    @click="deleteAccount(row)"
                  >
                    {{ t('common.delete') }}
                  </Button>
                </div>
              </div>
            </template>

            <!-- 空状态插槽 -->
            <template #empty>
              <EmptyState
                :title="t('mail.noAccounts')"
                :description="domain.status !== 'verified' ? t('mail.verifyFirst') : t('mail.accountsDescription')"
              >
                <template #action v-if="domain.status === 'verified'">
                  <Button variant="primary" size="sm" @click="openCreateAccountModal">
                    <template #icon>
                      <Plus class="w-4 h-4" />
                    </template>
                    {{ t('mail.createAccount') }}
                  </Button>
                </template>
              </EmptyState>
            </template>
          </Table>
        </Card>
      </div>

      <!-- DNS 配置 TAB -->
      <div v-show="activeTab === 'dns'" class="space-y-6">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
          <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
            {{ t('mail.dnsDescription') }}
          </p>
          <Button
            variant="secondary"
            size="sm"
            class="shrink-0"
            :loading="actionLoading === 'verify'"
            @click="verifyDomain"
          >
            <template #icon>
              <RefreshCw class="w-4 h-4" />
            </template>
            {{ t('mail.refreshStatus') }}
          </Button>
        </div>

        <div v-if="!dnsConfig" class="space-y-4">
          <Card loading />
          <Card loading />
        </div>

        <!-- DNS 记录列表 -->
        <div v-else class="space-y-4">
          <!-- TXT 验证记录 -->
          <Card v-if="dnsConfig.txtRecord" padding="md">
            <template #header>
              <div class="flex items-center gap-2">
                <Badge variant="info" size="sm">TXT</Badge>
                <span class="text-xs font-medium text-[var(--text-secondary)]">{{ t('mail.txtVerification') }}</span>
              </div>
            </template>

            <div class="grid grid-cols-1 md:grid-cols-[220px_1fr] gap-4 text-sm">
              <div>
                <div class="text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ t('mail.hostRecord') }}</div>
                <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                  <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] truncate">workspace-verification</code>
                  <button
                    type="button"
                    class="p-1 rounded text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                    :title="t('common.copy')"
                    @click="copyValue('workspace-verification', 'txt-host')"
                  >
                    <Check v-if="copiedKey === 'txt-host'" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>

              <div>
                <div class="text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ t('mail.recordValue') }}</div>
                <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                  <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] break-all">{{ dnsConfig.txtRecord }}</code>
                  <button
                    type="button"
                    class="p-1 rounded text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                    :title="t('common.copy')"
                    @click="copyValue(dnsConfig.txtRecord, 'txt-value')"
                  >
                    <Check v-if="copiedKey === 'txt-value'" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>
            </div>

            <p class="text-xs text-[var(--text-tertiary)] mt-3 leading-relaxed">
              {{ t('mail.dnsHint.txt') }}
            </p>
          </Card>

          <!-- 其他 DNS 记录（MX、CNAME、SPF、DKIM、DMARC 等） -->
          <Card
            v-for="(record, idx) in filteredDnsRecords"
            :key="idx"
            padding="md"
          >
            <template #header>
              <div class="flex items-center gap-2">
                <Badge :variant="getDnsBadgeVariant(record.type)" size="sm">{{ record.type }}</Badge>
              </div>
            </template>

            <div class="grid grid-cols-1 md:grid-cols-[220px_1fr] gap-4 text-sm">
              <div>
                <div class="text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ t('mail.hostRecord') }}</div>
                <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                  <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] truncate">{{ formatHostRecord(record.record) }}</code>
                  <button
                    type="button"
                    class="p-1 rounded text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                    :title="t('common.copy')"
                    @click="copyValue(formatHostRecord(record.record), `dns-${idx}-host`)"
                  >
                    <Check v-if="copiedKey === `dns-${idx}-host`" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>

              <div>
                <div class="text-xs font-medium text-[var(--text-secondary)] mb-1.5">{{ t('mail.recordValue') }}</div>
                <div class="flex items-center justify-between gap-2 p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] min-h-[44px]">
                  <code class="text-xs sm:text-sm font-mono text-[var(--text-primary)] break-all">{{ formatRecordValue(record) }}</code>
                  <button
                    type="button"
                    class="p-1 rounded text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors shrink-0 active:scale-95"
                    :title="t('common.copy')"
                    @click="copyValue(formatRecordValue(record), `dns-${idx}-value`)"
                  >
                    <Check v-if="copiedKey === `dns-${idx}-value`" class="w-3.5 h-3.5 text-[var(--success)]" />
                    <Copy v-else class="w-3.5 h-3.5" />
                  </button>
                </div>
              </div>
            </div>

            <p class="text-xs text-[var(--text-tertiary)] mt-3 leading-relaxed">
              {{ getDnsRecordDescription(record) }}
            </p>
          </Card>
        </div>
      </div>

      <!-- 设置 TAB -->
      <div v-show="activeTab === 'settings'" class="space-y-6">
        <!-- 域名信息卡片 -->
        <Card :title="t('mail.domainInfo')" padding="md">
          <div class="divide-y divide-[var(--border-color)] text-sm">
            <div class="py-3 flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">{{ t('mail.domainName') }}</span>
              <span class="font-mono font-medium text-[var(--text-primary)]">{{ domain.domain }}</span>
            </div>
            <div class="py-3 flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">{{ t('common.status') }}</span>
              <Badge :variant="getStatusBadgeVariant(domain.status)" size="sm" dot>
                {{ t('mail.domainStatus.' + domain.status) }}
              </Badge>
            </div>
            <div class="py-3 flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">{{ t('mail.createdAt') }}</span>
              <span class="text-[var(--text-primary)]">{{ formatDate(domain.createdAt) }}</span>
            </div>
            <div v-if="domain.verifiedAt" class="py-3 flex items-center justify-between">
              <span class="text-[var(--text-secondary)]">{{ t('mail.verifiedAt') }}</span>
              <span class="text-[var(--text-primary)]">{{ formatDate(domain.verifiedAt) }}</span>
            </div>
          </div>
        </Card>

        <!-- 危险操作区域 -->
        <div class="rounded-2xl border border-[color-mix(in_srgb,var(--error)_30%,var(--border-color))] bg-[var(--bg-surface)] overflow-hidden">
          <div class="px-6 py-4 bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] border-b border-[color-mix(in_srgb,var(--error)_20%,var(--border-color))] flex items-center gap-2">
            <AlertTriangle class="w-4 h-4 text-[var(--error)] shrink-0" />
            <h4 class="text-sm font-semibold text-[var(--error)]">{{ t('mail.dangerZone') }}</h4>
          </div>
          <div class="p-6 flex flex-col sm:flex-row sm:items-center justify-between gap-4">
            <p class="text-sm text-[var(--text-secondary)] leading-relaxed">
              {{ t('mail.deleteDomainWarning') }}
            </p>
            <Button
              variant="danger"
              size="md"
              class="shrink-0"
              :loading="actionLoading === 'delete-domain'"
              @click="deleteDomain"
            >
              {{ t('mail.deleteDomain') }}
            </Button>
          </div>
        </div>
      </div>
    </template>

    <!-- 创建账户抽屉弹窗 -->
    <DrawerModal
      v-model="showCreateAccountModal"
      :title="t('mail.createAccount')"
      max-width="max-w-lg"
    >
      <div class="space-y-4 py-2">
        <!-- 用户名 -->
        <Input
          v-model="createAccountForm.username"
          :label="`${t('mail.username')} *`"
          :placeholder="t('mail.usernamePlaceholder')"
          autocomplete="off"
        >
          <template #suffix v-if="domain">
            <span class="text-xs font-mono text-[var(--text-secondary)] select-none">@{{ domain.domain }}</span>
          </template>
        </Input>

        <!-- 密码 -->
        <Input
          v-model="createAccountForm.password"
          type="password"
          :label="`${t('mail.password')} *`"
          :placeholder="t('mail.passwordPlaceholder')"
          :hint="t('mail.passwordHint')"
          autocomplete="new-password"
        />

        <!-- 显示名称 -->
        <Input
          v-model="createAccountForm.displayName"
          :label="t('mail.displayName')"
          :placeholder="t('mail.displayNamePlaceholder')"
        />

        <!-- 空间配额 -->
        <Input
          v-model="createAccountForm.diskLimitMb"
          type="number"
          :label="`${t('mail.diskLimit')} (MB)`"
          placeholder="1024"
        />

        <!-- 管理员选项 -->
        <div class="pt-2">
          <label class="inline-flex items-center gap-2 cursor-pointer text-sm text-[var(--text-primary)]">
            <input
              v-model="createAccountForm.isAdmin"
              type="checkbox"
              class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)]"
            />
            <span>{{ t('mail.setAsAdmin') }}</span>
          </label>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end gap-3">
          <Button variant="ghost" @click="showCreateAccountModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button
            variant="primary"
            :loading="actionLoading === 'create-account'"
            :disabled="!createAccountForm.username.trim() || !createAccountForm.password.trim()"
            @click="handleCreateAccount"
          >
            {{ t('common.confirm') }}
          </Button>
        </div>
      </template>
    </DrawerModal>

    <!-- 编辑账户抽屉弹窗 -->
    <DrawerModal
      v-model="showEditAccountModal"
      :title="t('mail.editAccount')"
      max-width="max-w-lg"
    >
      <div v-if="editingAccount" class="space-y-4 py-2">
        <Input
          :model-value="editingAccount.email"
          :label="t('mail.emailAddress')"
          readonly
          disabled
        />

        <Input
          v-model="editAccountForm.displayName"
          :label="t('mail.displayName')"
          :placeholder="t('mail.displayNamePlaceholder')"
        />

        <Input
          v-model="editAccountForm.diskLimitMb"
          type="number"
          :label="`${t('mail.diskLimit')} (MB)`"
          placeholder="1024"
        />
      </div>

      <template #footer>
        <div class="flex justify-end gap-3">
          <Button variant="ghost" @click="showEditAccountModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button
            variant="primary"
            :loading="actionLoading === 'update-account'"
            @click="handleUpdateAccount"
          >
            {{ t('common.save') }}
          </Button>
        </div>
      </template>
    </DrawerModal>

    <!-- 重置密码抽屉弹窗 -->
    <DrawerModal
      v-model="showResetPasswordModal"
      :title="t('mail.resetPassword')"
      max-width="max-w-md"
    >
      <div v-if="resettingAccount" class="space-y-4 py-2">
        <div class="p-3 bg-[var(--bg-surface-soft)] rounded-xl border border-[var(--border-color)]">
          <div class="text-xs text-[var(--text-secondary)] mb-0.5">{{ t('mail.emailAddress') }}</div>
          <div class="font-mono text-sm text-[var(--text-primary)] font-medium">{{ resettingAccount.email }}</div>
        </div>

        <Input
          v-model="resetPasswordForm.password"
          type="password"
          :label="`${t('mail.newPassword')} *`"
          :placeholder="t('mail.newPasswordPlaceholder')"
          :hint="t('mail.passwordHint')"
          autocomplete="new-password"
        />
      </div>

      <template #footer>
        <div class="flex justify-end gap-3">
          <Button variant="ghost" @click="showResetPasswordModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button
            variant="primary"
            :loading="actionLoading === 'reset-password'"
            :disabled="!resetPasswordForm.password.trim() || resetPasswordForm.password.length < 8"
            @click="handleResetPassword"
          >
            {{ t('common.confirm') }}
          </Button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>
