// Port of server/src/routes/system-update.ts (prefix /api/admin/system-update).
//
// OTA IS THE HIGHEST-RISK PATH IN THIS REPOSITORY (AGENTS.md §2.1). The Node
// backend is still production and owns the OTA machinery: it spawns the updater
// and rollback worker scripts (or starts a systemd template unit), which download
// release artifacts, rewrite the `current` symlink and restart services. While the
// Go backend runs beside it, starting an update from Go would race Node for the
// same install directory.
//
// The module therefore splits into two deliberately different halves:
//
//	READ-ONLY, FULLY IMPLEMENTED
//	  GET  /version              version metadata (version.json + local git)
//	  GET  /check                update check through an injectable checker seam
//	  GET  /tasks                newest 30 tasks, with starter username
//	  GET  /tasks/:id            one task or 400/404
//	  GET  /tasks/:id/logs       bounded log tail (last 200k UTF-16 units)
//
//	VALIDATED, THEN EXPLICITLY REFUSED
//	  POST /start                501 OTA_DISABLED_ON_GO_BACKEND
//	  POST /tasks/:id/rollback   501 OTA_DISABLED_ON_GO_BACKEND
//
// The two write routes run the same authentication, super-admin allowlist,
// schema/route-id validation and existence checks as Node, and then stop: they
// never touch the database, never write a log row, never exec a script and never
// signal a process. The 501 is intentional and temporary — after the traffic
// switch the owner decides whether OTA is wired into Go (see docs/WAVE_D_SPEC.md).
// TestSystemUpdateStartRefusesWithoutSideEffects and
// TestSystemUpdateRollbackRefusesWithoutSideEffects pin that contract, and
// TestSystemUpdateSourceHasNoOTAExecutionSurface fails if a worker-script or
// service-manager invocation ever appears in this module.
//
// `GET /check` in Node performs `git fetch --tags` (mutating the working tree's
// refs) and calls api.github.com. Both are outbound side effects that the Go
// backend must not perform by default, so the default checker is a network-free
// "disabled" implementation. The real GitHub checker is ported behind the
// systemUpdateHTTPDoer seam and is exercised only with a fake client in tests.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const (
	// systemUpdatePrefix mirrors `{ prefix: '/api/admin/system-update' }`.
	systemUpdatePrefix = "/api/admin/system-update"
	// systemUpdateTaskListLimit mirrors listSystemUpdateTasks(30).
	systemUpdateTaskListLimit = 30
	// systemUpdateLogTailLimit mirrors `logs.slice(-200000)`.
	systemUpdateLogTailLimit = 200000
	// systemUpdateGitTimeout mirrors the 30_000 ms execFile timeout.
	systemUpdateGitTimeout = 30 * time.Second
	// systemUpdateDisabledCode is the machine-readable refusal. It is NOT a Node
	// code: the Node backend has no equivalent response.
	systemUpdateDisabledCode = "OTA_DISABLED_ON_GO_BACKEND"
	// systemUpdateMaxInt32 is the Prisma Int ceiling. Node's parsePositiveId accepts
	// any safe integer, then Prisma fails on the 32-bit column and the request
	// answers 500; the handler mirrors that status instead of truncating the id.
	systemUpdateMaxInt32 = math.MaxInt32
)

// systemUpdateDisabledMessage is the human-readable refusal.
const systemUpdateDisabledMessage = "OTA updates are disabled on the Go backend while the Node backend still serves them; no update or rollback was started"

// systemUpdateDisabledCheckReason explains the network-free default of /check.
const systemUpdateDisabledCheckReason = "Go 后端默认不执行在线版本检查（不访问 api.github.com，也不执行 git fetch）；OTA 仍由 Node 后端负责，切流后由 owner 决定是否接线"

// systemUpdateGitHubUnavailableMessage is the literal from
// checkForUpdatesWithoutGit() when the Releases API cannot be reached.
const systemUpdateGitHubUnavailableMessage = "当前部署目录不是 Git 工作区，且无法访问 GitHub Releases（网络不通或仓库不可达），暂时无法在线更新。请检查服务器到 api.github.com 的网络后重试。"

// systemUpdateReleaseTagPattern mirrors
// /^v\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/ from lib/system-version.ts.
var systemUpdateReleaseTagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// systemUpdateStartBodyFields mirrors the /start body schema
// `{ targetVersion: { type: 'string', minLength: 5, maxLength: 64 } }`.
var systemUpdateStartBodyFields = []ajvFieldSpec{
	{Name: "targetVersion", Kind: ajvKindString, MinLength: 5, MaxLength: 64},
}

// ==================== seams ====================

// systemUpdateTaskStore is the read-only DB surface this module needs. It has no
// create/update method on purpose: the Go backend must not be able to record an
// OTA task, so the refusal seam cannot accidentally write one.
type systemUpdateTaskStore interface {
	List(ctx context.Context, limit int) ([]db.SystemUpdateTaskWithUser, error)
	Get(ctx context.Context, id int32) (*db.SystemUpdateTaskWithUser, error)
}

