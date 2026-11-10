// Port of server/src/db/checkin.ts (the parts routes/checkin.ts consumes).
//
// The check-in claim is a once-per-day, once-per-user consumption path, so it is
// serialized with the PostgreSQL *transaction-scoped* advisory lock named
// USER_POINTS_LOCK_NAMESPACE (4111) held on the very same pgx.Tx as the rows it
// guards. That is exactly what the TypeScript does
// (`advisoryTransactionLock(tx, USER_POINTS_LOCK_NAMESPACE, userId)`), and it is
// the only thing that stops two racing POSTs from minting two point grants: the
// daily_checkins unique constraint is the second line of defence (P2002/23505 is
// mapped to CHECKIN_ALREADY_TODAY, same as the TypeScript).
//
// Money/points are integers. points are Int columns and the TS keeps them as JS
// integers, so they stay int32 here; nothing in this file goes through float64.
//
// Not ported (no caller in the ported routes, kept for the modules that own
// them): performCheckin (legacy resource-pool check-in),
// getCheckinRecordByCode/useRedeemCode/updateOwnerShareStatus (legacy check-in
// code sharing), getAdminDailyCheckinLogs and updateDailyCheckinSettings
// (admin surface).
package db

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sort"
	"time"
	_ "time/tzdata" // keep the Asia/Shanghai lookup independent of host tzdata
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ==================== errors ====================

// Check-in claim sentinel errors. The messages are byte-identical to the
// `throw new Error('...')` payloads in checkin.ts, because routes/checkin.ts
// switches on the error *message*.
var (
	CheckinErrDisabled     = errors.New("CHECKIN_DISABLED")
	CheckinErrAlreadyToday = errors.New("CHECKIN_ALREADY_TODAY")
	CheckinErrNoInstance   = errors.New("CHECKIN_NO_INSTANCE")
	CheckinErrUserBlocked  = errors.New("CHECKIN_USER_BLOCKED")
	// CheckinErrPointsLimitExceeded has no dedicated API error code; routes/checkin.ts
	// falls through to its 500 branch (INTERNAL_ERROR strips the detail).
	CheckinErrPointsLimitExceeded = errors.New("CHECKIN_POINTS_LIMIT_EXCEEDED")
)

// checkinMaxPointsValue mirrors the `currentPoints > 2_147_483_647` guard.
const checkinMaxPointsValue = 2147483647

// checkinBeijingLocation mirrors `{ timeZone: 'Asia/Shanghai' }`.
var checkinBeijingLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}()

// checkinRandomFloat mirrors Math.random. It is a variable so tests can make the
// daily reward deterministic.
var checkinRandomFloat = rand.Float64

// ==================== dates ====================

// CheckinDateKeyInBeijing mirrors
// `date.toLocaleDateString('sv-SE', { timeZone: 'Asia/Shanghai' })` → YYYY-MM-DD.
func CheckinDateKeyInBeijing(moment time.Time) string {
	return moment.In(checkinBeijingLocation).Format("2006-01-02")
}

// checkinYesterdayDateKey mirrors getYesterdayDateKey: the calendar day before
// dateKey, evaluated in Asia/Shanghai.
func checkinYesterdayDateKey(dateKey string) string {
	parsed, err := time.ParseInLocation("2006-01-02", dateKey, checkinBeijingLocation)
	if err != nil {
		// The TS `new Date(\`${dateKey}T00:00:00+08:00\`)` yields Invalid Date here and
		// its locale formatting throws; there is no such failure mode for the values
		// this module produces (always YYYY-MM-DD), so be defensive instead.
		return dateKey
	}
	return parsed.AddDate(0, 0, -1).Format("2006-01-02")
}

// checkinRandomizePoints mirrors randomizePoints:
// Math.floor(Math.random() * (max - min + 1)) + min.
func checkinRandomizePoints(minPoints, maxPoints int) int {
	span := maxPoints - minPoints + 1
	return int(math.Floor(checkinRandomFloat()*float64(span))) + minPoints
}

