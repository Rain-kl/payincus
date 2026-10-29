// Port of server/src/lib/notifier.ts.
//
// Notification delivery (Telegram / Discord / generic webhook) plus the in-app
// inbox message that is written on every event.
//
// Outbound policy: the TypeScript module validates every user-controlled URL with
// assertSafeWebhookUrl and then sends it through safeFetch (SSRF guard). Go keeps
// both layers: AssertSafeHTTPURL rejects private/reserved targets up front and the
// request is issued through the shared connect-time-revalidating transport
// (SafeOutboundHTTPClient().Transport), which also re-checks addresses at dial
// time. The per-request client only overrides the redirect policy to mirror
// `redirect: 'manual'`.
package lib

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== 配置 ====================

const (
	// MAX_RETRIES / RETRY_DELAY_MS mirror the TypeScript retry configuration.
	notifierMaxRetries   = 3
	notifierRetryDelay   = 2000 * time.Millisecond
	notifierFetchTimeout = 15 * time.Second
	notifierErrorPreview = 2000
)

// EventType mirrors the TypeScript EventType union.
type EventType string

const (
	EventSnapshotCreated            EventType = "snapshot_created"
	EventSnapshotRestored           EventType = "snapshot_restored"
	EventSnapshotDeleted            EventType = "snapshot_deleted"
	EventSnapshotFailed             EventType = "snapshot_failed"
	EventBackupCreated              EventType = "backup_created"
	EventBackupFailed               EventType = "backup_failed"
	EventBackupDeleted              EventType = "backup_deleted"
	EventBackupRestored             EventType = "backup_restored"
	EventBackupUploaded             EventType = "backup_uploaded"
	EventInstanceCreated            EventType = "instance_created"
	EventInstanceCreateFailed       EventType = "instance_create_failed"
	EventInstanceCreateTimeout      EventType = "instance_create_timeout"
	EventInstanceStarted            EventType = "instance_started"
	EventInstanceStopped            EventType = "instance_stopped"
	EventInstanceRestarted          EventType = "instance_restarted"
	EventInstanceRebuilt            EventType = "instance_rebuilt"
	EventInstanceCloned             EventType = "instance_cloned"
	EventInstanceHostChanged        EventType = "instance_host_changed"
	EventInstanceTaskFailed         EventType = "instance_task_failed"
	EventInstanceUnexpectedStop     EventType = "instance_unexpected_stop"
	EventInstanceDeleted            EventType = "instance_deleted"
	EventInstanceDeletedByHostOwner EventType = "instance_deleted_by_host_owner"
	EventInstanceSuspended          EventType = "instance_suspended"
	EventInstanceUnsuspended        EventType = "instance_unsuspended"

	// 计费相关
	EventInstanceRenewed         EventType = "instance_renewed"
	EventInstancePlanChanged     EventType = "instance_plan_changed"
	EventInstancePriceChanged    EventType = "instance_price_changed"
	EventInstanceExpiring        EventType = "instance_expiring"
	EventInstanceExtended        EventType = "instance_extended"
	EventInstancesBatchExtended  EventType = "instances_batch_extended"
	EventAutoRenewFailed         EventType = "auto_renew_failed"
	EventRefundCompleted         EventType = "refund_completed"
	EventInstanceDeletedRefunded EventType = "instance_deleted_refunded"
	EventAutoSnapshot            EventType = "auto_snapshot"
	EventAutoSnapshotRotated     EventType = "auto_snapshot_rotated"
	EventAutoBackup              EventType = "auto_backup"
	EventAutoBackupRotated       EventType = "auto_backup_rotated"

	// 社交互动类
	EventFriendRequest  EventType = "friend_request"
	EventFriendAccepted EventType = "friend_accepted"
	EventFriendRejected EventType = "friend_rejected"
	EventFriendRemoved  EventType = "friend_removed"

	// 套餐与共享类
	EventPackageShared       EventType = "package_shared"
	EventPackageShareRevoked EventType = "package_share_revoked"

	// 实例转移类
	EventTransferReceived          EventType = "transfer_received"
	EventTransferAccepted          EventType = "transfer_accepted"
	EventTransferOwnershipReceived EventType = "transfer_ownership_received"
	EventTransferRejected          EventType = "transfer_rejected"
	EventTransferCancelled         EventType = "transfer_cancelled"

	// 安全与账户类
	EventLoginNewDevice  EventType = "login_new_device"
	EventPasswordChanged EventType = "password_changed"
	Event2FAEnabled      EventType = "2fa_enabled"
	Event2FADisabled     EventType = "2fa_disabled"

	// 配额与资源类
	EventQuotaWarning EventType = "quota_warning"

	// 工单类
	EventTicketCreated       EventType = "ticket_created"
	EventTicketReplied       EventType = "ticket_replied"
	EventTicketStatusChanged EventType = "ticket_status_changed"
	EventTicketClosed        EventType = "ticket_closed"
	EventTicketAutoClosed    EventType = "ticket_auto_closed"

	// 签到类
	EventRedeemCodeUsed EventType = "redeem_code_used"

	// AFF 相关
	EventAffApplied EventType = "aff_applied"

	// 续费价格调整
	EventInstanceRenewalPriceUpdated EventType = "instance_renewal_price_updated"

	// 公共 API 受控通知
	EventPublicAPINotification EventType = "public_api_notification"
)

// EventData mirrors the TypeScript EventData interface. It is intentionally a map
// so unknown keys survive into the webhook payload and the inbox `data` column,
// exactly like the TypeScript `[key: string]: unknown` index signature.
type EventData map[string]any

// EventTemplate mirrors EventTemplate: the message depends on the event data.
type EventTemplate struct {
	Title   string
	Message func(data EventData) string
}

// NotificationResult mirrors NotificationResult. An empty Error means "null".
type NotificationResult struct {
	Success bool
	Error   string
}

// SendResult mirrors SendResult.
type SendResult struct {
	Sent   int `json:"sent"`
	Failed int `json:"failed"`
}

// TelegramConfig / DiscordConfig / WebhookConfig mirror the TypeScript configs.
type TelegramConfig struct {
	BotToken string
	ChatID   any
}

type DiscordConfig struct {
	WebhookURL string
}

type WebhookConfig struct {
	URL    string
	Secret string
}

// NotificationChannel mirrors the subset of types/database.ts NotificationChannel
// that the outbound senders need (config here is the raw JSON string).
type NotificationChannel struct {
	ID     int32
	Type   string
	Config string
}

// notifierDelay is the sleep used between retries; tests may shorten it.
var notifierDelay = func(d time.Duration) { time.Sleep(d) }

