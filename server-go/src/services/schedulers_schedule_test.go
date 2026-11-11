// Pure "next fire time" tests. Every scheduler's cron arithmetic is pinned here,
// including the "exactly on the boundary → next one" rule node-cron implements, and
// the UTC/Asia-Shanghai separation.
package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestTrafficSchedulerNextRun(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"on boundary moves forward", base, base.Add(3 * time.Minute)},
		{"one minute past", base.Add(time.Minute), base.Add(3 * time.Minute)},
		{"just before boundary", base.Add(2*time.Minute + 59*time.Second + 999*time.Millisecond), base.Add(3 * time.Minute)},
		{"just after boundary", base.Add(3*time.Minute + time.Millisecond), base.Add(6 * time.Minute)},
		{"last slot of the hour", base.Add(57 * time.Minute), base.Add(60 * time.Minute)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TrafficSchedulerNextRun(testCase.now); !got.Equal(testCase.want) {
				t.Fatalf("TrafficSchedulerNextRun(%s) = %s, want %s", testCase.now, got, testCase.want)
			}
		})
	}

	// A UTC+8 caller must not shift the boundary: node-cron on the production
	// container (UTC clock) fires at the UTC multiple of three minutes.
	shanghai := time.FixedZone("UTC+8", 8*3600)
	localNow := time.Date(2026, 11, 11, 18, 1, 0, 0, shanghai) // 10:01 UTC
	want := time.Date(2026, 11, 11, 10, 3, 0, 0, time.UTC)
	if got := TrafficSchedulerNextRun(localNow); !got.Equal(want) {
		t.Fatalf("TrafficSchedulerNextRun(local) = %s, want %s (UTC boundary)", got, want)
	}
}

func TestTrafficSchedulerDailyResetNextRun(t *testing.T) {
	// 16:04Z == 00:04 Asia/Shanghai on the next calendar day.
	now := time.Date(2026, 11, 11, 16, 4, 0, 0, time.UTC)
	want := time.Date(2026, 11, 11, 16, 5, 0, 0, time.UTC)
	if got := TrafficSchedulerDailyResetNextRun(now); !got.Equal(want) {
		t.Fatalf("TrafficSchedulerDailyResetNextRun(%s) = %s, want %s", now, got, want)
	}
	// Exactly on 00:05 Shanghai → the following day.
	exact := time.Date(2026, 11, 11, 16, 5, 0, 0, time.UTC)
	wantNext := time.Date(2026, 11, 12, 16, 5, 0, 0, time.UTC)
	if got := TrafficSchedulerDailyResetNextRun(exact); !got.Equal(wantNext) {
		t.Fatalf("TrafficSchedulerDailyResetNextRun(exact) = %s, want %s", got, wantNext)
	}
}

func TestTrafficSchedulerCleanupNextRuns(t *testing.T) {
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	if got, want := TrafficSchedulerCleanupNextRun(now), time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("cleanup next = %s, want %s", got, want)
	}
	if got, want := TrafficSchedulerNotificationCleanupNextRun(now), time.Date(2026, 11, 11, 3, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("notification cleanup next = %s, want %s", got, want)
	}
	if got, want := TrafficSchedulerSystemCleanupNextRun(now), time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("system cleanup next = %s, want %s", got, want)
	}

	// Exactly on the boundary → next day, not immediately.
	exact := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	if got, want := TrafficSchedulerCleanupNextRun(exact), time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("cleanup next at boundary = %s, want %s", got, want)
	}
}

func TestAutoPolicySchedulerNextRun(t *testing.T) {
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{base, base.Add(5 * time.Minute)},
		{base.Add(4*time.Minute + 30*time.Second), base.Add(5 * time.Minute)},
		{base.Add(5 * time.Minute), base.Add(10 * time.Minute)},
		{base.Add(59 * time.Minute), base.Add(60 * time.Minute)},
	}
	for _, testCase := range cases {
		if got := AutoPolicySchedulerNextRun(testCase.now); !got.Equal(testCase.want) {
			t.Fatalf("AutoPolicySchedulerNextRun(%s) = %s, want %s", testCase.now, got, testCase.want)
		}
	}
}

