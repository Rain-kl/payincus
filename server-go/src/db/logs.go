// Port of server/src/db/logs.ts.
//
// The pure helpers that the TypeScript module imports from lib/log-localization
// and lib/risk-audit cannot be imported here: server-go/src/lib already imports
// server-go/src/db (token_invalidation.go / operation_verification.go), so a
// db -> lib import would be an import cycle. Instead the lib package registers
// its canonical implementations through the Register* hooks below during init()
// (see lib/log_localization.go and lib/risk_audit.go). The default values keep
// the package usable in isolation and are overwritten as soon as lib is linked.
package db

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ==================== 日志模块常量 ====================

const (
	logModuleFilterMaxLength       = 64
	logSearchFilterMaxLength       = 128
	logInstanceNameFilterMaxLength = 128

	// logMaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER.
	logMaxSafeInteger = 1<<53 - 1
)

// LogModule mirrors the TypeScript LogModule constant object.
const (
	LogModuleSecurity     = "security"
	LogModuleInstance     = "instance"
	LogModuleSnapshot     = "snapshot"
	LogModuleBackup       = "backup"
	LogModuleImage        = "image"
	LogModuleHost         = "host"
	LogModulePackage      = "package"
	LogModuleUser         = "user"
	LogModulePersonal     = "personal"
	LogModuleSSHKey       = "ssh_key"
	LogModuleNotification = "notification"
	LogModulePlugin       = "plugin"
	LogModuleSystem       = "system"
	LogModuleAuth         = "auth"
)

// LogAction mirrors the TypeScript LogAction constant object.
const (
	LogActionInstanceCreate      = "instance.create"
	LogActionInstanceDelete      = "instance.delete"
	LogActionInstanceStart       = "instance.start"
	LogActionInstanceStop        = "instance.stop"
	LogActionInstanceRestart     = "instance.restart"
	LogActionInstanceRebuild     = "instance.rebuild"
	LogActionInstanceUpdateQuota = "instance.update_quota"
	LogActionPortAdd             = "port.add"
	LogActionPortDelete          = "port.delete"
	LogActionSnapshotCreate      = "snapshot.create"
	LogActionSnapshotDelete      = "snapshot.delete"
	LogActionSnapshotRestore     = "snapshot.restore"
	LogActionBackupCreate        = "backup.create"
	LogActionBackupDelete        = "backup.delete"
	LogActionBackupExport        = "backup.export"
	LogActionImageSync           = "image.sync"
	LogActionImageDelete         = "image.delete"
	LogActionImageCreate         = "image.create"
	LogActionImageUpdate         = "image.update"
	LogActionHostCreate          = "host.create"
	LogActionHostUpdate          = "host.update"
	LogActionHostDelete          = "host.delete"
	LogActionHostTest            = "host.test"
	LogActionPackageCreate       = "package.create"
	LogActionPackageUpdate       = "package.update"
	LogActionPackageDelete       = "package.delete"
	LogActionUserBan             = "user.ban"
	LogActionUserUnban           = "user.unban"
	LogActionUserDelete          = "user.delete"
	LogActionUserUpdateQuota     = "user.update_quota"
	LogActionUserRevokeSessions  = "user.revoke_sessions"
	LogActionProfileUpdate       = "profile.update"
	LogActionPasswordChange      = "password.change"
	LogActionSSHKeyAdd           = "ssh_key.add"
	LogActionSSHKeyDelete        = "ssh_key.delete"
	LogActionNotificationAdd     = "notification.add"
	LogActionNotificationDelete  = "notification.delete"
	LogAction2FASetup            = "2fa.setup"
	LogAction2FAEnable           = "2fa.enable"
	LogAction2FADisable          = "2fa.disable"
	LogAction2FARecoveryReset    = "2fa.recovery_reset"
	LogActionSessionRevoke       = "session.revoke"
	LogActionSessionRevokeAll    = "session.revoke_all"
	LogActionSystemConfigUpdate  = "system.config_update"
)

