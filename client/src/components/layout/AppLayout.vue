<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import { useInboxStore } from '@/stores/inbox'
import { useConfigStore } from '@/stores/config'
import { supportedLocales, setLocale, type Locale } from '@/locales'
import { useBrand } from '@/composables/useBrand'
import SideNav from './SideNav.vue'
import MenuSearchBox from './MenuSearchBox.vue'
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
const sidebarCollapsed = ref<boolean>(false)
const mobileMenuOpen = ref<boolean>(false)
const userMenuOpen = ref<boolean>(false)
const langDropdownOpen = ref<boolean>(false)
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
  langDropdownOpen.value = false
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
  if (!userMenuOpen.value) {
    langDropdownOpen.value = false
  }
}

function toggleLangDropdown() {
  langDropdownOpen.value = !langDropdownOpen.value
}

function navigateTo(path: string) {
  router.push(path)
  userMenuOpen.value = false
  langDropdownOpen.value = false
}

function handleClickOutside(event: MouseEvent) {
  if (userMenuRef.value && !userMenuRef.value.contains(event.target as Node)) {
    userMenuOpen.value = false
    langDropdownOpen.value = false
  }
}

function changeLocale(code: Locale) {
  setLocale(code)
  langDropdownOpen.value = false
}

function getLocaleDisplayName(code: Locale): string {
  switch (code) {
    case 'zh-CN':
      return '中文 (简体)'
    case 'zh-TW':
      return '中文 (繁體)'
    case 'en':
    default:
      return 'English'
  }
}

const currentLocaleDisplay = computed(() => {
  return getLocaleDisplayName(locale.value as Locale)
})

function updateLayoutVars() {
  if (typeof document === 'undefined') return
  document.documentElement.style.setProperty('--topbar-height', '62px')
  document.documentElement.style.setProperty('--footer-height', '32px')
  document.documentElement.style.setProperty('--sidebar-width', sidebarCollapsed.value ? '64px' : '240px')
}

function resetLayoutVars() {
  if (typeof document === 'undefined') return
  document.documentElement.style.removeProperty('--topbar-height')
  document.documentElement.style.removeProperty('--footer-height')
  document.documentElement.style.removeProperty('--sidebar-width')
}

watch(sidebarCollapsed, () => {
  updateLayoutVars()
})

onMounted(() => {
  document.addEventListener('click', handleClickOutside)
  // 启动站内信轮询
  inboxStore.startPolling()
  updateLayoutVars()
})

onUnmounted(() => {
  document.removeEventListener('click', handleClickOutside)
  resetLayoutVars()
})
</script>

