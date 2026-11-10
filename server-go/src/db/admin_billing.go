// Data access for server/src/routes/admin-billing.ts (Wave D step 4).
//
// This file holds only SQL and row shaping. Every money computation lives in
// routes/admin_billing.go so that ONE function produces the quote that is both
// previewed and executed (docs/WAVE_D_SPEC §5, "退款报价与执行必须同一函数").
//
// Transaction rules that apply to every helper here:
//
//   - callers pass an existing *gorm.DB transaction when the write must join one;
//     helpers never open a nested transaction on their own;
//   - timestamps written to `timestamp(3) without time zone` columns are always
//     explicit UTC values (WAVE_D_SPEC §0.2b);
//   - `financial_reconciliation_runs.updated_at` and `.created_at` have no default,
//     so the INSERT spells both out (TestRawInsertsSetUpdatedAt enforces the INSERT
//     half against schema.prisma);
//   - decimal columns are scanned into models.Decimal, never float64.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== overview ====================

// AdminBillingRechargeAggregate mirrors { amount, count } from the raw recharge
// aggregate query.
type AdminBillingRechargeAggregate struct {
	Amount float64
	Count  int64
}

// AdminBillingOverviewInput carries the date boundaries the route computes with
// lib.GetTodayRange / GetThisMonthStart / GetLastMonthRange.
type AdminBillingOverviewInput struct {
	TodayStart       time.Time
	Tomorrow         time.Time
	ThisMonthStart   time.Time
	LastMonthStart   time.Time
	LastMonthEnd     time.Time
	Now              time.Time
	ExpiringDeadline time.Time
}

// AdminBillingOverviewAff mirrors the aff block of the overview response.
type AdminBillingOverviewAff struct {
	TotalCommission     float64
	TotalOrders         int64
	ThisMonthCommission float64
	TotalConverted      float64
	PendingConvertCount int64
}

// AdminBillingOverview aggregates every figure the overview endpoint reports.
type AdminBillingOverview struct {
	TotalRevenue     float64
	ThisMonthRevenue float64
	LastMonthRevenue float64
	TodayRevenue     float64
	TotalRefunds     float64
	// TotalRefundsRaw is the SIGNED sum of the refund rows. The overview's
	// netRevenue is `totalRevenue + rawRefundSum` (the TypeScript adds the raw
	// sum, whose sign depends on how the refund row was written), not
	// totalRevenue - abs(there).
	TotalRefundsRaw          float64
	PaidInstancesCount       int64
	ActivePaidInstancesCount int64
	SuspendedCount           int64
	ExpiringCount            int64
	RechargeTotal            AdminBillingRechargeAggregate
	RechargeThisMonth        AdminBillingRechargeAggregate
	RechargeToday            AdminBillingRechargeAggregate
	Aff                      AdminBillingOverviewAff
}

// adminBillingRevenueTypes mirrors `revenueTypes` in the overview handler.
var adminBillingRevenueTypes = []string{"newPurchase", "renew", "upgrade", "transfer_fee"}

