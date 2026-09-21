# 前端隐藏托管功能与控制面板更新菜单迁移至系统设置设计文档

## 背景与目标

1. **隐藏托管功能（前端全部隐藏，不删后端与数据，拒绝历史包袱与临时垫片）**
   - 用户端侧边栏（`side-nav-items-user.ts`）：彻底移除「资源」分组及所属菜单（我的节点 `/resources/hosts`、我的套餐 `/resources/packages`、托管收益 `/hosting-wallet`）。
   - 全局搜索框（`MenuSearchBox.vue`）：移除托管关键词与冗余逻辑。
   - 用户端实例创建页（`/instances/create`）：彻底移除托管套餐及专区逻辑，仅保留官方直营套餐。
   - 管理端侧边栏（`side-nav-items-admin.ts`）：移除「托管 (`/admin/hosting`)」。
   - 系统设置导航项（`constants/adminSettings.ts`）：移除「托管与站点 (`/admin/settings/hosting`)」。
   - 后端与 DB 模型：100% 保持不变。

2. **控制面板调整：将运维->更新直接迁移到系统设置新建 Tab（不做历史重定向包袱）**
   - 管理端侧边栏：从「运维 (`nav.operations`)」分组中移除「更新 (`admin-system-update`)」。
   - 系统设置导航选项卡：在 `client/src/constants/adminSettings.ts` 中增加 `update`（更新）选项卡，路径为 `/admin/settings/update`。
   - 路由配置：直接将系统更新路由挂载到 `/admin/settings/update`，不保留旧路径 `/admin/system-update`，不做历史重定向。
   - 视图表现：在 `SystemUpdateView.vue` 顶部引入与 `SystemConfigView`、`TelegramConfigView` 完全一致的系统设置选项卡导航条，确保在系统设置各 Tab 之间平滑切换。
   - 守卫测试：更新 `server/scripts/test-system-update-guards.ts` 与 `test-frontend-route-guards.ts`，严格匹配最新纯净架构。

## 详细设计方案

### 1. 用户端隐藏托管功能
- **侧边栏与搜索框 (`SideNav.vue`, `MenuSearchBox.vue`)**
  - 在 `SideNav.vue` 的菜单计算属性中，对于非管理端（用户端），无条件过滤掉 `nav.resources` 分组以及 `my-hosts`、`my-packages`、`hosting-wallet` 菜单项。
  - 在 `MenuSearchBox.vue` 中，用户端无条件过滤 `my-hosts`、`my-packages`、`hosting-wallet` 搜索建议。
- **实例创建页 (`InstanceCreateView.vue`)**
  - `sourceTabs` 计算属性中，移除托管专区 (`zone:*`) 与托管市场 (`market`) 的注入逻辑，仅保留官方套餐 (`official`)。
  - 单一源时不渲染源切换胶囊按钮；默认请求官方套餐列表。

### 2. 管理端隐藏托管菜单与设置
- **侧边栏与搜索框 (`side-nav-items-admin.ts`, `SideNav.vue`, `MenuSearchBox.vue`)**
  - `side-nav-items-admin.ts` 中隐藏 `admin-hosting`，或在 `SideNav.vue` 与 `MenuSearchBox.vue` 渲染层中对 `admin-hosting` 进行过滤。
- **系统设置 (`constants/adminSettings.ts`)**
  - `systemSettingsSections` 数组中移除或隐藏 `hosting`（`/admin/settings/hosting`）项。

### 3. 系统更新迁移至系统设置选项卡
- **系统设置 Tab 定义 (`constants/adminSettings.ts`)**
  - 新增 `SystemSettingsNavigationItem`：
    ```ts
    {
      key: 'update',
      path: '/admin/settings/update',
      labelKey: 'nav.systemUpdate'
    }
    ```
- **路由定义 (`router/admin.ts`)**
  - 新增 `/admin/settings/update`，component 为 `SystemUpdateView.vue`。
  - 原 `/admin/system-update` 重定向至 `/admin/settings/update`。
- **视图调整 (`SystemUpdateView.vue`)**
  - 顶部增加系统设置统一 PageHeader 与 `systemSettingsNavigationItems` 标签栏，与 `TelegramConfigView.vue` 保持结构与样式规范一致。
- **管理端侧边栏 (`side-nav-items-admin.ts`)**
  - 移除 `admin-system-update` 菜单项（已移至系统设置）。
- **守卫测试维护**
  - 更新 `test-system-update-guards.ts` 中断言，匹配系统设置选项卡结构。

## 验证计划
- 运行 `pnpm --filter server type-check` 与 `pnpm --filter client type-check`。
- 运行 `pnpm --filter server test:system-update-guards`、`pnpm --filter server test:frontend-route-guards`、`pnpm --filter server test:frontend-dist-boundary-guards`、`pnpm --filter server test:frontend-color-tokens`。
- 运行 `make check`。
- 运行 `pnpm build:client` 确保双端构建成功。
