// Port of server/src/lib/release-notifier.ts.
//
// When an instance is destroyed/deleted/expired, the package's host resources are
// released back to the node. This notifier pushes one message to the FIRST global
// notification channel describing how much CPU/memory is now free.
//
// Fidelity notes:
//
//   - The whole function is wrapped in a catch-all in the TypeScript
//     (`try { ... } catch { /* 通知发送失败不影响主流程 */ }`). Destruction must never
//     fail because a webhook is down, so this port reports nothing and always
//     returns normally. Callers still `.catch(() => {})` on the TypeScript promise;
//     the Go caller can simply call it synchronously.
//   - The `source` parameter is accepted but unused in the TypeScript
//     (`source: _source`), so the message text never mentions it. The caller-
//     supplied label is deliberately NOT substituted for the package source: the
//     buy link uses `official`/`market`, derived from the package owner's role.
//   - `getPackageById` returns snake_case fields (`host_ids`, `user_id`), which is
//     what PackageDetailRow exposes.
//   - Only the first global channel (`isGlobal: true`, no ordering) is used, which
//     the TypeScript expresses as findFirst without orderBy. The Go query pins
//     `ORDER BY id ASC` to make that choice deterministic; the TypeScript's choice
//     is row-order dependent but stable in practice for the single global channel
//     the product configures.
package lib

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ReleaseNotificationParams mirrors the sendReleaseNotification argument object.
type ReleaseNotificationParams struct {
	PackageID int32
	CPU       int32
	Memory    int32
	// Source is documentation only: the TypeScript discards it.
	Source string
}

// ReleaseNotifierTimeZone is the zone used for the "⏰" footer, matching
// `toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai' })`.
var ReleaseNotifierTimeZone = time.FixedZone("CST", 8*3600)

// ReleaseNotifierFormatMemory mirrors formatMem:
//
//	mb >= 1024 -> (mb/1024).toFixed(1) + " GB"
//	else        -> mb + " MB"
func ReleaseNotifierFormatMemory(mb int32) string {
	if mb >= 1024 {
		return strconv.FormatFloat(JSFixed(float64(mb)/1024, 1), 'f', 1, 64) + " GB"
	}
	return strconv.FormatInt(int64(mb), 10) + " MB"
}

// ReleaseNotifierTimestamp mirrors
// `new Date().toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai' })` — Node's
// zh-CN rendering is `YYYY/M/D HH:mm:ss` with no zero padding on the date.
func ReleaseNotifierTimestamp(now time.Time) string {
	return now.In(ReleaseNotifierTimeZone).Format("2006/1/2 15:04:05")
}

// ReleaseNotifierPackageSource mirrors
// `pkgOwner?.role === 'admin' ? 'official' : 'market'`.
func ReleaseNotifierPackageSource(ctx context.Context, ownerID int32) string {
	if ownerID == 0 {
		// `pkg.user_id ? ... : null` — a falsy owner id means no lookup, and
		// `pkgOwner?.role === 'admin'` is false, so the source is 'market'.
		return "market"
	}
	if db.DB == nil {
		return "market"
	}
	var user models.User
	err := db.DB.WithContext(ctx).
		Select("role").
		First(&user, ownerID).Error
	if err != nil || user.Role != models.UserRoleAdmin {
		return "market"
	}
	return "official"
}

// ReleaseNotifierHostLine mirrors the per-host line:
// `• ${h.name.toUpperCase()}: CPU ${cpuAvailable}% | 内存 ${formatMem(memAvailable)}`.
func ReleaseNotifierHostLine(hostName string, cpuAvailable, memAvailable int64) string {
	return fmt.Sprintf("• %s: CPU %d%% | 内存 %s",
		strings.ToUpper(hostName), cpuAvailable, ReleaseNotifierFormatMemory(int32(memAvailable)))
}

// ReleaseNotifierMessage mirrors the notifyMessage array joined with "\n".
func ReleaseNotifierMessage(cpu, memory int32, hostLines []string, now time.Time) string {
	return strings.Join([]string{
		fmt.Sprintf("+%d%% CPU / +%s 内存", cpu, ReleaseNotifierFormatMemory(memory)),
		"",
		"当前可用配额：",
		strings.Join(hostLines, "\n"),
		"",
		"⏰ " + ReleaseNotifierTimestamp(now),
	}, "\n")
}

// SendReleaseNotification mirrors sendReleaseNotification: it never returns an
// error because the TypeScript swallows every failure.
func SendReleaseNotification(ctx context.Context, params ReleaseNotificationParams) {
	// Recover from a nil database and from any unexpected panic in the same way
	// the TypeScript's `catch {}` would: the notification is best-effort.
	defer func() { _ = recover() }()

	if db.DB == nil {
		return
	}

	var channel models.NotificationChannel
	err := db.DB.WithContext(ctx).
		Where("is_global = ?", true).
		Order("id ASC").
		Take(&channel).Error
	if err != nil {
		return
	}

	pkg, err := db.GetPackageById(ctx, params.PackageID)
	if err != nil || pkg == nil {
		return
	}

	hostDetails := make([]string, 0, len(pkg.HostIDs))
	for _, hostID := range pkg.HostIDs {
		host, hostErr := db.HostsGetByID(ctx, hostID)
		if hostErr != nil || host == nil {
			continue
		}
		cpuAvailable := int64(host.CPUAllowanceMax) - int64(host.CPUUsed)
		memAvailable := int64(host.MemoryMax) - int64(host.MemoryUsed)
		hostDetails = append(hostDetails, ReleaseNotifierHostLine(host.Name, cpuAvailable, memAvailable))
	}

	title := "⭕ 资源释放 - " + pkg.Name

	var link *NotificationLink
	if siteURL := os.Getenv("SITE_URL"); siteURL != "" {
		source := ReleaseNotifierPackageSource(ctx, pkg.UserID)
		link = &NotificationLink{
			Label: "🛒 立即开通",
			URL: siteURL + "/instances/create?source=" + source +
				"&package=" + strconv.FormatInt(int64(params.PackageID), 10),
		}
	}

	message := ReleaseNotifierMessage(params.CPU, params.Memory, hostDetails, time.Now().UTC())
	_ = SendToChannel(ctx, int64(channel.ID), title, message, link)
}
