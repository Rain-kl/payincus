// Port of server/src/services/system-monitor.ts.
package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	systemAlertCooldown = 10 * time.Minute
	systemAlertTimeout  = 10 * time.Second
)

var systemStartTime = time.Now()

var systemMonitorAlertState = struct {
	mu       sync.Mutex
	lastSent time.Time
}{}

// SystemMetrics mirrors SystemMetrics in system-monitor.ts.
type SystemMetrics struct {
	Uptime                  int64 `json:"uptime"`
	InstanceCount           int64 `json:"instanceCount"`
	HostCount               int64 `json:"hostCount"`
	OnlineHostCount         int64 `json:"onlineHostCount"`
	PendingTasks            int64 `json:"pendingTasks"`
	ProcessingTasks         int64 `json:"processingTasks"`
	PendingRestoreTasks     int64 `json:"pendingRestoreTasks"`
	ProcessingRestoreTasks  int64 `json:"processingRestoreTasks"`
	PendingUploadTasks      int64 `json:"pendingUploadTasks"`
	ProcessingUploadTasks   int64 `json:"processingUploadTasks"`
	IncusConnectionPoolSize int   `json:"incusConnectionPoolSize"`
	DBPoolTotalCount        int   `json:"dbPoolTotalCount"`
	DBPoolIdleCount         int   `json:"dbPoolIdleCount"`
	DBPoolWaitingCount      int   `json:"dbPoolWaitingCount"`
	AvgResponseTime         int64 `json:"avgResponseTime"`
	ErrorHosts              int   `json:"errorHosts"`
	MemoryUsageMB           int64 `json:"memoryUsageMB"`
}

// GetSystemMetrics mirrors getSystemMetrics().
func GetSystemMetrics(ctx context.Context) (SystemMetrics, error) {
	return CollectSystemMetrics(ctx)
}

