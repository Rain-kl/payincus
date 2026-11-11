// Scheduler-owned mail state transitions.
//
// The three mail schedulers (expiry, auto-renew, usage) were written as inline
// `prisma.*` statements, so unlike the route-facing db/mail.go helpers none of
// them existed as db functions. They live in this file to keep db/mail.go free of
// scheduler-only projections.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

var errMailSchedulerDatabaseMissing = errors.New("database not initialized")

// MailExpiredSubscription mirrors the id-only projection of the expiry scan.
type MailExpiredSubscription struct {
	ID        int32
	UserID    int32
	ExpiresAt time.Time
}

// MailListActiveExpiredSubscriptions mirrors the first findMany of
// checkMailSubscriptionExpiry.
func MailListActiveExpiredSubscriptions(ctx context.Context, now time.Time) ([]MailExpiredSubscription, error) {
	if Pool() == nil {
		return nil, errMailSchedulerDatabaseMissing
	}
	rows, err := Pool().Query(ctx,
		`SELECT id, user_id, expires_at FROM mail_subscriptions
		  WHERE status = $1 AND expires_at < $2 ORDER BY id`,
		string(models.MailSubscriptionStatusActive), now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]MailExpiredSubscription, 0, 8)
	for rows.Next() {
		var subscription MailExpiredSubscription
		if err := rows.Scan(&subscription.ID, &subscription.UserID, &subscription.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, subscription)
	}
	return result, rows.Err()
}

// MailMarkSubscriptionsExpired mirrors the guarded updateMany: the ids limit the
// page, and the status/expiry predicates are re-checked so a renewal that landed
// after the SELECT is not expired.
func MailMarkSubscriptionsExpired(ctx context.Context, ids []int32, now time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if Pool() == nil {
		return 0, errMailSchedulerDatabaseMissing
	}
	tag, err := Pool().Exec(ctx,
		`UPDATE mail_subscriptions SET status = $1, updated_at = NOW()
		  WHERE id = ANY($2) AND status = $3 AND expires_at < $4`,
		string(models.MailSubscriptionStatusExpired), ids,
		string(models.MailSubscriptionStatusActive), now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MailExpiredDomain mirrors one domain of an expired subscription.
type MailExpiredDomain struct {
	ID         int32
	Domain     string
	VerifiedAt *time.Time
}

// MailExpiredSubscriptionWithDomains mirrors the second findMany plus the
// `domains: { where: { status: { not: 'suspended' } } }` include.
type MailExpiredSubscriptionWithDomains struct {
	ID       int32
	UserID   int32
	SourceID int32
	Domains  []MailExpiredDomain
}

// MailListExpiredSubscriptionsWithUnsuspendedDomains mirrors the query above.
func MailListExpiredSubscriptionsWithUnsuspendedDomains(ctx context.Context, now time.Time) ([]MailExpiredSubscriptionWithDomains, error) {
	if Pool() == nil {
		return nil, errMailSchedulerDatabaseMissing
	}
	rows, err := Pool().Query(ctx,
		`SELECT s.id, s.user_id, s.source_id, d.id, d.domain, d.verified_at
		   FROM mail_subscriptions s
		   JOIN mail_domains d ON d.subscription_id = s.id
		  WHERE s.status = $1 AND s.expires_at < $2 AND d.status <> $3
		  ORDER BY s.id, d.id`,
		string(models.MailSubscriptionStatusExpired), now.UTC(),
		string(models.MailDomainStatusSuspended))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]MailExpiredSubscriptionWithDomains, 0, 8)
	index := make(map[int32]int)
	for rows.Next() {
		var (
			subscriptionID int32
			userID         int32
			sourceID       int32
			domain         MailExpiredDomain
		)
		if err := rows.Scan(&subscriptionID, &userID, &sourceID, &domain.ID, &domain.Domain, &domain.VerifiedAt); err != nil {
			return nil, err
		}
		position, ok := index[subscriptionID]
		if !ok {
			position = len(result)
			index[subscriptionID] = position
			result = append(result, MailExpiredSubscriptionWithDomains{
				ID: subscriptionID, UserID: userID, SourceID: sourceID,
			})
		}
		result[position].Domains = append(result[position].Domains, domain)
	}
	return result, rows.Err()
}

