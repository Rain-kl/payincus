<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  AlertCircle,
  Check,
  Globe,
  Info,
  KeyRound
} from 'lucide-vue-next'
import api from '@/api'
import type { OAuthProviderConsentResponse, PublicApiScopeMetadata } from '@/types/api'
import { dashboardPath } from '@/utils/app-paths'
import { useAuthStore } from '@/stores/auth'
import { useBrand } from '@/composables/useBrand'
import { useReveal } from '@/composables/useReveal'
import { Badge, Button, Card } from '@/components/ui'

defineOptions({ name: 'OAuthAuthorizeView' })

const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const brand = useBrand()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const loading = ref(true)
const submitting = ref(false)
const error = ref('')
const errorHint = ref('')
const consent = ref<OAuthProviderConsentResponse | null>(null)
const scopeCatalog = ref<PublicApiScopeMetadata[]>([])

const requestParams = computed(() => {
  const clientId = String(route.query.client_id || route.query.clientId || '')
  const redirectUri = String(route.query.redirect_uri || route.query.redirectUri || '')
  const scope = String(route.query.scope || 'profile:read')
  const state = typeof route.query.state === 'string' ? route.query.state : null
  return {
    response_type: 'code',
    client_id: clientId,
    redirect_uri: redirectUri,
    scope,
    state
  }
})

const requestedScopes = computed(() => consent.value?.requestedScopes || [])
const scopeMetadataByScope = computed(() => {
  const map = new Map<string, PublicApiScopeMetadata>()
  for (const item of scopeCatalog.value) map.set(item.scope, item)
  for (const item of consent.value?.scopeMetadata || []) map.set(item.scope, item)
  return map
})

function redirectTo(url: string) {
  window.location.href = url
}

function describeConsentError(err: unknown): { message: string; hint: string } {
  const apiError = err as { code?: unknown; message?: unknown }
  const message = typeof apiError?.message === 'string' ? apiError.message : String(err)
  if (
    apiError?.code === 'OAUTH_CONSENT_FAILED' &&
    /disabled or missing|invalid client_id|invalid redirect_uri/i.test(message)
  ) {
    return {
      message: '授权应用不可用',
      hint: '该应用不存在、已停用，或回调地址与配置不匹配。请返回来源页面重新发起授权。'
    }
  }
  return {
    message: message || 'OAuth 授权请求加载失败',
    hint: '请确认授权链接完整有效，或稍后重试。'
  }
}

function buildDeniedRedirect(): string | null {
  const redirectUri = consent.value?.request.redirectUri || requestParams.value.redirect_uri
  if (!redirectUri) return null
  const target = new URL(redirectUri)
  target.searchParams.set('error', 'access_denied')
  target.searchParams.set('error_description', 'The user denied the authorization request')
  const state = consent.value?.request.state || requestParams.value.state
  if (state) target.searchParams.set('state', state)
  return target.toString()
}

async function loadConsent() {
  loading.value = true
  error.value = ''
  errorHint.value = ''
  try {
    if (!requestParams.value.client_id || !requestParams.value.redirect_uri) {
      throw new Error('OAuth 授权请求缺少 client_id 或 redirect_uri')
    }
    const response = await api.oauthProvider.getConsent(requestParams.value)
    consent.value = response
    scopeCatalog.value = response.scopeMetadata
    if (!response.consentRequired) {
      await approve()
    }
  } catch (err: any) {
    const friendlyError = describeConsentError(err)
    error.value = friendlyError.message
    errorHint.value = friendlyError.hint
  } finally {
    loading.value = false
  }
}

async function loadScopeCatalog() {
  try {
    const response = await api.oauthProvider.listScopes()
    scopeCatalog.value = response.scopes
  } catch {
    scopeCatalog.value = []
  }
}

async function approve() {
  if (submitting.value) return
  submitting.value = true
  error.value = ''
  errorHint.value = ''
  try {
    const params = requestParams.value
    const response = await api.oauthProvider.confirm({
      responseType: 'code',
      clientId: params.client_id,
      redirectUri: params.redirect_uri,
      scope: params.scope,
      state: params.state,
      confirmed: true
    })
    redirectTo(response.redirectTo)
  } catch (err: any) {
    const friendlyError = describeConsentError(err)
    error.value = friendlyError.message
    errorHint.value = friendlyError.hint
  } finally {
    submitting.value = false
  }
}

