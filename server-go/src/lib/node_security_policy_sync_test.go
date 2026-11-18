package lib

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
	"gorm.io/datatypes"
)

type stubPolicySyncClient struct {
	mu      sync.Mutex
	calls   []string
	handler func(method, path string, body any) (json.RawMessage, error)
}

func (s *stubPolicySyncClient) Request(_ context.Context, method, path string, body any) (json.RawMessage, error) {
	s.mu.Lock()
	s.calls = append(s.calls, method+" "+path)
	s.mu.Unlock()
	return s.handler(method, path, body)
}

func (s *stubPolicySyncClient) Close() error { return nil }

func TestSyncDesiredSecurityPolicy(t *testing.T) {
	origNow := policySyncNow
	origLoadState := policySyncLoadState
	origLoadHost := policySyncLoadHost
	origTunnelOn := policySyncTunnelOn
	origRecord := policySyncRecordFn
	origNewClient := policySyncNewClient
	defer func() {
		policySyncMu.Lock()
		policySyncLastAt = map[int32]time.Time{}
		policySyncInFlight = map[int32]bool{}
		policySyncMu.Unlock()
		policySyncNow = origNow
		policySyncLoadState = origLoadState
		policySyncLoadHost = origLoadHost
		policySyncTunnelOn = origTunnelOn
		policySyncRecordFn = origRecord
		policySyncNewClient = origNewClient
	}()

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	policySyncNow = func() time.Time { return now }
	fp := "sha256:abc"
	policySyncLoadHost = func(context.Context, int32) (*models.Host, error) {
		return &models.Host{ID: 7, URL: "https://127.0.0.1:8443", TunnelEnabled: true}, nil
	}
	policySyncTunnelOn = func(int32) bool { return true }

	var (
		mu          sync.Mutex
		recordedOpt []db.NodeSecurityRecordReportOptions
	)
	policySyncRecordFn = func(_ context.Context, _ int32, _ any, opts db.NodeSecurityRecordReportOptions) ([]string, error) {
		mu.Lock()
		recordedOpt = append(recordedOpt, opts)
		mu.Unlock()
		return nil, nil
	}

	stub := &stubPolicySyncClient{
		handler: func(method, _ string, body any) (json.RawMessage, error) {
			if method == http.MethodGet {
				return json.RawMessage(`{"enabled":false,"locked":false,"revision":4}`), nil
			}
			payload, _ := body.(map[string]any)
			if payload["enabled"] != true || payload["revision"] != int64(4) {
				return nil, errors.New("unexpected policy payload")
			}
			return json.RawMessage(`{"enabled":true,"locked":false,"revision":5}`), nil
		},
	}
	policySyncNewClient = func(context.Context, *models.Host, string) (policySyncClient, error) {
		return stub, nil
	}

	policySyncLoadState = func(context.Context, int32) (*models.NodeSecurityState, error) {
		return &models.NodeSecurityState{
			HostID:            7,
			ServerFingerprint: &fp,
			RecoveryBlocked:   false,
			Report:            datatypes.JSON(`{"enabled":false,"locked":false,"revision":4,"_desiredEnabled":true}`),
		}, nil
	}

	if !SyncDesiredSecurityPolicy(context.Background(), 7, false) {
		t.Fatal("expected initial policy sync to succeed")
	}
	if len(stub.calls) != 2 || stub.calls[0] != "GET /1.0/agent/security" || stub.calls[1] != "POST /1.0/agent/security/policy" {
		t.Fatalf("unexpected calls: %#v", stub.calls)
	}
	if len(recordedOpt) != 2 || !recordedOpt[0].Authoritative || recordedOpt[1].DesiredEnabled == nil || !*recordedOpt[1].DesiredEnabled {
		t.Fatalf("unexpected record opts: %#v", recordedOpt)
	}

	// Cooldown blocks repeat non-forced attempt inside 15s window.
	stub.calls = nil
	if SyncDesiredSecurityPolicy(context.Background(), 7, false) {
		t.Fatal("expected cooldown to skip repeat sync")
	}
	if len(stub.calls) != 0 {
		t.Fatalf("unexpected calls during cooldown: %#v", stub.calls)
	}

	// ClearPolicySyncCooldown allows immediate sync again.
	ClearPolicySyncCooldown(7)
	if !SyncDesiredSecurityPolicy(context.Background(), 7, false) {
		t.Fatal("expected sync after cooldown clear")
	}

	// RecoveryBlocked prevents sync.
	ClearPolicySyncCooldown(7)
	policySyncLoadState = func(context.Context, int32) (*models.NodeSecurityState, error) {
		return &models.NodeSecurityState{
			HostID:            7,
			ServerFingerprint: &fp,
			RecoveryBlocked:   true,
			Report:            datatypes.JSON(`{"enabled":false,"locked":false,"_desiredEnabled":true}`),
		}, nil
	}
	if SyncDesiredSecurityPolicy(context.Background(), 7, true) {
		t.Fatal("expected recoveryBlocked node to skip policy sync")
	}
}