// MailSubscriptionStillExpired mirrors the per-domain re-check before each upstream
// call: a renewal can land between domains.
func MailSubscriptionStillExpired(ctx context.Context, subscriptionID int32, now time.Time) (bool, error) {
	if Pool() == nil {
		return false, errMailSchedulerDatabaseMissing
	}
	var exists bool
	err := Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM mail_subscriptions WHERE id = $1 AND status = $2 AND expires_at < $3)`,
		subscriptionID, string(models.MailSubscriptionStatusExpired), now.UTC()).Scan(&exists)
	return exists, err
}

// MailSubscriptionActiveNow mirrors the "did a renewal just land?" check.
func MailSubscriptionActiveNow(ctx context.Context, subscriptionID int32, now time.Time) (bool, error) {
	if Pool() == nil {
		return false, errMailSchedulerDatabaseMissing
	}
	var exists bool
	err := Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM mail_subscriptions WHERE id = $1 AND status = $2 AND expires_at > $3)`,
		subscriptionID, string(models.MailSubscriptionStatusActive), now.UTC()).Scan(&exists)
	return exists, err
}

// MailMarkDomainSuspendedIfSubscriptionExpired mirrors the guarded updateMany.
func MailMarkDomainSuspendedIfSubscriptionExpired(ctx context.Context, domainID int32, now time.Time) (bool, error) {
	if Pool() == nil {
		return false, errMailSchedulerDatabaseMissing
	}
	tag, err := Pool().Exec(ctx,
		`UPDATE mail_domains SET status = $1, updated_at = NOW()
		  WHERE id = $2 AND subscription_id IN (
		        SELECT id FROM mail_subscriptions WHERE status = $3 AND expires_at < $4)`,
		string(models.MailDomainStatusSuspended), domainID,
		string(models.MailSubscriptionStatusExpired), now.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MailRestoreDomainAfterResume mirrors the compensation write:
//
//	updateMany({ where: { id, subscription: { status: 'active', expiresAt: { gt: now } } },
//	             data: { status: verifiedAt ? 'verified' : 'pending' } })
func MailRestoreDomainAfterResume(ctx context.Context, domainID int32, verified bool, now time.Time) error {
	return mailSetDomainStatusForActiveSubscription(ctx, domainID, mailDomainRestoredStatus(verified), now)
}

// MailMarkDomainSuspendedForRenewed mirrors the failure branch of the same
// compensation: the upstream resume failed, so the row keeps the persistent
// "retry needed" marker (suspended).
func MailMarkDomainSuspendedForRenewed(ctx context.Context, domainID int32, now time.Time) error {
	return mailSetDomainStatusForActiveSubscription(ctx, domainID, models.MailDomainStatusSuspended, now)
}

func mailSetDomainStatusForActiveSubscription(ctx context.Context, domainID int32, status models.MailDomainStatus, now time.Time) error {
	if Pool() == nil {
		return errMailSchedulerDatabaseMissing
	}
	_, err := Pool().Exec(ctx,
		`UPDATE mail_domains SET status = $1, updated_at = NOW()
		  WHERE id = $2 AND subscription_id IN (
		        SELECT id FROM mail_subscriptions WHERE status = $3 AND expires_at > $4)`,
		string(status), domainID, string(models.MailSubscriptionStatusActive), now.UTC())
	return err
}

func mailDomainRestoredStatus(verified bool) models.MailDomainStatus {
	if verified {
		return models.MailDomainStatusVerified
	}
	return models.MailDomainStatusPending
}

// MailActiveSubscriptionWithSuspendedDomains mirrors the resume scan.
type MailActiveSubscriptionWithSuspendedDomains struct {
	ID       int32
	UserID   int32
	SourceID int32
	Domains  []MailExpiredDomain
}

// MailListActiveSubscriptionsWithSuspendedDomains mirrors:
//
//	findMany({ where: { status: 'active', expiresAt: { gt: now },
//	                    domains: { some: { status: 'suspended' } } },
//	           select: { id, userId, sourceId, domains: { where: { status: 'suspended' } } } })
func MailListActiveSubscriptionsWithSuspendedDomains(ctx context.Context, now time.Time) ([]MailActiveSubscriptionWithSuspendedDomains, error) {
	if Pool() == nil {
		return nil, errMailSchedulerDatabaseMissing
	}
	rows, err := Pool().Query(ctx,
		`SELECT s.id, s.user_id, s.source_id, d.id, d.domain, d.verified_at
		   FROM mail_subscriptions s
		   JOIN mail_domains d ON d.subscription_id = s.id
		  WHERE s.status = $1 AND s.expires_at > $2 AND d.status = $3
		  ORDER BY s.id, d.id`,
		string(models.MailSubscriptionStatusActive), now.UTC(),
		string(models.MailDomainStatusSuspended))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]MailActiveSubscriptionWithSuspendedDomains, 0, 8)
	index := make(map[int32]int)
	for rows.Next() {
		var (
			subscriptionID int32
			userID         int32
			sourceID       int32
			domain         MailExpiredDomain
		)
		if err := rows.Scan(&subscriptionID, &userID, &sourceID, &domain.ID, &domain.Domain, &domain.VerifiedAt); err != nil {
			return nil, err
		}
		position, ok := index[subscriptionID]
		if !ok {
			position = len(result)
			index[subscriptionID] = position
			result = append(result, MailActiveSubscriptionWithSuspendedDomains{
				ID: subscriptionID, UserID: userID, SourceID: sourceID,
			})
		}
		result[position].Domains = append(result[position].Domains, domain)
	}
	return result, rows.Err()
}

