<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { SearchAddon } from '@xterm/addon-search'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebglAddon } from '@xterm/addon-webgl'
import { SerializeAddon } from '@xterm/addon-serialize'
import { ClipboardAddon } from '@xterm/addon-clipboard'
import { useAuthStore } from '@/stores/auth'
import { useTerminalStore } from '@/stores/terminal'
import TerminalSavedCommandsSidebar from '@/components/terminal/TerminalSavedCommandsSidebar.vue'
import TerminalContextMenu from '@/components/instance/TerminalContextMenu.vue'
import TerminalAccessoryBar from '@/components/instance/TerminalAccessoryBar.vue'
import FlagIcon from '@/components/FlagIcon.vue'
import {
    buildTerminalWebSocketUrl,
    createTerminalRuntime,
    disposeTerminalRuntime,
    handleTerminalSocketPayload,
    shouldRetryTerminalClose,
    TERMINAL_MAX_RECONNECT_ATTEMPTS
} from '@/lib/terminal-core'
import api from '@/api'
import type { InstanceWithDetails } from '@/types/api'
import { Button, Input, Select, Badge, PageHeader, EmptyState } from '@/components/ui'
import {
    Plus,
    Terminal as TerminalIcon,
    TerminalSquare,
    Search,
    Trash2,
    Download,
    Sliders,
    HelpCircle,
    RotateCcw,
    Maximize2,
    Minimize2,
    X,
    ChevronLeft,
    ChevronRight,
    AlertCircle,
    AlertTriangle,
    Check,
    Minus,
    Loader2
} from 'lucide-vue-next'
import '@xterm/xterm/css/xterm.css'

const MAX_TABS = 5 // 集中式终端页面允许更多标签

interface InstanceOption {
    id: number
    name: string
    status: string
    imageName: string
    hostCountryCode: string
    hostName?: string
    packageName?: string
}

interface TerminalTab {
    id: string
    instanceId: number
    instanceName: string
    label: string
    terminal: Terminal | null
    fitAddon: FitAddon | null
    searchAddon: SearchAddon | null
    unicodeAddon: Unicode11Addon | null
    webglAddon: WebglAddon | null
    serializeAddon: SerializeAddon | null
    clipboardAddon: ClipboardAddon | null
    ws: WebSocket | null
    status: 'disconnected' | 'connecting' | 'connected' | 'reconnecting' | 'error'
    connectionMode: 'exec' | 'console' | null
    error: string | null
    container: HTMLElement | null
    eventsBound: boolean
    isWebGLEnabled: boolean
    consoleWarningShown: boolean
    connectAttempt: number
}

const { t } = useI18n()
const authStore = useAuthStore()
const terminalStore = useTerminalStore()

// 实例列表
const instances = ref<InstanceOption[]>([])
const loadingInstances = ref(false)

// 标签管理
const tabs = ref<TerminalTab[]>([])
const activeTabId = ref<string>('')
const containerRefs = ref<Map<string, HTMLElement>>(new Map())

// 全屏状态
const isFullscreen = ref(false)

// 实例选择器弹窗
const showInstanceSelector = ref(false)
const selectedInstanceId = ref<number | null>(null)
const instanceSearchQuery = ref('')
const instancePage = ref(1)
const instancePageSize = 3

// 过滤后的运行中实例
const filteredRunningInstances = computed(() => {
    const query = instanceSearchQuery.value.toLowerCase().trim()
    if (!query) return runningInstances.value
    return runningInstances.value.filter(i =>
        i.name.toLowerCase().includes(query) ||
        i.imageName.toLowerCase().includes(query)
    )
})

// 分页后的实例列表
const paginatedInstances = computed(() => {
    const start = (instancePage.value - 1) * instancePageSize
    return filteredRunningInstances.value.slice(start, start + instancePageSize)
})

// 总页数
const instanceTotalPages = computed(() =>
    Math.ceil(filteredRunningInstances.value.length / instancePageSize) || 1
)

// 搜索状态
const searchInputRef = ref<HTMLInputElement | null>(null)
const showSearch = ref(false)
const searchText = ref('')

// 帮助弹窗状态
const showHelp = ref(false)

// 设置弹窗状态
const showSettings = ref(false)
const showSavedCommandsMobile = ref(false)

// 右键菜单状态
const contextMenu = reactive({
    visible: false,
    x: 0,
    y: 0
})

// 字体大小（从 store 读取，已持久化）
const fontSize = computed(() => terminalStore.fontSize)
// 动态计算字体大小选项，确保当前值始终在列表中
const fontSizeOptions = computed(() => {
    const baseOptions = [12, 14, 16, 18, 20]
    const current = fontSize.value
    if (!baseOptions.includes(current)) {
        return [...baseOptions, current].sort((a, b) => a - b)
    }
    return baseOptions
})

// 触控手势状态
const touchState = reactive({
    initialDistance: 0,
    initialFontSize: 14,
    isZooming: false
})

// 移动端检测（用于禁用 WebGL 和显示快捷工具栏）
const isMobileDevice = (() => {
    const ua = navigator.userAgent
    if (/Android|webOS|iPhone|iPad|iPod|BlackBerry|IEMobile|Opera Mini/i.test(ua)) {
        return true
    }
    if (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1) {
        return true
    }
    if (navigator.maxTouchPoints > 0 && window.innerWidth <= 1024) {
        return true
    }
    if (window.innerWidth <= 768) {
        return true
    }
    return false
})()

// 网络延迟检测
const networkLatency = ref<Map<string, number>>(new Map())

// 链接预览 tooltip
const linkTooltip = reactive({
    visible: false,
    x: 0,
    y: 0,
    url: ''
})

// Cloud-init 状态检查（每个标签独立）
const cloudInitStatus = ref<Map<string, 'idle' | 'checking' | 'not_ready' | 'ready' | 'skipped'>>(new Map())
const cloudInitChecking = ref<Map<string, boolean>>(new Map())

// 重连配置
const reconnectAttempts = ref<Map<string, number>>(new Map())
const reconnectTimers = ref<Map<string, ReturnType<typeof setTimeout>>>(new Map())

// 计算属性
const activeTab = computed(() => tabs.value.find(t => t.id === activeTabId.value))
const runningInstances = computed(() => instances.value.filter(i => i.status.toLowerCase() === 'running'))

const statusText = computed(() => {
    const tab = activeTab.value
    if (!tab) return ''
    switch (tab.status) {
        case 'connecting': return t('terminal.connecting')
        case 'connected': return t('terminal.connected')
        case 'reconnecting': return t('terminal.reconnecting')
        case 'disconnected': return t('terminal.disconnected')
        case 'error': return tab.error || t('terminal.connectionFailed')
        default: return ''
    }
})

// 加载用户实例列表
async function loadInstances() {
    loadingInstances.value = true
    try {
        const response = await api.instances.list({ limit: 100 })
        const data = response as { instances?: Array<InstanceWithDetails & { imageName?: string; hostCountryCode?: string }>; total?: number }
        instances.value = (data.instances || []).map((i) => ({
            id: i.id,
            name: i.name || `${t('instance.title')} #${i.id}`,
            status: i.status,
            imageName: i.imageName || i.image || '',
            hostCountryCode: i.hostCountryCode || 'us',
            hostName: i.host?.name,
            packageName: i.package?.name
        }))
    } catch (err) {
        console.error('Failed to load instances:', err)
    } finally {
        loadingInstances.value = false
    }
}

// 生成唯一标签 ID
function generateTabId(): string {
    return `tab_${Date.now()}_${Math.random().toString(36).substr(2, 9)}`
}

// 生成酷炫的 Incudal 欢迎信息
function printWelcomeBanner(terminal: Terminal, instanceName: string) {
    const banner = [
        '',
        '\x1b[1;38;5;39m  ◆\x1b[38;5;44m INCUDAL\x1b[0m',
        '',
        '\x1b[38;5;245m  ─────────────────────\x1b[0m',
        `\x1b[38;5;250m  ${instanceName}\x1b[0m`,
        '\x1b[38;5;245m  ─────────────────────\x1b[0m',
        '',
    ]
    banner.forEach(line => terminal.writeln(line))

    // 移动端提示使用英文键盘
    if (isMobileDevice) {
        terminal.writeln(`\x1b[38;5;214m  ⚠ ${t('terminal.mobileKeyboardHint')}\x1b[0m`)
        terminal.writeln('')
    }
}

