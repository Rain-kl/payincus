// Port of server/src/lib/incus-config-generator.ts (the router) plus the
// container generator it delegates to in server/src/lib/incus-config-container.ts.
//
// Two things in this file are byte-sensitive and pinned by tests:
//
//   - the cloud-init / netplan documents we hand to Incus are compared with the
//     Node output by SHA-256 in incus_config_generator_test.go. A single space in
//     any of the shell templates below changes the hash, so the templates are kept
//     verbatim (including the trailing spaces on the blank lines of the retry
//     wrapper) and every non-ASCII comment survives.
//   - the JSON key order of configPayload / metaData. The TypeScript object
//     literals emit 'user.user-data' before 'user.network-config' and, for
//     metadata, sshPort → rootPassword → osFamily → imageAlias → instanceType →
//     ipAddress. Go marshals struct fields in declaration order, so the structs
//     here are declared in exactly that order; do not "tidy" them.
//
// The VM generator (server/src/lib/incus-config-vm.ts, 656 lines) is NOT ported:
// it depends on lib/vm-network-identifiers.ts for the deterministic MAC derivation,
// which is a separate module. The router still routes, but the VM branch fails
// loudly instead of silently returning a container payload. Note that the
// TypeScript router also drops `instanceIdSeed` when building the VM params; the
// seam signature mirrors the router's params, not the VM module's.
package lib

