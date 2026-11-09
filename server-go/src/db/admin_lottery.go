// Admin-side lottery / prize / records / user-points persistence, ported from
// server/src/db/lottery.ts (createLottery / updateLottery / deleteLottery /
// getAllLotteries / getLotteryStats / createPrize / updatePrize / deletePrize /
// getPrizeById / upsertLotteryNotificationConfig / getAllLotteryRecords /
// updateLotteryRecordStatus), server/src/db/points.ts (getAllUserPoints /
// adminAdjustPoints) and the admin slice of server/src/db/checkin.ts
// (updateDailyCheckinSettings / getAdminDailyCheckinLogs).
//
// Everything that the user-facing draw engine already needs lives in
// db/lottery.go; this file only adds the管理端 CRUD that was deliberately left
// unported. All package-level identifiers carry the `AdminLottery` / `adminLottery`
// prefix because the db package is shared by parallel module ports.
//
// Transaction semantics worth calling out:
//   - createLottery / updateLottery / deleteLottery / prize CRUD and the
//     notification-config upsert are single-row Prisma writes in the TypeScript.
//     Prisma always writes `updatedAt` on update (even when the payload is empty),
//     so the Go statements always set it too. lottery_prizes has NO updated_at
//     column, so prize updates must not touch one.
//   - createPrize stores remainQuantity = totalQuantity ("初始剩余等于总数").
//   - adminAdjustPoints serialises on pg_advisory_xact_lock(USER_POINTS_LOCK_NAMESPACE,
//     userId) and upserts user_points first (`upsert` with an empty update), exactly
//     like points.ts.
//   - getAllLotteries / getAllLotteryRecords / getAllUserPoints / the admin checkin
//     log read clamp pagination with the lottery.ts helper: page<=0 -> 1,
//     pageSize<=0 -> 20, pageSize>100 -> 100.
//
// Ordering: Prisma's `include` without orderBy leaves the relation rows in heap
// order. The Go port pins them with `ORDER BY id ASC` (scopes) or
// `ORDER BY display_order ASC, id ASC` (prizes), matching the deterministic order
// db/lottery.go already uses, so repeated calls cannot shuffle rows.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// adminLotteryMaxPointsBalance mirrors MAX_POINTS_BALANCE in points.ts.
const adminLotteryMaxPointsBalance = 2147483647

// adminLotteryMaxPointsMutationAmount mirrors MAX_POINTS_MUTATION_AMOUNT.
const adminLotteryMaxPointsMutationAmount = 1_000_000_000

// ==================== types ====================

// AdminLotteryInput mirrors createLottery's `data` argument. A nil Description /
// StartAt / EndAt means Prisma `undefined` (column keeps its default / stays null);
// a nil IsActive means `isActive ?? true`.
type AdminLotteryInput struct {
	Name        string
	Description *string
	CostPoints  int32
	IsActive    *bool
	StartAt     *time.Time
	EndAt       *time.Time
	CreatedBy   int32
}

// AdminLotteryUpdateInput mirrors updateLottery's partial `data` argument. The
// Set flags distinguish Prisma `undefined` (skip the column) from an explicit null.
type AdminLotteryUpdateInput struct {
	Name        *string
	Description *string
	CostPoints  *int32
	IsActive    *bool
	StartAtSet  bool
	StartAt     *time.Time
	EndAtSet    bool
	EndAt       *time.Time
}

// AdminLotteryListOptions mirrors getAllLotteries' options object.
type AdminLotteryListOptions struct {
	Page     int64
	PageSize int64
	IsActive *bool
}

// AdminLotteryListResult mirrors getAllLotteries' return shape.
type AdminLotteryListResult struct {
	Lotteries []LotteryWithPrizes
	Total     int64
	Page      int64
	PageSize  int64
}

// AdminLotteryPrizeStat mirrors one entry of getLotteryStats' prizeStats.
type AdminLotteryPrizeStat struct {
	Type  models.LotteryPrizeType `json:"type"`
	Count int64                   `json:"count"`
}

// AdminLotteryStats mirrors getLotteryStats' return shape.
type AdminLotteryStats struct {
	TotalDraws int64                   `json:"totalDraws"`
	PrizeStats []AdminLotteryPrizeStat `json:"prizeStats"`
}

// AdminLotteryPrizeInput mirrors createPrize's `data` argument.
type AdminLotteryPrizeInput struct {
	LotteryID     int32
	Name          string
	Type          models.LotteryPrizeType
	Value         int32
	Probability   models.Decimal
	TotalQuantity *int32
	DisplayOrder  int32
	InstanceDesc  *string
}

// AdminLotteryPrizeUpdate mirrors updatePrize's partial `data` argument.
type AdminLotteryPrizeUpdate struct {
	Name              *string
	Type              *models.LotteryPrizeType
	Value             *int32
	Probability       *models.Decimal
	TotalQuantitySet  bool
	TotalQuantity     *int32
	RemainQuantitySet bool
	RemainQuantity    *int32
	DisplayOrder      *int32
	InstanceDescSet   bool
	InstanceDesc      *string
}

// AdminLotteryPrizeWithCount mirrors getPrizeById:
//
//	include: { lottery: true, _count: { select: { records: true } } }
type AdminLotteryPrizeWithCount struct {
	models.LotteryPrize
	RecordsCount int64
}

// AdminLotteryNotificationConfigInput mirrors the `data` argument of
// upsertLotteryNotificationConfig.
type AdminLotteryNotificationConfigInput struct {
	Enabled        bool
	Type           string
	Config         []byte
	NotifyBalance  bool
	NotifyInstance bool
}