<template>
  <div class="kawaii-app-shell h-screen flex flex-col overflow-hidden" :class="routeThemeClass">
    <!-- 顶部栏：整行贯通全屏，防止标题栏被分割线切分 -->
    <header class="kawaii-topbar nimbus-topbar h-[62px] w-full flex items-center justify-between px-4 md:px-6 border-b border-[var(--topbar-border)] flex-shrink-0 z-30">
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

      <!-- 中间：搜索框 (居中放置且拉长，随页面大小变化自适应伸缩，屏幕过小时收缩为图标) -->
      <div class="flex-1 flex justify-center items-center px-2 sm:px-4 md:px-6 min-w-0 max-w-xl lg:max-w-2xl xl:max-w-3xl mx-auto">
        <MenuSearchBox />
      </div>

      <!-- 右侧菜单 -->
      <div class="flex items-center gap-2 md:gap-3 flex-shrink-0">
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

        <!-- 用户菜单 (仅显示方形图标) -->
        <div ref="userMenuRef" class="relative">
          <button
            type="button"
            class="nimbus-user-trigger flex items-center justify-center p-0.5 rounded transition-all cursor-pointer focus:outline-none"
            :class="userMenuOpen ? 'ring-1 ring-white/60' : ''"
            :title="authStore.user?.username || t('userMenu.profile')"
            :aria-label="t('userMenu.profile')"
            @click.stop="toggleUserMenu"
          >
            <div class="nimbus-user-icon-box w-[32px] h-[32px] rounded flex items-center justify-center overflow-hidden shadow-sm select-none">
              <svg class="w-4 h-4 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M16 7a4 4 0 11-8 0 4 4 0 018 0zM12 14a7 7 0 00-7 7h14a7 7 0 00-7-7z" />
              </svg>
            </div>
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
              class="dropdown-menu kawaii-menu-panel oci-user-dropdown absolute right-0 mt-2 w-[280px] rounded border border-themed bg-themed-surface p-5 shadow-xl z-50 select-none text-[15px]"
            >
              <!-- 概要信息 -->
              <div>
                <h3 class="text-base font-bold text-[var(--text-primary)] tracking-tight">
                  {{ t('userMenu.summary') }}
                </h3>
                <div class="mt-3.5 space-y-2.5">
                  <div class="text-[var(--accent)] truncate">
                    {{ authStore.user?.email || authStore.user?.username }}
                  </div>
                  <div class="text-[var(--accent)]">
                    {{ t('userMenu.identityDomain') }}：&nbsp;&nbsp;Default
                  </div>
                  <div class="text-[var(--accent)] truncate">
                    {{ t('userMenu.tenant') }}：&nbsp;{{ authStore.user?.username }}
                  </div>
                  
                  <!-- 语言设置移入这里 -->
                  <div class="relative">
                    <button
                      type="button"
                      class="w-full flex items-center justify-between text-[var(--accent)] hover:underline cursor-pointer group text-left"
                      @click.stop="toggleLangDropdown"
                    >
                      <span>{{ t('userMenu.language') }}：&nbsp;{{ currentLocaleDisplay }}</span>
                      <svg class="w-3.5 h-3.5 transition-transform opacity-70 group-hover:opacity-100" :class="langDropdownOpen ? 'rotate-180' : ''" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
                      </svg>
                    </button>

                    <!-- 展开语言选择列表 -->
                    <div
                      v-if="langDropdownOpen"
                      class="mt-2 py-1 px-1 rounded border border-themed bg-themed-secondary space-y-0.5"
                    >
                      <button
                        v-for="lang in supportedLocales"
                        :key="lang.code"
                        type="button"
                        class="w-full flex items-center justify-between px-3 py-1.5 text-xs rounded transition-colors"
                        :class="locale === lang.code 
                          ? 'bg-[color-mix(in_srgb,var(--accent)_10%,transparent)] text-[var(--accent)] font-medium' 
                          : 'text-[var(--text-primary)] hover:bg-black/5 dark:hover:bg-white/5'"
                        @click.stop="changeLocale(lang.code)"
                      >
                        <span>{{ getLocaleDisplayName(lang.code) }}</span>
                        <svg v-if="locale === lang.code" class="w-3.5 h-3.5 text-[var(--accent)]" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
                        </svg>
                      </button>
                    </div>
                  </div>
                </div>
              </div>

              <!-- 分割线 -->
              <div class="border-t border-themed my-4 -mx-5"></div>

              <!-- 操作设置项 -->
              <div class="space-y-2.5">
                <button
                  type="button"
                  class="block w-full text-left text-[var(--accent)] hover:underline cursor-pointer transition-colors"
                  @click="navigateTo(accountProfilePath)"
                >
                  {{ t('userMenu.profile') }}
                </button>
                <button
                  type="button"
                  class="block w-full text-left text-[var(--accent)] hover:underline cursor-pointer transition-colors"
                  @click="navigateTo(accountInstancesPath)"
                >
                  {{ t('userMenu.consoleSettings') }}
                </button>
              </div>

              <!-- 分割线 -->
              <div class="border-t border-themed my-4 -mx-5"></div>

              <!-- 注销 -->
              <div>
                <button
                  type="button"
                  class="block w-full text-left text-[var(--accent)] hover:underline cursor-pointer transition-colors"
                  @click="handleLogout"
                >
                  {{ t('userMenu.logout') }}
                </button>
              </div>
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
      <div class="flex-1 flex flex-col min-w-0 h-full overflow-hidden relative">
        <main class="kawaii-workspace nimbus-workspace flex-1 px-4 md:px-6 xl:px-10 py-5 md:py-6" :class="isSplitPane ? 'overflow-auto lg:overflow-hidden' : 'overflow-auto'">
          <div class="w-full mx-auto" :class="isSplitPane ? 'lg:h-full' : 'max-w-[1680px]'">
            <slot />
          </div>
        </main>
      </div>
    </div>

    <!-- 下区：横向贯通页脚 (高度 32px / h-8) -->
    <footer class="kawaii-app-footer h-8 w-full shrink-0 border-t border-[var(--border-color)] bg-[var(--footer-bg)] text-[var(--text-secondary)] text-xs px-4 flex items-center justify-between select-none z-30">
      <!-- 左侧：条款与隐私弹窗 + 联系方式 -->
      <div class="flex items-center gap-3 shrink-0 text-[11px] sm:text-xs">
        <button
          type="button"
          class="hover:text-[var(--nav-active)] transition-colors cursor-pointer text-[var(--text-secondary)]"
          @click="showTermsModal = true"
        >
          {{ $t('auth.tos.title') || '使用条款和隐私声明' }}
        </button>
        <span class="text-black/20">|</span>
        <div class="flex items-center gap-2.5">
          <a
            v-if="configStore.footerContactEmail"
            :href="`mailto:${configStore.footerContactEmail}`"
            class="hover:text-[var(--nav-active)] transition-colors flex items-center gap-1 text-[var(--text-secondary)]"
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
            class="hover:text-[var(--nav-active)] transition-colors flex items-center gap-1 text-[var(--text-secondary)]"
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
  background-color: var(--topbar-bg) !important;
  backdrop-filter: none !important;
  -webkit-backdrop-filter: none !important;
  box-shadow: none !important;
  border-bottom: 1px solid var(--topbar-border) !important;
  border-color: var(--topbar-border) !important;
  color: var(--topbar-text) !important;
}

