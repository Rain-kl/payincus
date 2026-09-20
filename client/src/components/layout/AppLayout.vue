<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import { useInboxStore } from '@/stores/inbox'
import { useConfigStore } from '@/stores/config'
import { supportedLocales, setLocale, type Locale } from '@/locales'
import { useBrand } from '@/composables/useBrand'
import SideNav from './SideNav.vue'
import UserAvatar from '@/components/UserAvatar.vue'
import NotificationBell from '@/components/NotificationBell.vue'
import TermsOfServiceModal from '@/components/TermsOfServiceModal.vue'
import { instancesPath, loginPath, profilePath, terminalPath } from '@/utils/app-paths'
import { dashboardPath } from '@/utils/app-paths'

const isAdminEntry = import.meta.env.VITE_APP_ENTRY === 'admin'

const router = useRouter()
const route = useRoute()
const { locale } = useI18n()

// 分栏布局页面：左栏独立滚动，右栏固定
const isSplitPane = computed(() => route.name === 'instance-create')
const authStore = useAuthStore()
const themeStore = useThemeStore()
const inboxStore = useInboxStore()
const configStore = useConfigStore()
const brand = useBrand()
const titleBrandName = computed(() => {
  const name = brand.brandName?.trim()
  return (!name || name === 'Incudal') ? 'Cloud' : name
})
const showTermsModal = ref(false)
const langMenuOpen = ref(false)
const langMenuRef = ref<HTMLElement | null>(null)
const sidebarCollapsed = ref<boolean>(false)
const mobileMenuOpen = ref<boolean>(false)
const userMenuOpen = ref<boolean>(false)
const userMenuRef = ref<HTMLElement | null>(null)
const accountProfilePath = profilePath()
const accountTerminalPath = terminalPath()
const accountInstancesPath = instancesPath()
const accountLoginPath = loginPath()
const navDashboardPath = dashboardPath()
const routeThemeClass = computed(() => {
  const rawName = typeof route.name === 'string'
    ? route.name
    : route.path.split('/').filter(Boolean)[0] || 'home'
  return `kawaii-route-${rawName.replace(/[^a-z0-9-]/gi, '-').toLowerCase()}`
})

async function handleLogout(): Promise<void> {
  userMenuOpen.value = false
  inboxStore.stopPolling()
  await authStore.logout()
  router.push(accountLoginPath)
}

const { t } = useI18n()

function getThemeTooltip(): string {
  switch (themeStore.mode) {
    case 'dark': return t('theme.dark')
    case 'light': return t('theme.light')
    default: return t('theme.system')
  }
}

function toggleMobileMenu() {
  mobileMenuOpen.value = !mobileMenuOpen.value
}

function closeMobileMenu() {
  mobileMenuOpen.value = false
}

function toggleUserMenu() {
  userMenuOpen.value = !userMenuOpen.value
}

function navigateTo(path: string) {
  router.push(path)
  userMenuOpen.value = false
}

function handleClickOutside(event: MouseEvent) {
  if (userMenuRef.value && !userMenuRef.value.contains(event.target as Node)) {
    userMenuOpen.value = false
  }
  if (langMenuRef.value && !langMenuRef.value.contains(event.target as Node)) {
    langMenuOpen.value = false
  }
}

function toggleLangMenu() {
  langMenuOpen.value = !langMenuOpen.value
}

function changeLocale(code: Locale) {
  setLocale(code)
  langMenuOpen.value = false
}

function getCurrentLocaleShort(): string {
  switch (locale.value) {
    case 'zh-CN':
      return '简'
    case 'zh-TW':
      return '繁'
    case 'en':
    default:
      return 'EN'
  }
}

onMounted(() => {
  document.addEventListener('click', handleClickOutside)
  // 启动站内信轮询
  inboxStore.startPolling()
})

onUnmounted(() => {
  document.removeEventListener('click', handleClickOutside)
})
</script>

