# 前端隐藏托管功能与控制面板更新菜单迁移至系统设置设计文档

## 背景与目标

1. **隐藏托管功能（前端全部隐藏，不删代码）**
   - 用户端：隐藏侧边栏「资源」分组及所属菜单（我的节点 `/resources/hosts`、我的套餐 `/resources/packages`、托管收益 `/hosting-wallet`），全局搜索框（`MenuSearchBox`）同步过滤。
   - 用户端实例创建页（`/instances/create`）：隐藏托管套餐及专区选项卡，仅展示官方/自营套餐，隐藏多源切换胶囊条。
   - 管理端：侧边栏隐藏「托管 (`/admin/hosting`)」，全局搜索框同步过滤；系统设置导航项中隐藏「托管与站点 (`/admin/settings/hosting`)」。
   - 代码与后端保持：后端所有 API 与 DB 数据模型 100% 保持不变；路由定义与客户端 API 保持，仅在前端视图与导航入口处彻底隐藏。

2. **控制面板调整：将运维->更新移动到系统设置新建 Tab**
   - 管理端侧边栏：从「运维 (`nav.operations`)」分组中移除「更新 (`admin-system-update`)」。
   - 系统设置导航选项卡：在 `client/src/constants/adminSettings.ts` 中增加 `update`（更新）选项卡，路径为 `/admin/settings/update`。
   - 路由配置：`/admin/settings/update` 挂载 `SystemUpdateView.vue`，保留原 `/admin/system-update` 路径并重定向至 `/admin/settings/update`。
   - 视图表现：在 `SystemUpdateView.vue` 顶部引入与 `SystemConfigView`、`TelegramConfigView` 完全一致的系统设置选项卡导航条，确保在系统设置各 Tab 之间平滑切换。
   - 守卫测试兼容：同步适配 `server/scripts/test-system-update-guards.ts` 及相关守卫测试。

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
