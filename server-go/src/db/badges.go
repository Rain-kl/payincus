// Port of server/src/db/badges.ts.
//
// Badge economics are a security boundary: every point-spending path takes the
// transaction-scoped PostgreSQL advisory lock USER_POINTS_LOCK_NAMESPACE (4111)
// on the *same* pgx transaction as the guarded UPDATE and the points_log row, so
// a racing double-draw cannot mint points (see db/advisory_locks.go). Mutations
// additionally run under SERIALIZABLE isolation with the same 5-attempt retry
// policy as the TypeScript (Prisma error P2034 ≡ PostgreSQL 40001/40P01).
//
// Every package-level identifier is prefixed `Badges`/`badges` because db is a
// flat package shared by parallel module ports.
//
// Timestamps on the wire use formatISOTime (Date.prototype.toISOString parity,
// millisecond precision). Decimal columns stay decimal/exact; JS `Number(...)`
// conversions are float64/int32 exactly where the TypeScript converts.
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Badge costs mirror BADGE_RANDOM_DRAW_COST / BADGE_SELECT_COST in
// config/badges.ts.
const (
	BadgesRandomDrawCost int32 = 500
	BadgesSelectCost     int32 = 1500

	badgesTransactionMaxAttempts = 5
	// Prisma maxWait: 5000 is the time allowed to acquire a transaction slot;
	// timeout: 10000 is the transaction lifetime. pgx has no separate maxWait, so
	// the pool acquisition gets its own 5s context and the body gets 10s.
	badgesTransactionMaxWait = 5 * time.Second
	badgesTransactionTimeout = 10 * time.Second
)

// BadgesRandomFloat is the injectable RNG used by the badge draw. Node's
// Math.random() sequence cannot be reproduced, so tests substitute this hook to
// pin the selected badge. It returns a float64 in [0,1).
var BadgesRandomFloat = func() float64 { return rand.Float64() }

// ==================== sentinels ====================
//
// The TypeScript throws plain Error objects whose message the route maps onto a
// 400 payload; these sentinels carry the identical text.

var (
	BadgesErrNoBadgesAvailable      = errors.New("NO_BADGES_AVAILABLE")
	BadgesErrNotFound               = errors.New("BADGE_NOT_FOUND")
	BadgesErrOwnershipNotFound      = errors.New("BADGE_OWNERSHIP_NOT_FOUND")
	BadgesErrAlreadyApplied         = errors.New("BADGE_ALREADY_APPLIED")
	BadgesErrNotApplied             = errors.New("BADGE_NOT_APPLIED")
	BadgesErrAvatarAlreadyApplied   = errors.New("AVATAR_BADGE_ALREADY_APPLIED")
	BadgesErrInstanceNotFound       = errors.New("INSTANCE_NOT_FOUND")
	BadgesErrInstanceAlreadyApplied = errors.New("INSTANCE_BADGE_ALREADY_APPLIED")
	BadgesErrStateConflict          = errors.New("BADGE_STATE_CONFLICT")
	BadgesErrSeriesHasBadges        = errors.New("BADGE_SERIES_HAS_BADGES")
	BadgesErrInUse                  = errors.New("BADGE_IN_USE")
	BadgesErrUserPointsCreateFailed = errors.New("USER_POINTS_CREATE_FAILED")
)

// BadgesErrorCode mirrors the route's sendBadgeError code extraction: a Prisma
// P2002 unique-constraint violation becomes BADGE_STATE_CONFLICT, everything else
// surfaces as the error message verbatim.
func BadgesErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return BadgesErrStateConflict.Error()
	}
	if err == nil {
		return "UNKNOWN_ERROR"
	}
	return err.Error()
}

// ==================== types ====================

// BadgesCatalogItem mirrors BadgeCatalogItem. Optional fields model the
// TypeScript's `undefined` (omitted from JSON) vs `null` distinction: pointers
// with omitempty are dropped when unset, non-omitempty pointers serialize null.
type BadgesCatalogItem struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	NameEn            *string `json:"nameEn"`
	FullLabel         string  `json:"fullLabel"`
	SourceID          *string `json:"sourceId,omitempty"`
	SourceLabel       *string `json:"sourceLabel,omitempty"`
	SeriesID          string  `json:"seriesId"`
	SeriesTitle       string  `json:"seriesTitle"`
	SeriesNameZh      string  `json:"seriesNameZh"`
	SeriesNameEn      *string `json:"seriesNameEn"`
	SeriesDescription string  `json:"seriesDescription"`
	AssetURL          string  `json:"assetUrl"`
	AssetURLDark      *string `json:"assetUrlDark,omitempty"`
	AssetURLLight     *string `json:"assetUrlLight,omitempty"`
	DisplayOrder      *int32  `json:"displayOrder,omitempty"`
	IsActive          *bool   `json:"isActive,omitempty"`
	SeriesIsActive    *bool   `json:"seriesIsActive,omitempty"`
}

// BadgesSeriesView mirrors BadgeSeriesView.
type BadgesSeriesView struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	NameZh           string  `json:"nameZh"`
	NameEn           *string `json:"nameEn"`
	Description      string  `json:"description"`
	SourceID         *string `json:"sourceId"`
	SourceLabel      *string `json:"sourceLabel"`
	DisplayOrder     int32   `json:"displayOrder"`
	IsActive         bool    `json:"isActive"`
	BadgeCount       int64   `json:"badgeCount"`
	ActiveBadgeCount int64   `json:"activeBadgeCount"`
	CreatedAt        string  `json:"createdAt"`
	UpdatedAt        string  `json:"updatedAt"`
}

// BadgesOwnershipView mirrors BadgeOwnershipView.
type BadgesOwnershipView struct {
	ID                  int32                          `json:"id"`
	BadgeID             string                         `json:"badgeId"`
	BadgeName           string                         `json:"badgeName"`
	BadgeNameEn         *string                        `json:"badgeNameEn"`
	BadgeLabel          string                         `json:"badgeLabel"`
	SeriesID            string                         `json:"seriesId"`
	SeriesTitle         string                         `json:"seriesTitle"`
	AssetURL            string                         `json:"assetUrl"`
	AssetURLDark        *string                        `json:"assetUrlDark"`
	AssetURLLight       *string                        `json:"assetUrlLight"`
	Source              models.UserBadgeSource         `json:"source"`
	ApplicationTarget   *models.BadgeApplicationTarget `json:"applicationTarget"`
	AppliedInstanceID   *int32                         `json:"appliedInstanceId"`
	AppliedInstanceName *string                        `json:"appliedInstanceName"`
	AppliedAt           *string                        `json:"appliedAt"`
	CreatedAt           string                         `json:"createdAt"`
}

// BadgesCosts mirrors the `costs` object of getBadgeOverview.
type BadgesCosts struct {
	RandomDraw int32 `json:"randomDraw"`
	Select     int32 `json:"select"`
}

// BadgesInstanceHostView mirrors the host projection inside getBadgeOverview.
type BadgesInstanceHostView struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Location    *string `json:"location"`
	CountryCode string  `json:"countryCode"`
}

// BadgesInstanceView mirrors the instance projection inside getBadgeOverview.
type BadgesInstanceView struct {
	ID            int32                  `json:"id"`
	Name          string                 `json:"name"`
	Status        models.InstanceStatus  `json:"status"`
	IconBadgeID   *string                `json:"iconBadgeId"`
	PackagePlanID *int32                 `json:"packagePlanId"`
	InstanceType  string                 `json:"instanceType"`
	Host          BadgesInstanceHostView `json:"host"`
}

// BadgesOverview mirrors getBadgeOverview's return shape.
type BadgesOverview struct {
	Costs         BadgesCosts           `json:"costs"`
	CurrentPoints int32                 `json:"currentPoints"`
	AvatarBadgeID *string               `json:"avatarBadgeId"`
	Catalog       []BadgesCatalogItem   `json:"catalog"`
	Ownerships    []BadgesOwnershipView `json:"ownerships"`
	Instances     []BadgesInstanceView  `json:"instances"`
}

// BadgesCatalogOptions mirrors `options: { activeOnly?: boolean }`.
type BadgesCatalogOptions struct {
	ActiveOnly bool
}