// AdminLotteryRecordOptions mirrors getAllLotteryRecords' options object.
type AdminLotteryRecordOptions struct {
	Page      int64
	PageSize  int64
	LotteryID *int32
	PrizeType models.LotteryPrizeType    // "" means absent
	Status    models.LotteryRecordStatus // "" means absent
	Search    *string
}

// AdminLotteryRecordRow is one row of getAdminLotteryRecords' includes: the record
// plus lottery.name, user.username/avatarStyle and prize.instanceDesc.
type AdminLotteryRecordRow struct {
	Record       models.LotteryRecord
	LotteryName  string
	Username     string
	UserAvatar   string
	InstanceDesc *string
}

// AdminLotteryRecordListResult mirrors getAllLotteryRecords' return shape.
type AdminLotteryRecordListResult struct {
	Records  []AdminLotteryRecordRow
	Total    int64
	Page     int64
	PageSize int64
}

// AdminLotteryPointsOptions mirrors getAllUserPoints' options object.
type AdminLotteryPointsOptions struct {
	Page     int64
	PageSize int64
	Search   *string
	OrderBy  string // "points" | "totalEarned" | "totalSpent"
	Order    string // "asc" | "desc"
}

// AdminLotteryPointsRow is one user_points row plus the selected user columns.
type AdminLotteryPointsRow struct {
	UserPoints models.UserPoints
	Username   string
	UserAvatar string
}

// AdminLotteryPointsListResult mirrors getAllUserPoints' return shape.
type AdminLotteryPointsListResult struct {
	Records  []AdminLotteryPointsRow
	Total    int64
	Page     int64
	PageSize int64
}

// AdminLotteryAdjustResult mirrors adminAdjustPoints' return shape.
type AdminLotteryAdjustResult struct {
	Success   bool
	NewPoints int32
	Message   string
}

// AdminLotteryCheckinLogOptions mirrors getAdminDailyCheckinLogs' options object.
type AdminLotteryCheckinLogOptions struct {
	Page     int64
	PageSize int64
	Search   *string
	DateKey  *string
}

// AdminLotteryCheckinLogRow is the projected daily_checkins row of
// getAdminDailyCheckinLogs (createdAt is kept as a time so the route can format it).
type AdminLotteryCheckinLogRow struct {
	ID         int32
	UserID     int32
	Username   string
	UserAvatar string
	DateKey    string
	Points     int32
	StreakDays int32
	IPAddress  *string
	UserAgent  *string
	CreatedAt  time.Time
}

// AdminLotteryCheckinLogResult mirrors getAdminDailyCheckinLogs' return shape.
type AdminLotteryCheckinLogResult struct {
	Records  []AdminLotteryCheckinLogRow
	Total    int64
	Page     int64
	PageSize int64
}

// AdminLotteryCheckinSettingsInput mirrors updateDailyCheckinSettings' partial
// input. Nil means "keep the current value".
type AdminLotteryCheckinSettingsInput struct {
	Enabled         *bool
	MinPoints       *int
	MaxPoints       *int
	RequireInstance *bool
}

// ==================== helpers ====================

