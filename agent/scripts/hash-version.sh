#!/usr/bin/env bash
# agent 变更守卫: 检测 agent 代码库是否已随发布版本更新。
#
#   hash 依据   : 仅计算 git 会提交的 agent 文件(git ls-files --exclude-standard,
#                 自动遵循 .gitignore, 排除 agent/dist 等构建产物与忽略项)。
#                 覆盖已跟踪文件 + 未跟踪但未被忽略的文件(= 未来会进 git 的改动)。
#   判定        : 记录不存在       -> 创建记录文件, 视为基线, 通过;
#                 代码不变         -> 校验通过, 沿用记录;
#                 代码变更         -> 若 agent/VERSION 已更新 -> 刷新记录, 通过;
#                                  若 agent/VERSION 未更新 -> 报错退出(exit 1),
#                                  提醒先递增版本, 防止发布后 agent 不更新。
#
# 注意: CI 发布的实际版本以 agent/VERSION 为准(vMAJOR.MINOR.PATCH),
# 本脚本维护的记录文件仅用于本地变更检测。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

RECORD_FILE="${HASH_RECORD_FILE:-${AGENT_DIR}/.package-hash}"
VERSION_FILE="${AGENT_DIR}/VERSION"

compute_code_hash() {
  local files
  files="$(cd "${AGENT_DIR}" && git ls-files --cached --others --exclude-standard -z | tr '\0' '\n' || true)"
  if [[ -z "${files}" ]]; then
    echo "[agent-hash] error: not a git repository or no tracked agent files" >&2
    exit 1
  fi

  # 记录文件自身按 .gitignore 规则被排除, 不会参与计算。
  # 对每个文件的内容单独做 sha256(顺序按路径稳定排序), 再拼接出总 hash。
  # 这样修改任意已跟踪文件的内容, 总 hash 必然变化。
  local combined_file
  combined_file="$(mktemp "${TMPDIR:-/tmp}/agent-hash.XXXXXX")"
  trap 'rm -f "${combined_file}"' RETURN

  local path
  while IFS= read -r path; do
    if [[ -f "${AGENT_DIR}/${path}" ]]; then
      (cd "${AGENT_DIR}" && sha256sum "${path}") >> "${combined_file}"
    fi
  done <<< "${files}"

  LC_ALL=C sort "${combined_file}" | sha256sum | awk '{print $1}'
}

read_current_version() {
  if [[ -f "${VERSION_FILE}" ]]; then
    tr -d '[:space:]' < "${VERSION_FILE}"
  fi
}

current_version="$(read_current_version)"
new_hash="$(compute_code_hash)"

if [[ ! -f "${RECORD_FILE}" ]]; then
  printf 'version=%s\nhash=%s\n' "${current_version}" "${new_hash}" > "${RECORD_FILE}"
  echo "[agent-hash] 记录不存在, 已创建基线: ${RECORD_FILE}"
  echo "[agent-hash] version=${current_version}"
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
  echo "[agent-hash] 校验通过: agent 代码没有变化 (hash 一致)"
  echo "[agent-hash] version=${current_version}"
  exit 0
fi

if [[ "${current_version}" == "${old_version}" ]]; then
  echo "[agent-hash] error: agent 代码已变更, 但 agent/VERSION 未更新 (${old_version})" >&2
  echo "[agent-hash] hint: 先递增 agent/VERSION, 否则发布的 agent 不会触发自动更新" >&2
  echo "[agent-hash] hint: 确认后重新运行 make agent-hash 更新记录" >&2
  exit 1
fi

printf 'version=%s\nhash=%s\n' "${current_version}" "${new_hash}" > "${RECORD_FILE}"
echo "[agent-hash] agent 代码已变更, 且 agent/VERSION 已更新 ${old_version} -> ${current_version}"
echo "[agent-hash] 记录已刷新"
