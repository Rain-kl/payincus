# Docker 单服务部署与 CI 调整设计方案

## 1. 目标与背景

将此前通过 PR #9 引入的 Docker 部署方案进行系统性调整与精简：
1. **前后端合一**：由原本的 backend + frontend 双容器结构改为单个 `payincus` 容器，单容器同时运行 Node.js 后端与轻量 Nginx 静态文件托管/反代，对外映射用户端与管理端端口。
2. **数据库升级**：PostgreSQL 升级至 `postgres:18-alpine`，配置 `PGDATA` 保证数据持久化，移除完全无用的 `redis` 服务。
3. **环境变量精简**：移除已废弃的插件/主题内部路径环境变量（如 `PLUGIN_INSTALL_DIR` 等）以及 Redis 变量，仅保留核心必须配置。
4. **CI 工作流对齐**：使用项目提供的 `.github/workflows/build-image.yml` 模板替换旧工作流，镜像名调整为 `payincus`，支持 amd64/arm64 双架构构建与 GHCR/Docker Hub 发布。
5. **清理 PR #9 冗余项**：清理 Trae IDE 生成的静态分析包与报告文件，恢复 `README.md` 中的官方 Telegram 群组链接。
6. **守卫测试维护**：调整 `server/scripts/test-split-deploy-config.ts` 中过时的「禁止存在 Docker 部署文件」断言，确保全套守卫测试顺利通过。

---

## 2. 架构设计

### 2.1 单容器前后端合一架构
```
外部请求
   │
   ├── Port 8080 (用户端) ──┐
   │                       ▼
   │                [ payincus 容器 ]
   │                ┌───────────────────────────────────┐
   │                │ Nginx (前台反代与静态资源)            │
   │                │   - Port 80: /usr/share/nginx/html/user │
   │                │   - Port 81: /usr/share/nginx/html/admin│
   │                │   - /api, /api/ws: 反代至 127.0.0.1:3001│
   │                └───────────────┬───────────────────┘
   │                                │ 反向代理
   │                                ▼
   │                ┌───────────────────────────────────┐
   │                │ Node.js 后端 Fastify (私有单实例)    │
   │                │   - 127.0.0.1:3001                │
   │                │   - 处理业务逻辑与 Prisma ORM       │
   │                └───────────────┬───────────────────┘
   │                                │ SQL
   │                                ▼
   └── Port 8081 (管理端) ─> [ postgres:18-alpine ]
```

### 2.2 为什么内置 Nginx 而非 Pure Node.js
- **安全不变量**：PayIncus 规范（`AGENTS.md`）明确规定「双端构建独立源，不共享 Cookie 域」。如果纯 Node 单端口通过 `/admin` 路径挂载，会导致用户态与管理员态的 refresh cookie / session 在同一域名/同一源下碰撞。
- **端口隔离**：内置 Nginx 同时监听 80（用户端）与 81（管理端），对外分别暴露到 `${USER_HTTP_PORT:-8080}` 与 `${ADMIN_HTTP_PORT:-8081}`。
- **静态资源性能**：Nginx 处理前端 SPA 路由回落（`try_files $uri $uri/ /index.html`）、Gzip/Brotli 压缩、缓存头以及 WebSocket upgrade（`/api/ws`），Node 专心处理 API 与长轮询。
- **镜像体积可控**：在 Debian slim 镜像中安装 `nginx` 和 `dumb-init` 仅增加约 10MB，运行时内存仅增加约 5MB。

---

## 3. 详细变动设计

### 3.1 Dockerfile
采用多阶段构建：
- **`build` 阶段**：基于 `node:22-bookworm-slim`，安装依赖，执行 `pnpm --filter client build && pnpm --filter server build`，产出 `client/dist/user`、`client/dist/admin` 和 `server/dist`。
- **`runtime` 阶段**：
  - 基于 `node:22-bookworm-slim`，安装 `nginx`、`dumb-init`、`openssl`、`ca-certificates`。
  - 拷贝构建好的前端产物至 `/usr/share/nginx/html/user` 与 `/usr/share/nginx/html/admin`。
  - 拷贝 Nginx 配置文件模板至 `/etc/nginx/templates/default.conf.template`（或直接使用固定配置）。
  - 拷贝编译后的后端代码与必要的生产依赖。
  - 入口脚本 `deploy/docker/entrypoint.sh`：
    1. 执行数据库迁移（`pnpm --dir /app/server exec prisma migrate deploy`）。
    2. 启动 Node 后端（监听 `127.0.0.1:3001`）。
    3. 启动 Nginx（监听 80 与 81），并通过 `dumb-init` 管理进程树与优雅停机（SIGTERM/SIGINT）。
  - 暴露端口 `80` 与 `81`。

