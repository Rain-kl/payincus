package services

import (
	"context"
	"testing"
	"time"
)

func TestSystemMonitorCollectMetricsAndAlerts(t *testing.T) {
	m, err := CollectSystemMetrics(context.Background())
	if err != nil {
		t.Fatalf("CollectSystemMetrics: %v", err)
	}
	if m.Uptime < 0 {
		t.Fatalf("negative uptime: %d", m.Uptime)
	}

	alerts := SystemEvaluateHealthStatus(SystemMetrics{
		ErrorHosts:      2,
		ProcessingTasks: 12,
		AvgResponseTime: 1500,
		MemoryUsageMB:   600,
	})
	if len(alerts) != 4 {
		t.Fatalf("alerts = %v, want 4", alerts)
	}

	origFetch := systemAlertFetch
	defer func() { systemAlertFetch = origFetch }()
	systemMonitorAlertState.mu.Lock()
	systemMonitorAlertState.lastSent = time.Time{}
	systemMonitorAlertState.mu.Unlock()

	t.Setenv("ALERT_WEBHOOK_URL", "https://example.com/hook")
	calls := 0
	systemAlertFetch = func(ctx context.Context, rawURL string, body []byte) (int, error) {
		calls++
		return 200, nil
	}
	sendSystemAlert(context.Background(), alerts)
	sendSystemAlert(context.Background(), alerts)
	if calls != 1 {
		t.Fatalf("webhook calls = %d, want 1 (cooldown should suppress second alert)", calls)
	}
}
