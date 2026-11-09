<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute } from 'vue-router'
import { onClickOutside } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import {
  Ban,
  Check,
  ChevronDown,
  Copy,
  ExternalLink,
  Info,
  Key,
  Pencil,
  RefreshCw,
  Search,
  Shield,
  Trash2,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import { systemSettingsNavigationItems } from '@/constants/adminSettings'
import type {
  AdminOAuthAuthorization,
  OAuthClientApp,
  OAuthConfig,
  PublicApiScope,
  PublicApiScopeMetadata,
  UpdateOAuthConfigRequest
} from '@/types/api'
import { buildPublicApiUrl } from '@/utils/api-url'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Select
} from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'

const route = useRoute()
const { t } = useI18n()
const toast = useToast()

const loading = ref<boolean>(true)
interface ProviderConfig {
  clientId: string
  clientSecret: string
  enabled: boolean
  configured: boolean
}
type ProviderType = 'github' | 'google'
const configs = ref<Record<ProviderType, ProviderConfig>>({
  github: { clientId: '', clientSecret: '', enabled: false, configured: false },
  google: { clientId: '', clientSecret: '', enabled: false, configured: false }
})

// Edit modal
const showModal = ref<boolean>(false)
const editProvider = ref<ProviderType | ''>('')
const form = ref<UpdateOAuthConfigRequest>({ clientId: '', clientSecret: '', enabled: false })
const formLoading = ref<boolean>(false)
const oauthApps = ref<OAuthClientApp[]>([])
const oauthAppsLoading = ref(false)
const oauthAuthorizations = ref<AdminOAuthAuthorization[]>([])
const oauthAuthorizationsLoading = ref(false)
const oauthAuthorizationFilters = ref<{
  appId: number | null
  user: string
  status: 'all' | 'active' | 'revoked' | 'disabled'
  page: number
  pageSize: number
}>({
  appId: null,
  user: '',
  status: 'all',
  page: 1,
  pageSize: 20
})
const oauthAuthorizationTotal = ref(0)
const oauthAuthorizationTotalPages = ref(1)
const oauthAppSaving = ref(false)
const oauthAppSecret = ref('')
const copiedSecret = ref(false)
const copiedCallbackMap = ref<Record<string, boolean>>({})
const availableOAuthScopes = ref<PublicApiScopeMetadata[]>([])
const oauthAppForm = ref<{
  id: number | null
  name: string
  redirectUris: string
  scopes: PublicApiScope[]
  enabled: boolean
}>({
  id: null,
  name: '',
  redirectUris: '',
  scopes: ['profile:read'],
  enabled: true
})

const isScopeDropdownOpen = ref(false)
const scopeDropdownRef = ref<HTMLElement | null>(null)
const scopeSearchQuery = ref('')

onClickOutside(scopeDropdownRef, () => {
  isScopeDropdownOpen.value = false
})

const filteredOAuthScopes = computed(() => {
  const query = scopeSearchQuery.value.trim().toLowerCase()
  if (!query) return availableOAuthScopes.value
  return availableOAuthScopes.value.filter(scope => {
    return (
      scope.scope.toLowerCase().includes(query) ||
      scope.title.toLowerCase().includes(query) ||
      scope.description.toLowerCase().includes(query) ||
      scope.resources.some(r => r.toLowerCase().includes(query))
    )
  })
})

function toggleScopeDropdown(): void {
  isScopeDropdownOpen.value = !isScopeDropdownOpen.value
}

function toggleScope(scopeKey: PublicApiScope): void {
  const index = oauthAppForm.value.scopes.indexOf(scopeKey)
  if (index > -1) {
    oauthAppForm.value.scopes.splice(index, 1)
  } else {
    oauthAppForm.value.scopes.push(scopeKey)
  }
}

function removeScope(scopeKey: PublicApiScope): void {
  const index = oauthAppForm.value.scopes.indexOf(scopeKey)
  if (index > -1) {
    oauthAppForm.value.scopes.splice(index, 1)
  }
}

function selectAllScopes(): void {
  oauthAppForm.value.scopes = availableOAuthScopes.value.map(s => s.scope)
}

function clearAllScopes(): void {
  oauthAppForm.value.scopes = []
}

onMounted(async (): Promise<void> => {
  await Promise.all([loadConfigs(), loadOAuthScopes(), loadOAuthApps(), loadOAuthAuthorizations()])
})

async function loadOAuthScopes(): Promise<void> {
  try {
    const response = await api.oauthApps.listScopes()
    availableOAuthScopes.value = response.scopes
  } catch (err: any) {
    toast.error('加载 OAuth scope 元数据失败：' + (err?.message || String(err)))
  }
}

async function loadConfigs(): Promise<void> {
  loading.value = true
  try {
    const response = await api.oauth.getConfigs()
    const data = response as { configs?: OAuthConfig[] }

    // Reset configs
    configs.value = {
      github: { clientId: '', clientSecret: '', enabled: false, configured: false },
      google: { clientId: '', clientSecret: '', enabled: false, configured: false }
    }

    // Map response to configs
    for (const config of data.configs || []) {
      const provider = config.provider as ProviderType
      if (provider && configs.value[provider]) {
        const configAny = config as any
        configs.value[provider] = {
          clientId: configAny.clientId || config.client_id || '',
          clientSecret: configAny.clientSecretMasked || '',
          enabled: Boolean(configAny.enabled ?? config.enabled),
          configured: true
        }
      }
    }
  } finally {
    loading.value = false
  }
}

async function loadOAuthApps(): Promise<void> {
  oauthAppsLoading.value = true
  try {
    const response = await api.oauthApps.list()
    oauthApps.value = response.apps
  } catch (err: any) {
    toast.error('加载对外 OAuth 应用失败：' + (err?.message || String(err)))
  } finally {
    oauthAppsLoading.value = false
  }
}

async function loadOAuthAuthorizations(): Promise<void> {
  oauthAuthorizationsLoading.value = true
  try {
    const response = await api.oauthApps.listAuthorizations({
      appId: oauthAuthorizationFilters.value.appId || undefined,
      user: oauthAuthorizationFilters.value.user.trim() || undefined,
      status: oauthAuthorizationFilters.value.status,
      page: oauthAuthorizationFilters.value.page,
      pageSize: oauthAuthorizationFilters.value.pageSize
    })
    oauthAuthorizations.value = response.authorizations
    oauthAuthorizationTotal.value = response.total
    oauthAuthorizationTotalPages.value = response.totalPages
  } catch (err: any) {
    toast.error('加载 OAuth 授权记录失败：' + (err?.message || String(err)))
  } finally {
    oauthAuthorizationsLoading.value = false
  }
}

