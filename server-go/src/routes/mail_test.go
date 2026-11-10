// Tests for the mail module port.
//
// Three layers, matching how the Node guards would have pinned them:
//
//  1. Pure contract tests (always run): request normalisation, JS number/date
//     semantics, refund period math, ISO rendering, and the account-provisioning
//     compensation matrix. These are the branches that decide whether a value is
//     ever written.
//  2. Namespace pins (always run): the advisory lock constants the mail flows use.
//  3. DB-backed guard tests (skipped unless MAIL_TEST_DATABASE_URL is set): they
//     drive the real handlers against a throwaway database and prove
//     - a failed DNS/TXT verification never persists `verified`;
//     - a failed upstream domain/account provisioning writes no local row, so
//     the subscription's disk allocation is unchanged;
//     - the subscription flow really takes advisory lock 4109.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== helpers ====================

func mailTestUser() *lib.AccessTokenClaims {
	return &lib.AccessTokenClaims{ID: 2, Username: "parityuser", Role: "user", Status: "active"}
}

func mailTestDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func mailTestBody(t *testing.T, body any) string {
	t.Helper()
	if body == nil {
		return ""
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(encoded)
}

// ==================== normalisation contract ====================

func TestMailNormalizePlanInput(t *testing.T) {
	_, apiErr := mailNormalizePlanInput(map[string]any{}, true)
	if apiErr == nil || apiErr.Details != "请填写所有必填字段" {
		t.Fatalf("require-all error = %+v", apiErr)
	}

	input, apiErr := mailNormalizePlanInput(map[string]any{
		"sourceId":     float64(7),
		"name":         " Basic ",
		"description":  "  ",
		"domainLimit":  float64(3),
		"diskLimitGb":  float64(10),
		"billingCycle": "monthly",
		"price":        12.34,
		"enabled":      "yes",
		"sortOrder":    float64(-5),
	}, false)
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if input.Name == nil || *input.Name != "Basic" {
		t.Fatalf("name = %v", input.Name)
	}
	if input.Description != nil {
		t.Fatalf("blank description must normalise to undefined, got %v", *input.Description)
	}
	// `input.enabled === true` — a truthy non-boolean becomes false.
	if input.Enabled == nil || *input.Enabled {
		t.Fatalf("enabled = %v", input.Enabled)
	}

	_, apiErr = mailNormalizePlanInput(map[string]any{"price": 12.345, "sourceId": float64(1), "name": "x",
		"domainLimit": float64(1), "diskLimitGb": float64(1), "billingCycle": "monthly"}, true)
	if apiErr == nil || apiErr.Details != "价格最多支持两位小数" {
		t.Fatalf("two-decimal error = %+v", apiErr)
	}

	_, apiErr = mailNormalizePlanInput(map[string]any{"billingCycle": "weekly"}, false)
	if apiErr == nil || apiErr.Details != "计费周期无效" {
		t.Fatalf("billing-cycle error = %+v", apiErr)
	}

	// Safe-integer guard: 1.5 is a number but not an integer.
	_, apiErr = mailNormalizePlanInput(map[string]any{"domainLimit": 1.5}, false)
	if apiErr == nil || apiErr.Details != "域名数量限制 无效" {
		t.Fatalf("integer error = %+v", apiErr)
	}
}

func TestMailNormalizeSourceInput(t *testing.T) {
	_, apiErr := mailNormalizeSourceInput(nil, false)
	if apiErr == nil || apiErr.Details != "请求参数无效" {
		t.Fatalf("non-object error = %+v", apiErr)
	}

	input, apiErr := mailNormalizeSourceInput(map[string]any{
		"name":           "  US West  ",
		"code":           "US_W",
		"apiUrl":         "https://api.example.com/",
		"smarterMailUrl": "https://mail.example.com",
		"apiKey":         "  secret  ",
		"enabled":        true,
		"sortOrder":      float64(3),
	}, false)
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if input.Code == nil || *input.Code != "us_w" {
		t.Fatalf("code = %v", input.Code)
	}
	if input.APIKey == nil || *input.APIKey != "secret" {
		t.Fatalf("apiKey = %v", input.APIKey)
	}

	_, apiErr = mailNormalizeSourceInput(map[string]any{"code": "a"}, false)
	if apiErr == nil || !strings.Contains(apiErr.Details, "地区代码只能包含") {
		t.Fatalf("code error = %+v", apiErr)
	}

	_, apiErr = mailNormalizeSourceInput(map[string]any{"enabled": "true"}, false)
	if apiErr == nil || apiErr.Details != "启用状态无效" {
		t.Fatalf("enabled error = %+v", apiErr)
	}

	// requireAll uses JS truthiness: an empty apiKey is "missing".
	_, apiErr = mailNormalizeSourceInput(map[string]any{"name": "n", "code": "ab", "apiUrl": "https://a.example.com",
		"smarterMailUrl": "https://b.example.com", "apiKey": ""}, true)
	if apiErr == nil || apiErr.Details != "请填写所有必填字段" {
		t.Fatalf("require-all error = %+v", apiErr)
	}
}

func TestMailNormalizeAccountInput(t *testing.T) {
	_, apiErr := mailNormalizeAccountInput(map[string]any{}, true, true, 2048)
	if apiErr == nil || !strings.Contains(apiErr.Details, "用户名只能包含") {
		t.Fatalf("username error = %+v", apiErr)
	}

	input, apiErr := mailNormalizeAccountInput(map[string]any{
		"username":    " Bob.Smith ",
		"password":    "12345678",
		"displayName": "  ",
		"diskLimitMb": float64(512),
	}, true, true, 2048)
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if *input.Username != "bob.smith" {
		t.Fatalf("username = %q", *input.Username)
	}
	if input.DisplayName != nil {
		t.Fatalf("blank displayName must normalise to undefined")
	}

	_, apiErr = mailNormalizeAccountInput(map[string]any{"username": "bob", "password": "1234567"}, true, true, 2048)
	if apiErr == nil || apiErr.Details != "密码长度需为 8-128 位" {
		t.Fatalf("password error = %+v", apiErr)
	}

	_, apiErr = mailNormalizeAccountInput(map[string]any{"username": "bob", "password": "12345678",
		"diskLimitMb": float64(2049)}, true, true, 2048)
	if apiErr == nil || apiErr.Details != "邮箱容量需为 1-2048 MB 的整数" {
		t.Fatalf("disk error = %+v", apiErr)
	}

	// Zero quota is invalid before any field is looked at.
	_, apiErr = mailNormalizeAccountInput(map[string]any{"username": "bob"}, true, true, 0)
	if apiErr == nil || apiErr.Details != "订阅磁盘配额无效" {
		t.Fatalf("quota error = %+v", apiErr)
	}
}

func TestMailNormalizeCancelInput(t *testing.T) {
	_, apiErr := mailNormalizeCancelInput(map[string]any{"refundType": "bogus"})
	if apiErr == nil || apiErr.Details != "请选择退款方式" {
		t.Fatalf("refund-type error = %+v", apiErr)
	}
	_, apiErr = mailNormalizeCancelInput(map[string]any{"refundType": "full"})
	if apiErr == nil || apiErr.Details != "退款时必须填写原因" {
		t.Fatalf("reason-required error = %+v", apiErr)
	}
	input, apiErr := mailNormalizeCancelInput(map[string]any{"refundType": "remaining", "reason": "  不想要了  "})
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if input.Reason == nil || *input.Reason != "不想要了" {
		t.Fatalf("reason = %v", input.Reason)
	}
}

func TestMailMergeSourceAPIKey(t *testing.T) {
	empty := ""
	masked := db.MaskedMailSourceAPIKeyValue
	stars := "***abc"
	real := "real-key"
	for _, value := range []*string{nil, &empty, &masked, &stars} {
		if got := mailMergeSourceAPIKey(value); got != nil {
			t.Fatalf("value %v must be ignored", value)
		}
	}
	if got := mailMergeSourceAPIKey(&real); got == nil || *got != "real-key" {
		t.Fatalf("real key dropped")
	}
}

// ==================== JS semantics ====================

func TestMailCalendarArithmeticMatchesJSSetMonth(t *testing.T) {
	// JS: new Date(2026,0,31); setMonth(+1) -> 2026-03-03 (February has 28 days).
	// Go normalises the same way through time.Date.
	base := time.Date(2026, 1, 31, 12, 0, 0, 0, time.Local)
	got := db.MailAddCalendarMonths(base, 1)
	if got.Month() != time.March || got.Day() != 3 {
		t.Fatalf("setMonth(Jan 31 + 1) = %v, want Mar 3", got)
	}

	// Leap day + 1 year -> Mar 1.
	leap := time.Date(2028, 2, 29, 8, 30, 0, 0, time.Local)
	if got := db.MailAddCalendarYears(leap, 1); got.Month() != time.March || got.Day() != 1 {
		t.Fatalf("setFullYear(Feb 29 + 1) = %v, want Mar 1", got)
	}

	// setDate overflow: Jan 31 + 1 day -> Feb 1.
	if got := db.MailAddCalendarDays(base, 1); got.Month() != time.February || got.Day() != 1 {
		t.Fatalf("setDate(Jan 31 + 1) = %v, want Feb 1", got)
	}
}

func TestMailJSNumberString(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{3.3333333333333335, "3.3333333333333335"},
		{5, "5"},
		{-0.5, "-0.5"},
		{120, "120"},
	}
	for _, testCase := range cases {
		if got := mailJSNumberString(testCase.value); got != testCase.want {
			t.Fatalf("mailJSNumberString(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

func TestMailGetPaymentPeriodMs(t *testing.T) {
	monthly := models.MailBillingCycleMonthly
	yearly := models.MailBillingCycleYearly
	day := int64(24 * 60 * 60 * 1000)

	if got := mailGetPaymentPeriodMs(nil, monthly); got != 30*day {
		t.Fatalf("monthly default = %d", got)
	}
	if got := mailGetPaymentPeriodMs(nil, yearly); got != 365*day {
		t.Fatalf("yearly default = %d", got)
	}
	remark := "续费域名邮箱 3 个月"
	if got := mailGetPaymentPeriodMs(&remark, monthly); got != 90*day {
		t.Fatalf("renew 3 months = %d", got)
	}
	yearlyRemark := "续费域名邮箱 12 个月"
	if got := mailGetPaymentPeriodMs(&yearlyRemark, yearly); got != 365*day {
		t.Fatalf("yearly renew 12 months = %d", got)
	}
	// Purchase remarks always fall back to the billing-cycle default.
	purchase := "购买域名邮箱 - Basic"
	if got := mailGetPaymentPeriodMs(&purchase, monthly); got != 30*day {
		t.Fatalf("purchase = %d", got)
	}
}

func TestMailISOTime(t *testing.T) {
	value := time.Date(2026, 1, 29, 12, 34, 56, 789*int(time.Millisecond), time.FixedZone("X", 8*3600))
	if got := mailISOTime(value); got != "2026-01-29T04:34:56.789Z" {
		t.Fatalf("mailISOTime = %q", got)
	}
}

// ==================== advisory lock namespace pins ====================

// TestMailLockNamespaces pins the cross-process contract from WAVE_D_SPEC §0.4.
// A different number silently stops excluding the Node backend's own holders.
func TestMailLockNamespaces(t *testing.T) {
	if db.MailSubscriptionLockNamespace != 4109 {
		t.Fatalf("MAIL_SUBSCRIPTION_LOCK_NAMESPACE = %d, want 4109", db.MailSubscriptionLockNamespace)
	}
	if db.MailDomainLockNamespace != 4116 {
		t.Fatalf("MAIL_DOMAIN_LOCK_NAMESPACE = %d, want 4116", db.MailDomainLockNamespace)
	}
}

// ==================== account provisioning compensation ====================

func TestMailProvisionMailAccountMatrix(t *testing.T) {
	account := models.MailAccount{ID: 7, DomainID: 1}

	t.Run("upstream failure writes nothing", func(t *testing.T) {
		localCalled := false
		compensated := false
		result := mailProvisionMailAccount(
			func() error { return errors.New("fetch failed") },
			func() error { compensated = true; return nil },
			func() (models.MailAccount, error) { localCalled = true; return account, nil },
		)
		if localCalled || compensated {
			t.Fatalf("localCalled=%v compensated=%v, want both false", localCalled, compensated)
		}
		if result.Status != http.StatusBadGateway || result.Error == nil ||
			result.Error.Details != "创建邮箱账户失败: fetch failed" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("local failure compensates upstream", func(t *testing.T) {
		compensated := false
		result := mailProvisionMailAccount(
			func() error { return nil },
			func() error { compensated = true; return nil },
			func() (models.MailAccount, error) { return models.MailAccount{}, errors.New("unique violation") },
		)
		if !compensated {
			t.Fatalf("upstream mailbox was not compensated")
		}
		if result.Status != http.StatusInternalServerError || result.Error == nil ||
			result.Error.Code != lib.ErrInternalError {
			t.Fatalf("result = %+v", result)
		}
		// apiError(INTERNAL_ERROR, ...) drops the details, like the TypeScript.
		if result.Error.Details != "" {
			t.Fatalf("internal errors must not expose details, got %q", result.Error.Details)
		}
	})

	t.Run("failed compensation surfaces 502", func(t *testing.T) {
		result := mailProvisionMailAccount(
			func() error { return nil },
			func() error { return errors.New("compensation down") },
			func() (models.MailAccount, error) { return models.MailAccount{}, errors.New("unique violation") },
		)
		if result.Status != http.StatusBadGateway || result.Error == nil ||
			result.Error.Details != "邮箱账户已在远端创建，但本地记录保存失败且远端补偿删除失败，请联系管理员处理" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("success returns the local row", func(t *testing.T) {
		result := mailProvisionMailAccount(
			func() error { return nil },
			func() error { return nil },
			func() (models.MailAccount, error) { return account, nil },
		)
		if result.Status != http.StatusOK || result.Account == nil || result.Account.ID != 7 {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestMailDiskQuotaAllows(t *testing.T) {
	if !db.MailDiskQuotaAllows(2048, 2048, 10) { // 10 GB = 10240 MB
		t.Fatalf("2048+2048 must fit in 10 GB")
	}
	if db.MailDiskQuotaAllows(9000, 2048, 10) {
		t.Fatalf("9000+2048 must not fit in 10 GB")
	}
	if !db.MailDiskQuotaAllows(8192, 2048, 10) {
		t.Fatalf("exact boundary must fit")
	}
}

// ==================== DB-backed guard tests ====================

// mailTestSetup wires a throwaway database for the guard tests. It is skipped
// unless MAIL_TEST_DATABASE_URL points at a disposable database: the handlers
// commit real rows, so they must never run against a shared environment.
func mailTestSetup(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("MAIL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MAIL_TEST_DATABASE_URL to a throwaway database to run the DB-backed mail guard tests")
	}
	os.Setenv("DATABASE_URL", dsn)
	if err := db.Init(context.Background()); err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	gin.SetMode(gin.TestMode)
}

func mailTestUserID(t *testing.T, ctx context.Context, username string) int32 {
	t.Helper()
	var id int32
	err := db.Pool().QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Skipf("user %q not present in the test database", username)
	}
	if err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	return id
}

type mailTestFixture struct {
	userID         int32
	sourceID       int32
	planID         int32
	subscriptionID int32
}

func mailTestCreateFixture(t *testing.T, ctx context.Context, username string) mailTestFixture {
	t.Helper()
	userID := mailTestUserID(t, ctx, username)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	source, err := db.MailCreateSource(ctx, db.MailCreateSourceInput{
		Name:           "Mail guard source " + suffix,
		Code:           "mg" + suffix[len(suffix)-8:],
		APIURL:         "https://cranemail.guard.invalid",
		APIKey:         "guard-key",
		SmarterMailURL: "https://smartermail.guard.invalid",
		Enabled:        true,
		SortOrder:      0,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	plan, err := db.MailCreatePlan(ctx, db.MailCreatePlanInput{
		SourceID:     source.ID,
		Name:         "Guard plan " + suffix,
		DomainLimit:  3,
		DiskLimitGB:  10,
		BillingCycle: models.MailBillingCycleMonthly,
		Price:        9.99,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	subscription, err := db.MailCreateSubscription(ctx, db.MailCreateSubscriptionInput{
		UserID:      userID,
		SourceID:    source.ID,
		PlanID:      plan.ID,
		DomainLimit: 3,
		DiskLimitGB: 10,
		ExpiresAt:   time.Now().Add(30 * 24 * time.Hour).UTC(),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	fixture := mailTestFixture{userID: userID, sourceID: source.ID, planID: plan.ID, subscriptionID: subscription.ID}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_subscriptions WHERE id = $1`, fixture.subscriptionID)
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_plans WHERE id = $1`, fixture.planID)
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_sources WHERE id = $1`, fixture.sourceID)
	})
	return fixture
}

func mailTestDomainCount(t *testing.T, ctx context.Context, subscriptionID int32) int64 {
	t.Helper()
	var count int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_domains WHERE subscription_id = $1`, subscriptionID).Scan(&count); err != nil {
		t.Fatalf("count domains: %v", err)
	}
	return count
}

// TestMailVerificationFailureNeverWritesVerified is the Wave D §2 reverse test:
// a DNS/TXT check that fails (or reports unverified) must not persist "verified".
func TestMailVerificationFailureNeverWritesVerified(t *testing.T) {
	mailTestSetup(t)
	ctx := context.Background()
	fixture := mailTestCreateFixture(t, ctx, "parityuser")

	domain, err := db.MailCreateDomain(ctx, db.MailCreateDomainInput{
		SubscriptionID: fixture.subscriptionID,
		SourceID:       fixture.sourceID,
		Domain:         fmt.Sprintf("guard-%d.example.com", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool().Exec(context.Background(), `DELETE FROM mail_domains WHERE id = $1`, domain.ID)
	})

	original := mailCraneGetDomainInfo
	t.Cleanup(func() { mailCraneGetDomainInfo = original })

	// 1. Upstream says "not verified" -> 200 pending, and nothing is written.
	mailCraneGetDomainInfo = func(context.Context, models.MailSource, string) (services.CraneMailDomainInfo, error) {
		return services.CraneMailDomainInfo{Verified: false, DNSRecords: []services.CraneMailDNSRecord{}}, nil
	}
	context1, recorder1 := affTestContext(t, http.MethodPost, "/api/mail/domains/1/verify", "{}",
		"application/json", gin.Params{{Key: "id", Value: fmt.Sprintf("%d", domain.ID)}}, mailTestUser())
	mailUserVerifyDomain(context1)
	if recorder1.Code != http.StatusOK {
		t.Fatalf("unverified status = %d body=%s", recorder1.Code, recorder1.Body.String())
	}
	body := mailTestDecode(t, recorder1)
	if body["verified"] != false || body["status"] != "pending" {
		t.Fatalf("unverified body = %v", body)
	}
	// `txtRecord` is optional in the TypeScript: without an upstream value the key
	// must be omitted, not sent as null.
	if _, present := body["txtRecord"]; present {
		t.Fatalf("txtRecord must be omitted when absent, body = %v", body)
	}

	var status string
	var verifiedAt *time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT status, verified_at FROM mail_domains WHERE id = $1`, domain.ID).Scan(&status, &verifiedAt); err != nil {
		t.Fatalf("read domain: %v", err)
	}
	if status != "pending" || verifiedAt != nil {
		t.Fatalf("failed verification persisted status=%q verifiedAt=%v", status, verifiedAt)
	}

	// 2. Upstream transport failure -> 502, still nothing written.
	mailCraneGetDomainInfo = func(context.Context, models.MailSource, string) (services.CraneMailDomainInfo, error) {
		return services.CraneMailDomainInfo{}, errors.New("fetch failed")
	}
	context2, recorder2 := affTestContext(t, http.MethodPost, "/api/mail/domains/1/verify", "{}",
		"application/json", gin.Params{{Key: "id", Value: fmt.Sprintf("%d", domain.ID)}}, mailTestUser())
	mailUserVerifyDomain(context2)
	if recorder2.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure status = %d body=%s", recorder2.Code, recorder2.Body.String())
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT status, verified_at FROM mail_domains WHERE id = $1`, domain.ID).Scan(&status, &verifiedAt); err != nil {
		t.Fatalf("read domain: %v", err)
	}
	if status != "pending" || verifiedAt != nil {
		t.Fatalf("upstream failure persisted status=%q verifiedAt=%v", status, verifiedAt)
	}

	// 3. Only an actually-verified answer writes the column.
	mailCraneGetDomainInfo = func(context.Context, models.MailSource, string) (services.CraneMailDomainInfo, error) {
		return services.CraneMailDomainInfo{Verified: true, DNSRecords: []services.CraneMailDNSRecord{}}, nil
	}
	context3, recorder3 := affTestContext(t, http.MethodPost, "/api/mail/domains/1/verify", "{}",
		"application/json", gin.Params{{Key: "id", Value: fmt.Sprintf("%d", domain.ID)}}, mailTestUser())
	mailUserVerifyDomain(context3)
	if recorder3.Code != http.StatusOK {
		t.Fatalf("verified status = %d body=%s", recorder3.Code, recorder3.Body.String())
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT status, verified_at FROM mail_domains WHERE id = $1`, domain.ID).Scan(&status, &verifiedAt); err != nil {
		t.Fatalf("read domain: %v", err)
	}
	if status != "verified" || verifiedAt == nil {
		t.Fatalf("verified answer did not persist: status=%q verifiedAt=%v", status, verifiedAt)
	}
}

// TestMailDomainCreateUpstreamFailureKeepsQuota proves the provisioning rule: a
// failed CraneMail call writes no local domain row, so the subscription's domain
// quota is not consumed.
func TestMailDomainCreateUpstreamFailureKeepsQuota(t *testing.T) {
	mailTestSetup(t)
	ctx := context.Background()
	fixture := mailTestCreateFixture(t, ctx, "parityuser")

	original := mailCraneCreateDomain
	t.Cleanup(func() { mailCraneCreateDomain = original })
	called := false
	mailCraneCreateDomain = func(context.Context, models.MailSource, string, float64, int) (services.CraneMailDomainCredentials, error) {
		called = true
		return services.CraneMailDomainCredentials{}, errors.New("fetch failed")
	}

	body := mailTestBody(t, map[string]any{"domain": fmt.Sprintf("guard-create-%d.example.com", time.Now().UnixNano())})
	context1, recorder1 := affTestContext(t, http.MethodPost, "/api/mail/domains", body,
		"application/json", nil, mailTestUser())
	mailUserCreateDomain(context1)

	if !called {
		t.Fatalf("upstream provisioning was not attempted")
	}
	if recorder1.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", recorder1.Code, recorder1.Body.String())
	}
	if count := mailTestDomainCount(t, ctx, fixture.subscriptionID); count != 0 {
		t.Fatalf("failed provisioning left %d domain row(s); quota was consumed", count)
	}
}

// TestMailAccountCreateUpstreamFailureKeepsQuota proves the mailbox variant: the
// failed SmarterMail call writes no account row, so the disk allocation is
// unchanged (quota is SUM(disk_limit_mb) of the local rows).
func TestMailAccountCreateUpstreamFailureKeepsQuota(t *testing.T) {
	mailTestSetup(t)
	ctx := context.Background()
	fixture := mailTestCreateFixture(t, ctx, "parityuser")

	domain, err := db.MailCreateDomain(ctx, db.MailCreateDomainInput{
		SubscriptionID: fixture.subscriptionID,
		SourceID:       fixture.sourceID,
		Domain:         fmt.Sprintf("guard-account-%d.example.com", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool().Exec(context.Background(), `DELETE FROM mail_domains WHERE id = $1`, domain.ID)
	})
	verified := models.MailDomainStatusVerified
	if _, err := db.MailUpdateDomain(ctx, domain.ID, db.MailUpdateDomainInput{Status: &verified}); err != nil {
		t.Fatalf("verify domain: %v", err)
	}

	original := mailSmarterCreateAccount
	t.Cleanup(func() { mailSmarterCreateAccount = original })
	compensated := false
	mailSmarterCreateAccount = func(context.Context, models.MailSource, string, string, string, services.SmarterMailAccountInput) error {
		return errors.New("fetch failed")
	}
	originalDelete := mailSmarterDeleteAccount
	t.Cleanup(func() { mailSmarterDeleteAccount = originalDelete })
	mailSmarterDeleteAccount = func(context.Context, models.MailSource, string, string, string, string) error {
		compensated = true
		return nil
	}

	body := mailTestBody(t, map[string]any{"username": "guarduser", "password": "12345678"})
	context1, recorder1 := affTestContext(t, http.MethodPost, "/api/mail/domains/1/accounts", body,
		"application/json", gin.Params{{Key: "id", Value: fmt.Sprintf("%d", domain.ID)}}, mailTestUser())
	mailUserCreateAccount(context1)

	if recorder1.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", recorder1.Code, recorder1.Body.String())
	}
	if compensated {
		t.Fatalf("compensation must not run when the upstream create itself failed")
	}

	var accountCount int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_accounts WHERE domain_id = $1`, domain.ID).Scan(&accountCount); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accountCount != 0 {
		t.Fatalf("failed provisioning left %d account row(s)", accountCount)
	}

	allocated, err := db.MailSubscriptionAllocatedDiskMB(ctx, nil, fixture.subscriptionID, nil)
	if err != nil {
		t.Fatalf("allocated: %v", err)
	}
	if allocated != 0 {
		t.Fatalf("allocated disk = %d MB after a failed create, want 0", allocated)
	}
}

// TestMailAutoRenewTakesSubscriptionLock proves the subscription flow really
// takes advisory lock 4109: while another transaction holds it, the handler must
// answer "正在处理" instead of writing.
func TestMailAutoRenewTakesSubscriptionLock(t *testing.T) {
	mailTestSetup(t)
	ctx := context.Background()
	fixture := mailTestCreateFixture(t, ctx, "parityuser")

	holder, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	locked, err := db.TryAdvisoryTransactionLock(ctx, holder, db.MailSubscriptionLockNamespace, fixture.userID)
	if err != nil || !locked {
		_ = holder.Rollback(ctx)
		t.Fatalf("could not hold lock 4109: locked=%v err=%v", locked, err)
	}

	body := mailTestBody(t, map[string]any{"autoRenew": true})
	blockedContext, blockedRecorder := affTestContext(t, http.MethodPatch, "/api/mail/subscription/auto-renew",
		body, "application/json", nil, mailTestUser())
	mailUserSetAutoRenew(blockedContext)
	if blockedRecorder.Code != http.StatusBadRequest {
		_ = holder.Rollback(ctx)
		t.Fatalf("locked status = %d body=%s", blockedRecorder.Code, blockedRecorder.Body.String())
	}
	blocked := mailTestDecode(t, blockedRecorder)
	if blocked["details"] != "邮箱订阅正在处理，请稍后重试" {
		_ = holder.Rollback(ctx)
		t.Fatalf("locked body = %v", blocked)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	freeContext, freeRecorder := affTestContext(t, http.MethodPatch, "/api/mail/subscription/auto-renew",
		body, "application/json", nil, mailTestUser())
	mailUserSetAutoRenew(freeContext)
	if freeRecorder.Code != http.StatusOK {
		t.Fatalf("unlocked status = %d body=%s", freeRecorder.Code, freeRecorder.Body.String())
	}
	if decoded := mailTestDecode(t, freeRecorder); decoded["autoRenew"] != true {
		t.Fatalf("unlocked body = %v", decoded)
	}

	var autoRenew bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT auto_renew FROM mail_subscriptions WHERE id = $1`, fixture.subscriptionID).Scan(&autoRenew); err != nil {
		t.Fatalf("read auto_renew: %v", err)
	}
	if !autoRenew {
		t.Fatalf("auto_renew was not persisted")
	}
}

// TestMailPurchaseRejectsInsufficientBalance keeps the money path honest without a
// DB: the message format is part of the API contract.
func TestMailInsufficientBalanceMessage(t *testing.T) {
	if got := mailInsufficientBalanceMessage(8.88, 0); got != "余额不足，需要 ¥8.88，当前余额 ¥0.00" {
		t.Fatalf("message = %q", got)
	}
	if math.Abs(mailRoundCurrency(1.005)-1) > 1e-9 {
		t.Fatalf("mailRoundCurrency must mirror Number(x.toFixed(2)): %v", mailRoundCurrency(1.005))
	}
}

// TestMailUserID guards the claims lookup used by every write path.
func TestMailUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context1, _ := gin.CreateTestContext(recorder)
	context1.Set(middleware.ContextUserKey, mailTestUser())
	if got := mailUserID(context1); got != 2 {
		t.Fatalf("mailUserID = %d", got)
	}

	empty, _ := gin.CreateTestContext(recorder)
	if got := mailUserID(empty); got != 0 {
		t.Fatalf("mailUserID without claims = %d", got)
	}
}

// mailTestCreatePlanOnly creates a source + plan without a subscription, so the
// purchase route starts from "no existing subscription on this source".
func mailTestCreatePlanOnly(t *testing.T, ctx context.Context) (int32, int32) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	source, err := db.MailCreateSource(ctx, db.MailCreateSourceInput{
		Name:           "Mail purchase source " + suffix,
		Code:           "mp" + suffix[len(suffix)-8:],
		APIURL:         "https://cranemail.guard.invalid",
		APIKey:         "guard-key",
		SmarterMailURL: "https://smartermail.guard.invalid",
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	plan, err := db.MailCreatePlan(ctx, db.MailCreatePlanInput{
		SourceID:     source.ID,
		Name:         "Purchase plan " + suffix,
		DomainLimit:  2,
		DiskLimitGB:  5,
		BillingCycle: models.MailBillingCycleMonthly,
		Price:        9.99,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_plans WHERE id = $1`, plan.ID)
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_sources WHERE id = $1`, source.ID)
	})
	return source.ID, plan.ID
}

// TestMailPurchaseDebitsOnceUnderConcurrency is the money-path test for the mail
// module: two simultaneous purchases on the same source must produce exactly one
// subscription and exactly one debit, because both the subscription lock (4109) and
// the balance lock (4110) are taken inside the same transaction as the balance
// update and the BalanceLog insert.
func TestMailPurchaseDebitsOnceUnderConcurrency(t *testing.T) {
	mailTestSetup(t)
	ctx := context.Background()
	userID := mailTestUserID(t, ctx, "parityuser")
	sourceID, planID := mailTestCreatePlanOnly(t, ctx)
	_ = sourceID

	var originalBalance string
	if err := db.Pool().QueryRow(ctx, `SELECT balance::text FROM users WHERE id = $1`, userID).
		Scan(&originalBalance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	var lastBalanceLogID int64
	if err := db.Pool().QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM balance_logs WHERE user_id = $1`, userID).
		Scan(&lastBalanceLogID); err != nil {
		t.Fatalf("read balance_log max id: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE users SET balance = 1000 WHERE id = $1`, userID); err != nil {
		t.Fatalf("fund user: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM mail_subscriptions WHERE user_id = $1 AND source_id = $2`, userID, sourceID)
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM balance_logs WHERE user_id = $1 AND id > $2`, userID, lastBalanceLogID)
		_, _ = db.Pool().Exec(cleanupCtx, `UPDATE users SET balance = $1::numeric WHERE id = $2`, originalBalance, userID)
	})

	body := mailTestBody(t, map[string]any{"planId": planID})
	statuses := make(chan int, 2)
	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	for index := 0; index < 2; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			recorder := httptest.NewRecorder()
			context1, _ := gin.CreateTestContext(recorder)
			request := httptest.NewRequest(http.MethodPost, "/api/mail/subscription", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			context1.Request = request
			context1.Set(middleware.ContextUserKey, mailTestUser())
			<-start
			mailUserPurchaseSubscription(context1)
			statuses <- recorder.Code
		}()
	}
	close(start)
	waitGroup.Wait()
	close(statuses)

	okCount, rejectedCount := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			okCount++
		case http.StatusBadRequest:
			rejectedCount++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if okCount != 1 || rejectedCount != 1 {
		t.Fatalf("ok=%d rejected=%d, want exactly one of each", okCount, rejectedCount)
	}

	var balance string
	if err := db.Pool().QueryRow(ctx, `SELECT balance::text FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != "990.01" {
		t.Fatalf("balance = %s, want a single 9.99 debit (990.01)", balance)
	}

	var subscriptionCount int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_subscriptions WHERE user_id = $1 AND source_id = $2`, userID, sourceID).
		Scan(&subscriptionCount); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subscriptionCount != 1 {
		t.Fatalf("subscription count = %d, want 1", subscriptionCount)
	}

	var debitCount int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM balance_logs
		  WHERE user_id = $1 AND id > $2 AND type = 'consume' AND remark LIKE '购买域名邮箱 - %'`,
		userID, lastBalanceLogID).Scan(&debitCount); err != nil {
		t.Fatalf("count balance logs: %v", err)
	}
	if debitCount != 1 {
		t.Fatalf("balance log count = %d, want exactly one debit", debitCount)
	}
}
