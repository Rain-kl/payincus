// Port of server/src/lib/lottery-notifier.ts.
//
// Sends "you won" alerts to the admin-configured channel of a lottery
// (telegram / discord / webhook) and then marks the lottery record as notified.
// The exported entry point never returns an error: like the TypeScript, every
// failure is logged inside the notifier, so the draw route's fire-and-forget
// setImmediate/setTimeout call can never surface it to the user.
//
// Wire-format notes:
//   - Telegram is messaged with a *plain* fetch in the TypeScript (no SSRF assert
//     and no safeFetch), so this port uses notifierHTTPClient directly with
//     `redirect: 'manual'` semantics and the same 15s timeout. Bot tokens are
//     interpolated into the URL exactly as the TypeScript does.
//   - Discord/webhook go through assertSafeWebhookUrl + safeFetch, i.e. the Go
//     equivalent notifierPost (AssertSafeHTTPURL + revalidating transport with
//     redirects disabled).
//   - The webhook HMAC-SHA256 covers the compact JSON of
//     {event,title,message,data}; `timestamp` is appended afterwards. Struct
//     field order therefore reproduces the TypeScript object-literal order and
//     JSON is encoded without HTML escaping, because Node's JSON.stringify does
//     not escape <, > or &.
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
	"net/url"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

const (
	lotteryNotificationFetchTimeoutMS    = 15_000
	lotteryNotificationErrorPreviewChars = 2_000
)

// LotteryWinNotification mirrors LotteryWinNotification.
type LotteryWinNotification struct {
	LotteryID    int32
	RecordID     int32
	PrizeType    string // 'balance' | 'instance'
	Username     string
	LotteryName  string
	PrizeName    string
	Amount       *int64 // balance prize, in cents
	InstanceDesc *string
}

// lotteryWinPayload is the JSON `data` object. Field order and omitempty mirror
// the TypeScript object that is spread into the webhook body; `amount: 0` must
// still be emitted, hence the pointer.
type lotteryWinPayload struct {
	LotteryID    int32   `json:"lotteryId"`
	RecordID     int32   `json:"recordId"`
	PrizeType    string  `json:"prizeType"`
	Username     string  `json:"username"`
	LotteryName  string  `json:"lotteryName"`
	PrizeName    string  `json:"prizeName"`
	Amount       *int64  `json:"amount,omitempty"`
	InstanceDesc *string `json:"instanceDesc,omitempty"`
}

type lotteryWebhookData struct {
	Event   string            `json:"event"`
	Title   string            `json:"title"`
	Message string            `json:"message"`
	Data    lotteryWinPayload `json:"data"`
}

type lotteryWebhookBody struct {
	Event     string            `json:"event"`
	Title     string            `json:"title"`
	Message   string            `json:"message"`
	Data      lotteryWinPayload `json:"data"`
	Timestamp string            `json:"timestamp"`
}

// ==================== content templates ====================

func lotteryNotificationBalanceMessage(data LotteryWinNotification) string {
	amount := int64(0)
	if data.Amount != nil {
		amount = *data.Amount
	}
	return fmt.Sprintf("用户 %s 在「%s」抽中了「%s」\n奖励金额：¥%.2f\n已自动发放到用户余额",
		data.Username, data.LotteryName, data.PrizeName, float64(amount)/100)
}

func lotteryNotificationInstanceMessage(data LotteryWinNotification) string {
	lines := fmt.Sprintf("用户 %s 在「%s」抽中了「%s」\n", data.Username, data.LotteryName, data.PrizeName)
	if data.InstanceDesc != nil && *data.InstanceDesc != "" {
		lines += fmt.Sprintf("奖励内容：%s\n", *data.InstanceDesc)
	}
	return lines + "请等待用户提交工单后手动发放"
}

// lotteryDiscordColor mirrors the title-based colour selection.
func lotteryDiscordColor(title string) int {
	if strings.Contains(title, "余额") {
		return 0xffd700
	}
	if strings.Contains(title, "实例") {
		return 0x9b59b6
	}
	return 0x00ff00
}