// GetAdminBillingOverview runs the sixteen aggregates of GET
// /api/admin/billing/overview. They are independent reads, so they run
// sequentially on the shared pool rather than in a transaction (Prisma's batch
// `$transaction([...])` is only an optimisation for round trips).
func GetAdminBillingOverview(ctx context.Context, input AdminBillingOverviewInput) (AdminBillingOverview, error) {
	if err := instancesRequireDB(); err != nil {
		return AdminBillingOverview{}, err
	}
	var overview AdminBillingOverview

	revenueSum := func(where string, args ...any) (float64, error) {
		var rows []struct {
			Total *models.Decimal `gorm:"column:total"`
		}
		query := `SELECT SUM(amount)::numeric AS total FROM instance_billing_records WHERE type IN ?`
		queryArgs := []any{adminBillingRevenueTypes}
		if where != "" {
			query += " AND " + where
			queryArgs = append(queryArgs, args...)
		}
		if err := DB.WithContext(ctx).Raw(query, queryArgs...).Scan(&rows).Error; err != nil {
			return 0, err
		}
		if len(rows) == 0 || rows[0].Total == nil {
			return 0, nil
		}
		return rows[0].Total.InexactFloat64(), nil
	}

	var err error
	if overview.TotalRevenue, err = revenueSum(""); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.ThisMonthRevenue, err = revenueSum("created_at >= ?", input.ThisMonthStart.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.LastMonthRevenue, err = revenueSum("created_at >= ? AND created_at <= ?",
		input.LastMonthStart.UTC(), input.LastMonthEnd.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.TodayRevenue, err = revenueSum("created_at >= ? AND created_at < ?",
		input.TodayStart.UTC(), input.Tomorrow.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}

	var refundRows []struct {
		Total *models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT SUM(amount)::numeric AS total FROM instance_billing_records WHERE type = 'refund'`,
	).Scan(&refundRows).Error; err != nil {
		return AdminBillingOverview{}, err
	}
	totalRefunds := 0.0
	if len(refundRows) > 0 && refundRows[0].Total != nil {
		totalRefunds = refundRows[0].Total.InexactFloat64()
	}
	overview.TotalRefundsRaw = totalRefunds
	// Math.abs for the displayed totalRefunds.
	if totalRefunds < 0 {
		totalRefunds = -totalRefunds
	}
	overview.TotalRefunds = totalRefunds

	countInstances := func(where string, args ...any) (int64, error) {
		var total int64
		query := DB.WithContext(ctx).Model(&models.Instance{}).Where(where, args...)
		if err := query.Count(&total).Error; err != nil {
			return 0, err
		}
		return total, nil
	}
	if overview.PaidInstancesCount, err = countInstances(
		"package_plan_id IS NOT NULL AND status <> ?", string(models.InstanceStatusDeleted)); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.ActivePaidInstancesCount, err = countInstances(
		"package_plan_id IS NOT NULL AND status NOT IN ? AND expires_at > ?",
		[]string{string(models.InstanceStatusDeleted), string(models.InstanceStatusSuspended)},
		input.Now.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.SuspendedCount, err = countInstances(
		"package_plan_id IS NOT NULL AND status = ?", string(models.InstanceStatusSuspended)); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.ExpiringCount, err = countInstances(
		"package_plan_id IS NOT NULL AND expires_at IS NOT NULL AND expires_at > ? AND expires_at <= ? AND status NOT IN ?",
		input.Now.UTC(), input.ExpiringDeadline.UTC(),
		[]string{string(models.InstanceStatusDeleted), string(models.InstanceStatusSuspended)}); err != nil {
		return AdminBillingOverview{}, err
	}

	rechargeAggregate := func(where string, args ...any) (AdminBillingRechargeAggregate, error) {
		var rows []struct {
			Amount *models.Decimal `gorm:"column:amount"`
			Count  int64           `gorm:"column:count"`
		}
		query := `SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS amount,
		                 COUNT(*)::int AS count
		            FROM recharge_records
		           WHERE status = 'completed'`
		if where != "" {
			query += " AND " + where
		}
		if err := DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
			return AdminBillingRechargeAggregate{}, err
		}
		if len(rows) == 0 {
			return AdminBillingRechargeAggregate{}, nil
		}
		amount := 0.0
		if rows[0].Amount != nil {
			amount = rows[0].Amount.InexactFloat64()
		}
		return AdminBillingRechargeAggregate{Amount: amount, Count: rows[0].Count}, nil
	}
	if overview.RechargeTotal, err = rechargeAggregate(""); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.RechargeThisMonth, err = rechargeAggregate("created_at >= ?", input.ThisMonthStart.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}
	if overview.RechargeToday, err = rechargeAggregate("created_at >= ? AND created_at < ?",
		input.TodayStart.UTC(), input.Tomorrow.UTC()); err != nil {
		return AdminBillingOverview{}, err
	}

	var affRows []struct {
		Total *models.Decimal `gorm:"column:total"`
		Count int64           `gorm:"column:count"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT SUM(amount)::numeric AS total, COUNT(id)::bigint AS count
		   FROM aff_logs WHERE type IN ?`, []string{"new_purchase", "renew"},
	).Scan(&affRows).Error; err != nil {
		return AdminBillingOverview{}, err
	}
	if len(affRows) > 0 {
		if affRows[0].Total != nil {
			overview.Aff.TotalCommission = affRows[0].Total.InexactFloat64()
		}
		overview.Aff.TotalOrders = affRows[0].Count
	}

	var thisMonthAffRows []struct {
		Total *models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT SUM(amount)::numeric AS total FROM aff_logs
		  WHERE type IN ? AND created_at >= ?`,
		[]string{"new_purchase", "renew"}, input.ThisMonthStart.UTC(),
	).Scan(&thisMonthAffRows).Error; err != nil {
		return AdminBillingOverview{}, err
	}
	if len(thisMonthAffRows) > 0 && thisMonthAffRows[0].Total != nil {
		overview.Aff.ThisMonthCommission = thisMonthAffRows[0].Total.InexactFloat64()
	}

	var convertedRows []struct {
		Total *models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT SUM(amount)::numeric AS total FROM aff_withdrawals WHERE status = 'approved'`,
	).Scan(&convertedRows).Error; err != nil {
		return AdminBillingOverview{}, err
	}
	if len(convertedRows) > 0 && convertedRows[0].Total != nil {
		overview.Aff.TotalConverted = convertedRows[0].Total.InexactFloat64()
	}

	if err := DB.WithContext(ctx).Model(&models.AffWithdrawal{}).
		Where("status = ?", string(models.AffWithdrawalStatusPending)).
		Count(&overview.Aff.PendingConvertCount).Error; err != nil {
		return AdminBillingOverview{}, err
	}
	return overview, nil
}

// ==================== billing record list ====================

// AdminBillingUserRef is the `{id, username}` projection used in several lists.
type AdminBillingUserRef struct {
	ID       int32  `json:"id" gorm:"column:id"`
	Username string `json:"username" gorm:"column:username"`
}

// AdminBillingBillingRecordListOptions mirrors the query of GET /records.
type AdminBillingBillingRecordListOptions struct {
	Page       int
	PageSize   int
	Type       string
	UserID     *int32
	InstanceID *int32
}

// AdminBillingBillingRecordInstance is the `{id, name}` include on a billing record.
type AdminBillingBillingRecordInstance struct {
	ID   int32  `json:"id" gorm:"column:id"`
	Name string `json:"name" gorm:"column:name"`
}

// AdminBillingBillingRecord is one row of the billing record list with its
// include projections.
type AdminBillingBillingRecord struct {
	Record   models.InstanceBillingRecord
	Instance *AdminBillingBillingRecordInstance
	User     *AdminBillingUserRef
}

// GetAdminBillingBillingRecords mirrors the findMany+count pair of GET /records.
func GetAdminBillingBillingRecords(ctx context.Context, options AdminBillingBillingRecordListOptions) ([]AdminBillingBillingRecord, int64, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, 0, err
	}

	query := DB.WithContext(ctx).Model(&models.InstanceBillingRecord{})
	if options.Type != "" {
		query = query.Where("type = ?", options.Type)
	}
	if options.UserID != nil {
		query = query.Where("user_id = ?", *options.UserID)
	}
	if options.InstanceID != nil {
		query = query.Where("instance_id = ?", *options.InstanceID)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var records []models.InstanceBillingRecord
	if err := query.Order("created_at DESC").
		Offset((options.Page - 1) * options.PageSize).
		Limit(options.PageSize).
		Find(&records).Error; err != nil {
		return nil, 0, err
	}

	rows := make([]AdminBillingBillingRecord, 0, len(records))
	instanceIDs := make([]int32, 0, len(records))
	userIDs := make([]int32, 0, len(records))
	seenInstance := map[int32]bool{}
	seenUser := map[int32]bool{}
	for _, record := range records {
		if !seenInstance[record.InstanceID] {
			seenInstance[record.InstanceID] = true
			instanceIDs = append(instanceIDs, record.InstanceID)
		}
		if !seenUser[record.UserID] {
			seenUser[record.UserID] = true
			userIDs = append(userIDs, record.UserID)
		}
	}

	instances := map[int32]AdminBillingBillingRecordInstance{}
	if len(instanceIDs) > 0 {
		var instanceRows []AdminBillingBillingRecordInstance
		if err := DB.WithContext(ctx).Model(&models.Instance{}).
			Select("id, name").Where("id IN ?", instanceIDs).
			Scan(&instanceRows).Error; err != nil {
			return nil, 0, err
		}
		for _, row := range instanceRows {
			instances[row.ID] = row
		}
	}

	users := map[int32]AdminBillingUserRef{}
	if len(userIDs) > 0 {
		var userRows []AdminBillingUserRef
		if err := DB.WithContext(ctx).Model(&models.User{}).
			Select("id, username").Where("id IN ?", userIDs).
			Scan(&userRows).Error; err != nil {
			return nil, 0, err
		}
		for _, row := range userRows {
			users[row.ID] = row
		}
	}

	for _, record := range records {
		row := AdminBillingBillingRecord{Record: record}
		if instance, ok := instances[record.InstanceID]; ok {
			copied := instance
			row.Instance = &copied
		}
		if user, ok := users[record.UserID]; ok {
			copied := user
			row.User = &copied
		}
		rows = append(rows, row)
	}
	return rows, total, nil
}

// ==================== paid instance list ====================

// AdminBillingPaidInstanceListOptions mirrors the query of GET
// /api/admin/billing/instances.
type AdminBillingPaidInstanceListOptions struct {
	Page       int
	PageSize   int
	Status     string
	Expiring   bool
	HostID     *int32
	Search     string
	Now        time.Time
	ExpiryCeil time.Time
}

// AdminBillingPaidInstanceUser is the user include of the paid instance list.
type AdminBillingPaidInstanceUser struct {
	ID            int32
	Username      string
	Email         *string
	AvatarStyle   string
	AvatarBadgeID *string
	Balance       float64
}

// AdminBillingPaidInstanceHost is the host include (plus the host owner role).
type AdminBillingPaidInstanceHost struct {
	ID           int32
	Name         string
	UserID       int32
	InstanceType string
	UserRole     string
}

// AdminBillingPackageRef is the `{id, name, instanceType}` package include.
type AdminBillingPackageRef struct {
	ID           int32
	Name         string
	InstanceType string
}

// AdminBillingPlanRef is the `{id, name}` packagePlan include.
type AdminBillingPlanRef struct {
	ID   int32
	Name string
}

// AdminBillingPaidInstance is one row of the paid instance list.
type AdminBillingPaidInstance struct {
	Instance        models.Instance
	User            AdminBillingPaidInstanceUser
	Host            AdminBillingPaidInstanceHost
	Package         *AdminBillingPackageRef
	PackagePlan     *AdminBillingPlanRef
	HasAffBinding   bool
	AffCodeEnabled  bool
	AffDiscountRate float64
}

// AdminBillingHostRef is the `{id, name}` host projection returned with the list.
type AdminBillingHostRef struct {
	ID   int32  `json:"id" gorm:"column:id"`
	Name string `json:"name" gorm:"column:name"`
}

type adminBillingPaidInstanceFlat struct {
	ID               int32           `gorm:"column:id"`
	IncusID          string          `gorm:"column:incus_id"`
	Name             string          `gorm:"column:name"`
	Status           string          `gorm:"column:status"`
	BillingPrice     *models.Decimal `gorm:"column:billing_price"`
	BillingCycle     *int32          `gorm:"column:billing_cycle"`
	ExpiresAt        *time.Time      `gorm:"column:expires_at"`
	CreatedAt        time.Time       `gorm:"column:created_at"`
	AutoRenew        bool            `gorm:"column:auto_renew"`
	SuspendedAt      *time.Time      `gorm:"column:suspended_at"`
	SuspendReason    *string         `gorm:"column:suspend_reason"`
	PackagePlanID    *int32          `gorm:"column:package_plan_id"`
	UserID           int32           `gorm:"column:user_id"`
	Username         string          `gorm:"column:username"`
	Email            *string         `gorm:"column:email"`
	AvatarStyle      string          `gorm:"column:avatar_style"`
	AvatarBadgeID    *string         `gorm:"column:avatar_badge_id"`
	Balance          models.Decimal  `gorm:"column:balance"`
	HostID           int32           `gorm:"column:host_id"`
	HostName         string          `gorm:"column:host_name"`
	HostUserID       int32           `gorm:"column:host_user_id"`
	HostInstanceType string          `gorm:"column:host_instance_type"`
	HostOwnerRole    string          `gorm:"column:host_owner_role"`
	PackageID        *int32          `gorm:"column:package_id"`
	PackageName      *string         `gorm:"column:package_name"`
	PackageType      *string         `gorm:"column:package_type"`
	PlanName         *string         `gorm:"column:plan_name"`
	AffBindingID     *int32          `gorm:"column:aff_binding_id"`
	AffDiscountRate  *models.Decimal `gorm:"column:aff_discount_rate"`
	AffCodeEnabled   *bool           `gorm:"column:aff_code_enabled"`
}

const adminBillingPaidInstanceSelect = `
	SELECT i.id, i.incus_id, i.name, i.status, i.billing_price, i.billing_cycle,
	       i.expires_at, i.created_at, i.auto_renew, i.suspended_at, i.suspend_reason,
	       i.package_plan_id, i.user_id,
	       u.username, u.email, u.avatar_style, u.avatar_badge_id, u.balance,
	       i.host_id, h.name AS host_name, h.user_id AS host_user_id,
	       h.instance_type AS host_instance_type, hu.role AS host_owner_role,
	       i.package_id, p.name AS package_name, p.instance_type AS package_type,
	       pp.name AS plan_name,
	       ab.id AS aff_binding_id, ac.discount_rate AS aff_discount_rate,
	       ac.enabled AS aff_code_enabled
	  FROM instances i
	  JOIN users u ON u.id = i.user_id
	  JOIN hosts h ON h.id = i.host_id
	  JOIN users hu ON hu.id = h.user_id
	  LEFT JOIN packages p ON p.id = i.package_id
	  LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
	  LEFT JOIN aff_bindings ab ON ab.instance_id = i.id
	  LEFT JOIN aff_codes ac ON ac.id = ab.aff_code_id`

// GetAdminBillingPaidInstances mirrors the findMany+count pair of
// GET /api/admin/billing/instances. `search` is already trimmed and truncated by
// the caller.
func GetAdminBillingPaidInstances(ctx context.Context, options AdminBillingPaidInstanceListOptions) ([]AdminBillingPaidInstance, int64, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, 0, err
	}

	where := []string{"i.package_plan_id IS NOT NULL"}
	args := []any{}
	if options.Status != "" {
		where = append(where, "i.status = ?")
		args = append(args, options.Status)
	} else {
		where = append(where, "i.status <> ?")
		args = append(args, string(models.InstanceStatusDeleted))
	}
	if options.Expiring {
		where = append(where, "i.expires_at IS NOT NULL AND i.expires_at > ? AND i.expires_at <= ?")
		args = append(args, options.Now.UTC(), options.ExpiryCeil.UTC())
	}
	if options.HostID != nil {
		where = append(where, "i.host_id = ?")
		args = append(args, *options.HostID)
	}
	if options.Search != "" {
		like := "%" + options.Search + "%"
		where = append(where, `(i.name ILIKE ? OR u.username ILIKE ? OR h.name ILIKE ?
			OR p.name ILIKE ? OR pp.name ILIKE ?)`)
		args = append(args, like, like, like, like, like)
	}
	whereSQL := " WHERE " + adminBillingJoin(where, " AND ")

	// The count must ignore join-side fan-out (there is at most one aff binding per
	// instance, so COUNT(*) on the same FROM is exact; DISTINCT makes that explicit).
	var total int64
	countQuery := `
	SELECT COUNT(*)::bigint AS total FROM (
		SELECT DISTINCT i.id FROM instances i
		  JOIN users u ON u.id = i.user_id
		  JOIN hosts h ON h.id = i.host_id
		  JOIN users hu ON hu.id = h.user_id
		  LEFT JOIN packages p ON p.id = i.package_id
		  LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		  LEFT JOIN aff_bindings ab ON ab.instance_id = i.id
		  LEFT JOIN aff_codes ac ON ac.id = ab.aff_code_id` + whereSQL + `
	) AS matched`
	var countRows []struct {
		Total int64 `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(countQuery, args...).Scan(&countRows).Error; err != nil {
		return nil, 0, err
	}
	if len(countRows) > 0 {
		total = countRows[0].Total
	}

	// Two-step paging, deliberately: the TypeScript's Prisma findMany with an
	// `include` issues its ORDER BY/LIMIT against the instances table alone, and
	// rows that share `expires_at` are ordered by whatever that scan returns. Doing
	// the page selection on an instances-only query reproduces that; adding a
	// secondary sort key (or sorting the joined result) would reorder the ties and
	// break parity.
	// The page selection runs on the instances table alone whenever the filter does
	// not need a join (no keyword search). Prisma's `include` loads relations with a
	// second query, so its ORDER BY/LIMIT sees a plain instances scan; keeping the
	// Go query identical keeps the tie order of equal `expires_at` values identical
	// too. A keyword search has to join (name/host/package/plan), and in that case
	// the fixture rows have distinct expiries.
	var pageQuery string
	if options.Search == "" {
		pageQuery = `SELECT i.id FROM instances i` + whereSQL + `
		ORDER BY i.expires_at ASC LIMIT ? OFFSET ?`
	} else {
		pageQuery = `SELECT i.id FROM instances i
		  JOIN users u ON u.id = i.user_id
		  JOIN hosts h ON h.id = i.host_id
		  JOIN users hu ON hu.id = h.user_id
		  LEFT JOIN packages p ON p.id = i.package_id
		  LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		  LEFT JOIN aff_bindings ab ON ab.instance_id = i.id
		  LEFT JOIN aff_codes ac ON ac.id = ab.aff_code_id` + whereSQL + `
		ORDER BY i.expires_at ASC LIMIT ? OFFSET ?`
	}
	pageArgs := append(append([]any{}, args...), options.PageSize, (options.Page-1)*options.PageSize)
	var pageRows []struct {
		ID int32 `gorm:"column:id"`
	}
	if err := DB.WithContext(ctx).Raw(pageQuery, pageArgs...).Scan(&pageRows).Error; err != nil {
		return nil, 0, err
	}
	if len(pageRows) == 0 {
		return []AdminBillingPaidInstance{}, total, nil
	}

	pageIDs := make([]int32, 0, len(pageRows))
	for _, row := range pageRows {
		pageIDs = append(pageIDs, row.ID)
	}
	var flat []adminBillingPaidInstanceFlat
	if err := DB.WithContext(ctx).Raw(
		adminBillingPaidInstanceSelect+` WHERE i.id IN ?`, pageIDs,
	).Scan(&flat).Error; err != nil {
		return nil, 0, err
	}
	ordered := make([]adminBillingPaidInstanceFlat, 0, len(flat))
	byID := make(map[int32]adminBillingPaidInstanceFlat, len(flat))
	for _, item := range flat {
		byID[item.ID] = item
	}
	for _, id := range pageIDs {
		if item, ok := byID[id]; ok {
			ordered = append(ordered, item)
		}
	}
	flat = ordered

	rows := make([]AdminBillingPaidInstance, 0, len(flat))
	for _, item := range flat {
		instance := models.Instance{
			ID:            item.ID,
			IncusID:       item.IncusID,
			Name:          item.Name,
			Status:        models.InstanceStatus(item.Status),
			AutoRenew:     item.AutoRenew,
			BillingCycle:  item.BillingCycle,
			ExpiresAt:     item.ExpiresAt,
			CreatedAt:     item.CreatedAt,
			SuspendedAt:   item.SuspendedAt,
			SuspendReason: item.SuspendReason,
			PackagePlanID: item.PackagePlanID,
			UserID:        item.UserID,
			HostID:        item.HostID,
			BillingPrice:  item.BillingPrice,
		}
		row := AdminBillingPaidInstance{
			Instance: instance,
			User: AdminBillingPaidInstanceUser{
				ID:            item.UserID,
				Username:      item.Username,
				Email:         item.Email,
				AvatarStyle:   item.AvatarStyle,
				AvatarBadgeID: item.AvatarBadgeID,
				Balance:       item.Balance.InexactFloat64(),
			},
			Host: AdminBillingPaidInstanceHost{
				ID:           item.HostID,
				Name:         item.HostName,
				UserID:       item.HostUserID,
				InstanceType: item.HostInstanceType,
				UserRole:     item.HostOwnerRole,
			},
		}
		if item.PackageID != nil && item.PackageName != nil {
			packageType := "container"
			if item.PackageType != nil {
				packageType = *item.PackageType
			}
			row.Package = &AdminBillingPackageRef{ID: *item.PackageID, Name: *item.PackageName, InstanceType: packageType}
		}
		if item.PackagePlanID != nil && item.PlanName != nil {
			row.PackagePlan = &AdminBillingPlanRef{ID: *item.PackagePlanID, Name: *item.PlanName}
		}
		if item.AffBindingID != nil {
			row.HasAffBinding = true
			if item.AffCodeEnabled != nil && *item.AffCodeEnabled && item.AffDiscountRate != nil {
				row.AffCodeEnabled = true
				row.AffDiscountRate = item.AffDiscountRate.InexactFloat64()
			}
		}
		rows = append(rows, row)
	}
	return rows, total, nil
}

// adminBillingJoin joins SQL fragments with a separator (small helper so the
// where-clause builder stays readable).
func adminBillingJoin(parts []string, separator string) string {
	out := ""
	for index, part := range parts {
		if index > 0 {
			out += separator
		}
		out += part
	}
	return out
}

// GetAdminBillingPaidInstanceHosts mirrors the `distinct hosts` query: every host
// that owns at least one non-deleted paid instance, deduplicated by id.
func GetAdminBillingPaidInstanceHosts(ctx context.Context) ([]AdminBillingHostRef, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var rows []AdminBillingHostRef
	if err := DB.WithContext(ctx).Raw(
		`SELECT DISTINCT h.id AS id, h.name AS name
		   FROM instances i JOIN hosts h ON h.id = i.host_id
		  WHERE i.package_plan_id IS NOT NULL AND i.status <> ?`,
		string(models.InstanceStatusDeleted),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ==================== instance + user context ====================

// AdminBillingInstanceContext is the `include: {user: {id, username, balance}}`
// projection several admin-billing handlers need.
type AdminBillingInstanceContext struct {
	Instance models.Instance
	User     AdminBillingPaidInstanceUser
}

// GetAdminBillingInstanceContext loads the instance with its owner. It returns
// (nil, nil) when the instance does not exist, like Prisma's findUnique.
func GetAdminBillingInstanceContext(ctx context.Context, instanceID int32) (*AdminBillingInstanceContext, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var instances []models.Instance
	if err := DB.WithContext(ctx).Where("id = ?", instanceID).Limit(1).Find(&instances).Error; err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, nil
	}
	instance := instances[0]

	var users []struct {
		ID            int32          `gorm:"column:id"`
		Username      string         `gorm:"column:username"`
		Email         *string        `gorm:"column:email"`
		AvatarStyle   string         `gorm:"column:avatar_style"`
		AvatarBadgeID *string        `gorm:"column:avatar_badge_id"`
		Balance       models.Decimal `gorm:"column:balance"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, username, email, avatar_style, avatar_badge_id, balance FROM users WHERE id = ?`,
		instance.UserID,
	).Scan(&users).Error; err != nil {
		return nil, err
	}
	result := &AdminBillingInstanceContext{Instance: instance}
	if len(users) > 0 {
		result.User = AdminBillingPaidInstanceUser{
			ID:            users[0].ID,
			Username:      users[0].Username,
			Email:         users[0].Email,
			AvatarStyle:   users[0].AvatarStyle,
			AvatarBadgeID: users[0].AvatarBadgeID,
			Balance:       users[0].Balance.InexactFloat64(),
		}
	}
	return result, nil
}

// GetAdminBillingAvailablePlans mirrors the available-plans query:
// `isActive: true, isSoldOut: false, orderBy: {sortOrder: 'asc'}`.
//
// It reuses GetPackagePlansByPackageId (the typed row scanner) instead of a
// Find(&models.PackagePlan): the generated model declares several columns as
// `serializer:json` Int[] values, and a full-model scan fails on this schema.
// is_sold_out is filtered in Go because the shared query only knows activeOnly.
func GetAdminBillingAvailablePlans(ctx context.Context, packageID int32) ([]models.PackagePlan, error) {
	plans, err := GetPackagePlansByPackageId(ctx, packageID, PackagePlansOptions{ActiveOnly: true})
	if err != nil {
		return nil, err
	}
	result := make([]models.PackagePlan, 0, len(plans))
	for _, plan := range plans {
		if plan.IsSoldOut {
			continue
		}
		result = append(result, plan)
	}
	return result, nil
}

// ==================== reconciliation ====================

// AdminBillingReconciliationRun is a run with its include projections.
type AdminBillingReconciliationRun struct {
	Run       models.FinancialReconciliationRun
	CreatedBy *AdminBillingUserRef
	UpdatedBy *AdminBillingUserRef
	Items     []AdminBillingReconciliationItem
}

// AdminBillingReconciliationItem is one run item with the `user` / `handledBy`
// includes.
type AdminBillingReconciliationItem struct {
	Item      models.FinancialReconciliationItem
	User      *AdminBillingUserRef
	HandledBy *AdminBillingUserRef
}

// GetAdminBillingReconciliationRun mirrors the findUnique by date with includes.
// Returns (nil, nil) when the date has no run yet.
func GetAdminBillingReconciliationRun(ctx context.Context, dateString string) (*AdminBillingReconciliationRun, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var runs []models.FinancialReconciliationRun
	if err := DB.WithContext(ctx).
		Where("date = ?::date", dateString).
		Limit(1).
		Find(&runs).Error; err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return adminBillingLoadRun(ctx, DB, runs[0].ID)
}

// adminBillingLoadRun loads a run plus its relations, ordered exactly like the
// TypeScript (`orderBy: [{status: 'asc'}, {id: 'asc'}]`).
func adminBillingLoadRun(ctx context.Context, client *gorm.DB, runID int32) (*AdminBillingReconciliationRun, error) {
	result := &AdminBillingReconciliationRun{Items: []AdminBillingReconciliationItem{}}

	runs := []models.FinancialReconciliationRun{}
	if err := client.WithContext(ctx).Where("id = ?", runID).Limit(1).Find(&runs).Error; err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, errors.New("对账记录不存在")
	}
	result.Run = runs[0]

	userIDs := make([]int32, 0, 2)
	if result.Run.CreatedByUserID != nil {
		userIDs = append(userIDs, *result.Run.CreatedByUserID)
	}
	if result.Run.UpdatedByUserID != nil {
		userIDs = append(userIDs, *result.Run.UpdatedByUserID)
	}

	items := []models.FinancialReconciliationItem{}
	if err := client.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("status ASC").
		Order("id ASC").
		Find(&items).Error; err != nil {
		return nil, err
	}

	for _, item := range items {
		if item.UserID != nil {
			userIDs = append(userIDs, *item.UserID)
		}
		if item.HandledByUserID != nil {
			userIDs = append(userIDs, *item.HandledByUserID)
		}
	}

	users, err := adminBillingLoadUserRefs(ctx, client, userIDs)
	if err != nil {
		return nil, err
	}
	if result.Run.CreatedByUserID != nil {
		if user, ok := users[*result.Run.CreatedByUserID]; ok {
			copied := user
			result.CreatedBy = &copied
		}
	}
	if result.Run.UpdatedByUserID != nil {
		if user, ok := users[*result.Run.UpdatedByUserID]; ok {
			copied := user
			result.UpdatedBy = &copied
		}
	}

	for _, item := range items {
		row := AdminBillingReconciliationItem{Item: item}
		if item.UserID != nil {
			if user, ok := users[*item.UserID]; ok {
				copied := user
				row.User = &copied
			}
		}
		if item.HandledByUserID != nil {
			if user, ok := users[*item.HandledByUserID]; ok {
				copied := user
				row.HandledBy = &copied
			}
		}
		result.Items = append(result.Items, row)
	}
	return result, nil
}

// GetAdminBillingUserRefs is the exported wrapper around adminBillingLoadUserRefs
// for route modules that need `{id, username}` projections on a result set.
func GetAdminBillingUserRefs(ctx context.Context, userIDs []int32) (map[int32]AdminBillingUserRef, error) {
	return adminBillingLoadUserRefs(ctx, DB, userIDs)
}

// adminBillingLoadUserRefs resolves `{id, username}` for a list of ids.
func adminBillingLoadUserRefs(ctx context.Context, client *gorm.DB, userIDs []int32) (map[int32]AdminBillingUserRef, error) {
	result := map[int32]AdminBillingUserRef{}
	unique := make([]int32, 0, len(userIDs))
	seen := map[int32]bool{}
	for _, id := range userIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return result, nil
	}

	var rows []AdminBillingUserRef
	if err := client.WithContext(ctx).Raw(
		`SELECT id, username FROM users WHERE id IN ?`, unique,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

// AdminBillingReconciliationRunInput is the input of the run (or rerun) action.
type AdminBillingReconciliationRunInput struct {
	DateString string
	Start      time.Time
	End        time.Time
	ActorID    int32
}

// AdminBillingReconciliationCandidate mirrors ReconciliationCandidate.
type AdminBillingReconciliationCandidate struct {
	ItemKey    string
	ItemType   string
	SourceType string
	SourceID   *int32
	UserID     *int32
	Amount     *models.Decimal
	Title      string
	Detail     map[string]any
}

// RunAdminBillingReconciliation mirrors POST /reconciliation/run: detect the four
// classes of discrepancy, upsert the run for that date and upsert every item by
// (runId, itemKey), then recompute the run status.
//
// Idempotency (docs/WAVE_D_SPEC §5): the run is unique per date and the items are
// unique per (run, itemKey), so rerunning the same day cannot duplicate a row and
// cannot resurrect an item an operator already resolved — its `status`, `note`,
// `handledBy` and `handledAt` are left untouched. Items that are no longer
// detected are deleted ONLY while their status is still `discrepancy`, so an
// operator's decision survives a rerun.
func RunAdminBillingReconciliation(ctx context.Context, input AdminBillingReconciliationRunInput) (*AdminBillingReconciliationRun, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}

	var runID int32
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidates, summary, err := adminBillingDetectDiscrepancies(ctx, tx, input)
		if err != nil {
			return err
		}

		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		if err := tx.WithContext(ctx).Raw(
			`INSERT INTO financial_reconciliation_runs
				(date, status, summary, created_by_user_id, updated_by_user_id, created_at, updated_at)
			 VALUES (?::date, 'normal', ?::jsonb, ?, ?, ?, ?)
			 ON CONFLICT (date) DO UPDATE
			    SET summary = EXCLUDED.summary,
			        updated_by_user_id = EXCLUDED.updated_by_user_id,
			        updated_at = EXCLUDED.updated_at
			 RETURNING id`,
			input.DateString, string(summaryJSON), input.ActorID, input.ActorID, now, now,
		).Scan(&runID).Error; err != nil {
			return err
		}

		activeKeys := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			activeKeys = append(activeKeys, candidate.ItemKey)
		}

		if len(activeKeys) > 0 {
			if err := tx.Exec(
				`DELETE FROM financial_reconciliation_items
				  WHERE run_id = ? AND status = 'discrepancy' AND item_key NOT IN ?`,
				runID, activeKeys,
			).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Exec(
				`DELETE FROM financial_reconciliation_items WHERE run_id = ? AND status = 'discrepancy'`,
				runID,
			).Error; err != nil {
				return err
			}
		}

		for _, candidate := range candidates {
			detailJSON, err := json.Marshal(candidate.Detail)
			if err != nil {
				return err
			}
			var amount any
			if candidate.Amount != nil {
				amount = *candidate.Amount
			}
			if err := tx.Exec(
				`INSERT INTO financial_reconciliation_items
					(run_id, item_key, item_type, status, source_type, source_id, user_id, amount,
					 title, detail, created_at, updated_at)
				 VALUES (?, ?, ?, 'discrepancy', ?, ?, ?, ?, ?, ?::jsonb, ?, ?)
				 ON CONFLICT (run_id, item_key) DO UPDATE
				    SET item_type = EXCLUDED.item_type,
				        source_type = EXCLUDED.source_type,
				        source_id = EXCLUDED.source_id,
				        user_id = EXCLUDED.user_id,
				        amount = EXCLUDED.amount,
				        title = EXCLUDED.title,
				        detail = EXCLUDED.detail,
				        updated_at = EXCLUDED.updated_at`,
				runID, candidate.ItemKey, candidate.ItemType, candidate.SourceType,
				candidate.SourceID, candidate.UserID, amount, candidate.Title,
				string(detailJSON), now, now,
			).Error; err != nil {
				return err
			}
		}

		statuses := []string{}
		if len(activeKeys) > 0 {
			var statusRows []struct {
				Status string `gorm:"column:status"`
			}
			if err := tx.WithContext(ctx).Raw(
				`SELECT status FROM financial_reconciliation_items WHERE run_id = ? AND item_key IN ?`,
				runID, activeKeys,
			).Scan(&statusRows).Error; err != nil {
				return err
			}
			for _, row := range statusRows {
				statuses = append(statuses, row.Status)
			}
		}
		nextStatus := resolveAdminBillingRunStatus(statuses)

		return tx.Exec(
			`UPDATE financial_reconciliation_runs SET status = ?, updated_at = ? WHERE id = ?`,
			nextStatus, now, runID,
		).Error
	})
	if err != nil {
		return nil, err
	}
	return adminBillingLoadRun(ctx, DB, runID)
}