// notifierHTTPClient sends outbound notification requests. It reuses the
// SSRF-revalidating transport from SafeOutboundHTTPClient but keeps
// `redirect: 'manual'` semantics: 3xx responses are returned to the caller
// instead of being followed.
var notifierHTTPClient = &http.Client{
	Transport: SafeOutboundHTTPClient().Transport,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ==================== JS 值语义辅助 ====================

// notifierTruthy mirrors JavaScript truthiness for values that arrive in an
// EventData map.
func notifierTruthy(value any, present bool) bool {
	if !present || value == nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int32:
		return typed != 0
	case int64:
		return typed != 0
	case json.Number:
		number, err := typed.Float64()
		return err != nil || number != 0
	default:
		// Objects and arrays are always truthy in JavaScript.
		return true
	}
}

// notifierNumber mirrors Number(value) for the scalar values used in templates.
func notifierNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case bool:
		if typed {
			return 1
		}
		return 0
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0
		}
		return number
	case string:
		return jsToNumber(typed)
	default:
		return 0
	}
}

// notifierIsStrictZero mirrors `value !== 0`: only an actual numeric zero is "equal".
func notifierIsStrictZero(value any, present bool) bool {
	if !present || value == nil {
		return false
	}
	switch value.(type) {
	case float64, float32, int, int32, int64, json.Number:
		return notifierNumber(value) == 0
	default:
		return false
	}
}

// notifierString mirrors String(value) inside a template literal. A missing key is
// "undefined" and an explicit null is "null", matching JavaScript.
func notifierString(value any, present bool) string {
	if !present {
		return "undefined"
	}
	if value == nil {
		return "null"
	}
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, notifierString(item, true))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return jsNumberToString(notifierNumber(value))
	}
}

// notifierGet returns a value together with its "present" flag.
func notifierGet(data EventData, key string) (any, bool) {
	if data == nil {
		return nil, false
	}
	value, present := data[key]
	return value, present
}

// notifierValue is notifierString for a key lookup.
func notifierValue(data EventData, key string) string {
	value, present := notifierGet(data, key)
	return notifierString(value, present)
}

// hasKey mirrors `data.x !== undefined`.
func hasKey(data EventData, key string) bool {
	_, present := notifierGet(data, key)
	return present
}

// ifTruthy mirrors `if (data.x)`.
func ifTruthy(data EventData, key string) bool {
	value, present := notifierGet(data, key)
	return notifierTruthy(value, present)
}

// ==================== 格式化辅助 ====================

// formatNotificationMemory mirrors the notifier's local formatMemory helper:
// falsy input renders "-", >= 1024 renders GB with one decimal, else MB.
func formatNotificationMemory(data EventData, key string) string {
	value, present := notifierGet(data, key)
	if !notifierTruthy(value, present) {
		return "-"
	}
	megabytes := notifierNumber(value)
	if megabytes >= 1024 {
		return toFixedJS(megabytes/1024, 1) + " GB"
	}
	return jsNumberToString(megabytes) + " MB"
}

// formatNotificationDisk mirrors the notifier's local formatDisk helper (GB without
// decimals).
func formatNotificationDisk(data EventData, key string) string {
	value, present := notifierGet(data, key)
	if !notifierTruthy(value, present) {
		return "-"
	}
	megabytes := notifierNumber(value)
	if megabytes >= 1024 {
		return toFixedJS(megabytes/1024, 0) + " GB"
	}
	return jsNumberToString(megabytes) + " MB"
}

// money mirrors `Number(x).toFixed(2)`.
func money(value any) string {
	return toFixedJS(notifierNumber(value), 2)
}

// ==================== 事件模板 ====================