// 重连时的提示信息
function printReconnectMessage(terminal: Terminal) {
    terminal.write('\r\n\r\n\x1b[38;5;245m─── \x1b[38;5;214mReconnecting...\x1b[38;5;245m ───\x1b[0m\r\n\r\n')
}

function getConnectionModeLabel(mode: 'exec' | 'console' | null | undefined): string {
    if (mode === 'console') return t('terminal.modeConsole')
    if (mode === 'exec') return t('terminal.modeExec')
    return t('terminal.modeUnknown')
}

function maybePrintConsoleFallbackWarning(tab: TerminalTab) {
    if (!tab.terminal || tab.connectionMode !== 'console' || tab.consoleWarningShown) return

    tab.terminal.writeln('')
    tab.terminal.writeln(`\x1b[33m⚠ ${t('terminal.consoleFallback')}\x1b[0m`)
    tab.terminal.writeln(`\x1b[90m${t('terminal.consoleFallbackHint')}\x1b[0m`)
    tab.terminal.writeln('')
    tab.consoleWarningShown = true
}

// 打开实例选择器
function openInstanceSelector() {
    if (tabs.value.length >= MAX_TABS) return
    selectedInstanceId.value = null
    instanceSearchQuery.value = ''
    instancePage.value = 1
    showInstanceSelector.value = true
}

// 实例搜索时重置分页
function handleInstanceSearch(query: string) {
    instanceSearchQuery.value = query
    instancePage.value = 1
    if (selectedInstanceId.value && !filteredRunningInstances.value.find(i => i.id === selectedInstanceId.value)) {
        selectedInstanceId.value = null
    }
}

// 确认添加实例终端
function confirmAddInstance() {
    if (!selectedInstanceId.value) return
    const instance = instances.value.find(i => i.id === selectedInstanceId.value)
    if (!instance) return

    showInstanceSelector.value = false
    addInstanceTab(instance.id, instance.name)
}

// 添加实例终端标签
function addInstanceTab(instanceId: number, instanceName: string) {
    if (tabs.value.length >= MAX_TABS) return

    const id = generateTabId()
    const tab: TerminalTab = {
        id,
        instanceId,
        instanceName,
        label: instanceName,
        terminal: null,
        fitAddon: null,
        searchAddon: null,
        unicodeAddon: null,
        webglAddon: null,
        serializeAddon: null,
        clipboardAddon: null,
        ws: null,
        status: 'disconnected',
        connectionMode: null,
        error: null,
        container: null,
        eventsBound: false,
        isWebGLEnabled: false,
        consoleWarningShown: false,
        connectAttempt: 0
    }

    tabs.value.push(tab)
    activeTabId.value = id

    nextTick(() => {
        initTabTerminal(id)
    })
}

// 初始化标签的终端
function initTabTerminal(tabId: string) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (!tab) return

    const container = containerRefs.value.get(tabId)
    if (!container) return

    tab.container = container

    const runtime = createTerminalRuntime({
        container,
        fontSize: fontSize.value,
        fontFamily: "'JetBrains Mono', 'Fira Code', 'Consolas', monospace",
        theme: terminalStore.currentTheme,
        isMobileDevice,
        enableWebgl: false,
        onBell: () => terminalStore.playBell(),
        onSelectionCopy: (selection) => {
            if (terminalStore.autoCopyOnSelect) {
                navigator.clipboard.writeText(selection).catch(() => {
                    // ignore
                })
            }
        },
        onLinkHover: (event, uri) => {
            if (terminalStore.linkPreview) {
                linkTooltip.visible = true
                linkTooltip.url = uri
                linkTooltip.x = event.clientX + 10
                linkTooltip.y = event.clientY + 10
            }
        },
        onLinkLeave: () => {
            linkTooltip.visible = false
        }
    })

    tab.terminal = runtime.terminal
    tab.fitAddon = runtime.fitAddon
    tab.searchAddon = runtime.searchAddon
    tab.unicodeAddon = runtime.unicodeAddon
    tab.webglAddon = runtime.webglAddon
    tab.serializeAddon = runtime.serializeAddon
    tab.clipboardAddon = runtime.clipboardAddon
    tab.isWebGLEnabled = runtime.isWebGLEnabled

    // 输出欢迎信息
    printWelcomeBanner(runtime.terminal, tab.instanceName)

    // 检查 Cloud-init 状态后再连接
    checkCloudInitAndConnect(tabId)
}

// 检查 Cloud-init 状态并连接
async function checkCloudInitAndConnect(tabId: string) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (!tab) return

    const status = cloudInitStatus.value.get(tabId)
    if (status === 'ready' || status === 'skipped') {
        connectTab(tabId)
        return
    }

    cloudInitStatus.value.set(tabId, 'checking')
    cloudInitChecking.value.set(tabId, true)

    try {
        const result = await api.instances.checkCloudInitStatus(tab.instanceId)
        if (result.ready) {
            cloudInitStatus.value.set(tabId, 'ready')
            connectTab(tabId)
        } else {
            cloudInitStatus.value.set(tabId, 'not_ready')
            if (tab.terminal) {
                tab.terminal.writeln('')
                tab.terminal.writeln('\x1b[33m⚠ ' + t('terminal.cloudInitInProgress') + '\x1b[0m')
                tab.terminal.writeln('\x1b[90m' + t('terminal.cloudInitInProgressHint') + '\x1b[0m')
                tab.terminal.writeln('')
            }
        }
    } catch (err) {
        console.error('[CloudInit] Check failed:', err)
        cloudInitStatus.value.set(tabId, 'ready')
        connectTab(tabId)
    } finally {
        cloudInitChecking.value.set(tabId, false)
    }
}

// 重新检查 Cloud-init 状态
async function retryCloudInitCheck(tabId: string) {
    await checkCloudInitAndConnect(tabId)
}

// 跳过 Cloud-init 检查并连接
function skipCloudInitAndConnect(tabId: string) {
    cloudInitStatus.value.set(tabId, 'skipped')
    connectTab(tabId)
}

// 连接标签到终端服务器
async function connectTab(tabId: string) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (!tab) return

    const token = authStore.token
    if (!token) {
        tab.status = 'error'
        tab.error = t('terminal.errors.authenticationRequired')
        return
    }

    if (tab.ws && (tab.ws.readyState === WebSocket.OPEN || tab.ws.readyState === WebSocket.CONNECTING)) {
        return
    }

    tab.status = 'connecting'
    tab.error = null
    tab.connectAttempt += 1
    const connectAttempt = tab.connectAttempt

    try {
        const ticketResponse = await api.instances.createTerminalTicket(tab.instanceId)
        const currentTab = tabs.value.find(t => t.id === tabId)
        if (!currentTab || currentTab.connectAttempt !== connectAttempt) {
            return
        }

        const wsUrl = buildTerminalWebSocketUrl(currentTab.instanceId, ticketResponse.ticket)
        const ws = new WebSocket(wsUrl)
        ws.binaryType = 'arraybuffer'
        currentTab.ws = ws

        ws.onopen = () => {
            console.log(`[Terminal Tab ${tabId}] WebSocket connected`)
        }

        ws.onmessage = (event) => {
            handleTerminalSocketPayload(event.data, () => tab.terminal, (message) => {
                handleControlMessage(tabId, message)
            })
        }

        ws.onerror = () => {
            currentTab.error = t('terminal.connectionError')
        }

        ws.onclose = (event) => {
            if (currentTab.ws !== ws) return

            console.log(`[Terminal Tab ${tabId}] WebSocket closed:`, event.code)

            if (shouldRetryTerminalClose(event.code) && (currentTab.status === 'connected' || currentTab.status === 'connecting' || currentTab.status === 'reconnecting')) {
                const attempts = reconnectAttempts.value.get(tabId) || 0
                if (attempts < TERMINAL_MAX_RECONNECT_ATTEMPTS) {
                    reconnectAttempts.value.set(tabId, attempts + 1)
                    currentTab.status = 'reconnecting'
                    const delay = Math.min(1000 + attempts * 2000, 15000)
                    console.log(`[Terminal Tab ${tabId}] Auto reconnecting in ${delay}ms (attempt ${attempts + 1}/${TERMINAL_MAX_RECONNECT_ATTEMPTS})`)
                    const existingTimer = reconnectTimers.value.get(tabId)
                    if (existingTimer) {
                        clearTimeout(existingTimer)
                    }
                    const timer = setTimeout(() => {
                        reconnectTimers.value.delete(tabId)
                        if (currentTab.terminal) {
                            printReconnectMessage(currentTab.terminal)
                        }
                        void connectTab(tabId)
                    }, delay)
                    reconnectTimers.value.set(tabId, timer)
                } else {
                    currentTab.status = 'error'
                    currentTab.error = t('terminal.errors.connectionLost')
                    reconnectAttempts.value.delete(tabId)
                }
            } else {
                currentTab.status = 'disconnected'
            }
        }

        if (currentTab.terminal && !currentTab.eventsBound) {
            currentTab.eventsBound = true

            currentTab.terminal.onData((data) => {
                if (currentTab.ws && currentTab.ws.readyState === WebSocket.OPEN) {
                    try {
                        currentTab.ws.send(data)
                    } catch {
                        // ignore
                    }
                }
            })

            currentTab.terminal.onResize(({ cols, rows }) => {
                if (tabs.value.find(t => t.id === tabId)) {
                    sendResize(tabId, cols, rows)
                }
            })
        }
    } catch {
        const currentTab = tabs.value.find(t => t.id === tabId)
        if (!currentTab || currentTab.connectAttempt !== connectAttempt) {
            return
        }

        currentTab.status = 'error'
        currentTab.error = t('terminal.connectionFailed')
    }
}