// resolveAdminBillingRunStatus mirrors resolveRunStatus. Empty means 'normal'
// (nothing to reconcile), not 'ignored'.
func resolveAdminBillingRunStatus(statuses []string) string {
	if len(statuses) == 0 {
		return "normal"
	}
	for _, status := range statuses {
		if status == "discrepancy" {
			return "discrepancy"
		}
	}
	for _, status := range statuses {
		if status == "confirmed" {
			return "confirmed"
		}
	}
	return "ignored"
}

// adminBillingDetectDiscrepancies runs the four detection queries and builds the
// candidates plus the summary object.
func adminBillingDetectDiscrepancies(
	ctx context.Context,
	tx *gorm.DB,
	input AdminBillingReconciliationRunInput,
) ([]AdminBillingReconciliationCandidate, map[string]any, error) {
	start := input.Start.UTC()
	end := input.End.UTC()

	type rechargeRow struct {
		ID           int32           `gorm:"column:id"`
		OrderNo      string          `gorm:"column:order_no"`
		UserID       int32           `gorm:"column:user_id"`
		Amount       models.Decimal  `gorm:"column:amount"`
		ActualAmount *models.Decimal `gorm:"column:actual_amount"`
		Fee          models.Decimal  `gorm:"column:fee"`
		CompletedAt  *time.Time      `gorm:"column:completed_at"`
		CallbackAt   *time.Time      `gorm:"column:callback_at"`
		CreatedAt    time.Time       `gorm:"column:created_at"`
	}
	var completedRecharges []rechargeRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT id, order_no, user_id, amount, actual_amount, fee, completed_at, callback_at, created_at
		   FROM recharge_records
		  WHERE status = 'completed'
		    AND ( (completed_at >= ? AND completed_at < ?)
		       OR (completed_at IS NULL AND callback_at >= ? AND callback_at < ?)
		       OR (completed_at IS NULL AND callback_at IS NULL AND created_at >= ? AND created_at < ?) )`,
		start, end, start, end, start, end,
	).Scan(&completedRecharges).Error; err != nil {
		return nil, nil, err
	}

	rechargeOrderNos := make([]string, 0, len(completedRecharges))
	for _, record := range completedRecharges {
		rechargeOrderNos = append(rechargeOrderNos, record.OrderNo)
	}
	rechargeLogOrderNos := map[string]bool{}
	if len(rechargeOrderNos) > 0 {
		var logRows []struct {
			OrderID *string `gorm:"column:order_id"`
		}
		if err := tx.WithContext(ctx).Raw(
			`SELECT order_id FROM balance_logs WHERE type = 'recharge' AND order_id IN ?`,
			rechargeOrderNos,
		).Scan(&logRows).Error; err != nil {
			return nil, nil, err
		}
		for _, row := range logRows {
			if row.OrderID != nil {
				rechargeLogOrderNos[*row.OrderID] = true
			}
		}
	}

	type balanceLogRow struct {
		ID         int32          `gorm:"column:id"`
		UserID     int32          `gorm:"column:user_id"`
		Type       string         `gorm:"column:type"`
		Amount     models.Decimal `gorm:"column:amount"`
		OrderID    *string        `gorm:"column:order_id"`
		InstanceID *int32         `gorm:"column:instance_id"`
		Remark     *string        `gorm:"column:remark"`
		CreatedAt  time.Time      `gorm:"column:created_at"`
	}
	var businessBalanceLogs []balanceLogRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT id, user_id, type, amount, order_id, instance_id, remark, created_at
		   FROM balance_logs
		  WHERE created_at >= ? AND created_at < ?
		    AND type IN ?`,
		start, end, []string{"recharge", "consume", "refund", "transfer_fee", "transfer_refund"},
	).Scan(&businessBalanceLogs).Error; err != nil {
		return nil, nil, err
	}

	type deliveredInstanceRow struct {
		ID        int32     `gorm:"column:id"`
		Name      string    `gorm:"column:name"`
		UserID    int32     `gorm:"column:user_id"`
		IncusID   string    `gorm:"column:incus_id"`
		Status    string    `gorm:"column:status"`
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	var deliveredInstances []deliveredInstanceRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT i.id, i.name, i.user_id, i.incus_id, i.status, i.created_at
		   FROM instances i
		  WHERE i.package_plan_id IS NOT NULL
		    AND i.status <> 'deleted'
		    AND i.created_at >= ? AND i.created_at < ?
		    AND NOT EXISTS (SELECT 1 FROM instance_billing_records r WHERE r.instance_id = i.id)`,
		start, end,
	).Scan(&deliveredInstances).Error; err != nil {
		return nil, nil, err
	}

	type adjustmentRow struct {
		ID          int32          `gorm:"column:id"`
		UserID      int32          `gorm:"column:user_id"`
		Amount      models.Decimal `gorm:"column:amount"`
		RequestType string         `gorm:"column:request_type"`
		OrderNo     *string        `gorm:"column:order_no"`
		SourceType  *string        `gorm:"column:source_type"`
		SourceID    *int32         `gorm:"column:source_id"`
		ReviewedAt  *time.Time     `gorm:"column:reviewed_at"`
		UpdatedAt   time.Time      `gorm:"column:updated_at"`
	}
	var approvedAdjustments []adjustmentRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT id, user_id, amount, request_type, order_no, source_type, source_id, reviewed_at, updated_at
		   FROM balance_adjustment_requests
		  WHERE status = 'approved'
		    AND balance_log_id IS NULL
		    AND ( (reviewed_at >= ? AND reviewed_at < ?)
		       OR (reviewed_at IS NULL AND updated_at >= ? AND updated_at < ?) )`,
		start, end, start, end,
	).Scan(&approvedAdjustments).Error; err != nil {
		return nil, nil, err
	}

	candidates := make([]AdminBillingReconciliationCandidate, 0, len(completedRecharges)+len(businessBalanceLogs))
	for _, record := range completedRecharges {
		if rechargeLogOrderNos[record.OrderNo] {
			continue
		}
		amount := record.Amount
		if record.ActualAmount != nil {
			amount = *record.ActualAmount
		}
		candidates = append(candidates, AdminBillingReconciliationCandidate{
			ItemKey:    "recharge:" + itoa32(record.ID) + ":missing-balance-log",
			ItemType:   "recharge_missing_balance_log",
			SourceType: "recharge",
			SourceID:   adminBillingInt32Ptr(record.ID),
			UserID:     adminBillingInt32Ptr(record.UserID),
			Amount:     adminBillingAmountToMoney(amount),
			Title:      "成功充值未找到入账流水：" + record.OrderNo,
			Detail: map[string]any{
				"orderNo":     record.OrderNo,
				"completedAt": adminBillingISOTimeOrNil(record.CompletedAt),
				"callbackAt":  adminBillingISOTimeOrNil(record.CallbackAt),
				"createdAt":   adminBillingISOTime(record.CreatedAt),
			},
		})
	}

	for _, log := range businessBalanceLogs {
		if log.OrderID != nil || log.InstanceID != nil {
			continue
		}
		candidates = append(candidates, AdminBillingReconciliationCandidate{
			ItemKey:    "balance-log:" + itoa32(log.ID) + ":missing-source",
			ItemType:   "orphan_balance_log",
			SourceType: "balance_log",
			SourceID:   adminBillingInt32Ptr(log.ID),
			UserID:     adminBillingInt32Ptr(log.UserID),
			Amount:     adminBillingAmountToMoney(log.Amount),
			Title:      "余额流水缺少业务来源：#" + itoa32(log.ID),
			Detail: map[string]any{
				"type":      log.Type,
				"remark":    adminBillingStringOrNil(log.Remark),
				"createdAt": adminBillingISOTime(log.CreatedAt),
			},
		})
	}

	for _, instance := range deliveredInstances {
		candidates = append(candidates, AdminBillingReconciliationCandidate{
			ItemKey:    "instance:" + itoa32(instance.ID) + ":missing-billing",
			ItemType:   "delivered_instance_missing_billing",
			SourceType: "instance",
			SourceID:   adminBillingInt32Ptr(instance.ID),
			UserID:     adminBillingInt32Ptr(instance.UserID),
			Amount:     nil,
			Title:      "付费实例已交付但无扣费记录：" + instance.Name,
			Detail: map[string]any{
				"incusId":   instance.IncusID,
				"status":    instance.Status,
				"createdAt": adminBillingISOTime(instance.CreatedAt),
			},
		})
	}

	for _, request := range approvedAdjustments {
		candidates = append(candidates, AdminBillingReconciliationCandidate{
			ItemKey:    "adjustment:" + itoa32(request.ID) + ":missing-balance-log",
			ItemType:   "approved_adjustment_missing_balance_log",
			SourceType: "balance_adjustment_request",
			SourceID:   adminBillingInt32Ptr(request.ID),
			UserID:     adminBillingInt32Ptr(request.UserID),
			Amount:     adminBillingAmountToMoney(request.Amount),
			Title:      "已通过调账审批未找到余额流水：#" + itoa32(request.ID),
			Detail: map[string]any{
				"requestType": request.RequestType,
				"orderNo":     adminBillingStringOrNil(request.OrderNo),
				"sourceType":  adminBillingStringOrNil(request.SourceType),
				"sourceId":    request.SourceID,
				"reviewedAt":  adminBillingISOTimeOrNil(request.ReviewedAt),
				"updatedAt":   adminBillingISOTime(request.UpdatedAt),
			},
		})
	}

	// ---- summary ----
	//
	// recharge.amount is the payable total of the period's completed recharges
	// (actual_amount ?? amount), rounded to cents exactly like
	// `Number(payableRechargeAmount.toFixed(2))`.
	payable := 0.0
	for _, record := range completedRecharges {
		if record.ActualAmount != nil {
			payable += record.ActualAmount.InexactFloat64()
		} else {
			payable += record.Amount.InexactFloat64()
		}
	}

	var rechargeAgg struct {
		Count int64           `gorm:"column:count"`
		Fee   *models.Decimal `gorm:"column:fee"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT COUNT(id)::bigint AS count, SUM(fee)::numeric AS fee
		   FROM recharge_records
		  WHERE status = 'completed'
		    AND ( (completed_at >= ? AND completed_at < ?)
		       OR (completed_at IS NULL AND callback_at >= ? AND callback_at < ?)
		       OR (completed_at IS NULL AND callback_at IS NULL AND created_at >= ? AND created_at < ?) )`,
		start, end, start, end, start, end,
	).Scan(&rechargeAgg).Error; err != nil {
		return nil, nil, err
	}

	var balanceAgg struct {
		Count  int64           `gorm:"column:count"`
		Amount *models.Decimal `gorm:"column:amount"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT COUNT(id)::bigint AS count, SUM(amount)::numeric AS amount
		   FROM balance_logs WHERE created_at >= ? AND created_at < ?`,
		start, end,
	).Scan(&balanceAgg).Error; err != nil {
		return nil, nil, err
	}

	var billingAgg struct {
		Count  int64           `gorm:"column:count"`
		Amount *models.Decimal `gorm:"column:amount"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT COUNT(id)::bigint AS count, SUM(amount)::numeric AS amount
		   FROM instance_billing_records WHERE created_at >= ? AND created_at < ?`,
		start, end,
	).Scan(&billingAgg).Error; err != nil {
		return nil, nil, err
	}

	var adjustmentAgg struct {
		Count  int64           `gorm:"column:count"`
		Amount *models.Decimal `gorm:"column:amount"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT COUNT(id)::bigint AS count, SUM(amount)::numeric AS amount
		   FROM balance_adjustment_requests
		  WHERE status = 'approved'
		    AND ( (reviewed_at >= ? AND reviewed_at < ?)
		       OR (reviewed_at IS NULL AND updated_at >= ? AND updated_at < ?) )`,
		start, end, start, end,
	).Scan(&adjustmentAgg).Error; err != nil {
		return nil, nil, err
	}

	fee := 0.0
	if rechargeAgg.Fee != nil {
		fee = rechargeAgg.Fee.InexactFloat64()
	}
	balanceAmount := 0.0
	if balanceAgg.Amount != nil {
		balanceAmount = balanceAgg.Amount.InexactFloat64()
	}
	billingAmount := 0.0
	if billingAgg.Amount != nil {
		billingAmount = billingAgg.Amount.InexactFloat64()
	}
	adjustmentAmount := 0.0
	if adjustmentAgg.Amount != nil {
		adjustmentAmount = adjustmentAgg.Amount.InexactFloat64()
	}

	byType := map[string]int{}
	for _, candidate := range candidates {
		byType[candidate.ItemType]++
	}

	summary := map[string]any{
		"timezone": "Asia/Shanghai",
		"recharge": map[string]any{
			"count":  rechargeAgg.Count,
			"amount": paymentProviderToFixed(payable, 2),
			"fee":    fee,
		},
		"balanceLogs": map[string]any{
			"count":  balanceAgg.Count,
			"amount": balanceAmount,
		},
		"instanceBilling": map[string]any{
			"count":  billingAgg.Count,
			"amount": billingAmount,
		},
		"approvedAdjustments": map[string]any{
			"count":  adjustmentAgg.Count,
			"amount": adjustmentAmount,
		},
		"discrepancies": map[string]any{
			"total":  len(candidates),
			"byType": byType,
		},
	}
	return candidates, summary, nil
}

