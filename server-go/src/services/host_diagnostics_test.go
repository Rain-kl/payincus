package services

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

type stubDiagClient struct {
	connectErr error
	responses  map[string]json.RawMessage
	errors     map[string]error
}

func (s *stubDiagClient) Connect(context.Context) error { return s.connectErr }
func (s *stubDiagClient) Request(_ context.Context, _ string, path string) (json.RawMessage, error) {
	if err := s.errors[path]; err != nil {
		return nil, err
	}
	return s.responses[path], nil
}
func (s *stubDiagClient) Close() error { return nil }

func TestRunHostDiagnosticsHealthyAndDrifted(t *testing.T) {
	origNow := diagnosticNow
	origLoad := diagnosticLoadHost
	origPing := diagnosticMeasurePing
	origNewClient := diagnosticNewClient
	origRecSuccess := diagnosticRecordSuccess
	origRecReport := diagnosticRecordReport
	origSyncPolicy := diagnosticSyncPolicy
	defer func() {
		diagnosticNow = origNow
		diagnosticLoadHost = origLoad
		diagnosticMeasurePing = origPing
		diagnosticNewClient = origNewClient
		diagnosticRecordSuccess = origRecSuccess
		diagnosticRecordReport = origRecReport
		diagnosticSyncPolicy = origSyncPolicy
	}()

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(-5 * time.Second)
	version := "1.3.8"
	fp := "sha256:pin"
	diagnosticNow = func() time.Time { return now }
	diagnosticRecordSuccess = func(int32, int64) {}
	diagnosticRecordReport = func(context.Context, int32, any, db.NodeSecurityRecordReportOptions) ([]string, error) {
		return nil, nil
	}

	var policySyncedCalled bool
	diagnosticSyncPolicy = func(_ context.Context, _ int32, force bool) bool {
		if !force {
			t.Fatal("diagnostics must call syncDesiredSecurityPolicy with force=true")
		}
		policySyncedCalled = true
		return true
	}

	diagnosticMeasurePing = func(context.Context, int32) lib.TunnelPingResult {
		ms := int64(12)
		return lib.TunnelPingResult{OK: true, LatencyMs: &ms}
	}

	diagnosticLoadHost = func(context.Context, int32) (*diagnosticHostSnapshot, error) {
		return &diagnosticHostSnapshot{
			HostID:            5,
			HostName:          "sg-node",
			TunnelEnabled:     true,
			HasAgent:          true,
			AgentStatus:       "online",
			AgentVersion:      &version,
			AgentLastSeenAt:   &lastSeen,
			ServerFingerprint: &fp,
			SecurityReport:    map[string]any{"_desiredEnabled": true},
			Pools: []diagnosticPoolRow{
				{Name: "default", Driver: "dir", Status: "ready", Config: map[string]any{}},
			},
		}, nil
	}

	diagnosticNewClient = func(*diagnosticHostSnapshot) diagnosticClient {
		return &stubDiagClient{
			responses: map[string]json.RawMessage{
				"/1.0/agent": json.RawMessage(`{"api_extensions":["security-v1"]}`),
				"/1.0/agent/baseline": json.RawMessage(`{
					"nodeSecurity": {"state": {"enabled": false, "locked": false, "revision": 7}},
					"storage": {"pools": [{"name": "default", "driver": "dir", "status": "ready"}]}
				}`),
			},
		}
	}

	res, err := RunHostDiagnostics(context.Background(), 5)
	if err != nil {
		t.Fatalf("RunHostDiagnostics error: %v", err)
	}
	if !res.OK || res.OverallStatus != "healthy" || res.NeedsBaselineSync {
		t.Fatalf("unexpected diagnostic summary: %#v", res)
	}
	if !policySyncedCalled || res.Checks.NodeSecurity.Meta["enabled"] != true {
		t.Fatalf("expected desired policy auto-sync, meta=%#v", res.Checks.NodeSecurity.Meta)
	}
	if res.Checks.ReverseTunnel.LatencyMs == nil || *res.Checks.ReverseTunnel.LatencyMs != 12 {
		t.Fatalf("unexpected tunnel check: %#v", res.Checks.ReverseTunnel)
	}

	// Tunnel down short-circuits mTLS checks cleanly.
	diagnosticMeasurePing = func(context.Context, int32) lib.TunnelPingResult {
		msg := "反向隧道未连接"
		return lib.TunnelPingResult{OK: false, Error: &msg}
	}
	resDown, err := RunHostDiagnostics(context.Background(), 5)
	if err != nil {
		t.Fatalf("tunnel-down diagnostics error: %v", err)
	}
	if resDown.OK || resDown.OverallStatus != "error" || resDown.Checks.IncusMtls.Status != DiagnosticStatusFail {
		t.Fatalf("unexpected tunnel-down result: %#v", resDown)
	}

	// Fallback on 404 from /1.0/agent/baseline and storage drift detection.
	diagnosticMeasurePing = func(context.Context, int32) lib.TunnelPingResult {
		ms := int64(8)
		return lib.TunnelPingResult{OK: true, LatencyMs: &ms}
	}
	diagnosticNewClient = func(*diagnosticHostSnapshot) diagnosticClient {
		return &stubDiagClient{
			errors: map[string]error{
				"/1.0/agent/baseline": lib.NewIncusAPIError("missing", http.StatusNotFound, nil),
			},
			responses: map[string]json.RawMessage{
				"/1.0/agent":                     json.RawMessage(`{"api_extensions":[]}`),
				"/1.0/agent/security":            json.RawMessage(`{"enabled":true,"locked":true,"revision":8,"reason":"cli:exec"}`),
				"/1.0/storage-pools?recursion=1": json.RawMessage(`[{"name":"extra","driver":"zfs","status":"Created"}]`),
			},
		}
	}
	resDrift, err := RunHostDiagnostics(context.Background(), 5)
	if err != nil {
		t.Fatalf("drift diagnostics error: %v", err)
	}
	if resDrift.OK || resDrift.OverallStatus != "warning" || !resDrift.NeedsBaselineSync {
		t.Fatalf("unexpected drift result: %#v", resDrift)
	}
	if resDrift.Checks.NodeSecurity.Status != DiagnosticStatusWarn || resDrift.Checks.StorageBaseline.Status != DiagnosticStatusWarn {
		t.Fatalf("expected warnings for locked security & drifted pools: sec=%#v storage=%#v", resDrift.Checks.NodeSecurity, resDrift.Checks.StorageBaseline)
	}
}
