// Port of server/src/routes/admin-entertainment.ts (23 routes).
//
// app.ts registers the module once:
//
//	await fastify.register(adminEntertainmentRoutes, { prefix: '/api/admin/entertainment' })
//
// Every route carries `onRequest: [fastify.authenticate, fastify.requireAdmin]`
// (no plugin-level hooks), so the Go group uses middleware.AuthenticateAdmin.
// `prefixTrailingSlash` is not configured anywhere in server/src, so Fastify's
// default keeps routes exactly as written — the module has no '/' route, hence
// nothing to mount twice.
//
// Parity notes:
//   - No route declares a JSON schema, so bodies are raw decoded JSON and every
//     handler reproduces JavaScript coercion (typeof/Number/Boolean/?? /||) rather
//     than relying on Go zero values.
//   - Error payloads are the TypeScript's hand-written
//     `{ error: <CODE>, message: <text> }` objects — never the apiError envelope.
//     An unhandled throw (bad Date, Prisma type error) is `{ error: ... }` with a
//     500, like app.ts' global handler.
//   - Prisma `undefined` vs `null` matters for the update paths: several fields are
//     skipped when absent but written as NULL when explicitly null. The request
//     structs below carry Set flags for that.
//   - PUT /prizes/:id rejects an explicit `totalQuantity: null` /
//     `remainQuantity: null` with INVALID_QUANTITY even though the message says
//     "or null": that is the TypeScript's actual branch order and is preserved.
//   - `updateLottery` cannot clear description/name (the TS uses `x?.trim()`, which
//     is undefined for null); the Go port skips those columns too.
//   - Relation reads without an explicit orderBy (Prisma heap order) are pinned:
//     prizes by display_order then id, notification configs by lottery_id. The
//     stats groupBy is ordered by the prize-type enum so repeated calls are stable.
//   - Users' points are read through the already-ported points layer
//     (db.LotteryGetUserPoints / LotteryGetConvertibleConsume /
//     LotteryGetPointsLogs); the admin-only CRUD lives in db/admin_lottery.go.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Constants mirror admin-entertainment.ts.
const (
	adminEntertainmentMaxPointsAdjustAmount         = 1_000_000
	adminEntertainmentLotteryTotalProbability       = 100.0
	adminEntertainmentMaxBalancePrizeCostMultiplier = 100.0
)

// adminEntertainmentCatalogIDPattern mirrors `/^[a-zA-Z0-9_-]{1,64}$/`.
var adminEntertainmentCatalogIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// adminEntertainmentRecordStatuses mirrors LOTTERY_RECORD_STATUSES.
var adminEntertainmentRecordStatuses = map[models.LotteryRecordStatus]bool{
	models.LotteryRecordStatusPending:   true,
	models.LotteryRecordStatusDelivered: true,
	models.LotteryRecordStatusClaimed:   true,
}

// adminEntertainmentNotificationTypes mirrors NOTIFICATION_TYPES.
var adminEntertainmentNotificationTypes = map[string]bool{
	"telegram": true,
	"discord":  true,
	"webhook":  true,
}

// adminEntertainmentUserPointsOrderBy mirrors USER_POINTS_ORDER_BY_FIELDS.
var adminEntertainmentUserPointsOrderBy = map[string]bool{
	"points":      true,
	"totalEarned": true,
	"totalSpent":  true,
}

// adminEntertainmentSortOrders mirrors SORT_ORDERS.
var adminEntertainmentSortOrders = map[string]bool{"asc": true, "desc": true}

// ==================== helpers ====================

// adminEntertainmentRespondError mirrors `reply.code(status).send({ error, message })`.
func adminEntertainmentRespondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": code, "message": message})
}

// adminEntertainmentRespondInternal mirrors app.ts' global error handler for an
// unhandled throw. The public text never carries internals.
func adminEntertainmentRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// adminEntertainmentReadBody reproduces `request.body` access. ok=false means the
// response was already written: a missing or null body is `undefined`/`null` in the
// TypeScript, whose destructuring throws (500). Every other non-object body simply
// yields undefined properties, so an empty map is returned.
func adminEntertainmentReadBody(c *gin.Context) (map[string]any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	if decoded.Value == nil {
		adminEntertainmentRespondInternal(c)
		return nil, false
	}
	if object, ok := decoded.Value.(map[string]any); ok {
		return object, true
	}
	return map[string]any{}, true
}

// adminEntertainmentParsePaginationValue mirrors
// `parsePositiveIntegerQuery(value, fallback, max)`: the value must match
// ^[1-9]\d*$ (so "0", "-1", "01", "1.5", "abc" and repeated keys all fall back),
// then it is clamped to max.
func adminEntertainmentParsePaginationValue(c *gin.Context, key string, fallback, max int64) int64 {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present || isArray {
		return fallback
	}
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return fallback
	}
	if parsed > max {
		return max
	}
	return parsed
}

// adminEntertainmentQueryText mirrors `query.<key>?.trim()` when the caller needs
// the TypeError path. Repeated keys produce an array with no .trim, so ok=false
// means the TypeScript would throw.
func adminEntertainmentQueryText(c *gin.Context, key string) (string, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present {
		return "", true
	}
	if isArray {
		return "", false
	}
	return promoCodesJSTrim(value), true
}

// adminEntertainmentQueryOptionalText mirrors `query.<key>?.trim()` where the
// TypeScript tolerates a non-string (it checks typeof before trimming).
func adminEntertainmentQueryOptionalText(c *gin.Context, key string) (string, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present || isArray {
		return "", false
	}
	return promoCodesJSTrim(value), true
}

// adminEntertainmentParseRouteID mirrors parsePositiveRouteId narrowed to INT4.
func adminEntertainmentParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// adminEntertainmentParseOptionalRouteID mirrors parseOptionalPositiveId:
// absent/empty -> (0, false, true); invalid -> (0, false, false).
func adminEntertainmentParseOptionalRouteID(c *gin.Context, key string) (int32, bool, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present || value == "" {
		return 0, false, true
	}
	if isArray {
		return 0, false, false
	}
	parsed, ok := adminEntertainmentParseRouteID(value)
	if !ok {
		return 0, false, false
	}
	return parsed, true, true
}

// adminEntertainmentBodyInt is the result of the TS body integer helpers.
//
//	Parsed=false -> the field was absent (undefined)
//	Valid=false  -> the value was not a safe integer (the helper returns null)
//	Overflow     -> a safe integer the INT4 column cannot hold (Prisma throws)
type adminEntertainmentBodyInt struct {
	Parsed   bool
	Valid    bool
	Value    int64
	Overflow bool
}

func adminEntertainmentNewBodyInt(parsed, valid bool, value int64) adminEntertainmentBodyInt {
	return adminEntertainmentBodyInt{
		Parsed:   parsed,
		Valid:    valid,
		Value:    value,
		Overflow: valid && (value > math.MaxInt32 || value < math.MinInt32),
	}
}

// adminEntertainmentParseIntegerBody mirrors parseIntegerBody.
func adminEntertainmentParseIntegerBody(value any) adminEntertainmentBodyInt {
	number, ok := value.(float64)
	if !ok || !promoCodesIsSafeInteger(number) {
		return adminEntertainmentBodyInt{Parsed: true, Valid: false}
	}
	return adminEntertainmentNewBodyInt(true, true, int64(number))
}

// adminEntertainmentParseOptionalPositiveBodyID mirrors parseOptionalPositiveBodyId.
func adminEntertainmentParseOptionalPositiveBodyID(value any) adminEntertainmentBodyInt {
	if value == nil {
		return adminEntertainmentBodyInt{}
	}
	number, ok := value.(float64)
	if !ok || !promoCodesIsSafeInteger(number) || number <= 0 {
		return adminEntertainmentBodyInt{Parsed: true, Valid: false}
	}
	return adminEntertainmentNewBodyInt(true, true, int64(number))
}

// adminEntertainmentParseOptionalNonNegativeBodyInt mirrors
// parseOptionalNonNegativeIntegerBody.
func adminEntertainmentParseOptionalNonNegativeBodyInt(value any) adminEntertainmentBodyInt {
	if value == nil {
		return adminEntertainmentBodyInt{}
	}
	number, ok := value.(float64)
	if !ok || !promoCodesIsSafeInteger(number) || number < 0 {
		return adminEntertainmentBodyInt{Parsed: true, Valid: false}
	}
	return adminEntertainmentNewBodyInt(true, true, int64(number))
}

// adminEntertainmentOptionalFiniteNumber mirrors parseOptionalFiniteNumber.
// present=false means undefined; valid=false means null (a non-number value).
func adminEntertainmentOptionalFiniteNumber(value any) (number float64, present, valid bool) {
	if value == nil {
		return 0, false, true
	}
	parsed, ok := value.(float64)
	if !ok || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, true, false
	}
	return parsed, true, true
}

// adminEntertainmentParsePrizeType mirrors parseLotteryPrizeType.
func adminEntertainmentParsePrizeType(value any) (models.LotteryPrizeType, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	prizeType := models.LotteryPrizeType(text)
	if !entertainmentLotteryPrizeTypes[prizeType] {
		return "", false
	}
	return prizeType, true
}

// adminEntertainmentParseOptionalPrizeType mirrors parseOptionalLotteryPrizeType.
func adminEntertainmentParseOptionalPrizeType(c *gin.Context, key string) (models.LotteryPrizeType, bool, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present || value == "" {
		return "", false, true
	}
	if isArray {
		return "", false, false
	}
	prizeType := models.LotteryPrizeType(value)
	if !entertainmentLotteryPrizeTypes[prizeType] {
		return "", false, false
	}
	return prizeType, true, true
}