func lotteryNotificationPayload(data LotteryWinNotification) lotteryWinPayload {
	return lotteryWinPayload{
		LotteryID:    data.LotteryID,
		RecordID:     data.RecordID,
		PrizeType:    data.PrizeType,
		Username:     data.Username,
		LotteryName:  data.LotteryName,
		PrizeName:    data.PrizeName,
		Amount:       data.Amount,
		InstanceDesc: data.InstanceDesc,
	}
}

// lotteryMarshalJSON mirrors JSON.stringify: compact JSON without HTML escaping
// and without the trailing newline json.Encoder appends.
func lotteryMarshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// ==================== entry point ====================

// SendLotteryWinNotification mirrors sendLotteryWinNotification: it resolves the
// lottery's notification channel, sends the message and marks the record as
// notified. Errors are logged and swallowed.
func SendLotteryWinNotification(ctx context.Context, data LotteryWinNotification) {
	if err := lotterySendWinNotification(ctx, data); err != nil {
		log.Printf("[LotteryNotifier] Failed to send notification: %v", err)
	}
}

func lotterySendWinNotification(ctx context.Context, data LotteryWinNotification) error {
	notifyConfig, err := db.LotteryGetNotificationConfig(ctx, data.LotteryID)
	if err != nil {
		return err
	}
	if notifyConfig == nil || !notifyConfig.Enabled {
		log.Printf("[LotteryNotifier] Lottery %d notification disabled or not configured", data.LotteryID)
		return nil
	}
	if data.PrizeType == "balance" && !notifyConfig.NotifyBalance {
		log.Printf("[LotteryNotifier] Balance notification disabled for lottery %d", data.LotteryID)
		return nil
	}
	if data.PrizeType == "instance" && !notifyConfig.NotifyInstance {
		log.Printf("[LotteryNotifier] Instance notification disabled for lottery %d", data.LotteryID)
		return nil
	}

	var title, message string
	if data.PrizeType == "balance" {
		title = "💰 余额中奖通知"
		message = lotteryNotificationBalanceMessage(data)
	} else {
		title = "🎁 实例中奖通知"
		message = lotteryNotificationInstanceMessage(data)
	}

	config := map[string]any{}
	if len(notifyConfig.Config) > 0 {
		_ = json.Unmarshal(notifyConfig.Config, &config)
	}

	switch notifyConfig.Type {
	case "telegram":
		botToken, botTokenPresent := config["botToken"]
		chatID, chatIDPresent := config["chatId"]
		if err := lotterySendTelegram(ctx, botToken, botTokenPresent, chatID, chatIDPresent, title, message); err != nil {
			return err
		}
	case "discord":
		webhookURL, present := config["webhookUrl"]
		if err := lotterySendDiscord(ctx, notifierString(webhookURL, present), title, message); err != nil {
			return err
		}
	case "webhook":
		rawURL, urlPresent := config["url"]
		secret, secretPresent := config["secret"]
		if err := lotterySendWebhook(ctx, notifierString(rawURL, urlPresent),
			notifierString(secret, secretPresent), notifierTruthy(secret, secretPresent),
			"lottery_win_"+data.PrizeType, title, message, data); err != nil {
			return err
		}
	default:
		log.Printf("[LotteryNotifier] Unknown channel type: %s", notifyConfig.Type)
	}

	if err := db.LotteryMarkRecordNotificationSent(ctx, data.RecordID); err != nil {
		return err
	}

	log.Printf("[LotteryNotifier] Sent %s win notification for lottery %d, user %s",
		data.PrizeType, data.LotteryID, data.Username)
	return nil
}

// ==================== channels ====================