import (
	"crypto/rand"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// IncusConfigSSHPort is the fixed SSH port baked into both generators.
const IncusConfigSSHPort = 22

// Container generator constants (the TypeScript CONFIG object).
const (
	incusConfigNetworkRetryCount = 20
	incusConfigNetworkRetryDelay = 3
	incusConfigInterfaceName     = "eth0"
)

// incusConfigDefaultDNS points at the bridge dnsmasq gateway, which forwards
// upstream; pure-IPv6 hosts have DNS64 configured there.
var incusConfigDefaultDNS = []string{"10.10.0.1"}

// IncusConfigStaticNetwork mirrors StaticNetworkConfig (a panel-computed static
// address; absent fields are the zero value, which is what the TypeScript treats
// as falsy/undefined).
type IncusConfigStaticNetwork struct {
	// IPAddress is CIDR, e.g. 10.10.1.100/24.
	IPAddress string
	// Gateway is the IPv4 gateway, e.g. 10.10.1.1.
	Gateway string
	// DNS defaults to ["10.10.0.1"]; a non-nil empty slice is honoured as empty,
	// matching `net.dns || CONFIG.DEFAULT_DNS` ([] is truthy in JavaScript).
	DNS []string
	// IPv6Address is CIDR, e.g. 2001:db8::100/64.
	IPv6Address string
	// IPv6Gateway is kept for parity with the TypeScript type. The container
	// generator always uses the link-local fe80::1 for the routed eth1 block and
	// never reads this field; it is carried so callers can pass the full record.
	IPv6Gateway string
	// IPv6DNS defaults to the Cloudflare/Google pair.
	IPv6DNS []string
}

// IncusConfigParams mirrors the IncusConfigParams interface (the container
// module's version, which is the one the router accepts).
type IncusConfigParams struct {
	InstanceName string
	ImageAlias   string
	RootPassword string
	SSHKey       *string
	// Type is 'container' or 'virtual-machine'. Anything else — including the
	// empty string — takes the container branch, exactly like the TypeScript.
	Type string
	// NetworkMode is one of nat / nat_ipv6 / nat_ipv6_nat / ipv6_only /
	// ipv6_nat / public_ipv4 / public_ipv4_ipv6. Empty means "not supplied".
	NetworkMode string
	// Network is nil when the panel wants DHCP.
	Network *IncusConfigStaticNetwork
	// ExtraShellCommands is multi-line text split on '\n'.
	ExtraShellCommands string
}

// IncusConfigOsFamily mirrors the OsFamily union of the container generator.
type IncusConfigOsFamily string

// Container OS families (the router reports 'linux' for VMs).
const (
	IncusConfigOsFamilyDebian IncusConfigOsFamily = "debian"
	IncusConfigOsFamilyUbuntu IncusConfigOsFamily = "ubuntu"
	IncusConfigOsFamilyRhel   IncusConfigOsFamily = "rhel"
	IncusConfigOsFamilyRhel8  IncusConfigOsFamily = "rhel8"
	IncusConfigOsFamilySuse   IncusConfigOsFamily = "suse"
	IncusConfigOsFamilyAlpine IncusConfigOsFamily = "alpine"
	IncusConfigOsFamilyArch   IncusConfigOsFamily = "arch"
	IncusConfigOsFamilyLinux  IncusConfigOsFamily = "linux"
	IncusConfigOsFamilyOther  IncusConfigOsFamily = "other"
)

// IncusConfigInstanceType mirrors `'container' | 'virtual-machine'`.
type IncusConfigInstanceType string

// Instance types as they appear in metaData.
const (
	IncusConfigTypeContainer IncusConfigInstanceType = "container"
	IncusConfigTypeVM        IncusConfigInstanceType = "virtual-machine"
)

// IncusConfigMetaData mirrors IncusConfigMetaData. Field order is the JSON order.
type IncusConfigMetaData struct {
	SSHPort      int                     `json:"sshPort"`
	RootPassword string                  `json:"rootPassword"`
	OsFamily     IncusConfigOsFamily     `json:"osFamily"`
	ImageAlias   string                  `json:"imageAlias"`
	InstanceType IncusConfigInstanceType `json:"instanceType"`
	IPAddress    string                  `json:"ipAddress"`
}

// IncusConfigPayload mirrors `configPayload`. Field order is the JSON order.
type IncusConfigPayload struct {
	UserUserData      string `json:"user.user-data"`
	UserNetworkConfig string `json:"user.network-config"`
}

// IncusConfigResult mirrors IncusConfigResult.
type IncusConfigResult struct {
	ConfigPayload IncusConfigPayload  `json:"configPayload"`
	MetaData      IncusConfigMetaData `json:"metaData"`
}

// Errors the generators throw. The messages are the literal TypeScript strings
// because they travel back to the API caller.
var (
	// ErrIncusConfigMissingParams mirrors `throw new Error('Missing params')`.
	ErrIncusConfigMissingParams = errors.New("Missing params")
	// ErrIncusConfigInvalidSSHKey mirrors `throw new Error('Invalid SSH Key')`.
	ErrIncusConfigInvalidSSHKey = errors.New("Invalid SSH Key")
	// ErrIncusConfigVMGeneratorMissing is returned by the VM branch until
	// server/src/lib/incus-config-vm.ts (and vm-network-identifiers.ts) is ported.
	// It is deliberately loud: a VM must never receive a container payload.
	ErrIncusConfigVMGeneratorMissing = errors.New("lib/incus-config-vm.ts 尚未移植：generateVmConfig 依赖 vm-network-identifiers.ts 的确定性 MAC 派生")
)

// IncusConfigGenerateVMFunc is the VM generator seam. It is a variable so a future
// incus_config_vm.go can install the real implementation without touching the
// router (mirrors the router's generateVmConfig call).
//
// The TypeScript generateVmConfig returns only { configPayload }; the router builds
// the VM metadata itself with osFamily 'linux'. The seam keeps that split.
var IncusConfigGenerateVMFunc func(params IncusConfigParams) (IncusConfigPayload, error)

// incusConfigRandomPasswordChars spells out the TypeScript character set:
// A-Z a-z 0-9 !@#$%^&* (70 characters).
const incusConfigRandomPasswordChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*"

// GenerateRandomPassword mirrors generateRandomPassword(length = 16).
//
// crypto.randomInt(chars.length) is uniform; rand.Int over the same bound is the
// faithful equivalent (no modulo bias). Callers pass 16 for the default.
func GenerateRandomPassword(length int) string {
	if length <= 0 {
		return ""
	}
	limit := big.NewInt(int64(len(incusConfigRandomPasswordChars)))
	var builder strings.Builder
	builder.Grow(length)
	for i := 0; i < length; i++ {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			// crypto/rand failing is fatal for any secret; the TypeScript would have
			// thrown here too.
			panic("lib: crypto/rand unavailable: " + err.Error())
		}
		builder.WriteByte(incusConfigRandomPasswordChars[index.Int64()])
	}
	return builder.String()
}

// incusConfigEscapeForShellSingleQuote mirrors escapeForShellSingleQuote.
func incusConfigEscapeForShellSingleQuote(value string) string {
	return strings.ReplaceAll(value, "'", `'"'"'`)
}

// incusConfigEscapeForShellDoubleQuote mirrors escapeForShellDoubleQuote. The
// replacement order matters: the backslash pass must run first.
func incusConfigEscapeForShellDoubleQuote(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`$`, `\$`,
		"`", "\\`",
		`"`, `\"`,
	)
	return replacer.Replace(value)
}

var incusConfigSSHKeyPattern = regexp.MustCompile(`^(ssh-rsa|ssh-ed25519|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|ssh-dss)\s+[A-Za-z0-9+/=]+`)

// incusConfigIsValidSSHKey mirrors isValidSshKey.
func incusConfigIsValidSSHKey(key string) bool {
	return incusConfigSSHKeyPattern.MatchString(strings.TrimSpace(key))
}