// adminEntertainmentParseOptionalRecordStatus mirrors parseOptionalLotteryRecordStatus.
func adminEntertainmentParseOptionalRecordStatus(c *gin.Context, key string) (models.LotteryRecordStatus, bool, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present || value == "" {
		return "", false, true
	}
	if isArray {
		return "", false, false
	}
	status := models.LotteryRecordStatus(value)
	if !adminEntertainmentRecordStatuses[status] {
		return "", false, false
	}
	return status, true, true
}

// adminEntertainmentRequireText mirrors requireText.
func adminEntertainmentRequireText(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := promoCodesJSTrim(text)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// adminEntertainmentNormalizeOptionalText mirrors normalizeOptionalText:
// undefined stays undefined (skipped), null stays null (cleared), a non-string is
// treated as undefined.
func adminEntertainmentNormalizeOptionalText(value any) *string {
	if text, ok := value.(string); ok {
		trimmed := promoCodesJSTrim(text)
		if trimmed == "" {
			return nil
		}
		return &trimmed
	}
	return nil
}

// adminEntertainmentOptionalTextSet reports whether the field should be written:
// a string (possibly empty) or null yes, undefined or a non-string no.
func adminEntertainmentOptionalTextSet(value any, present bool) bool {
	if !present {
		return false
	}
	if value == nil {
		return true
	}
	_, isString := value.(string)
	return isString
}

// adminEntertainmentIsValidCatalogID mirrors isValidCatalogId.
func adminEntertainmentIsValidCatalogID(value string) bool {
	return adminEntertainmentCatalogIDPattern.MatchString(value)
}

// adminEntertainmentIsValidAssetURL mirrors isValidAssetUrl.
func adminEntertainmentIsValidAssetURL(url string) bool {
	if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") {
		return true
	}
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
}

// adminEntertainmentParseJSDate mirrors `new Date(value)` for the strings and
// numbers the lottery routes accept. An unparseable value yields ok=false, which
// the TypeScript turns into a Prisma validation error (500).
func adminEntertainmentParseJSDate(value any) (*time.Time, bool) {
	switch typed := value.(type) {
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05",
			"2006-01-02T15:04",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, typed); err == nil {
				utc := parsed.UTC().Truncate(time.Millisecond)
				return &utc, true
			}
		}
		return nil, false
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Abs(typed) > 8.64e15 {
			return nil, false
		}
		seconds := math.Trunc(typed / 1000)
		remainder := typed - seconds*1000
		moment := time.Unix(int64(seconds), int64(remainder*float64(time.Millisecond))).UTC()
		return &moment, true
	case bool:
		millis := 0.0
		if typed {
			millis = 1
		}
		moment := time.Unix(0, int64(millis*float64(time.Millisecond))).UTC()
		return &moment, true
	default:
		return nil, false
	}
}

// ==================== lottery config validation ====================

// adminEntertainmentPrizeConfig mirrors LotteryPrizeConfig.
type adminEntertainmentPrizeConfig struct {
	Type        models.LotteryPrizeType
	Value       int64
	Probability float64
}

// adminEntertainmentConfigError mirrors LotteryConfigValidationError.
type adminEntertainmentConfigError struct {
	Error   string
	Message string
}

// adminEntertainmentValidateLotteryConfig mirrors validateLotteryConfig, including
// its float64 arithmetic (the TS mixes ints and Numbers the same way).
func adminEntertainmentValidateLotteryConfig(costPoints int64, prizes []adminEntertainmentPrizeConfig) *adminEntertainmentConfigError {
	totalProbability := 0.0
	for _, prize := range prizes {
		totalProbability += prize.Probability
	}

	if totalProbability > adminEntertainmentLotteryTotalProbability {
		return &adminEntertainmentConfigError{
			Error:   "INVALID_PROBABILITY_TOTAL",
			Message: "Total prize probability must not exceed 100%",
		}
	}

	if totalProbability < adminEntertainmentLotteryTotalProbability {
		hasNothing := false
		for _, prize := range prizes {
			if prize.Type == models.LotteryPrizeTypeNothing {
				hasNothing = true
				break
			}
		}
		if !hasNothing {
			return &adminEntertainmentConfigError{
				Error:   "MISSING_NOTHING_PRIZE",
				Message: "A nothing prize is required when total prize probability is below 100%",
			}
		}
	}

	expectedBalanceValue := 0.0
	for _, prize := range prizes {
		if prize.Type != models.LotteryPrizeTypeBalance {
			continue
		}
		expectedBalanceValue += (prize.Probability / 100) * (float64(prize.Value) / 100)
	}
	drawCostValue := float64(costPoints) / 100
	if expectedBalanceValue > drawCostValue {
		return &adminEntertainmentConfigError{
			Error:   "BALANCE_EXPECTED_VALUE_EXCEEDED",
			Message: "Expected balance payout per draw must not exceed the point cost value",
		}
	}

	maxBalancePrizeValue := float64(costPoints) * adminEntertainmentMaxBalancePrizeCostMultiplier
	for _, prize := range prizes {
		if prize.Type == models.LotteryPrizeTypeBalance && float64(prize.Value) > maxBalancePrizeValue {
			return &adminEntertainmentConfigError{
				Error: "BALANCE_PRIZE_VALUE_EXCEEDED",
				Message: fmt.Sprintf("A balance prize must not exceed %d times the point cost value",
					int64(adminEntertainmentMaxBalancePrizeCostMultiplier)),
			}
		}
	}

	return nil
}

// adminEntertainmentLotteryPrizesConfig maps the stored prizes onto the validator's
// input shape.
func adminEntertainmentLotteryPrizesConfig(prizes []models.LotteryPrize) []adminEntertainmentPrizeConfig {
	configs := make([]adminEntertainmentPrizeConfig, 0, len(prizes))
	for _, prize := range prizes {
		configs = append(configs, adminEntertainmentPrizeConfig{
			Type:        prize.Type,
			Value:       int64(prize.Value),
			Probability: entertainmentDecimalFloat(prize.Probability),
		})
	}
	return configs
}

// ==================== response builders ====================

// adminEntertainmentPrizeJSON mirrors the prize object of the lottery list/detail.
func adminEntertainmentPrizeJSON(prize models.LotteryPrize) gin.H {
	return gin.H{
		"id":             prize.ID,
		"name":           prize.Name,
		"type":           prize.Type,
		"value":          prize.Value,
		"probability":    entertainmentDecimalFloat(prize.Probability),
		"totalQuantity":  prize.TotalQuantity,
		"remainQuantity": prize.RemainQuantity,
		"displayOrder":   prize.DisplayOrder,
		"instanceDesc":   prize.InstanceDesc,
	}
}

// adminEntertainmentPrizesJSON maps a prize slice.
func adminEntertainmentPrizesJSON(prizes []models.LotteryPrize) []gin.H {
	items := make([]gin.H, 0, len(prizes))
	for _, prize := range prizes {
		items = append(items, adminEntertainmentPrizeJSON(prize))
	}
	return items
}

// adminEntertainmentLotteryJSON mirrors the list object of GET /lotteries.
func adminEntertainmentLotteryJSON(lottery db.LotteryWithPrizes) gin.H {
	return gin.H{
		"id":          lottery.Lottery.ID,
		"name":        lottery.Lottery.Name,
		"description": lottery.Lottery.Description,
		"costPoints":  lottery.Lottery.CostPoints,
		"isActive":    lottery.Lottery.IsActive,
		"startAt":     entertainmentOptionalISOTime(lottery.Lottery.StartAt),
		"endAt":       entertainmentOptionalISOTime(lottery.Lottery.EndAt),
		"totalDraws":  lottery.TotalDraws,
		"prizesCount": len(lottery.Prizes),
		"createdAt":   entertainmentISOTime(lottery.Lottery.CreatedAt),
		"prizes":      adminEntertainmentPrizesJSON(lottery.Prizes),
	}
}

// adminEntertainmentLotteryDetailJSON mirrors the detail object of GET /lotteries/:id.
func adminEntertainmentLotteryDetailJSON(lottery db.LotteryWithPrizes, config *models.LotteryNotificationConfig, stats db.AdminLotteryStats) gin.H {
	var notificationConfig any
	if config != nil {
		notificationConfig = gin.H{
			"enabled":        config.Enabled,
			"type":           config.Type,
			"config":         config.Config,
			"notifyBalance":  config.NotifyBalance,
			"notifyInstance": config.NotifyInstance,
		}
	}

	return gin.H{
		"id":                 lottery.Lottery.ID,
		"name":               lottery.Lottery.Name,
		"description":        lottery.Lottery.Description,
		"costPoints":         lottery.Lottery.CostPoints,
		"isActive":           lottery.Lottery.IsActive,
		"startAt":            entertainmentOptionalISOTime(lottery.Lottery.StartAt),
		"endAt":              entertainmentOptionalISOTime(lottery.Lottery.EndAt),
		"totalDraws":         stats.TotalDraws,
		"createdAt":          entertainmentISOTime(lottery.Lottery.CreatedAt),
		"prizes":             adminEntertainmentPrizesJSON(lottery.Prizes),
		"notificationConfig": notificationConfig,
		"stats":              stats,
	}
}

// adminEntertainmentBadgeSeriesJSON mirrors the raw BadgeSeries row the create
// endpoint returns (Prisma dates serialize as ISO strings).
func adminEntertainmentBadgeSeriesJSON(series models.BadgeSeries) gin.H {
	return gin.H{
		"id":           series.ID,
		"title":        series.Title,
		"nameZh":       series.NameZh,
		"nameEn":       series.NameEn,
		"description":  series.Description,
		"sourceId":     series.SourceID,
		"sourceLabel":  series.SourceLabel,
		"displayOrder": series.DisplayOrder,
		"isActive":     series.IsActive,
		"createdAt":    entertainmentISOTime(series.CreatedAt),
		"updatedAt":    entertainmentISOTime(series.UpdatedAt),
	}
}