// EventTemplates maps every event type to its title/message template. It is
// exported so feature modules can preview text; SendNotification uses it directly.
var EventTemplates = map[EventType]EventTemplate{
	EventSnapshotCreated: {
		Title: "✅ 快照创建成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的快照「" + notifierValue(data, "snapshotName") + "」创建成功"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventSnapshotRestored: {
		Title: "🔄 快照恢复成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已从快照「" + notifierValue(data, "snapshotName") + "」恢复"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventSnapshotDeleted: {
		Title: "🗑️ 快照已删除",
		Message: func(data EventData) string {
			return "实例「" + notifierValue(data, "instanceName") + "」的快照「" + notifierValue(data, "snapshotName") + "」已删除"
		},
	},
	EventSnapshotFailed: {
		Title: "❌ 快照创建失败",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的快照创建失败"
			if ifTruthy(data, "error") {
				msg += "\n错误: " + notifierValue(data, "error")
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventBackupCreated: {
		Title: "✅ 备份创建成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的备份「" + notifierValue(data, "backupName") + "」创建成功"
			if ifTruthy(data, "backupSize") {
				msg += " (" + notifierValue(data, "backupSize") + ")"
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventBackupFailed: {
		Title: "❌ 备份创建失败",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的备份创建失败"
			if ifTruthy(data, "error") {
				msg += "\n错误: " + notifierValue(data, "error")
			}
			return msg
		},
	},
	EventBackupDeleted: {
		Title: "🗑️ 备份已删除",
		Message: func(data EventData) string {
			return "实例「" + notifierValue(data, "instanceName") + "」的备份「" + notifierValue(data, "backupName") + "」已删除"
		},
	},
	EventBackupRestored: {
		Title: "🔄 备份恢复成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已从备份「" + notifierValue(data, "backupName") + "」恢复"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventBackupUploaded: {
		Title: "☁️ 备份上传成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的备份「" + notifierValue(data, "backupName") + "」已上传"
			if ifTruthy(data, "storageName") {
				msg += "\n存储: " + notifierValue(data, "storageName")
			}
			if ifTruthy(data, "fileName") {
				msg += "\n文件: " + notifierValue(data, "fileName")
			}
			return msg
		},
	},
	EventInstanceCreated: {
		Title: "🚀 实例创建成功",
		Message: func(data EventData) string {
			msg := "您的实例「" + notifierValue(data, "instanceName") + "」已创建成功！"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
				if ifTruthy(data, "hostLocation") {
					msg += " (" + notifierValue(data, "hostLocation") + ")"
				}
			}
			if ifTruthy(data, "image") {
				msg += "\n镜像: " + notifierValue(data, "image")
			}
			resources := make([]string, 0, 3)
			if ifTruthy(data, "cpu") {
				resources = append(resources, "CPU "+notifierValue(data, "cpu")+"%")
			}
			if ifTruthy(data, "memory") {
				resources = append(resources, "内存 "+formatNotificationMemory(data, "memory"))
			}
			if ifTruthy(data, "disk") {
				resources = append(resources, "磁盘 "+formatNotificationDisk(data, "disk"))
			}
			if len(resources) > 0 {
				msg += "\n配置: " + strings.Join(resources, " | ")
			}
			if ifTruthy(data, "networkMode") {
				networkMode := notifierValue(data, "networkMode")
				label := networkMode
				switch networkMode {
				case "nat_ipv6":
					label = "NAT + IPv6"
				case "nat":
					label = "NAT"
				}
				msg += "\n网络: " + label
			}
			if ifTruthy(data, "ipv4") {
				msg += "\nIPv4: " + notifierValue(data, "ipv4")
			}
			if ifTruthy(data, "ipv6") {
				msg += "\nIPv6: " + notifierValue(data, "ipv6")
			}
			return msg
		},
	},
	EventInstanceCreateFailed: {
		Title: "❌ 实例创建失败",
		Message: func(data EventData) string {
			msg := "您的实例「" + notifierValue(data, "instanceName") + "」创建失败"
			if ifTruthy(data, "error") {
				msg += "\n错误: " + notifierValue(data, "error")
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			msg += "\n资源已自动回滚，请稍后重试或联系管理员"
			return msg
		},
	},
	EventInstanceCreateTimeout: {
		Title: "⏰ 实例创建超时",
		Message: func(data EventData) string {
			msg := "您的实例「" + notifierValue(data, "instanceName") + "」创建超时（超过10分钟）"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			msg += "\n资源已自动回滚，请稍后重试"
			msg += "\n可能原因: 镜像下载缓慢 / 节点繁忙 / 网络问题"
			return msg
		},
	},
	EventInstanceStarted: {
		Title: "▶️ 实例已启动",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已成功启动"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "ipv4") {
				msg += "\nIPv4: " + notifierValue(data, "ipv4")
			}
			if ifTruthy(data, "ipv6") {
				msg += "\nIPv6: " + notifierValue(data, "ipv6")
			}
			return msg
		},
	},
	EventInstanceStopped: {
		Title: "⏹️ 实例已停止",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已停止运行"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventInstanceRestarted: {
		Title: "🔄 实例已重启",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已重启完成"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "ipv4") {
				msg += "\nIPv4: " + notifierValue(data, "ipv4")
			}
			if ifTruthy(data, "ipv6") {
				msg += "\nIPv6: " + notifierValue(data, "ipv6")
			}
			return msg
		},
	},
	EventInstanceRebuilt: {
		Title: "🛠️ 实例已重装",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已成功重装系统"
			if ifTruthy(data, "newImage") {
				msg += "\n新镜像: " + notifierValue(data, "newImage")
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventInstanceCloned: {
		Title: "📋 实例已克隆",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已成功克隆"
			if ifTruthy(data, "newInstanceName") {
				msg += "\n新实例: " + notifierValue(data, "newInstanceName")
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventInstanceHostChanged: {
		Title: "🔁 实例已改节点",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已成功迁移到新节点"
			if ifTruthy(data, "oldHostName") || ifTruthy(data, "newHostName") {
				oldName := "-"
				if ifTruthy(data, "oldHostName") {
					oldName = notifierValue(data, "oldHostName")
				}
				newName := "-"
				if ifTruthy(data, "newHostName") {
					newName = notifierValue(data, "newHostName")
				}
				msg += "\n节点: " + oldName + " -> " + newName
			}
			if ifTruthy(data, "ipv4") {
				msg += "\nIPv4: " + notifierValue(data, "ipv4")
			}
			if ifTruthy(data, "ipv6") {
				msg += "\nIPv6: " + notifierValue(data, "ipv6")
			}
			return msg
		},
	},
	EventInstanceTaskFailed: {
		Title: "❌ 实例操作失败",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的操作失败"
			if ifTruthy(data, "taskType") {
				msg += "\n操作: " + notifierValue(data, "taskType")
			}
			if ifTruthy(data, "error") {
				msg += "\n错误: " + notifierValue(data, "error")
			}
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventInstanceUnexpectedStop: {
		Title: "⚠️ 实例异常停止",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已异常停止运行"
			msg += "\n可能原因: 节点资源不足 / 实例崩溃 / 管理员操作"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + strings.ToUpper(notifierValue(data, "hostName"))
			}
			msg += "\n请前往面板检查实例状态"
			return msg
		},
	},
	EventInstanceDeleted: {
		Title: "🗑️ 实例已删除",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已永久删除"
			if ifTruthy(data, "hostName") {
				msg += "\n原节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventInstanceDeletedByHostOwner: {
		Title: "⚠️ 您的实例已被删除",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已被删除"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "deleteReason") {
				msg += "\n原因: " + notifierValue(data, "deleteReason")
			}
			return msg
		},
	},
	EventInstanceSuspended: {
		Title: "🚫 您的实例已被封停",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已被封停"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "suspendReason") {
				msg += "\n原因: " + notifierValue(data, "suspendReason")
			}
			msg += "\n封停期间，实例无法启动、重启、重装等操作"
			msg += "\n如有疑问，请发送工单"
			return msg
		},
	},
	EventInstanceUnsuspended: {
		Title: "✅ 您的实例已解除封停",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已解除封停"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			msg += "\n您现在可以正常使用该实例"
			return msg
		},
	},
	EventAutoSnapshot: {
		Title: "📸 自动快照完成",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」自动快照完成"
			msg += "\n快照: " + notifierValue(data, "snapshotName")
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventAutoSnapshotRotated: {
		Title: "🔄 自动快照轮换",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」快照配额已满 (" + notifierValue(data, "quotaLimit") + " 个)"
			msg += "\n已删除: " + notifierValue(data, "deletedName")
			msg += "\n已创建: " + notifierValue(data, "snapshotName")
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventAutoBackup: {
		Title: "💾 自动备份完成",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」自动备份完成"
			msg += "\n备份: " + notifierValue(data, "backupName")
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventAutoBackupRotated: {
		Title: "🔄 自动备份轮换",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」备份配额已满 (" + notifierValue(data, "quotaLimit") + " 个)"
			msg += "\n已删除: " + notifierValue(data, "deletedName")
			msg += "\n已创建: " + notifierValue(data, "backupName")
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},

	// ==================== 社交互动类 ====================
	EventFriendRequest: {
		Title: "👥 新的好友申请",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "fromUsername") + "」向您发送了好友申请，请前往好友管理页面处理"
		},
	},
	EventFriendAccepted: {
		Title: "✅ 好友申请已通过",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "toUsername") + "」已接受您的好友申请，您们现在是好友了"
		},
	},
	EventFriendRejected: {
		Title: "❌ 好友申请被拒绝",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "toUsername") + "」拒绝了您的好友申请"
		},
	},
	EventFriendRemoved: {
		Title: "💔 好友关系已解除",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "fromUsername") + "」已将您从好友列表中移除"
		},
	},

	// ==================== 套餐与共享类 ====================
	EventPackageShared: {
		Title: "🎁 收到套餐共享",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "ownerUsername") + "」将套餐「" + notifierValue(data, "packageName") + "」共享给您，您现在可以使用该套餐创建实例"
		},
	},
	EventPackageShareRevoked: {
		Title: "🚫 套餐共享已撤销",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "ownerUsername") + "」已撤销套餐「" + notifierValue(data, "packageName") + "」的共享"
		},
	},

	// ==================== 实例转移类 ====================
	EventTransferReceived: {
		Title: "📨 收到实例转移请求",
		Message: func(data EventData) string {
			msg := "用户「" + notifierValue(data, "senderUsername") + "」希望将实例「" + notifierValue(data, "instanceName") + "」转移给您"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			msg += "\n请前往转移管理页面处理"
			return msg
		},
	},
	EventTransferAccepted: {
		Title: "✅ 实例转移已接受",
		Message: func(data EventData) string {
			msg := "用户「" + notifierValue(data, "receiverUsername") + "」已接受实例「" + notifierValue(data, "instanceName") + "」的转移请求"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventTransferOwnershipReceived: {
		Title: "✅ 实例过户完成",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已过户至您的账户"
			msg += "\n自动续费已关闭，请根据需要自行重新开启。"
			msg += "\n原系统未重装，请立即重置 SSH 密钥、登录密码等实例凭证。"
			return msg
		},
	},
	EventTransferRejected: {
		Title: "❌ 实例转移被拒绝",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "receiverUsername") + "」拒绝了实例「" + notifierValue(data, "instanceName") + "」的转移请求"
		},
	},
	EventTransferCancelled: {
		Title: "🚫 实例转移已取消",
		Message: func(data EventData) string {
			return "用户「" + notifierValue(data, "senderUsername") + "」取消了实例「" + notifierValue(data, "instanceName") + "」的转移请求"
		},
	},

	// ==================== 安全与账户类 ====================
	EventLoginNewDevice: {
		Title: "🔐 新设备登录提醒",
		Message: func(data EventData) string {
			msg := "您的账户在新设备上登录"
			if ifTruthy(data, "deviceInfo") {
				msg += "\n设备: " + notifierValue(data, "deviceInfo")
			}
			if ifTruthy(data, "ipAddress") {
				msg += "\nIP: " + notifierValue(data, "ipAddress")
			}
			if ifTruthy(data, "loginTime") {
				msg += "\n时间: " + notifierValue(data, "loginTime")
			}
			msg += "\n如非本人操作，请立即修改密码"
			return msg
		},
	},
	EventPasswordChanged: {
		Title: "🔑 密码已修改",
		Message: func(data EventData) string {
			msg := "您的账户密码已成功修改"
			if ifTruthy(data, "ipAddress") {
				msg += "\n操作 IP: " + notifierValue(data, "ipAddress")
			}
			msg += "\n如非本人操作，请立即联系管理员"
			return msg
		},
	},
	Event2FAEnabled: {
		Title: "🛡️ 两步验证已启用",
		Message: func(EventData) string {
			return "您的账户已成功启用两步验证，账户安全性已提升"
		},
	},
	Event2FADisabled: {
		Title: "⚠️ 两步验证已禁用",
		Message: func(EventData) string {
			return "您的账户已禁用两步验证，建议重新启用以提高账户安全性"
		},
	},

	// ==================== 配额与资源类 ====================
	EventQuotaWarning: {
		Title: "⚠️ 配额即将用尽",
		Message: func(data EventData) string {
			msg := "您的" + notifierValue(data, "quotaType") + "配额即将用尽"
			msg += "\n使用: " + notifierValue(data, "currentUsed") + " / " + notifierValue(data, "maxLimit")
			if ifTruthy(data, "usagePercent") {
				msg += " (" + notifierValue(data, "usagePercent") + "%)"
			}
			msg += "\n请前往个人设置添加配额"
			return msg
		},
	},

	// ==================== 工单类 ====================
	EventTicketCreated: {
		Title: "📩 收到新工单",
		Message: func(data EventData) string {
			msg := "用户「" + notifierValue(data, "username") + "」在节点「" + notifierValue(data, "hostName") + "」提交了新工单"
			msg += "\n主题: " + notifierValue(data, "subject")
			if ifTruthy(data, "instanceName") {
				msg += "\n相关实例: " + notifierValue(data, "instanceName")
			}
			msg += "\n请前往工单管理页面处理"
			return msg
		},
	},
	EventTicketReplied: {
		Title: "💬 工单有新回复",
		Message: func(data EventData) string {
			msg := "工单「" + notifierValue(data, "subject") + "」有新回复"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "replyFrom") {
				msg += "\n来自: " + notifierValue(data, "replyFrom")
			}
			msg += "\n请前往工单页面查看详情"
			return msg
		},
	},
	EventTicketStatusChanged: {
		Title: "📋 工单状态已更新",
		Message: func(data EventData) string {
			statusMap := map[string]string{
				"open":        "待处理",
				"in_progress": "处理中",
				"resolved":    "已解决",
				"closed":      "已关闭",
			}
			lookupKey := ""
			if ifTruthy(data, "newStatus") {
				lookupKey = notifierValue(data, "newStatus")
			}
			// `statusMap[...] || data.newStatus`: the raw value is the fallback, so a
			// missing status interpolates as "undefined" exactly like the original.
			statusText := notifierValue(data, "newStatus")
			if mapped, ok := statusMap[lookupKey]; ok && mapped != "" {
				statusText = mapped
			}
			msg := "工单「" + notifierValue(data, "subject") + "」状态已更新为「" + statusText + "」"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventTicketClosed: {
		Title: "🔒 工单已关闭",
		Message: func(data EventData) string {
			msg := "工单「" + notifierValue(data, "subject") + "」已被关闭"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			if ifTruthy(data, "closedBy") {
				msg += "\n关闭者: " + notifierValue(data, "closedBy")
			}
			return msg
		},
	},
	EventTicketAutoClosed: {
		Title: "🔒 工单已自动关闭",
		Message: func(data EventData) string {
			msg := "工单「" + notifierValue(data, "subject") + "」已因距最后回复超过24小时而自动关闭"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			msg += "\n如有新问题，请创建新工单"
			return msg
		},
	},
	EventRedeemCodeUsed: {
		Title: "🎁 您的兑换码已被使用",
		Message: func(data EventData) string {
			return "用户 " + notifierValue(data, "username") + " 使用了您的兑换码\n兑换码: " +
				notifierValue(data, "redeemCode") + "\n资源类型: " +
				notifierValue(data, "codeType") + " +" +
				notifierValue(data, "codeValue") + notifierValue(data, "unit")
		},
	},

	// ==================== 计费相关 ====================
	EventInstanceRenewed: {
		Title: "✅ 实例续费成功",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」续费成功"
			if ifTruthy(data, "renewMonths") {
				msg += "\n续费时长: " + notifierValue(data, "renewMonths") + " 个月"
			}
			if hasKey(data, "renewAmount") {
				msg += "\n扣费金额: ¥" + money(data["renewAmount"])
			}
			if ifTruthy(data, "newExpiresAt") {
				msg += "\n新到期时间: " + notifierValue(data, "newExpiresAt")
			}
			return msg
		},
	},
	EventInstancePlanChanged: {
		Title: "🔄 实例套餐已变更",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」套餐已变更"
			if ifTruthy(data, "oldPlanName") && ifTruthy(data, "newPlanName") {
				msg += "\n原套餐: " + notifierValue(data, "oldPlanName")
				msg += "\n新套餐: " + notifierValue(data, "newPlanName")
			}
			if hasKey(data, "priceDiff") {
				priceDiff := notifierNumber(data["priceDiff"])
				var diffText string
				if priceDiff > 0 {
					diffText = "补差价 ¥" + toFixedJS(priceDiff, 2)
				} else {
					diffText = "退还 ¥" + toFixedJS(absFloat(priceDiff), 2)
				}
				msg += "\n费用: " + diffText
			}
			return msg
		},
	},
	EventInstancePriceChanged: {
		Title: "💰 实例价格已调整",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」价格已调整"
			if hasKey(data, "oldPrice") && hasKey(data, "newPrice") {
				msg += "\n原价格: ¥" + money(data["oldPrice"])
				msg += "\n新价格: ¥" + money(data["newPrice"])
			}
			if hasKey(data, "priceDiff") && !notifierIsStrictZero(data["priceDiff"], true) {
				var diffText string
				if ifTruthy(data, "isIncrease") {
					diffText = "已扣除 ¥" + money(data["priceDiff"])
				} else {
					diffText = "已退还 ¥" + money(data["priceDiff"])
				}
				msg += "\n" + diffText
			}
			return msg
		},
	},
	EventInstanceExpiring: {
		Title: "⚠️ 实例即将到期",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」即将到期"
			if hasKey(data, "daysRemaining") {
				msg += "\n剩余时间: " + notifierValue(data, "daysRemaining") + " 天"
			}
			if ifTruthy(data, "newExpiresAt") {
				msg += "\n到期时间: " + notifierValue(data, "newExpiresAt")
			}
			msg += "\n请及时续费以避免实例被封停或删除"
			return msg
		},
	},
	EventAutoRenewFailed: {
		Title: "❌ 自动续费失败",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」自动续费失败"
			if ifTruthy(data, "failReason") {
				msg += "\n原因: " + notifierValue(data, "failReason")
			}
			if hasKey(data, "renewAmount") {
				msg += "\n所需金额: ¥" + money(data["renewAmount"])
			}
			msg += "\n请确保余额充足或手动续费"
			return msg
		},
	},
	EventInstanceExtended: {
		Title: "📅 实例已延期",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」已延期"
			if ifTruthy(data, "days") {
				msg += "\n延期时长: " + notifierValue(data, "days") + " 天"
			}
			if hasKey(data, "amount") {
				msg += "\n费用: ¥" + money(data["amount"])
			}
			if ifTruthy(data, "newExpiresAt") {
				msg += "\n新到期时间: " + notifierValue(data, "newExpiresAt")
			}
			return msg
		},
	},
	EventInstancesBatchExtended: {
		Title: "🎁 管理员已为您的实例赠送时长",
		Message: func(data EventData) string {
			msg := "您在节点「" + notifierValue(data, "hostName") + "」的付费实例已免费延期"
			if ifTruthy(data, "days") {
				msg += "\n延期时长: " + notifierValue(data, "days") + " 天"
			}
			msg += "\n请前往面板查看最新到期时间"
			return msg
		},
	},
	EventRefundCompleted: {
		Title: "💰 退款已完成",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的退款已完成"
			if hasKey(data, "amount") {
				msg += "\n退款金额: ¥" + money(data["amount"])
			}
			if ifTruthy(data, "reason") {
				msg += "\n原因: " + notifierValue(data, "reason")
			}
			msg += "\n退款已到账至您的余额"
			return msg
		},
	},
	EventInstanceDeletedRefunded: {
		Title: "🗑️ 实例已删除",
		Message: func(data EventData) string {
			// 区分用户销毁和管理员删除
			isUserDestroy := hasKey(data, "isFreeInstance") || hasKey(data, "feeAmount")
			msg := "您的实例「" + notifierValue(data, "instanceName") + "」已被管理员删除"
			if isUserDestroy {
				msg = "您的实例「" + notifierValue(data, "instanceName") + "」已成功销毁"
			}
			if ifTruthy(data, "reason") {
				msg += "\n删除原因: " + notifierValue(data, "reason")
			}
			refundAmount, hasRefund := notifierGet(data, "refundAmount")
			if hasRefund && notifierNumber(refundAmount) > 0 {
				msg += "\n退款金额: ¥" + money(refundAmount)
				if fee, hasFee := notifierGet(data, "feeAmount"); hasFee && notifierNumber(fee) > 0 {
					msg += "\n手续费: ¥" + money(fee)
				}
				if ifTruthy(data, "refundType") {
					msg += "\n退款方式: " + notifierValue(data, "refundType")
				}
				msg += "\n退款已到账至您的余额"
			} else if ifTruthy(data, "isFreeInstance") {
				msg += "\n免费实例无退款"
			}
			return msg
		},
	},
	EventAffApplied: {
		Title: "🎁 已应用续费折扣",
		Message: func(data EventData) string {
			msg := "您的实例「" + notifierValue(data, "instanceName") + "」已应用推荐计划优惠"
			if ifTruthy(data, "discountPercent") {
				msg += "\n折扣优惠: " + notifierValue(data, "discountPercent") + "% 续费折扣"
			}
			msg += "\n后续续费时将自动享受折扣价格"
			return msg
		},
	},
	EventInstanceRenewalPriceUpdated: {
		Title: "💰 实例续费价格已调整",
		Message: func(data EventData) string {
			msg := "实例「" + notifierValue(data, "instanceName") + "」的续费价格已调整"
			if hasKey(data, "oldPrice") && hasKey(data, "newPrice") {
				msg += "\n原价格: ¥" + money(data["oldPrice"]) + "/月"
				msg += "\n新价格: ¥" + money(data["newPrice"]) + "/月"
			}
			msg += "\n\n该价格将于下次续费时生效，本月不受影响。"
			if ifTruthy(data, "hostName") {
				msg += "\n节点: " + notifierValue(data, "hostName")
			}
			return msg
		},
	},
	EventPublicAPINotification: {
		Title: "📣 扩展通知",
		Message: func(data EventData) string {
			source := ""
			if ifTruthy(data, "publicApiSource") {
				source = "\n来源: " + notifierValue(data, "publicApiSource")
			}
			title := "来自扩展的通知"
			if ifTruthy(data, "publicApiTitle") {
				title = notifierValue(data, "publicApiTitle")
			}
			body := ""
			if ifTruthy(data, "publicApiMessage") {
				body = notifierValue(data, "publicApiMessage")
			}
			return title + "\n\n" + body + source
		},
	},
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// ==================== 重试 ====================