var (
	incusConfigRhel8Pattern     = regexp.MustCompile(`(?i)alma.*8|rocky.*8|oracle.*8|centos.*8`)
	incusConfigOldDebianPattern = regexp.MustCompile(`(?i)debian.*(10|buster|11|bullseye)`)
)

// incusConfigOsInfo mirrors OsInfo.
type incusConfigOsInfo struct {
	Family          IncusConfigOsFamily
	PackageManager  string
	InitSystem      string
	SSHServiceName  string
	DefaultShell    string
	DefaultUsers    []string
	SupportsInclude bool
}

// incusConfigDetectOsInfo mirrors detectOsInfo. Order is load-bearing: `kali`
// contains no `debian` substring, but `debian` is checked before `alpine` etc.
func incusConfigDetectOsInfo(imageAlias string) incusConfigOsInfo {
	alias := strings.ToLower(imageAlias)

	if strings.Contains(alias, "ubuntu") {
		return incusConfigOsInfo{IncusConfigOsFamilyUbuntu, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "ubuntu"}, true}
	}
	if strings.Contains(alias, "arch") {
		return incusConfigOsInfo{IncusConfigOsFamilyArch, "pacman", "systemd", "sshd", "/bin/bash", []string{"root", "arch"}, true}
	}
	if strings.Contains(alias, "kali") {
		return incusConfigOsInfo{IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "kali"}, true}
	}
	if strings.Contains(alias, "debian") {
		isOld := incusConfigOldDebianPattern.MatchString(alias)
		return incusConfigOsInfo{IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "debian"}, !isOld}
	}
	if strings.Contains(alias, "alpine") {
		return incusConfigOsInfo{IncusConfigOsFamilyAlpine, "apk", "openrc", "sshd", "/bin/ash", []string{"root"}, true}
	}
	if strings.Contains(alias, "opensuse") || strings.Contains(alias, "suse") ||
		strings.Contains(alias, "tumbleweed") || strings.Contains(alias, "leap") {
		return incusConfigOsInfo{IncusConfigOsFamilySuse, "zypper", "systemd", "sshd", "/bin/bash", []string{"root"}, true}
	}
	if strings.Contains(alias, "fedora") {
		return incusConfigOsInfo{IncusConfigOsFamilyRhel, "dnf", "systemd", "sshd", "/bin/bash", []string{"root", "fedora"}, true}
	}
	if strings.Contains(alias, "alma") || strings.Contains(alias, "rocky") || strings.Contains(alias, "oracle") ||
		strings.Contains(alias, "centos") || strings.Contains(alias, "rhel") {
		isVersion8 := incusConfigRhel8Pattern.MatchString(alias)
		defaultUser := "root"
		switch {
		case strings.Contains(alias, "alma"):
			defaultUser = "almalinux"
		case strings.Contains(alias, "rocky"):
			defaultUser = "rocky"
		case strings.Contains(alias, "oracle"):
			defaultUser = "oracle"
		case strings.Contains(alias, "centos"):
			defaultUser = "centos"
		}
		family := IncusConfigOsFamilyRhel
		packageManager := "dnf"
		if isVersion8 {
			family = IncusConfigOsFamilyRhel8
			packageManager = "yum"
		}
		return incusConfigOsInfo{family, packageManager, "systemd", "sshd", "/bin/bash", []string{"root", defaultUser}, true}
	}

	return incusConfigOsInfo{IncusConfigOsFamilyOther, "apt", "systemd", "sshd", "/bin/bash", []string{"root"}, true}
}

// incusConfigPackagesForOs mirrors getPackagesForOs.
func incusConfigPackagesForOs(osInfo incusConfigOsInfo) []string {
	commonUtils := []string{"curl", "wget", "sudo", "ca-certificates"}
	appendAll := func(extra ...string) []string {
		return append(append([]string{}, commonUtils...), extra...)
	}
	switch osInfo.PackageManager {
	case "apt":
		return appendAll("openssh-server", "sed", "grep", "iproute2")
	case "dnf", "yum":
		return appendAll("openssh-server", "openssh-clients", "sed", "grep", "iproute")
	case "zypper":
		return appendAll("openssh", "sed", "grep", "iproute2")
	case "pacman":
		return []string{}
	case "apk":
		return []string{}
	default:
		return []string{}
	}
}