// adminEntertainmentBadgeJSON mirrors the raw Badge row the create endpoint returns.
func adminEntertainmentBadgeJSON(badge models.Badge) gin.H {
	return gin.H{
		"id":            badge.ID,
		"name":          badge.Name,
		"nameEn":        badge.NameEn,
		"fullLabel":     badge.FullLabel,
		"seriesId":      badge.SeriesID,
		"sourceId":      badge.SourceID,
		"sourceLabel":   badge.SourceLabel,
		"assetUrl":      badge.AssetURL,
		"assetUrlDark":  badge.AssetURLDark,
		"assetUrlLight": badge.AssetURLLight,
		"displayOrder":  badge.DisplayOrder,
		"isActive":      badge.IsActive,
		"createdAt":     entertainmentISOTime(badge.CreatedAt),
		"updatedAt":     entertainmentISOTime(badge.UpdatedAt),
	}
}

// ==================== shared data helpers ====================

// adminEntertainmentLoadLottery mirrors db.getLotteryById (prizes ordered by
// displayOrder, live draw count) plus the notification config include.
func adminEntertainmentLoadLottery(ctx context.Context, id int32) (*db.LotteryWithPrizes, *models.LotteryNotificationConfig, error) {
	lottery, err := db.LotteryGetByID(ctx, id)
	if err != nil || lottery == nil {
		return nil, nil, err
	}
	config, err := db.LotteryGetNotificationConfig(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return lottery, config, nil
}

// adminEntertainmentIsUniqueViolation mirrors Prisma P2002.
func adminEntertainmentIsUniqueViolation(err error) bool {
	return db.PromoIsUniqueViolation(err)
}

// adminEntertainmentIsForeignKeyViolation mirrors Prisma P2003.
func adminEntertainmentIsForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23503" {
		return true
	}
	return strings.Contains(adminEntertainmentErrString(err), "SQLSTATE 23503")
}

func adminEntertainmentErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// adminEntertainmentNotFound mirrors Prisma P2025 (pgx.ErrNoRows / no row scanned).
func adminEntertainmentNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// adminEntertainmentInt32 narrows a safe integer to the INT4 column domain.
func adminEntertainmentInt32(value int64) (int32, bool) {
	if value > math.MaxInt32 || value < math.MinInt32 {
		return 0, false
	}
	return int32(value), true
}

// adminEntertainmentInt64Ptr widens a user id for db.CreateLog's *int64 argument.
func adminEntertainmentInt64Ptr(value int32) *int64 {
	widened := int64(value)
	return &widened
}

// adminEntertainmentHasKey mirrors `key in object` for a decoded JSON body.
func adminEntertainmentHasKey(body map[string]any, key string) bool {
	_, present := body[key]
	return present
}

// RegisterAdminEntertainment mounts /api/admin/entertainment.
func RegisterAdminEntertainment(r *gin.Engine) {
	group := r.Group("/api/admin/entertainment", middleware.AuthenticateAdmin())

	// ==================== 抽奖管理 ====================
	group.GET("/lotteries", adminEntertainmentListLotteriesHandler)
	group.POST("/lotteries", adminEntertainmentCreateLotteryHandler)
	group.PUT("/lotteries/:id", adminEntertainmentUpdateLotteryHandler)
	group.DELETE("/lotteries/:id", adminEntertainmentDeleteLotteryHandler)
	group.GET("/lotteries/:id", adminEntertainmentGetLotteryHandler)

	// ==================== 奖品管理 ====================
	group.POST("/lotteries/:id/prizes", adminEntertainmentCreatePrizeHandler)
	group.PUT("/prizes/:id", adminEntertainmentUpdatePrizeHandler)
	group.DELETE("/prizes/:id", adminEntertainmentDeletePrizeHandler)

	// ==================== 通知配置 ====================
	group.PUT("/lotteries/:id/notification", adminEntertainmentUpdateNotificationHandler)

	// ==================== 中奖记录 ====================
	group.GET("/lottery-records", adminEntertainmentListRecordsHandler)
	group.POST("/lottery-records/:id/deliver", adminEntertainmentDeliverRecordHandler)

	// ==================== 用户积分管理 ====================
	group.GET("/user-points", adminEntertainmentListUserPointsHandler)
	group.POST("/user-points/:userId/adjust", adminEntertainmentAdjustPointsHandler)
	group.GET("/user-points/:userId", adminEntertainmentGetUserPointsHandler)
	group.GET("/user-points/:userId/logs", adminEntertainmentListPointsLogsHandler)

	// ==================== 每日签到管理 ====================
	group.GET("/checkin/settings", adminEntertainmentGetCheckinSettingsHandler)
	group.PUT("/checkin/settings", adminEntertainmentUpdateCheckinSettingsHandler)
	group.GET("/checkin/logs", adminEntertainmentListCheckinLogsHandler)

	// ==================== 徽章目录管理 ====================
	group.GET("/badges/catalog", adminEntertainmentGetBadgeCatalogHandler)
	group.POST("/badges/series", adminEntertainmentCreateBadgeSeriesHandler)
	group.PUT("/badges/series/:id", adminEntertainmentUpdateBadgeSeriesHandler)
	group.DELETE("/badges/series/:id", adminEntertainmentDeleteBadgeSeriesHandler)
	group.POST("/badges", adminEntertainmentCreateBadgeHandler)
	group.PUT("/badges/:id", adminEntertainmentUpdateBadgeHandler)
	group.DELETE("/badges/:id", adminEntertainmentDeleteBadgeHandler)
}

func init() {
	Register(RegisterAdminEntertainment)
}

// ==================== handlers ====================

// adminEntertainmentListLotteriesHandler mirrors GET /lotteries.
func adminEntertainmentListLotteriesHandler(c *gin.Context) {
	ctx := c.Request.Context()

	page := adminEntertainmentParsePaginationValue(c, "page", 1, 100000)
	pageSize := adminEntertainmentParsePaginationValue(c, "pageSize", 20, 100)

	var isActive *bool
	if value, present, isArray := promoCodesQueryValue(c, "isActive"); present && !isArray {
		if value == "true" {
			enabled := true
			isActive = &enabled
		} else if value == "false" {
			disabled := false
			isActive = &disabled
		}
	}

	result, err := db.AdminLotteryGetAll(ctx, db.AdminLotteryListOptions{
		Page: page, PageSize: pageSize, IsActive: isActive,
	})
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	lotteries := make([]gin.H, 0, len(result.Lotteries))
	for _, lottery := range result.Lotteries {
		lotteries = append(lotteries, adminEntertainmentLotteryJSON(lottery))
	}

	c.JSON(http.StatusOK, gin.H{
		"lotteries": lotteries,
		"total":     result.Total,
		"page":      result.Page,
		"pageSize":  result.PageSize,
	})
}

// adminEntertainmentCreateLotteryHandler mirrors POST /lotteries.
func adminEntertainmentCreateLotteryHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	rawName, namePresent := body["name"]
	name, nameIsString := rawName.(string)
	if !namePresent || name == "" || !nameIsString {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "NAME_REQUIRED", "Lottery name is required")
		return
	}
	trimmedName := promoCodesJSTrim(name)
	if trimmedName == "" {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "NAME_REQUIRED", "Lottery name is required")
		return
	}

	rawCost, costPresent := body["costPoints"]
	costPoints := adminEntertainmentBodyInt{Parsed: true}
	if costPresent {
		costPoints = adminEntertainmentParseIntegerBody(rawCost)
	}
	if !costPoints.Valid || costPoints.Value < 1 {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_COST", "Cost points must be at least 1")
		return
	}
	costValue, fits := adminEntertainmentInt32(costPoints.Value)
	if !fits {
		adminEntertainmentRespondInternal(c)
		return
	}

	var description *string
	if raw, present := body["description"]; present && raw != nil {
		text, isString := raw.(string)
		if !isString {
			adminEntertainmentRespondInternal(c)
			return
		}
		trimmed := promoCodesJSTrim(text)
		description = &trimmed
	}

	var isActive *bool
	if raw, present := body["isActive"]; present && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondInternal(c)
			return
		}
		isActive = &flag
	}

	var startAt, endAt *time.Time
	if raw, present := body["startAt"]; present && promoCodesTruthy(raw) {
		parsed, parsedOK := adminEntertainmentParseJSDate(raw)
		if !parsedOK {
			adminEntertainmentRespondInternal(c)
			return
		}
		startAt = parsed
	}
	if raw, present := body["endAt"]; present && promoCodesTruthy(raw) {
		parsed, parsedOK := adminEntertainmentParseJSDate(raw)
		if !parsedOK {
			adminEntertainmentRespondInternal(c)
			return
		}
		endAt = parsed
	}

	lottery, err := db.AdminLotteryCreate(ctx, db.AdminLotteryInput{
		Name:        trimmedName,
		Description: description,
		CostPoints:  costValue,
		IsActive:    isActive,
		StartAt:     startAt,
		EndAt:       endAt,
		CreatedBy:   user.ID,
	})
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.create",
		fmt.Sprintf("Created lottery \"%s\" (ID: %d)", lottery.Name, lottery.ID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"lottery": gin.H{"id": lottery.ID, "name": lottery.Name},
	})
}