<template>
  <div class="kawaii-app-shell h-screen flex flex-col overflow-hidden" :class="routeThemeClass">
    <!-- 顶部栏：整行贯通全屏，防止标题栏被分割线切分 -->
    <header class="kawaii-topbar nimbus-topbar h-[72px] w-full flex items-center justify-between px-4 md:px-6 border-b border-[#2c2a28] flex-shrink-0 z-30">
      <div class="flex items-center gap-2.5 md:gap-3 min-w-0">
        <!-- Mobile: Hamburger menu -->
        <button 
          class="kawaii-header-icon md:!hidden p-1.5 rounded transition-colors touch-target"
          :aria-label="t('nav.openMenu')"
          @click="toggleMobileMenu"
        >
          <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M4 6h16M4 12h16M4 18h16" />
          </svg>
        </button>

        <!-- Desktop: Toggle sidebar -->
        <button 
          class="kawaii-header-icon !hidden md:!inline-flex p-1.5 rounded transition-colors"
          :aria-label="t('nav.collapseSidebar')"
          @click="sidebarCollapsed = !sidebarCollapsed"
        >
          <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M4 6h16M4 12h16M4 18h16" />
          </svg>
        </button>

        <!-- Brand: Logo + Name (桌面端与移动端统一位于标题栏最左侧) -->
        <RouterLink :to="navDashboardPath" class="flex items-center gap-2.5 min-w-0 flex-shrink-0">
          <img
            :src="brand.topBarLogoUrl"
            :alt="titleBrandName"
            class="w-7 h-7 rounded flex-shrink-0 object-contain"
          />
          <span
            class="font-semibold text-base text-white truncate tracking-tight"
          >{{ titleBrandName }}</span>
        </RouterLink>
      </div>

      <!-- 右侧菜单 -->
      <div class="flex items-center gap-2 md:gap-3">
        <!-- 终端管理入口 -->
        <button
          v-if="!isAdminEntry"
          class="kawaii-header-icon p-1.5 rounded transition-colors touch-target"
          :title="t('nav.terminal')"
          :aria-label="t('nav.terminal')"
          @click="router.push(accountTerminalPath)"
        >
          <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M8 9l3 3-3 3m5 0h3M5 20h14a2 2 0 002-2V6a2 2 0 00-2-2H5a2 2 0 00-2 2v12a2 2 0 002 2z" />
          </svg>
        </button>

        <!-- 站内信铃铛 -->
        <NotificationBell />

        <!-- 主题切换按钮 -->
        <button
          class="kawaii-header-icon theme-toggle relative group p-1.5 rounded transition-colors touch-target"
          :title="getThemeTooltip()"
          :aria-label="t('nav.toggleTheme')"
          @click="themeStore.toggleTheme"
        >
          <!-- 深色图标 (月亮) -->
          <svg 
            v-if="themeStore.mode === 'dark'" 
            class="w-5 h-5" 
            fill="none" 
            stroke="currentColor" 
            viewBox="0 0 24 24"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z" />
          </svg>
          <!-- 浅色图标 (太阳) -->
          <svg 
            v-else-if="themeStore.mode === 'light'" 
            class="w-5 h-5" 
            fill="none" 
            stroke="currentColor" 
            viewBox="0 0 24 24"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z" />
          </svg>
          <!-- 系统图标 (显示器) -->
          <svg 
            v-else 
            class="w-5 h-5" 
            fill="none" 
            stroke="currentColor" 
            viewBox="0 0 24 24"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
          </svg>
          
          <!-- 悬停提示 (Desktop only) -->
          <span 
            class="hidden md:block absolute -bottom-8 left-1/2 -translate-x-1/2 px-2 py-1 text-xs rounded whitespace-nowrap opacity-0 group-hover:opacity-100 transition-opacity pointer-events-none"
            :class="'kawaii-menu-panel text-themed'"
          >
            {{ getThemeTooltip() }}
          </span>
        </button>

        <!-- 语言切换 -->
        <div ref="langMenuRef" class="relative">
          <button
            class="kawaii-header-icon relative group px-2 py-1.5 rounded transition-colors touch-target text-xs font-medium"
            :title="$t('language.' + (locale === 'zh-CN' ? 'zh' : 'en'))"
            :aria-label="t('nav.toggleLanguage')"
            @click.stop="toggleLangMenu"
          >
            {{ getCurrentLocaleShort() }}
          </button>

          <!-- 语言下拉菜单 -->
          <Transition
            enter-active-class="transition ease-out duration-100"
            enter-from-class="transform opacity-0 scale-95"
            enter-to-class="transform opacity-100 scale-100"
            leave-active-class="transition ease-in duration-75"
            leave-from-class="transform opacity-100 scale-100"
            leave-to-class="transform opacity-0 scale-95"
          >
            <div 
              v-if="langMenuOpen"
              class="kawaii-menu-panel absolute right-0 mt-2 w-36 rounded py-1 z-50 border"
            >
              <button
                v-for="lang in supportedLocales"
                :key="lang.code"
                class="kawaii-menu-item w-full flex items-center justify-between px-3 py-2 text-sm transition-colors"
                :class="{ 'is-active': locale === lang.code }"
                @click="changeLocale(lang.code)"
              >
                {{ lang.name }}
                <svg v-if="locale === lang.code" class="w-4 h-4 -scale-x-100 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
                </svg>
              </button>
            </div>
          </Transition>
        </div>

        <!-- 用户菜单 -->
        <div ref="userMenuRef" class="relative">
          <button
            class="kawaii-header-icon nimbus-userpill flex items-center gap-2 px-2 py-1 rounded transition-colors cursor-pointer"
            @click.stop="toggleUserMenu"
          >
            <UserAvatar 
              :username="authStore.user?.username || ''" 
              :email="authStore.user?.email"
              :avatar-style="authStore.user?.avatarStyle || ''"
              :size="28"
            />
            <span class="hidden sm:block text-sm text-white">{{ authStore.user?.username }}</span>
            <svg class="hidden sm:block w-4 h-4 transition-transform text-white/70" :class="userMenuOpen ? 'rotate-180' : ''" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
            </svg>
          </button>
          
          <!-- 下拉菜单 -->
          <Transition
            enter-active-class="transition ease-out duration-100"
            enter-from-class="transform opacity-0 scale-95"
            enter-to-class="transform opacity-100 scale-100"
            leave-active-class="transition ease-in duration-75"
            leave-from-class="transform opacity-100 scale-100"
            leave-to-class="transform opacity-0 scale-95"
          >
            <div 
              v-if="userMenuOpen"
              class="kawaii-menu-panel absolute right-0 mt-2 w-48 rounded py-1 z-50 border"
            >
              <button
                class="kawaii-menu-item w-full flex items-center gap-3 px-4 py-2.5 text-sm transition-colors"
                @click="navigateTo(accountProfilePath)"
              >
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M16 7a4 4 0 11-8 0 4 4 0 018 0zM12 14a7 7 0 00-7 7h14a7 7 0 00-7-7z" />
                </svg>
                {{ $t('userMenu.profile') }}
              </button>
              <button
                v-if="!isAdminEntry"
                class="kawaii-menu-item w-full flex items-center gap-3 px-4 py-2.5 text-sm transition-colors"
                @click="navigateTo(accountInstancesPath)"
              >
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M5 12h14M5 12a2 2 0 01-2-2V6a2 2 0 012-2h14a2 2 0 012 2v4a2 2 0 01-2 2M5 12a2 2 0 00-2 2v4a2 2 0 002 2h14a2 2 0 002-2v-4a2 2 0 00-2-2" />
                </svg>
                {{ $t('userMenu.myInstances') }}
              </button>
              <div class="my-1 border-t border-themed"></div>
              <button
                class="kawaii-menu-item w-full flex items-center gap-3 px-4 py-2.5 text-sm text-red-500 transition-colors"
                @click="handleLogout"
              >
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M17 16l4-4m0 0l-4-4m4 4H7m6 4v1a3 3 0 01-3 3H6a3 3 0 01-3-3V7a3 3 0 013-3h4a3 3 0 013 3v1" />
                </svg>
                {{ $t('userMenu.logout') }}
              </button>
            </div>
          </Transition>
        </div>
      </div>
    </header>

    <!-- 下方：左右分栏 (左侧导航栏 + 右侧内容区) -->
    <div class="flex-1 flex min-h-0 min-w-0 overflow-hidden">
      <!-- 侧边导航 (下面左侧) -->
      <SideNav 
        :collapsed="sidebarCollapsed" 
        :mobile-open="mobileMenuOpen"
        @close-mobile="closeMobileMenu"
      />

      <!-- 主内容区 (下面右侧) -->
      <div class="flex-1 flex flex-col min-w-0 h-full overflow-hidden">
        <main class="kawaii-workspace nimbus-workspace flex-1 px-4 md:px-6 xl:px-10 py-5 md:py-6" :class="isSplitPane ? 'overflow-auto lg:overflow-hidden' : 'overflow-auto'">
          <div class="w-full mx-auto" :class="isSplitPane ? 'lg:h-full' : 'max-w-[1680px]'">
            <slot />
          </div>
        </main>
      </div>
    </div>

    <!-- 下区：横向贯通页脚 (高度 32px / h-8) -->
    <footer class="kawaii-app-footer h-8 w-full shrink-0 border-t border-[#e5e4e0] bg-[#F5F4F2] text-[#66615e] text-xs px-4 flex items-center justify-between select-none z-20">
      <!-- 左侧：条款与隐私弹窗 + 联系方式 -->
      <div class="flex items-center gap-3 shrink-0 text-[11px] sm:text-xs">
        <button
          type="button"
          class="hover:text-[#161513] transition-colors cursor-pointer text-[#66615e]"
          @click="showTermsModal = true"
        >
          {{ $t('auth.tos.title') || '使用条款和隐私声明' }}
        </button>
        <span class="text-black/20">|</span>
        <div class="flex items-center gap-2.5">
          <a
            v-if="configStore.footerContactEmail"
            :href="`mailto:${configStore.footerContactEmail}`"
            class="hover:text-[#161513] transition-colors flex items-center gap-1 text-[#66615e]"
            :title="configStore.footerContactEmail"
          >
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M3 8l7.89 5.26a2 2 0 002.22 0L21 8M5 19h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
            </svg>
            <span class="hidden sm:inline">{{ configStore.footerContactEmail }}</span>
          </a>
          <a
            v-if="configStore.footerTelegramLink"
            :href="configStore.footerTelegramLink"
            target="_blank"
            rel="noopener noreferrer"
            class="hover:text-[#161513] transition-colors flex items-center gap-1 text-[#66615e]"
            title="Telegram"
          >
            <svg class="w-3.5 h-3.5" fill="currentColor" viewBox="0 0 24 24">
              <path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm4.64 6.8c-.15 1.58-.8 5.42-1.13 7.19-.14.75-.42 1-.68 1.03-.58.05-1.02-.38-1.58-.75-.88-.58-1.38-.94-2.23-1.5-.99-.65-.35-1.01.22-1.59.15-.15 2.71-2.48 2.76-2.69a.2.2 0 00-.05-.18c-.06-.05-.14-.03-.21-.02-.09.02-1.49.95-4.22 2.79-.4.27-.76.41-1.08.4-.36-.01-1.04-.2-1.55-.37-.63-.2-1.12-.31-1.08-.66.02-.18.27-.36.75-.55 2.92-1.27 4.86-2.11 5.83-2.51 2.78-1.16 3.35-1.36 3.73-1.36.08 0 .27.02.39.12.1.08.13.19.14.27-.01.06.01.24 0 .38z"/>
            </svg>
            <span class="hidden md:inline">Telegram</span>
          </a>
        </div>
      </div>

      <!-- 右侧：版权所有 -->
      <div class="flex items-center gap-2 truncate pl-4 text-[11px] sm:text-xs">
        <span class="truncate">{{ brand.brandCopyright }}</span>
      </div>
    </footer>

    <!-- 服务条款与隐私声明弹窗 -->
    <TermsOfServiceModal
      :show="showTermsModal"
      @close="showTermsModal = false"
    />
  </div>
