# PayIncus 前端主题系统（THEME.md）

> 本文件是**前端取色/用色的权威指引**。所有 AI agent 在改动任何前端视觉（样式 / 布局 / 颜色）前必须先读本文。
> 硬规矩：**禁止硬编码 hex，禁止在组件里指定颜色**（AGENTS.md §2.9 / §2.10）。由 `test:frontend-color-tokens` 守卫强制。

---

## 1. 核心原则

1. **唯一事实来源**：所有颜色色值只定义在 `client/src/styles/theme.css`。
2. **组件零 hex**：`.vue` 组件（模板、scoped style、内联 style、SVG 属性）一律通过 `var(--xxx)` / 语义 class 引用，**不得出现任何 `#hex`**。
3. **改主题只改一处**：想换品牌色，只改 `theme.css` 里对应 token，全站自动跟随。
4. **明暗双主题**：`:root` = 暗色默认，`.light` = 亮色。核心 token 两套必须都定义（守卫校验）。
5. **守卫闸门**：`pnpm --filter server test:frontend-color-tokens` 全局扫描 `client/src`，发现 hex 即失败。

---

## 2. Token 清单（theme.css 当前全部语义色）

表内「浅色」= `.light` 值，「暗色」= `:root` 值。

### 2.1 背景层级

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--bg-primary` | `#F6F4F3` | `#161513` | **网页背景**（body 底色）|
| `--bg-secondary` | `#FFFFFF` | `#1f1d1b` | 次级背景（白卡片）|
| `--bg-tertiary` | `#FBF9F8` | `#282522` | 三级背景（表头/内嵌区）|
| `--bg-elevated` | `#FFFFFF` | `#24211e` | 浮层背景（菜单/弹窗内部）|
| `--bg-surface` | `#FFFFFF` | `#1f1d1b` | **表面白底**（卡片/弹窗主底）|
| `--bg-surface-soft` | `#FBF9F8` | `#282522` | 次级表面 |
| `--sidebar-bg` | `#E4E3E0` | `#1a1816` | **侧边栏背景** |
| `--success-soft` | `#C5D8AD` | `#1e3a2f` | 成功/在线标签浅底 |

### 2.2 边框

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--border-color` | `#E4E3E0` | `#2C2A29` | **常规边框**（divider/表格线）|
| `--border-hover` | `#2C2A29` | `#45413c` | 悬停边框 |
| `--border-strong` | `#2C2A29` | `#45413c` | 强调边框（按钮描边）|

### 2.3 文字

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--text-primary` | `#161513` | `#f4f4f3` | **主文字**（正文/标题）|
| `--text-secondary` | `#66615e` | `#a29e9a` | 次级文字（表头/说明）|
| `--text-tertiary` | `#8c8984` | `#736f6a` | 弱化文字（占位/hint）|

### 2.4 交互/语义（owner 指定品牌色）

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--accent` | `#295BA7` | `#4593DE` | **按钮主色 / 链接 / 强调** |
| `--accent-strong` | `#295BA7` | `#0b5cad` | 强调深色（hover）|
| `--nav-active` | `#161513` | `#f4f4f3` | **导航/菜单选中态** |
| `--topbar-bg` | `#383632` | `#1f1d1b` | **标题栏背景** |
| `--topbar-text` | `#FFFFFF` | `#FFFFFF` | 标题栏文字（恒白）|
| `--topbar-border` | `#2C2A29` | `#2C2A29` | 标题栏分隔线 |
| `--footer-bg` | `#F5F4F2` | `#1f1d1b` | 页脚背景 |

### 2.5 状态色（绿/琥珀/红三态保留）

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--success` | `#1e7e34` | `#5cd887` | 成功/在线 |
| `--warning` | `#8f5b00` | `#f5c842` | 待处理/警告 |
| `--error` | `#c74634` | `#f27568` | 失败/危险 |

> `rose`（真红，Tailwind ramp）在代码里当「危险/亏损」语义用，**保留不可灰化**（tailwind.config.js dangerRamp）。

### 2.6 专用色板（主题感知，组件内引用）

| Token | 浅色 | 暗色 | 用途 |
|---|---|---|---|
| `--terminal-bg` | `#0a0a0a`（两套同）| 同左 | xterm 终端深黑背景 |
| `--chart-1..4` | 深调 4 色 | 浅调 4 色 | 统计图表系列色 |
| `--alert-info/success/warning/danger/note` (+ `-bg`/`-border`) | — | — | 帮助文档 md-alert 状态色 |

---

## 3. 组件里怎么用（正确写法）

### 3.1 scoped style 引用 token

```css
/* ✅ 正确 */
.card {
  background-color: var(--bg-surface);
  border: 1px solid var(--border-color);
  color: var(--text-primary);
}
```

```css
/* ❌ 错误：禁止写死 */
.card {
  background-color: #FFFFFF;
  border: 1px solid #E4E3E0;
  color: #161513;
}
```

### 3.2 模板 class 引用 token

```html
<!-- ✅ 正确：Tailwind arbitrary value 里用 var() -->
<div class="bg-[var(--bg-surface)] border-[var(--border-color)] text-[var(--text-primary)]">…</div>

<!-- ✅ 正确：语义 class（已在 main.css 定义好） -->
<div class="card text-themed">…</div>
<button class="btn-primary">…</button>

<!-- ❌ 错误：写死 hex -->
<div class="bg-white border-[#E4E3E0] text-[#161513]">…</div>
```

### 3.3 SVG 属性