// IsNonRetryableNotificationError mirrors isNonRetryableError.
func IsNonRetryableNotificationError(errorMessage string) bool {
	if errorMessage == "" {
		return false
	}
	nonRetryablePatterns := []string{
		"Missing botToken",
		"Missing chatId",
		"Missing webhookUrl",
		"Missing webhook URL",
		"Invalid Discord Webhook URL",
		"not yet implemented",
		"Unknown channel type",
		"Unsupported channel type",
		"chat not found",
		"bot was blocked",
		"CHAT_ID_INVALID",
		"BOT_TOKEN_INVALID",
		"401",
		"403",
		"404",
	}
	lowerError := strings.ToLower(errorMessage)
	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(lowerError, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// sendWithRetry mirrors sendWithRetry: up to 3 attempts with an increasing delay,
// skipping retries for configuration-class errors.
func sendWithRetry(sendFn func() NotificationResult, channelType string) NotificationResult {
	lastError := ""

	for attempt := 1; attempt <= notifierMaxRetries; attempt++ {
		result := sendFn()
		if result.Success {
			return result
		}
		if IsNonRetryableNotificationError(result.Error) {
			return result
		}

		lastError = result.Error
		log.Printf("[Notifier] %s attempt %d/%d failed: %s", channelType, attempt, notifierMaxRetries, result.Error)

		if attempt < notifierMaxRetries {
			notifierDelay(notifierRetryDelay * time.Duration(attempt))
		}
	}

	return NotificationResult{Success: false, Error: fmt.Sprintf("Failed after %d attempts: %s", notifierMaxRetries, lastError)}
}

// readSafeNotificationError mirrors readSafeNotificationError: a sanitized,
// length-capped preview of the upstream body, falling back to the status code.
func readSafeNotificationError(response *http.Response) string {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	sanitized := SanitizeTokensInString(string(body))
	if runes := []rune(sanitized); len(runes) > notifierErrorPreview {
		sanitized = string(runes[:notifierErrorPreview])
	}
	if sanitized != "" {
		return sanitized
	}
	return fmt.Sprintf("HTTP %d", response.StatusCode)
}

// ==================== 出站请求 ====================

// notifierPost validates the target with the lib SSRF guard and issues the request
// through the shared revalidating transport without following redirects. It is the
// Go equivalent of `safeFetch(url, { redirect: 'manual' })`.
func notifierPost(ctx context.Context, rawURL, label string, body []byte, headers map[string]string) (*http.Response, error) {
	if _, err := AssertSafeHTTPURL(ctx, rawURL, label); err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	return notifierHTTPClient.Do(request)
}

// ==================== 发送 ====================

// SendNotification mirrors sendNotification: it always writes the inbox message,
// then fans out to every enabled external channel.
func SendNotification(ctx context.Context, userID int64, eventType EventType, data EventData) SendResult {
	template, ok := EventTemplates[eventType]
	if !ok {
		log.Printf("[Notifier] Unknown event type: %s", eventType)
		return SendResult{}
	}

	title := template.Title
	message := template.Message(data)

	// 1. 创建站内信（始终创建）
	// `data` is always an object in the TypeScript signatures, so a nil map here
	// is stored as an empty JSON object rather than SQL NULL.
	inboxData := map[string]any{}
	if data != nil {
		inboxData = map[string]any(data)
	}
	if _, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    int32(userID),
		EventType: string(eventType),
		Title:     title,
		Content:   message,
		Data:      inboxData,
	}); err != nil {
		log.Printf("[Notifier] Failed to create inbox message: %v", err)
	}

	// 2. 发送外部通知
	channels, err := db.GetEnabledChannelsByUserId(ctx, int32(userID))
	if err != nil {
		log.Printf("[Notifier] Failed to load notification channels: %v", err)
		return SendResult{}
	}
	if len(channels) == 0 {
		return SendResult{}
	}

	sent := 0
	failed := 0
	for _, channel := range channels {
		config, configOK := parseChannelConfig(channel.Config)
		if !configOK {
			// TypeScript: JSON.parse throws inside the per-channel try/catch.
			log.Printf("[Notifier] Failed to send notification to channel %d: invalid config JSON", channel.ID)
			failed++
			continue
		}

		logID, err := db.CreateNotificationLog(ctx, db.CreateNotificationLogData{
			ChannelID: channel.ID,
			EventType: string(eventType),
			Message:   message,
			Status:    models.NotificationLogStatusPending,
		})
		if err != nil {
			log.Printf("[Notifier] Failed to send notification to channel %d: %v", channel.ID, err)
			failed++
			continue
		}

		var result NotificationResult
		switch channel.Type {
		case "telegram":
			result = sendWithRetry(func() NotificationResult {
				return sendTelegram(ctx, telegramConfigFrom(config), title, message)
			}, "Telegram")
		case "discord":
			result = sendWithRetry(func() NotificationResult {
				return sendDiscord(ctx, discordConfigFrom(config), title, message)
			}, "Discord")
		case "webhook":
			result = sendWithRetry(func() NotificationResult {
				return sendWebhook(ctx, webhookConfigFrom(config), string(eventType), title, message, data)
			}, "Webhook")
		case "email":
			result = NotificationResult{Success: false, Error: "Email notifications not yet implemented"}
		default:
			result = NotificationResult{Success: false, Error: "Unknown channel type: " + string(channel.Type)}
		}

		status := models.NotificationLogStatusFailed
		if result.Success {
			status = models.NotificationLogStatusSent
		}
		// A failed log-status write throws inside the TypeScript per-channel
		// try/catch, which counts the channel as failed.
		if err := db.UpdateNotificationLogStatus(ctx, logID, status, notificationErrorPointer(result)); err != nil {
			log.Printf("[Notifier] Failed to send notification to channel %d: %v", channel.ID, err)
			failed++
			continue
		}

		if result.Success {
			sent++
		} else {
			failed++
		}
	}

	return SendResult{Sent: sent, Failed: failed}
}

