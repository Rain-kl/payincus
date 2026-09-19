#!/bin/bash
set -eo pipefail

export NGINX_CLIENT_MAX_BODY_SIZE="${NGINX_CLIENT_MAX_BODY_SIZE:-50m}"
export BACKEND_UPSTREAM="${BACKEND_UPSTREAM:-http://127.0.0.1:3001}"

if [ -n "${DATABASE_URL:-}" ]; then
  export DATABASE_URL="$(echo "$DATABASE_URL" | sed -E 's|@(127\.0\.0\.1|localhost):|@postgres:|')"
fi

if [ "${RUN_MIGRATIONS:-true}" = "true" ]; then
  echo "[entrypoint] running Prisma migrations..."
  pnpm --dir /app/server exec prisma migrate deploy
fi

if [ "${RUN_DATA_MIGRATIONS:-false}" = "true" ]; then
  echo "[entrypoint] running PayIncus data migrations..."
  pnpm --dir /app/server migrate:data
fi

echo "[entrypoint] configuring nginx..."
rm -rf /etc/nginx/sites-enabled/* /etc/nginx/sites-available/*
mkdir -p /etc/nginx/conf.d
envsubst '${NGINX_CLIENT_MAX_BODY_SIZE} ${BACKEND_UPSTREAM}' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

cleanup() {
  echo "[entrypoint] stopping services..."
  if [ -n "${NODE_PID:-}" ]; then
    kill -TERM "$NODE_PID" 2>/dev/null || true
  fi
  nginx -s quit 2>/dev/null || true
  if [ -n "${NODE_PID:-}" ]; then
    wait "$NODE_PID" 2>/dev/null || true
  fi
  exit 0
}
trap cleanup SIGTERM SIGINT

echo "[entrypoint] starting PayIncus backend..."
node /app/server/dist/app.js &
NODE_PID=$!

echo "[entrypoint] starting Nginx..."
nginx -g "daemon off;" &
NGINX_PID=$!

wait -n "$NODE_PID" "$NGINX_PID"
