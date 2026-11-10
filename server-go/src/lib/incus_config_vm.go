// Port of server/src/lib/incus-config-vm.ts (the VM cloud-init generator) plus
// the deterministic MAC derivation from lib/vm_network_identifiers.go.
//
// Byte-exactness contract (same as the container generator in
// incus_config_generator.go): user-data and network-config are compared with the
// Node output by SHA-256 in incus_config_vm_test.go. cloud-init YAML and the shell
// script embedded in it are whitespace sensitive — every indentation level, blank
// line and non-ASCII comment in the templates below is load-bearing. The expected
// hashes were produced with tsx against the real TypeScript module; do not
// "reformat" the raw strings.
//
// Where the TypeScript differs from the container generator (these are deliberate
// upstream behaviours, not port artefacts):
//
//   - DNS defaults apply when the list is *empty*, not only when it is absent
//     (`net.dns && net.dns.length > 0`), unlike the container's `net.dns ||`.
//   - The routed IPv6 gateway is always the link-local fe80::1; `ipv6Gateway` is
//     accepted but never read.
//   - A static network needs BOTH ipAddress and gateway, otherwise DHCP is emitted.
//   - Detection order is ubuntu → debian/kali → alpine → rhel family → linux, and
//     the rhel8 pattern includes `rhel.*8` (the container pattern does not).
//
// The router (GenerateIncusConfig) passes the container params object and never
// forwards instanceIdSeed, so a VM created through the router derives its MACs
// from the instance name. Direct callers (instance provisioning, rebuilds,
// migrations) do pass incus_id as the seed, which is why
// IncusConfigVMParams carries InstanceIDSeed.
package lib

import (
	"regexp"
	"strings"
)

// VM generator constants, mirroring the TypeScript module constants.
const (
	// IncusConfigVMDefaultIPv6Gateway is the link-local gateway Incus installs for
	// routed IPv6; the generator hardcodes it in both netplan and the guard script.
	IncusConfigVMDefaultIPv6Gateway = "fe80::1"
	// IncusConfigVMSshdPrimaryDropinPath is the 99- file, which sorts last and
	// therefore wins over cloud-init's drop-ins.
	IncusConfigVMSshdPrimaryDropinPath = "/etc/ssh/sshd_config.d/99-incudal-force.conf"
	// IncusConfigVMSshdLegacyDropinPath is a drop-in an earlier release wrote; it is
	// removed because it may carry `PasswordAuthentication no`.
	IncusConfigVMSshdLegacyDropinPath = "/etc/ssh/sshd_config.d/00-incudal-auth.conf"
)

// The VM resolver defaults. The IPv4 entry points at the bridge dnsmasq gateway
// (same as the container generator) so pure-IPv6 hosts keep DNS64 working.
var (
	incusConfigVMDefaultDNSV4 = []string{"10.10.0.1"}
	incusConfigVMDefaultDNSV6 = []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
)

// ==================== Types ====================

// IncusConfigVMOSFamily mirrors VmOsFamily.
type IncusConfigVMOSFamily string

// The VM families. Note 'linux' is the catch-all (not 'other').
const (
	IncusConfigVMOSFamilyUbuntu IncusConfigVMOSFamily = "ubuntu"
	IncusConfigVMOSFamilyDebian IncusConfigVMOSFamily = "debian"
	IncusConfigVMOSFamilyRhel   IncusConfigVMOSFamily = "rhel"
	IncusConfigVMOSFamilyRhel8  IncusConfigVMOSFamily = "rhel8"
	IncusConfigVMOSFamilyAlpine IncusConfigVMOSFamily = "alpine"
	IncusConfigVMOSFamilyLinux  IncusConfigVMOSFamily = "linux"
)

// IncusConfigVMPackageManager mirrors PackageManager.
type IncusConfigVMPackageManager string

// Package managers the generator knows how to install with.
const (
	IncusConfigVMPackageManagerApt IncusConfigVMPackageManager = "apt"
	IncusConfigVMPackageManagerDnf IncusConfigVMPackageManager = "dnf"
	IncusConfigVMPackageManagerYum IncusConfigVMPackageManager = "yum"
	IncusConfigVMPackageManagerApk IncusConfigVMPackageManager = "apk"
)

// IncusConfigVMInitSystem mirrors InitSystem.
type IncusConfigVMInitSystem string

// Init systems that change the serial-console and service-enable blocks.
const (
	IncusConfigVMInitSystemSystemd IncusConfigVMInitSystem = "systemd"
	IncusConfigVMInitSystemOpenRC  IncusConfigVMInitSystem = "openrc"
)

// IncusConfigVMOsInfo mirrors VmOsInfo.
type IncusConfigVMOsInfo struct {
	Family         IncusConfigVMOSFamily
	PackageManager IncusConfigVMPackageManager
	InitSystem     IncusConfigVMInitSystem
	// SSHServiceName is 'ssh' on Debian/Ubuntu, 'sshd' everywhere else.
	SSHServiceName string
	DefaultShell   string
	DefaultUsers   []string
}