// 带重连提示的连接函数
function reconnectTab(tabId: string) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (!tab) return

    if (tab.status === 'connecting' || tab.status === 'reconnecting') return

    if (tab.terminal) {
        printReconnectMessage(tab.terminal)
    }

    void connectTab(tabId)
}

// 处理控制消息
function handleControlMessage(tabId: string, message: { type: string; code?: string; sessionId?: string; message?: string; reason?: string; mode?: 'exec' | 'console' }) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (!tab) return

    switch (message.type) {
        case 'connected':
            tab.status = 'connected'
            tab.error = null
            if (message.mode) {
                tab.connectionMode = message.mode
                if (message.mode !== 'console') {
                    tab.consoleWarningShown = false
                }
            }
            reconnectAttempts.value.delete(tabId)
            if (tab.terminal && tab.fitAddon) {
                tab.fitAddon.fit()
                if (tab.terminal) {
                    sendResize(tabId, tab.terminal.cols, tab.terminal.rows)
                }
            }
            maybePrintConsoleFallbackWarning(tab)
            break
        case 'reconnecting':
            tab.status = 'reconnecting'
            tab.error = message.reason || null
            if (message.mode) {
                tab.connectionMode = message.mode
            }
            break
        case 'disconnected':
            tab.status = 'disconnected'
            tab.error = message.reason || null
            break
        case 'error':
            tab.status = 'error'
            tab.error = terminalControlError(message.code)
            if (tab.terminal) {
                tab.terminal.write(`\r\n\x1b[31m${t('terminal.errors.terminalLine', { error: tab.error })}\x1b[0m\r\n`)
            }
            break
    }
}

function terminalControlError(code?: string): string {
    const knownCodes: Record<string, string> = {
        INVALID_ID: 'invalidId',
        ORIGIN_NOT_ALLOWED: 'originNotAllowed',
        IP_LIMIT_EXCEEDED: 'ipLimitExceeded',
        UNAUTHORIZED: 'authenticationRequired',
        SESSION_INVALIDATED: 'sessionInvalidated',
        USER_LIMIT_EXCEEDED: 'userLimitExceeded',
        WS_LIMIT_EXCEEDED: 'connectionLimitExceeded',
        INSTANCE_NOT_FOUND: 'instanceNotFound',
        FORBIDDEN: 'forbidden',
        INSTANCE_NOT_RUNNING: 'instanceNotRunning',
        INSTANCE_LIMIT_EXCEEDED: 'instanceLimitExceeded',
        HOST_NOT_FOUND: 'hostNotFound',
        CONNECTION_FAILED: 'connectionFailed'
    }
    const key = code ? knownCodes[code] : undefined
    return t(`terminal.errors.${key || 'unknown'}`)
}

// 发送调整大小命令
function sendResize(tabId: string, cols: number, rows: number) {
    const tab = tabs.value.find(t => t.id === tabId)
    if (tab?.ws && tab.ws.readyState === WebSocket.OPEN) {
        try {
            tab.ws.send(JSON.stringify({ type: 'resize', cols, rows }))
        } catch {
            // ignore
        }
    }
}

// 关闭标签
function closeTab(tabId: string) {
    const index = tabs.value.findIndex(t => t.id === tabId)
    if (index === -1) return

    const tab = tabs.value[index]

    const timer = reconnectTimers.value.get(tabId)
    if (timer) {
        clearTimeout(timer)
        reconnectTimers.value.delete(tabId)
    }
    reconnectAttempts.value.delete(tabId)

    if (tab.ws) {
        tab.ws.close(1000, 'Tab closed')
        tab.ws = null
    }
    disposeTerminalRuntime(tab)

    tabs.value.splice(index, 1)
    containerRefs.value.delete(tabId)

    cloudInitStatus.value.delete(tabId)
    cloudInitChecking.value.delete(tabId)

    if (activeTabId.value === tabId && tabs.value.length > 0) {
        activeTabId.value = tabs.value[Math.min(index, tabs.value.length - 1)].id
        nextTick(() => {
            const newTab = activeTab.value
            if (newTab?.terminal && newTab.fitAddon) {
                newTab.fitAddon.fit()
                newTab.terminal.refresh(0, newTab.terminal.rows - 1)
                newTab.terminal.focus()
            }
        })
    }
}

// 切换标签
function switchTab(tabId: string) {
    if (activeTabId.value === tabId) return
    activeTabId.value = tabId

    nextTick(() => {
        const tab = activeTab.value
        if (tab?.terminal && tab.fitAddon) {
            tab.fitAddon.fit()
            tab.terminal.refresh(0, tab.terminal.rows - 1)
            tab.terminal.focus()
        }
    })
}

// 全屏切换
function toggleFullscreen() {
    isFullscreen.value = !isFullscreen.value
    nextTick(() => {
        const tab = activeTab.value
        if (tab?.fitAddon && tab.terminal) {
            tab.fitAddon.fit()
            tab.terminal.refresh(0, tab.terminal.rows - 1)
        }
    })
}

// 清空终端
function clearTerminal() {
    const tab = activeTab.value
    if (tab?.terminal) {
        tab.terminal.clear()
    }
}

// 处理右键菜单
function handleContextMenu(event: MouseEvent) {
    event.preventDefault()
    contextMenu.x = event.clientX
    contextMenu.y = event.clientY
    contextMenu.visible = true
}

function closeContextMenu() {
    contextMenu.visible = false
}

// 复制粘贴
async function handleCopy() {
    const tab = activeTab.value
    if (!tab?.terminal) {
        closeContextMenu()
        return
    }

    const selection = tab.terminal.getSelection()
    if (!selection) {
        closeContextMenu()
        return
    }

    try {
        await navigator.clipboard.writeText(selection)
    } catch {
        const textarea = document.createElement('textarea')
        textarea.value = selection
        textarea.style.position = 'fixed'
        textarea.style.opacity = '0'
        document.body.appendChild(textarea)
        textarea.select()
        document.execCommand('copy')
        document.body.removeChild(textarea)
    }
    closeContextMenu()
}

async function handlePaste() {
    const tab = activeTab.value
    if (!tab?.ws || tab.ws.readyState !== WebSocket.OPEN) {
        closeContextMenu()
        return
    }

    try {
        const text = await navigator.clipboard.readText()
        if (text) {
            try {
                tab.ws.send(text)
            } catch {
                // ignore
            }
        }
    } catch {
        // ignore
    }
    closeContextMenu()
}

function sendToActiveTerminal(data: string): boolean {
    const tab = activeTab.value
    if (!tab?.ws || tab.ws.readyState !== WebSocket.OPEN) return false

    try {
        tab.ws.send(data)
        return true
    } catch {
        return false
    }
}

// 移动端快捷工具栏 - 发送数据
function handleAccessorySend(data: string) {
    sendToActiveTerminal(data)
}

// 移动端快捷工具栏 - 粘贴
async function handleAccessoryPaste() {
    const tab = activeTab.value
    if (!tab?.ws || tab.ws.readyState !== WebSocket.OPEN) return

    try {
        const text = await navigator.clipboard.readText()
        if (text) {
            try {
                tab.ws.send(text)
            } catch {
                // ignore
            }
        }
    } catch {
        // ignore
    }
}

function executeSavedCommand(command: string) {
    const payload = command.endsWith('\n') || command.endsWith('\r')
        ? command
        : `${command}\r`
    sendToActiveTerminal(payload)
}

