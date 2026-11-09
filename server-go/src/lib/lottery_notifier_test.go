package lib

import (
	"context"
	"strings"
	"testing"
)

func lotteryTestAmount(value int64) *int64 { return &value }

func lotteryTestString(value string) *string { return &value }

func TestLotteryNotificationBalanceMessage(t *testing.T) {
	data := LotteryWinNotification{
		Username:    "alice",
		LotteryName: "周年庆",
		PrizeName:   "现金红包",
		Amount:      lotteryTestAmount(1234),
	}
	want := "用户 alice 在「周年庆」抽中了「现金红包」\n奖励金额：¥12.34\n已自动发放到用户余额"
	if got := lotteryNotificationBalanceMessage(data); got != want {
		t.Errorf("balance message = %q, want %q", got, want)
	}

	// `data.amount || 0` — a missing amount renders as 0.00.
	data.Amount = nil
	want = "用户 alice 在「周年庆」抽中了「现金红包」\n奖励金额：¥0.00\n已自动发放到用户余额"
	if got := lotteryNotificationBalanceMessage(data); got != want {
		t.Errorf("missing amount message = %q, want %q", got, want)
	}

	data.Amount = lotteryTestAmount(0)
	if got := lotteryNotificationBalanceMessage(data); got != want {
		t.Errorf("zero amount message = %q, want %q", got, want)
	}
}

func TestLotteryNotificationInstanceMessage(t *testing.T) {
	data := LotteryWinNotification{
		Username:     "alice",
		LotteryName:  "周年庆",
		PrizeName:    "云服务器",
		InstanceDesc: lotteryTestString("1核1G 30天"),
	}
	want := "用户 alice 在「周年庆」抽中了「云服务器」\n奖励内容：1核1G 30天\n请等待用户提交工单后手动发放"
	if got := lotteryNotificationInstanceMessage(data); got != want {
		t.Errorf("instance message = %q, want %q", got, want)
	}

	// An empty description is falsy in the TypeScript and drops the line.
	data.InstanceDesc = lotteryTestString("")
	want = "用户 alice 在「周年庆」抽中了「云服务器」\n请等待用户提交工单后手动发放"
	if got := lotteryNotificationInstanceMessage(data); got != want {
		t.Errorf("empty desc message = %q, want %q", got, want)
	}
	data.InstanceDesc = nil
	if got := lotteryNotificationInstanceMessage(data); got != want {
		t.Errorf("nil desc message = %q, want %q", got, want)
	}
}

func TestLotteryDiscordColor(t *testing.T) {
	cases := map[string]int{
		"💰 余额中奖通知": 0xffd700,
		"🎁 实例中奖通知": 0x9b59b6,
		"other":    0x00ff00,
	}
	for title, want := range cases {
		if got := lotteryDiscordColor(title); got != want {
			t.Errorf("lotteryDiscordColor(%q) = %#x, want %#x", title, got, want)
		}
	}
}

// The webhook `data` object is byte-identical to the TypeScript object literal:
// amount (cents) for balance prizes, instanceDesc for instance prizes, and no
// undefined keys.
func TestLotteryNotificationPayloadJSON(t *testing.T) {
	payload, err := lotteryMarshalJSON(lotteryNotificationPayload(LotteryWinNotification{
		LotteryID:   1,
		RecordID:    2,
		PrizeType:   "balance",
		Username:    "u",
		LotteryName: "L",
		PrizeName:   "P",
		Amount:      lotteryTestAmount(500),
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"lotteryId":1,"recordId":2,"prizeType":"balance","username":"u","lotteryName":"L","prizeName":"P","amount":500}`
	if string(payload) != want {
		t.Errorf("balance payload = %s, want %s", payload, want)
	}

	// amount 0 is still emitted (it is not undefined).
	zero := LotteryWinNotification{Amount: lotteryTestAmount(0)}
	payload, err = lotteryMarshalJSON(lotteryNotificationPayload(zero))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"amount":0`) {
		t.Errorf("zero amount missing from %s", payload)
	}

	payload, err = lotteryMarshalJSON(lotteryNotificationPayload(LotteryWinNotification{
		LotteryID: 1, RecordID: 2, PrizeType: "instance", Username: "u", LotteryName: "L", PrizeName: "P",
		InstanceDesc: lotteryTestString("1C1G"),
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want = `{"lotteryId":1,"recordId":2,"prizeType":"instance","username":"u","lotteryName":"L","prizeName":"P","instanceDesc":"1C1G"}`
	if string(payload) != want {
		t.Errorf("instance payload = %s, want %s", payload, want)
	}
}

// Node's JSON.stringify does not escape <, >, &, so neither may the signing
// payload (the HMAC covers these exact bytes).
func TestLotteryMarshalJSONDoesNotEscapeHTML(t *testing.T) {
	payload, err := lotteryMarshalJSON(lotteryNotificationPayload(LotteryWinNotification{
		Username: "a<b>&c",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"username":"a<b>&c"`) {
		t.Errorf("HTML characters were escaped: %s", payload)
	}
	if strings.Contains(string(payload), "\n") {
		t.Errorf("payload must not end with a newline: %q", string(payload))
	}
}

// Pins the HMAC over the exact compact JSON, independently computed with
// `openssl dgst -sha256 -hmac secret`.
func TestLotteryWebhookSignature(t *testing.T) {
	payload := []byte(`{"event":"lottery_win_balance","title":"t","message":"m","data":{"lotteryId":1,"recordId":2,"prizeType":"balance","username":"u","lotteryName":"L","prizeName":"P","amount":500}}`)
	want := "5ede1eccdbb9b6b0b9ea10fa1b166a78ba4d692a3d3092e8ebd2286cb519fb3d"
	if got := lotteryWebhookSignature("secret", payload); got != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestLotterySendTelegramRequiresTokenAndChatID(t *testing.T) {
	// Both missing.
	if err := lotterySendTelegram(context.Background(), nil, false, nil, false, "t", "m"); err == nil ||
		err.Error() != "Missing botToken or chatId" {
		t.Errorf("missing both error = %v", err)
	}
	// Empty strings are falsy in JavaScript.
	if err := lotterySendTelegram(context.Background(), "", true, "chat", true, "t", "m"); err == nil ||
		err.Error() != "Missing botToken or chatId" {
		t.Errorf("empty token error = %v", err)
	}
	if err := lotterySendTelegram(context.Background(), "token", true, "", true, "t", "m"); err == nil ||
		err.Error() != "Missing botToken or chatId" {
		t.Errorf("empty chat error = %v", err)
	}
}

func TestLotterySendDiscordAndWebhookRequireURL(t *testing.T) {
	if err := lotterySendDiscord(context.Background(), "", "t", "m"); err == nil ||
		err.Error() != "Missing webhookUrl" {
		t.Errorf("discord error = %v", err)
	}
	if err := lotterySendWebhook(context.Background(), "", "", false, "e", "t", "m", LotteryWinNotification{}); err == nil ||
		err.Error() != "Missing webhook URL" {
		t.Errorf("webhook error = %v", err)
	}
}

func TestLotteryNotificationTimeout(t *testing.T) {
	if lotteryNotificationFetchTimeoutMS != 15_000 {
		t.Errorf("timeout = %d, want 15000ms", lotteryNotificationFetchTimeoutMS)
	}
}