function deny() {
  const deniedRedirect = buildDeniedRedirect()
  if (deniedRedirect) {
    redirectTo(deniedRedirect)
    return
  }
  router.push(dashboardPath())
}

onMounted(() => {
  if (!authStore.user) {
    void authStore.fetchCurrentUser().catch(() => {})
  }
  void loadScopeCatalog()
  void loadConsent()
})
</script>

<template>
  <main
    ref="revealRoot"
    class="min-h-screen flex flex-col items-center justify-center px-4 py-12 sm:px-6 md:py-20 bg-[var(--bg-primary)] text-[var(--text-primary)] transition-colors duration-200"
  >
    <section class="w-full max-w-xl mx-auto" data-reveal>
      <Card padding="md" class="border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_4px_24px_rgba(0,0,0,0.04)]">
        <!-- 1. 加载中骨架屏 -->
        <div
          v-if="loading"
          class="space-y-6 py-2 animate-pulse"
          role="status"
          aria-label="正在加载授权信息"
        >
          <!-- App Header Skeleton -->
          <div class="flex flex-col items-center text-center space-y-3">
            <div class="w-16 h-16 rounded-2xl bg-[var(--bg-surface-soft)]"></div>
            <div class="h-5 w-44 rounded-full bg-[var(--bg-surface-soft)]"></div>
            <div class="h-3.5 w-60 rounded-full bg-[var(--bg-surface-soft)]"></div>
          </div>
          <!-- Scopes Skeleton -->
          <div class="space-y-2.5 pt-2">
            <div class="h-3 w-28 rounded-full bg-[var(--bg-surface-soft)]"></div>
            <div class="space-y-2">
              <div class="h-16 rounded-xl bg-[var(--bg-surface-soft)]"></div>
              <div class="h-16 rounded-xl bg-[var(--bg-surface-soft)]"></div>
            </div>
          </div>
          <!-- Actions Skeleton -->
          <div class="flex gap-3 pt-3">
            <div class="h-11 flex-1 rounded-full bg-[var(--bg-surface-soft)]"></div>
            <div class="h-11 flex-1 rounded-full bg-[var(--bg-surface-soft)]"></div>
          </div>
        </div>

        <!-- 2. 错误反馈状态 -->
        <div v-else-if="error" class="space-y-6 text-center py-2">
          <div class="w-14 h-14 mx-auto rounded-2xl flex items-center justify-center bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] border border-[color-mix(in_srgb,var(--error)_24%,var(--bg-surface))] shadow-sm">
            <AlertCircle class="w-7 h-7" />
          </div>

          <div class="space-y-2">
            <h1 class="text-xl font-semibold tracking-tight text-[var(--text-primary)]">
              OAuth 授权失败
            </h1>
            <p class="text-sm font-medium text-[var(--error)] leading-relaxed">
              {{ error }}
            </p>
          </div>

          <div
            v-if="errorHint"
            class="rounded-xl p-4 text-xs leading-relaxed text-left border bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))] text-[var(--text-secondary)] shadow-sm"
          >
            <div class="flex items-start gap-2.5">
              <Info class="w-4 h-4 shrink-0 text-[var(--warning)] mt-0.5" />
              <span class="flex-1">{{ errorHint }}</span>
            </div>
          </div>

          <div class="pt-2">
            <Button
              variant="secondary"
              block
              @click="router.push(dashboardPath())"
            >
              返回控制台
            </Button>
          </div>
        </div>

        <!-- 3. OAuth 确认授权表单 -->
        <div v-else-if="consent" class="space-y-6 py-1">
          <!-- 头部：应用信息与来源识别 -->
          <div class="text-center">
            <div class="w-16 h-16 mx-auto mb-3.5 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] flex items-center justify-center text-[var(--accent)] shadow-sm transition-transform duration-300 hover:scale-105">
              <KeyRound class="w-8 h-8" />
            </div>

            <h1 class="text-xl sm:text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
              授权 {{ consent.app.name }}
            </h1>
            <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-1.5">
              申请访问您的 {{ brand.brandName }} 账号
            </p>

            <div class="mt-3 flex flex-wrap items-center justify-center gap-2">
              <span
                class="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs font-mono bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] border border-[var(--border-color)] max-w-full"
                :title="consent.request.redirectUri"
              >
                <Globe class="w-3.5 h-3.5 shrink-0 text-[var(--text-tertiary)]" />
                <span class="truncate max-w-[240px] sm:max-w-xs">{{ consent.request.redirectUri }}</span>
              </span>
            </div>

            <div class="text-[11px] font-mono text-[var(--text-tertiary)] mt-1.5">
              Client ID: {{ consent.app.clientId }}
            </div>
          </div>

          <!-- 权限清单 -->
          <div class="space-y-3">
            <div class="flex items-center justify-between">
              <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-tertiary)]">
                {{ consent.app.name }} 申请获取的权限
              </span>
              <Badge variant="default" size="sm">
                {{ requestedScopes.length }} 项权限
              </Badge>
            </div>

            <div class="space-y-2.5">
              <div
                v-for="scope in requestedScopes"
                :key="scope"
                class="group rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 sm:p-4 flex items-start gap-3.5 transition-all duration-200 hover:border-[var(--border-hover)]"
              >
                <div
                  class="w-7 h-7 shrink-0 rounded-lg flex items-center justify-center transition-colors mt-0.5"
                  :class="consent.existingScopes.includes(scope)
                    ? 'bg-[color-mix(in_srgb,var(--success)_14%,var(--bg-surface))] text-[var(--success)]'
                    : 'bg-[color-mix(in_srgb,var(--accent)_14%,var(--bg-surface))] text-[var(--accent)]'"
                >
                  <Check class="w-4 h-4 stroke-[2.5]" />
                </div>

                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="text-sm font-medium text-[var(--text-primary)]">
                      {{ scopeMetadataByScope.get(scope)?.title || scope }}
                    </span>
                    <Badge
                      v-if="consent.existingScopes.includes(scope)"
                      variant="success"
                      size="sm"
                      dot
                    >
                      已授权
                    </Badge>
                    <Badge
                      v-else-if="scopeMetadataByScope.get(scope)?.risk === 'high'"
                      variant="error"
                      size="sm"
                    >
                      高风险
                    </Badge>
                    <Badge
                      v-else-if="scopeMetadataByScope.get(scope)?.risk === 'medium'"
                      variant="warning"
                      size="sm"
                    >
                      敏感
                    </Badge>
                  </div>

                  <p class="text-xs text-[var(--text-secondary)] mt-1 leading-relaxed">
                    {{ scopeMetadataByScope.get(scope)?.description || '访问授权范围内的公共 API' }}
                  </p>

                  <div
                    v-if="scopeMetadataByScope.get(scope)?.resources.length"
                    class="mt-2 flex flex-wrap gap-1.5"
                  >
                    <span
                      v-for="res in scopeMetadataByScope.get(scope)?.resources"
                      :key="res"
                      class="px-2 py-0.5 rounded-md font-mono text-[10px] bg-[var(--bg-surface)] text-[var(--text-tertiary)] border border-[var(--border-color)]"
                    >
                      {{ res }}
                    </span>
                  </div>
                </div>
              </div>
            </div>
          </div>

          <!-- 用户身份与隐私提示 -->
          <div v-if="authStore.user" class="text-center text-xs text-[var(--text-secondary)]">
            以 <span class="font-medium text-[var(--text-primary)]">{{ authStore.user.username }}</span> 的身份授权
          </div>

          <p class="text-[11px] leading-relaxed text-center text-[var(--text-tertiary)] px-2">
            点击“允许授权”即表示您同意向该应用授予上述权限。您可以随时在控制台中撤回此授权。
          </p>

          <!-- 交互操作按钮 -->
          <div class="flex flex-col-reverse sm:flex-row items-center gap-3 pt-2">
            <Button
              variant="secondary"
              class="w-full sm:flex-1"
              :disabled="submitting"
              @click="deny"
            >
              拒绝
            </Button>
            <Button
              variant="primary"
              class="w-full sm:flex-1"
              :loading="submitting"
              @click="approve"
            >
              {{ submitting ? '处理中...' : '允许授权' }}
            </Button>
          </div>
        </div>
      </Card>

      <!-- 底部版权/品牌说明 -->
      <div class="mt-8 text-center text-xs text-[var(--text-tertiary)]">
        {{ brand.brandCopyright }}
      </div>
    </section>
  </main>
</template>