// LogResult mirrors the TypeScript LogResult constant object.
const (
	LogResultSuccess = "success"
	LogResultFailed  = "failed"
	LogResultWarning = "warning"
)

// CreateLogOptions mirrors the TypeScript CreateLogOptions.
//
// The TypeScript type distinguishes `instanceId` being absent (infer it from the
// content) from being explicitly null (never infer). Go cannot express that with
// a bare *int32, so InstanceIDProvided carries the "was it provided" bit.
type CreateLogOptions struct {
	InstanceID         *int32
	InstanceIDProvided bool
}

// LogOptionsWithInstance mirrors `{ instanceId }` (an explicitly provided value,
// which may be nil to mean "explicitly null").
func LogOptionsWithInstance(instanceID *int32) CreateLogOptions {
	return CreateLogOptions{InstanceID: instanceID, InstanceIDProvided: true}
}

// AuditLogRow mirrors the TypeScript AuditLogRow. Field names are snake_case
// because the Node route serialises these objects verbatim (see routes/logs.ts),
// so the wire format is snake_case, not the Prisma camelCase shape.
type AuditLogRow struct {
	ID                   int32   `json:"id"`
	UserID               *int32  `json:"user_id"`
	InstanceID           *int32  `json:"instance_id"`
	Username             *string `json:"username"`
	Module               string  `json:"module"`
	Action               string  `json:"action"`
	Content              string  `json:"content"`
	Result               string  `json:"result"`
	CreatedAt            string  `json:"created_at"`
	RiskLevel            string  `json:"risk_level"`
	RiskTitle            string  `json:"risk_title"`
	ApprovalRequired     bool    `json:"approval_required"`
	VerificationRequired bool    `json:"verification_required"`
	BatchSensitive       bool    `json:"batch_sensitive"`
}

// ==================== lib registration hooks ====================

// LogRisk mirrors lib.LogRisk; it is declared here so the lib package can
// register its classifier without db importing lib.
type LogRisk struct {
	RiskLevel            string
	RiskTitle            string
	ApprovalRequired     bool
	VerificationRequired bool
	BatchSensitive       bool
}

var (
	logNormalizeContentForStorage = func(content string) string { return content }
	logClassifyRisk               = func(module, action, content string) LogRisk {
		return LogRisk{RiskLevel: "low", RiskTitle: "普通操作"}
	}
	logRedactAuditText = func(value string) string { return value }
)

// RegisterLogNormalizer wires lib.NormalizeLogContentForStorage into the db layer.
func RegisterLogNormalizer(fn func(string) string) {
	if fn != nil {
		logNormalizeContentForStorage = fn
	}
}

// RegisterLogRiskClassifier wires lib.ClassifyLogRisk into the db layer.
func RegisterLogRiskClassifier(fn func(module, action, content string) LogRisk) {
	if fn != nil {
		logClassifyRisk = fn
	}
}

// RegisterLogAuditRedactor wires lib.RedactAuditText into the db layer.
func RegisterLogAuditRedactor(fn func(string) string) {
	if fn != nil {
		logRedactAuditText = fn
	}
}

// ==================== filter helpers ====================

func normalizeLogFilterValue(value *string, maxLength int) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	runes := []rune(trimmed)
	if len(runes) > maxLength {
		runes = runes[:maxLength]
	}
	result := string(runes)
	return &result
}

var logDateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseValidDateFilter(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	for _, layout := range logDateLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return &parsed
		}
	}
	return nil
}

// ==================== 基础日志函数 ====================

var logInstanceNamePattern = regexp.MustCompile(`(?i)instance\s+"([^"]+)"`)

var logInstanceIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)instance\s*#(\d+)`),
	regexp.MustCompile(`(?i)instance:\s*[^()]*\(#(\d+)\)`),
	regexp.MustCompile(`(?i)new instance ID:\s*(\d+)`),
}