:global(.dark) .kawaii-topbar.nimbus-topbar {
  background-color: var(--topbar-bg) !important;
  border-bottom: 1px solid var(--topbar-border) !important;
  border-color: var(--topbar-border) !important;
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
  color: color-mix(in srgb, var(--topbar-text) 78%, transparent) !important;
  transition: color 160ms ease, background-color 160ms ease, border-color 160ms ease;
}

.kawaii-topbar :deep(.kawaii-header-icon:hover) {
  color: var(--topbar-text) !important;
  border-color: transparent !important;
  background: color-mix(in srgb, var(--topbar-text) 10%, transparent) !important;
}

/* User square trigger & icon box */
.nimbus-user-trigger {
  border-radius: var(--radius-btn, 4px);
  padding: 1px;
}

.nimbus-user-icon-box {
  background-color: #745872;
  background-image: repeating-linear-gradient(
    45deg,
    rgba(255, 255, 255, 0.1) 0px,
    rgba(255, 255, 255, 0.1) 1.5px,
    transparent 1.5px,
    transparent 3.5px
  );
  border: 1px solid rgba(255, 255, 255, 0.22);
  transition: border-color 160ms ease, filter 160ms ease;
}

.nimbus-user-trigger:hover .nimbus-user-icon-box {
  border-color: rgba(255, 255, 255, 0.45);
  filter: brightness(1.08);
}

.oci-user-dropdown {
  box-shadow: 0 8px 30px rgba(0, 0, 0, 0.15) !important;
  background-color: var(--kawaii-surface) !important;
  backdrop-filter: none !important;
  -webkit-backdrop-filter: none !important;
  opacity: 1 !important;
}

@media (prefers-reduced-motion: reduce) {
  .kawaii-topbar :deep(.kawaii-header-icon),
  .nimbus-user-icon-box {
    transition: none;
  }
}

/* Bottom Footer — compact 32px bar across full screen width */
.kawaii-app-footer {
  background-color: var(--footer-bg) !important;
  border-top: 1px solid var(--border-color) !important;
  color: var(--text-secondary) !important;
}

:global(.light) .kawaii-app-footer,
:global(.dark) .kawaii-app-footer {
  background-color: var(--footer-bg) !important;
  border-top: 1px solid var(--border-color) !important;
  color: var(--text-secondary) !important;
}

.kawaii-app-footer a,
.kawaii-app-footer button {
  color: var(--text-secondary) !important;
}

.kawaii-app-footer a:hover,
.kawaii-app-footer button:hover {
  color: var(--nav-active) !important;
}
</style>