// systemUpdateDBStore is the production store.
type systemUpdateDBStore struct{}

func (systemUpdateDBStore) List(ctx context.Context, limit int) ([]db.SystemUpdateTaskWithUser, error) {
	return db.ListSystemUpdateTasks(ctx, limit)
}

func (systemUpdateDBStore) Get(ctx context.Context, id int32) (*db.SystemUpdateTaskWithUser, error) {
	return db.GetSystemUpdateTask(ctx, id)
}

// systemUpdateTasks is replaced by tests with an in-memory store.
var systemUpdateTasks systemUpdateTaskStore = systemUpdateDBStore{}

// systemUpdateAdminAllowlist resolves the OTA super-admin allowlist. It is a seam
// so authorization can be tested without a database; the default is the exact
// runtime-settings lookup Node performs.
var systemUpdateAdminAllowlist = func(ctx context.Context) (lib.CombinedAdminIDAllowlist, error) {
	return lib.GetCombinedAdminIDAllowlist(
		ctx,
		"system_update_allowed_admin_ids",
		[]string{"SYSTEM_UPDATE_ALLOWED_ADMIN_IDS"},
	)
}

// systemUpdateHTTPDoer is the HTTP seam for the (disabled by default) GitHub
// checker. *http.Client satisfies it.
type systemUpdateHTTPDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

// systemUpdateCheckRunner mirrors checkForUpdates(root).
type systemUpdateCheckRunner interface {
	Check(ctx context.Context, root string, current systemUpdateVersionMetadata) (systemUpdateCheckResult, error)
}

// systemUpdateDisabledChecker is the production default: no network, no git
// mutation, and an explicit "repository unavailable" result instead of a
// misleading "no updates available".
type systemUpdateDisabledChecker struct{}

func (systemUpdateDisabledChecker) Check(_ context.Context, _ string, current systemUpdateVersionMetadata) (systemUpdateCheckResult, error) {
	reason := systemUpdateDisabledCheckReason
	return systemUpdateCheckResult{
		Current:             current,
		Latest:              nil,
		Updates:             []systemUpdateAvailableUpdate{},
		UpdateAvailable:     false,
		RepositoryAvailable: false,
		RepositoryError:     &reason,
	}, nil
}

// systemUpdateChecker is the /check seam.
var systemUpdateChecker systemUpdateCheckRunner = systemUpdateDisabledChecker{}

// ==================== wire shapes ====================

// systemUpdateVersionMetadata mirrors VersionMetadata.
type systemUpdateVersionMetadata struct {
	Version    string   `json:"version"`
	GitTag     *string  `json:"gitTag"`
	GitCommit  *string  `json:"gitCommit"`
	BuildTime  *string  `json:"buildTime"`
	DeployedAt *string  `json:"deployedAt"`
	Changelog  []string `json:"changelog"`
}

// systemUpdateOtaArtifact mirrors OtaArtifactInfo.
type systemUpdateOtaArtifact struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	URL      string `json:"url"`
	Sha256   string `json:"sha256"`
	Size     *int64 `json:"size"`
}

// systemUpdateOtaRelease mirrors OtaReleaseInfo.
type systemUpdateOtaRelease struct {
	ManifestAvailable bool                      `json:"manifestAvailable"`
	ManifestURL       *string                   `json:"manifestUrl"`
	Artifacts         []systemUpdateOtaArtifact `json:"artifacts"`
	Error             *string                   `json:"error"`
}

// systemUpdateAvailableUpdate mirrors AvailableUpdate.
type systemUpdateAvailableUpdate struct {
	Version   string                 `json:"version"`
	Commit    *string                `json:"commit"`
	Date      *string                `json:"date"`
	Changelog []string               `json:"changelog"`
	OTA       systemUpdateOtaRelease `json:"ota"`
}

// systemUpdateCheckResult mirrors UpdateCheckResult.
type systemUpdateCheckResult struct {
	Current             systemUpdateVersionMetadata   `json:"current"`
	Latest              *systemUpdateAvailableUpdate  `json:"latest"`
	Updates             []systemUpdateAvailableUpdate `json:"updates"`
	UpdateAvailable     bool                          `json:"updateAvailable"`
	RepositoryAvailable bool                          `json:"repositoryAvailable"`
	RepositoryError     *string                       `json:"repositoryError"`
}

// systemUpdateCheckResponse is `{ ...result, canManageUpdates }`.
type systemUpdateCheckResponse struct {
	Current             systemUpdateVersionMetadata   `json:"current"`
	Latest              *systemUpdateAvailableUpdate  `json:"latest"`
	Updates             []systemUpdateAvailableUpdate `json:"updates"`
	UpdateAvailable     bool                          `json:"updateAvailable"`
	RepositoryAvailable bool                          `json:"repositoryAvailable"`
	RepositoryError     *string                       `json:"repositoryError"`
	CanManageUpdates    bool                          `json:"canManageUpdates"`
}