// UpdateAdminBillingReconciliationItem mirrors PATCH /reconciliation/items/:id.
//
// Handling a discrepancy back to `discrepancy` clears the handler fields, exactly
// like `handledByUserId: status === 'discrepancy' ? null : actor.id`.
func UpdateAdminBillingReconciliationItem(
	ctx context.Context,
	itemID int32,
	status string,
	note *string,
	actorID int32,
) (AdminBillingReconciliationItem, string, error) {
	result := AdminBillingReconciliationItem{}
	runStatus := ""

	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var handledBy any
		var handledAt any
		if status != "discrepancy" {
			handledBy = actorID
			handledAt = now
		}

		update := tx.Exec(
			`UPDATE financial_reconciliation_items
			    SET status = ?, note = ?, handled_by_user_id = ?, handled_at = ?, updated_at = ?
			  WHERE id = ?`,
			status, note, handledBy, handledAt, now, itemID,
		)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			return ErrAdminBillingReconciliationItemNotFound
		}

		var runID int32
		if err := tx.WithContext(ctx).Raw(
			`SELECT run_id FROM financial_reconciliation_items WHERE id = ?`, itemID,
		).Scan(&runID).Error; err != nil {
			return err
		}

		var statusRows []struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.WithContext(ctx).Raw(
			`SELECT status FROM financial_reconciliation_items WHERE run_id = ?`, runID,
		).Scan(&statusRows).Error; err != nil {
			return err
		}
		statuses := make([]string, 0, len(statusRows))
		for _, row := range statusRows {
			statuses = append(statuses, row.Status)
		}
		runStatus = resolveAdminBillingRunStatus(statuses)

		if err := tx.Exec(
			`UPDATE financial_reconciliation_runs
			    SET status = ?, updated_by_user_id = ?, updated_at = ?
			  WHERE id = ?`,
			runStatus, actorID, now, runID,
		).Error; err != nil {
			return err
		}

		var items []models.FinancialReconciliationItem
		if err := tx.WithContext(ctx).Where("id = ?", itemID).Limit(1).Find(&items).Error; err != nil {
			return err
		}
		if len(items) == 0 {
			return ErrAdminBillingReconciliationItemNotFound
		}
		row := AdminBillingReconciliationItem{Item: items[0]}
		userIDs := []int32{}
		if row.Item.UserID != nil {
			userIDs = append(userIDs, *row.Item.UserID)
		}
		if row.Item.HandledByUserID != nil {
			userIDs = append(userIDs, *row.Item.HandledByUserID)
		}
		users, err := adminBillingLoadUserRefs(ctx, tx, userIDs)
		if err != nil {
			return err
		}
		if row.Item.UserID != nil {
			if user, ok := users[*row.Item.UserID]; ok {
				copied := user
				row.User = &copied
			}
		}
		if row.Item.HandledByUserID != nil {
			if user, ok := users[*row.Item.HandledByUserID]; ok {
				copied := user
				row.HandledBy = &copied
			}
		}
		result = row
		return nil
	})
	if err != nil {
		return AdminBillingReconciliationItem{}, "", err
	}
	return result, runStatus, nil
}

