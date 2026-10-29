// Port of server/src/routes/logs.ts.
//
// Behavioural notes that are easy to get wrong (all verified against fastify
// 5.6.2 + the installed AJV 8 unless stated otherwise):
//
//   - Neither route declares a JSON schema, so the raw query values reach the
//     handler unchanged. Fastify's query parser collapses a single occurrence to
//     a string and leaves repeated occurrences as an array. The TypeScript then
//     calls `value?.trim()` / `PATTERN.test(value)`, which throws for arrays
//     (→ 500) and fails the pattern (→ 400 INVALID_ID) respectively; both are
//     reproduced below.
//   - `instanceId` uses `instanceId !== null` (i.e. "key present"), so a present
//     but malformed value — including an empty string — is a 400 INVALID_ID,
//     while an absent key falls back to no filter.
//   - `limit` in /audit/export is `limit ? Math.min(parsePositiveInteger(limit)
//     ?? 500, 1000) : 500`: a falsy or unparseable value silently becomes 500.
//   - Ordinary users are scoped to their own logs (`userId || undefined`);
//     admins get no user filter at all.
//   - The CSV download writes an audit log row before rendering, so its body is
//     not reproducible across two consecutive runs (conformance uses skipBody).
package routes

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const (
	// logsModuleFilterMaxLength mirrors LOG_MODULE_FILTER_MAX_LENGTH.
	logsModuleFilterMaxLength = 64
	// logsSearchFilterMaxLength mirrors LOG_SEARCH_FILTER_MAX_LENGTH.
	logsSearchFilterMaxLength = 128
	// logsInstanceNameFilterMaxLength mirrors LOG_INSTANCE_NAME_FILTER_MAX_LENGTH.
	logsInstanceNameFilterMaxLength = 128
	// logsAuditExportLimitMax mirrors AUDIT_EXPORT_LIMIT_MAX.
	logsAuditExportLimitMax = 1000
	// logsAuditExportDefaultLimit mirrors the 500 default in the route.
	logsAuditExportDefaultLimit = 500
	// logsDateFilterMaxLength mirrors the `normalizeLogFilter(startDate, 32)` calls.
	logsDateFilterMaxLength = 32
	// logsDefaultPage / logsDefaultPageSize mirror the query-string defaults.
	logsDefaultPage     = 1
	logsDefaultPageSize = 20
	// logsMaxPageSize mirrors the `parseClampedPositiveIntegerQuery(pageSize, 20, 100)` cap.
	logsMaxPageSize = 100
	// logsMaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER.
	logsMaxSafeInteger = 1<<53 - 1
)

// logsUserModules mirrors the module list a regular user is allowed to see.
var logsUserModules = []string{
	"security",
	"instance",
	"snapshot",
	"backup",
	"personal",
	"ssh_key",
	"notification",
}

// logsQueryValue extracts one query parameter, mirroring Fastify's parser:
// present=false when the key is absent, isArray=true when it was repeated.
func logsQueryValue(c *gin.Context, name string) (value string, present bool, isArray bool) {
	values, present := c.Request.URL.Query()[name]
	if !present {
		return "", false, false
	}
	switch len(values) {
	case 0:
		return "", true, false
	case 1:
		return values[0], true, false
	}
	return "", true, true
}

// logsParsePositiveInteger mirrors parsePositiveInteger(): false for an empty
// value, for anything outside ^[1-9]\d*$ and for values above MAX_SAFE_INTEGER.
func logsParsePositiveInteger(value string) (int64, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 0, false
	}
	if parsed > logsMaxSafeInteger {
		return 0, false
	}
	return parsed, true
}

// logsParsePositiveIntegerQuery mirrors parsePositiveIntegerQuery().
func logsParsePositiveIntegerQuery(value string, fallback int) int {
	if parsed, ok := logsParsePositiveInteger(value); ok {
		return int(parsed)
	}
	return fallback
}

// logsParseClampedPositiveIntegerQuery mirrors
// parseClampedPositiveIntegerQuery().
func logsParseClampedPositiveIntegerQuery(value string, fallback, max int) int {
	parsed := logsParsePositiveIntegerQuery(value, fallback)
	if parsed > max {
		return max
	}
	return parsed
}

// logsSliceUTF16 mirrors String#slice(0, end), which counts UTF-16 code units.
func logsSliceUTF16(value string, end int) string {
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > end {
			return value[:index]
		}
		units += width
	}
	return value
}

