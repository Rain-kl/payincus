// Unit tests for the notifier port. They live in package lib so they can reach
// the unexported helpers, and they never touch PostgreSQL or the network: the
// outbound cases assert that the SSRF guard rejects a target *before* any dial.
package lib

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventTemplatesCoverEveryEventType pins that every exported EventType has a
// template: a missing entry would silently make SendNotification a no-op, which is
// exactly the bug the TypeScript `if (!template)` warning exists to surface.
func TestEventTemplatesCoverEveryEventType(t *testing.T) {
	eventTypes := []EventType{
		EventSnapshotCreated, EventSnapshotRestored, EventSnapshotDeleted, EventSnapshotFailed,
		EventBackupCreated, EventBackupFailed, EventBackupDeleted, EventBackupRestored, EventBackupUploaded,
		EventInstanceCreated, EventInstanceCreateFailed, EventInstanceCreateTimeout, EventInstanceStarted,
		EventInstanceStopped, EventInstanceRestarted, EventInstanceRebuilt, EventInstanceCloned,
		EventInstanceHostChanged, EventInstanceTaskFailed, EventInstanceUnexpectedStop, EventInstanceDeleted,
		EventInstanceDeletedByHostOwner, EventInstanceSuspended, EventInstanceUnsuspended,
		EventInstanceRenewed, EventInstancePlanChanged, EventInstancePriceChanged, EventInstanceExpiring,
		EventInstanceExtended, EventInstancesBatchExtended, EventAutoRenewFailed, EventRefundCompleted,
		EventInstanceDeletedRefunded, EventAutoSnapshot, EventAutoSnapshotRotated, EventAutoBackup,
		EventAutoBackupRotated, EventFriendRequest, EventFriendAccepted, EventFriendRejected, EventFriendRemoved,
		EventPackageShared, EventPackageShareRevoked, EventTransferReceived, EventTransferAccepted,
		EventTransferOwnershipReceived, EventTransferRejected, EventTransferCancelled, EventLoginNewDevice,
		EventPasswordChanged, Event2FAEnabled, Event2FADisabled, EventQuotaWarning, EventTicketCreated,
		EventTicketReplied, EventTicketStatusChanged, EventTicketClosed, EventTicketAutoClosed,
		EventRedeemCodeUsed, EventAffApplied, EventInstanceRenewalPriceUpdated, EventPublicAPINotification,
	}

	if len(EventTemplates) != len(eventTypes) {
		t.Fatalf("EventTemplates has %d entries, want %d", len(EventTemplates), len(eventTypes))
	}
	for _, eventType := range eventTypes {
		template, ok := EventTemplates[eventType]
		if !ok {
			t.Fatalf("missing template for %q", eventType)
		}
		if template.Title == "" {
			t.Fatalf("empty title for %q", eventType)
		}
		if template.Message == nil {
			t.Fatalf("nil message builder for %q", eventType)
		}
		if message := template.Message(EventData{}); message == "" {
			t.Fatalf("empty message for %q with empty data", eventType)
		}
	}
}

// TestFormatNotificationSizes pins the local formatMemory/formatDisk helpers,
// including the falsy (0 / missing) short-circuit to "-".
func TestFormatNotificationSizes(t *testing.T) {
	cases := []struct {
		name   string
		data   EventData
		key    string
		memory string
		disk   string
	}{
		{"missing renders dash", EventData{}, "memory", "-", "-"},
		{"explicit zero renders dash", EventData{"memory": float64(0)}, "memory", "-", "-"},
		{"null renders dash", EventData{"memory": nil}, "memory", "-", "-"},
		{"below 1 GiB stays in MB", EventData{"memory": float64(512)}, "memory", "512 MB", "512 MB"},
		{"half GiB rounds one decimal", EventData{"memory": float64(1536)}, "memory", "1.5 GB", "2 GB"},
		{"exactly 1 GiB", EventData{"memory": float64(1024)}, "memory", "1.0 GB", "1 GB"},
		{"string number is coerced", EventData{"memory": "2048"}, "memory", "2.0 GB", "2 GB"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatNotificationMemory(tc.data, tc.key); got != tc.memory {
				t.Fatalf("formatNotificationMemory = %q, want %q", got, tc.memory)
			}
			if got := formatNotificationDisk(tc.data, tc.key); got != tc.disk {
				t.Fatalf("formatNotificationDisk = %q, want %q", got, tc.disk)
			}
		})
	}
}