// CheckinJSSlice mirrors String.prototype.slice(0, limit): the bound counts
// UTF-16 code units, so an astral character consumes two.
func CheckinJSSlice(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		// Fast path: ASCII-only strings (the common case) cannot need UTF-16 fixing.
		return value
	}
	units := utf16.Encode([]rune(value))
	if len(units) <= limit {
		return value
	}
	sliced := units[:limit]
	// Deviation from V8, documented: when the cut lands in the middle of a
	// surrogate pair JavaScript keeps the lone high surrogate, which cannot be
	// represented in Go's UTF-8 strings. Drop the orphaned half instead of emitting
	// U+FFFD so the stored text stays valid UTF-8.
	if last := sliced[len(sliced)-1]; last >= 0xD800 && last <= 0xDBFF {
		sliced = sliced[:len(sliced)-1]
	}
	return string(utf16.Decode(sliced))
}

// checkinNullableSlicedText mirrors `meta.foo?.slice(0, n) || null`.
func checkinNullableSlicedText(value *string, limit int) *string {
	if value == nil {
		return nil
	}
	sliced := CheckinJSSlice(*value, limit)
	if sliced == "" {
		return nil
	}
	return &sliced
}

// ==================== row types ====================

// CheckinMeta mirrors DailyCheckinMeta.
type CheckinMeta struct {
	IPAddress *string
	UserAgent *string
}

// CheckinResult mirrors performDailyPointsCheckin's return value.
type CheckinResult struct {
	ID            int32
	DateKey       string
	Points        int32
	StreakDays    int32
	CurrentPoints int32
}

// CheckinStatusToday is the `today` / recentRecords row projection.
type CheckinStatusToday struct {
	ID         int32  `json:"id"`
	DateKey    string `json:"dateKey"`
	Points     int32  `json:"points"`
	StreakDays int32  `json:"streakDays"`
	CreatedAt  string `json:"createdAt"`
}

// CheckinStatus mirrors getCheckinStatus's return value. Field order is the
// TypeScript object literal order so the wire JSON stays byte-identical.
type CheckinStatus struct {
	Enabled              bool                 `json:"enabled"`
	HasCheckedIn         bool                 `json:"hasCheckedIn"`
	HasInstances         bool                 `json:"hasInstances"`
	RequireInstance      bool                 `json:"requireInstance"`
	MinPoints            int                  `json:"minPoints"`
	MaxPoints            int                  `json:"maxPoints"`
	CurrentPoints        int32                `json:"currentPoints"`
	TotalEarned          int32                `json:"totalEarned"`
	TotalSpent           int32                `json:"totalSpent"`
	Today                *CheckinStatusToday  `json:"today"`
	StreakDays           int32                `json:"streakDays"`
	TotalCheckins        int64                `json:"totalCheckins"`
	MonthCheckins        int64                `json:"monthCheckins"`
	RecentRecords        []CheckinStatusToday `json:"recentRecords"`
	SelfOnlyMode         bool                 `json:"selfOnlyMode"`
	ConsecutiveOthersUse int32                `json:"consecutiveOthersUse"`
}

// CheckinUserRef mirrors the `{id, username}` projections.
type CheckinUserRef struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

// CheckinInstanceRef mirrors the `{id, name}` instance projections.
type CheckinInstanceRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// CheckinRecordItem mirrors one entry of getCheckinRecords().records.
type CheckinRecordItem struct {
	ID         int32               `json:"id"`
	RedeemCode string              `json:"redeemCode"`
	CodeType   string              `json:"codeType"`
	CodeValue  int32               `json:"codeValue"`
	ExpiresAt  string              `json:"expiresAt"`
	UsedAt     *string             `json:"usedAt"`
	UsedBy     *CheckinUserRef     `json:"usedBy"`
	UsedFor    *CheckinInstanceRef `json:"usedFor"`
	CreatedAt  string              `json:"createdAt"`
}

// CheckinRecordsResult mirrors `{records, total}`.
type CheckinRecordsResult struct {
	Records []CheckinRecordItem `json:"records"`
	Total   int64               `json:"total"`
}

// CheckinRedeemRecordItem mirrors one entry of getRedeemRecords().records (the
// merged check-in-code + system-code shape).
type CheckinRedeemRecordItem struct {
	ID           int32               `json:"id"`
	RedeemCode   string              `json:"redeemCode"`
	CodeType     string              `json:"codeType"`
	CodeValue    int32               `json:"codeValue"`
	Owner        CheckinUserRef      `json:"owner"`
	UsedFor      *CheckinInstanceRef `json:"usedFor"`
	UsedAt       *string             `json:"usedAt"`
	IsSystemCode bool                `json:"isSystemCode"`
}

