/**
 * Frontend table action guard
 * ---------------------------
 * 钉死「表格操作列只能是扁平无边框图标按钮」这条验收线（AGENTS.md §2.13 / THEME.md §5）：
 *  - `<td>` 内**禁止**出现带文字的盒式按钮（`btn` / `btn-primary` / `btn-secondary` /
 *    `btn-ghost` / `btn-danger` / `btn-warning` / `btn-outline` / `btn-sm` / `btn-xs` / `btn-lg`）。
 *    这类浅色白底描边按钮一行出现两三个就把表格压成「按钮墙」，一律改用
 *    `<IconButton>`（底层 `.icon-btn`，shadcn 风格扁平无边框 + hover 才浮出浅底）。
 *  - 每个 `<IconButton>` 必须传 `label`（同时落到 `title` hint 与 `aria-label`），
 *    禁止只有图标没有功能描述的按钮。
 *  - `IconButton.vue` 与 `.icon-btn` 样式必须保持契约（hint 双绑定、只用主题 token）。
 *
 * 注意：`<td>` 内的**行内链接/药丸/展开**等非盒式交互（如可点价格、内容展开、状态药丸）
 * 不在本守卫范围内——它们不是「白色外框大按钮」。
 *
 * 移动端卡片（非 `<td>`）与抽屉/弹窗内按钮不受此限。
 *
 * 运行：pnpm --filter server test:frontend-table-actions
 */
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, extname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')
const clientSrc = resolve(repoRoot, 'client/src')

// 盒式（带底色/描边）按钮 class 家族：表格单元格内一律禁止
const BOXED_BUTTON_CLASSES = new Set([
  'btn',
  'btn-primary',
  'btn-secondary',
  'btn-ghost',
  'btn-danger',
  'btn-warning',
  'btn-outline',
  'btn-sm',
  'btn-xs',
  'btn-lg'
])

// 待迁移清单：仍在施工/他人正在改动的文件，迁移完成后**必须**从这里删除。
// 守卫会反向校验「清单里的文件确实还在违规」，所以清单只能缩小，不会烂掉。
const PENDING_MIGRATION = [
  'views/admin/AdminMailView.vue',
  'views/admin/UsersView.vue'
]

const TD_RE = /<td\b.*?<\/td>/gs
const BUTTON_RE = /<button\b[^>]*>/gs
const ICON_BUTTON_RE = /<IconButton\b[^>]*>/gs
const CLASS_ATTR_RE = /class="([^"]*)"/

function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (extname(full) === '.vue') out.push(full)
  }
  return out
}

function classTokens(tag: string): string[] {
  const match = CLASS_ATTR_RE.exec(tag)
  return match ? match[1].split(/\s+/).filter(Boolean) : []
}

const violations = new Map<string, string[]>()
const iconButtonsMissingLabel: string[] = []

for (const file of walk(clientSrc)) {
  const rel = relative(clientSrc, file)
  const source = readFileSync(file, 'utf8')

  for (const cell of source.match(TD_RE) ?? []) {
    for (const tag of cell.match(BUTTON_RE) ?? []) {
      const boxed = classTokens(tag).filter(token => BOXED_BUTTON_CLASSES.has(token))
      if (!boxed.length) continue
      const list = violations.get(rel) ?? []
      list.push(`${boxed.join(' ')} :: ${tag.replace(/\s+/g, ' ').trim().slice(0, 120)}`)
      violations.set(rel, list)
    }
  }

  for (const tag of source.match(ICON_BUTTON_RE) ?? []) {
    if (!/(^|\s)(:label|v-bind:label|label)=/.test(tag)) iconButtonsMissingLabel.push(`${rel} :: ${tag.replace(/\s+/g, ' ').trim().slice(0, 120)}`)
  }
}

const pending = new Set(PENDING_MIGRATION)

// 1. 表格单元格内不得有盒式文字按钮（待迁移清单除外）
const unexpected = [...violations.keys()].filter(rel => !pending.has(rel)).sort()
assert.deepEqual(
  unexpected,
  [],
  `表格 <td> 内禁止盒式文字按钮（btn-*）：改用 IconButton 图标按钮 + :label hint（AGENTS.md §2.13 / THEME.md §5）\n违规文件：\n${unexpected.map(rel => `  ${rel}\n${(violations.get(rel) ?? []).map(v => `    - ${v}`).join('\n')}`).join('\n')}`
)

// 2. 待迁移清单必须仍然真实违规：迁移完成后要删掉条目，清单只能缩小
const stale = PENDING_MIGRATION.filter(rel => !violations.has(rel)).sort()
assert.deepEqual(
  stale,
  [],
  `以下文件已不再违规，请从 test-frontend-table-actions.ts 的 PENDING_MIGRATION 中删除：${stale.join(', ')}`
)

// 3. 每个 IconButton 必须带 label（title hint + aria-label 的来源）
assert.deepEqual(
  iconButtonsMissingLabel,
  [],
  `每个 <IconButton> 必须传 :label（功能描述 hint）：\n${iconButtonsMissingLabel.map(v => `  - ${v}`).join('\n')}`
)

// 4. IconButton 组件契约：label 必须同时绑定到 title 与 aria-label
const iconButtonSource = readFileSync(join(clientSrc, 'components/IconButton.vue'), 'utf8')
assert.ok(iconButtonSource.includes(':title="label"'), 'IconButton 必须把 label 绑定为 title（hover hint）')
assert.ok(iconButtonSource.includes(':aria-label="label"'), 'IconButton 必须把 label 绑定为 aria-label（无障碍名）')
assert.ok(iconButtonSource.includes('icon-btn'), 'IconButton 必须使用 .icon-btn 扁平样式')

// 5. .icon-btn 样式契约：只用主题 token，且不得出现透明/半透明写法（AGENTS.md §2.11）
const mainCss = readFileSync(join(clientSrc, 'styles/main.css'), 'utf8')
const iconBtnBlocks = mainCss.match(/\.icon-btn[^{]*\{[^}]*\}/g) ?? []
assert.ok(iconBtnBlocks.length >= 3, 'main.css 必须定义 .icon-btn 及其 hover / danger / disabled 变体')
for (const block of iconBtnBlocks) {
  assert.ok(!/#[\da-fA-F]{3,8}\b/.test(block), `.icon-btn 不得硬编码 hex：${block}`)
  assert.ok(!/transparent|rgba\(|opacity/.test(block), `.icon-btn 不得使用透明/半透明写法：${block}`)
  assert.ok(!/(?:^|[^\w-])btn-/.test(block), `.icon-btn 不得依赖盒式 btn-* 样式：${block}`)
}

console.log(`frontend table action guard passed（待迁移 ${PENDING_MIGRATION.length} 个文件）`)