// adminEntertainmentUpdateLotteryHandler mirrors PUT /lotteries/:id.
func adminEntertainmentUpdateLotteryHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	lotteryID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}

	existing, _, err := adminEntertainmentLoadLottery(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if existing == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	var costPoints db.AdminLotteryUpdateInput
	if raw, present := body["costPoints"]; present {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if !parsed.Valid || parsed.Value < 1 {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_COST", "Cost points must be at least 1")
			return
		}
		value, fits := adminEntertainmentInt32(parsed.Value)
		if !fits {
			adminEntertainmentRespondInternal(c)
			return
		}
		costPoints.CostPoints = &value
	}

	if len(existing.Prizes) > 0 {
		effectiveCost := int64(existing.Lottery.CostPoints)
		if costPoints.CostPoints != nil {
			effectiveCost = int64(*costPoints.CostPoints)
		}
		if configError := adminEntertainmentValidateLotteryConfig(effectiveCost,
			adminEntertainmentLotteryPrizesConfig(existing.Prizes)); configError != nil {
			adminEntertainmentRespondError(c, http.StatusBadRequest, configError.Error, configError.Message)
			return
		}
	}

	update := db.AdminLotteryUpdateInput{CostPoints: costPoints.CostPoints}
	if raw, present := body["name"]; present && raw != nil {
		text, isString := raw.(string)
		if !isString {
			adminEntertainmentRespondInternal(c)
			return
		}
		trimmed := promoCodesJSTrim(text)
		update.Name = &trimmed
	}
	if raw, present := body["description"]; present && raw != nil {
		text, isString := raw.(string)
		if !isString {
			adminEntertainmentRespondInternal(c)
			return
		}
		trimmed := promoCodesJSTrim(text)
		update.Description = &trimmed
	}
	if raw, present := body["isActive"]; present {
		flag, isBool := raw.(bool)
		if !isBool {
			// `isActive: null` (or any non-boolean) reaches Prisma, which rejects it.
			adminEntertainmentRespondInternal(c)
			return
		}
		update.IsActive = &flag
	}
	// `startAt === null ? null : startAt ? new Date(startAt) : undefined`
	if raw, present := body["startAt"]; present {
		if raw == nil {
			update.StartAtSet = true
			update.StartAt = nil
		} else if promoCodesTruthy(raw) {
			parsed, parsedOK := adminEntertainmentParseJSDate(raw)
			if !parsedOK {
				adminEntertainmentRespondInternal(c)
				return
			}
			update.StartAtSet = true
			update.StartAt = parsed
		}
	}
	if raw, present := body["endAt"]; present {
		if raw == nil {
			update.EndAtSet = true
			update.EndAt = nil
		} else if promoCodesTruthy(raw) {
			parsed, parsedOK := adminEntertainmentParseJSDate(raw)
			if !parsedOK {
				adminEntertainmentRespondInternal(c)
				return
			}
			update.EndAtSet = true
			update.EndAt = parsed
		}
	}

	if _, err := db.AdminLotteryUpdate(ctx, lotteryID, update); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.update",
		fmt.Sprintf("Updated lottery \"%s\" (ID: %d)", existing.Lottery.Name, lotteryID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentDeleteLotteryHandler mirrors DELETE /lotteries/:id.
func adminEntertainmentDeleteLotteryHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	lotteryID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}

	existing, _, err := adminEntertainmentLoadLottery(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if existing == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	if _, err := db.AdminLotteryDelete(ctx, lotteryID); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.delete",
		fmt.Sprintf("Deleted lottery \"%s\" (ID: %d)", existing.Lottery.Name, lotteryID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentGetLotteryHandler mirrors GET /lotteries/:id.
func adminEntertainmentGetLotteryHandler(c *gin.Context) {
	ctx := c.Request.Context()

	lotteryID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}

	lottery, config, err := adminEntertainmentLoadLottery(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if lottery == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	stats, err := db.AdminLotteryGetStats(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"lottery": adminEntertainmentLotteryDetailJSON(*lottery, config, stats),
	})
}

// adminEntertainmentCreatePrizeHandler mirrors POST /lotteries/:id/prizes.
func adminEntertainmentCreatePrizeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	lotteryID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}

	lottery, _, err := adminEntertainmentLoadLottery(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if lottery == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	rawName, namePresent := body["name"]
	name, nameIsString := rawName.(string)
	if !namePresent || name == "" || !nameIsString {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "NAME_REQUIRED", "Prize name is required")
		return
	}

	prizeType, typeOK := adminEntertainmentParsePrizeType(body["type"])
	if !typeOK {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_PRIZE_TYPE", "Invalid prize type")
		return
	}

	probability, probabilityPresent, probabilityValid := adminEntertainmentOptionalFiniteNumber(body["probability"])
	if !probabilityPresent || !probabilityValid || probability < 0 || probability > 100 {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_PROBABILITY", "Probability must be between 0 and 100")
		return
	}

	var quantity adminEntertainmentBodyInt
	if raw, present := body["totalQuantity"]; present {
		quantity = adminEntertainmentParseOptionalPositiveBodyID(raw)
	}

	if (prizeType == models.LotteryPrizeTypePoints || prizeType == models.LotteryPrizeTypeNothing) &&
		quantity.Parsed && quantity.Valid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_QUANTITY",
			"Points and nothing prizes cannot have quantity limits")
		return
	}
	if quantity.Parsed && !quantity.Valid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_QUANTITY",
			"Quantity must be a positive integer")
		return
	}
	if quantity.Overflow {
		adminEntertainmentRespondInternal(c)
		return
	}

	var value adminEntertainmentBodyInt
	if raw, present := body["value"]; present {
		value = adminEntertainmentParseIntegerBody(raw)
	}
	if prizeType != models.LotteryPrizeTypeBadge && prizeType != models.LotteryPrizeTypeNothing &&
		prizeType != models.LotteryPrizeTypeInstance {
		if !value.Valid || value.Value < 0 {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_VALUE",
				"Prize value must be a non-negative integer")
			return
		}
		if value.Overflow {
			adminEntertainmentRespondInternal(c)
			return
		}
	}

	resolvedValue := int64(0)
	switch prizeType {
	case models.LotteryPrizeTypeBadge, models.LotteryPrizeTypeNothing, models.LotteryPrizeTypeInstance:
		resolvedValue = 0
	default:
		if value.Valid {
			resolvedValue = value.Value
		}
	}
	value32, fits := adminEntertainmentInt32(resolvedValue)
	if !fits {
		adminEntertainmentRespondInternal(c)
		return
	}

	candidatePrizes := append(adminEntertainmentLotteryPrizesConfig(lottery.Prizes),
		adminEntertainmentPrizeConfig{Type: prizeType, Value: resolvedValue, Probability: probability})
	if configError := adminEntertainmentValidateLotteryConfig(int64(lottery.Lottery.CostPoints), candidatePrizes); configError != nil {
		adminEntertainmentRespondError(c, http.StatusBadRequest, configError.Error, configError.Message)
		return
	}

	var totalQuantity *int32
	if quantity.Valid {
		parsed, ok := adminEntertainmentInt32(quantity.Value)
		if !ok {
			adminEntertainmentRespondInternal(c)
			return
		}
		totalQuantity = &parsed
	}

	displayOrderValue := int32(0)
	if raw, present := body["displayOrder"]; present && raw != nil {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if parsed.Valid {
			value32Display, fits := adminEntertainmentInt32(parsed.Value)
			if !fits {
				adminEntertainmentRespondInternal(c)
				return
			}
			displayOrderValue = value32Display
		}
	}

	var instanceDesc *string
	if raw, present := body["instanceDesc"]; present && raw != nil {
		text, isString := raw.(string)
		if !isString {
			adminEntertainmentRespondInternal(c)
			return
		}
		trimmed := promoCodesJSTrim(text)
		instanceDesc = &trimmed
	}

	probabilityDecimal, err := adminPromoCodesDecimalFromNumber(probability)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	prize, err := db.AdminLotteryCreatePrize(ctx, db.AdminLotteryPrizeInput{
		LotteryID:     lotteryID,
		Name:          promoCodesJSTrim(name),
		Type:          prizeType,
		Value:         value32,
		Probability:   probabilityDecimal,
		TotalQuantity: totalQuantity,
		DisplayOrder:  displayOrderValue,
		InstanceDesc:  instanceDesc,
	})
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.prize.create",
		fmt.Sprintf("Added prize \"%s\" to lottery \"%s\"", prize.Name, lottery.Lottery.Name), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"prize":   gin.H{"id": prize.ID, "name": prize.Name},
	})
}

