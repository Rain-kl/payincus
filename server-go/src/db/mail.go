// Port of server/src/db/mail.ts (772 lines) — the data layer for the domain-mail
// module: sources, plans, subscriptions, domains and accounts.
//
// Contract notes that are easy to get wrong (each one was checked against the
// running Node backend):
//
//   - Every lookup returns (nil, nil) when the row is absent, never an error.
//   - `apiKey` / `adminPassword` are stored encrypted (AES-GCM, same key
//     derivation as lib/security.ts) and returned decrypted to callers. The
//     routes re-sanitize before sending them to a client. The db package cannot
//     import lib (lib imports db), so the same private helpers that
//     db/system_config.go already carries are reused.
//   - An update whose SET list is empty performs NO write at all. Prisma skips
//     the statement in that case — verified on the wire: `PUT
//     /api/mail/admin/sources/900001` with `{}` returns the row with its
//     original updatedAt. Writing only `updated_at` here would change the
//     response and make a later `updatedAt` comparison diverge.
//   - `getMailPaymentPeriodMs` / the refundable-amount walk are part of this
//     module even though only the admin-cancel route calls them: they are the
//     money math for mailbox refunds.
package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// MaskedMailSourceAPIKeyValue mirrors MASKED_MAIL_SOURCE_API_KEY_VALUE.
const MaskedMailSourceAPIKeyValue = "__SECRET_SET__"

const (
	mailListSearchMaxLength = 128
	mailListMaxPageSize     = 100
)

var (
	mailSubscriptionStatuses = map[string]bool{
		string(models.MailSubscriptionStatusActive):    true,
		string(models.MailSubscriptionStatusExpired):   true,
		string(models.MailSubscriptionStatusSuspended): true,
	}
	mailDomainStatuses = map[string]bool{
		string(models.MailDomainStatusPending):   true,
		string(models.MailDomainStatusVerified):  true,
		string(models.MailDomainStatusSuspended): true,
	}
)

const (
	mailSourceColumns       = `id, name, code, api_url, api_key, smartermail_url, enabled, sort_order, created_at, updated_at`
	mailPlanColumns         = `id, source_id, name, description, domain_limit, disk_limit_gb, billing_cycle, price, enabled, sort_order, created_at, updated_at`
	mailSubscriptionColumns = `id, user_id, source_id, plan_id, status, domain_limit, disk_limit_gb,
		expires_at, auto_renew, created_at, updated_at`
	mailSubscriptionColumnsAliased = `s.id, s.user_id, s.source_id, s.plan_id, s.status, s.domain_limit,
		s.disk_limit_gb, s.expires_at, s.auto_renew, s.created_at, s.updated_at`
	mailDomainColumns = `id, subscription_id, source_id, domain, status, admin_username, admin_password,
		disk_used_mb, verified_at, created_at, updated_at`
	mailAccountColumns = `id, domain_id, email, username, display_name, disk_limit_mb, disk_used_mb,
		is_admin, created_at, updated_at`
	mailBalanceLogColumns = `id, user_id, type, amount, balance_before, balance_after, order_id,
		instance_id, remark, created_at`
)

// mailQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so every query has a
// single implementation whether or not it runs inside a transaction.
type mailQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// mailDB returns the transaction when one is supplied, the shared pool otherwise.
func mailDB(tx pgx.Tx) mailQuerier {
	if tx != nil {
		return tx
	}
	return Pool()
}

// ==================== normalization helpers ====================

func mailClampListPagination(page, pageSize *int) (int, int) {
	normalizedPage := 1
	if page != nil && *page > 0 {
		normalizedPage = *page
	}

	normalizedPageSize := 20
	if pageSize != nil {
		value := *pageSize
		if value < 1 {
			value = 1
		}
		if value > mailListMaxPageSize {
			value = mailListMaxPageSize
		}
		normalizedPageSize = value
	}

	return normalizedPage, normalizedPageSize
}

// mailNormalizeListSearch mirrors normalizeMailListSearch: trim, then cut to 128
// characters (UTF-16 code units in the TypeScript; runes here, which only differs
// on astral-plane input).
func mailNormalizeListSearch(search *string) *string {
	if search == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*search)
	if trimmed == "" {
		return nil
	}
	runes := []rune(trimmed)
	if len(runes) > mailListSearchMaxLength {
		trimmed = string(runes[:mailListSearchMaxLength])
	}
	return &trimmed
}

// MailParseSearchUserID mirrors parseMailSearchUserId: all digits, safe integer,
// greater than zero.
func MailParseSearchUserID(search *string) *int32 {
	if search == nil || *search == "" {
		return nil
	}
	for _, char := range *search {
		if char < '0' || char > '9' {
			return nil
		}
	}
	parsed, err := strconv.ParseInt(*search, 10, 64)
	if err != nil || parsed <= 0 || parsed > 1<<53-1 || parsed > 1<<31-1 {
		return nil
	}
	value := int32(parsed)
	return &value
}

// mailNormalizePositiveSourceID mirrors normalizePositiveMailSourceId.
func mailNormalizePositiveSourceID(sourceID *int32) *int32 {
	if sourceID == nil || *sourceID <= 0 {
		return nil
	}
	return sourceID
}

// mailNormalizeSubscriptionStatus mirrors normalizeMailSubscriptionStatus.
func mailNormalizeSubscriptionStatus(status string) models.MailSubscriptionStatus {
	if mailSubscriptionStatuses[status] {
		return models.MailSubscriptionStatus(status)
	}
	return ""
}

// mailNormalizeDomainStatus mirrors normalizeMailDomainStatus.
func mailNormalizeDomainStatus(status string) models.MailDomainStatus {
	if mailDomainStatuses[status] {
		return models.MailDomainStatus(status)
	}
	return ""
}

// mailDecryptSecret mirrors decryptMailSecret.
func mailDecryptSecret(value string) string {
	if value == "" {
		return value
	}
	if isEncryptedSensitiveData(value) {
		if decrypted := decryptSensitiveData(value); decrypted != "" {
			return decrypted
		}
	}
	return value
}

// mailEncryptSecret mirrors encryptMailSecret.
func mailEncryptSecret(value string) (string, error) {
	if isEncryptedSensitiveData(value) {
		return value, nil
	}
	return encryptSensitiveData(value)
}

func mailDecryptSource(source models.MailSource) models.MailSource {
	source.APIKey = mailDecryptSecret(source.APIKey)
	return source
}

func mailDecryptDomainAdminPassword(domain models.MailDomain) models.MailDomain {
	if domain.AdminPassword != nil && *domain.AdminPassword != "" {
		decrypted := mailDecryptSecret(*domain.AdminPassword)
		domain.AdminPassword = &decrypted
	}
	return domain
}

// ==================== scan helpers ====================

func mailScanSource(row interface{ Scan(...any) error }) (models.MailSource, error) {
	var source models.MailSource
	err := row.Scan(&source.ID, &source.Name, &source.Code, &source.APIURL, &source.APIKey,
		&source.SmarterMailURL, &source.Enabled, &source.SortOrder, &source.CreatedAt, &source.UpdatedAt)
	return source, err
}

func mailScanPlan(row interface{ Scan(...any) error }) (models.MailPlan, error) {
	var plan models.MailPlan
	var billingCycle string
	err := row.Scan(&plan.ID, &plan.SourceID, &plan.Name, &plan.Description, &plan.DomainLimit,
		&plan.DiskLimitGB, &billingCycle, &plan.Price, &plan.Enabled, &plan.SortOrder,
		&plan.CreatedAt, &plan.UpdatedAt)
	plan.BillingCycle = models.MailBillingCycle(billingCycle)
	return plan, err
}

func mailScanSubscription(row interface{ Scan(...any) error }) (models.MailSubscription, error) {
	var subscription models.MailSubscription
	var status string
	err := row.Scan(&subscription.ID, &subscription.UserID, &subscription.SourceID, &subscription.PlanID,
		&status, &subscription.DomainLimit, &subscription.DiskLimitGB, &subscription.ExpiresAt,
		&subscription.AutoRenew, &subscription.CreatedAt, &subscription.UpdatedAt)
	subscription.Status = models.MailSubscriptionStatus(status)
	return subscription, err
}

func mailScanDomain(row interface{ Scan(...any) error }) (models.MailDomain, error) {
	var domain models.MailDomain
	var status string
	err := row.Scan(&domain.ID, &domain.SubscriptionID, &domain.SourceID, &domain.Domain, &status,
		&domain.AdminUsername, &domain.AdminPassword, &domain.DiskUsedMB, &domain.VerifiedAt,
		&domain.CreatedAt, &domain.UpdatedAt)
	domain.Status = models.MailDomainStatus(status)
	return domain, err
}

func mailScanAccount(row interface{ Scan(...any) error }) (models.MailAccount, error) {
	var account models.MailAccount
	err := row.Scan(&account.ID, &account.DomainID, &account.Email, &account.Username,
		&account.DisplayName, &account.DiskLimitMB, &account.DiskUsedMB, &account.IsAdmin,
		&account.CreatedAt, &account.UpdatedAt)
	return account, err
}