func extractLogInstanceIDs(content string) []int64 {
	seen := map[int64]bool{}
	var ids []int64
	for _, pattern := range logInstanceIDPatterns {
		for _, match := range pattern.FindAllStringSubmatch(content, -1) {
			value, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				continue
			}
			if value > 0 && value <= logMaxSafeInteger && !seen[value] {
				seen[value] = true
				ids = append(ids, value)
			}
		}
	}
	return ids
}

func inferLogInstanceID(ctx context.Context, userID *int64, content string) (*int32, error) {
	instanceIDs := extractLogInstanceIDs(content)
	if len(instanceIDs) == 1 {
		var instance struct {
			ID     int32  `gorm:"column:id"`
			UserID *int32 `gorm:"column:user_id"`
		}
		if err := DB.WithContext(ctx).Raw("SELECT id, user_id FROM instances WHERE id = ? LIMIT 1", instanceIDs[0]).Scan(&instance).Error; err != nil {
			return nil, err
		}
		if instance.ID != 0 && (userID == nil || (instance.UserID != nil && int64(*instance.UserID) == *userID)) {
			id := instance.ID
			return &id, nil
		}
	}

	names := make([]string, 0)
	for _, match := range logInstanceNamePattern.FindAllStringSubmatch(content, -1) {
		if name := strings.TrimSpace(match[1]); name != "" {
			names = append(names, name)
		}
	}

	uniqueNames := uniqueLogStrings(names)
	if len(uniqueNames) != 1 {
		return nil, nil
	}

	sql := "SELECT id FROM instances WHERE name = ?"
	args := []any{uniqueNames[0]}
	if userID != nil {
		sql += " AND user_id = ?"
		args = append(args, *userID)
	}
	sql += " LIMIT 2"

	var matches []struct {
		ID int32 `gorm:"column:id"`
	}
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&matches).Error; err != nil {
		return nil, err
	}
	if len(matches) == 1 {
		id := matches[0].ID
		return &id, nil
	}
	return nil, nil
}

func uniqueLogStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

// CreateLog mirrors createLog(). Failures are logged to stderr and swallowed so
// a log write can never break the main business flow, matching the TypeScript.
func CreateLog(ctx context.Context, userID *int64, module, action, content, result string, options CreateLogOptions) {
	instanceID := options.InstanceID
	if !options.InstanceIDProvided {
		inferred, err := inferLogInstanceID(ctx, userID, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "日志记录失败: %v\n", err)
			return
		}
		instanceID = inferred
	}
	normalizedContent := logNormalizeContentForStorage(content)

	err := DB.WithContext(ctx).Exec(
		"INSERT INTO logs (user_id, instance_id, module, action, content, result) VALUES (?, ?, ?, ?, ?, ?)",
		nullableInt64(userID), nullableInt32(instanceID), module, action, normalizedContent, result,
	).Error
	if err != nil {
		fmt.Fprintf(os.Stderr, "日志记录失败: %v\n", err)
	}
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt32(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

// LogSuccess mirrors logSuccess().
func LogSuccess(ctx context.Context, userID *int64, module, action, content string, options CreateLogOptions) {
	CreateLog(ctx, userID, module, action, content, LogResultSuccess, options)
}

// LogFailure mirrors logFailure().
func LogFailure(ctx context.Context, userID *int64, module, action, content string, options CreateLogOptions) {
	CreateLog(ctx, userID, module, action, content, LogResultFailed, options)
}

// LogWarning mirrors logWarning().
func LogWarning(ctx context.Context, userID *int64, module, action, content string, options CreateLogOptions) {
	CreateLog(ctx, userID, module, action, content, LogResultWarning, options)
}

// ==================== where builder ====================

type logWhereInput struct {
	Module       *string
	UserID       *int64
	StartDate    *string
	EndDate      *string
	Search       *string
	InstanceID   *int64
	InstanceName *string
}

func buildLogWhere(input logWhereInput) (string, []any) {
	safeModule := normalizeLogFilterValue(input.Module, logModuleFilterMaxLength)
	safeSearch := normalizeLogFilterValue(input.Search, logSearchFilterMaxLength)
	safeInstanceName := normalizeLogFilterValue(input.InstanceName, logInstanceNameFilterMaxLength)
	safeStartDate := parseValidDateFilter(input.StartDate)
	safeEndDate := parseValidDateFilter(input.EndDate)

	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)

	if safeModule != nil {
		conditions = append(conditions, "l.module = ?")
		args = append(args, *safeModule)
	}
	if input.UserID != nil {
		conditions = append(conditions, "l.user_id = ?")
		args = append(args, *input.UserID)
	}
	if safeStartDate != nil {
		conditions = append(conditions, "l.created_at >= ?")
		args = append(args, *safeStartDate)
	}
	if safeEndDate != nil {
		conditions = append(conditions, "l.created_at <= ?")
		args = append(args, *safeEndDate)
	}
	if safeSearch != nil {
		pattern := "%" + *safeSearch + "%"
		conditions = append(conditions, "(l.content ILIKE ? OR l.action ILIKE ? OR u.username ILIKE ?)")
		args = append(args, pattern, pattern, pattern)
	}
	if input.InstanceID != nil {
		conditions = append(conditions, "l.instance_id = ?")
		args = append(args, *input.InstanceID)
	} else if safeInstanceName != nil {
		conditions = append(conditions, "l.content ILIKE ?")
		args = append(args, "%instance \""+*safeInstanceName+"\"%")
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conditions, " AND "), args
}

