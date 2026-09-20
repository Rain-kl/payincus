#!/usr/bin/env bash
# 计算 agent 包 hash 并与本地记录比对, 维护 UTC 日期版本 (YYMMDDHHMM)。
# 记录不存在 -> 创建记录文件, version 取当前 UTC 时间;
# hash 一致   -> 校验通过, 沿用旧 version;
# hash 变更   -> 刷新 version 为当前 UTC 时间并更新记录。
# 注意: CI 发布的实际版本以 agent/VERSION 为准(vMAJOR.MINOR.PATCH),
# 本脚本的 version 仅用于本地构建校验与记录。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_DIR="$(cd "${AGENT_DIR}/.." && pwd)"

DEFAULT_PACKAGE="${REPO_DIR}/agent/dist/manifest.json"
PACKAGE="${1:-${PACKAGE:-${DEFAULT_PACKAGE}}}"
RECORD_FILE="${HASH_RECORD_FILE:-${AGENT_DIR}/.package-hash}"

if [[ ! -f "${PACKAGE}" ]]; then
  echo "[agent-hash] error: package not found: ${PACKAGE}" >&2
  echo "[agent-hash] hint: build first (bash agent/scripts/build-release.sh)" >&2
  exit 1
fi

sha256_bin="sha256sum"
if ! command -v sha256sum >/dev/null 2>&1; then
  sha256_bin="shasum -a 256"
fi

new_hash="$(${sha256_bin} "${PACKAGE}" | awk '{print $1}')"
new_version="$(date -u +%y%m%d%H%M)"

if [[ ! -f "${RECORD_FILE}" ]]; then
  printf 'version=%s\nhash=%s\n' "${new_version}" "${new_hash}" > "${RECORD_FILE}"
  echo "[agent-hash] 记录不存在, 已创建: ${RECORD_FILE}"
  echo "[agent-hash] version=${new_version}"
  echo "[agent-hash] hash=${new_hash}"
  exit 0
fi

old_version="$(sed -n 's/^version=//p' "${RECORD_FILE}")"
old_hash="$(sed -n 's/^hash=//p' "${RECORD_FILE}")"

if [[ -z "${old_version}" || -z "${old_hash}" ]]; then
  echo "[agent-hash] error: record file malformed: ${RECORD_FILE}" >&2
  exit 1
fi

if [[ "${old_hash}" == "${new_hash}" ]]; then
  echo "[agent-hash] 校验通过: agent 包没有更新 (hash 一致)"
  echo "[agent-hash] version=${old_version}"
  echo "[agent-hash] hash=${new_hash}"
  exit 0
fi

printf 'version=%s\nhash=%s\n' "${new_version}" "${new_hash}" > "${RECORD_FILE}"
echo "[agent-hash] agent 包已变更, 版本已更新"
echo "[agent-hash] version ${old_version} -> ${new_version}"
echo "[agent-hash] hash"
echo "[agent-hash]   ${old_hash}"
echo "[agent-hash] -> ${new_hash}"