// ErrAdminBillingReconciliationItemNotFound mirrors Prisma P2025 for a missing
// reconciliation item (the route surfaces it as a 500 with the message).
var ErrAdminBillingReconciliationItemNotFound = errors.New("记录不存在")

// ==================== reconciliation CSV exports ====================

// AdminBillingExportOrder is one row of the `orders` CSV.
type AdminBillingExportOrder struct {
	ID            int32
	OrderNo       string
	UserID        int32
	Username      string
	Amount        float64
	ActualAmount  *float64
	Fee           float64
	Status        string
	ProviderName  string
	PaymentMethod *string
	TradeNo       *string
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

// AdminBillingExportBalanceLog is one row of the `balance_logs` CSV.
type AdminBillingExportBalanceLog struct {
	ID            int32
	UserID        int32
	Username      string
	Type          string
	Amount        float64
	BalanceBefore float64
	BalanceAfter  float64
	OrderID       *string
	InstanceID    *int32
	Remark        *string
	CreatedAt     time.Time
}

// AdminBillingExportAdjustment is one row of the `adjustments` CSV.
type AdminBillingExportAdjustment struct {
	ID                  int32
	UserID              int32
	Username            string
	Amount              float64
	RequestType         string
	Status              string
	SourceType          *string
	SourceID            *int32
	OrderNo             *string
	RequestedByUsername string
	ReviewedByUsername  string
	Reason              string
	ReviewRemark        *string
	BalanceLogID        *int32
	CreatedAt           time.Time
	ReviewedAt          *time.Time
}

// GetAdminBillingExportOrders mirrors the `orders` export query.
func GetAdminBillingExportOrders(ctx context.Context, start, end time.Time) ([]AdminBillingExportOrder, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var rows []AdminBillingExportOrder
	if err := DB.WithContext(ctx).Raw(
		`SELECT r.id, r.order_no, r.user_id, COALESCE(u.username, '') AS username,
		        r.amount, r.actual_amount, r.fee, r.status,
		        COALESCE(p.name, '') AS provider_name, r.payment_method, r.trade_no,
		        r.created_at, r.completed_at
		   FROM recharge_records r
		   LEFT JOIN users u ON u.id = r.user_id
		   LEFT JOIN payment_providers p ON p.id = r.provider_id
		  WHERE ( (r.completed_at >= ? AND r.completed_at < ?)
		     OR (r.completed_at IS NULL AND r.callback_at >= ? AND r.callback_at < ?)
		     OR (r.completed_at IS NULL AND r.callback_at IS NULL AND r.created_at >= ? AND r.created_at < ?) )
		  ORDER BY r.created_at ASC`,
		start.UTC(), end.UTC(), start.UTC(), end.UTC(), start.UTC(), end.UTC(),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetAdminBillingExportBalanceLogs mirrors the `balance_logs` export query.
func GetAdminBillingExportBalanceLogs(ctx context.Context, start, end time.Time) ([]AdminBillingExportBalanceLog, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var rows []AdminBillingExportBalanceLog
	if err := DB.WithContext(ctx).Raw(
		`SELECT l.id, l.user_id, COALESCE(u.username, '') AS username, l.type,
		        l.amount, l.balance_before, l.balance_after, l.order_id, l.instance_id,
		        l.remark, l.created_at
		   FROM balance_logs l
		   LEFT JOIN users u ON u.id = l.user_id
		  WHERE l.created_at >= ? AND l.created_at < ?
		  ORDER BY l.created_at ASC`,
		start.UTC(), end.UTC(),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetAdminBillingExportAdjustments mirrors the `adjustments` export query.
func GetAdminBillingExportAdjustments(ctx context.Context, start, end time.Time) ([]AdminBillingExportAdjustment, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var rows []AdminBillingExportAdjustment
	if err := DB.WithContext(ctx).Raw(
		`SELECT a.id, a.user_id, COALESCE(u.username, '') AS username, a.amount,
		        a.request_type, a.status, a.source_type, a.source_id, a.order_no,
		        COALESCE(rb.username, '') AS requested_by_username,
		        COALESCE(vb.username, '') AS reviewed_by_username,
		        a.reason, a.review_remark, a.balance_log_id, a.created_at, a.reviewed_at
		   FROM balance_adjustment_requests a
		   LEFT JOIN users u ON u.id = a.user_id
		   LEFT JOIN users rb ON rb.id = a.requested_by_user_id
		   LEFT JOIN users vb ON vb.id = a.reviewed_by_user_id
		  WHERE ( (a.reviewed_at >= ? AND a.reviewed_at < ?)
		     OR (a.reviewed_at IS NULL AND a.updated_at >= ? AND a.updated_at < ?)
		     OR (a.created_at >= ? AND a.created_at < ?) )
		  ORDER BY a.created_at ASC`,
		start.UTC(), end.UTC(), start.UTC(), end.UTC(), start.UTC(), end.UTC(),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ==================== small shared helpers ====================

// adminBillingAmountToMoney mirrors toDecimalMoney:
//
//	new Prisma.Decimal(Number.isFinite(Number(value)) ? Number(value).toFixed(2) : '0.00')
//
// `paymentProviderToFixed` is this package's exact `Number(x.toFixed(n))` (it was
// written for the payment-provider money path and is unit-tested against the JS
// boundaries 1.005 / 2.675 / 0.1+0.2).
func adminBillingAmountToMoney(value models.Decimal) *models.Decimal {
	money := models.Decimal{Decimal: moneyRound2(value.InexactFloat64())}
	return &money
}

// moneyRound2 rounds a float64 to cents with JS toFixed semantics.
func moneyRound2(value float64) decimal.Decimal {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return decimal.Zero
	}
	return decimal.NewFromFloat(paymentProviderToFixed(value, 2))
}

func adminBillingISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func adminBillingISOTimeOrNil(value *time.Time) any {
	if value == nil {
		return nil
	}
	return adminBillingISOTime(*value)
}

func adminBillingStringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func adminBillingInt32Ptr(value int32) *int32 { return &value }

// itoa32 formats an int32 for an itemKey ("recharge:12:missing-balance-log").
func itoa32(value int32) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [12]byte
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
