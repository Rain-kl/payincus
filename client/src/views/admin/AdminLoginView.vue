<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { AlertCircle, ShieldCheck, Moon, Sun, Monitor, Mail } from 'lucide-vue-next'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import { getSafeRedirectUrl } from '@/utils/validation'
import { dashboardPath } from '@/utils/app-paths'
import api from '@/api/admin'
import { useBrand } from '@/composables/useBrand'
import { buildApiUrl } from '@/utils/api-url'
import { getDemoLoginAccount } from '@/utils/demo-login'
import { Button, Card, Input, Badge } from '@/components/ui'

const router = useRouter()
const route = useRoute()
const { t } = useI18n()
const authStore = useAuthStore()
const themeStore = useThemeStore()
const brand = useBrand()

const username = ref<string>('')
const password = ref<string>('')
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
void turnstileRef.value // 模板中通过 ref 使用
const footerContactEmail = ref<string | null>(null)

const contactEmailHref = computed(() => {
  const email = footerContactEmail.value?.trim()
  if (!email) return null
  return email.startsWith('mailto:') ? email : `mailto:${email}`
})
const demoAccount = computed(() => getDemoLoginAccount('admin'))

// OAuth 提供商
const oauthProviders = ref<string[]>([])

onMounted(async (): Promise<void> => {
  // 加载 Turnstile 配置和 OAuth 提供商
  try {
    const [configResponse, oauthResponse] = await Promise.all([
      api.systemConfig.getPublic(),
      api.oauth.getProviders()
    ])

    turnstileEnabled.value = configResponse.turnstileEnabled || false
    turnstileSiteKey.value = configResponse.turnstileSiteKey || ''
    footerContactEmail.value = configResponse.footerContactEmail || null
    oauthProviders.value = (oauthResponse as { providers?: string[] }).providers || []
  } catch (e) {
    console.error('Failed to load config:', e)
  }

  // 守卫与参数提示：管理员账号请使用独立管理后台登录
  if (route.query.adminOnly === '1') {
    error.value = '管理员账号请使用独立管理后台登录'
  }

  // 检查 OAuth 回调错误
  checkOAuthErrors()
})

