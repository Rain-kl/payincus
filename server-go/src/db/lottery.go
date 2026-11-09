// Port of server/src/db/lottery.ts plus the four points.ts functions that
// routes/entertainment.ts reaches through `db` (getUserPoints,
// getConvertibleConsume, convertPointsFromConsumption, getPointsLogs).
//
// Concurrency / security boundary
// ------------------------------
// A draw spends points, so it runs in ONE transaction-scoped critical section:
//
//	pg_advisory_xact_lock(USER_POINTS_LOCK_NAMESPACE = 4111, userId)
//
// taken before the lottery row is even read, on the same pgx transaction as every
// subsequent read/UPDATE. Racing draws for the same user serialize; a second draw
// re-reads the post-decrement balance and cannot spend the same points twice.
// Prize stock uses the TypeScript's optimistic guard
// (`UPDATE ... WHERE remain_quantity > 0`) instead of a lock, so over-issue is
// impossible even across different users. Inside that section the balance prize
// optionally takes the non-blocking USER_BALANCE_LOCK_NAMESPACE = 4110 lock and
// resource prizes take USER_RESOURCE_POOL_LOCK_NAMESPACE = 4114 (same values as
// server/src/db/advisory-locks.ts and db/advisory_locks.go).
//
// Transaction isolation is REPEATABLE READ (Prisma TransactionIsolationLevel.
// RepeatableRead) with maxWait 5000 / timeout 10000 approximated as a 5s pool
// acquisition context plus a 10s transaction context.
//
// Randomness: Node's Math.random() sequence is not reproducible. The weighted
// selector is ported exactly (absolute-percentage intervals over ALL prizes,
// exhausted intervals falling back to the `nothing` prize) but draws from the
// injectable LotteryRandomFloat hook, so tests pin outcomes; production uses
// math/rand/v2.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// LotteryRandomFloat is the injectable source of [0,1) randomness for the
// weighted prize selection. Override it in tests to make the draw deterministic.
var LotteryRandomFloat = func() float64 { return rand.Float64() }

const (
	lotteryTransactionMaxAttempts = 5
	lotteryTransactionMaxWait     = 5 * time.Second
	lotteryTransactionTimeout     = 10 * time.Second

	lotteryDefaultPageSize = 20
	lotteryMaxPageSize     = 100
)

// lotteryPrizeTypes mirrors LOTTERY_PRIZE_TYPES.
var lotteryPrizeTypes = map[models.LotteryPrizeType]bool{
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

// ==================== sentinels ====================

var (
	LotteryErrNoAvailablePrizes = errors.New("NO_AVAILABLE_PRIZES")
	LotteryErrBalanceConflict   = errors.New("BALANCE_CONFLICT: 余额正在处理，请稍后重试")
	LotteryErrConfigError       = errors.New("LOTTERY_CONFIG_ERROR: 奖品概率总和不足100%且缺少\"谢谢参与\"奖品，请添加一个type为nothing的奖品来补足剩余概率")
	LotteryErrUserPointsFailed  = errors.New("USER_POINTS_CREATE_FAILED")
)

// ==================== types ====================

// LotteryWithPrizes mirrors the shape of getLotteryById/getActiveLotteries:
// the lottery row, its prizes and the live record count.
type LotteryWithPrizes struct {
	Lottery    models.Lottery
	Prizes     []models.LotteryPrize
	TotalDraws int64
}

// LotteryDrawPrize mirrors the `prize` object performDraw returns.
type LotteryDrawPrize struct {
	ID             int32                   `json:"id"`
	Name           string                  `json:"name"`
	Type           models.LotteryPrizeType `json:"type"`
	Value          int32                   `json:"value"`
	InstanceDesc   *string                 `json:"instanceDesc"`
	BadgeOwnership *BadgesOwnershipView    `json:"badgeOwnership"`
}

// LotteryDrawResult mirrors performDraw's return shape.
type LotteryDrawResult struct {
	Success       bool
	Record        *models.LotteryRecord
	Prize         *LotteryDrawPrize
	CurrentPoints int32
	Message       string
}

// LotteryRecordWithRelations mirrors the include of getUserLotteryRecords.
type LotteryRecordWithRelations struct {
	Record       models.LotteryRecord
	LotteryName  string
	InstanceDesc *string
}

// LotteryRecordListResult mirrors getUserLotteryRecords' return shape.
type LotteryRecordListResult struct {
	Records  []LotteryRecordWithRelations
	Total    int64
	Page     int
	PageSize int
}

// LotteryRecordListOptions mirrors the options object of getUserLotteryRecords.
type LotteryRecordListOptions struct {
	Page      int
	PageSize  int
	PrizeType models.LotteryPrizeType
}

// LotteryPointsLogListResult mirrors getPointsLogs' return shape.
type LotteryPointsLogListResult struct {
	Logs     []models.PointsLog
	Total    int64
	Page     int
	PageSize int
}

// LotteryPointsLogOptions mirrors getPointsLogs' options object.
type LotteryPointsLogOptions struct {
	Page     int
	PageSize int
	Type     models.PointsLogType
}

// LotteryConvertibleConsume mirrors getConvertibleConsume's return shape.
type LotteryConvertibleConsume struct {
	TotalConsume      float64
	ConvertedConsume  float64
	ConvertibleAmount float64
	ConvertiblePoints int64
}

// LotteryConvertResult mirrors convertPointsFromConsumption's return shape.
type LotteryConvertResult struct {
	Success          bool
	Converted        int32
	NewPoints        int32
	ConsumeConverted float64
	Message          string
}

// ==================== helpers ====================

func lotteryNormalizePrizeType(t models.LotteryPrizeType) models.LotteryPrizeType {
	if t != "" && lotteryPrizeTypes[t] {
		return t
	}
	return ""
}

// lotteryClampPagination mirrors clampPagination from lottery.ts. Unlike
// db.clampPagination (balance.ts) a pageSize of 0 means "use the fallback"; both
// helpers agree for the values the route passes.
func lotteryClampPagination(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = lotteryDefaultPageSize
	}
	if pageSize > lotteryMaxPageSize {
		pageSize = lotteryMaxPageSize
	}
	return page, pageSize
}