// CheckinRedeemRecordsResult mirrors getRedeemRecords's return value.
type CheckinRedeemRecordsResult struct {
	Records []CheckinRedeemRecordItem `json:"records"`
	Total   int64                     `json:"total"`
}

// CheckinInstancePackage mirrors the package select() of
// getUserInstancesForRedeem.
type CheckinInstancePackage struct {
	ID                  int32   `json:"id"`
	Name                string  `json:"name"`
	CPUMax              int32   `json:"cpuMax"`
	MemoryMax           int32   `json:"memoryMax"`
	DiskMax             int32   `json:"diskMax"`
	MonthlyTrafficLimit *string `json:"monthlyTrafficLimit"`
}

// CheckinInstanceHost mirrors the host select() of getUserInstancesForRedeem.
type CheckinInstanceHost struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Location    *string `json:"location"`
	CountryCode string  `json:"countryCode"`
}

// CheckinRedeemInstanceItem mirrors one entry of getUserInstancesForRedeem.
type CheckinRedeemInstanceItem struct {
	ID                  int32                   `json:"id"`
	Name                string                  `json:"name"`
	Status              string                  `json:"status"`
	CPU                 int32                   `json:"cpu"`
	Memory              int32                   `json:"memory"`
	Disk                int32                   `json:"disk"`
	MonthlyTrafficLimit *string                 `json:"monthlyTrafficLimit"`
	Package             *CheckinInstancePackage `json:"package"`
	Host                CheckinInstanceHost     `json:"host"`
}

// CheckinInstance is the instance projection routes/checkin.ts needs.
type CheckinInstance struct {
	ID      int32
	IncusID string
	Name    string
	UserID  int32
	HostID  int32
	Status  string
	CPU     int32
	Memory  int32
	Disk    int32
}

// CheckinHostConnection is the host projection needed to build an IncusClient.
type CheckinHostConnection struct {
	ID       int32
	URL      string
	CertPath *string
	KeyPath  *string
}

// ==================== helpers ====================

// checkinISOTime mirrors Date.prototype.toISOString().
func checkinISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// checkinIsUniqueViolation mirrors `error.code === 'P2002'`.
func checkinIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CheckinUserHasInstances mirrors userHasInstances: any instance that is not
// deleted or errored.
func CheckinUserHasInstances(ctx context.Context, userID int32) (bool, error) {
	var exists bool
	err := DB.WithContext(ctx).Raw(
		`SELECT EXISTS (SELECT 1 FROM instances WHERE user_id = ? AND status NOT IN ('deleted','error'))`,
		userID,
	).Scan(&exists).Error
	return exists, err
}

// ==================== status ====================

// CheckinGetStatus mirrors getCheckinStatus.
func CheckinGetStatus(ctx context.Context, userID int32) (CheckinStatus, error) {
	if Pool() == nil {
		return CheckinStatus{}, errors.New("checkin: database not initialized")
	}

	now := time.Now().UTC()
	dateKey := CheckinDateKeyInBeijing(now)
	monthKey := ""
	if len(dateKey) >= 7 {
		monthKey = dateKey[:7]
	}

	settings, err := CheckinGetDailyCheckinSettings(ctx)
	if err != nil {
		return CheckinStatus{}, err
	}

	var selfOnlyMode bool
	var consecutiveOthersUse int32
	err = Pool().QueryRow(ctx,
		`SELECT self_only_mode, consecutive_others_use FROM checkin_stats WHERE user_id = $1`, userID,
	).Scan(&selfOnlyMode, &consecutiveOthersUse)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CheckinStatus{}, err
	}

	today, err := checkinFindDaily(ctx, userID, dateKey)
	if err != nil {
		return CheckinStatus{}, err
	}

	hasInstances, err := CheckinUserHasInstances(ctx, userID)
	if err != nil {
		return CheckinStatus{}, err
	}

	var currentPoints, totalEarned, totalSpent int32
	err = Pool().QueryRow(ctx,
		`SELECT points, total_earned, total_spent FROM user_points WHERE user_id = $1`, userID,
	).Scan(&currentPoints, &totalEarned, &totalSpent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CheckinStatus{}, err
	}

	var totalCheckins int64
	if err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM daily_checkins WHERE user_id = $1`, userID,
	).Scan(&totalCheckins); err != nil {
		return CheckinStatus{}, err
	}

	var monthCheckins int64
	if err := Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM daily_checkins WHERE user_id = $1 AND date_key LIKE $2`, userID, monthKey+"%",
	).Scan(&monthCheckins); err != nil {
		return CheckinStatus{}, err
	}

	recent, err := checkinRecentDaily(ctx, userID, 7)
	if err != nil {
		return CheckinStatus{}, err
	}

	status := CheckinStatus{
		Enabled:              settings.Enabled,
		HasCheckedIn:         today != nil,
		HasInstances:         hasInstances,
		RequireInstance:      settings.RequireInstance,
		MinPoints:            settings.MinPoints,
		MaxPoints:            settings.MaxPoints,
		CurrentPoints:        currentPoints,
		TotalEarned:          totalEarned,
		TotalSpent:           totalSpent,
		Today:                today,
		RecentRecords:        recent,
		SelfOnlyMode:         selfOnlyMode,
		ConsecutiveOthersUse: consecutiveOthersUse,
	}
	// `today?.streakDays ?? recentRecords[0]?.streakDays ?? 0`
	switch {
	case today != nil:
		status.StreakDays = today.StreakDays
	case len(recent) > 0:
		status.StreakDays = recent[0].StreakDays
	}
	return status, nil
}

