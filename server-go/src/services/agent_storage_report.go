// Port of server/src/services/agent-storage-report.ts.
//
// The node reports the storage pools it can see. Telemetry must NEVER recreate a
// deleted pool, change a pool's purpose, or resurrect an import bookkeeping row:
// only pools that already exist for this host are touched, and only when the
// sample is newer than what the row already holds.
//
// Time: every time.Time that reaches SQL is UTC (WAVE_D_SPEC §0.2b).
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// AgentStorageReportPoolsMax mirrors `payload.pools.length > 256`.
const AgentStorageReportPoolsMax = 256

// agentStorageIsRecord mirrors `record(value)`: a non-nil, non-array object.
func agentStorageIsRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

// agentStorageIsBytes mirrors `bytes(value)` (Number.isSafeInteger && >= 0).
func agentStorageIsBytes(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok {
		switch typed := value.(type) {
		case int:
			number, ok = float64(typed), true
		case int64:
			number, ok = float64(typed), true
		default:
			return 0, false
		}
	}
	if number != float64(int64(number)) {
		return 0, false
	}
	if number < 0 || number > 9007199254740991 {
		return 0, false
	}
	return int64(number), true
}

// agentStorageJSString mirrors `String(value)` for the JSON shapes involved.
func agentStorageJSString(value any) string {
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
		return agentStorageJSNumber(typed)
	default:
		return fmt.Sprintf("%v", value)
	}
}

// agentStorageJSNumber mirrors String(number) for the values that can appear in
// a pool config (source / zfs.pool_name). The exponent spelling matches
// ECMAScript: 1e+21 rather than 1e+21-always.
func agentStorageJSNumber(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == 0:
		return "0"
	}
	absolute := math.Abs(value)
	if absolute < 1e-6 || absolute >= 1e21 {
		text := strconv.FormatFloat(value, 'e', -1, 64)
		if index := strings.IndexByte(text, 'e'); index >= 0 {
			sign := text[index+1 : index+2]
			digits := strings.TrimLeft(text[index+2:], "0")
			if digits == "" {
				digits = "0"
			}
			text = text[:index] + "e" + sign + digits
		}
		return text
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// agentStorageJSTruthy mirrors JavaScript truthiness for decoded JSON values.
func agentStorageJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0 && typed == typed
	default:
		return true
	}
}

// agentStorageSlice mirrors `String(value).slice(0, limit)`: JavaScript counts
// UTF-16 code units, so an astral character counts twice and is never split.
func agentStorageSlice(value string, limit int) string {
	units := utf16.Encode([]rune(value))
	if len(units) <= limit {
		return value
	}
	if limit > 0 && units[limit-1] >= 0xD800 && units[limit-1] <= 0xDBFF {
		limit--
	}
	return string(utf16.Decode(units[:limit]))
}