// BadgesRandomDrawResult mirrors performRandomBadgeDraw's return shape.
type BadgesRandomDrawResult struct {
	Success   bool                 `json:"success"`
	Points    int32                `json:"points"`
	Ownership *BadgesOwnershipView `json:"ownership,omitempty"`
}

// BadgesRandomMultiDrawResult mirrors performRandomBadgeMultiDraw's return shape.
type BadgesRandomMultiDrawResult struct {
	Success    bool                  `json:"success"`
	Points     int32                 `json:"points"`
	Ownerships []BadgesOwnershipView `json:"ownerships"`
}

// BadgesSelectResult mirrors performSelectBadge's return shape.
type BadgesSelectResult struct {
	Success   bool                 `json:"success"`
	Points    int32                `json:"points"`
	Ownership *BadgesOwnershipView `json:"ownership,omitempty"`
}

// BadgesSeriesInput mirrors createBadgeSeries' input (pointer fields keep the
// `undefined` vs explicit-value distinction for updateBadgeSeries).
type BadgesSeriesInput struct {
	ID           string
	Title        string
	NameZh       string
	NameEn       *string
	Description  string
	SourceID     *string
	SourceLabel  *string
	DisplayOrder *int32
	IsActive     *bool
}

// BadgesSeriesUpdate mirrors updateBadgeSeries' partial input.
type BadgesSeriesUpdate struct {
	Title        *string
	NameZh       *string
	NameEn       *string
	Description  *string
	SourceID     *string
	SourceLabel  *string
	DisplayOrder *int32
	IsActive     *bool
}

// BadgesInput mirrors createBadge's input.
type BadgesInput struct {
	ID            string
	Name          string
	NameEn        *string
	FullLabel     string
	SeriesID      string
	SourceID      *string
	SourceLabel   *string
	AssetURL      string
	AssetURLDark  *string
	AssetURLLight *string
	DisplayOrder  *int32
	IsActive      *bool
}

// BadgesUpdateInput mirrors updateBadge's partial input.
type BadgesUpdateInput struct {
	Name          *string
	NameEn        *string
	FullLabel     *string
	SeriesID      *string
	SourceID      *string
	SourceLabel   *string
	AssetURL      *string
	AssetURLDark  *string
	AssetURLLight *string
	DisplayOrder  *int32
	IsActive      *bool
}

// BadgesAdminCatalogItem mirrors the spread of mapBadgeRow plus the admin counts.
type BadgesAdminCatalogItem struct {
	BadgesCatalogItem
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
	OwnershipCount   int64  `json:"ownershipCount"`
	AvatarUseCount   int64  `json:"avatarUseCount"`
	InstanceUseCount int64  `json:"instanceUseCount"`
}

// BadgesAdminCatalog mirrors getAdminBadgeCatalog's return shape.
type BadgesAdminCatalog struct {
	Series []BadgesSeriesView       `json:"series"`
	Badges []BadgesAdminCatalogItem `json:"badges"`
}

// ==================== query plumbing ====================

// BadgesQuerier is satisfied by *pgxpool.Pool and pgx.Tx, so the same query can
// run on the pool or inside the lock-holding transaction.
type BadgesQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var _ BadgesQuerier = (*pgxpool.Pool)(nil)
var _ BadgesQuerier = (pgx.Tx)(nil)

const badgesJoinedSelect = `SELECT
	b.id, b.name, b.name_en, b.full_label, b.series_id, b.source_id, b.source_label,
	b.asset_url, b.asset_url_dark, b.asset_url_light, b.display_order, b.is_active,
	b.created_at, b.updated_at,
	s.title, s.name_zh, s.name_en, s.description, s.source_id, s.source_label, s.is_active
FROM badges b
JOIN badge_series s ON s.id = b.series_id`

// badgesJoinedRow is the projection of the badge+series join.
type badgesJoinedRow struct {
	ID                string
	Name              string
	NameEn            *string
	FullLabel         string
	SeriesID          string
	SourceID          *string
	SourceLabel       *string
	AssetURL          string
	AssetURLDark      *string
	AssetURLLight     *string
	DisplayOrder      int32
	IsActive          bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SeriesTitle       string
	SeriesNameZh      string
	SeriesNameEn      *string
	SeriesDescription string
	SeriesSourceID    *string
	SeriesSourceLabel *string
	SeriesIsActive    bool
}

func badgesScanJoinedRow(row pgx.Row) (badgesJoinedRow, error) {
	var out badgesJoinedRow
	err := row.Scan(
		&out.ID, &out.Name, &out.NameEn, &out.FullLabel, &out.SeriesID, &out.SourceID, &out.SourceLabel,
		&out.AssetURL, &out.AssetURLDark, &out.AssetURLLight, &out.DisplayOrder, &out.IsActive,
		&out.CreatedAt, &out.UpdatedAt,
		&out.SeriesTitle, &out.SeriesNameZh, &out.SeriesNameEn, &out.SeriesDescription,
		&out.SeriesSourceID, &out.SeriesSourceLabel, &out.SeriesIsActive,
	)
	return out, err
}

func badgesMapJoinedRow(row badgesJoinedRow) BadgesCatalogItem {
	sourceID := row.SourceID
	if sourceID == nil {
		sourceID = row.SeriesSourceID
	}
	sourceLabel := row.SourceLabel
	if sourceLabel == nil {
		sourceLabel = row.SeriesSourceLabel
	}
	displayOrder := row.DisplayOrder
	isActive := row.IsActive
	seriesIsActive := row.SeriesIsActive
	return BadgesCatalogItem{
		ID:                row.ID,
		Name:              row.Name,
		NameEn:            row.NameEn,
		FullLabel:         row.FullLabel,
		SourceID:          sourceID,
		SourceLabel:       sourceLabel,
		SeriesID:          row.SeriesID,
		SeriesTitle:       row.SeriesTitle,
		SeriesNameZh:      row.SeriesNameZh,
		SeriesNameEn:      row.SeriesNameEn,
		SeriesDescription: row.SeriesDescription,
		AssetURL:          row.AssetURL,
		AssetURLDark:      row.AssetURLDark,
		AssetURLLight:     row.AssetURLLight,
		DisplayOrder:      &displayOrder,
		IsActive:          &isActive,
		SeriesIsActive:    &seriesIsActive,
	}
}

// badgesFallbackForID mirrors fallbackBadgeForId: a placeholder that keeps
// historical ownerships renderable when the catalogue row was deleted.
func badgesFallbackForID(id string) BadgesCatalogItem {
	dark := "/badges/dark/" + id + ".svg"
	light := "/badges/light/" + id + ".svg"
	inactive := false
	unknown := "未知系列"
	return BadgesCatalogItem{
		ID:                id,
		Name:              id,
		NameEn:            nil,
		FullLabel:         id,
		SeriesID:          "unknown",
		SeriesTitle:       unknown,
		SeriesNameZh:      unknown,
		SeriesNameEn:      nil,
		SeriesDescription: "该徽章目录记录不存在，保留占位以兼容历史数据。",
		AssetURL:          dark,
		AssetURLDark:      &dark,
		AssetURLLight:     &light,
		IsActive:          &inactive,
		SeriesIsActive:    &inactive,
	}
}

// ==================== reads ====================

// BadgesGetCatalog mirrors getBadgeCatalog.
func BadgesGetCatalog(ctx context.Context, options BadgesCatalogOptions) ([]BadgesCatalogItem, error) {
	return badgesGetCatalog(ctx, Pool(), options)
}

func badgesGetCatalog(ctx context.Context, q BadgesQuerier, options BadgesCatalogOptions) ([]BadgesCatalogItem, error) {
	sql := badgesJoinedSelect
	if options.ActiveOnly {
		sql += " WHERE b.is_active = true AND s.is_active = true"
	}
	sql += " ORDER BY s.display_order ASC, b.display_order ASC, b.id ASC"

	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]BadgesCatalogItem, 0, 16)
	for rows.Next() {
		row, scanErr := badgesScanJoinedRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, badgesMapJoinedRow(row))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// BadgesGetByID mirrors getBadgeById.