// IncusConfigVMParams mirrors the VM module's own IncusConfigParams (it has an
// instanceIdSeed field the container params do not).
type IncusConfigVMParams struct {
	InstanceName string
	ImageAlias   string
	RootPassword string
	SSHKey       *string
	// Network is nil when the panel wants DHCP. It is the same shape the container
	// generator accepts, because the router forwards its own network object.
	Network            *IncusConfigStaticNetwork
	ExtraShellCommands string
	// InstanceIDSeed overrides the MAC seed. Empty means "use InstanceName".
	InstanceIDSeed string
	// NetworkMode is carried for signature parity; the VM generator never reads it
	// (routed IPv6 is detected from network.ipv6Address instead).
	NetworkMode string
}

// ==================== Utilities ====================

// incusConfigVMIndentString mirrors indentString: prepend `count` spaces to every
// non-empty line. Empty lines stay empty so the YAML block scalar keeps them as
// empty lines rather than whitespace-only ones.
func incusConfigVMIndentString(value string, count int) string {
	indent := strings.Repeat(" ", count)
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if line != "" {
			lines[index] = indent + line
		}
	}
	return strings.Join(lines, "\n")
}

// incusConfigVMRhel8Pattern mirrors /alma.*8|rocky.*8|oracle.*8|centos.*8|rhel.*8/i.
// The container generator's pattern deliberately omits the `rhel.*8` alternative.
var incusConfigVMRhel8Pattern = regexp.MustCompile(`(?i)alma.*8|rocky.*8|oracle.*8|centos.*8|rhel.*8`)

// incusConfigVMIsRhel8Series mirrors isRhel8Series.
func incusConfigVMIsRhel8Series(alias string) bool {
	return incusConfigVMRhel8Pattern.MatchString(alias)
}

// incusConfigVMUniqueUsers mirrors uniqueUsers: drop falsy entries and duplicates,
// preserving first-seen order.
func incusConfigVMUniqueUsers(users []string) []string {
	seen := make(map[string]struct{}, len(users))
	unique := make([]string, 0, len(users))
	for _, user := range users {
		if user == "" {
			continue
		}
		if _, exists := seen[user]; exists {
			continue
		}
		seen[user] = struct{}{}
		unique = append(unique, user)
	}
	return unique
}

// incusConfigVMDetectOSInfo mirrors detectVmOsInfo. The order of the branches is
// load-bearing: `kali` is matched by the debian branch, and the RHEL family is
// only consulted after alpine.
func incusConfigVMDetectOSInfo(imageAlias string) IncusConfigVMOsInfo {
	alias := strings.ToLower(imageAlias)

	if strings.Contains(alias, "ubuntu") {
		return IncusConfigVMOsInfo{
			Family:         IncusConfigVMOSFamilyUbuntu,
			PackageManager: IncusConfigVMPackageManagerApt,
			InitSystem:     IncusConfigVMInitSystemSystemd,
			SSHServiceName: "ssh",
			DefaultShell:   "/bin/bash",
			DefaultUsers:   []string{"root", "ubuntu"},
		}
	}

	if strings.Contains(alias, "debian") || strings.Contains(alias, "kali") {
		return IncusConfigVMOsInfo{
			Family:         IncusConfigVMOSFamilyDebian,
			PackageManager: IncusConfigVMPackageManagerApt,
			InitSystem:     IncusConfigVMInitSystemSystemd,
			SSHServiceName: "ssh",
			DefaultShell:   "/bin/bash",
			DefaultUsers:   []string{"root", "debian"},
		}
	}

	if strings.Contains(alias, "alpine") {
		return IncusConfigVMOsInfo{
			Family:         IncusConfigVMOSFamilyAlpine,
			PackageManager: IncusConfigVMPackageManagerApk,
			InitSystem:     IncusConfigVMInitSystemOpenRC,
			SSHServiceName: "sshd",
			DefaultShell:   "/bin/ash",
			DefaultUsers:   []string{"root"},
		}
	}

	if strings.Contains(alias, "alma") || strings.Contains(alias, "rocky") ||
		strings.Contains(alias, "oracle") || strings.Contains(alias, "centos") ||
		strings.Contains(alias, "rhel") || strings.Contains(alias, "fedora") {
		detectedUser := "cloud-user"
		switch {
		case strings.Contains(alias, "alma"):
			detectedUser = "almalinux"
		case strings.Contains(alias, "rocky"):
			detectedUser = "rocky"
		case strings.Contains(alias, "oracle"):
			detectedUser = "opc"
		case strings.Contains(alias, "centos"):
			detectedUser = "centos"
		case strings.Contains(alias, "fedora"):
			detectedUser = "fedora"
		}

		family := IncusConfigVMOSFamilyRhel
		packageManager := IncusConfigVMPackageManagerDnf
		if incusConfigVMIsRhel8Series(alias) {
			family = IncusConfigVMOSFamilyRhel8
			packageManager = IncusConfigVMPackageManagerYum
		}

		return IncusConfigVMOsInfo{
			Family:         family,
			PackageManager: packageManager,
			InitSystem:     IncusConfigVMInitSystemSystemd,
			SSHServiceName: "sshd",
			DefaultShell:   "/bin/bash",
			DefaultUsers:   incusConfigVMUniqueUsers([]string{"root", "cloud-user", detectedUser}),
		}
	}

	return IncusConfigVMOsInfo{
		Family:         IncusConfigVMOSFamilyLinux,
		PackageManager: IncusConfigVMPackageManagerApt,
		InitSystem:     IncusConfigVMInitSystemSystemd,
		SSHServiceName: "sshd",
		DefaultShell:   "/bin/bash",
		DefaultUsers:   []string{"root"},
	}
}