// lotterySendTelegram mirrors sendTelegram. It deliberately uses the raw client
// (not notifierPost) because the TypeScript never SSRF-validates this URL: the
// host is hard-coded and only the bot token is interpolated.
func lotterySendTelegram(ctx context.Context, botTokenValue any, botTokenPresent bool, chatID any, chatIDPresent bool, title, message string) error {
	if !notifierTruthy(botTokenValue, botTokenPresent) || !notifierTruthy(chatID, chatIDPresent) {
		return fmt.Errorf("Missing botToken or chatId")
	}
	botToken := notifierString(botTokenValue, botTokenPresent)

	text := "*" + escapeTelegramMarkdown(title) + "*\n\n" + escapeTelegramMarkdown(message)

	payload, err := lotteryMarshalJSON(struct {
		ChatID    any    `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}{ChatID: chatID, Text: text, ParseMode: "MarkdownV2"})
	if err != nil {
		return err
	}

	requestCtx, cancel := context.WithTimeout(ctx, lotteryNotificationFetchTimeoutMS*time.Millisecond)
	defer cancel()

	endpoint := "https://api.telegram.org/bot" + botToken + "/sendMessage"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := notifierHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		if result.Description != "" {
			return fmt.Errorf("%s", result.Description)
		}
		return fmt.Errorf("Unknown Telegram error")
	}
	return nil
}

// lotterySendDiscord mirrors sendDiscord.
func lotterySendDiscord(ctx context.Context, webhookURL, title, message string) error {
	if webhookURL == "" {
		return fmt.Errorf("Missing webhookUrl")
	}

	payload, err := lotteryMarshalJSON(struct {
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
		Color:       lotteryDiscordColor(title),
		Timestamp:   formatISOTimeNow(),
		Footer: struct {
			Text string `json:"text"`
		}{Text: "Incudal 抽奖系统"},
	}}})
	if err != nil {
		return err
	}

	requestCtx, cancel := context.WithTimeout(ctx, lotteryNotificationFetchTimeoutMS*time.Millisecond)
	defer cancel()

	parsedURL, err := assertSafeWebhookTarget(requestCtx, webhookURL)
	if err != nil {
		return err
	}

	response, err := notifierPost(requestCtx, parsedURL.String(), "Webhook URL", payload,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("%s", lotteryReadSafeError(response))
	}
	return nil
}

// lotterySendWebhook mirrors sendWebhook, including the HMAC signature header.
func lotterySendWebhook(ctx context.Context, rawURL, secret string, secretPresent bool, event, title, message string, data LotteryWinNotification) error {
	if rawURL == "" {
		return fmt.Errorf("Missing webhook URL")
	}

	payloadData := lotteryNotificationPayload(data)
	payload, err := lotteryMarshalJSON(lotteryWebhookData{
		Event: event, Title: title, Message: message, Data: payloadData,
	})
	if err != nil {
		return err
	}

	headers := map[string]string{"Content-Type": "application/json"}
	if secretPresent {
		headers["X-Signature"] = lotteryWebhookSignature(secret, payload)
	}

	body, err := lotteryMarshalJSON(lotteryWebhookBody{
		Event: event, Title: title, Message: message, Data: payloadData,
		Timestamp: formatISOTimeNow(),
	})
	if err != nil {
		return err
	}

	requestCtx, cancel := context.WithTimeout(ctx, lotteryNotificationFetchTimeoutMS*time.Millisecond)
	defer cancel()

	parsedURL, err := assertSafeWebhookTarget(requestCtx, rawURL)
	if err != nil {
		return err
	}

	response, err := notifierPost(requestCtx, parsedURL.String(), "Webhook URL", body, headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

// lotteryWebhookSignature mirrors
// crypto.createHmac('sha256', secret).update(JSON.stringify(payload)).digest('hex').
func lotteryWebhookSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// assertSafeWebhookTarget mirrors assertSafeWebhookUrl: parse/validate the URL and
// return it for the request.
func assertSafeWebhookTarget(ctx context.Context, rawURL string) (*url.URL, error) {
	return AssertSafeWebhookURL(ctx, rawURL)
}

// lotteryReadSafeError mirrors readSafeNotificationError in lottery-notifier.ts,
// which is byte-for-byte the same helper as lib/notifier.ts (sanitize tokens,
// preview the first 2000 chars, else "HTTP <status>").
func lotteryReadSafeError(response *http.Response) string {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	sanitized := SanitizeTokensInString(string(body))
	if runes := []rune(sanitized); len(runes) > lotteryNotificationErrorPreviewChars {
		sanitized = string(runes[:lotteryNotificationErrorPreviewChars])
	}
	if sanitized != "" {
		return sanitized
	}
	return fmt.Sprintf("HTTP %d", response.StatusCode)
}