type checkinDailyRow struct {
	ID         int32
	DateKey    string
	Points     int32
	StreakDays int32
	CreatedAt  time.Time
}

// checkinFindDaily mirrors the `dailyCheckin.findUnique({ userId_dateKey })`
// projection.
func checkinFindDaily(ctx context.Context, userID int32, dateKey string) (*CheckinStatusToday, error) {
	var rows []checkinDailyRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, date_key, points, streak_days, created_at FROM daily_checkins
		 WHERE user_id = ? AND date_key = ? LIMIT 1`, userID, dateKey,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &CheckinStatusToday{
		ID:         row.ID,
		DateKey:    row.DateKey,
		Points:     row.Points,
		StreakDays: row.StreakDays,
		CreatedAt:  checkinISOTime(row.CreatedAt),
	}, nil
}

// checkinRecentDaily mirrors `dailyCheckin.findMany({ orderBy: { createdAt: 'desc' }, take })`.
func checkinRecentDaily(ctx context.Context, userID int32, take int) ([]CheckinStatusToday, error) {
	var rows []checkinDailyRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, date_key, points, streak_days, created_at FROM daily_checkins
		 WHERE user_id = ? ORDER BY created_at DESC LIMIT ?`, userID, take,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]CheckinStatusToday, 0, len(rows))
	for _, row := range rows {
		records = append(records, CheckinStatusToday{
			ID:         row.ID,
			DateKey:    row.DateKey,
			Points:     row.Points,
			StreakDays: row.StreakDays,
			CreatedAt:  checkinISOTime(row.CreatedAt),
		})
	}
	return records, nil
}

// ==================== daily claim ====================

// CheckinPerformDailyPointsCheckin mirrors performDailyPointsCheckin.
//
// Concurrency contract: the whole claim runs in one READ COMMITTED transaction
// under pg_advisory_xact_lock(USER_POINTS_LOCK_NAMESPACE, userId), so two racing
// requests for the same user serialize; the second one sees the first one's
// daily_checkins row (committed before its lock was released) and fails with
// CHECKIN_ALREADY_TODAY. Points are never minted twice.
func CheckinPerformDailyPointsCheckin(ctx context.Context, userID int32, meta CheckinMeta) (CheckinResult, error) {
	settings, err := CheckinGetDailyCheckinSettings(ctx)
	if err != nil {
		return CheckinResult{}, err
	}
	if !settings.Enabled {
		return CheckinResult{}, CheckinErrDisabled
	}

	var status string
	err = Pool().QueryRow(ctx, `SELECT status::text FROM users WHERE id = $1`, userID).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CheckinResult{}, err
	}
	if err != nil || status != "active" {
		return CheckinResult{}, CheckinErrUserBlocked
	}
	if settings.RequireInstance {
		hasInstances, err := CheckinUserHasInstances(ctx, userID)
		if err != nil {
			return CheckinResult{}, err
		}
		if !hasInstances {
			return CheckinResult{}, CheckinErrNoInstance
		}
	}

	now := time.Now().UTC()
	dateKey := CheckinDateKeyInBeijing(now)
	yesterdayKey := checkinYesterdayDateKey(dateKey)
	points := checkinRandomizePoints(settings.MinPoints, settings.MaxPoints)

	result, err := checkinRunDailyClaim(ctx, userID, dateKey, yesterdayKey, now, points, meta)
	if err != nil {
		if checkinIsUniqueViolation(err) {
			return CheckinResult{}, CheckinErrAlreadyToday
		}
		return CheckinResult{}, err
	}
	return result, nil
}