func lotteryIsRetryableTransactionError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "deadlock") ||
		strings.Contains(message, "write conflict") ||
		strings.Contains(message, "could not serialize")
}

// lotteryEnsureUserPoints mirrors ensureUserPoints from points.ts: an insert that
// does nothing on conflict, then a re-read. No updated_at bump for existing rows.
func lotteryEnsureUserPoints(ctx context.Context, q BadgesQuerier, userID int32) (models.UserPoints, error) {
	return badgesGetUserPoints(ctx, q, userID)
}

// ==================== reads: lotteries ====================

func lotteryScanRow(row pgx.Row) (models.Lottery, error) {
	var (
		id          int32
		name        string
		description *string
		costPoints  int32
		isActive    bool
		startAt     *time.Time
		endAt       *time.Time
		totalDraws  int32
		createdAt   time.Time
		updatedAt   time.Time
		createdBy   int32
	)
	err := row.Scan(&id, &name, &description, &costPoints, &isActive, &startAt, &endAt,
		&totalDraws, &createdAt, &updatedAt, &createdBy)
	if err != nil {
		return models.Lottery{}, err
	}
	return models.Lottery{
		ID:          id,
		Name:        name,
		Description: description,
		CostPoints:  costPoints,
		IsActive:    isActive,
		StartAt:     startAt,
		EndAt:       endAt,
		TotalDraws:  totalDraws,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		CreatedBy:   createdBy,
	}, nil
}

const lotteryColumns = `id, name, description, cost_points, is_active, start_at, end_at,
	total_draws, created_at, updated_at, created_by`

const lotteryPrizeColumns = `id, lottery_id, name, type::text, value, probability, total_quantity,
	remain_quantity, display_order, created_at, instance_desc`

