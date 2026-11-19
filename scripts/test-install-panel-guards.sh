#!/usr/bin/env bash
set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=install-panel.sh
source "$REPO_ROOT/scripts/install-panel.sh"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

mkdir -p "$WORK_DIR/payload"
printf 'payincus installer guard\n' > "$WORK_DIR/payload/version.txt"

SAFE_ARCHIVE="$WORK_DIR/incudal-v0.0.0-linux-amd64.tar.gz"
tar -czf "$SAFE_ARCHIVE" -C "$WORK_DIR/payload" .
sha256sum "$SAFE_ARCHIVE" > "${SAFE_ARCHIVE}.sha256"

verify_release_checksum "$SAFE_ARCHIVE"
validate_release_archive "$SAFE_ARCHIVE"

printf '%064d  %s\n' 0 "$(basename "$SAFE_ARCHIVE")" > "${SAFE_ARCHIVE}.sha256"
if verify_release_checksum "$SAFE_ARCHIVE"; then
    echo "checksum guard accepted a mismatched archive" >&2
    exit 1
fi

UNSAFE_ARCHIVE="$WORK_DIR/unsafe.tar.gz"
if tar --help 2>&1 | grep -q -- '--transform'; then
    tar -czf "$UNSAFE_ARCHIVE" --transform='s#^#../#' -C "$WORK_DIR/payload" .
else
    python3 - "$UNSAFE_ARCHIVE" "$WORK_DIR/payload/version.txt" <<'PY'
import sys, tarfile
archive_path, src_path = sys.argv[1], sys.argv[2]
with tarfile.open(archive_path, "w:gz") as tf:
    tf.add(src_path, arcname="../version.txt")
PY
fi
if validate_release_archive "$UNSAFE_ARCHIVE"; then
    echo "archive guard accepted a parent traversal path" >&2
    exit 1
fi

grep -Fq -- '-f "${INSTALL_DIR}/current/server/incudal-server"' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq '检测到原子 OTA 布局' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq 'ExecStart=${app_dir}/server/incudal-server' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq 'ExecStart=/usr/local/libexec/incudal/incudal-online-task update %i' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq 'ExecStart=/usr/local/libexec/incudal/incudal-online-task rollback %i' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq 'chown "root:${RUN_USER}" "$ENV_FILE"' "$REPO_ROOT/scripts/install-panel.sh"
grep -Fq 'Defaults:${RUN_USER} secure_path=/usr/local/libexec/incudal:' "$REPO_ROOT/scripts/install-panel.sh"

if grep -Fq 'prisma migrate deploy' \
    "$REPO_ROOT/scripts/install-panel.sh" \
    "$REPO_ROOT/deploy/incudal-backend.service.example" \
    "$REPO_ROOT/scripts/migrate-ota-atomic-layout.sh"; then
    echo "systemd unit or installer still executes prisma migrate deploy separately" >&2
    exit 1
fi

for helper in \
    incudal-online-task.sh.example \
    incudal-systemctl-wrapper.sh.example \
    incudal-ota-chown-wrapper.sh.example; do
    bash -n "$REPO_ROOT/deploy/$helper"
done

if grep -Fq 'ExecStart=/usr/bin/node /opt/incudal/current/server/dist/scripts/' \
    "$REPO_ROOT/deploy/incudal-online-update@.service.example" \
    "$REPO_ROOT/deploy/incudal-online-rollback@.service.example"; then
    echo "root unit still executes a worker from current directly" >&2
    exit 1
fi

grep -Fq 'ExecStart=/opt/incudal/current/server/incudal-server' "$REPO_ROOT/deploy/incudal-backend.service.example"

echo "install-panel guards passed"