// incusConfigWrapWithNetworkRetry mirrors wrapWithNetworkRetry. The leading
// newline, the four-space "blank" lines and the two trailing spaces before the
// closing backtick are all part of the emitted script.
func incusConfigWrapWithNetworkRetry(command string) string {
	return `
  j=0
  while [ $j -lt ` + strconv.Itoa(incusConfigNetworkRetryCount) + ` ]; do
    # 动态获取 IPv4 或 IPv6 默认网关作为 DNS，若依然全空，则给予双栈的知名公测兜底 DNS，护航纯 IPv6 及异常段启动
    GW4=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $3}' | head -n 1)
    GW6=$(ip -6 route show default 2>/dev/null | awk '/default/ {print $3}' | head -n 1)
    if [ -n "$GW4" ]; then echo "nameserver $GW4" > /etc/resolv.conf; elif [ -n "$GW6" ]; then echo "nameserver $GW6" > /etc/resolv.conf; else echo -e "nameserver 8.8.8.8\nnameserver 2001:4860:4860::8888" > /etc/resolv.conf; fi
    
    if ` + command + `; then
      break
    fi
    
    echo "Command failed (attempt $j), retrying in ` + strconv.Itoa(incusConfigNetworkRetryDelay) + `s..."
    j=$((j+1))
    sleep ` + strconv.Itoa(incusConfigNetworkRetryDelay) + `
  done
  `
}

// incusConfigSetupCommands mirrors getSetupCommands. The ordering of the package
// manager branch and the universal tail is asserted by the golden hash.
func incusConfigSetupCommands(osInfo incusConfigOsInfo) []string {
	cmds := make([]string, 0, 12)

	if osInfo.Family == IncusConfigOsFamilyAlpine {
		cmds = append(cmds, `sed -i "s/https:/http:/g" /etc/apk/repositories || true`)
		essentialPackages := "openssh openssh-server-pam bash shadow sed grep dhcpcd util-linux ca-certificates iproute2"
		installCmd := "apk update && apk add --no-cache --allow-untrusted " + essentialPackages
		cmds = append(cmds, incusConfigWrapWithNetworkRetry(installCmd))
		cmds = append(cmds, `sed -i "s/http:/https:/g" /etc/apk/repositories || true`)
		cmds = append(cmds, `touch /etc/shadow && chmod 640 /etc/shadow || true`)
	} else if osInfo.Family == IncusConfigOsFamilyArch {
		cmds = append(cmds, incusConfigWrapWithNetworkRetry("pacman -Sy --noconfirm openssh sed grep iproute2"))
		cmds = append(cmds, "ssh-keygen -A || true")
	} else {
		switch osInfo.PackageManager {
		case "apt":
			cmds = append(cmds, incusConfigWrapWithNetworkRetry("DEBIAN_FRONTEND=noninteractive apt-get update"))
			cmds = append(cmds, "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server sed grep iproute2 || true")
		case "dnf":
			cmds = append(cmds, incusConfigWrapWithNetworkRetry("dnf install -y openssh-server openssh-clients sed grep iproute"))
		case "yum":
			cmds = append(cmds, incusConfigWrapWithNetworkRetry("yum install -y openssh-server openssh-clients sed grep iproute"))
		case "zypper":
			cmds = append(cmds, incusConfigWrapWithNetworkRetry("zypper --non-interactive refresh"))
			cmds = append(cmds, "zypper --non-interactive install -y openssh sed grep iproute2 || true")
		}
	}

	// 通用配置
	cmds = append(cmds, "mkdir -p /etc/ssh/sshd_config.d")
	cmds = append(cmds, `sed -i -E "s/^[#[:space:]]*Subsystem[[:space:]]+sftp.*/Subsystem sftp internal-sftp/" /etc/ssh/sshd_config 2>/dev/null || true`)
	cmds = append(cmds, `grep -q "^Subsystem sftp " /etc/ssh/sshd_config || printf "\n%s\n" "Subsystem sftp internal-sftp" >> /etc/ssh/sshd_config`)

	if osInfo.SupportsInclude {
		cmds = append(cmds, `grep -q "^Include /etc/ssh/sshd_config.d" /etc/ssh/sshd_config || sed -i "1i Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config || true`)
	}

	cmds = append(cmds, `command -v ssh-keygen >/dev/null && { test -f /etc/ssh/ssh_host_rsa_key || ssh-keygen -A 2>/dev/null; } || true`)

	if osInfo.InitSystem == "systemd" {
		cmds = append(cmds, "systemctl unmask "+osInfo.SSHServiceName+" || true")
		cmds = append(cmds, "systemctl enable "+osInfo.SSHServiceName+" || true")
	} else {
		cmds = append(cmds, "[ -f /etc/init.d/"+osInfo.SSHServiceName+" ] && rc-update add "+osInfo.SSHServiceName+" default 2>/dev/null || true")
		cmds = append(cmds, "[ -f /etc/init.d/dhcpcd ] && rc-update add dhcpcd default 2>/dev/null || true")
	}

	return cmds
}