// systemUpdateTaskView mirrors SerializedSystemUpdateTask (Prisma camelCase names
// are the wire format because Node returns the serialized row verbatim).
type systemUpdateTaskView struct {
	ID                int32   `json:"id"`
	TargetVersion     string  `json:"targetVersion"`
	FromVersion       *string `json:"fromVersion"`
	Status            string  `json:"status"`
	StartedByUserID   int32   `json:"startedByUserId"`
	StartedByUsername *string `json:"startedByUsername"`
	BackupPath        *string `json:"backupPath"`
	LogPath           *string `json:"logPath"`
	ErrorMessage      *string `json:"errorMessage"`
	StartedAt         *string `json:"startedAt"`
	FinishedAt        *string `json:"finishedAt"`
	CreatedAt         string  `json:"createdAt"`
	UpdatedAt         string  `json:"updatedAt"`
}

// ==================== helpers ====================

// systemUpdatePtr returns a pointer to value.
func systemUpdatePtr(value string) *string {
	return &value
}

// systemUpdateISOTime mirrors Date#toISOString() for a database timestamp.
func systemUpdateISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// systemUpdateISONullableTime mirrors `value?.toISOString() || null`.
func systemUpdateISONullableTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := systemUpdateISOTime(*value)
	return &formatted
}

// systemUpdateTaskViewFromRow mirrors serializeSystemUpdateTask().
func systemUpdateTaskViewFromRow(row db.SystemUpdateTaskWithUser) systemUpdateTaskView {
	return systemUpdateTaskView{
		ID:                row.ID,
		TargetVersion:     row.TargetVersion,
		FromVersion:       row.FromVersion,
		Status:            string(row.Status),
		StartedByUserID:   row.StartedByUserID,
		StartedByUsername: row.StartedByUsername,
		BackupPath:        row.BackupPath,
		LogPath:           row.LogPath,
		ErrorMessage:      row.ErrorMessage,
		StartedAt:         systemUpdateISONullableTime(row.StartedAt),
		FinishedAt:        systemUpdateISONullableTime(row.FinishedAt),
		CreatedAt:         systemUpdateISOTime(row.CreatedAt),
		UpdatedAt:         systemUpdateISOTime(row.UpdatedAt),
	}
}

// systemUpdateProjectRoot mirrors getProjectRoot():
// resolve(process.env.INCUDAL_APP_DIR || process.cwd()).
func systemUpdateProjectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	raw := os.Getenv("INCUDAL_APP_DIR")
	if raw == "" {
		raw = cwd
	}
	resolved, err := filepath.Abs(raw)
	if err != nil {
		return filepath.Clean(raw)
	}
	return resolved
}

// systemUpdateLogDir mirrors getLogDir(root):
// resolve(process.env.SYSTEM_UPDATE_LOG_DIR || join(root, 'update-logs')).
func systemUpdateLogDir(root string) string {
	raw := os.Getenv("SYSTEM_UPDATE_LOG_DIR")
	if raw == "" {
		raw = filepath.Join(root, "update-logs")
	}
	resolved, err := filepath.Abs(raw)
	if err != nil {
		return filepath.Clean(raw)
	}
	return resolved
}

// systemUpdateRunGit mirrors runGit(): stdout trimmed, or not-ok on any failure
// (spawn error, timeout, non-zero exit). Errors are swallowed exactly like the
// TypeScript helper, so a deployment without git degrades to file metadata.
func systemUpdateRunGit(ctx context.Context, root string, args ...string) (string, bool) {
	commandContext, cancel := context.WithTimeout(ctx, systemUpdateGitTimeout)
	defer cancel()

	command := exec.CommandContext(commandContext, "git", args...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(output)), true
}

// systemUpdateReadJSONFile mirrors readJsonFile(): nil/false on any read or parse
// failure.
func systemUpdateReadJSONFile(path string, target any) bool {
	payload, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return false
	}
	return true
}

// systemUpdateStringEntry mirrors the `x || fallback` chain for string values
// coming from version.json, where a non-string is ignored rather than coerced.
func systemUpdateStringEntry(entries map[string]any, key string) string {
	value, ok := entries[key].(string)
	if !ok {
		return ""
	}
	return value
}

// systemUpdateFirstNonEmpty returns the first non-empty value.
func systemUpdateFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// systemUpdateStripListMarker mirrors `line.replace(/^[-*]\s+/, ”)`.
func systemUpdateStripListMarker(line string) string {
	if line == "" || (line[0] != '-' && line[0] != '*') {
		return line
	}
	rest := line[1:]
	trimmed := strings.TrimLeftFunc(rest, unicode.IsSpace)
	if len(trimmed) == len(rest) {
		// No whitespace after the marker: the regex would not match.
		return line
	}
	return trimmed
}