func BadgesGetByID(ctx context.Context, q BadgesQuerier, id string, options BadgesCatalogOptions) (*BadgesCatalogItem, error) {
	sql := badgesJoinedSelect + " WHERE b.id = $1"
	if options.ActiveOnly {
		sql += " AND b.is_active = true AND s.is_active = true"
	}
	sql += " LIMIT 1"

	row, err := badgesScanJoinedRow(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item := badgesMapJoinedRow(row)
	return &item, nil
}

// badgesGetCatalogMap mirrors getBadgeCatalogMap: every badge (active or not),
// keyed by id, used to decorate ownership rows.
func badgesGetCatalogMap(ctx context.Context, q BadgesQuerier) (map[string]BadgesCatalogItem, error) {
	rows, err := q.Query(ctx, badgesJoinedSelect)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := map[string]BadgesCatalogItem{}
	for rows.Next() {
		row, scanErr := badgesScanJoinedRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result[row.ID] = badgesMapJoinedRow(row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// BadgesGetSeriesList mirrors getBadgeSeriesList, including the two aggregate
// counts (all badges vs active badges in an active series).
func BadgesGetSeriesList(ctx context.Context, options BadgesCatalogOptions) ([]BadgesSeriesView, error) {
	return badgesGetSeriesList(ctx, Pool(), options)
}

func badgesGetSeriesList(ctx context.Context, q BadgesQuerier, options BadgesCatalogOptions) ([]BadgesSeriesView, error) {
	sql := `SELECT s.id, s.title, s.name_zh, s.name_en, s.description, s.source_id, s.source_label,
			s.display_order, s.is_active, s.created_at, s.updated_at, COUNT(b.id)
		FROM badge_series s
		LEFT JOIN badges b ON b.series_id = s.id`
	if options.ActiveOnly {
		sql += " WHERE s.is_active = true"
	}
	sql += " GROUP BY s.id ORDER BY s.display_order ASC, s.id ASC"

	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type seriesRow struct {
		view       BadgesSeriesView
		createdAt  time.Time
		updatedAt  time.Time
		badgeCount int64
	}
	seriesRows := make([]seriesRow, 0, 8)
	ids := make([]string, 0, 8)
	for rows.Next() {
		var (
			id           string
			title        string
			nameZh       string
			nameEn       *string
			description  string
			sourceID     *string
			sourceLabel  *string
			displayOrder int32
			isActive     bool
			createdAt    time.Time
			updatedAt    time.Time
			badgeCount   int64
		)
		if err := rows.Scan(&id, &title, &nameZh, &nameEn, &description, &sourceID, &sourceLabel,
			&displayOrder, &isActive, &createdAt, &updatedAt, &badgeCount); err != nil {
			return nil, err
		}
		seriesRows = append(seriesRows, seriesRow{
			view: BadgesSeriesView{
				ID:           id,
				Title:        title,
				NameZh:       nameZh,
				NameEn:       nameEn,
				Description:  description,
				SourceID:     sourceID,
				SourceLabel:  sourceLabel,
				DisplayOrder: displayOrder,
				IsActive:     isActive,
				BadgeCount:   badgeCount,
			},
			createdAt: createdAt,
			updatedAt: updatedAt,
		})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	activeCounts := map[string]int64{}
	if len(ids) > 0 {
		countRows, err := q.Query(ctx, `SELECT b.series_id, COUNT(*)
			FROM badges b
			JOIN badge_series s ON s.id = b.series_id
			WHERE b.is_active = true AND b.series_id = ANY($1) AND s.is_active = true
			GROUP BY b.series_id`, ids)
		if err != nil {
			return nil, err
		}
		defer countRows.Close()
		for countRows.Next() {
			var seriesID string
			var count int64
			if err := countRows.Scan(&seriesID, &count); err != nil {
				return nil, err
			}
			activeCounts[seriesID] = count
		}
		if err := countRows.Err(); err != nil {
			return nil, err
		}
	}

	views := make([]BadgesSeriesView, 0, len(seriesRows))
	for _, item := range seriesRows {
		view := item.view
		view.ActiveBadgeCount = activeCounts[view.ID]
		view.CreatedAt = formatISOTime(item.createdAt)
		view.UpdatedAt = formatISOTime(item.updatedAt)
		views = append(views, view)
	}
	return views, nil
}

// ==================== ownership mapping ====================

// badgesOwnershipRecord is the row projection for user_badge_ownerships plus the
// applied instance relation.
type badgesOwnershipRecord struct {
	ID                  int32
	BadgeID             string
	Source              string
	ApplicationTarget   *string
	AppliedInstanceID   *int32
	AppliedAt           *time.Time
	CreatedAt           time.Time
	AppliedInstanceName *string
}

const badgesOwnershipSelect = `SELECT o.id, o.badge_id, o.source, o.application_target,
		o.applied_instance_id, o.applied_at, o.created_at, i.name
	FROM user_badge_ownerships o
	LEFT JOIN instances i ON i.id = o.applied_instance_id`

func badgesScanOwnership(rows pgx.Rows) (badgesOwnershipRecord, error) {
	var record badgesOwnershipRecord
	err := rows.Scan(&record.ID, &record.BadgeID, &record.Source, &record.ApplicationTarget,
		&record.AppliedInstanceID, &record.AppliedAt, &record.CreatedAt, &record.AppliedInstanceName)
	return record, err
}

func badgesScanOwnershipRow(row pgx.Row) (badgesOwnershipRecord, error) {
	var record badgesOwnershipRecord
	err := row.Scan(&record.ID, &record.BadgeID, &record.Source, &record.ApplicationTarget,
		&record.AppliedInstanceID, &record.AppliedAt, &record.CreatedAt, &record.AppliedInstanceName)
	return record, err
}

func badgesMapOwnership(record badgesOwnershipRecord, badgeMap map[string]BadgesCatalogItem) BadgesOwnershipView {
	badge, ok := badgeMap[record.BadgeID]
	if !ok {
		badge = badgesFallbackForID(record.BadgeID)
	}

	var target *models.BadgeApplicationTarget
	if record.ApplicationTarget != nil && *record.ApplicationTarget != "" {
		converted := models.BadgeApplicationTarget(*record.ApplicationTarget)
		target = &converted
	}

	view := BadgesOwnershipView{
		ID:                  record.ID,
		BadgeID:             badge.ID,
		BadgeName:           badge.Name,
		BadgeNameEn:         badge.NameEn,
		BadgeLabel:          badge.FullLabel,
		SeriesID:            badge.SeriesID,
		SeriesTitle:         badge.SeriesTitle,
		AssetURL:            badge.AssetURL,
		AssetURLDark:        badge.AssetURLDark,
		AssetURLLight:       badge.AssetURLLight,
		Source:              models.UserBadgeSource(record.Source),
		ApplicationTarget:   target,
		AppliedInstanceID:   record.AppliedInstanceID,
		AppliedInstanceName: record.AppliedInstanceName,
		CreatedAt:           formatISOTime(record.CreatedAt),
	}
	if record.AppliedAt != nil {
		formatted := formatISOTime(*record.AppliedAt)
		view.AppliedAt = &formatted
	}
	return view
}

// BadgesCreateOwnership mirrors createBadgeOwnership.
func BadgesCreateOwnership(ctx context.Context, tx pgx.Tx, userID int32, badgeID string, source models.UserBadgeSource, badge *BadgesCatalogItem) (BadgesOwnershipView, error) {
	var (
		id        int32
		createdAt time.Time
	)
	err := tx.QueryRow(ctx, `INSERT INTO user_badge_ownerships (user_id, badge_id, source, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		RETURNING id, created_at`, userID, badgeID, string(source)).Scan(&id, &createdAt)
	if err != nil {
		return BadgesOwnershipView{}, err
	}

	record := badgesOwnershipRecord{
		ID:        id,
		BadgeID:   badgeID,
		Source:    string(source),
		CreatedAt: createdAt,
	}

	resolved := badge
	if resolved == nil {
		fetched, err := BadgesGetByID(ctx, tx, badgeID, BadgesCatalogOptions{})
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		resolved = fetched
	}
	badgeMap := map[string]BadgesCatalogItem{}
	if resolved != nil {
		badgeMap[resolved.ID] = *resolved
	} else {
		fallback := badgesFallbackForID(badgeID)
		badgeMap[badgeID] = fallback
	}
	return badgesMapOwnership(record, badgeMap), nil
}

// badgesRandomIndex mirrors `Math.floor(Math.random() * length)` for a non-empty
// catalogue, clamped defensively against a hook that returns exactly 1.0.
func badgesRandomIndex(length int) int {
	if length <= 0 {
		return 0
	}
	index := int(math.Floor(BadgesRandomFloat() * float64(length)))
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

// BadgesSelectRandomOrThrow mirrors selectRandomBadgeOrThrow: the active catalogue
// in display order, then a uniform index picked with BadgesRandomFloat.
func BadgesSelectRandomOrThrow(ctx context.Context, q BadgesQuerier) (BadgesCatalogItem, error) {
	catalog, err := badgesGetCatalog(ctx, q, BadgesCatalogOptions{ActiveOnly: true})
	if err != nil {
		return BadgesCatalogItem{}, err
	}
	if len(catalog) == 0 {
		return BadgesCatalogItem{}, BadgesErrNoBadgesAvailable
	}
	return catalog[badgesRandomIndex(len(catalog))], nil
}

// ==================== points helpers ====================

// badgesEnsureUserPointsRow mirrors `userPoints.upsert({ update: {}, create: { userId } })`:
// INSERT ... ON CONFLICT DO NOTHING, so an existing row is left untouched.
func badgesEnsureUserPointsRow(ctx context.Context, q BadgesQuerier, userID int32) error {
	_, err := q.Exec(ctx, `INSERT INTO user_points (user_id, updated_at) VALUES ($1, NOW())
		ON CONFLICT (user_id) DO NOTHING`, userID)
	return err
}

// badgesGetUserPoints mirrors ensureUserPoints from points.ts and returns the
// full row (getBadgeOverview needs only points).
func badgesGetUserPoints(ctx context.Context, q BadgesQuerier, userID int32) (models.UserPoints, error) {
	var (
		points           int32
		totalEarned      int32
		totalSpent       int32
		lastConvertedAt  *time.Time
		convertedConsume decimal.Decimal
		createdAt        time.Time
		updatedAt        time.Time
	)
	scan := func(row pgx.Row) error {
		return row.Scan(&userID, &points, &totalEarned, &totalSpent, &lastConvertedAt,
			&convertedConsume, &createdAt, &updatedAt)
	}

	err := scan(q.QueryRow(ctx, `SELECT user_id, points, total_earned, total_spent, last_converted_at,
		converted_consume, created_at, updated_at FROM user_points WHERE user_id = $1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		if insertErr := badgesEnsureUserPointsRow(ctx, q, userID); insertErr != nil {
			return models.UserPoints{}, insertErr
		}
		err = scan(q.QueryRow(ctx, `SELECT user_id, points, total_earned, total_spent, last_converted_at,
			converted_consume, created_at, updated_at FROM user_points WHERE user_id = $1`, userID))
		if errors.Is(err, pgx.ErrNoRows) {
			return models.UserPoints{}, BadgesErrUserPointsCreateFailed
		}
	}
	if err != nil {
		return models.UserPoints{}, err
	}

	return models.UserPoints{
		UserID:           userID,
		Points:           points,
		TotalEarned:      totalEarned,
		TotalSpent:       totalSpent,
		LastConvertedAt:  lastConvertedAt,
		ConvertedConsume: models.Decimal{Decimal: convertedConsume},
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}, nil
}

// badgesSpendPoints mirrors spendBadgePoints. The advisory lock must run on the
// caller's transaction so the guarded UPDATE and its log row commit together.
func badgesSpendPoints(ctx context.Context, tx pgx.Tx, userID int32, amount int32, logType models.PointsLogType, remark string) (bool, int32, error) {
	if err := AdvisoryTransactionLock(ctx, tx, UserPointsLockNamespace, userID); err != nil {
		return false, 0, err
	}
	if err := badgesEnsureUserPointsRow(ctx, tx, userID); err != nil {
		return false, 0, err
	}

	tag, err := tx.Exec(ctx, `UPDATE user_points
		SET points = points - $1, total_spent = total_spent + $1, updated_at = NOW()
		WHERE user_id = $2 AND points >= $1`, amount, userID)
	if err != nil {
		return false, 0, err
	}

	if tag.RowsAffected() == 0 {
		var current int32
		err := tx.QueryRow(ctx, `SELECT points FROM user_points WHERE user_id = $1`, userID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, nil
		}
		if err != nil {
			return false, 0, err
		}
		return false, current, nil
	}

	var pointsAfter int32
	if err := tx.QueryRow(ctx, `SELECT points FROM user_points WHERE user_id = $1`, userID).Scan(&pointsAfter); err != nil {
		return false, 0, err
	}
	pointsBefore := pointsAfter + amount

	_, err = tx.Exec(ctx, `INSERT INTO points_logs (user_id, type, amount, points_before, points_after, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
		userID, string(logType), -amount, pointsBefore, pointsAfter, remark)
	if err != nil {
		return false, 0, err
	}

	return true, pointsAfter, nil
}

// ==================== transactions ====================

// badgesIsRetryableTransactionError mirrors isRetryableTransactionError: Prisma
// P2034 is PostgreSQL 40001/40P01, plus the driver message fragments.
func badgesIsRetryableTransactionError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "40001" || pgErr.Code == "40P01" {
			return true
		}
	}
	message := err.Error()
	return strings.Contains(message, "deadlock") ||
		strings.Contains(message, "write conflict") ||
		strings.Contains(message, "could not serialize")
}

// badgesRunTransaction mirrors runBadgeTransaction: SERIALIZABLE, maxWait 5s,
// timeout 10s, up to 5 attempts with `(floor(random*250)+50) * attempt` backoff.
func badgesRunTransaction[T any](ctx context.Context, handler func(context.Context, pgx.Tx) (T, error)) (T, error) {
	var zero T
	var lastErr error

	for attempt := 1; attempt <= badgesTransactionMaxAttempts; attempt++ {
		result, err := badgesRunTransactionOnce(ctx, handler)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !badgesIsRetryableTransactionError(err) || attempt == badgesTransactionMaxAttempts {
			return zero, err
		}
		delay := time.Duration(math.Floor(rand.Float64()*250)+50) * time.Millisecond * time.Duration(attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

func badgesRunTransactionOnce[T any](ctx context.Context, handler func(context.Context, pgx.Tx) (T, error)) (T, error) {
	var zero T
	if Pool() == nil {
		return zero, errors.New("database not initialized")
	}

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, badgesTransactionMaxWait)
	defer cancelAcquire()

	tx, err := Pool().BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	txCtx, cancelTx := context.WithTimeout(ctx, badgesTransactionTimeout)
	defer cancelTx()

	result, err := handler(txCtx, tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(txCtx); err != nil {
		return zero, err
	}
	return result, nil
}

// ==================== overview ====================

// BadgesGetOverview mirrors getBadgeOverview.
func BadgesGetOverview(ctx context.Context, userID int32) (BadgesOverview, error) {
	var avatarBadgeID *string
	err := Pool().QueryRow(ctx, `SELECT avatar_badge_id FROM users WHERE id = $1`, userID).Scan(&avatarBadgeID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BadgesOverview{}, err
	}

	userPoints, err := badgesGetUserPoints(ctx, Pool(), userID)
	if err != nil {
		return BadgesOverview{}, err
	}

	ownerships, err := badgesListOwnerships(ctx, Pool(), userID)
	if err != nil {
		return BadgesOverview{}, err
	}

	badgeMap, err := badgesGetCatalogMap(ctx, Pool())
	if err != nil {
		return BadgesOverview{}, err
	}
	ownershipViews := make([]BadgesOwnershipView, 0, len(ownerships))
	for _, ownership := range ownerships {
		ownershipViews = append(ownershipViews, badgesMapOwnership(ownership, badgeMap))
	}

	instances, err := badgesListInstances(ctx, Pool(), userID)
	if err != nil {
		return BadgesOverview{}, err
	}

	catalog, err := BadgesGetCatalog(ctx, BadgesCatalogOptions{ActiveOnly: true})
	if err != nil {
		return BadgesOverview{}, err
	}

	return BadgesOverview{
		Costs:         BadgesCosts{RandomDraw: BadgesRandomDrawCost, Select: BadgesSelectCost},
		CurrentPoints: userPoints.Points,
		AvatarBadgeID: avatarBadgeID,
		Catalog:       catalog,
		Ownerships:    ownershipViews,
		Instances:     instances,
	}, nil
}

func badgesListOwnerships(ctx context.Context, q BadgesQuerier, userID int32) ([]badgesOwnershipRecord, error) {
	rows, err := q.Query(ctx, badgesOwnershipSelect+" WHERE o.user_id = $1 ORDER BY o.created_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]badgesOwnershipRecord, 0, 8)
	for rows.Next() {
		record, err := badgesScanOwnership(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func badgesListInstances(ctx context.Context, q BadgesQuerier, userID int32) ([]BadgesInstanceView, error) {
	rows, err := q.Query(ctx, `SELECT i.id, i.name, i.status, i.icon_badge_id, i.package_plan_id,
			p.instance_type, h.id, h.name, h.location, h.country_code
		FROM instances i
		JOIN hosts h ON h.id = i.host_id
		LEFT JOIN packages p ON p.id = i.package_id
		WHERE i.user_id = $1 AND i.status <> 'deleted'
		ORDER BY i.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	instances := make([]BadgesInstanceView, 0, 8)
	for rows.Next() {
		var (
			id            int32
			name          string
			status        string
			iconBadgeID   *string
			packagePlanID *int32
			instanceType  *string
			hostID        int32
			hostName      string
			hostLocation  *string
			countryCode   string
		)
		if err := rows.Scan(&id, &name, &status, &iconBadgeID, &packagePlanID,
			&instanceType, &hostID, &hostName, &hostLocation, &countryCode); err != nil {
			return nil, err
		}
		resolvedType := "container"
		if instanceType != nil && *instanceType == string(models.InstanceTypeVm) {
			resolvedType = "vm"
		}
		instances = append(instances, BadgesInstanceView{
			ID:            id,
			Name:          name,
			Status:        models.InstanceStatus(status),
			IconBadgeID:   iconBadgeID,
			PackagePlanID: packagePlanID,
			InstanceType:  resolvedType,
			Host: BadgesInstanceHostView{
				ID:          hostID,
				Name:        hostName,
				Location:    hostLocation,
				CountryCode: countryCode,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return instances, nil
}

// ==================== draws ====================

// BadgesPerformRandomDraw mirrors performRandomBadgeDraw.
func BadgesPerformRandomDraw(ctx context.Context, userID int32) (BadgesRandomDrawResult, error) {
	outcome, err := badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesRandomDrawResult, error) {
		selected, err := BadgesSelectRandomOrThrow(txCtx, tx)
		if err != nil {
			return BadgesRandomDrawResult{}, err
		}

		ok, points, err := badgesSpendPoints(txCtx, tx, userID, BadgesRandomDrawCost,
			models.PointsLogTypeBadgeDrawSpend, "徽章随机抽取："+selected.FullLabel)
		if err != nil {
			return BadgesRandomDrawResult{}, err
		}
		if !ok {
			return BadgesRandomDrawResult{Success: false, Points: points}, nil
		}

		ownership, err := BadgesCreateOwnership(txCtx, tx, userID, selected.ID, models.UserBadgeSourceDraw, &selected)
		if err != nil {
			return BadgesRandomDrawResult{}, err
		}
		return BadgesRandomDrawResult{Success: true, Points: points, Ownership: &ownership}, nil
	})
	if err != nil {
		return BadgesRandomDrawResult{}, err
	}
	return outcome, nil
}

// BadgesPerformRandomMultiDraw mirrors performRandomBadgeMultiDraw. The whole
// batch runs in one transaction and charges upfront (10x cost); each badge is
// then drawn uniformly from the active catalogue.
func BadgesPerformRandomMultiDraw(ctx context.Context, userID int32, drawCount int32) (BadgesRandomMultiDrawResult, error) {
	if drawCount <= 0 {
		return BadgesRandomMultiDrawResult{Success: false, Points: 0, Ownerships: []BadgesOwnershipView{}}, nil
	}
	totalCost := BadgesRandomDrawCost * drawCount

	outcome, err := badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesRandomMultiDrawResult, error) {
		ok, points, err := badgesSpendPoints(txCtx, tx, userID, totalCost,
			models.PointsLogTypeBadgeDrawSpend, fmt.Sprintf("徽章十连抽取：%d 次", drawCount))
		if err != nil {
			return BadgesRandomMultiDrawResult{}, err
		}
		if !ok {
			return BadgesRandomMultiDrawResult{Success: false, Points: points, Ownerships: []BadgesOwnershipView{}}, nil
		}

		ownerships := make([]BadgesOwnershipView, 0, drawCount)
		for index := int32(0); index < drawCount; index++ {
			selected, err := BadgesSelectRandomOrThrow(txCtx, tx)
			if err != nil {
				return BadgesRandomMultiDrawResult{}, err
			}
			ownership, err := BadgesCreateOwnership(txCtx, tx, userID, selected.ID, models.UserBadgeSourceDraw, &selected)
			if err != nil {
				return BadgesRandomMultiDrawResult{}, err
			}
			ownerships = append(ownerships, ownership)
		}
		return BadgesRandomMultiDrawResult{Success: true, Points: points, Ownerships: ownerships}, nil
	})
	if err != nil {
		return BadgesRandomMultiDrawResult{}, err
	}
	return outcome, nil
}

// BadgesPerformSelect mirrors performSelectBadge.
func BadgesPerformSelect(ctx context.Context, userID int32, badgeID string) (BadgesSelectResult, error) {
	outcome, err := badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesSelectResult, error) {
		badge, err := BadgesGetByID(txCtx, tx, badgeID, BadgesCatalogOptions{ActiveOnly: true})
		if err != nil {
			return BadgesSelectResult{}, err
		}
		if badge == nil {
			return BadgesSelectResult{}, BadgesErrNotFound
		}

		ok, points, err := badgesSpendPoints(txCtx, tx, userID, BadgesSelectCost,
			models.PointsLogTypeBadgeSelectSpend, "徽章自选领取："+badge.FullLabel)
		if err != nil {
			return BadgesSelectResult{}, err
		}
		if !ok {
			return BadgesSelectResult{Success: false, Points: points}, nil
		}

		ownership, err := BadgesCreateOwnership(txCtx, tx, userID, badge.ID, models.UserBadgeSourceSelect, badge)
		if err != nil {
			return BadgesSelectResult{}, err
		}
		return BadgesSelectResult{Success: true, Points: points, Ownership: &ownership}, nil
	})
	if err != nil {
		return BadgesSelectResult{}, err
	}
	return outcome, nil
}

// ==================== application ====================

// badgesFindOwnership mirrors `userBadgeOwnership.findFirst({ where: { id, userId } })`
// including the appliedInstance relation.
func badgesFindOwnership(ctx context.Context, q BadgesQuerier, ownershipID, userID int32) (*badgesOwnershipRecord, error) {
	record, err := badgesScanOwnershipRow(q.QueryRow(ctx,
		badgesOwnershipSelect+" WHERE o.id = $1 AND o.user_id = $2 LIMIT 1", ownershipID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func badgesFindOwnershipByID(ctx context.Context, q BadgesQuerier, ownershipID int32) (*badgesOwnershipRecord, error) {
	record, err := badgesScanOwnershipRow(q.QueryRow(ctx,
		badgesOwnershipSelect+" WHERE o.id = $1 LIMIT 1", ownershipID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// BadgesApplyToAvatar mirrors applyBadgeToAvatar.
func BadgesApplyToAvatar(ctx context.Context, userID int32, ownershipID int32) (BadgesOwnershipView, error) {
	return badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesOwnershipView, error) {
		var avatarBadgeID *string
		err := tx.QueryRow(txCtx, `SELECT avatar_badge_id FROM users WHERE id = $1`, userID).Scan(&avatarBadgeID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return BadgesOwnershipView{}, err
		}

		ownership, err := badgesFindOwnership(txCtx, tx, ownershipID, userID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if ownership == nil {
			return BadgesOwnershipView{}, BadgesErrOwnershipNotFound
		}

		badgeMap, err := badgesGetCatalogMap(txCtx, tx)
		if err != nil {
			return BadgesOwnershipView{}, err
		}

		if ownership.ApplicationTarget != nil && *ownership.ApplicationTarget == string(models.BadgeApplicationTargetAvatar) &&
			avatarBadgeID != nil && *avatarBadgeID == ownership.BadgeID {
			return badgesMapOwnership(*ownership, badgeMap), nil
		}

		if ownership.ApplicationTarget != nil && *ownership.ApplicationTarget == string(models.BadgeApplicationTargetInstance) &&
			ownership.AppliedInstanceID != nil {
			if _, err := tx.Exec(txCtx, `UPDATE instances SET icon_badge_id = NULL, updated_at = NOW()
				WHERE id = $1 AND user_id = $2 AND icon_badge_id = $3`,
				*ownership.AppliedInstanceID, userID, ownership.BadgeID); err != nil {
				return BadgesOwnershipView{}, err
			}
		}

		if _, err := tx.Exec(txCtx, `UPDATE user_badge_ownerships
			SET application_target = NULL, applied_instance_id = NULL, applied_at = NULL, updated_at = NOW()
			WHERE user_id = $1 AND application_target = 'avatar' AND id <> $2`,
			userID, ownership.ID); err != nil {
			return BadgesOwnershipView{}, err
		}

		if _, err := tx.Exec(txCtx, `UPDATE users SET avatar_badge_id = $1, updated_at = NOW() WHERE id = $2`,
			ownership.BadgeID, userID); err != nil {
			return BadgesOwnershipView{}, err
		}

		tag, err := tx.Exec(txCtx, `UPDATE user_badge_ownerships
			SET application_target = 'avatar', applied_instance_id = NULL, applied_at = NOW(), updated_at = NOW()
			WHERE id = $1`, ownership.ID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if tag.RowsAffected() == 0 {
			return BadgesOwnershipView{}, BadgesErrStateConflict
		}

		updated, err := badgesFindOwnershipByID(txCtx, tx, ownership.ID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if updated == nil {
			// Mirrors findUniqueOrThrow.
			return BadgesOwnershipView{}, errors.New("BADGE_OWNERSHIP_NOT_FOUND")
		}
		return badgesMapOwnership(*updated, badgeMap), nil
	})
}

// BadgesApplyToInstance mirrors applyBadgeToInstance.
func BadgesApplyToInstance(ctx context.Context, userID int32, ownershipID int32, instanceID int32) (BadgesOwnershipView, error) {
	return badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesOwnershipView, error) {
		ownership, err := badgesFindOwnership(txCtx, tx, ownershipID, userID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}

		var (
			instanceIconBadgeID *string
			instanceName        string
		)
		instanceErr := tx.QueryRow(txCtx, `SELECT name, icon_badge_id FROM instances
			WHERE id = $1 AND user_id = $2 AND status <> 'deleted' LIMIT 1`,
			instanceID, userID).Scan(&instanceName, &instanceIconBadgeID)
		if instanceErr != nil && !errors.Is(instanceErr, pgx.ErrNoRows) {
			return BadgesOwnershipView{}, instanceErr
		}
		instanceFound := instanceErr == nil

		badgeMap, err := badgesGetCatalogMap(txCtx, tx)
		if err != nil {
			return BadgesOwnershipView{}, err
		}

		if ownership == nil {
			return BadgesOwnershipView{}, BadgesErrOwnershipNotFound
		}
		if !instanceFound {
			return BadgesOwnershipView{}, BadgesErrInstanceNotFound
		}

		if ownership.ApplicationTarget != nil && *ownership.ApplicationTarget == string(models.BadgeApplicationTargetInstance) &&
			ownership.AppliedInstanceID != nil && *ownership.AppliedInstanceID == instanceID &&
			instanceIconBadgeID != nil && *instanceIconBadgeID == ownership.BadgeID {
			return badgesMapOwnership(*ownership, badgeMap), nil
		}

		if ownership.ApplicationTarget != nil && *ownership.ApplicationTarget == string(models.BadgeApplicationTargetAvatar) {
			if _, err := tx.Exec(txCtx, `UPDATE users SET avatar_badge_id = NULL, updated_at = NOW()
				WHERE id = $1 AND avatar_badge_id = $2`, userID, ownership.BadgeID); err != nil {
				return BadgesOwnershipView{}, err
			}
		}

		if ownership.ApplicationTarget != nil && *ownership.ApplicationTarget == string(models.BadgeApplicationTargetInstance) &&
			ownership.AppliedInstanceID != nil && *ownership.AppliedInstanceID != instanceID {
			if _, err := tx.Exec(txCtx, `UPDATE instances SET icon_badge_id = NULL, updated_at = NOW()
				WHERE id = $1 AND user_id = $2 AND icon_badge_id = $3`,
				*ownership.AppliedInstanceID, userID, ownership.BadgeID); err != nil {
				return BadgesOwnershipView{}, err
			}
		}

		if _, err := tx.Exec(txCtx, `UPDATE user_badge_ownerships
			SET application_target = NULL, applied_instance_id = NULL, applied_at = NULL, updated_at = NOW()
			WHERE user_id = $1 AND application_target = 'instance' AND applied_instance_id = $2 AND id <> $3`,
			userID, instanceID, ownership.ID); err != nil {
			return BadgesOwnershipView{}, err
		}

		tag, err := tx.Exec(txCtx, `UPDATE instances SET icon_badge_id = $1, updated_at = NOW()
			WHERE id = $2 AND user_id = $3 AND status <> 'deleted'`, ownership.BadgeID, instanceID, userID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if tag.RowsAffected() == 0 {
			return BadgesOwnershipView{}, BadgesErrStateConflict
		}

		tag, err = tx.Exec(txCtx, `UPDATE user_badge_ownerships
			SET application_target = 'instance', applied_instance_id = $1, applied_at = NOW(), updated_at = NOW()
			WHERE id = $2`, instanceID, ownership.ID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if tag.RowsAffected() == 0 {
			return BadgesOwnershipView{}, BadgesErrStateConflict
		}

		updated, err := badgesFindOwnershipByID(txCtx, tx, ownership.ID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if updated == nil {
			return BadgesOwnershipView{}, errors.New("BADGE_OWNERSHIP_NOT_FOUND")
		}
		return badgesMapOwnership(*updated, badgeMap), nil
	})
}

// BadgesUnapply mirrors unapplyBadge.
func BadgesUnapply(ctx context.Context, userID int32, ownershipID int32) (BadgesOwnershipView, error) {
	return badgesRunTransaction(ctx, func(txCtx context.Context, tx pgx.Tx) (BadgesOwnershipView, error) {
		ownership, err := badgesFindOwnership(txCtx, tx, ownershipID, userID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		badgeMap, err := badgesGetCatalogMap(txCtx, tx)
		if err != nil {
			return BadgesOwnershipView{}, err
		}

		if ownership == nil {
			return BadgesOwnershipView{}, BadgesErrOwnershipNotFound
		}
		if ownership.ApplicationTarget == nil {
			return BadgesOwnershipView{}, BadgesErrNotApplied
		}

		if *ownership.ApplicationTarget == string(models.BadgeApplicationTargetAvatar) {
			tag, err := tx.Exec(txCtx, `UPDATE users SET avatar_badge_id = NULL, updated_at = NOW()
				WHERE id = $1 AND avatar_badge_id = $2`, userID, ownership.BadgeID)
			if err != nil {
				return BadgesOwnershipView{}, err
			}
			if tag.RowsAffected() == 0 {
				return BadgesOwnershipView{}, BadgesErrNotApplied
			}
		} else if ownership.AppliedInstanceID != nil {
			tag, err := tx.Exec(txCtx, `UPDATE instances SET icon_badge_id = NULL, updated_at = NOW()
				WHERE id = $1 AND user_id = $2 AND icon_badge_id = $3`,
				*ownership.AppliedInstanceID, userID, ownership.BadgeID)
			if err != nil {
				return BadgesOwnershipView{}, err
			}
			if tag.RowsAffected() == 0 {
				return BadgesOwnershipView{}, BadgesErrNotApplied
			}
		}

		tag, err := tx.Exec(txCtx, `UPDATE user_badge_ownerships
			SET application_target = NULL, applied_instance_id = NULL, applied_at = NULL, updated_at = NOW()
			WHERE id = $1 AND user_id = $2 AND application_target = $3
				AND applied_instance_id IS NOT DISTINCT FROM $4`,
			ownership.ID, userID, *ownership.ApplicationTarget, ownership.AppliedInstanceID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if tag.RowsAffected() == 0 {
			return BadgesOwnershipView{}, BadgesErrNotApplied
		}

		updated, err := badgesFindOwnershipByID(txCtx, tx, ownership.ID)
		if err != nil {
			return BadgesOwnershipView{}, err
		}
		if updated == nil {
			return BadgesOwnershipView{}, errors.New("BADGE_OWNERSHIP_NOT_FOUND")
		}
		return badgesMapOwnership(*updated, badgeMap), nil
	})
}

// ==================== admin catalogue ====================

// BadgesGetAdminCatalog mirrors getAdminBadgeCatalog.
func BadgesGetAdminCatalog(ctx context.Context) (BadgesAdminCatalog, error) {
	series, err := BadgesGetSeriesList(ctx, BadgesCatalogOptions{})
	if err != nil {
		return BadgesAdminCatalog{}, err
	}

	rows, err := Pool().Query(ctx, badgesJoinedSelect+
		" ORDER BY s.display_order ASC, b.display_order ASC, b.id ASC")
	if err != nil {
		return BadgesAdminCatalog{}, err
	}
	type adminRow struct {
		item      BadgesCatalogItem
		createdAt time.Time
		updatedAt time.Time
	}
	items := make([]adminRow, 0, 16)
	for rows.Next() {
		row, scanErr := badgesScanJoinedRow(rows)
		if scanErr != nil {
			rows.Close()
			return BadgesAdminCatalog{}, scanErr
		}
		items = append(items, adminRow{item: badgesMapJoinedRow(row), createdAt: row.CreatedAt, updatedAt: row.UpdatedAt})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return BadgesAdminCatalog{}, err
	}
	rows.Close()

	ownershipCounts, err := badgesCountByKey(ctx, `SELECT badge_id, COUNT(*) FROM user_badge_ownerships GROUP BY badge_id`)
	if err != nil {
		return BadgesAdminCatalog{}, err
	}
	avatarCounts, err := badgesCountByKey(ctx,
		`SELECT avatar_badge_id, COUNT(*) FROM users WHERE avatar_badge_id IS NOT NULL GROUP BY avatar_badge_id`)
	if err != nil {
		return BadgesAdminCatalog{}, err
	}
	instanceCounts, err := badgesCountByKey(ctx,
		`SELECT icon_badge_id, COUNT(*) FROM instances WHERE icon_badge_id IS NOT NULL GROUP BY icon_badge_id`)
	if err != nil {
		return BadgesAdminCatalog{}, err
	}

	badges := make([]BadgesAdminCatalogItem, 0, len(items))
	for _, item := range items {
		displayOrder := int32(0)
		if item.item.DisplayOrder != nil {
			displayOrder = *item.item.DisplayOrder
		}
		isActive := false
		if item.item.IsActive != nil {
			isActive = *item.item.IsActive
		}
		item.item.DisplayOrder = &displayOrder
		item.item.IsActive = &isActive

		createdAt, updatedAt := "", ""
		if !item.createdAt.IsZero() {
			createdAt = formatISOTime(item.createdAt)
		}
		if !item.updatedAt.IsZero() {
			updatedAt = formatISOTime(item.updatedAt)
		}
		badges = append(badges, BadgesAdminCatalogItem{
			BadgesCatalogItem: item.item,
			CreatedAt:         createdAt,
			UpdatedAt:         updatedAt,
			OwnershipCount:    ownershipCounts[item.item.ID],
			AvatarUseCount:    avatarCounts[item.item.ID],
			InstanceUseCount:  instanceCounts[item.item.ID],
		})
	}

	return BadgesAdminCatalog{Series: series, Badges: badges}, nil
}

func badgesCountByKey(ctx context.Context, sql string) (map[string]int64, error) {
	rows, err := Pool().Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var key *string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		if key != nil {
			counts[*key] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// ==================== admin CRUD ====================

const badgesSeriesColumns = `id, title, name_zh, name_en, description, source_id, source_label,
	display_order, is_active, created_at, updated_at`

const badgesColumns = `id, name, name_en, full_label, series_id, source_id, source_label,
	asset_url, asset_url_dark, asset_url_light, display_order, is_active, created_at, updated_at`

func badgesScanSeriesRow(row pgx.Row) (models.BadgeSeries, error) {
	var (
		id           string
		title        string
		nameZh       string
		nameEn       *string
		description  string
		sourceID     *string
		sourceLabel  *string
		displayOrder int32
		isActive     bool
		createdAt    time.Time
		updatedAt    time.Time
	)
	err := row.Scan(&id, &title, &nameZh, &nameEn, &description, &sourceID, &sourceLabel,
		&displayOrder, &isActive, &createdAt, &updatedAt)
	if err != nil {
		return models.BadgeSeries{}, err
	}
	return models.BadgeSeries{
		ID:           id,
		Title:        title,
		NameZh:       nameZh,
		NameEn:       nameEn,
		Description:  description,
		SourceID:     sourceID,
		SourceLabel:  sourceLabel,
		DisplayOrder: displayOrder,
		IsActive:     isActive,
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
	}, nil
}

func badgesScanBadgeRow(row pgx.Row) (models.Badge, error) {
	var (
		id            string
		name          string
		nameEn        *string
		fullLabel     string
		seriesID      string
		sourceID      *string
		sourceLabel   *string
		assetURL      string
		assetURLDark  *string
		assetURLLight *string
		displayOrder  int32
		isActive      bool
		createdAt     time.Time
		updatedAt     time.Time
	)
	err := row.Scan(&id, &name, &nameEn, &fullLabel, &seriesID, &sourceID, &sourceLabel,
		&assetURL, &assetURLDark, &assetURLLight, &displayOrder, &isActive, &createdAt, &updatedAt)
	if err != nil {
		return models.Badge{}, err
	}
	return models.Badge{
		ID:            id,
		Name:          name,
		NameEn:        nameEn,
		FullLabel:     fullLabel,
		SeriesID:      seriesID,
		SourceID:      sourceID,
		SourceLabel:   sourceLabel,
		AssetURL:      assetURL,
		AssetURLDark:  assetURLDark,
		AssetURLLight: assetURLLight,
		DisplayOrder:  displayOrder,
		IsActive:      isActive,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	}, nil
}

// BadgesCreateSeries mirrors createBadgeSeries.
func BadgesCreateSeries(ctx context.Context, input BadgesSeriesInput) (models.BadgeSeries, error) {
	displayOrder := int32(0)
	if input.DisplayOrder != nil {
		displayOrder = *input.DisplayOrder
	}
	isActive := true
	if input.IsActive != nil {
		isActive = *input.IsActive
	}
	return badgesScanSeriesRow(Pool().QueryRow(ctx, `INSERT INTO badge_series
		(id, title, name_zh, name_en, description, source_id, source_label, display_order, is_active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
		RETURNING `+badgesSeriesColumns,
		input.ID, input.Title, input.NameZh, input.NameEn, input.Description,
		input.SourceID, input.SourceLabel, displayOrder, isActive))
}

// BadgesUpdateSeries mirrors updateBadgeSeries. A nil pointer is `undefined` and
// is skipped; an explicit nil inner value clears the column.
func BadgesUpdateSeries(ctx context.Context, id string, input BadgesSeriesUpdate) (models.BadgeSeries, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if input.Title != nil {
		add("title", *input.Title)
	}
	if input.NameZh != nil {
		add("name_zh", *input.NameZh)
	}
	if input.NameEn != nil {
		add("name_en", *input.NameEn)
	}
	if input.Description != nil {
		add("description", *input.Description)
	}
	if input.SourceID != nil {
		add("source_id", *input.SourceID)
	}
	if input.SourceLabel != nil {
		add("source_label", *input.SourceLabel)
	}
	if input.DisplayOrder != nil {
		add("display_order", *input.DisplayOrder)
	}
	if input.IsActive != nil {
		add("is_active", *input.IsActive)
	}
	// Prisma still touches @updatedAt on an empty update.
	add("updated_at", time.Now().UTC())

	args = append(args, id)
	return badgesScanSeriesRow(Pool().QueryRow(ctx, `UPDATE badge_series SET `+strings.Join(sets, ", ")+
		` WHERE id = $`+fmt.Sprint(len(args))+` RETURNING `+badgesSeriesColumns, args...))
}

// BadgesDeleteSeries mirrors deleteBadgeSeries.
func BadgesDeleteSeries(ctx context.Context, id string) (models.BadgeSeries, error) {
	var count int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM badges WHERE series_id = $1`, id).Scan(&count); err != nil {
		return models.BadgeSeries{}, err
	}
	if count > 0 {
		return models.BadgeSeries{}, BadgesErrSeriesHasBadges
	}
	return badgesScanSeriesRow(Pool().QueryRow(ctx,
		`DELETE FROM badge_series WHERE id = $1 RETURNING `+badgesSeriesColumns, id))
}

// BadgesCreate mirrors createBadge.
func BadgesCreate(ctx context.Context, input BadgesInput) (models.Badge, error) {
	displayOrder := int32(0)
	if input.DisplayOrder != nil {
		displayOrder = *input.DisplayOrder
	}
	isActive := true
	if input.IsActive != nil {
		isActive = *input.IsActive
	}
	return badgesScanBadgeRow(Pool().QueryRow(ctx, `INSERT INTO badges
		(id, name, name_en, full_label, series_id, source_id, source_label, asset_url,
		 asset_url_dark, asset_url_light, display_order, is_active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW(), NOW())
		RETURNING `+badgesColumns,
		input.ID, input.Name, input.NameEn, input.FullLabel, input.SeriesID,
		input.SourceID, input.SourceLabel, input.AssetURL, input.AssetURLDark,
		input.AssetURLLight, displayOrder, isActive))
}

// BadgesUpdate mirrors updateBadge.
func BadgesUpdate(ctx context.Context, id string, input BadgesUpdateInput) (models.Badge, error) {
	sets := make([]string, 0, 10)
	args := make([]any, 0, 12)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.NameEn != nil {
		add("name_en", *input.NameEn)
	}
	if input.FullLabel != nil {
		add("full_label", *input.FullLabel)
	}
	if input.SeriesID != nil {
		add("series_id", *input.SeriesID)
	}
	if input.SourceID != nil {
		add("source_id", *input.SourceID)
	}
	if input.SourceLabel != nil {
		add("source_label", *input.SourceLabel)
	}
	if input.AssetURL != nil {
		add("asset_url", *input.AssetURL)
	}
	if input.AssetURLDark != nil {
		add("asset_url_dark", *input.AssetURLDark)
	}
	if input.AssetURLLight != nil {
		add("asset_url_light", *input.AssetURLLight)
	}
	if input.DisplayOrder != nil {
		add("display_order", *input.DisplayOrder)
	}
	if input.IsActive != nil {
		add("is_active", *input.IsActive)
	}
	add("updated_at", time.Now().UTC())

	args = append(args, id)
	return badgesScanBadgeRow(Pool().QueryRow(ctx, `UPDATE badges SET `+strings.Join(sets, ", ")+
		` WHERE id = $`+fmt.Sprint(len(args))+` RETURNING `+badgesColumns, args...))
}

// BadgesDelete mirrors deleteBadge.
func BadgesDelete(ctx context.Context, id string) (models.Badge, error) {
	var (
		ownershipCount   int64
		avatarUseCount   int64
		instanceUseCount int64
	)
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM user_badge_ownerships WHERE badge_id = $1`, id).Scan(&ownershipCount); err != nil {
		return models.Badge{}, err
	}
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE avatar_badge_id = $1`, id).Scan(&avatarUseCount); err != nil {
		return models.Badge{}, err
	}
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM instances WHERE icon_badge_id = $1`, id).Scan(&instanceUseCount); err != nil {
		return models.Badge{}, err
	}
	if ownershipCount > 0 || avatarUseCount > 0 || instanceUseCount > 0 {
		return models.Badge{}, BadgesErrInUse
	}
	return badgesScanBadgeRow(Pool().QueryRow(ctx,
		`DELETE FROM badges WHERE id = $1 RETURNING `+badgesColumns, id))
}

// BadgesSeedDefault mirrors seedDefaultBadges with the generated catalogue passed
// in: the TypeScript imports DEFAULT_BADGE_CATALOG from config/badges.generated.ts,
// whose 1.4k-line data file is not part of this module's file ownership. The
// seeding logic (guarded by system_config badge_catalog_seeded, no-op when any
// badge/series already exists) is otherwise identical.
func BadgesSeedDefault(ctx context.Context, catalog []BadgesCatalogItem) error {
	var seeded string
	err := Pool().QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = 'badge_catalog_seeded'`).Scan(&seeded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if seeded == "true" {
		return nil
	}

	var seriesCount int64
	var badgeCount int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM badge_series`).Scan(&seriesCount); err != nil {
		return err
	}
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM badges`).Scan(&badgeCount); err != nil {
		return err
	}
	if seriesCount > 0 || badgeCount > 0 {
		_, err := Pool().Exec(ctx, `INSERT INTO system_config (key, value, type, label, description, updated_at)
			VALUES ('badge_catalog_seeded', 'true', 'boolean', '徽章目录已初始化', '用于避免启动时重复导入默认徽章目录', NOW())
			ON CONFLICT (key) DO UPDATE SET value = 'true', updated_at = NOW()`)
		return err
	}

	type seriesSeed struct {
		id           string
		title        string
		nameZh       string
		nameEn       *string
		description  string
		sourceID     *string
		sourceLabel  *string
		displayOrder int32
	}
	seriesOrder := make([]string, 0, 8)
	seriesMap := map[string]seriesSeed{}
	for _, badge := range catalog {
		if _, exists := seriesMap[badge.SeriesID]; exists {
			continue
		}
		nameZh := badge.SeriesNameZh
		if nameZh == "" {
			nameZh = badge.SeriesTitle
		}
		seriesMap[badge.SeriesID] = seriesSeed{
			id:           badge.SeriesID,
			title:        badge.SeriesTitle,
			nameZh:       nameZh,
			nameEn:       badge.SeriesNameEn,
			description:  badge.SeriesDescription,
			sourceID:     badge.SourceID,
			sourceLabel:  badge.SourceLabel,
			displayOrder: int32(len(seriesMap)+1) * 100,
		}
		seriesOrder = append(seriesOrder, badge.SeriesID)
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, id := range seriesOrder {
		series := seriesMap[id]
		if _, err := tx.Exec(ctx, `INSERT INTO badge_series
			(id, title, name_zh, name_en, description, source_id, source_label, display_order, is_active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`,
			series.id, series.title, series.nameZh, series.nameEn, series.description,
			series.sourceID, series.sourceLabel, series.displayOrder); err != nil {
			return err
		}
	}

	for index, badge := range catalog {
		if _, err := tx.Exec(ctx, `INSERT INTO badges
			(id, name, name_en, full_label, series_id, source_id, source_label, asset_url,
			 asset_url_dark, asset_url_light, display_order, is_active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, true, NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`,
			badge.ID, badge.Name, badge.NameEn, badge.FullLabel, badge.SeriesID,
			badge.SourceID, badge.SourceLabel, badge.AssetURL, badge.AssetURLDark,
			badge.AssetURLLight, int32(index+1)*10); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `INSERT INTO system_config (key, value, type, label, description, updated_at)
		VALUES ('badge_catalog_seeded', 'true', 'boolean', '徽章目录已初始化', '用于避免启动时重复导入默认徽章目录', NOW())
		ON CONFLICT (key) DO UPDATE SET value = 'true', updated_at = NOW()`); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