### 3.2 Docker Compose (`docker-compose.yml`)
精简为仅 2 个服务：
1. **`postgres`**：
   - 镜像：`postgres:18-alpine`
   - 环境变量：
     - `POSTGRES_DB: ${POSTGRES_DB:-payincus}`
     - `POSTGRES_USER: ${POSTGRES_USER:-payincus}`
     - `POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}`
     - `PGDATA: /var/lib/postgresql/data`
   - 数据卷：`postgres-data:/var/lib/postgresql/data`
   - 健康检查：`pg_isready`
2. **`payincus`**：
   - 镜像：`payincus:latest`（或构建自当前目录）
   - 端口映射：
     - `"${USER_HTTP_PORT:-8080}:80"`
     - `"${ADMIN_HTTP_PORT:-8081}:81"`
   - 环境变量：读取 `.env.docker`
   - 依赖：`postgres`（`condition: service_healthy`）
   - 数据卷：`payincus-data:/opt/incudal`

### 3.3 环境变量精简 (`.env.docker.example`)
删除所有不必要的变量：
- 删除 `REDIS_*`（无 Redis）
- 删除 `PLUGIN_*`（`PLUGIN_INSTALL_DIR`, `PLUGIN_DATA_DIR`, `PLUGIN_LOG_DIR`, `PLUGIN_STAGING_DIR`, `PLUGIN_MARKET_PUBLISH_DIR`）
- 删除 `THEME_*`（`THEME_INSTALL_DIR`, `THEME_DATA_DIR`, `THEME_STAGING_DIR`, `THEME_MARKET_PUBLISH_DIR`）
- 删除 `SYSTEM_UPDATE_LOG_DIR` 等已由容器内部默认处理的变量

保留核心变量：
- 数据库连接：`POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`
- 密钥与安全：`JWT_SECRET`, `COOKIE_SECRET`, `ENCRYPTION_KEY`, `ADMIN_PASSWORD`
- 端口设置：`USER_HTTP_PORT=8080`, `ADMIN_HTTP_PORT=8081`
- 生产域名（可选）：`FRONTEND_URL`, `ADMIN_FRONTEND_URL`, `SITE_URL`

### 3.4 GitHub Actions CI (`.github/workflows/build-image.yml`)
- 基于用户提供的模板：
  - `IMAGE_NAME: payincus`
  - `DOCKERFILE: Dockerfile`
  - 支持 `workflow_dispatch`、`tags: ["v*"]`、`branches: ["canary"]`
  - 多架构构建：`linux/amd64` 和 `linux/arm64`
  - 推送至 `ghcr.io`，若配置了 Secret 则推送至 Docker Hub
- 移除 PR #9 中遗留的硬编码 `aklibk` 的 `.github/workflows/docker-publish.yml`。

### 3.5 清理与回归
1. **删除临时分析文件**：
   - 删除 `.trae-html-share-packages/`
   - 删除 `docker-deploy-analysis/`
   - 删除 `test-bug-analysis/`
2. **README 修复**：
   - 将 Telegram 链接重置回官方群：`https://t.me/Payincus`
   - 更新 Docker Compose 章节为全新的单服务、postgres 18-alpine 与精简环境变量说明。
3. **守卫测试维护**：
   - 在 `server/scripts/test-split-deploy-config.ts` 中移除已过时的 `retiredDockerDeploymentFiles` 检查（允许 `Dockerfile`、`docker-compose.yml`、`.dockerignore` 存在于仓库根目录）。

---

## 4. 验证计划

1. **守卫与语法检查**：
   - 运行 `npx tsx server/scripts/test-split-deploy-config.ts` 确认断言通过。
   - 运行 `git status` 确认多余文件已彻底清理。
2. **配置与 Docker 语法校验**：
   - 校验 `Dockerfile` 语法与指令。
   - 校验 `docker-compose.yml` 语法（使用 `docker compose config`）。
   - 校验 GitHub Actions 工作流语法。