// systemUpdateJSString mirrors String(value) for the JSON scalar types a
// changelog entry can hold. Objects/arrays are skipped (they would stringify to
// "[object Object]"/"…"); a changelog is a list of plain lines in practice.
func systemUpdateJSString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return formatTerminalCommandNumber(typed)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "null"
	default:
		return ""
	}
}

// systemUpdateNormalizeChangelog mirrors normalizeChangelog().
func systemUpdateNormalizeChangelog(input any) []string {
	out := make([]string, 0, 4)

	switch typed := input.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(systemUpdateJSString(item)); text != "" {
				out = append(out, text)
			}
			if len(out) >= 50 {
				break
			}
		}
	case string:
		for _, line := range strings.Split(typed, "\n") {
			line = strings.TrimSuffix(line, "\r")
			line = systemUpdateStripListMarker(line)
			if text := strings.TrimSpace(line); text != "" {
				out = append(out, text)
			}
			if len(out) >= 50 {
				break
			}
		}
	}

	return out
}

// systemUpdatePackageVersion mirrors getPackageVersion().
func systemUpdatePackageVersion(root string) string {
	var pkg map[string]any
	if systemUpdateReadJSONFile(filepath.Join(root, "package.json"), &pkg) {
		if version := systemUpdateStringEntry(pkg, "version"); version != "" {
			return version
		}
	}
	return "0.0.0"
}

// systemUpdateTagChangelog mirrors getTagChangelog(tag, root).
func systemUpdateTagChangelog(ctx context.Context, root, tag string) []string {
	if message, ok := systemUpdateRunGit(ctx, root, "tag", "-l", tag, "--format=%(contents)"); ok {
		if normalized := systemUpdateNormalizeChangelog(message); len(normalized) > 0 {
			return normalized
		}
	}
	if log, ok := systemUpdateRunGit(ctx, root, "log", "-1", "--pretty=%s", tag); ok {
		return systemUpdateNormalizeChangelog(log)
	}
	return []string{}
}

// systemUpdateCurrentVersionMetadata mirrors getCurrentVersionMetadata(root).
func systemUpdateCurrentVersionMetadata(ctx context.Context, root string) systemUpdateVersionMetadata {
	var versionFile map[string]any
	hasVersionFile := systemUpdateReadJSONFile(filepath.Join(root, "version.json"), &versionFile)

	gitTag, hasExactTag := systemUpdateRunGit(ctx, root, "describe", "--tags", "--exact-match", "HEAD")
	if !hasExactTag {
		gitTag = ""
	}
	nearestTag, hasNearestTag := systemUpdateRunGit(ctx, root, "describe", "--tags", "--abbrev=0")
	if !hasNearestTag {
		nearestTag = ""
	}
	gitCommit, hasCommit := systemUpdateRunGit(ctx, root, "rev-parse", "--short=12", "HEAD")
	if !hasCommit {
		gitCommit = ""
	}
	packageVersion := systemUpdatePackageVersion(root)

	fileVersion := ""
	fileGitTag := ""
	fileGitCommit := ""
	fileBuildTime := ""
	fileDeployedAt := ""
	var fileChangelog any
	if hasVersionFile {
		fileVersion = systemUpdateStringEntry(versionFile, "version")
		fileGitTag = systemUpdateStringEntry(versionFile, "gitTag")
		fileGitCommit = systemUpdateStringEntry(versionFile, "gitCommit")
		fileBuildTime = systemUpdateStringEntry(versionFile, "buildTime")
		fileDeployedAt = systemUpdateStringEntry(versionFile, "deployedAt")
		fileChangelog = versionFile["changelog"]
	}

	version := systemUpdateFirstNonEmpty(fileVersion, gitTag, nearestTag, "v"+packageVersion)

	metadata := systemUpdateVersionMetadata{
		Version:   version,
		Changelog: []string{},
	}
	if resolvedTag := systemUpdateFirstNonEmpty(fileGitTag, gitTag, nearestTag); resolvedTag != "" {
		metadata.GitTag = systemUpdatePtr(resolvedTag)
	}
	if resolvedCommit := systemUpdateFirstNonEmpty(fileGitCommit, gitCommit); resolvedCommit != "" {
		metadata.GitCommit = systemUpdatePtr(resolvedCommit)
	}
	if fileBuildTime != "" {
		metadata.BuildTime = systemUpdatePtr(fileBuildTime)
	}
	if fileDeployedAt != "" {
		metadata.DeployedAt = systemUpdatePtr(fileDeployedAt)
	}

	if changelog := systemUpdateNormalizeChangelog(fileChangelog); len(changelog) > 0 {
		metadata.Changelog = changelog
	} else if nearestTag != "" {
		metadata.Changelog = systemUpdateTagChangelog(ctx, root, nearestTag)
	}

	return metadata
}

// systemUpdateIsValidReleaseTag mirrors isValidReleaseTag().
func systemUpdateIsValidReleaseTag(value string) bool {
	return systemUpdateReleaseTagPattern.MatchString(strings.TrimSpace(value))
}