func TestAutoPolicySnapshotNameAndEligibility(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 5, 3, 123_000_000, time.UTC)
	if got, want := AutoPolicySnapshotName("auto", now), "auto-20261111-090503"; got != want {
		t.Fatalf("AutoPolicySnapshotName = %q, want %q", got, want)
	}

	cases := []struct {
		status     models.InstanceStatus
		hostStatus models.HostStatus
		want       bool
	}{
		{models.InstanceStatusRunning, models.HostStatusOnline, true},
		{models.InstanceStatusStopped, models.HostStatusOnline, true},
		{models.InstanceStatusRunning, models.HostStatusOffline, false},
		{models.InstanceStatusCreating, models.HostStatusOnline, false},
		{models.InstanceStatusDeleted, models.HostStatusOnline, false},
	}
	for _, testCase := range cases {
		if got := AutoPolicyInstanceEligible(testCase.status, testCase.hostStatus); got != testCase.want {
			t.Fatalf("AutoPolicyInstanceEligible(%s, %s) = %v, want %v",
				testCase.status, testCase.hostStatus, got, testCase.want)
		}
	}

	due := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	past := due.Add(-time.Minute)
	future := due.Add(time.Minute)
	if !AutoPolicyPolicyDue(true, nil, due) {
		t.Fatal("a policy with no nextRunAt must be due")
	}
	if !AutoPolicyPolicyDue(true, &past, due) {
		t.Fatal("a policy whose nextRunAt is in the past must be due")
	}
	if AutoPolicyPolicyDue(true, &future, due) {
		t.Fatal("a policy whose nextRunAt is in the future must not be due")
	}
	if AutoPolicyPolicyDue(false, &past, due) {
		t.Fatal("a disabled policy must never be due")
	}
}

func TestBillingSchedulerNextRuns(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.UTC)

	if got, want := BillingSchedulerAutoRenewNextRun(now), time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("auto-renew next = %s, want %s", got, want)
	}
	exactHour := time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)
	if got, want := BillingSchedulerAutoRenewNextRun(exactHour), time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("auto-renew next at boundary = %s, want %s", got, want)
	}

	if got, want := BillingSchedulerExpirySuspendNextRun(now), time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("expiry suspend next = %s, want %s", got, want)
	}
	exactHalf := time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC)
	if got, want := BillingSchedulerExpirySuspendNextRun(exactHalf), time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("expiry suspend next at boundary = %s, want %s", got, want)
	}

	if got, want := BillingSchedulerExpiryDeleteNextRun(now), time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("expiry delete next = %s, want %s", got, want)
	}
	if got, want := BillingSchedulerExpiryNotifyNextRun(now), time.Date(2026, 11, 11, 11, 15, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("expiry notify next = %s, want %s", got, want)
	}
	if got, want := BillingSchedulerRechargeCleanupNextRun(now), time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("recharge cleanup next = %s, want %s", got, want)
	}
	if got, want := BillingSchedulerPaymentCallbackCleanupNextRun(now), time.Date(2026, 11, 12, 4, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("callback cleanup next = %s, want %s", got, want)
	}
}

func TestBillingSchedulerNotifyWindow(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.UTC)
	min, max := BillingSchedulerNotifyWindow(now, 3)
	if want := now.Add(60 * time.Hour); !min.Equal(want) {
		t.Fatalf("min = %s, want %s", min, want)
	}
	if want := now.Add(84 * time.Hour); !max.Equal(want) {
		t.Fatalf("max = %s, want %s", max, want)
	}
}

