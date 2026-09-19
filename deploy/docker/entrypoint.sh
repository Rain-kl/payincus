#!/bin/bash
set -eo pipefail

export NGINX_CLIENT_MAX_BODY_SIZE="${NGINX_CLIENT_MAX_BODY_SIZE:-50m}"
export BACKEND_UPSTREAM="${BACKEND_UPSTREAM:-http://127.0.0.1:3001}"


if [ "${RUN_MIGRATIONS:-true}" = "true" ]; then
  echo "[entrypoint] running Prisma migrations..."
  pnpm --dir /app/server exec prisma migrate deploy
fi

if [ "${RUN_DATA_MIGRATIONS:-false}" = "true" ]; then
  echo "[entrypoint] running PayIncus data migrations..."
  pnpm --dir /app/server migrate:data
fi

CERT_DIR="${INCUDAL_INSTALL_DIR:-/opt/incudal}/server/certs"
mkdir -p "$CERT_DIR" /app/server/certs
if [ ! -f "$CERT_DIR/client.crt" ] || [ ! -f "$CERT_DIR/client.key" ]; then
  echo "[entrypoint] generating panel mTLS client certificate..."
  openssl req -x509 -newkey rsa:4096 \
    -keyout "$CERT_DIR/client.key" \
    -out "$CERT_DIR/client.crt" \
    -days 3650 -nodes \
    -subj "/CN=incudal-panel/O=PayIncus" 2>/dev/null
  chmod 600 "$CERT_DIR/client.key"
  chmod 644 "$CERT_DIR/client.crt"
fi
ln -sf "$CERT_DIR/client.crt" /app/server/certs/client.crt
ln -sf "$CERT_DIR/client.key" /app/server/certs/client.key

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