</template>

<style scoped>
/* ============================================================
   Nimbus topbar — translucent blurred console header + clean
   34px icon buttons with indigo hover. Colors from CSS tokens.
   ============================================================ */

.kawaii-topbar.nimbus-topbar {
  position: relative;
  z-index: 40;
  background-color: #393632 !important;
  backdrop-filter: none !important;
  -webkit-backdrop-filter: none !important;
  box-shadow: none !important;
  border-bottom: 1px solid #2c2a28 !important;
  border-color: #2c2a28 !important;
  color: #ffffff !important;
}

:global(.dark) .kawaii-topbar.nimbus-topbar {
  background-color: #1f1d1b !important;
  border-bottom: 1px solid #2c2a28 !important;
  border-color: #2c2a28 !important;
}

/* Icon buttons — covers direct topbar buttons + child components
   (e.g. NotificationBell) that carry the shared .kawaii-header-icon class */
.kawaii-topbar :deep(.kawaii-header-icon) {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  height: 32px;
  min-width: 32px;
  padding: 0 7px;
  border-radius: var(--radius-btn, 4px);
  border: 1px solid transparent;
  color: #c7c5c2 !important;
  transition: color 160ms ease, background-color 160ms ease, border-color 160ms ease;
}

.kawaii-topbar :deep(.kawaii-header-icon:hover) {
  color: #ffffff !important;
  border-color: transparent !important;
  background: rgba(255, 255, 255, 0.1) !important;
}