function applyOAuthAuthorizationFilters(): void {
  oauthAuthorizationFilters.value.page = 1
  void loadOAuthAuthorizations()
}

async function revokeOAuthAuthorization(authorization: AdminOAuthAuthorization): Promise<void> {
  if (!confirm(`确认撤销 ${authorization.user.username} 对 ${authorization.app.name} 的授权？相关 access token 和 refresh token 会立即失效。`)) return
  try {
    await api.oauthApps.revokeAuthorization(authorization.id)
    toast.success('OAuth 授权已撤销')
    await loadOAuthAuthorizations()
  } catch (err: any) {
    toast.error('撤销 OAuth 授权失败：' + (err?.message || String(err)))
  }
}

function formatOAuthAuthorizationStatus(authorization: AdminOAuthAuthorization): string {
  if (authorization.revokedAt) return '已撤销'
  if (!authorization.app.enabled) return '应用停用'
  return '有效'
}

function setOAuthAuthorizationPage(page: number): void {
  oauthAuthorizationFilters.value.page = Math.min(Math.max(1, page), oauthAuthorizationTotalPages.value)
  void loadOAuthAuthorizations()
}

function resetOAuthAppForm(): void {
  oauthAppForm.value = {
    id: null,
    name: '',
    redirectUris: '',
    scopes: ['profile:read'],
    enabled: true
  }
  isScopeDropdownOpen.value = false
  scopeSearchQuery.value = ''
}

function editOAuthApp(app: OAuthClientApp): void {
  oauthAppForm.value = {
    id: app.id,
    name: app.name,
    redirectUris: app.redirectUris.join('\n'),
    scopes: app.scopes.length ? [...app.scopes] : ['profile:read'],
    enabled: app.enabled
  }
  oauthAppSecret.value = ''
  isScopeDropdownOpen.value = false
  scopeSearchQuery.value = ''
}

function parseRedirectUrisInput(value: string): string[] {
  return Array.from(new Set(value.split(/\r?\n/).map(item => item.trim()).filter(Boolean)))
}

async function saveOAuthApp(): Promise<void> {
  const redirectUris = parseRedirectUrisInput(oauthAppForm.value.redirectUris)
  if (!oauthAppForm.value.name.trim()) {
    toast.error('请输入应用名称')
    return
  }
  if (redirectUris.length === 0) {
    toast.error('至少需要一个 Redirect URI')
    return
  }
  if (oauthAppForm.value.scopes.length === 0) {
    toast.error('至少需要一个 scope')
    return
  }
  oauthAppSaving.value = true
  try {
    const payload = {
      name: oauthAppForm.value.name.trim(),
      redirectUris,
      scopes: oauthAppForm.value.scopes,
      enabled: oauthAppForm.value.enabled
    }
    if (oauthAppForm.value.id) {
      await api.oauthApps.update(oauthAppForm.value.id, payload)
      toast.success('OAuth 应用已更新')
    } else {
      const response = await api.oauthApps.create(payload)
      oauthAppSecret.value = response.clientSecret
      toast.success('OAuth 应用已创建，请立即保存 Client Secret')
    }
    resetOAuthAppForm()
    await Promise.all([loadOAuthApps(), loadOAuthAuthorizations()])
  } catch (err: any) {
    toast.error('保存 OAuth 应用失败：' + (err?.message || String(err)))
  } finally {
    oauthAppSaving.value = false
  }
}

async function rotateOAuthAppSecret(app: OAuthClientApp): Promise<void> {
  if (!confirm(`确认轮换 ${app.name} 的 Client Secret？旧 secret 将立即失效。`)) return
  try {
    const response = await api.oauthApps.rotateSecret(app.id)
    oauthAppSecret.value = response.clientSecret
    toast.success('Client Secret 已轮换，请立即保存')
    await Promise.all([loadOAuthApps(), loadOAuthAuthorizations()])
  } catch (err: any) {
    toast.error('轮换 Client Secret 失败：' + (err?.message || String(err)))
  }
}

async function deleteOAuthApp(app: OAuthClientApp): Promise<void> {
  if (!confirm(`确认删除 ${app.name}？关联授权码和 access token 会一起失效。`)) return
  try {
    await api.oauthApps.delete(app.id)
    toast.success('OAuth 应用已删除')
    if (oauthAppForm.value.id === app.id) resetOAuthAppForm()
    await Promise.all([loadOAuthApps(), loadOAuthAuthorizations()])
  } catch (err: any) {
    toast.error('删除 OAuth 应用失败：' + (err?.message || String(err)))
  }
}

function openEdit(provider: ProviderType): void {
  editProvider.value = provider
  const config = configs.value[provider]
  form.value = {
    clientId: config.clientId || '',
    clientSecret: '',
    enabled: config.enabled
  }
  showModal.value = true
}

async function saveConfig(): Promise<void> {
  if (!form.value.clientId) {
    toast.error(t('admin.oauth.clientId'))
    return
  }

  if (!editProvider.value) return

  const isUpdate = configs.value[editProvider.value].configured
  if (!isUpdate && !form.value.clientSecret) {
    toast.error(t('admin.oauth.clientSecret'))
    return
  }

  formLoading.value = true
  try {
    const payload: UpdateOAuthConfigRequest = {
      clientId: form.value.clientId,
      enabled: form.value.enabled
    }
    if (form.value.clientSecret) {
      payload.clientSecret = form.value.clientSecret
    }

    await api.oauth.updateConfig(editProvider.value, payload)

    toast.success(t('admin.oauth.saveSuccess'))
    showModal.value = false
    await loadConfigs()
  } catch (err: any) {
    toast.error(t('admin.oauth.saveFailed') + ': ' + (err?.message || String(err)))
  } finally {
    formLoading.value = false
  }
}

async function deleteConfig(provider: ProviderType): Promise<void> {
  if (!confirm(`Delete ${provider.toUpperCase()} config?`)) return

  try {
    await api.oauth.deleteConfig(provider)
    toast.success(t('common.success'))
    await loadConfigs()
  } catch (err: any) {
    toast.error(err?.message || String(err))
  }
}

interface ProviderInfo {
  name: string
  icon: string
  docsUrl: string
  callbackPath: string
}

