package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// adminEntertainmentTestContext builds a Gin context whose request carries the
// given JSON body ("" means no body at all).
func adminEntertainmentTestContext(t *testing.T, method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	context.Request = request
	return context, recorder
}

// adminEntertainmentTestJSON decodes the recorder body.
func adminEntertainmentTestJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// ==================== pure helpers ====================

func TestAdminEntertainmentValidateLotteryConfig(t *testing.T) {
	points := func(value int64, probability float64) adminEntertainmentPrizeConfig {
		return adminEntertainmentPrizeConfig{Type: models.LotteryPrizeTypePoints, Value: value, Probability: probability}
	}
	nothing := func(probability float64) adminEntertainmentPrizeConfig {
		return adminEntertainmentPrizeConfig{Type: models.LotteryPrizeTypeNothing, Probability: probability}
	}
	balance := func(value int64, probability float64) adminEntertainmentPrizeConfig {
		return adminEntertainmentPrizeConfig{Type: models.LotteryPrizeTypeBalance, Value: value, Probability: probability}
	}

	cases := []struct {
		name    string
		cost    int64
		prizes  []adminEntertainmentPrizeConfig
		wantErr string
	}{
		{"total above 100", 10, []adminEntertainmentPrizeConfig{points(1, 60), points(1, 50)}, "INVALID_PROBABILITY_TOTAL"},
		{"below 100 without nothing", 10, []adminEntertainmentPrizeConfig{points(1, 50)}, "MISSING_NOTHING_PRIZE"},
		{"below 100 with nothing", 10, []adminEntertainmentPrizeConfig{points(1, 50), nothing(20)}, ""},
		{"exactly 100 without nothing", 10, []adminEntertainmentPrizeConfig{points(1, 100)}, ""},
		// (probability/100) * (value/100) = 0.5 * 0.1 = 0.05 > cost 1 / 100 = 0.01
		{"balance expected value exceeded", 1, []adminEntertainmentPrizeConfig{balance(10, 50), nothing(50)}, "BALANCE_EXPECTED_VALUE_EXCEEDED"},
		// expected = (0.1/100) * (2000000/100) = 20 <= 100 (draw cost value), but the
		// single prize value 2000000 > cost 10000 * 100 -> exceeded
		{"balance value exceeded", 10000, []adminEntertainmentPrizeConfig{balance(2_000_000, 0.1), nothing(99.9)}, "BALANCE_PRIZE_VALUE_EXCEEDED"},
		{"balance within limits", 100, []adminEntertainmentPrizeConfig{balance(50, 100)}, ""},
	}

	for _, tc := range cases {
		err := adminEntertainmentValidateLotteryConfig(tc.cost, tc.prizes)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || err.Error != tc.wantErr {
			t.Errorf("%s: error = %v, want %s", tc.name, err, tc.wantErr)
		}
	}
}

func TestAdminEntertainmentParseJSDate(t *testing.T) {
	parsed, ok := adminEntertainmentParseJSDate("2026-03-04T05:06:07.890Z")
	if !ok || !parsed.Equal(time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)) {
		t.Errorf("ISO parse = %v/%v", parsed, ok)
	}
	if _, ok := adminEntertainmentParseJSDate("nope"); ok {
		t.Errorf("invalid string accepted")
	}
	if _, ok := adminEntertainmentParseJSDate(float64(2_000)); !ok {
		t.Errorf("epoch millis rejected")
	}
	if _, ok := adminEntertainmentParseJSDate([]any{}); ok {
		t.Errorf("array accepted")
	}
}

