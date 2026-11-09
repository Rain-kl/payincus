<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted, watch } from 'vue'
import { useRouter, useRoute, RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import { validateIdentifier, containsDangerousChars } from '@/utils/validation'
import { translateError } from '@/utils/errorHandler'
import { focusTurnstileSection, readTurnstileToken } from '@/utils/turnstile'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import TermsOfServiceModal from '@/components/TermsOfServiceModal.vue'
import api from '@/api'
import { useBrand } from '@/composables/useBrand'
import { dashboardPath, loginPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import { Badge, Button, Card, Input, Select } from '@/components/ui'
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle2,
  Eye,
  EyeOff,
  KeyRound,
  Loader2,
  Lock,
  Mail,
  Monitor,
  Moon,
  ShieldCheck,
  Sun,
  User,
  X
} from 'lucide-vue-next'

const isAdminEntry = import.meta.env.VITE_APP_ENTRY === 'admin'

const router = useRouter()
const route = useRoute()
const { t } = useI18n()
const authStore = useAuthStore()
const themeStore = useThemeStore()
const brand = useBrand()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

interface RegisterForm {
  username: string
  email: string
  password: string
  confirmPassword: string
  inviteCode: string
  emailCode: string
}

const form = ref<RegisterForm>({
  username: '',
  email: '',
  password: '',
  confirmPassword: '',
  inviteCode: (route.params.code as string) || '',
  emailCode: ''
})

const showPassword = ref<boolean>(false)
const showConfirmPassword = ref<boolean>(false)
const loading = ref<boolean>(false)
const error = ref<string>('')
const success = ref<boolean>(false)
const registrationEnabled = ref<boolean>(true)
const requireInviteCode = ref<boolean>(true)
const configLoading = ref<boolean>(true)

// Email verification
const emailVerificationEnabled = ref<boolean>(false)
const sendingCode = ref<boolean>(false)
const codeSent = ref<boolean>(false)
const codeCountdown = ref<number>(0)
let countdownTimer: ReturnType<typeof setInterval> | null = null

// 组件卸载时清理定时器
onUnmounted(() => {
  if (countdownTimer) {
    clearInterval(countdownTimer)
    countdownTimer = null
  }
})

// Email domain whitelist
const emailDomainWhitelistEnabled = ref<boolean>(false)
const allowedEmailDomains = ref<string[]>([])
const emailUsername = ref<string>('')
const selectedEmailDomain = ref<string>('')

// 当白名单模式时，自动组合邮箱地址
watch([emailUsername, selectedEmailDomain], () => {
  if (emailDomainWhitelistEnabled.value && allowedEmailDomains.value.length > 0) {
    if (emailUsername.value && selectedEmailDomain.value) {
      form.value.email = `${emailUsername.value}@${selectedEmailDomain.value}`
    } else {
      form.value.email = ''
    }
  }
})

// Turnstile
const turnstileEnabled = ref<boolean>(false)
const turnstileSiteKey = ref<string>('')
const turnstileToken = ref<string>('')
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileSectionRef = ref<HTMLElement | null>(null)
void turnstileRef.value // 模板中通过 ref 使用
const isTurnstileChallengeAvailable = computed<boolean>(() => turnstileEnabled.value && Boolean(turnstileSiteKey.value))

// Terms of Service
const agreedToTerms = ref<boolean>(false)
const showTermsModal = ref<boolean>(false)

// Email confirmation modal
const showEmailConfirmModal = ref<boolean>(false)

function onTurnstileExpire() {
  turnstileToken.value = ''
}

function resetTurnstileChallenge(): void {
  if (!isTurnstileChallengeAvailable.value) return
  turnstileToken.value = ''
  turnstileRef.value?.reset?.()
}

function getRegisterTurnstileToken(): string | null | undefined {
  if (!isTurnstileChallengeAvailable.value) return undefined

  const token = readTurnstileToken(turnstileRef.value, turnstileToken.value)
  if (token) {
    turnstileToken.value = token
    return token
  }

  focusTurnstileSection(turnstileSectionRef.value)
  error.value = t('auth.turnstileRequired')
  return null
}

// Show email confirmation before sending code
function handleSendCodeClick(): void {
  if (!registrationEnabled.value) {
    error.value = t('auth.registrationClosedMessage')
    return
  }

  if (!form.value.email) {
    error.value = t('auth.invalidEmail')
    return
  }

  // Validate email format
  const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
  if (!emailRegex.test(form.value.email)) {
    error.value = t('auth.invalidEmail')
    return
  }

  // Check Turnstile if enabled
  if (getRegisterTurnstileToken() === null) return

  // Show confirmation modal
  showEmailConfirmModal.value = true
}

// Confirm and send verification code
function confirmSendCode(): void {
  showEmailConfirmModal.value = false
  sendVerificationCode()
}

// 加载系统配置
onMounted(async () => {
  try {
    const config = await api.systemConfig.getPublic()
    registrationEnabled.value = config.registrationEnabled ?? true
    requireInviteCode.value = config.requireInviteCode
    turnstileEnabled.value = config.turnstileEnabled || false
    turnstileSiteKey.value = config.turnstileSiteKey || ''
    emailVerificationEnabled.value = config.emailVerificationEnabled || false
    emailDomainWhitelistEnabled.value = config.emailDomainWhitelistEnabled || false
    allowedEmailDomains.value = config.allowedEmailDomains || []
    // 设置默认选中的域名
    if (allowedEmailDomains.value.length > 0) {
      selectedEmailDomain.value = allowedEmailDomains.value[0]
    }
  } catch {
    // 默认需要邀请码
    requireInviteCode.value = true
  } finally {
    configLoading.value = false
  }
})

// Send verification code (called after confirmation)
async function sendVerificationCode(): Promise<void> {
  if (!registrationEnabled.value) {
    error.value = t('auth.registrationClosedMessage')
    return
  }

  const verificationToken = getRegisterTurnstileToken()
  if (verificationToken === null) return

  sendingCode.value = true
  error.value = ''

  try {
    await api.auth.sendVerificationCode(
      form.value.email,
      verificationToken || undefined
    )
    codeSent.value = true
    // Start countdown
    codeCountdown.value = 60
    countdownTimer = setInterval(() => {
      codeCountdown.value--
      if (codeCountdown.value <= 0) {
        if (countdownTimer) {
          clearInterval(countdownTimer)
          countdownTimer = null
        }
      }
    }, 1000)
    // Token can only be used once. Keep the form and refresh only the challenge.
    resetTurnstileChallenge()
  } catch (err: any) {
    error.value = translateError(err)
    resetTurnstileChallenge()
  } finally {
    sendingCode.value = false
  }
}

async function handleRegister(): Promise<void> {
  if (!registrationEnabled.value) {
    error.value = t('auth.registrationClosedMessage')
    return
  }

  if (!form.value.username || !form.value.password || !form.value.email) {
    error.value = t('auth.fillAllRequired')
    return
  }

  // 如果需要邀请码但未填写
  if (requireInviteCode.value && !form.value.inviteCode) {
    error.value = t('auth.fillAllRequired')
    return
  }

  // 如果需要邮件验证码但未填写
  if (emailVerificationEnabled.value && !form.value.emailCode) {
    error.value = t('auth.emailCodeRequired')
    return
  }

  // 验证用户名（防止危险字符）
  const fieldName = t('auth.username')
  const usernameValidation = validateIdentifier(form.value.username, fieldName, 3, 32)
  if (!usernameValidation.valid) {
    error.value = usernameValidation.message || t('auth.fillAllRequired')
    return
  }

  // 验证邮箱格式
  const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
  if (!emailRegex.test(form.value.email)) {
    error.value = t('auth.invalidEmail')
    return
  }

  // 检查邮箱是否包含危险字符
  const emailWithoutAllowed = form.value.email.replace(/@/g, '').replace(/\./g, '')
  if (containsDangerousChars(emailWithoutAllowed)) {
    error.value = t('auth.emailContainsIllegal')
    return
  }

  if (form.value.password !== form.value.confirmPassword) {
    error.value = t('auth.passwordMismatch')
    return
  }

  if (form.value.password.length < 8) {
    error.value = t('auth.passwordTooShort')
    return
  }

  // 密码复杂度检查
  if (!/[A-Z]/.test(form.value.password)) {
    error.value = t('auth.passwordNeedsUppercase')
    return
  }
  if (!/[a-z]/.test(form.value.password)) {
    error.value = t('auth.passwordNeedsLowercase')
    return
  }
  if (!/[0-9]/.test(form.value.password)) {
    error.value = t('auth.passwordNeedsNumber')
    return
  }

  // 检查 Turnstile 验证
  const verificationToken = getRegisterTurnstileToken()
  if (verificationToken === null) return

  // 检查是否同意服务条款
  if (!agreedToTerms.value) {
    error.value = t('auth.tos.mustAgree')
    return
  }

  loading.value = true
  error.value = ''

  try {
    const response = await api.auth.register({
      username: form.value.username,
      email: form.value.email,
      password: form.value.password,
      inviteCode: form.value.inviteCode,
      turnstileToken: verificationToken || undefined,
      emailCode: emailVerificationEnabled.value ? form.value.emailCode : undefined
    })
    localStorage.setItem('token', response.token)
    authStore.syncToken()
    await authStore.fetchCurrentUser()
    // Clean up countdown timer
    if (countdownTimer) {
      clearInterval(countdownTimer)
      countdownTimer = null
    }
    if (isAdminEntry || authStore.isAdmin) {
      await authStore.logout()
      success.value = false
      error.value = '该入口不支持管理员注册'
      return
    }

    success.value = true
    await router.replace(dashboardPath())
  } catch (err: any) {
    error.value = translateError(err)
    resetTurnstileChallenge()
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div ref="revealRoot" class="nimbus-auth kawaii-public-shell kawaii-auth-shell kawaii-user-auth min-h-screen flex items-center justify-center p-4 sm:p-6 md:p-8">
    <div class="nimbus-aurora" aria-hidden="true"></div>
    <div class="relative z-10 w-full max-w-lg">

      <!-- Logo lockup -->
      <div class="text-center mb-7" data-reveal>
        <div class="mx-auto mb-3 flex h-14 w-14 items-center justify-center rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)]">
          <img :src="brand.brandLogoUrl" :alt="brand.brandName" class="h-8 w-8 rounded-xl object-contain" />
        </div>
        <h1 class="text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
          {{ brand.brandName }}
        </h1>
        <p class="mt-1 text-sm text-[var(--text-secondary)]">
          {{ $t('auth.createAccount') }}
        </p>
      </div>

      <!-- 注册成功状态 -->
      <Card v-if="success" padding="lg" class="text-center shadow-sm" data-reveal>
        <div class="flex flex-col items-center justify-center py-6 gap-3">
          <div class="flex h-14 w-14 items-center justify-center rounded-2xl bg-[color-mix(in_srgb,var(--success)_14%,var(--bg-surface))] text-[var(--success)] shadow-sm">
            <CheckCircle2 class="h-8 w-8" />
          </div>
          <h3 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">
            {{ $t('auth.registerSuccess') }}
          </h3>
        </div>
      </Card>

      <!-- 配置加载中状态 -->
      <Card v-else-if="configLoading" padding="lg" class="text-center shadow-sm" data-reveal>
        <div class="flex flex-col items-center justify-center py-8 gap-3">
          <Loader2 class="h-8 w-8 animate-spin text-[var(--accent)]" />
          <p class="text-sm text-[var(--text-secondary)]">{{ $t('common.loading') }}...</p>
        </div>
      </Card>

      <!-- 注册已关闭状态 -->
      <Card v-else-if="!registrationEnabled" padding="lg" class="text-center shadow-sm" data-reveal>
        <div class="flex flex-col items-center justify-center py-4 gap-4">
          <div class="flex h-14 w-14 items-center justify-center rounded-2xl bg-[color-mix(in_srgb,var(--warning)_14%,var(--bg-surface))] text-[var(--warning)] shadow-sm">
            <AlertTriangle class="h-8 w-8" />
          </div>
          <div class="space-y-1.5">
            <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
              {{ $t('auth.registrationClosedTitle') }}
            </h3>
            <p class="text-sm leading-relaxed text-[var(--text-secondary)] max-w-sm">
              {{ $t('auth.registrationClosedMessage') }}
            </p>
          </div>
          <Button
            type="button"
            variant="primary"
            block
            class="mt-2"
            @click="router.push(loginPath())"
          >
            {{ $t('auth.backToLogin') }}
          </Button>
        </div>
      </Card>

      <!-- 注册表单 -->
      <Card v-else padding="md" class="shadow-sm" data-reveal>
        <form class="space-y-4" @submit.prevent="handleRegister">
          <!-- 邀请码 -->
          <div v-if="requireInviteCode">
            <Input
              v-model="form.inviteCode"
              type="text"
              class="font-mono"
              :placeholder="$t('auth.inviteCodePlaceholder')"
              autocomplete="off"
            >
              <template #label>
                <span class="flex items-center gap-1.5">
                  {{ $t('auth.inviteCode') }}
                  <span class="text-[var(--error)]">*</span>
                </span>
              </template>
              <template #prefix>
                <KeyRound class="w-4 h-4 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>

          <!-- 用户名 -->
          <Input
            v-model="form.username"
            type="text"
            :placeholder="$t('auth.usernameHint')"
            autocomplete="username"
          >
            <template #label>
              <span class="flex items-center gap-1.5">
                {{ $t('auth.username') }}
                <span class="text-[var(--error)]">*</span>
              </span>
            </template>
            <template #prefix>
              <User class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>

          <!-- 邮箱 -->
          <div v-if="emailDomainWhitelistEnabled && allowedEmailDomains.length > 0" class="flex flex-col gap-1.5">
            <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center gap-1.5">
              {{ $t('auth.email') }}
              <span class="text-[var(--error)]">*</span>
            </label>
            <div class="flex items-center gap-2">
              <Input
                v-model="emailUsername"
                type="text"
                class="min-w-0 flex-1"
                :placeholder="$t('auth.emailUsernamePlaceholder')"
                autocomplete="username"
              >
                <template #prefix>
                  <Mail class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
              <span class="shrink-0 text-sm font-semibold text-[var(--text-tertiary)]">@</span>
              <Select
                v-model="selectedEmailDomain"
                :options="allowedEmailDomains"
                :block="false"
                class="w-36 sm:w-48 shrink-0"
              />
            </div>
          </div>
          <Input
            v-else
            v-model="form.email"
            type="email"
            placeholder="your@email.com"
            autocomplete="email"
          >
            <template #label>
              <span class="flex items-center gap-1.5">
                {{ $t('auth.email') }}
                <span class="text-[var(--error)]">*</span>
              </span>
            </template>
            <template #prefix>
              <Mail class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>

          <!-- 邮箱验证码 -->
          <div v-if="emailVerificationEnabled" class="flex flex-col gap-1.5">
            <div class="flex items-center justify-between">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center gap-1.5">
                {{ $t('auth.emailCode') }}
                <span class="text-[var(--error)]">*</span>
              </label>
              <Badge v-if="codeSent" variant="success" size="sm" dot>
                {{ $t('auth.codeSentHint') }}
              </Badge>
            </div>
            <div class="flex flex-col sm:flex-row gap-2 sm:items-center">
              <Input
                v-model="form.emailCode"
                type="text"
                class="min-w-0 flex-1 font-mono tracking-[0.25em]"
                :maxlength="6"
                :placeholder="$t('auth.emailCodePlaceholder')"
                autocomplete="one-time-code"
              >
                <template #prefix>
                  <ShieldCheck class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
              <Button
                type="button"
                variant="secondary"
                :disabled="sendingCode || codeCountdown > 0 || !form.email"
                :loading="sendingCode"
                class="whitespace-nowrap sm:w-auto shrink-0"
                :title="!form.email ? $t('auth.enterEmailFirst') : ''"
                @click="handleSendCodeClick"
              >
                <template v-if="codeCountdown > 0">
                  {{ codeCountdown }}s
                </template>
                <template v-else>
                  {{ $t('auth.sendCode') }}
                </template>
              </Button>
            </div>
          </div>

          <!-- 密码 -->
          <Input
            v-model="form.password"
            :type="showPassword ? 'text' : 'password'"
            :placeholder="$t('auth.passwordHint')"
            autocomplete="new-password"
          >
            <template #label>
              <span class="flex items-center gap-1.5">
                {{ $t('auth.password') }}
                <span class="text-[var(--error)]">*</span>
              </span>
            </template>
            <template #prefix>
              <Lock class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
            <template #suffix>
              <button
                type="button"
                tabindex="-1"
                class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors focus:outline-none"
                :aria-label="showPassword ? 'Hide password' : 'Show password'"
                @click="showPassword = !showPassword"
              >
                <EyeOff v-if="showPassword" class="w-4 h-4" />
                <Eye v-else class="w-4 h-4" />
              </button>
            </template>
          </Input>

          <!-- 确认密码 -->
          <Input
            v-model="form.confirmPassword"
            :type="showConfirmPassword ? 'text' : 'password'"
            :placeholder="$t('auth.confirmPasswordPlaceholder')"
            autocomplete="new-password"
          >
            <template #label>
              <span class="flex items-center gap-1.5">
                {{ $t('auth.confirmPassword') }}
                <span class="text-[var(--error)]">*</span>
              </span>
            </template>
            <template #prefix>
              <Lock class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
            <template #suffix>
              <button
                type="button"
                tabindex="-1"
                class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors focus:outline-none"
                :aria-label="showConfirmPassword ? 'Hide password' : 'Show password'"
                @click="showConfirmPassword = !showConfirmPassword"
              >
                <EyeOff v-if="showConfirmPassword" class="w-4 h-4" />
                <Eye v-else class="w-4 h-4" />
              </button>
            </template>
          </Input>

          <!-- Turnstile 验证 -->
          <div
            v-if="isTurnstileChallengeAvailable"
            ref="turnstileSectionRef"
            tabindex="-1"
            class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          >
            <TurnstileWidget
              ref="turnstileRef"
              v-model="turnstileToken"
              :site-key="turnstileSiteKey"
              :theme="themeStore.isDark ? 'dark' : 'light'"
              @expire="onTurnstileExpire"
            />
          </div>

          <!-- 服务条款 -->
          <div class="flex items-start gap-2.5 pt-0.5">
            <input
              id="agree-terms"
              v-model="agreedToTerms"
              type="checkbox"
              class="mt-1 h-4 w-4 rounded-md border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--accent)] accent-[var(--accent)] focus:ring-2 focus:ring-[var(--accent)] focus:outline-none cursor-pointer transition-colors"
            />
            <label for="agree-terms" class="text-xs text-[var(--text-secondary)] leading-relaxed select-none cursor-pointer">
              {{ $t('auth.tos.agreePrefix') }}
              <button
                type="button"
                class="font-medium text-[var(--accent)] hover:underline focus:outline-none transition-colors ml-0.5"
                @click="showTermsModal = true"
              >
                {{ $t('auth.tos.termsLink') }}
              </button>
            </label>
          </div>

          <!-- 错误提示 -->
          <div
            v-if="error"
            class="flex items-start gap-2.5 rounded-xl border border-[color-mix(in_srgb,var(--error)_28%,transparent)] bg-[color-mix(in_srgb,var(--error)_10%,transparent)] p-3 text-xs leading-relaxed text-[var(--error)]"
            role="alert"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1">{{ error }}</span>
          </div>

          <!-- 提交按钮 -->
          <Button
            type="submit"
            variant="primary"
            size="lg"
            block
            :loading="loading"
            :disabled="loading || (emailVerificationEnabled && !form.emailCode) || !agreedToTerms"
            class="mt-2"
          >
            {{ loading ? $t('auth.creatingAccount') : $t('auth.createAccount') }}
          </Button>
        </form>
      </Card>

      <!-- 服务条款弹窗 -->
      <TermsOfServiceModal
        :show="showTermsModal"
        @close="showTermsModal = false"
      />

      <!-- 邮箱确认弹窗 -->
      <Teleport to="body">
        <Transition name="modal">
          <div
            v-if="showEmailConfirmModal"
            class="fixed inset-0 z-50 flex items-center justify-center p-4"
          >
            <!-- 背景遮罩 -->
            <div
              class="fixed inset-0 bg-[color-mix(in_srgb,var(--text-primary)_35%,transparent)] backdrop-blur-sm"
              @click="showEmailConfirmModal = false"
            />

            <!-- 弹窗卡片 -->
            <div
              class="modal-card relative z-10 w-full max-w-sm rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-xl transition-all"
              role="dialog"
              aria-modal="true"
            >
              <div class="flex items-center justify-between pb-3 border-b border-[var(--border-color)]">
                <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ $t('auth.confirmEmail') }}
                </h3>
                <button
                  type="button"
                  class="rounded-full p-1 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors focus:outline-none"
                  @click="showEmailConfirmModal = false"
                >
                  <X class="w-4 h-4" />
                </button>
              </div>

              <div class="py-5 text-center space-y-3">
                <p class="text-xs text-[var(--text-secondary)]">
                  {{ $t('auth.confirmEmailMessage') }}
                </p>
                <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-3 text-sm font-mono break-all">
                  <span class="text-[var(--text-primary)] font-medium">{{ emailUsername || form.email.split('@')[0] }}</span>
                  <span class="text-[var(--error)] font-bold">@{{ selectedEmailDomain || form.email.split('@')[1] }}</span>
                </div>
              </div>

              <div class="flex items-center justify-end gap-2.5 pt-2">
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  @click="showEmailConfirmModal = false"
                >
                  {{ $t('common.cancel') }}
                </Button>
                <Button
                  type="button"
                  variant="primary"
                  size="sm"
                  @click="confirmSendCode"
                >
                  {{ $t('auth.confirmAndSend') }}
                </Button>
              </div>
            </div>
          </div>
        </Transition>
      </Teleport>

      <!-- 底部登录链接 -->
      <p class="mt-6 text-center text-sm text-[var(--text-secondary)]" data-reveal>
        {{ $t('auth.hasAccount') }}
        <RouterLink :to="loginPath()" class="font-medium text-[var(--accent)] hover:underline ml-1 transition-colors focus:outline-none">
          {{ $t('auth.login') }}
        </RouterLink>
      </p>

      <!-- 右下角主题切换悬浮按钮 -->
      <button
        type="button"
        class="fixed bottom-4 right-4 z-20 flex h-10 w-10 items-center justify-center rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] shadow-sm hover:text-[var(--text-primary)] hover:border-[var(--border-hover)] active:scale-[0.98] transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
        :title="themeStore.mode === 'dark' ? $t('theme.dark') : themeStore.mode === 'light' ? $t('theme.light') : $t('theme.system')"
        :aria-label="themeStore.mode === 'dark' ? $t('theme.dark') : themeStore.mode === 'light' ? $t('theme.light') : $t('theme.system')"
        @click="themeStore.toggleTheme"
      >
        <Moon v-if="themeStore.mode === 'dark'" class="w-4 h-4" />
        <Sun v-else-if="themeStore.mode === 'light'" class="w-4 h-4" />
        <Monitor v-else class="w-4 h-4" />
      </button>
    </div>
  </div>
</template>

<style scoped>
/* ============ Nimbus 认证画布 ============ */
.nimbus-auth {
  position: relative;
  overflow: hidden;
}

.nimbus-aurora {
  position: fixed;
  inset: 0;
  z-index: 0;
  pointer-events: none;
  overflow: hidden;
}

.nimbus-aurora::before {
  content: '';
  position: absolute;
  top: -22%;
  left: 50%;
  width: min(760px, 128vw);
  height: min(760px, 128vw);
  transform: translateX(-50%);
  background: radial-gradient(circle, color-mix(in srgb, var(--accent) 18%, transparent), transparent 62%);
  opacity: 0.55;
}

.nimbus-aurora::after {
  content: '';
  position: absolute;
  inset: 0;
  background-image: radial-gradient(color-mix(in srgb, var(--text-primary) 8%, transparent) 1px, transparent 1px);
  background-size: 26px 26px;
  -webkit-mask-image: radial-gradient(ellipse 78% 52% at 50% 0%, black 0%, transparent 70%);
  mask-image: radial-gradient(ellipse 78% 52% at 50% 0%, black 0%, transparent 70%);
  opacity: 0.4;
}

/* ============ 模态弹窗动画 ============ */
.modal-enter-active,
.modal-leave-active {
  transition: opacity 0.22s cubic-bezier(0.25, 0.1, 0.25, 1);
}

.modal-enter-from,
.modal-leave-to {
  opacity: 0;
}

.modal-enter-active .modal-card,
.modal-leave-active .modal-card {
  transition: transform 0.22s cubic-bezier(0.25, 0.1, 0.25, 1);
}

.modal-enter-from .modal-card,
.modal-leave-to .modal-card {
  transform: scale(0.96);
}
</style>
