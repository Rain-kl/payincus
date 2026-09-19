# Docker 单服务部署与 CI 调整实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 PayIncus Docker 部署改造成单服务前后端合一架构，升级数据库为 PostgreSQL 18-alpine，精简无用环境变量，并配置基于用户模板的 GitHub Actions 多架构镜像构建 CI。

**Architecture:** 单个 `payincus` Docker 容器内集成 Node.js Fastify 后端（监听 `127.0.0.1:3001`）与轻量 Nginx（监听 80 用户端与 81 管理端），保证两端独立源不共享 Cookie 域；Compose 仅保留 `postgres`（18-alpine）与 `payincus` 两个服务。

**Tech Stack:** Docker, Docker Compose, Nginx, Node.js 22, Fastify, Prisma, PostgreSQL 18, GitHub Actions.

## Global Constraints

- **私有后端单实例**：私有后端 `127.0.0.1:3001` 必须且只能运行 1 个 Node.js 进程实例。
- **双端 Cookie 域隔离**：用户端与管理端必须通过不同端口或独立源暴露，不可挂载在同一端口同一源下。
- **硬规矩遵循**：绝不执行 `git push`，绝不泄露凭据。

---

### Task 1: 清理 PR #9 遗留的临时分析文件与冗余配置

**Files:**
- Delete: `.trae-html-share-packages/`
- Delete: `docker-deploy-analysis/`
- Delete: `test-bug-analysis/`
- Delete: `.github/workflows/docker-publish.yml`
- Delete: `deploy/docker/build-images.ps1`
- Delete: `docker-compose.prod.yml`

- [x] **Step 1: 删除 Trae 临时分析产物与冗余文件**
```bash
rm -rf .trae-html-share-packages docker-deploy-analysis test-bug-analysis .github/workflows/docker-publish.yml deploy/docker/build-images.ps1 docker-compose.prod.yml
```

- [x] **Step 2: 验证文件已清理**
```bash
git status --short
```

---

### Task 2: 编写单容器 Dockerfile、Nginx 模板与入口脚本

**Files:**
- Modify: `Dockerfile`
- Modify: `deploy/docker/nginx.conf.template`
- Create/Modify: `deploy/docker/backend-entrypoint.sh`

- [x] **Step 1: 编写 `deploy/docker/nginx.conf.template`**
配置 80 端口（用户端）与 81 端口（管理端），分别托管 SPA 并反代 `/api` 与 `/api/ws` 到 `http://127.0.0.1:3001`。
```nginx
server {
  listen 80;
  server_name _;

  root /usr/share/nginx/html/user;
  index index.html;

  client_max_body_size ${NGINX_CLIENT_MAX_BODY_SIZE};

  location /api/ws/ {
    proxy_pass http://127.0.0.1:3001/api/ws/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_buffering off;
  }

  location /api/ {
    proxy_pass http://127.0.0.1:3001/api/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
  }

  location / {
    try_files $uri $uri/ /index.html;
  }
}

server {
  listen 81;
  server_name _;

  root /usr/share/nginx/html/admin;
  index index.html;

  client_max_body_size ${NGINX_CLIENT_MAX_BODY_SIZE};

  location /api/ws/ {
    proxy_pass http://127.0.0.1:3001/api/ws/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_buffering off;
  }

  location /api/ {
    proxy_pass http://127.0.0.1:3001/api/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
  }

  location / {
    try_files $uri $uri/ /index.html;
  }
}
```

- [x] **Step 2: 编写 `deploy/docker/backend-entrypoint.sh`**
处理数据库迁移、启动 Node 后端、启动 Nginx，并处理信号捕获：
```bash
#!/bin/bash
set -eo pipefail

export NGINX_CLIENT_MAX_BODY_SIZE="${NGINX_CLIENT_MAX_BODY_SIZE:-50m}"

if [ "${RUN_MIGRATIONS:-true}" = "true" ]; then
  echo "[entrypoint] running Prisma migrations..."
  pnpm --dir /app/server exec prisma migrate deploy
fi

if [ "${RUN_DATA_MIGRATIONS:-false}" = "true" ]; then
  echo "[entrypoint] running PayIncus data migrations..."
  pnpm --dir /app/server migrate:data
fi

echo "[entrypoint] configuring nginx..."
envsubst '${NGINX_CLIENT_MAX_BODY_SIZE}' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

# 优雅退出信号处理
cleanup() {
  echo "[entrypoint] stopping services..."
  if [ -n "$NODE_PID" ]; then
    kill -TERM "$NODE_PID" 2>/dev/null || true
  fi
  nginx -s quit 2>/dev/null || true
  wait "$NODE_PID" 2>/dev/null || true
  exit 0
}
trap cleanup SIGTERM SIGINT

echo "[entrypoint] starting PayIncus backend..."
node /app/server/dist/app.js &
NODE_PID=$!

echo "[entrypoint] starting Nginx..."
nginx -g "daemon off;" &
NGINX_PID=$!

# 等待任一主进程退出
wait -n "$NODE_PID" "$NGINX_PID"
```

