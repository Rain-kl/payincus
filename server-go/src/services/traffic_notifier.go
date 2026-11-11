// Port of server/src/services/traffic-notifier.ts.
//
// The module sends the two traffic messages (80% warning, over-limit throttle) to
// the user's enabled notification channels. Its retry policy is part of the
// contract with services/traffic-scheduler.ts:
//
//   - up to 3 attempts with a linear backoff (2s, 4s);
//   - known-permanent failures (missing credentials, chat not found, 401/403/404)
//     stop immediately — retrying cannot help and would delay the whole job;
//   - every attempt writes a notification_log row, so the admin panel shows
//     sent/failed with the sanitized upstream error.
//
// The channel transports live here rather than in lib/notifier.go because the
// TypeScript module owns them; lib/notifier.go's SendToChannel hardcodes the
// `package.quota_release` log event and would mislabel traffic messages.
package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// TrafficNotifierMaxRetries is MAX_RETRIES.
	TrafficNotifierMaxRetries = 3
	// TrafficNotifierRetryDelay is RETRY_DELAY_MS; attempt N waits N * delay.
	TrafficNotifierRetryDelay = 2 * time.Second
	// TrafficNotifierFetchTimeout is the per-attempt HTTP budget.
	TrafficNotifierFetchTimeout = 15 * time.Second
	// TrafficNotifierErrorPreviewMaxChars bounds the upstream body kept in the log.
	TrafficNotifierErrorPreviewMaxChars = 2000
)