// adminLotteryClampPagination mirrors clampPagination from lottery.ts.
func adminLotteryClampPagination(page, pageSize int64) (int64, int64) {
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

// adminLotteryListColumns is lotteryColumns with explicit table aliases so the
// admin list can join the per-lottery record count.
const adminLotteryListColumns = `l.id, l.name, l.description, l.cost_points, l.is_active,
	l.start_at, l.end_at, l.total_draws, l.created_at, l.updated_at, l.created_by`

func adminLotteryScanRow(row pgx.Row) (models.Lottery, error) {
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
	if err := row.Scan(&id, &name, &description, &costPoints, &isActive, &startAt, &endAt,
		&totalDraws, &createdAt, &updatedAt, &createdBy); err != nil {
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

// adminLotteryLoadPrizes mirrors the `include: { prizes: { orderBy: { displayOrder: 'asc' } } }`
// relation read the admin list/detail performs.
func adminLotteryLoadPrizes(ctx context.Context, q BadgesQuerier, lotteryID int32) ([]models.LotteryPrize, error) {
	prizes, err := lotteryLoadPrizes(ctx, q, lotteryID, "ORDER BY display_order ASC, id ASC")
	if err != nil {
		return nil, err
	}
	if prizes == nil {
		prizes = []models.LotteryPrize{}
	}
	return prizes, nil
}

// ==================== lotteries ====================

// AdminLotteryCreate mirrors createLottery. description/startAt/endAt are only
// written when provided; isActive defaults to true.
func AdminLotteryCreate(ctx context.Context, input AdminLotteryInput) (models.Lottery, error) {
	isActive := true
	if input.IsActive != nil {
		isActive = *input.IsActive
	}

	return adminLotteryScanRow(Pool().QueryRow(ctx, `INSERT INTO lotteries
		(name, description, cost_points, is_active, start_at, end_at, created_at, updated_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW(), $7)
		RETURNING `+lotteryColumns,
		input.Name, input.Description, input.CostPoints, isActive, input.StartAt, input.EndAt, input.CreatedBy))
}

// AdminLotteryUpdate mirrors updateLottery. Prisma still bumps @updatedAt when the
// payload carries no updatable field, so the SET list always includes updated_at.
func AdminLotteryUpdate(ctx context.Context, id int32, input AdminLotteryUpdateInput) (models.Lottery, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 8)
	add := func(expression string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", expression, len(args)))
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.Description != nil {
		add("description", *input.Description)
	}
	if input.CostPoints != nil {
		add("cost_points", *input.CostPoints)
	}
	if input.IsActive != nil {
		add("is_active", *input.IsActive)
	}
	if input.StartAtSet {
		add("start_at", input.StartAt)
	}
	if input.EndAtSet {
		add("end_at", input.EndAt)
	}
	add("updated_at", time.Now().UTC())

	args = append(args, id)
	return adminLotteryScanRow(Pool().QueryRow(ctx, `UPDATE lotteries SET `+strings.Join(sets, ", ")+
		` WHERE id = $`+fmt.Sprint(len(args))+` RETURNING `+lotteryColumns, args...))
}

// AdminLotteryDelete mirrors deleteLottery. The schema cascades to prizes,
// records and the notification config.
func AdminLotteryDelete(ctx context.Context, id int32) (models.Lottery, error) {
	return adminLotteryScanRow(Pool().QueryRow(ctx,
		`DELETE FROM lotteries WHERE id = $1 RETURNING `+lotteryColumns, id))
}

// AdminLotteryGetAll mirrors getAllLotteries (newest first, prizes by displayOrder,
// plus the live record count per lottery).
func AdminLotteryGetAll(ctx context.Context, options AdminLotteryListOptions) (AdminLotteryListResult, error) {
	page, pageSize := adminLotteryClampPagination(options.Page, options.PageSize)
	skip := (page - 1) * pageSize

	where := ""
	args := make([]any, 0, 3)
	if options.IsActive != nil {
		args = append(args, *options.IsActive)
		where = "WHERE l.is_active = $1"
	}

	var total int64
	if err := Pool().QueryRow(ctx, "SELECT COUNT(*) FROM lotteries l "+where, args...).Scan(&total); err != nil {
		return AdminLotteryListResult{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, skip)
	rows, err := Pool().Query(ctx, "SELECT "+adminLotteryListColumns+
		", (SELECT COUNT(*) FROM lottery_records r WHERE r.lottery_id = l.id)"+
		" FROM lotteries l "+where+
		" ORDER BY l.created_at DESC LIMIT $"+fmt.Sprint(len(args)+1)+" OFFSET $"+fmt.Sprint(len(args)+2),
		listArgs...)
	if err != nil {
		return AdminLotteryListResult{}, err
	}

	var (
		lotteries []models.Lottery
		counts    []int64
	)
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
			records     int64
		)
		if err := rows.Scan(&id, &name, &description, &costPoints, &isActive, &startAt, &endAt,
			&totalDraws, &createdAt, &updatedAt, &createdBy, &records); err != nil {
			rows.Close()
			return AdminLotteryListResult{}, err
		}
		lotteries = append(lotteries, models.Lottery{
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
		})
		counts = append(counts, records)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return AdminLotteryListResult{}, err
	}
	rows.Close()

	result := make([]LotteryWithPrizes, 0, len(lotteries))
	for index, lottery := range lotteries {
		prizes, err := adminLotteryLoadPrizes(ctx, Pool(), lottery.ID)
		if err != nil {
			return AdminLotteryListResult{}, err
		}
		result = append(result, LotteryWithPrizes{Lottery: lottery, Prizes: prizes, TotalDraws: counts[index]})
	}

	return AdminLotteryListResult{Lotteries: result, Total: total, Page: page, PageSize: pageSize}, nil
}

// AdminLotteryGetStats mirrors getLotteryStats. The TS groupBy carries no orderBy
// (PostgreSQL picks the aggregate order); the Go port orders by the enum column so
// repeated calls are stable, which is the only observable difference.
func AdminLotteryGetStats(ctx context.Context, lotteryID int32) (AdminLotteryStats, error) {
	var total int64
	if err := Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM lottery_records WHERE lottery_id = $1", lotteryID).Scan(&total); err != nil {
		return AdminLotteryStats{}, err
	}

	rows, err := Pool().Query(ctx, `SELECT prize_type::text, COUNT(*)
		FROM lottery_records WHERE lottery_id = $1
		GROUP BY prize_type ORDER BY prize_type`, lotteryID)
	if err != nil {
		return AdminLotteryStats{}, err
	}
	defer rows.Close()

	stats := make([]AdminLotteryPrizeStat, 0, 4)
	for rows.Next() {
		var (
			prizeType string
			count     int64
		)
		if err := rows.Scan(&prizeType, &count); err != nil {
			return AdminLotteryStats{}, err
		}
		stats = append(stats, AdminLotteryPrizeStat{Type: models.LotteryPrizeType(prizeType), Count: count})
	}
	if err := rows.Err(); err != nil {
		return AdminLotteryStats{}, err
	}

	return AdminLotteryStats{TotalDraws: total, PrizeStats: stats}, nil
}

// ==================== prizes ====================

// AdminLotteryCreatePrize mirrors createPrize: remainQuantity starts equal to
// totalQuantity.
func AdminLotteryCreatePrize(ctx context.Context, input AdminLotteryPrizeInput) (models.LotteryPrize, error) {
	row, err := lotteryScanPrizeRow(Pool().QueryRow(ctx, `INSERT INTO lottery_prizes
		(lottery_id, name, type, value, probability, total_quantity, remain_quantity,
		 display_order, created_at, instance_desc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), $9)
		RETURNING `+lotteryPrizeColumns,
		input.LotteryID, input.Name, string(input.Type), input.Value,
		input.Probability.Decimal, input.TotalQuantity, input.TotalQuantity,
		input.DisplayOrder, input.InstanceDesc))
	return row, err
}

// lotteryScanPrizeRow scans one lottery_prizes row using lotteryPrizeColumns.
func lotteryScanPrizeRow(row pgx.Row) (models.LotteryPrize, error) {
	var (
		id             int32
		lotteryID      int32
		name           string
		prizeType      string
		value          int32
		probability    models.Decimal
		totalQuantity  *int32
		remainQuantity *int32
		displayOrder   int32
		createdAt      time.Time
		instanceDesc   *string
	)
	if err := row.Scan(&id, &lotteryID, &name, &prizeType, &value, &probability,
		&totalQuantity, &remainQuantity, &displayOrder, &createdAt, &instanceDesc); err != nil {
		return models.LotteryPrize{}, err
	}
	return models.LotteryPrize{
		ID:             id,
		LotteryID:      lotteryID,
		Name:           name,
		Type:           models.LotteryPrizeType(prizeType),
		Value:          value,
		Probability:    probability,
		TotalQuantity:  totalQuantity,
		RemainQuantity: remainQuantity,
		DisplayOrder:   displayOrder,
		CreatedAt:      createdAt,
		InstanceDesc:   instanceDesc,
	}, nil
}

// AdminLotteryGetPrizeByID mirrors getPrizeById (prize + record count). The
// `include: { lottery: true }` is not loaded because the only caller re-reads the
// lottery through AdminLotteryGetByID anyway.
func AdminLotteryGetPrizeByID(ctx context.Context, id int32) (*AdminLotteryPrizeWithCount, error) {
	prize, err := lotteryScanPrizeRow(Pool().QueryRow(ctx,
		"SELECT "+lotteryPrizeColumns+" FROM lottery_prizes WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var count int64
	if err := Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM lottery_records WHERE prize_id = $1", id).Scan(&count); err != nil {
		return nil, err
	}
	return &AdminLotteryPrizeWithCount{LotteryPrize: prize, RecordsCount: count}, nil
}

// AdminLotteryUpdatePrize mirrors updatePrize. lottery_prizes has no updated_at
// column, so only the provided fields are written.
func AdminLotteryUpdatePrize(ctx context.Context, id int32, input AdminLotteryPrizeUpdate) (models.LotteryPrize, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 8)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.Type != nil {
		add("type", string(*input.Type))
	}
	if input.Value != nil {
		add("value", *input.Value)
	}
	if input.Probability != nil {
		add("probability", input.Probability.Decimal)
	}
	if input.TotalQuantitySet {
		add("total_quantity", input.TotalQuantity)
	}
	if input.RemainQuantitySet {
		add("remain_quantity", input.RemainQuantity)
	}
	if input.DisplayOrder != nil {
		add("display_order", *input.DisplayOrder)
	}
	if input.InstanceDescSet {
		add("instance_desc", input.InstanceDesc)
	}
	if len(sets) == 0 {
		// Prisma's update still touches the row (no @updatedAt column here, so the
		// only observable effect is the P2025 miss). Re-select to surface "gone".
		return lotteryScanPrizeRow(Pool().QueryRow(ctx,
			"SELECT "+lotteryPrizeColumns+" FROM lottery_prizes WHERE id = $1", id))
	}

	args = append(args, id)
	return lotteryScanPrizeRow(Pool().QueryRow(ctx, `UPDATE lottery_prizes SET `+strings.Join(sets, ", ")+
		` WHERE id = $`+fmt.Sprint(len(args))+` RETURNING `+lotteryPrizeColumns, args...))
}

// AdminLotteryDeletePrize mirrors deletePrize.
func AdminLotteryDeletePrize(ctx context.Context, id int32) (models.LotteryPrize, error) {
	return lotteryScanPrizeRow(Pool().QueryRow(ctx,
		`DELETE FROM lottery_prizes WHERE id = $1 RETURNING `+lotteryPrizeColumns, id))
}

// ==================== notification config ====================

// AdminLotteryUpsertNotificationConfig mirrors upsertLotteryNotificationConfig.
func AdminLotteryUpsertNotificationConfig(ctx context.Context, lotteryID int32, input AdminLotteryNotificationConfigInput) (models.LotteryNotificationConfig, error) {
	var config models.LotteryNotificationConfig
	err := DB.WithContext(ctx).Raw(`INSERT INTO lottery_notification_configs
		(lottery_id, enabled, type, config, notify_balance, notify_instance, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, NOW(), NOW())
		ON CONFLICT (lottery_id) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			type = EXCLUDED.type,
			config = EXCLUDED.config,
			notify_balance = EXCLUDED.notify_balance,
			notify_instance = EXCLUDED.notify_instance,
			updated_at = NOW()
		RETURNING id, lottery_id, enabled, type, config, notify_balance, notify_instance, created_at, updated_at`,
		lotteryID, input.Enabled, input.Type, string(input.Config), input.NotifyBalance, input.NotifyInstance,
	).Scan(&config).Error
	if err != nil {
		return models.LotteryNotificationConfig{}, err
	}
	return config, nil
}

// ==================== records ====================

// AdminLotteryGetAllRecords mirrors getAllLotteryRecords. `search` is matched
// against users.username with Prisma's case-insensitive contains.
func AdminLotteryGetAllRecords(ctx context.Context, options AdminLotteryRecordOptions) (AdminLotteryRecordListResult, error) {
	page, pageSize := adminLotteryClampPagination(options.Page, options.PageSize)
	skip := (page - 1) * pageSize

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if options.LotteryID != nil {
		args = append(args, *options.LotteryID)
		conditions = append(conditions, "r.lottery_id = $"+fmt.Sprint(len(args)))
	}
	if options.PrizeType != "" {
		args = append(args, string(options.PrizeType))
		conditions = append(conditions, "r.prize_type = $"+fmt.Sprint(len(args)))
	}
	if options.Status != "" {
		args = append(args, string(options.Status))
		conditions = append(conditions, "r.status = $"+fmt.Sprint(len(args)))
	}
	if options.Search != nil {
		// `search.trim().slice(0, 128)` — the slice counts UTF-16 units like V8.
		keyword := CheckinJSSlice(ticketsJSTrim(*options.Search), 128)
		if keyword != "" {
			args = append(args, keyword)
			conditions = append(conditions, "u.username ILIKE '%' || $"+fmt.Sprint(len(args))+" || '%'")
		}
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM lottery_records r
		JOIN users u ON u.id = r.user_id `+where, args...).Scan(&total); err != nil {
		return AdminLotteryRecordListResult{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, skip)
	rows, err := Pool().Query(ctx, `SELECT r.id, r.lottery_id, r.prize_id, r.user_id, r.prize_type::text,
			r.prize_value, r.prize_name, r.status::text, r.points_spent, r.delivered_at, r.delivered_by,
			r.ticket_id, r.notification_sent, r.created_at,
			l.name, u.username, u.avatar_style, p.instance_desc
		FROM lottery_records r
		JOIN lotteries l ON l.id = r.lottery_id
		JOIN users u ON u.id = r.user_id
		LEFT JOIN lottery_prizes p ON p.id = r.prize_id
		`+where+`
		ORDER BY r.created_at DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return AdminLotteryRecordListResult{}, err
	}
	defer rows.Close()

	records := make([]AdminLotteryRecordRow, 0, pageSize)
	for rows.Next() {
		var (
			id               int32
			lotteryID        int32
			prizeID          int32
			userID           int32
			prizeType        string
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
			username         string
			avatarStyle      string
			instanceDesc     *string
		)
		if err := rows.Scan(&id, &lotteryID, &prizeID, &userID, &prizeType, &prizeValue, &prizeName,
			&status, &pointsSpent, &deliveredAt, &deliveredBy, &ticketID, &notificationSent,
			&createdAt, &lotteryName, &username, &avatarStyle, &instanceDesc); err != nil {
			return AdminLotteryRecordListResult{}, err
		}
		records = append(records, AdminLotteryRecordRow{
			Record: models.LotteryRecord{
				ID:               id,
				LotteryID:        lotteryID,
				PrizeID:          prizeID,
				UserID:           userID,
				PrizeType:        models.LotteryPrizeType(prizeType),
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
			Username:     username,
			UserAvatar:   avatarStyle,
			InstanceDesc: instanceDesc,
		})
	}
	if err := rows.Err(); err != nil {
		return AdminLotteryRecordListResult{}, err
	}

	return AdminLotteryRecordListResult{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// AdminLotteryUpdateRecordStatus mirrors updateLotteryRecordStatus: deliveredAt is
// set only for delivered/claimed, and deliveredBy/ticketId are only written when
// provided (Prisma `undefined`).
func AdminLotteryUpdateRecordStatus(ctx context.Context, id int32, status models.LotteryRecordStatus, deliveredBy *int32, ticketID *int32) (models.LotteryRecord, error) {
	// GORM's Raw() only substitutes `?` placeholders.
	sets := []string{"status = ?"}
	args := []any{string(status)}
	if status == models.LotteryRecordStatusDelivered || status == models.LotteryRecordStatusClaimed {
		args = append(args, time.Now().UTC())
		sets = append(sets, "delivered_at = ?")
	}
	if deliveredBy != nil {
		args = append(args, *deliveredBy)
		sets = append(sets, "delivered_by = ?")
	}
	if ticketID != nil {
		args = append(args, *ticketID)
		sets = append(sets, "ticket_id = ?")
	}

	args = append(args, id)
	var record models.LotteryRecord
	err := DB.WithContext(ctx).Raw(`UPDATE lottery_records SET `+strings.Join(sets, ", ")+
		` WHERE id = ? RETURNING `+adminLotteryRecordColumns, args...).Scan(&record).Error
	if err != nil {
		return models.LotteryRecord{}, err
	}
	return record, nil
}

// adminLotteryRecordColumns is the wire-relevant projection of lottery_records.
const adminLotteryRecordColumns = `id, lottery_id, prize_id, user_id, prize_type::text AS prize_type,
	prize_value, prize_name, status::text AS status, points_spent, delivered_at, delivered_by,
	ticket_id, notification_sent, created_at`

// ==================== user points ====================

// adminLotteryPointsOrderColumn whitelists the sortable columns.
var adminLotteryPointsOrderColumn = map[string]string{
	"points":      "up.points",
	"totalEarned": "up.total_earned",
	"totalSpent":  "up.total_spent",
}

// AdminLotteryGetAllUserPoints mirrors getAllUserPoints.
func AdminLotteryGetAllUserPoints(ctx context.Context, options AdminLotteryPointsOptions) (AdminLotteryPointsListResult, error) {
	page, pageSize := adminLotteryClampPagination(options.Page, options.PageSize)
	skip := (page - 1) * pageSize

	orderColumn, ok := adminLotteryPointsOrderColumn[options.OrderBy]
	if !ok {
		orderColumn = "up.points"
	}
	order := "DESC"
	if strings.EqualFold(options.Order, "asc") {
		order = "ASC"
	}

	conditions := make([]string, 0, 1)
	args := make([]any, 0, 1)
	if options.Search != nil {
		// `search.trim().slice(0, 128)` (UTF-16 units, like V8).
		keyword := CheckinJSSlice(ticketsJSTrim(*options.Search), 128)
		if keyword != "" {
			args = append(args, keyword)
			conditions = append(conditions, "u.username ILIKE '%' || $1 || '%'")
		}
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM user_points up JOIN users u ON u.id = up.user_id "+where, args...).Scan(&total); err != nil {
		return AdminLotteryPointsListResult{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, skip)
	rows, err := Pool().Query(ctx, `SELECT up.user_id, up.points, up.total_earned, up.total_spent,
			up.last_converted_at, up.converted_consume, up.created_at, up.updated_at,
			u.username, u.avatar_style
		FROM user_points up
		JOIN users u ON u.id = up.user_id
		`+where+`
		ORDER BY `+orderColumn+` `+order+`
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return AdminLotteryPointsListResult{}, err
	}
	defer rows.Close()

	records := make([]AdminLotteryPointsRow, 0, pageSize)
	for rows.Next() {
		var (
			row     AdminLotteryPointsRow
			userID  int32
			points  int32
			earned  int32
			spent   int32
			lastAt  *time.Time
			convert models.Decimal
			created time.Time
			updated time.Time
		)
		if err := rows.Scan(&userID, &points, &earned, &spent, &lastAt, &convert, &created, &updated,
			&row.Username, &row.UserAvatar); err != nil {
			return AdminLotteryPointsListResult{}, err
		}
		row.UserPoints = models.UserPoints{
			UserID:           userID,
			Points:           points,
			TotalEarned:      earned,
			TotalSpent:       spent,
			LastConvertedAt:  lastAt,
			ConvertedConsume: convert,
			CreatedAt:        created,
			UpdatedAt:        updated,
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return AdminLotteryPointsListResult{}, err
	}

	return AdminLotteryPointsListResult{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// adminLotteryIsSafePointsAdjustment mirrors isSafePointsAdjustment.
func adminLotteryIsSafePointsAdjustment(value int64) bool {
	return value != 0 && value >= -adminLotteryMaxPointsMutationAmount && value <= adminLotteryMaxPointsMutationAmount
}

// AdminLotteryAdjustPoints mirrors adminAdjustPoints.
//
// The advisory lock and every read/write share one transaction; the pre-flight
// rejection (`调整积分数量无效`) happens before the transaction opens, like the
// TypeScript's early return.
func AdminLotteryAdjustPoints(ctx context.Context, userID int32, amount int32, remark string) (AdminLotteryAdjustResult, error) {
	if !adminLotteryIsSafePointsAdjustment(int64(amount)) {
		return AdminLotteryAdjustResult{Success: false, NewPoints: 0, Message: "调整积分数量无效"}, nil
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return AdminLotteryAdjustResult{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := AdvisoryTransactionLock(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		return AdminLotteryAdjustResult{}, err
	}

	userPoints, err := lotteryEnsureUserPoints(ctx, tx, userID)
	if err != nil {
		return AdminLotteryAdjustResult{}, err
	}

	points := int64(userPoints.Points)
	delta := int64(amount)
	totalEarned := int64(userPoints.TotalEarned)
	totalSpent := int64(userPoints.TotalSpent)

	if amount < 0 && points+delta < 0 {
		return AdminLotteryAdjustResult{Success: false, NewPoints: userPoints.Points, Message: "扣除后积分不能为负数"}, nil
	}
	if amount > 0 && points+delta > adminLotteryMaxPointsBalance {
		return AdminLotteryAdjustResult{Success: false, NewPoints: userPoints.Points, Message: "积分余额超出系统允许范围"}, nil
	}
	if amount > 0 && totalEarned+delta > adminLotteryMaxPointsBalance {
		return AdminLotteryAdjustResult{Success: false, NewPoints: userPoints.Points, Message: "积分统计超出系统允许范围"}, nil
	}
	if amount < 0 && totalSpent-delta > adminLotteryMaxPointsBalance {
		return AdminLotteryAdjustResult{Success: false, NewPoints: userPoints.Points, Message: "积分统计超出系统允许范围"}, nil
	}

	// Prisma increments only the counter that matches the sign of the amount.
	earnedDelta := int64(0)
	if amount > 0 {
		earnedDelta = delta
	}
	spentDelta := int64(0)
	if amount < 0 {
		spentDelta = -delta
	}

	var newPoints int32
	if err := tx.QueryRow(ctx, `UPDATE user_points
		SET points = points + $1,
		    total_earned = total_earned + $2,
		    total_spent = total_spent + $3,
		    updated_at = NOW()
		WHERE user_id = $4
		RETURNING points`, delta, earnedDelta, spentDelta, userID).Scan(&newPoints); err != nil {
		return AdminLotteryAdjustResult{}, err
	}

	if _, err := tx.Exec(ctx, `INSERT INTO points_logs
		(user_id, type, amount, points_before, points_after, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, string(models.PointsLogTypeAdminAdjust), amount, userPoints.Points, newPoints, remark); err != nil {
		return AdminLotteryAdjustResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return AdminLotteryAdjustResult{}, err
	}
	return AdminLotteryAdjustResult{Success: true, NewPoints: newPoints}, nil
}

// ==================== daily check-in admin ====================

// AdminLotteryUpdateDailyCheckinSettings mirrors updateDailyCheckinSettings.
func AdminLotteryUpdateDailyCheckinSettings(ctx context.Context, input AdminLotteryCheckinSettingsInput) (CheckinDailySettings, error) {
	current, err := CheckinGetDailyCheckinSettings(ctx)
	if err != nil {
		return CheckinDailySettings{}, err
	}

	next := CheckinDailySettings{
		Enabled:         current.Enabled,
		MinPoints:       current.MinPoints,
		MaxPoints:       current.MaxPoints,
		RequireInstance: current.RequireInstance,
	}
	if input.Enabled != nil {
		next.Enabled = *input.Enabled
	}
	if input.MinPoints != nil {
		next.MinPoints = *input.MinPoints
	}
	if input.MaxPoints != nil {
		next.MaxPoints = *input.MaxPoints
	}
	if input.RequireInstance != nil {
		next.RequireInstance = *input.RequireInstance
	}

	normalized := checkinNormalizeSettings(next)
	if err := UpdateSystemConfigs(ctx, []SystemConfigUpdate{
		{Key: "checkin_enabled", Value: adminLotteryFormatBool(normalized.Enabled)},
		{Key: "checkin_min_points", Value: fmt.Sprint(normalized.MinPoints)},
		{Key: "checkin_max_points", Value: fmt.Sprint(normalized.MaxPoints)},
		{Key: "checkin_require_instance", Value: adminLotteryFormatBool(normalized.RequireInstance)},
	}); err != nil {
		return CheckinDailySettings{}, err
	}
	return normalized, nil
}

// adminLotteryFormatBool mirrors String(boolean) in JavaScript.
func adminLotteryFormatBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// AdminLotteryGetAdminDailyCheckinLogs mirrors getAdminDailyCheckinLogs.
func AdminLotteryGetAdminDailyCheckinLogs(ctx context.Context, options AdminLotteryCheckinLogOptions) (AdminLotteryCheckinLogResult, error) {
	page := options.Page
	if page <= 0 {
		page = 1
	}
	pageSize := options.PageSize
	if pageSize <= 0 {
		pageSize = lotteryDefaultPageSize
	}
	if pageSize > lotteryMaxPageSize {
		pageSize = lotteryMaxPageSize
	}
	skip := (page - 1) * pageSize

	conditions := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if options.DateKey != nil && *options.DateKey != "" {
		args = append(args, *options.DateKey)
		conditions = append(conditions, "dc.date_key = $"+fmt.Sprint(len(args)))
	}
	if options.Search != nil && *options.Search != "" {
		args = append(args, *options.Search)
		conditions = append(conditions, "u.username ILIKE '%' || $"+fmt.Sprint(len(args))+" || '%'")
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM daily_checkins dc
		JOIN users u ON u.id = dc.user_id `+where, args...).Scan(&total); err != nil {
		return AdminLotteryCheckinLogResult{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, skip)
	rows, err := Pool().Query(ctx, `SELECT dc.id, dc.user_id, dc.date_key, dc.points, dc.streak_days,
			dc.ip_address, dc.user_agent, dc.created_at, u.username, u.avatar_style
		FROM daily_checkins dc
		JOIN users u ON u.id = dc.user_id
		`+where+`
		ORDER BY dc.created_at DESC
		LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2), listArgs...)
	if err != nil {
		return AdminLotteryCheckinLogResult{}, err
	}
	defer rows.Close()

	records := make([]AdminLotteryCheckinLogRow, 0, pageSize)
	for rows.Next() {
		var row AdminLotteryCheckinLogRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.DateKey, &row.Points, &row.StreakDays,
			&row.IPAddress, &row.UserAgent, &row.CreatedAt, &row.Username, &row.UserAvatar); err != nil {
			return AdminLotteryCheckinLogResult{}, err
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return AdminLotteryCheckinLogResult{}, err
	}

	return AdminLotteryCheckinLogResult{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// ==================== user lookups ====================

// AdminLotteryUserSummary mirrors `prisma.user.findUnique({ select: { id,
// username, avatarStyle } })`.
type AdminLotteryUserSummary struct {
	ID          int32
	Username    string
	AvatarStyle string
}

// AdminLotteryGetUserSummary loads the user columns the admin entertainment routes
// read. nil, nil means "no such row".
func AdminLotteryGetUserSummary(ctx context.Context, userID int32) (*AdminLotteryUserSummary, error) {
	var summary AdminLotteryUserSummary
	err := DB.WithContext(ctx).Raw(
		`SELECT id, username, avatar_style FROM users WHERE id = ?`, userID,
	).Scan(&summary).Error
	if err != nil {
		return nil, err
	}
	if summary.ID == 0 {
		return nil, nil
	}
	return &summary, nil
}

// AdminLotteryRecordWithUser mirrors the include of the deliver route's
// `lotteryRecord.findUnique({ include: { user: { select: { username: true } } } })`.
type AdminLotteryRecordWithUser struct {
	Record   models.LotteryRecord
	Username string
}

// AdminLotteryGetRecordWithUser loads one record plus its owner's username.
func AdminLotteryGetRecordWithUser(ctx context.Context, id int32) (*AdminLotteryRecordWithUser, error) {
	var row struct {
		ID               int32      `gorm:"column:id"`
		LotteryID        int32      `gorm:"column:lottery_id"`
		PrizeID          int32      `gorm:"column:prize_id"`
		UserID           int32      `gorm:"column:user_id"`
		PrizeType        string     `gorm:"column:prize_type"`
		PrizeValue       int32      `gorm:"column:prize_value"`
		PrizeName        string     `gorm:"column:prize_name"`
		Status           string     `gorm:"column:status"`
		PointsSpent      int32      `gorm:"column:points_spent"`
		DeliveredAt      *time.Time `gorm:"column:delivered_at"`
		DeliveredBy      *int32     `gorm:"column:delivered_by"`
		TicketID         *int32     `gorm:"column:ticket_id"`
		NotificationSent bool       `gorm:"column:notification_sent"`
		CreatedAt        time.Time  `gorm:"column:created_at"`
		Username         string     `gorm:"column:username"`
	}
	err := DB.WithContext(ctx).Raw(`SELECT r.id, r.lottery_id, r.prize_id, r.user_id,
			r.prize_type::text AS prize_type, r.prize_value, r.prize_name, r.status::text AS status,
			r.points_spent, r.delivered_at, r.delivered_by, r.ticket_id, r.notification_sent,
			r.created_at, u.username
		FROM lottery_records r JOIN users u ON u.id = r.user_id
		WHERE r.id = ?`, id).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &AdminLotteryRecordWithUser{
		Record: models.LotteryRecord{
			ID:               row.ID,
			LotteryID:        row.LotteryID,
			PrizeID:          row.PrizeID,
			UserID:           row.UserID,
			PrizeType:        models.LotteryPrizeType(row.PrizeType),
			PrizeValue:       row.PrizeValue,
			PrizeName:        row.PrizeName,
			Status:           models.LotteryRecordStatus(row.Status),
			PointsSpent:      row.PointsSpent,
			DeliveredAt:      row.DeliveredAt,
			DeliveredBy:      row.DeliveredBy,
			TicketID:         row.TicketID,
			NotificationSent: row.NotificationSent,
			CreatedAt:        row.CreatedAt,
		},
		Username: row.Username,
	}, nil
}

// AdminLotteryPendingInstancePrize mirrors one row of getPendingInstancePrizes:
//
//	where: { prizeType: 'instance', status: 'pending' }
//	include: { lottery: { select: { id, name } }, user: { select: { id, username } } }
type AdminLotteryPendingInstancePrize struct {
	Record      models.LotteryRecord
	LotteryName string
	Username    string
}

// AdminLotteryGetPendingInstancePrizes mirrors getPendingInstancePrizes. The
// TypeScript function is exported by db/lottery.ts but no route currently calls it;
// it is ported so the admin lottery surface is complete.
func AdminLotteryGetPendingInstancePrizes(ctx context.Context) ([]AdminLotteryPendingInstancePrize, error) {
	var rows []struct {
		ID               int32      `gorm:"column:id"`
		LotteryID        int32      `gorm:"column:lottery_id"`
		PrizeID          int32      `gorm:"column:prize_id"`
		UserID           int32      `gorm:"column:user_id"`
		PrizeType        string     `gorm:"column:prize_type"`
		PrizeValue       int32      `gorm:"column:prize_value"`
		PrizeName        string     `gorm:"column:prize_name"`
		Status           string     `gorm:"column:status"`
		PointsSpent      int32      `gorm:"column:points_spent"`
		DeliveredAt      *time.Time `gorm:"column:delivered_at"`
		DeliveredBy      *int32     `gorm:"column:delivered_by"`
		TicketID         *int32     `gorm:"column:ticket_id"`
		NotificationSent bool       `gorm:"column:notification_sent"`
		CreatedAt        time.Time  `gorm:"column:created_at"`
		LotteryName      string     `gorm:"column:lottery_name"`
		Username         string     `gorm:"column:username"`
	}
	err := DB.WithContext(ctx).Raw(`SELECT r.id, r.lottery_id, r.prize_id, r.user_id,
			r.prize_type::text AS prize_type, r.prize_value, r.prize_name, r.status::text AS status,
			r.points_spent, r.delivered_at, r.delivered_by, r.ticket_id, r.notification_sent,
			r.created_at, l.name AS lottery_name, u.username
		FROM lottery_records r
		JOIN lotteries l ON l.id = r.lottery_id
		JOIN users u ON u.id = r.user_id
		WHERE r.prize_type = 'instance' AND r.status = 'pending'
		ORDER BY r.created_at ASC`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	prizes := make([]AdminLotteryPendingInstancePrize, 0, len(rows))
	for _, row := range rows {
		prizes = append(prizes, AdminLotteryPendingInstancePrize{
			Record: models.LotteryRecord{
				ID:               row.ID,
				LotteryID:        row.LotteryID,
				PrizeID:          row.PrizeID,
				UserID:           row.UserID,
				PrizeType:        models.LotteryPrizeType(row.PrizeType),
				PrizeValue:       row.PrizeValue,
				PrizeName:        row.PrizeName,
				Status:           models.LotteryRecordStatus(row.Status),
				PointsSpent:      row.PointsSpent,
				DeliveredAt:      row.DeliveredAt,
				DeliveredBy:      row.DeliveredBy,
				TicketID:         row.TicketID,
				NotificationSent: row.NotificationSent,
				CreatedAt:        row.CreatedAt,
			},
			LotteryName: row.LotteryName,
			Username:    row.Username,
		})
	}
	return prizes, nil
}
