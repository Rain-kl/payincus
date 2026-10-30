# PayIncus 前端主题与 Apple 设计规范（THEME.md）

> **权威指引**：本文件是**前端设计系统、组件配方与取色/用色的权威依据**。所有 AI agent 在改动任何前端视觉（样式 / 布局 / 颜色 / 动效）前必须精读。  
> **规矩来源**：本规范**直接采用 StyleKit 官方统一提示词与设计规格**（[StyleKit Apple 风格与前端完成度规范](https://www.stylekit.top/zh/styles/apple-style#frontend-readiness)），并根据 PayIncus 当前项目架构状态（单源主题 Token、表格 IconButton、抽屉弹窗等守卫纪律）进行精准适配调整。

---

# Part 1: StyleKit 官方统一提示词（Hard Prompt）

STYLEKIT_STYLE_REFERENCE
style_name: Apple 风格
style_slug: apple-style
style_source: https://www.stylekit.top/zh/styles/apple-style#frontend-readiness

## 什么时候用
当你希望 AI 严格按风格规则生成代码时使用。它是生产界面最稳的默认选择。

## 怎么用
- 把完整提示词复制到 ChatGPT、Claude、Cursor 或其他编码助手。
- 在提示词后追加具体产品、页面或组件需求。
- 生成后按禁止项和交互状态检查，确认没有风格漂移。

请严格遵守以下风格规则并保持一致性，禁止风格漂移。

## 执行要求

- 优先保证风格一致性，其次再做创意延展。
- 遇到冲突时以禁止项为最高优先级。
- 输出前自检：颜色、排版、间距、交互是否仍属于该风格。

## Style Rules

你是一名专精 Apple Style 设计风格的前端开发专家。生成的所有代码都必须严格遵守以下约束：

## 绝对禁止

- 禁止使用过多颜色
- 禁止使用渐变背景
- 禁止使用重阴影
- 禁止元素过于拥挤
- 禁止使用花哨的装饰

## 必须遵守

- 使用大量留白，让内容呼吸
- 使用 Apple 灰 #f5f5f7 作为背景
- 使用 Apple 蓝 #0071e3 作为强调色
- 使用精致的圆角 rounded-xl 或 rounded-2xl
- 使用微妙的阴影
- 使用 SF Pro 风格字体（-apple-system）

## 配色方案

主色：
- 黑色：text-black, bg-black
- 白色：text-white, bg-white
- Apple 灰：bg-[#f5f5f7]

强调色：
- Apple 蓝：#0071e3
- Apple 绿：#34c759
- Apple 红：#ff3b30

## 字体排印

- 标题：font-semibold tracking-tight
- 正文：font-normal
- 链接：text-[#0071e3] hover:underline

## 布局

- 最大宽度：max-w-5xl 或 max-w-[980px]
- 大量留白：py-20, py-24
- 居中对齐：text-center, mx-auto

## 动效与交互规则

- Spring Physics（弹簧物理）：严禁使用默认的 linear 或基础 ease。必须使用丝滑的减速曲线，如 `transition-all duration-500 ease-[cubic-bezier(0.25,0.1,0.25,1)]`。
- Haptic Touch（触感反馈）：所有可交互元素（按钮、卡片）都必须具备物理按压的阻尼感，强制添加 `active:scale-[0.98]` 或 `active:scale-[0.96]`。
- Contextual Depth（情境纵深）：卡片悬停时，利用 `group-hover` 让内部图片或图标产生微妙放大（`scale-105`），营造视差纵深感。
- Subtle Blurs（微妙模糊）：交互过程可以伴随背景模糊度或不透明度的平滑过渡。

## 自检清单

每次生成代码后请检查：
1. 留白是否足够充分
2. 配色是否克制（黑白灰 + 蓝色点缀）
3. 是否没有使用渐变
4. 整体是否呈现高端简约的观感

---

# Part 2: Apple Style (Apple 风格) Design System / Spec

> 极致简约的高端设计风格，大量留白、精致圆角、微妙阴影和 SF Pro 风格字体，传达高端科技产品的品质感。

## 核心理念

Apple Style 是一种源于 Apple 设计语言的极致简约风格，通过大量留白、精致的细节和克制的配色，传达高端科技产品的品质感和信任感。

核心理念：
- 极致简约：去除一切不必要的元素
- 大量留白：让内容呼吸，突出重点
- 精致细节：每个像素都经过精心设计
- 克制配色：黑白灰为主，蓝色点缀

设计原则：
- 视觉一致性：所有组件必须遵循统一的视觉语言，从色彩到字体到间距保持谐调
- 层次分明：通过颜色深浅、字号大小、留白空间建立清晰的信息层级
- 交互反馈：每个可交互元素都必须有明确的 hover、active、focus 状态反馈
- 响应式适配：设计必须在移动端、平板、桌面端上保持一致的体验
- 无障碍性：确保色彩对比度符合 WCAG 2.1 AA 标准，所有交互元素可键盘访问

---

## Token 字典（精确 Class 映射）

### 边框
```
宽度: border-0
颜色: border-gray-200
圆角: rounded-2xl
```

### 阴影
```
小: shadow-[0_2px_8px_rgba(0,0,0,0.04)]
中: shadow-[0_4px_12px_rgba(0,0,0,0.08)]
大: shadow-[0_8px_24px_rgba(0,0,0,0.12)]
悬停: hover:shadow-[0_8px_24px_rgba(0,0,0,0.12)]
聚焦: focus:shadow-[0_0_0_4px_rgba(0,113,227,0.3)]
```

### 交互效果
```
悬停位移: （无）
悬停缩放: （无）
悬停透明度: hover:opacity-80
过渡动画: transition-all duration-200
按下状态: active:scale-95
```

### 字体
```
标题: font-semibold tracking-tight text-black
正文: text-gray-600
等宽: font-mono text-sm
```

### 字号
```
Hero: text-5xl md:text-7xl lg:text-8xl
H1: text-4xl md:text-5xl lg:text-6xl
H2: text-3xl md:text-4xl
H3: text-xl md:text-2xl
正文: text-base md:text-lg
小字: text-sm
```

### 间距
```
Section: py-20 md:py-32
容器: px-6 md:px-8
卡片: p-8 md:p-12
小间距: gap-4
中间距: gap-8
大间距: gap-12
```

### 颜色角色
```
背景主色: bg-white
背景辅色: bg-[#f5f5f7]
背景强调色: bg-black, bg-[#0071e3]
正文主色: text-black
正文辅色: text-gray-500
正文弱化色: text-gray-400
按钮主色: bg-[#0071e3] text-white
按钮辅色: bg-black text-white
```

---

## [FORBIDDEN] 绝对禁止（匹配即拒绝）

以下 class 在本风格中**绝对禁止使用**，生成时必须检查并避免：

### 禁止的 Class
- `bg-gradient-to-r`
- `bg-gradient-to-br`
- `shadow-2xl`
- `shadow-inner`
- `border-2`
- `border-4`

### 禁止的模式
- 匹配 `^bg-gradient`
- 匹配 `^shadow-(?:2xl|inner)`
- 匹配 `^border-(?:2|4|8)`

### 禁止原因
- `bg-gradient-to-r`: Apple style uses solid colors, not gradients
- `shadow-2xl`: Apple style uses subtle, refined shadows
- `border-2`: Apple style uses minimal or no borders

---

## [REQUIRED] 必须包含

### 按钮必须包含
```
rounded-full
font-medium
transition-all
duration-300
```

### 卡片必须包含
```
bg-white
rounded-2xl
shadow-sm
```

### 输入框必须包含
```
bg-[#f5f5f7]
rounded-xl
text-[#1d1d1f]
focus:outline-none
focus:ring-2
focus:ring-[#0071e3]/30
transition-all
```

---

## [COMPARE] Apple Style 错误 vs 正确对比

以下错误示例只代表“未经过当前风格适配的通用默认值”，不要把错误示例当成视觉建议。

### 按钮

[WRONG] **错误示例**（通用组件库默认样式，不要直接复制）：
```html
<button class="{GENERIC_LIBRARY_BUTTON_DEFAULT}">
  点击我
</button>
```

[CORRECT] **正确示例**（使用当前风格的 token）：
```html
<button class="rounded-full font-medium transition-all duration-300 bg-[#0071e3] text-white">
  点击我
</button>
```

### 卡片

[WRONG] **错误示例**（未经当前风格适配的通用卡片）：
```html
<div class="{GENERIC_LIBRARY_CARD_DEFAULT}">
  <h3>{TITLE}</h3>
</div>
```

[CORRECT] **正确示例**（使用当前风格的 card token）：
```html
<div class="bg-white rounded-2xl shadow-sm p-8 md:p-12">
  <h3 class="font-semibold tracking-tight text-black text-xl md:text-2xl">{TITLE}</h3>
</div>
```

### 输入框

[WRONG] **错误示例**（未经当前风格适配的通用输入框）：
```html
<input class="{GENERIC_LIBRARY_INPUT_DEFAULT}" />
```

[CORRECT] **正确示例**（使用当前风格的 input token）：
```html
<input class="bg-[#f5f5f7] rounded-xl text-[#1d1d1f] focus:outline-none focus:ring-2 focus:ring-[#0071e3]/30 transition-all" placeholder="{PLACEHOLDER}" />
```

---

## [TEMPLATES] Apple Style 页面骨架模板

### 导航栏骨架
```html
<nav class="bg-white text-black border-0 border-gray-200 px-6 md:px-8">
  <div class="flex items-center justify-between max-w-6xl mx-auto gap-8">
    <a href="/" class="font-semibold tracking-tight text-black text-xl md:text-2xl">
      {LOGO_TEXT}
    </a>
    <div class="flex gap-8 text-gray-600 text-sm">
      {NAV_LINKS}
    </div>
  </div>
</nav>
```

### Hero 区块骨架
```html
<section class="bg-black text-black py-20 md:py-32 px-6 md:px-8">
  <div class="max-w-4xl mx-auto">
    <h1 class="font-semibold tracking-tight text-black text-5xl md:text-7xl lg:text-8xl">
      {HEADLINE}
    </h1>
    <p class="text-gray-600 text-base md:text-lg max-w-xl">
      {SUBHEADLINE}
    </p>
    <button class="rounded-full font-medium transition-all duration-300 bg-[#0071e3] text-white">
      {CTA_TEXT}
    </button>
  </div>
</section>
```

### 卡片网格骨架
```html
<section class="bg-white text-black py-20 md:py-32 px-6 md:px-8">
  <div class="max-w-6xl mx-auto">
    <h2 class="font-semibold tracking-tight text-black text-3xl md:text-4xl">{SECTION_TITLE}</h2>
    <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-8">
      <div class="bg-white rounded-2xl shadow-sm p-8 md:p-12">
        <h3 class="font-semibold tracking-tight text-black text-xl md:text-2xl">{CARD_TITLE}</h3>
        <p class="text-gray-600 text-base md:text-lg text-gray-400">{CARD_DESCRIPTION}</p>
      </div>
    </div>
  </div>
</section>
```

---

# Part 3: 真实前端完成度（Frontend Readiness）

这层检查这个风格是否已经具备真实网站常见的主题、状态反馈、键盘可访问性和性能约束。

### 1. 暗色模式（Dark Mode）
- 暗色模式需要使用语义 tokens，不要直接把浅色主题反相。
- 文本在浅色和暗色模式下的对比度必须达到 WCAG 2.1 AA 标准（正文 ≥ 4.5:1）。

### 2. 组件全状态覆盖（Component States）
组件至少要覆盖以下 8 种核心状态：
- **默认（Default）**
- **悬停（Hover）**
- **键盘焦点（Focus-Visible）**
- **按下阻尼（Active）**
- **禁用（Disabled）**
- **加载中（Loading / Skeleton）**
- **空状态（Empty）**
- **错误与成功反馈（Error / Success）**

各基元必须覆盖的状态规格：
- **按钮**：默认 / 悬停 / 键盘焦点 / 按下 / 禁用
- **输入框**：默认 / 悬停 / 键盘焦点 / 禁用 / 错误
- **卡片**：默认 / 悬停 / 键盘焦点 / 加载中 / 骨架屏
- **表单**：默认 / 键盘焦点 / 禁用 / 加载中 / 错误

### 3. 动效规则（Motion & Physics）
- **Spring Physics 物理缓动**：必须使用丝滑的减速曲线 `cubic-bezier(0.25, 0.1, 0.25, 1)`。
- **Haptic Touch 触感反馈**：所有可交互元素（按钮、卡片）都必须具备物理按压阻尼感，强制添加 `active:scale-[0.98]` 或 `active:scale-[0.96]`。
- **可访问性降级**：必须提供 `prefers-reduced-motion` 备选降级方案。
- **性能约束**：性能上避免动画改变布局（仅允许 `transform` 与 `opacity`）；重 blur/shadow 的风格要控制层数和滚动区域。

### 4. 可访问性（Accessibility）
- **触控区域保底**：移动端触控交互区域不得低于 44px（`min-h-[44px]`）。
- **键盘导航**：所有交互元素具备清晰键盘焦点外发光环。
- **无障碍信息**：无文字纯图标按钮强制附加 `:label`、`title`、`aria-label`。

---

# Part 4: 结合 PayIncus 当前项目状态的必要适配规矩

根据 PayIncus 仓库现有架构、守卫测试及运营不变量，对上述官方提示词进行以下工程落地适配调整：

### 4.1 色彩 Token 化适配（Single Source of Truth）
> **硬规矩**：网站提示词中的 `#f5f5f7`、`#0071e3`、`#1d1d1f` 等色值在 PayIncus 项目中**统一收敛到 `client/src/styles/theme.css` 的语义 Token 中**。组件、全局样式、scoped style 中**一律不得散落硬编码 `#hex`**，一律使用 `var(--xxx)` 或语义 class 引用（AGENTS.md §2.9/§2.10，由 `test:frontend-color-tokens` 守卫强制拦截）。

#### Token 对照表（:root 暗色默认 + .light 浅色）
| 角色 | Token | 浅色（.light） | 暗色（:root） | StyleKit 映射说明 |
|---|---|---|---|---|
| **页面主背景** | `--bg-primary` | `#f5f5f7` | `#000000` | 浅色 Apple 灰 / 暗色 Apple 纯黑 |
| **卡片/表面基底** | `--bg-surface` | `#ffffff` | `#1c1c1e` | 纯白卡片底 / 暗色 System Gray 6 |
| **次级表面** | `--bg-secondary` | `#ffffff` | `#161617` | 二级卡片与面板 |
| **内嵌/表头三级底** | `--bg-tertiary` | `#e8e8ed` | `#242426` | 表头、分割灰底 |
| **柔和次表面** | `--bg-surface-soft` | `#f5f5f7` | `#2c2c2e` | 输入框底色、次级悬停浅底 |
| **标准发丝边框** | `--border-color` | `#d2d2d7` | `#38383a` | 细发丝边框（Apple Hairline） |
| **主文字** | `--text-primary` | `#1d1d1f` | `#f5f5f7` | Apple Near Black / 暗色反白 |
| **次级文字** | `--text-secondary` | `#86868b` | `#86868b` | 说明文案与标签灰 |
| **弱化文字** | `--text-tertiary` | `#a1a1a6` | `#636366` | 占位符与小辅助说明 |
| **Apple 官方交互蓝** | `--accent` | `#0071e3` | `#2997ff` | 主行动按钮、链接、激活指示 |
| **交互 Hover 蓝** | `--accent-strong` | `#0077ed` | `#0077ed` | 悬停提亮 |
| **导航选中态** | `--nav-active` | `#0071e3` | `#2997ff` | 激活导航菜单项 |
| **成功状态** | `--success` | `#34c759` | `#30d158` | Apple Green |
| **警告状态** | `--warning` | `#ff9f0a` | `#ffd60a` | Apple Orange |
| **危险/错误状态** | `--error` | `#ff3b30` | `#ff453a` | Apple Red（保留不可灰化） |

> **透明度规则**：禁止 `rgba()` 或带 alpha 的 8 位 hex；需要「主色 + 透明度」时一律使用 `color-mix(in srgb, var(--accent) 10%, transparent)`（AGENTS.md §2.11）。

### 4.2 弹窗统一抽屉适配（Drawer Modal Invariant）
- **硬规矩**：所有操作模态框、表单浮层一律使用 `<DrawerModal>` 侧边抽屉布局（AGENTS.md §2.12），严禁使用居中弹窗。
- **视觉要求**：抽屉内胆继承 `--bg-surface`，左上/左下大圆角继承 `--radius-modal`（20px），取消按钮使用白底黑字胶囊 `btn-secondary`，确认按钮使用 `btn-primary`。

### 4.3 表格操作列扁平化适配（Table Actions Invariant）
- **硬规矩**：表格（`<table>`）操作列（`<td>`）内**严禁出现任何带文字的按钮**（`btn-primary` / `btn-secondary` 等），必须改用 `client/src/components/IconButton.vue`（底层 class `.icon-btn`，扁平无边框，hover 才浮出浅底，带 `active:scale-95` 触感阻尼）。
- **hint 与 a11y**：每个图标按钮必须具备 `:label`（自动下发至 `title` 与 `aria-label`，文案走 i18n，禁止硬编码文字）。
- **布局锁定**：受保护的表格保持 `table-fixed` 定宽且隐藏溢出，严禁引入横向滚动条（`overflow-x-auto`，AGENTS.md §4）。

### 4.4 豁免白名单（允许 hex 的区域）
仅以下白名单文件允许存在 `#hex`（新增需同步更新 `server/scripts/test-frontend-color-tokens.ts`）：
- `styles/theme.css`（唯一色彩 Token 来源）
- `styles/markdown-theme.css`（Markdown 语法高亮）
- `styles/kawaii-cloud.css`（渐进改造区）
- `client/src/theme/**`（集中色板常量：图表/搜索面板/终端色板等）
- 娱乐游戏化模块彩色（EntertainmentView 等）
- 第三方品牌符号色（Google/GitHub/Turnstile）
- 帮助中心分类色（HelpView 的 category.color）

### 4.5 质量卡口验证命令
每次改动前端必须依次运行并通过以下守卫：
```bash
pnpm --filter server test:frontend-color-tokens    # 主题色纪律守卫（必跑，拦截一切散落 hex）
pnpm --filter server test:frontend-table-actions   # 表格操作列守卫（必跑，拦截表格文字按钮）
pnpm --filter server test:frontend-route-guards    # 路由与锁定表格守卫（必跑，防横向滚动）
pnpm --filter client type-check                    # 客户端 TypeScript 类型检查
pnpm build:client                                  # 用户端 + 管理端双端构建验证
```