// systemUpdateParsePositiveID mirrors parsePositiveId():
// Number(value), then Number.isSafeInteger(id) && id > 0.
//
// The value is returned as float64 because that is what JavaScript's Number is;
// the caller converts to int32 exactly where Prisma's Int column does.
func systemUpdateParsePositiveID(raw string) (float64, bool) {
	number, ok := systemUpdateJSNumber(raw)
	if !ok {
		return 0, false
	}
	if number != math.Trunc(number) || math.IsInf(number, 0) || math.IsNaN(number) {
		return 0, false
	}
	if math.Abs(number) > float64(1<<53-1) {
		return 0, false
	}
	if number <= 0 {
		return 0, false
	}
	return number, true
}

// systemUpdateJSNumber mirrors Number(string) for the inputs a route parameter
// can carry. A strict regex runs first because Go's ParseFloat is more permissive
// than JavaScript: it accepts "inf", "Infinity" in any case and underscores in
// decimal literals, all of which Number() maps to NaN.
func systemUpdateJSNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		// Number('') === 0.
		return 0, true
	}

	switch trimmed {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}

	if len(trimmed) > 2 && trimmed[0] == '0' {
		switch trimmed[1] {
		case 'x', 'X':
			if parsed, ok := systemUpdateParseUint(trimmed[2:], 16); ok {
				return parsed, true
			}
			return 0, false
		case 'b', 'B':
			if parsed, ok := systemUpdateParseUint(trimmed[2:], 2); ok {
				return parsed, true
			}
			return 0, false
		case 'o', 'O':
			if parsed, ok := systemUpdateParseUint(trimmed[2:], 8); ok {
				return parsed, true
			}
			return 0, false
		}
	}

	if !systemUpdateJSDecimalPattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		if errors, ok := err.(*strconv.NumError); ok && errors.Err == strconv.ErrRange {
			// 1e999 becomes Infinity in JavaScript.
			return parsed, true
		}
		return 0, false
	}
	return parsed, true
}

// systemUpdateJSDecimalPattern is JavaScript's decimal Number() grammar:
// [+-]?(digits[.digits?] | .digits)([eE][+-]?digits)?.
var systemUpdateJSDecimalPattern = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// systemUpdateParseUint parses a JS 0x/0b/0o literal.
func systemUpdateParseUint(digits string, base int) (float64, bool) {
	if digits == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return float64(parsed), true
}

// systemUpdateLogTail mirrors `text.slice(-limit)`.
//
// JavaScript counts UTF-16 code units; Go strings are UTF-8. The suffix is
// computed in UTF-16 units so ASCII and BMP logs are byte-for-byte identical to
// Node. A slice that would split a surrogate pair keeps the whole code point
// instead of emitting a lone surrogate, which cannot be represented in UTF-8.
func systemUpdateLogTail(text string, limit int) string {
	units := 0
	index := len(text)
	for index > 0 {
		runeValue, size := utf8.DecodeLastRuneInString(text[:index])
		cost := 1
		if runeValue > 0xFFFF {
			cost = 2
		}
		if units+cost > limit {
			break
		}
		units += cost
		index -= size
	}
	return text[index:]
}

// ==================== authorization ====================

// systemUpdateCanManage mirrors canManageSystemUpdates(): role admin is already
// guaranteed by authenticateAdmin; the allowlist, when configured, is the
// authority, otherwise the literal username "admin" is.
func systemUpdateCanManage(ctx context.Context, user *lib.AccessTokenClaims) (bool, error) {
	if user == nil || user.Role != "admin" {
		return false, nil
	}
	allowlist, err := systemUpdateAdminAllowlist(ctx)
	if err != nil {
		return false, err
	}
	if len(allowlist.IDs) > 0 {
		return allowlist.Contains(int64(user.ID)), nil
	}
	return user.Username == "admin", nil
}

// systemUpdateRequireManager mirrors requireUpdateManager(): a denied attempt is
// audited and answered with 403 SUPER_ADMIN_REQUIRED.
func systemUpdateRequireManager(c *gin.Context) bool {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	allowed, err := systemUpdateCanManage(ctx, user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return false
	}
	if allowed {
		return true
	}

	userID := int64(user.ID)
	db.CreateLog(
		ctx,
		&userID,
		db.LogModuleSystem,
		"system.update.denied",
		"Denied system update access for "+user.Username,
		db.LogResultWarning,
		db.CreateLogOptions{},
	)
	c.JSON(http.StatusForbidden, gin.H{
		"error": "Super administrator privileges required",
		"code":  "SUPER_ADMIN_REQUIRED",
	})
	return false
}

// systemUpdateRefuseOTA is the owner-approved refusal seam for the two mutating
// routes. It performs no I/O and no state change: 501 + a stable machine code.
func systemUpdateRefuseOTA(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": systemUpdateDisabledMessage,
		"code":  systemUpdateDisabledCode,
	})
}