func TestBillingSchedulerPurePredicates(t *testing.T) {
	if got := BillingSchedulerMonthlyPrice(100, nil); got != 100 {
		t.Fatalf("monthly price with nil cycle = %v, want 100", got)
	}
	three := int32(3)
	if got := BillingSchedulerMonthlyPrice(100, &three); got != 100.0/3 {
		t.Fatalf("monthly price with cycle 3 = %v, want %v", got, 100.0/3)
	}
	zero := int32(0)
	if got := BillingSchedulerMonthlyPrice(100, &zero); got != 100 {
		t.Fatalf("monthly price with cycle 0 = %v, want 100", got)
	}

	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	threshold := now.Add(24 * time.Hour)
	inFive := now.Add(5 * time.Hour)
	past := now.Add(-time.Hour)

	eligible := &models.Instance{AutoRenew: true, ExpiresAt: &inFive, Status: models.InstanceStatusRunning}
	if !BillingSchedulerAutoRenewEligible(eligible, now, threshold) {
		t.Fatal("auto-renewing instance expiring in 5h must be eligible")
	}
	disabled := &models.Instance{AutoRenew: false, ExpiresAt: &inFive, Status: models.InstanceStatusRunning}
	if BillingSchedulerAutoRenewEligible(disabled, now, threshold) {
		t.Fatal("autoRenew=false must not be eligible")
	}
	expired := &models.Instance{AutoRenew: true, ExpiresAt: &past, Status: models.InstanceStatusRunning}
	if BillingSchedulerAutoRenewEligible(expired, now, threshold) {
		t.Fatal("an already-expired instance must not be eligible")
	}
	tooFar := &models.Instance{AutoRenew: true, ExpiresAt: timePointer(now.Add(48 * time.Hour)), Status: models.InstanceStatusRunning}
	if BillingSchedulerAutoRenewEligible(tooFar, now, threshold) {
		t.Fatal("an instance beyond the 24h window must not be eligible")
	}
	suspended := &models.Instance{AutoRenew: true, ExpiresAt: &inFive, Status: models.InstanceStatusSuspended}
	if BillingSchedulerAutoRenewEligible(suspended, now, threshold) {
		t.Fatal("a suspended instance must not be eligible")
	}
}

func TestBillingSchedulerRenewBilling(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	price := models.Decimal{Decimal: decimal.NewFromFloat(100)}
	cycle := int32(1)
	expires := now.Add(10 * 24 * time.Hour)
	instance := &models.Instance{BillingPrice: &price, BillingCycle: &cycle, ExpiresAt: &expires}

	monthly, amount, newExpires, err := BillingSchedulerCalculateRenewBilling(instance, 3, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if monthly != 100 || amount != 300 {
		t.Fatalf("monthly=%v amount=%v, want 100/300", monthly, amount)
	}
	if want := expires.AddDate(0, 0, 3*31); !newExpires.Equal(want) {
		t.Fatalf("new expiry = %s, want %s", newExpires, want)
	}
	if _, _, _, err := BillingSchedulerCalculateRenewBilling(instance, 0, now); err == nil {
		t.Fatal("months=0 must be rejected")
	}
	if _, _, _, err := BillingSchedulerCalculateRenewBilling(instance, 25, now); err == nil {
		t.Fatal("months=25 must be rejected")
	}

	// An expired instance renews from `now`, not from the stale expiry.
	stale := now.Add(-48 * time.Hour)
	expired := &models.Instance{BillingPrice: &price, BillingCycle: &cycle, ExpiresAt: &stale}
	if _, _, newExpires, err := BillingSchedulerCalculateRenewBilling(expired, 1, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if want := now.AddDate(0, 0, 31); !newExpires.Equal(want) {
		t.Fatalf("expired instance new expiry = %s, want %s", newExpires, want)
	}
}

func TestMailSchedulerNextRuns(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.UTC)

	if got, want := MailExpirySchedulerNextRun(now), now.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("mail expiry next = %s, want %s", got, want)
	}
	if got, want := MailAutoRenewSchedulerNextRun(now), time.Date(2026, 11, 12, 2, 43, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("mail autorenew next = %s, want %s", got, want)
	}
	if got, want := MailUsageSchedulerNextRun(now), time.Date(2026, 11, 12, 3, 17, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("mail usage next = %s, want %s", got, want)
	}

	// Exactly on the scheduled minute → tomorrow.
	exact := time.Date(2026, 11, 11, 2, 43, 0, 0, time.UTC)
	if got, want := MailAutoRenewSchedulerNextRun(exact), time.Date(2026, 11, 12, 2, 43, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("mail autorenew next at boundary = %s, want %s", got, want)
	}

	if got, want := MailAutoRenewPeriodKey(7, time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)),
		"mail-auto-renew:7:2026-11-20T08:00:00.000Z"; got != want {
		t.Fatalf("period key = %q, want %q", got, want)
	}
}

