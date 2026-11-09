<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import { getSafeRedirectUrl } from '@/utils/validation'
import { focusTurnstileSection, readTurnstileToken } from '@/utils/turnstile'
import api from '@/api'
import { useBrand } from '@/composables/useBrand'
import { buildApiUrl } from '@/utils/api-url'
import { forgotPasswordPath, registerPath } from '@/utils/app-paths'
import { dashboardPath } from '@/utils/app-paths'
import { getDemoLoginAccount } from '@/utils/demo-login'
import { useReveal } from '@/composables/useReveal'
import { Button, Card, Input, Badge } from '@/components/ui'
import { Eye, EyeOff, AlertCircle, Moon, Sun, Monitor, Mail } from 'lucide-vue-next'

const router = useRouter()
const route = useRoute()
const { t } = useI18n()
const authStore = useAuthStore()
const themeStore = useThemeStore()
const brand = useBrand()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const username = ref<string>('')
const password = ref<string>('')
const showPassword = ref<boolean>(false)
const rememberMe = ref<boolean>(false)
const totpCode = ref<string>('')
const recoveryCode = ref<string>('')
const useRecoveryCode = ref<boolean>(false)
const loading = ref<boolean>(false)
const error = ref<string>('')

// Turnstile 验证
const turnstileEnabled = ref<boolean>(false)
const turnstileSiteKey = ref<string>('')
const turnstileToken = ref<string>('')
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileSectionRef = ref<HTMLElement | null>(null)
void turnstileRef.value // 模板中通过 ref 使用
const footerContactEmail = ref<string | null>(null)
const registrationEnabled = ref<boolean>(true)
const isTurnstileChallengeAvailable = computed<boolean>(() => turnstileEnabled.value && Boolean(turnstileSiteKey.value))

const contactEmailHref = computed(() => {
  const email = footerContactEmail.value?.trim()
  if (!email) return null
  return email.startsWith('mailto:') ? email : `mailto:${email}`
})
const demoAccount = computed(() => getDemoLoginAccount('user'))

// OAuth 提供商
const oauthProviders = ref<string[]>([])

onMounted(async (): Promise<void> => {
  // 加载记住的用户名
  try {
    const savedUsername = localStorage.getItem('login_remembered_username')
    if (savedUsername) {
      username.value = savedUsername
      rememberMe.value = true
    }
  } catch {
    // 忽略存储访问异常
  }

  // 加载 Turnstile 配置和 OAuth 提供商
  try {
    const [configResponse, oauthResponse] = await Promise.all([
      api.systemConfig.getPublic(),
      api.oauth.getProviders()
    ])

    registrationEnabled.value = configResponse.registrationEnabled ?? true
    turnstileEnabled.value = configResponse.turnstileEnabled || false
    turnstileSiteKey.value = configResponse.turnstileSiteKey || ''
    footerContactEmail.value = configResponse.footerContactEmail || null
    oauthProviders.value = (oauthResponse as { providers?: string[] }).providers || []
  } catch (e) {
    console.error('Failed to load config:', e)
  }

  if (route.query.adminOnly === '1') {
    error.value = '管理员账号请使用独立管理后台登录'
  }

  // 检查 OAuth 回调错误
  checkOAuthErrors()
})

function onTurnstileExpire() {
  turnstileToken.value = ''
}