// MailMarkDomainResumedIfSubscriptionActive mirrors the guarded resume write:
// only a domain still suspended under an active, unexpired subscription flips.
func MailMarkDomainResumedIfSubscriptionActive(ctx context.Context, domainID int32, verified bool, now time.Time) (bool, error) {
	if Pool() == nil {
		return false, errMailSchedulerDatabaseMissing
	}
	tag, err := Pool().Exec(ctx,
		`UPDATE mail_domains SET status = $1, updated_at = NOW()
		  WHERE id = $2 AND status = $3 AND subscription_id IN (
		        SELECT id FROM mail_subscriptions WHERE status = $4 AND expires_at > $5)`,
		string(mailDomainRestoredStatus(verified)), domainID,
		string(models.MailDomainStatusSuspended),
		string(models.MailSubscriptionStatusActive), now.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ==================== usage sync ====================

// MailDomainSetDiskUsed mirrors
// `prisma.mailDomain.updateMany({ where: { id, subscription: { status: 'active' } },
// data: { diskUsedMb } })`.
func MailDomainSetDiskUsed(ctx context.Context, domainID int32, diskUsedMB int64) error {
	if Pool() == nil {
		return errMailSchedulerDatabaseMissing
	}
	_, err := Pool().Exec(ctx,
		`UPDATE mail_domains SET disk_used_mb = $1, updated_at = NOW()
		  WHERE id = $2 AND subscription_id IN (
		        SELECT id FROM mail_subscriptions WHERE status = $3)`,
		diskUsedMB, domainID, string(models.MailSubscriptionStatusActive))
	return err
}

// MailAccountSetDiskUsed mirrors
// `prisma.mailAccount.updateMany({ where: { id, domain: { subscription: { status:
// 'active' } } }, data: { diskUsedMb } })`.
func MailAccountSetDiskUsed(ctx context.Context, accountID int32, diskUsedMB int32) error {
	if Pool() == nil {
		return errMailSchedulerDatabaseMissing
	}
	_, err := Pool().Exec(ctx,
		`UPDATE mail_accounts SET disk_used_mb = $1, updated_at = NOW()
		  WHERE id = $2 AND domain_id IN (
		        SELECT d.id FROM mail_domains d
		          JOIN mail_subscriptions s ON s.id = d.subscription_id
		         WHERE s.status = $3)`,
		diskUsedMB, accountID, string(models.MailSubscriptionStatusActive))
	return err
}

// MailDomainIDsDueForUsageSync mirrors syncMailUsage's domain query.
func MailDomainIDsDueForUsageSync(ctx context.Context) ([]int32, error) {
	if Pool() == nil {
		return nil, errMailSchedulerDatabaseMissing
	}
	rows, err := Pool().Query(ctx,
		`SELECT d.id FROM mail_domains d
		   JOIN mail_subscriptions s ON s.id = d.subscription_id
		   JOIN mail_sources src ON src.id = d.source_id
		  WHERE s.status = $1 AND src.enabled = true AND d.status <> $2
		  ORDER BY d.id ASC`,
		string(models.MailSubscriptionStatusActive), string(models.MailDomainStatusSuspended))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int32, 0, 16)
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ==================== auto-renew ====================

// MailAutoRenewSubscription is the `include: { plan, affBinding: { include: {
// affCode } } }` projection.
type MailAutoRenewSubscription struct {
	Subscription models.MailSubscription
	Plan         models.MailPlan
	AffBinding   *models.MailSubscriptionAffBinding
	AffCode      *models.AffCode
}

// MailAutoRenewLoad mirrors the findUnique inside processMailAutoRenew.
func MailAutoRenewLoad(ctx context.Context, tx pgx.Tx, subscriptionID int32) (*MailAutoRenewSubscription, error) {
	const subscriptionColumns = `id, user_id, source_id, plan_id, status::text, domain_limit, disk_limit_gb,
		expires_at, auto_renew, created_at, updated_at`
	const planColumns = `id, source_id, name, description, domain_limit, disk_limit_gb,
		billing_cycle::text, price::text AS price, enabled, sort_order, created_at, updated_at`

	var (
		result      MailAutoRenewSubscription
		status      string
		planCycle   string
		priceText   string
		planCreated time.Time
		planUpdated time.Time
		description *string
	)
	err := tx.QueryRow(ctx,
		`SELECT `+subscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, subscriptionID).
		Scan(&result.Subscription.ID, &result.Subscription.UserID, &result.Subscription.SourceID,
			&result.Subscription.PlanID, &status, &result.Subscription.DomainLimit,
			&result.Subscription.DiskLimitGB, &result.Subscription.ExpiresAt,
			&result.Subscription.AutoRenew, &result.Subscription.CreatedAt, &result.Subscription.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.Subscription.Status = models.MailSubscriptionStatus(status)

	err = tx.QueryRow(ctx,
		`SELECT `+planColumns+` FROM mail_plans WHERE id = $1`, result.Subscription.PlanID).
		Scan(&result.Plan.ID, &result.Plan.SourceID, &result.Plan.Name, &description,
			&result.Plan.DomainLimit, &result.Plan.DiskLimitGB, &planCycle, &priceText,
			&result.Plan.Enabled, &result.Plan.SortOrder, &planCreated, &planUpdated)
	if err != nil {
		return nil, err
	}
	result.Plan.Description = description
	result.Plan.BillingCycle = models.MailBillingCycle(planCycle)
	result.Plan.CreatedAt = planCreated
	result.Plan.UpdatedAt = planUpdated
	price, err := decimal.NewFromString(priceText)
	if err != nil {
		return nil, err
	}
	result.Plan.Price = models.Decimal{Decimal: price}

	var (
		bindingID        int32
		bindingSubID     int32
		bindingAffCodeID int32
		bindingCreated   time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT id, mail_subscription_id, aff_code_id, created_at
		   FROM mail_subscription_aff_bindings WHERE mail_subscription_id = $1`, subscriptionID).
		Scan(&bindingID, &bindingSubID, &bindingAffCodeID, &bindingCreated)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		result.AffBinding = &models.MailSubscriptionAffBinding{
			ID: bindingID, MailSubscriptionID: bindingSubID, AffCodeID: bindingAffCodeID, CreatedAt: bindingCreated,
		}
		affCode, affErr := affFindCodeByID(ctx, bindingAffCodeID, tx)
		if affErr != nil {
			return nil, affErr
		}
		result.AffCode = affCode
	}
	return &result, nil
}

// MailAutoRenewChargeExists mirrors the balanceLog dedupe by remark marker.
func MailAutoRenewChargeExists(ctx context.Context, tx pgx.Tx, userID int32, marker string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM balance_logs
		                WHERE user_id = $1 AND type = 'consume' AND remark LIKE '%' || $2 || '%')`,
		userID, marker).Scan(&exists)
	return exists, err
}

// MailAutoRenewDebitBalance mirrors the conditional `user.updateMany`:
// `where: { id, balance: { gte: finalPrice } }`, `data: { balance: { decrement } }`.
func MailAutoRenewDebitBalance(ctx context.Context, tx pgx.Tx, userID int32, amount float64) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET balance = balance - $1 WHERE id = $2 AND balance >= $1`,
		decimal.NewFromFloat(amount).Round(2).String(), userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UserBalanceNumberTx mirrors `Number(user.balance)` inside a transaction.
func UserBalanceNumberTx(ctx context.Context, tx pgx.Tx, userID int32) (float64, error) {
	var value *string
	err := tx.QueryRow(ctx, `SELECT balance::text FROM users WHERE id = $1`, userID).Scan(&value)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, nil
	}
	parsed, parseErr := decimal.NewFromString(*value)
	if parseErr != nil {
		return 0, parseErr
	}
	return parsed.InexactFloat64(), nil
}

// MailAutoRenewCreateBalanceLog mirrors `tx.balanceLog.create`.
func MailAutoRenewCreateBalanceLog(
	ctx context.Context,
	tx pgx.Tx,
	userID int32,
	amount, balanceBefore, balanceAfter float64,
	remark string,
) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, remark, created_at)
		 VALUES ($1, 'consume', $2, $3, $4, $5, NOW())`,
		userID,
		decimal.NewFromFloat(amount).Round(2).String(),
		decimal.NewFromFloat(balanceBefore).Round(2).String(),
		decimal.NewFromFloat(balanceAfter).Round(2).String(),
		remark)
	return err
}

// MailAutoRenewReminderExists mirrors the inboxMessage dedupe keyed by
// `data.data.path['periodKey']`.
func MailAutoRenewReminderExists(ctx context.Context, tx pgx.Tx, userID int32, eventType, periodKey string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM inbox_messages
		                WHERE user_id = $1 AND event_type = $2
		                  AND data #>> '{periodKey}' = $3)`,
		userID, eventType, periodKey).Scan(&exists)
	return exists, err
}

// MailAutoRenewCreateInsufficientReminder mirrors the inbox message + operation log
// written when the balance cannot cover the renewal.
func MailAutoRenewCreateInsufficientReminder(
	ctx context.Context,
	tx pgx.Tx,
	userID int32,
	subscriptionID int32,
	periodKey string,
	renewMonths int,
	renewAmount, balance float64,
	expiresAt time.Time,
	title, content string,
) error {
	payload := map[string]any{
		"subscriptionId": subscriptionID,
		"periodKey":      periodKey,
		"renewMonths":    renewMonths,
		"renewAmount":    renewAmount,
		"expiresAt":      expiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO inbox_messages (user_id, event_type, title, content, data, created_at)
		 VALUES ($1, $2, $3, $4, $5::jsonb, NOW())`,
		userID, "mail_auto_renew_insufficient_balance", title,
		content, string(encoded)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO logs (user_id, module, action, content, result, created_at)
		 VALUES ($1, 'mail', 'mail_auto_renew_insufficient_balance', $2, 'failed', NOW())`,
		userID, content)
	return err
}