// agentStorageParseJSDate mirrors `new Date(value)` for the ISO strings nodes
// send. ok=false is an Invalid Date (NaN), which the caller treats as a reject.
func agentStorageParseJSDate(value string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// agentStoragePoolRow is the projection processAgentStorageReport reads.
type agentStoragePoolRow struct {
	ID         int32
	Name       string
	Driver     string
	Purpose    string
	Config     map[string]any
	Status     string
	TotalBytes int64
	UsedBytes  int64
	Telemetry  []byte
	ReportedAt *time.Time
	CreatedAt  time.Time
}

// ProcessAgentStorageReport mirrors processAgentStorageReport. The bool is the
// `accepted` flag the heartbeat uses to decide whether the node is healthy.
func ProcessAgentStorageReport(ctx context.Context, hostID int32, payload any, receivedAt time.Time) (bool, error) {
	if db.Pool() == nil {
		return false, errors.New("database not initialized")
	}
	report, ok := agentStorageIsRecord(payload)
	if !ok {
		return false, nil
	}
	reportedAtText, ok := report["reportedAt"].(string)
	if !ok {
		return false, nil
	}
	sampledAt, ok := agentStorageParseJSDate(reportedAtText)
	if !ok {
		return false, nil
	}
	if sampledAt.UnixMilli() > receivedAt.UTC().UnixMilli()+30000 ||
		sampledAt.UnixMilli() < receivedAt.UTC().UnixMilli()-120000 {
		return false, nil
	}

	available := report["available"]
	availableTrue := available == true
	availableFalse := available == false
	if !availableFalse {
		poolsValue, poolsOK := report["pools"].([]any)
		if !availableTrue || !poolsOK || len(poolsValue) > AgentStorageReportPoolsMax {
			return false, nil
		}
	}

	items := []any{}
	if availableTrue {
		items, _ = report["pools"].([]any)
	}
	byName := make(map[string]map[string]any, len(items))
	for _, item := range items {
		record, ok := agentStorageIsRecord(item)
		if !ok {
			return false, nil
		}
		name, ok := record["name"].(string)
		if !ok {
			return false, nil
		}
		if _, duplicate := byName[name]; duplicate {
			return false, nil
		}
		byName[name] = record
	}

	rows, err := db.Pool().Query(ctx, `
		SELECT id, name, driver, purpose::text, config, status, total_bytes, used_bytes,
		       telemetry, reported_at, created_at
		FROM storage_pools WHERE host_id = $1 ORDER BY id ASC`, hostID)
	if err != nil {
		return false, err
	}
	pools := make([]agentStoragePoolRow, 0, 8)
	for rows.Next() {
		var row agentStoragePoolRow
		var configJSON []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Driver, &row.Purpose, &configJSON,
			&row.Status, &row.TotalBytes, &row.UsedBytes, &row.Telemetry,
			&row.ReportedAt, &row.CreatedAt); err != nil {
			rows.Close()
			return false, err
		}
		if len(configJSON) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(configJSON, &decoded); err == nil {
				row.Config = decoded
			}
		}
		if row.Config == nil {
			row.Config = map[string]any{}
		}
		pools = append(pools, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()

	accepted := true
	for _, pool := range pools {
		if sampledAt.Before(pool.CreatedAt) || (pool.ReportedAt != nil && !sampledAt.After(*pool.ReportedAt)) {
			accepted = false
			continue
		}
		config := pool.Config
		if importConfig, ok := config["_payincusDeletion"]; ok && agentStorageJSTruthy(importConfig) {
			continue
		}

		item := byName[pool.Name]
		if item == nil {
			if importConfig, ok := config["_payincusImport"]; ok {
				if importObject, objectOK := agentStorageIsRecord(importConfig); objectOK {
					if datasets, datasetsOK := report["datasets"].([]any); datasetsOK {
						source, _ := importObject["source"].(string)
						for _, datasetValue := range datasets {
							dataset, datasetOK := agentStorageIsRecord(datasetValue)
							if !datasetOK || dataset["dataset"] != source {
								continue
							}
							encryptionRoot, _ := dataset["encryptionRoot"].(string)
							var lock map[string]any
							if locks, locksOK := report["locks"].(map[string]any); locksOK {
								lock, _ = agentStorageIsRecord(locks[encryptionRoot])
							}
							pendingRecovery := 0
							if lock != nil {
								if resume, resumeOK := lock["resumeInstances"].([]any); resumeOK {
									pendingRecovery = len(resume)
								}
							}
							keyState := "locked"
							if keyStatus, _ := dataset["keyStatus"].(string); keyStatus == "available" {
								keyState = "unlocked"
							}
							locked := lock != nil && lock["locked"] == true
							item = map[string]any{
								"name":   pool.Name,
								"driver": pool.Driver,
								"status": "locked",
								"config": map[string]any{"source": source},
								"encryption": map[string]any{
									"encrypted":       true,
									"source":          source,
									"encryptionRoot":  encryptionRoot,
									"state":           keyState,
									"keyFormat":       dataset["keyFormat"],
									"manualLocked":    locked,
									"lockPending":     locked && lock["complete"] != true,
									"pendingRecovery": pendingRecovery,
								},
							}
							break
						}
					}
				}
			}
		}
		if item == nil && availableTrue && report["complete"] != true {
			continue
		}

		status := "unavailable"
		if item == nil {
			if availableTrue {
				status = "missing"
			}
		}

		source := ""
		if value, ok := config["zfs.pool_name"]; ok && agentStorageJSTruthy(value) {
			source = agentStorageJSString(value)
		} else if value, ok := config["source"]; ok && agentStorageJSTruthy(value) {
			source = agentStorageJSString(value)
		}
		actualSource := ""
		if item != nil {
			itemConfig, _ := agentStorageIsRecord(item["config"])
			if value, ok := itemConfig["zfs.pool_name"]; ok && agentStorageJSTruthy(value) {
				actualSource = agentStorageJSString(value)
			} else if value, ok := itemConfig["source"]; ok && agentStorageJSTruthy(value) {
				actualSource = agentStorageJSString(value)
			}
		}
		matchingSource := source == "" || strings.HasPrefix(source, "/") || source == actualSource

		var (
			totalBytes *int64
			usedBytes  *int64
			telemetry  any
			mergedCfg  map[string]any
		)
		itemDriver, _ := item["driver"].(string)
		if item != nil && itemDriver == pool.Driver && matchingSource {
			space, spaceOK := agentStorageIsRecord(item["space"])
			total, totalOK := agentStorageIsBytes(space["total"])
			used, usedOK := agentStorageIsBytes(space["used"])
			validSpace := spaceOK && totalOK && total > 0 && usedOK && used <= total

			status = "unavailable"
			if importValue, importOK := config["_payincusImport"]; importOK && agentStorageJSTruthy(importValue) {
				status = "pending"
			} else if itemStatus, _ := item["status"].(string); itemStatus == "locked" {
				status = "locked"
			} else if itemStatus, _ := item["status"].(string); itemStatus == "ready" && validSpace {
				status = "ready"
			}

			encryptionValue, hasEncryption := agentStorageIsRecord(item["encryption"])
			if pool.Driver == "zfs" && !hasEncryption {
				status = "unavailable"
			}
			if hasEncryption && encryptionValue["encrypted"] == true {
				if state, _ := encryptionValue["state"].(string); state != "unlocked" {
					status = "locked"
				}
			}
			if validSpace {
				totalValue, usedValue := total, used
				totalBytes = &totalValue
				usedBytes = &usedValue
			}

			var encryption any
			if hasEncryption {
				manualLocked := encryptionValue["manualLocked"] == true
				encryption = map[string]any{
					"encrypted": encryptionValue["encrypted"] == true,
					"state": func() string {
						if state, _ := encryptionValue["state"].(string); state == "unlocked" {
							return "unlocked"
						}
						return "locked"
					}(),
					"dataset":        agentStorageSlice(agentStorageJSString(encryptionValue["source"]), 200),
					"encryptionRoot": agentStorageSlice(agentStorageJSString(encryptionValue["encryptionRoot"]), 200),
					"manualLocked":   manualLocked,
					"lockPending":    encryptionValue["lockPending"] == true,
					"pendingRecovery": func() any {
						if value, ok := agentStorageIsBytes(encryptionValue["pendingRecovery"]); ok {
							return value
						}
						return 0
					}(),
					"nodeLocked": encryptionValue["nodeLocked"] == true,
					"keyFormat":  agentStorageSlice(agentStorageJSString(encryptionValue["keyFormat"]), 20),
				}
			}

			var backing any
			if backingValue, ok := agentStorageIsRecord(item["backing"]); ok {
				sizeBytes, sizeOK := agentStorageIsBytes(backingValue["sizeBytes"])
				availableBytes, availableOK := agentStorageIsBytes(backingValue["availableBytes"])
				idValue, idOK := backingValue["id"].(string)
				if sizeOK && availableOK && idOK {
					backing = map[string]any{
						"sizeBytes":      sizeBytes,
						"availableBytes": availableBytes,
						"id":             agentStorageSlice(idValue, 100),
					}
				}
			}

			usedBy := any(0)
			if value, ok := agentStorageIsBytes(item["usedBy"]); ok {
				usedBy = value
			}
			var filesystemID any
			if value, ok := item["filesystemId"].(string); ok {
				filesystemID = agentStorageSlice(value, 100)
			}
			telemetry = map[string]any{
				"usedBy":       usedBy,
				"encryption":   encryption,
				"backing":      backing,
				"filesystemId": filesystemID,
			}

			if itemConfig, ok := agentStorageIsRecord(item["config"]); ok {
				allStrings := true
				for _, value := range itemConfig {
					if _, isString := value.(string); !isString {
						allStrings = false
						break
					}
				}
				if allStrings {
					merged := make(map[string]any, len(config)+len(itemConfig))
					for key, value := range config {
						merged[key] = value
					}
					for key, value := range itemConfig {
						merged[key] = value
					}
					mergedCfg = merged
				}
			}
		}

		query := `UPDATE storage_pools SET reported_at = $2, status = $3, updated_at = $4`
		args := []any{pool.ID, sampledAt, status, time.Now().UTC()}
		if totalBytes != nil && usedBytes != nil {
			query += fmt.Sprintf(", total_bytes = $%d, used_bytes = $%d", len(args)+1, len(args)+2)
			args = append(args, *totalBytes, *usedBytes)
		}
		if telemetry != nil {
			encoded, err := json.Marshal(telemetry)
			if err != nil {
				return false, err
			}
			query += fmt.Sprintf(", telemetry = $%d::jsonb", len(args)+1)
			args = append(args, string(encoded))
		}
		if mergedCfg != nil {
			encoded, err := json.Marshal(mergedCfg)
			if err != nil {
				return false, err
			}
			query += fmt.Sprintf(", config = $%d::jsonb", len(args)+1)
			args = append(args, string(encoded))
		}
		query += fmt.Sprintf(" WHERE id = $1 AND created_at <= $%d AND (reported_at IS NULL OR reported_at < $%d)",
			len(args)+1, len(args)+1)
		args = append(args, sampledAt)

		tag, err := db.Pool().Exec(ctx, query, args...)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 || status == "unavailable" || status == "missing" {
			accepted = false
		}
	}

	return accepted, nil
}
