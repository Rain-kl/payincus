package caddy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// 面板<->Agent 已有 HMAC/隧道鉴权；Caddy Admin 只绑定宿主机回环 localhost，
// 无管理凭据、无证书。Agent 安装时也不从面板拉取脚本（消除脚本注入面），
// Caddyfile 内容由本包内联生成。

const caddyAdminPort = 2019
const caddyConfigDir = "/etc/caddy"
const caddyfilePath = "/etc/caddy/Caddyfile"

// Status 描述本机 Caddy 可用状态（随心跳上报）。
type Status struct {
	Available bool   `json:"available"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
	Active    bool   `json:"active"`
}

// Detect 快速探测：caddy 二进制存在且 systemd 服务 active 即视为可用。
func Detect() Status {
	status := Status{Available: false, Port: caddyAdminPort}
	if _, err := exec.LookPath("caddy"); err != nil {
		return status
	}
	version, err := exec.Command("caddy", "version").Output()
	if err == nil {
		status.Version = strings.TrimSpace(string(version))
	}
	status.Active = serviceActive("caddy")
	status.Available = status.Active
	return status
}

func serviceActive(unit string) bool {
	err := exec.Command("systemctl", "is-active", "--quiet", unit).Run()
	return err == nil
}

// Install 在宿主机本地部署 Caddy（仅回环 admin，无公网监听）。
// 幂等：已安装且服务正常时直接返回 success。
func Install(ctx context.Context) error {
	// 1. 安装/更新 caddy 二进制（Ubuntu/Debian apt）。
	if err := installPackage(ctx); err != nil {
		return err
	}

	// 2. 写最小 Caddyfile：只暴露回环 Admin API，不监听任何公网端口。
	if err := writeCaddyfile(); err != nil {
		return err
	}

	// 3. systemd enable + start。
	if err := ensureService(ctx); err != nil {
		return err
	}

	// 4. 探活 127.0.0.1:2019。
	if err := waitAdminReady(ctx, caddyAdminPort); err != nil {
		return err
	}
	log.Printf("[caddy] installed and ready on 127.0.0.1:%d", caddyAdminPort)
	return nil
}

func installPackage(ctx context.Context) error {
	if _, err := exec.LookPath("caddy"); err == nil {
		return nil // 已装
	}

	// 动态识别发行版 codename，避免硬编码 bullseye 导致非 Debian 11 装不上。
	codename := detectDebianCodename()

	if err := runStep(ctx, "apt-get", "update", "-qq"); err != nil {
		return err
	}
	if err := runStep(ctx, "apt-get", "install", "-y", "-qq", "curl", "debian-keyring", "debian-archive-keyring", "apt-transport-https", "openssl", "gpg"); err != nil {
		return err
	}

	// Caddy 官方源：先下载 ASCII armored 公钥，再 dearmor 为二进制 keyring，
	// 否则 apt 报 "unsupported filetype"/NO_PUBKEY 拒绝该仓库。
	keyring := "/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
	if err := runStep(ctx, "bash", "-c",
		"curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o "+keyring); err != nil {
		return err
	}
	if err := runStep(ctx, "bash", "-c",
		"echo 'deb [signed-by="+keyring+"] https://dl.cloudsmith.io/public/caddy/stable/deb/debian/ "+codename+" main' > /etc/apt/sources.list.d/caddy-stable.list"); err != nil {
		return err
	}
	if err := runStep(ctx, "apt-get", "update", "-qq"); err != nil {
		return err
	}
	if err := runStep(ctx, "apt-get", "install", "-y", "-qq", "caddy"); err != nil {
		// 新发行版（如 Ubuntu 26.04）cloudsmith 可能还没有对应 codename，
		// apt 源不可用时回退官方静态二进制，保证 Caddy 仍可安装。
		log.Printf("[caddy] apt install failed, falling back to official static binary: %v", err)
		if fallbackErr := installStaticBinary(ctx); fallbackErr != nil {
			return fmt.Errorf("caddy apt install failed and static binary fallback also failed: %w (apt: %v)", fallbackErr, err)
		}
	}
	return nil
}

// installStaticBinary 下载 Caddy 官方静态二进制到 /usr/bin/caddy。
func installStaticBinary(ctx context.Context) error {
	arch := runtime.GOARCH
	// Go 的 amd64/arm64 映射到 Caddy 下载参数。
	if arch == "amd64" {
		arch = "amd64"
	} else if arch == "arm64" {
		arch = "arm64"
	} else {
		return fmt.Errorf("unsupported arch for static caddy: %s", arch)
	}
	url := "https://caddyserver.com/api/download?os=linux&arch=" + arch
	tmp := "/tmp/caddy-download"
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", "-o", tmp, url)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("download caddy static binary: %w (output: %s)", err, truncate(string(output), 512))
	}
	if err := runStep(ctx, "install", "-m", "0755", tmp, "/usr/bin/caddy"); err != nil {
		return err
	}
	_ = os.Remove(tmp)
	return nil
}

// detectDebianCodename 从 /etc/os-release 读取 VERSION_CODENAME；失败回退 bullseye。
func detectDebianCodename() string {
	content, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "bullseye"
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "VERSION_CODENAME=") {
			value := strings.TrimPrefix(line, "VERSION_CODENAME=")
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if value != "" {
				return value
			}
		}
	}
	return "bullseye"
}

func runStep(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("caddy install step %q failed: %w (output: %s)", strings.Join(append([]string{name}, args...), " "), err, truncate(string(output), 512))
	}
	return nil
}

func writeCaddyfile() error {
	if err := os.MkdirAll(caddyConfigDir, 0o750); err != nil {
		return err
	}
	content := `# Managed by incudal-agent: Caddy Admin API on loopback only.
# No public listener, no credentials, no certificates.
{
    admin localhost:2019
    auto_https disable_redirects
}
`
	if err := os.WriteFile(caddyfilePath, []byte(content), 0o640); err != nil {
		return err
	}
	// 确保 caddy 系统用户存在（手动安装二进制时 apt 不会创建它）。
	if err := exec.Command("id", "-u", "caddy").Run(); err != nil {
		_ = exec.Command("useradd", "--system", "--home", "/var/lib/caddy", "--shell", "/usr/sbin/nologin", "--no-create-home", "caddy").Run()
	}
	// systemd unit 以 User=caddy 运行，Caddyfile 与目录必须 root:caddy 可读，
	// 否则 caddy 进程报 "reading config from file: permission denied" 起不来。
	_ = exec.Command("chown", "-R", "root:caddy", caddyConfigDir).Run()
	_ = exec.Command("chmod", "0640", caddyfilePath).Run()
	_ = exec.Command("chmod", "0750", caddyConfigDir).Run()
	return nil
}

func ensureService(ctx context.Context) error {
	// 兼容非 deb 包安装（手动二进制）：caddy.service unit 可能不存在，
	// 缺了直接 enable/restart 会失败。没有就写一个标准 unit。
	if err := ensureSystemdUnit(); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "caddy"},
		{"restart", "caddy"},
	} {
		cmd := exec.CommandContext(ctx, "systemctl", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s failed: %w (output: %s)", strings.Join(args, " "), err, truncate(string(output), 512))
		}
	}
	return nil
}

// ensureSystemdUnit 检查 caddy.service 是否存在，不存在则写入最小 unit。
func ensureSystemdUnit() error {
	const unitPath = "/etc/systemd/system/caddy.service"
	if _, err := os.Stat(unitPath); err == nil {
		return nil
	}
	content := `[Unit]
Description=Caddy
After=network.target

[Service]
Type=simple
User=caddy
Group=caddy
ExecStart=/usr/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/bin/caddy reload --config /etc/caddy/Caddyfile
TimeoutStopSec=5s
LimitNOFILE=1048576
LimitNPROC=512
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return err
	}
	_ = os.MkdirAll(caddyConfigDir, 0o750)
	// unit 内 User=caddy 需要 caddy 系统用户存在；apt 包会创建，手动安装不一定。缺则用 nologin 创建。
	if err := exec.Command("id", "-u", "caddy").Run(); err != nil {
		_ = exec.Command("useradd", "--system", "--home", "/var/lib/caddy", "--shell", "/usr/sbin/nologin", "--no-create-home", "caddy").Run()
	}
	return nil
}

func waitAdminReady(ctx context.Context, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/config/", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		client := &http.Client{Timeout: 2 * time.Second}
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("caddy admin not ready on 127.0.0.1:%d within 30s", port)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