// notificationErrorPointer mirrors `result.error` being null on success.
func notificationErrorPointer(result NotificationResult) *string {
	if result.Success {
		return nil
	}
	message := result.Error
	return &message
}

// parseChannelConfig mirrors
// `typeof channel.config === 'string' ? JSON.parse(channel.config) : channel.config`.
//
// The second return value is false only when JSON.parse itself would throw. A
// parsed value that is not an object yields an empty map: the TypeScript
// destructuring then reads `undefined` for every key and the sender reports its
// usual "Missing botToken" style error.
func parseChannelConfig(raw string) (map[string]any, bool) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, true
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, false
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}, true
	}
	return object, true
}

func telegramConfigFrom(config map[string]any) TelegramConfig {
	botToken, _ := config["botToken"].(string)
	return TelegramConfig{BotToken: botToken, ChatID: config["chatId"]}
}

func discordConfigFrom(config map[string]any) DiscordConfig {
	webhookURL, _ := config["webhookUrl"].(string)
	return DiscordConfig{WebhookURL: webhookURL}
}

func webhookConfigFrom(config map[string]any) WebhookConfig {
	url, _ := config["url"].(string)
	secret, _ := config["secret"].(string)
	return WebhookConfig{URL: url, Secret: secret}
}

// escapeTelegramMarkdown mirrors escapeMarkdown (MarkdownV2 special characters).
func escapeTelegramMarkdown(text string) string {
	const special = "_*[]()~`>#+-=|{}.!"
	var builder strings.Builder
	builder.Grow(len(text) + 16)
	for _, char := range text {
		if strings.ContainsRune(special, char) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// telegramSendMessage mirrors the request body of sendMessage.
func telegramSendMessage(ctx context.Context, botToken string, chatID any, text string) NotificationResult {
	payload, err := json.Marshal(struct {
		ChatID    any    `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}{ChatID: chatID, Text: text, ParseMode: "MarkdownV2"})
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}

	requestCtx, cancel := context.WithTimeout(ctx, notifierFetchTimeout)
	defer cancel()

	response, err := notifierPost(requestCtx, "https://api.telegram.org/bot"+botToken+"/sendMessage", "Telegram API URL",
		payload, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}
	defer response.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}
	if !result.OK {
		if result.Description != "" {
			return NotificationResult{Success: false, Error: result.Description}
		}
		return NotificationResult{Success: false, Error: "Unknown Telegram error"}
	}
	return NotificationResult{Success: true}
}

// sendTelegram mirrors sendTelegram.
func sendTelegram(ctx context.Context, config TelegramConfig, title, message string) NotificationResult {
	if config.BotToken == "" || !chatIDPresent(config.ChatID) {
		return NotificationResult{Success: false, Error: "Missing botToken or chatId"}
	}

	text := "*" + escapeTelegramMarkdown(title) + "*\n\n" + escapeTelegramMarkdown(message)
	return telegramSendMessage(ctx, config.BotToken, config.ChatID, text)
}

// chatIDPresent mirrors JavaScript truthiness for the chatId field.
func chatIDPresent(chatID any) bool {
	return notifierTruthy(chatID, true)
}

// discordColor mirrors the colour selection in sendDiscord.
func discordColor(title string) int {
	switch {
	case strings.Contains(title, "失败") || strings.Contains(title, "❌"):
		return 0xff0000
	case strings.Contains(title, "删除") || strings.Contains(title, "🗑️"):
		return 0x808080
	case strings.Contains(title, "恢复") || strings.Contains(title, "🔄"):
		return 0x0099ff
	case strings.Contains(title, "自动") || strings.Contains(title, "📸") || strings.Contains(title, "💾"):
		return 0x9b59b6
	default:
		return 0x00ff00
	}
}

// sendDiscord mirrors sendDiscord.
func sendDiscord(ctx context.Context, config DiscordConfig, title, message string) NotificationResult {
	if config.WebhookURL == "" {
		return NotificationResult{Success: false, Error: "Missing webhookUrl"}
	}

	parsedURL, err := AssertSafeWebhookURL(ctx, config.WebhookURL)
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}

	// A struct (not a map) so the JSON byte order matches the TypeScript object
	// literal, which keeps the wire payload byte-identical.
	payload, err := json.Marshal(struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Color       int    `json:"color"`
			Timestamp   string `json:"timestamp"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
	}{Embeds: []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       int    `json:"color"`
		Timestamp   string `json:"timestamp"`
		Footer      struct {
			Text string `json:"text"`
		} `json:"footer"`
	}{{
		Title:       title,
		Description: message,
		Color:       discordColor(title),
		Timestamp:   formatISOTimeNow(),
		Footer: struct {
			Text string `json:"text"`
		}{Text: "Incudal"},
	}}})
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}

	requestCtx, cancel := context.WithTimeout(ctx, notifierFetchTimeout)
	defer cancel()

	response, err := notifierPost(requestCtx, parsedURL.String(), "Webhook URL", payload,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return NotificationResult{Success: false, Error: readSafeNotificationError(response)}
	}
	return NotificationResult{Success: true}
}