func TestMailSchedulerAutoRenewEligibility(t *testing.T) {
	scanNow := time.Date(2026, 11, 11, 2, 43, 0, 0, time.UTC)
	deadline := scanNow.AddDate(0, 0, 3)
	expires := scanNow.AddDate(0, 0, 2)
	subscription := &models.MailSubscription{
		UserID: 9, Status: models.MailSubscriptionStatusActive, AutoRenew: true, ExpiresAt: expires,
	}
	if !MailAutoRenewSubscriptionEligible(subscription, 9, expires, scanNow, deadline) {
		t.Fatal("an active auto-renewing subscription inside the window must be eligible")
	}
	if MailAutoRenewSubscriptionEligible(subscription, 10, expires, scanNow, deadline) {
		t.Fatal("a mismatched user id must not be eligible")
	}
	if MailAutoRenewSubscriptionEligible(subscription, 9, expires.Add(time.Second), scanNow, deadline) {
		t.Fatal("a moved expires_at must invalidate the candidate")
	}
	notAuto := *subscription
	notAuto.AutoRenew = false
	if MailAutoRenewSubscriptionEligible(&notAuto, 9, expires, scanNow, deadline) {
		t.Fatal("autoRenew=false must not be eligible")
	}
	expired := *subscription
	expired.ExpiresAt = scanNow.Add(-time.Minute)
	if MailAutoRenewSubscriptionEligible(&expired, 9, expired.ExpiresAt, scanNow, deadline) {
		t.Fatal("an expired subscription must not be eligible")
	}
}

func TestTicketAutoCloseNextRunAndTimeout(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.UTC)
	if got, want := TicketAutoCloseSchedulerNextRun(now), now.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("ticket auto close next = %s, want %s", got, want)
	}

	cases := []struct {
		configured int
		want       time.Duration
	}{
		{0, 24 * time.Hour},
		{-5, 24 * time.Hour},
		{1, time.Hour},
		{48, 48 * time.Hour},
	}
	for _, testCase := range cases {
		if got := TicketAutoCloseTimeout(testCase.configured); got != testCase.want {
			t.Fatalf("TicketAutoCloseTimeout(%d) = %s, want %s", testCase.configured, got, testCase.want)
		}
	}
}

func TestStartupCleanupNextRuns(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.UTC)
	if got, want := StartupCleanupTransferNextRun(now), now.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("transfer cleanup next = %s, want %s", got, want)
	}
	if got, want := StartupCleanupCreateNextRun(now), now.Add(2*time.Minute); !got.Equal(want) {
		t.Fatalf("create timeout next = %s, want %s", got, want)
	}
	if got, want := StartupCleanupDailyNextRun(now), now.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("daily cleanup next = %s, want %s", got, want)
	}
}

func TestTrafficSchedulerQuotaArithmetic(t *testing.T) {
	userLimit := int64(1000)
	instanceLimit := int64(500)

	decision := TrafficSchedulerEvaluateQuota(&userLimit, 200, 0, &instanceLimit, 0, 100)
	if decision.UserEffectiveLimit == nil || *decision.UserEffectiveLimit != 1200 {
		t.Fatalf("user effective limit = %v, want 1200", decision.UserEffectiveLimit)
	}
	if decision.InstanceLimit == nil || *decision.InstanceLimit != 800 {
		t.Fatalf("instance limit = %v, want 800 (500 + 100 vip + 200 extra)", decision.InstanceLimit)
	}
	if decision.InstanceUsesExtraTraffic {
		t.Fatal("instance usage below the vip base must not count as using extra traffic")
	}
	if decision.UserOverLimit(1199) || !decision.UserOverLimit(1200) {
		t.Fatal("user over-limit boundary must be >= limit")
	}
	if !decision.InstanceOverLimit(800) {
		t.Fatal("instance at the limit must be over")
	}

	usingExtra := TrafficSchedulerEvaluateQuota(&userLimit, 200, 0, &instanceLimit, 600, 100)
	if !usingExtra.InstanceUsesExtraTraffic {
		t.Fatal("usage at/over the vip base must count as using extra traffic")
	}
	exhausted := TrafficSchedulerEvaluateQuota(&userLimit, 200, 200, &instanceLimit, 600, 100)
	if !exhausted.ExtraTrafficExhausted {
		t.Fatal("extra traffic quota fully used must be exhausted")
	}
	if !exhausted.InstanceOverLimit(600) {
		t.Fatal("an instance using exhausted extra traffic must be over the limit")
	}
	notExhausted := TrafficSchedulerEvaluateQuota(&userLimit, 200, 199, &instanceLimit, 600, 100)
	if notExhausted.ExtraTrafficExhausted {
		t.Fatal("extra traffic with one byte left must not be exhausted")
	}

	unlimited := TrafficSchedulerEvaluateQuota(nil, 0, 0, nil, 1<<40, 0)
	if unlimited.UserOverLimit(1<<40) || unlimited.InstanceOverLimit(1<<40) {
		t.Fatal("a nil limit means unlimited")
	}
}