func TestAdminEntertainmentBodyIntHelpers(t *testing.T) {
	integer := adminEntertainmentParseIntegerBody(float64(42))
	if !integer.Parsed || !integer.Valid || integer.Value != 42 || integer.Overflow {
		t.Errorf("parseIntegerBody(42) = %+v", integer)
	}
	if invalid := adminEntertainmentParseIntegerBody("42"); invalid.Valid {
		t.Errorf("parseIntegerBody(\"42\") = %+v, want invalid", invalid)
	}
	if overflow := adminEntertainmentParseIntegerBody(float64(3_000_000_000)); !overflow.Overflow {
		t.Errorf("parseIntegerBody(3e9) = %+v, want overflow", overflow)
	}

	if absent := adminEntertainmentParseOptionalPositiveBodyID(nil); absent.Parsed {
		t.Errorf("parseOptionalPositiveBodyID(nil) = %+v, want absent", absent)
	}
	if zero := adminEntertainmentParseOptionalPositiveBodyID(float64(0)); !zero.Parsed || zero.Valid {
		t.Errorf("parseOptionalPositiveBodyID(0) = %+v, want invalid", zero)
	}
	if ok := adminEntertainmentParseOptionalPositiveBodyID(float64(3)); !ok.Valid || ok.Value != 3 {
		t.Errorf("parseOptionalPositiveBodyID(3) = %+v", ok)
	}

	if negative := adminEntertainmentParseOptionalNonNegativeBodyInt(float64(-1)); negative.Valid {
		t.Errorf("parseOptionalNonNegativeBodyInt(-1) = %+v", negative)
	}
	if zero := adminEntertainmentParseOptionalNonNegativeBodyInt(float64(0)); !zero.Valid {
		t.Errorf("parseOptionalNonNegativeBodyInt(0) = %+v", zero)
	}
}

func TestAdminEntertainmentOptionalFiniteNumber(t *testing.T) {
	if _, present, valid := adminEntertainmentOptionalFiniteNumber(nil); present || !valid {
		t.Errorf("absent value = %v/%v", present, valid)
	}
	if value, present, valid := adminEntertainmentOptionalFiniteNumber(float64(12.5)); !present || !valid || value != 12.5 {
		t.Errorf("number = %v/%v/%v", value, present, valid)
	}
	if _, present, valid := adminEntertainmentOptionalFiniteNumber("12.5"); !present || valid {
		t.Errorf("string should be null: present=%v valid=%v", present, valid)
	}
}

func TestAdminEntertainmentAssetAndCatalogValidation(t *testing.T) {
	if !adminEntertainmentIsValidCatalogID("abc_DEF-123") {
		t.Errorf("valid catalogue id rejected")
	}
	if adminEntertainmentIsValidCatalogID("bad space") || adminEntertainmentIsValidCatalogID("") {
		t.Errorf("invalid catalogue id accepted")
	}
	if !adminEntertainmentIsValidAssetURL("/badges/dark/x.svg") {
		t.Errorf("local path rejected")
	}
	if adminEntertainmentIsValidAssetURL("//evil.example/x.svg") {
		t.Errorf("protocol-relative URL accepted")
	}
	if !adminEntertainmentIsValidAssetURL("https://cdn.example/x.svg") {
		t.Errorf("https URL rejected")
	}
	if adminEntertainmentIsValidAssetURL("ftp://example/x.svg") {
		t.Errorf("ftp URL accepted")
	}
}

func TestAdminEntertainmentPaginationValue(t *testing.T) {
	cases := []struct {
		key      string
		query    string
		fallback int64
		max      int64
		want     int64
	}{
		{"page", "", 1, 100000, 1},
		{"page", "?page=0", 1, 100000, 1},
		{"page", "?page=01", 1, 100000, 1},
		{"page", "?page=-2", 1, 100000, 1},
		{"page", "?page=abc", 1, 100000, 1},
		{"page", "?page=2.5", 1, 100000, 1},
		{"page", "?page=7", 1, 100000, 7},
		{"page", "?page=500000", 1, 100000, 100000},
		{"pageSize", "?pageSize=500", 20, 100, 100},
		{"pageSize", "?pageSize=1", 20, 100, 1},
		{"pageSize", "", 20, 100, 20},
	}
	for _, tc := range cases {
		context, _ := adminEntertainmentTestContext(t, http.MethodGet, "/x"+tc.query, "")
		if got := adminEntertainmentParsePaginationValue(context, tc.key, tc.fallback, tc.max); got != tc.want {
			t.Errorf("%s %q = %d, want %d", tc.key, tc.query, got, tc.want)
		}
	}

	// The pageSize key is separate from page; verify it explicitly.
	context, _ := adminEntertainmentTestContext(t, http.MethodGet, "/x?page=3&pageSize=5", "")
	if got := adminEntertainmentParsePaginationValue(context, "page", 1, 100000); got != 3 {
		t.Errorf("page = %d, want 3", got)
	}
	if got := adminEntertainmentParsePaginationValue(context, "pageSize", 20, 100); got != 5 {
		t.Errorf("pageSize = %d, want 5", got)
	}
}

