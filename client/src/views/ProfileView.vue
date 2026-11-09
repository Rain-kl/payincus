<script setup lang="ts">
/**
 * ProfileView - Apple HIG 风格现代化用户设置页面
 *
 * 拆分并复用的子组件：
 * - AccountSection: 账户信息和配额显示、API Keys、头像风格修改
 * - PasswordSection: 密码修改与敏感验证
 * - TwoFactorSection: 双因素认证 (2FA) 与恢复码
 * - OAuthSection: OAuth 快捷登录绑定
 * - TelegramBindingSection: Telegram 账号通知与绑定
 * - SSHKeysSection: SSH 公钥管理（仅普通用户展示）
 * - NotificationSection: 通知渠道与推送配置
 * - LoginHistorySection: 登录历史记录与安全审计
 *
 * 遵循 THEME.md Apple HIG 设计规范，复用 @/components/ui 组件库
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Bell,
  History,
  Lock,
  RefreshCw,
  Send,
  Share2,
  ShieldCheck,
  Terminal,
  User
} from 'lucide-vue-next'
import { useAuthStore } from '@/stores/auth'
import { useReveal } from '@/composables/useReveal'
import { Badge, Button, Card, PageHeader } from '@/components/ui'

import AccountSection from '@/components/profile/AccountSection.vue'
import PasswordSection from '@/components/profile/PasswordSection.vue'
import TwoFactorSection from '@/components/profile/TwoFactorSection.vue'
import OAuthSection from '@/components/profile/OAuthSection.vue'
import TelegramBindingSection from '@/components/profile/TelegramBindingSection.vue'
import SSHKeysSection from '@/components/profile/SSHKeysSection.vue'
import NotificationSection from '@/components/profile/NotificationSection.vue'
import LoginHistorySection from '@/components/profile/LoginHistorySection.vue'

// 为 KeepAlive include 匹配定义组件名称（必须在所有 import 之后）
defineOptions({ name: 'ProfileView' })

const { t } = useI18n()
const authStore = useAuthStore()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

const accountSectionRef = ref<InstanceType<typeof AccountSection> | null>(null)
const refreshing = ref(false)
const activeSection = ref('section-account')

// 页面导航项
const navSections = computed(() => {
  const items = [
    { id: 'section-account', label: t('profile.account.title'), icon: User },
    { id: 'section-password', label: t('profile.password.title'), icon: Lock },
    { id: 'section-2fa', label: t('profile.twoFactorAuth.title'), icon: ShieldCheck },
    { id: 'section-oauth', label: t('profile.oauth.title'), icon: Share2 },
    { id: 'section-telegram', label: t('profile.telegramBinding.title'), icon: Send }
  ]
  if (!authStore.isAdmin) {
    items.push({ id: 'section-ssh', label: t('profile.sshKeys.title'), icon: Terminal })
  }
  items.push(
    { id: 'section-notifications', label: t('profile.notifications.title'), icon: Bell },
    { id: 'section-history', label: t('profile.loginHistory.title'), icon: History }
  )
  return items
})

// 滚动跳转至对应区域
function scrollToSection(id: string) {
  activeSection.value = id
  const target = document.getElementById(id)
  if (target) {
    target.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }
}

// 刷新用户资料与账户详情
async function handleRefresh() {
  refreshing.value = true
  try {
    await Promise.all([
      authStore.fetchCurrentUser(),
      accountSectionRef.value?.loadUserDetails?.()
    ])
  } catch (err) {
    console.error('Failed to refresh profile data:', err)
  } finally {
    refreshing.value = false
  }
}

// 视口 IntersectionObserver 动态跟随当前激活卡片
let observer: IntersectionObserver | null = null

onMounted(() => {
  if (typeof IntersectionObserver !== 'undefined') {
    observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            activeSection.value = entry.target.id
          }
        }
      },
      {
        rootMargin: '-20% 0px -60% 0px',
        threshold: 0
      }
    )

    navSections.value.forEach((item) => {
      const el = document.getElementById(item.id)
      if (el && observer) {
        observer.observe(el)
      }
    })
  }
})

onUnmounted(() => {
  if (observer) {
    observer.disconnect()
    observer = null
  }
})
</script>

<template>
  <div ref="revealRoot" class="max-w-5xl mx-auto space-y-6 sm:space-y-8 animate-fade-in pb-16">
    <!-- Apple HIG 风格页面顶栏 -->
    <PageHeader
      data-reveal
      :title="$t('profile.title')"
      :description="authStore.user?.email ? `${authStore.user.username} (${authStore.user.email})` : authStore.user?.username || ''"
    >
      <template #actions>
        <div class="flex items-center gap-2 flex-wrap">
          <Badge :variant="authStore.isAdmin ? 'warning' : 'success'" dot size="md">
            {{ authStore.isAdmin ? $t('profile.account.admin') : $t('profile.account.user') }}
          </Badge>
          <Badge variant="default" size="md" class="tabular-nums font-mono">
            {{ $t('profile.account.uid') }}: {{ authStore.user?.id }}
          </Badge>
          <Button
            variant="secondary"
            size="sm"
            :loading="refreshing"
            @click="handleRefresh"
          >
            <template #icon>
              <RefreshCw class="w-3.5 h-3.5" :class="{ 'animate-spin': refreshing }" />
            </template>
            <span>{{ $t('common.refresh') }}</span>
          </Button>
        </div>
      </template>
    </PageHeader>

    <!-- Apple 风格分段快速导航卡片 -->
    <Card data-reveal padding="none" class="p-1.5 sm:p-2 sticky top-4 z-10 backdrop-blur-md bg-[var(--bg-surface)]/90 border border-[var(--border-color)]">
      <div class="flex items-center gap-1 sm:gap-1.5 overflow-x-auto tab-scroll-hide py-0.5" role="tablist">
        <button
          v-for="item in navSections"
          :key="item.id"
          type="button"
          role="tab"
          :aria-selected="activeSection === item.id"
          class="inline-flex items-center gap-1.5 px-3.5 py-1.5 sm:py-2 rounded-full text-xs sm:text-sm font-medium whitespace-nowrap transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.97] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="activeSection === item.id
            ? 'bg-[var(--accent)] text-white shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)]'"
          @click="scrollToSection(item.id)"
        >
          <component :is="item.icon" class="w-3.5 h-3.5 shrink-0" />
          <span>{{ item.label }}</span>
        </button>
      </div>
    </Card>

    <!-- 各设置 Section 栈 -->
    <div class="space-y-6 sm:space-y-8">
      <!-- 账户信息和配额 -->
      <section id="section-account" class="scroll-mt-24">
        <AccountSection ref="accountSectionRef" data-reveal />
      </section>

      <!-- 修改密码 -->
      <section id="section-password" class="scroll-mt-24">
        <PasswordSection data-reveal />
      </section>

      <!-- 双因素认证 -->
      <section id="section-2fa" class="scroll-mt-24">
        <TwoFactorSection data-reveal />
      </section>

      <!-- OAuth 账号绑定 -->
      <section id="section-oauth" class="scroll-mt-24">
        <OAuthSection data-reveal />
      </section>

      <!-- Telegram 绑定 -->
      <section id="section-telegram" class="scroll-mt-24">
        <TelegramBindingSection data-reveal />
      </section>

      <!-- SSH 公钥（仅普通用户） -->
      <section v-if="!authStore.isAdmin" id="section-ssh" class="scroll-mt-24">
        <SSHKeysSection data-reveal />
      </section>

      <!-- 通知渠道 -->
      <section id="section-notifications" class="scroll-mt-24">
        <NotificationSection data-reveal />
      </section>

      <!-- 登录历史记录 -->
      <section id="section-history" class="scroll-mt-24">
        <LoginHistorySection data-reveal />
      </section>
    </div>
  </div>
</template>

<style scoped>
.tab-scroll-hide {
  -ms-overflow-style: none;
  scrollbar-width: none;
}
.tab-scroll-hide::-webkit-scrollbar {
  display: none;
}

/* Apple HIG 标准卡片修饰：将各 Section 统一收敛为 18px 圆角、微阴影与发丝边框 */
:deep(.card.nimbus-section),
:deep(.nimbus-section) {
  border-radius: 1.125rem !important; /* 18px Apple standard */
  border: 1px solid var(--border-color) !important;
  background-color: var(--bg-surface) !important;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.04) !important;
  transition: all 0.3s cubic-bezier(0.25, 0.1, 0.25, 1);
}

:deep(.card.nimbus-section:hover),
:deep(.nimbus-section:hover) {
  border-color: var(--border-hover) !important;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.06) !important;
}

:deep(.nimbus-section-title) {
  font-size: 1.125rem !important;
  font-weight: 600 !important;
  letter-spacing: -0.02em !important;
  color: var(--text-primary) !important;
}

:deep(.nimbus-section-desc) {
  font-size: 0.8125rem !important;
  color: var(--text-secondary) !important;
  margin-top: 0.25rem !important;
}
</style>
