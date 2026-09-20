/**
 * Frontend color token guard
 * --------------------------
 * 钉死「主题颜色统一管理、禁止组件指定颜色」这个验收线：
 *  - client/src/** 下的 .vue 组件不得散落十六进制色值；
 *  - 全局样式（main.css / kawaii-cloud.css）不得写死 hex（变量定义已收敛到 theme.css）；
 *  - theme.css 必须保持完整（关键 token 明暗两套都在，防止 AI 重构误删）；
 *  - tailwind.config.js 的 primary 静态 ramp 必须与 theme.css 的 --accent 同步。
 *
 * 允许 hex 的白名单（集中管理或豁免区域）：
 *  - client/src/styles/theme.css      —— 唯一色彩 Token 来源
 *  - client/src/styles/markdown-theme.css —— 内容渲染（GitHub 官方 Markdown 高亮）
 *  - client/src/theme/**               —— 集中色板常量（chart / menu-search）
 *  - 娱乐/游戏化模块                    —— AGENTS.md 明确「刻意保留」
 *  - 第三方品牌符号色（Google/GitHub OAuth SVG、帮助中心分类色等）
 *  - MenuSearchBox.vue                 —— 他人施工中（色调板已抽为 menu-search-palette.ts）
 *  - VipLevelRulesEditor.vue           —— placeholder 是用户可输入的 hex 示例文本（非样式色）
 *
 * 运行：pnpm --filter server test:frontend-color-tokens
 */
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, extname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')
const clientSrc = resolve(repoRoot, 'client/src')
const themeSource = readFileSync(resolve(clientSrc, 'styles/theme.css'), 'utf8')
const tailwindConfigSource = readFileSync(resolve(repoRoot, 'client/tailwind.config.js'), 'utf8')

// 正则：3/4/6/8 位 hex（#abc / #abcd / #a1b2c3 / #a1b2c3d4），排除 #{ 插值
const HEX_RE = /#[\da-fA-F]{3,8}\b/g

// 豁免白名单（相对 client/src 的路径，支持目录前缀）
const EXEMPT_PATHS = [
  'styles/theme.css',
  'styles/markdown-theme.css',
  'theme/', // 集中色板常量目录
  // 娱乐/游戏化模块（AGENTS.md 刻意保留）
  'views/EntertainmentView.vue',
  'components/CheckinModal.vue',
  'components/BadgeRewardModal.vue',
  'components/instance/InstanceBadgeModal.vue', // 若存在
  // 第三方品牌符号色所在文件
  'components/profile/OAuthSection.vue',
  'views/LoginView.vue', // Google/GitHub OAuth 品牌色
  'views/admin/AdminLoginView.vue',
  'views/admin/OAuthConfigView.vue',
  // 帮助中心分类色（后端下发数据）
  'views/HelpView.vue',
  'views/admin/HelpManageView.vue',
  // 他人施工中（色调板已抽为集中常量）
  'components/layout/MenuSearchBox.vue',
  // placeholder 为 hex 示例文本（非样式色）
  'components/admin/VipLevelRulesEditor.vue',
  // 内容渲染集中定义（markdown 渲染器注入的 alert/代码高亮色板，与 markdown-theme.css 同源）
  'utils/markdown.ts',
  // 终端模拟器标准色板（ANSI/xterm 16 色，数据型集中定义）
  'composables/useTerminal.ts',
  'stores/terminal.ts',
  // VIP 徽章稀有度色板（游戏化/稀有度，AGENTS.md 例外；集中定义）
  'utils/vipBadge.ts',
  // kawaii-cloud.css 为渐进改造区：hero/装饰类保留自身配色（变量定义已 token 化）
  'styles/kawaii-cloud.css',
  // stores/theme.ts 的 meta theme-color 兜底色值（与 theme.css 的 --bg-primary 同值）
  'stores/theme.ts',
  // Turnstile 校验状态字色（第三方组件状态色）
  'components/TurnstileWidget.vue',
  // 公告铃铛多色渐变图标（品牌装饰图标）
  'components/icons/AnnouncementIcon.vue',
  // 用户头像装饰条纹底（品牌占位装饰）
  'components/layout/AppLayout.vue',
  // Google Material Design 3 完整色板（第三方设计语言系统，集中定义）
  'composables/useM3Auth.ts',
]

// theme.css 必须存在的关键 token（:root 暗色 + .light 亮色两套都要有）。
// 防止 AI / 重构过程误删核心色板导致主题缺色。
const REQUIRED_TOKENS = [
  '--bg-primary',
  '--bg-secondary',
  '--bg-tertiary',
  '--bg-elevated',
  '--bg-surface',
  '--bg-surface-soft',
  '--sidebar-bg',
  '--border-color',
  '--border-hover',
  '--border-strong',
  '--text-primary',
  '--text-secondary',
  '--text-tertiary',
  '--accent',
  '--accent-strong',
  '--nav-active',
  '--topbar-bg',
  '--topbar-text',
  '--topbar-border',
  '--footer-bg',
  '--terminal-bg',
  '--success',
  '--warning',
  '--error',
  '--success-soft',
  '--chart-1',
  '--chart-2',
  '--chart-3',
  '--chart-4',
  '--radius-card',
  '--radius-modal',
  '--radius-btn',
]

