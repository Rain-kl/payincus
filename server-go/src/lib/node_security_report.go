// Port of server/src/lib/node-security/report.ts.
//
// parseReport is a SECURITY allow-list, not a deserialiser: the node agent is the
// only caller, but its payload is still treated as hostile. Whatever the agent
// sends, only the fixed scalar fields below survive; every arbitrary field,
// command payload, environment variable or file path is dropped. That is what
// keeps a compromised node from smuggling attacker-controlled JSON into the panel
// and from filling the database with unbounded strings.
//
// Every bound in the TypeScript is reproduced exactly:
//
//	events                 <= 128, each strictly validated
//	results                first 256, target sliced to 200 chars
//	resumeInstances        first 10000, each project/name sliced to 255 chars
//	reason                 enum or "cli:<lower_snake>", sliced to 64 chars
//	revision               non-negative JavaScript safe integer
//
// recordReport then serialises per node with
// pg_advisory_xact_lock(700260, hostId) and refuses to persist an older revision,
// so a delayed delivery cannot roll a newer lock state backwards.
package lib

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Report clamps, mirroring parseReport.
const (
	nodeReportMaxEvents          = 128
	nodeReportMaxResults         = 256
	nodeReportMaxResumeInstances = 10000

	nodeReportResultTargetMaxLength       = 200
	nodeReportResumeFieldMaxLength        = 255
	nodeReportReasonMaxLength             = 64
	nodeReportMaxUID                      = 4294967295
	nodeReportMaxPID                      = 2147483647
	nodeReportSafeIntegerMax        int64 = 1<<53 - 1
)

var (
	// nodeReportEventIDPattern mirrors /^[0-9a-f]{32}$/.
	nodeReportEventIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// nodeReportCLIReasonPattern mirrors /^cli:[a-z_]+$/.
	nodeReportCLIReasonPattern = regexp.MustCompile(`^cli:[a-z_]+$`)
)

// NodeReportReasons is the reason allow-list, in the TypeScript's order.
var NodeReportReasons = []string{
	"startup_verification",
	"daemon_restart",
	"panel_request",
	"state_unavailable",
	"audit_unavailable",
}

// incusCLICommandList mirrors INCUS_CLI_COMMANDS in the TypeScript, in the same
// order so a diff of the two sources stays readable.
var incusCLICommandList = []string{
	"help", "help_or_version", "list", "ls", "info", "version", "completion",
	"exec", "shell", "console", "file", "export", "publish", "copy", "cp", "move",
	"mv", "config", "profile", "storage", "network", "query", "start", "restart",
	"restore", "snapshot", "delete", "launch", "init", "admin", "operation", "stop",
	"pause", "import", "image", "project", "remote", "alias", "alias_or_unknown",
	"unknown_option", "info_log", "config_read", "profile_read", "storage_read",
	"network_read", "image_read", "project_read", "remote_read", "operation_read",
}

// incusCLICommands is INCUS_CLI_COMMANDS as a set: an event whose command is not
// on this list is rejected, so a node cannot invent command names.
var incusCLICommands = func() map[string]struct{} {
	set := make(map[string]struct{}, len(incusCLICommandList))
	for _, command := range incusCLICommandList {
		set[command] = struct{}{}
	}
	return set
}()

// IncusCLICommands returns the allow-listed command names (a copy).
func IncusCLICommands() []string {
	return append([]string(nil), incusCLICommandList...)
}

// IsIncusCLICommand reports whether command is on the allow-list.
func IsIncusCLICommand(command string) bool {
	_, ok := incusCLICommands[command]
	return ok
}

// NodeCLIEvent is one validated CLI event. Only these seven fields survive; the
// TypeScript rebuilds the object literally (`{ id, time, uid, pid, command,
// critical, blocked }`) so a payload field cannot ride along.
type NodeCLIEvent struct {
	ID       string
	Time     string
	UID      int64
	PID      int32
	Command  string
	Critical bool
	Blocked  bool
}

