#!/usr/bin/env bash
set -euo pipefail

log() { echo -e "\033[1;32m[+] $1\033[0m"; }
warn() { echo -e "\033[1;33m[!] $1\033[0m"; }
error() { echo -e "\033[1;31m[-] $1\033[0m"; }

readonly CADDY_CONFIG_DIR="/etc/caddy"
readonly CADDYFILE="/etc/caddy/Caddyfile"
readonly CADDY_OVERRIDE_DIR="/etc/systemd/system/caddy.service.d"
readonly CADDY_OVERRIDE_FILE="/etc/systemd/system/caddy.service.d/override.conf"
# 旧安装残留路径（成功安装后清理，绝不提前删 —— 备份/回滚依据）
readonly OLD_AUTOSAVE_FILE="/var/lib/caddy/.config/caddy/autosave.json"
readonly OLD_CADDY_STORAGE_DIR="/var/lib/caddy/.local/share/caddy"

if [[ "$EUID" -ne 0 ]]; then error "Must run as root"; exit 1; fi
if ! command -v systemctl >/dev/null 2>&1; then error "systemctl is required"; exit 1; fi

if [[ -f /etc/os-release ]]; then
    source /etc/os-release
    if [[ "${ID:-}" != "ubuntu" && "${ID:-}" != "debian" ]]; then
        error "Only Ubuntu/Debian supported"
        exit 1
    fi
else
    error "Cannot detect OS"; exit 1
fi

OLD_SERVICE_ACTIVE=false
OLD_SERVICE_ENABLED=false
systemctl is-active --quiet caddy 2>/dev/null && OLD_SERVICE_ACTIVE=true
systemctl is-enabled --quiet caddy 2>/dev/null && OLD_SERVICE_ENABLED=true
CADDYFILE_NEW=""
OVERRIDE_NEW=""
CADDY_INSTALL_COMPLETE=false
ROLLBACK_HANDLED=false

# 成功安装后的残留清理：只在「新配置已生效」之后执行，绝不碰备份源。
cleanup_old_installation() {
    rm -f "$OLD_AUTOSAVE_FILE" 2>/dev/null || true
    rm -rf "$OLD_CADDY_STORAGE_DIR" 2>/dev/null || true
}

# EXIT trap：只在「未显式回滚」且「未完成」时，把服务还原到进入脚本前的状态。
cleanup_caddy_install() {
    local exit_status=$?
    rm -f "${CADDYFILE_NEW:-}" "${OVERRIDE_NEW:-}" 2>/dev/null || true
    if [[ "$exit_status" -ne 0 && "$CADDY_INSTALL_COMPLETE" != "true" && "$ROLLBACK_HANDLED" != "true" ]]; then
        set +e
        if [[ "$OLD_SERVICE_ENABLED" == "true" ]]; then
            systemctl enable caddy >/dev/null 2>&1 || true
        else
            systemctl disable caddy >/dev/null 2>&1 || true
        fi
        if [[ "$OLD_SERVICE_ACTIVE" == "true" ]]; then
            systemctl is-active --quiet caddy 2>/dev/null || systemctl start caddy >/dev/null 2>&1
        else
            systemctl stop caddy >/dev/null 2>&1 || true
        fi
    fi
}
trap cleanup_caddy_install EXIT

log "Installing Caddy Web Server & Dependencies..."

export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y -qq curl debian-keyring debian-archive-keyring apt-transport-https openssl >/dev/null

if ! command -v caddy &> /dev/null; then
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
    apt-get update -qq
    apt-get install -y -qq caddy >/dev/null
    log "Caddy installed successfully"
else
    log "Caddy already installed, updating..."
    apt-get install -y -qq caddy >/dev/null
fi

if ! id -u caddy >/dev/null 2>&1; then
    error "Caddy package installed but system user 'caddy' does not exist; refusing to continue"
    exit 1
fi

install -d -o root -g caddy -m 0750 "$CADDY_CONFIG_DIR"
install -d -o caddy -g caddy -m 0750 /var/log/caddy

log "Preparing and validating Caddy configuration..."
CADDYFILE_NEW=$(mktemp "$CADDY_CONFIG_DIR/.Caddyfile.new.XXXXXX")
install -d -o root -g root -m 0755 "$CADDY_OVERRIDE_DIR"
OVERRIDE_NEW=$(mktemp "$CADDY_OVERRIDE_DIR/.override.conf.new.XXXXXX")

# Caddy Admin API 仅绑定宿主机回环，无公网监听、无凭据、无证书。
# 面板经 Agent 反向隧道（HMAC 鉴权）访问 127.0.0.1:2019。
cat > "$CADDYFILE_NEW" <<EOF
{
    admin localhost:2019
    auto_https disable_redirects
}
EOF

cat > "$OVERRIDE_NEW" <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile
EOF

chown root:caddy "$CADDYFILE_NEW"
chmod 0640 "$CADDYFILE_NEW"
chown root:root "$OVERRIDE_NEW"
chmod 0644 "$OVERRIDE_NEW"

if ! caddy validate --config "$CADDYFILE_NEW" --adapter caddyfile; then
    error "New Caddy configuration is invalid; existing service was not changed"
    exit 1
fi