// logsNormalizeFilter mirrors normalizeLogFilter(): trim, then slice to
// maxLength; an empty result is "not provided". isArray reports the duplicated
// query key, for which the TypeScript throws and Fastify answers 500.
func logsNormalizeFilter(value string, isArray bool, maxLength int) (*string, bool) {
	if isArray {
		return nil, false
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, true
	}
	trimmed = logsSliceUTF16(trimmed, maxLength)
	if trimmed == "" {
		return nil, true
	}
	return &trimmed, true
}

// logsCSVEscape mirrors csvEscape().
func logsCSVEscape(value string) string {
	if !strings.ContainsAny(value, "\",\n\r") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// logsBuildAuditCSV mirrors buildAuditCsv().
func logsBuildAuditCSV(rows []db.AuditLogRow) string {
	header := []string{
		"时间",
		"用户",
		"模块",
		"操作",
		"风险等级",
		"风险说明",
		"需要审批",
		"需要验证码",
		"批量敏感",
		"结果",
		"内容",
	}

	escapedHeader := make([]string, 0, len(header))
	for _, column := range header {
		escapedHeader = append(escapedHeader, logsCSVEscape(column))
	}
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, strings.Join(escapedHeader, ","))

	for _, row := range rows {
		username := "系统"
		if row.Username != nil && *row.Username != "" {
			username = *row.Username
		}
		fields := []string{
			row.CreatedAt,
			username,
			row.Module,
			row.Action,
			row.RiskLevel,
			row.RiskTitle,
			logsYesNo(row.ApprovalRequired),
			logsYesNo(row.VerificationRequired),
			logsYesNo(row.BatchSensitive),
			row.Result,
			row.Content,
		}
		escaped := make([]string, 0, len(fields))
		for _, field := range fields {
			escaped = append(escaped, logsCSVEscape(field))
		}
		lines = append(lines, strings.Join(escaped, ","))
	}

	return strings.Join(lines, "\n")
}

// logsYesNo mirrors the `flag ? '是' : '否'` ternaries.
func logsYesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// logsRespondInternalError mirrors the generic 500 of app.ts' setErrorHandler for
// a production build (the only build that is deployed).
func logsRespondInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// logsListHandler mirrors `fastify.get('/', { onRequest: [fastify.authenticate] })`.
func logsListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)

	rawInstanceID, instanceIDPresent, instanceIDIsArray := logsQueryValue(c, "instanceId")
	var instanceID *int64
	if instanceIDPresent {
		if instanceIDIsArray {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		parsed, ok := logsParsePositiveInteger(rawInstanceID)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		instanceID = &parsed
	}

	rawPage, pagePresent, pageIsArray := logsQueryValue(c, "page")
	rawPageSize, pageSizePresent, pageSizeIsArray := logsQueryValue(c, "pageSize")
	rawModule, modulePresent, moduleIsArray := logsQueryValue(c, "module")
	rawSearch, searchPresent, searchIsArray := logsQueryValue(c, "search")
	rawInstanceName, instanceNamePresent, instanceNameIsArray := logsQueryValue(c, "instanceName")

	// The object literal is evaluated left to right in the TypeScript, so a
	// duplicated `module`/`search`/`instanceName` key 500s before the DB is hit.
	module, ok := logsNormalizeFilter(rawModule, modulePresent && moduleIsArray, logsModuleFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}

	page := logsDefaultPage
	if pagePresent && !pageIsArray {
		page = logsParsePositiveIntegerQuery(rawPage, logsDefaultPage)
	}

	pageSize := logsDefaultPageSize
	if pageSizePresent && !pageSizeIsArray {
		pageSize = logsParseClampedPositiveIntegerQuery(rawPageSize, logsDefaultPageSize, logsMaxPageSize)
	}

	search, ok := logsNormalizeFilter(rawSearch, searchPresent && searchIsArray, logsSearchFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}

	instanceName, ok := logsNormalizeFilter(rawInstanceName, instanceNamePresent && instanceNameIsArray, logsInstanceNameFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}

	var userID *int64
	if user != nil && user.Role != "admin" {
		id := int64(user.ID)
		userID = &id
	}

	result, err := db.GetLogsPaginated(c.Request.Context(), db.LogPaginatedOptions{
		Page:         page,
		PageSize:     pageSize,
		Module:       module,
		UserID:       userID,
		Search:       search,
		InstanceID:   instanceID,
		InstanceName: instanceName,
	})
	if err != nil {
		logsRespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"logs":       result.Items,
		"total":      result.Total,
		"page":       result.Page,
		"pageSize":   result.PageSize,
		"totalPages": result.TotalPages,
	})
}