function getLoginTurnstileToken(): string | null | undefined {
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

function checkOAuthErrors(): void {
  const urlError = route.query.error as string | undefined
  const provider = route.query.provider as string | undefined

  if (urlError) {
    const errorMessages: Record<string, string> = {
      'not_bound': t('auth.oauthNotBound', { provider: (provider || '').toUpperCase() }),
      'user_not_found': t('auth.oauthUserNotFound'),
      'account_banned': t('auth.oauthAccountBanned'),
      'provider_disabled': t('auth.oauthProviderDisabled'),
      'token_error': t('auth.oauthTokenError'),
      'oauth_error': t('auth.oauthError')
    }
    error.value = errorMessages[urlError] || t('auth.loginFailed') + ': ' + urlError

    // 清除 URL 参数
    router.replace({ query: {} })
  }
}

async function handleLogin(): Promise<void> {
  if (!username.value || !password.value) {
    error.value = t('auth.enterUsernameOrEmailPassword')
    return
  }

  // 检查 Turnstile 验证
  const verificationToken = getLoginTurnstileToken()
  if (verificationToken === null) return

  loading.value = true
  error.value = ''

  try {
    // 始终发送2FA代码（如果有），后端会根据用户是否启用2FA来决定是否验证
    await authStore.login(
      username.value,
      password.value,
      !useRecoveryCode.value && totpCode.value ? totpCode.value : undefined,
      useRecoveryCode.value && recoveryCode.value ? recoveryCode.value : undefined,
      verificationToken || undefined
    )

    if (authStore.isAdmin) {
      await authStore.logout()
      error.value = '管理员账号请使用独立管理后台登录'
      return
    }

    // 记住我持久化
    try {
      if (rememberMe.value) {
        localStorage.setItem('login_remembered_username', username.value)
      } else {
        localStorage.removeItem('login_remembered_username')
      }
    } catch {
      // 忽略存储访问异常
    }

    // 安全改进：验证 redirect 参数防止开放重定向漏洞
    const safeRedirect = getSafeRedirectUrl(route.query.redirect as string, dashboardPath())
    await router.replace(safeRedirect)
  } catch (err: any) {
    error.value = err?.message || String(err)
    // 如果验证码错误，清空验证码让用户重新输入
    if (useRecoveryCode.value) {
      recoveryCode.value = ''
    } else {
      totpCode.value = ''
    }
    // Reset turnstile on any error (token can only be used once)
    if (turnstileRef.value) {
      turnstileRef.value.reset?.()
    }
    turnstileToken.value = ''
  } finally {
    loading.value = false
  }
}

async function loginWithDemoAccount(): Promise<void> {
  const account = demoAccount.value
  if (!account) return
  username.value = account.username
  password.value = account.password
  totpCode.value = ''
  recoveryCode.value = ''
  useRecoveryCode.value = false
  await handleLogin()
}

function loginWithOAuth(provider: string): void {
  // 安全改进：验证 redirect 参数防止开放重定向漏洞
  const safeRedirect = getSafeRedirectUrl(route.query.redirect as string, dashboardPath())
  window.location.href = buildApiUrl(`/oauth/authorize/${provider}?mode=login&redirect=${encodeURIComponent(safeRedirect)}`)
}

interface ProviderInfo {
  name: string
  bgClass: string
  textClass: string
  icon: string
}

function getProviderInfo(provider: string): ProviderInfo {
  const p = provider.toLowerCase()
  const info: Record<string, ProviderInfo> = {
    github: {
      name: 'GitHub',
      bgClass: 'bg-[#24292e] hover:bg-[#2f363d] border-0 text-white',
      textClass: 'text-white',
      icon: `<svg class="w-5 h-5 shrink-0" fill="currentColor" viewBox="0 0 24 24"><path fill-rule="evenodd" d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z" clip-rule="evenodd"></path></svg>`
    },
    google: {
      name: 'Google',
      bgClass: 'bg-[var(--bg-surface)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)]',
      textClass: 'text-[var(--text-primary)]',
      icon: `<svg class="w-5 h-5 shrink-0" viewBox="0 0 24 24"><path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"></path><path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"></path><path fill="#FBBC05" d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"></path><path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"></path></svg>`
    },
    linuxdo: {
      name: 'LINUX DO',
      bgClass: 'bg-[var(--bg-surface)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)]',
      textClass: 'text-[var(--text-primary)]',
      icon: `<svg class="w-5 h-5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><path d="m9 12 2 2 4-4"></path></svg>`
    }
  }
  return info[p] || {
    name: provider.toUpperCase(),
    bgClass: 'bg-[var(--bg-surface)] hover:bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-[var(--text-primary)]',
    textClass: 'text-[var(--text-primary)]',
    icon: `<svg class="w-5 h-5 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M11 16l-4-4m0 0l4-4m-4 4h14m-5 4v1a3 3 0 01-3 3H6a3 3 0 01-3-3V7a3 3 0 013-3h7a3 3 0 013 3v1" /></svg>`
  }
}
</script>

<template>
  <div
    ref="revealRoot"
    class="kawaii-public-shell kawaii-auth-shell kawaii-user-auth min-h-screen flex items-center justify-center py-12 px-4 sm:px-6 bg-[var(--bg-primary)] transition-colors duration-200"
  >
    <div class="relative z-10 w-full max-w-[420px] mx-auto">

      <!-- Logo & 标题区域 (Apple Style Lockup) -->
      <div class="text-center mb-8" data-reveal>
        <div class="w-14 h-14 mx-auto mb-4 flex items-center justify-center rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] transition-transform duration-300 hover:scale-105">
          <img :src="brand.brandLogoUrl" :alt="brand.brandName" class="w-9 h-9 rounded-xl object-contain" />
        </div>
        <h1 class="text-2xl sm:text-3xl font-semibold tracking-tight text-[var(--text-primary)]">
          {{ brand.brandName }}
        </h1>
        <p class="mt-1 text-sm text-[var(--text-secondary)]">
          {{ $t('auth.loginTo') }}
        </p>
      </div>

      <!-- 登录表单卡片 (18px rounded-2xl Card) -->
      <Card padding="md" class="border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)]" data-reveal>
        <form class="space-y-4" @submit.prevent="handleLogin">
          <!-- 用户名 / 邮箱输入框 -->
          <Input
            v-model="username"
            type="text"
            :label="$t('auth.usernameOrEmail')"
            :placeholder="$t('auth.usernameOrEmailPlaceholder')"
            autocomplete="username"
            size="md"
          />

          <!-- 密码输入框 (带显示/隐藏切换) -->
          <Input
            v-model="password"
            :type="showPassword ? 'text' : 'password'"
            :label="$t('auth.password')"
            :placeholder="$t('auth.passwordPlaceholder')"
            autocomplete="current-password"
            size="md"
          >
            <template #suffix>
              <button
                type="button"
                tabindex="-1"
                class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors p-1 rounded-md focus:outline-none"
                :title="showPassword ? '隐藏密码' : '显示密码'"
                :aria-label="showPassword ? '隐藏密码' : '显示密码'"
                @click="showPassword = !showPassword"
              >
                <EyeOff v-if="showPassword" class="w-4 h-4" />
                <Eye v-else class="w-4 h-4" />
              </button>
            </template>
          </Input>

          <!-- 记住我 & 找回密码快捷行 -->
          <div class="flex items-center justify-between text-xs pt-0.5">
            <label class="inline-flex items-center gap-2 cursor-pointer select-none text-[var(--text-secondary)] hover:text-[var(--text-primary)] transition-colors">
              <input
                v-model="rememberMe"
                type="checkbox"
                class="w-4 h-4 rounded border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--accent)] focus:ring-[var(--accent)] focus:ring-offset-0 focus:outline-none transition-colors"
              />
              <span>{{ $t('auth.rememberMe') }}</span>
            </label>
            <RouterLink
              :to="forgotPasswordPath()"
              class="text-[var(--text-secondary)] hover:text-[var(--accent)] hover:underline transition-colors"
            >
              {{ $t('auth.forgotPasswordLink') }}
            </RouterLink>
          </div>

          <!-- 2FA 验证码输入（始终显示，可选） -->
          <div class="space-y-2 pt-1">
            <!-- TOTP 验证码 -->
            <div v-if="!useRecoveryCode" class="space-y-1.5">
              <div class="flex items-center justify-between">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none flex items-center gap-2">
                  <span>{{ $t('auth.twoFactorCode') }}</span>
                  <Badge size="sm" variant="default" shape="pill">{{ $t('auth.twoFactorOptional') }}</Badge>
                </label>
                <button
                  type="button"
                  class="text-xs text-[var(--accent)] hover:underline focus:outline-none transition-colors"
                  @click="useRecoveryCode = true; totpCode = ''; recoveryCode = ''"
                >
                  {{ $t('auth.useRecoveryCode') }}
                </button>
              </div>
              <Input
                v-model="totpCode"
                type="text"
                :maxlength="6"
                :placeholder="$t('auth.twoFactorCodePlaceholder')"
                autocomplete="one-time-code"
                size="md"
                class="font-mono tracking-[0.25em]"
                :hint="$t('auth.twoFactorOptionalHint')"
              />
            </div>

            <!-- 恢复码 -->
            <div v-else class="space-y-1.5">
              <div class="flex items-center justify-between">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ $t('auth.recoveryCode') }}
                </label>
                <button
                  type="button"
                  class="text-xs text-[var(--accent)] hover:underline focus:outline-none transition-colors"
                  @click="useRecoveryCode = false; totpCode = ''; recoveryCode = ''"
                >
                  {{ $t('auth.useTotpCode') }}
                </button>
              </div>
              <Input
                v-model="recoveryCode"
                type="text"
                :placeholder="$t('auth.recoveryCodePlaceholder')"
                size="md"
                class="font-mono"
                :hint="$t('auth.recoveryCodeHint')"
              />
            </div>
          </div>

          <!-- Turnstile 验证 -->
          <div
            v-if="isTurnstileChallengeAvailable"
            ref="turnstileSectionRef"
            tabindex="-1"
            class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 focus:outline-none focus:ring-2 focus:ring-[var(--accent)] transition-all duration-200"
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
            role="alert"
            class="flex items-start gap-2.5 p-3 rounded-xl text-xs leading-relaxed text-[var(--error)] bg-[color-mix(in_srgb,var(--error)_10%,transparent)] border border-[color-mix(in_srgb,var(--error)_24%,transparent)] transition-all duration-200"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1 font-medium">{{ error }}</span>
          </div>

          <!-- 提交登录按钮 (Apple Style 胶囊按钮，min-h-[44px]) -->
          <Button
            type="submit"
            variant="primary"
            size="lg"
            block
            :loading="loading"
            :disabled="loading"
            class="mt-2 min-h-[44px]"
          >
            {{ loading ? $t('auth.loggingIn') : $t('auth.continue') }}
          </Button>

          <!-- 演示账号面板 -->
          <div v-if="demoAccount" class="mt-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-2.5 text-xs">
            <div class="flex items-center justify-between gap-3">
              <span class="font-semibold text-[var(--text-primary)]">{{ demoAccount.label }}</span>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                :disabled="loading"
                @click="loginWithDemoAccount"
              >
                一键登录
              </Button>
            </div>
            <div class="grid grid-cols-[44px_1fr] gap-x-2 gap-y-1 font-mono text-[var(--text-secondary)] break-all text-xs">
              <span class="text-[var(--text-tertiary)]">账号</span>
              <span class="text-[var(--text-primary)]">{{ demoAccount.username }}</span>
              <span class="text-[var(--text-tertiary)]">邮箱</span>
              <span class="text-[var(--text-primary)]">{{ demoAccount.email }}</span>
              <span class="text-[var(--text-tertiary)]">密码</span>
              <span class="text-[var(--text-primary)]">{{ demoAccount.password }}</span>
            </div>
          </div>
        </form>

        <!-- OAuth 快捷登录 -->
        <div v-if="oauthProviders.length > 0" class="mt-6 pt-6 border-t border-[var(--border-color)]">
          <div class="relative text-center mb-4">
            <span class="text-xs font-medium text-[var(--text-tertiary)] uppercase tracking-wider">
              {{ $t('auth.orUse') }}
            </span>
          </div>

          <div class="grid gap-3" :class="oauthProviders.length > 1 ? 'grid-cols-2' : 'grid-cols-1'">
            <button
              v-for="provider in oauthProviders"
              :key="provider"
              type="button"
              :class="[
                'inline-flex items-center justify-center gap-2.5 px-4 py-2.5 min-h-[44px] rounded-full text-sm font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98] shadow-sm focus:outline-none focus:ring-2 focus:ring-[var(--accent)]',
                getProviderInfo(provider).bgClass,
                getProviderInfo(provider).textClass
              ]"
              @click="loginWithOAuth(provider)"
            >
              <span class="shrink-0 flex items-center justify-center" v-html="getProviderInfo(provider).icon"></span>
              <span>{{ getProviderInfo(provider).name }}</span>
            </button>
          </div>

          <p class="mt-3 text-xs text-center text-[var(--text-tertiary)]">
            {{ $t('auth.oauthBindHint') }}
          </p>
        </div>
      </Card>

      <!-- 底部注册导航 -->
      <div class="mt-8 space-y-2 text-center text-sm" data-reveal>
        <p v-if="registrationEnabled" class="text-[var(--text-secondary)]">
          {{ $t('auth.noAccount') }}
          <RouterLink
            :to="registerPath()"
            class="font-medium text-[var(--accent)] hover:text-[var(--accent-strong)] hover:underline transition-colors ml-1"
          >
            {{ $t('auth.register') }}
          </RouterLink>
        </p>
        <p v-else class="text-[var(--text-tertiary)]">
          {{ $t('auth.registrationClosedShort') }}
        </p>
        <p>
          <RouterLink
            :to="forgotPasswordPath()"
            class="text-xs text-[var(--text-secondary)] hover:text-[var(--accent)] hover:underline transition-colors"
          >
            {{ $t('auth.forgotPasswordLink') }}
          </RouterLink>
        </p>
      </div>

      <!-- 右下角悬浮工具操作区 (Apple Style 磨砂胶囊) -->
      <div class="fixed bottom-5 right-5 z-20 flex items-center gap-2.5">
        <a
          v-if="contactEmailHref"
          :href="contactEmailHref"
          class="inline-flex items-center justify-center w-10 h-10 rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)]/80 backdrop-blur-md text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:border-[var(--border-hover)] shadow-sm active:scale-95 transition-all duration-200"
          :title="$t('auth.contactEmail')"
          :aria-label="$t('auth.contactEmail')"
        >
          <Mail class="w-4 h-4" />
        </a>

        <button
          type="button"
          class="inline-flex items-center justify-center w-10 h-10 rounded-full border border-[var(--border-color)] bg-[var(--bg-surface)]/80 backdrop-blur-md text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:border-[var(--border-hover)] shadow-sm active:scale-95 transition-all duration-200 focus:outline-none"
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
  </div>
</template>