// ==================== audit projections ====================

type logAuditRow struct {
	ID         int32     `gorm:"column:id"`
	UserID     *int32    `gorm:"column:user_id"`
	InstanceID *int32    `gorm:"column:instance_id"`
	Module     string    `gorm:"column:module"`
	Action     string    `gorm:"column:action"`
	Content    string    `gorm:"column:content"`
	Result     string    `gorm:"column:result"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	Username   *string   `gorm:"column:username"`
}

const logAuditSelectColumns = `
	l.id, l.user_id, l.instance_id, l.module, l.action, l.content, l.result, l.created_at,
	u.username AS username`

const logAuditFrom = ` FROM logs l LEFT JOIN users u ON u.id = l.user_id `

func mapLogWithRisk(row logAuditRow) AuditLogRow {
	risk := logClassifyRisk(row.Module, row.Action, row.Content)

	username := ""
	if row.Username != nil {
		username = *row.Username
	}
	var usernamePtr *string
	if redacted := logRedactAuditText(username); redacted != "" {
		usernamePtr = &redacted
	}

	return AuditLogRow{
		ID:                   row.ID,
		UserID:               row.UserID,
		InstanceID:           row.InstanceID,
		Username:             usernamePtr,
		Module:               row.Module,
		Action:               row.Action,
		Content:              logRedactAuditText(row.Content),
		Result:               row.Result,
		CreatedAt:            formatISOTime(row.CreatedAt),
		RiskLevel:            risk.RiskLevel,
		RiskTitle:            risk.RiskTitle,
		ApprovalRequired:     risk.ApprovalRequired,
		VerificationRequired: risk.VerificationRequired,
		BatchSensitive:       risk.BatchSensitive,
	}
}

// formatISOTime mirrors Date.prototype.toISOString() (UTC, millisecond precision).
func formatISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

func queryAuditRows(ctx context.Context, whereSQL string, args []any, limit, offset int) ([]logAuditRow, error) {
	listArgs := append(append([]any{}, args...), limit, offset)
	sql := "SELECT " + logAuditSelectColumns + logAuditFrom + whereSQL +
		" ORDER BY l.created_at DESC LIMIT ? OFFSET ?"

	var rows []logAuditRow
	if err := DB.WithContext(ctx).Raw(sql, listArgs...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// LogPage is the paginated envelope returned by GetLogsPaginated.
type LogPage struct {
	Items      []AuditLogRow `json:"items"`
	Total      int64         `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"pageSize"`
	TotalPages int64         `json:"totalPages"`
}

// LogPaginatedOptions mirrors the getLogsPaginated() option object.
type LogPaginatedOptions struct {
	Page         int
	PageSize     int
	Module       *string
	UserID       *int64
	StartDate    *string
	EndDate      *string
	Search       *string
	InstanceID   *int64
	InstanceName *string
}

// GetLogsPaginated mirrors getLogsPaginated().
func GetLogsPaginated(ctx context.Context, options LogPaginatedOptions) (LogPage, error) {
	safePage := 1
	if options.Page > 0 {
		safePage = options.Page
	}
	safePageSize := 20
	if options.PageSize != 0 {
		safePageSize = options.PageSize
		if safePageSize < 1 {
			safePageSize = 1
		}
		if safePageSize > 100 {
			safePageSize = 100
		}
	}

	whereSQL, args := buildLogWhere(logWhereInput{
		Module:       options.Module,
		UserID:       options.UserID,
		StartDate:    options.StartDate,
		EndDate:      options.EndDate,
		Search:       options.Search,
		InstanceID:   options.InstanceID,
		InstanceName: options.InstanceName,
	})

	var total int64
	countSQL := "SELECT COUNT(*)" + logAuditFrom + whereSQL
	if err := DB.WithContext(ctx).Raw(countSQL, args...).Scan(&total).Error; err != nil {
		return LogPage{}, err
	}

	rows, err := queryAuditRows(ctx, whereSQL, args, safePageSize, (safePage-1)*safePageSize)
	if err != nil {
		return LogPage{}, err
	}

	items := make([]AuditLogRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, mapLogWithRisk(row))
	}

	return LogPage{
		Items:      items,
		Total:      total,
		Page:       safePage,
		PageSize:   safePageSize,
		TotalPages: totalPages(total, safePageSize),
	}, nil
}

// LogAuditExportOptions mirrors the getLogsForAuditExport() option object.
type LogAuditExportOptions struct {
	Module       *string
	StartDate    *string
	EndDate      *string
	Search       *string
	InstanceID   *int64
	InstanceName *string
	Limit        int
}

// GetLogsForAuditExport mirrors getLogsForAuditExport().
func GetLogsForAuditExport(ctx context.Context, options LogAuditExportOptions) ([]AuditLogRow, error) {
	safeLimit := 500
	if options.Limit != 0 {
		safeLimit = options.Limit
		if safeLimit < 1 {
			safeLimit = 1
		}
		if safeLimit > 1000 {
			safeLimit = 1000
		}
	}

	whereSQL, args := buildLogWhere(logWhereInput{
		Module:       options.Module,
		StartDate:    options.StartDate,
		EndDate:      options.EndDate,
		Search:       options.Search,
		InstanceID:   options.InstanceID,
		InstanceName: options.InstanceName,
	})

	rows, err := queryAuditRows(ctx, whereSQL, args, safeLimit, 0)
	if err != nil {
		return nil, err
	}

	items := make([]AuditLogRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, mapLogWithRisk(row))
	}
	return items, nil
}

// GetLogModules mirrors getLogModules().
func GetLogModules(ctx context.Context) ([]string, error) {
	var rows []struct {
		Module string `gorm:"column:module"`
	}
	if err := DB.WithContext(ctx).Raw("SELECT DISTINCT module FROM logs ORDER BY module ASC").Scan(&rows).Error; err != nil {
		return nil, err
	}

	modules := make([]string, 0, len(rows))
	for _, row := range rows {
		modules = append(modules, row.Module)
	}
	return modules, nil
}

// CleanOldLogs mirrors cleanOldLogs(); it deletes logs older than `days` days and
// returns the number of removed rows.
func CleanOldLogs(ctx context.Context, days int) (int64, error) {
	cutoffDate := time.Now().AddDate(0, 0, -days)

	tx := DB.WithContext(ctx).Exec("DELETE FROM logs WHERE created_at < ?", cutoffDate)
	if tx.Error != nil {
		return 0, tx.Error
	}
	return tx.RowsAffected, nil
}