// 移动端快捷工具栏 - 收起键盘
function handleHideKeyboard() {
    if (document.activeElement instanceof HTMLElement) {
        document.activeElement.blur()
    }
    setTimeout(() => {
        activeTab.value?.terminal?.focus()
    }, 100)
}

function handleSelectAll() {
    const tab = activeTab.value
    if (tab?.terminal) {
        tab.terminal.selectAll()
    }
    closeContextMenu()
}

// 搜索功能
function openSearch() {
    showSearch.value = true
    nextTick(() => searchInputRef.value?.focus())
}

function closeSearch() {
    showSearch.value = false
    searchText.value = ''
}

function handleSearch() {
    const tab = activeTab.value
    if (tab?.searchAddon && searchText.value) {
        tab.searchAddon.findNext(searchText.value)
    }
}

function handleSearchPrevious() {
    const tab = activeTab.value
    if (tab?.searchAddon && searchText.value) {
        tab.searchAddon.findPrevious(searchText.value)
    }
}

// 去除 ANSI 转义序列
function stripAnsiSequences(text: string): string {
    const ansiRegex = new RegExp(String.fromCharCode(27) + '\\[[0-9;]*[a-zA-Z]', 'g')
    return text.replace(ansiRegex, '')
}

// 导出终端日志
function exportTerminalLog() {
    const tab = activeTab.value
    if (!tab?.serializeAddon) return

    try {
        const raw = tab.serializeAddon.serialize()
        if (!raw) return

        const content = stripAnsiSequences(raw)
        const blob = new Blob([content], { type: 'text/plain;charset=utf-8' })
        const url = URL.createObjectURL(blob)
        const a = document.createElement('a')
        a.href = url
        a.download = `terminal-${tab.instanceName}-${Date.now()}.log`
        document.body.appendChild(a)
        a.click()
        document.body.removeChild(a)
        URL.revokeObjectURL(url)
    } catch (err) {
        console.error('[Terminal] Failed to export log:', err)
    }
}

// 增大字体
function increaseFontSize() {
    const newSize = Math.min(fontSize.value + 2, 32)
    updateFontSize(newSize)
}

// 减小字体
function decreaseFontSize() {
    const newSize = Math.max(fontSize.value - 2, 8)
    updateFontSize(newSize)
}

// 重置字体大小
function resetFontSize() {
    updateFontSize(14)
}

// 更新字体大小
function updateFontSize(size: number) {
    terminalStore.setFontSize(size)
    tabs.value.forEach(tab => {
        if (tab.terminal) {
            tab.terminal.options.fontSize = size
            tab.fitAddon?.fit()
        }
    })
}

// 切换终端主题
function handleThemeChangeValue(value: string) {
    const theme = value as 'dark' | 'light' | 'highContrast'
    terminalStore.setTheme(theme)
    tabs.value.forEach(tab => {
        if (tab.terminal) {
            tab.terminal.options.theme = terminalStore.currentTheme
        }
    })
}

function handleThemeChange(event: Event) {
    const value = (event.target as HTMLSelectElement).value
    handleThemeChangeValue(value)
}

// 键盘快捷键
function handleKeydown(event: KeyboardEvent) {
    if (event.ctrlKey && event.shiftKey && event.key === 'F') {
        event.preventDefault()
        if (showSearch.value) {
            closeSearch()
        } else {
            openSearch()
        }
        return
    }
    if (event.ctrlKey && event.shiftKey && event.key.toLowerCase() === 'c') {
        event.preventDefault()
        handleCopy()
        return
    }
    if (event.ctrlKey && event.shiftKey && event.key.toLowerCase() === 'v') {
        event.preventDefault()
        handlePaste()
        return
    }
    if (event.ctrlKey && (event.key === '=' || event.key === '+')) {
        event.preventDefault()
        increaseFontSize()
        return
    }
    if (event.ctrlKey && event.key === '-') {
        event.preventDefault()
        decreaseFontSize()
        return
    }
    if (event.ctrlKey && event.key === '0') {
        event.preventDefault()
        resetFontSize()
        return
    }
    if (event.ctrlKey && event.shiftKey && event.key.toLowerCase() === 's') {
        event.preventDefault()
        exportTerminalLog()
        return
    }
    if (event.key === 'Escape') {
        if (contextMenu.visible) {
            closeContextMenu()
            return
        }
        if (showSearch.value) {
            closeSearch()
            return
        }
        if (isFullscreen.value) {
            toggleFullscreen()
            return
        }
        if (showSettings.value) {
            showSettings.value = false
            return
        }
        if (showHelp.value) {
            showHelp.value = false
            return
        }
    }
}

// 触控手势处理
function handleTouchStart(event: TouchEvent) {
    if (!terminalStore.touchEnabled) return
    if (event.touches.length === 2) {
        event.preventDefault()
        touchState.isZooming = true
        touchState.initialDistance = getTouchDistance(event.touches)
        touchState.initialFontSize = terminalStore.fontSize
    }
}

function handleTouchMove(event: TouchEvent) {
    if (!terminalStore.touchEnabled || !touchState.isZooming) return
    if (event.touches.length === 2) {
        event.preventDefault()
        const currentDistance = getTouchDistance(event.touches)
        if (touchState.initialDistance <= 0) return
        const scale = currentDistance / touchState.initialDistance
        const newSize = Math.round(touchState.initialFontSize * scale)
        const clampedSize = Math.max(8, Math.min(32, newSize))
        if (clampedSize !== terminalStore.fontSize) {
            updateFontSize(clampedSize)
        }
    }
}

function handleTouchEnd() {
    touchState.isZooming = false
}

function getTouchDistance(touches: TouchList): number {
    const dx = touches[0].clientX - touches[1].clientX
    const dy = touches[0].clientY - touches[1].clientY
    return Math.sqrt(dx * dx + dy * dy)
}

// 窗口大小变化
function handleResize() {
    const tab = activeTab.value
    if (tab?.fitAddon) {
        tab.fitAddon.fit()
    }
}

// 设置容器引用
function setContainerRef(tabId: string, el: HTMLElement | null) {
    if (el) {
        containerRefs.value.set(tabId, el)
    }
}

// 断开所有连接
function disconnectAll() {
    showSavedCommandsMobile.value = false
    reconnectTimers.value.forEach(timer => clearTimeout(timer))
    reconnectTimers.value.clear()
    reconnectAttempts.value.clear()

    tabs.value.forEach(tab => {
        if (tab.ws) {
            tab.ws.close(1000, 'User disconnect')
            tab.ws = null
        }
        disposeTerminalRuntime(tab)
    })
    tabs.value = []
    containerRefs.value.clear()

    cloudInitStatus.value.clear()
    cloudInitChecking.value.clear()
}

onMounted(() => {
    loadInstances()
    window.addEventListener('resize', handleResize)
    window.addEventListener('keydown', handleKeydown)
    document.addEventListener('visibilitychange', handleVisibilityChange)

    if (isMobileDevice && window.visualViewport) {
        window.visualViewport.addEventListener('resize', handleViewportResize)
    }
})

onUnmounted(() => {
    window.removeEventListener('resize', handleResize)
    window.removeEventListener('keydown', handleKeydown)
    document.removeEventListener('visibilitychange', handleVisibilityChange)

    if (isMobileDevice && window.visualViewport) {
        window.visualViewport.removeEventListener('resize', handleViewportResize)
    }

    disconnectAll()
})

// 移动端 visualViewport 变化处理
function handleViewportResize() {
    setTimeout(() => {
        const tab = activeTab.value
        if (tab?.fitAddon && tab.terminal) {
            tab.fitAddon.fit()
            tab.terminal.refresh(0, tab.terminal.rows - 1)
        }
    }, 100)
}

// 页面可见性变化时自动重连
function handleVisibilityChange() {
    if (document.visibilityState === 'visible') {
        tabs.value.forEach(tab => {
            if (tab.status === 'disconnected' || tab.status === 'error') {
                console.log(`[Terminal Tab ${tab.id}] Page visible, auto reconnecting...`)
                reconnectTab(tab.id)
            }
        })
    }
}
</script>