// ==================== mail sources ====================

// MailGetAllSources mirrors getAllMailSources.
func MailGetAllSources(ctx context.Context, includeDisabled bool) ([]models.MailSource, error) {
	query := `SELECT ` + mailSourceColumns + ` FROM mail_sources`
	if !includeDisabled {
		query += ` WHERE enabled = true`
	}
	query += ` ORDER BY sort_order ASC, id ASC`

	rows, err := Pool().Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sources := make([]models.MailSource, 0)
	for rows.Next() {
		source, scanErr := mailScanSource(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		sources = append(sources, mailDecryptSource(source))
	}
	return sources, rows.Err()
}

// MailGetSourceByID mirrors getMailSourceById.
func MailGetSourceByID(ctx context.Context, id int32) (*models.MailSource, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailSourceColumns+` FROM mail_sources WHERE id = $1`, id)
	source, err := mailScanSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decrypted := mailDecryptSource(source)
	return &decrypted, nil
}

// MailGetSourceByCode mirrors getMailSourceByCode.
func MailGetSourceByCode(ctx context.Context, code string) (*models.MailSource, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailSourceColumns+` FROM mail_sources WHERE code = $1`, code)
	source, err := mailScanSource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decrypted := mailDecryptSource(source)
	return &decrypted, nil
}

// MailCreateSourceInput mirrors the createMailSource argument.
type MailCreateSourceInput struct {
	Name           string
	Code           string
	APIURL         string
	APIKey         string
	SmarterMailURL string
	Enabled        bool
	SortOrder      int32
}

// MailCreateSource mirrors createMailSource (the API key is stored encrypted).
func MailCreateSource(ctx context.Context, input MailCreateSourceInput) (models.MailSource, error) {
	encryptedKey, err := mailEncryptSecret(input.APIKey)
	if err != nil {
		return models.MailSource{}, err
	}

	row := Pool().QueryRow(ctx,
		`INSERT INTO mail_sources (name, code, api_url, api_key, smartermail_url, enabled, sort_order, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		 RETURNING `+mailSourceColumns,
		input.Name, input.Code, input.APIURL, encryptedKey, input.SmarterMailURL, input.Enabled, input.SortOrder)
	source, err := mailScanSource(row)
	if err != nil {
		return models.MailSource{}, err
	}
	return mailDecryptSource(source), nil
}

// MailUpdateSourceInput mirrors the updateMailSource argument. Nil fields are
// omitted from the SET list (Prisma ignores undefined).
type MailUpdateSourceInput struct {
	Name           *string
	Code           *string
	APIURL         *string
	APIKey         *string
	SmarterMailURL *string
	Enabled        *bool
	SortOrder      *int32
}

// MailUpdateSource mirrors updateMailSource.
func MailUpdateSource(ctx context.Context, id int32, input MailUpdateSourceInput) (models.MailSource, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 8)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if input.Name != nil {
		sets = append(sets, "name = "+arg(*input.Name))
	}
	if input.Code != nil {
		sets = append(sets, "code = "+arg(*input.Code))
	}
	if input.APIURL != nil {
		sets = append(sets, "api_url = "+arg(*input.APIURL))
	}
	if input.APIKey != nil {
		encrypted, err := mailEncryptSecret(*input.APIKey)
		if err != nil {
			return models.MailSource{}, err
		}
		sets = append(sets, "api_key = "+arg(encrypted))
	}
	if input.SmarterMailURL != nil {
		sets = append(sets, "smartermail_url = "+arg(*input.SmarterMailURL))
	}
	if input.Enabled != nil {
		sets = append(sets, "enabled = "+arg(*input.Enabled))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = "+arg(*input.SortOrder))
	}

	if len(sets) == 0 {
		// Prisma runs no statement for an all-undefined update, so @updatedAt is
		// not bumped either. Return the stored row untouched.
		return mailGetSourceByIDRequired(ctx, id)
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	row := Pool().QueryRow(ctx,
		`UPDATE mail_sources SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailSourceColumns,
		args...)
	source, err := mailScanSource(row)
	if err != nil {
		return models.MailSource{}, err
	}
	return mailDecryptSource(source), nil
}

func mailGetSourceByIDRequired(ctx context.Context, id int32) (models.MailSource, error) {
	source, err := MailGetSourceByID(ctx, id)
	if err != nil {
		return models.MailSource{}, err
	}
	if source == nil {
		return models.MailSource{}, pgx.ErrNoRows
	}
	return *source, nil
}

// MailDeleteSource mirrors deleteMailSource.
func MailDeleteSource(ctx context.Context, id int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM mail_sources WHERE id = $1`, id)
	return err
}

// MailSourceStats mirrors getMailSourceStats.
type MailSourceStats struct {
	PlanCount         int64
	SubscriptionCount int64
	DomainCount       int64
}

// MailGetSourceStats mirrors getMailSourceStats.
func MailGetSourceStats(ctx context.Context, sourceID int32) (MailSourceStats, error) {
	var stats MailSourceStats
	err := Pool().QueryRow(ctx,
		`SELECT (SELECT count(*) FROM mail_plans WHERE source_id = $1),
		        (SELECT count(*) FROM mail_subscriptions WHERE source_id = $1),
		        (SELECT count(*) FROM mail_domains WHERE source_id = $1)`,
		sourceID).Scan(&stats.PlanCount, &stats.SubscriptionCount, &stats.DomainCount)
	return stats, err
}

// ==================== mail plans ====================

