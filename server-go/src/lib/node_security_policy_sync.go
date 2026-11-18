// Port of server/src/lib/node-security/policy-sync.ts.
//
// Syncs the controller's desired node security mode (_desiredEnabled) back to
// a reconnected or rebooted node over the fingerprint-pinned mTLS channel,
// guarded by per-host cooldown and in-flight deduplication.
package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const policySyncCooldown = 15 * time.Second

var (
	policySyncMu        sync.Mutex
	policySyncLastAt    = map[int32]time.Time{}
	policySyncInFlight  = map[int32]bool{}
	policySyncNow       = func() time.Time { return time.Now() }
	policySyncLoadState = db.GetNodeSecurityState
	policySyncLoadHost  = db.GetHostForNodeSecurity
	policySyncTunnelOn  = func(hostID int32) bool { return TunnelManagerInstance().IsTunnelOnline(hostID) }
	policySyncRecordFn  = RecordReportWithOptions
	policySyncNewClient = policySyncDefaultNewClient
)

// ClearPolicySyncCooldown clears the per-host cooldown timestamp after an
// explicit panel policy change or host identity re-enrollment.
func ClearPolicySyncCooldown(hostID int32) {
	policySyncMu.Lock()
	delete(policySyncLastAt, hostID)
	policySyncMu.Unlock()
}

type policySyncClient interface {
	Request(ctx context.Context, method, path string, body any) (json.RawMessage, error)
	Close() error
}

type policySyncIncusClient struct {
	client *IncusClient
}

func (p *policySyncIncusClient) Request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	return p.client.Request(ctx, method, path, body, 0, body)
}

func (p *policySyncIncusClient) Close() error {
	return p.client.Close()
}

func policySyncDefaultNewClient(ctx context.Context, host *models.Host, fingerprint string) (policySyncClient, error) {
	client := NewIncusClient(IncusClientOptions{
		URL:               host.URL,
		CertPath:          host.CertPath,
		KeyPath:           host.KeyPath,
		HostID:            int64(host.ID),
		TunnelEnabled:     host.TunnelEnabled,
		TargetHost:        host.TargetHost,
		TargetPort:        int(host.TargetPort),
		ServerFingerprint: fingerprint,
	})
	if _, err := client.Connect(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &policySyncIncusClient{client: client}, nil
}

// SyncDesiredSecurityPolicy mirrors syncDesiredSecurityPolicy(hostId, force)
// from lib/node-security/policy-sync.ts.
func SyncDesiredSecurityPolicy(ctx context.Context, hostID int32, force bool) bool {
	now := policySyncNow()
	policySyncMu.Lock()
	if policySyncInFlight[hostID] {
		policySyncMu.Unlock()
		return false
	}
	if !force {
		if last, ok := policySyncLastAt[hostID]; ok && last.Add(policySyncCooldown).After(now) {
			policySyncMu.Unlock()
			return false
		}
	}
	policySyncMu.Unlock()

	securityState, err := policySyncLoadState(ctx, hostID)
	if err != nil || securityState == nil || securityState.ServerFingerprint == nil || *securityState.ServerFingerprint == "" || securityState.RecoveryBlocked {
		return false
	}

	var report map[string]any
	if len(securityState.Report) > 0 {
		_ = json.Unmarshal(securityState.Report, &report)
	}
	if report == nil {
		return false
	}
	desiredEnabled, ok := report["_desiredEnabled"].(bool)
	if !ok {
		return false
	}
	if locked, _ := report["locked"].(bool); locked {
		return false
	}
	if enabled, ok := report["enabled"].(bool); ok && enabled == desiredEnabled {
		return false
	}

	host, err := policySyncLoadHost(ctx, hostID)
	if err != nil || host == nil {
		return false
	}
	if host.TunnelEnabled && !policySyncTunnelOn(hostID) {
		return false
	}

	policySyncMu.Lock()
	if policySyncInFlight[hostID] {
		policySyncMu.Unlock()
		return false
	}
	policySyncLastAt[hostID] = now
	policySyncInFlight[hostID] = true
	policySyncMu.Unlock()

	defer func() {
		policySyncMu.Lock()
		delete(policySyncInFlight, hostID)
		policySyncMu.Unlock()
	}()

	client, err := policySyncNewClient(ctx, host, *securityState.ServerFingerprint)
	if err != nil {
		return false
	}
	defer func() { _ = client.Close() }()

	rawLive, err := client.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
	if err != nil {
		return false
	}
	var live map[string]any
	if err := json.Unmarshal(rawLive, &live); err != nil || live == nil {
		return false
	}
	desiredCopy := desiredEnabled
	opts := db.NodeSecurityRecordReportOptions{Authoritative: true, DesiredEnabled: &desiredCopy}
	if _, err := policySyncRecordFn(ctx, hostID, map[string]any{"state": live, "events": []any{}}, opts); err != nil {
		return false
	}
	liveLocked, _ := live["locked"].(bool)
	liveEnabled, hasEnabled := live["enabled"].(bool)
	revFloat, hasRev := live["revision"].(float64)
	rev, revOK := nodeReportSafeInteger(revFloat)
	if liveLocked || (hasEnabled && liveEnabled == desiredEnabled) || !hasRev || !revOK {
		return hasEnabled && liveEnabled == desiredEnabled
	}
	rawUpdated, err := client.Request(ctx, http.MethodPost, "/1.0/agent/security/policy", map[string]any{
		"enabled":  desiredEnabled,
		"revision": rev,
	})
	if err != nil {
		return false
	}
	var updated any
	if err := json.Unmarshal(rawUpdated, &updated); err != nil {
		return false
	}
	if _, err := policySyncRecordFn(ctx, hostID, map[string]any{"state": updated, "events": []any{}}, opts); err != nil {
		return false
	}
	return true
}