<template>
  <div class="space-y-6">
    <!-- Apple Page Header -->
    <PageHeader
      :title="t('nav.terminal')"
      :description="t('terminalPage.description')"
    >
      <template #actions>
        <Button
          v-if="tabs.length < MAX_TABS"
          variant="primary"
          size="md"
          @click="openInstanceSelector"
        >
          <template #prefix>
            <Plus class="w-4 h-4 stroke-[2]" />
          </template>
          {{ t('terminalPage.newConnection') }}
        </Button>
        <Button
          v-if="isMobileDevice"
          variant="secondary"
          size="md"
          @click="showSavedCommandsMobile = true"
        >
          <template #prefix>
            <TerminalSquare class="w-4 h-4 stroke-[2]" />
          </template>
          {{ t('terminal.savedCommands.title') }}
        </Button>
      </template>
    </PageHeader>

    <!-- macOS Apple Style Terminal Window Chrome -->
    <div
      class="terminal-window group/terminal-workspace relative flex flex-col transition-all duration-300"
      :class="[
        isFullscreen
          ? 'fixed inset-0 z-50 rounded-none border-0 bg-[var(--terminal-bg)] shadow-none overflow-hidden'
          : 'rounded-2xl border border-[var(--border-color)] bg-[var(--terminal-bg)] shadow-[0_4px_16px_rgba(0,0,0,0.08)] overflow-hidden'
      ]"
    >
      <!-- macOS Window Titlebar -->
      <div
        class="flex items-center justify-between gap-3 border-b border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2.5 sm:px-4 select-none shrink-0"
      >
        <!-- Left: Traffic Light Aesthetic & Tab Bar -->
        <div class="flex items-center gap-3.5 flex-1 min-w-0">
          <!-- Apple Traffic Lights -->
          <div class="flex items-center gap-1.5 shrink-0 pl-0.5">
            <!-- Red: Close Tab -->
            <button
              type="button"
              class="group/dot flex h-3 w-3 items-center justify-center rounded-full bg-rose-500/90 border border-rose-600/30 transition-all hover:bg-rose-500 active:scale-90"
              :class="{ 'opacity-40 cursor-not-allowed': tabs.length === 0 }"
              :title="activeTabId ? t('terminal.clear') : ''"
              @click="activeTabId && closeTab(activeTabId)"
            >
              <X class="h-2 w-2 stroke-[2.5] text-rose-950 opacity-0 group-hover/dot:opacity-100 transition-opacity" />
            </button>
            <!-- Yellow: Clear Terminal -->
            <button
              type="button"
              class="group/dot flex h-3 w-3 items-center justify-center rounded-full bg-amber-400/90 border border-amber-500/30 transition-all hover:bg-amber-400 active:scale-90"
              :class="{ 'opacity-40 cursor-not-allowed': tabs.length === 0 }"
              :title="t('terminal.clear')"
              @click="clearTerminal"
            >
              <Minus class="h-2 w-2 stroke-[2.5] text-amber-950 opacity-0 group-hover/dot:opacity-100 transition-opacity" />
            </button>
            <!-- Green: Fullscreen Toggle -->
            <button
              type="button"
              class="group/dot flex h-3 w-3 items-center justify-center rounded-full bg-emerald-500/90 border border-emerald-600/30 transition-all hover:bg-emerald-500 active:scale-90"
              :title="isFullscreen ? 'Exit Fullscreen' : 'Fullscreen'"
              @click="toggleFullscreen"
            >
              <Maximize2 class="h-2 w-2 stroke-[2.5] text-emerald-950 opacity-0 group-hover/dot:opacity-100 transition-opacity" />
            </button>
          </div>

          <!-- Tab Bar -->
          <div class="flex items-center gap-1.5 overflow-x-auto min-w-0 flex-1 py-0.5 scrollbar-none">
            <template v-if="tabs.length > 0">
              <button
                v-for="tab in tabs"
                :key="tab.id"
                type="button"
                class="group flex items-center gap-2 whitespace-nowrap rounded-xl px-3 py-1.5 text-xs font-mono font-medium transition-all duration-200 active:scale-[0.98]"
                :class="activeTabId === tab.id
                  ? 'bg-[var(--bg-surface-soft)] text-[var(--text-primary)] shadow-sm border border-[var(--border-color)]'
                  : 'text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)]/60 hover:text-[var(--text-primary)]'"
                @click="switchTab(tab.id)"
              >
                <!-- Status Dot -->
                <span
                  class="h-1.5 w-1.5 shrink-0 rounded-full"
                  :class="{
                    'bg-emerald-500': tab.status === 'connected',
                    'bg-amber-500 animate-pulse': tab.status === 'connecting' || tab.status === 'reconnecting',
                    'bg-rose-500': tab.status === 'error',
                    'bg-neutral-400': tab.status === 'disconnected'
                  }"
                />
                <span class="max-w-[130px] truncate">{{ tab.label }}</span>
                <!-- Close Button -->
                <button
                  type="button"
                  class="ml-0.5 rounded-full p-0.5 text-[var(--text-tertiary)] hover:bg-[var(--bg-tertiary)] hover:text-[var(--text-primary)] opacity-0 group-hover:opacity-100 transition-opacity"
                  @click.stop="closeTab(tab.id)"
                >
                  <X class="h-3 w-3 stroke-[2]" />
                </button>
              </button>

              <!-- New Tab Button -->
              <button
                v-if="tabs.length < MAX_TABS"
                type="button"
                class="flex h-7 w-7 items-center justify-center rounded-lg text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95 shrink-0"
                :title="t('terminalPage.newConnection')"
                @click="openInstanceSelector"
              >
                <Plus class="h-4 w-4 stroke-[2]" />
              </button>
            </template>
            <template v-else>
              <span class="text-xs font-mono text-[var(--text-tertiary)] pl-1">
                incudal &mdash; terminal
              </span>
            </template>
          </div>
        </div>

        <!-- Right: Toolbar Controls -->
        <div class="flex items-center gap-1.5 shrink-0">
          <!-- Active Status Badge -->
          <Badge
            v-if="activeTab"
            :variant="activeTab.status === 'connected' ? 'success' : (activeTab.status === 'connecting' || activeTab.status === 'reconnecting') ? 'warning' : activeTab.status === 'error' ? 'error' : 'default'"
            size="sm"
            dot
            :pulse="activeTab.status === 'connecting' || activeTab.status === 'reconnecting'"
            class="hidden sm:inline-flex"
          >
            {{ statusText }}
          </Badge>

          <!-- Connection Mode Badge -->
          <Badge
            v-if="activeTab?.connectionMode"
            variant="default"
            size="sm"
            class="hidden sm:inline-flex font-mono"
          >
            {{ getConnectionModeLabel(activeTab.connectionMode) }}
          </Badge>

          <!-- Font Size Selector -->
          <select
            :value="fontSize"
            class="hidden sm:block h-7 rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 font-mono text-xs text-[var(--text-secondary)] hover:border-[var(--border-hover)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)] transition-all cursor-pointer"
            @change="updateFontSize(Number(($event.target as HTMLSelectElement).value))"
          >
            <option v-for="size in fontSizeOptions" :key="size" :value="size">{{ size }}px</option>
          </select>

          <!-- Search Button -->
          <button
            v-if="tabs.length > 0"
            type="button"
            class="rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.helpShortcutSearch')"
            @click="showSearch = !showSearch; if (showSearch) nextTick(() => searchInputRef?.focus())"
          >
            <Search class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Clear Button -->
          <button
            v-if="tabs.length > 0"
            type="button"
            class="rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.clear')"
            @click="clearTerminal"
          >
            <Trash2 class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Export Log Button -->
          <button
            v-if="tabs.length > 0"
            type="button"
            class="hidden sm:block rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.exportLog')"
            @click="exportTerminalLog"
          >
            <Download class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Settings Button -->
          <button
            type="button"
            class="rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.settings')"
            @click="showSettings = true"
          >
            <Sliders class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Help Button -->
          <button
            type="button"
            class="hidden sm:block rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.help')"
            @click="showHelp = true"
          >
            <HelpCircle class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Fullscreen Toggle Button -->
          <button
            type="button"
            class="rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="isFullscreen ? 'Exit Fullscreen' : 'Fullscreen'"
            @click="toggleFullscreen"
          >
            <Minimize2 v-if="isFullscreen" class="h-4 w-4 stroke-[1.8]" />
            <Maximize2 v-else class="h-4 w-4 stroke-[1.8]" />
          </button>

          <!-- Reconnect Button -->
          <button
            v-if="activeTab && (activeTab.status === 'disconnected' || activeTab.status === 'error')"
            type="button"
            class="rounded-lg p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
            :title="t('terminal.reconnect')"
            @click="reconnectTab(activeTabId)"
          >
            <RotateCcw class="h-4 w-4 stroke-[1.8]" />
          </button>
        </div>
      </div>

      <!-- Terminal Area -->
      <div
        class="terminal-area relative flex overflow-hidden flex-1"
        :class="{ 'is-fullscreen': isFullscreen }"
      >
        <div class="relative min-w-0 flex-1 overflow-hidden">
          <!-- Empty State (No tabs) -->
          <div
            v-if="tabs.length === 0"
            class="absolute inset-0 flex items-center justify-center bg-[var(--terminal-bg)]"
          >
            <EmptyState
              :title="t('terminalPage.noConnections')"
              :description="t('terminalPage.selectionHint')"
            >
              <template #icon>
                <TerminalIcon class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
              </template>
              <template #action>
                <div class="flex flex-col sm:flex-row items-center gap-3">
                  <Button
                    variant="primary"
                    size="md"
                    @click="openInstanceSelector"
                  >
                    <template #prefix>
                      <Plus class="w-4 h-4 stroke-[2]" />
                    </template>
                    {{ t('terminalPage.newConnection') }}
                  </Button>
                  <Button
                    v-if="isMobileDevice"
                    variant="secondary"
                    size="md"
                    @click="showSavedCommandsMobile = true"
                  >
                    <template #prefix>
                      <TerminalSquare class="w-4 h-4 stroke-[2]" />
                    </template>
                    {{ t('terminal.savedCommands.title') }}
                  </Button>
                </div>
              </template>
            </EmptyState>
          </div>

          <!-- Search Overlay Bar -->
          <Transition name="fade">
            <div
              v-if="showSearch"
              class="absolute top-3 right-3 z-30 flex items-center gap-2 p-2 bg-[var(--bg-surface)] border border-[var(--border-color)] rounded-xl shadow-[0_8px_24px_rgba(0,0,0,0.12)] backdrop-blur-md"
            >
              <input
                ref="searchInputRef"
                v-model="searchText"
                type="text"
                class="w-48 px-3 py-1.5 text-xs bg-[var(--bg-surface-soft)] border border-[var(--border-color)] rounded-lg text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none transition-all font-mono"
                :placeholder="t('terminal.searchPlaceholder')"
                @keydown.enter.exact="handleSearch"
                @keydown.shift.enter.exact="handleSearchPrevious"
                @keydown.escape="closeSearch"
              >
              <button
                type="button"
                class="p-1.5 text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] rounded-lg transition-colors active:scale-95"
                @click="handleSearchPrevious"
              >
                <ChevronLeft class="w-4 h-4 stroke-[2]" />
              </button>
              <button
                type="button"
                class="p-1.5 text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] rounded-lg transition-colors active:scale-95"
                @click="handleSearch"
              >
                <ChevronRight class="w-4 h-4 stroke-[2]" />
              </button>
              <button
                type="button"
                class="p-1.5 text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] rounded-lg transition-colors active:scale-95"
                @click="closeSearch"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>
          </Transition>

          <!-- Terminal Container for Each Tab -->
          <div
            v-for="tab in tabs"
            :key="tab.id"
            :ref="(el) => setContainerRef(tab.id, el as HTMLElement)"
            class="terminal-container bg-[var(--terminal-bg)]"
            :class="activeTabId === tab.id ? 'visible' : 'invisible absolute'"
            @contextmenu="handleContextMenu"
            @touchstart="handleTouchStart"
            @touchmove="handleTouchMove"
            @touchend="handleTouchEnd"
          />

          <!-- Connecting / Reconnecting Overlay -->
          <Transition name="fade">
            <div
              v-if="activeTab && (activeTab.status === 'connecting' || activeTab.status === 'reconnecting')"
              class="absolute inset-0 z-20 flex items-center justify-center bg-[var(--terminal-bg)]/85 backdrop-blur-sm"
            >
              <div class="flex flex-col items-center gap-3.5 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-[0_8px_24px_rgba(0,0,0,0.12)]">
                <Loader2 class="w-8 h-8 text-[var(--accent)] animate-spin stroke-[2]" />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ statusText }}</span>
              </div>
            </div>
          </Transition>

          <!-- Cloud-init Checking Overlay -->
          <Transition name="fade">
            <div
              v-if="activeTab && cloudInitStatus.get(activeTab.id) === 'checking'"
              class="absolute inset-0 z-20 flex items-center justify-center bg-[var(--terminal-bg)]/85 backdrop-blur-sm"
            >
              <div class="flex flex-col items-center gap-3.5 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-[0_8px_24px_rgba(0,0,0,0.12)]">
                <Loader2 class="w-8 h-8 text-[var(--accent)] animate-spin stroke-[2]" />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.cloudInitChecking') }}</span>
              </div>
            </div>
          </Transition>

          <!-- Cloud-init Not Ready Overlay -->
          <Transition name="fade">
            <div
              v-if="activeTab && cloudInitStatus.get(activeTab.id) === 'not_ready'"
              class="absolute inset-0 z-20 flex items-center justify-center bg-[var(--terminal-bg)]/85 backdrop-blur-sm"
            >
              <div class="flex flex-col items-center gap-4 text-center p-6 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] max-w-md mx-4">
                <div class="w-12 h-12 rounded-2xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] flex items-center justify-center border border-[color-mix(in_srgb,var(--warning)_28%,var(--bg-surface))]">
                  <AlertTriangle class="w-6 h-6 stroke-[2]" />
                </div>
                <div>
                  <p class="font-semibold text-[var(--text-primary)] text-base tracking-tight">{{ t('terminal.cloudInitInProgress') }}</p>
                  <p class="text-sm text-[var(--text-secondary)] mt-1.5 leading-relaxed">{{ t('terminal.cloudInitInProgressHint') }}</p>
                </div>
                <div class="flex flex-wrap gap-3 justify-center w-full mt-1">
                  <Button
                    variant="primary"
                    size="sm"
                    :loading="cloudInitChecking.get(activeTab.id)"
                    @click="retryCloudInitCheck(activeTab.id)"
                  >
                    {{ t('terminal.cloudInitRetry') }}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    @click="skipCloudInitAndConnect(activeTab.id)"
                  >
                    {{ t('terminal.cloudInitSkip') }}
                  </Button>
                </div>
              </div>
            </div>
          </Transition>

          <!-- Connection Error Overlay -->
          <Transition name="fade">
            <div
              v-if="activeTab && activeTab.status === 'error'"
              class="absolute inset-0 z-20 flex items-center justify-center bg-[var(--terminal-bg)]/85 backdrop-blur-sm"
            >
              <div class="flex flex-col items-center gap-4 text-center p-6 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_8px_24px_rgba(0,0,0,0.12)] max-w-md mx-4">
                <div class="w-12 h-12 rounded-2xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] text-[var(--error)] flex items-center justify-center border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))]">
                  <AlertCircle class="w-6 h-6 stroke-[2]" />
                </div>
                <div>
                  <p class="font-semibold text-[var(--text-primary)] text-base tracking-tight">{{ t('terminal.connectionFailed') }}</p>
                  <p class="text-sm text-[var(--text-secondary)] mt-1.5 leading-relaxed">{{ activeTab.error }}</p>
                </div>
                <Button
                  variant="primary"
                  size="sm"
                  @click="reconnectTab(activeTabId)"
                >
                  <template #prefix>
                    <RotateCcw class="w-4 h-4 stroke-[2]" />
                  </template>
                  {{ t('terminal.reconnect') }}
                </Button>
              </div>
            </div>
          </Transition>
        </div>

        <!-- Saved Commands Sidebar (Desktop) -->
        <TerminalSavedCommandsSidebar
          v-if="!isMobileDevice"
          :connected="activeTab?.status === 'connected'"
          @execute="executeSavedCommand"
        />
      </div>

      <!-- Mobile Accessory Bar -->
      <TerminalAccessoryBar
        v-if="isMobileDevice && tabs.length > 0"
        :connected="activeTab?.status === 'connected'"
        @send="handleAccessorySend"
        @paste="handleAccessoryPaste"
        @hide-keyboard="handleHideKeyboard"
        @saved-commands="showSavedCommandsMobile = true"
      />

      <!-- Mobile Saved Commands Sidebar -->
      <TerminalSavedCommandsSidebar
        v-if="isMobileDevice"
        mobile
        :visible="showSavedCommandsMobile"
        :connected="activeTab?.status === 'connected'"
        @execute="executeSavedCommand"
        @close="showSavedCommandsMobile = false"
      />
    </div>

    <!-- Terminal Context Menu -->
    <TerminalContextMenu
      :visible="contextMenu.visible"
      :x="contextMenu.x"
      :y="contextMenu.y"
      :has-selection="!!activeTab?.terminal?.hasSelection()"
      :can-paste="activeTab?.status === 'connected'"
      @close="closeContextMenu"
      @copy="handleCopy"
      @paste="handlePaste"
      @select-all="handleSelectAll"
      @clear="clearTerminal"
    />

    <!-- Instance Selector DrawerModal -->
    <DrawerModal
      :show="showInstanceSelector"
      max-width="max-w-xl sm:max-w-2xl"
      raw
      @close="showInstanceSelector = false"
    >
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-5 bg-[var(--bg-surface)]">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_28%,var(--bg-surface))]">
            <TerminalIcon class="h-5 w-5 stroke-[2]" />
          </div>
          <div>
            <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">{{ t('terminalPage.selectInstance') }}</h3>
            <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminalPage.selectionHint') }}</p>
          </div>
        </div>
        <button
          type="button"
          class="flex h-8 w-8 items-center justify-center rounded-full bg-[var(--bg-surface-soft)] text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)] transition-all active:scale-95"
          aria-label="close"
          @click="showInstanceSelector = false"
        >
          <X class="w-4 h-4 stroke-[2]" />
        </button>
      </div>

      <!-- Search & Filter Status Bar -->
      <div class="border-b border-[var(--border-color)] bg-[var(--bg-surface-soft)]/50 px-6 py-3.5 shrink-0">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <Badge variant="success" size="sm" dot>
            {{ t('terminalPage.runningCount', { count: runningInstances.length }) }}
          </Badge>
          <div class="w-full sm:max-w-xs">
            <Input
              :model-value="instanceSearchQuery"
              :placeholder="t('terminalPage.searchInstances')"
              clearable
              size="sm"
              @update:model-value="handleInstanceSearch(String($event))"
              @clear="handleInstanceSearch('')"
            >
              <template #prefix>
                <Search class="w-3.5 h-3.5 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>
        </div>
      </div>

      <!-- Content Area: Instances List -->
      <div class="min-h-0 flex-1 overflow-y-auto px-6 py-5">
        <div v-if="loadingInstances" class="flex items-center justify-center py-20">
          <Loader2 class="h-8 w-8 animate-spin text-[var(--accent)] stroke-[2]" />
        </div>
        <div v-else-if="runningInstances.length === 0" class="py-12">
          <EmptyState
            :title="t('terminalPage.noRunningInstances')"
            :description="t('terminalPage.noRunningInstancesHint')"
          >
            <template #icon>
              <TerminalIcon class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
            </template>
          </EmptyState>
        </div>
        <div v-else-if="filteredRunningInstances.length === 0" class="py-12">
          <EmptyState
            :title="t('terminalPage.noMatchingInstances')"
            :description="t('terminalPage.noMatchingInstancesHint')"
          >
            <template #icon>
              <Search class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
            </template>
          </EmptyState>
        </div>
        <div v-else class="space-y-3">
          <button
            v-for="instance in paginatedInstances"
            :key="instance.id"
            type="button"
            class="group w-full rounded-2xl border p-4 text-left transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98]"
            :class="selectedInstanceId === instance.id
              ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))] ring-1 ring-[var(--accent)] shadow-sm'
              : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)] shadow-[0_2px_8px_rgba(0,0,0,0.04)]'"
            @click="selectedInstanceId = instance.id"
          >
            <div class="flex items-start gap-3.5 sm:gap-4">
              <!-- Flag container -->
              <div class="relative flex-shrink-0">
                <div class="flex h-11 w-11 items-center justify-center rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] sm:h-12 sm:w-12">
                  <FlagIcon :code="instance.hostCountryCode" class="h-4 w-6 sm:h-4.5 sm:w-7" />
                </div>
                <span
                  class="absolute -bottom-0.5 -right-0.5 h-3 w-3 rounded-full border-2 border-[var(--bg-surface)] bg-emerald-500"
                />
              </div>

              <!-- Content -->
              <div class="min-w-0 flex-1">
                <div class="flex items-start justify-between gap-2">
                  <div class="min-w-0">
                    <div class="flex items-center gap-2">
                      <span class="truncate text-sm font-semibold tracking-tight text-[var(--text-primary)]">{{ instance.name }}</span>
                      <Badge variant="default" size="sm" shape="pill" class="font-mono">
                        #{{ instance.id }}
                      </Badge>
                    </div>
                    <div class="mt-0.5 truncate font-mono text-xs text-[var(--text-secondary)]">{{ instance.imageName }}</div>
                  </div>

                  <!-- Radio Selection Indicator -->
                  <div
                    class="mt-0.5 flex h-5 w-5 flex-shrink-0 items-center justify-center rounded-full border transition-all duration-200"
                    :class="selectedInstanceId === instance.id
                      ? 'border-[var(--accent)] bg-[var(--accent)] text-white shadow-sm'
                      : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-transparent'"
                  >
                    <Check class="h-3 w-3 stroke-[3]" />
                  </div>
                </div>

                <!-- Tags / Metadata -->
                <div class="mt-3 flex flex-wrap items-center gap-1.5 sm:gap-2">
                  <Badge v-if="instance.hostName" variant="default" size="sm">
                    {{ t('terminalPage.host') }}: {{ instance.hostName }}
                  </Badge>
                  <Badge v-if="instance.packageName" variant="default" size="sm">
                    {{ t('terminalPage.package') }}: {{ instance.packageName }}
                  </Badge>
                  <Badge variant="success" size="sm" dot>
                    {{ t('terminalPage.statusRunning') }}
                  </Badge>
                </div>
              </div>
            </div>
          </button>
        </div>

        <!-- Pagination Controls -->
        <div
          v-if="filteredRunningInstances.length > instancePageSize"
          class="mt-4 flex items-center justify-between rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-4 py-2.5"
        >
          <span class="font-mono text-xs text-[var(--text-secondary)]">
            {{ (instancePage - 1) * instancePageSize + 1 }}-{{ Math.min(instancePage * instancePageSize, filteredRunningInstances.length) }} / {{ filteredRunningInstances.length }}
          </span>
          <div class="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              :disabled="instancePage <= 1"
              @click="instancePage--"
            >
              <template #prefix>
                <ChevronLeft class="w-3.5 h-3.5 stroke-[2]" />
              </template>
              {{ t('common.prevPage') }}
            </Button>
            <span class="font-mono text-xs font-medium text-[var(--text-primary)] px-1">{{ instancePage }} / {{ instanceTotalPages }}</span>
            <Button
              variant="secondary"
              size="sm"
              :disabled="instancePage >= instanceTotalPages"
              @click="instancePage++"
            >
              {{ t('common.nextPage') }}
              <template #suffix>
                <ChevronRight class="w-3.5 h-3.5 stroke-[2]" />
              </template>
            </Button>
          </div>
        </div>
      </div>

      <!-- Footer -->
      <div class="flex shrink-0 flex-col sm:flex-row gap-3 border-t border-[var(--border-color)] px-6 py-4 sm:items-center sm:justify-between bg-[var(--bg-surface)]">
        <div class="text-xs text-[var(--text-secondary)] hidden sm:block">
          {{ t('terminalPage.runningCount', { count: filteredRunningInstances.length }) }}
        </div>
        <div class="flex items-center gap-3 w-full sm:w-auto sm:justify-end">
          <Button
            type="button"
            variant="secondary"
            size="md"
            class="flex-1 sm:flex-none"
            @click="showInstanceSelector = false"
          >
            {{ t('common.cancel') }}
          </Button>
          <Button
            type="button"
            variant="primary"
            size="md"
            class="flex-1 sm:flex-none"
            :disabled="!selectedInstanceId"
            @click="confirmAddInstance"
          >
            <template #prefix>
              <TerminalIcon class="w-4 h-4 stroke-[2]" />
            </template>
            {{ t('terminalPage.connect') }}
          </Button>
        </div>
      </div>
    </DrawerModal>

    <!-- Help DrawerModal -->
    <DrawerModal
      :show="showHelp"
      :title="t('terminal.helpTitle')"
      max-width="max-w-md"
      @close="showHelp = false"
    >
      <!-- Shortcuts -->
      <div class="mb-6">
        <h4 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">{{ t('terminal.helpShortcuts') }}</h4>
        <div class="space-y-2 text-sm">
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutSearch') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+Shift+F</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutCopy') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+Shift+C</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutPaste') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+Shift+V</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutFontIncrease') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl++</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutFontDecrease') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+-</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutFontReset') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+0</kbd>
          </div>
          <div class="flex items-center justify-between py-1.5 border-b border-[var(--border-color)]/40">
            <span class="text-[var(--text-secondary)]">{{ t('terminal.helpShortcutExport') }}</span>
            <kbd class="rounded-lg border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-2 py-0.5 font-mono text-xs text-[var(--text-primary)] shadow-sm">Ctrl+Shift+S</kbd>
          </div>
        </div>
      </div>

      <!-- Mouse Operations -->
      <div class="mb-6">
        <h4 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">{{ t('terminal.helpMouseOps') }}</h4>
        <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/60 p-4 space-y-2 text-sm text-[var(--text-secondary)]">
          <div>&bull; {{ t('terminal.helpMouseSelect') }}</div>
          <div>&bull; {{ t('terminal.helpMouseCopy') }}</div>
          <div>&bull; {{ t('terminal.helpMouseScroll') }}</div>
        </div>
      </div>

      <!-- Touch Operations -->
      <div>
        <h4 class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] mb-3">{{ t('terminal.helpTouchOps') }}</h4>
        <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]/60 p-4 space-y-2 text-sm text-[var(--text-secondary)]">
          <div>&bull; {{ t('terminal.helpTouchPinchZoom') }}</div>
          <div>&bull; {{ t('terminal.helpTouchSwipeScroll') }}</div>
        </div>
      </div>
    </DrawerModal>

    <!-- Link Preview Tooltip -->
    <Teleport to="body">
      <Transition name="fade">
        <div
          v-if="linkTooltip.visible"
          class="fixed z-[9999] px-3 py-1.5 text-xs bg-[var(--bg-surface)] text-[var(--text-primary)] border border-[var(--border-color)] rounded-xl shadow-[0_4px_16px_rgba(0,0,0,0.12)] max-w-xs truncate pointer-events-none font-mono"
          :style="{ left: linkTooltip.x + 'px', top: linkTooltip.y + 'px' }"
        >
          {{ linkTooltip.url }}
        </div>
      </Transition>
    </Teleport>

    <!-- Settings Modal -->
    <Teleport to="body">
      <Transition name="fade">
        <div
          v-if="showSettings"
          class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 backdrop-blur-md"
          @click.self="showSettings = false"
        >
          <div class="w-full max-w-md rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 shadow-[0_12px_32px_rgba(0,0,0,0.12)] space-y-5 animate-in fade-in duration-200">
            <div class="flex items-center justify-between border-b border-[var(--border-color)] pb-4">
              <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">{{ t('terminal.settings') }}</h3>
              <button
                type="button"
                class="rounded-full p-1.5 text-[var(--text-secondary)] hover:bg-[var(--bg-surface-soft)] hover:text-[var(--text-primary)] transition-all active:scale-95"
                @click="showSettings = false"
              >
                <X class="w-4 h-4 stroke-[2]" />
              </button>
            </div>

            <div class="space-y-4">
              <!-- Bell Setting -->
              <div class="flex items-center justify-between">
                <div>
                  <div class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.settingBell') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminal.settingBellDesc') }}</div>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="terminalStore.bellEnabled"
                  class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-300 ease-in-out focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-95"
                  :class="terminalStore.bellEnabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-surface-soft)] border border-[var(--border-color)]'"
                  @click="terminalStore.toggleBell()"
                >
                  <span
                    class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-md transition duration-300 ease-in-out"
                    :class="terminalStore.bellEnabled ? 'translate-x-5' : 'translate-x-0'"
                  />
                </button>
              </div>

              <!-- Auto Copy Setting -->
              <div class="flex items-center justify-between">
                <div>
                  <div class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.settingAutoCopy') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminal.settingAutoCopyDesc') }}</div>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="terminalStore.autoCopyOnSelect"
                  class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-300 ease-in-out focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-95"
                  :class="terminalStore.autoCopyOnSelect ? 'bg-[var(--accent)]' : 'bg-[var(--bg-surface-soft)] border border-[var(--border-color)]'"
                  @click="terminalStore.toggleAutoCopy()"
                >
                  <span
                    class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-md transition duration-300 ease-in-out"
                    :class="terminalStore.autoCopyOnSelect ? 'translate-x-5' : 'translate-x-0'"
                  />
                </button>
              </div>

              <!-- Link Preview Setting -->
              <div class="flex items-center justify-between">
                <div>
                  <div class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.settingLinkPreview') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminal.settingLinkPreviewDesc') }}</div>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="terminalStore.linkPreview"
                  class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-300 ease-in-out focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-95"
                  :class="terminalStore.linkPreview ? 'bg-[var(--accent)]' : 'bg-[var(--bg-surface-soft)] border border-[var(--border-color)]'"
                  @click="terminalStore.linkPreview = !terminalStore.linkPreview"
                >
                  <span
                    class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-md transition duration-300 ease-in-out"
                    :class="terminalStore.linkPreview ? 'translate-x-5' : 'translate-x-0'"
                  />
                </button>
              </div>

              <!-- Touch Optimization Setting -->
              <div class="flex items-center justify-between">
                <div>
                  <div class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.settingTouch') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminal.settingTouchDesc') }}</div>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="terminalStore.touchEnabled"
                  class="relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-300 ease-in-out focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-95"
                  :class="terminalStore.touchEnabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-surface-soft)] border border-[var(--border-color)]'"
                  @click="terminalStore.toggleTouch()"
                >
                  <span
                    class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-md transition duration-300 ease-in-out"
                    :class="terminalStore.touchEnabled ? 'translate-x-5' : 'translate-x-0'"
                  />
                </button>
              </div>

              <!-- Terminal Theme Setting -->
              <div class="flex items-center justify-between">
                <div>
                  <div class="text-sm font-medium text-[var(--text-primary)]">{{ t('terminal.settingTheme') }}</div>
                  <div class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('terminal.settingThemeDesc') }}</div>
                </div>
                <div class="w-40">
                  <Select
                    :model-value="terminalStore.theme"
                    :options="[
                      { label: t('terminal.themeDark'), value: 'dark' },
                      { label: t('terminal.themeLight'), value: 'light' },
                      { label: t('terminal.themeHighContrast'), value: 'highContrast' }
                    ]"
                    size="sm"
                    @update:model-value="handleThemeChangeValue(String($event))"
                    @change="handleThemeChange"
                  />
                </div>
              </div>

              <!-- Status Footer -->
              <div class="pt-4 border-t border-[var(--border-color)]">
                <div class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] mb-2">{{ t('terminal.currentStatus') }}</div>
                <div class="flex items-center gap-4 text-xs text-[var(--text-primary)]">
                  <Badge
                    :variant="activeTab?.isWebGLEnabled ? 'success' : 'warning'"
                    size="sm"
                    dot
                  >
                    {{ activeTab?.isWebGLEnabled ? 'WebGL' : 'Canvas' }}
                  </Badge>
                  <span v-if="networkLatency.get(activeTabId)" class="font-mono text-[var(--text-secondary)]">
                    {{ t('terminal.latency') }}: {{ networkLatency.get(activeTabId) }}ms
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.15s ease;
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

