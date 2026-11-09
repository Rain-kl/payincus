# PayIncus 前端主题与 Apple 设计规范（THEME.md）

> **权威指引**：本文件是**前端设计系统、组件配方与取色/用色的权威依据**。所有 AI agent 在改动任何前端视觉（样式 / 布局 / 颜色 / 动效）前必须精读。  
> **规矩来源**：本规范**直接采用 StyleKit 官方统一提示词与设计规格**（[StyleKit Apple 风格与前端完成度规范](https://www.stylekit.top/zh/styles/apple-style#frontend-readiness)）。  
> **原则**：**忽略旧项目的一切陈旧主题限制（如禁止透明度、表格禁止文字按钮等旧限制已全部废止）**，全站完全遵循 StyleKit Apple 风格。

---

# Part 1: StyleKit 官方统一提示词（Hard Prompt）

STYLEKIT_STYLE_REFERENCE
style_name: Apple 风格
style_slug: apple-style
style_source: https://www.stylekit.top/zh/styles/apple-style#frontend-readiness

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
- 禁止硬编码十六进制颜色
- 禁止在组件里指定主题色值

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
- Subtle Blurs（微妙模糊）：交互过程伴随背景模糊度（如 `backdrop-blur`、磨砂玻璃效果）与微妙不透明度过渡（如 `hover:opacity-80`）。

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
- **悬停（Hover）**：例如 `hover:opacity-80`
- **键盘焦点（Focus-Visible）**：例如 `focus-visible:ring-2 focus-visible:ring-[var(--accent)]`
- **按下阻尼（Active）**：`active:scale-[0.98]` 或 `active:scale-95`
- **禁用（Disabled）**：`opacity-50 pointer-events-none`
- **加载中（Loading / Skeleton）**
- **空状态（Empty）**
- **错误与成功反馈（Error / Success）**

各基元必须覆盖的状态规格：
- **按钮**：默认 / 悬停 / 键盘焦点 / 按下 / 禁用（支持在页面、表单、卡片和表格操作列中作为胶囊按钮使用）
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

---

# Part 4: 项目工程化集中管理（组件库复用）

为满足“未来改动按钮/表格/卡片样式只需在组件库一处调整，即可全局生效”的架构要求，全站页面统一通过 `@/components/ui` 引用标准基础组件：

- **`Button.vue`**：Apple 胶囊按钮（`rounded-full`），支持在任意页面、卡片、表格操作列中使用。
- **`Card.vue`**：Apple 标准 18px 圆角卡片（`rounded-2xl`，微阴影 `shadow-sm`）。
- **`Table.vue`**：Apple 标准表格容器（支持定宽 `table-fixed`、表头、数据行与响应式卡片）。
- **`Input.vue` / `Select.vue`**：Apple 标准 12px 圆角（`rounded-xl`）表单输入框。
- **`Badge.vue`**：Apple 状态胶囊标签。
- **`EmptyState.vue`**：Apple 大留白居中空状态。
- **`PageHeader.vue`**：Apple 页面标题与操作顶栏。
- **`Pagination.vue`**：Apple 分页器。

### 常用验证命令
```bash
pnpm --filter client type-check                    # 客户端 TypeScript 类型检查
pnpm --filter server test:frontend-color-tokens    # 主题色规范
pnpm --filter server test:frontend-route-guards    # 路由与定宽表格防横向溢出守卫
pnpm build:client                                  # 用户端 + 管理端双端编译构建
```
