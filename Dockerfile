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
ENV PNPM_HOME=/pnpm
ENV PATH=$PNPM_HOME:$PATH
WORKDIR /app

RUN corepack enable && corepack prepare pnpm@9.14.2 --activate
RUN apt-get update \
  && apt-get install -y --no-install-recommends openssl ca-certificates nginx dumb-init gettext-base \
  && rm -rf /var/lib/apt/lists/* /etc/nginx/sites-enabled/* /etc/nginx/sites-available/* \
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
COPY deploy/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 80 81

HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
  CMD node -e "fetch('http://127.0.0.1:3001/api/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/usr/local/bin/entrypoint.sh"]