// ==================== handler validation (no database access) ====================

func TestAdminEntertainmentLotteryHandlersRejectInvalidIDs(t *testing.T) {
	cases := []struct {
		name    string
		handler gin.HandlerFunc
		body    string
		method  string
		want    string
	}{
		{"update", adminEntertainmentUpdateLotteryHandler, "{}", http.MethodPut, "Invalid lottery ID"},
		{"delete", adminEntertainmentDeleteLotteryHandler, "", http.MethodDelete, "Invalid lottery ID"},
		{"detail", adminEntertainmentGetLotteryHandler, "", http.MethodGet, "Invalid lottery ID"},
		{"prize create", adminEntertainmentCreatePrizeHandler, "{}", http.MethodPost, "Invalid lottery ID"},
		{"notification", adminEntertainmentUpdateNotificationHandler, "{}", http.MethodPut, "Invalid lottery ID"},
		{"prize update", adminEntertainmentUpdatePrizeHandler, "{}", http.MethodPut, "Invalid prize ID"},
		{"prize delete", adminEntertainmentDeletePrizeHandler, "", http.MethodDelete, "Invalid prize ID"},
		{"deliver", adminEntertainmentDeliverRecordHandler, "{}", http.MethodPost, "Invalid record ID"},
		{"user points", adminEntertainmentGetUserPointsHandler, "", http.MethodGet, "Invalid user ID"},
		{"user points logs", adminEntertainmentListPointsLogsHandler, "", http.MethodGet, "Invalid user ID"},
		{"adjust", adminEntertainmentAdjustPointsHandler, "{}", http.MethodPost, "Invalid user ID"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, tc.method, "/x", tc.body)
		context.Params = gin.Params{{Key: "id", Value: "0"}, {Key: "userId", Value: "0"}}
		tc.handler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != "INVALID_ID" || decoded["message"] != tc.want {
			t.Errorf("%s: payload = %v", tc.name, decoded)
		}
	}
}

func TestAdminEntertainmentCreateLotteryValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing name", `{}`, "NAME_REQUIRED"},
		{"blank name", `{"name":"   "}`, "NAME_REQUIRED"},
		{"numeric name", `{"name":7,"costPoints":10}`, "NAME_REQUIRED"},
		{"missing cost", `{"name":"Spring"}`, "INVALID_COST"},
		{"zero cost", `{"name":"Spring","costPoints":0}`, "INVALID_COST"},
		{"fractional cost", `{"name":"Spring","costPoints":1.5}`, "INVALID_COST"},
		{"string cost", `{"name":"Spring","costPoints":"10"}`, "INVALID_COST"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPost, "/x", tc.body)
		adminEntertainmentCreateLotteryHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
		if _, present := decoded["message"]; !present {
			t.Errorf("%s: missing message field: %v", tc.name, decoded)
		}
	}
}