func checkinRunDailyClaim(
	ctx context.Context,
	userID int32,
	dateKey string,
	yesterdayKey string,
	now time.Time,
	points int,
	meta CheckinMeta,
) (CheckinResult, error) {
	if pool == nil {
		return CheckinResult{}, errors.New("checkin: database not initialized")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return CheckinResult{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same namespace and ordering as checkin.ts: one lock, before any read.
	if err := AdvisoryTransactionLock(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		return CheckinResult{}, err
	}

	var existingID int32
	err = tx.QueryRow(ctx,
		`SELECT id FROM daily_checkins WHERE user_id = $1 AND date_key = $2`, userID, dateKey).Scan(&existingID)
	if err == nil {
		return CheckinResult{}, CheckinErrAlreadyToday
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CheckinResult{}, err
	}

	streakDays := int32(1)
	var lastDateKey string
	var lastStreakDays int32
	err = tx.QueryRow(ctx,
		`SELECT date_key, streak_days FROM daily_checkins WHERE user_id = $1
		 ORDER BY created_at DESC LIMIT 1`, userID).Scan(&lastDateKey, &lastStreakDays)
	switch {
	case err == nil:
		if lastDateKey == yesterdayKey {
			streakDays = lastStreakDays + 1
		}
	case errors.Is(err, pgx.ErrNoRows):
		// first ever check-in
	default:
		return CheckinResult{}, err
	}

	// `userPoints.upsert({ update: {}, create: { userId } })`
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_points (user_id, updated_at) VALUES ($1, NOW())
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return CheckinResult{}, err
	}
	var pointsBefore int32
	if err := tx.QueryRow(ctx, `SELECT points FROM user_points WHERE user_id = $1`, userID).Scan(&pointsBefore); err != nil {
		return CheckinResult{}, err
	}
	if int64(pointsBefore)+int64(points) > checkinMaxPointsValue {
		return CheckinResult{}, CheckinErrPointsLimitExceeded
	}

	var recordID int32
	if err := tx.QueryRow(ctx,
		`INSERT INTO daily_checkins (user_id, date_key, points, streak_days, ip_address, user_agent, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		userID, dateKey, points, streakDays,
		checkinNullableSlicedText(meta.IPAddress, 255),
		checkinNullableSlicedText(meta.UserAgent, 500),
		now,
	).Scan(&recordID); err != nil {
		return CheckinResult{}, err
	}

	var pointsAfter int32
	if err := tx.QueryRow(ctx,
		`UPDATE user_points SET points = points + $1, total_earned = total_earned + $1, updated_at = NOW()
		 WHERE user_id = $2 RETURNING points`, points, userID).Scan(&pointsAfter); err != nil {
		return CheckinResult{}, err
	}

	remark := "每日签到奖励 " + CheckinFormatInt(points) + " 积分"
	if _, err := tx.Exec(ctx,
		`INSERT INTO points_logs (user_id, type, amount, points_before, points_after, related_id, remark, created_at)
		 VALUES ($1, 'checkin', $2, $3, $4, $5, $6, $7)`,
		userID, points, pointsBefore, pointsAfter, recordID, remark, now); err != nil {
		return CheckinResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO checkin_stats (user_id, last_checkin_date) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET last_checkin_date = EXCLUDED.last_checkin_date`,
		userID, now); err != nil {
		return CheckinResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return CheckinResult{}, err
	}

	return CheckinResult{
		ID:            recordID,
		DateKey:       dateKey,
		Points:        int32(points),
		StreakDays:    streakDays,
		CurrentPoints: pointsAfter,
	}, nil
}

// CheckinFormatInt mirrors String(number) for a non-negative integer (avoids
// pulling strconv into the remark formatting call sites).
func CheckinFormatInt(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

// ==================== records ====================

// CheckinGetRecords mirrors getCheckinRecords.
func CheckinGetRecords(ctx context.Context, userID int32, limit, offset int) (CheckinRecordsResult, error) {
	type row struct {
		ID             int32
		RedeemCode     string
		CodeType       string
		CodeValue      int32
		ExpiresAt      time.Time
		UsedAt         *time.Time
		UsedByID       *int32
		UsedByUsername *string
		InstanceID     *int32
		InstanceName   *string
		CreatedAt      time.Time
	}

	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT cr.id, cr.redeem_code, cr.code_type::text AS code_type, cr.code_value,
		        cr.expires_at, cr.used_at,
		        u.id AS used_by_id, u.username AS used_by_username,
		        i.id AS instance_id, i.name AS instance_name,
		        cr.created_at
		   FROM checkin_records cr
		   LEFT JOIN users u ON u.id = cr.used_by
		   LEFT JOIN instances i ON i.id = cr.used_for
		  WHERE cr.user_id = ?
		  ORDER BY cr.created_at DESC
		  LIMIT ? OFFSET ?`, userID, limit, offset,
	).Scan(&rows).Error; err != nil {
		return CheckinRecordsResult{}, err
	}

	records := make([]CheckinRecordItem, 0, len(rows))
	for _, item := range rows {
		entry := CheckinRecordItem{
			ID:         item.ID,
			RedeemCode: item.RedeemCode,
			CodeType:   item.CodeType,
			CodeValue:  item.CodeValue,
			ExpiresAt:  checkinISOTime(item.ExpiresAt),
			CreatedAt:  checkinISOTime(item.CreatedAt),
		}
		if item.UsedAt != nil {
			formatted := checkinISOTime(*item.UsedAt)
			entry.UsedAt = &formatted
		}
		if item.UsedByID != nil {
			username := ""
			if item.UsedByUsername != nil {
				username = *item.UsedByUsername
			}
			entry.UsedBy = &CheckinUserRef{ID: *item.UsedByID, Username: username}
		}
		if item.InstanceID != nil {
			name := ""
			if item.InstanceName != nil {
				name = *item.InstanceName
			}
			entry.UsedFor = &CheckinInstanceRef{ID: *item.InstanceID, Name: name}
		}
		records = append(records, entry)
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM checkin_records WHERE user_id = ?`, userID,
	).Scan(&total).Error; err != nil {
		return CheckinRecordsResult{}, err
	}

	return CheckinRecordsResult{Records: records, Total: total}, nil
}

// CheckinGetRedeemRecords mirrors getRedeemRecords: it merges used check-in codes
// and used system h-codes into one list, orders by usedAt desc (stable, so equal
// timestamps keep check-in-before-system order) and then applies the pagination
// window in memory. System rows get `id + 1000000` to avoid colliding with
// check-in record ids.
func CheckinGetRedeemRecords(ctx context.Context, userID int32, limit, offset int) (CheckinRedeemRecordsResult, error) {
	type checkinRow struct {
		ID            int32
		RedeemCode    string
		CodeType      string
		CodeValue     int32
		OwnerID       int32
		OwnerUsername string
		InstanceID    *int32
		InstanceName  *string
		UsedAt        *time.Time
	}
	type systemRow struct {
		ID           int32
		Code         string
		CodeType     string
		CodeValue    int32
		CreatorID    int32
		CreatorName  string
		InstanceID   *int32
		InstanceName *string
		UsedAt       time.Time
	}

	var checkins []checkinRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT cr.id, cr.redeem_code, cr.code_type::text AS code_type, cr.code_value,
		        u.id AS owner_id, u.username AS owner_username,
		        i.id AS instance_id, i.name AS instance_name,
		        cr.used_at
		   FROM checkin_records cr
		   JOIN users u ON u.id = cr.user_id
		   LEFT JOIN instances i ON i.id = cr.used_for
		  WHERE cr.used_by = ?
		  ORDER BY cr.used_at DESC`, userID,
	).Scan(&checkins).Error; err != nil {
		return CheckinRedeemRecordsResult{}, err
	}

	var systemRecords []systemRow
	if err := DB.WithContext(ctx).Raw(
		`SELECT ru.id, rc.code, rc.code_type::text AS code_type, rc.code_value,
		        cu.id AS creator_id, cu.username AS creator_name,
		        i.id AS instance_id, i.name AS instance_name,
		        ru.used_at
		   FROM redeem_code_usages ru
		   JOIN redeem_codes rc ON rc.id = ru.redeem_code_id
		   JOIN users cu ON cu.id = rc.created_by_id
		   LEFT JOIN instances i ON i.id = ru.instance_id
		  WHERE ru.user_id = ?
		  ORDER BY ru.used_at DESC`, userID,
	).Scan(&systemRecords).Error; err != nil {
		return CheckinRedeemRecordsResult{}, err
	}

	var checkinTotal, systemTotal int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM checkin_records WHERE used_by = ?`, userID,
	).Scan(&checkinTotal).Error; err != nil {
		return CheckinRedeemRecordsResult{}, err
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM redeem_code_usages WHERE user_id = ?`, userID,
	).Scan(&systemTotal).Error; err != nil {
		return CheckinRedeemRecordsResult{}, err
	}

	all := make([]CheckinRedeemRecordItem, 0, len(checkins)+len(systemRecords))
	for _, item := range checkins {
		entry := CheckinRedeemRecordItem{
			ID:         item.ID,
			RedeemCode: item.RedeemCode,
			CodeType:   item.CodeType,
			CodeValue:  item.CodeValue,
			Owner:      CheckinUserRef{ID: item.OwnerID, Username: item.OwnerUsername},
		}
		if item.UsedAt != nil {
			formatted := checkinISOTime(*item.UsedAt)
			entry.UsedAt = &formatted
		}
		if item.InstanceID != nil {
			name := ""
			if item.InstanceName != nil {
				name = *item.InstanceName
			}
			entry.UsedFor = &CheckinInstanceRef{ID: *item.InstanceID, Name: name}
		}
		all = append(all, entry)
	}
	for _, item := range systemRecords {
		entry := CheckinRedeemRecordItem{
			ID:           item.ID + 1000000,
			RedeemCode:   item.Code,
			CodeType:     item.CodeType,
			CodeValue:    item.CodeValue,
			Owner:        CheckinUserRef{ID: item.CreatorID, Username: item.CreatorName},
			IsSystemCode: true,
		}
		formatted := checkinISOTime(item.UsedAt)
		entry.UsedAt = &formatted
		if item.InstanceID != nil {
			name := ""
			if item.InstanceName != nil {
				name = *item.InstanceName
			}
			entry.UsedFor = &CheckinInstanceRef{ID: *item.InstanceID, Name: name}
		}
		all = append(all, entry)
	}

	// `sort((a, b) => timeB - timeA)`: Array.prototype.sort is stable.
	sort.SliceStable(all, func(i, j int) bool {
		return checkinUsedAtMillis(all[i].UsedAt) > checkinUsedAtMillis(all[j].UsedAt)
	})

	windowed := all
	if offset < 0 {
		offset = 0
	}
	if offset >= len(windowed) {
		windowed = []CheckinRedeemRecordItem{}
	} else {
		end := offset + limit
		if end > len(windowed) || end < offset {
			end = len(windowed)
		}
		windowed = windowed[offset:end]
	}
	if windowed == nil {
		windowed = []CheckinRedeemRecordItem{}
	}

	return CheckinRedeemRecordsResult{Records: windowed, Total: checkinTotal + systemTotal}, nil
}

// checkinUsedAtMillis mirrors `a.usedAt ? new Date(a.usedAt).getTime() : 0`.
func checkinUsedAtMillis(value *string) int64 {
	if value == nil {
		return 0
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", *value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// ==================== instances ====================

// CheckinGetUserInstancesForRedeem mirrors getUserInstancesForRedeem: only free
// instances (packagePlanId IS NULL) that are running or stopped.
func CheckinGetUserInstancesForRedeem(ctx context.Context, userID int32) ([]CheckinRedeemInstanceItem, error) {
	type row struct {
		ID                  int32
		Name                string
		Status              string
		CPU                 int32
		Memory              int32
		Disk                int32
		MonthlyTrafficLimit *int64
		PackageID           *int32
		PackageName         *string
		PackageCPUMax       *int32
		PackageMemoryMax    *int32
		PackageDiskMax      *int32
		PackageTrafficLimit *int64
		HostID              int32
		HostName            string
		HostLocation        *string
		HostCountryCode     string
	}

	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT i.id, i.name, i.status::text AS status, i.cpu, i.memory, i.disk,
		        i.monthly_traffic_limit,
		        p.id AS package_id, p.name AS package_name, p.cpu_max AS package_cpu_max,
		        p.memory_max AS package_memory_max, p.disk_max AS package_disk_max,
		        p.monthly_traffic_limit AS package_traffic_limit,
		        h.id AS host_id, h.name AS host_name, h.location AS host_location,
		        h.country_code AS host_country_code
		   FROM instances i
		   JOIN hosts h ON h.id = i.host_id
		   LEFT JOIN packages p ON p.id = i.package_id
		  WHERE i.user_id = ? AND i.status IN ('running','stopped') AND i.package_plan_id IS NULL
		  ORDER BY i.name ASC`, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	instances := make([]CheckinRedeemInstanceItem, 0, len(rows))
	for _, item := range rows {
		entry := CheckinRedeemInstanceItem{
			ID:                  item.ID,
			Name:                item.Name,
			Status:              item.Status,
			CPU:                 item.CPU,
			Memory:              item.Memory,
			Disk:                item.Disk,
			MonthlyTrafficLimit: checkinInt64String(item.MonthlyTrafficLimit),
			Host: CheckinInstanceHost{
				ID:          item.HostID,
				Name:        item.HostName,
				Location:    item.HostLocation,
				CountryCode: item.HostCountryCode,
			},
		}
		if item.PackageID != nil {
			entry.Package = &CheckinInstancePackage{
				ID:                  *item.PackageID,
				Name:                checkinDerefString(item.PackageName),
				CPUMax:              checkinDerefInt32(item.PackageCPUMax),
				MemoryMax:           checkinDerefInt32(item.PackageMemoryMax),
				DiskMax:             checkinDerefInt32(item.PackageDiskMax),
				MonthlyTrafficLimit: checkinInt64String(item.PackageTrafficLimit),
			}
		}
		instances = append(instances, entry)
	}
	return instances, nil
}

// CheckinGetInstanceByID mirrors the db.getInstanceById projection used by the
// redeem route. It returns nil when the instance does not exist.
func CheckinGetInstanceByID(ctx context.Context, instanceID int64) (*CheckinInstance, error) {
	type row struct {
		ID      int32
		IncusID string
		Name    string
		UserID  int32
		HostID  int32
		Status  string
		CPU     int32
		Memory  int32
		Disk    int32
	}
	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, incus_id, name, user_id, host_id, status::text AS status, cpu, memory, disk
		   FROM instances WHERE id = ? LIMIT 1`, instanceID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	item := rows[0]
	return &CheckinInstance{
		ID:      item.ID,
		IncusID: item.IncusID,
		Name:    item.Name,
		UserID:  item.UserID,
		HostID:  item.HostID,
		Status:  item.Status,
		CPU:     item.CPU,
		Memory:  item.Memory,
		Disk:    item.Disk,
	}, nil
}

// CheckinGetHostByID mirrors the db.getHostById projection the redeem route needs
// to build an IncusClient. It returns nil when the host does not exist.
func CheckinGetHostByID(ctx context.Context, hostID int64) (*CheckinHostConnection, error) {
	type row struct {
		ID       int32
		URL      string
		CertPath *string
		KeyPath  *string
	}
	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, url, cert_path, key_path FROM hosts WHERE id = ? LIMIT 1`, hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &CheckinHostConnection{
		ID:       rows[0].ID,
		URL:      rows[0].URL,
		CertPath: rows[0].CertPath,
		KeyPath:  rows[0].KeyPath,
	}, nil
}

// checkinInt64String mirrors `bigint?.toString() ?? null`.
func checkinInt64String(value *int64) *string {
	if value == nil {
		return nil
	}
	formatted := CheckinFormatInt64(*value)
	return &formatted
}

// CheckinFormatInt64 mirrors String(bigint).
func CheckinFormatInt64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	// Careful with math.MinInt64; negate via unsigned arithmetic.
	var magnitude uint64
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	} else {
		magnitude = uint64(value)
	}
	var digits [24]byte
	position := len(digits)
	for magnitude > 0 {
		position--
		digits[position] = byte('0' + magnitude%10)
		magnitude /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

func checkinDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func checkinDerefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
