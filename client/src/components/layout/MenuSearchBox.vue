<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useConfigStore } from '@/stores/config'
import { useAuthStore } from '@/stores/auth'
import { isAdminEntry, navMenuItems, type MenuItem } from '@/config/side-nav-items'

const { t } = useI18n()
const router = useRouter()
const configStore = useConfigStore()
const authStore = useAuthStore()

const query = ref('')
const isOpen = ref(false)
const selectedIndex = ref(0)
const inputRef = ref<HTMLInputElement | null>(null)
const mobileInputRef = ref<HTMLInputElement | null>(null)
const containerRef = ref<HTMLElement | null>(null)
const mobileModalOpen = ref(false)

// 针对不同平台的快捷键提示
const isMac = typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform)
const shortcutKey = isMac ? '⌘K' : 'Ctrl+K'

// 标签映射与翻译
const navLabelFallbacks: Record<string, string> = {
  'nav.billing': '计费',
  'nav.main': '常用',
  'nav.resources': '资源',
  'nav.support': '支持',
  'nav.system': '系统',
  'nav.admin': '管理',
  'nav.operations': '运维',
  'nav.expand': '扩展'
}

function getNavLabel(item: MenuItem): string {
  if (item.labelText) return item.labelText
  if (!item.label) return ''
  const translated = t(item.label)
  if (translated && translated !== item.label) return translated
  if (navLabelFallbacks[item.label]) return navLabelFallbacks[item.label]
  return item.label.split('.').pop() || item.label
}

// 关键词同义词映射
const menuKeywords: Record<string, string[]> = {
  dashboard: ['概览', '仪表盘', '主页', 'home', 'overview', 'dashboard', 'usercenter', '用户中心'],
  instances: ['实例', '服务器', '虚拟机', 'vps', 'vm', 'server', 'kvm', 'lxc', 'instance', 'instances'],
  'instance-create': ['创建实例', '购买', '开通', '新建', 'buy', 'create', 'deploy', 'order'],
  terminal: ['终端', 'ssh', '命令行', 'console', 'shell', 'webssh', 'terminal'],
  mail: ['邮箱', '域名邮箱', '邮件', 'email', 'mail', 'postfix'],
  wallet: ['钱包', '余额', '充值', '提现', 'balance', 'wallet', 'recharge', 'pay', 'money'],
  orders: ['订单', '账单', '消费记录', 'order', 'orders', 'invoice', 'bill'],
  'gift-cards': ['礼品卡', '兑换码', '卡券', 'gift', 'coupon', 'voucher', 'redeem'],
  invites: ['邀请', '推广', '返利', '佣金', 'invite', 'invites', 'referral', 'affiliate'],
  friends: ['好友', '共享', '协同', 'friend', 'friends', 'share'],
  transfers: ['转移', '过户', 'push', 'transfer', 'transfers'],
  tickets: ['工单', '客服', '帮助', '支持', 'ticket', 'tickets', 'support'],
  help: ['帮助', '文档', '指南', 'faq', 'doc', 'docs', 'manual', 'help'],
  logs: ['日志', '操作记录', '审计', 'logs', 'audit', 'history'],
  'my-hosts': ['我的节点', '宿主机', '服务器', 'host', 'hosts', 'node', 'nodes'],
  'my-packages': ['我的套餐', '套餐', '规格', 'package', 'packages', 'plan', 'plans'],
  'hosting-wallet': ['托管收益', '收益', '提现', 'earnings', 'revenue', 'hosting'],
  extensions: ['扩展', '插件', '应用', 'extension', 'extensions', 'plugin', 'addon'],
  entertainment: ['福利', '签到', '抽奖', '活动', 'welfare', 'checkin', 'lottery', 'bonus'],
  profile: ['个人设置', '账号', '修改密码', '2fa', 'settings', 'account', 'password', 'security', 'profile'],
  // 管理端菜单关键词
  'admin-users': ['用户', '用户管理', '封禁', '解封', 'users', 'accounts', 'ban', 'admin'],
  'admin-instances': ['实例', '实例管理', '所有实例', 'instances', 'vps', 'servers'],
  'admin-hosting': ['托管', '节点托管', '资源', 'hosting'],
  'admin-settings': ['系统设置', '参数配置', '系统参数', 'settings', 'system', 'config'],
  'admin-my-hosts': ['节点管理', '宿主机', 'hosts', 'nodes'],
  'admin-my-packages': ['套餐管理', '套餐配置', 'packages', 'plans'],
  'admin-images': ['镜像管理', '系统镜像', 'os', 'images', 'templates'],
  'admin-instance-create': ['创建实例', '管理员创建', 'create'],
  'admin-mail': ['邮箱管理', '域名邮箱', 'mail'],
  'admin-billing': ['计费管理', '网关', '支付通道', 'billing', 'gateway', 'payment'],
  'admin-gift-cards': ['礼品卡管理', '生成卡密', 'gift', 'cards'],
  'admin-orders': ['订单管理', '所有订单', 'orders'],
  'admin-system-update': ['系统更新', '在线升级', 'ota', 'update', 'upgrade'],
  'admin-tickets': ['工单管理', '所有工单', 'tickets'],
  'admin-broadcast': ['系统公告', '发布公告', 'broadcast', 'announcement'],
  'admin-logs': ['系统日志', '操作日志', 'audit', 'logs'],
  'admin-help': ['帮助文档管理', '文档中心', 'help', 'docs'],
  'admin-entertainment': ['福利管理', '抽奖配置', 'entertainment'],
  'admin-profile': ['管理员设置', '账号设置', 'admin', 'profile'],
  'admin-statistics': ['系统统计', '业务监控', '数据看板', 'statistics', 'metrics', 'analytics'],
  'admin-oauth': ['OAuth配置', '第三方登录', 'oauth', 'sso']
}