func TestAdminEntertainmentListRecordsRejectsInvalidFilters(t *testing.T) {
	cases := []struct {
		query string
		code  string
	}{
		{"?lotteryId=0", "INVALID_ID"},
		{"?lotteryId=abc", "INVALID_ID"},
		{"?prizeType=gift", "INVALID_PRIZE_TYPE"},
		{"?status=nope", "INVALID_STATUS"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodGet, "/x"+tc.query, "")
		adminEntertainmentListRecordsHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", tc.query, recorder.Code)
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%q: code = %v, want %s", tc.query, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentListUserPointsRejectsInvalidSort(t *testing.T) {
	cases := []struct {
		query string
		code  string
	}{
		{"?orderBy=nope", "INVALID_ORDER_BY"},
		{"?order=sideways", "INVALID_ORDER"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodGet, "/x"+tc.query, "")
		adminEntertainmentListUserPointsHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", tc.query, recorder.Code)
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%q: code = %v, want %s", tc.query, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentAdjustPointsValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing amount", `{"remark":"ok"}`, "INVALID_AMOUNT"},
		{"zero amount", `{"amount":0,"remark":"ok"}`, "INVALID_AMOUNT"},
		{"fractional amount", `{"amount":1.5,"remark":"ok"}`, "INVALID_AMOUNT"},
		{"amount above limit", `{"amount":1000001,"remark":"ok"}`, "INVALID_AMOUNT"},
		{"missing remark", `{"amount":5}`, "REMARK_REQUIRED"},
		{"blank remark", `{"amount":5,"remark":"  "}`, "REMARK_REQUIRED"},
		{"numeric remark", `{"amount":5,"remark":3}`, "REMARK_REQUIRED"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPost, "/x", tc.body)
		context.Params = gin.Params{{Key: "userId", Value: "9"}}
		adminEntertainmentAdjustPointsHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentCheckinSettingsValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"enabled not boolean", `{"enabled":"true"}`, "INVALID_ENABLED"},
		{"requireInstance not boolean", `{"requireInstance":1}`, "INVALID_REQUIRE_INSTANCE"},
		{"minPoints zero", `{"minPoints":0}`, "INVALID_MIN_POINTS"},
		{"minPoints fractional", `{"minPoints":1.5}`, "INVALID_MIN_POINTS"},
		{"maxPoints negative", `{"maxPoints":-1}`, "INVALID_MAX_POINTS"},
		{"range inverted", `{"minPoints":10,"maxPoints":5}`, "INVALID_POINT_RANGE"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPut, "/x", tc.body)
		adminEntertainmentUpdateCheckinSettingsHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentBadgeSeriesValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing id", `{"title":"t","nameZh":"z","description":"d"}`, "INVALID_SERIES_ID"},
		{"bad id", `{"id":"bad id","title":"t","nameZh":"z","description":"d"}`, "INVALID_SERIES_ID"},
		{"missing title", `{"id":"ok","nameZh":"z","description":"d"}`, "INVALID_SERIES_DATA"},
		{"blank nameZh", `{"id":"ok","title":"t","nameZh":"  ","description":"d"}`, "INVALID_SERIES_DATA"},
		{"missing description", `{"id":"ok","title":"t","nameZh":"z"}`, "INVALID_SERIES_DATA"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPost, "/x", tc.body)
		adminEntertainmentCreateBadgeSeriesHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentUpdateBadgeSeriesValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"blank title", `{"title":"  "}`},
		{"blank nameZh", `{"nameZh":""}`},
		{"blank description", `{"description":"   "}`},
		{"non-string title", `{"title":3}`},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPut, "/x", tc.body)
		context.Params = gin.Params{{Key: "id", Value: "series-1"}}
		adminEntertainmentUpdateBadgeSeriesHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != "INVALID_SERIES_DATA" {
			t.Errorf("%s: payload = %v", tc.name, decoded)
		}
	}
}

func TestAdminEntertainmentBadgeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"missing id", `{"name":"n","fullLabel":"f","seriesId":"s","assetUrl":"/a.svg"}`, "INVALID_BADGE_ID"},
		{"bad id", `{"id":"bad id","name":"n","fullLabel":"f","seriesId":"s","assetUrl":"/a.svg"}`, "INVALID_BADGE_ID"},
		{"missing name", `{"id":"ok","fullLabel":"f","seriesId":"s","assetUrl":"/a.svg"}`, "INVALID_BADGE_DATA"},
		{"missing series", `{"id":"ok","name":"n","fullLabel":"f","assetUrl":"/a.svg"}`, "INVALID_BADGE_DATA"},
		{"missing asset", `{"id":"ok","name":"n","fullLabel":"f","seriesId":"s"}`, "INVALID_BADGE_DATA"},
		{"bad asset", `{"id":"ok","name":"n","fullLabel":"f","seriesId":"s","assetUrl":"ftp://x/a.svg"}`, "INVALID_ASSET_URL"},
		{"bad dark asset", `{"id":"ok","name":"n","fullLabel":"f","seriesId":"s","assetUrl":"/a.svg","assetUrlDark":"//x/a.svg"}`, "INVALID_ASSET_URL"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPost, "/x", tc.body)
		adminEntertainmentCreateBadgeHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentUpdateBadgeValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty asset", `{"assetUrl":"  "}`, "INVALID_ASSET_URL"},
		{"bad asset", `{"assetUrl":"ftp://x/a.svg"}`, "INVALID_ASSET_URL"},
		{"blank name", `{"name":"   "}`, "INVALID_BADGE_DATA"},
		{"blank label", `{"fullLabel":""}`, "INVALID_BADGE_DATA"},
		{"blank series", `{"seriesId":" "}`, "INVALID_BADGE_DATA"},
	}
	for _, tc := range cases {
		context, recorder := adminEntertainmentTestContext(t, http.MethodPut, "/x", tc.body)
		context.Params = gin.Params{{Key: "id", Value: "badge-1"}}
		adminEntertainmentUpdateBadgeHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminEntertainmentTestJSON(t, recorder)
		if decoded["error"] != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.name, decoded["error"], tc.code)
		}
	}
}