// ==================== routes ====================

// RegisterSystemUpdate mounts /api/admin/system-update. Every route uses
// authenticateAdmin (onRequest: [fastify.authenticateAdmin]), including the reads.
func RegisterSystemUpdate(r *gin.Engine) {
	group := r.Group(systemUpdatePrefix, middleware.AuthenticateAdmin())

	// GET /api/admin/system-update/version
	group.GET("/version", func(c *gin.Context) {
		root := systemUpdateProjectRoot()
		metadata := systemUpdateCurrentVersionMetadata(c.Request.Context(), root)
		c.JSON(http.StatusOK, gin.H{"version": metadata})
	})

	// GET /api/admin/system-update/check
	group.GET("/check", func(c *gin.Context) {
		ctx := c.Request.Context()
		root := systemUpdateProjectRoot()
		current := systemUpdateCurrentVersionMetadata(ctx, root)

		result, err := systemUpdateChecker.Check(ctx, root, current)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		canManage, err := systemUpdateCanManage(ctx, middleware.CurrentUser(c))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		c.JSON(http.StatusOK, systemUpdateCheckResponse{
			Current:             result.Current,
			Latest:              result.Latest,
			Updates:             result.Updates,
			UpdateAvailable:     result.UpdateAvailable,
			RepositoryAvailable: result.RepositoryAvailable,
			RepositoryError:     result.RepositoryError,
			CanManageUpdates:    canManage,
		})
	})

	// GET /api/admin/system-update/tasks
	group.GET("/tasks", func(c *gin.Context) {
		rows, err := systemUpdateTasks.List(c.Request.Context(), systemUpdateTaskListLimit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		tasks := make([]systemUpdateTaskView, 0, len(rows))
		for _, row := range rows {
			tasks = append(tasks, systemUpdateTaskViewFromRow(row))
		}
		c.JSON(http.StatusOK, gin.H{"tasks": tasks})
	})

	// GET /api/admin/system-update/tasks/:id
	systemUpdateGetTask := func(c *gin.Context) {
		task, ok := systemUpdateLoadTask(c)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"task": systemUpdateTaskViewFromRow(*task)})
	}
	group.GET("/tasks/:id", systemUpdateGetTask)

	// find-my-way (Fastify's router) matches a trailing slash at a parameter
	// position as an EMPTY parameter, so GET /tasks/ reaches the :id handler with
	// id="" and answers 400 INVALID_TASK_ID (measured against the Node backend).
	// Gin's tree does not, so the literal alias keeps that contract; it is a
	// router-behaviour alias, not an eighth endpoint.
	group.GET("/tasks/", systemUpdateGetTask)

	// GET /api/admin/system-update/tasks/:id/logs
	//
	// NOT an SSE stream: Node reads the log file and returns one bounded JSON
	// snapshot, so the connection closes with the response body (pinned by
	// TestSystemUpdateLogsEndpoint).
	group.GET("/tasks/:id/logs", func(c *gin.Context) {
		if !systemUpdateRequireManager(c) {
			return
		}

		task, ok := systemUpdateLoadTask(c)
		if !ok {
			return
		}
		if task.LogPath == nil || *task.LogPath == "" {
			c.JSON(http.StatusOK, gin.H{"logs": ""})
			return
		}

		root := systemUpdateProjectRoot()
		logDir := systemUpdateLogDir(root)
		logPath, err := filepath.Abs(*task.LogPath)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"logs": ""})
			return
		}
		// The log file must stay inside the update log directory: the column is
		// database data, and a relative/absolute path outside it must never be read.
		if logPath != logDir && !strings.HasPrefix(logPath, logDir+string(os.PathSeparator)) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Log path is outside update log directory",
				"code":  "LOG_PATH_FORBIDDEN",
			})
			return
		}

		payload, err := os.ReadFile(logPath)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"logs": ""})
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": systemUpdateLogTail(string(payload), systemUpdateLogTailLimit)})
	})

	// POST /api/admin/system-update/start
	//
	// Validation and authorization run exactly like Node; the action does not.
	// Fastify's order matters: onRequest auth → body parser → body schema (AJV) →
	// handler (where requireUpdateManager runs). An administrator outside the OTA
	// allowlist therefore gets 400 for a malformed body before the 403, exactly as
	// the Node route does.
	group.POST("/start", func(c *gin.Context) {
		body, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		validated, validationMessage := validateAJVObject(body.Value, []string{"targetVersion"}, systemUpdateStartBodyFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}
		if !systemUpdateRequireManager(c) {
			return
		}
		targetVersion, _ := validated.stringValue("targetVersion")
		if !systemUpdateIsValidReleaseTag(targetVersion) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Target version must be a release tag like v1.2.3",
				"code":  "INVALID_TARGET_VERSION",
			})
			return
		}

		// AGENTS.md §2.1: no OTA from the Go backend while Node owns production.
		// Node would call canApplyUpdate() (network), create the task row, write an
		// audit log and spawn the updater here. None of that happens: the request
		// stops at a deterministic 501.
		systemUpdateRefuseOTA(c)
	})

	// POST /api/admin/system-update/tasks/:id/rollback
	//
	// Same split: the route id and task existence are real checks, the rollback is
	// refused.
	group.POST("/tasks/:id/rollback", func(c *gin.Context) {
		if !systemUpdateRequireManager(c) {
			return
		}

		task, ok := systemUpdateLoadTask(c)
		if !ok {
			return
		}
		if task.BackupPath == nil || *task.BackupPath == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Task has no backup path",
				"code":  "BACKUP_MISSING",
			})
			return
		}

		systemUpdateRefuseOTA(c)
	})
}