// incusConfigSetupScriptTemplate is the shell script written to
// /usr/local/bin/incus-setup.sh. Tokens are substituted rather than formatted so
// that no '%' or stray brace in the script can be mistaken for a directive.
const incusConfigSetupScriptTemplate = `#!/bin/sh
set -e

SSH_KEY="@@SSH_KEY@@"
ROOT_PASSWORD='@@ROOT_PASSWORD@@'
USERS="@@USERS@@"

if echo "root:$ROOT_PASSWORD" | chpasswd >/dev/null 2>&1; then
  :
else
  echo -e "$ROOT_PASSWORD\n$ROOT_PASSWORD" | passwd root >/dev/null 2>&1 || true
fi

passwd -u root >/dev/null 2>&1 || usermod -U root >/dev/null 2>&1 || true

mkdir -p /root/.ssh
chmod 700 /root/.ssh
if [ -n "$SSH_KEY" ]; then
  echo "$SSH_KEY" > /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
  chown root:root /root/.ssh /root/.ssh/authorized_keys
else
  chown root:root /root/.ssh
fi

for u in $USERS; do
  if [ "$u" = "root" ]; then continue; fi
  if id "$u" >/dev/null 2>&1; then
    HOMEDIR="/home/$u"
    if [ ! -d "$HOMEDIR" ]; then continue; fi
    mkdir -p "$HOMEDIR/.ssh"
    if [ -n "$SSH_KEY" ]; then
      echo "$SSH_KEY" > "$HOMEDIR/.ssh/authorized_keys"
      chmod 600 "$HOMEDIR/.ssh/authorized_keys"
    fi
    chown -R "$u" "$HOMEDIR/.ssh" 2>/dev/null || true
    chmod 700 "$HOMEDIR/.ssh"
    echo "$u:$ROOT_PASSWORD" | chpasswd >/dev/null 2>&1 || true
  fi
done

SED_BIN="sed"
SSH_CONFIG="/etc/ssh/sshd_config"

if [ -d "/etc/ssh/sshd_config.d" ]; then
    rm -f /etc/ssh/sshd_config.d/*cloud-init*.conf 2>/dev/null || true
    for f in /etc/ssh/sshd_config.d/*.conf; do
        if [ -f "$f" ]; then
            $SED_BIN -i 's/^PasswordAuthentication no/PasswordAuthentication yes/g' "$f" 2>/dev/null || true
            $SED_BIN -i 's/^#*PermitRootLogin.*/PermitRootLogin yes/g' "$f" 2>/dev/null || true
        fi
    done
fi

$SED_BIN -i 's/^PermitRootLogin/#PermitRootLogin/g' $SSH_CONFIG 2>/dev/null || true
$SED_BIN -i 's/^PasswordAuthentication/#PasswordAuthentication/g' $SSH_CONFIG 2>/dev/null || true
$SED_BIN -i 's/^PubkeyAuthentication/#PubkeyAuthentication/g' $SSH_CONFIG 2>/dev/null || true

if ! grep -q "^Include /etc/ssh/sshd_config.d" $SSH_CONFIG; then
    $SED_BIN -i "1i Include /etc/ssh/sshd_config.d/*.conf" $SSH_CONFIG 2>/dev/null || true
fi

echo "PermitRootLogin yes" >> $SSH_CONFIG
echo "PasswordAuthentication yes" >> $SSH_CONFIG
echo "PubkeyAuthentication yes" >> $SSH_CONFIG

@@RESTART_SSH@@

exit 0
`

// incusConfigGenerateSetupScript mirrors generateSetupScript.
func incusConfigGenerateSetupScript(sshKey *string, rootPassword string, users []string, osInfo incusConfigOsInfo) string {
	escapedKey := incusConfigEscapeForShellDoubleQuote(incusConfigDeref(sshKey))
	escapedPassword := incusConfigEscapeForShellSingleQuote(rootPassword)
	usersListForLoop := strings.Join(users, " ")
	sshServiceName := osInfo.SSHServiceName

	var restartSSHCmd string
	if osInfo.InitSystem == "openrc" {
		restartSSHCmd = "rc-service " + sshServiceName + " restart 2>/dev/null || /etc/init.d/" + sshServiceName +
			" restart || /etc/init.d/" + sshServiceName + " start || true"
	} else {
		restartSSHCmd = "systemctl restart " + sshServiceName + " 2>/dev/null || service " + sshServiceName + " restart || true"
	}

	return strings.NewReplacer(
		"@@SSH_KEY@@", escapedKey,
		"@@ROOT_PASSWORD@@", escapedPassword,
		"@@USERS@@", usersListForLoop,
		"@@RESTART_SSH@@", restartSSHCmd,
	).Replace(incusConfigSetupScriptTemplate)
}

