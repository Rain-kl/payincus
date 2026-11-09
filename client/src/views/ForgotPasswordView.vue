<script setup lang="ts">
import { computed, ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Mail, KeyRound, Lock, ArrowLeft, Check, AlertCircle, CheckCircle2 } from 'lucide-vue-next'
import { Button, Card, Input, Badge } from '@/components/ui'
import { useThemeStore } from '@/stores/theme'
import { useToast } from '@/stores/toast'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import { focusTurnstileSection, readTurnstileToken } from '@/utils/turnstile'
import { translateError } from '@/utils/errorHandler'
import api from '@/api'
import { useBrand } from '@/composables/useBrand'
import { loginPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'

defineOptions({ name: 'ForgotPasswordView' })

const router = useRouter()
const { t } = useI18n()
const themeStore = useThemeStore()
const toast = useToast()
const brand = useBrand()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const email = ref<string>('')
const code = ref<string>('')
const password = ref<string>('')
const confirmPassword = ref<string>('')
const loading = ref<boolean>(false)
const sendingCode = ref<boolean>(false)
const error = ref<string>('')
const success = ref<string>('')
const step = ref<'email' | 'verify'>('email')

// Turnstile 验证
const turnstileEnabled = ref<boolean>(false)
const turnstileSiteKey = ref<string>('')
const turnstileToken = ref<string>('')
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileSectionRef = ref<HTMLElement | null>(null)
const isTurnstileChallengeAvailable = computed<boolean>(() => turnstileEnabled.value && Boolean(turnstileSiteKey.value))

onMounted(async (): Promise<void> => {
  try {
    const configResponse = await api.systemConfig.getPublic()
    turnstileEnabled.value = configResponse.turnstileEnabled || false
    turnstileSiteKey.value = configResponse.turnstileSiteKey || ''
  } catch (e) {
    console.error('Failed to load config:', e)
  }
})

function onTurnstileExpire() {
  turnstileToken.value = ''
}

function getForgotPasswordTurnstileToken(): string | null | undefined {
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

async function sendVerificationCode(): Promise<void> {
  if (!email.value || !email.value.includes('@')) {
    error.value = t('auth.invalidEmail')
    return
  }

  const verificationToken = getForgotPasswordTurnstileToken()
  if (verificationToken === null) return

  sendingCode.value = true
  error.value = ''
  success.value = ''

  try {
    await api.auth.sendForgotPasswordCode(email.value, verificationToken || undefined)
    success.value = t('auth.forgotPassword.codeSent')
    step.value = 'verify'
    // Reset turnstile
    if (turnstileRef.value) {
      turnstileRef.value.reset?.()
    }
    turnstileToken.value = ''
  } catch (err: any) {
    error.value = translateError(err)
    // Reset turnstile on error
    if (turnstileRef.value) {
      turnstileRef.value.reset?.()
    }
    turnstileToken.value = ''
  } finally {
    sendingCode.value = false
  }
}

async function resetPassword(): Promise<void> {
  if (!code.value || code.value.length !== 6) {
    error.value = t('auth.invalidCode')
    return
  }

  if (password.value !== confirmPassword.value) {
    error.value = t('auth.passwordMismatch')
    return
  }

  if (password.value.length < 8) {
    error.value = t('auth.passwordTooShort')
    return
  }

  if (!/[A-Z]/.test(password.value)) {
    error.value = t('auth.passwordNeedsUppercase')
    return
  }

  if (!/[a-z]/.test(password.value)) {
    error.value = t('auth.passwordNeedsLowercase')
    return
  }

  if (!/[0-9]/.test(password.value)) {
    error.value = t('auth.passwordNeedsNumber')
    return
  }

  const verificationToken = getForgotPasswordTurnstileToken()
  if (verificationToken === null) return

  loading.value = true
  error.value = ''
  success.value = ''

  try {
    const response = await api.auth.resetPassword(
      email.value,
      code.value,
      password.value,
      verificationToken || undefined
    )

    toast.success(t('auth.forgotPassword.resetSuccess'))

    // 如果取消了2FA，显示提示
    if (response.twoFactorDisabled) {
      toast.info(t('auth.forgotPassword.twoFactorDisabled'))
    }

    // 跳转到登录页面
    setTimeout(() => {
      router.push(loginPath())
    }, 2000)
  } catch (err: any) {
    error.value = translateError(err)
    // Reset turnstile on error
    if (turnstileRef.value) {
      turnstileRef.value.reset?.()
    }
    turnstileToken.value = ''
  } finally {
    loading.value = false
  }
}

function backToEmail() {
  step.value = 'email'
  code.value = ''
  password.value = ''
  confirmPassword.value = ''
  error.value = ''
  success.value = ''
  if (turnstileRef.value) {
    turnstileRef.value.reset?.()
  }
  turnstileToken.value = ''
}
</script>

<template>
  <div
    ref="revealRoot"
    class="min-h-screen flex items-center justify-center p-4 sm:p-6 lg:p-8 bg-[var(--bg-primary)] transition-colors duration-200"
  >
    <div class="relative z-10 w-full max-w-md mx-auto">
      <!-- Logo lockup -->
      <div class="text-center mb-8" data-reveal>
        <div class="w-14 h-14 mx-auto mb-4 flex items-center justify-center rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] hover:scale-105">
          <img :src="brand.brandLogoUrl" :alt="brand.brandName" class="w-9 h-9 object-contain rounded-xl" />
        </div>
        <h1 class="text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
          {{ $t('auth.forgotPassword.title') }}
        </h1>
        <p class="text-sm text-[var(--text-secondary)] mt-1.5 max-w-xs mx-auto">
          {{ $t('auth.forgotPassword.subtitle') }}
        </p>
      </div>

      <!-- 步骤指示 -->
      <div class="flex items-center justify-center gap-2.5 mb-6 select-none" data-reveal>
        <Badge
          :variant="step === 'email' ? 'info' : 'success'"
          size="md"
          shape="pill"
          :class="{ 'cursor-pointer hover:opacity-80 active:scale-95 transition-all': step === 'verify' }"
          @click="step === 'verify' ? backToEmail() : undefined"
        >
          <Check v-if="step === 'verify'" class="w-3 h-3 mr-1 shrink-0 stroke-[2.5]" />
          <span>{{ step === 'verify' ? $t('auth.email') : `1. ${$t('auth.email')}` }}</span>
        </Badge>
        <div
          class="w-6 h-0.5 rounded-full transition-colors duration-300"
          :class="step === 'verify' ? 'bg-[var(--accent)]' : 'bg-[var(--border-color)]'"
        />
        <Badge
          :variant="step === 'verify' ? 'info' : 'default'"
          size="md"
          shape="pill"
        >
          <span>2. {{ $t('auth.verificationCode') }}</span>
        </Badge>
      </div>

      <!-- 表单卡片 -->
      <Card padding="md" data-reveal>
        <!-- 步骤1：输入邮箱 -->
        <form v-if="step === 'email'" class="space-y-5" @submit.prevent="sendVerificationCode">
          <Input
            v-model="email"
            type="email"
            :label="$t('auth.email')"
            :placeholder="$t('auth.emailPlaceholder')"
            autocomplete="email"
            size="md"
          >
            <template #prefix>
              <Mail class="w-4 h-4" />
            </template>
          </Input>

          <!-- Turnstile 验证 -->
          <div
            v-if="isTurnstileChallengeAvailable"
            ref="turnstileSectionRef"
            tabindex="-1"
            class="flex justify-center rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 transition-colors focus:outline-none focus:ring-2 focus:ring-[var(--accent)]"
          >
            <TurnstileWidget
              ref="turnstileRef"
              v-model="turnstileToken"
              :site-key="turnstileSiteKey"
              :theme="themeStore.isDark ? 'dark' : 'light'"
              @expire="onTurnstileExpire"
            />
          </div>

          <!-- 错误提示 -->
          <div
            v-if="error"
            class="flex items-start gap-2.5 rounded-xl border border-[var(--error)]/30 bg-[var(--error)]/10 px-3.5 py-2.5 text-xs text-[var(--error)] font-medium leading-relaxed transition-all"
            role="alert"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1">{{ error }}</span>
          </div>

          <!-- 成功提示 -->
          <div
            v-if="success"
            class="flex items-start gap-2.5 rounded-xl border border-[var(--success)]/30 bg-[var(--success)]/10 px-3.5 py-2.5 text-xs text-[var(--success)] font-medium leading-relaxed transition-all"
            role="status"
          >
            <CheckCircle2 class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1">{{ success }}</span>
          </div>

          <div class="space-y-2.5 pt-1">
            <Button
              type="submit"
              variant="primary"
              size="md"
              block
              :loading="sendingCode"
            >
              {{ sendingCode ? $t('common.sending') : $t('auth.forgotPassword.sendCode') }}
            </Button>

            <Button
              type="button"
              variant="ghost"
              size="md"
              block
              @click="router.push(loginPath())"
            >
              <template #prefix>
                <ArrowLeft class="w-4 h-4 mr-1 shrink-0" />
              </template>
              {{ $t('common.back') }}
            </Button>
          </div>
        </form>

        <!-- 步骤2：验证码与新密码 -->
        <form v-else class="space-y-5" @submit.prevent="resetPassword">
          <Input
            :model-value="email"
            type="email"
            :label="$t('auth.email')"
            disabled
            size="md"
          >
            <template #prefix>
              <Mail class="w-4 h-4" />
            </template>
          </Input>

          <Input
            v-model="code"
            type="text"
            :maxlength="6"
            class="font-mono tracking-[0.25em]"
            :label="$t('auth.verificationCode')"
            :placeholder="$t('auth.verificationCodePlaceholder')"
            :hint="$t('auth.forgotPassword.codeHint')"
            autocomplete="one-time-code"
            size="md"
          >
            <template #prefix>
              <KeyRound class="w-4 h-4 tracking-normal" />
            </template>
          </Input>

          <Input
            v-model="password"
            type="password"
            :label="$t('auth.newPassword')"
            :placeholder="$t('auth.passwordHint')"
            autocomplete="new-password"
            size="md"
          >
            <template #prefix>
              <Lock class="w-4 h-4" />
            </template>
          </Input>

          <Input
            v-model="confirmPassword"
            type="password"
            :label="$t('auth.confirmPassword')"
            :placeholder="$t('auth.confirmPasswordPlaceholder')"
            autocomplete="new-password"
            size="md"
          >
            <template #prefix>
              <Lock class="w-4 h-4" />
            </template>
          </Input>

          <!-- Turnstile 验证 -->
          <div
            v-if="isTurnstileChallengeAvailable"
            ref="turnstileSectionRef"
            tabindex="-1"
            class="flex justify-center rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 transition-colors focus:outline-none focus:ring-2 focus:ring-[var(--accent)]"
          >
            <TurnstileWidget
              ref="turnstileRef"
              v-model="turnstileToken"
              :site-key="turnstileSiteKey"
              :theme="themeStore.isDark ? 'dark' : 'light'"
              @expire="onTurnstileExpire"
            />
          </div>

          <!-- 错误提示 -->
          <div
            v-if="error"
            class="flex items-start gap-2.5 rounded-xl border border-[var(--error)]/30 bg-[var(--error)]/10 px-3.5 py-2.5 text-xs text-[var(--error)] font-medium leading-relaxed transition-all"
            role="alert"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1">{{ error }}</span>
          </div>

          <!-- 成功提示 -->
          <div
            v-if="success"
            class="flex items-start gap-2.5 rounded-xl border border-[var(--success)]/30 bg-[var(--success)]/10 px-3.5 py-2.5 text-xs text-[var(--success)] font-medium leading-relaxed transition-all"
            role="status"
          >
            <CheckCircle2 class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1">{{ success }}</span>
          </div>

          <div class="space-y-2.5 pt-1">
            <Button
              type="submit"
              variant="primary"
              size="md"
              block
              :loading="loading"
            >
              {{ loading ? $t('common.processing') : $t('auth.forgotPassword.resetPassword') }}
            </Button>

            <Button
              type="button"
              variant="ghost"
              size="md"
              block
              @click="backToEmail"
            >
              <template #prefix>
                <ArrowLeft class="w-4 h-4 mr-1 shrink-0" />
              </template>
              {{ $t('common.back') }}
            </Button>
          </div>
        </form>
      </Card>

      <p class="mt-8 text-center text-sm text-[var(--text-secondary)]" data-reveal>
        {{ $t('auth.rememberPassword') }}
        <RouterLink
          :to="loginPath()"
          class="font-medium text-[var(--accent)] hover:underline ml-1 transition-colors duration-200"
        >
          {{ $t('auth.login') }}
        </RouterLink>
      </p>
    </div>
  </div>
</template>