func TestTrafficSchedulerNotificationRetryPolicy(t *testing.T) {
	if !IsNonRetryableTrafficNotificationError("Missing botToken or chatId") {
		t.Fatal("missing credentials must be non-retryable")
	}
	if !IsNonRetryableTrafficNotificationError("HTTP 403") {
		t.Fatal("403 must be non-retryable")
	}
	if IsNonRetryableTrafficNotificationError("dial tcp: i/o timeout") {
		t.Fatal("a transport timeout must be retryable")
	}
	if IsNonRetryableTrafficNotificationError("") {
		t.Fatal("an empty error must not be classified as non-retryable")
	}
}

// ==================== per-item failure isolation ====================

func TestMailSchedulerAutoRenewContinuesAfterItemFailure(t *testing.T) {
	// Reset the module guard so the test is order-independent.
	mailAutoRenewSetRunningForTest(false)

	previousScan := mailAutoRenewScan
	previousProcess := mailAutoRenewProcess
	previousLog := mailAutoRenewWriteLog
	defer func() {
		mailAutoRenewScan = previousScan
		mailAutoRenewProcess = previousProcess
		mailAutoRenewWriteLog = previousLog
	}()

	candidates := []db.MailExpiringSubscription{
		{ID: 1, UserID: 1},
		{ID: 2, UserID: 2},
		{ID: 3, UserID: 3},
	}
	mailAutoRenewScan = func(context.Context, int, time.Time) ([]db.MailExpiringSubscription, error) {
		return candidates, nil
	}

	processed := make([]int32, 0, len(candidates))
	mailAutoRenewProcess = func(_ context.Context, candidate db.MailExpiringSubscription, _, _ time.Time) (MailAutoRenewResult, error) {
		processed = append(processed, candidate.ID)
		if candidate.ID == 2 {
			return MailAutoRenewResult{}, context.DeadlineExceeded
		}
		return MailAutoRenewResult{Status: MailAutoRenewStatusRenewed, Amount: 10}, nil
	}

	logs := make([]string, 0, len(candidates))
	mailAutoRenewWriteLog = func(_ context.Context, _ int32, action, _ string, result string) {
		logs = append(logs, action+"/"+result)
	}

	RunMailAutoRenew(context.Background())

	if len(processed) != 3 {
		t.Fatalf("processed %d candidates (%v), want all 3 despite the failure", len(processed), processed)
	}
	if len(logs) != 3 {
		t.Fatalf("wrote %d logs (%v), want 3", len(logs), logs)
	}
	if logs[0] != "mail_auto_renew/success" || logs[1] != "mail_auto_renew_failed/failed" || logs[2] != "mail_auto_renew/success" {
		t.Fatalf("log sequence = %v, want success/failed/success", logs)
	}
}

