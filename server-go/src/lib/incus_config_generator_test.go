package lib

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The expected hashes are SHA-256 of the strings produced by
// server/src/lib/incus-config-container.ts for the same inputs (generated with
// tsx against the canary source). They are byte-exact on purpose: cloud-init and
// netplan are whitespace sensitive and the shell templates below are full of
// load-bearing indentation.
func incusConfigSha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func incusConfigStrPtr(value string) *string { return &value }

func TestGenerateContainerConfigMatchesNodeByteForByte(t *testing.T) {
	cases := []struct {
		name            string
		params          IncusConfigParams
		wantUserData    string
		wantNetworkData string
		wantFragments   []string
		absentFragments []string
	}{
		{
			name:            "debian-minimal",
			params:          IncusConfigParams{InstanceName: "pi-1", ImageAlias: "debian/12", RootPassword: "p@ss w0rd"},
			wantUserData:    "f7ac92309083cb4367542d76fd6560c771bf95964e95e55fe9dea7a1a78aeb2f",
			wantNetworkData: "2467f2f35bc753a4947c7f6c175ea81ad507cd0d93030ff3c51803f102c33a7c",
			wantFragments: []string{
				"#cloud-config\npackage_update: true\npackage_upgrade: false\ntimezone: Asia/Shanghai\n",
				"ssh_pwauth: true\ndisable_root: false\n",
				"chpasswd:\n  expire: false\n  list: |\n    root:p@ss w0rd\n",
				"  - name: debian\n",
				"  - name: root\n",
				"cloud-init-per once apt_update_early apt-get update || true",
				`  - "/usr/local/bin/incus-setup.sh || true"`,
				"packages:\n  - curl\n  - wget\n  - sudo\n  - ca-certificates\n  - openssh-server\n  - sed\n  - grep\n  - iproute2\n",
			},
			// networkMode and network are both unset, so the routed-IPv6 guard is off
			// and netplan must ask for DHCPv6 as well.
			absentFragments: []string{"net.ipv6.conf.eth0.disable_ipv6"},
		},
		{
			name: "ubuntu-static-v4",
			params: IncusConfigParams{
				InstanceName: "pi-2", ImageAlias: "ubuntu/noble", RootPassword: "pa'ss\\word",
				SSHKey:  incusConfigStrPtr("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyBlobHere user@host"),
				Network: &IncusConfigStaticNetwork{IPAddress: "10.10.1.100/24", Gateway: "10.10.1.1", DNS: []string{"1.1.1.1", "8.8.8.8"}},
				// nat is NOT in the routed list: networkMode overrides the network
				// heuristic, so the guard still applies.
				NetworkMode: "nat",
			},
			wantUserData:    "3eec15fa31f2f1c22d6ca3d6dccf3dbecf8a61c3ef59acbcdc116022fe210c06",
			wantNetworkData: "83c494e37459ad07b73537c8d527e8cc3120491a7aaed1dc29d077cdf3eb84e4",
			wantFragments: []string{
				// Shell single-quote escaping of the root password.
				`ROOT_PASSWORD='pa'"'"'ss\word'`,
				// YAML single-quote escaping in chpasswd.
				"    root:pa''ss\\\\word\n",
				"      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyBlobHere user@host",
				"        address: 10.10.1.100/24",
				"          - 1.1.1.1\n          - 8.8.8.8\n",
				"ip -6 route replace default via fe80::1 dev eth1 metric 100 >/dev/null 2>&1 || true",
			},
		},
		{
			name: "alpine-routed-v6",
			params: IncusConfigParams{
				InstanceName: "pi-3", ImageAlias: "alpine/3.20", RootPassword: "alpinepw",
				SSHKey:      incusConfigStrPtr("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCexample root@x"),
				Network:     &IncusConfigStaticNetwork{IPAddress: "10.10.2.5/24", Gateway: "10.10.2.1", IPv6Address: "2001:db8::5/64", IPv6DNS: []string{"2606:4700:4700::1111"}},
				NetworkMode: "nat_ipv6",
			},
			wantUserData:    "0d02d674a3d44714ee5864fe521f1ad17f3e0c7b76a81cc322a02258118efb46",
			wantNetworkData: "fb3990c9bb5e074b09ba8ba8a76c88a92ad45f48cf36cd7f42a1cbaa4b204f4f",
			wantFragments: []string{
				// Alpine gets no chpasswd block and package_update false.
				"package_update: false\n",
				"rc-update add networking boot || true",
				"rc-service networking start 2>/dev/null || true",
				"apk update && apk add --no-cache --allow-untrusted openssh openssh-server-pam bash shadow sed grep dhcpcd util-linux ca-certificates iproute2",
				"    name: eth1\n",
				"        address: 2001:db8::5/64\n        gateway: fe80::1\n",
				"          - 2606:4700:4700::1111\n",
			},
			absentFragments: []string{"chpasswd:", "package_update: true"},
		},
		{
			name: "rocky8-custom-cmds",
			params: IncusConfigParams{
				InstanceName: "pi-4", ImageAlias: "rockylinux/8", RootPassword: "r0cky",
				ExtraShellCommands: "  echo \"hello\"\n\n  touch /tmp/x  \n", NetworkMode: "nat_ipv6_nat",
			},
			wantUserData:    "be6b918d2c66573ab5b3e532f4e915f7168e5cbde57be36bf6cbb230623d1e80",
			wantNetworkData: "2467f2f35bc753a4947c7f6c175ea81ad507cd0d93030ff3c51803f102c33a7c",
			wantFragments: []string{
				// rhel8 → yum, and the default user is the distro user.
				"yum install -y openssh-server openssh-clients sed grep iproute",
				"  - name: rocky\n",
				"  - \"echo \\\"hello\\\"\"\n",
				"  - \"touch /tmp/x\"\n",
			},
			// nat_ipv6_nat is a dual-stack mode: eth0 keeps IPv6.
			absentFragments: []string{"net.ipv6.conf.eth0.disable_ipv6"},
		},
		{
			name:            "unknown-other",
			params:          IncusConfigParams{InstanceName: "pi-5", ImageAlias: "windows/2022", RootPassword: "pw"},
			wantUserData:    "f2b35e97b500e560c64408cd84fa22daa7dc51be8cc95382eb6dd523ca7f19ad",
			wantNetworkData: "2467f2f35bc753a4947c7f6c175ea81ad507cd0d93030ff3c51803f102c33a7c",
			wantFragments: []string{
				// Unknown alias falls through to family 'other', apt, sshd.
				"systemctl unmask sshd || true",
				"  - name: root\n",
			},
		},
		{
			name:            "kali-nonet",
			params:          IncusConfigParams{InstanceName: "pi-6", ImageAlias: "kali/2024.1", RootPassword: "pw", NetworkMode: "ipv6_only"},
			wantUserData:    "d3e361c27f99bc301de3dcda0e76dbac098087e380944859d57f988b83d80567",
			wantNetworkData: "2c8b0546965f9576cde044e55f34bc7c0be6832e437b02c027d84784bb9bc23a",
			wantFragments: []string{
				"  - name: kali\n",
				// No network record + blocked shared IPv6 → DHCPv4 only.
				"subnets:\n      - type: dhcp\n",
			},
			absentFragments: []string{"- type: dhcp6"},
		},
		{
			name:            "opensuse",
			params:          IncusConfigParams{InstanceName: "pi-7", ImageAlias: "opensuse/tumbleweed", RootPassword: "pw", NetworkMode: "public_ipv4"},
			wantUserData:    "874a2ec35bfe7ec5abc4fec50a9efd4b35f1849993931be0e0e822099f1fb112",
			wantNetworkData: "2c8b0546965f9576cde044e55f34bc7c0be6832e437b02c027d84784bb9bc23a",
			wantFragments: []string{
				"zypper --non-interactive refresh",
				"zypper --non-interactive install -y openssh sed grep iproute2 || true",
				"cloud-init-per once zypper_refresh zypper --non-interactive refresh || true",
			},
		},
		{
			name: "fedora",
			params: IncusConfigParams{InstanceName: "pi-8", ImageAlias: "fedora/40", RootPassword: "pw", NetworkMode: "public_ipv4_ipv6",
				Network: &IncusConfigStaticNetwork{IPAddress: "192.0.2.9/24", Gateway: "192.0.2.1"}},
			wantUserData:    "3f54021cc78e476846b6eeddbe294f7b3f61a5c0c234ce93c71d51582c042408",
			wantNetworkData: "51e7888af403fad4b26789cf34a2bc317718966e0a886f8478f68f19b0a22bf5",
			wantFragments: []string{
				"  - name: fedora\n",
				// No DNS supplied → the bridge gateway is the default resolver.
				"        dns_nameservers:\n          - 10.10.0.1\n",
				"        address: 192.0.2.9/24\n        gateway: 192.0.2.1\n",
			},
			// No IPv6 address → no eth1 block, and a static subnet replaces DHCP.
			absentFragments: []string{"name: eth1", "- type: dhcp"},
		},
		{
			name:            "arch",
			params:          IncusConfigParams{InstanceName: "pi-9", ImageAlias: "archlinux/current", RootPassword: "pw"},
			wantUserData:    "79b343b7e9a68efbdb1e1d324f43068694d501b2044e0c96115a2527e54cc78e",
			wantNetworkData: "2467f2f35bc753a4947c7f6c175ea81ad507cd0d93030ff3c51803f102c33a7c",
			wantFragments: []string{
				"pacman -Sy --noconfirm openssh sed grep iproute2",
				"ssh-keygen -A || true",
				// pacman → no package list, and no bootcmd section at all.
				"  - name: arch\n",
			},
			absentFragments: []string{"packages:", "bootcmd:"},
		},
		{
			name: "old-debian-dns-empty",
			params: IncusConfigParams{InstanceName: "pi-10", ImageAlias: "debian/10", RootPassword: "pw",
				Network:     &IncusConfigStaticNetwork{IPAddress: "10.0.0.5/24", Gateway: "10.0.0.1", DNS: []string{}, IPv6Address: "2001:db8::9", IPv6DNS: []string{}},
				NetworkMode: "nat"},
			wantUserData:    "b83427f5f065284c5b5d5f38cd5e657461c2076cc288734af04f3395a811cb7e",
			wantNetworkData: "6ea8bfe9cfc9e3c22c911862f1baad2df7d7ac60d79ab416e97d337b96aab8db",
			wantFragments: []string{
				// `net.dns || DEFAULT_DNS`: an empty array is truthy in JS, so the
				// default resolver must NOT come back.
				"        dns_nameservers:\n\n",
				// IPv6 without a prefix length gets /128, and empty ipv6Dns stays empty.
				"        address: 2001:db8::9/128\n",
			},
			// debian/10 is an "old" Debian: the Include-drop-in command is not emitted
			// (the line inside the setup script itself is unconditional).
			absentFragments: []string{"10.10.0.1", `grep -q "^Include /etc/ssh/sshd_config.d" /etc/ssh/sshd_config || sed -i`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := IncusConfigGenerateContainer(tc.params)
			if err != nil {
				t.Fatalf("IncusConfigGenerateContainer: %v", err)
			}

			userData := result.ConfigPayload.UserUserData
			if got := incusConfigSha256Hex(userData); got != tc.wantUserData {
				t.Errorf("user-data sha256 = %s, want %s\n--- actual payload ---\n%s", got, tc.wantUserData, userData)
			}
			networkConfig := result.ConfigPayload.UserNetworkConfig
			if got := incusConfigSha256Hex(networkConfig); got != tc.wantNetworkData {
				t.Errorf("network-config sha256 = %s, want %s\n--- actual ---\n%q", got, tc.wantNetworkData, networkConfig)
			}

			for _, fragment := range tc.wantFragments {
				if !strings.Contains(userData, fragment) && !strings.Contains(networkConfig, fragment) {
					t.Errorf("payload is missing %q", fragment)
				}
			}
			for _, fragment := range tc.absentFragments {
				if strings.Contains(userData, fragment) || strings.Contains(networkConfig, fragment) {
					t.Errorf("payload unexpectedly contains %q", fragment)
				}
			}

			if userData[:len("#cloud-config\n")] != "#cloud-config\n" {
				t.Errorf("user-data must start with the #cloud-config header, got %q", userData[:20])
			}
			if !strings.HasSuffix(userData, "\n") {
				t.Error("user-data must end with a newline")
			}

			meta := result.MetaData
			if meta.SSHPort != 22 {
				t.Errorf("metaData.sshPort = %d, want 22", meta.SSHPort)
			}
			if meta.RootPassword != tc.params.RootPassword {
				t.Errorf("metaData.rootPassword = %q, want %q", meta.RootPassword, tc.params.RootPassword)
			}
			if meta.InstanceType != IncusConfigTypeContainer {
				t.Errorf("metaData.instanceType = %q, want container", meta.InstanceType)
			}
			if meta.ImageAlias != tc.params.ImageAlias {
				t.Errorf("metaData.imageAlias = %q, want %q", meta.ImageAlias, tc.params.ImageAlias)
			}
			if tc.params.Network != nil && tc.params.Network.IPAddress != "" {
				if meta.IPAddress != tc.params.Network.IPAddress {
					t.Errorf("metaData.ipAddress = %q, want %q", meta.IPAddress, tc.params.Network.IPAddress)
				}
			} else if meta.IPAddress != "dhcp" {
				t.Errorf("metaData.ipAddress = %q, want dhcp", meta.IPAddress)
			}
		})
	}
}