// TestEventTemplateMessageBranches covers the template branches that depend on
// truthiness, optional money fields and the network-mode mapping.
func TestEventTemplateMessageBranches(t *testing.T) {
	cases := []struct {
		name  string
		event EventType
		data  EventData
		want  string
	}{
		{
			name:  "instance_created renders resources and NAT label",
			event: EventInstanceCreated,
			data: EventData{
				"instanceName": "web-1", "hostName": "hk-01", "hostLocation": "Hong Kong",
				"image": "debian-12", "cpu": float64(200), "memory": float64(2048), "disk": float64(20480),
				"networkMode": "nat_ipv6", "ipv4": "1.2.3.4", "ipv6": "2001:db8::1",
			},
			want: "您的实例「web-1」已创建成功！\n节点: hk-01 (Hong Kong)\n镜像: debian-12\n配置: CPU 200% | 内存 2.0 GB | 磁盘 20 GB\n网络: NAT + IPv6\nIPv4: 1.2.3.4\nIPv6: 2001:db8::1",
		},
		{
			name:  "instance_created omits falsy resources",
			event: EventInstanceCreated,
			data:  EventData{"instanceName": "web-1", "cpu": float64(0), "networkMode": "nat"},
			want:  "您的实例「web-1」已创建成功！\n网络: NAT",
		},
		{
			name:  "instance_host_changed falls back to dash",
			event: EventInstanceHostChanged,
			data:  EventData{"instanceName": "web-1", "newHostName": "new-01"},
			want:  "实例「web-1」已成功迁移到新节点\n节点: - -> new-01",
		},
		{
			name:  "instance_unexpected_stop uppercases the host name",
			event: EventInstanceUnexpectedStop,
			data:  EventData{"instanceName": "web-1", "hostName": "hk-01"},
			want: "实例「web-1」已异常停止运行\n可能原因: 节点资源不足 / 实例崩溃 / 管理员操作" +
				"\n节点: HK-01\n请前往面板检查实例状态",
		},
		{
			name:  "renewed prints a two-decimal amount",
			event: EventInstanceRenewed,
			data:  EventData{"instanceName": "web-1", "renewMonths": float64(3), "renewAmount": 12.5},
			want:  "实例「web-1」续费成功\n续费时长: 3 个月\n扣费金额: ¥12.50",
		},
		{
			name:  "renewed omits an absent amount",
			event: EventInstanceRenewed,
			data:  EventData{"instanceName": "web-1"},
			want:  "实例「web-1」续费成功",
		},
		{
			name:  "plan_changed charges the difference on upgrade",
			event: EventInstancePlanChanged,
			data:  EventData{"instanceName": "web-1", "oldPlanName": "Pro", "newPlanName": "Max", "priceDiff": 5.126},
			want:  "实例「web-1」套餐已变更\n原套餐: Pro\n新套餐: Max\n费用: 补差价 ¥5.13",
		},
		{
			name:  "plan_changed refunds the absolute difference",
			event: EventInstancePlanChanged,
			data:  EventData{"instanceName": "web-1", "priceDiff": -3.456},
			want:  "实例「web-1」套餐已变更\n费用: 退还 ¥3.46",
		},
		{
			name:  "deleted_refunded treats isFreeInstance as a user destroy",
			event: EventInstanceDeletedRefunded,
			data: EventData{
				"instanceName": "web-1", "reason": "用户申请", "refundAmount": 10.0,
				"feeAmount": 0.5, "refundType": "余额", "isFreeInstance": true,
			},
			want: "您的实例「web-1」已成功销毁\n删除原因: 用户申请\n退款金额: ¥10.00" +
				"\n手续费: ¥0.50\n退款方式: 余额\n退款已到账至您的余额",
		},
		{
			name:  "deleted_refunded without refund and without free flag is an admin delete",
			event: EventInstanceDeletedRefunded,
			data:  EventData{"instanceName": "web-1"},
			want:  "您的实例「web-1」已被管理员删除",
		},
		{
			name:  "ticket_status_changed maps known statuses",
			event: EventTicketStatusChanged,
			data:  EventData{"subject": "无法登录", "newStatus": "in_progress", "hostName": "hk-01"},
			want:  "工单「无法登录」状态已更新为「处理中」\n节点: hk-01",
		},
		{
			name:  "ticket_status_changed keeps unknown statuses verbatim",
			event: EventTicketStatusChanged,
			data:  EventData{"subject": "无法登录", "newStatus": "waiting"},
			want:  "工单「无法登录」状态已更新为「waiting」",
		},
		{
			name:  "public_api_notification defaults title and message",
			event: EventPublicAPINotification,
			data:  EventData{},
			want:  "来自扩展的通知\n\n",
		},
		{
			name:  "public_api_notification renders the source line",
			event: EventPublicAPINotification,
			data:  EventData{"publicApiTitle": "维护", "publicApiMessage": "今晚 22 点", "publicApiSource": "plugin-a"},
			want:  "维护\n\n今晚 22 点\n来源: plugin-a",
		},
		{
			name:  "quota_warning skips a zero percentage",
			event: EventQuotaWarning,
			data:  EventData{"quotaType": "CPU", "currentUsed": float64(8), "maxLimit": float64(10)},
			want:  "您的CPU配额即将用尽\n使用: 8 / 10\n请前往个人设置添加配额",
		},
		{
			name:  "redeem_code_used keeps the raw code value",
			event: EventRedeemCodeUsed,
			data:  EventData{"username": "bob", "redeemCode": "CODE-1", "codeType": "CPU", "codeValue": float64(2), "unit": "%"},
			want:  "用户 bob 使用了您的兑换码\n兑换码: CODE-1\n资源类型: CPU +2%",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			template, ok := EventTemplates[tc.event]
			if !ok {
				t.Fatalf("no template for %q", tc.event)
			}
			if got := template.Message(tc.data); got != tc.want {
				t.Fatalf("message =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestEscapeTelegramMarkdown pins the MarkdownV2 escaping set (the TypeScript
// regexp is /([_*\[\]()~`>#+\-=|{}.!])/g).
func TestEscapeTelegramMarkdown(t *testing.T) {
	cases := []struct{ input, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{"a_b", `a\_b`},
		{"v1.2.3", `v1\.2\.3`},
		{"a-b+c=d|e", `a\-b\+c\=d\|e`},
		{"[x](y)", `\[x\]\(y\)`},
		{"`code`", "\\`code\\`"},
		{"emoji ✅ ok", "emoji ✅ ok"},
	}
	for _, tc := range cases {
		if got := escapeTelegramMarkdown(tc.input); got != tc.want {
			t.Errorf("escapeTelegramMarkdown(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestReadSafeNotificationErrorSanitizesToken pins the sanitizer requirement from
// test-notification-fetch-guards: upstream bodies that echo credentials must be
// redacted, and long previews are capped.
func TestReadSafeNotificationErrorSanitizesToken(t *testing.T) {
	response := &http.Response{
		StatusCode: 500,
		Body: io.NopCloser(strings.NewReader(
			`{"error":"bad token Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"}`)),
	}
	got := readSafeNotificationError(response)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("JWT leaked through the error preview: %q", got)
	}
	if !strings.Contains(got, RedactedJWT) {
		t.Fatalf("expected the JWT redaction marker, got %q", got)
	}

	response = &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(""))}
	if got := readSafeNotificationError(response); got != "HTTP 502" {
		t.Fatalf("empty body = %q, want \"HTTP 502\"", got)
	}

	long := strings.Repeat("x", notifierErrorPreview+100)
	response = &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(long))}
	if got := readSafeNotificationError(response); len([]rune(got)) != notifierErrorPreview {
		t.Fatalf("preview length = %d, want %d", len([]rune(got)), notifierErrorPreview)
	}
}

// TestNotifierPostRejectsUnsafeTargets pins the SSRF guard: private, loopback and
// non-http targets must be rejected before any connection attempt.
func TestNotifierPostRejectsUnsafeTargets(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"loopback ipv4", "http://127.0.0.1:9/hook"},
		{"private 10/8", "https://10.1.2.3/hook"},
		{"private 192.168", "https://192.168.1.1/hook"},
		{"link local metadata", "http://169.254.169.254/latest/meta-data"},
		{"localhost name", "https://localhost/hook"},
		{"dot local", "https://printer.local/hook"},
		{"ipv6 loopback", "http://[::1]:9/hook"},
		{"ipv6 ula", "https://[fd00::1]/hook"},
		{"ftp scheme", "ftp://example.com/hook"},
		{"empty", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, err := notifierPost(ctx, tc.url, "Webhook URL", []byte(`{}`), nil)
			if err == nil {
				t.Fatalf("expected %q to be rejected", tc.url)
			}
			var validationError *OutboundTargetValidationError
			if !errors.As(err, &validationError) {
				t.Fatalf("error = %T %v, want *OutboundTargetValidationError", err, err)
			}
		})
	}
}

// TestSendDiscordRejectsPrivateWebhook pins that the Discord sender validates the
// webhook URL before sending (the guard asserts `await assertSafeWebhookUrl(...)`).
func TestSendDiscordRejectsPrivateWebhook(t *testing.T) {
	result := sendDiscord(context.Background(), DiscordConfig{WebhookURL: "http://127.0.0.1:9/hook"}, "t", "m")
	if result.Success {
		t.Fatal("sendDiscord must not report success for a private target")
	}
	if !strings.Contains(strings.ToLower(result.Error), "private") {
		t.Fatalf("error = %q, want a private-target rejection", result.Error)
	}
}

// TestSendWebhookRejectsPrivateURL covers the generic webhook sender, including the
// missing-URL fast path.
func TestSendWebhookRejectsPrivateURL(t *testing.T) {
	result := sendWebhook(context.Background(), WebhookConfig{URL: "http://127.0.0.1:9/hook"}, "test", "t", "m", EventData{})
	if result.Success || !strings.Contains(strings.ToLower(result.Error), "private") {
		t.Fatalf("result = %#v, want a private-target rejection", result)
	}

	result = sendWebhook(context.Background(), WebhookConfig{}, "test", "t", "m", EventData{})
	if result.Error != "Missing webhook URL" {
		t.Fatalf("missing url error = %q", result.Error)
	}
}

// TestSendTelegramMissingConfig pins the non-retryable configuration errors.
func TestSendTelegramMissingConfig(t *testing.T) {
	if result := sendTelegram(context.Background(), TelegramConfig{ChatID: "@chan"}, "t", "m"); result.Error != "Missing botToken or chatId" {
		t.Fatalf("missing botToken error = %q", result.Error)
	}
	if result := sendTelegram(context.Background(), TelegramConfig{BotToken: "123:abc"}, "t", "m"); result.Error != "Missing botToken or chatId" {
		t.Fatalf("missing chatId error = %q", result.Error)
	}
	if result := sendDiscord(context.Background(), DiscordConfig{}, "t", "m"); result.Error != "Missing webhookUrl" {
		t.Fatalf("missing webhookUrl error = %q", result.Error)
	}
}

// TestIsNonRetryableNotificationError pins the pattern list (case-insensitive
// substring match, including the bare "401"/"403"/"404" statuses).
func TestIsNonRetryableNotificationError(t *testing.T) {
	retryable := []string{"", "connection reset", "timeout", "500", "Bad Gateway"}
	for _, message := range retryable {
		if IsNonRetryableNotificationError(message) {
			t.Errorf("%q must be retryable", message)
		}
	}

	nonRetryable := []string{
		"Missing botToken or chatId", "Missing webhookUrl", "Missing webhook URL",
		"Invalid Discord Webhook URL", "Email notifications not yet implemented",
		"Unknown channel type: fax", "Unsupported channel type: fax",
		"Bad Request: chat not found", "Forbidden: bot was blocked by the user",
		"CHAT_ID_INVALID", "BOT_TOKEN_INVALID", "HTTP 401", "HTTP 403", "HTTP 404",
		"MISSING BOTTOKEN",
	}
	for _, message := range nonRetryable {
		if !IsNonRetryableNotificationError(message) {
			t.Errorf("%q must not be retried", message)
		}
	}
}

// TestSendWithRetry pins the retry loop: non-retryable errors stop after one
// attempt, retryable ones run three times and surface the wrapped message.
func TestSendWithRetry(t *testing.T) {
	original := notifierDelay
	notifierDelay = func(time.Duration) {}
	defer func() { notifierDelay = original }()

	attempts := 0
	result := sendWithRetry(func() NotificationResult {
		attempts++
		return NotificationResult{Success: false, Error: "Missing chatId"}
	}, "Telegram")
	if attempts != 1 {
		t.Fatalf("non-retryable attempts = %d, want 1", attempts)
	}
	if result.Error != "Missing chatId" {
		t.Fatalf("non-retryable error = %q", result.Error)
	}

	attempts = 0
	result = sendWithRetry(func() NotificationResult {
		attempts++
		return NotificationResult{Success: false, Error: "connection reset"}
	}, "Discord")
	if attempts != notifierMaxRetries {
		t.Fatalf("retryable attempts = %d, want %d", attempts, notifierMaxRetries)
	}
	want := "Failed after 3 attempts: connection reset"
	if result.Error != want {
		t.Fatalf("retryable error = %q, want %q", result.Error, want)
	}

	attempts = 0
	result = sendWithRetry(func() NotificationResult {
		attempts++
		return NotificationResult{Success: true}
	}, "Webhook")
	if attempts != 1 || !result.Success || result.Error != "" {
		t.Fatalf("success path = %#v after %d attempts", result, attempts)
	}

	// The second attempt can succeed.
	attempts = 0
	result = sendWithRetry(func() NotificationResult {
		attempts++
		return NotificationResult{Success: attempts == 2}
	}, "Webhook")
	if attempts != 2 || !result.Success {
		t.Fatalf("recovered on attempt %d, result %#v", attempts, result)
	}
}

// TestSendNotificationUnknownEventSkipsWork pins that an unknown event type is a
// no-op: it returns {0,0} before touching the database.
func TestSendNotificationUnknownEventSkipsWork(t *testing.T) {
	if got := SendNotification(context.Background(), 1, EventType("nope"), EventData{}); got.Sent != 0 || got.Failed != 0 {
		t.Fatalf("unknown event = %#v, want zeroes", got)
	}
}

// TestParseChannelConfig pins the JSON handling applied to the stored config
// string: malformed JSON is a failure, a non-object value behaves like `{}`.
func TestParseChannelConfig(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"object", `{"botToken":"1:a","chatId":"-1"}`, true},
		{"empty string", ``, true},
		{"null", `null`, true},
		{"array", `[1,2]`, true},
		{"truncated json", `{"botToken":`, false},
	}
	for _, tc := range cases {
		_, ok := parseChannelConfig(tc.raw)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
		}
	}

	config, ok := parseChannelConfig(`[1,2]`)
	if !ok || len(config) != 0 {
		t.Fatalf("non-object config = %#v (ok=%v), want an empty map", config, ok)
	}

	config, _ = parseChannelConfig(`{"chatId":"-100"}`)
	if got := telegramConfigFrom(config); got.BotToken != "" || got.ChatID != "-100" {
		t.Fatalf("telegramConfigFrom = %#v", got)
	}
}

