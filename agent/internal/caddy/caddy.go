package caddy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
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

	commands := [][]string{
		{"apt-get", "update", "-qq"},
		{"apt-get", "install", "-y", "-qq", "curl", "debian-keyring", "debian-archive-keyring", "apt-transport-https", "openssl"},
		{"curl", "-1sLf", "https://dl.cloudsmith.io/public/caddy/stable/gpg.key", "-o", "/usr/share/keyrings/caddy-stable-archive-keyring.gpg"},
		{"bash", "-c", "echo 'deb [signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/caddy/stable/deb/debian/ bullseye main' > /etc/apt/sources.list.d/caddy-stable.list"},
		{"apt-get", "update", "-qq"},
		{"apt-get", "install", "-y", "-qq", "caddy"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("caddy install step %q failed: %w (output: %s)", strings.Join(args, " "), err, truncate(string(output), 512))
		}
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
	return nil
}

func ensureService(ctx context.Context) error {
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
