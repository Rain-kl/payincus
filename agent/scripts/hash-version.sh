#!/usr/bin/env bash
# 计算 agent 包 hash 并与本地记录比对, 维护 UTC 日期版本 (YYMMDDHHMM)。
#
#   hash 依据   : 仅计算 git 会提交的 agent 文件(git ls-files --exclude-standard,
#                 自动遵循 .gitignore, 排除 agent/dist 等构建产物与忽略项)。
#                 覆盖已跟踪文件 + 未跟踪但未被忽略的文件(= 未来会进 git 的改动)。
#   version     : 记录不存在 -> 创建记录文件, version 取当前 UTC 时间;
#                 记录不变   -> 校验通过, 沿用旧 version;
#                 记录变更   -> 刷新 version 为当前 UTC 时间。
#
# 注意: CI 发布的实际版本以 agent/VERSION 为准(vMAJOR.MINOR.PATCH),
# 本脚本的 version 仅用于本地构建校验与记录。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# 可显式指定单个文件计算 hash(此时由调用者保证该文件应参与校验)。
# 默认: 所有 git 会提交的 agent 文件。
PACKAGE="${1:-}"
RECORD_FILE="${HASH_RECORD_FILE:-${AGENT_DIR}/.package-hash}"

compute_hash() {
  if [[ -n "${PACKAGE}" ]]; then
    local sha256_bin="sha256sum"
    if ! command -v sha256sum >/dev/null 2>&1; then
      sha256_bin="shasum -a 256"
    fi
    if [[ ! -f "${PACKAGE}" ]]; then
      echo "[agent-hash] error: package not found: ${PACKAGE}" >&2
      exit 1
    fi
    ${sha256_bin} "${PACKAGE}" | awk '{print $1}'
    return
  fi

  local files
  files="$(cd "${AGENT_DIR}" && git ls-files --cached --others --exclude-standard 2>/dev/null || true)"
  if [[ -z "${files}" ]]; then
    echo "[agent-hash] error: not a git repository or no tracked agent files" >&2
    exit 1
  fi

  # 记录文件自身按 .gitignore 规则被排除, 不会参与计算。
  printf '%s\n' "${files}" | LC_ALL=C sort | sha256sum | awk '{print $1}'
}

new_hash="$(compute_hash)"
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
if [[ "${new_version}" == "${old_version}" ]]; then
  echo "[agent-hash] agent 包已变更, 版本已刷新(同一分钟内, version 相同)"
else
  echo "[agent-hash] agent 包已变更, 版本已更新"
fi
echo "[agent-hash] version ${old_version} -> ${new_version}"
echo "[agent-hash] hash"
echo "[agent-hash]   ${old_hash}"
echo "[agent-hash] -> ${new_hash}"