// incusConfigVMNormalizeIPv6Address mirrors normalizeIpv6Address: trim, and append
// /128 when no prefix length is present.
func incusConfigVMNormalizeIPv6Address(address string) string {
	trimmed := strings.TrimSpace(address)
	if strings.Contains(trimmed, "/") {
		return trimmed
	}
	return trimmed + "/128"
}

// incusConfigVMGetPackageInstallCommand mirrors getPackageInstallCommand.
func incusConfigVMGetPackageInstallCommand(osInfo IncusConfigVMOsInfo) string {
	switch osInfo.PackageManager {
	case IncusConfigVMPackageManagerApt:
		return `retry_cmd "DEBIAN_FRONTEND=noninteractive apt-get update" && retry_cmd "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends qemu-guest-agent openssh-server curl ca-certificates"`
	case IncusConfigVMPackageManagerDnf:
		return `retry_cmd "dnf install -y qemu-guest-agent openssh-server curl ca-certificates"`
	case IncusConfigVMPackageManagerYum:
		return `retry_cmd "yum install -y qemu-guest-agent openssh-server curl ca-certificates"`
	case IncusConfigVMPackageManagerApk:
		return `retry_cmd "apk update" && retry_cmd "apk add --no-cache qemu-guest-agent openssh-server openssh-keygen curl ca-certificates shadow" || retry_cmd "apk add --no-cache qemu-guest-agent openssh curl ca-certificates shadow"`
	default:
		return ":"
	}
}

// incusConfigVMGetCloudInitPackages mirrors getCloudInitPackages. apk images get no
// package list (cloud-init would use the wrong manager), so package_update is false.
func incusConfigVMGetCloudInitPackages(osInfo IncusConfigVMOsInfo) []string {
	switch osInfo.PackageManager {
	case IncusConfigVMPackageManagerApt, IncusConfigVMPackageManagerDnf, IncusConfigVMPackageManagerYum:
		return []string{"qemu-guest-agent", "openssh-server", "curl", "ca-certificates"}
	default:
		return []string{}
	}
}

// incusConfigVMGetSshdDropInContent mirrors getSshdDropInContent.
func incusConfigVMGetSshdDropInContent(osInfo IncusConfigVMOsInfo) string {
	lines := []string{
		"PermitRootLogin yes",
		"PasswordAuthentication yes",
		"PubkeyAuthentication yes",
		"ChallengeResponseAuthentication no",
	}
	if osInfo.Family != IncusConfigVMOSFamilyAlpine {
		lines = append(lines, "UsePAM yes")
	}
	return strings.Join(lines, "\n")
}

// incusConfigVMGetNetworkMatchYAML mirrors getNetworkMatchYaml.
func incusConfigVMGetNetworkMatchYAML(macAddress string) string {
	return `      macaddress: "` + strings.ToLower(macAddress) + `"`
}

// ==================== Setup script ====================