func TestAdminEntertainmentDetailJSONShape(t *testing.T) {
	created := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	probability, err := adminPromoCodesDecimalFromNumber(25)
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}
	lottery := db.LotteryWithPrizes{
		Lottery: models.Lottery{
			ID:         4,
			Name:       "Summer",
			CostPoints: 100,
			IsActive:   true,
			CreatedAt:  created,
		},
		Prizes: []models.LotteryPrize{{
			ID:          8,
			LotteryID:   4,
			Name:        "100 points",
			Type:        models.LotteryPrizeTypePoints,
			Value:       100,
			Probability: probability,
		}},
	}
	stats := db.AdminLotteryStats{
		TotalDraws: 3,
		PrizeStats: []db.AdminLotteryPrizeStat{{Type: models.LotteryPrizeTypePoints, Count: 3}},
	}

	encoded, err := json.Marshal(adminEntertainmentLotteryDetailJSON(lottery, nil, stats))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["notificationConfig"] != nil {
		t.Errorf("notificationConfig = %v, want null", decoded["notificationConfig"])
	}
	if decoded["createdAt"] != "2026-07-08T09:10:11.000Z" {
		t.Errorf("createdAt = %v", decoded["createdAt"])
	}
	if decoded["totalDraws"] != float64(3) {
		t.Errorf("totalDraws = %v", decoded["totalDraws"])
	}
	prizes := decoded["prizes"].([]any)
	prize := prizes[0].(map[string]any)
	if prize["probability"] != float64(25) {
		t.Errorf("probability = %v (want the JS number)", prize["probability"])
	}
	statPayload := decoded["stats"].(map[string]any)
	if statPayload["totalDraws"] != float64(3) {
		t.Errorf("stats.totalDraws = %v", statPayload["totalDraws"])
	}
	prizeStats := statPayload["prizeStats"].([]any)
	first := prizeStats[0].(map[string]any)
	if first["type"] != "points" || first["count"] != float64(3) {
		t.Errorf("prizeStats[0] = %v", first)
	}
}

// ==================== registration ====================

func TestAdminEntertainmentRegistersAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAdminEntertainment(router)

	prefix := "/api/admin/entertainment"
	expected := map[string]bool{
		"GET " + prefix + "/lotteries":                    true,
		"POST " + prefix + "/lotteries":                   true,
		"PUT " + prefix + "/lotteries/:id":                true,
		"DELETE " + prefix + "/lotteries/:id":             true,
		"GET " + prefix + "/lotteries/:id":                true,
		"POST " + prefix + "/lotteries/:id/prizes":        true,
		"PUT " + prefix + "/prizes/:id":                   true,
		"DELETE " + prefix + "/prizes/:id":                true,
		"PUT " + prefix + "/lotteries/:id/notification":   true,
		"GET " + prefix + "/lottery-records":              true,
		"POST " + prefix + "/lottery-records/:id/deliver": true,
		"GET " + prefix + "/user-points":                  true,
		"POST " + prefix + "/user-points/:userId/adjust":  true,
		"GET " + prefix + "/user-points/:userId":          true,
		"GET " + prefix + "/user-points/:userId/logs":     true,
		"GET " + prefix + "/checkin/settings":             true,
		"PUT " + prefix + "/checkin/settings":             true,
		"GET " + prefix + "/checkin/logs":                 true,
		"GET " + prefix + "/badges/catalog":               true,
		"POST " + prefix + "/badges/series":               true,
		"PUT " + prefix + "/badges/series/:id":            true,
		"DELETE " + prefix + "/badges/series/:id":         true,
		"POST " + prefix + "/badges":                      true,
		"PUT " + prefix + "/badges/:id":                   true,
		"DELETE " + prefix + "/badges/:id":                true,
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