// formatISOTimeNow mirrors new Date().toISOString().
func formatISOTimeNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// sendWebhook mirrors sendWebhook, including the optional HMAC-SHA256 signature.
func sendWebhook(ctx context.Context, config WebhookConfig, eventType, title, message string, data EventData) NotificationResult {
	if config.URL == "" {
		return NotificationResult{Success: false, Error: "Missing webhook URL"}
	}

	parsedURL, err := AssertSafeWebhookURL(ctx, config.URL)
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}

	var payloadData any
	if data != nil {
		payloadData = map[string]any(data)
	}
	// Struct field order mirrors the TypeScript literal so JSON.stringify and this
	// encoding produce the same bytes (the HMAC covers the encoded body).
	payload, err := json.Marshal(struct {
		Event     string `json:"event"`
		Title     string `json:"title"`
		Message   string `json:"message"`
		Data      any    `json:"data"`
		Timestamp string `json:"timestamp"`
	}{
		Event:     eventType,
		Title:     title,
		Message:   message,
		Data:      payloadData,
		Timestamp: formatISOTimeNow(),
	})
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}

	headers := map[string]string{"Content-Type": "application/json"}
	if config.Secret != "" {
		mac := hmac.New(sha256.New, []byte(config.Secret))
		_, _ = mac.Write(payload)
		headers["X-Signature"] = hex.EncodeToString(mac.Sum(nil))
	}

	requestCtx, cancel := context.WithTimeout(ctx, notifierFetchTimeout)
	defer cancel()

	response, err := notifierPost(requestCtx, parsedURL.String(), "Webhook URL", payload, headers)
	if err != nil {
		return NotificationResult{Success: false, Error: err.Error()}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return NotificationResult{Success: false, Error: fmt.Sprintf("HTTP %d", response.StatusCode)}
	}
	return NotificationResult{Success: true}
}