// incusConfigVMSetupScriptTemplate is the guest script written to
// /root/incudal-vm-setup.sh. It is a byte-for-byte copy of the TypeScript template
// literal with the interpolations replaced by @@TOKENS@@ (tokens are substituted
// with strings.Replacer, which does not rescan replacement text, so a password
// containing a token cannot inject anything).
//
// No backticks appear in this script, which is what allows a raw Go string.
const incusConfigVMSetupScriptTemplate = `#!/bin/sh
LOG_FILE="/var/log/incudal-vm-setup.log"
exec >>"$LOG_FILE" 2>&1
# 不启用 shell trace，避免 ROOT_PASSWORD/SSH_KEY 写入 guest 日志

SSH_KEY="@@SSH_KEY@@"
ROOT_PASSWORD='@@ROOT_PASSWORD@@'
DEFAULT_USERS="@@DEFAULT_USERS@@"
SSH_SERVICE="@@SSH_SERVICE@@"

retry_cmd() {
  command="$1"
  attempt=0
  while [ "$attempt" -lt 20 ]; do
    if sh -c "$command"; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 3
  done
  return 1
}

ensure_dns_fallback() {
  if [ ! -s /etc/resolv.conf ] || ! grep -q '^nameserver ' /etc/resolv.conf 2>/dev/null; then
    {
      # DNS 指向网桥 dnsmasq，兼容纯 IPv6 宿主机
      printf '%s\n' 'nameserver 10.10.0.1'
    } > /etc/resolv.conf
  fi
}

wait_for_network() {
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    if ip route show default >/dev/null 2>&1; then
      return 0
    fi
    if ping -c 1 -W 1 1.1.1.1 >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 2
  done
  return 1
}

user_home_dir() {
  user="$1"
  if [ "$user" = "root" ]; then
    echo "/root"
    return 0
  fi
  awk -F: -v target="$user" '$1 == target { print $6; exit }' /etc/passwd 2>/dev/null
}

set_password() {
  user="$1"
  id "$user" >/dev/null 2>&1 || return 0
  echo "$user:$ROOT_PASSWORD" | chpasswd >/dev/null 2>&1 || true
  passwd -u "$user" >/dev/null 2>&1 || usermod -U "$user" >/dev/null 2>&1 || true
}

set_authorized_key() {
  [ -n "$SSH_KEY" ] || return 0
  user="$1"
  id "$user" >/dev/null 2>&1 || return 0

  home_dir="$(user_home_dir "$user")"
  [ -n "$home_dir" ] || return 0

  mkdir -p "$home_dir/.ssh"
  chmod 700 "$home_dir/.ssh"
  printf '%s\n' "$SSH_KEY" > "$home_dir/.ssh/authorized_keys"
  chmod 600 "$home_dir/.ssh/authorized_keys"

  if [ "$user" = "root" ]; then
    chown root:root "$home_dir/.ssh" "$home_dir/.ssh/authorized_keys" 2>/dev/null || true
  else
    chown -R "$user":"$user" "$home_dir/.ssh" 2>/dev/null || true
  fi
}

ensure_include_directive() {
  config_path="/etc/ssh/sshd_config"
  [ -f "$config_path" ] || return 0

  grep -q '^Include /etc/ssh/sshd_config.d/\*\.conf$' "$config_path" && return 0

  tmp_path="$config_path.incudal.tmp"
  {
    printf '%s\n' 'Include /etc/ssh/sshd_config.d/*.conf'
    cat "$config_path"
  } > "$tmp_path" && mv "$tmp_path" "$config_path"
}

write_sshd_dropin() {
  mkdir -p /etc/ssh/sshd_config.d
  ensure_include_directive
  # 暴力清理各大云系统的防修改枷锁文件
  rm -f "@@SSHD_LEGACY_PATH@@" 2>/dev/null || true
  rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf 2>/dev/null || true
  rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf 2>/dev/null || true
  # 以 99 结尾占据排序优势
  cat > "@@SSHD_PRIMARY_PATH@@" <<'EOF'
@@SSHD_DROPIN_CONTENT@@
EOF
}

update_sshd_main_config() {
  config_path="/etc/ssh/sshd_config"
  [ -f "$config_path" ] || return 0

  # 极其暴力的 SED：不管前面有没有注释，只要包含这两个关键句语意，统统连根铲除或覆盖
  sed -i -E 's/^[#[:space:]]*PermitRootLogin.*/PermitRootLogin yes/' "$config_path" 2>/dev/null || true
  sed -i -E 's/^[#[:space:]]*PasswordAuthentication.*/PasswordAuthentication yes/' "$config_path" 2>/dev/null || true
  sed -i -E 's/^[#[:space:]]*PubkeyAuthentication.*/PubkeyAuthentication yes/' "$config_path" 2>/dev/null || true
  sed -i -E 's/^[#[:space:]]*ChallengeResponseAuthentication.*/ChallengeResponseAuthentication no/' "$config_path" 2>/dev/null || true

  # 有些极端的镜像会直接把 PasswordAuthentication no 写好几遍，甚至藏在别的 drop-in 里。
  # 我们强行剔除其他零散文件里的阻碍
  if ls /etc/ssh/sshd_config.d/*.conf 1> /dev/null 2>&1; then
      for f in /etc/ssh/sshd_config.d/*.conf; do
          if [ "$f" != "@@SSHD_PRIMARY_PATH@@" ]; then
              sed -i -E 's/^[#[:space:]]*(PasswordAuthentication|PermitRootLogin)[[:space:]]+no/# &/' "$f" 2>/dev/null || true
          fi
      done
  fi

  grep -q '^PermitRootLogin ' "$config_path" || printf '\n%s\n' 'PermitRootLogin yes' >> "$config_path"
  grep -q '^PasswordAuthentication ' "$config_path" || printf '%s\n' 'PasswordAuthentication yes' >> "$config_path"
  grep -q '^PubkeyAuthentication ' "$config_path" || printf '%s\n' 'PubkeyAuthentication yes' >> "$config_path"
  grep -q '^ChallengeResponseAuthentication ' "$config_path" || printf '%s\n' 'ChallengeResponseAuthentication no' >> "$config_path"

  # 无论如何暴力统一注入内置 internal-sftp 子系统来抵抗外部缺失包
  sed -i -E 's/^[#[:space:]]*Subsystem[[:space:]]+sftp.*/Subsystem sftp internal-sftp/' "$config_path" 2>/dev/null || true
  grep -q '^Subsystem sftp ' "$config_path" || printf '\n%s\n' 'Subsystem sftp internal-sftp' >> "$config_path"

  if [ "@@OS_FAMILY@@" != "alpine" ]; then
    sed -i -E 's/^[#[:space:]]*UsePAM.*/UsePAM yes/' "$config_path" 2>/dev/null || true
    grep -q '^UsePAM ' "$config_path" || printf '%s\n' 'UsePAM yes' >> "$config_path"
  fi
}

disable_firewall() {
  if command -v ufw >/dev/null 2>&1; then
    ufw disable || true
  fi

  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now firewalld 2>/dev/null || true
  fi
}

echo "Installing base packages..."
ensure_dns_fallback
wait_for_network || true
@@PACKAGE_INSTALL@@ || true

command -v update-ca-certificates >/dev/null 2>&1 && update-ca-certificates || true
command -v ssh-keygen >/dev/null 2>&1 && ssh-keygen -A || true

echo "Configuring SSH access..."
disable_firewall
write_sshd_dropin
update_sshd_main_config

for user in $DEFAULT_USERS; do
  set_password "$user"
  set_authorized_key "$user"
done

if command -v restorecon >/dev/null 2>&1; then
  restorecon -RF /root/.ssh /etc/ssh /home 2>/dev/null || true
fi

echo "Enabling serial console..."
@@SERIAL_CONSOLE@@

echo "Enabling guest services..."
@@SERVICE_ENABLE@@

echo "Installing persistent IPv4-only guard for primary NIC..."
cat > /usr/local/bin/incudal-ipv4-only.sh <<'EOF'
#!/bin/sh
set -e

PRIMARY_IFACE="$(ip -4 route show default 2>/dev/null | awk '/default/ {print $5; exit}')"
if [ -z "$PRIMARY_IFACE" ]; then
  PRIMARY_IFACE="$(ip route show default 2>/dev/null | awk '/default/ {for(i=1;i<=NF;i++) if($i=="dev") {print $(i+1); exit}}')"
fi
if [ -z "$PRIMARY_IFACE" ]; then
  PRIMARY_IFACE="eth0"
fi

mkdir -p /etc/sysctl.d
cat > /etc/sysctl.d/99-incudal-ipv4-only.conf <<CONF
net.ipv6.conf.${PRIMARY_IFACE}.disable_ipv6 = 1
net.ipv6.conf.${PRIMARY_IFACE}.accept_ra = 0
net.ipv6.conf.${PRIMARY_IFACE}.autoconf = 0
CONF

sysctl -p /etc/sysctl.d/99-incudal-ipv4-only.conf >/dev/null 2>&1 || true
ip -6 addr flush dev "${PRIMARY_IFACE}" scope global >/dev/null 2>&1 || true
ip -6 route del default dev "${PRIMARY_IFACE}" >/dev/null 2>&1 || true
EOF
chmod +x /usr/local/bin/incudal-ipv4-only.sh
/usr/local/bin/incudal-ipv4-only.sh || true

if command -v systemctl >/dev/null 2>&1; then
  cat > /etc/systemd/system/incudal-ipv4-only.service <<'EOF'
[Unit]
Description=Incudal primary NIC IPv4-only guard
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/incudal-ipv4-only.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload || true
  systemctl enable incudal-ipv4-only.service || true
  systemctl start incudal-ipv4-only.service || true
elif [ -d /etc/local.d ]; then
  cat > /etc/local.d/incudal-ipv4-only.start <<'EOF'
#!/bin/sh
/usr/local/bin/incudal-ipv4-only.sh || true
EOF
  chmod +x /etc/local.d/incudal-ipv4-only.start
  rc-update add local default 2>/dev/null || true
  /etc/local.d/incudal-ipv4-only.start || true
fi

@@IPV6_GUARD_BLOCK@@

echo "Running user custom commands..."
@@EXTRA_COMMANDS@@
`