func TestGenerateContainerConfigErrors(t *testing.T) {
	cases := []struct {
		name   string
		params IncusConfigParams
		want   error
	}{
		{"empty image alias", IncusConfigParams{ImageAlias: "", RootPassword: "pw"}, ErrIncusConfigMissingParams},
		{"empty password", IncusConfigParams{ImageAlias: "debian/12", RootPassword: ""}, ErrIncusConfigMissingParams},
		{"invalid ssh key", IncusConfigParams{ImageAlias: "debian/12", RootPassword: "pw", SSHKey: incusConfigStrPtr("not-a-key")}, ErrIncusConfigInvalidSSHKey},
		{"empty ssh key is ignored", IncusConfigParams{ImageAlias: "debian/12", RootPassword: "pw", SSHKey: incusConfigStrPtr("")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := IncusConfigGenerateContainer(tc.params)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			// The messages travel to API callers, so they must stay literal.
			if err.Error() != tc.want.Error() {
				t.Errorf("message = %q, want %q", err.Error(), tc.want.Error())
			}
		})
	}

	for _, key := range []string{
		"ssh-rsa AAAA",
		"ssh-ed25519 AAAA",
		"ecdsa-sha2-nistp256 AAAA",
		"ecdsa-sha2-nistp384 AAAA",
		"ecdsa-sha2-nistp521 AAAA",
		"ssh-dss AAAA",
		"  ssh-ed25519 AAAA  ",
	} {
		if _, err := IncusConfigGenerateContainer(IncusConfigParams{ImageAlias: "debian/12", RootPassword: "pw", SSHKey: &key}); err != nil {
			t.Errorf("valid key %q rejected: %v", key, err)
		}
	}
	for _, key := range []string{"AAAAB3NzaC1yc2E", "ssh-rsa", "ssh-rsa !!!!", "sk-ssh-ed25519 AAAA"} {
		if _, err := IncusConfigGenerateContainer(IncusConfigParams{ImageAlias: "debian/12", RootPassword: "pw", SSHKey: &key}); !errors.Is(err, ErrIncusConfigInvalidSSHKey) {
			t.Errorf("invalid key %q accepted (err=%v)", key, err)
		}
	}
}