// ==================== 测试 / 验证码 / 指定渠道 ====================

// TestNotificationChannel mirrors testNotificationChannel.
func TestNotificationChannel(ctx context.Context, channel NotificationChannel) NotificationResult {
	config, ok := parseChannelConfig(channel.Config)
	if !ok {
		return NotificationResult{Success: false, Error: "Invalid channel config"}
	}

	const testMessage = "这是一条来自 Incudal 的测试通知。\n如果您收到此消息，说明通知配置正确！"
	testData := EventData{"instanceName": "test-instance", "snapshotName": "test-snapshot"}

	switch channel.Type {
	case "telegram":
		return sendWithRetry(func() NotificationResult {
			return sendTelegram(ctx, telegramConfigFrom(config), "🔔 测试通知", testMessage)
		}, "Telegram")
	case "discord":
		return sendWithRetry(func() NotificationResult {
			return sendDiscord(ctx, discordConfigFrom(config), "🔔 测试通知", testMessage)
		}, "Discord")
	case "webhook":
		return sendWithRetry(func() NotificationResult {
			return sendWebhook(ctx, webhookConfigFrom(config), "test", "🔔 测试通知", testMessage, testData)
		}, "Webhook")
	default:
		return NotificationResult{Success: false, Error: "Unsupported channel type: " + string(channel.Type)}
	}
}