- [x] **Step 3: 编写单目标 `Dockerfile`**
多阶段构建：build 阶段构建前后端；runtime 阶段安装 nginx + dumb-init，拷贝产物，暴露 80 和 81。
```dockerfile
# syntax=docker/dockerfile:1.7

ARG NODE_VERSION=22-bookworm-slim

FROM node:${NODE_VERSION} AS base
ENV PNPM_HOME=/pnpm
ENV PATH=$PNPM_HOME:$PATH
RUN corepack enable && corepack prepare pnpm@9.14.2 --activate
WORKDIR /app

FROM base AS deps
ENV NODE_ENV=development
ENV DATABASE_URL=postgresql://payincus:payincus@postgres:5432/payincus
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY server/package.json ./server/package.json
COPY server/prisma.config.ts ./server/prisma.config.ts
COPY server/prisma ./server/prisma
COPY client/package.json ./client/package.json
RUN pnpm install --frozen-lockfile

FROM deps AS build
ARG VITE_API_BASE_URL=/api
ARG VITE_CUSTOMER_BASE_URL=http://localhost:8080
ARG VITE_ADMIN_BASE_URL=http://localhost:8081
ENV VITE_API_BASE_URL=${VITE_API_BASE_URL}
ENV VITE_CUSTOMER_BASE_URL=${VITE_CUSTOMER_BASE_URL}
ENV VITE_ADMIN_BASE_URL=${VITE_ADMIN_BASE_URL}
COPY . .
RUN pnpm --filter client build && pnpm --filter server build && pnpm --filter server exec prisma generate

FROM node:${NODE_VERSION} AS runtime
ENV NODE_ENV=production
ENV HOST=127.0.0.1
ENV PORT=3001
ENV SERVE_STATIC_CLIENT=false
ENV INCUDAL_APP_DIR=/app
ENV INCUDAL_INSTALL_DIR=/opt/incudal
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@9.14.2 --activate
RUN apt-get update \
  && apt-get install -y --no-install-recommends openssl ca-certificates nginx dumb-init gettext-base \
  && rm -rf /var/lib/apt/lists/* \
  && mkdir -p /opt/incudal /etc/nginx/templates /etc/nginx/conf.d

COPY --from=build /app/package.json /app/pnpm-lock.yaml /app/pnpm-workspace.yaml ./
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/server/package.json ./server/package.json
COPY --from=build /app/server/node_modules ./server/node_modules
COPY --from=build /app/server/dist ./server/dist
COPY --from=build /app/server/prisma ./server/prisma
COPY --from=build /app/server/prisma.config.ts ./server/prisma.config.ts
COPY --from=build /app/server/templates ./server/templates
COPY --from=build /app/server/certs ./server/certs

COPY --from=build /app/client/dist/user /usr/share/nginx/html/user
COPY --from=build /app/client/dist/admin /usr/share/nginx/html/admin
COPY deploy/docker/nginx.conf.template /etc/nginx/templates/default.conf.template
COPY deploy/docker/backend-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 80 81

HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:3001/api/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/usr/local/bin/entrypoint.sh"]
```

---

### Task 3: 调整 `docker-compose.yml` 与精简 `.env.docker.example`

**Files:**
- Modify: `docker-compose.yml`
- Modify: `.env.docker.example`

- [x] **Step 1: 更新 `docker-compose.yml`**
仅保留 `postgres`（18-alpine）与 `payincus` 单一综合服务：
```yaml
services:
  postgres:
    image: postgres:18-alpine
    restart: unless-stopped
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-payincus}
      POSTGRES_USER: ${POSTGRES_USER:-payincus}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env.docker}
      PGDATA: /var/lib/postgresql/data
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
      interval: 10s
      timeout: 5s
      retries: 10

  payincus:
    build:
      context: .
      dockerfile: Dockerfile
    image: payincus:latest
    restart: unless-stopped
    env_file:
      - .env.docker
    environment:
      NODE_ENV: production
      DATABASE_URL: postgresql://${POSTGRES_USER:-payincus}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB:-payincus}
      INCUDAL_APP_DIR: /app
      INCUDAL_INSTALL_DIR: /opt/incudal
    depends_on:
      postgres:
        condition: service_healthy
    volumes:
      - payincus-data:/opt/incudal
    ports:
      - "${USER_HTTP_PORT:-8080}:80"
      - "${ADMIN_HTTP_PORT:-8081}:81"

volumes:
  postgres-data:
  payincus-data:
```

- [x] **Step 2: 精简 `.env.docker.example`**
移除所有 `REDIS_*`、`PLUGIN_*`、`THEME_*` 等内部变量，保持最小配置集。

---

### Task 4: 适配 GitHub Actions CI 工作流 (`build-image.yml`)

**Files:**
- Modify: `.github/workflows/build-image.yml`

- [x] **Step 1: 修改 `build-image.yml`**
  - `IMAGE_NAME: payincus`
  - `DOCKERFILE: Dockerfile`
  - 确认构建参数与多架构配置。

- [x] **Step 2: 校验工作流文件格式**
使用 yml 语法检查。

---

### Task 5: 恢复 `README.md` 官方群链接与更新 Docker 说明

**Files:**
- Modify: `README.md`

- [x] **Step 1: 将 Telegram 链接恢复为 `https://t.me/Payincus`**
- [x] **Step 2: 更新 Docker Compose 使用说明，体现单容器模式与 `postgres:18-alpine`**

---

### Task 6: 维护守卫测试 `test-split-deploy-config.ts` 并完成全量验证

**Files:**
- Modify: `server/scripts/test-split-deploy-config.ts`

- [x] **Step 1: 调整 `retiredDockerDeploymentFiles` 检查**
移除已重新支持的 Docker 根目录文件断言。
- [x] **Step 2: 运行测试验证**
运行 `npx tsx server/scripts/test-split-deploy-config.ts` 确保守卫绿灯。
- [x] **Step 3: 运行 `docker compose config` 验证 Compose 配置语法**
确保 Compose 语法无误。