// incusConfigVMIPv6GuardTemplate is the optional routed-IPv6 default-route guard.
// The TypeScript splices this whole block in (or nothing) at the placeholder above;
// note that it ends with a newline, which is why the IPv6 variant emits one blank
// line fewer than the IPv4-only variant.
const incusConfigVMIPv6GuardTemplate = `echo "Installing persistent IPv6 default-route guard..."
cat > /usr/local/bin/incudal-ipv6-route-guard.sh <<'EOF'
#!/bin/sh
set -e

TARGET_IPV6="@@PRIMARY_IPV6@@"
if [ -z "$TARGET_IPV6" ]; then
  exit 0
fi

IPV6_IFACE="$(ip -6 -o addr show scope global 2>/dev/null | awk -v target="$TARGET_IPV6" '$4 ~ ("^" target "/") {print $2; exit}')"
if [ -z "$IPV6_IFACE" ]; then
  exit 0
fi

if ! ip -6 route show default 2>/dev/null | grep -q "dev ${IPV6_IFACE}"; then
  ip -6 route replace default via fe80::1 dev "${IPV6_IFACE}" metric 100 onlink >/dev/null 2>&1 || true
fi
EOF
chmod +x /usr/local/bin/incudal-ipv6-route-guard.sh
/usr/local/bin/incudal-ipv6-route-guard.sh || true

if command -v systemctl >/dev/null 2>&1; then
  cat > /etc/systemd/system/incudal-ipv6-route-guard.service <<'EOF'
[Unit]
Description=Incudal IPv6 routed default-route guard
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/incudal-ipv6-route-guard.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload || true
  systemctl enable incudal-ipv6-route-guard.service || true
  systemctl start incudal-ipv6-route-guard.service || true
elif [ -d /etc/local.d ]; then
  cat > /etc/local.d/incudal-ipv6-route-guard.start <<'EOF'
#!/bin/sh
/usr/local/bin/incudal-ipv6-route-guard.sh || true
EOF
  chmod +x /etc/local.d/incudal-ipv6-route-guard.start
  rc-update add local default 2>/dev/null || true
  /etc/local.d/incudal-ipv6-route-guard.start || true
fi
`