func TestGenerateIncusConfigRoutesByType(t *testing.T) {
	network := &IncusConfigStaticNetwork{IPAddress: "10.10.1.100/24", Gateway: "10.10.1.1"}

	// Container branch: type empty or explicit container.
	for _, instanceType := range []string{"", "container"} {
		result, err := GenerateIncusConfig(IncusConfigParams{
			InstanceName: "c1", ImageAlias: "ubuntu/noble", RootPassword: "pw",
			Type: instanceType, Network: network,
		})
		if err != nil {
			t.Fatalf("type %q: %v", instanceType, err)
		}
		if result.MetaData.InstanceType != IncusConfigTypeContainer {
			t.Errorf("type %q → instanceType %q, want container", instanceType, result.MetaData.InstanceType)
		}
		if result.MetaData.OsFamily != IncusConfigOsFamilyUbuntu {
			t.Errorf("type %q → osFamily %q, want ubuntu", instanceType, result.MetaData.OsFamily)
		}
		if result.MetaData.IPAddress != "10.10.1.100/24" {
			t.Errorf("type %q → ipAddress %q", instanceType, result.MetaData.IPAddress)
		}
	}

	// VM branch without a generator must fail loudly, never return a container
	// payload.
	original := IncusConfigGenerateVMFunc
	IncusConfigGenerateVMFunc = nil
	t.Cleanup(func() { IncusConfigGenerateVMFunc = original })

	if _, err := GenerateIncusConfig(IncusConfigParams{
		InstanceName: "v1", ImageAlias: "ubuntu/noble", RootPassword: "pw", Type: "virtual-machine",
	}); !errors.Is(err, ErrIncusConfigVMGeneratorMissing) {
		t.Fatalf("VM without generator: err = %v, want ErrIncusConfigVMGeneratorMissing", err)
	}

	// VM branch with a stub: the router rebuilds its own metadata, exactly like
	// the TypeScript (sshPort 22, osFamily 'linux', imageAlias echoed back).
	var seen IncusConfigParams
	IncusConfigGenerateVMFunc = func(params IncusConfigParams) (IncusConfigPayload, error) {
		seen = params
		return IncusConfigPayload{UserUserData: "vm-user-data", UserNetworkConfig: "vm-network-config"}, nil
	}

	result, err := GenerateIncusConfig(IncusConfigParams{
		InstanceName: "v1", ImageAlias: "ubuntu/noble", RootPassword: "pw", Type: "virtual-machine",
		SSHKey: incusConfigStrPtr("ssh-ed25519 AAAA"), Network: network, NetworkMode: "nat", ExtraShellCommands: "echo hi",
	})
	if err != nil {
		t.Fatalf("VM with stub: %v", err)
	}
	if result.ConfigPayload.UserUserData != "vm-user-data" || result.ConfigPayload.UserNetworkConfig != "vm-network-config" {
		t.Errorf("VM payload was not passed through: %+v", result.ConfigPayload)
	}
	if seen.InstanceName != "v1" || seen.ImageAlias != "ubuntu/noble" || seen.RootPassword != "pw" ||
		seen.SSHKey == nil || *seen.SSHKey != "ssh-ed25519 AAAA" || seen.NetworkMode != "nat" ||
		seen.ExtraShellCommands != "echo hi" || seen.Network != network {
		t.Errorf("VM generator received %+v", seen)
	}
	want := IncusConfigMetaData{
		SSHPort:      22,
		RootPassword: "pw",
		OsFamily:     IncusConfigOsFamilyLinux,
		ImageAlias:   "ubuntu/noble",
		InstanceType: IncusConfigTypeVM,
		IPAddress:    "10.10.1.100/24",
	}
	if result.MetaData != want {
		t.Errorf("VM metadata = %+v, want %+v", result.MetaData, want)
	}

	// No network → ipAddress falls back to dhcp.
	result, err = GenerateIncusConfig(IncusConfigParams{
		InstanceName: "v2", ImageAlias: "debian/12", RootPassword: "pw", Type: "virtual-machine",
	})
	if err != nil {
		t.Fatalf("VM without network: %v", err)
	}
	if result.MetaData.IPAddress != "dhcp" {
		t.Errorf("VM ipAddress = %q, want dhcp", result.MetaData.IPAddress)
	}

	// A generator error propagates unchanged.
	IncusConfigGenerateVMFunc = func(IncusConfigParams) (IncusConfigPayload, error) {
		return IncusConfigPayload{}, errors.New("vm boom")
	}
	if _, err := GenerateIncusConfig(IncusConfigParams{ImageAlias: "x", RootPassword: "y", Type: "virtual-machine"}); err == nil || err.Error() != "vm boom" {
		t.Errorf("VM generator error = %v", err)
	}
}