func lotteryScanPrizes(rows pgx.Rows) ([]models.LotteryPrize, error) {
	prizes := make([]models.LotteryPrize, 0, 8)
	for rows.Next() {
		var (
			id             int32
			lotteryID      int32
			name           string
			prizeType      string
			value          int32
			probability    decimal.Decimal
			totalQuantity  *int32
			remainQuantity *int32
			displayOrder   int32
			createdAt      time.Time
			instanceDesc   *string
		)
		if err := rows.Scan(&id, &lotteryID, &name, &prizeType, &value, &probability,
			&totalQuantity, &remainQuantity, &displayOrder, &createdAt, &instanceDesc); err != nil {
			return nil, err
		}
		prizes = append(prizes, models.LotteryPrize{
			ID:             id,
			LotteryID:      lotteryID,
			Name:           name,
			Type:           models.LotteryPrizeType(prizeType),
			Value:          value,
			Probability:    models.Decimal{Decimal: probability},
			TotalQuantity:  totalQuantity,
			RemainQuantity: remainQuantity,
			DisplayOrder:   displayOrder,
			CreatedAt:      createdAt,
			InstanceDesc:   instanceDesc,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return prizes, nil
}

// lotteryLoadPrizes loads the prize pool in the requested order. The TypeScript
// `include: { prizes: true }` has no orderBy (PostgreSQL heap order, i.e. earlier
// inserts first); the draw path therefore orders by id so selection is
// deterministic, while getLotteryById asks for display_order ASC exactly like the
// TypeScript.
func lotteryLoadPrizes(ctx context.Context, q BadgesQuerier, lotteryID int32, orderBy string) ([]models.LotteryPrize, error) {
	rows, err := q.Query(ctx, "SELECT "+lotteryPrizeColumns+" FROM lottery_prizes WHERE lottery_id = $1 "+orderBy, lotteryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return lotteryScanPrizes(rows)
}

// LotteryGetByID mirrors getLotteryById (prizes ordered by displayOrder ASC, plus
// the live record count). The notificationConfig include is not needed by the
// user-facing routes and is loaded by LotteryGetNotificationConfig instead.
func LotteryGetByID(ctx context.Context, id int32) (*LotteryWithPrizes, error) {
	row, err := lotteryScanRow(Pool().QueryRow(ctx, "SELECT "+lotteryColumns+" FROM lotteries WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	prizes, err := lotteryLoadPrizes(ctx, Pool(), id, "ORDER BY display_order ASC")
	if err != nil {
		return nil, err
	}
	var total int64
	if err := Pool().QueryRow(ctx, "SELECT COUNT(*) FROM lottery_records WHERE lottery_id = $1", id).Scan(&total); err != nil {
		return nil, err
	}
	return &LotteryWithPrizes{Lottery: row, Prizes: prizes, TotalDraws: total}, nil
}

// LotteryGetActiveLotteries mirrors getActiveLotteries: active, already started,
// not yet ended, newest first.
func LotteryGetActiveLotteries(ctx context.Context) ([]LotteryWithPrizes, error) {
	now := time.Now().UTC()
	rows, err := Pool().Query(ctx, `SELECT l.id, l.name, l.description, l.cost_points, l.is_active,
			l.start_at, l.end_at, l.total_draws, l.created_at, l.updated_at, l.created_by,
			(SELECT COUNT(*) FROM lottery_records r WHERE r.lottery_id = l.id)
		FROM lotteries l
		WHERE l.is_active = true
			AND (l.start_at IS NULL OR l.start_at <= $1)
			AND (l.end_at IS NULL OR l.end_at >= $1)
		ORDER BY l.created_at DESC`, now)
	if err != nil {
		return nil, err
	}

	type activeRow struct {
		lottery models.Lottery
		total   int64
	}
	active := make([]activeRow, 0, 8)
	ids := make([]int32, 0, 8)
	for rows.Next() {
		var (
			id          int32
			name        string
			description *string
			costPoints  int32
			isActive    bool
			startAt     *time.Time
			endAt       *time.Time
			totalDraws  int32
			createdAt   time.Time
			updatedAt   time.Time
			createdBy   int32
			total       int64
		)
		if err := rows.Scan(&id, &name, &description, &costPoints, &isActive, &startAt, &endAt,
			&totalDraws, &createdAt, &updatedAt, &createdBy, &total); err != nil {
			rows.Close()
			return nil, err
		}
		active = append(active, activeRow{
			lottery: models.Lottery{
				ID:          id,
				Name:        name,
				Description: description,
				CostPoints:  costPoints,
				IsActive:    isActive,
				StartAt:     startAt,
				EndAt:       endAt,
				TotalDraws:  totalDraws,
				CreatedAt:   createdAt,
				UpdatedAt:   updatedAt,
				CreatedBy:   createdBy,
			},
			total: total,
		})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	prizeMap := map[int32][]models.LotteryPrize{}
	if len(ids) > 0 {
		prizeRows, err := Pool().Query(ctx, "SELECT "+lotteryPrizeColumns+
			" FROM lottery_prizes WHERE lottery_id = ANY($1) ORDER BY display_order ASC, id ASC", ids)
		if err != nil {
			return nil, err
		}
		prizes, err := lotteryScanPrizes(prizeRows)
		prizeRows.Close()
		if err != nil {
			return nil, err
		}
		for _, prize := range prizes {
			prizeMap[prize.LotteryID] = append(prizeMap[prize.LotteryID], prize)
		}
	}

	result := make([]LotteryWithPrizes, 0, len(active))
	for _, item := range active {
		prizes := prizeMap[item.lottery.ID]
		if prizes == nil {
			prizes = []models.LotteryPrize{}
		}
		result = append(result, LotteryWithPrizes{Lottery: item.lottery, Prizes: prizes, TotalDraws: item.total})
	}
	return result, nil
}

// ==================== reads: records ====================

// LotteryGetUserLotteryRecords mirrors getUserLotteryRecords.
func LotteryGetUserLotteryRecords(ctx context.Context, userID int32, options LotteryRecordListOptions) (LotteryRecordListResult, error) {
	page, pageSize := lotteryClampPagination(options.Page, options.PageSize)
	prizeType := lotteryNormalizePrizeType(options.PrizeType)
	skip := (page - 1) * pageSize

	buildWhere := func() (string, []any) {
		where := "r.user_id = $1"
		args := []any{userID}
		if prizeType != "" {
			where += " AND r.prize_type = $2"
			args = append(args, string(prizeType))
		}
		return where, args
	}

	where, args := buildWhere()
	var total int64
	if err := Pool().QueryRow(ctx, "SELECT COUNT(*) FROM lottery_records r WHERE "+where, args...).Scan(&total); err != nil {
		return LotteryRecordListResult{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, skip)
	rows, err := Pool().Query(ctx, `SELECT r.id, r.lottery_id, r.prize_id, r.user_id, r.prize_type::text,
			r.prize_value, r.prize_name, r.status::text, r.points_spent, r.delivered_at, r.delivered_by,
			r.ticket_id, r.notification_sent, r.created_at,
			l.name, p.instance_desc
		FROM lottery_records r
		JOIN lotteries l ON l.id = r.lottery_id
		LEFT JOIN lottery_prizes p ON p.id = r.prize_id
		WHERE `+where+`
		ORDER BY r.created_at DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return LotteryRecordListResult{}, err
	}
	defer rows.Close()

	records := make([]LotteryRecordWithRelations, 0, pageSize)
	for rows.Next() {
		var (
			id               int32
			lotteryID        int32
			prizeID          int32
			user             int32
			prize            string
			prizeValue       int32
			prizeName        string
			status           string
			pointsSpent      int32
			deliveredAt      *time.Time
			deliveredBy      *int32
			ticketID         *int32
			notificationSent bool
			createdAt        time.Time
			lotteryName      string
			instanceDesc     *string
		)
		if err := rows.Scan(&id, &lotteryID, &prizeID, &user, &prize, &prizeValue, &prizeName,
			&status, &pointsSpent, &deliveredAt, &deliveredBy, &ticketID, &notificationSent,
			&createdAt, &lotteryName, &instanceDesc); err != nil {
			return LotteryRecordListResult{}, err
		}
		records = append(records, LotteryRecordWithRelations{
			Record: models.LotteryRecord{
				ID:               id,
				LotteryID:        lotteryID,
				PrizeID:          prizeID,
				UserID:           user,
				PrizeType:        models.LotteryPrizeType(prize),
				PrizeValue:       prizeValue,
				PrizeName:        prizeName,
				Status:           models.LotteryRecordStatus(status),
				PointsSpent:      pointsSpent,
				DeliveredAt:      deliveredAt,
				DeliveredBy:      deliveredBy,
				TicketID:         ticketID,
				NotificationSent: notificationSent,
				CreatedAt:        createdAt,
			},
			LotteryName:  lotteryName,
			InstanceDesc: instanceDesc,
		})
	}
	if err := rows.Err(); err != nil {
		return LotteryRecordListResult{}, err
	}

	return LotteryRecordListResult{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// ==================== notification config ====================

// LotteryGetNotificationConfig mirrors getLotteryNotificationConfig.
func LotteryGetNotificationConfig(ctx context.Context, lotteryID int32) (*models.LotteryNotificationConfig, error) {
	var config models.LotteryNotificationConfig
	err := DB.WithContext(ctx).Where("lottery_id = ?", lotteryID).First(&config).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// LotteryMarkRecordNotificationSent mirrors markRecordNotificationSent.
func LotteryMarkRecordNotificationSent(ctx context.Context, recordID int32) error {
	_, err := Pool().Exec(ctx, `UPDATE lottery_records SET notification_sent = true WHERE id = $1`, recordID)
	return err
}

// ==================== draw ====================

// LotteryWeightedRandomSelect mirrors weightedRandomSelect exactly:
//
//   - the interval walk covers ALL prizes (so an exhausted prize keeps its
//     probability slot and the hit falls back to the `nothing` prize);
//   - probability is an absolute percentage; a random in [0,100) is compared
//     against the running sum with a strict `<` (so a prize boundary belongs to
//     the next slot);
//   - a pool with no prize left in stock throws NO_AVAILABLE_PRIZES;
//   - a random above the total probability sum returns `nothing`, and if that
//     prize does not exist the configuration is rejected rather than silently
//     redistributing the remaining probability.
func LotteryWeightedRandomSelect(prizes []models.LotteryPrize) (models.LotteryPrize, error) {
	var nothingPrize *models.LotteryPrize
	for index := range prizes {
		if prizes[index].Type == models.LotteryPrizeTypeNothing {
			nothingPrize = &prizes[index]
			break
		}
	}

	hasAvailable := false
	for _, prize := range prizes {
		if prize.RemainQuantity == nil || *prize.RemainQuantity > 0 {
			hasAvailable = true
			break
		}
	}
	if !hasAvailable {
		return models.LotteryPrize{}, LotteryErrNoAvailablePrizes
	}

	random := LotteryRandomFloat() * 100
	cumulative := 0.0
	for _, prize := range prizes {
		probability, _ := prize.Probability.Decimal.Float64()
		cumulative += probability
		if random < cumulative {
			if prize.RemainQuantity == nil || *prize.RemainQuantity > 0 {
				return prize, nil
			}
			if nothingPrize != nil {
				return *nothingPrize, nil
			}
		}
	}

	if nothingPrize != nil {
		return *nothingPrize, nil
	}
	return models.LotteryPrize{}, LotteryErrConfigError
}

// LotteryPerformDraw mirrors performDraw. It must run as a single transaction: the
// points advisory lock is transaction-scoped and released on commit/rollback.
func LotteryPerformDraw(ctx context.Context, userID int32, lotteryID int32) (LotteryDrawResult, error) {
	if Pool() == nil {
		return LotteryDrawResult{}, errors.New("database not initialized")
	}

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, lotteryTransactionMaxWait)
	defer cancelAcquire()
	tx, err := Pool().BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return LotteryDrawResult{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	txCtx, cancelTx := context.WithTimeout(ctx, lotteryTransactionTimeout)
	defer cancelTx()

	result, err := lotteryPerformDrawInTx(txCtx, tx, userID, lotteryID)
	if err != nil {
		return LotteryDrawResult{}, err
	}
	if err := tx.Commit(txCtx); err != nil {
		return LotteryDrawResult{}, err
	}
	return result, nil
}

func lotteryPerformDrawInTx(ctx context.Context, tx pgx.Tx, userID int32, lotteryID int32) (LotteryDrawResult, error) {
	// Serialize every points mutation for this user before reading anything.
	if err := AdvisoryTransactionLock(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		return LotteryDrawResult{}, err
	}

	lottery, err := lotteryScanRow(tx.QueryRow(ctx, "SELECT "+lotteryColumns+" FROM lotteries WHERE id = $1", lotteryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LotteryDrawResult{Success: false, Message: "抽奖活动不存在"}, nil
	}
	if err != nil {
		return LotteryDrawResult{}, err
	}

	if !lottery.IsActive {
		return LotteryDrawResult{Success: false, Message: "抽奖活动已关闭"}, nil
	}

	now := time.Now().UTC()
	if lottery.StartAt != nil && lottery.StartAt.After(now) {
		return LotteryDrawResult{Success: false, Message: "抽奖活动尚未开始"}, nil
	}
	if lottery.EndAt != nil && lottery.EndAt.Before(now) {
		return LotteryDrawResult{Success: false, Message: "抽奖活动已结束"}, nil
	}

	prizes, err := lotteryLoadPrizes(ctx, tx, lotteryID, "ORDER BY id ASC")
	if err != nil {
		return LotteryDrawResult{}, err
	}
	if len(prizes) == 0 {
		return LotteryDrawResult{Success: false, Message: "奖池为空"}, nil
	}

	userPoints, err := lotteryEnsureUserPoints(ctx, tx, userID)
	if err != nil {
		return LotteryDrawResult{}, err
	}
	if userPoints.Points < lottery.CostPoints {
		return LotteryDrawResult{Success: false, Message: "积分不足"}, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE user_points
		SET points = points - $1, total_spent = total_spent + $1, updated_at = NOW()
		WHERE user_id = $2`, lottery.CostPoints, userID); err != nil {
		return LotteryDrawResult{}, err
	}

	selected, err := LotteryWeightedRandomSelect(prizes)
	if err != nil {
		return LotteryDrawResult{}, err
	}

	prize := selected
	for _, candidate := range prizes {
		if candidate.ID == selected.ID {
			prize = candidate
			break
		}
	}

	// Optimistic stock decrement: only a row with remaining stock is updated.
	if prize.RemainQuantity != nil {
		tag, err := tx.Exec(ctx, `UPDATE lottery_prizes SET remain_quantity = remain_quantity - 1
			WHERE id = $1 AND remain_quantity > 0`, prize.ID)
		if err != nil {
			return LotteryDrawResult{}, err
		}
		if tag.RowsAffected() == 0 {
			fallback := -1
			for index := range prizes {
				if prizes[index].Type == models.LotteryPrizeTypeNothing {
					fallback = index
					break
				}
			}
			if fallback < 0 {
				return LotteryDrawResult{Success: false, Message: "奖品库存不足，请稍后再试"}, nil
			}
			prize = prizes[fallback]
		}
	}

	status := models.LotteryRecordStatusDelivered
	currentPoints := userPoints.Points - lottery.CostPoints
	resolvedPrizeName := prize.Name
	resolvedPrizeValue := prize.Value
	if prize.Type == models.LotteryPrizeTypeBadge {
		resolvedPrizeValue = 0
	}
	var badgeOwnership *BadgesOwnershipView

	switch prize.Type {
	case models.LotteryPrizeTypeBalance:
		// value is stored in cents; Prisma increments the Decimal balance by
		// value/100 (an exact two-decimal amount).
		balanceAmount := decimal.NewFromInt(int64(prize.Value)).Div(decimal.NewFromInt(100))
		locked, err := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, userID)
		if err != nil {
			return LotteryDrawResult{}, err
		}
		if !locked {
			return LotteryDrawResult{}, LotteryErrBalanceConflict
		}

		var balanceBefore models.Decimal
		userErr := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&balanceBefore)
		if userErr != nil && !errors.Is(userErr, pgx.ErrNoRows) {
			return LotteryDrawResult{}, userErr
		}
		if userErr == nil {
			balanceBeforeValue := decimalToFloat64(balanceBefore)
			var balanceAfter models.Decimal
			if err := tx.QueryRow(ctx, `UPDATE users SET balance = balance + $1, updated_at = NOW()
				WHERE id = $2 RETURNING balance`, models.Decimal{Decimal: balanceAmount}, userID).Scan(&balanceAfter); err != nil {
				return LotteryDrawResult{}, err
			}
			balanceAfterValue := decimalToFloat64(balanceAfter)
			if _, err := tx.Exec(ctx, `INSERT INTO balance_logs
				(user_id, type, amount, balance_before, balance_after, remark, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
				userID, string(models.BalanceLogTypeGift), models.Decimal{Decimal: balanceAmount},
				balanceBeforeValue, balanceAfterValue, "抽奖中奖："+prize.Name); err != nil {
				return LotteryDrawResult{}, err
			}
		}

	case models.LotteryPrizeTypePoints:
		if _, err := tx.Exec(ctx, `UPDATE user_points
			SET points = points + $1, total_earned = total_earned + $1, updated_at = NOW()
			WHERE user_id = $2`, prize.Value, userID); err != nil {
			return LotteryDrawResult{}, err
		}
		currentPoints += prize.Value

	case models.LotteryPrizeTypeBadge:
		selectedBadge, err := BadgesSelectRandomOrThrow(ctx, tx)
		if err != nil {
			return LotteryDrawResult{}, err
		}
		ownership, err := BadgesCreateOwnership(ctx, tx, userID, selectedBadge.ID, models.UserBadgeSourceLottery, &selectedBadge)
		if err != nil {
			return LotteryDrawResult{}, err
		}
		badgeOwnership = &ownership
		resolvedPrizeName = selectedBadge.FullLabel

	case models.LotteryPrizeTypeInstance:
		// Instance rewards are delivered manually after the user opens a ticket.
		status = models.LotteryRecordStatusPending

	case models.LotteryPrizeTypeCpu, models.LotteryPrizeTypeMemory,
		models.LotteryPrizeTypeDisk, models.LotteryPrizeTypeTraffic:
		field := "cpu"
		resourceType := models.RedeemCodeTypeC
		switch prize.Type {
		case models.LotteryPrizeTypeMemory:
			field = "memory"
			resourceType = models.RedeemCodeTypeR
		case models.LotteryPrizeTypeDisk:
			field = "disk"
			resourceType = models.RedeemCodeTypeD
		case models.LotteryPrizeTypeTraffic:
			field = "traffic"
			resourceType = models.RedeemCodeTypeT
		}

		if err := AdvisoryTransactionLock(ctx, tx, UserResourcePoolLockNamespace, userID); err != nil {
			return LotteryDrawResult{}, err
		}

		if field == "traffic" {
			if _, err := tx.Exec(ctx, `INSERT INTO user_resource_pools (user_id, traffic, updated_at)
				VALUES ($1, $2, NOW())
				ON CONFLICT (user_id) DO UPDATE
				SET traffic = user_resource_pools.traffic + $2, updated_at = NOW()`,
				userID, int64(prize.Value)); err != nil {
				return LotteryDrawResult{}, err
			}
		} else {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO user_resource_pools (user_id, %s, updated_at)
				VALUES ($1, $2, NOW())
				ON CONFLICT (user_id) DO UPDATE
				SET %s = user_resource_pools.%s + $2, updated_at = NOW()`, field, field, field),
				userID, prize.Value); err != nil {
				return LotteryDrawResult{}, err
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO resource_pool_logs
			(user_id, action, resource_type, amount, remark, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())`,
			userID, string(models.ResourcePoolActionLottery), string(resourceType), prize.Value,
			"抽奖中奖："+prize.Name); err != nil {
			return LotteryDrawResult{}, err
		}
	}

	deliveredAt := (*time.Time)(nil)
	if status == models.LotteryRecordStatusDelivered {
		value := time.Now().UTC()
		deliveredAt = &value
	}

	var (
		recordID        int32
		recordCreatedAt time.Time
	)
	if err := tx.QueryRow(ctx, `INSERT INTO lottery_records
		(lottery_id, prize_id, user_id, prize_type, prize_value, prize_name, status,
		 points_spent, delivered_at, notification_sent, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, false, NOW())
		RETURNING id, created_at`,
		lotteryID, prize.ID, userID, string(prize.Type), resolvedPrizeValue, resolvedPrizeName,
		string(status), lottery.CostPoints, deliveredAt).Scan(&recordID, &recordCreatedAt); err != nil {
		return LotteryDrawResult{}, err
	}

	record := models.LotteryRecord{
		ID:               recordID,
		LotteryID:        lotteryID,
		PrizeID:          prize.ID,
		UserID:           userID,
		PrizeType:        prize.Type,
		PrizeValue:       resolvedPrizeValue,
		PrizeName:        resolvedPrizeName,
		Status:           status,
		PointsSpent:      lottery.CostPoints,
		DeliveredAt:      deliveredAt,
		NotificationSent: false,
		CreatedAt:        recordCreatedAt,
	}

	if _, err := tx.Exec(ctx, `INSERT INTO points_logs
		(user_id, type, amount, points_before, points_after, related_id, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		userID, string(models.PointsLogTypeLotterySpend), -lottery.CostPoints,
		userPoints.Points, userPoints.Points-lottery.CostPoints, recordID,
		"抽奖消耗："+lottery.Name); err != nil {
		return LotteryDrawResult{}, err
	}

	if prize.Type == models.LotteryPrizeTypePoints {
		if _, err := tx.Exec(ctx, `INSERT INTO points_logs
			(user_id, type, amount, points_before, points_after, related_id, remark, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
			userID, string(models.PointsLogTypeLotteryWin), prize.Value,
			userPoints.Points-lottery.CostPoints, userPoints.Points-lottery.CostPoints+prize.Value,
			recordID, "抽奖中奖："+prize.Name); err != nil {
			return LotteryDrawResult{}, err
		}
	}

	return LotteryDrawResult{
		Success: true,
		Record:  &record,
		Prize: &LotteryDrawPrize{
			ID:             prize.ID,
			Name:           resolvedPrizeName,
			Type:           prize.Type,
			Value:          resolvedPrizeValue,
			InstanceDesc:   prize.InstanceDesc,
			BadgeOwnership: badgeOwnership,
		},
		CurrentPoints: currentPoints,
	}, nil
}

// ==================== points.ts ====================

// LotteryGetUserPoints mirrors getUserPoints (ensureUserPoints + row).
func LotteryGetUserPoints(ctx context.Context, userID int32) (models.UserPoints, error) {
	return lotteryEnsureUserPoints(ctx, Pool(), userID)
}

// LotteryGetConvertibleConsume mirrors getConvertibleConsume.
func LotteryGetConvertibleConsume(ctx context.Context, userID int32) (LotteryConvertibleConsume, error) {
	userPoints, err := LotteryGetUserPoints(ctx, userID)
	if err != nil {
		return LotteryConvertibleConsume{}, err
	}

	totalConsume, err := GetUserTotalConsume(ctx, userID, nil)
	if err != nil {
		return LotteryConvertibleConsume{}, err
	}

	convertedConsume, _ := userPoints.ConvertedConsume.Decimal.Float64()
	convertibleAmount := math.Max(0, totalConsume-convertedConsume)
	convertiblePoints := int64(math.Floor(convertibleAmount * 100))

	return LotteryConvertibleConsume{
		TotalConsume:      totalConsume,
		ConvertedConsume:  convertedConsume,
		ConvertibleAmount: convertibleAmount,
		ConvertiblePoints: convertiblePoints,
	}, nil
}

// lotteryIsSafePointsAmount mirrors isSafePointsAmount.
func lotteryIsSafePointsAmount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) &&
		value > 0 && value <= 1_000_000_000
}

// lotteryIsSafePointsBalance mirrors isSafePointsBalance.
func lotteryIsSafePointsBalance(value int64) bool {
	return value >= 0 && value <= 2_147_483_647
}

// lotteryAdvisoryLockOnGormTx issues pg_advisory_xact_lock on the *sql.Tx that
// backs a GORM transaction, so the lock shares the transaction's lifetime.
func lotteryAdvisoryLockOnGormTx(ctx context.Context, tx *gorm.DB, namespace, key int32) error {
	execer, ok := tx.Statement.ConnPool.(interface {
		ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	})
	if !ok {
		return errors.New("unsupported gorm connection pool")
	}
	_, err := execer.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1, $2)", namespace, key)
	return err
}

// LotteryConvertPointsFromConsumption mirrors convertPointsFromConsumption.
//
// The advisory lock and all writes share one GORM transaction so
// GetUserTotalConsume (which reads balance_logs through the same handle) observes
// the same snapshot the TypeScript obtains from its TransactionClient.
func LotteryConvertPointsFromConsumption(ctx context.Context, userID int32) (LotteryConvertResult, error) {
	if DB == nil {
		return LotteryConvertResult{}, errors.New("database not initialized")
	}

	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return LotteryConvertResult{}, tx.Error
	}
	defer func() {
		if tx.Error != nil {
			tx.Rollback()
		}
	}()

	if err := lotteryAdvisoryLockOnGormTx(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		tx.Rollback()
		return LotteryConvertResult{}, err
	}

	userPoints, err := badgesGetUserPointsGorm(ctx, tx, userID)
	if err != nil {
		tx.Rollback()
		return LotteryConvertResult{}, err
	}

	totalConsume, err := GetUserTotalConsume(ctx, userID, tx)
	if err != nil {
		tx.Rollback()
		return LotteryConvertResult{}, err
	}

	alreadyConverted, _ := userPoints.ConvertedConsume.Decimal.Float64()
	newConvertableConsume := totalConsume - alreadyConverted
	if newConvertableConsume <= 0 {
		tx.Rollback()
		return LotteryConvertResult{
			Success: false, Converted: 0, NewPoints: userPoints.Points,
			ConsumeConverted: 0, Message: "暂无可兑换的消费额",
		}, nil
	}

	newPoints := math.Floor(newConvertableConsume * 100)
	if !lotteryIsSafePointsAmount(newPoints) {
		tx.Rollback()
		return LotteryConvertResult{
			Success: false, Converted: 0, NewPoints: userPoints.Points,
			ConsumeConverted: 0, Message: "可兑换积分数量超出系统允许范围",
		}, nil
	}
	newPointsValue := int64(newPoints)
	if !lotteryIsSafePointsBalance(int64(userPoints.Points) + newPointsValue) {
		tx.Rollback()
		return LotteryConvertResult{}, errors.New("积分余额超出系统允许范围")
	}
	if !lotteryIsSafePointsBalance(int64(userPoints.TotalEarned) + newPointsValue) {
		tx.Rollback()
		return LotteryConvertResult{}, errors.New("积分统计超出系统允许范围")
	}

	var updatedPoints int32
	if err := tx.Raw(`UPDATE user_points
		SET points = points + $1, total_earned = total_earned + $1,
			converted_consume = $2, last_converted_at = NOW(), updated_at = NOW()
		WHERE user_id = $3
		RETURNING points`,
		newPointsValue, decimal.NewFromFloat(totalConsume), userID).Scan(&updatedPoints).Error; err != nil {
		tx.Rollback()
		return LotteryConvertResult{}, err
	}

	remark := fmt.Sprintf("消费兑换：¥%.2f → %d积分", newConvertableConsume, newPointsValue)
	if err := tx.Exec(`INSERT INTO points_logs
		(user_id, type, amount, points_before, points_after, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, string(models.PointsLogTypeConvert), newPointsValue, userPoints.Points,
		updatedPoints, remark).Error; err != nil {
		tx.Rollback()
		return LotteryConvertResult{}, err
	}

	if err := tx.Commit().Error; err != nil {
		return LotteryConvertResult{}, err
	}

	return LotteryConvertResult{
		Success:          true,
		Converted:        int32(newPointsValue),
		NewPoints:        updatedPoints,
		ConsumeConverted: newConvertableConsume,
	}, nil
}

// badgesGetUserPointsGorm mirrors badgesGetUserPoints on a GORM transaction so
// convertPointsFromConsumption can reuse GetUserTotalConsume's *gorm.DB contract.
func badgesGetUserPointsGorm(ctx context.Context, tx *gorm.DB, userID int32) (models.UserPoints, error) {
	var userPoints models.UserPoints
	err := tx.WithContext(ctx).Where("user_id = ?", userID).First(&userPoints).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if insertErr := tx.WithContext(ctx).Exec(
			`INSERT INTO user_points (user_id, updated_at) VALUES (?, NOW()) ON CONFLICT (user_id) DO NOTHING`,
			userID).Error; insertErr != nil {
			return models.UserPoints{}, insertErr
		}
		err = tx.WithContext(ctx).Where("user_id = ?", userID).First(&userPoints).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return models.UserPoints{}, LotteryErrUserPointsFailed
		}
	}
	if err != nil {
		return models.UserPoints{}, err
	}
	return userPoints, nil
}

// LotteryGetPointsLogs mirrors getPointsLogs.
func LotteryGetPointsLogs(ctx context.Context, userID int32, options LotteryPointsLogOptions) (LotteryPointsLogListResult, error) {
	page, pageSize := lotteryClampPagination(options.Page, options.PageSize)
	logType := options.Type
	if logType != "" {
		normalized := ""
		switch logType {
		case models.PointsLogTypeConvert, models.PointsLogTypeLotteryWin,
			models.PointsLogTypeLotterySpend, models.PointsLogTypeAdminAdjust,
			models.PointsLogTypeCheckin, models.PointsLogTypeBadgeDrawSpend,
			models.PointsLogTypeBadgeSelectSpend, models.PointsLogTypeInviteGenerate,
			models.PointsLogTypeVipBenefit:
			normalized = string(logType)
		}
		logType = models.PointsLogType(normalized)
	}
	skip := (page - 1) * pageSize

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.PointsLog{}).Where("user_id = ?", userID)
		if logType != "" {
			query = query.Where("type = ?", string(logType))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return LotteryPointsLogListResult{}, err
	}

	var logs []models.PointsLog
	if err := build().Order("created_at DESC").Offset(skip).Limit(pageSize).Find(&logs).Error; err != nil {
		return LotteryPointsLogListResult{}, err
	}
	if logs == nil {
		logs = []models.PointsLog{}
	}

	return LotteryPointsLogListResult{Logs: logs, Total: total, Page: page, PageSize: pageSize}, nil
}
