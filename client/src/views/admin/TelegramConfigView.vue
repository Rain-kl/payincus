<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  AlertCircle,
  Bell,
  Bot,
  Crown,
  ExternalLink,
  KeyRound,
  Link as LinkIcon,
  Pencil,
  Plus,
  RefreshCw,
  Send,
  Trash2,
  Users
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import type { TelegramAdminBinding, TelegramAdminBindingsResponse } from '@/types/api'
import { systemSettingsNavigationItems } from '@/constants/adminSettings'
import { buildPublicApiUrl, getFrontendOrigin } from '@/utils/api-url'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select
} from '@/components/ui'

const route = useRoute()
const toast = useToast()
const { t } = useI18n()

const loading = ref(true)
const savingTelegramBot = ref(false)
const savingFooterTelegramLink = ref(false)
const settingTelegramWebhook = ref(false)
const checkingTelegramWebhook = ref(false)
const deletingTelegramWebhook = ref(false)

interface ConfigItem {
  id: number
  key: string
  value: string
  type: string
  label: string | null
  description: string | null
}

interface TelegramWebhookInfoView {
  url: string
  pending_update_count?: number
  last_error_date?: number
  last_error_message?: string
  max_connections?: number
  allowed_updates?: string[]
}

interface GlobalChannel {
  id: number
  name: string
  type: string
  enabled: boolean
  boundPackages: number
  configPreview: string
  createdAt: string
}

const configs = ref<ConfigItem[]>([])
const telegramWebhookInfo = ref<TelegramWebhookInfoView | null>(null)
const globalChannels = ref<GlobalChannel[]>([])
const channelLoading = ref(false)
const savingChannel = ref(false)
const deletingChannelId = ref<number | null>(null)
const testingChannelId = ref<number | null>(null)
const showChannelForm = ref(false)
const editingChannel = ref<{ id: number; name: string; botToken: string; chatId: string; enabled: boolean } | null>(null)
const channelFormError = ref('')
const channelForm = ref({ name: '', botToken: '', chatId: '', enabled: true })
const bindingLoading = ref(false)
const bindingSearch = ref('')
const telegramBindings = ref<TelegramAdminBinding[]>([])
const telegramBindingsTotal = ref(0)
const telegramBindingsPage = ref(1)
const telegramBindingsPageSize = ref(5)
const unlinkingBindingId = ref<number | null>(null)
const telegramBindingGroup = ref<TelegramAdminBindingsResponse['group'] | null>(null)
const telegramBindingVipGroup = ref<TelegramAdminBindingsResponse['vipGroup'] | null>(null)

const form = ref({
  footer_telegram_link: '',
  telegram_bot_enabled: false,
  telegram_bot_username: '',
  telegram_bot_token: '',
  telegram_webhook_secret: '',
  telegram_group_join_enabled: false,
  telegram_group_chat_id: '',
  telegram_group_join_mode: 'any',
  telegram_group_min_recharge: 0,
  telegram_group_min_consume: 0,
  telegram_group_invite_expire_minutes: 30,
  telegram_vip_group_join_enabled: false,
  telegram_vip_group_chat_id: '',
  telegram_vip_group_join_mode: 'all',
  telegram_vip_group_min_recharge: 0,
  telegram_vip_group_min_consume: 0,
  telegram_vip_group_invite_expire_minutes: 30
})

const numericConfigKeys = ['telegram_group_invite_expire_minutes', 'telegram_vip_group_invite_expire_minutes']
const floatConfigKeys = ['telegram_group_min_recharge', 'telegram_group_min_consume', 'telegram_vip_group_min_recharge', 'telegram_vip_group_min_consume']
const booleanConfigKeys = ['telegram_bot_enabled', 'telegram_group_join_enabled', 'telegram_vip_group_join_enabled']
const stringConfigKeys = [
  'footer_telegram_link',
  'telegram_bot_username',
  'telegram_bot_token',
  'telegram_webhook_secret',
  'telegram_group_chat_id',
  'telegram_group_join_mode',
  'telegram_vip_group_chat_id',
  'telegram_vip_group_join_mode'
]

const telegramBotKeys = [
  'telegram_bot_enabled',
  'telegram_bot_username',
  'telegram_bot_token',
  'telegram_webhook_secret',
  'telegram_group_join_enabled',
  'telegram_group_chat_id',
  'telegram_group_join_mode',
  'telegram_group_min_recharge',
  'telegram_group_min_consume',
  'telegram_group_invite_expire_minutes',
  'telegram_vip_group_join_enabled',
  'telegram_vip_group_chat_id',
  'telegram_vip_group_join_mode',
  'telegram_vip_group_min_recharge',
  'telegram_vip_group_min_consume',
  'telegram_vip_group_invite_expire_minutes'
]
const footerTelegramLinkKeys = ['footer_telegram_link']

const maskedSecretPlaceholder = '********'
const telegramWebhookSecretPattern = /^[A-Za-z0-9_-]{1,256}$/

const telegramWebhookUrl = computed(() => buildPublicApiUrl('/telegram/webhook'))

const telegramWebhookSecretError = computed(() => {
  const value = form.value.telegram_webhook_secret
  if (!value || value === maskedSecretPlaceholder) {
    return ''
  }
  return telegramWebhookSecretPattern.test(value)
    ? ''
    : t('telegramConfig.secretInvalid')
})