```html
<!-- ✅ 正确 -->
<path fill="var(--accent)" />

<!-- ❌ 错误 -->
<path fill="#295BA7" />
```

### 3.4 需要「主色 + 透明度」时

用 `color-mix`，不要写死 rgba 或带 alpha 的 hex：

```css
/* ✅ */
background: color-mix(in srgb, var(--accent) 10%, transparent);
border-color: color-mix(in srgb, var(--accent) 24%, var(--bg-surface));

/* ❌ */
background: rgba(41, 91, 167, 0.1);
border-color: #295ba73d; /* 8 位 hex 也禁止 */
```

> 注意：在 Tailwind `@apply` 内用 `color-mix` 需下划线写成 `color-mix(in_srgb, …)`。

---

## 4. 明暗双主题怎么处理

- **主题切换机制**：`client/src/stores/theme.ts` 把 `light`/`dark` class 挂到 `<html>`（`darkMode: 'class'`）。
- **CSS 变量自动跟随**：`var(--xxx)` 会自动取当前 class 下的值，无需手写 `.dark .xxx` 覆盖。
- **必须显式区分明暗的场景**（如浅色下用深字、深色下用浅字）直接用 token 即可——`--text-primary` 浅色 `#161513`、暗色 `#f4f4f3`，`var()` 自动正确。
- **禁止**在组件里写 `dark:text-[#xxx]` / `dark:bg-[#xxx]`（硬编码）；用 token 后不需要 dark: 变体。

---

## 5. 只改主题颜色时（改 theme.css）

1. 打开 `client/src/styles/theme.css`。
2. 改 `:root`（暗）和 `.light`（亮）两处对应 token。
3. 若改的是 `--accent`，**同步**更新 `client/tailwind.config.js` 的 `ociBlueRamp`（守卫校验两处一致）。
4. 跑 `pnpm --filter server test:frontend-color-tokens` + `pnpm build:client`。

**禁止**：在组件里新增一个局部颜色「绕过」主题——那样守卫会拦下你，而且主题管理就失效了。

---

## 6. 豁免白名单（允许 hex 的区域）

以下位置允许多个 hex 色值，**新增豁免必须同步更新** `server/scripts/test-frontend-color-tokens.ts` 的 `EXEMPT_PATHS`：

| 区域 | 理由 |
|---|---|
| `styles/theme.css` | 唯一 token 来源 |
| `styles/markdown-theme.css` | 内容渲染（GitHub 官方 Markdown 高亮）|
| `styles/kawaii-cloud.css` | 渐进改造区（hero/装饰类），变量已 token 化 |
| `client/src/theme/**` | 集中色板常量（chart / menu-search 等）|
| 娱乐/游戏化模块（EntertainmentView/CheckinModal/BadgeReward 等）| AGENTS.md「刻意保留」|
| 第三方品牌符号色（Google/GitHub/MD3/Turnstile）| 品牌标识色不可改 |
| 帮助中心分类色（HelpView/HelpManageView 的 category.color）| 后端下发数据 |
| `utils/markdown.ts` / `useTerminal.ts` / `stores/terminal.ts` / `utils/vipBadge.ts` | 内容/终端/徽章集中色板 |
| `stores/theme.ts` | meta theme-color 兜底（与 --bg-primary 同值）|
| `MenuSearchBox.vue` | 他人施工中（色板已抽 menu-search-palette.ts）|

> 原则：**数据型/内容型/品牌型色板**集中放 `client/src/theme/` 或样式文件，**不算组件散落**；UI 皮肤色必须走 token。

---

## 7. 常见错误清单（开发时自查）

| # | 错误写法 | 正确写法 |
|---|---|---|
| 1 | `bg-white`（页面级背景）| `bg-[var(--bg-surface)]` 或 `card` 类 |
| 2 | `text-black` / `text-gray-900`（正文）| `text-[var(--text-primary)]` 或 `text-themed` |
| 3 | `dark:bg-[#1f1d1b]` | `bg-[var(--bg-surface)]`（自动跟随）|
| 4 | `stroke="#fff"` / `fill="#000"` | `stroke="var(--topbar-text)"` / `fill="var(--text-primary)"` |
| 5 | `rgba(11, 92, 173, 0.08)`（选中态）| `color-mix(in srgb, var(--accent) 8%, transparent)` |
| 6 | 想在组件里「临时换个颜色」| 先查 token 表；没有就加 token 到 theme.css（明暗两套）|
| 7 | `placeholder="#FEF3C7"`（输入框示例色）| 仅限 VipLevelRulesEditor 这类**用户输入示例**豁免，其余禁止 |

---

## 8. 常用语义 class（main.css 已定义，直接可用）

- 文字：`text-themed` / `text-themed-secondary` / `text-themed-muted` / `text-themed-faint`
- 背景：`bg-themed` / `bg-themed-surface` / `bg-themed-secondary` / `bg-themed-tertiary`
- 边框：`border-themed` / `border-themed-hover`
- 按钮：`btn-primary` / `btn-secondary` / `btn-ghost` / `btn-danger` / `btn-warning`
- 卡片：`card` / `card-hover` / `stat-card`
- 徽章：`badge` / `badge-success` / `badge-warning` / `badge-error` / `badge-default`
- 状态：`text-success` / `text-warning` / `text-error`

---

## 9. 验证命令

```bash
pnpm --filter server test:frontend-color-tokens   # 主题色纪律守卫（必跑）
pnpm --filter client type-check                   # 类型检查
pnpm build:client                                 # 双端构建
pnpm --filter server test:frontend-route-guards   # 改前端必跑
```