// incusConfigVMSerialConsoleCommands mirrors the serialConsoleCommands ternary.
func incusConfigVMSerialConsoleCommands(osInfo IncusConfigVMOsInfo) string {
	if osInfo.InitSystem == IncusConfigVMInitSystemOpenRC {
		return `if [ -f /etc/inittab ] && ! grep -q '^ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100$' /etc/inittab 2>/dev/null; then
  echo 'ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100' >> /etc/inittab
fi
kill -HUP 1 2>/dev/null || true`
	}
	return `systemctl enable serial-getty@ttyS0.service || true
systemctl restart serial-getty@ttyS0.service || true`
}

// incusConfigVMServiceEnableCommands mirrors the serviceEnableCommands ternary.
func incusConfigVMServiceEnableCommands(osInfo IncusConfigVMOsInfo) string {
	sshService := osInfo.SSHServiceName
	if osInfo.InitSystem == IncusConfigVMInitSystemOpenRC {
		return `[ -f /etc/init.d/` + sshService + ` ] && rc-update add ` + sshService + ` default 2>/dev/null || true
[ -f /etc/init.d/` + sshService + ` ] && rc-service ` + sshService + ` restart 2>/dev/null || /etc/init.d/` + sshService + ` restart 2>/dev/null || /etc/init.d/` + sshService + ` start 2>/dev/null || true
[ -f /etc/init.d/qemu-guest-agent ] && rc-update add qemu-guest-agent default 2>/dev/null || true
[ -f /etc/init.d/qemu-guest-agent ] && rc-service qemu-guest-agent restart 2>/dev/null || /etc/init.d/qemu-guest-agent restart 2>/dev/null || /etc/init.d/qemu-guest-agent start 2>/dev/null || true`
	}
	return `systemctl daemon-reload || true
systemctl unmask ` + sshService + ` || true
systemctl enable ` + sshService + ` 2>/dev/null || true
systemctl restart ` + sshService + ` 2>/dev/null || true
systemctl enable qemu-guest-agent 2>/dev/null || true
systemctl restart qemu-guest-agent 2>/dev/null || true`
}

