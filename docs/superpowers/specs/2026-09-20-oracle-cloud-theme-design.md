# Oracle Cloud (OCI) 风格与配色规范设计 (Design Spec)

- **日期**：2026-09-20
- **目标**：将 PayIncus 前端（用户端 + 管理端）核心主题升级为 Oracle Cloud Infrastructure (OCI Console) 现代企业云控制台风格。
- **核心原则**：
  - **不改布局骨架**：保留现有的 DOM 结构与响应式逻辑，改动聚焦于 Design Tokens、CSS 变量及组件视觉规范。
  - **严格守卫合规**：严禁破坏既有的定宽表格排版守卫（保持 `table-fixed` / `overflow-hidden`，严禁横向滚动）。
  - **平整小圆角**：卡片无阴影（Flat Design, `box-shadow: none`）、统一小圆角（4px ~ 6px）。

---

## 1. 核心设计语言与色彩规范 (Color Tokens)

### 1.1 顶栏专色 (Top Navigation Bar)
- **背景底色**：恒定采用 OCI 经典深木炭暖色 `#393632`（浅色模式）与 `#1F1D1B`（深色模式）。
- **底部边框**：`1px solid #2C2A28`。
- **前景与图标**：
  - 品牌文字与主要操作反白：`#FFFFFF`。
  - 次要图标与辅助信息：`#C7C5C2`，悬浮底色 `rgba(255, 255, 255, 0.08)`。
- **OCI 嵌入式搜索框**：
  - 底色：`rgba(0, 0, 0, 0.25)`（半透暗底）。
  - 边框：`1px solid #55504A`，圆角 `4px`。
  - 占位符与图标：`#A8A49E`。
  - 聚焦高亮：边框高亮为 `#8C8984`，背景微调至 `rgba(0, 0, 0, 0.35)`。

### 1.2 画布与表面色 (Surfaces & Backgrounds)
| Token 变量 | 浅色模式（Light / 对标参考图） | 深色模式（OCI Dark Console） | 说明 |
| :--- | :--- | :--- | :--- |
| `--bg-primary` / `--kawaii-bg` | `#FFFFFF` | `#161513` | 全局主画布背景 |
| `--bg-secondary` / `--kawaii-surface` | `#FFFFFF` | `#1F1D1B` | 卡片、面板主体背景 |
| `--bg-tertiary` / `--kawaii-surface-soft` | `#F9F9F8` | `#282522` | 表格表头、二级容器、未选中项 |
| `--border-color` / `--kawaii-line` | `#E0DFDD` | `#312E2B` | 容器外边框与主分割线 |
| `--border-hover` / `--kawaii-line-strong` | `#B8B6B2` | `#45413C` | 悬浮加深分割线 |
| `--text-primary` / `--kawaii-text` | `#161513` | `#F4F4F3` | 正文与标题高对比度文字 |
| `--text-secondary` / `--kawaii-muted` | `#66615E` | `#A29E9A` | 次要描述与辅助信息文字 |

### 1.3 强调色与链接色 (Accent & Primary)
全面替换原有的 Nimbus 靛紫（`#5e6ad2`），引入 **OCI 云蓝（Classic OCI Blue）** 色阶：
- `primary-50`: `#edf5fc`
- `primary-100`: `#d6e9f8`
- `primary-200`: `#b0d4f2`
- `primary-300`: `#7db6e9`
- `primary-400`: `#4593de`
- `primary-500`（主链接与交互高亮）：`#0B5CAD`
- `primary-600`（悬浮色）：`#006699`
- `primary-700`: `#00507a`
- `primary-800`: `#003b5a`
- `primary-900`: `#00273d`

### 1.4 状态胶囊色 (Status Badges)
参考 OCI 标准柔和胶囊指示设计：
- **运行中 / 可用 (Success)**：
  - 浅色：背景 `#E2F3E8`，文字 `#1E7E34`
  - 深色：背景 `#15321E`，文字 `#5CD887`
- **警告 / 排队中 (Warning)**：
  - 浅色：背景 `#FFF3D6`，文字 `#8F5B00`
  - 深色：背景 `#3B2B0A`，文字 `#F5C842`
- **停止 / 严重 / 错误 (Error)**：
  - 浅色：背景 `#FCE8E6`，文字 `#C74634`（OCI Redwood 陶土红）
  - 深色：背景 `#3B1411`，文字 `#F27568`

---

## 2. 几何与组件主题规范 (Components & Geometry)

