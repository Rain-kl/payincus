// Port of server/src/routes/entertainment.ts (16 routes: points, lottery, badges).
//
// Every route is mounted under /api/entertainment and requires authenticate (the
// plugin-level onRequest hook in the TypeScript). The auth middleware also applies
// Fastify's body-parser rules (400/413/415) before the handler runs, exactly like
// Fastify's onRequest → body-parser ordering, so handlers never re-check body
// presence.
//
// Parity notes:
//   - All ids go through lib.ParsePositiveRouteID (^[1-9]\d*$); values above the
//     PostgreSQL INT4 range are rejected with the same 400 the other Go modules
//     use (Node would forward them to Prisma and surface a 500).
//   - Error payloads are hand-built where the TypeScript uses `reply.send`
//     literals (these are NOT apiError() envelopes: no `code`, custom messages).
//   - The draw path retries transaction conflicts 5 times with
//     `(floor(random*250)+50) * attempt` ms backoff, like the TypeScript. Only the
//     jitter sequence can differ (Node's Math.random).
//   - Win notifications are fire-and-forget (setImmediate in the TypeScript), so
//     they run in a goroutine and never affect the response.
package routes

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// entertainmentPointsLogTypes mirrors POINTS_LOG_TYPES.
var entertainmentPointsLogTypes = map[models.PointsLogType]bool{
	models.PointsLogTypeConvert:          true,
	models.PointsLogTypeLotteryWin:       true,
	models.PointsLogTypeLotterySpend:     true,
	models.PointsLogTypeAdminAdjust:      true,
	models.PointsLogTypeCheckin:          true,
	models.PointsLogTypeBadgeDrawSpend:   true,
	models.PointsLogTypeBadgeSelectSpend: true,
	models.PointsLogTypeInviteGenerate:   true,
	models.PointsLogTypeVipBenefit:       true,
}

// entertainmentLotteryPrizeTypes mirrors LOTTERY_PRIZE_TYPES.
var entertainmentLotteryPrizeTypes = map[models.LotteryPrizeType]bool{
	models.LotteryPrizeTypeNothing:  true,
	models.LotteryPrizeTypePoints:   true,
	models.LotteryPrizeTypeBalance:  true,
	models.LotteryPrizeTypeBadge:    true,
	models.LotteryPrizeTypeInstance: true,
	models.LotteryPrizeTypeCpu:      true,
	models.LotteryPrizeTypeMemory:   true,
	models.LotteryPrizeTypeDisk:     true,
	models.LotteryPrizeTypeTraffic:  true,
}

// entertainmentBadgeErrorMessages mirrors the messageMap in sendBadgeError.
var entertainmentBadgeErrorMessages = map[string]string{
	"NO_BADGES_AVAILABLE":            "暂无可用徽章",
	"BADGE_NOT_FOUND":                "徽章不存在",
	"BADGE_OWNERSHIP_NOT_FOUND":      "未找到该徽章副本",
	"BADGE_ALREADY_APPLIED":          "该徽章副本已应用到目标位置",
	"BADGE_NOT_APPLIED":              "该徽章当前未应用",
	"AVATAR_BADGE_ALREADY_APPLIED":   "头像徽章状态冲突，请刷新后重试",
	"INSTANCE_NOT_FOUND":             "实例不存在",
	"INSTANCE_BADGE_ALREADY_APPLIED": "实例徽章状态冲突，请刷新后重试",
	"BADGE_STATE_CONFLICT":           "徽章状态已变化，请刷新后重试",
}

// ==================== helpers ====================

// entertainmentParseRouteID mirrors parsePositiveRouteId, capped at INT4 (the
// column type) which the Go db layer accepts.
func entertainmentParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// entertainmentParseBodyID mirrors parsePositiveBodyId for AJV-coerced numbers.
func entertainmentParseBodyID(value any) (int32, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number <= 0 {
		return 0, false
	}
	if number > math.MaxInt32 {
		return 0, false
	}
	return int32(number), true
}

func entertainmentParsePointsLogType(value string) (models.PointsLogType, bool, bool) {
	if value == "" {
		return "", false, true
	}
	typed := models.PointsLogType(value)
	if entertainmentPointsLogTypes[typed] {
		return typed, true, true
	}
	return "", false, false
}