// TestIncusConfigJSONFieldOrder pins the serialisation order the TypeScript object
// literals produce; the parity harness may compare documents byte for byte.
func TestIncusConfigJSONFieldOrder(t *testing.T) {
	result, err := IncusConfigGenerateContainer(IncusConfigParams{
		InstanceName: "pi-1", ImageAlias: "debian/12", RootPassword: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	prefix := `{"configPayload":{"user.user-data":"#cloud-config\n`
	if !strings.HasPrefix(string(encoded), prefix) {
		t.Fatalf("payload JSON does not start with %q: %.120s", prefix, encoded)
	}
	if !strings.Contains(string(encoded), `","user.network-config":"version: 1\nconfig:\n`) {
		t.Errorf("network-config must follow user-data: %.200s", encoded)
	}
	tail := `,"metaData":{"sshPort":22,"rootPassword":"pw","osFamily":"debian","imageAlias":"debian/12","instanceType":"container","ipAddress":"dhcp"}}`
	if !strings.HasSuffix(string(encoded), tail) {
		t.Errorf("metadata JSON tail mismatch: %s", string(encoded)[len(encoded)-len(tail)-40:])
	}
}

func TestIncusConfigDetectOsInfo(t *testing.T) {
	cases := []struct {
		alias       string
		family      IncusConfigOsFamily
		pkg         string
		init        string
		sshService  string
		shell       string
		users       []string
		supportsInc bool
	}{
		{"ubuntu/noble", IncusConfigOsFamilyUbuntu, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "ubuntu"}, true},
		{"archlinux/current", IncusConfigOsFamilyArch, "pacman", "systemd", "sshd", "/bin/bash", []string{"root", "arch"}, true},
		{"kali/2024.1", IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "kali"}, true},
		{"debian/12", IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "debian"}, true},
		{"debian/bullseye", IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "debian"}, false},
		{"debian/10", IncusConfigOsFamilyDebian, "apt", "systemd", "ssh", "/bin/bash", []string{"root", "debian"}, false},
		{"alpine/3.20", IncusConfigOsFamilyAlpine, "apk", "openrc", "sshd", "/bin/ash", []string{"root"}, true},
		{"opensuse/tumbleweed", IncusConfigOsFamilySuse, "zypper", "systemd", "sshd", "/bin/bash", []string{"root"}, true},
		{"leap/15", IncusConfigOsFamilySuse, "zypper", "systemd", "sshd", "/bin/bash", []string{"root"}, true},
		{"fedora/40", IncusConfigOsFamilyRhel, "dnf", "systemd", "sshd", "/bin/bash", []string{"root", "fedora"}, true},
		{"almalinux/9", IncusConfigOsFamilyRhel, "dnf", "systemd", "sshd", "/bin/bash", []string{"root", "almalinux"}, true},
		{"almalinux/8", IncusConfigOsFamilyRhel8, "yum", "systemd", "sshd", "/bin/bash", []string{"root", "almalinux"}, true},
		{"rockylinux/8", IncusConfigOsFamilyRhel8, "yum", "systemd", "sshd", "/bin/bash", []string{"root", "rocky"}, true},
		{"oracle/8", IncusConfigOsFamilyRhel8, "yum", "systemd", "sshd", "/bin/bash", []string{"root", "oracle"}, true},
		{"centos/8", IncusConfigOsFamilyRhel8, "yum", "systemd", "sshd", "/bin/bash", []string{"root", "centos"}, true},
		// The container generator's rhel8 regex deliberately omits `rhel.*8`
		// (the VM module has a different pattern).
		{"rhel/8", IncusConfigOsFamilyRhel, "dnf", "systemd", "sshd", "/bin/bash", []string{"root", "root"}, true},
		{"windows/2022", IncusConfigOsFamilyOther, "apt", "systemd", "sshd", "/bin/bash", []string{"root"}, true},
	}

	for _, tc := range cases {
		got := incusConfigDetectOsInfo(tc.alias)
		if got.Family != tc.family || got.PackageManager != tc.pkg || got.InitSystem != tc.init ||
			got.SSHServiceName != tc.sshService || got.DefaultShell != tc.shell || got.SupportsInclude != tc.supportsInc {
			t.Errorf("%s → %+v", tc.alias, got)
		}
		if strings.Join(got.DefaultUsers, ",") != strings.Join(tc.users, ",") {
			t.Errorf("%s users = %v, want %v", tc.alias, got.DefaultUsers, tc.users)
		}
	}
}