// logsAuditExportHandler mirrors `fastify.get('/audit/export', { onRequest:
// [fastify.authenticateAdmin] })`.
func logsAuditExportHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	rawInstanceID, instanceIDPresent, instanceIDIsArray := logsQueryValue(c, "instanceId")
	var instanceID *int64
	if instanceIDPresent {
		if instanceIDIsArray {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		parsed, ok := logsParsePositiveInteger(rawInstanceID)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		instanceID = &parsed
	}

	rawLimit, limitPresent, limitIsArray := logsQueryValue(c, "limit")
	exportLimit := logsAuditExportDefaultLimit
	if limitPresent && !limitIsArray && rawLimit != "" {
		parsed, ok := logsParsePositiveInteger(rawLimit)
		if !ok {
			parsed = logsAuditExportDefaultLimit
		}
		exportLimit = int(parsed)
		if exportLimit > logsAuditExportLimitMax {
			exportLimit = logsAuditExportLimitMax
		}
	}

	rawModule, modulePresent, moduleIsArray := logsQueryValue(c, "module")
	rawSearch, searchPresent, searchIsArray := logsQueryValue(c, "search")
	rawInstanceName, instanceNamePresent, instanceNameIsArray := logsQueryValue(c, "instanceName")
	rawStartDate, startDatePresent, startDateIsArray := logsQueryValue(c, "startDate")
	rawEndDate, endDatePresent, endDateIsArray := logsQueryValue(c, "endDate")

	module, ok := logsNormalizeFilter(rawModule, modulePresent && moduleIsArray, logsModuleFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}
	search, ok := logsNormalizeFilter(rawSearch, searchPresent && searchIsArray, logsSearchFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}
	instanceName, ok := logsNormalizeFilter(rawInstanceName, instanceNamePresent && instanceNameIsArray, logsInstanceNameFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}
	startDate, ok := logsNormalizeFilter(rawStartDate, startDatePresent && startDateIsArray, logsDateFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}
	endDate, ok := logsNormalizeFilter(rawEndDate, endDatePresent && endDateIsArray, logsDateFilterMaxLength)
	if !ok {
		logsRespondInternalError(c)
		return
	}

	rows, err := db.GetLogsForAuditExport(ctx, db.LogAuditExportOptions{
		Module:       module,
		Search:       search,
		InstanceID:   instanceID,
		InstanceName: instanceName,
		StartDate:    startDate,
		EndDate:      endDate,
		Limit:        exportLimit,
	})
	if err != nil {
		logsRespondInternalError(c)
		return
	}

	// `module || '全部'` uses the raw query value, not the normalized one.
	moduleLabel := "全部"
	if rawModule != "" && !moduleIsArray {
		moduleLabel = rawModule
	}
	// The audit row is written *before* the response; note the loop above already
	// consumed the rows, so this log never appears in its own export.
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSystem, "audit.export",
		"导出审计日志 "+formatTerminalCommandNumber(float64(len(rows)))+" 条 [module="+moduleLabel+
			", limit="+formatTerminalCommandNumber(float64(exportLimit))+"]",
		db.LogResultSuccess, db.CreateLogOptions{})

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="payincus-audit-logs-`+
		formatTerminalCommandNumber(float64(time.Now().UnixMilli()))+`.csv"`)
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, "\uFEFF"+logsBuildAuditCSV(rows))
}

// RegisterLogs mounts /api/logs. The two admin routes use authenticateAdmin,
// the remaining two use authenticate.
func RegisterLogs(r *gin.Engine) {
	group := r.Group("/api/logs")

	// GET /risk-definitions — the risk catalogue used by the audit UI.
	group.GET("/risk-definitions", middleware.AuthenticateAdmin(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"definitions": lib.GetRiskOperationDefinitions(),
		})
	})

	// GET /audit/export — CSV download of the (redacted) audit trail.
	group.GET("/audit/export", middleware.AuthenticateAdmin(), logsAuditExportHandler)

	// GET / — paginated logs. Fastify registers both "/api/logs" and "/api/logs/"
	// for a plugin root, so both spellings are mounted.
	authenticated := middleware.Authenticate()
	group.GET("", authenticated, logsListHandler)
	group.GET("/", authenticated, logsListHandler)

	// GET /modules — available module names for the filter dropdown.
	group.GET("/modules", authenticated, func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		if user != nil && user.Role == "admin" {
			modules, err := db.GetLogModules(c.Request.Context())
			if err != nil {
				logsRespondInternalError(c)
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "modules": modules})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "modules": logsUserModules})
	})
}

func init() {
	Register(RegisterLogs)
}