// ParseNodeReport mirrors parseReport(input: unknown).
//
// The error messages are the TypeScript's own strings: the route surfaces them
// through nodeErrorDetail, so they must stay byte-identical.
func ParseNodeReport(input any) (db.NodeSecurityReport, []NodeCLIEvent, error) {
	var report db.NodeSecurityReport

	if input == nil {
		return report, nil, errors.New("invalid report")
	}
	object, isObject := input.(map[string]any)
	if !isObject {
		if _, isArray := input.([]any); isArray {
			// `typeof [] === 'object'` passes the first guard in JavaScript and then
			// fails on `state`, so the message is the state one.
			return report, nil, errors.New("invalid state")
		}
		return report, nil, errors.New("invalid report")
	}

	rawState, present := object["state"]
	if !present {
		return report, nil, errors.New("invalid state")
	}
	state, ok := rawState.(map[string]any)
	if !ok {
		return report, nil, errors.New("invalid state")
	}

	enabled, ok := state["enabled"].(bool)
	if !ok {
		return report, nil, errors.New("invalid state")
	}
	locked, ok := state["locked"].(bool)
	if !ok {
		return report, nil, errors.New("invalid state")
	}
	revisionNumber, ok := state["revision"].(float64)
	if !ok {
		return report, nil, errors.New("invalid state")
	}
	revision, ok := nodeReportSafeInteger(revisionNumber)
	if !ok {
		// Number.isSafeInteger(...) && Number(...) >= 0
		return report, nil, errors.New("invalid state")
	}

	rawEvents, present := object["events"]
	if !present {
		return report, nil, errors.New("invalid events")
	}
	eventList, ok := rawEvents.([]any)
	if !ok || len(eventList) > nodeReportMaxEvents {
		return report, nil, errors.New("invalid events")
	}

	events := make([]NodeCLIEvent, 0, len(eventList))
	for _, rawEvent := range eventList {
		event, err := nodeReportParseEvent(rawEvent)
		if err != nil {
			return report, nil, err
		}
		events = append(events, event)
	}

	report = db.NodeSecurityReport{
		Enabled:           enabled,
		Locked:            locked,
		RecoveryPending:   nodeReportIsTrue(state["recoveryPending"]),
		StartupRecovering: nodeReportIsTrue(state["startupRecovering"]),
		Revision:          revision,
		Results:           nodeReportParseResults(state["results"]),
		ResumeInstances:   nodeReportParseResumeInstances(state["resumeInstances"]),
		Reason:            nodeReportParseReason(state["reason"]),
	}
	return report, events, nil
}

// nodeReportParseEvent mirrors the per-event validation block.
func nodeReportParseEvent(raw any) (NodeCLIEvent, error) {
	var event NodeCLIEvent

	value, ok := raw.(map[string]any)
	if !ok {
		// `!e` rejects null; anything without fields fails the first regex test.
		return event, errors.New("invalid event")
	}

	id, ok := value["id"].(string)
	if !ok || !nodeReportEventIDPattern.MatchString(id) {
		return event, errors.New("invalid event")
	}
	command, ok := value["command"].(string)
	if !ok || !IsIncusCLICommand(command) {
		return event, errors.New("invalid event")
	}
	critical, ok := value["critical"].(bool)
	if !ok {
		return event, errors.New("invalid event")
	}
	blocked, ok := value["blocked"].(bool)
	if !ok {
		return event, errors.New("invalid event")
	}

	uidNumber, ok := value["uid"].(float64)
	if !ok {
		return event, errors.New("invalid event")
	}
	uid, ok := nodeReportJSInteger(uidNumber, 0, nodeReportMaxUID)
	if !ok {
		return event, errors.New("invalid event")
	}

	pidNumber, ok := value["pid"].(float64)
	if !ok {
		return event, errors.New("invalid event")
	}
	pid, ok := nodeReportJSInteger(pidNumber, 0, nodeReportMaxPID)
	if !ok {
		return event, errors.New("invalid event")
	}

	occurredAt, ok := value["time"].(string)
	if !ok {
		return event, errors.New("invalid event")
	}
	if _, valid := nodeReportParseTime(occurredAt); !valid {
		return event, errors.New("invalid event")
	}

	return NodeCLIEvent{
		ID:       id,
		Time:     occurredAt,
		UID:      uid,
		PID:      int32(pid),
		Command:  command,
		Critical: critical,
		Blocked:  blocked,
	}, nil
}

// nodeReportParseResults mirrors:
//
//	state.results.slice(0, 256).map(r => ({
//	  target: String(r.target || '').slice(0, 200),
//	  success: r.success === true,
//	  detail: r.detail === 'complete' || r.detail === 'not_configured' ? r.detail : 'failed'
//	}))
func nodeReportParseResults(raw any) []db.NodeSecurityResult {
	results := make([]db.NodeSecurityResult, 0)
	list, ok := raw.([]any)
	if !ok {
		return results
	}
	if len(list) > nodeReportMaxResults {
		list = list[:nodeReportMaxResults]
	}
	for _, item := range list {
		target := ""
		success := false
		detail := "failed"
		if entry, ok := item.(map[string]any); ok {
			// `r.target || ''`: any falsy target becomes the empty string.
			if value, present := entry["target"]; present && nodeReportTruthy(value) {
				target = nodeReportStringOf(value)
			}
			success = nodeReportIsTrue(entry["success"])
			if rawDetail, ok := entry["detail"].(string); ok && (rawDetail == "complete" || rawDetail == "not_configured") {
				detail = rawDetail
			}
		}
		results = append(results, db.NodeSecurityResult{
			Target:  jsTruncateUTF16(target, nodeReportResultTargetMaxLength),
			Success: success,
			Detail:  detail,
		})
	}
	return results
}