function onTurnstileExpire() {
  turnstileToken.value = ''
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
  if (turnstileEnabled.value && !turnstileToken.value) {
    error.value = t('auth.turnstileRequired')
    return
  }

  loading.value = true
  error.value = ''

  try {
    // 始终发送2FA代码（如果有），后端会根据用户是否启用2FA来决定是否验证
    await authStore.login(
      username.value,
      password.value,
      !useRecoveryCode.value && totpCode.value ? totpCode.value : undefined,
      useRecoveryCode.value && recoveryCode.value ? recoveryCode.value : undefined,
      turnstileEnabled.value ? turnstileToken.value : undefined
    )

    if (!authStore.isAdmin) {
      await authStore.logout()
      error.value = '该入口仅限管理员登录'
      return
    }

    // 安全改进：验证 redirect 参数防止开放重定向漏洞
    const safeRedirect = getSafeRedirectUrl(route.query.redirect as string, dashboardPath())
    router.push(safeRedirect)
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
  icon: string
}

function getProviderInfo(provider: string): ProviderInfo {
  const info: Record<string, ProviderInfo> = {
    github: {
      name: 'GitHub',
      icon: `<svg class="w-4 h-4" fill="currentColor" viewBox="0 0 24 24"><path fill-rule="evenodd" d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0112 6.844c.85.004 1.705.115 2.504.337 1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0022 12.017C22 6.484 17.522 2 12 2z" clip-rule="evenodd"></path></svg>`
    },
    google: {
      name: 'Google',
      icon: `<svg class="w-4 h-4" viewBox="0 0 24 24"><path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z"></path><path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z"></path><path fill="#FBBC05" d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z"></path><path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z"></path></svg>`
    }
  }
  return info[provider] || { name: provider, icon: '' }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center p-4 sm:p-6 bg-[var(--bg-secondary)] text-[var(--text-primary)]">
    <div class="admin-auth-wrap w-full max-w-[400px]">
      <!-- Brand & Header -->
      <div class="mb-8 text-center">
        <div class="mx-auto mb-4 inline-flex items-center justify-center p-2 rounded-2xl bg-[var(--bg-surface)] border border-[var(--border-color)] shadow-[0_2px_8px_rgba(0,0,0,0.04)]">
          <img :src="brand.brandLogoUrl" :alt="brand.brandName" class="h-12 w-12 rounded-xl object-contain" />
        </div>
        <h1 class="text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
          后台管理系统
        </h1>
        <div class="mt-2 flex items-center justify-center gap-2">
          <Badge variant="default" size="sm">
            {{ brand.brandName }} Admin
          </Badge>
        </div>
        <p class="mt-2 text-sm text-[var(--text-secondary)]">
          {{ $t('auth.loginTo') }}
        </p>
      </div>

      <!-- 登录卡片 -->
      <Card padding="md" class="shadow-[0_4px_16px_rgba(0,0,0,0.06)] border border-[var(--border-color)] bg-[var(--bg-surface)]">
        <form class="space-y-4" @submit.prevent="handleLogin">
          <Input
            v-model="username"
            type="text"
            :label="$t('auth.usernameOrEmail')"
            :placeholder="$t('auth.usernameOrEmailPlaceholder')"
            autocomplete="username"
            :disabled="loading"
          />

          <Input
            v-model="password"
            type="password"
            :label="$t('auth.password')"
            :placeholder="$t('auth.passwordPlaceholder')"
            autocomplete="current-password"
            :disabled="loading"
          />

          <!-- 2FA 验证码输入（始终显示，可选） -->
          <div class="space-y-1.5">
            <!-- TOTP 验证码 -->
            <div v-if="!useRecoveryCode">
              <Input
                v-model="totpCode"
                type="text"
                :maxlength="6"
                :placeholder="$t('auth.twoFactorCodePlaceholder')"
                autocomplete="one-time-code"
                :disabled="loading"
                :hint="$t('auth.twoFactorOptionalHint')"
                class="font-mono tracking-[0.25em]"
              >
                <template #label>
                  <div class="flex items-center justify-between w-full">
                    <span>{{ $t('auth.twoFactorCode') }}</span>
                    <Badge variant="default" size="sm">
                      {{ $t('auth.twoFactorOptional') }}
                    </Badge>
                  </div>
                </template>
              </Input>
            </div>
            <!-- 恢复码 -->
            <div v-else>
              <Input
                v-model="recoveryCode"
                type="text"
                :label="$t('auth.recoveryCode')"
                :placeholder="$t('auth.recoveryCodePlaceholder')"
                :disabled="loading"
                :hint="$t('auth.recoveryCodeHint')"
                class="font-mono"
              />
            </div>
            <!-- 切换按钮 -->
            <button
              type="button"
              class="text-xs font-medium text-[var(--accent)] hover:text-[var(--accent-strong)] hover:underline transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] rounded"
              @click="useRecoveryCode = !useRecoveryCode; totpCode = ''; recoveryCode = ''"
            >
              {{ useRecoveryCode ? $t('auth.useTotpCode') : $t('auth.useRecoveryCode') }}
            </button>
          </div>

          <!-- Turnstile 验证 -->
          <div v-if="turnstileEnabled && turnstileSiteKey" class="flex justify-center py-1">
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
            class="flex items-start gap-2.5 p-3 rounded-xl border border-[color-mix(in_srgb,var(--error)_24%,transparent)] bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] text-[var(--error)] text-xs leading-relaxed"
          >
            <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
            <span class="flex-1 font-medium">{{ error }}</span>
          </div>

          <!-- 提交登录按钮 -->
          <Button
            type="submit"
            variant="primary"
            size="lg"
            block
            :loading="loading"
            :disabled="loading"
          >
            {{ loading ? $t('auth.loggingIn') : $t('auth.continue') }}
          </Button>

          <!-- 演示账号快捷填充 -->
          <div
            v-if="demoAccount"
            class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 text-xs transition-all duration-200"
          >
            <div class="mb-2.5 flex items-center justify-between gap-3">
              <div class="flex items-center gap-2">
                <Badge variant="info" size="sm">演示</Badge>
                <span class="font-medium text-[var(--text-primary)]">
                  {{ demoAccount.label }}
                </span>
              </div>
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
            <div class="grid grid-cols-[48px_1fr] gap-x-2 gap-y-1 font-mono text-xs">
              <span class="text-[var(--text-tertiary)]">账号</span>
              <span class="text-[var(--text-secondary)] select-all">{{ demoAccount.username }}</span>
              <span class="text-[var(--text-tertiary)]">邮箱</span>
              <span class="text-[var(--text-secondary)] select-all">{{ demoAccount.email }}</span>
              <span class="text-[var(--text-tertiary)]">密码</span>
              <span class="text-[var(--text-secondary)] select-all">{{ demoAccount.password }}</span>
            </div>
          </div>
        </form>

        <!-- OAuth 快捷登录 -->
        <div v-if="oauthProviders.length > 0" class="mt-6">
          <div class="relative flex items-center justify-center my-4">
            <div class="absolute inset-0 flex items-center">
              <div class="w-full border-t border-[var(--border-color)]"></div>
            </div>
            <div class="relative px-3 bg-[var(--bg-surface)] text-xs text-[var(--text-tertiary)]">
              {{ $t('auth.orUse') }}
            </div>
          </div>

          <div class="grid gap-3" :class="oauthProviders.length > 1 ? 'grid-cols-2' : 'grid-cols-1'">
            <Button
              v-for="provider in oauthProviders"
              :key="provider"
              type="button"
              variant="secondary"
              size="md"
              block
              class="justify-center"
              @click="loginWithOAuth(provider)"
            >
              <template #prefix>
                <span class="inline-flex items-center shrink-0" v-html="getProviderInfo(provider).icon"></span>
              </template>
              <span>{{ getProviderInfo(provider).name }}</span>
            </Button>
          </div>

          <p class="mt-3 text-center text-xs text-[var(--text-tertiary)]">
            {{ $t('auth.oauthBindHint') }}
          </p>
        </div>
      </Card>

      <!-- 仅限管理员账号登录提示 -->
      <p class="mt-6 text-center text-xs text-[var(--text-tertiary)] flex items-center justify-center gap-1.5">
        <ShieldCheck class="w-3.5 h-3.5 shrink-0" />
        <span>仅限管理员账号登录</span>
      </p>
    </div>

    <!-- 右下角操作按钮 -->
    <div class="fixed bottom-4 right-4 z-10 flex items-center gap-2">
      <a
        v-if="contactEmailHref"
        :href="contactEmailHref"
        class="min-h-[44px] min-w-[44px] rounded-full flex items-center justify-center bg-[var(--bg-surface)] border border-[var(--border-color)] text-[var(--text-secondary)] shadow-sm hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
        :title="$t('auth.contactEmail')"
        :aria-label="$t('auth.contactEmail')"
      >
        <Mail class="w-4 h-4" />
      </a>

      <button
        type="button"
        class="min-h-[44px] min-w-[44px] rounded-full flex items-center justify-center bg-[var(--bg-surface)] border border-[var(--border-color)] text-[var(--text-secondary)] shadow-sm hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-all duration-200 active:scale-95 focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
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
.admin-auth-wrap {
  animation: adminAuthRise 0.4s cubic-bezier(0.25, 0.1, 0.25, 1) both;
}

@keyframes adminAuthRise {
  from {
    opacity: 0;
    transform: translateY(10px) scale(0.99);
  }
  to {
    opacity: 1;
    transform: none;
  }
}

@media (prefers-reduced-motion: reduce) {
  .admin-auth-wrap {
    animation: none;
  }
}
</style>