interface SearchableItem {
  name: string
  path: string
  icon: string
  title: string
  groupName: string
  keywords: string[]
}

const hiddenExpandMenuNames = new Set(['my-hosts', 'my-packages', 'hosting-wallet'])
const hiddenWhenTicketDisabledMenuNames = new Set(['tickets'])
const hiddenWhenMailUnavailableMenuNames = new Set(['mail'])

const searchableList = computed<SearchableItem[]>(() => {
  const result: SearchableItem[] = []
  let currentGroup = ''
  const allItems = [...navMenuItems]

  for (const item of allItems) {
    if (item.divider) {
      currentGroup = getNavLabel(item)
      continue
    }

    if (!item.name || !item.path) continue

    // 权限与功能开关过滤
    if (!isAdminEntry && !configStore.ticketEnabled && hiddenWhenTicketDisabledMenuNames.has(item.name)) {
      continue
    }
    if (!isAdminEntry && !configStore.mailAvailable && hiddenWhenMailUnavailableMenuNames.has(item.name)) {
      continue
    }
    if (!isAdminEntry && !authStore.isAdmin && authStore.user?.canAccessHostingFeature === false && hiddenExpandMenuNames.has(item.name)) {
      continue
    }

    const title = getNavLabel(item)
    const keywords = menuKeywords[item.name] || []

    result.push({
      name: item.name,
      path: item.path,
      icon: item.icon || 'home',
      title,
      groupName: currentGroup || (isAdminEntry ? '管理' : '常用'),
      keywords
    })
  }

  return result
})

// 根据搜索词过滤
const filteredResults = computed<SearchableItem[]>(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) {
    return searchableList.value
  }

  return searchableList.value.filter(item => {
    if (item.title.toLowerCase().includes(q)) return true
    if (item.path.toLowerCase().includes(q)) return true
    if (item.groupName.toLowerCase().includes(q)) return true
    return item.keywords.some(k => k.toLowerCase().includes(q))
  })
})

// 重置选中项
watch(filteredResults, () => {
  selectedIndex.value = 0
})

function openSearch() {
  isOpen.value = true
  selectedIndex.value = 0
  nextTick(() => {
    inputRef.value?.focus()
  })
}

function closeSearch() {
  isOpen.value = false
  mobileModalOpen.value = false
  query.value = ''
}

function handleSelect(item: SearchableItem) {
  closeSearch()
  router.push(item.path)
}

function handleKeyDown(e: KeyboardEvent) {
  if (!isOpen.value) {
    if (e.key === 'ArrowDown' || e.key === 'Enter') {
      openSearch()
      e.preventDefault()
    }
    return
  }

  const items = filteredResults.value
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    if (items.length > 0) {
      selectedIndex.value = (selectedIndex.value + 1) % items.length
      scrollToSelected()
    }
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    if (items.length > 0) {
      selectedIndex.value = (selectedIndex.value - 1 + items.length) % items.length
      scrollToSelected()
    }
  } else if (e.key === 'Enter') {
    e.preventDefault()
    if (items[selectedIndex.value]) {
      handleSelect(items[selectedIndex.value])
    }
  } else if (e.key === 'Escape') {
    e.preventDefault()
    closeSearch()
    inputRef.value?.blur()
  }
}