// nodeReportParseResumeInstances mirrors:
//
//	state.resumeInstances.slice(0, 10000)
//	  .filter(item => typeof item?.project === 'string' && typeof item?.name === 'string')
//	  .map(item => ({ project: item.project.slice(0, 255), name: item.name.slice(0, 255) }))
//
// Note the filter accepts an EMPTY string (only the typeof is checked), so the
// slicing below must not drop it either.
func nodeReportParseResumeInstances(raw any) []db.NodeSecurityResumeInstance {
	instances := make([]db.NodeSecurityResumeInstance, 0)
	list, ok := raw.([]any)
	if !ok {
		return instances
	}
	if len(list) > nodeReportMaxResumeInstances {
		list = list[:nodeReportMaxResumeInstances]
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		project, projectOK := entry["project"].(string)
		name, nameOK := entry["name"].(string)
		if !projectOK || !nameOK {
			continue
		}
		instances = append(instances, db.NodeSecurityResumeInstance{
			Project: jsTruncateUTF16(project, nodeReportResumeFieldMaxLength),
			Name:    jsTruncateUTF16(name, nodeReportResumeFieldMaxLength),
		})
	}
	return instances
}

// nodeReportParseReason mirrors the reason gate: a known enum value or
// "cli:<lowercase/underscore>", sliced to 64 characters; anything else is "".
func nodeReportParseReason(raw any) string {
	reason, ok := raw.(string)
	if !ok {
		return ""
	}
	allowed := false
	for _, candidate := range NodeReportReasons {
		if reason == candidate {
			allowed = true
			break
		}
	}
	if !allowed && !nodeReportCLIReasonPattern.MatchString(reason) {
		return ""
	}
	return jsTruncateUTF16(reason, nodeReportReasonMaxLength)
}

// RecordReport mirrors recordReport(hostId, input).
//
// It returns the accepted event ids. Everything else (allow-listing, the
// recoveryBlocked override, the revision comparison, the 30-day cleanup) happens
// inside the db transaction that holds the per-node advisory lock.
func RecordReport(ctx context.Context, hostID int32, input any) ([]string, error) {
	report, events, err := ParseNodeReport(input)
	if err != nil {
		return nil, err
	}

	inputs := make([]db.NodeCLIEventInput, 0, len(events))
	ids := make([]string, 0, len(events))
	for _, event := range events {
		occurredAt, ok := nodeReportParseTime(event.Time)
		if !ok {
			// Unreachable: the event was validated above.
			return nil, errors.New("invalid event")
		}
		inputs = append(inputs, db.NodeCLIEventInput{
			EventID:    event.ID,
			Command:    event.Command,
			UID:        event.UID,
			PID:        event.PID,
			Critical:   event.Critical,
			Blocked:    event.Blocked,
			OccurredAt: occurredAt,
		})
		ids = append(ids, event.ID)
	}

	if _, err := db.RecordNodeSecurityReport(ctx, hostID, report, inputs); err != nil {
		return nil, err
	}
	return ids, nil
}

// ==================== JavaScript scalar semantics ====================

// nodeReportIsTrue mirrors `value === true`.
func nodeReportIsTrue(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

// nodeReportTruthy mirrors JavaScript truthiness for JSON values.
func nodeReportTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case []any:
		return true
	case map[string]any:
		return true
	}
	return true
}

// nodeReportStringOf mirrors String(value) for JSON values.
func nodeReportStringOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberToString(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, nodeReportStringOf(item))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		// String({}) is "[object Object]" in JavaScript.
		return "[object Object]"
	}
	return ""
}

// nodeReportSafeInteger mirrors `Number.isSafeInteger(value) && value >= 0`: an
// integral JSON number within [0, 2^53-1].
func nodeReportSafeInteger(value float64) (int64, bool) {
	return nodeReportJSInteger(value, 0, nodeReportSafeIntegerMax)
}

// nodeReportJSInteger mirrors `Number.isInteger(value) && value >= min && value <= max`.
// The range is checked before converting so no out-of-range float ever reaches an
// int64 conversion.
func nodeReportJSInteger(value float64, min, max int64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value < float64(min) || value > float64(max) {
		return 0, false
	}
	if value != math.Trunc(value) {
		return 0, false
	}
	return int64(value), true
}

// jsTruncateUTF16 mirrors String#slice for a start-only call: it counts UTF-16
// code units (what JavaScript's .length and .slice use) and never splits a
// surrogate pair, so the stored text stays valid UTF-8.
func jsTruncateUTF16(value string, limit int) string {
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return value[:index]
		}
		units += width
	}
	return value
}

// nodeReportDateOnlyPattern matches the ISO date-only form Date.parse accepts as
// UTC midnight.
var nodeReportDateOnlyPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// nodeReportLocalTimeLayouts are the zone-less ISO forms V8's legacy parser
// accepts and interprets in the process's local time.
var nodeReportLocalTimeLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
}

// nodeReportParseTime mirrors `new Date(value)` far enough to reproduce the
// `Number.isFinite(Date.parse(value))` gate and the stored instant.
//
// RFC 3339 (what the agent emits via toISOString) is exact. The zone-less ISO
// forms are read in local time like V8. V8 also accepts some free-form English
// dates; those are rejected here rather than guessed, which can only reject a
// malformed agent event, never accept one (documented in the module report).
func nodeReportParseTime(value string) (time.Time, bool) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, true
	}
	for _, layout := range nodeReportLocalTimeLayouts {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, true
		}
	}
	if nodeReportDateOnlyPattern.MatchString(value) {
		if parsed, err := time.Parse("2006-01-02", value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