HAD_OLD_CADDYFILE=false
HAD_OLD_OVERRIDE=false
CADDYFILE_BACKUP=""
OVERRIDE_BACKUP=""
BACKUP_TS=$(date +%Y%m%d%H%M%S)
if [[ -e "$CADDYFILE" ]]; then
    HAD_OLD_CADDYFILE=true
    CADDYFILE_BACKUP="${CADDYFILE}.before-incudal.${BACKUP_TS}"
    cp -a "$CADDYFILE" "$CADDYFILE_BACKUP"
fi
if [[ -e "$CADDY_OVERRIDE_FILE" ]]; then
    HAD_OLD_OVERRIDE=true
    OVERRIDE_BACKUP="${CADDY_OVERRIDE_FILE}.before-incudal.${BACKUP_TS}"
    cp -a "$CADDY_OVERRIDE_FILE" "$OVERRIDE_BACKUP"
fi

SWITCH_OK=true
mv -f "$CADDYFILE_NEW" "$CADDYFILE" || SWITCH_OK=false
if [[ "$SWITCH_OK" == "true" ]]; then
    mv -f "$OVERRIDE_NEW" "$CADDY_OVERRIDE_FILE" || SWITCH_OK=false
fi
if [[ "$SWITCH_OK" == "true" ]] && ! systemctl daemon-reload; then SWITCH_OK=false; fi
if [[ "$SWITCH_OK" == "true" ]] && ! systemctl enable caddy >/dev/null 2>&1; then SWITCH_OK=false; fi
if [[ "$SWITCH_OK" == "true" ]] && ! systemctl restart caddy; then SWITCH_OK=false; fi

MAIN_PID=""
if [[ "$SWITCH_OK" == "true" ]]; then
    sleep 2
    MAIN_PID=$(systemctl show -p MainPID --value caddy 2>/dev/null || true)
    CADDY_UID=$(id -u caddy 2>/dev/null || true)
    if ! systemctl is-enabled --quiet caddy 2>/dev/null || \
       ! systemctl is-active --quiet caddy 2>/dev/null || \
       [[ ! "$MAIN_PID" =~ ^[1-9][0-9]*$ ]] || \
       [[ -z "$CADDY_UID" || ! -d "/proc/${MAIN_PID}" ]] || \
       [[ "$(stat -c %u "/proc/${MAIN_PID}" 2>/dev/null || true)" != "$CADDY_UID" ]]; then
        SWITCH_OK=false
    fi
fi

# 探活回环 Admin API（无凭据）
if [[ "$SWITCH_OK" == "true" ]]; then
    if ! curl -fsS --max-time 5 http://127.0.0.1:2019/config/ >/dev/null 2>&1; then
        error "Caddy Admin API on 127.0.0.1:2019 is not responding"
        SWITCH_OK=false
    fi
fi

if [[ "$SWITCH_OK" != "true" ]]; then
    error "Caddy failed enabled/active/admin-probe validation; restoring previous configuration"
    systemctl stop caddy >/dev/null 2>&1 || true
    RESTORE_OK=true
    if [[ "$HAD_OLD_CADDYFILE" == "true" && -e "$CADDYFILE_BACKUP" ]]; then
        mv -f "$CADDYFILE_BACKUP" "$CADDYFILE" || RESTORE_OK=false
    else
        rm -f "$CADDYFILE" || RESTORE_OK=false
    fi
    if [[ "$HAD_OLD_OVERRIDE" == "true" && -e "$OVERRIDE_BACKUP" ]]; then
        mv -f "$OVERRIDE_BACKUP" "$CADDY_OVERRIDE_FILE" || RESTORE_OK=false
    else
        rm -f "$CADDY_OVERRIDE_FILE" || RESTORE_OK=false
    fi
    systemctl daemon-reload >/dev/null 2>&1 || RESTORE_OK=false
    if [[ "$OLD_SERVICE_ENABLED" == "true" ]]; then
        systemctl enable caddy >/dev/null 2>&1 || RESTORE_OK=false
    else
        systemctl disable caddy >/dev/null 2>&1 || true
    fi
    if [[ "$OLD_SERVICE_ACTIVE" == "true" ]]; then
        systemctl restart caddy >/dev/null 2>&1 || RESTORE_OK=false
        systemctl is-active --quiet caddy 2>/dev/null || RESTORE_OK=false
    else
        systemctl stop caddy >/dev/null 2>&1 || true
    fi
    if [[ "$RESTORE_OK" == "true" ]]; then
        error "New Caddy configuration failed; previous configuration and service state were restored"
    else
        error "Automatic rollback was incomplete; preserved backup paths must be inspected immediately"
    fi
    ROLLBACK_HANDLED=true
    exit 1
fi

CADDY_INSTALL_COMPLETE=true
log "Caddy installation complete!"
cleanup_old_installation
if [[ -n "$CADDYFILE_BACKUP" ]]; then
    warn "Previous Caddyfile backup preserved at: ${CADDYFILE_BACKUP}"
fi
echo ""
echo -e "\033[1;36m========================================\033[0m"
echo -e "\033[1;36m  Caddy Reverse Proxy Ready\033[0m"
echo -e "\033[1;36m========================================\033[0m"
echo -e "\033[1;33m  Admin API:    http://127.0.0.1:2019 (loopback only)\033[0m"
echo -e "\033[1;33m  Auth:         none (trust via Agent HMAC tunnel)\033[0m"
echo -e "\033[1;33m  Persistence:  Config file (--config mode)\033[0m"
echo -e "\033[1;36m========================================\033[0m"
echo ""