// systemUpdateLoadTask parses :id and loads the row, writing the 400/404/500
// response itself when the route must stop.
func systemUpdateLoadTask(c *gin.Context) (*db.SystemUpdateTaskWithUser, bool) {
	number, ok := systemUpdateParsePositiveID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid task id",
			"code":  "INVALID_TASK_ID",
		})
		return nil, false
	}
	if number > systemUpdateMaxInt32 {
		// Prisma's Int is 32-bit; Node's Number() accepts the value and the query
		// layer fails, so the observable status is 500.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil, false
	}

	task, err := systemUpdateTasks.Get(c.Request.Context(), int32(number))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil, false
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Task not found",
			"code":  "TASK_NOT_FOUND",
		})
		return nil, false
	}
	return task, true
}

// ==================== GitHub update checker (disabled by default) ====================

// systemUpdateGitHubChecker mirrors checkForUpdatesWithoutGit(): version
// discovery through the GitHub Releases API and OTA manifest lookup, with all
// network I/O going through the injected doer.
//
// It intentionally does NOT run `git fetch --tags`, which the git-based Node path
// performs: that mutates local refs, an outbound side effect the Go backend must
// not have while the Node process owns the deployment.
type systemUpdateGitHubChecker struct {
	client systemUpdateHTTPDoer
}

// systemUpdateNewGitHubChecker builds the checker around an HTTP doer. Tests pass
// a fake; nothing in the default wiring constructs this type.
func systemUpdateNewGitHubChecker(client systemUpdateHTTPDoer) systemUpdateCheckRunner {
	return systemUpdateGitHubChecker{client: client}
}

// Check mirrors checkForUpdatesWithoutGit().
func (checker systemUpdateGitHubChecker) Check(ctx context.Context, root string, current systemUpdateVersionMetadata) (systemUpdateCheckResult, error) {
	_ = root // the Releases API path never shells out to git

	tags := systemUpdateListReleaseTagsFromGitHub(ctx, checker.client)
	if tags == nil {
		reason := systemUpdateGitHubUnavailableMessage
		return systemUpdateCheckResult{
			Current:             current,
			Latest:              nil,
			Updates:             []systemUpdateAvailableUpdate{},
			UpdateAvailable:     false,
			RepositoryAvailable: false,
			RepositoryError:     &reason,
		}, nil
	}

	currentTag := current.Version
	if current.GitTag != nil && *current.GitTag != "" {
		currentTag = *current.GitTag
	}

	build := func(tag string) systemUpdateAvailableUpdate {
		return systemUpdateAvailableUpdate{
			Version:   tag,
			Commit:    nil,
			Date:      nil,
			Changelog: []string{},
			OTA:       systemUpdateGetOtaReleaseInfo(ctx, checker.client, tag),
		}
	}

	var latest *systemUpdateAvailableUpdate
	if len(tags) > 0 {
		built := build(tags[0])
		latest = &built
	}

	updates := make([]systemUpdateAvailableUpdate, 0, len(tags))
	for _, tag := range tags {
		if tag == currentTag || tag == current.Version {
			break
		}
		if latest != nil && latest.Version == tag {
			updates = append(updates, *latest)
			continue
		}
		updates = append(updates, build(tag))
	}

	return systemUpdateCheckResult{
		Current:             current,
		Latest:              latest,
		Updates:             updates,
		UpdateAvailable:     len(updates) > 0,
		RepositoryAvailable: true,
		RepositoryError:     nil,
	}, nil
}

// systemUpdateReleaseRepository mirrors getReleaseRepository().
func systemUpdateReleaseRepository() string {
	if value := os.Getenv("SYSTEM_UPDATE_RELEASE_REPOSITORY"); value != "" {
		return value
	}
	if value := os.Getenv("GITHUB_REPO"); value != "" {
		return value
	}
	return "Rain-kl/payincus"
}

// systemUpdateReleaseToken mirrors getReleaseToken().
func systemUpdateReleaseToken() string {
	if value := os.Getenv("SYSTEM_UPDATE_RELEASE_TOKEN"); value != "" {
		return value
	}
	return os.Getenv("GITHUB_TOKEN")
}