// adminEntertainmentUpdatePrizeHandler mirrors PUT /prizes/:id.
func adminEntertainmentUpdatePrizeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	prizeID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid prize ID")
		return
	}

	existing, err := db.AdminLotteryGetPrizeByID(ctx, prizeID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if existing == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Prize not found")
		return
	}

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	var (
		parsedType      models.LotteryPrizeType
		typeParsed      bool
		typeValid       = true
		probability     float64
		probabilitySet  bool
		probabilityOK   = true
		value           adminEntertainmentBodyInt
		quantity        adminEntertainmentBodyInt
		remainQuantity  adminEntertainmentBodyInt
		quantitySet     bool
		remainSet       bool
		displayOrder    adminEntertainmentBodyInt
		displayOrderSet bool
	)

	if raw, present := body["type"]; present {
		typeParsed = true
		parsedType, typeValid = adminEntertainmentParsePrizeType(raw)
	}
	if raw, present := body["probability"]; present {
		probability, probabilitySet, probabilityOK = adminEntertainmentOptionalFiniteNumber(raw)
	}
	if raw, present := body["value"]; present {
		value = adminEntertainmentParseIntegerBody(raw)
	}
	if raw, present := body["totalQuantity"]; present {
		quantitySet = true
		if raw == nil {
			quantity = adminEntertainmentBodyInt{Parsed: true, Valid: false} // explicit null
		} else {
			quantity = adminEntertainmentParseOptionalPositiveBodyID(raw)
		}
	}
	if raw, present := body["remainQuantity"]; present {
		remainSet = true
		if raw == nil {
			remainQuantity = adminEntertainmentBodyInt{Parsed: true, Valid: false}
		} else {
			remainQuantity = adminEntertainmentParseOptionalNonNegativeBodyInt(raw)
		}
	}
	if raw, present := body["displayOrder"]; present {
		displayOrderSet = true
		displayOrder = adminEntertainmentParseIntegerBody(raw)
	}

	if typeParsed && !typeValid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_PRIZE_TYPE", "Invalid prize type")
		return
	}

	if !probabilityOK || (probabilitySet && (probability < 0 || probability > 100)) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_PROBABILITY", "Probability must be between 0 and 100")
		return
	}

	prizeType := existing.Type
	if typeParsed && typeValid {
		prizeType = parsedType
	}

	if (prizeType == models.LotteryPrizeTypePoints || prizeType == models.LotteryPrizeTypeNothing) &&
		quantitySet && quantity.Valid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_QUANTITY",
			"Points and nothing prizes cannot have quantity limits")
		return
	}

	if (quantitySet && !quantity.Valid) || (remainSet && !remainQuantity.Valid) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_QUANTITY",
			"Quantity must be a positive integer or null")
		return
	}
	if quantity.Overflow || remainQuantity.Overflow {
		adminEntertainmentRespondInternal(c)
		return
	}

	if value.Parsed && prizeType != models.LotteryPrizeTypeBadge && prizeType != models.LotteryPrizeTypeNothing {
		if !value.Valid || value.Value < 0 {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_VALUE",
				"Prize value must be a non-negative integer")
			return
		}
		if value.Overflow {
			adminEntertainmentRespondInternal(c)
			return
		}
	}
	if value.Parsed && (prizeType == models.LotteryPrizeTypeBadge || prizeType == models.LotteryPrizeTypeNothing) && value.Overflow {
		adminEntertainmentRespondInternal(c)
		return
	}

	if displayOrderSet && !displayOrder.Valid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_DISPLAY_ORDER",
			"Display order must be an integer")
		return
	}
	if displayOrderSet && displayOrder.Overflow {
		adminEntertainmentRespondInternal(c)
		return
	}

	lottery, _, err := adminEntertainmentLoadLottery(ctx, existing.LotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if lottery == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	configPrizes := make([]adminEntertainmentPrizeConfig, 0, len(lottery.Prizes))
	for _, prize := range lottery.Prizes {
		config := adminEntertainmentPrizeConfig{
			Type:        prize.Type,
			Value:       int64(prize.Value),
			Probability: entertainmentDecimalFloat(prize.Probability),
		}
		if prize.ID == prizeID {
			config.Type = prizeType
			if prizeType == models.LotteryPrizeTypeBadge || prizeType == models.LotteryPrizeTypeNothing {
				config.Value = 0
			} else if value.Valid {
				config.Value = value.Value
			}
			if probabilitySet && probabilityOK {
				config.Probability = probability
			}
		}
		configPrizes = append(configPrizes, config)
	}
	if configError := adminEntertainmentValidateLotteryConfig(int64(lottery.Lottery.CostPoints), configPrizes); configError != nil {
		adminEntertainmentRespondError(c, http.StatusBadRequest, configError.Error, configError.Message)
		return
	}

	update := db.AdminLotteryPrizeUpdate{}
	if raw, present := body["name"]; present && raw != nil {
		text, isString := raw.(string)
		if !isString {
			adminEntertainmentRespondInternal(c)
			return
		}
		trimmed := promoCodesJSTrim(text)
		update.Name = &trimmed
	}
	if typeParsed && typeValid {
		update.Type = &prizeType
	}
	if prizeType == models.LotteryPrizeTypeBadge || prizeType == models.LotteryPrizeTypeNothing {
		zero := int32(0)
		update.Value = &zero
	} else if value.Valid {
		parsed, ok := adminEntertainmentInt32(value.Value)
		if !ok {
			adminEntertainmentRespondInternal(c)
			return
		}
		update.Value = &parsed
	}
	if probabilitySet && probabilityOK {
		probabilityDecimal, err := adminPromoCodesDecimalFromNumber(probability)
		if err != nil {
			adminEntertainmentRespondInternal(c)
			return
		}
		update.Probability = &probabilityDecimal
	}
	if quantitySet {
		update.TotalQuantitySet = true
		if quantity.Valid {
			parsed, ok := adminEntertainmentInt32(quantity.Value)
			if !ok {
				adminEntertainmentRespondInternal(c)
				return
			}
			update.TotalQuantity = &parsed
		}
	}
	if remainSet {
		update.RemainQuantitySet = true
		if remainQuantity.Valid {
			parsed, ok := adminEntertainmentInt32(remainQuantity.Value)
			if !ok {
				adminEntertainmentRespondInternal(c)
				return
			}
			update.RemainQuantity = &parsed
		}
	}
	if displayOrderSet && displayOrder.Valid {
		parsed, ok := adminEntertainmentInt32(displayOrder.Value)
		if !ok {
			adminEntertainmentRespondInternal(c)
			return
		}
		update.DisplayOrder = &parsed
	}
	if raw, present := body["instanceDesc"]; present {
		update.InstanceDescSet = true
		if raw == nil {
			update.InstanceDesc = nil
		} else if text, isString := raw.(string); isString {
			update.InstanceDesc = &text
		} else {
			adminEntertainmentRespondInternal(c)
			return
		}
	}

	if _, err := db.AdminLotteryUpdatePrize(ctx, prizeID, update); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.prize.update",
		fmt.Sprintf("Updated prize \"%s\" (ID: %d)", existing.Name, prizeID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentDeletePrizeHandler mirrors DELETE /prizes/:id.
func adminEntertainmentDeletePrizeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	prizeID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid prize ID")
		return
	}

	existing, err := db.AdminLotteryGetPrizeByID(ctx, prizeID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if existing == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Prize not found")
		return
	}
	if existing.RecordsCount > 0 {
		adminEntertainmentRespondError(c, http.StatusConflict, "PRIZE_HAS_RECORDS",
			"Prize with lottery records cannot be deleted; set its probability to 0 to disable it")
		return
	}

	lottery, _, err := adminEntertainmentLoadLottery(ctx, existing.LotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if lottery == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	remaining := make([]adminEntertainmentPrizeConfig, 0, len(lottery.Prizes))
	for _, prize := range lottery.Prizes {
		if prize.ID == prizeID {
			continue
		}
		remaining = append(remaining, adminEntertainmentPrizeConfig{
			Type:        prize.Type,
			Value:       int64(prize.Value),
			Probability: entertainmentDecimalFloat(prize.Probability),
		})
	}
	if len(remaining) > 0 {
		if configError := adminEntertainmentValidateLotteryConfig(int64(lottery.Lottery.CostPoints), remaining); configError != nil {
			adminEntertainmentRespondError(c, http.StatusBadRequest, configError.Error, configError.Message)
			return
		}
	}

	if _, err := db.AdminLotteryDeletePrize(ctx, prizeID); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.prize.delete",
		fmt.Sprintf("Deleted prize \"%s\" (ID: %d)", existing.Name, prizeID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentUpdateNotificationHandler mirrors PUT /lotteries/:id/notification.
func adminEntertainmentUpdateNotificationHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	lotteryID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}

	lottery, _, err := adminEntertainmentLoadLottery(ctx, lotteryID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if lottery == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Lottery not found")
		return
	}

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	rawType, typePresent := body["type"]
	notificationType, typeIsString := rawType.(string)
	if !typePresent || !typeIsString || !adminEntertainmentNotificationTypes[notificationType] {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_TYPE", "Invalid notification type")
		return
	}

	rawEnabled, enabledPresent := body["enabled"]
	enabled, enabledIsBool := rawEnabled.(bool)
	if !enabledPresent || !enabledIsBool {
		adminEntertainmentRespondInternal(c)
		return
	}
	rawNotifyBalance, notifyBalancePresent := body["notifyBalance"]
	notifyBalance, notifyBalanceIsBool := rawNotifyBalance.(bool)
	if !notifyBalancePresent || !notifyBalanceIsBool {
		adminEntertainmentRespondInternal(c)
		return
	}
	rawNotifyInstance, notifyInstancePresent := body["notifyInstance"]
	notifyInstance, notifyInstanceIsBool := rawNotifyInstance.(bool)
	if !notifyInstancePresent || !notifyInstanceIsBool {
		adminEntertainmentRespondInternal(c)
		return
	}

	rawConfig, configPresent := body["config"]
	if !configPresent || rawConfig == nil {
		// Prisma rejects a missing/null required Json argument.
		adminEntertainmentRespondInternal(c)
		return
	}
	configJSON, err := json.Marshal(rawConfig)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	if _, err := db.AdminLotteryUpsertNotificationConfig(ctx, lotteryID, db.AdminLotteryNotificationConfigInput{
		Enabled:        enabled,
		Type:           notificationType,
		Config:         configJSON,
		NotifyBalance:  notifyBalance,
		NotifyInstance: notifyInstance,
	}); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	adminID := int64(user.ID)
	db.CreateLog(ctx, &adminID, "admin", "lottery.notification.update",
		fmt.Sprintf("Updated notification config for lottery \"%s\"", lottery.Lottery.Name), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentListRecordsHandler mirrors GET /lottery-records.
func adminEntertainmentListRecordsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	page := adminEntertainmentParsePaginationValue(c, "page", 1, 100000)
	pageSize := adminEntertainmentParsePaginationValue(c, "pageSize", 20, 100)

	filterLotteryID, lotteryIDSet, lotteryIDValid := adminEntertainmentParseOptionalRouteID(c, "lotteryId")
	if !lotteryIDValid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid lottery ID")
		return
	}
	filterPrizeType, prizeTypeSet, prizeTypeValid := adminEntertainmentParseOptionalPrizeType(c, "prizeType")
	if !prizeTypeValid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_PRIZE_TYPE", "Invalid prize type")
		return
	}
	filterStatus, statusSet, statusValid := adminEntertainmentParseOptionalRecordStatus(c, "status")
	if !statusValid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_STATUS", "Invalid record status")
		return
	}
	search, searchPresent := adminEntertainmentQueryOptionalText(c, "search")

	options := db.AdminLotteryRecordOptions{Page: page, PageSize: pageSize}
	if lotteryIDSet {
		id := filterLotteryID
		options.LotteryID = &id
	}
	if prizeTypeSet {
		options.PrizeType = filterPrizeType
	}
	if statusSet {
		options.Status = filterStatus
	}
	if searchPresent && search != "" {
		// The db layer applies trim + the 128 UTF-16 unit slice.
		options.Search = &search
	}

	result, err := db.AdminLotteryGetAllRecords(ctx, options)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	records := make([]gin.H, 0, len(result.Records))
	for _, row := range result.Records {
		var instanceDesc *string
		if row.InstanceDesc != nil && *row.InstanceDesc != "" {
			instanceDesc = row.InstanceDesc
		}

		var deliveredAt *string
		if row.Record.DeliveredAt != nil {
			formatted := entertainmentISOTime(*row.Record.DeliveredAt)
			deliveredAt = &formatted
		}

		records = append(records, gin.H{
			"id":           row.Record.ID,
			"lotteryId":    row.Record.LotteryID,
			"lotteryName":  row.LotteryName,
			"userId":       row.Record.UserID,
			"username":     row.Username,
			"userAvatar":   row.UserAvatar,
			"prizeType":    row.Record.PrizeType,
			"prizeName":    row.Record.PrizeName,
			"prizeValue":   row.Record.PrizeValue,
			"instanceDesc": instanceDesc,
			"status":       row.Record.Status,
			"pointsSpent":  row.Record.PointsSpent,
			"deliveredAt":  deliveredAt,
			"deliveredBy":  row.Record.DeliveredBy,
			"ticketId":     row.Record.TicketID,
			"createdAt":    entertainmentISOTime(row.Record.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// adminEntertainmentDeliverRecordHandler mirrors POST /lottery-records/:id/deliver.
func adminEntertainmentDeliverRecordHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	recordID, ok := adminEntertainmentParseRouteID(c.Param("id"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid record ID")
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	if decoded.Value == nil {
		// `request.body.ticketId` on an absent body throws.
		adminEntertainmentRespondInternal(c)
		return
	}
	body, isObject := decoded.Value.(map[string]any)
	if !isObject {
		body = map[string]any{}
	}

	rawTicket, ticketPresent := body["ticketId"]
	ticketID := adminEntertainmentBodyInt{}
	if ticketPresent {
		ticketID = adminEntertainmentParseOptionalPositiveBodyID(rawTicket)
	}
	if ticketID.Parsed && !ticketID.Valid {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_TICKET_ID", "Invalid ticket ID")
		return
	}
	if ticketID.Overflow {
		adminEntertainmentRespondInternal(c)
		return
	}

	record, err := db.AdminLotteryGetRecordWithUser(ctx, recordID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if record == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "NOT_FOUND", "Record not found")
		return
	}
	if record.Record.PrizeType != models.LotteryPrizeTypeInstance {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "NOT_INSTANCE_PRIZE", "This is not an instance prize")
		return
	}
	if record.Record.Status != models.LotteryRecordStatusPending {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "ALREADY_DELIVERED", "Prize has already been delivered")
		return
	}

	var ticketValue *int32
	if ticketID.Valid {
		parsed, ok := adminEntertainmentInt32(ticketID.Value)
		if !ok {
			adminEntertainmentRespondInternal(c)
			return
		}
		ticketValue = &parsed
	}
	adminID := user.ID
	if _, err := db.AdminLotteryUpdateRecordStatus(ctx, recordID, models.LotteryRecordStatusDelivered, &adminID, ticketValue); err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(adminID), "admin", "lottery.record.deliver",
		fmt.Sprintf("Delivered instance prize to user \"%s\" (Record ID: %d)", record.Username, recordID),
		"success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentListUserPointsHandler mirrors GET /user-points.
func adminEntertainmentListUserPointsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	// `if (!value) return 'points'`: an empty value falls back to the default,
	// only an unknown non-empty value is a 400.
	orderBy := "points"
	if value, present, isArray := promoCodesQueryValue(c, "orderBy"); present && value != "" {
		if isArray || !adminEntertainmentUserPointsOrderBy[value] {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ORDER_BY", "Invalid order by field")
			return
		}
		orderBy = value
	}
	order := "desc"
	if value, present, isArray := promoCodesQueryValue(c, "order"); present && value != "" {
		if isArray || !adminEntertainmentSortOrders[value] {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ORDER", "Invalid sort order")
			return
		}
		order = value
	}

	page := adminEntertainmentParsePaginationValue(c, "page", 1, 100000)
	pageSize := adminEntertainmentParsePaginationValue(c, "pageSize", 20, 100)

	search, searchPresent := adminEntertainmentQueryText(c, "search")
	if !searchPresent {
		// `search.trim()` on a repeated-key array throws in the TypeScript.
		adminEntertainmentRespondInternal(c)
		return
	}

	options := db.AdminLotteryPointsOptions{
		Page: page, PageSize: pageSize, OrderBy: orderBy, Order: order,
	}
	if search != "" {
		options.Search = &search
	}

	result, err := db.AdminLotteryGetAllUserPoints(ctx, options)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	records := make([]gin.H, 0, len(result.Records))
	for _, row := range result.Records {
		var lastConvertedAt *string
		if row.UserPoints.LastConvertedAt != nil {
			formatted := entertainmentISOTime(*row.UserPoints.LastConvertedAt)
			lastConvertedAt = &formatted
		}
		records = append(records, gin.H{
			"userId":           row.UserPoints.UserID,
			"username":         row.Username,
			"userAvatar":       row.UserAvatar,
			"points":           row.UserPoints.Points,
			"totalEarned":      row.UserPoints.TotalEarned,
			"totalSpent":       row.UserPoints.TotalSpent,
			"convertedConsume": entertainmentDecimalFloat(row.UserPoints.ConvertedConsume),
			"lastConvertedAt":  lastConvertedAt,
			"updatedAt":        entertainmentISOTime(row.UserPoints.UpdatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// adminEntertainmentAdjustPointsHandler mirrors POST /user-points/:userId/adjust.
func adminEntertainmentAdjustPointsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	userID, ok := adminEntertainmentParseRouteID(c.Param("userId"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid user ID")
		return
	}

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	rawAmount, amountPresent := body["amount"]
	amount := adminEntertainmentBodyInt{Parsed: true}
	if amountPresent {
		amount = adminEntertainmentParseIntegerBody(rawAmount)
	}
	if !amount.Valid || amount.Value == 0 || math.Abs(float64(amount.Value)) > adminEntertainmentMaxPointsAdjustAmount {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_AMOUNT",
			"Amount must be a non-zero safe integer within the allowed range")
		return
	}
	amountValue, fits := adminEntertainmentInt32(amount.Value)
	if !fits {
		adminEntertainmentRespondInternal(c)
		return
	}

	rawRemark, remarkPresent := body["remark"]
	remark, remarkIsString := rawRemark.(string)
	if !remarkPresent || !remarkIsString || promoCodesJSTrim(remark) == "" {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "REMARK_REQUIRED", "Remark is required")
		return
	}

	summary, err := db.AdminLotteryGetUserSummary(ctx, userID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if summary == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		return
	}

	result, err := db.AdminLotteryAdjustPoints(ctx, userID, amountValue,
		fmt.Sprintf("[管理员%s] %s", user.Username, promoCodesJSTrim(remark)))
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if !result.Success {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "ADJUST_FAILED", result.Message)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "lottery.points.adjust",
		fmt.Sprintf("Adjusted user \"%s\" points by %d (New: %d): %s", summary.Username, amountValue, result.NewPoints, remark),
		"success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true, "newPoints": result.NewPoints})
}

// adminEntertainmentGetUserPointsHandler mirrors GET /user-points/:userId.
func adminEntertainmentGetUserPointsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok := adminEntertainmentParseRouteID(c.Param("userId"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid user ID")
		return
	}

	// Promise.all: the points row is ensured (and the convertible amount read)
	// before the user existence check, exactly like the TypeScript.
	userPoints, err := db.LotteryGetUserPoints(ctx, userID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	convertible, err := db.LotteryGetConvertibleConsume(ctx, userID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	summary, err := db.AdminLotteryGetUserSummary(ctx, userID)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if summary == nil {
		adminEntertainmentRespondError(c, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		return
	}

	var lastConvertedAt *string
	if userPoints.LastConvertedAt != nil {
		formatted := entertainmentISOTime(*userPoints.LastConvertedAt)
		lastConvertedAt = &formatted
	}

	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id":          summary.ID,
			"username":    summary.Username,
			"avatarStyle": summary.AvatarStyle,
		},
		"points":            userPoints.Points,
		"totalEarned":       userPoints.TotalEarned,
		"totalSpent":        userPoints.TotalSpent,
		"convertedConsume":  entertainmentDecimalFloat(userPoints.ConvertedConsume),
		"lastConvertedAt":   lastConvertedAt,
		"totalConsume":      convertible.TotalConsume,
		"convertibleAmount": convertible.ConvertibleAmount,
		"convertiblePoints": convertible.ConvertiblePoints,
	})
}

// adminEntertainmentListPointsLogsHandler mirrors GET /user-points/:userId/logs.
func adminEntertainmentListPointsLogsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok := adminEntertainmentParseRouteID(c.Param("userId"))
	if !ok {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ID", "Invalid user ID")
		return
	}

	page := adminEntertainmentParsePaginationValue(c, "page", 1, 100000)
	pageSize := adminEntertainmentParsePaginationValue(c, "pageSize", 20, 100)

	result, err := db.LotteryGetPointsLogs(ctx, userID, db.LotteryPointsLogOptions{
		Page: int(page), PageSize: int(pageSize),
	})
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	logs := make([]gin.H, 0, len(result.Logs))
	for _, entry := range result.Logs {
		logs = append(logs, gin.H{
			"id":           entry.ID,
			"type":         entry.Type,
			"amount":       entry.Amount,
			"pointsBefore": entry.PointsBefore,
			"pointsAfter":  entry.PointsAfter,
			"remark":       entry.Remark,
			"createdAt":    entertainmentISOTime(entry.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":     logs,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// adminEntertainmentGetCheckinSettingsHandler mirrors GET /checkin/settings.
func adminEntertainmentGetCheckinSettingsHandler(c *gin.Context) {
	settings, err := db.CheckinGetDailyCheckinSettings(c.Request.Context())
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, settings)
}

// adminEntertainmentUpdateCheckinSettingsHandler mirrors PUT /checkin/settings.
func adminEntertainmentUpdateCheckinSettingsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	input := db.AdminLotteryCheckinSettingsInput{}
	if raw, present := body["enabled"]; present && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ENABLED", "enabled must be boolean")
			return
		}
		input.Enabled = &flag
	}
	if raw, present := body["requireInstance"]; present && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_REQUIRE_INSTANCE",
				"requireInstance must be boolean")
			return
		}
		input.RequireInstance = &flag
	}
	if raw, present := body["minPoints"]; present && raw != nil {
		number, isNumber := raw.(float64)
		if !isNumber || !promoCodesIsSafeInteger(number) || number < 1 {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_MIN_POINTS",
				"minPoints must be a positive integer")
			return
		}
		value := int(number)
		input.MinPoints = &value
	}
	if raw, present := body["maxPoints"]; present && raw != nil {
		number, isNumber := raw.(float64)
		if !isNumber || !promoCodesIsSafeInteger(number) || number < 1 {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_MAX_POINTS",
				"maxPoints must be a positive integer")
			return
		}
		value := int(number)
		input.MaxPoints = &value
	}
	if input.MinPoints != nil && input.MaxPoints != nil && *input.MaxPoints < *input.MinPoints {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_POINT_RANGE",
			"maxPoints must be greater than or equal to minPoints")
		return
	}

	settings, err := db.AdminLotteryUpdateDailyCheckinSettings(ctx, input)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "checkin.settings.update",
		fmt.Sprintf("Updated daily check-in settings: enabled=%t, points=%d-%d, requireInstance=%t",
			settings.Enabled, settings.MinPoints, settings.MaxPoints, settings.RequireInstance),
		"success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, settings)
}