// incusConfigBootcmd mirrors getBootcmd.
func incusConfigBootcmd(osInfo incusConfigOsInfo) []string {
	cmds := make([]string, 0, 3)

	if osInfo.Family == IncusConfigOsFamilyAlpine {
		// 动态获取 IPv4/v6 默认网关并给到默认公共回退 DNS，确立不同网段/纯 v6 下的不报错初始化
		cmds = append(cmds, `GW4=$(ip -4 route show default 2>/dev/null | awk '/default/ {print $3}' | head -n 1); GW6=$(ip -6 route show default 2>/dev/null | awk '/default/ {print $3}' | head -n 1); if [ -n "$GW4" ]; then echo "nameserver $GW4" > /etc/resolv.conf; elif [ -n "$GW6" ]; then echo "nameserver $GW6" > /etc/resolv.conf; else echo -e "nameserver 8.8.8.8\nnameserver 2001:4860:4860::8888" > /etc/resolv.conf; fi`)
		cmds = append(cmds, "rc-update add networking boot || true")
		cmds = append(cmds, "rc-service networking start 2>/dev/null || true")
		return cmds
	}

	switch osInfo.PackageManager {
	case "apt":
		cmds = append(cmds, "cloud-init-per once apt_update_early apt-get update || true")
	case "dnf":
		cmds = append(cmds, "cloud-init-per once dnf_makecache dnf makecache || true")
	case "yum":
		cmds = append(cmds, "cloud-init-per once yum_makecache yum makecache || true")
	case "zypper":
		cmds = append(cmds, "cloud-init-per once zypper_refresh zypper --non-interactive refresh || true")
	}

	return cmds
}

// incusConfigNetworkConfig mirrors getNetworkConfig.
func incusConfigNetworkConfig(network *IncusConfigStaticNetwork, shouldBlockSharedIpv6OnEth0 bool) string {
	if network == nil {
		if shouldBlockSharedIpv6OnEth0 {
			return "version: 1\nconfig:\n  - type: physical\n    name: " + incusConfigInterfaceName + "\n    subnets:\n      - type: dhcp\n"
		}
		return "version: 1\nconfig:\n  - type: physical\n    name: " + incusConfigInterfaceName + "\n    subnets:\n      - type: dhcp\n      - type: dhcp6\n"
	}

	dnsV4 := network.DNS
	if dnsV4 == nil {
		dnsV4 = incusConfigDefaultDNS
	}
	dnsV4Lines := make([]string, 0, len(dnsV4))
	for _, dns := range dnsV4 {
		dnsV4Lines = append(dnsV4Lines, "          - "+dns)
	}

	subnet := "      - type: static\n        address: " + network.IPAddress +
		"\n        gateway: " + network.Gateway +
		"\n        dns_nameservers:\n" + strings.Join(dnsV4Lines, "\n")

	config := "version: 1\nconfig:\n  - type: physical\n    name: " + incusConfigInterfaceName +
		"\n    subnets:\n" + subnet + "\n"

	// eth1: IPv6 routed (dual-NIC: eth0 = bridged IPv4, eth1 = routed IPv6). The
	// gateway is fe80::1, Incus's standard link-local gateway for routed mode.
	if network.IPv6Address != "" {
		ipv6Addr := network.IPv6Address
		if !strings.Contains(ipv6Addr, "/") {
			ipv6Addr += "/128"
		}
		dnsV6 := network.IPv6DNS
		if dnsV6 == nil {
			dnsV6 = []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
		}
		dnsV6Lines := make([]string, 0, len(dnsV6))
		for _, dns := range dnsV6 {
			dnsV6Lines = append(dnsV6Lines, "          - "+dns)
		}
		config += "  - type: physical\n    name: eth1\n    subnets:\n      - type: static\n        address: " + ipv6Addr +
			"\n        gateway: fe80::1\n        dns_nameservers:\n" + strings.Join(dnsV6Lines, "\n") + "\n"
	}

	return config
}