// VerificationNotificationData mirrors the argument object of
// sendVerificationNotification.
type VerificationNotificationData struct {
	OperationName    string
	Code             string
	ExpiresInMinutes int
}

// SendVerificationNotification mirrors sendVerificationNotification; it only uses
// the first enabled channel.
func SendVerificationNotification(ctx context.Context, userID int64, data VerificationNotificationData) OperationVerificationSendResult {
	channels, err := db.GetEnabledChannelsByUserId(ctx, int32(userID))
	if err != nil {
		return OperationVerificationSendResult{Success: false, Error: err.Error()}
	}
	if len(channels) == 0 {
		return OperationVerificationSendResult{Success: false, Error: "No notification channel configured"}
	}

	title := "🔐 操作验证码"
	message := "您正在执行「" + data.OperationName + "」操作\n\n验证码：" + data.Code +
		"\n\n此验证码将在 " + jsNumberToString(float64(data.ExpiresInMinutes)) +
		" 分钟后失效。\n如非本人操作，请立即修改密码。"

	channel := channels[0]
	config, ok := parseChannelConfig(channel.Config)
	if !ok {
		return OperationVerificationSendResult{Success: false, Error: "Invalid channel config"}
	}

	var result NotificationResult
	switch channel.Type {
	case "telegram":
		result = sendWithRetry(func() NotificationResult {
			return sendTelegram(ctx, telegramConfigFrom(config), title, message)
		}, "Telegram")
	case "discord":
		result = sendWithRetry(func() NotificationResult {
			return sendDiscord(ctx, discordConfigFrom(config), title, message)
		}, "Discord")
	case "webhook":
		result = sendWithRetry(func() NotificationResult {
			return sendWebhook(ctx, webhookConfigFrom(config), "verification", title, message,
				EventData{"instanceName": "", "code": data.Code, "operationName": data.OperationName})
		}, "Webhook")
	default:
		return OperationVerificationSendResult{Success: false, Error: "Unsupported channel type: " + string(channel.Type)}
	}

	if !result.Success {
		errorMessage := result.Error
		if errorMessage == "" {
			errorMessage = "Failed to send notification"
		}
		return OperationVerificationSendResult{Success: false, Error: errorMessage}
	}
	return OperationVerificationSendResult{Success: true}
}

// NotificationLink mirrors the optional `{ label, url }` link argument.
type NotificationLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// SendToChannel mirrors sendToChannel: it sends a custom message to one channel
// without checking `enabled` (the resource-release channel is disabled by design).
func SendToChannel(ctx context.Context, channelID int64, title, message string, link *NotificationLink) OperationVerificationSendResult {
	const eventType = "package.quota_release"

	channel, err := db.GetNotificationChannelById(ctx, channelID)
	if err != nil {
		return OperationVerificationSendResult{Success: false, Error: err.Error()}
	}
	if channel == nil {
		return OperationVerificationSendResult{Success: false, Error: "Channel not found"}
	}

	config, ok := parseChannelConfig(channel.Config)
	if !ok {
		return OperationVerificationSendResult{Success: false, Error: "Invalid channel config"}
	}

	logID, err := db.CreateNotificationLog(ctx, db.CreateNotificationLogData{
		ChannelID: channel.ID,
		EventType: eventType,
		Message:   message,
		Status:    models.NotificationLogStatusPending,
	})
	if err != nil {
		return OperationVerificationSendResult{Success: false, Error: err.Error()}
	}

	var result NotificationResult
	switch channel.Type {
	case "telegram":
		telegramConfig := telegramConfigFrom(config)
		if telegramConfig.BotToken == "" || !chatIDPresent(telegramConfig.ChatID) {
			result = NotificationResult{Success: false, Error: "Missing botToken or chatId"}
			break
		}
		text := "*" + escapeTelegramMarkdown(title) + "*\n\n" + escapeTelegramMarkdown(message)
		if link != nil {
			// [转义后的 label](原始 url) — URL 不转义，确保链接可点击
			text += "\n[" + escapeTelegramMarkdown(link.Label) + "](" + link.URL + ")"
		}
		result = telegramSendMessage(ctx, telegramConfig.BotToken, telegramConfig.ChatID, text)
	case "discord":
		discordMessage := message
		if link != nil {
			discordMessage = message + "\n" + link.Label + ": " + link.URL
		}
		result = sendWithRetry(func() NotificationResult {
			return sendDiscord(ctx, discordConfigFrom(config), title, discordMessage)
		}, "Discord")
	case "webhook":
		webhookMessage := message
		if link != nil {
			webhookMessage = message + "\n" + link.Label + ": " + link.URL
		}
		// `{ instanceName: '', link }` with an undefined link drops the key during
		// JSON serialisation, so only set it when a link was supplied.
		webhookData := EventData{"instanceName": ""}
		if link != nil {
			webhookData["link"] = link
		}
		result = sendWithRetry(func() NotificationResult {
			return sendWebhook(ctx, webhookConfigFrom(config), eventType, title, webhookMessage, webhookData)
		}, "Webhook")
	default:
		result = NotificationResult{Success: false, Error: "Unsupported channel type: " + string(channel.Type)}
	}

	status := models.NotificationLogStatusFailed
	if result.Success {
		status = models.NotificationLogStatusSent
	}
	// A failed log-status write throws in the TypeScript original, surfacing as
	// `{ success: false, error }`.
	if err := db.UpdateNotificationLogStatus(ctx, logID, status, notificationErrorPointer(result)); err != nil {
		return OperationVerificationSendResult{Success: false, Error: err.Error()}
	}

	if !result.Success {
		errorMessage := result.Error
		if errorMessage == "" {
			errorMessage = "Failed to send notification"
		}
		return OperationVerificationSendResult{Success: false, Error: errorMessage}
	}
	return OperationVerificationSendResult{Success: true}
}

// init wires the notifier into the operation-verification seam, replacing the
// explicit "sender not configured" failure that lib/operation_verification.go
// reports while no sender is registered.
func init() {
	OperationVerificationNotificationSender = func(userID int64, data OperationVerificationNotificationData) OperationVerificationSendResult {
		return SendVerificationNotification(context.Background(), userID, VerificationNotificationData{
			OperationName:    data.OperationName,
			Code:             data.Code,
			ExpiresInMinutes: data.ExpiresInMinutes,
		})
	}
}