// adminEntertainmentListCheckinLogsHandler mirrors GET /checkin/logs.
func adminEntertainmentListCheckinLogsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	page := adminEntertainmentParsePaginationValue(c, "page", 1, 100000)
	pageSize := adminEntertainmentParsePaginationValue(c, "pageSize", 20, 100)

	search, searchOK := adminEntertainmentQueryText(c, "search")
	if !searchOK {
		adminEntertainmentRespondInternal(c)
		return
	}
	dateKey, dateKeyOK := adminEntertainmentQueryText(c, "dateKey")
	if !dateKeyOK {
		adminEntertainmentRespondInternal(c)
		return
	}

	options := db.AdminLotteryCheckinLogOptions{Page: page, PageSize: pageSize}
	if search != "" {
		options.Search = &search
	}
	if dateKey != "" {
		options.DateKey = &dateKey
	}

	result, err := db.AdminLotteryGetAdminDailyCheckinLogs(ctx, options)
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}

	records := make([]gin.H, 0, len(result.Records))
	for _, row := range result.Records {
		records = append(records, gin.H{
			"id":         row.ID,
			"userId":     row.UserID,
			"username":   row.Username,
			"userAvatar": row.UserAvatar,
			"dateKey":    row.DateKey,
			"points":     row.Points,
			"streakDays": row.StreakDays,
			"ipAddress":  row.IPAddress,
			"userAgent":  row.UserAgent,
			"createdAt":  entertainmentISOTime(row.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// adminEntertainmentGetBadgeCatalogHandler mirrors GET /badges/catalog.
func adminEntertainmentGetBadgeCatalogHandler(c *gin.Context) {
	catalog, err := db.BadgesGetAdminCatalog(c.Request.Context())
	if err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, catalog)
}

// adminEntertainmentCreateBadgeSeriesHandler mirrors POST /badges/series.
func adminEntertainmentCreateBadgeSeriesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	id := adminEntertainmentRequireText(body["id"])
	title := adminEntertainmentRequireText(body["title"])
	nameZh := adminEntertainmentRequireText(body["nameZh"])
	description := adminEntertainmentRequireText(body["description"])
	displayOrder := int32(0)
	if raw, present := body["displayOrder"]; present && raw != nil {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if parsed.Valid {
			value, fits := adminEntertainmentInt32(parsed.Value)
			if !fits {
				adminEntertainmentRespondInternal(c)
				return
			}
			displayOrder = value
		}
	}

	if id == nil || !adminEntertainmentIsValidCatalogID(*id) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_SERIES_ID", "Invalid series ID")
		return
	}
	if title == nil || nameZh == nil || description == nil {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_SERIES_DATA",
			"Series title, Chinese name and description are required")
		return
	}

	var isActive *bool
	if raw, present := body["isActive"]; present && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondInternal(c)
			return
		}
		isActive = &flag
	}

	series, err := db.BadgesCreateSeries(ctx, db.BadgesSeriesInput{
		ID:           *id,
		Title:        *title,
		NameZh:       *nameZh,
		NameEn:       adminEntertainmentNormalizeOptionalText(body["nameEn"]),
		Description:  *description,
		SourceID:     adminEntertainmentNormalizeOptionalText(body["sourceId"]),
		SourceLabel:  adminEntertainmentNormalizeOptionalText(body["sourceLabel"]),
		DisplayOrder: &displayOrder,
		IsActive:     isActive,
	})
	if err != nil {
		if adminEntertainmentIsUniqueViolation(err) {
			adminEntertainmentRespondError(c, http.StatusConflict, "SERIES_ID_EXISTS", "Series ID already exists")
			return
		}
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.series.create",
		fmt.Sprintf("Created badge series \"%s\" (%s)", series.Title, series.ID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true, "series": adminEntertainmentBadgeSeriesJSON(series)})
}

