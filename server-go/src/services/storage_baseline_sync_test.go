package services

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestSyncHostStorageBaselineEndpointAndFallback(t *testing.T) {
	origNow := baselineNow
	origLoadHost := baselineLoadHost
	origGetClient := baselineGetClient
	origRequest := baselineNodeRequest
	origTimestamps := baselineLoadPoolTimestamps
	origRecord := baselineRecordReport
	origProcess := baselineProcessStorage
	origSyncGPU := baselineSyncGPU
	origSaveAgent := baselineSaveAgentReport
	origUpdateHost := baselineUpdateHostArchAndIPv6
	origPolicy := baselineSyncDesiredPolicyAsync
	origLoadPools := baselineLoadStoredPools
	defer func() {
		baselineNow = origNow
		baselineLoadHost = origLoadHost
		baselineGetClient = origGetClient
		baselineNodeRequest = origRequest
		baselineLoadPoolTimestamps = origTimestamps
		baselineRecordReport = origRecord
		baselineProcessStorage = origProcess
		baselineSyncGPU = origSyncGPU
		baselineSaveAgentReport = origSaveAgent
		baselineUpdateHostArchAndIPv6 = origUpdateHost
		baselineSyncDesiredPolicyAsync = origPolicy
		baselineLoadStoredPools = origLoadPools
	}()

	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	baselineNow = func() time.Time { return now }
	baselineLoadHost = func(context.Context, int32) (*baselineHostState, error) {
		return &baselineHostState{
			Host:        &db.HostsRecord{ID: 9, Name: "n9"},
			HasAgent:    true,
			AgentReport: []byte(`{"resources":{"cpu":{"cores":8}}}`),
		}, nil
	}
	baselineGetClient = func(context.Context, *db.HostsRecord) (*lib.IncusClient, error) {
		return &lib.IncusClient{}, nil
	}
	futureReported := now.Add(5 * time.Second)
	baselineLoadPoolTimestamps = func(context.Context, int32) ([]baselinePoolTimestamps, error) {
		return []baselinePoolTimestamps{{CreatedAt: now.Add(-time.Hour), ReportedAt: &futureReported}}, nil
	}

	var (
		capturedReportTime time.Time
		policyTriggered    bool
		gpuSynced          bool
		updatedArch        string
		updatedIPv6        string
		savedResources     map[string]any
	)
	baselineRecordReport = func(_ context.Context, _ int32, _ any, opts db.NodeSecurityRecordReportOptions) ([]string, error) {
		if !opts.Authoritative {
			t.Fatal("baseline sync must record authoritative security state")
		}
		return nil, nil
	}
	baselineSaveAgentReport = func(_ context.Context, _ int32, report map[string]any) error {
		savedResources, _ = report["resources"].(map[string]any)
		return nil
	}
	baselineSyncGPU = func(context.Context, int32, any) error {
		gpuSynced = true
		return nil
	}
	baselineUpdateHostArchAndIPv6 = func(_ context.Context, _ int32, arch, ipv6 *string) error {
		if arch != nil {
			updatedArch = *arch
		}
		if ipv6 != nil {
			updatedIPv6 = *ipv6
		}
		return nil
	}
	baselineProcessStorage = func(_ context.Context, _ int32, payload any, sampledAt time.Time) (bool, error) {
		capturedReportTime = sampledAt
		if payload == nil {
			t.Fatal("expected storage payload")
		}
		return true, nil
	}
	baselineSyncDesiredPolicyAsync = func(int32) { policyTriggered = true }
	baselineLoadStoredPools = func(context.Context, int32) ([]StoragePoolView, error) {
		return []StoragePoolView{
			{Name: "default", Driver: "zfs", Status: "ready"},
			{Name: "old-pool", Driver: "dir", Status: "missing"},
		}, nil
	}

	// 1. Primary /1.0/agent/baseline path.
	baselineNodeRequest = func(_ context.Context, _ *lib.IncusClient, path string) (json.RawMessage, error) {
		if path != "/1.0/agent/baseline" {
			t.Fatalf("unexpected path %q", path)
		}
		return json.RawMessage(`{
			"runtime": {"goarch": "arm64"},
			"storage": {"available": true, "complete": true, "pools": [{"name": "default"}, {"name": "extra"}]},
			"nodeSecurity": {"state": {"enabled": true, "locked": false, "revision": 3}},
			"resources": {
				"nvidiaGpu": {"status": "unavailable"},
				"network": {"cards": [{"addresses": [{"family": "inet6", "scope": "global", "address": "2001:db8::1"}]}]}
			}
		}`), nil
	}

	res, err := SyncHostStorageBaseline(context.Background(), 9, nil)
	if err != nil {
		t.Fatalf("SyncHostStorageBaseline error: %v", err)
	}
	if !res.UsedBaselineEndpoint || !res.StorageAccepted || !res.NodeSecuritySynced || !policyTriggered {
		t.Fatalf("unexpected result flags: %#v policy=%v", res, policyTriggered)
	}
	if !capturedReportTime.Equal(futureReported.Add(time.Millisecond)) {
		t.Fatalf("monotonic syncTime = %v, want %v", capturedReportTime, futureReported.Add(time.Millisecond))
	}
	if !gpuSynced || updatedArch != "aarch64" || updatedIPv6 != "2001:db8::1" || savedResources["cpu"] == nil {
		t.Fatalf("gpu=%v arch=%q ipv6=%q saved=%#v", gpuSynced, updatedArch, updatedIPv6, savedResources)
	}
	if len(res.MissingPools) != 1 || res.MissingPools[0] != "old-pool" {
		t.Fatalf("missingPools = %#v", res.MissingPools)
	}
	if len(res.UntrackedPools) != 1 || res.UntrackedPools[0] != "extra" {
		t.Fatalf("untrackedPools = %#v", res.UntrackedPools)
	}

	// 2. Fallback path on 404 from /1.0/agent/baseline.
	policyTriggered = false
	baselineNodeRequest = func(_ context.Context, _ *lib.IncusClient, path string) (json.RawMessage, error) {
		switch path {
		case "/1.0/agent/baseline":
			return nil, lib.NewIncusAPIError("not found", http.StatusNotFound, nil)
		case "/1.0/storage-pools?recursion=1":
			return json.RawMessage(`[{"name":"default","driver":"dir","status":"Created","config":{"source":"/var/lib/incus/storage-pools/default"}}]`), nil
		case "/1.0/agent/storage":
			return json.RawMessage(`{}`), nil
		case "/1.0/agent/security":
			return json.RawMessage(`{"enabled":false,"locked":false,"revision":2}`), nil
		default:
			if strings.HasSuffix(path, "/resources") {
				return json.RawMessage(`{"space":{"total":1000,"used":200,"available":800}}`), nil
			}
			return nil, lib.NewIncusAPIError("unexpected", 500, nil)
		}
	}
	res2, err := SyncHostStorageBaseline(context.Background(), 9, &lib.IncusClient{}, SyncHostStorageBaselineOptions{SkipPoolReload: true})
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if res2.UsedBaselineEndpoint || !res2.NodeSecuritySynced || !policyTriggered || len(res2.NodePoolNames) != 1 || res2.NodePoolNames[0] != "default" {
		t.Fatalf("unexpected fallback result: %#v", res2)
	}
}