// CollectSystemMetrics mirrors collectMetrics().
func CollectSystemMetrics(ctx context.Context) (SystemMetrics, error) {
	var metrics SystemMetrics
	metrics.Uptime = int64(time.Since(systemStartTime).Minutes())

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	metrics.MemoryUsageMB = int64(mem.HeapAlloc / 1024 / 1024)

	poolStatus := lib.GetIncusPoolStatus()
	metrics.IncusConnectionPoolSize = len(poolStatus)
	var totalResponseMs int64
	var responseCount int64
	for _, st := range poolStatus {
		if st.LastResponseTime > 0 {
			totalResponseMs += st.LastResponseTime.Milliseconds()
			responseCount++
		}
		if st.ErrorCount >= 3 {
			metrics.ErrorHosts++
		}
	}
	if responseCount > 0 {
		metrics.AvgResponseTime = totalResponseMs / responseCount
	}

	metrics.DBPoolTotalCount, metrics.DBPoolIdleCount, metrics.DBPoolWaitingCount = db.PoolStats()

	if db.DB == nil {
		return metrics, nil
	}

	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM instances WHERE status <> 'deleted'`).Scan(&metrics.InstanceCount).Error

	type hostStatusCount struct {
		Status string `gorm:"column:status"`
		Cnt    int64  `gorm:"column:cnt"`
	}
	var hostStats []hostStatusCount
	if db.DB.WithContext(ctx).Raw(`SELECT status::text AS status, COUNT(*) AS cnt FROM hosts GROUP BY status`).Scan(&hostStats).Error == nil {
		for _, h := range hostStats {
			metrics.HostCount += h.Cnt
			if h.Status == "online" {
				metrics.OnlineHostCount += h.Cnt
			}
		}
	}

	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM instance_tasks WHERE status = 'PENDING'`).Scan(&metrics.PendingTasks).Error
	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM instance_tasks WHERE status = 'PROCESSING'`).Scan(&metrics.ProcessingTasks).Error
	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM restore_tasks WHERE status = 'PENDING'`).Scan(&metrics.PendingRestoreTasks).Error
	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM restore_tasks WHERE status = 'PROCESSING'`).Scan(&metrics.ProcessingRestoreTasks).Error
	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM backup_upload_tasks WHERE status = 'PENDING'`).Scan(&metrics.PendingUploadTasks).Error
	_ = db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM backup_upload_tasks WHERE status = 'PROCESSING'`).Scan(&metrics.ProcessingUploadTasks).Error

	return metrics, nil
}

// SystemEvaluateHealthStatus returns alert summary items matching reportHealth().
func SystemEvaluateHealthStatus(m SystemMetrics) []string {
	var alerts []string
	if m.ErrorHosts > 0 {
		alerts = append(alerts, fmt.Sprintf("⚠️ %d hosts with errors", m.ErrorHosts))
	}
	if m.ProcessingTasks > 10 {
		alerts = append(alerts, fmt.Sprintf("⚠️ High task load: %d processing", m.ProcessingTasks))
	}
	if m.AvgResponseTime > 1000 {
		alerts = append(alerts, fmt.Sprintf("⚠️ Slow response: %dms avg", m.AvgResponseTime))
	}
	if m.MemoryUsageMB > 512 {
		alerts = append(alerts, fmt.Sprintf("⚠️ High memory: %dMB", m.MemoryUsageMB))
	}
	return alerts
}

var systemAlertFetch = func(ctx context.Context, rawURL string, body []byte) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, systemAlertTimeout)
	defer cancel()
	parsed, err := lib.AssertSafeWebhookURL(reqCtx, rawURL)
	if err != nil {
		return 0, err
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	resp, err := lib.SafeFetch(reqCtx, parsed.String(), &lib.SafeFetchInit{
		Method:  http.MethodPost,
		Headers: headers,
		Body:    bytes.NewReader(body),
	}, "Webhook URL")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func sendSystemAlert(ctx context.Context, alerts []string) {
	webhookURL := strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL"))
	if webhookURL == "" || len(alerts) == 0 {
		return
	}
	now := time.Now().UTC()
	systemMonitorAlertState.mu.Lock()
	if !systemMonitorAlertState.lastSent.IsZero() && now.Sub(systemMonitorAlertState.lastSent) < systemAlertCooldown {
		systemMonitorAlertState.mu.Unlock()
		log.Printf("[Monitor] Alert cooldown, skipping webhook")
		return
	}
	systemMonitorAlertState.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"type":      "system_alert",
		"timestamp": now.Format("2006-01-02T15:04:05.000Z"),
		"alerts":    alerts,
		"source":    "incudal-monitor",
	})
	if err != nil {
		return
	}

	status, err := systemAlertFetch(ctx, webhookURL, payload)
	if err != nil {
		log.Printf("[Monitor] Failed to send alert: %v", err)
		return
	}
	if status >= 200 && status < 300 {
		systemMonitorAlertState.mu.Lock()
		systemMonitorAlertState.lastSent = now
		systemMonitorAlertState.mu.Unlock()
		log.Printf("[Monitor] Alert sent successfully")
	} else {
		log.Printf("[Monitor] Alert webhook failed: %d", status)
	}
}

// ReportSystemHealth mirrors reportHealth().
func ReportSystemHealth(ctx context.Context) {
	if ctx.Err() != nil || db.DB == nil {
		return
	}
	m, err := CollectSystemMetrics(ctx)
	if err != nil {
		log.Printf("[Monitor] Failed to collect metrics: %v", err)
		return
	}
	alerts := SystemEvaluateHealthStatus(m)
	healthStatus := "✓ All systems healthy"
	if len(alerts) > 0 {
		healthStatus = strings.Join(alerts, ", ")
		sendSystemAlert(ctx, alerts)
	}
	totalPending := m.PendingTasks + m.PendingRestoreTasks + m.PendingUploadTasks
	totalProcessing := m.ProcessingTasks + m.ProcessingRestoreTasks + m.ProcessingUploadTasks
	log.Printf("[Monitor] Health Report: Uptime=%dmin Memory=%dMB Instances=%d Hosts=%d/%d Tasks=%d/%d DBPool=%d/%d/%d IncusPool=%d(%dms avg) Status=%s",
		m.Uptime, m.MemoryUsageMB, m.InstanceCount, m.OnlineHostCount, m.HostCount,
		totalPending, totalProcessing, m.DBPoolTotalCount, m.DBPoolIdleCount, m.DBPoolWaitingCount,
		m.IncusConnectionPoolSize, m.AvgResponseTime, healthStatus,
	)
}

var systemMonitorRunning atomic.Bool