// adminEntertainmentUpdateBadgeSeriesHandler mirrors PUT /badges/series/:id.
func adminEntertainmentUpdateBadgeSeriesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	seriesID := c.Param("id")

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	var (
		title       *string
		titleSet    bool
		nameZh      *string
		nameZhSet   bool
		description *string
		descSet     bool
	)
	if raw, present := body["title"]; present {
		titleSet = true
		title = adminEntertainmentRequireText(raw)
	}
	if raw, present := body["nameZh"]; present {
		nameZhSet = true
		nameZh = adminEntertainmentRequireText(raw)
	}
	if raw, present := body["description"]; present {
		descSet = true
		description = adminEntertainmentRequireText(raw)
	}

	if (titleSet && title == nil) || (nameZhSet && nameZh == nil) || (descSet && description == nil) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_SERIES_DATA",
			"Series title, Chinese name and description cannot be empty")
		return
	}

	// GORM's Raw() only substitutes `?` placeholders.
	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = ?")
	}
	if titleSet {
		add("title", *title)
	}
	if nameZhSet {
		add("name_zh", *nameZh)
	}
	if adminEntertainmentOptionalTextSet(body["nameEn"], adminEntertainmentHasKey(body, "nameEn")) {
		add("name_en", adminEntertainmentNormalizeOptionalText(body["nameEn"]))
	}
	if descSet {
		add("description", *description)
	}
	if adminEntertainmentOptionalTextSet(body["sourceId"], adminEntertainmentHasKey(body, "sourceId")) {
		add("source_id", adminEntertainmentNormalizeOptionalText(body["sourceId"]))
	}
	if adminEntertainmentOptionalTextSet(body["sourceLabel"], adminEntertainmentHasKey(body, "sourceLabel")) {
		add("source_label", adminEntertainmentNormalizeOptionalText(body["sourceLabel"]))
	}
	// Prisma rejects null for these non-nullable columns, so a present-but-null
	// value is a 500, not a skipped column.
	if raw, present := body["displayOrder"]; present {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if !parsed.Valid {
			adminEntertainmentRespondInternal(c)
			return
		}
		value, fits := adminEntertainmentInt32(parsed.Value)
		if !fits {
			adminEntertainmentRespondInternal(c)
			return
		}
		add("display_order", value)
	}
	if raw, present := body["isActive"]; present {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondInternal(c)
			return
		}
		add("is_active", flag)
	}
	add("updated_at", time.Now().UTC())

	args = append(args, seriesID)
	var returnedID string
	if err := db.DB.WithContext(ctx).Raw(`UPDATE badge_series SET `+strings.Join(sets, ", ")+
		` WHERE id = ? RETURNING id`, args...).Scan(&returnedID).Error; err != nil {
		adminEntertainmentRespondInternal(c)
		return
	}
	if returnedID == "" {
		adminEntertainmentRespondError(c, http.StatusNotFound, "SERIES_NOT_FOUND", "Series not found")
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.series.update",
		fmt.Sprintf("Updated badge series \"%s\"", seriesID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentDeleteBadgeSeriesHandler mirrors DELETE /badges/series/:id.
func adminEntertainmentDeleteBadgeSeriesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	seriesID := c.Param("id")

	if _, err := db.BadgesDeleteSeries(ctx, seriesID); err != nil {
		if errors.Is(err, db.BadgesErrSeriesHasBadges) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "BADGE_SERIES_HAS_BADGES",
				"Series still has badges; disable it instead or move/delete badges first")
			return
		}
		if adminEntertainmentNotFound(err) {
			adminEntertainmentRespondError(c, http.StatusNotFound, "SERIES_NOT_FOUND", "Series not found")
			return
		}
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.series.delete",
		fmt.Sprintf("Deleted badge series \"%s\"", seriesID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentCreateBadgeHandler mirrors POST /badges.
func adminEntertainmentCreateBadgeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	id := adminEntertainmentRequireText(body["id"])
	name := adminEntertainmentRequireText(body["name"])
	fullLabel := adminEntertainmentRequireText(body["fullLabel"])
	seriesID := adminEntertainmentRequireText(body["seriesId"])
	assetURL := adminEntertainmentRequireText(body["assetUrl"])
	assetURLDark := adminEntertainmentNormalizeOptionalText(body["assetUrlDark"])
	assetURLLight := adminEntertainmentNormalizeOptionalText(body["assetUrlLight"])
	displayOrder := int32(0)
	if raw, present := body["displayOrder"]; present && raw != nil {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if parsed.Valid {
			value, fits := adminEntertainmentInt32(parsed.Value)
			if !fits {
				adminEntertainmentRespondInternal(c)
				return
			}
			displayOrder = value
		}
	}

	if id == nil || !adminEntertainmentIsValidCatalogID(*id) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_BADGE_ID", "Invalid badge ID")
		return
	}
	if name == nil || fullLabel == nil || seriesID == nil || assetURL == nil {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_BADGE_DATA",
			"Badge name, label, series and asset URL are required")
		return
	}
	for _, url := range []*string{assetURL, assetURLDark, assetURLLight} {
		if url != nil && !adminEntertainmentIsValidAssetURL(*url) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ASSET_URL",
				"Asset URL must be a local path or http(s) URL")
			return
		}
	}

	var isActive *bool
	if raw, present := body["isActive"]; present && raw != nil {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondInternal(c)
			return
		}
		isActive = &flag
	}

	badge, err := db.BadgesCreate(ctx, db.BadgesInput{
		ID:            *id,
		Name:          *name,
		NameEn:        adminEntertainmentNormalizeOptionalText(body["nameEn"]),
		FullLabel:     *fullLabel,
		SeriesID:      *seriesID,
		SourceID:      adminEntertainmentNormalizeOptionalText(body["sourceId"]),
		SourceLabel:   adminEntertainmentNormalizeOptionalText(body["sourceLabel"]),
		AssetURL:      *assetURL,
		AssetURLDark:  assetURLDark,
		AssetURLLight: assetURLLight,
		DisplayOrder:  &displayOrder,
		IsActive:      isActive,
	})
	if err != nil {
		if adminEntertainmentIsUniqueViolation(err) {
			adminEntertainmentRespondError(c, http.StatusConflict, "BADGE_ID_EXISTS", "Badge ID already exists")
			return
		}
		if adminEntertainmentIsForeignKeyViolation(err) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "SERIES_NOT_FOUND", "Series not found")
			return
		}
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.create",
		fmt.Sprintf("Created badge \"%s\" (%s)", badge.Name, badge.ID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true, "badge": adminEntertainmentBadgeJSON(badge)})
}