/* Account chip keeps avatar + name; resting hairline border */
.kawaii-topbar :deep(.nimbus-userpill) {
  gap: 8px;
  padding: 0 10px 0 6px;
  border: 1px solid rgba(255, 255, 255, 0.15) !important;
  border-radius: var(--radius-btn, 4px);
  color: #ffffff !important;
  transition: color 160ms ease, background-color 160ms ease, border-color 160ms ease;
}

.kawaii-topbar :deep(.nimbus-userpill:hover) {
  border-color: rgba(255, 255, 255, 0.3) !important;
  background: rgba(255, 255, 255, 0.1) !important;
  color: #ffffff !important;
  box-shadow: none !important;
}

@media (prefers-reduced-motion: reduce) {
  .kawaii-topbar :deep(.kawaii-header-icon),
  .kawaii-topbar :deep(.nimbus-userpill) {
    transition: none;
  }
}

/* Bottom Footer — compact 32px bar across full screen width */
.kawaii-app-footer {
  background-color: #F5F4F2 !important;
  border-top: 1px solid #e5e4e0 !important;
  color: #66615e !important;
}

:global(.light) .kawaii-app-footer,
:global(.dark) .kawaii-app-footer {
  background-color: #F5F4F2 !important;
  border-top: 1px solid #e5e4e0 !important;
  color: #66615e !important;
}

.kawaii-app-footer a,
.kawaii-app-footer button {
  color: #66615e !important;
}

.kawaii-app-footer a:hover,
.kawaii-app-footer button:hover {
  color: #161513 !important;
}
</style>