// systemUpdateIsTrustedReleaseHost mirrors isTrustedReleaseHost(): the token is
// only ever attached to GitHub-owned hosts.
func systemUpdateIsTrustedReleaseHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "github.com" ||
		host == "api.github.com" ||
		host == "codeload.github.com" ||
		host == "objects.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// systemUpdateFetchJSON mirrors fetchJson().
func systemUpdateFetchJSON(ctx context.Context, client systemUpdateHTTPDoer, rawURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "payincus-online-update")
	if token := systemUpdateReleaseToken(); token != "" && systemUpdateIsTrustedReleaseHost(rawURL) {
		request.Header.Set("authorization", "Bearer "+token)
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

// systemUpdateNormalizeOtaArtifact mirrors normalizeOtaArtifact().
func systemUpdateNormalizeOtaArtifact(input any) *systemUpdateOtaArtifact {
	value, ok := input.(map[string]any)
	if !ok {
		return nil
	}

	text := func(key string) string {
		raw, ok := value[key].(string)
		if !ok {
			return ""
		}
		return strings.TrimSpace(raw)
	}
	name := text("name")
	platform := text("platform")
	arch := text("arch")
	artifactURL := text("url")
	sha256 := strings.ToLower(text("sha256"))

	var size *int64
	if number, ok := value["size"].(float64); ok {
		if number >= 0 && number == math.Trunc(number) && math.Abs(number) <= float64(1<<53-1) {
			converted := int64(number)
			size = &converted
		}
	}

	if name == "" || platform == "" || arch == "" || artifactURL == "" {
		return nil
	}
	if !systemUpdateSha256Pattern.MatchString(sha256) {
		return nil
	}
	return &systemUpdateOtaArtifact{
		Name:     name,
		Platform: platform,
		Arch:     arch,
		URL:      artifactURL,
		Sha256:   sha256,
		Size:     size,
	}
}

// systemUpdateSha256Pattern mirrors /^[a-f0-9]{64}$/.
var systemUpdateSha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// systemUpdateGetOtaReleaseInfo mirrors getOtaReleaseInfo(tag).
func systemUpdateGetOtaReleaseInfo(ctx context.Context, client systemUpdateHTTPDoer, tag string) systemUpdateOtaRelease {
	repository := systemUpdateReleaseRepository()
	releaseAPIURL := "https://api.github.com/repos/" + repository + "/releases/tags/" + url.PathEscape(tag)

	empty := func(reason string, manifestURL *string) systemUpdateOtaRelease {
		return systemUpdateOtaRelease{
			ManifestAvailable: false,
			ManifestURL:       manifestURL,
			Artifacts:         []systemUpdateOtaArtifact{},
			Error:             &reason,
		}
	}

	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := systemUpdateFetchJSON(ctx, client, releaseAPIURL, &release); err != nil {
		return empty("读取 GitHub Release OTA manifest 失败: "+err.Error(), nil)
	}

	manifestURL := ""
	for _, asset := range release.Assets {
		if asset.Name == "ota-manifest.json" || asset.Name == "incudal-"+tag+"-ota-manifest.json" {
			manifestURL = asset.BrowserDownloadURL
			break
		}
	}
	if manifestURL == "" {
		return empty("GitHub Release 未提供 OTA manifest", nil)
	}

	var manifest struct {
		Artifacts []any `json:"artifacts"`
	}
	if err := systemUpdateFetchJSON(ctx, client, manifestURL, &manifest); err != nil {
		return empty("读取 GitHub Release OTA manifest 失败: "+err.Error(), nil)
	}

	artifacts := make([]systemUpdateOtaArtifact, 0, len(manifest.Artifacts))
	for _, raw := range manifest.Artifacts {
		if artifact := systemUpdateNormalizeOtaArtifact(raw); artifact != nil {
			artifacts = append(artifacts, *artifact)
		}
	}

	if len(artifacts) == 0 {
		return empty("OTA manifest 中没有有效 artifact", systemUpdatePtr(manifestURL))
	}
	return systemUpdateOtaRelease{
		ManifestAvailable: true,
		ManifestURL:       systemUpdatePtr(manifestURL),
		Artifacts:         artifacts,
		Error:             nil,
	}
}

// systemUpdateListReleaseTagsFromGitHub mirrors listReleaseTagsFromGitHub(): nil
// means "could not read the Releases API".
func systemUpdateListReleaseTagsFromGitHub(ctx context.Context, client systemUpdateHTTPDoer) []string {
	repository := systemUpdateReleaseRepository()
	requestURL := "https://api.github.com/repos/" + repository + "/releases?per_page=30"

	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := systemUpdateFetchJSON(ctx, client, requestURL, &releases); err != nil {
		return nil
	}

	tags := make([]string, 0, len(releases))
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if !systemUpdateIsValidReleaseTag(tag) {
			continue
		}
		tags = append(tags, tag)
	}
	return tags
}

func init() {
	Register(RegisterSystemUpdate)
}