// adminEntertainmentUpdateBadgeHandler mirrors PUT /badges/:id.
func adminEntertainmentUpdateBadgeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	badgeID := c.Param("id")

	body, ok := adminEntertainmentReadBody(c)
	if !ok {
		return
	}

	var (
		assetURL      *string
		assetURLSet   bool
		assetURLDark  *string
		darkSet       bool
		assetURLLight *string
		lightSet      bool
	)
	if raw, present := body["assetUrl"]; present {
		assetURLSet = true
		assetURL = adminEntertainmentRequireText(raw)
	}
	if raw, present := body["assetUrlDark"]; present {
		darkSet = adminEntertainmentOptionalTextSet(raw, true)
		assetURLDark = adminEntertainmentNormalizeOptionalText(raw)
	}
	if raw, present := body["assetUrlLight"]; present {
		lightSet = adminEntertainmentOptionalTextSet(raw, true)
		assetURLLight = adminEntertainmentNormalizeOptionalText(raw)
	}

	if assetURLSet && assetURL == nil {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ASSET_URL", "Asset URL cannot be empty")
		return
	}
	for _, url := range []*string{assetURL, assetURLDark, assetURLLight} {
		if url != nil && !adminEntertainmentIsValidAssetURL(*url) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_ASSET_URL",
				"Asset URL must be a local path or http(s) URL")
			return
		}
	}

	var (
		name      *string
		nameSet   bool
		fullLabel *string
		labelSet  bool
		seriesID  *string
		seriesSet bool
	)
	if raw, present := body["name"]; present {
		nameSet = true
		name = adminEntertainmentRequireText(raw)
	}
	if raw, present := body["fullLabel"]; present {
		labelSet = true
		fullLabel = adminEntertainmentRequireText(raw)
	}
	if raw, present := body["seriesId"]; present {
		seriesSet = true
		seriesID = adminEntertainmentRequireText(raw)
	}

	if (nameSet && name == nil) || (labelSet && fullLabel == nil) || (seriesSet && seriesID == nil) {
		adminEntertainmentRespondError(c, http.StatusBadRequest, "INVALID_BADGE_DATA",
			"Badge name, label and series cannot be empty")
		return
	}

	// GORM's Raw() only substitutes `?` placeholders.
	sets := make([]string, 0, 10)
	args := make([]any, 0, 12)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = ?")
	}
	if nameSet {
		add("name", *name)
	}
	if adminEntertainmentOptionalTextSet(body["nameEn"], adminEntertainmentHasKey(body, "nameEn")) {
		add("name_en", adminEntertainmentNormalizeOptionalText(body["nameEn"]))
	}
	if labelSet {
		add("full_label", *fullLabel)
	}
	if seriesSet {
		add("series_id", *seriesID)
	}
	if adminEntertainmentOptionalTextSet(body["sourceId"], adminEntertainmentHasKey(body, "sourceId")) {
		add("source_id", adminEntertainmentNormalizeOptionalText(body["sourceId"]))
	}
	if adminEntertainmentOptionalTextSet(body["sourceLabel"], adminEntertainmentHasKey(body, "sourceLabel")) {
		add("source_label", adminEntertainmentNormalizeOptionalText(body["sourceLabel"]))
	}
	if assetURLSet {
		add("asset_url", *assetURL)
	}
	if darkSet {
		add("asset_url_dark", assetURLDark)
	}
	if lightSet {
		add("asset_url_light", assetURLLight)
	}
	// Prisma rejects null for these non-nullable columns (500), so no nil guard.
	if raw, present := body["displayOrder"]; present {
		parsed := adminEntertainmentParseIntegerBody(raw)
		if !parsed.Valid {
			adminEntertainmentRespondInternal(c)
			return
		}
		value, fits := adminEntertainmentInt32(parsed.Value)
		if !fits {
			adminEntertainmentRespondInternal(c)
			return
		}
		add("display_order", value)
	}
	if raw, present := body["isActive"]; present {
		flag, isBool := raw.(bool)
		if !isBool {
			adminEntertainmentRespondInternal(c)
			return
		}
		add("is_active", flag)
	}
	add("updated_at", time.Now().UTC())

	args = append(args, badgeID)
	var returnedID string
	if err := db.DB.WithContext(ctx).Raw(`UPDATE badges SET `+strings.Join(sets, ", ")+
		` WHERE id = ? RETURNING id`, args...).Scan(&returnedID).Error; err != nil {
		if adminEntertainmentIsForeignKeyViolation(err) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "SERIES_NOT_FOUND", "Series not found")
			return
		}
		adminEntertainmentRespondInternal(c)
		return
	}
	if returnedID == "" {
		adminEntertainmentRespondError(c, http.StatusNotFound, "BADGE_NOT_FOUND", "Badge not found")
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.update",
		fmt.Sprintf("Updated badge \"%s\"", badgeID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// adminEntertainmentDeleteBadgeHandler mirrors DELETE /badges/:id.
func adminEntertainmentDeleteBadgeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	badgeID := c.Param("id")

	if _, err := db.BadgesDelete(ctx, badgeID); err != nil {
		if errors.Is(err, db.BadgesErrInUse) {
			adminEntertainmentRespondError(c, http.StatusBadRequest, "BADGE_IN_USE",
				"Badge is in use; disable it instead")
			return
		}
		if adminEntertainmentNotFound(err) {
			adminEntertainmentRespondError(c, http.StatusNotFound, "BADGE_NOT_FOUND", "Badge not found")
			return
		}
		adminEntertainmentRespondInternal(c)
		return
	}

	db.CreateLog(ctx, adminEntertainmentInt64Ptr(user.ID), "admin", "badge.delete",
		fmt.Sprintf("Deleted badge \"%s\"", badgeID), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}