function scrollToSelected() {
  nextTick(() => {
    const el = document.querySelector('.search-result-item.is-selected')
    if (el) {
      el.scrollIntoView({ block: 'nearest' })
    }
  })
}

// 移动端搜索弹层控制
function openMobileSearch() {
  mobileModalOpen.value = true
  isOpen.value = true
  nextTick(() => {
    mobileInputRef.value?.focus()
  })
}

// 点击外部关闭
function handleClickOutside(e: MouseEvent) {
  if (containerRef.value && !containerRef.value.contains(e.target as Node)) {
    closeSearch()
  }
}

// 全局快捷键 ⌘K / Ctrl+K
function handleGlobalKeyDown(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault()
    if (window.innerWidth < 768) {
      openMobileSearch()
    } else {
      openSearch()
    }
  }
}

onMounted(() => {
  document.addEventListener('click', handleClickOutside)
  window.addEventListener('keydown', handleGlobalKeyDown)
})

onUnmounted(() => {
  document.removeEventListener('click', handleClickOutside)
  window.removeEventListener('keydown', handleGlobalKeyDown)
})
</script>

<template>
  <div ref="containerRef" class="relative w-full">
    <!-- 桌面端与平板：居中、拉长、纯黑背景、随页面宽度自适应伸缩 -->
    <div class="hidden md:block relative w-full">
      <div
        class="menu-search-bar relative flex items-center h-[33px] w-full rounded-[4px] border transition-all duration-150"
        :class="[
          isOpen
            ? 'bg-white border-white text-[#161513] ring-1 ring-[#6e6861]/30 shadow-lg'
            : 'bg-black/60 border-[#383531]'
        ]"
      >
        <!-- 放大镜图标 -->
        <span class="pl-2.5 flex items-center pointer-events-none flex-shrink-0">
          <svg
            class="w-4 h-4 transition-colors"
            :class="isOpen ? 'text-[#161513]' : 'text-[#8a857e]'"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M21 21l-5.197-5.197m0 0A7.5 7.5 0 105.196 5.196a7.5 7.5 0 0010.607 10.607z" />
          </svg>
        </span>

        <!-- 搜索输入框 (纯黑背景，无默认灰色背景与边框) -->
        <input
          ref="inputRef"
          v-model="query"
          type="text"
          class="menu-search-input flex-1 h-full px-2.5 text-[13px] outline-none transition-colors"
          :class="isOpen
            ? 'text-[#161513] caret-black placeholder-[#161513]/50'
            : 'text-white caret-white placeholder-white/50 bg-transparent'"
          :placeholder="t('nav.searchPlaceholder')"
          autocomplete="off"
          spellcheck="false"
          @focus="isOpen = true"
          @keydown="handleKeyDown"
        />      

        <!-- 快捷键徽标 -->
        <div class="pr-2 flex items-center pointer-events-none select-none flex-shrink-0">
          <kbd
            class="px-1.5 py-0.5 text-[10px] font-mono rounded transition-colors"
            :class="isOpen
              ? 'bg-[#f0eeec] text-[#161513] border border-[#d8d5d2]'
              : 'bg-[#1c1a18] text-[#9c968e] border border-[#33302c]'"
          >
            {{ shortcutKey }}
          </kbd>
        </div>
      </div>

      <!-- 下拉菜单面板 (居中且贴合黑色操作框宽度) -->
      <transition
        enter-active-class="transition duration-100 ease-out"
        enter-from-class="opacity-0 scale-98"
        enter-to-class="opacity-100 scale-100"
        leave-active-class="transition duration-75 ease-in"
        leave-from-class="opacity-100 scale-100"
        leave-to-class="opacity-0 scale-98"
      >
        <div
          v-if="isOpen"
          class="absolute left-0 right-0 top-full mt-1.5 w-full bg-[#141210] border border-[#33302d] shadow-2xl rounded-[4px] z-50 overflow-hidden flex flex-col"
        >
          <!-- 结果统计栏 -->
          <div class="px-3 py-2 border-b border-[#262421] flex items-center justify-between text-[11px] text-[#918b83] bg-[#0d0c0b]">
            <span>{{ query ? t('nav.searchPlaceholder') : t('nav.allMenus') }}</span>
            <span>{{ filteredResults.length }} 项菜单</span>
          </div>

          <!-- 菜单列表滚动区 -->
          <div class="max-h-[360px] overflow-y-auto py-1 divide-y divide-transparent">
            <template v-if="filteredResults.length > 0">
              <button
                v-for="(item, idx) in filteredResults"
                :key="item.path"
                type="button"
                class="search-result-item w-full px-3 py-2 flex items-center gap-2.5 text-left transition-colors cursor-pointer"
                :class="[
                  idx === selectedIndex
                    ? 'bg-[#262320] text-white is-selected'
                    : 'text-[#cfcac2] hover:bg-[#1b1917]'
                ]"
                @click="handleSelect(item)"
                @mouseenter="selectedIndex = idx"
              >
                <!-- 菜单图标 -->
                <span
                  class="w-7 h-7 rounded flex items-center justify-center flex-shrink-0 transition-colors"
                  :class="[
                    idx === selectedIndex
                      ? 'bg-[#383430] text-white'
                      : 'bg-[#1f1c1a] text-[#8f8a83]'
                  ]"
                >
                  <svg class="w-4 h-4" fill="none" stroke="currentColor" stroke-width="1.8" viewBox="0 0 24 24">
                    <path v-if="item.icon === 'home'" stroke-linecap="round" stroke-linejoin="round" d="M2.25 12l8.954-8.955c.44-.439 1.152-.439 1.591 0L21.75 12M4.5 9.75v10.125c0 .621.504 1.125 1.125 1.125H9.75v-4.875c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125V21h4.125c.621 0 1.125-.504 1.125-1.125V9.75M8.25 21h8.25" />
                    <path v-else-if="item.icon === 'server'" stroke-linecap="round" stroke-linejoin="round" d="M5.25 14.25h13.5m-13.5 0a3 3 0 01-3-3m3 3a3 3 0 100 6h13.5a3 3 0 100-6m-16.5-3a3 3 0 013-3h13.5a3 3 0 013 3m-19.5 0a4.5 4.5 0 01.9-2.7L5.737 5.1a3.375 3.375 0 012.7-1.35h7.126c1.062 0 2.062.5 2.7 1.35l2.587 3.45a4.5 4.5 0 01.9 2.7m0 0a3 3 0 01-3 3m0 3h.008v.008h-.008v-.008zm0-6h.008v.008h-.008v-.008zm-3 6h.008v.008h-.008v-.008zm0-6h.008v.008h-.008v-.008z" />
                    <path v-else-if="item.icon === 'feather'" stroke-linecap="round" stroke-linejoin="round" d="M12 4.5v15m7.5-7.5h-15" />
                    <path v-else-if="item.icon === 'terminal'" stroke-linecap="round" stroke-linejoin="round" d="M6.75 7.5l3 2.25-3 2.25m4.5 0h3m-9 8.25h13.5A2.25 2.25 0 0021 18V6a2.25 2.25 0 00-2.25-2.25H5.25A2.25 2.25 0 003 6v12a2.25 2.25 0 002.25 2.25z" />
                    <path v-else-if="item.icon === 'mail'" stroke-linecap="round" stroke-linejoin="round" d="M21.75 6.75v10.5a2.25 2.25 0 01-2.25 2.25h-15a2.25 2.25 0 01-2.25-2.25V6.75m19.5 0A2.25 2.25 0 0019.5 4.5h-15a2.25 2.25 0 00-2.25 2.25m19.5 0v.243a2.25 2.25 0 01-1.07 1.916l-7.5 4.615a2.25 2.25 0 01-2.36 0L3.32 8.91a2.25 2.25 0 01-1.07-1.916V6.75" />
                    <path v-else-if="item.icon === 'wallet'" stroke-linecap="round" stroke-linejoin="round" d="M21 12a2.25 2.25 0 00-2.25-2.25H15a3 3 0 11-6 0H5.25A2.25 2.25 0 003 12m18 0v6a2.25 2.25 0 01-2.25 2.25H5.25A2.25 2.25 0 013 18v-6m18 0V9M3 12V9m18 0a2.25 2.25 0 00-2.25-2.25H5.25A2.25 2.25 0 003 9m18 0V6a2.25 2.25 0 00-2.25-2.25H5.25A2.25 2.25 0 003 6v3" />
                    <path v-else-if="item.icon === 'card'" stroke-linecap="round" stroke-linejoin="round" d="M2.25 8.25h19.5M2.25 9h19.5m-16.5 5.25h6m-6 2.25h3m-6-10.5h16.5a2.25 2.25 0 012.25 2.25v10.5a2.25 2.25 0 01-2.25 2.25H4.5A2.25 2.25 0 012.25 18V6.75A2.25 2.25 0 014.5 4.5z" />
                    <path v-else-if="item.icon === 'gift'" stroke-linecap="round" stroke-linejoin="round" d="M21 11.25v8.25a1.5 1.5 0 01-1.5 1.5H4.5a1.5 1.5 0 01-1.5-1.5v-8.25M12 4.875A2.625 2.625 0 109.375 7.5H12m0-2.625V7.5m0-2.625A2.625 2.625 0 1114.625 7.5H12m0 0V21m-8.625-9.75h17.25c.621 0 1.125-.504 1.125-1.125v-1.5c0-.621-.504-1.125-1.125-1.125H3.375c-.621 0-1.125.504-1.125 1.125v1.5c0 .621.504 1.125 1.125 1.125z" />
                    <path v-else-if="item.icon === 'key'" stroke-linecap="round" stroke-linejoin="round" d="M15.75 5.25a3 3 0 013 3m3 0a6 6 0 01-7.029 5.912c-.563-.097-1.159.026-1.563.43L10.5 17.25H8.25v2.25H6v2.25H2.25v-2.818c0-.597.237-1.17.659-1.591l6.499-6.499c.404-.404.527-1 .43-1.563A6 6 0 1121.75 8.25z" />
                    <path v-else-if="item.icon === 'users'" stroke-linecap="round" stroke-linejoin="round" d="M15 19.128a9.38 9.38 0 002.625.372 9.337 9.337 0 004.121-.952 4.125 4.125 0 00-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 018.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0111.964-3.07M12 6.375a3.375 3.375 0 11-6.75 0 3.375 3.375 0 016.75 0zm8.25 2.25a2.625 2.625 0 11-5.25 0 2.625 2.625 0 015.25 0z" />
                    <path v-else-if="item.icon === 'transfer'" stroke-linecap="round" stroke-linejoin="round" d="M7.5 21L3 16.5m0 0L7.5 12M3 16.5h13.5m0-13.5L21 7.5m0 0L16.5 12M21 7.5H7.5" />
                    <path v-else-if="item.icon === 'ticket'" stroke-linecap="round" stroke-linejoin="round" d="M16.5 6v.75m0 3v.75m0 3v.75m0 3V18m-9-5.25h5.25M7.5 15h3M3.375 5.25c-.621 0-1.125.504-1.125 1.125v3.026a2.999 2.999 0 010 5.198v3.026c0 .621.504 1.125 1.125 1.125h17.25c.621 0 1.125-.504 1.125-1.125v-3.026a2.999 2.999 0 010-5.198V6.375c0-.621-.504-1.125-1.125-1.125H3.375z" />
                    <path v-else-if="item.icon === 'help'" stroke-linecap="round" stroke-linejoin="round" d="M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9 5.25h.008v.008H12v-.008z" />
                    <path v-else-if="item.icon === 'logs'" stroke-linecap="round" stroke-linejoin="round" d="M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z" />
                    <path v-else-if="item.icon === 'database'" stroke-linecap="round" stroke-linejoin="round" d="M20.25 6.375c0 2.278-3.694 4.125-8.25 4.125S3.75 8.653 3.75 6.375m16.5 0c0-2.278-3.694-4.125-8.25-4.125S3.75 4.097 3.75 6.375m16.5 0v11.25c0 2.278-3.694 4.125-8.25 4.125s-8.25-1.847-8.25-4.125V6.375m16.5 0v3.75m-16.5-3.75v3.75m16.5 0v3.75C20.25 16.153 16.556 18 12 18s-8.25-1.847-8.25-4.125v-3.75m16.5 0c0 2.278-3.694 4.125-8.25 4.125s-8.25-1.847-8.25-4.125" />
                    <path v-else-if="item.icon === 'package'" stroke-linecap="round" stroke-linejoin="round" d="M21 7.5l-9-5.25L3 7.5m18 0l-9 5.25m9-5.25v9l-9 5.25M3 7.5l9 5.25M3 7.5v9l9 5.25m0-9v9" />
                    <path v-else-if="item.icon === 'coin'" stroke-linecap="round" stroke-linejoin="round" d="M12 6v12m-3-2.818l.879.659c1.171.879 3.07.879 4.242 0 1.172-.879 1.172-2.303 0-3.182C13.536 12.219 12.768 12 12 12c-.725 0-1.45-.22-2.003-.659-1.106-.879-1.106-2.303 0-3.182s2.9-.879 4.006 0l.415.33M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                    <path v-else-if="item.icon === 'puzzle'" stroke-linecap="round" stroke-linejoin="round" d="M14.25 6.087c0-.355.186-.676.401-.959.221-.29.349-.634.349-1.003 0-1.036-1.007-1.875-2.25-1.875s-2.25.84-2.25 1.875c0 .369.128.713.349 1.003.215.283.401.604.401.959v0a.75.75 0 01-.75.75H7.5a2.25 2.25 0 00-2.25 2.25v3a.75.75 0 01-.75.75h0c-.355 0-.676-.186-.959-.401a1.802 1.802 0 00-1.003-.349c-1.036 0-1.875 1.007-1.875 2.25s.84 2.25 1.875 2.25c.369 0 .713-.128 1.003-.349.283-.215.604-.401.959-.401v0a.75.75 0 01.75.75v3a2.25 2.25 0 002.25 2.25h3a.75.75 0 01.75.75v0c0 .355-.186.676-.401.959a1.802 1.802 0 00-.349 1.003c0 1.036 1.007 1.875 2.25 1.875s2.25-.84 2.25-1.875c0-.369-.128-.713-.349-1.003a2.122 2.122 0 01-.401-.959v0a.75.75 0 01.75-.75h3a2.25 2.25 0 002.25-2.25v-3a.75.75 0 01.75-.75h0c.355 0 .676.186.959.401.29.221.634.349 1.003.349 1.036 0 1.875-1.007 1.875-2.25s-.84-2.25-1.875-2.25c-.369 0-.713.128-1.003.349a2.122 2.122 0 01-.959.401v0a.75.75 0 01-.75-.75v-3a2.25 2.25 0 00-2.25-2.25h-3a.75.75 0 01-.75-.75v0z" />
                    <path v-else-if="item.icon === 'settings' || item.icon === 'cog'" stroke-linecap="round" stroke-linejoin="round" d="M9.594 3.94c.09-.542.56-.94 1.11-.94h2.593c.55 0 1.02.398 1.11.94l.213 1.281c.063.374.313.686.645.87.074.04.147.083.22.127.324.196.72.257 1.075.124l1.217-.456a1.125 1.125 0 011.37.49l1.296 2.247a1.125 1.125 0 01-.26 1.431l-1.003.827c-.293.24-.438.613-.431.992a6.759 6.759 0 010 .255c-.007.378.138.75.43.99l1.005.828c.424.35.534.954.26 1.43l-1.298 2.247a1.125 1.125 0 01-1.369.491l-1.217-.456c-.355-.133-.75-.072-1.076.124a6.57 6.57 0 01-.22.128c-.331.183-.581.495-.644.869l-.213 1.28c-.09.543-.56.941-1.11.941h-2.594c-.55 0-1.02-.398-1.11-.94l-.213-1.281c-.062-.374-.312-.686-.644-.87a6.52 6.52 0 01-.22-.127c-.325-.196-.72-.257-1.076-.124l-1.217.456a1.125 1.125 0 01-1.369-.49l-1.297-2.247a1.125 1.125 0 01.26-1.431l1.004-.827c.292-.24.437-.613.43-.992a6.932 6.932 0 010-.255c.007-.378-.138-.75-.43-.99l-1.004-.828a1.125 1.125 0 01-.26-1.43l1.297-2.247a1.125 1.125 0 011.37-.491l1.216.456c.356.133.751.072 1.076-.124.072-.044.146-.087.22-.128.332-.183.582-.495.644-.869l.214-1.281z" />
                    <path v-else-if="item.icon === 'image'" stroke-linecap="round" stroke-linejoin="round" d="M20.25 7.5l-.625 10.632a2.25 2.25 0 01-2.247 2.118H6.622a2.25 2.25 0 01-2.247-2.118L3.75 7.5m8.25 3v6.75m0 0l-3-3m3 3l3-3M3.375 7.5h17.25c.621 0 1.125-.504 1.125-1.125v-1.5c0-.621-.504-1.125-1.125-1.125H3.375c-.621 0-1.125.504-1.125 1.125v1.5c0 .621.504 1.125 1.125 1.125z" />
                    <path v-else-if="item.icon === 'sparkles'" stroke-linecap="round" stroke-linejoin="round" d="M9.813 15.904L9 18.75l-.813-2.846a4.5 4.5 0 00-3.09-3.09L2.25 12l2.846-.813a4.5 4.5 0 003.09-3.09L9 5.25l.813 2.846a4.5 4.5 0 003.09 3.09L15.75 12l-2.846.813a4.5 4.5 0 00-3.09 3.09zM18.259 8.715L18 9.75l-.259-1.035a3.375 3.375 0 00-2.455-2.456L14.25 6l1.036-.259a3.375 3.375 0 002.455-2.456L18 2.25l.259 1.035a3.375 3.375 0 002.456 2.456L21.75 6l-1.035.259a3.375 3.375 0 00-2.456 2.456z" />
                    <path v-else-if="item.icon === 'bell'" stroke-linecap="round" stroke-linejoin="round" d="M14.857 17.082a23.848 23.848 0 005.454-1.31A8.967 8.967 0 0118 9.75v-.7V9A6 6 0 006 9v.75a8.967 8.967 0 01-2.312 6.022c1.733.64 3.56 1.085 5.455 1.31m5.714 0a24.255 24.255 0 01-5.714 0m5.714 0a3 3 0 11-5.714 0" />
                    <path v-else-if="item.icon === 'book'" stroke-linecap="round" stroke-linejoin="round" d="M12 6.042A8.967 8.967 0 006 3.75c-1.052 0-2.062.18-3 .512v14.25A8.987 8.987 0 016 18c2.305 0 4.408.867 6 2.292m0-14.25a8.966 8.966 0 016-2.292c1.052 0 2.062.18 3 .512v14.25A8.987 8.987 0 0018 18a8.967 8.967 0 00-6 2.292m0-14.25v14.25" />
                    <path v-else-if="item.icon === 'chart'" stroke-linecap="round" stroke-linejoin="round" d="M4 19h16M7 15.5V10m5 5.5V6m5 9.5v-3M6 7.5 10 10l4-5 4 2" />
                    <path v-else stroke-linecap="round" stroke-linejoin="round" d="M3.75 6.75h16.5M3.75 12h16.5m-16.5 5.25h16.5" />
                  </svg>
                </span>

                <!-- 标题与路径 -->
                <div class="flex-1 min-w-0">
                  <div class="flex items-center gap-2">
                    <span class="text-[13px] font-medium truncate">{{ item.title }}</span>
                    <span class="text-[10px] px-1.5 py-0.2 rounded bg-[#1c1a18] text-[#9c968e] font-normal flex-shrink-0">
                      {{ item.groupName }}
                    </span>
                  </div>
                  <div class="text-[11px] text-[#736e67] font-mono truncate">
                    {{ item.path }}
                  </div>
                </div>

                <!-- 选中箭头 -->
                <svg
                  v-if="idx === selectedIndex"
                  class="w-3.5 h-3.5 text-[#a8a39b] flex-shrink-0"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                >
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
                </svg>
              </button>
            </template>

            <!-- 无匹配结果 -->
            <div
              v-else
              class="px-4 py-8 text-center text-[#736e67]"
            >
              <svg class="w-8 h-8 mx-auto mb-2 text-[#47433f]" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M21 21l-5.197-5.197m0 0A7.5 7.5 0 105.196 5.196a7.5 7.5 0 0010.607 10.607z" />
              </svg>
              <div class="text-[13px] font-medium">{{ t('nav.searchNoResults') }}</div>
            </div>
          </div>

          <!-- 底部键盘快捷提示栏 -->
          <div class="px-3 py-1.5 border-t border-[#262421] text-[11px] text-[#736e67] flex items-center justify-between bg-[#0a0908]">
            <div class="flex items-center gap-2">
              <span><kbd class="font-mono">↑↓</kbd> 切换</span>
              <span><kbd class="font-mono">↵</kbd> 跳转</span>
              <span><kbd class="font-mono">Esc</kbd> 关闭</span>
            </div>
            <span class="hidden sm:inline text-[10px] text-[#5e5a54]">{{ t('nav.menuSearchHint') }}</span>
          </div>
        </div>
      </transition>
    </div>

    <!-- 小屏幕模式：页面过小时收缩为显示搜索图标按钮 -->
    <div class="md:hidden flex justify-end">
      <button
        type="button"
        class="kawaii-header-icon p-1.5 rounded transition-colors touch-target"
        :aria-label="t('nav.searchPlaceholder')"
        @click="openMobileSearch"
      >
        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.8" d="M21 21l-5.197-5.197m0 0A7.5 7.5 0 105.196 5.196a7.5 7.5 0 0010.607 10.607z" />
        </svg>
      </button>

      <!-- 小屏弹出的黑色搜索模态层 -->
      <transition
        enter-active-class="transition duration-150 ease-out"
        enter-from-class="opacity-0 translate-y-[-10px]"
        enter-to-class="opacity-100 translate-y-0"
        leave-active-class="transition duration-100 ease-in"
        leave-from-class="opacity-100 translate-y-0"
        leave-to-class="opacity-0 translate-y-[-10px]"
      >
        <div
          v-if="mobileModalOpen"
          class="fixed inset-0 z-50 bg-black/75 backdrop-blur-sm flex flex-col p-3"
          @click.self="closeSearch"
        >
          <div class="bg-[#121110] rounded-lg shadow-2xl overflow-hidden border border-[#33302c] flex flex-col max-h-[85vh]">
            <!-- 移动端顶部搜索输入 -->
            <div class="p-3 border-b border-[#262421] flex items-center gap-2 bg-black">
              <svg class="w-4 h-4 text-[#8a857e] flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-5.197-5.197m0 0A7.5 7.5 0 105.196 5.196a7.5 7.5 0 0010.607 10.607z" />
              </svg>
              <input
                ref="mobileInputRef"
                v-model="query"
                type="text"
                class="menu-search-input flex-1 h-9 outline-none text-sm text-white caret-white"
                :placeholder="t('nav.searchPlaceholder')"
                autocomplete="off"
                spellcheck="false"
                @keydown="handleKeyDown"
              />
              <button
                type="button"
                class="p-1 rounded text-[#8a857e] hover:text-white text-xs px-2"
                @click="closeSearch"
              >
                取消
              </button>
            </div>

            <!-- 移动端结果列表 -->
            <div class="flex-1 overflow-y-auto py-1">
              <template v-if="filteredResults.length > 0">
                <button
                  v-for="(item, idx) in filteredResults"
                  :key="item.path"
                  type="button"
                  class="w-full px-3.5 py-2.5 flex items-center gap-3 text-left transition-colors cursor-pointer"
                  :class="[
                    idx === selectedIndex
                      ? 'bg-[#262320] text-white'
                      : 'text-[#cfcac2] hover:bg-[#1c1917]'
                  ]"
                  @click="handleSelect(item)"
                >
                  <div class="flex-1 min-w-0">
                    <div class="flex items-center gap-2">
                      <span class="text-sm font-medium truncate">{{ item.title }}</span>
                      <span class="text-[10px] px-1.5 py-0.5 rounded bg-[#201d1b] text-[#9c968e]">
                        {{ item.groupName }}
                      </span>
                    </div>
                    <div class="text-xs text-[#736e67] font-mono truncate">
                      {{ item.path }}
                    </div>
                  </div>
                  <svg class="w-4 h-4 text-[#736e67] flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
                  </svg>
                </button>
              </template>
              <div v-else class="px-4 py-8 text-center text-[#736e67] text-sm">
                {{ t('nav.searchNoResults') }}
              </div>
            </div>
          </div>
        </div>
      </transition>
    </div>
  </div>
</template>

<style scoped>
.menu-search-bar {
  background-color: #000000 !important;
  background: #000000 !important;
}

:deep(.menu-search-input),
.menu-search-input {
  background-color: transparent !important;
  background: transparent !important;
  border: none !important;
  border-radius: 0 !important;
  box-shadow: none !important;
  outline: none !important;
  color: #ffffff !important;
}

:deep(.menu-search-input:focus),
.menu-search-input:focus {
  background-color: transparent !important;
  background: transparent !important;
  border: none !important;
  box-shadow: none !important;
  outline: none !important;
}

:deep(.menu-search-input::placeholder),
.menu-search-input::placeholder {
  color: #78736c !important;
}
</style>
