// Unit tests for the release-notifier port (server/src/lib/release-notifier.ts).
//
// The notifier is intentionally fire-and-forget ("通知发送失败不影响主流程"), so the
// tests focus on the two things that are observable to a user: the message format
// and the fact that a missing database/channel never panics the destroy flow.
package lib

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// TestReleaseNotifierFormatMemory mirrors formatMem:
//
//	>= 1024 MB → (mb/1024).toFixed(1) + " GB"
//	<  1024 MB → mb + " MB"
func TestReleaseNotifierFormatMemory(t *testing.T) {
	for _, tc := range []struct {
		mb   int32
		want string
	}{
		{0, "0 MB"},
		{800, "800 MB"},
		{1023, "1023 MB"},
		// 1024/1024 = 1 → toFixed(1) = "1.0 GB"
		{1024, "1.0 GB"},
		{1536, "1.5 GB"},
		{2048, "2.0 GB"},
		{2560, "2.5 GB"},
		// 4096+512 = 4608/1024 = 4.5
		{4608, "4.5 GB"},
	} {
		if got := ReleaseNotifierFormatMemory(tc.mb); got != tc.want {
			t.Errorf("formatMem(%d) = %q, want %q", tc.mb, got, tc.want)
		}
	}
}

// TestReleaseNotifierTimestamp pins the zh-CN Asia/Shanghai rendering used in the
// "⏰" footer. Node prints `2026/11/10 20:00:00` for this instant (verified with
// `node -e "new Date(...).toLocaleString('zh-CN',{timeZone:'Asia/Shanghai'})"`).
func TestReleaseNotifierTimestamp(t *testing.T) {
	instant := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if got, want := ReleaseNotifierTimestamp(instant), "2026/11/10 20:00:00"; got != want {
		t.Errorf("timestamp = %q, want %q", got, want)
	}
	// A UTC instant that lands on the previous Shanghai day must not roll over.
	early := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got, want := ReleaseNotifierTimestamp(early), "2026/1/2 11:04:05"; got != want {
		t.Errorf("timestamp = %q, want %q (no zero padding, like zh-CN)", got, want)
	}
}

// TestReleaseNotifierHostLine pins the per-host quota line.
func TestReleaseNotifierHostLine(t *testing.T) {
	got := ReleaseNotifierHostLine("parity-host", 9500, 2048)
	want := "• PARITY-HOST: CPU 9500% | 内存 2.0 GB"
	if got != want {
		t.Errorf("host line = %q, want %q", got, want)
	}
	// Negative availability is printed verbatim: Node subtracts without clamping.
	if got := ReleaseNotifierHostLine("h", -5, 512); !strings.Contains(got, "CPU -5%") {
		t.Errorf("host line = %q, want negative CPU preserved", got)
	}
}

// TestReleaseNotifierMessage pins the full message body layout.
func TestReleaseNotifierMessage(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	message := ReleaseNotifierMessage(2, 1024, []string{
		"• HOST-A: CPU 50% | 内存 1.0 GB",
		"• HOST-B: CPU 10% | 内存 512 MB",
	}, now)

	want := strings.Join([]string{
		"+2% CPU / +1.0 GB 内存",
		"",
		"当前可用配额：",
		"• HOST-A: CPU 50% | 内存 1.0 GB",
		"• HOST-B: CPU 10% | 内存 512 MB",
		"",
		"⏰ 2026/11/10 20:00:00",
	}, "\n")
	if message != want {
		t.Errorf("message =\n%s\nwant\n%s", message, want)
	}
}

// TestReleaseNotifierPackageSourceWithoutDatabase pins the fallback: with no
// database the role lookup cannot prove 'admin', so the link says market.
func TestReleaseNotifierPackageSourceWithoutDatabase(t *testing.T) {
	original := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = original })

	if got := ReleaseNotifierPackageSource(context.Background(), 123); got != "market" {
		t.Errorf("source = %q, want market (no database)", got)
	}
	if got := ReleaseNotifierPackageSource(context.Background(), 0); got != "market" {
		t.Errorf("source = %q, want market (no owner)", got)
	}
}

// TestSendReleaseNotificationNeverFails pins the `catch {}` contract: the destroy
// flow must not fail or panic when there is no database.
func TestReleaseSendNotificationNeverFails(t *testing.T) {
	original := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = original })

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("SendReleaseNotification panicked: %v", recovered)
		}
	}()
	SendReleaseNotification(context.Background(), ReleaseNotificationParams{
		PackageID: 950001, CPU: 1, Memory: 1024, Source: "用户销毁实例",
	})
}

// TestReleaseNotifierSiteURLEnv documents that the buy link is only attached when
// SITE_URL is configured (the env read happens in SendReleaseNotification).
func TestReleaseNotifierSiteURLEnv(t *testing.T) {
	t.Setenv("SITE_URL", "")
	if value := os.Getenv("SITE_URL"); value != "" {
		t.Fatalf("SITE_URL = %q, want empty", value)
	}
}