// incusConfigVMGenerateSetupScript mirrors generateSetupScript.
func incusConfigVMGenerateSetupScript(params IncusConfigVMParams, osInfo IncusConfigVMOsInfo) string {
	escapedKey := incusConfigEscapeForShellDoubleQuote(incusConfigDeref(params.SSHKey))
	escapedPassword := incusConfigEscapeForShellSingleQuote(params.RootPassword)
	usersList := strings.Join(osInfo.DefaultUsers, " ")

	// `params.extraShellCommands?.trim() || ': # No extra commands'` — only the
	// trimmed value is emitted (no trailing whitespace, no blank lines).
	extraCommands := strings.TrimSpace(params.ExtraShellCommands)
	if extraCommands == "" {
		extraCommands = ": # No extra commands"
	}

	hasRoutedIPv6 := params.Network != nil && params.Network.IPv6Address != ""
	escapedPrimaryIPv6 := ""
	if hasRoutedIPv6 {
		// trim().split('/')[0] — the guard only needs the bare address.
		escapedPrimaryIPv6 = incusConfigEscapeForShellDoubleQuote(
			strings.Split(strings.TrimSpace(params.Network.IPv6Address), "/")[0])
	}

	ipv6GuardBlock := ""
	if hasRoutedIPv6 {
		ipv6GuardBlock = strings.ReplaceAll(incusConfigVMIPv6GuardTemplate, "@@PRIMARY_IPV6@@", escapedPrimaryIPv6)
	}

	return strings.NewReplacer(
		"@@SSH_KEY@@", escapedKey,
		"@@ROOT_PASSWORD@@", escapedPassword,
		"@@DEFAULT_USERS@@", usersList,
		"@@SSH_SERVICE@@", osInfo.SSHServiceName,
		"@@SSHD_LEGACY_PATH@@", IncusConfigVMSshdLegacyDropinPath,
		"@@SSHD_PRIMARY_PATH@@", IncusConfigVMSshdPrimaryDropinPath,
		"@@SSHD_DROPIN_CONTENT@@", incusConfigVMGetSshdDropInContent(osInfo),
		"@@OS_FAMILY@@", string(osInfo.Family),
		"@@PACKAGE_INSTALL@@", incusConfigVMGetPackageInstallCommand(osInfo),
		"@@SERIAL_CONSOLE@@", incusConfigVMSerialConsoleCommands(osInfo),
		"@@SERVICE_ENABLE@@", incusConfigVMServiceEnableCommands(osInfo),
		"@@IPV6_GUARD_BLOCK@@", ipv6GuardBlock,
		"@@EXTRA_COMMANDS@@", extraCommands,
	).Replace(incusConfigVMSetupScriptTemplate)
}

// ==================== cloud-init: user-data ====================

// incusConfigVMUserDataTemplate is the cloud-config document. Placeholders are
// substituted once; the indented setup script (already 6-space indented) is spliced
// into the `content: |` block.
const incusConfigVMUserDataTemplate = `#cloud-config
package_update: @@PACKAGE_UPDATE@@
package_upgrade: false
hostname: @@HOSTNAME@@
manage_etc_hosts: true

@@PACKAGES_SECTION@@users:
  - default
  - name: root
    lock_passwd: false
    shell: @@DEFAULT_SHELL@@
@@SSH_KEY_SECTION@@
chpasswd:
  expire: false
  list:
    - 'root:@@YAML_PASSWORD@@'

ssh_pwauth: true
disable_root: false

write_files:
  - path: /root/incudal-vm-setup.sh
    permissions: '0755'
    owner: root:root
    content: |
@@INDENTED_SETUP@@

runcmd:
  - [ "/bin/sh", "-c", "/root/incudal-vm-setup.sh" ]
`

// incusConfigVMGenerateUserData mirrors generateUserData.
func incusConfigVMGenerateUserData(params IncusConfigVMParams, osInfo IncusConfigVMOsInfo) string {
	setupScriptContent := incusConfigVMGenerateSetupScript(params, osInfo)
	// escapeForYamlSingleQuote only doubles single quotes (no backslash handling).
	yamlSafePassword := strings.ReplaceAll(params.RootPassword, "'", "''")
	packages := incusConfigVMGetCloudInitPackages(osInfo)

	packageLines := make([]string, 0, len(packages))
	for _, pkg := range packages {
		packageLines = append(packageLines, "  - "+pkg)
	}
	packagesSection := ""
	if len(packages) > 0 {
		packagesSection = "packages:\n" + strings.Join(packageLines, "\n") + "\n\n"
	}

	sshKeySection := ""
	if params.SSHKey != nil && strings.TrimSpace(*params.SSHKey) != "" {
		sshKeySection = "    ssh_authorized_keys:\n      - " + *params.SSHKey + "\n"
	}

	packageUpdate := "false"
	if len(packages) > 0 {
		packageUpdate = "true"
	}

	return strings.NewReplacer(
		"@@PACKAGE_UPDATE@@", packageUpdate,
		"@@HOSTNAME@@", params.InstanceName,
		"@@PACKAGES_SECTION@@", packagesSection,
		"@@DEFAULT_SHELL@@", osInfo.DefaultShell,
		"@@SSH_KEY_SECTION@@", sshKeySection,
		"@@YAML_PASSWORD@@", yamlSafePassword,
		"@@INDENTED_SETUP@@", incusConfigVMIndentString(setupScriptContent, 6),
	).Replace(incusConfigVMUserDataTemplate)
}