// MailGetPlansBySource mirrors getMailPlansBySource.
func MailGetPlansBySource(ctx context.Context, sourceID int32, includeDisabled bool) ([]models.MailPlan, error) {
	query := `SELECT ` + mailPlanColumns + ` FROM mail_plans WHERE source_id = $1`
	if !includeDisabled {
		query += ` AND enabled = true`
	}
	query += ` ORDER BY sort_order ASC, price ASC`

	rows, err := Pool().Query(ctx, query, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans := make([]models.MailPlan, 0)
	for rows.Next() {
		plan, scanErr := mailScanPlan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// MailHasAvailableOffering mirrors hasAvailableMailOffering.
func MailHasAvailableOffering(ctx context.Context) (bool, error) {
	var count int64
	err := Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_plans p JOIN mail_sources s ON s.id = p.source_id
		  WHERE p.enabled = true AND s.enabled = true`).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// MailPlanWithSource mirrors `MailPlan & { source: MailSource }`.
type MailPlanWithSource struct {
	Plan   models.MailPlan
	Source models.MailSource
}

// MailGetAllPlans mirrors getAllMailPlans.
func MailGetAllPlans(ctx context.Context) ([]MailPlanWithSource, error) {
	rows, err := Pool().Query(ctx,
		`SELECT `+mailPlanColumns+` FROM mail_plans ORDER BY source_id ASC, sort_order ASC, price ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans := make([]models.MailPlan, 0)
	for rows.Next() {
		plan, scanErr := mailScanPlan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sources, err := MailGetAllSources(ctx, true)
	if err != nil {
		return nil, err
	}
	byID := make(map[int32]models.MailSource, len(sources))
	for _, source := range sources {
		byID[source.ID] = source
	}

	result := make([]MailPlanWithSource, 0, len(plans))
	for _, plan := range plans {
		source, ok := byID[plan.SourceID]
		if !ok {
			continue
		}
		result = append(result, MailPlanWithSource{Plan: plan, Source: source})
	}
	return result, nil
}

// MailGetPlanByID mirrors getMailPlanById.
func MailGetPlanByID(ctx context.Context, id int32) (*MailPlanWithSource, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailPlanColumns+` FROM mail_plans WHERE id = $1`, id)
	plan, err := mailScanPlan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	source, err := MailGetSourceByID(ctx, plan.SourceID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, nil
	}
	return &MailPlanWithSource{Plan: plan, Source: *source}, nil
}

// MailCreatePlanInput mirrors the createMailPlan argument.
type MailCreatePlanInput struct {
	SourceID     int32
	Name         string
	Description  *string
	DomainLimit  int32
	DiskLimitGB  int32
	BillingCycle models.MailBillingCycle
	Price        float64
	Enabled      bool
	SortOrder    int32
}

// MailCreatePlan mirrors createMailPlan.
func MailCreatePlan(ctx context.Context, input MailCreatePlanInput) (models.MailPlan, error) {
	row := Pool().QueryRow(ctx,
		`INSERT INTO mail_plans (source_id, name, description, domain_limit, disk_limit_gb, billing_cycle,
		                         price, enabled, sort_order, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
		 RETURNING `+mailPlanColumns,
		input.SourceID, input.Name, input.Description, input.DomainLimit, input.DiskLimitGB,
		string(input.BillingCycle), decimalFromFloat(input.Price), input.Enabled, input.SortOrder)
	return mailScanPlan(row)
}

// MailUpdatePlanInput mirrors the updateMailPlan argument.
type MailUpdatePlanInput struct {
	SourceID     *int32
	Name         *string
	Description  *string
	DomainLimit  *int32
	DiskLimitGB  *int32
	BillingCycle *models.MailBillingCycle
	Price        *float64
	Enabled      *bool
	SortOrder    *int32
}

// MailUpdatePlan mirrors updateMailPlan.
func MailUpdatePlan(ctx context.Context, id int32, input MailUpdatePlanInput) (models.MailPlan, error) {
	sets := make([]string, 0, 9)
	args := make([]any, 0, 9)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if input.SourceID != nil {
		sets = append(sets, "source_id = "+arg(*input.SourceID))
	}
	if input.Name != nil {
		sets = append(sets, "name = "+arg(*input.Name))
	}
	if input.Description != nil {
		sets = append(sets, "description = "+arg(*input.Description))
	}
	if input.DomainLimit != nil {
		sets = append(sets, "domain_limit = "+arg(*input.DomainLimit))
	}
	if input.DiskLimitGB != nil {
		sets = append(sets, "disk_limit_gb = "+arg(*input.DiskLimitGB))
	}
	if input.BillingCycle != nil {
		sets = append(sets, "billing_cycle = "+arg(string(*input.BillingCycle)))
	}
	if input.Price != nil {
		sets = append(sets, "price = "+arg(decimalFromFloat(*input.Price)))
	}
	if input.Enabled != nil {
		sets = append(sets, "enabled = "+arg(*input.Enabled))
	}
	if input.SortOrder != nil {
		sets = append(sets, "sort_order = "+arg(*input.SortOrder))
	}

	if len(sets) == 0 {
		return mailGetPlanByIDRequired(ctx, id)
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	row := Pool().QueryRow(ctx,
		`UPDATE mail_plans SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailPlanColumns,
		args...)
	return mailScanPlan(row)
}

func mailGetPlanByIDRequired(ctx context.Context, id int32) (models.MailPlan, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailPlanColumns+` FROM mail_plans WHERE id = $1`, id)
	plan, err := mailScanPlan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.MailPlan{}, nil
	}
	return plan, err
}

// MailDeletePlan mirrors deleteMailPlan.
func MailDeletePlan(ctx context.Context, id int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM mail_plans WHERE id = $1`, id)
	return err
}

// MailCountSubscriptionsByPlan mirrors the inline prisma.mailSubscription.count in
// the delete-plan route.
func MailCountSubscriptionsByPlan(ctx context.Context, planID int32) (int64, error) {
	var count int64
	err := Pool().QueryRow(ctx, `SELECT count(*) FROM mail_subscriptions WHERE plan_id = $1`, planID).Scan(&count)
	return count, err
}

// ==================== users (small projections) ====================

// MailUserBrief mirrors `{ id, username }`.
type MailUserBrief struct {
	ID       int32
	Username string
}

// MailUserSelect mirrors the five-field user select used by the admin lists.
type MailUserSelect struct {
	ID            int32
	Username      string
	Email         *string
	AvatarStyle   *string
	AvatarBadgeID *string
}

// ==================== mail subscriptions ====================

// MailDomainWithAccounts mirrors `MailDomain & { accounts: MailAccount[] }`.
type MailDomainWithAccounts struct {
	Domain   models.MailDomain
	Accounts []models.MailAccount
}

// MailSubscriptionDetail mirrors the included subscription shapes.
type MailSubscriptionDetail struct {
	Subscription models.MailSubscription
	Source       models.MailSource
	Plan         models.MailPlan
	Domains      []MailDomainWithAccounts
	User         *MailUserSelect
}

func mailLoadDomainsWithAccounts(ctx context.Context, subscriptionID int32, orderByCreatedAtDesc bool) ([]MailDomainWithAccounts, error) {
	order := ` ORDER BY id ASC`
	if orderByCreatedAtDesc {
		order = ` ORDER BY created_at DESC`
	}
	rows, err := Pool().Query(ctx,
		`SELECT `+mailDomainColumns+` FROM mail_domains WHERE subscription_id = $1`+order, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	domains := make([]models.MailDomain, 0)
	for rows.Next() {
		domain, scanErr := mailScanDomain(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		domains = append(domains, mailDecryptDomainAdminPassword(domain))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]MailDomainWithAccounts, 0, len(domains))
	for _, domain := range domains {
		accounts, err := mailLoadAccountsOrdered(ctx, domain.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, MailDomainWithAccounts{Domain: domain, Accounts: accounts})
	}
	return result, nil
}

func mailLoadAccountsOrdered(ctx context.Context, domainID int32) ([]models.MailAccount, error) {
	rows, err := Pool().Query(ctx,
		`SELECT `+mailAccountColumns+` FROM mail_accounts WHERE domain_id = $1 ORDER BY is_admin DESC, created_at ASC, id ASC`,
		domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := make([]models.MailAccount, 0)
	for rows.Next() {
		account, scanErr := mailScanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// mailLoadSubscriptionBasics loads the source and plan of a subscription.
func mailLoadSubscriptionBasics(ctx context.Context, subscription models.MailSubscription) (models.MailSource, models.MailPlan, error) {
	source, err := MailGetSourceByID(ctx, subscription.SourceID)
	if err != nil {
		return models.MailSource{}, models.MailPlan{}, err
	}
	if source == nil {
		return models.MailSource{}, models.MailPlan{}, pgx.ErrNoRows
	}
	row := Pool().QueryRow(ctx, `SELECT `+mailPlanColumns+` FROM mail_plans WHERE id = $1`, subscription.PlanID)
	plan, err := mailScanPlan(row)
	if err != nil {
		return models.MailSource{}, models.MailPlan{}, err
	}
	return *source, plan, nil
}

// MailGetUserSubscription mirrors getUserMailSubscription: the newest active
// subscription, falling back to the newest expired one.
func MailGetUserSubscription(ctx context.Context, userID int32, sourceID *int32) (*MailSubscriptionDetail, error) {
	var subscription *models.MailSubscription
	for _, status := range []models.MailSubscriptionStatus{
		models.MailSubscriptionStatusActive,
		models.MailSubscriptionStatusExpired,
	} {
		query := `SELECT ` + mailSubscriptionColumns + ` FROM mail_subscriptions
		           WHERE user_id = $1 AND status = $2`
		args := []any{userID, string(status)}
		if sourceID != nil {
			query += ` AND source_id = $3`
			args = append(args, *sourceID)
		}
		query += ` ORDER BY updated_at DESC LIMIT 1`

		row := Pool().QueryRow(ctx, query, args...)
		candidate, err := mailScanSubscription(row)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		subscription = &candidate
		break
	}
	if subscription == nil {
		return nil, nil
	}

	source, plan, err := mailLoadSubscriptionBasics(ctx, *subscription)
	if err != nil {
		return nil, err
	}
	domains, err := mailLoadDomainsWithAccounts(ctx, subscription.ID, false)
	if err != nil {
		return nil, err
	}

	return &MailSubscriptionDetail{
		Subscription: *subscription,
		Source:       source,
		Plan:         plan,
		Domains:      domains,
	}, nil
}

// MailGetSubscriptionByID mirrors getMailSubscriptionById.
func MailGetSubscriptionByID(ctx context.Context, id int32) (*MailSubscriptionDetail, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, id)
	subscription, err := mailScanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	source, plan, err := mailLoadSubscriptionBasics(ctx, subscription)
	if err != nil {
		return nil, err
	}
	domains, err := mailLoadDomainsWithAccounts(ctx, subscription.ID, false)
	if err != nil {
		return nil, err
	}

	return &MailSubscriptionDetail{
		Subscription: subscription,
		Source:       source,
		Plan:         plan,
		Domains:      domains,
	}, nil
}

// MailSubscriptionListOptions mirrors the getAllMailSubscriptions options.
type MailSubscriptionListOptions struct {
	SourceID *int32
	Status   string
	Search   *string
	Page     int
	PageSize int
}

// MailSubscriptionListResult mirrors the getAllMailSubscriptions result.
type MailSubscriptionListResult struct {
	Subscriptions []MailSubscriptionDetail
	Total         int64
}

// mailSubscriptionListWhere builds the shared WHERE fragment.
func mailSubscriptionListWhere(opts MailSubscriptionListOptions) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if sourceID := mailNormalizePositiveSourceID(opts.SourceID); sourceID != nil {
		clauses = append(clauses, "s.source_id = "+arg(*sourceID))
	}
	if status := mailNormalizeSubscriptionStatus(opts.Status); status != "" {
		clauses = append(clauses, "s.status = "+arg(string(status)))
	}
	if search := mailNormalizeListSearch(opts.Search); search != nil {
		pattern := "%" + *search + "%"
		placeholder := arg(pattern)
		parts := []string{"u.username ILIKE " + placeholder, "u.email ILIKE " + placeholder}
		if searchUserID := MailParseSearchUserID(search); searchUserID != nil {
			parts = append(parts, "s.user_id = "+arg(*searchUserID))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// MailGetAllSubscriptions mirrors getAllMailSubscriptions.
func MailGetAllSubscriptions(ctx context.Context, opts MailSubscriptionListOptions) (MailSubscriptionListResult, error) {
	page, pageSize := mailClampListPagination(&opts.Page, &opts.PageSize)
	where, whereArgs := mailSubscriptionListWhere(opts)

	var total int64
	if err := Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_subscriptions s JOIN users u ON u.id = s.user_id`+where,
		whereArgs...).Scan(&total); err != nil {
		return MailSubscriptionListResult{}, err
	}

	args := append([]any(nil), whereArgs...)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, pageSize)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, (page-1)*pageSize)

	rows, err := Pool().Query(ctx,
		`SELECT `+mailSubscriptionColumnsAliased+` FROM mail_subscriptions s
		 JOIN users u ON u.id = s.user_id`+where+
			` ORDER BY s.created_at DESC LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder,
		args...)
	if err != nil {
		return MailSubscriptionListResult{}, err
	}
	defer rows.Close()

	subscriptions := make([]models.MailSubscription, 0)
	for rows.Next() {
		subscription, scanErr := mailScanSubscription(rows)
		if scanErr != nil {
			return MailSubscriptionListResult{}, scanErr
		}
		subscriptions = append(subscriptions, subscription)
	}
	if err := rows.Err(); err != nil {
		return MailSubscriptionListResult{}, err
	}

	details, err := mailAttachSubscriptionRelations(ctx, subscriptions, true)
	if err != nil {
		return MailSubscriptionListResult{}, err
	}
	return MailSubscriptionListResult{Subscriptions: details, Total: total}, nil
}

// mailAttachSubscriptionRelations loads user/source/plan in bulk.
func mailAttachSubscriptionRelations(
	ctx context.Context,
	subscriptions []models.MailSubscription,
	includeUser bool,
) ([]MailSubscriptionDetail, error) {
	userIDs := make([]int32, 0, len(subscriptions))
	sourceIDs := make([]int32, 0, len(subscriptions))
	planIDs := make([]int32, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		userIDs = append(userIDs, subscription.UserID)
		sourceIDs = append(sourceIDs, subscription.SourceID)
		planIDs = append(planIDs, subscription.PlanID)
	}

	users := map[int32]MailUserSelect{}
	if includeUser && len(userIDs) > 0 {
		rows, err := Pool().Query(ctx,
			`SELECT id, username, email, avatar_style, avatar_badge_id FROM users WHERE id = ANY($1)`, userIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var user MailUserSelect
			if scanErr := rows.Scan(&user.ID, &user.Username, &user.Email, &user.AvatarStyle, &user.AvatarBadgeID); scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			users[user.ID] = user
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	sources := map[int32]models.MailSource{}
	if len(sourceIDs) > 0 {
		rows, err := Pool().Query(ctx,
			`SELECT `+mailSourceColumns+` FROM mail_sources WHERE id = ANY($1)`, sourceIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			source, scanErr := mailScanSource(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			sources[source.ID] = mailDecryptSource(source)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	plans := map[int32]models.MailPlan{}
	if len(planIDs) > 0 {
		rows, err := Pool().Query(ctx,
			`SELECT `+mailPlanColumns+` FROM mail_plans WHERE id = ANY($1)`, planIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			plan, scanErr := mailScanPlan(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			plans[plan.ID] = plan
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	details := make([]MailSubscriptionDetail, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		detail := MailSubscriptionDetail{Subscription: subscription}
		detail.Source = sources[subscription.SourceID]
		detail.Plan = plans[subscription.PlanID]
		if includeUser {
			user := users[subscription.UserID]
			detail.User = &user
		}
		details = append(details, detail)
	}
	return details, nil
}

// MailCreateSubscriptionInput mirrors the createMailSubscription argument.
type MailCreateSubscriptionInput struct {
	UserID      int32
	SourceID    int32
	PlanID      int32
	DomainLimit int32
	DiskLimitGB int32
	ExpiresAt   time.Time
	AutoRenew   bool
}

// MailCreateSubscription mirrors createMailSubscription.
func MailCreateSubscription(ctx context.Context, input MailCreateSubscriptionInput) (models.MailSubscription, error) {
	return mailCreateSubscription(ctx, nil, input)
}

func mailCreateSubscription(ctx context.Context, tx pgx.Tx, input MailCreateSubscriptionInput) (models.MailSubscription, error) {
	row := mailDB(tx).QueryRow(ctx,
		`INSERT INTO mail_subscriptions (user_id, source_id, plan_id, status, domain_limit, disk_limit_gb,
		                                 expires_at, auto_renew, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
		 RETURNING `+mailSubscriptionColumns,
		input.UserID, input.SourceID, input.PlanID, string(models.MailSubscriptionStatusActive),
		input.DomainLimit, input.DiskLimitGB, input.ExpiresAt.UTC(), input.AutoRenew)
	return mailScanSubscription(row)
}

// MailUpdateSubscriptionInput mirrors the updateMailSubscription argument.
type MailUpdateSubscriptionInput struct {
	Status    *models.MailSubscriptionStatus
	ExpiresAt *time.Time
	AutoRenew *bool
}

// MailUpdateSubscription mirrors updateMailSubscription.
func MailUpdateSubscription(ctx context.Context, id int32, input MailUpdateSubscriptionInput) (models.MailSubscription, error) {
	sets := make([]string, 0, 4)
	args := make([]any, 0, 4)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if input.Status != nil {
		sets = append(sets, "status = "+arg(string(*input.Status)))
	}
	if input.ExpiresAt != nil {
		sets = append(sets, "expires_at = "+arg(input.ExpiresAt.UTC()))
	}
	if input.AutoRenew != nil {
		sets = append(sets, "auto_renew = "+arg(*input.AutoRenew))
	}
	if len(sets) == 0 {
		row := Pool().QueryRow(ctx, `SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, id)
		return mailScanSubscription(row)
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	row := Pool().QueryRow(ctx,
		`UPDATE mail_subscriptions SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailSubscriptionColumns,
		args...)
	return mailScanSubscription(row)
}

// MailRenewSubscription mirrors renewMailSubscription(id, months, client).
func MailRenewSubscription(ctx context.Context, id int32, months int, tx pgx.Tx) (models.MailSubscription, error) {
	if months < 1 || months > 12 {
		return models.MailSubscription{}, errors.New("Invalid mail renewal months")
	}

	querier := mailDB(tx)
	row := querier.QueryRow(ctx, `SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, id)
	subscription, err := mailScanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.MailSubscription{}, errors.New("Subscription not found")
	}
	if err != nil {
		return models.MailSubscription{}, err
	}

	now := time.Now()
	periodStart := now
	if subscription.ExpiresAt.After(now) {
		periodStart = subscription.ExpiresAt
	}
	newExpiry := MailAddCalendarMonths(periodStart, months)

	row = querier.QueryRow(ctx,
		`UPDATE mail_subscriptions SET expires_at = $1, status = $2, updated_at = NOW()
		  WHERE id = $3 RETURNING `+mailSubscriptionColumns,
		newExpiry.UTC(), string(models.MailSubscriptionStatusActive), id)
	return mailScanSubscription(row)
}

// ==================== date arithmetic ====================

// MailAddCalendarMonths mirrors `date.setMonth(date.getMonth() + months)` in the
// process-local timezone: the wall clock is preserved and day overflow rolls into
// the next month exactly like the JavaScript Date normalisation (Jan 31 + 1 month
// = Mar 3). It is NOT billing_calc.AddMonths (31-day arithmetic).
func MailAddCalendarMonths(date time.Time, months int) time.Time {
	local := date.In(time.Local)
	year, month, day := local.Date()
	return time.Date(year, month+time.Month(months), day,
		local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.Local)
}

// MailAddCalendarYears mirrors `date.setFullYear(date.getFullYear() + years)`.
func MailAddCalendarYears(date time.Time, years int) time.Time {
	local := date.In(time.Local)
	return time.Date(local.Year()+years, local.Month(), local.Day(),
		local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.Local)
}

// MailAddCalendarDays mirrors `date.setDate(date.getDate() + days)`.
func MailAddCalendarDays(date time.Time, days int) time.Time {
	local := date.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day()+days,
		local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.Local)
}

// ==================== refund inputs ====================

// MailPaymentRecord is one `balance_logs` row that paid for a subscription.
type MailPaymentRecord struct {
	Amount models.Decimal
	Remark *string
}

// MailRefundInputs carries the ledger rows getMailRefundableAmount() walks. The
// arithmetic itself lives in routes/mail.go, exactly like the TypeScript (the
// helper is defined in routes/mail.ts, not db/mail.ts).
type MailRefundInputs struct {
	// PurchaseFound is false when no purchase BalanceLog predates the
	// subscription; both refund ceilings are then zero.
	PurchaseFound bool
	Payments      []MailPaymentRecord
	Refunds       []models.Decimal
}

// MailLoadRefundInputs mirrors the three findMany statements of
// getMailRefundableAmount.
func MailLoadRefundInputs(ctx context.Context, tx pgx.Tx, userID int32, createdAt time.Time) (MailRefundInputs, error) {
	const purchasePrefix = "购买域名邮箱 - "
	const renewPrefix = "续费域名邮箱 "
	const refundPrefix = "管理员退订邮箱 - "

	var purchaseRecordID int32
	err := tx.QueryRow(ctx,
		`SELECT id FROM balance_logs
		  WHERE user_id = $1 AND type = 'consume' AND remark LIKE $2 AND created_at <= $3
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		userID, purchasePrefix+"%", createdAt).Scan(&purchaseRecordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MailRefundInputs{PurchaseFound: false, Payments: []MailPaymentRecord{}, Refunds: []models.Decimal{}}, nil
	}
	if err != nil {
		return MailRefundInputs{}, err
	}

	inputs := MailRefundInputs{
		PurchaseFound: true,
		Payments:      make([]MailPaymentRecord, 0),
		Refunds:       make([]models.Decimal, 0),
	}

	rows, err := tx.Query(ctx,
		`SELECT amount, remark FROM balance_logs
		  WHERE user_id = $1 AND id >= $2 AND type = 'consume'
		    AND (remark LIKE $3 OR remark LIKE $4)
		  ORDER BY id ASC`,
		userID, purchaseRecordID, purchasePrefix+"%", renewPrefix+"%")
	if err != nil {
		return MailRefundInputs{}, err
	}
	for rows.Next() {
		var record MailPaymentRecord
		if scanErr := rows.Scan(&record.Amount, &record.Remark); scanErr != nil {
			rows.Close()
			return MailRefundInputs{}, scanErr
		}
		inputs.Payments = append(inputs.Payments, record)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MailRefundInputs{}, err
	}

	rows, err = tx.Query(ctx,
		`SELECT amount FROM balance_logs
		  WHERE user_id = $1 AND id >= $2 AND type = 'refund' AND remark LIKE $3`,
		userID, purchaseRecordID, refundPrefix+"%")
	if err != nil {
		return MailRefundInputs{}, err
	}
	for rows.Next() {
		var amount models.Decimal
		if scanErr := rows.Scan(&amount); scanErr != nil {
			rows.Close()
			return MailRefundInputs{}, scanErr
		}
		inputs.Refunds = append(inputs.Refunds, amount)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MailRefundInputs{}, err
	}

	return inputs, nil
}

// MailSubscriptionAdminDetail mirrors the admin-cancel include: user (id,
// username, balance), plan and domains.
type MailSubscriptionAdminDetail struct {
	Subscription models.MailSubscription
	UserID       int32
	Username     string
	UserBalance  models.Decimal
	Plan         models.MailPlan
	Domains      []models.MailDomain
}

// MailGetSubscriptionAdminDetail mirrors the include block of the admin-cancel route.
func MailGetSubscriptionAdminDetail(ctx context.Context, id int32) (*MailSubscriptionAdminDetail, error) {
	var detail MailSubscriptionAdminDetail
	row := Pool().QueryRow(ctx,
		`SELECT `+mailSubscriptionColumnsAliased+`, u.id, u.username, u.balance
		   FROM mail_subscriptions s JOIN users u ON u.id = s.user_id
		  WHERE s.id = $1`, id)

	var subscription models.MailSubscription
	var status string
	err := row.Scan(&subscription.ID, &subscription.UserID, &subscription.SourceID, &subscription.PlanID,
		&status, &subscription.DomainLimit, &subscription.DiskLimitGB, &subscription.ExpiresAt,
		&subscription.AutoRenew, &subscription.CreatedAt, &subscription.UpdatedAt,
		&detail.UserID, &detail.Username, &detail.UserBalance)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	subscription.Status = models.MailSubscriptionStatus(status)
	detail.Subscription = subscription

	planRow := Pool().QueryRow(ctx, `SELECT `+mailPlanColumns+` FROM mail_plans WHERE id = $1`, subscription.PlanID)
	plan, err := mailScanPlan(planRow)
	if err != nil {
		return nil, err
	}
	detail.Plan = plan

	rows, err := Pool().Query(ctx,
		`SELECT `+mailDomainColumns+` FROM mail_domains WHERE subscription_id = $1 ORDER BY id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	domains := make([]models.MailDomain, 0)
	for rows.Next() {
		domain, scanErr := mailScanDomain(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		domains = append(domains, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	detail.Domains = domains
	return &detail, nil
}

// ==================== mail domains ====================

// MailGetDomainsBySubscription mirrors getMailDomainsBySubscription.
func MailGetDomainsBySubscription(ctx context.Context, subscriptionID int32) ([]MailDomainWithAccounts, error) {
	return mailLoadDomainsWithAccounts(ctx, subscriptionID, true)
}

// MailDomainDetail mirrors getMailDomainById's include shape.
type MailDomainDetail struct {
	Domain       models.MailDomain
	Subscription models.MailSubscription
	User         MailUserBrief
	Source       models.MailSource
	Accounts     []models.MailAccount
}

// MailGetDomainByID mirrors getMailDomainById.
func MailGetDomainByID(ctx context.Context, id int32) (*MailDomainDetail, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE id = $1`, id)
	domain, err := mailScanDomain(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	domain = mailDecryptDomainAdminPassword(domain)

	subscriptionRow := Pool().QueryRow(ctx,
		`SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, domain.SubscriptionID)
	subscription, err := mailScanSubscription(subscriptionRow)
	if err != nil {
		return nil, err
	}

	var user MailUserBrief
	err = Pool().QueryRow(ctx, `SELECT id, username FROM users WHERE id = $1`, subscription.UserID).
		Scan(&user.ID, &user.Username)
	if err != nil {
		return nil, err
	}

	source, err := MailGetSourceByID(ctx, domain.SourceID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, pgx.ErrNoRows
	}

	accounts, err := mailLoadAccountsOrdered(ctx, domain.ID)
	if err != nil {
		return nil, err
	}

	return &MailDomainDetail{
		Domain:       domain,
		Subscription: subscription,
		User:         user,
		Source:       *source,
		Accounts:     accounts,
	}, nil
}

// MailCheckDomainExists mirrors checkMailDomainExists.
func MailCheckDomainExists(ctx context.Context, domain string, sourceID int32) (bool, error) {
	var count int64
	err := Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_domains WHERE domain = $1 AND source_id = $2`, domain, sourceID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// MailCreateDomainInput mirrors the createMailDomain argument.
type MailCreateDomainInput struct {
	SubscriptionID int32
	SourceID       int32
	Domain         string
	AdminUsername  *string
	AdminPassword  *string
}

// mailPrepareAdminPassword mirrors `adminPassword ? encrypt(adminPassword) : adminPassword`:
// an empty string is stored verbatim, a non-empty one is encrypted.
func mailPrepareAdminPassword(password *string) (*string, error) {
	if password == nil || *password == "" {
		return password, nil
	}
	encrypted, err := mailEncryptSecret(*password)
	if err != nil {
		return nil, err
	}
	return &encrypted, nil
}

// MailCreateDomain mirrors createMailDomain.
func MailCreateDomain(ctx context.Context, input MailCreateDomainInput) (models.MailDomain, error) {
	return mailCreateDomain(ctx, nil, input)
}

// MailCreateDomainWithTx mirrors createMailDomainWithTx.
func MailCreateDomainWithTx(ctx context.Context, tx pgx.Tx, input MailCreateDomainInput) (models.MailDomain, error) {
	return mailCreateDomain(ctx, tx, input)
}

func mailCreateDomain(ctx context.Context, tx pgx.Tx, input MailCreateDomainInput) (models.MailDomain, error) {
	adminPassword, err := mailPrepareAdminPassword(input.AdminPassword)
	if err != nil {
		return models.MailDomain{}, err
	}

	row := mailDB(tx).QueryRow(ctx,
		`INSERT INTO mail_domains (subscription_id, source_id, domain, status, admin_username, admin_password,
		                           disk_used_mb, verified_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 0, NULL, NOW(), NOW())
		 RETURNING `+mailDomainColumns,
		input.SubscriptionID, input.SourceID, input.Domain, string(models.MailDomainStatusPending),
		input.AdminUsername, adminPassword)
	domain, err := mailScanDomain(row)
	if err != nil {
		return models.MailDomain{}, err
	}
	return mailDecryptDomainAdminPassword(domain), nil
}

// MailUpdateDomainInput mirrors the updateMailDomain argument.
type MailUpdateDomainInput struct {
	Status        *models.MailDomainStatus
	AdminUsername *string
	AdminPassword *string
	DiskUsedMB    *int32
	VerifiedAt    *time.Time
}

// MailUpdateDomain mirrors updateMailDomain.
func MailUpdateDomain(ctx context.Context, id int32, input MailUpdateDomainInput) (models.MailDomain, error) {
	sets := make([]string, 0, 6)
	args := make([]any, 0, 6)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if input.Status != nil {
		sets = append(sets, "status = "+arg(string(*input.Status)))
	}
	if input.AdminUsername != nil {
		sets = append(sets, "admin_username = "+arg(*input.AdminUsername))
	}
	if input.AdminPassword != nil {
		password, err := mailPrepareAdminPassword(input.AdminPassword)
		if err != nil {
			return models.MailDomain{}, err
		}
		sets = append(sets, "admin_password = "+arg(password))
	}
	if input.DiskUsedMB != nil {
		sets = append(sets, "disk_used_mb = "+arg(*input.DiskUsedMB))
	}
	if input.VerifiedAt != nil {
		sets = append(sets, "verified_at = "+arg(input.VerifiedAt.UTC()))
	}

	if len(sets) == 0 {
		row := Pool().QueryRow(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE id = $1`, id)
		domain, err := mailScanDomain(row)
		if err != nil {
			return models.MailDomain{}, err
		}
		return mailDecryptDomainAdminPassword(domain), nil
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	row := Pool().QueryRow(ctx,
		`UPDATE mail_domains SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailDomainColumns,
		args...)
	domain, err := mailScanDomain(row)
	if err != nil {
		return models.MailDomain{}, err
	}
	return mailDecryptDomainAdminPassword(domain), nil
}

// MailDeleteDomain mirrors deleteMailDomain.
func MailDeleteDomain(ctx context.Context, id int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM mail_domains WHERE id = $1`, id)
	return err
}

// MailDomainListOptions mirrors the getAllMailDomains options.
type MailDomainListOptions struct {
	SourceID *int32
	Status   string
	Search   *string
	Page     int
	PageSize int
}

// MailDomainWithRelations mirrors the getAllMailDomains include shape.
type MailDomainWithRelations struct {
	Domain       models.MailDomain
	Subscription models.MailSubscription
	User         MailUserSelect
	Source       models.MailSource
	AccountCount int64
}

// MailDomainListResult mirrors the getAllMailDomains result.
type MailDomainListResult struct {
	Domains []MailDomainWithRelations
	Total   int64
}

func mailDomainListWhere(opts MailDomainListOptions) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if sourceID := mailNormalizePositiveSourceID(opts.SourceID); sourceID != nil {
		clauses = append(clauses, "d.source_id = "+arg(*sourceID))
	}
	if status := mailNormalizeDomainStatus(opts.Status); status != "" {
		clauses = append(clauses, "d.status = "+arg(string(status)))
	}
	if search := mailNormalizeListSearch(opts.Search); search != nil {
		pattern := "%" + *search + "%"
		placeholder := arg(pattern)
		parts := []string{
			"d.domain ILIKE " + placeholder,
			"u.username ILIKE " + placeholder,
			"u.email ILIKE " + placeholder,
		}
		if searchUserID := MailParseSearchUserID(search); searchUserID != nil {
			parts = append(parts, "s.user_id = "+arg(*searchUserID))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// MailGetAllDomains mirrors getAllMailDomains.
func MailGetAllDomains(ctx context.Context, opts MailDomainListOptions) (MailDomainListResult, error) {
	page, pageSize := mailClampListPagination(&opts.Page, &opts.PageSize)
	where, whereArgs := mailDomainListWhere(opts)

	var total int64
	if err := Pool().QueryRow(ctx,
		`SELECT count(*) FROM mail_domains d
		   JOIN mail_subscriptions s ON s.id = d.subscription_id
		   JOIN users u ON u.id = s.user_id`+where, whereArgs...).Scan(&total); err != nil {
		return MailDomainListResult{}, err
	}

	args := append([]any(nil), whereArgs...)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, pageSize)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, (page-1)*pageSize)

	rows, err := Pool().Query(ctx,
		`SELECT d.id, d.subscription_id, d.source_id, d.domain, d.status, d.admin_username,
		        d.admin_password, d.disk_used_mb, d.verified_at, d.created_at, d.updated_at,
		        (SELECT count(*) FROM mail_accounts a WHERE a.domain_id = d.id) AS account_count
		   FROM mail_domains d
		   JOIN mail_subscriptions s ON s.id = d.subscription_id
		   JOIN users u ON u.id = s.user_id`+where+
			` ORDER BY d.created_at DESC LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder,
		args...)
	if err != nil {
		return MailDomainListResult{}, err
	}
	defer rows.Close()

	domains := make([]models.MailDomain, 0)
	counts := make([]int64, 0)
	for rows.Next() {
		var domain models.MailDomain
		var status string
		var accountCount int64
		if scanErr := rows.Scan(&domain.ID, &domain.SubscriptionID, &domain.SourceID, &domain.Domain,
			&status, &domain.AdminUsername, &domain.AdminPassword, &domain.DiskUsedMB,
			&domain.VerifiedAt, &domain.CreatedAt, &domain.UpdatedAt, &accountCount); scanErr != nil {
			return MailDomainListResult{}, scanErr
		}
		domain.Status = models.MailDomainStatus(status)
		domains = append(domains, mailDecryptDomainAdminPassword(domain))
		counts = append(counts, accountCount)
	}
	if err := rows.Err(); err != nil {
		return MailDomainListResult{}, err
	}

	subscriptionIDs := make([]int32, 0, len(domains))
	sourceIDs := make([]int32, 0, len(domains))
	for _, domain := range domains {
		subscriptionIDs = append(subscriptionIDs, domain.SubscriptionID)
		sourceIDs = append(sourceIDs, domain.SourceID)
	}

	subscriptions := map[int32]models.MailSubscription{}
	if len(subscriptionIDs) > 0 {
		subRows, err := Pool().Query(ctx,
			`SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = ANY($1)`, subscriptionIDs)
		if err != nil {
			return MailDomainListResult{}, err
		}
		for subRows.Next() {
			subscription, scanErr := mailScanSubscription(subRows)
			if scanErr != nil {
				subRows.Close()
				return MailDomainListResult{}, scanErr
			}
			subscriptions[subscription.ID] = subscription
		}
		subRows.Close()
		if err := subRows.Err(); err != nil {
			return MailDomainListResult{}, err
		}
	}

	userIDs := make([]int32, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		userIDs = append(userIDs, subscription.UserID)
	}
	users := map[int32]MailUserSelect{}
	if len(userIDs) > 0 {
		userRows, err := Pool().Query(ctx,
			`SELECT id, username, email, avatar_style, avatar_badge_id FROM users WHERE id = ANY($1)`, userIDs)
		if err != nil {
			return MailDomainListResult{}, err
		}
		for userRows.Next() {
			var user MailUserSelect
			if scanErr := userRows.Scan(&user.ID, &user.Username, &user.Email, &user.AvatarStyle, &user.AvatarBadgeID); scanErr != nil {
				userRows.Close()
				return MailDomainListResult{}, scanErr
			}
			users[user.ID] = user
		}
		userRows.Close()
		if err := userRows.Err(); err != nil {
			return MailDomainListResult{}, err
		}
	}

	sources := map[int32]models.MailSource{}
	if len(sourceIDs) > 0 {
		sourceRows, err := Pool().Query(ctx,
			`SELECT `+mailSourceColumns+` FROM mail_sources WHERE id = ANY($1)`, sourceIDs)
		if err != nil {
			return MailDomainListResult{}, err
		}
		for sourceRows.Next() {
			source, scanErr := mailScanSource(sourceRows)
			if scanErr != nil {
				sourceRows.Close()
				return MailDomainListResult{}, scanErr
			}
			sources[source.ID] = mailDecryptSource(source)
		}
		sourceRows.Close()
		if err := sourceRows.Err(); err != nil {
			return MailDomainListResult{}, err
		}
	}

	result := make([]MailDomainWithRelations, 0, len(domains))
	for index, domain := range domains {
		subscription := subscriptions[domain.SubscriptionID]
		result = append(result, MailDomainWithRelations{
			Domain:       domain,
			Subscription: subscription,
			User:         users[subscription.UserID],
			Source:       sources[domain.SourceID],
			AccountCount: counts[index],
		})
	}
	return MailDomainListResult{Domains: result, Total: total}, nil
}

// ==================== mail accounts ====================

// MailGetAccountsByDomain mirrors getMailAccountsByDomain.
func MailGetAccountsByDomain(ctx context.Context, domainID int32) ([]models.MailAccount, error) {
	return mailLoadAccountsOrdered(ctx, domainID)
}

// MailGetAccountByID mirrors getMailAccountById (include: domain).
func MailGetAccountByID(ctx context.Context, id int32) (*models.MailAccount, error) {
	row := Pool().QueryRow(ctx, `SELECT `+mailAccountColumns+` FROM mail_accounts WHERE id = $1`, id)
	account, err := mailScanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	domainRow := Pool().QueryRow(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE id = $1`, account.DomainID)
	domain, err := mailScanDomain(domainRow)
	if err != nil {
		return nil, err
	}
	domain = mailDecryptDomainAdminPassword(domain)
	account.Domain = &domain
	return &account, nil
}

// MailCheckAccountExists mirrors checkMailAccountExists.
func MailCheckAccountExists(ctx context.Context, domainID int32, username string) (bool, error) {
	return mailCheckAccountExists(ctx, nil, domainID, username)
}

func mailCheckAccountExists(ctx context.Context, tx pgx.Tx, domainID int32, username string) (bool, error) {
	var count int64
	err := mailDB(tx).QueryRow(ctx,
		`SELECT count(*) FROM mail_accounts WHERE domain_id = $1 AND username = $2`, domainID, username).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// MailCreateAccountInput mirrors the createMailAccount argument.
type MailCreateAccountInput struct {
	DomainID    int32
	Email       string
	Username    string
	DisplayName *string
	DiskLimitMB *int32
	IsAdmin     *bool
}

// MailCreateAccount mirrors createMailAccount.
func MailCreateAccount(ctx context.Context, input MailCreateAccountInput) (models.MailAccount, error) {
	return mailCreateAccount(ctx, nil, input)
}

// MailCreateAccountWithTx runs createMailAccount on the caller's transaction.
func MailCreateAccountWithTx(ctx context.Context, tx pgx.Tx, input MailCreateAccountInput) (models.MailAccount, error) {
	return mailCreateAccount(ctx, tx, input)
}

func mailCreateAccount(ctx context.Context, tx pgx.Tx, input MailCreateAccountInput) (models.MailAccount, error) {
	row := mailDB(tx).QueryRow(ctx,
		`INSERT INTO mail_accounts (domain_id, email, username, display_name, disk_limit_mb, disk_used_mb,
		                            is_admin, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, COALESCE($5, 2048), 0, COALESCE($6, false), NOW(), NOW())
		 RETURNING `+mailAccountColumns,
		input.DomainID, input.Email, input.Username, input.DisplayName, input.DiskLimitMB, input.IsAdmin)
	return mailScanAccount(row)
}

// MailUpdateAccountInput mirrors the updateMailAccount argument.
type MailUpdateAccountInput struct {
	DisplayName *string
	DiskLimitMB *int32
	DiskUsedMB  *int32
	IsAdmin     *bool
}

// MailUpdateAccount mirrors updateMailAccount.
func MailUpdateAccount(ctx context.Context, id int32, input MailUpdateAccountInput) (models.MailAccount, error) {
	return mailUpdateAccount(ctx, nil, id, input)
}

// MailUpdateAccountWithTx runs updateMailAccount on the caller's transaction.
func MailUpdateAccountWithTx(ctx context.Context, tx pgx.Tx, id int32, input MailUpdateAccountInput) (models.MailAccount, error) {
	return mailUpdateAccount(ctx, tx, id, input)
}

func mailUpdateAccount(ctx context.Context, tx pgx.Tx, id int32, input MailUpdateAccountInput) (models.MailAccount, error) {
	querier := mailDB(tx)
	sets := make([]string, 0, 5)
	args := make([]any, 0, 5)
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}

	if input.DisplayName != nil {
		sets = append(sets, "display_name = "+arg(*input.DisplayName))
	}
	if input.DiskLimitMB != nil {
		sets = append(sets, "disk_limit_mb = "+arg(*input.DiskLimitMB))
	}
	if input.DiskUsedMB != nil {
		sets = append(sets, "disk_used_mb = "+arg(*input.DiskUsedMB))
	}
	if input.IsAdmin != nil {
		sets = append(sets, "is_admin = "+arg(*input.IsAdmin))
	}

	if len(sets) == 0 {
		row := querier.QueryRow(ctx, `SELECT `+mailAccountColumns+` FROM mail_accounts WHERE id = $1`, id)
		return mailScanAccount(row)
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	row := querier.QueryRow(ctx,
		`UPDATE mail_accounts SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailAccountColumns,
		args...)
	return mailScanAccount(row)
}

// MailDeleteAccount mirrors deleteMailAccount.
func MailDeleteAccount(ctx context.Context, id int32) error {
	_, err := Pool().Exec(ctx, `DELETE FROM mail_accounts WHERE id = $1`, id)
	return err
}

// MailGetAccountCount mirrors getMailAccountCount.
func MailGetAccountCount(ctx context.Context, domainID int32) (int64, error) {
	var count int64
	err := Pool().QueryRow(ctx, `SELECT count(*) FROM mail_accounts WHERE domain_id = $1`, domainID).Scan(&count)
	return count, err
}

// ==================== statistics ====================

// MailUsageStats mirrors getSubscriptionUsageStats.
type MailUsageStats struct {
	DomainCount  int64
	AccountCount int64
	DiskUsedMB   int64
}

// MailGetSubscriptionUsageStats mirrors getSubscriptionUsageStats.
func MailGetSubscriptionUsageStats(ctx context.Context, subscriptionID int32) (MailUsageStats, error) {
	var stats MailUsageStats
	err := Pool().QueryRow(ctx,
		`SELECT count(*),
		        COALESCE((SELECT count(*) FROM mail_accounts a
		                   JOIN mail_domains d ON d.id = a.domain_id
		                  WHERE d.subscription_id = $1), 0),
		        COALESCE(SUM(disk_used_mb), 0)
		   FROM mail_domains WHERE subscription_id = $1`, subscriptionID).
		Scan(&stats.DomainCount, &stats.AccountCount, &stats.DiskUsedMB)
	return stats, err
}

// MailExpiringSubscription mirrors getExpiringSubscriptions' projection.
type MailExpiringSubscription struct {
	ID        int32
	UserID    int32
	ExpiresAt time.Time
}

// MailGetExpiringSubscriptions mirrors getExpiringSubscriptions.
func MailGetExpiringSubscriptions(ctx context.Context, daysAhead int, now time.Time) ([]MailExpiringSubscription, error) {
	deadline := MailAddCalendarDays(now, daysAhead)

	rows, err := Pool().Query(ctx,
		`SELECT id, user_id, expires_at FROM mail_subscriptions
		  WHERE status = $1 AND auto_renew = true AND expires_at > $2 AND expires_at <= $3
		  ORDER BY expires_at ASC, id ASC`,
		string(models.MailSubscriptionStatusActive), now.UTC(), deadline.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]MailExpiringSubscription, 0)
	for rows.Next() {
		var subscription MailExpiringSubscription
		if scanErr := rows.Scan(&subscription.ID, &subscription.UserID, &subscription.ExpiresAt); scanErr != nil {
			return nil, scanErr
		}
		result = append(result, subscription)
	}
	return result, rows.Err()
}

// ==================== transaction helpers used by routes/mail.go ====================

// MailSubscriptionAllocatedDiskMB mirrors getMailSubscriptionAllocatedDiskMb.
func MailSubscriptionAllocatedDiskMB(ctx context.Context, tx pgx.Tx, subscriptionID int32, excludeAccountID *int32) (int64, error) {
	var allocated int64
	err := mailDB(tx).QueryRow(ctx,
		`SELECT COALESCE(SUM(a.disk_limit_mb), 0)
		   FROM mail_accounts a JOIN mail_domains d ON d.id = a.domain_id
		  WHERE d.subscription_id = $1 AND ($2::int IS NULL OR a.id <> $2)`,
		subscriptionID, excludeAccountID).Scan(&allocated)
	return allocated, err
}

// MailDiskQuotaAllows mirrors the arithmetic of mailSubscriptionHasDiskQuota:
// already-allocated MB plus the requested MB must fit in the subscription's GB
// budget converted to MB.
func MailDiskQuotaAllows(allocatedDiskLimitMB, requestedDiskLimitMB, subscriptionDiskLimitGB int64) bool {
	return allocatedDiskLimitMB+requestedDiskLimitMB <= subscriptionDiskLimitGB*1024
}

// MailSubscriptionHasDiskQuota mirrors mailSubscriptionHasDiskQuota.
func MailSubscriptionHasDiskQuota(
	ctx context.Context,
	tx pgx.Tx,
	subscriptionID int32,
	subscriptionDiskLimitGB int64,
	requestedDiskLimitMB int64,
	excludeAccountID *int32,
) (bool, error) {
	allocated, err := MailSubscriptionAllocatedDiskMB(ctx, tx, subscriptionID, excludeAccountID)
	if err != nil {
		return false, err
	}
	return MailDiskQuotaAllows(allocated, requestedDiskLimitMB, subscriptionDiskLimitGB), nil
}

// MailCountDomainsForSubscription mirrors the inline mailDomain.count in the
// add-domain route.
func MailCountDomainsForSubscription(ctx context.Context, tx pgx.Tx, subscriptionID int32) (int64, error) {
	var count int64
	err := mailDB(tx).QueryRow(ctx,
		`SELECT count(*) FROM mail_domains WHERE subscription_id = $1`, subscriptionID).Scan(&count)
	return count, err
}

// MailFindDomainByDomainAndSource mirrors the compound-unique lookup
// `domain_sourceId: { domain, sourceId }`.
func MailFindDomainByDomainAndSource(ctx context.Context, tx pgx.Tx, domain string, sourceID int32) (*models.MailDomain, error) {
	row := mailDB(tx).QueryRow(ctx,
		`SELECT `+mailDomainColumns+` FROM mail_domains WHERE domain = $1 AND source_id = $2`, domain, sourceID)
	found, err := mailScanDomain(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &found, nil
}

// MailFindSubscriptionByIDTx re-reads a subscription inside a transaction.
func MailFindSubscriptionByIDTx(ctx context.Context, tx pgx.Tx, id int32) (*models.MailSubscription, error) {
	row := mailDB(tx).QueryRow(ctx, `SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions WHERE id = $1`, id)
	subscription, err := mailScanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subscription, nil
}

// MailFindExistingSubscriptionForPurchase mirrors the findFirst on
// (userId, sourceId, status in [active, expired]) ordered by updatedAt desc.
func MailFindExistingSubscriptionForPurchase(ctx context.Context, tx pgx.Tx, userID, sourceID int32) (*models.MailSubscription, error) {
	row := mailDB(tx).QueryRow(ctx,
		`SELECT `+mailSubscriptionColumns+` FROM mail_subscriptions
		  WHERE user_id = $1 AND source_id = $2 AND status IN ($3, $4)
		  ORDER BY updated_at DESC LIMIT 1`,
		userID, sourceID,
		string(models.MailSubscriptionStatusActive), string(models.MailSubscriptionStatusExpired))
	subscription, err := mailScanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subscription, nil
}

// MailFindAffBindingTx mirrors `include: { affBinding: true }` for one subscription.
func MailFindAffBindingTx(ctx context.Context, tx pgx.Tx, subscriptionID int32) (*models.MailSubscriptionAffBinding, error) {
	var binding models.MailSubscriptionAffBinding
	err := mailDB(tx).QueryRow(ctx,
		`SELECT id, mail_subscription_id, aff_code_id, created_at
		   FROM mail_subscription_aff_bindings WHERE mail_subscription_id = $1`, subscriptionID).
		Scan(&binding.ID, &binding.MailSubscriptionID, &binding.AffCodeID, &binding.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

// MailFindAffCodeByIDTx loads an aff code's enabled flag and rates for the
// purchase/renew discount decision.
func MailFindAffCodeByIDTx(ctx context.Context, tx pgx.Tx, id int32) (*models.AffCode, error) {
	var code models.AffCode
	err := mailDB(tx).QueryRow(ctx,
		`SELECT id, code, user_id, package_plan_id, discount_rate, commission_rate,
		        enabled, used_count, total_earnings, created_at
		   FROM aff_codes WHERE id = $1`, id).
		Scan(&code.ID, &code.Code, &code.UserID, &code.PackagePlanID, &code.DiscountRate,
			&code.CommissionRate, &code.Enabled, &code.UsedCount, &code.TotalEarnings, &code.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &code, nil
}

// decimalFromFloat mirrors Prisma's Decimal-from-number conversion: the shortest
// decimal representation of the double (what decimal.js does when Prisma receives
// a JavaScript number for a Decimal column).
func decimalFromFloat(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(value)}
}

// mailNullableString renders a *string parameter.
func mailNullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// ==================== balance helpers (mail flows) ====================

// MailReadUserBalanceTx reads a user's balance as a decimal inside a transaction.
func MailReadUserBalanceTx(ctx context.Context, tx pgx.Tx, userID int32) (models.Decimal, bool, error) {
	var balance models.Decimal
	err := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Decimal{}, false, nil
	}
	if err != nil {
		return models.Decimal{}, false, err
	}
	return balance, true, nil
}

// MailDeductUserBalanceTx mirrors
// `tx.user.updateMany({ where: { id, balance: { gte: amount } }, data: { balance: { decrement: amount } } })`
// and reports whether a row was updated.
func MailDeductUserBalanceTx(ctx context.Context, tx pgx.Tx, userID int32, amount models.Decimal) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET balance = balance - $1, updated_at = NOW() WHERE id = $2 AND balance >= $1`,
		amount, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MailIncrementUserBalanceTx mirrors `tx.user.update({ data: { balance: { increment } } })`
// and returns the balance before and after.
func MailIncrementUserBalanceTx(ctx context.Context, tx pgx.Tx, userID int32, amount models.Decimal) (models.Decimal, models.Decimal, error) {
	var before models.Decimal
	if err := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id = $1`, userID).Scan(&before); err != nil {
		return models.Decimal{}, models.Decimal{}, err
	}
	var after models.Decimal
	if err := tx.QueryRow(ctx,
		`UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2 RETURNING balance`,
		amount, userID).Scan(&after); err != nil {
		return models.Decimal{}, models.Decimal{}, err
	}
	return before, after, nil
}

// MailInsertBalanceLogTx mirrors `tx.balanceLog.create({ data: {...} })`.
func MailInsertBalanceLogTx(
	ctx context.Context,
	tx pgx.Tx,
	userID int32,
	logType models.BalanceLogType,
	amount models.Decimal,
	balanceBefore models.Decimal,
	balanceAfter models.Decimal,
	remark *string,
) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, order_id,
		                           instance_id, remark, created_at)
		 VALUES ($1, $2, $3, $4, $5, NULL, NULL, $6, NOW())`,
		userID, string(logType), amount, balanceBefore, balanceAfter, mailNullableString(remark))
	return err
}

// MailUpdateSubscriptionWithTx applies the purchase/renew mutations inside a
// transaction and returns the updated row.
func MailUpdateSubscriptionWithTx(
	ctx context.Context,
	tx pgx.Tx,
	id int32,
	status models.MailSubscriptionStatus,
	expiresAt time.Time,
	planID *int32,
	domainLimit *int32,
	diskLimitGB *int32,
) (models.MailSubscription, error) {
	sets := []string{"status = $1", "expires_at = $2", "updated_at = NOW()"}
	args := []any{string(status), expiresAt.UTC()}
	arg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if planID != nil {
		sets = append(sets, "plan_id = "+arg(*planID))
	}
	if domainLimit != nil {
		sets = append(sets, "domain_limit = "+arg(*domainLimit))
	}
	if diskLimitGB != nil {
		sets = append(sets, "disk_limit_gb = "+arg(*diskLimitGB))
	}
	args = append(args, id)
	row := tx.QueryRow(ctx,
		`UPDATE mail_subscriptions SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(len(args))+` RETURNING `+mailSubscriptionColumns,
		args...)
	return mailScanSubscription(row)
}

// MailSetSubscriptionAutoRenewTx mirrors
// `tx.mailSubscription.updateMany({ where: { id, userId }, data: { autoRenew } })`
// and reports the affected row count.
func MailSetSubscriptionAutoRenewTx(ctx context.Context, tx pgx.Tx, id, userID int32, autoRenew bool) (int64, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE mail_subscriptions SET auto_renew = $1, updated_at = NOW()
		  WHERE id = $2 AND user_id = $3`, autoRenew, id, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MailDeleteSubscriptionTx mirrors `tx.mailSubscription.delete(...)`.
func MailDeleteSubscriptionTx(ctx context.Context, tx pgx.Tx, id int32) error {
	_, err := tx.Exec(ctx, `DELETE FROM mail_subscriptions WHERE id = $1`, id)
	return err
}

// MailUpdateDomainStatusForActiveSubscription mirrors the updateMany guard used by
// resumeMailSubscriptionDomains: the row is only touched while its subscription is
// still active and unexpired.
func MailUpdateDomainStatusForActiveSubscription(
	ctx context.Context,
	domainID int32,
	status models.MailDomainStatus,
	now time.Time,
) (int64, error) {
	tag, err := Pool().Exec(ctx,
		`UPDATE mail_domains SET status = $1, updated_at = NOW()
		  WHERE id = $2 AND subscription_id IN (
		        SELECT id FROM mail_subscriptions WHERE status = $3 AND expires_at > $4)`,
		string(status), domainID, string(models.MailSubscriptionStatusActive), now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MailCreateSubscriptionWithTx runs createMailSubscription on the caller's transaction.
func MailCreateSubscriptionWithTx(ctx context.Context, tx pgx.Tx, input MailCreateSubscriptionInput) (models.MailSubscription, error) {
	return mailCreateSubscription(ctx, tx, input)
}

// MailCheckAccountExistsTx runs checkMailAccountExists on the caller's transaction.
func MailCheckAccountExistsTx(ctx context.Context, tx pgx.Tx, domainID int32, username string) (bool, error) {
	return mailCheckAccountExists(ctx, tx, domainID, username)
}

// MailFindDomainByIDTx re-reads a domain inside a transaction (admin password
// decrypted, like every other domain read).
func MailFindDomainByIDTx(ctx context.Context, tx pgx.Tx, id int32) (*models.MailDomain, error) {
	row := mailDB(tx).QueryRow(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE id = $1`, id)
	domain, err := mailScanDomain(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decrypted := mailDecryptDomainAdminPassword(domain)
	return &decrypted, nil
}

// MailFindAccountByIDTx re-reads a mailbox account inside a transaction.
func MailFindAccountByIDTx(ctx context.Context, tx pgx.Tx, id int32) (*models.MailAccount, error) {
	row := mailDB(tx).QueryRow(ctx, `SELECT `+mailAccountColumns+` FROM mail_accounts WHERE id = $1`, id)
	account, err := mailScanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &account, nil
}
