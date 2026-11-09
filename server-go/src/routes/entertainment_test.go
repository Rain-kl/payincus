package routes

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func entertainmentTestParseJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return decoded
}

func mustRouteDecimal(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

func TestEntertainmentParseRouteID(t *testing.T) {
	valid := map[string]int32{"1": 1, "42": 42, "2147483647": math.MaxInt32}
	for raw, want := range valid {
		got, ok := entertainmentParseRouteID(raw)
		if !ok || got != want {
			t.Errorf("entertainmentParseRouteID(%q) = (%d,%v), want (%d,true)", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "01", "-1", "1.5", "abc", " 1", "2147483648", "99999999999999999999"} {
		if _, ok := entertainmentParseRouteID(raw); ok {
			t.Errorf("entertainmentParseRouteID(%q) accepted", raw)
		}
	}
}

func TestEntertainmentParseBodyID(t *testing.T) {
	if got, ok := entertainmentParseBodyID(float64(7)); !ok || got != 7 {
		t.Errorf("body id 7 = (%d,%v)", got, ok)
	}
	for _, value := range []any{nil, "1", float64(0), float64(-1), float64(1.5), float64(math.MaxInt32) + 1} {
		if _, ok := entertainmentParseBodyID(value); ok {
			t.Errorf("entertainmentParseBodyID(%v) accepted", value)
		}
	}
}

func TestEntertainmentParsePointsLogType(t *testing.T) {
	if _, present, valid := entertainmentParsePointsLogType(""); present || !valid {
		t.Errorf("empty type = (present=%v, valid=%v), want absent and valid", present, valid)
	}
	for _, value := range []string{
		"convert", "lottery_win", "lottery_spend", "admin_adjust", "checkin",
		"badge_draw_spend", "badge_select_spend", "invite_generate", "vip_benefit",
	} {
		parsed, present, valid := entertainmentParsePointsLogType(value)
		if !present || !valid || string(parsed) != value {
			t.Errorf("type %q = (%q,%v,%v)", value, parsed, present, valid)
		}
	}
	if _, present, valid := entertainmentParsePointsLogType("nope"); present || valid {
		t.Errorf("invalid type accepted")
	}
}

func TestEntertainmentParseLotteryPrizeType(t *testing.T) {
	if _, present, valid := entertainmentParseLotteryPrizeType(""); present || !valid {
		t.Errorf("empty prize type = (present=%v, valid=%v)", present, valid)
	}
	for _, value := range []string{
		"nothing", "points", "balance", "badge", "instance", "cpu", "memory", "disk", "traffic",
	} {
		parsed, present, valid := entertainmentParseLotteryPrizeType(value)
		if !present || !valid || string(parsed) != value {
			t.Errorf("prize type %q = (%q,%v,%v)", value, parsed, present, valid)
		}
	}
	if _, present, valid := entertainmentParseLotteryPrizeType("gift"); present || valid {
		t.Errorf("invalid prize type accepted")
	}
}

func TestEntertainmentISOTime(t *testing.T) {
	value := time.Date(2027, 1, 2, 3, 4, 5, 678_999_000, time.UTC)
	if got := entertainmentISOTime(value); got != "2027-01-02T03:04:05.678Z" {
		t.Errorf("entertainmentISOTime = %q", got)
	}
	if got := entertainmentOptionalISOTime(nil); got != nil {
		t.Errorf("nil optional time = %v", got)
	}
}

func TestEntertainmentLotteryJSONShape(t *testing.T) {
	startAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	remain := int32(0)
	lottery := db.LotteryWithPrizes{
		Lottery: models.Lottery{
			ID:          3,
			Name:        "周年庆",
			Description: nil,
			CostPoints:  100,
			IsActive:    true,
			StartAt:     &startAt,
			EndAt:       nil,
		},
		Prizes: []models.LotteryPrize{{
			ID:             9,
			Name:           "谢谢参与",
			Type:           models.LotteryPrizeTypeNothing,
			Probability:    models.Decimal{Decimal: mustRouteDecimal(t, "30.00")},
			RemainQuantity: &remain,
			DisplayOrder:   1,
		}},
		TotalDraws: 7,
	}

	payload := entertainmentLotteryJSON(lottery)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := entertainmentTestParseJSON(t, encoded)

	if decoded["id"] != float64(3) || decoded["costPoints"] != float64(100) || decoded["totalDraws"] != float64(7) {
		t.Errorf("scalar fields = %v/%v/%v", decoded["id"], decoded["costPoints"], decoded["totalDraws"])
	}
	if decoded["description"] != nil {
		t.Errorf("description = %v, want null", decoded["description"])
	}
	if decoded["startAt"] != "2026-01-01T00:00:00.000Z" {
		t.Errorf("startAt = %v", decoded["startAt"])
	}
	if value, present := decoded["endAt"]; !present || value != nil {
		t.Errorf("endAt = %v (present=%v), want null", value, present)
	}
	prizes, ok := decoded["prizes"].([]any)
	if !ok || len(prizes) != 1 {
		t.Fatalf("prizes = %v", decoded["prizes"])
	}
	prize := prizes[0].(map[string]any)
	// Number(Decimal('30.00')) is the JSON number 30.
	if prize["probability"] != float64(30) {
		t.Errorf("probability = %v, want 30", prize["probability"])
	}
	if prize["remainQuantity"] != float64(0) || prize["totalQuantity"] != nil {
		t.Errorf("quantities = %v/%v", prize["remainQuantity"], prize["totalQuantity"])
	}
	for _, key := range []string{"id", "name", "type", "probability", "remainQuantity", "totalQuantity", "displayOrder", "instanceDesc"} {
		if _, present := prize[key]; !present {
			t.Errorf("prize missing key %q", key)
		}
	}
}

func TestEntertainmentPrizeJSONProbabilityIsNumber(t *testing.T) {
	encoded, err := json.Marshal(entertainmentPrizeJSON(models.LotteryPrize{
		Probability: models.Decimal{Decimal: mustRouteDecimal(t, "12.50")},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := entertainmentTestParseJSON(t, encoded)
	if decoded["probability"] != 12.5 {
		t.Errorf("probability = %v, want the number 12.5", decoded["probability"])
	}
}

func TestEntertainmentDrawRecordJSONKeys(t *testing.T) {
	record := models.LotteryRecord{
		ID:          1,
		PrizeID:     2,
		PrizeType:   models.LotteryPrizeTypePoints,
		PrizeName:   "100积分",
		PrizeValue:  100,
		Status:      models.LotteryRecordStatusDelivered,
		PointsSpent: 50,
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	encoded, err := json.Marshal(entertainmentDrawRecordJSON(record, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := entertainmentTestParseJSON(t, encoded)
	expected := []string{"id", "prizeId", "prizeType", "prizeName", "prizeValue", "badgeOwnership", "status", "pointsSpent", "createdAt"}
	if len(decoded) != len(expected) {
		t.Errorf("record has %d keys, want %d: %v", len(decoded), len(expected), decoded)
	}
	for _, key := range expected {
		if _, present := decoded[key]; !present {
			t.Errorf("record missing key %q", key)
		}
	}
	if decoded["badgeOwnership"] != nil {
		t.Errorf("badgeOwnership = %v, want null", decoded["badgeOwnership"])
	}
	if decoded["createdAt"] != "2026-01-01T00:00:00.000Z" {
		t.Errorf("createdAt = %v", decoded["createdAt"])
	}
}

func TestEntertainmentBadgeMessages(t *testing.T) {
	cases := []struct {
		prizeType models.LotteryPrizeType
		name      string
		want      string
	}{
		{models.LotteryPrizeTypeNothing, "x", "再接再厉！"},
		{models.LotteryPrizeTypeInstance, "x", "恭喜中奖！请提交工单领取您的实例奖励。"},
		{models.LotteryPrizeTypeBadge, "精英", "恭喜获得徽章！精英"},
		{models.LotteryPrizeTypePoints, "100积分", "恭喜中奖！100积分"},
	}
	for _, tc := range cases {
		if got := entertainmentBadgeMessage(tc.prizeType, tc.name); got != tc.want {
			t.Errorf("message(%s) = %q, want %q", tc.prizeType, got, tc.want)
		}
	}
}

func TestEntertainmentSendBadgeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/known", func(c *gin.Context) {
		entertainmentSendBadgeError(c, errors.New("BADGE_NOT_FOUND"))
	})
	router.GET("/conflict", func(c *gin.Context) {
		entertainmentSendBadgeError(c, db.BadgesErrStateConflict)
	})
	router.GET("/unknown", func(c *gin.Context) {
		entertainmentSendBadgeError(c, errors.New("SOMETHING_ELSE"))
	})

	cases := []struct {
		path        string
		wantError   string
		wantMessage string
	}{
		{"/known", "BADGE_NOT_FOUND", "徽章不存在"},
		{"/conflict", "BADGE_STATE_CONFLICT", "徽章状态已变化，请刷新后重试"},
		{"/unknown", "SOMETHING_ELSE", "徽章操作失败"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", tc.path, recorder.Code)
		}
		decoded := entertainmentTestParseJSON(t, recorder.Body.Bytes())
		if decoded["error"] != tc.wantError || decoded["message"] != tc.wantMessage {
			t.Errorf("%s body = %v, want error=%q message=%q", tc.path, decoded, tc.wantError, tc.wantMessage)
		}
	}
}

func TestEntertainmentRespondUnexpected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/boom", func(c *gin.Context) {
		entertainmentRespondUnexpected(c, errors.New("boom"))
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := entertainmentTestParseJSON(t, recorder.Body.Bytes())["error"]; got != "boom" {
		t.Errorf("development error = %v, want boom", got)
	}

	t.Setenv("NODE_ENV", "production")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if got := entertainmentTestParseJSON(t, recorder.Body.Bytes())["error"]; got != "Internal server error" {
		t.Errorf("production error = %v, want Internal server error", got)
	}
}

func TestEntertainmentRetryableDrawErrors(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"P2034", true},
		{"deadlock detected", true},
		{"write conflict", true},
		{"could not serialize access", true},
		{"40001", true},
		{"积分不足", false},
	}
	for _, tc := range cases {
		if got := entertainmentIsRetryableDrawError(errors.New(tc.message)); got != tc.want {
			t.Errorf("retryable(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
	if entertainmentIsRetryableDrawError(nil) {
		t.Error("nil must not be retryable")
	}
}

func TestEntertainmentIntString(t *testing.T) {
	cases := map[int64]string{0: "0", 7: "7", -3: "-3", 1234567890: "1234567890"}
	for value, want := range cases {
		if got := entertainmentIntString(value); got != want {
			t.Errorf("entertainmentIntString(%d) = %q, want %q", value, got, want)
		}
	}
}

// ==================== AJV body validation ====================

func entertainmentTestValidationRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	selectFields := []ajvFieldSpec{
		{Name: "badgeId", Kind: ajvKindString, MinLength: 1, MaxLength: 64, Minimum: math.NaN(), Maximum: math.NaN()},
	}
	router.POST("/select", func(c *gin.Context) {
		body, ok := entertainmentReadValidatedBody(c, []string{"badgeId"}, selectFields)
		if !ok {
			return
		}
		raw, _ := body.stringValue("badgeId")
		c.JSON(http.StatusOK, gin.H{"value": strings.TrimSpace(raw)})
	})

	ownershipFields := []ajvFieldSpec{
		{Name: "ownershipId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
	}
	router.POST("/avatar", func(c *gin.Context) {
		body, ok := entertainmentReadValidatedBody(c, []string{"ownershipId"}, ownershipFields)
		if !ok {
			return
		}
		converted, valid := entertainmentParseBodyID(body.Values["ownershipId"])
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_OWNERSHIP_ID", "message": "Invalid ownershipId"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"value": converted})
	})

	instanceFields := []ajvFieldSpec{
		{Name: "ownershipId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		{Name: "instanceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
	}
	router.POST("/instance", func(c *gin.Context) {
		body, ok := entertainmentReadValidatedBody(c, []string{"ownershipId", "instanceId"}, instanceFields)
		if !ok {
			return
		}
		ownershipID, ownershipOK := entertainmentParseBodyID(body.Values["ownershipId"])
		instanceID, instanceOK := entertainmentParseBodyID(body.Values["instanceId"])
		if !ownershipOK || !instanceOK {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_PARAMS", "message": "Invalid ownershipId or instanceId"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ownershipId": ownershipID, "instanceId": instanceID})
	})
	return router
}

func TestEntertainmentBodyValidation(t *testing.T) {
	router := entertainmentTestValidationRouter()

	cases := []struct {
		name        string
		path        string
		body        string
		contentType string
		wantStatus  int
		wantError   string
		// validation is true when the response is the AJV envelope
		// {error, code:'VALIDATION_ERROR'}; body-parser errors carry no code.
		validation bool
	}{
		{"missing badgeId", "/select", `{}`, "application/json", 400, "badgeId is required", true},
		{"empty badgeId", "/select", `{"badgeId":""}`, "application/json", 400, "badgeId is too short", true},
		{"badgeId too long", "/select", `{"badgeId":"` + strings.Repeat("a", 65) + `"}`, "application/json", 400, "badgeId is too long", true},
		{"badgeId object", "/select", `{"badgeId":{}}`, "application/json", 400, "must be string", true},
		{"badgeId numeric coercion", "/select", `{"badgeId":123}`, "application/json", 200, "", false},
		{"non-object body", "/select", `[]`, "application/json", 400, "must be object", true},
		{"string body", "/select", `"x"`, "application/json", 400, "must be object", true},
		{"null body", "/select", `null`, "application/json", 400, "must be object", true},
		{"missing ownershipId", "/avatar", `{}`, "application/json", 400, "ownershipId is required", true},
		{"ownershipId zero", "/avatar", `{"ownershipId":0}`, "application/json", 400, "must be >= 1", true},
		{"ownershipId negative", "/avatar", `{"ownershipId":-2}`, "application/json", 400, "must be >= 1", true},
		{"ownershipId fractional", "/avatar", `{"ownershipId":1.5}`, "application/json", 400, "must be integer", true},
		{"ownershipId text", "/avatar", `{"ownershipId":"abc"}`, "application/json", 400, "must be integer", true},
		{"ownershipId string coercion", "/avatar", `{"ownershipId":"3"}`, "application/json", 200, "", false},
		// AJV coerces null to 0 for an integer property, then minimum fails.
		{"ownershipId null", "/avatar", `{"ownershipId":null}`, "application/json", 400, "must be >= 1", true},
		{"missing instanceId", "/instance", `{"ownershipId":1}`, "application/json", 400, "instanceId is required", true},
		{"missing content type with body", "/select", `{"badgeId":"x"}`, "", 415, "Unsupported Media Type: undefined", false},
		{"unsupported content type", "/select", `{"badgeId":"x"}`, "application/xml", 415, "Unsupported Media Type: application/xml", false},
		// text/plain is a supported parser; the parsed string then fails the object schema.
		{"text plain body", "/select", `{"badgeId":"x"}`, "text/plain", 400, "must be object", true},
		{"empty JSON body", "/select", ``, "application/json", 400, "Body cannot be empty when content-type is set to 'application/json'", false},
		{"invalid JSON body", "/select", `not json`, "application/json", 400, "Body is not valid JSON but content-type is set to 'application/json'", false},
	}

	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		if tc.contentType != "" {
			request.Header.Set("Content-Type", tc.contentType)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		if recorder.Code != tc.wantStatus {
			t.Errorf("%s: status = %d (%s), want %d", tc.name, recorder.Code, recorder.Body.String(), tc.wantStatus)
			continue
		}
		if tc.wantError == "" {
			continue
		}
		decoded := entertainmentTestParseJSON(t, recorder.Body.Bytes())
		if decoded["error"] != tc.wantError {
			t.Errorf("%s: error = %v, want %q (body %s)", tc.name, decoded["error"], tc.wantError, recorder.Body.String())
		}
		if tc.validation {
			if decoded["code"] != "VALIDATION_ERROR" {
				t.Errorf("%s: code = %v, want VALIDATION_ERROR", tc.name, decoded["code"])
			}
		} else if _, present := decoded["code"]; present {
			t.Errorf("%s: body-parser errors must not carry a code: %v", tc.name, decoded)
		}
	}
}

func TestEntertainmentBodyValidationSuccessPayloads(t *testing.T) {
	router := entertainmentTestValidationRouter()

	request := httptest.NewRequest(http.MethodPost, "/select", strings.NewReader(`{"badgeId":123}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if got := entertainmentTestParseJSON(t, recorder.Body.Bytes())["value"]; got != "123" {
		t.Errorf("numeric badgeId coerced to %v, want \"123\"", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/instance", strings.NewReader(`{"ownershipId":"3","instanceId":4}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	decoded := entertainmentTestParseJSON(t, recorder.Body.Bytes())
	if decoded["ownershipId"] != float64(3) || decoded["instanceId"] != float64(4) {
		t.Errorf("coerced ids = %v/%v", decoded["ownershipId"], decoded["instanceId"])
	}
}

// ==================== registration ====================

func TestEntertainmentRegistersAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterEntertainment(router)

	expected := map[string]bool{
		"GET /api/entertainment/points":                    true,
		"POST /api/entertainment/points/convert":           true,
		"GET /api/entertainment/points/logs":               true,
		"GET /api/entertainment/lotteries":                 true,
		"GET /api/entertainment/lotteries/:id":             true,
		"POST /api/entertainment/lotteries/:id/draw":       true,
		"POST /api/entertainment/lotteries/:id/draw-multi": true,
		"GET /api/entertainment/lottery-records":           true,
		"GET /api/entertainment/badges/catalog":            true,
		"GET /api/entertainment/badges/overview":           true,
		"POST /api/entertainment/badges/draw/random":       true,
		"POST /api/entertainment/badges/draw/random-multi": true,
		"POST /api/entertainment/badges/draw/select":       true,
		"POST /api/entertainment/badges/apply/avatar":      true,
		"POST /api/entertainment/badges/apply/instance":    true,
		"POST /api/entertainment/badges/unapply":           true,
	}

	seen := map[string]bool{}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if !expected[key] {
			t.Errorf("unexpected route %s", key)
		}
		seen[key] = true
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("registered %d routes, want %d", len(seen), len(expected))
	}
}
