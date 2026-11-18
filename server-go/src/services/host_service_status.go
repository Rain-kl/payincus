// Port of server/src/services/host-service-status.ts.
//
// Evaluates per-host runtime availability from host status, reverse tunnel
// online state, agent heartbeat freshness and node security lock/recovery state.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// HostServiceStatus mirrors HostServiceStatus in host-service-status.ts.
type HostServiceStatus struct {
	Status           string   `json:"status"`
	PendingInstances []string `json:"pendingInstances"`
	Available        bool     `json:"available"`
}

// HostServiceStatusRow is the joined DB row evaluated into HostServiceStatus.
type HostServiceStatusRow struct {
	ID            int32
	Status        string
	TunnelEnabled bool
	Report        []byte
	AgentStatus   *string
	AgentLastSeen *time.Time
	AgentReport   []byte
}

func hostServiceJSONNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

// LoadHostServiceStatuses mirrors loadHostServiceStatuses(hostIds?).
func LoadHostServiceStatuses(ctx context.Context, ids []int32) (map[int32]HostServiceStatus, error) {
	out := map[int32]HostServiceStatus{}
	if ids != nil && len(ids) == 0 {
		return out, nil
	}
	if db.DB == nil {
		return out, errors.New("hosts: database not initialized")
	}
	query := db.DB.WithContext(ctx).
		Table("hosts AS h").
		Select(`h.id, h.status, h.tunnel_enabled,
			ns.report AS report,
			a.status AS agent_status, a.last_seen_at AS agent_last_seen, a.last_report AS agent_report`).
		Joins("LEFT JOIN node_security_states ns ON ns.host_id = h.id").
		Joins("LEFT JOIN host_agents a ON a.host_id = h.id AND a.enabled = true")
	if ids != nil {
		query = query.Where("h.id IN ?", ids)
	}
	var rows []HostServiceStatusRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, row := range rows {
		out[row.ID] = EvaluateHostService(row, now)
	}
	return out, nil
}

// EvaluateHostService mirrors evaluateHostService(row, now).
func EvaluateHostService(row HostServiceStatusRow, now time.Time) HostServiceStatus {
	state := map[string]any{}
	if len(row.Report) > 0 {
		_ = json.Unmarshal(row.Report, &state)
	}
	report := map[string]any{}
	if len(row.AgentReport) > 0 {
		_ = json.Unmarshal(row.AgentReport, &report)
	}

	tunnelEnabled := row.TunnelEnabled
	status := lib.TunnelDeriveEffectiveHostStatus(lib.TunnelEffectiveHostStatusInput{
		ID:            row.ID,
		Status:        row.Status,
		TunnelEnabled: &tunnelEnabled,
	})
	if locked, ok := state["locked"].(bool); ok && locked {
		status = "locked"
	}

	interval := float64(5)
	if metrics, ok := report["metrics"].(map[string]any); ok {
		if reported, ok := hostServiceJSONNumber(metrics["heartbeatIntervalSeconds"]); ok && reported >= 5 && reported <= 3600 {
			interval = reported
		}
	}
	stale := false
	if row.AgentStatus != nil {
		threshold := math.Max(120, interval*3)
		stale = *row.AgentStatus != "online" || row.AgentLastSeen == nil ||
			now.Sub(row.AgentLastSeen.UTC()).Seconds() > threshold
	}

	pending := []string{}
	if items, ok := state["resumeInstances"].([]any); ok {
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if project, _ := entry["project"].(string); project == "default" {
				if name, ok := entry["name"].(string); ok {
					pending = append(pending, name)
				}
			}
		}
	}

	recoveryPending, _ := state["recoveryPending"].(bool)
	agentPresent := row.AgentStatus != nil
	incusAvailable := false
	if incus, ok := report["incus"].(map[string]any); ok {
		incusAvailable, _ = incus["available"].(bool)
	}
	instancesUnavailable := false
	if instances, ok := report["instances"].(map[string]any); ok {
		if available, ok := instances["available"].(bool); ok && !available {
			instancesUnavailable = true
		}
	}

	available := !recoveryPending &&
		(status == "online" || status == "maintenance") &&
		(!row.TunnelEnabled || lib.TunnelManagerInstance().IsTunnelOnline(row.ID)) &&
		!stale &&
		(!agentPresent || incusAvailable) &&
		!instancesUnavailable &&
		lib.IsHostHealthy(row.ID)

	return HostServiceStatus{Status: status, PendingInstances: pending, Available: available}
}