### 2.1 几何与投影体系 (Radius & Shadows)
- **圆角收敛**：
  - `DEFAULT`: `4px`
  - `sm`: `2px`
  - `md`: `4px`
  - `lg`: `6px`
  - `xl`: `8px`
  - 去除所有 12px/16px 及以上的弧度感，全面呈现硬朗工业感。
- **投影清零 (Flat Design)**：
  - 卡片投影全部重置为 `box-shadow: none`（`--kawaii-shadow: none`）。
  - 彻底移除所有彩色光晕（`glow-*` 类）。
  - 仅全局浮动下拉框与模态弹窗保留 `0 4px 12px rgba(0, 0, 0, 0.06)` 的微中性阴影。

### 2.2 卡片容器 (.card)
- 背景：`var(--bg-secondary)`
- 边框：`1px solid var(--border-color)`
- 圆角：`4px`
- 阴影：`none`
- 悬停态（.card-hover）：无浮起位移，仅边框加深至 `var(--border-hover)`。

### 2.3 数据表格 (.table)
- **表头 (`th`)**：
  - 背景：`var(--bg-tertiary)`（`#F9F9F8` / `#282522`）
  - 文字：小号加粗，字色 `var(--text-secondary)`
  - 底边框：`1px solid var(--border-color)`
- **单元格 (`td`)**：
  - 紧凑内边距 `py-2.5 px-4`
  - 底边细线：`#EBEAE8`
  - 资源链接列：高亮采用 `text-primary-500`（`#0B5CAD`），悬停显示下划线。
- **锁定守卫规范**：严格维持 `table-fixed` 与 `overflow-hidden`，绝对不引入 `overflow-x-auto`。

### 2.4 按钮体系 (.btn)
- **次级 / 线框按钮 (.btn-secondary)**：
  - 参考图左下角样式：底色纯白 `#FFFFFF`，边框 `1px solid #393632`，文字 `#161513`，圆角 `4px`。
- **主要按钮 (.btn-primary)**：
  - 浅色模式：深木炭黑 `#161513` 底色 + 白色文字，圆角 `4px`。
  - 深色模式：高对比度白底/浅灰底 + 黑字。
- **危险按钮 (.btn-danger)**：
  - 浅红底 `#FCE8E6` + 陶土红字 `#C74634`，圆角 `4px`。

### 2.5 侧边栏导航 (SideNav)
- 浅色模式下采用纯白底色 `#FFFFFF`，右侧 1px 分割线 `#E0DFDD`。
- 导航激活态：采用 OCI 标志性指示——左边缘 `3px solid #0B5CAD` 竖条高亮 + 背景淡蓝 `rgba(11, 92, 173, 0.08)` + 文字 `#0B5CAD`。

---

## 3. 实施文件影响面 (Target Files)

1. [client/tailwind.config.js](file:///Users/ryan/Code/Node/payincus/client/tailwind.config.js)：
   - 配置 `borderRadius`（收敛为 4px/6px）与 `boxShadow`（清零）。
   - 替换 `primary` 色阶为 OCI 云蓝，收敛灰阶为 OCI 中性石墨色。
2. [client/src/styles/main.css](file:///Users/ryan/Code/Node/payincus/client/src/styles/main.css)：
   - 重构 `:root` 与 `.light` 变量（纯白画布、细中性线、深木炭字）。
   - 更新 `.card`、`.btn`、`.input`、`.table`、`.badge` 类。
3. [client/src/styles/kawaii-cloud.css](file:///Users/ryan/Code/Node/payincus/client/src/styles/kawaii-cloud.css)：
   - 重构 `--kawaii-*` 设计变量。
   - 顶栏 `.kawaii-topbar` 强制固定 `#393632` 暖深木炭色。
   - 侧边栏菜单激活态改为 OCI 蓝左侧竖条。
4. [client/src/components/layout/AppLayout.vue](file:///Users/ryan/Code/Node/payincus/client/src/components/layout/AppLayout.vue)：
   - 顶栏控件着色与 OCI 嵌入式搜索栏样式挂载。

---

## 4. 验证与守卫保障 (Verification Plan)

所有改动必须通过以下自动化校验：
1. `pnpm --filter client type-check`：前端 TypeScript 类型安全检查通过。
2. `pnpm --filter server test:frontend-route-guards`：所有受锁定的定宽表格与路由守卫通过。
3. `pnpm --filter server test:frontend-dist-boundary-guards`：用户端与管理端 dist 边界守卫通过。
4. `pnpm --filter server test:frontend-i18n-keys`：i18n 键完整性通过。
5. `pnpm build:client`：双端（`dist/user` 与 `dist/admin`）全量打包 0 错误。