// 明暗两套都校验的 token（其余允许只用一套，如状态色）
const DUAL_THEME_TOKENS: string[] = [
  '--bg-primary',
  '--bg-surface',
  '--bg-surface-soft',
  '--sidebar-bg',
  '--border-color',
  '--text-primary',
  '--text-secondary',
  '--accent',
  '--nav-active',
  '--topbar-bg',
  '--topbar-text',
  '--footer-bg',
  '--success',
  '--warning',
  '--error',
  '--chart-1',
  '--chart-2',
  '--chart-3',
  '--chart-4',
]

function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    const st = statSync(full)
    if (st.isDirectory()) out.push(...walk(full))
    else if (extname(full) === '.vue' || extname(full) === '.css' || extname(full) === '.ts') out.push(full)
  }
  return out
}

function isExempt(rel: string): boolean {
  return EXEMPT_PATHS.some((p) => rel === p || rel.startsWith(p))
}

const violations: string[] = []

for (const file of walk(clientSrc)) {
  const rel = relative(clientSrc, file)
  if (isExempt(rel)) continue
  const source = readFileSync(file, 'utf8')
  const matches = source.match(HEX_RE)
  if (matches) {
    const unique = [...new Set(matches)].join(', ')
    violations.push(`${rel}: ${unique}`)
  }
}

assert.ok(
  violations.length === 0,
  `组件/全局样式中发现硬编码十六进制色（应改用 theme.css 的 token 或集中色板）：\n${violations.join('\n')}`,
)

// ---- theme.css 完整性：关键 token 必须存在 ----
const missingTokens = REQUIRED_TOKENS.filter((tok) => !themeSource.includes(`${tok}:`))
assert.ok(
  missingTokens.length === 0,
  `theme.css 缺少关键 token（主题色板不完整）：\n${missingTokens.join('\n')}\n请补回 client/src/styles/theme.css`,
)

// ---- theme.css 明暗双套：核心 token 在 :root 与 .light 都要有 ----
// 按块提取（避免头部注释里的 ".light" 字样干扰 indexOf）
function blockOf(selector: string): string {
  const start = themeSource.indexOf(`${selector} {`)
  assert.notEqual(start, -1, `theme.css 缺少 ${selector} 块`)
  const open = start + themeSource.slice(start).indexOf('{')
  const close = themeSource.indexOf('}', open)
  assert.notEqual(close, -1, `theme.css ${selector} 块未闭合`)
  return themeSource.slice(open, close)
}
const darkBlock = blockOf(':root')
const lightBlock = blockOf('.light')
const missingDual = DUAL_THEME_TOKENS.filter(
  (tok) => !darkBlock.includes(`${tok}:`) || !lightBlock.includes(`${tok}:`),
)
assert.ok(
  missingDual.length === 0,
  `theme.css 中以下 token 未同时定义在 :root 与 .light（明暗双主题缺色）：\n${missingDual.join('\n')}`,
)

// ---- tailwind.config.js 与 theme.css 同步：primary 静态 ramp 500 对齐 --accent 浅色 ----
// 取 .light 块内的 --accent（浅色主题主色），tailwind primary-500 同源。
function hexFromVarIn(block: string, token: string): string {
  const re = new RegExp(`${token}\\s*:\\s*(#[\\da-fA-F]{6})\\b`)
  const m = block.match(re)
  return m ? m[1].toLowerCase() : ''
}

const lightBlock3 = blockOf('.light')
const accentLight = hexFromVarIn(lightBlock3, '--accent')
// 取 ociBlueRamp 常量块内的 500 级（tailwind primary 映射到该 ramp）
const rampStart = tailwindConfigSource.indexOf('const ociBlueRamp')
assert.notEqual(rampStart, -1, 'tailwind.config.js 缺少 ociBlueRamp 常量')
const rampBlock = tailwindConfigSource.slice(rampStart, tailwindConfigSource.indexOf('}\n', rampStart))
const rampMatch = rampBlock.match(/500:\s*'(#[0-9a-fA-F]{6})'/)
assert.ok(
  rampMatch !== null,
  'tailwind.config.js 里 primary ramp 缺少 500 级色值',
)
const ramp500 = rampMatch ? rampMatch[1].toLowerCase() : ''
const rampCommentSynced = tailwindConfigSource.includes('theme.css')
assert.ok(
  ramp500 === accentLight && rampCommentSynced,
  `tailwind.config.js 的 primary-500（#${ramp500}）与 theme.css 的 .light --accent（#${accentLight}）不同步，或缺少"与 theme.css 同源"注释。两处必须一致（单一来源）。`,
)

console.log('frontend color token guard passed')