.terminal-area {
  background-color: var(--terminal-bg);
  height: calc(100vh - 220px);
  height: calc(100dvh - 220px);
  min-height: 320px;
}

@media (min-width: 640px) {
  .terminal-area {
    height: calc(100vh - 280px);
    height: calc(100dvh - 280px);
    min-height: 420px;
  }
}

.terminal-area.is-fullscreen {
  height: calc(100vh - 45px) !important;
  height: calc(100dvh - 45px) !important;
}

.terminal-container {
  position: absolute;
  inset: 0;
}

.terminal-container.visible {
  z-index: 1;
}

.terminal-container.invisible {
  z-index: 0;
  pointer-events: none;
}

:deep(.xterm) {
  height: 100%;
  padding: 0;
}

:deep(.xterm-viewport) {
  overflow-y: auto !important;
  background-color: var(--terminal-bg) !important;
}

:deep(.xterm-screen) {
  height: 100%;
}

:deep(.xterm-cursor-layer) {
  z-index: 10;
}

:deep(.xterm-cursor) {
  opacity: 1 !important;
}

@media (hover: none) and (pointer: coarse) {
  :deep(.xterm) {
    touch-action: manipulation;
    -webkit-touch-callout: none;
  }

  :deep(.xterm-viewport) {
    -webkit-overflow-scrolling: touch;
  }

  :deep(.xterm-cursor) {
    box-shadow: 0 0 2px 1px rgba(255, 255, 255, 0.5);
  }
}
</style>