// ==================== cloud-init: network-config ====================

// incusConfigVMGenerateDHCPNetworkConfig mirrors generateDhcpNetworkConfig.
func incusConfigVMGenerateDHCPNetworkConfig(nicMacs VMNetworkNicMacs) string {
	return `version: 2
ethernets:
  incus_eth0:
    match:
` + incusConfigVMGetNetworkMatchYAML(nicMacs.Eth0) + `
    dhcp4: true
    dhcp6: false
    accept-ra: false
    link-local: [ ipv4 ]
`
}

// incusConfigVMGenerateNetworkConfig mirrors generateNetworkConfig.
//
// A network record only takes effect when BOTH ipAddress and gateway are set;
// otherwise the instance is emitted as DHCP even if an IPv6 address is present.
func incusConfigVMGenerateNetworkConfig(network *IncusConfigStaticNetwork, nicMacs VMNetworkNicMacs) string {
	if network == nil || network.IPAddress == "" || network.Gateway == "" {
		return incusConfigVMGenerateDHCPNetworkConfig(nicMacs)
	}

	// VM semantics: an EMPTY list also falls back to the defaults
	// (`net.dns && net.dns.length > 0`), unlike the container's `net.dns ||`.
	dnsV4 := incusConfigVMDefaultDNSV4
	if len(network.DNS) > 0 {
		dnsV4 = network.DNS
	}
	dnsV6 := incusConfigVMDefaultDNSV6
	if len(network.IPv6DNS) > 0 {
		dnsV6 = network.IPv6DNS
	}

	dnsV4Lines := make([]string, 0, len(dnsV4))
	for _, dns := range dnsV4 {
		dnsV4Lines = append(dnsV4Lines, "        - "+dns)
	}

	ipv4Section := `  incus_eth0:
    match:
` + incusConfigVMGetNetworkMatchYAML(nicMacs.Eth0) + `
    dhcp4: false
    dhcp6: false
    accept-ra: false
    link-local: [ ipv4 ]
    addresses:
      - ` + network.IPAddress + `
    gateway4: ` + network.Gateway + `
    nameservers:
      addresses:
` + strings.Join(dnsV4Lines, "\n")

	if network.IPv6Address == "" {
		return `version: 2
ethernets:
` + ipv4Section + "\n"
	}

	dnsV6Lines := make([]string, 0, len(dnsV6))
	for _, dns := range dnsV6 {
		dnsV6Lines = append(dnsV6Lines, "        - "+dns)
	}

	ipv6Section := `  incus_eth1:
    match:
` + incusConfigVMGetNetworkMatchYAML(nicMacs.Eth1) + `
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - ` + incusConfigVMNormalizeIPv6Address(network.IPv6Address) + `
    routes:
      - to: ::/0
        via: ` + IncusConfigVMDefaultIPv6Gateway + `
        on-link: true
    nameservers:
      addresses:
` + strings.Join(dnsV6Lines, "\n")

	return `version: 2
ethernets:
` + ipv4Section + "\n" + ipv6Section + "\n"
}

// ==================== Entry point ====================

// IncusConfigGenerateVM mirrors generateVmConfig.
//
// The TypeScript never throws (no parameter validation), so the error return is
// always nil; it exists so the router seam can use the same shape as the container
// generator.
func IncusConfigGenerateVM(params IncusConfigVMParams) (IncusConfigPayload, error) {
	osInfo := incusConfigVMDetectOSInfo(params.ImageAlias)

	seed := params.InstanceIDSeed
	if seed == "" {
		seed = params.InstanceName
	}
	nicMacs := VMNetworkGenerateMacs(seed)

	return IncusConfigPayload{
		UserUserData:      incusConfigVMGenerateUserData(params, osInfo),
		UserNetworkConfig: incusConfigVMGenerateNetworkConfig(params.Network, nicMacs),
	}, nil
}

// init installs the VM generator into the router seam, which is what makes
// GenerateIncusConfig's 'virtual-machine' branch real instead of returning
// ErrIncusConfigVMGeneratorMissing.
//
// The adapter mirrors server/src/lib/incus-config-generator.ts exactly: it builds
// the VM params from the container params and drops instanceIdSeed, so a VM created
// through the router seeds its MACs from the instance name (callers that need the
// incus_id seed call IncusConfigGenerateVM directly).
func init() {
	IncusConfigGenerateVMFunc = func(params IncusConfigParams) (IncusConfigPayload, error) {
		return IncusConfigGenerateVM(IncusConfigVMParams{
			InstanceName:       params.InstanceName,
			ImageAlias:         params.ImageAlias,
			RootPassword:       params.RootPassword,
			SSHKey:             params.SSHKey,
			Network:            params.Network,
			ExtraShellCommands: params.ExtraShellCommands,
			NetworkMode:        params.NetworkMode,
		})
	}
}
