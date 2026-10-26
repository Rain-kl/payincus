#!/usr/bin/env bash
# Node setup uses the Incus package and its terminal interface.
set -euo pipefail
[[ $# -eq 0 ]] || { echo 'Configure INCUS_NODE_CHANNEL=latest or canary.' >&2; exit 1; }
INJECT_PANEL_URL=""
INJECT_AGENT_INSTALL_TOKEN=""
export INCUDAL_PANEL_URL="${INJECT_PANEL_URL:-${INCUDAL_PANEL_URL:-}}"
export INCUDAL_AGENT_INSTALL_TOKEN="${INJECT_AGENT_INSTALL_TOKEN:-${INCUDAL_AGENT_INSTALL_TOKEN:-}}"
[[ $EUID -eq 0 ]] || { echo 'Run as root.' >&2; exit 1; }
channel=${INCUS_NODE_CHANNEL:-canary}
case "$channel" in latest) branch=main ;; canary) branch=canary ;; *) echo 'Expected latest or canary.' >&2; exit 1 ;; esac
umask 077
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
curl --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 120 \
  "https://raw.githubusercontent.com/Rain-kl/incus/$branch/scripts/payincus/install.sh" -o "$stage/install.sh"
bash "$stage/install.sh" "$channel"