func entertainmentParseLotteryPrizeType(value string) (models.LotteryPrizeType, bool, bool) {
	if value == "" {
		return "", false, true
	}
	typed := models.LotteryPrizeType(value)
	if entertainmentLotteryPrizeTypes[typed] {
		return typed, true, true
	}
	return "", false, false
}

// entertainmentISOTime mirrors Date.prototype.toISOString().
func entertainmentISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

func entertainmentOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := entertainmentISOTime(*value)
	return &formatted
}

func entertainmentDecimalFloat(value models.Decimal) float64 {
	result, _ := value.Decimal.Float64()
	return result
}

// entertainmentRespondUnexpected mirrors Fastify's setErrorHandler default branch:
// the error message is public only outside production.
func entertainmentRespondUnexpected(c *gin.Context, err error) {
	message := "Internal server error"
	if err != nil && !lib.IsProductionEnv() {
		message = err.Error()
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

// entertainmentSendBadgeError mirrors sendBadgeError: always HTTP 400 with
// `{error: code, message: mapped-or-default}`.
func entertainmentSendBadgeError(c *gin.Context, err error) {
	code := db.BadgesErrorCode(err)
	message, ok := entertainmentBadgeErrorMessages[code]
	if !ok {
		message = "徽章操作失败"
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": code, "message": message})
}

// entertainmentIsRetryableDrawError mirrors the retry predicate of the draw
// routes (Prisma P2034 / deadlock / write conflict / could not serialize).
func entertainmentIsRetryableDrawError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "P2034") ||
		strings.Contains(message, "deadlock") ||
		strings.Contains(message, "write conflict") ||
		strings.Contains(message, "could not serialize") ||
		strings.Contains(message, "40001") ||
		strings.Contains(message, "40P01")
}

// entertainmentBadgeMessage builds the per-prize response message.
func entertainmentBadgeMessage(prizeType models.LotteryPrizeType, prizeName string) string {
	switch prizeType {
	case models.LotteryPrizeTypeNothing:
		return "再接再厉！"
	case models.LotteryPrizeTypeInstance:
		return "恭喜中奖！请提交工单领取您的实例奖励。"
	case models.LotteryPrizeTypeBadge:
		return "恭喜获得徽章！" + prizeName
	default:
		return "恭喜中奖！" + prizeName
	}
}

// entertainmentPrizeJSON mirrors the prize object used by both list endpoints.
func entertainmentPrizeJSON(prize models.LotteryPrize) gin.H {
	return gin.H{
		"id":             prize.ID,
		"name":           prize.Name,
		"type":           prize.Type,
		"probability":    entertainmentDecimalFloat(prize.Probability),
		"remainQuantity": prize.RemainQuantity,
		"totalQuantity":  prize.TotalQuantity,
		"displayOrder":   prize.DisplayOrder,
		"instanceDesc":   prize.InstanceDesc,
	}
}

func entertainmentPrizesJSON(prizes []models.LotteryPrize) []gin.H {
	items := make([]gin.H, 0, len(prizes))
	for _, prize := range prizes {
		items = append(items, entertainmentPrizeJSON(prize))
	}
	return items
}

// entertainmentLotteryJSON mirrors the lottery object of /lotteries and
// /lotteries/:id.
func entertainmentLotteryJSON(lottery db.LotteryWithPrizes) gin.H {
	return gin.H{
		"id":          lottery.Lottery.ID,
		"name":        lottery.Lottery.Name,
		"description": lottery.Lottery.Description,
		"costPoints":  lottery.Lottery.CostPoints,
		"startAt":     entertainmentOptionalISOTime(lottery.Lottery.StartAt),
		"endAt":       entertainmentOptionalISOTime(lottery.Lottery.EndAt),
		"totalDraws":  lottery.TotalDraws,
		"prizes":      entertainmentPrizesJSON(lottery.Prizes),
	}
}

// entertainmentDrawRecordJSON mirrors the record object returned by draw and
// draw-multi (key order is irrelevant, the set of keys is not).
func entertainmentDrawRecordJSON(record models.LotteryRecord, badgeOwnership *db.BadgesOwnershipView) gin.H {
	return gin.H{
		"id":             record.ID,
		"prizeId":        record.PrizeID,
		"prizeType":      record.PrizeType,
		"prizeName":      record.PrizeName,
		"prizeValue":     record.PrizeValue,
		"badgeOwnership": badgeOwnership,
		"status":         record.Status,
		"pointsSpent":    record.PointsSpent,
		"createdAt":      entertainmentISOTime(record.CreatedAt),
	}
}

// entertainmentNotification builds the payload for the async win notification.
func entertainmentNotification(lotteryID int32, record models.LotteryRecord, prize *db.LotteryDrawPrize, username, lotteryName string) lib.LotteryWinNotification {
	data := lib.LotteryWinNotification{
		LotteryID:   lotteryID,
		RecordID:    record.ID,
		PrizeType:   string(prize.Type),
		Username:    username,
		LotteryName: lotteryName,
		PrizeName:   prize.Name,
	}
	if prize.Type == models.LotteryPrizeTypeBalance {
		amount := int64(prize.Value)
		data.Amount = &amount
	}
	if prize.Type == models.LotteryPrizeTypeInstance && prize.InstanceDesc != nil && *prize.InstanceDesc != "" {
		data.InstanceDesc = prize.InstanceDesc
	}
	return data
}

// entertainmentDrawRetryDelay mirrors `(Math.floor(Math.random()*250)+50)*attempt`.
func entertainmentDrawRetryDelay(attempt int) time.Duration {
	base := rand.IntN(250) + 50
	return time.Duration(base*attempt) * time.Millisecond
}

// entertainmentReadValidatedBody decodes the body and applies Fastify/AJV
// validation. The response has already been written when ok is false.
func entertainmentReadValidatedBody(c *gin.Context, required []string, fields []ajvFieldSpec) (ajvValidated, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return ajvValidated{}, false
	}
	body, validationMessage := validateAJVObject(decoded.Value, required, fields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return ajvValidated{}, false
	}
	return body, true
}