const providerInfo = computed<Record<ProviderType, ProviderInfo>>(() => {
  return {
    github: {
      name: 'GitHub',
      icon: `<svg class="w-6 h-6" fill="currentColor" viewBox="0 0 24 24"><path fill-rule="evenodd" d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z" clip-rule="evenodd"></path></svg>`,
      docsUrl: 'https://github.com/settings/developers',
      callbackPath: '/oauth/callback/github'
    },
    google: {
      name: 'Google',
      icon: `<svg class="w-6 h-6" viewBox="0 0 24 24"><path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"></path><path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"></path><path fill="#FBBC05" d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"></path><path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"></path></svg>`,
      docsUrl: 'https://console.cloud.google.com/apis/credentials',
      callbackPath: '/oauth/callback/google'
    }
  }
})

function getCallbackUrl(path?: string): string {
  return path ? buildPublicApiUrl(path) : ''
}

async function copySecret(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    copiedSecret.value = true
    toast.success('Client Secret 已复制到剪贴板')
    setTimeout(() => {
      copiedSecret.value = false
    }, 2000)
  } catch {
    toast.error('复制失败，请手动选择复制')
  }
}

async function copyCallbackUrl(url: string, provider: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(url)
    copiedCallbackMap.value[provider] = true
    toast.success('回调 URL 已复制')
    setTimeout(() => {
      copiedCallbackMap.value[provider] = false
    }, 2000)
  } catch {
    toast.error('复制失败')
  }
}

function formatScopeRisk(risk: PublicApiScopeMetadata['risk']): string {
  return {
    low: '低风险',
    medium: '中风险',
    high: '高风险'
  }[risk]
}

function getScopeRiskVariant(risk: PublicApiScopeMetadata['risk']): 'success' | 'warning' | 'error' | 'default' {
  switch (risk) {
    case 'low':
      return 'success'
    case 'medium':
      return 'warning'
    case 'high':
      return 'error'
    default:
      return 'default'
  }
}

function formatScopeAccess(access: PublicApiScopeMetadata['access']): string {
  return {
    read: '读取',
    write: '写入',
    operate: '操作'
  }[access]
}

const statusOptions = [
  { label: '全部状态', value: 'all' },
  { label: '有效授权', value: 'active' },
  { label: '已撤销', value: 'revoked' },
  { label: '应用停用', value: 'disabled' }
]

const appFilterOptions = computed(() => [
  { label: '全部 OAuth App', value: '' },
  ...oauthApps.value.map(app => ({ label: app.name, value: String(app.id) }))
])
</script>