func TestIncusConfigNetworkConfig(t *testing.T) {
	t.Run("dhcp dual stack", func(t *testing.T) {
		want := "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: dhcp\n      - type: dhcp6\n"
		if got := incusConfigNetworkConfig(nil, false); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("dhcp blocked shared ipv6", func(t *testing.T) {
		want := "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: dhcp\n"
		if got := incusConfigNetworkConfig(nil, true); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("static v4 plus routed v6", func(t *testing.T) {
		network := &IncusConfigStaticNetwork{IPAddress: "10.0.0.2/24", Gateway: "10.0.0.1", IPv6Address: "2001:db8::2"}
		want := "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: static\n" +
			"        address: 10.0.0.2/24\n        gateway: 10.0.0.1\n        dns_nameservers:\n          - 10.10.0.1\n" +
			"  - type: physical\n    name: eth1\n    subnets:\n      - type: static\n        address: 2001:db8::2/128\n" +
			"        gateway: fe80::1\n        dns_nameservers:\n          - 2606:4700:4700::1111\n          - 2001:4860:4860::8888\n"
		if got := incusConfigNetworkConfig(network, true); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestIncusConfigJSONStringify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", `"plain"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"tab\tnew\nline\rret", `"tab\tnew\nline\rret"`},
		{"bell\x07", `"bell\u0007"`},
		{"less<than>&amp", `"less<than>&amp"`},
		{"中文", `"中文"`},
		{"slash/ok", `"slash/ok"`},
	}
	for _, tc := range cases {
		if got := incusConfigJSONStringify(tc.in); got != tc.want {
			t.Errorf("incusConfigJSONStringify(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestIncusConfigShellEscaping(t *testing.T) {
	// Double-quote escaping: backslash first, then $, backtick and quote.
	input := "a\\b$c`d\"e" // a\b$c`d"e
	got := incusConfigEscapeForShellDoubleQuote(input)
	want := "a\\\\b\\$c\\`d\\\"e" // a\\b\$c\`d\"e
	if got != want {
		t.Errorf("escapeForShellDoubleQuote = %q, want %q", got, want)
	}
	// Single-quote escaping uses the close-escape-reopen dance.
	if got, want := incusConfigEscapeForShellSingleQuote("it's"), `it'"'"'s`; got != want {
		t.Errorf("escapeForShellSingleQuote = %q, want %q", got, want)
	}
}

func TestGenerateRandomPassword(t *testing.T) {
	if got := GenerateRandomPassword(0); got != "" {
		t.Errorf("length 0 = %q, want empty", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		password := GenerateRandomPassword(16)
		if len(password) != 16 {
			t.Fatalf("length = %d, want 16", len(password))
		}
		for _, char := range password {
			if !strings.ContainsRune(incusConfigRandomPasswordChars, char) {
				t.Fatalf("password contains %q, which is outside the TypeScript charset", char)
			}
		}
		if seen[password] {
			t.Fatalf("duplicate password generated: %q", password)
		}
		seen[password] = true
	}
	if chars := len(incusConfigRandomPasswordChars); chars != 70 {
		t.Errorf("charset length = %d, want 70", chars)
	}
	if got := len(GenerateRandomPassword(1)); got != 1 {
		t.Errorf("length 1 produced %d chars", got)
	}
}