// ==================== route registration ====================

// RegisterEntertainment mounts /api/entertainment.
func RegisterEntertainment(r *gin.Engine) {
	group := r.Group("/api/entertainment", middleware.Authenticate())

	// GET /points — 获取用户积分信息
	group.GET("/points", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		userPoints, err := db.LotteryGetUserPoints(ctx, user.ID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		convertible, err := db.LotteryGetConvertibleConsume(ctx, user.ID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"points":            userPoints.Points,
			"totalEarned":       userPoints.TotalEarned,
			"totalSpent":        userPoints.TotalSpent,
			"lastConvertedAt":   entertainmentOptionalISOTime(userPoints.LastConvertedAt),
			"totalConsume":      convertible.TotalConsume,
			"convertedConsume":  convertible.ConvertedConsume,
			"convertibleAmount": convertible.ConvertibleAmount,
			"convertiblePoints": convertible.ConvertiblePoints,
		})
	})

	// POST /points/convert — 兑换积分
	group.POST("/points/convert", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		result, err := db.LotteryConvertPointsFromConsumption(c.Request.Context(), user.ID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{"error": "NOTHING_TO_CONVERT", "message": result.Message})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"converted":        result.Converted,
			"newPoints":        result.NewPoints,
			"consumeConverted": result.ConsumeConverted,
		})
	})

	// GET /points/logs — 获取积分变动日志
	group.GET("/points/logs", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		logType, hasType, valid := entertainmentParsePointsLogType(query.Get("type"))
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "INVALID_POINTS_LOG_TYPE",
				"message": "Invalid points log type",
			})
			return
		}

		options := db.LotteryPointsLogOptions{
			Page:     lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize: lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
		}
		if hasType {
			options.Type = logType
		}

		result, err := db.LotteryGetPointsLogs(c.Request.Context(), user.ID, options)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}

		logs := make([]gin.H, 0, len(result.Logs))
		for _, log := range result.Logs {
			logs = append(logs, gin.H{
				"id":           log.ID,
				"type":         log.Type,
				"amount":       log.Amount,
				"pointsBefore": log.PointsBefore,
				"pointsAfter":  log.PointsAfter,
				"remark":       log.Remark,
				"createdAt":    entertainmentISOTime(log.CreatedAt),
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"logs":     logs,
			"total":    result.Total,
			"page":     result.Page,
			"pageSize": result.PageSize,
		})
	})

	// GET /lotteries — 获取可用抽奖列表
	group.GET("/lotteries", func(c *gin.Context) {
		lotteries, err := db.LotteryGetActiveLotteries(c.Request.Context())
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		items := make([]gin.H, 0, len(lotteries))
		for _, lottery := range lotteries {
			items = append(items, entertainmentLotteryJSON(lottery))
		}
		c.JSON(http.StatusOK, gin.H{"lotteries": items})
	})

	// GET /lotteries/:id — 获取抽奖详情
	group.GET("/lotteries/:id", func(c *gin.Context) {
		lotteryID, ok := entertainmentParseRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_ID", "message": "Invalid lottery ID"})
			return
		}
		lottery, err := db.LotteryGetByID(c.Request.Context(), lotteryID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		if lottery == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "message": "Lottery not found"})
			return
		}
		if !lottery.Lottery.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "LOTTERY_INACTIVE", "message": "Lottery is not active"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"lottery": entertainmentLotteryJSON(*lottery)})
	})

	// POST /lotteries/:id/draw — 执行抽奖
	group.POST("/lotteries/:id/draw", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()
		lotteryID, ok := entertainmentParseRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_ID", "message": "Invalid lottery ID"})
			return
		}

		const maxRetries = 5
		var lastError error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			result, err := db.LotteryPerformDraw(ctx, user.ID, lotteryID)
			if err != nil {
				lastError = err
				if entertainmentIsRetryableDrawError(err) && attempt < maxRetries {
					select {
					case <-time.After(entertainmentDrawRetryDelay(attempt)):
					case <-ctx.Done():
						entertainmentRespondUnexpected(c, ctx.Err())
						return
					}
					continue
				}
				if errors.Is(err, db.BadgesErrNoBadgesAvailable) {
					entertainmentSendBadgeError(c, err)
					return
				}
				entertainmentRespondUnexpected(c, err)
				return
			}

			if !result.Success {
				c.JSON(http.StatusBadRequest, gin.H{"error": "DRAW_FAILED", "message": result.Message})
				return
			}

			record := result.Record
			prize := result.Prize

			// 异步发送中奖通知（不阻塞响应）
			if prize.Type == models.LotteryPrizeTypeBalance || prize.Type == models.LotteryPrizeTypeInstance {
				lottery, lookupErr := db.LotteryGetByID(ctx, lotteryID)
				if lookupErr != nil {
					entertainmentRespondUnexpected(c, lookupErr)
					return
				}
				lotteryName := ""
				if lottery != nil {
					lotteryName = lottery.Lottery.Name
				}
				notification := entertainmentNotification(lotteryID, *record, prize, user.Username, lotteryName)
				go lib.SendLotteryWinNotification(context.Background(), notification)
			}

			var action any
			if prize.Type == models.LotteryPrizeTypeInstance {
				action = "SUBMIT_TICKET"
			}
			c.JSON(http.StatusOK, gin.H{
				"success":       true,
				"currentPoints": result.CurrentPoints,
				"record":        entertainmentDrawRecordJSON(*record, prize.BadgeOwnership),
				"action":        action,
				"message":       entertainmentBadgeMessage(prize.Type, prize.Name),
			})
			return
		}

		entertainmentRespondUnexpected(c, lastError)
	})

	// POST /lotteries/:id/draw-multi — 十连抽
	group.POST("/lotteries/:id/draw-multi", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()
		lotteryID, ok := entertainmentParseRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_ID", "message": "Invalid lottery ID"})
			return
		}
		const drawCount = 10

		lottery, err := db.LotteryGetByID(ctx, lotteryID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		if lottery == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "message": "Lottery not found"})
			return
		}
		if !lottery.Lottery.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "LOTTERY_INACTIVE", "message": "Lottery is not active"})
			return
		}
		now := time.Now().UTC()
		if lottery.Lottery.StartAt != nil && lottery.Lottery.StartAt.After(now) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "LOTTERY_NOT_STARTED", "message": "Lottery has not started"})
			return
		}
		if lottery.Lottery.EndAt != nil && lottery.Lottery.EndAt.Before(now) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "LOTTERY_ENDED", "message": "Lottery has ended"})
			return
		}

		userPoints, err := db.LotteryGetUserPoints(ctx, user.ID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		totalCost := lottery.Lottery.CostPoints * drawCount
		if userPoints.Points < totalCost {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":    "INSUFFICIENT_POINTS",
				"message":  "积分不足，需要 " + entertainmentIntString(int64(totalCost)) + " 积分，当前仅有 " + entertainmentIntString(int64(userPoints.Points)) + " 积分",
				"required": totalCost,
				"current":  userPoints.Points,
			})
			return
		}

		records := make([]gin.H, 0, drawCount)
		const maxRetries = 5

		for index := 0; index < drawCount; index++ {
			var lastError error
			success := false

			for attempt := 1; attempt <= maxRetries; attempt++ {
				result, err := db.LotteryPerformDraw(ctx, user.ID, lotteryID)
				if err != nil {
					lastError = err
					if errors.Is(err, db.BadgesErrNoBadgesAvailable) {
						if len(records) == 0 {
							entertainmentSendBadgeError(c, err)
							return
						}
						c.JSON(http.StatusOK, gin.H{
							"success":    true,
							"records":    records,
							"totalDraws": len(records),
							"stoppedAt":  index + 1,
							"stopReason": "暂无可用徽章",
						})
						return
					}
					if entertainmentIsRetryableDrawError(err) && attempt < maxRetries {
						select {
						case <-time.After(entertainmentDrawRetryDelay(attempt)):
						case <-ctx.Done():
							entertainmentRespondUnexpected(c, ctx.Err())
							return
						}
						continue
					}
					c.JSON(http.StatusOK, gin.H{
						"success":    len(records) > 0,
						"records":    records,
						"totalDraws": len(records),
						"stoppedAt":  index + 1,
						"stopReason": err.Error(),
					})
					return
				}

				if !result.Success {
					// 非事务冲突类错误（如积分不足），直接返回已有结果
					c.JSON(http.StatusOK, gin.H{
						"success":    len(records) > 0,
						"records":    records,
						"totalDraws": len(records),
						"stoppedAt":  index + 1,
						"stopReason": result.Message,
					})
					return
				}

				record := result.Record
				prize := result.Prize
				records = append(records, entertainmentDrawRecordJSON(*record, prize.BadgeOwnership))

				// 异步发送中奖通知（不阻塞）
				if prize.Type == models.LotteryPrizeTypeBalance || prize.Type == models.LotteryPrizeTypeInstance {
					notification := entertainmentNotification(lotteryID, *record, prize, user.Username, lottery.Lottery.Name)
					go lib.SendLotteryWinNotification(context.Background(), notification)
				}

				success = true
				break
			}

			if !success {
				stopReason := "Max retries exceeded"
				if lastError != nil {
					stopReason = lastError.Error()
				}
				c.JSON(http.StatusOK, gin.H{
					"success":    len(records) > 0,
					"records":    records,
					"totalDraws": len(records),
					"stoppedAt":  index + 1,
					"stopReason": stopReason,
				})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"records":          records,
			"totalDraws":       len(records),
			"totalPointsSpent": lottery.Lottery.CostPoints * int32(len(records)),
		})
	})

	// GET /lottery-records — 获取用户中奖记录
	group.GET("/lottery-records", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		prizeType, hasPrizeType, valid := entertainmentParseLotteryPrizeType(query.Get("prizeType"))
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_PRIZE_TYPE", "message": "Invalid prize type"})
			return
		}

		options := db.LotteryRecordListOptions{
			Page:     lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize: lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
		}
		if hasPrizeType {
			options.PrizeType = prizeType
		}

		result, err := db.LotteryGetUserLotteryRecords(c.Request.Context(), user.ID, options)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}

		records := make([]gin.H, 0, len(result.Records))
		for _, item := range result.Records {
			var instanceDesc *string
			if item.InstanceDesc != nil && *item.InstanceDesc != "" {
				instanceDesc = item.InstanceDesc
			}
			records = append(records, gin.H{
				"id":           item.Record.ID,
				"lotteryId":    item.Record.LotteryID,
				"lotteryName":  item.LotteryName,
				"prizeType":    item.Record.PrizeType,
				"prizeName":    item.Record.PrizeName,
				"prizeValue":   item.Record.PrizeValue,
				"instanceDesc": instanceDesc,
				"status":       item.Record.Status,
				"pointsSpent":  item.Record.PointsSpent,
				"createdAt":    entertainmentISOTime(item.Record.CreatedAt),
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"records":  records,
			"total":    result.Total,
			"page":     result.Page,
			"pageSize": result.PageSize,
		})
	})

	// GET /badges/catalog — 徽章目录
	group.GET("/badges/catalog", func(c *gin.Context) {
		ctx := c.Request.Context()
		series, err := db.BadgesGetSeriesList(ctx, db.BadgesCatalogOptions{})
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		badges, err := db.BadgesGetCatalog(ctx, db.BadgesCatalogOptions{})
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"series": series, "badges": badges})
	})

	// GET /badges/overview — 徽章总览
	group.GET("/badges/overview", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		overview, err := db.BadgesGetOverview(c.Request.Context(), user.ID)
		if err != nil {
			entertainmentRespondUnexpected(c, err)
			return
		}
		c.JSON(http.StatusOK, overview)
	})

	// POST /badges/draw/random — 随机抽取徽章
	group.POST("/badges/draw/random", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		result, err := db.BadgesPerformRandomDraw(c.Request.Context(), user.ID)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":         "INSUFFICIENT_POINTS",
				"message":       "积分不足",
				"currentPoints": result.Points,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"currentPoints": result.Points,
			"ownership":     result.Ownership,
		})
	})

	// POST /badges/draw/random-multi — 随机十连抽
	group.POST("/badges/draw/random-multi", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		result, err := db.BadgesPerformRandomMultiDraw(c.Request.Context(), user.ID, 10)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":         "INSUFFICIENT_POINTS",
				"message":       "积分不足",
				"currentPoints": result.Points,
			})
			return
		}
		ownerships := result.Ownerships
		if ownerships == nil {
			ownerships = []db.BadgesOwnershipView{}
		}
		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"currentPoints": result.Points,
			"ownerships":    ownerships,
		})
	})

	// POST /badges/draw/select — 自选徽章
	group.POST("/badges/draw/select", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		body, ok := entertainmentReadValidatedBody(c, []string{"badgeId"}, []ajvFieldSpec{
			{Name: "badgeId", Kind: ajvKindString, MinLength: 1, MaxLength: 64, Minimum: math.NaN(), Maximum: math.NaN()},
		})
		if !ok {
			return
		}
		rawBadgeID, _ := body.stringValue("badgeId")
		badgeID := strings.TrimSpace(rawBadgeID)
		if badgeID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "BADGE_ID_REQUIRED", "message": "badgeId is required"})
			return
		}

		result, err := db.BadgesPerformSelect(c.Request.Context(), user.ID, badgeID)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":         "INSUFFICIENT_POINTS",
				"message":       "积分不足",
				"currentPoints": result.Points,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"currentPoints": result.Points,
			"ownership":     result.Ownership,
		})
	})

	// POST /badges/apply/avatar — 应用到头像
	group.POST("/badges/apply/avatar", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		body, ok := entertainmentReadValidatedBody(c, []string{"ownershipId"}, []ajvFieldSpec{
			{Name: "ownershipId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		})
		if !ok {
			return
		}
		ownershipID, valid := entertainmentParseBodyID(body.Values["ownershipId"])
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_OWNERSHIP_ID", "message": "Invalid ownershipId"})
			return
		}

		ownership, err := db.BadgesApplyToAvatar(c.Request.Context(), user.ID, ownershipID)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "ownership": ownership})
	})

	// POST /badges/apply/instance — 应用到实例
	group.POST("/badges/apply/instance", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		body, ok := entertainmentReadValidatedBody(c, []string{"ownershipId", "instanceId"}, []ajvFieldSpec{
			{Name: "ownershipId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
			{Name: "instanceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		})
		if !ok {
			return
		}
		ownershipID, ownershipOK := entertainmentParseBodyID(body.Values["ownershipId"])
		instanceID, instanceOK := entertainmentParseBodyID(body.Values["instanceId"])
		if !ownershipOK || !instanceOK {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_PARAMS", "message": "Invalid ownershipId or instanceId"})
			return
		}

		ownership, err := db.BadgesApplyToInstance(c.Request.Context(), user.ID, ownershipID, instanceID)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "ownership": ownership})
	})

	// POST /badges/unapply — 取消应用
	group.POST("/badges/unapply", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		body, ok := entertainmentReadValidatedBody(c, []string{"ownershipId"}, []ajvFieldSpec{
			{Name: "ownershipId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		})
		if !ok {
			return
		}
		ownershipID, valid := entertainmentParseBodyID(body.Values["ownershipId"])
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": "INVALID_OWNERSHIP_ID", "message": "Invalid ownershipId"})
			return
		}

		ownership, err := db.BadgesUnapply(c.Request.Context(), user.ID, ownershipID)
		if err != nil {
			entertainmentSendBadgeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "ownership": ownership})
	})
}

// entertainmentIntString renders an integer the way a JS template literal does
// (no separators/padding).
func entertainmentIntString(value int64) string {
	return strconv.FormatInt(value, 10)
}

func init() { Register(RegisterEntertainment) }