func TestTicketAutoCloseContinuesAfterItemFailure(t *testing.T) {
	previousNotify := ticketAutoCloseSendNotification
	previousLog := ticketAutoCloseWriteLog
	previousHost := ticketAutoCloseLoadHostName
	defer func() {
		ticketAutoCloseSendNotification = previousNotify
		ticketAutoCloseWriteLog = previousLog
		ticketAutoCloseLoadHostName = previousHost
	}()

	hostName := "host-a"
	ticketAutoCloseLoadHostName = func(context.Context, *int32) (string, error) { return hostName, nil }
	ticketAutoCloseSendNotification = func(_ context.Context, userID int32, _, _ string) {
		if userID == 1 {
			panic("notifier exploded")
		}
	}
	logged := make([]int32, 0, 3)
	ticketAutoCloseWriteLog = func(_ context.Context, _ int32, ticketID int32, _ string, _ int) {
		logged = append(logged, ticketID)
	}

	closed := []db.TicketsAutoClosedTicket{
		{ID: 11, UserID: 1, Subject: "first"},
		{ID: 12, UserID: 2, Subject: "second"},
		{ID: 13, UserID: 3, Subject: "third"},
	}
	processAutoClosedTickets(context.Background(), closed, 24)

	if len(logged) != 2 || logged[0] != 12 || logged[1] != 13 {
		t.Fatalf("logged tickets = %v, want [12 13] (first ticket panicked)", logged)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

// ==================== overlap / first-run semantics ====================

func TestMailSchedulerAutoRenewSkipsWhileRunning(t *testing.T) {
	mailAutoRenewSetRunningForTest(true)
	defer mailAutoRenewSetRunningForTest(false)

	called := false
	previousScan := mailAutoRenewScan
	mailAutoRenewScan = func(context.Context, int, time.Time) ([]db.MailExpiringSubscription, error) {
		called = true
		return nil, nil
	}
	defer func() { mailAutoRenewScan = previousScan }()

	RunMailAutoRenew(context.Background())
	if called {
		t.Fatal("a run started while autoRenewRunning was true; the TypeScript skips that tick")
	}
}

func TestMailSchedulerUsageSkipsWhileRunning(t *testing.T) {
	mailUsageSetRunningForTest(true)
	defer mailUsageSetRunningForTest(false)

	// SyncMailUsage must return before touching the database. If it got as far as
	// the pool it would clear the flag in its defer; keeping it set proves the skip.
	SyncMailUsage(context.Background())
	if !MailUsageSyncIsRunning() {
		t.Fatal("usage sync cleared the running flag, so it did not skip")
	}
}

func TestSchedulersIntervalFirstRunSemantics(t *testing.T) {
	// No timer ever fires: only the immediate pass can run.
	never := func(time.Duration) <-chan time.Time { return make(chan time.Time) }

	immediate := newSchedulerGroup(context.Background())
	immediate.after = never
	var immediateRuns atomic.Int64
	immediate.startIntervalLoop("immediate", time.Hour, true, func(context.Context) error {
		immediateRuns.Add(1)
		return nil
	}, nil)
	if !waitFor(t, time.Second, func() bool { return immediateRuns.Load() == 1 }) {
		t.Fatalf("immediate interval ran %d times, want 1", immediateRuns.Load())
	}
	immediate.stop()

	delayed := newSchedulerGroup(context.Background())
	delayed.after = never
	var delayedRuns atomic.Int64
	delayed.startIntervalLoop("delayed", time.Hour, false, func(context.Context) error {
		delayedRuns.Add(1)
		return nil
	}, nil)
	time.Sleep(30 * time.Millisecond)
	if delayedRuns.Load() != 0 {
		t.Fatalf("non-immediate interval ran %d times before the interval, want 0", delayedRuns.Load())
	}
	delayed.stop()
}

func TestSchedulersCronWaitsForBoundary(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 1, 30, 0, time.UTC)
	requested := make(chan time.Duration, 1)

	group := newSchedulerGroup(context.Background())
	group.now = func() time.Time { return now }
	group.after = func(wait time.Duration) <-chan time.Time {
		select {
		case requested <- wait:
		default:
		}
		return make(chan time.Time)
	}

	var runs atomic.Int64
	group.startLoop("cron", TrafficSchedulerNextRun, func(context.Context) error {
		runs.Add(1)
		return nil
	}, nil)

	select {
	case wait := <-requested:
		if want := 90 * time.Second; wait != want {
			t.Fatalf("first wait = %s, want %s (10:01:30 → 10:03:00)", wait, want)
		}
	case <-time.After(time.Second):
		t.Fatal("loop never scheduled its first timer")
	}
	if runs.Load() != 0 {
		t.Fatalf("cron loop ran %d times before the boundary, want 0", runs.Load())
	}
	group.stop()
}