// incusConfigDeref reads an optional string, treating nil as "" (the TypeScript
// `sshKey || ”` / `sshKey?.trim()` pattern).
func incusConfigDeref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// incusConfigJSONStringify mirrors JSON.stringify(string): quote, escape
// backslash and quote, use the short escapes for \b \t \n \f \r and \u00XX for
// the remaining control characters. Everything else (including non-ASCII and
// U+2028/U+2029) is emitted verbatim. encoding/json is not used because it
// escapes HTML characters and U+2028 by default.
func incusConfigJSONStringify(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\t':
			builder.WriteString(`\t`)
		case '\n':
			builder.WriteString(`\n`)
		case '\f':
			builder.WriteString(`\f`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			if r < 0x20 {
				const hexDigits = "0123456789abcdef"
				builder.WriteString(`\u00`)
				builder.WriteByte(hexDigits[(r>>4)&0xF])
				builder.WriteByte(hexDigits[r&0xF])
				continue
			}
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// IncusConfigGenerateContainer mirrors generateContainerConfig (LXC).
//
// The TypeScript throws instead of returning an error; Go returns the same
// messages as sentinels so callers can map them to the same API responses.
func IncusConfigGenerateContainer(params IncusConfigParams) (IncusConfigResult, error) {
	if params.ImageAlias == "" || params.RootPassword == "" {
		return IncusConfigResult{}, ErrIncusConfigMissingParams
	}
	if params.SSHKey != nil && *params.SSHKey != "" && !incusConfigIsValidSSHKey(*params.SSHKey) {
		return IncusConfigResult{}, ErrIncusConfigInvalidSSHKey
	}

	osInfo := incusConfigDetectOsInfo(params.ImageAlias)
	isAlpine := osInfo.Family == IncusConfigOsFamilyAlpine
	sshPort := IncusConfigSSHPort
	commonUsers := osInfo.DefaultUsers
	sshKeyTrimmed := strings.TrimSpace(incusConfigDeref(params.SSHKey))

	// 1. Users YAML
	userBlocks := make([]string, 0, len(commonUsers))
	for _, user := range commonUsers {
		lines := []string{
			"  - name: " + user,
			"    lock_passwd: false",
			"    create_home: true",
			"    shell: " + osInfo.DefaultShell,
			"    sudo: ALL=(ALL) NOPASSWD:ALL",
		}
		if sshKeyTrimmed != "" {
			lines = append(lines, "    ssh_authorized_keys:", "      - "+sshKeyTrimmed)
		}
		userBlocks = append(userBlocks, strings.Join(lines, "\n"))
	}
	usersYaml := strings.Join(userBlocks, "\n")

	// 2. SSH config
	sshdDropInPath := "/etc/ssh/sshd_config.d/50-incus.conf"
	sshdDropInContent := strings.Join([]string{
		"Port " + strconv.Itoa(sshPort),
		"PermitRootLogin yes",
		"PasswordAuthentication yes",
		"PubkeyAuthentication yes",
	}, "\n")

	// 3. Scripts
	setupScript := incusConfigGenerateSetupScript(params.SSHKey, params.RootPassword, commonUsers, osInfo)
	setupCmds := incusConfigSetupCommands(osInfo)

	// 4. Runcmd
	userCustomCmds := make([]string, 0)
	if params.ExtraShellCommands != "" {
		for _, cmd := range strings.Split(params.ExtraShellCommands, "\n") {
			if trimmed := strings.TrimSpace(cmd); trimmed != "" {
				userCustomCmds = append(userCustomCmds, trimmed)
			}
		}
	}

	var shouldBlockSharedIpv6OnEth0 bool
	if params.NetworkMode != "" {
		shouldBlockSharedIpv6OnEth0 = params.NetworkMode != "nat_ipv6_nat" && params.NetworkMode != "ipv6_nat"
	} else {
		shouldBlockSharedIpv6OnEth0 = params.Network != nil && params.Network.IPv6Address != ""
	}

	routedIpv6GuardCmds := make([]string, 0, 6)
	if shouldBlockSharedIpv6OnEth0 {
		// Routed IPv6 模式下，eth0 只承担 IPv4 NAT。
		routedIpv6GuardCmds = append(routedIpv6GuardCmds,
			"mkdir -p /etc/sysctl.d || true",
			`printf "%s\n%s\n%s\n" "net.ipv6.conf.eth0.disable_ipv6 = 1" "net.ipv6.conf.eth0.accept_ra = 0" "net.ipv6.conf.eth0.autoconf = 0" > /etc/sysctl.d/99-incudal-routed-ipv6.conf`,
			"sysctl -p /etc/sysctl.d/99-incudal-routed-ipv6.conf >/dev/null 2>&1 || true",
			"ip -6 addr flush dev eth0 scope global >/dev/null 2>&1 || true",
			"ip -6 route del default dev eth0 >/dev/null 2>&1 || true",
			"ip -6 route replace default via fe80::1 dev eth1 metric 100 >/dev/null 2>&1 || true",
		)
	}

	runcmd := make([]string, 0, len(setupCmds)+len(routedIpv6GuardCmds)+len(userCustomCmds)+2)
	runcmd = append(runcmd, "mkdir -p /etc/ssh/sshd_config.d || true")
	runcmd = append(runcmd, setupCmds...)
	runcmd = append(runcmd, "/usr/local/bin/incus-setup.sh || true")
	runcmd = append(runcmd, routedIpv6GuardCmds...)
	runcmd = append(runcmd, userCustomCmds...)

	// 5. write_files
	packages := incusConfigPackagesForOs(osInfo)
	packageLines := make([]string, 0, len(packages))
	for _, pkg := range packages {
		packageLines = append(packageLines, "  - "+pkg)
	}
	packagesYaml := strings.Join(packageLines, "\n")

	writeFilesLines := []string{
		"  - path: " + sshdDropInPath,
		"    owner: root:root",
		"    permissions: '0644'",
		"    content: |",
	}
	for _, line := range strings.Split(sshdDropInContent, "\n") {
		writeFilesLines = append(writeFilesLines, "      "+line)
	}
	writeFilesLines = append(writeFilesLines,
		"  - path: /usr/local/bin/incus-setup.sh",
		"    owner: root:root",
		"    permissions: '0755'",
		"    content: |",
	)
	for _, line := range strings.Split(setupScript, "\n") {
		writeFilesLines = append(writeFilesLines, "      "+line)
	}
	writeFilesYaml := strings.Join(writeFilesLines, "\n")

	runcmdLines := make([]string, 0, len(runcmd))
	for _, cmd := range runcmd {
		runcmdLines = append(runcmdLines, "  - "+incusConfigJSONStringify(cmd))
	}
	runcmdYaml := strings.Join(runcmdLines, "\n")

	bootcmdList := incusConfigBootcmd(osInfo)
	bootcmdSection := ""
	if len(bootcmdList) > 0 {
		bootcmdLines := make([]string, 0, len(bootcmdList))
		for _, cmd := range bootcmdList {
			bootcmdLines = append(bootcmdLines, "  - "+cmd)
		}
		bootcmdSection = "bootcmd:\n" + strings.Join(bootcmdLines, "\n") + "\n"
	}

	yamlSafePassword := strings.ReplaceAll(params.RootPassword, "'", "''")
	yamlSafePassword = strings.ReplaceAll(yamlSafePassword, `\`, `\\`)

	packagesSection := ""
	if len(packages) > 0 {
		packagesSection = "packages:\n" + packagesYaml + "\n\n"
	}

	chpasswdSection := ""
	if !isAlpine {
		chpasswdSection = "chpasswd:\n  expire: false\n  list: |\n    root:" + yamlSafePassword + "\n\n"
	}

	// 6. Network config
	networkConfigYaml := incusConfigNetworkConfig(params.Network, shouldBlockSharedIpv6OnEth0)

	// 7. Output
	packageUpdate := "true"
	if isAlpine {
		packageUpdate = "false"
	}
	cloudConfigYaml := "#cloud-config\n" +
		"package_update: " + packageUpdate + "\n" +
		"package_upgrade: false\n" +
		"timezone: Asia/Shanghai\n" +
		"\n" +
		"ssh_pwauth: true\n" +
		"disable_root: false\n" +
		"\n" +
		packagesSection + chpasswdSection +
		"users:\n" + usersYaml + "\n" +
		"\n" +
		"write_files:\n" + writeFilesYaml + "\n" +
		"\n" +
		bootcmdSection + "runcmd:\n" + runcmdYaml + "\n"

	ipAddress := "dhcp"
	if params.Network != nil && params.Network.IPAddress != "" {
		ipAddress = params.Network.IPAddress
	}

	return IncusConfigResult{
		ConfigPayload: IncusConfigPayload{
			UserUserData:      cloudConfigYaml,
			UserNetworkConfig: networkConfigYaml,
		},
		MetaData: IncusConfigMetaData{
			SSHPort:      sshPort,
			RootPassword: params.RootPassword,
			OsFamily:     osInfo.Family,
			ImageAlias:   params.ImageAlias,
			InstanceType: IncusConfigTypeContainer,
			IPAddress:    ipAddress,
		},
	}, nil
}

// GenerateIncusConfig mirrors generateIncusConfig: route by params.type.
//
// The TypeScript returns synchronously and throws; the Go port returns
// (result, error) and keeps the same error messages/conditions.
func GenerateIncusConfig(params IncusConfigParams) (IncusConfigResult, error) {
	if params.Type == "virtual-machine" {
		generator := IncusConfigGenerateVMFunc
		if generator == nil {
			return IncusConfigResult{}, ErrIncusConfigVMGeneratorMissing
		}

		payload, err := generator(params)
		if err != nil {
			return IncusConfigResult{}, err
		}

		ipAddress := "dhcp"
		if params.Network != nil && params.Network.IPAddress != "" {
			ipAddress = params.Network.IPAddress
		}

		return IncusConfigResult{
			ConfigPayload: payload,
			MetaData: IncusConfigMetaData{
				SSHPort:      IncusConfigSSHPort,
				RootPassword: params.RootPassword,
				OsFamily:     IncusConfigOsFamilyLinux,
				ImageAlias:   params.ImageAlias,
				InstanceType: IncusConfigTypeVM,
				IPAddress:    ipAddress,
			},
		}, nil
	}

	return IncusConfigGenerateContainer(params)
}