// TestDiscordColor pins the title-driven embed colour selection.
func TestDiscordColor(t *testing.T) {
	cases := []struct {
		title string
		want  int
	}{
		{"❌ 实例创建失败", 0xff0000},
		{"快照创建失败", 0xff0000},
		{"🗑️ 实例已删除", 0x808080},
		{"🔬 无关标题 删除", 0x808080},
		{"🔄 快照恢复成功", 0x0099ff},
		{"💾 自动备份完成", 0x9b59b6},
		{"✅ 实例续费成功", 0x00ff00},
	}
	for _, tc := range cases {
		if got := discordColor(tc.title); got != tc.want {
			t.Errorf("discordColor(%q) = %#x, want %#x", tc.title, got, tc.want)
		}
	}
}

// TestNotifierValueSemantics pins the JS template-literal coercion rules used by
// every message builder.
func TestNotifierValueSemantics(t *testing.T) {
	data := EventData{
		"str":   "x",
		"num":   float64(12.5),
		"int":   float64(3),
		"zero":  float64(0),
		"empty": "",
		"false": false,
		"null":  nil,
		"list":  []any{"a", float64(1)},
		"obj":   map[string]any{"a": float64(1)},
	}

	cases := []struct{ key, want string }{
		{"str", "x"},
		{"num", "12.5"},
		{"int", "3"},
		{"zero", "0"},
		{"empty", ""},
		{"false", "false"},
		{"null", "null"},
		{"missing", "undefined"},
		{"list", "a,1"},
		{"obj", "[object Object]"},
	}
	for _, tc := range cases {
		if got := notifierValue(data, tc.key); got != tc.want {
			t.Errorf("notifierValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}

	if !ifTruthy(data, "num") || ifTruthy(data, "zero") || ifTruthy(data, "empty") ||
		ifTruthy(data, "false") || ifTruthy(data, "null") || ifTruthy(data, "missing") {
		t.Fatal("ifTruthy does not match JavaScript truthiness")
	}
	if hasKey(data, "null") != true || hasKey(data, "missing") != false {
		t.Fatal("hasKey must distinguish an explicit null from an absent key")
	}
	if !notifierIsStrictZero(data["zero"], true) || notifierIsStrictZero(data["null"], true) {
		t.Fatal("notifierIsStrictZero must only match numeric zero")
	}
}

// TestNotificationErrorPointer pins that success stores SQL NULL for the error.
func TestNotificationErrorPointer(t *testing.T) {
	if pointer := notificationErrorPointer(NotificationResult{Success: true}); pointer != nil {
		t.Fatalf("success error = %v, want nil", *pointer)
	}
	pointer := notificationErrorPointer(NotificationResult{Error: "boom"})
	if pointer == nil || *pointer != "boom" {
		t.Fatalf("failure error = %v", pointer)
	}
}

// TestSendVerificationNotificationSeamWiring pins that init() replaced the
// operation-verification seam (which otherwise reports SEND_FAILED).
func TestSendVerificationNotificationSeamWiring(t *testing.T) {
	if OperationVerificationNotificationSender == nil {
		t.Fatal("notifier init() must register OperationVerificationNotificationSender")
	}
}

// TestTestNotificationChannelUnsupportedType pins the test-channel switch: email
// and unknown types are rejected without attempting an outbound request.
func TestTestNotificationChannelUnsupportedType(t *testing.T) {
	cases := []struct {
		channelType string
		want        string
	}{
		{"email", "Unsupported channel type: email"},
		{"fax", "Unsupported channel type: fax"},
	}
	for _, tc := range cases {
		result := TestNotificationChannel(context.Background(), NotificationChannel{
			ID: 1, Type: tc.channelType, Config: `{"email":"a@b.c"}`,
		})
		if result.Success || result.Error != tc.want {
			t.Errorf("%s => %#v, want error %q", tc.channelType, result, tc.want)
		}
	}

	// A malformed config JSON fails before the type switch is reached.
	result := TestNotificationChannel(context.Background(), NotificationChannel{
		ID: 1, Type: "webhook", Config: `{"url":`,
	})
	if result.Success || result.Error != "Invalid channel config" {
		t.Errorf("malformed config => %#v", result)
	}
}