<template>
  <div class="max-w-6xl mx-auto space-y-6 pb-12 animate-fade-in">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('admin.system.title') || '系统设置'"
      :description="t('admin.oauth.description') || '配置第三方登录（GitHub、Google）及 OAuth 客户端应用'"
    />

    <!-- Segmented Navigation Bar -->
    <nav class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto no-scrollbar shadow-sm">
      <router-link
        v-for="item in systemSettingsNavigationItems"
        :key="item.path"
        :to="item.path"
        class="shrink-0 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98] select-none"
        :class="route.path === item.path
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold border border-[var(--border-color)]/60'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/50 border border-transparent'"
      >
        {{ t(item.labelKey) }}
      </router-link>
    </nav>

    <!-- Loading Skeleton -->
    <div v-if="loading" class="grid grid-cols-1 md:grid-cols-2 gap-6">
      <Card v-for="i in 2" :key="`skeleton-${i}`" loading padding="md" />
    </div>

    <!-- Provider Cards -->
    <div v-else class="grid grid-cols-1 md:grid-cols-2 gap-6">
      <Card
        v-for="(config, provider) in configs"
        :key="provider"
        padding="md"
        class="transition-all duration-300"
      >
        <div class="flex items-start justify-between gap-4 mb-5">
          <div class="flex items-center gap-3.5 min-w-0">
            <div
              class="w-12 h-12 rounded-2xl flex items-center justify-center shrink-0 border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-primary)] transition-transform duration-300 hover:scale-105"
              v-html="providerInfo[provider]?.icon"
            ></div>
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <h3 class="font-semibold text-base text-[var(--text-primary)] truncate">
                  {{ providerInfo[provider]?.name }}
                </h3>
                <Badge
                  v-if="config.configured"
                  :variant="config.enabled ? 'success' : 'warning'"
                  size="sm"
                >
                  {{ config.enabled ? t('admin.oauth.enabled') : t('admin.oauth.disabled') }}
                </Badge>
                <Badge v-else variant="default" size="sm">
                  未配置
                </Badge>
              </div>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5 truncate">
                {{ config.configured ? (config.enabled ? '已启用登录服务' : '已暂停登录服务') : '尚未接入此提供商' }}
              </p>
            </div>
          </div>

          <div class="flex items-center gap-2 shrink-0">
            <Button size="sm" variant="secondary" @click="openEdit(provider)">
              <Pencil class="w-3.5 h-3.5" />
              <span>{{ config.configured ? t('admin.oauth.edit') : t('common.create') }}</span>
            </Button>
            <Button
              v-if="config.configured"
              size="sm"
              variant="ghost"
              class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
              @click="deleteConfig(provider)"
            >
              <Trash2 class="w-3.5 h-3.5" />
              <span>{{ t('common.delete') }}</span>
            </Button>
          </div>
        </div>

        <!-- Configured Details -->
        <div v-if="config.configured" class="space-y-2.5 text-xs">
          <div class="flex items-center justify-between p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="text-[var(--text-secondary)] font-medium">Client ID</span>
            <span class="font-mono text-2xs text-[var(--text-primary)] select-all truncate max-w-[220px]">{{ config.clientId }}</span>
          </div>
          <div class="flex items-center justify-between p-2.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="text-[var(--text-secondary)] font-medium">Client Secret</span>
            <span class="font-mono text-2xs text-[var(--text-primary)] select-all truncate max-w-[220px]">{{ config.clientSecret }}</span>
          </div>
        </div>

        <!-- Not Configured Notice -->
        <div v-else class="text-xs text-[var(--text-secondary)] leading-relaxed p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
          <p>
            {{ t('admin.oauth.notConfigured') }}
            <a
              :href="providerInfo[provider]?.docsUrl"
              target="_blank"
              class="text-[var(--accent)] hover:underline inline-flex items-center gap-1 font-medium mx-1"
            >
              <span>{{ providerInfo[provider]?.name }} {{ t('admin.oauth.developerConsole') }}</span>
              <ExternalLink class="w-3 h-3" />
            </a>
            {{ t('admin.oauth.createOAuthApp') }}
          </p>
        </div>

        <!-- Callback URL Info Micro Card -->
        <div class="mt-4 rounded-xl bg-[var(--bg-surface-soft)]/70 p-3 border border-[var(--border-color)]">
          <div class="flex items-center justify-between mb-1.5">
            <span class="text-2xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
              {{ t('admin.oauth.callbackUrl') }}
            </span>
            <button
              type="button"
              class="text-2xs font-medium text-[var(--accent)] hover:underline inline-flex items-center gap-1 transition-colors"
              @click="copyCallbackUrl(getCallbackUrl(providerInfo[provider]?.callbackPath), provider)"
            >
              <component :is="copiedCallbackMap[provider] ? Check : Copy" class="w-3 h-3" />
              <span>{{ copiedCallbackMap[provider] ? '已复制' : '复制 URL' }}</span>
            </button>
          </div>
          <code class="block break-all font-mono text-xs text-[var(--text-primary)] select-all bg-[var(--bg-surface)] p-2 rounded-lg border border-[var(--border-color)]">
            {{ getCallbackUrl(providerInfo[provider]?.callbackPath) }}
          </code>
        </div>
      </Card>
    </div>

    <!-- Usage Guide Card -->
    <Card padding="md" class="relative overflow-hidden">
      <template #header>
        <div class="flex items-center gap-2.5">
          <div class="w-8 h-8 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)]">
            <Info class="w-4 h-4 stroke-[2]" />
          </div>
          <div>
            <h3 class="font-semibold text-base text-[var(--text-primary)]">
              {{ t('admin.oauth.usageGuide') }}
            </h3>
            <p class="text-xs text-[var(--text-secondary)]">第三方登录对接配置流程与安全建议</p>
          </div>
        </div>
      </template>

      <div class="space-y-3.5 text-xs text-[var(--text-secondary)]">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <div class="flex items-start gap-2.5 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="w-5 h-5 rounded-full bg-[var(--bg-surface)] border border-[var(--border-color)] flex items-center justify-center font-mono text-2xs font-semibold text-[var(--text-primary)] shrink-0">1</span>
            <span class="leading-relaxed">{{ t('admin.oauth.step1') }}</span>
          </div>
          <div class="flex items-start gap-2.5 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="w-5 h-5 rounded-full bg-[var(--bg-surface)] border border-[var(--border-color)] flex items-center justify-center font-mono text-2xs font-semibold text-[var(--text-primary)] shrink-0">2</span>
            <span class="leading-relaxed">{{ t('admin.oauth.step2') }}</span>
          </div>
          <div class="flex items-start gap-2.5 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="w-5 h-5 rounded-full bg-[var(--bg-surface)] border border-[var(--border-color)] flex items-center justify-center font-mono text-2xs font-semibold text-[var(--text-primary)] shrink-0">3</span>
            <span class="leading-relaxed">{{ t('admin.oauth.step3') }}</span>
          </div>
          <div class="flex items-start gap-2.5 p-3 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
            <span class="w-5 h-5 rounded-full bg-[var(--bg-surface)] border border-[var(--border-color)] flex items-center justify-center font-mono text-2xs font-semibold text-[var(--text-primary)] shrink-0">4</span>
            <span class="leading-relaxed">{{ t('admin.oauth.step4') }}</span>
          </div>
        </div>
        <div class="flex items-start gap-2.5 rounded-xl border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--warning)_8%,var(--bg-surface))] p-3.5 text-xs text-[var(--warning)] leading-relaxed">
          <Shield class="w-4 h-4 shrink-0 mt-0.5" />
          <span>{{ t('admin.oauth.warning') }}</span>
        </div>
      </div>
    </Card>

    <!-- PayIncus OAuth 服务端 -->
    <Card padding="none" class="overflow-hidden">
      <template #header>
        <div class="flex flex-col gap-3 md:flex-row md:items-center md:justify-between w-full">
          <div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">PayIncus OAuth 服务端</h3>
            <p class="mt-0.5 text-xs text-[var(--text-secondary)]">为第三方应用创建 OAuth 应用，通过授权码换取 Bearer token 后访问 /api/v1。</p>
          </div>
          <Button
            size="sm"
            variant="secondary"
            class="shrink-0"
            :loading="oauthAppsLoading"
            @click="loadOAuthApps"
          >
            <RefreshCw class="w-3.5 h-3.5" />
            <span>{{ oauthAppsLoading ? '加载中...' : '刷新应用' }}</span>
          </Button>
        </div>
      </template>

      <!-- Client Secret One-Time Display Banner -->
      <div v-if="oauthAppSecret" class="mx-6 md:mx-8 mt-6">
        <div class="flex items-start gap-3.5 rounded-2xl border border-[color-mix(in_srgb,var(--warning)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--warning)_8%,var(--bg-surface))] p-4 text-sm text-[var(--text-primary)] shadow-sm">
          <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_18%,var(--bg-surface))] text-[var(--warning)]">
            <Key class="h-4 w-4 stroke-[2]" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2 font-semibold text-[var(--text-primary)]">
              <span>Client Secret 只显示一次</span>
              <Badge variant="warning" size="sm">请立即妥善保存</Badge>
            </div>
            <p class="mt-1 text-xs text-[var(--text-secondary)]">离开此页面或重新加载后将无法再次查看该密钥。如丢失，只能重新轮换生成。</p>
            <div class="mt-3 flex items-center gap-2">
              <code class="flex-1 break-all rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3 py-2 font-mono text-xs text-[var(--text-primary)] select-all">{{ oauthAppSecret }}</code>
              <Button
                size="sm"
                variant="secondary"
                class="shrink-0"
                @click="copySecret(oauthAppSecret)"
              >
                <component :is="copiedSecret ? Check : Copy" class="w-3.5 h-3.5" />
                <span>{{ copiedSecret ? '已复制' : '复制' }}</span>
              </Button>
            </div>
          </div>
        </div>
      </div>

      <!-- App Registration Form -->
      <form class="space-y-5 p-6 md:p-8" @submit.prevent="saveOAuthApp">
        <div class="grid min-w-0 gap-5 md:grid-cols-2">
          <div>
            <label class="mb-1.5 block text-xs font-semibold tracking-wide text-[var(--text-secondary)]">应用名称 *</label>
            <Input
              v-model="oauthAppForm.name"
              type="text"
              placeholder="例如：Flash Sale App"
            />
          </div>
          <div>
            <label class="mb-1.5 block text-xs font-semibold tracking-wide text-[var(--text-secondary)]">应用状态</label>
            <div class="flex h-[44px] items-center">
              <label class="flex items-center gap-2.5 text-sm text-[var(--text-primary)] cursor-pointer select-none">
                <input
                  v-model="oauthAppForm.enabled"
                  type="checkbox"
                  class="h-4 w-4 rounded-md text-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface-soft)] focus:ring-[var(--accent)] focus:ring-offset-0 cursor-pointer"
                />
                <span class="font-medium">启用此 OAuth App</span>
              </label>
            </div>
          </div>
        </div>

        <!-- 授权 Scope 多选下拉框 -->
        <div ref="scopeDropdownRef" class="relative" @keydown.esc="isScopeDropdownOpen = false">
          <div class="mb-1.5 flex items-center justify-between">
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)]">授权 Scope *</label>
            <div class="flex items-center gap-3">
              <span class="text-2xs text-[var(--text-tertiary)]">已选 {{ oauthAppForm.scopes.length }} / {{ availableOAuthScopes.length }} 项</span>
              <button
                v-if="oauthAppForm.scopes.length > 0"
                type="button"
                class="text-2xs font-medium text-[var(--text-secondary)] hover:text-[var(--error)] transition-colors"
                @click="clearAllScopes"
              >
                清空所选
              </button>
            </div>
          </div>

          <!-- 下拉框触发条（展示已选 Tag） -->
          <div
            class="min-h-[44px] h-auto cursor-pointer rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] py-2 px-3 pr-10 relative flex flex-wrap items-center gap-1.5 transition-all duration-200 hover:border-[var(--border-hover)]"
            :class="isScopeDropdownOpen ? 'ring-2 ring-[var(--accent)] border-[var(--accent)]' : ''"
            @click="toggleScopeDropdown"
          >
            <span v-if="oauthAppForm.scopes.length === 0" class="text-[var(--text-tertiary)] text-sm select-none">
              请选择授权 Scope（至少选择一项）...
            </span>
            <template v-else>
              <span
                v-for="scopeKey in oauthAppForm.scopes"
                :key="scopeKey"
                class="inline-flex items-center gap-1 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] px-2.5 py-0.5 text-xs font-mono text-[var(--text-primary)] shadow-sm transition-all hover:bg-[var(--bg-surface-soft)]"
              >
                <span>{{ scopeKey }}</span>
                <button
                  type="button"
                  class="ml-0.5 text-[var(--text-tertiary)] hover:text-[var(--error)] transition-colors"
                  title="移除此 Scope"
                  @click.stop="removeScope(scopeKey)"
                >
                  <X class="w-3 h-3" />
                </button>
              </span>
            </template>

            <!-- 右侧展开箭头图标 -->
            <div class="absolute right-3 top-1/2 -translate-y-1/2 flex items-center pointer-events-none text-[var(--text-tertiary)]">
              <ChevronDown
                class="h-4 w-4 transition-transform duration-200"
                :class="isScopeDropdownOpen ? 'rotate-180 text-[var(--accent)]' : ''"
              />
            </div>
          </div>

          <!-- 下拉浮层面板 -->
          <div
            v-if="isScopeDropdownOpen"
            class="absolute left-0 right-0 z-50 mt-2 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-2xl overflow-hidden animate-fade-in"
          >
            <!-- 搜索与快捷操作 -->
            <div class="p-3 border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)]/60 flex flex-col sm:flex-row items-stretch sm:items-center gap-2.5 justify-between">
              <div class="relative flex-1">
                <input
                  v-model="scopeSearchQuery"
                  type="text"
                  class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] pl-8 pr-7 py-1.5 text-xs text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]"
                  placeholder="搜索 Scope 标识、名称或资源..."
                  @click.stop
                />
                <Search class="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-[var(--text-tertiary)] pointer-events-none" />
                <button
                  v-if="scopeSearchQuery"
                  type="button"
                  class="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] text-xs p-1"
                  @click.stop="scopeSearchQuery = ''"
                >
                  <X class="w-3 h-3" />
                </button>
              </div>
              <div class="flex items-center gap-2 shrink-0 justify-end">
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-xs"
                  @click.stop="selectAllScopes"
                >
                  全选 ({{ availableOAuthScopes.length }})
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-xs text-[var(--text-secondary)] hover:text-[var(--error)]"
                  @click.stop="clearAllScopes"
                >
                  清空
                </Button>
              </div>
            </div>

            <!-- 选项列表 -->
            <div class="max-h-72 overflow-y-auto p-2.5 space-y-1.5 text-sm bg-[var(--bg-surface)]">
              <div
                v-for="scope in filteredOAuthScopes"
                :key="scope.scope"
                class="flex items-start gap-3 rounded-xl border p-3 transition-all duration-150 cursor-pointer"
                :class="oauthAppForm.scopes.includes(scope.scope)
                  ? 'bg-[var(--bg-surface-soft)] border-[var(--accent)] shadow-sm'
                  : 'bg-[var(--bg-surface)] border-[var(--border-color)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                @click.stop="toggleScope(scope.scope)"
              >
                <input
                  type="checkbox"
                  class="mt-1 h-4 w-4 rounded-md text-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface)] focus:ring-[var(--accent)] focus:ring-offset-0 cursor-pointer"
                  :checked="oauthAppForm.scopes.includes(scope.scope)"
                  @click.stop="toggleScope(scope.scope)"
                />
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-1.5">
                    <span class="rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] px-2 py-0.5 font-mono text-xs font-semibold text-[var(--text-primary)]">
                      {{ scope.scope }}
                    </span>
                    <Badge :variant="getScopeRiskVariant(scope.risk)" size="sm">
                      {{ formatScopeRisk(scope.risk) }}
                    </Badge>
                    <Badge variant="default" size="sm">
                      {{ formatScopeAccess(scope.access) }}
                    </Badge>
                    <span class="text-xs font-medium text-[var(--text-primary)] ml-1">{{ scope.title }}</span>
                  </div>
                  <div class="mt-1 text-xs text-[var(--text-secondary)] break-words leading-relaxed">
                    {{ scope.description }}
                  </div>
                  <div class="mt-1 font-mono text-2xs text-[var(--text-tertiary)] break-all">
                    {{ scope.resources.join(', ') }}
                  </div>
                </div>
              </div>
              <div v-if="filteredOAuthScopes.length === 0" class="py-8 text-center text-xs text-[var(--text-tertiary)]">
                未找到匹配的 Scope
              </div>
            </div>
          </div>
        </div>

        <div>
          <label class="mb-1.5 block text-xs font-semibold tracking-wide text-[var(--text-secondary)]">Redirect URI，每行一个 *</label>
          <textarea
            v-model="oauthAppForm.redirectUris"
            class="w-full min-h-[96px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-sm font-mono text-[var(--text-primary)] placeholder-[var(--text-tertiary)] transition-all duration-200 hover:border-[var(--border-hover)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)] focus:border-[var(--accent)] resize-y"
            placeholder="https://example.com/oauth/callback"
          ></textarea>
        </div>

        <div class="flex items-center gap-3 pt-2">
          <Button
            type="submit"
            variant="primary"
            :loading="oauthAppSaving"
          >
            {{ oauthAppSaving ? '保存中...' : (oauthAppForm.id ? '更新应用' : '创建应用') }}
          </Button>
          <Button
            type="button"
            variant="secondary"
            @click="resetOAuthAppForm"
          >
            清空
          </Button>
        </div>
      </form>

      <!-- Mobile Cards: App List (EXACT GUARD MATCH 1/2) -->
      <div class="mt-6 space-y-3 lg:hidden">
        <div class="space-y-3 px-6 pb-6">
          <EmptyState
            v-if="oauthApps.length === 0"
            title="暂无对外 OAuth 应用"
            description="通过上方表单创建您的第一个对外 OAuth 应用"
          />
          <div
            v-for="appItem in oauthApps"
            :key="appItem.id"
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/50 p-4 text-sm transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-semibold text-[var(--text-primary)] truncate">{{ appItem.name }}</div>
                <div class="mt-1 break-all font-mono text-xs text-[var(--text-secondary)] select-all">{{ appItem.clientId }}</div>
              </div>
              <Badge :variant="appItem.enabled ? 'success' : 'warning'" size="sm">
                {{ appItem.enabled ? '启用' : '停用' }}
              </Badge>
            </div>
            <div class="mt-3 space-y-2.5 text-xs text-[var(--text-secondary)]">
              <div>
                <div class="mb-1 font-medium text-[var(--text-primary)]">Redirect URI</div>
                <div v-for="uri in appItem.redirectUris" :key="uri" class="break-all font-mono bg-[var(--bg-surface)] px-2 py-1 rounded-lg border border-[var(--border-color)] mt-1 select-all">{{ uri }}</div>
              </div>
              <div>
                <div class="mb-1 font-medium text-[var(--text-primary)]">Scope</div>
                <div class="flex flex-wrap gap-1.5">
                  <span
                    v-for="scope in appItem.scopes"
                    :key="scope"
                    class="rounded-full bg-[var(--bg-surface)] px-2.5 py-0.5 font-mono text-2xs text-[var(--text-primary)] border border-[var(--border-color)]"
                  >
                    {{ scope }}
                  </span>
                </div>
              </div>
            </div>
            <div class="mt-4 pt-3 border-t border-[var(--border-color)]/60 flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" @click="editOAuthApp(appItem)">
                <Pencil class="w-3.5 h-3.5" />
                <span>编辑</span>
              </Button>
              <Button size="sm" variant="secondary" @click="rotateOAuthAppSecret(appItem)">
                <RefreshCw class="w-3.5 h-3.5" />
                <span>轮换 Secret</span>
              </Button>
              <Button size="sm" variant="ghost" class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]" @click="deleteOAuthApp(appItem)">
                <Trash2 class="w-3.5 h-3.5" />
                <span>删除</span>
              </Button>
            </div>
          </div>
        </div>
      </div>

      <!-- Desktop Table: App List (EXACT GUARD MATCH 1/2) -->
      <div class="mt-6 hidden overflow-hidden lg:block">
        <table class="w-full table-fixed text-sm">
          <thead>
            <tr class="border-b border-[var(--border-color)] text-left text-2xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] bg-[var(--bg-surface-soft)]/50">
              <th class="w-[20%] py-3.5 pr-4 pl-6">应用</th>
              <th class="w-[24%] py-3.5 pr-4">Redirect URI</th>
              <th class="w-[25%] py-3.5 pr-4">Scope</th>
              <th class="w-[10%] py-3.5 pr-4">状态</th>
              <th class="w-[21%] py-3.5 pr-6">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]/60">
            <tr
              v-for="appItem in oauthApps"
              :key="appItem.id"
              class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
            >
              <td class="py-3.5 pr-4 pl-6">
                <div class="font-semibold text-[var(--text-primary)]">{{ appItem.name }}</div>
                <div class="break-all font-mono text-xs text-[var(--text-secondary)] select-all">{{ appItem.clientId }}</div>
              </td>
              <td class="py-3.5 pr-4 font-mono text-xs text-[var(--text-secondary)]">
                <div v-for="uri in appItem.redirectUris" :key="uri" class="break-all select-all">{{ uri }}</div>
              </td>
              <td class="py-3.5 pr-4">
                <div class="flex flex-wrap gap-1">
                  <span
                    v-for="scope in appItem.scopes"
                    :key="scope"
                    class="rounded-full bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-2xs text-[var(--text-primary)] border border-[var(--border-color)]"
                  >
                    {{ scope }}
                  </span>
                </div>
              </td>
              <td class="py-3.5 pr-4">
                <Badge :variant="appItem.enabled ? 'success' : 'warning'" size="sm">
                  {{ appItem.enabled ? '启用' : '停用' }}
                </Badge>
              </td>
              <td class="py-3.5 pr-6">
                <div class="flex items-center gap-1.5">
                  <Button size="sm" variant="secondary" title="编辑应用" @click="editOAuthApp(appItem)">
                    <Pencil class="w-3.5 h-3.5" />
                    <span>编辑</span>
                  </Button>
                  <Button size="sm" variant="secondary" title="轮换 Secret" @click="rotateOAuthAppSecret(appItem)">
                    <RefreshCw class="w-3.5 h-3.5" />
                    <span>轮换</span>
                  </Button>
                  <Button size="sm" variant="ghost" class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]" title="删除应用" @click="deleteOAuthApp(appItem)">
                    <Trash2 class="w-3.5 h-3.5" />
                    <span>删除</span>
                  </Button>
                </div>
              </td>
            </tr>
            <tr v-if="oauthApps.length === 0">
              <td colspan="5" class="py-12 text-center">
                <EmptyState
                  title="暂无对外 OAuth 应用"
                  description="通过上方表单创建您的第一个对外 OAuth 应用"
                />
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </Card>

    <!-- OAuth 授权审计 -->
    <Card padding="none" class="overflow-hidden">
      <template #header>
        <div class="flex flex-col gap-3 md:flex-row md:items-center md:justify-between w-full">
          <div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">OAuth 授权审计</h3>
            <p class="mt-0.5 text-xs text-[var(--text-secondary)]">按应用、用户和状态筛选第三方授权记录，可撤销授权并使关联 access token / refresh token 失效。</p>
          </div>
          <Button
            size="sm"
            variant="secondary"
            class="shrink-0"
            :loading="oauthAuthorizationsLoading"
            @click="loadOAuthAuthorizations"
          >
            <RefreshCw class="w-3.5 h-3.5" />
            <span>{{ oauthAuthorizationsLoading ? '加载中...' : '刷新授权' }}</span>
          </Button>
        </div>
      </template>

      <!-- Filter Controls Bar -->
      <div class="grid gap-3 px-6 pt-6 md:px-8 md:grid-cols-[200px_minmax(0,1fr)_160px_auto]">
        <Select
          :model-value="oauthAuthorizationFilters.appId ? String(oauthAuthorizationFilters.appId) : ''"
          :options="appFilterOptions"
          placeholder="全部 OAuth App"
          size="md"
          @update:model-value="(val: any) => oauthAuthorizationFilters.appId = val ? Number(val) : null"
        />
        <Input
          v-model="oauthAuthorizationFilters.user"
          type="search"
          placeholder="按用户 ID、用户名或邮箱搜索"
          clearable
          size="md"
          @enter="applyOAuthAuthorizationFilters"
        />
        <Select
          v-model="oauthAuthorizationFilters.status"
          :options="statusOptions"
          size="md"
        />
        <Button variant="primary" size="md" @click="applyOAuthAuthorizationFilters">
          <Search class="w-4 h-4" />
          <span>筛选</span>
        </Button>
      </div>

      <!-- Mobile Cards: Authorization List (EXACT GUARD MATCH 2/2) -->
      <div class="mt-6 space-y-3 lg:hidden">
        <div class="space-y-3 px-6">
          <EmptyState
            v-if="oauthAuthorizations.length === 0"
            title="暂无 OAuth 授权记录"
            description="当前筛选条件下没有匹配的授权记录"
          />
          <div
            v-for="authorization in oauthAuthorizations"
            :key="authorization.id"
            class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/50 p-4 text-sm transition-all duration-200"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="font-semibold text-[var(--text-primary)]">{{ authorization.user.username }}</div>
                <div class="mt-0.5 break-all text-xs text-[var(--text-secondary)]">#{{ authorization.user.id }} · {{ authorization.user.email || '无邮箱' }}</div>
              </div>
              <Badge :variant="authorization.active ? 'success' : 'warning'" size="sm">
                {{ formatOAuthAuthorizationStatus(authorization) }}
              </Badge>
            </div>
            <div class="mt-3 grid gap-2 text-xs text-[var(--text-secondary)] sm:grid-cols-2">
              <div class="rounded-xl bg-[var(--bg-surface)] p-2.5 border border-[var(--border-color)]">
                <div class="text-[var(--text-tertiary)] text-2xs font-medium uppercase tracking-wider">应用</div>
                <div class="mt-1 font-semibold text-[var(--text-primary)]">{{ authorization.app.name }}</div>
                <div class="mt-0.5 break-all font-mono text-2xs">{{ authorization.app.clientId }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface)] p-2.5 border border-[var(--border-color)]">
                <div class="text-[var(--text-tertiary)] text-2xs font-medium uppercase tracking-wider">活跃 Token</div>
                <div class="mt-1 font-mono tabular-nums text-xs text-[var(--text-primary)]">Access: {{ authorization.tokenStats.activeAccessTokens }}</div>
                <div class="font-mono tabular-nums text-xs text-[var(--text-secondary)]">Refresh: {{ authorization.tokenStats.activeRefreshTokens }}</div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface)] p-2.5 border border-[var(--border-color)] sm:col-span-2">
                <div class="text-[var(--text-tertiary)] text-2xs font-medium uppercase tracking-wider">Scope</div>
                <div class="mt-1 flex flex-wrap gap-1">
                  <span
                    v-for="scope in authorization.scopes"
                    :key="scope"
                    class="rounded-full bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-2xs text-[var(--text-primary)] border border-[var(--border-color)]"
                  >
                    {{ scope }}
                  </span>
                </div>
              </div>
              <div class="rounded-xl bg-[var(--bg-surface)] p-2.5 border border-[var(--border-color)] sm:col-span-2">
                <div class="text-[var(--text-tertiary)] text-2xs font-medium uppercase tracking-wider">最后授权</div>
                <div class="mt-1 text-[var(--text-primary)]">{{ new Date(authorization.lastAuthorizedAt).toLocaleString() }}</div>
                <div v-if="authorization.revokedAt" class="mt-0.5 text-[var(--text-secondary)]">撤销：{{ new Date(authorization.revokedAt).toLocaleString() }}</div>
              </div>
            </div>
            <div class="mt-4 pt-3 border-t border-[var(--border-color)]/60">
              <Button
                size="sm"
                variant="ghost"
                class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                :disabled="Boolean(authorization.revokedAt)"
                @click="revokeOAuthAuthorization(authorization)"
              >
                <Ban class="w-3.5 h-3.5" />
                <span>撤销授权</span>
              </Button>
            </div>
          </div>
        </div>
      </div>

      <!-- Desktop Table: Authorization List (EXACT GUARD MATCH 2/2) -->
      <div class="mt-6 hidden overflow-hidden lg:block">
        <table class="w-full table-fixed text-sm">
          <thead>
            <tr class="border-b border-[var(--border-color)] text-left text-2xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)] bg-[var(--bg-surface-soft)]/50">
              <th class="w-[16%] py-3.5 pr-4 pl-6">用户</th>
              <th class="w-[16%] py-3.5 pr-4">应用</th>
              <th class="w-[19%] py-3.5 pr-4">Scope</th>
              <th class="w-[12%] py-3.5 pr-4">状态</th>
              <th class="w-[12%] py-3.5 pr-4">活跃 Token</th>
              <th class="w-[15%] py-3.5 pr-4">最后授权</th>
              <th class="w-[10%] py-3.5 pr-6">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--border-color)]/60">
            <tr
              v-for="authorization in oauthAuthorizations"
              :key="authorization.id"
              class="transition-colors hover:bg-[var(--bg-surface-soft)]/50"
            >
              <td class="py-3.5 pr-4 pl-6">
                <div class="font-semibold text-[var(--text-primary)]">{{ authorization.user.username }}</div>
                <div class="break-all text-xs text-[var(--text-secondary)]">#{{ authorization.user.id }} · {{ authorization.user.email || '无邮箱' }}</div>
              </td>
              <td class="py-3.5 pr-4">
                <div class="font-semibold text-[var(--text-primary)]">{{ authorization.app.name }}</div>
                <div class="break-all font-mono text-xs text-[var(--text-secondary)] select-all">{{ authorization.app.clientId }}</div>
              </td>
              <td class="py-3.5 pr-4">
                <div class="flex flex-wrap gap-1">
                  <span
                    v-for="scope in authorization.scopes"
                    :key="scope"
                    class="rounded-full bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-2xs text-[var(--text-primary)] border border-[var(--border-color)]"
                  >
                    {{ scope }}
                  </span>
                </div>
              </td>
              <td class="py-3.5 pr-4">
                <Badge :variant="authorization.active ? 'success' : 'warning'" size="sm">
                  {{ formatOAuthAuthorizationStatus(authorization) }}
                </Badge>
                <div v-if="authorization.revokedAt" class="mt-1 text-2xs text-[var(--text-tertiary)]">{{ new Date(authorization.revokedAt).toLocaleString() }}</div>
              </td>
              <td class="py-3.5 pr-4 font-mono tabular-nums text-xs">
                <div class="text-[var(--text-primary)]">Access: {{ authorization.tokenStats.activeAccessTokens }}</div>
                <div class="text-[var(--text-secondary)]">Refresh: {{ authorization.tokenStats.activeRefreshTokens }}</div>
              </td>
              <td class="py-3.5 pr-4 text-xs text-[var(--text-secondary)]">
                <div>{{ new Date(authorization.lastAuthorizedAt).toLocaleString() }}</div>
              </td>
              <td class="py-3.5 pr-6">
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_8%,transparent)]"
                  title="撤销授权"
                  :disabled="Boolean(authorization.revokedAt)"
                  @click="revokeOAuthAuthorization(authorization)"
                >
                  <Ban class="w-3.5 h-3.5" />
                  <span>撤销</span>
                </Button>
              </td>
            </tr>
            <tr v-if="oauthAuthorizations.length === 0">
              <td colspan="7" class="py-12 text-center">
                <EmptyState
                  title="暂无 OAuth 授权记录"
                  description="当前筛选条件下没有匹配的授权记录"
                />
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Pagination Footer -->
      <div class="flex flex-col gap-3 border-t border-[var(--border-color)] px-6 py-4 text-xs text-[var(--text-secondary)] md:px-8 md:flex-row md:items-center md:justify-between bg-[var(--bg-surface-soft)]/30">
        <div>共 <span class="font-mono tabular-nums text-[var(--text-primary)] font-semibold">{{ oauthAuthorizationTotal }}</span> 条，当前第 <span class="font-mono tabular-nums text-[var(--text-primary)] font-semibold">{{ oauthAuthorizationFilters.page }}</span> / <span class="font-mono tabular-nums text-[var(--text-primary)] font-semibold">{{ oauthAuthorizationTotalPages }}</span> 页</div>
        <div class="flex items-center gap-2">
          <Button
            size="sm"
            variant="secondary"
            :disabled="oauthAuthorizationFilters.page <= 1"
            @click="setOAuthAuthorizationPage(oauthAuthorizationFilters.page - 1)"
          >
            上一页
          </Button>
          <Button
            size="sm"
            variant="secondary"
            :disabled="oauthAuthorizationFilters.page >= oauthAuthorizationTotalPages"
            @click="setOAuthAuthorizationPage(oauthAuthorizationFilters.page + 1)"
          >
            下一页
          </Button>
        </div>
      </div>
    </Card>

    <!-- Edit Provider DrawerModal -->
    <DrawerModal
      v-model="showModal"
      :title="`${t('admin.oauth.configure')} ${editProvider ? providerInfo[editProvider]?.name : ''}`"
      max-width="max-w-lg"
    >
      <template #icon>
        <div
          class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--text-primary)] border border-[var(--border-color)]"
          v-html="editProvider ? providerInfo[editProvider]?.icon : ''"
        ></div>
      </template>

      <form class="space-y-4" @submit.prevent="saveConfig">
        <div>
          <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5">
            {{ t('admin.oauth.clientId') }} *
          </label>
          <Input
            v-model="form.clientId"
            type="text"
            :placeholder="t('admin.oauth.enterClientId')"
          />
        </div>

        <div>
          <label class="block text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-1.5">
            {{ t('admin.oauth.clientSecret') }} {{ editProvider && configs[editProvider]?.configured ? t('admin.oauth.leaveEmptyUnchanged') : '*' }}
          </label>
          <Input
            v-model="form.clientSecret"
            type="password"
            :placeholder="t('admin.oauth.enterClientSecret')"
          />
        </div>

        <div class="pt-1">
          <label class="flex items-start gap-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 cursor-pointer hover:border-[var(--border-hover)] transition-colors select-none">
            <input
              v-model="form.enabled"
              type="checkbox"
              class="mt-0.5 h-4 w-4 rounded-md text-[var(--accent)] border-[var(--border-color)] bg-[var(--bg-surface)] focus:ring-[var(--accent)] focus:ring-offset-0 cursor-pointer"
            />
            <div class="min-w-0 flex-1">
              <div class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.oauth.enableLogin') }}</div>
              <div class="text-xs text-[var(--text-secondary)] mt-0.5 leading-relaxed">{{ t('admin.oauth.enableLoginHint') }}</div>
            </div>
          </label>
        </div>
      </form>

      <template #footer>
        <div class="flex items-center justify-end gap-3">
          <Button variant="secondary" @click="showModal = false">
            {{ t('common.cancel') }}
          </Button>
          <Button variant="primary" :loading="formLoading" @click="saveConfig">
            {{ formLoading ? t('admin.oauth.saving') : t('admin.oauth.save') }}
          </Button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  * {
    transition-duration: 0.001ms !important;
    animation-duration: 0.001ms !important;
  }
}
</style>