// trafficNotifierSleep is a seam: retry tests must not wait for real time.
var trafficNotifierSleep = func(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// FormatBytes mirrors `export { formatBytes }` (re-exported from
// services/traffic-utils.ts).
func FormatBytes(bytes int64) string { return lib.FormatBytes(bytes) }

// trafficNotifierResult mirrors `{ success, error }`.
type trafficNotifierResult struct {
	Success bool
	Error   string
}

// TrafficNotificationContext mirrors TrafficNotificationContext.
type TrafficNotificationContext struct {
	ThrottleSpeed   string
	NormalLineSpeed string
	ResetDay        string
}

// trafficNonRetryablePatterns mirrors isNonRetryableError's list.
var trafficNonRetryablePatterns = []string{
	"Missing botToken",
	"Missing chatId",
	"Missing webhookUrl",
	"chat not found",
	"bot was blocked",
	"CHAT_ID_INVALID",
	"BOT_TOKEN_INVALID",
	"401",
	"403",
	"404",
}

// IsNonRetryableTrafficNotificationError mirrors isNonRetryableError.
func IsNonRetryableTrafficNotificationError(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	for _, pattern := range trafficNonRetryablePatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// trafficSendWithRetry mirrors sendWithRetry.
func trafficSendWithRetry(ctx context.Context, channelType string, send func(context.Context) trafficNotifierResult) trafficNotifierResult {
	lastError := ""
	for attempt := 1; attempt <= TrafficNotifierMaxRetries; attempt++ {
		result := send(ctx)
		if result.Success {
			return result
		}
		if IsNonRetryableTrafficNotificationError(result.Error) {
			return result
		}
		lastError = result.Error
		log.Printf("[TrafficNotifier] %s attempt %d/%d failed: %s", channelType, attempt, TrafficNotifierMaxRetries, result.Error)
		if attempt < TrafficNotifierMaxRetries {
			trafficNotifierSleep(ctx, TrafficNotifierRetryDelay*time.Duration(attempt))
		}
	}
	return trafficNotifierResult{Success: false, Error: fmt.Sprintf("Failed after %d attempts: %s", TrafficNotifierMaxRetries, lastError)}
}

// trafficReadSafeNotificationError mirrors readSafeNotificationError.
func trafficReadSafeNotificationError(response *http.Response) string {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	sanitized := lib.SanitizeTokensInString(string(body))
	if len(sanitized) > TrafficNotifierErrorPreviewMaxChars {
		sanitized = sanitized[:TrafficNotifierErrorPreviewMaxChars]
	}
	if sanitized == "" {
		return fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	return sanitized
}

// ResolveTrafficNotificationContext mirrors resolveTrafficNotificationContext.
//
// It is exported because the message text is user-visible: an admin route that
// previews the notification must show the same numbers.
func ResolveTrafficNotificationContext(ctx context.Context, userID int32, instanceName, hostName string) (TrafficNotificationContext, error) {
	rows, err := db.TrafficNotificationContextRows(ctx, userID, instanceName, hostName)
	if err != nil {
		return TrafficNotificationContext{}, err
	}
	if len(rows) == 0 {
		return TrafficNotificationContext{}, fmt.Errorf("no active traffic notification context for user %d", userID)
	}

	configured, err := db.GetSystemConfig(ctx, TrafficBandwidthOverageThrottleConfigKey, true)
	if err != nil {
		return TrafficNotificationContext{}, err
	}
	throttleSpeed := TrafficBandwidthDefaultOverageThrottleSpeed
	if configured != nil {
		if normalized := TrafficBandwidthNormalizePlanTrafficLimitSpeed(configured); normalized.Value != nil {
			throttleSpeed = *normalized.Value
		}
	}

	normalSpeeds := make(map[string]struct{})
	resetDays := make(map[int]struct{})
	for _, row := range rows {
		planSpeed := TrafficBandwidthNormalizePlanTrafficLimitSpeed(row.PlanTrafficLimitSpeed)
		normalLineSpeed := planSpeed.Value
		if normalLineSpeed == nil {
			normalLineSpeed = row.PackageLimitsIngress
		}
		if normalLineSpeed == nil {
			normalLineSpeed = row.PackageLimitsEgress
		}
		if normalLineSpeed == nil || *normalLineSpeed == "" {
			normalSpeeds["不限速"] = struct{}{}
		} else {
			normalSpeeds[*normalLineSpeed] = struct{}{}
		}
		day := row.TrafficResetDay
		if day < 1 {
			day = 1
		}
		if day > 28 {
			day = 28
		}
		resetDays[day] = struct{}{}
	}

	speeds := make([]string, 0, len(normalSpeeds))
	for speed := range normalSpeeds {
		speeds = append(speeds, speed)
	}
	// The TypeScript keeps a Set's insertion order. The SQL projection is ordered by
	// instance id, so reconstructing that order would require the rows themselves;
	// sorting is a stable, deterministic substitute and the joined text is identical
	// whenever every instance shares a speed (the normal case).
	sort.Strings(speeds)

	days := make([]int, 0, len(resetDays))
	for day := range resetDays {
		days = append(days, day)
	}
	sort.Ints(days)
	dayTexts := make([]string, 0, len(days))
	for _, day := range days {
		dayTexts = append(dayTexts, fmt.Sprintf("%d", day))
	}

	return TrafficNotificationContext{
		ThrottleSpeed:   throttleSpeed,
		NormalLineSpeed: strings.Join(speeds, " / "),
		ResetDay:        strings.Join(dayTexts, "、"),
	}, nil
}

// SendTrafficWarningNotification mirrors sendTrafficWarningNotification.
//
// A user with no enabled channels counts as delivered: nothing failed, there is
// just nowhere to send. That is the TypeScript's `if (channels.length === 0)
// return true` and it keeps the warning claim (so it is not retried forever).
func SendTrafficWarningNotification(ctx context.Context, userID int32, used, limit int64, instanceName string) bool {
	if limit <= 0 {
		return false
	}
	channels, err := db.GetEnabledChannelsByUserId(ctx, userID)
	if err != nil {
		log.Printf("[TrafficNotifier] sendTrafficWarningNotification error: %v", err)
		return false
	}
	if len(channels) == 0 {
		return true
	}

	percentage := float64(used) / float64(limit) * 100
	notifyContext, err := ResolveTrafficNotificationContext(ctx, userID, instanceName, "")
	if err != nil {
		log.Printf("[TrafficNotifier] sendTrafficWarningNotification error: %v", err)
		return false
	}
	title := "⚠️ 流量预警"
	scope := "您的月度流量"
	if instanceName != "" {
		scope = fmt.Sprintf("您的实例 %s 月度流量", instanceName)
	}
	message := fmt.Sprintf("%s已使用 %.1f%%\n已用: %s\n限额: %s\n\n超出限额后，您的实例将被限速至 %s。\n流量于每月 %s 日重置后，带宽将恢复至 %s。",
		scope, percentage, FormatBytes(used), FormatBytes(limit),
		notifyContext.ThrottleSpeed, notifyContext.ResetDay, notifyContext.NormalLineSpeed)
	return trafficNotifyChannels(ctx, channels, "traffic_warning", title, message)
}

// SendTrafficThrottledNotification mirrors sendTrafficThrottledNotification.
func SendTrafficThrottledNotification(ctx context.Context, userID int32, instanceName, hostName string) {
	channels, err := db.GetEnabledChannelsByUserId(ctx, userID)
	if err != nil {
		log.Printf("[TrafficNotifier] sendTrafficThrottledNotification error: %v", err)
		return
	}
	if len(channels) == 0 {
		return
	}

	notifyContext, err := ResolveTrafficNotificationContext(ctx, userID, instanceName, hostName)
	if err != nil {
		log.Printf("[TrafficNotifier] sendTrafficThrottledNotification error: %v", err)
		return
	}
	title := "🚫 流量限速通知"
	message := fmt.Sprintf("您的实例 %s（节点：%s）已因流量超额被限速至 %s。\n\n您仍可通过 SSH 管理实例。\n流量将于每月 %s 日自动重置，届时带宽将恢复至 %s。",
		instanceName, hostName, notifyContext.ThrottleSpeed, notifyContext.ResetDay, notifyContext.NormalLineSpeed)
	trafficNotifyChannels(ctx, channels, "traffic_throttled", title, message)
}

// trafficNotifyChannels mirrors the shared loop of both senders: log → send →
// update the log status, isolating each channel's failure.
func trafficNotifyChannels(
	ctx context.Context,
	channels []db.NotificationChannelRow,
	eventType, title, message string,
) bool {
	allSent := true
	for _, channel := range channels {
		logID, err := db.CreateNotificationLog(ctx, db.CreateNotificationLogData{
			ChannelID: channel.ID,
			EventType: eventType,
			Message:   message,
			Status:    models.NotificationLogStatusPending,
		})
		if err != nil {
			allSent = false
			log.Printf("[TrafficNotifier] Failed to send warning to channel %d: %v", channel.ID, err)
			continue
		}

		var result trafficNotifierResult
		switch string(channel.Type) {
		case "telegram":
			result = trafficSendWithRetry(ctx, "Telegram", func(sendCtx context.Context) trafficNotifierResult {
				return trafficSendTelegram(sendCtx, channel.Config, title, message)
			})
		case "discord":
			result = trafficSendWithRetry(ctx, "Discord", func(sendCtx context.Context) trafficNotifierResult {
				return trafficSendDiscord(sendCtx, channel.Config, title, message)
			})
		default:
			result = trafficNotifierResult{Success: false, Error: fmt.Sprintf("Unsupported channel type: %s", channel.Type)}
		}

		status := models.NotificationLogStatusFailed
		if result.Success {
			status = models.NotificationLogStatusSent
		}
		var errorMessage *string
		if !result.Success && result.Error != "" {
			value := result.Error
			errorMessage = &value
		}
		if err := db.UpdateNotificationLogStatus(ctx, logID, status, errorMessage); err != nil {
			allSent = false
			log.Printf("[TrafficNotifier] Failed to update notification log %d: %v", logID, err)
			continue
		}
		if !result.Success {
			allSent = false
		}
	}
	return allSent
}

// trafficChannelConfig parses the JSON `config` column.
func trafficChannelConfig(raw string) (map[string]any, bool) {
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, false
	}
	return config, true
}

func trafficChannelString(config map[string]any, key string) string {
	value, ok := config[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

// trafficSendTelegram mirrors sendTelegram (MarkdownV2, manual redirects).
func trafficSendTelegram(ctx context.Context, rawConfig, title, message string) trafficNotifierResult {
	config, ok := trafficChannelConfig(rawConfig)
	if !ok {
		return trafficNotifierResult{Success: false, Error: "Invalid channel config"}
	}
	botToken := trafficChannelString(config, "botToken")
	chatID := trafficChannelString(config, "chatId")
	if botToken == "" || chatID == "" {
		return trafficNotifierResult{Success: false, Error: "Missing botToken or chatId"}
	}

	payload, err := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       "*" + escapeTrafficTelegramMarkdown(title) + "*\n\n" + escapeTrafficTelegramMarkdown(message),
		"parse_mode": "MarkdownV2",
	})
	if err != nil {
		return trafficNotifierResult{Success: false, Error: err.Error()}
	}

	requestCtx, cancel := context.WithTimeout(ctx, TrafficNotifierFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		"https://api.telegram.org/bot"+botToken+"/sendMessage", bytes.NewReader(payload))
	if err != nil {
		return trafficNotifierResult{Success: false, Error: err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: TrafficNotifierFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return trafficNotifierResult{Success: false, Error: lib.SanitizeTokensInString(err.Error())}
	}
	defer response.Body.Close()

	var decoded struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&decoded); err != nil {
		return trafficNotifierResult{Success: false, Error: lib.SanitizeTokensInString(err.Error())}
	}
	if !decoded.OK {
		description := decoded.Description
		if description == "" {
			description = "Unknown error"
		}
		return trafficNotifierResult{Success: false, Error: description}
	}
	return trafficNotifierResult{Success: true}
}

// trafficSendDiscord mirrors sendDiscord: the webhook target is validated by
// lib.SafeFetch (assertSafeWebhookUrl + revalidating transport).
func trafficSendDiscord(ctx context.Context, rawConfig, title, message string) trafficNotifierResult {
	config, ok := trafficChannelConfig(rawConfig)
	if !ok {
		return trafficNotifierResult{Success: false, Error: "Invalid channel config"}
	}
	webhookURL := trafficChannelString(config, "webhookUrl")
	if webhookURL == "" {
		return trafficNotifierResult{Success: false, Error: "Missing webhookUrl"}
	}

	color := 0xff0000
	if strings.Contains(title, "预警") {
		color = 0xffa500
	}
	payload, err := json.Marshal(map[string]any{
		"embeds": []map[string]any{{
			"title":       title,
			"description": message,
			"color":       color,
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
			"footer":      map[string]any{"text": "Incudal"},
		}},
	})
	if err != nil {
		return trafficNotifierResult{Success: false, Error: err.Error()}
	}

	requestCtx, cancel := context.WithTimeout(ctx, TrafficNotifierFetchTimeout)
	defer cancel()
	response, err := lib.SafeFetch(requestCtx, webhookURL, &lib.SafeFetchInit{
		Method:  http.MethodPost,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    bytes.NewReader(payload),
	}, "Webhook URL")
	if err != nil {
		return trafficNotifierResult{Success: false, Error: lib.SanitizeTokensInString(err.Error())}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return trafficNotifierResult{Success: false, Error: trafficReadSafeNotificationError(response)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	response.Body.Close()
	return trafficNotifierResult{Success: true}
}

// escapeTrafficTelegramMarkdown mirrors escapeMarkdown (MarkdownV2).
func escapeTrafficTelegramMarkdown(text string) string {
	var builder strings.Builder
	builder.Grow(len(text) + len(text)/4)
	for _, char := range text {
		switch char {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
			builder.WriteByte('\\')
		}
		builder.WriteRune(char)
	}
	return builder.String()
}

// sendTrafficWarningNotification / sendTrafficThrottledNotification keep the
// TypeScript names for the scheduler call sites.
func sendTrafficWarningNotification(ctx context.Context, userID int32, used, limit int64, instanceName string) bool {
	return SendTrafficWarningNotification(ctx, userID, used, limit, instanceName)
}

func sendTrafficThrottledNotification(ctx context.Context, userID int32, instanceName, hostName string) {
	SendTrafficThrottledNotification(ctx, userID, instanceName, hostName)
}