const hasTelegramBotChanges = computed(() => {
  return telegramBotKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const canManageTelegramWebhook = computed(() => {
  return Boolean(form.value.telegram_bot_token && form.value.telegram_webhook_secret && !telegramWebhookSecretError.value)
})

const hasFooterTelegramLinkChanges = computed(() => {
  return footerTelegramLinkKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const telegramBindingsTotalPages = computed(() => {
  return Math.max(1, Math.ceil(telegramBindingsTotal.value / telegramBindingsPageSize.value))
})

onMounted(async () => {
  await Promise.all([loadConfigs(), loadChannels(), loadTelegramBindings()])
})

async function loadConfigs() {
  loading.value = true
  try {
    const response = await api.systemConfig.list()
    configs.value = Array.isArray(response.configs) ? response.configs : []

    for (const config of configs.value) {
      if (!(config.key in form.value)) continue

      if (numericConfigKeys.includes(config.key)) {
        ;(form.value as any)[config.key] = parseInt(config.value, 10) || 0
      } else if (floatConfigKeys.includes(config.key)) {
        ;(form.value as any)[config.key] = parseFloat(config.value) || 0
      } else if (booleanConfigKeys.includes(config.key)) {
        ;(form.value as any)[config.key] = config.value === 'true'
      } else if (stringConfigKeys.includes(config.key)) {
        ;(form.value as any)[config.key] = config.value || ''
      }
    }
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.configLoadFailed', { error: err?.message || String(err) }))
    configs.value = []
  } finally {
    loading.value = false
  }
}

async function saveConfigGroup(keys: string[], savingRef: { value: boolean }) {
  savingRef.value = true
  try {
    const configsToUpdate = keys.map(key => ({
      key,
      value: typeof (form.value as any)[key] === 'boolean'
        ? ((form.value as any)[key] ? 'true' : 'false')
        : String((form.value as any)[key])
    }))

    await api.systemConfig.update(configsToUpdate)
    toast.success(t('telegramConfig.messages.saveSuccess'))
    await loadConfigs()
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.saveFailed', { error: err?.message || String(err) }))
  } finally {
    savingRef.value = false
  }
}

async function saveTelegramBot() {
  if (telegramWebhookSecretError.value) {
    toast.error(telegramWebhookSecretError.value)
    return
  }
  if (!['any', 'all'].includes(form.value.telegram_group_join_mode)) {
    toast.error(t('telegramConfig.messages.groupModeInvalid'))
    return
  }
  if (!['any', 'all'].includes(form.value.telegram_vip_group_join_mode)) {
    toast.error(t('telegramConfig.messages.vipGroupModeInvalid'))
    return
  }
  if (form.value.telegram_group_invite_expire_minutes < 1 || form.value.telegram_group_invite_expire_minutes > 10080) {
    toast.error(t('telegramConfig.messages.groupExpiryInvalid'))
    return
  }
  if (form.value.telegram_vip_group_invite_expire_minutes < 1 || form.value.telegram_vip_group_invite_expire_minutes > 10080) {
    toast.error(t('telegramConfig.messages.vipGroupExpiryInvalid'))
    return
  }
  await saveConfigGroup(telegramBotKeys, savingTelegramBot)
}

async function saveFooterTelegramLink() {
  await saveConfigGroup(footerTelegramLinkKeys, savingFooterTelegramLink)
}

function generateTelegramWebhookSecret() {
  const cryptoApi = globalThis.crypto
  if (!cryptoApi?.getRandomValues) {
    toast.error(t('telegramConfig.messages.randomUnsupported'))
    return
  }

  const bytes = new Uint8Array(32)
  cryptoApi.getRandomValues(bytes)
  form.value.telegram_webhook_secret = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
  toast.success(t('telegramConfig.messages.secretGenerated'))
}

function getApiErrorMessage(err: unknown): string {
  if (err && typeof err === 'object') {
    const apiError = err as { error?: unknown; message?: unknown; description?: unknown }
    if (apiError.error) return String(apiError.error)
    if (apiError.message) return String(apiError.message)
    if (apiError.description) return String(apiError.description)
  }
  return String(err)
}

function formatTelegramWebhookDate(value?: number): string {
  if (!value) return ''
  return new Date(value * 1000).toLocaleString()
}

function formatDate(value: string): string {
  return new Date(value).toLocaleString()
}

function formatMoney(value: number): string {
  return Number(value || 0).toFixed(2)
}

function formatTelegramName(binding: TelegramAdminBinding): string {
  if (binding.telegramUsername) return `@${binding.telegramUsername}`
  const fullName = [binding.firstName, binding.lastName].filter(Boolean).join(' ')
  return fullName || `ID ${binding.telegramUserId}`
}

function eligibilityLabel(status: TelegramAdminBinding['eligibility']['status']): string {
  switch (status) {
    case 'eligible':
      return t('telegramConfig.eligibility.eligible')
    case 'ineligible':
      return t('telegramConfig.eligibility.ineligible')
    case 'disabled':
      return t('telegramConfig.eligibility.disabled')
    case 'unconfigured':
      return t('telegramConfig.eligibility.unconfigured')
    default:
      return t('telegramConfig.eligibility.unknown')
  }
}

function eligibilityBadgeVariant(status: TelegramAdminBinding['eligibility']['status']): 'success' | 'warning' | 'default' {
  switch (status) {
    case 'eligible':
      return 'success'
    case 'ineligible':
      return 'warning'
    default:
      return 'default'
  }
}

function formatGroupRule(group: TelegramAdminBindingsResponse['group'] | null): string {
  if (!group) return t('common.loading')
  if (!group.enabled) return t('telegramConfig.eligibility.disabled')
  if (!group.configured) return t('telegramConfig.groupRule.chatIdMissing')

  const thresholds = [
    group.minRecharge > 0 ? t('telegramConfig.groupRule.recharge', { amount: formatMoney(group.minRecharge) }) : null,
    group.minConsume > 0 ? t('telegramConfig.groupRule.consume', { amount: formatMoney(group.minConsume) }) : null
  ].filter(Boolean)

  if (thresholds.length === 0) {
    return t('telegramConfig.groupRule.noThreshold')
  }

  return t(group.joinMode === 'all' ? 'telegramConfig.groupRule.all' : 'telegramConfig.groupRule.any', { thresholds: thresholds.join(t('telegramConfig.groupRule.separator')) })
}

async function setupTelegramWebhook() {
  settingTelegramWebhook.value = true
  try {
    const response = await api.telegram.setupWebhook({
      baseUrl: getFrontendOrigin()
    })
    toast.success(response.commandsSynced ? t('telegramConfig.messages.webhookCommandsSet') : t('telegramConfig.messages.webhookSet'))
    telegramWebhookInfo.value = {
      ...(telegramWebhookInfo.value || { url: '' }),
      url: response.webhookUrl
    }
    await checkTelegramWebhook(false)
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.webhookSetFailed', { error: getApiErrorMessage(err) }))
  } finally {
    settingTelegramWebhook.value = false
  }
}

async function checkTelegramWebhook(showSuccess = true) {
  checkingTelegramWebhook.value = true
  try {
    const response = await api.telegram.getWebhookInfo()
    telegramWebhookInfo.value = response.info
    if (showSuccess) {
      toast.success(t('telegramConfig.messages.webhookUpdated'))
    }
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.webhookCheckFailed', { error: getApiErrorMessage(err) }))
  } finally {
    checkingTelegramWebhook.value = false
  }
}

async function deleteTelegramWebhook() {
  if (!confirm(t('telegramConfig.messages.webhookDeleteConfirm'))) return

  deletingTelegramWebhook.value = true
  try {
    await api.telegram.deleteWebhook()
    telegramWebhookInfo.value = { url: '' }
    toast.success(t('telegramConfig.messages.webhookDeleted'))
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.webhookDeleteFailed', { error: getApiErrorMessage(err) }))
  } finally {
    deletingTelegramWebhook.value = false
  }
}

async function loadTelegramBindings(page = telegramBindingsPage.value) {
  bindingLoading.value = true
  try {
    const response = await api.telegram.listBindings({
      page,
      pageSize: telegramBindingsPageSize.value,
      search: bindingSearch.value.trim() || undefined
    })
    telegramBindings.value = response.bindings
    telegramBindingsTotal.value = response.total
    telegramBindingsPage.value = response.page
    telegramBindingsPageSize.value = response.pageSize
    telegramBindingGroup.value = response.group
    telegramBindingVipGroup.value = response.vipGroup
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.bindingsLoadFailed', { error: err?.message || String(err) }))
    telegramBindings.value = []
    telegramBindingsTotal.value = 0
    telegramBindingGroup.value = null
    telegramBindingVipGroup.value = null
  } finally {
    bindingLoading.value = false
  }
}

async function searchTelegramBindings() {
  telegramBindingsPage.value = 1
  await loadTelegramBindings(1)
}

async function resetTelegramBindingSearch() {
  bindingSearch.value = ''
  telegramBindingsPage.value = 1
  await loadTelegramBindings(1)
}

async function goTelegramBindingsPage(page: number) {
  if (page < 1 || page > telegramBindingsTotalPages.value || page === telegramBindingsPage.value) return
  await loadTelegramBindings(page)
}

async function unlinkTelegramBinding(binding: TelegramAdminBinding) {
  const userLabel = binding.user?.username || t('telegramConfig.userNumber', { id: binding.userId })
  if (!confirm(t('telegramConfig.messages.unlinkConfirm', { user: userLabel }))) return

  unlinkingBindingId.value = binding.id
  try {
    await api.telegram.unlinkAdminBinding(binding.id)
    toast.success(t('telegramConfig.messages.unlinked'))
    await loadTelegramBindings()
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.unlinkFailed', { error: err?.message || String(err) }))
  } finally {
    unlinkingBindingId.value = null
  }
}

async function loadChannels() {
  channelLoading.value = true
  try {
    const response = await api.adminNotificationChannels.list()
    globalChannels.value = response.channels || []
  } catch {
    globalChannels.value = []
  } finally {
    channelLoading.value = false
  }
}

function openCreateChannelForm() {
  editingChannel.value = null
  channelForm.value = { name: '', botToken: '', chatId: '', enabled: true }
  channelFormError.value = ''
  showChannelForm.value = true
}

async function openEditChannelForm(ch: GlobalChannel) {
  channelFormError.value = ''
  try {
    const response = await api.adminNotificationChannels.get(ch.id)
    const channel = response.channel as any
    const config = channel.config || {}
    const botToken = typeof config.botToken === 'string' ? config.botToken : ''
    const chatId = typeof config.chatId === 'string' ? config.chatId : ''

    editingChannel.value = {
      id: channel.id,
      name: channel.name,
      botToken,
      chatId,
      enabled: channel.enabled
    }
    channelForm.value = {
      name: channel.name,
      botToken,
      chatId,
      enabled: channel.enabled
    }
    showChannelForm.value = true
  } catch {
    toast.error(t('telegramConfig.messages.channelLoadFailed'))
  }
}

async function saveChannel() {
  channelFormError.value = ''
  if (!channelForm.value.name || !channelForm.value.chatId) {
    channelFormError.value = t('telegramConfig.messages.channelFieldsRequired')
    return
  }
  if (!editingChannel.value && !channelForm.value.botToken) {
    channelFormError.value = t('telegramConfig.messages.botTokenRequired')
    return
  }

  savingChannel.value = true
  try {
    if (editingChannel.value) {
      await api.adminNotificationChannels.update(editingChannel.value.id, {
        name: channelForm.value.name,
        chatId: channelForm.value.chatId,
        enabled: channelForm.value.enabled,
        ...(channelForm.value.botToken && !channelForm.value.botToken.includes('...') ? { botToken: channelForm.value.botToken } : {})
      })
      toast.success(t('telegramConfig.messages.channelUpdated'))
    } else {
      await api.adminNotificationChannels.create(channelForm.value)
      toast.success(t('telegramConfig.messages.channelCreated'))
    }
    showChannelForm.value = false
    await loadChannels()
  } catch (err: any) {
    channelFormError.value = err?.message || t('telegramConfig.messages.saveFailedShort')
  } finally {
    savingChannel.value = false
  }
}

async function deleteChannel(id: number) {
  if (!confirm(t('telegramConfig.messages.channelDeleteConfirm'))) return

  deletingChannelId.value = id
  try {
    await api.adminNotificationChannels.delete(id)
    toast.success(t('common.deleteSuccess'))
    await loadChannels()
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.channelDeleteFailed', { error: err?.message || '' }))
  } finally {
    deletingChannelId.value = null
  }
}

async function testChannel(id: number) {
  testingChannelId.value = id
  try {
    await api.adminNotificationChannels.test(id)
    toast.success(t('telegramConfig.messages.testSuccess'))
  } catch (err: any) {
    toast.error(t('telegramConfig.messages.testFailed', { error: err?.message || '' }))
  } finally {
    testingChannelId.value = null
  }
}
</script>

<template>
  <div class="max-w-6xl mx-auto space-y-6 pb-12 animate-fade-in">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('telegramConfig.title')"
      :description="t('telegramConfig.description')"
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

    <!-- Skeleton Loading State -->
    <div v-if="loading" class="space-y-6 animate-pulse">
      <Card v-for="i in 3" :key="i" padding="md">
        <div class="flex items-center justify-between border-b border-[var(--border-color)] pb-4 mb-6">
          <div class="space-y-2">
            <div class="h-5 w-40 bg-[var(--bg-surface-soft)] rounded-md"></div>
            <div class="h-3.5 w-64 bg-[var(--bg-surface-soft)] rounded-md"></div>
          </div>
          <div class="h-8 w-20 bg-[var(--bg-surface-soft)] rounded-full"></div>
        </div>
        <div class="space-y-4">
          <div v-for="j in 2" :key="j" class="h-14 bg-[var(--bg-surface-soft)] rounded-xl"></div>
        </div>
      </Card>
    </div>

    <!-- Main Config Sections -->
    <div v-else class="space-y-6">
      <!-- 1. Footer Telegram Link Card -->
      <Card>
        <template #title>
          <div class="flex items-center gap-2.5">
            <div class="w-8 h-8 rounded-xl bg-[var(--accent)]/10 text-[var(--accent)] flex items-center justify-center shrink-0">
              <LinkIcon class="w-4 h-4" />
            </div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">
              {{ t('telegramConfig.footer.title') }}
            </h3>
          </div>
        </template>
        <template #subtitle>
          <p class="text-xs text-[var(--text-secondary)] mt-1">
            {{ t('telegramConfig.footer.description') }}
          </p>
        </template>
        <template #extra>
          <Button
            size="sm"
            :disabled="!hasFooterTelegramLinkChanges || savingFooterTelegramLink"
            :loading="savingFooterTelegramLink"
            @click="saveFooterTelegramLink"
          >
            {{ savingFooterTelegramLink ? t('common.saving') : t('common.save') }}
          </Button>
        </template>

        <div class="max-w-xl">
          <Input
            v-model="form.footer_telegram_link"
            :label="t('telegramConfig.footer.link')"
            :hint="t('telegramConfig.footer.hint')"
            placeholder="https://t.me/your_group"
          />
        </div>
      </Card>

      <!-- 2. Telegram Bot & Webhook & Group Thresholds Card -->
      <Card>
        <template #title>
          <div class="flex items-center gap-2.5">
            <div class="w-8 h-8 rounded-xl bg-[var(--accent)]/10 text-[var(--accent)] flex items-center justify-center shrink-0">
              <Bot class="w-4 h-4" />
            </div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">
              {{ t('telegramConfig.bot.title') }}
            </h3>
          </div>
        </template>
        <template #subtitle>
          <p class="text-xs text-[var(--text-secondary)] mt-1">
            {{ t('telegramConfig.bot.description') }}
          </p>
        </template>
        <template #extra>
          <Button
            size="sm"
            :disabled="!hasTelegramBotChanges || savingTelegramBot"
            :loading="savingTelegramBot"
            @click="saveTelegramBot"
          >
            {{ savingTelegramBot ? t('common.saving') : t('common.save') }}
          </Button>
        </template>

        <div class="space-y-6">
          <!-- Bot Enable Switch -->
          <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
            <div class="flex-1 pr-4">
              <label class="text-sm font-semibold text-[var(--text-primary)]">
                {{ t('telegramConfig.bot.enableBinding') }}
              </label>
              <p class="text-xs text-[var(--text-secondary)] mt-0.5 leading-relaxed">
                {{ t('telegramConfig.bot.disabledHint') }}
              </p>
            </div>
            <div class="flex items-center gap-3 shrink-0">
              <span class="text-xs font-medium" :class="form.telegram_bot_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                {{ t('common.close') }}
              </span>
              <button
                type="button"
                role="switch"
                :aria-checked="form.telegram_bot_enabled"
                class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                :class="form.telegram_bot_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                @click="form.telegram_bot_enabled = !form.telegram_bot_enabled"
              >
                <span
                  class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :class="form.telegram_bot_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                />
              </button>
              <span class="text-xs font-medium" :class="form.telegram_bot_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                {{ t('telegramConfig.enabled') }}
              </span>
            </div>
          </div>

          <!-- Bot Configuration Fields -->
          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <Input
              v-model="form.telegram_bot_username"
              :label="t('telegramConfig.bot.username')"
              :hint="t('telegramConfig.bot.usernameHint')"
              placeholder="your_bot_username"
            />

            <Input
              v-model="form.telegram_bot_token"
              type="password"
              label="Bot Token"
              :hint="t('telegramConfig.bot.tokenHint')"
              placeholder="********"
              class="font-mono"
            />

            <div class="md:col-span-2 space-y-1.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                Webhook Secret
              </label>
              <div class="flex flex-col sm:flex-row gap-2">
                <div class="flex-1">
                  <Input
                    v-model="form.telegram_webhook_secret"
                    type="password"
                    :error="telegramWebhookSecretError"
                    placeholder="********"
                    class="font-mono"
                  />
                </div>
                <Button
                  variant="secondary"
                  size="md"
                  class="shrink-0"
                  @click="generateTelegramWebhookSecret"
                >
                  <template #icon>
                    <KeyRound class="w-4 h-4 mr-1.5" />
                  </template>
                  {{ t('telegramConfig.bot.generateSecret') }}
                </Button>
              </div>
              <p class="text-xs text-[var(--text-tertiary)] mt-0.5">
                {{ t('telegramConfig.bot.secretHint') }}
              </p>
            </div>
          </div>

          <!-- Webhook Section Card -->
          <div class="p-5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] space-y-4">
            <!-- Address Box -->
            <div class="space-y-1.5">
              <span class="text-xs font-semibold text-[var(--text-secondary)]">{{ t('telegramConfig.webhook.address') }}</span>
              <div class="flex items-center gap-2 p-3 rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] font-mono text-xs text-[var(--text-primary)] break-all select-all">
                <code>{{ telegramWebhookUrl }}</code>
              </div>
              <p class="text-xs text-[var(--text-tertiary)]">
                {{ t('telegramConfig.webhook.addressHint') }}
              </p>
            </div>

            <!-- Webhook Controls Header -->
            <div class="flex flex-col lg:flex-row lg:items-center lg:justify-between gap-3 pt-2 border-t border-[var(--border-color)]/60">
              <div>
                <h4 class="text-sm font-semibold text-[var(--text-primary)]">{{ t('telegramConfig.webhook.title') }}</h4>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('telegramConfig.webhook.description') }}
                </p>
              </div>
              <div class="flex flex-wrap items-center gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  :disabled="!canManageTelegramWebhook || settingTelegramWebhook"
                  :loading="settingTelegramWebhook"
                  @click="setupTelegramWebhook"
                >
                  <template #icon>
                    <Send class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ settingTelegramWebhook ? t('telegramConfig.webhook.setting') : t('telegramConfig.webhook.setup') }}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  :disabled="!form.telegram_bot_token || checkingTelegramWebhook"
                  :loading="checkingTelegramWebhook"
                  @click="() => checkTelegramWebhook()"
                >
                  <template #icon>
                    <RefreshCw class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ checkingTelegramWebhook ? t('telegramConfig.webhook.checking') : t('telegramConfig.webhook.check') }}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  class="text-[var(--error)] hover:bg-[var(--error)]/10"
                  :disabled="!form.telegram_bot_token || deletingTelegramWebhook"
                  :loading="deletingTelegramWebhook"
                  @click="deleteTelegramWebhook"
                >
                  <template #icon>
                    <Trash2 class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ deletingTelegramWebhook ? t('common.deleting') : t('telegramConfig.webhook.delete') }}
                </Button>
              </div>
            </div>

            <!-- Webhook Status Details -->
            <div v-if="telegramWebhookInfo" class="space-y-2 p-3.5 rounded-xl bg-[var(--bg-surface)] border border-[var(--border-color)] text-xs text-[var(--text-secondary)]">
              <div class="flex flex-wrap items-center gap-1.5">
                <span class="font-medium text-[var(--text-primary)]">{{ t('telegramConfig.webhook.currentAddress') }}</span>
                <span class="font-mono text-[var(--text-primary)] break-all">{{ telegramWebhookInfo.url || t('common.notSet') }}</span>
              </div>
              <div class="flex flex-wrap gap-4 text-[var(--text-secondary)]">
                <span>{{ t('telegramConfig.webhook.pendingUpdates', { count: telegramWebhookInfo.pending_update_count ?? 0 }) }}</span>
                <span v-if="telegramWebhookInfo.max_connections">
                  {{ t('telegramConfig.webhook.maxConnections', { count: telegramWebhookInfo.max_connections }) }}
                </span>
                <span v-if="telegramWebhookInfo.allowed_updates?.length">
                  {{ t('telegramConfig.webhook.allowedUpdates', { updates: telegramWebhookInfo.allowed_updates.join(', ') }) }}
                </span>
              </div>
              <div v-if="telegramWebhookInfo.last_error_message" class="text-[var(--error)] flex items-start gap-1.5 mt-1">
                <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
                <span>
                  {{ t('telegramConfig.webhook.lastError', { error: telegramWebhookInfo.last_error_message }) }}
                  <span v-if="telegramWebhookInfo.last_error_date" class="text-[var(--text-tertiary)]">
                    （{{ formatTelegramWebhookDate(telegramWebhookInfo.last_error_date) }}）
                  </span>
                </span>
              </div>
            </div>
          </div>

          <!-- Regular Group Threshold Section -->
          <div class="p-5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] space-y-5">
            <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
              <div class="flex items-center gap-2">
                <Users class="w-4 h-4 text-[var(--accent)]" />
                <div>
                  <h4 class="text-sm font-semibold text-[var(--text-primary)]">{{ t('telegramConfig.group.title') }}</h4>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('telegramConfig.group.description') }}
                  </p>
                </div>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.telegram_group_join_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('common.close') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.telegram_group_join_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.telegram_group_join_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.telegram_group_join_enabled = !form.telegram_group_join_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.telegram_group_join_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.telegram_group_join_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('telegramConfig.enabled') }}
                </span>
              </div>
            </div>

            <div class="grid grid-cols-1 md:grid-cols-2 gap-5 pt-1">
              <Input
                v-model="form.telegram_group_chat_id"
                :label="t('telegramConfig.group.chatId')"
                :hint="t('telegramConfig.group.chatIdHint')"
                placeholder="-1001234567890"
                class="font-mono"
              />

              <Select
                v-model="form.telegram_group_join_mode"
                :label="t('telegramConfig.group.thresholdMode')"
                :hint="t('telegramConfig.group.thresholdHint')"
                :options="[
                  { label: t('telegramConfig.group.modeAny'), value: 'any' },
                  { label: t('telegramConfig.group.modeAll'), value: 'all' }
                ]"
              />

              <div class="space-y-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.rechargeThreshold') }}
                </label>
                <div class="relative flex items-center">
                  <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                  <input
                    v-model.number="form.telegram_group_min_recharge"
                    type="number"
                    min="0"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-8 pr-4 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="0.00"
                  />
                </div>
              </div>

              <div class="space-y-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.consumeThreshold') }}
                </label>
                <div class="relative flex items-center">
                  <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                  <input
                    v-model.number="form.telegram_group_min_consume"
                    type="number"
                    min="0"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-8 pr-4 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="0.00"
                  />
                </div>
              </div>

              <div class="space-y-1.5 md:col-span-2">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.inviteExpiry') }}
                </label>
                <div class="relative flex items-center max-w-xs">
                  <input
                    v-model.number="form.telegram_group_invite_expire_minutes"
                    type="number"
                    min="1"
                    max="10080"
                    step="1"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-3.5 pr-16 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="30"
                  />
                  <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] pointer-events-none select-none">
                    {{ t('telegramConfig.minutes') }}
                  </span>
                </div>
                <p class="text-xs text-[var(--text-tertiary)] mt-0.5">
                  {{ t('telegramConfig.group.inviteHint') }}
                </p>
              </div>
            </div>
          </div>

          <!-- VIP Group Threshold Section -->
          <div class="p-5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] space-y-5">
            <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
              <div class="flex items-center gap-2">
                <Crown class="w-4 h-4 text-amber-500" />
                <div>
                  <h4 class="text-sm font-semibold text-[var(--text-primary)]">{{ t('telegramConfig.vipGroup.title') }}</h4>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('telegramConfig.vipGroup.description') }}
                  </p>
                </div>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.telegram_vip_group_join_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('common.close') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.telegram_vip_group_join_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.telegram_vip_group_join_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.telegram_vip_group_join_enabled = !form.telegram_vip_group_join_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.telegram_vip_group_join_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.telegram_vip_group_join_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('telegramConfig.enabled') }}
                </span>
              </div>
            </div>

            <div class="grid grid-cols-1 md:grid-cols-2 gap-5 pt-1">
              <Input
                v-model="form.telegram_vip_group_chat_id"
                :label="t('telegramConfig.vipGroup.chatId')"
                :hint="t('telegramConfig.vipGroup.chatIdHint')"
                placeholder="-1001234567890"
                class="font-mono"
              />

              <Select
                v-model="form.telegram_vip_group_join_mode"
                :label="t('telegramConfig.group.thresholdMode')"
                :hint="t('telegramConfig.vipGroup.thresholdHint')"
                :options="[
                  { label: t('telegramConfig.group.modeAny'), value: 'any' },
                  { label: t('telegramConfig.group.modeAll'), value: 'all' }
                ]"
              />

              <div class="space-y-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.rechargeThreshold') }}
                </label>
                <div class="relative flex items-center">
                  <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                  <input
                    v-model.number="form.telegram_vip_group_min_recharge"
                    type="number"
                    min="0"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-8 pr-4 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="0.00"
                  />
                </div>
              </div>

              <div class="space-y-1.5">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.consumeThreshold') }}
                </label>
                <div class="relative flex items-center">
                  <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                  <input
                    v-model.number="form.telegram_vip_group_min_consume"
                    type="number"
                    min="0"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-8 pr-4 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="0.00"
                  />
                </div>
              </div>

              <div class="space-y-1.5 md:col-span-2">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('telegramConfig.group.inviteExpiry') }}
                </label>
                <div class="relative flex items-center max-w-xs">
                  <input
                    v-model.number="form.telegram_vip_group_invite_expire_minutes"
                    type="number"
                    min="1"
                    max="10080"
                    step="1"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-3.5 pr-16 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                    placeholder="30"
                  />
                  <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] pointer-events-none select-none">
                    {{ t('telegramConfig.minutes') }}
                  </span>
                </div>
                <p class="text-xs text-[var(--text-tertiary)] mt-0.5">
                  {{ t('telegramConfig.vipGroup.inviteHint') }}
                </p>
              </div>
            </div>
          </div>
        </div>
      </Card>

      <!-- 3. Telegram User Bindings Card -->
      <Card>
        <template #title>
          <div class="flex items-center gap-2.5">
            <div class="w-8 h-8 rounded-xl bg-[var(--accent)]/10 text-[var(--accent)] flex items-center justify-center shrink-0">
              <Users class="w-4 h-4" />
            </div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">
              {{ t('telegramConfig.bindings.title') }}
            </h3>
          </div>
        </template>
        <template #subtitle>
          <p class="text-xs text-[var(--text-secondary)] mt-1">
            {{ t('telegramConfig.bindings.description') }}
          </p>
        </template>
        <template #extra>
          <Button
            variant="secondary"
            size="sm"
            :disabled="bindingLoading"
            :loading="bindingLoading"
            @click="() => loadTelegramBindings()"
          >
            <template #icon>
              <RefreshCw class="w-3.5 h-3.5 mr-1" />
            </template>
            {{ bindingLoading ? t('telegramConfig.bindings.refreshing') : t('telegramConfig.bindings.refresh') }}
          </Button>
        </template>

        <div class="space-y-4">
          <!-- Search Bar -->
          <div class="flex flex-col sm:flex-row gap-2.5">
            <div class="flex-1">
              <Input
                v-model="bindingSearch"
                type="search"
                size="sm"
                :placeholder="t('telegramConfig.bindings.searchPlaceholder')"
                clearable
                @enter="searchTelegramBindings"
                @clear="resetTelegramBindingSearch"
              />
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <Button
                size="sm"
                :disabled="bindingLoading"
                @click="searchTelegramBindings"
              >
                {{ t('common.search') }}
              </Button>
              <Button
                v-if="bindingSearch"
                variant="ghost"
                size="sm"
                :disabled="bindingLoading"
                @click="resetTelegramBindingSearch"
              >
                {{ t('telegramConfig.bindings.clear') }}
              </Button>
            </div>
          </div>

          <!-- Threshold Summary Cards -->
          <div class="grid grid-cols-1 md:grid-cols-2 gap-3 text-xs">
            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <div class="text-[var(--text-primary)] font-semibold">{{ t('telegramConfig.bindings.groupThreshold') }}</div>
              <div class="text-[var(--text-secondary)] mt-1 leading-relaxed">{{ formatGroupRule(telegramBindingGroup) }}</div>
            </div>
            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <div class="text-[var(--text-primary)] font-semibold">{{ t('telegramConfig.bindings.vipGroupThreshold') }}</div>
              <div class="text-[var(--text-secondary)] mt-1 leading-relaxed">{{ formatGroupRule(telegramBindingVipGroup) }}</div>
            </div>
          </div>

          <!-- Bindings List / Loading / Empty -->
          <div v-if="bindingLoading" class="py-8 text-center text-sm text-[var(--text-tertiary)]">
            <div class="animate-pulse space-y-3">
              <div v-for="i in 3" :key="i" class="h-16 bg-[var(--bg-surface-soft)] rounded-xl"></div>
            </div>
          </div>
          <EmptyState
            v-else-if="telegramBindings.length === 0"
            :title="t('telegramConfig.bindings.empty')"
          />
          <div v-else class="space-y-2.5">
            <div
              v-for="binding in telegramBindings"
              :key="binding.id"
              class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-colors hover:border-[var(--border-hover)]"
            >
              <div class="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="text-sm font-semibold text-[var(--text-primary)]">
                      {{ binding.user?.username || t('telegramConfig.userNumber', { id: binding.userId }) }}
                    </span>
                    <Badge
                      v-if="binding.user"
                      :variant="binding.user.status === 'active' ? 'success' : 'error'"
                      size="sm"
                    >
                      {{ binding.user.status === 'active' ? t('telegramConfig.bindings.active') : t('telegramConfig.bindings.banned') }}
                    </Badge>
                    <Badge variant="default" size="sm">
                      {{ t('telegramConfig.bindings.userId', { id: binding.userId }) }}
                    </Badge>
                    <span class="text-xs font-mono text-[var(--accent)] font-medium">Telegram {{ formatTelegramName(binding) }}</span>
                    <span class="text-xs font-mono text-[var(--text-tertiary)]">ID: {{ binding.telegramUserId }}</span>
                  </div>
                  <div class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-[var(--text-secondary)]">
                    <span v-if="binding.user?.email" class="break-all">{{ binding.user.email }}</span>
                    <span class="tabular-nums font-medium text-[var(--text-primary)]">
                      {{ t('telegramConfig.bindings.recharge', { amount: formatMoney(binding.stats.totalRecharge) }) }}
                    </span>
                    <span class="tabular-nums font-medium text-[var(--text-primary)]">
                      {{ t('telegramConfig.bindings.consume', { amount: formatMoney(binding.stats.totalConsume) }) }}
                    </span>
                    <span class="text-[var(--text-tertiary)]">
                      {{ t('telegramConfig.bindings.boundAt', { time: formatDate(binding.boundAt) }) }}
                    </span>
                  </div>
                </div>

                <div class="flex flex-wrap items-center gap-2 xl:ml-4 xl:justify-end shrink-0">
                  <Badge :variant="eligibilityBadgeVariant(binding.eligibility.status)" size="sm">
                    {{ t('telegramConfig.bindings.normal') }}{{ eligibilityLabel(binding.eligibility.status) }}
                  </Badge>
                  <Badge :variant="eligibilityBadgeVariant(binding.vipEligibility.status)" size="sm">
                    {{ t('telegramConfig.bindings.vip') }}{{ eligibilityLabel(binding.vipEligibility.status) }}
                  </Badge>
                  <Button
                    variant="ghost"
                    size="sm"
                    class="text-[var(--error)] hover:bg-[var(--error)]/10"
                    :disabled="unlinkingBindingId === binding.id"
                    :loading="unlinkingBindingId === binding.id"
                    @click="unlinkTelegramBinding(binding)"
                  >
                    <template #icon>
                      <Trash2 class="w-3.5 h-3.5 mr-1" />
                    </template>
                    {{ unlinkingBindingId === binding.id ? t('telegramConfig.bindings.unlinking') : t('telegramConfig.bindings.unlink') }}
                  </Button>
                </div>
              </div>
            </div>
          </div>

          <!-- Pagination -->
          <div v-if="telegramBindingsTotal > 0" class="pt-2">
            <Pagination
              :page="telegramBindingsPage"
              :page-size="telegramBindingsPageSize"
              :total="telegramBindingsTotal"
              @update:page="goTelegramBindingsPage"
            >
              <template #total="{ total, page, totalPages }">
                {{ t('telegramConfig.bindings.pagination', { total, page, totalPages }) }}
              </template>
            </Pagination>
          </div>
        </div>
      </Card>

      <!-- 4. Global Notification Channels Card -->
      <Card>
        <template #title>
          <div class="flex items-center gap-2.5">
            <div class="w-8 h-8 rounded-xl bg-[var(--accent)]/10 text-[var(--accent)] flex items-center justify-center shrink-0">
              <Bell class="w-4 h-4" />
            </div>
            <h3 class="font-semibold tracking-tight text-lg text-[var(--text-primary)]">
              {{ t('telegramConfig.channels.title') }}
            </h3>
          </div>
        </template>
        <template #subtitle>
          <p class="text-xs text-[var(--text-secondary)] mt-1">
            {{ t('telegramConfig.channels.description') }}
          </p>
        </template>
        <template #extra>
          <Button size="sm" @click="openCreateChannelForm">
            <template #icon>
              <Plus class="w-3.5 h-3.5 mr-1" />
            </template>
            {{ t('telegramConfig.channels.add') }}
          </Button>
        </template>

        <div class="space-y-4">
          <!-- Loading / Empty -->
          <div v-if="channelLoading" class="py-6 text-center text-sm text-[var(--text-tertiary)]">
            <div class="animate-pulse space-y-3">
              <div v-for="i in 2" :key="i" class="h-16 bg-[var(--bg-surface-soft)] rounded-xl"></div>
            </div>
          </div>
          <EmptyState
            v-else-if="globalChannels.length === 0"
            :title="t('telegramConfig.channels.empty')"
          />
          <div v-else class="space-y-2.5">
            <div
              v-for="ch in globalChannels"
              :key="ch.id"
              class="flex flex-col gap-3 md:flex-row md:items-center md:justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] transition-colors hover:border-[var(--border-hover)]"
            >
              <div class="flex-1 min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-semibold text-sm text-[var(--text-primary)]">{{ ch.name }}</span>
                  <Badge :variant="ch.enabled ? 'success' : 'default'" size="sm">
                    {{ ch.enabled ? t('telegramConfig.channels.enabled') : t('telegramConfig.channels.disabled') }}
                  </Badge>
                  <span class="text-xs text-[var(--text-secondary)]">
                    {{ t('telegramConfig.channels.boundPackages', { count: ch.boundPackages }) }}
                  </span>
                </div>
                <p class="text-xs text-[var(--text-tertiary)] mt-1 font-mono break-all">{{ ch.configPreview }}</p>
              </div>
              <div class="flex items-center gap-2 md:ml-4 shrink-0">
                <Button
                  variant="ghost"
                  size="sm"
                  :disabled="testingChannelId === ch.id"
                  :loading="testingChannelId === ch.id"
                  @click="testChannel(ch.id)"
                >
                  <template #icon>
                    <Send class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ testingChannelId === ch.id ? t('common.sending') : t('telegramConfig.channels.test') }}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  @click="openEditChannelForm(ch)"
                >
                  <template #icon>
                    <Pencil class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ t('common.edit') }}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  class="text-[var(--error)] hover:bg-[var(--error)]/10"
                  :disabled="deletingChannelId === ch.id"
                  :loading="deletingChannelId === ch.id"
                  @click="deleteChannel(ch.id)"
                >
                  <template #icon>
                    <Trash2 class="w-3.5 h-3.5 mr-1" />
                  </template>
                  {{ deletingChannelId === ch.id ? t('common.deleting') : t('common.delete') }}
                </Button>
              </div>
            </div>
          </div>

          <!-- Channel Form Card -->
          <div v-if="showChannelForm" class="p-5 rounded-2xl border border-[var(--accent)]/40 bg-[var(--bg-surface)] shadow-sm space-y-4">
            <div class="flex items-center justify-between border-b border-[var(--border-color)] pb-3">
              <h4 class="font-semibold text-sm text-[var(--text-primary)]">
                {{ editingChannel ? t('telegramConfig.channels.edit') : t('telegramConfig.channels.new') }}
              </h4>
              <button
                type="button"
                class="text-xs text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
                @click="showChannelForm = false"
              >
                {{ t('common.cancel') }}
              </button>
            </div>

            <div v-if="channelFormError" class="p-3 rounded-xl bg-[var(--error)]/10 border border-[var(--error)]/20 text-xs text-[var(--error)] flex items-center gap-2">
              <AlertCircle class="w-4 h-4 shrink-0" />
              <span>{{ channelFormError }}</span>
            </div>

            <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
              <Input
                v-model="channelForm.name"
                :label="t('telegramConfig.channels.name') + ' *'"
                :placeholder="t('telegramConfig.channels.namePlaceholder')"
              />

              <Input
                v-model="channelForm.chatId"
                label="Chat ID *"
                :hint="t('telegramConfig.channels.chatIdHint')"
                :placeholder="t('telegramConfig.channels.chatIdPlaceholder')"
                class="font-mono"
              />

              <div class="md:col-span-2">
                <Input
                  v-model="channelForm.botToken"
                  type="password"
                  :label="'Bot Token ' + (editingChannel ? t('telegramConfig.channels.keepUnchanged') : '*')"
                  :placeholder="t('telegramConfig.channels.tokenPlaceholder')"
                  class="font-mono"
                />
              </div>

              <div class="md:col-span-2 flex items-center justify-between p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
                <div>
                  <label class="text-sm font-semibold text-[var(--text-primary)] select-none cursor-pointer">
                    {{ t('telegramConfig.channels.enable') }}
                  </label>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="channelForm.enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="channelForm.enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="channelForm.enabled = !channelForm.enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="channelForm.enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
              </div>
            </div>

            <div class="flex gap-2 justify-end pt-2">
              <Button variant="secondary" size="sm" @click="showChannelForm = false">
                {{ t('common.cancel') }}
              </Button>
              <Button size="sm" :disabled="savingChannel" :loading="savingChannel" @click="saveChannel">
                {{ savingChannel ? t('common.saving') : t('common.save') }}
              </Button>
            </div>
          </div>

          <!-- Tip Note -->
          <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/80 flex items-start gap-2.5">
            <ExternalLink class="w-4 h-4 text-[var(--accent)] shrink-0 mt-0.5" />
            <p class="text-xs text-[var(--text-secondary)] leading-relaxed">
              {{ t('telegramConfig.channels.needToken') }}
              <a
                href="https://t.me/BotFather"
                target="_blank"
                rel="noopener noreferrer"
                class="font-medium text-[var(--accent)] underline hover:text-[var(--accent-strong)]"
              >
                @BotFather
              </a>
              {{ t('telegramConfig.channels.botFatherHint') }}
            </p>
          </div>
        </div>
      </Card>
    </div>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  .animate-fade-in {
    animation: none;
  }
}
</style>
