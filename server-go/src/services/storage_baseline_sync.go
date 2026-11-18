// Port of server/src/services/storage-baseline-sync.ts and the storedStoragePools
// projection from server/src/lib/storage/read-model.ts.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// StoragePoolSpaceView mirrors `space` inside storedStoragePools.
type StoragePoolSpaceView struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Available float64 `json:"available"`
}

// StoragePoolView mirrors one entry returned by storedStoragePools.
type StoragePoolView struct {
	Name          string                `json:"name"`
	Driver        string                `json:"driver"`
	Description   string                `json:"description"`
	Purpose       string                `json:"purpose"`
	Config        map[string]any        `json:"config"`
	Status        string                `json:"status"`
	PendingImport bool                  `json:"pendingImport"`
	UsedBy        float64               `json:"usedBy"`
	ReportedAt    *string               `json:"reportedAt"`
	Space         *StoragePoolSpaceView `json:"space"`
}

// StorageBaselineSyncResult mirrors StorageBaselineSyncResult in storage-baseline-sync.ts.
type StorageBaselineSyncResult struct {
	UsedBaselineEndpoint bool              `json:"usedBaselineEndpoint"`
	StorageAccepted      bool              `json:"storageAccepted"`
	Locked               bool              `json:"locked"`
	NodePoolNames        []string          `json:"nodePoolNames"`
	MissingPools         []string          `json:"missingPools"`
	UntrackedPools       []string          `json:"untrackedPools"`
	Pools                []StoragePoolView `json:"pools"`
	NodeSecuritySynced   bool              `json:"nodeSecuritySynced"`
}

// SyncHostStorageBaselineOptions mirrors SyncHostStorageBaselineOptions.
type SyncHostStorageBaselineOptions struct {
	SkipPoolReload bool
}

type baselineHostState struct {
	Host        *db.HostsRecord
	HasAgent    bool
	AgentReport []byte
}

var (
	baselineNow                    = func() time.Time { return time.Now().UTC() }
	baselineLoadHost               = loadBaselineHostState
	baselineGetClient              = lib.GetIncusClientForHost
	baselineNodeRequest            = baselineDefaultNodeRequest
	baselineLoadPoolTimestamps     = loadBaselinePoolTimestamps
	baselineRecordReport           = lib.RecordReportWithOptions
	baselineProcessStorage         = ProcessAgentStorageReport
	baselineSyncGPU                = db.GpusSyncHostReport
	baselineSaveAgentReport        = saveBaselineAgentReport
	baselineUpdateHostArchAndIPv6  = updateBaselineHostArchAndIPv6
	baselineSyncDesiredPolicyAsync = func(hostID int32) {
		go lib.SyncDesiredSecurityPolicy(context.Background(), hostID, false)
	}
	baselineLoadStoredPools = loadStoredStoragePools
)

func baselineDefaultNodeRequest(ctx context.Context, client *lib.IncusClient, path string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodGet, path, nil, 0, nil)
}

func baselineIsEndpointUnsupported(err error) bool {
	var apiErr *lib.IncusApiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound ||
		apiErr.StatusCode == http.StatusMethodNotAllowed ||
		apiErr.StatusCode == http.StatusNotImplemented
}

func loadBaselineHostState(ctx context.Context, hostID int32) (*baselineHostState, error) {
	host, err := db.HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, nil
	}
	state := &baselineHostState{
		Host: host,
	}
	if db.DB != nil {
		var agent struct {
			ID         int32  `gorm:"column:id"`
			LastReport []byte `gorm:"column:last_report"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT id, last_report FROM host_agents WHERE host_id = ? LIMIT 1`,
			hostID,
		).Scan(&agent).Error; err == nil && agent.ID != 0 {
			state.HasAgent = true
			state.AgentReport = agent.LastReport
		}
	}
	return state, nil
}

type baselinePoolTimestamps struct {
	CreatedAt  time.Time
	ReportedAt *time.Time
}

func loadBaselinePoolTimestamps(ctx context.Context, hostID int32) ([]baselinePoolTimestamps, error) {
	if db.DB == nil {
		return nil, nil
	}
	var rows []baselinePoolTimestamps
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT created_at, reported_at FROM storage_pools WHERE host_id = ?`,
		hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func computeMonotonicSyncTimestamp(ctx context.Context, hostID int32) (time.Time, error) {
	floor := baselineNow()
	rows, err := baselineLoadPoolTimestamps(ctx, hostID)
	if err != nil {
		return floor, err
	}
	for _, row := range rows {
		if row.CreatedAt.UTC().After(floor) {
			floor = row.CreatedAt.UTC()
		}
		if row.ReportedAt != nil {
			next := row.ReportedAt.UTC().Add(time.Millisecond)
			if next.After(floor) {
				floor = next
			}
		}
	}
	return floor, nil
}

func saveBaselineAgentReport(ctx context.Context, hostID int32, report map[string]any) error {
	if db.DB == nil {
		return nil
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return db.DB.WithContext(ctx).Exec(
		`UPDATE host_agents SET last_report = ?::jsonb, updated_at = ? WHERE host_id = ?`,
		string(encoded), time.Now().UTC(), hostID,
	).Error
}

func updateBaselineHostArchAndIPv6(ctx context.Context, hostID int32, architecture, ipv6 *string) error {
	if db.DB == nil || (architecture == nil && ipv6 == nil) {
		return nil
	}
	if architecture != nil {
		_ = db.DB.WithContext(ctx).Exec(
			`UPDATE hosts SET architecture = ? WHERE id = ? AND (architecture IS NULL OR architecture = '')`,
			*architecture, hostID,
		).Error
	}
	if ipv6 != nil {
		_ = db.DB.WithContext(ctx).Exec(
			`UPDATE hosts SET nat_public_ipv6 = ? WHERE id = ? AND nat_public_ipv6 IS NULL`,
			*ipv6, hostID,
		).Error
	}
	return nil
}

// StoredStoragePools mirrors storedStoragePools(hostId) in lib/storage/read-model.ts.
func StoredStoragePools(ctx context.Context, hostID int32) ([]StoragePoolView, error) {
	return baselineLoadStoredPools(ctx, hostID)
}

func loadStoredStoragePools(ctx context.Context, hostID int32) ([]StoragePoolView, error) {
	if db.DB == nil {
		return []StoragePoolView{}, nil
	}
	type poolRow struct {
		Name        string     `gorm:"column:name"`
		Driver      string     `gorm:"column:driver"`
		Description *string    `gorm:"column:description"`
		Purpose     string     `gorm:"column:purpose"`
		Config      []byte     `gorm:"column:config"`
		Status      string     `gorm:"column:status"`
		TotalBytes  int64      `gorm:"column:total_bytes"`
		UsedBytes   int64      `gorm:"column:used_bytes"`
		Telemetry   []byte     `gorm:"column:telemetry"`
		ReportedAt  *time.Time `gorm:"column:reported_at"`
	}
	var (
		rows        []poolRow
		rowsErr     error
		services    map[int32]HostServiceStatus
		servicesErr error
		wg          sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		rowsErr = db.DB.WithContext(ctx).Raw(`
			SELECT name, driver, description, purpose::text AS purpose, config, status,
			       total_bytes, used_bytes, telemetry, reported_at
			FROM storage_pools
			WHERE host_id = ?
			ORDER BY created_at ASC`, hostID).Scan(&rows).Error
	}()
	go func() {
		defer wg.Done()
		services, servicesErr = LoadHostServiceStatuses(ctx, []int32{hostID})
	}()
	wg.Wait()
	if rowsErr != nil {
		return nil, rowsErr
	}
	if servicesErr != nil {
		return nil, servicesErr
	}

	service, hasService := services[hostID]
	out := make([]StoragePoolView, 0, len(rows))
	for _, pool := range rows {
		var telemetry map[string]any
		if len(pool.Telemetry) > 0 {
			_ = json.Unmarshal(pool.Telemetry, &telemetry)
		}
		config := map[string]any{}
		if len(pool.Config) > 0 {
			_ = json.Unmarshal(pool.Config, &config)
		}
		if config == nil {
			config = map[string]any{}
		}
		if backing, ok := telemetry["backing"].(map[string]any); ok {
			if sizeBytes, ok := hostServiceJSONNumber(backing["sizeBytes"]); ok && sizeBytes != 0 {
				config["size"] = fmt.Sprintf("%gGiB", sizeBytes/math.Pow(1024, 3))
			}
		}

		total := float64(pool.TotalBytes)
		used := float64(pool.UsedBytes)
		available := math.Max(0, total-used)
		if sa, ok := hostServiceJSONNumber(telemetry["spaceAvailable"]); ok && sa >= 0 {
			available = math.Min(sa, total)
		}

		status := pool.Status
		switch {
		case pool.Status == "missing":
			status = "missing"
		case hasService && service.Status == "locked":
			status = "locked"
		case pool.Status == "ready" && (!hasService || !service.Available):
			status = "unavailable"
		}

		var reportedAt *string
		if pool.ReportedAt != nil {
			iso := pool.ReportedAt.UTC().Format("2006-01-02T15:04:05.000Z")
			reportedAt = &iso
		}
		var space *StoragePoolSpaceView
		if pool.TotalBytes > 0 {
			space = &StoragePoolSpaceView{Total: total, Used: used, Available: available}
		}
		usedBy := float64(0)
		if ub, ok := hostServiceJSONNumber(telemetry["usedBy"]); ok {
			usedBy = ub
		}
		desc := ""
		if pool.Description != nil {
			desc = *pool.Description
		}
		_, hasImport := config["_payincusImport"]
		pendingImport := false
		if hasImport && agentStorageJSTruthy(config["_payincusImport"]) {
			pendingImport = true
		}

		out = append(out, StoragePoolView{
			Name:          pool.Name,
			Driver:        pool.Driver,
			Description:   desc,
			Purpose:       pool.Purpose,
			Config:        config,
			Status:        status,
			PendingImport: pendingImport,
			UsedBy:        usedBy,
			ReportedAt:    reportedAt,
			Space:         space,
		})
	}
	return out, nil
}

func collectFallbackStoragePayload(
	ctx context.Context,
	client *lib.IncusClient,
) (map[string]any, map[string]any, error) {
	var (
		rawPools    json.RawMessage
		poolsErr    error
		rawStorage  json.RawMessage
		rawSecurity json.RawMessage
		wg          sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		rawPools, poolsErr = baselineNodeRequest(ctx, client, "/1.0/storage-pools?recursion=1")
	}()
	go func() {
		defer wg.Done()
		if raw, err := baselineNodeRequest(ctx, client, "/1.0/agent/storage"); err == nil {
			rawStorage = raw
		}
	}()
	go func() {
		defer wg.Done()
		if raw, err := baselineNodeRequest(ctx, client, "/1.0/agent/security"); err == nil {
			rawSecurity = raw
		}
	}()
	wg.Wait()

	if poolsErr != nil {
		return nil, nil, poolsErr
	}

	var incusPools []map[string]any
	_ = json.Unmarshal(rawPools, &incusPools)

	var agentStorage map[string]any
	if len(rawStorage) > 0 {
		_ = json.Unmarshal(rawStorage, &agentStorage)
	}
	var securityState map[string]any
	if len(rawSecurity) > 0 {
		_ = json.Unmarshal(rawSecurity, &securityState)
	}

	datasets, _ := agentStorage["datasets"].([]any)
	locks, _ := agentStorageIsRecord(agentStorage["locks"])
	nodeLocked := securityState != nil && securityState["locked"] == true

	pools := make([]any, 0, len(incusPools))
	for _, pool := range incusPools {
		if pool == nil {
			continue
		}
		name, ok := pool["name"].(string)
		if !ok {
			continue
		}
		driver, _ := pool["driver"].(string)
		poolStatusRaw, _ := pool["status"].(string)
		cfg, _ := agentStorageIsRecord(pool["config"])
		if cfg == nil {
			cfg = map[string]any{}
		}
		source := agentStorageJSString(cfg["zfs.pool_name"])
		if source == "" {
			source = agentStorageJSString(cfg["source"])
		}

		var space map[string]any
		if !nodeLocked {
			resPath := "/1.0/storage-pools/" + url.PathEscape(name) + "/resources"
			if rawRes, err := baselineNodeRequest(ctx, client, resPath); err == nil {
				var resBody map[string]any
				if json.Unmarshal(rawRes, &resBody) == nil {
					if spaceObj, ok := agentStorageIsRecord(resBody["space"]); ok {
						totalF, hasTotal := hostServiceJSONNumber(spaceObj["total"])
						usedF, hasUsed := hostServiceJSONNumber(spaceObj["used"])
						if hasTotal && hasUsed {
							total := math.Max(0, math.Floor(totalF))
							used := math.Max(0, math.Min(total, math.Floor(usedF)))
							avail := math.Max(0, total-used)
							if availF, hasAvail := hostServiceJSONNumber(spaceObj["available"]); hasAvail {
								avail = math.Max(0, math.Min(total, math.Floor(availF)))
							}
							space = map[string]any{
								"total":     total,
								"used":      used,
								"available": avail,
							}
						}
					}
				}
			}
		}

		var encryption map[string]any
		if driver == "zfs" {
			var matchedDS map[string]any
			for _, rawDS := range datasets {
				ds, ok := agentStorageIsRecord(rawDS)
				if ok && ds["dataset"] == source {
					matchedDS = ds
					break
				}
			}
			if matchedDS != nil {
				encRoot, _ := matchedDS["encryptionRoot"].(string)
				var lock map[string]any
				if encRoot != "" && locks != nil {
					lock, _ = agentStorageIsRecord(locks[encRoot])
				}
				encrypted := true
				if alg, ok := matchedDS["algorithm"].(string); ok && alg != "" {
					encrypted = alg != "none"
				}
				state := "locked"
				if ks, _ := matchedDS["keyStatus"].(string); ks == "available" {
					state = "unlocked"
				}
				if encRoot == "" {
					encRoot, _ = matchedDS["dataset"].(string)
				}
				pendingRecovery := 0
				if lock != nil {
					if resume, ok := lock["resumeInstances"].([]any); ok {
						pendingRecovery = len(resume)
					}
				}
				keyFormat, _ := matchedDS["keyFormat"].(string)
				encryption = map[string]any{
					"encrypted":       encrypted,
					"state":           state,
					"source":          matchedDS["dataset"],
					"encryptionRoot":  encRoot,
					"manualLocked":    lock != nil && lock["locked"] == true,
					"lockPending":     lock != nil && lock["locked"] == true && lock["complete"] != true,
					"pendingRecovery": pendingRecovery,
					"nodeLocked":      nodeLocked,
					"keyFormat":       keyFormat,
				}
			} else {
				state := "unlocked"
				if nodeLocked {
					state = "locked"
				}
				encryption = map[string]any{
					"encrypted":       false,
					"state":           state,
					"source":          source,
					"encryptionRoot":  "",
					"manualLocked":    false,
					"lockPending":     false,
					"pendingRecovery": 0,
					"nodeLocked":      nodeLocked,
					"keyFormat":       "",
				}
			}
		}

		poolStatus := "unavailable"
		if nodeLocked {
			poolStatus = "locked"
		} else if poolStatusRaw != "" && poolStatusRaw != "Created" {
			poolStatus = "unavailable"
		} else if space != nil {
			if total, ok := hostServiceJSONNumber(space["total"]); ok && total > 0 {
				poolStatus = "ready"
			}
		}

		usedBy := 0
		if ub, ok := pool["used_by"].([]any); ok {
			usedBy = len(ub)
		}
		entry := map[string]any{
			"name":   name,
			"driver": driver,
			"status": poolStatus,
			"usedBy": usedBy,
			"config": cfg,
		}
		if space != nil {
			entry["space"] = space
		}
		if encryption != nil {
			entry["encryption"] = encryption
		}
		pools = append(pools, entry)
	}

	storage := map[string]any{
		"available":          true,
		"complete":           true,
		"collectionComplete": true,
		"pools":              pools,
	}
	if len(datasets) > 0 {
		storage["datasets"] = datasets
	}
	if len(locks) > 0 {
		storage["locks"] = locks
	}
	return storage, securityState, nil
}

// SyncHostStorageBaseline mirrors syncHostStorageBaseline in storage-baseline-sync.ts.
func SyncHostStorageBaseline(
	ctx context.Context,
	hostID int32,
	existingClient *lib.IncusClient,
	options ...SyncHostStorageBaselineOptions,
) (StorageBaselineSyncResult, error) {
	var opts SyncHostStorageBaselineOptions
	if len(options) > 0 {
		opts = options[0]
	}

	state, err := baselineLoadHost(ctx, hostID)
	if err != nil {
		return StorageBaselineSyncResult{}, err
	}
	if state == nil || state.Host == nil {
		return StorageBaselineSyncResult{}, errors.New("宿主机不存在")
	}

	client := existingClient
	if client == nil {
		client, err = baselineGetClient(ctx, state.Host)
		if err != nil {
			return StorageBaselineSyncResult{}, err
		}
	}

	usedBaselineEndpoint := false
	var (
		baselinePayload map[string]any
		storagePayload  map[string]any
		securityState   map[string]any
	)

	rawBaseline, err := baselineNodeRequest(ctx, client, "/1.0/agent/baseline")
	if err != nil {
		if !baselineIsEndpointUnsupported(err) {
			return StorageBaselineSyncResult{}, err
		}
	} else {
		usedBaselineEndpoint = true
		_ = json.Unmarshal(rawBaseline, &baselinePayload)
		if st, ok := agentStorageIsRecord(baselinePayload["storage"]); ok {
			copied := make(map[string]any, len(st))
			for k, v := range st {
				copied[k] = v
			}
			storagePayload = copied
		}
		if ns, ok := agentStorageIsRecord(baselinePayload["nodeSecurity"]); ok {
			if secState, ok := agentStorageIsRecord(ns["state"]); ok {
				securityState = secState
			}
		}
	}

	if storagePayload == nil {
		fallbackStorage, fallbackSec, err := collectFallbackStoragePayload(ctx, client)
		if err != nil {
			return StorageBaselineSyncResult{}, err
		}
		storagePayload = fallbackStorage
		if securityState == nil {
			securityState = fallbackSec
		}
	}

	syncTime, err := computeMonotonicSyncTimestamp(ctx, hostID)
	if err != nil {
		return StorageBaselineSyncResult{}, err
	}
	storagePayload["reportedAt"] = syncTime.UTC().Format("2006-01-02T15:04:05.000Z")

	lib.RecordHostSuccess(hostID, 0)

	nodeSecuritySynced := false
	locked := securityState != nil && securityState["locked"] == true
	if securityState != nil {
		if _, hasEnabled := securityState["enabled"].(bool); hasEnabled {
			if _, hasLocked := securityState["locked"].(bool); hasLocked {
				if rev, hasRev := hostServiceJSONNumber(securityState["revision"]); hasRev && rev == math.Trunc(rev) && rev >= 0 {
					if _, err := baselineRecordReport(ctx, hostID, map[string]any{"state": securityState, "events": []any{}}, db.NodeSecurityRecordReportOptions{Authoritative: true}); err != nil {
						return StorageBaselineSyncResult{}, err
					}
					nodeSecuritySynced = true
					locked = securityState["locked"] == true
				}
			}
		}
	}

	if baselinePayload != nil && !locked && state.HasAgent {
		if baselineResources, ok := agentStorageIsRecord(baselinePayload["resources"]); ok {
			var prevReport map[string]any
			if len(state.AgentReport) > 0 {
				_ = json.Unmarshal(state.AgentReport, &prevReport)
			}
			if prevReport == nil {
				prevReport = map[string]any{}
			}
			prevResources, _ := agentStorageIsRecord(prevReport["resources"])
			mergedResources := make(map[string]any, len(prevResources)+len(baselineResources))
			for k, v := range prevResources {
				mergedResources[k] = v
			}
			for k, v := range baselineResources {
				mergedResources[k] = v
			}
			mergedReport := make(map[string]any, len(prevReport)+4)
			for k, v := range prevReport {
				mergedReport[k] = v
			}
			for _, section := range []string{"runtime", "incus", "caddy"} {
				if obj, ok := agentStorageIsRecord(baselinePayload[section]); ok {
					mergedReport[section] = obj
				}
			}
			mergedReport["resources"] = mergedResources
			_ = baselineSaveAgentReport(ctx, hostID, mergedReport)

			if nvidiaGPU, present := baselineResources["nvidiaGpu"]; present {
				_ = baselineSyncGPU(ctx, hostID, nvidiaGPU)
			}
			var archPtr *string
			if state.Host.Architecture == "" {
				if rt, ok := agentStorageIsRecord(baselinePayload["runtime"]); ok {
					switch rt["goarch"] {
					case "arm64":
						v := "aarch64"
						archPtr = &v
					case "amd64":
						v := "x86_64"
						archPtr = &v
					}
				}
			}
			var ipv6Ptr *string
			if state.Host.NATPublicIPv6 == nil {
				ipv6Ptr = lib.ExtractHostPublicIPv6FromResources(baselineResources)
			}
			if archPtr != nil || ipv6Ptr != nil {
				_ = baselineUpdateHostArchAndIPv6(ctx, hostID, archPtr, ipv6Ptr)
			}
		}
	}

	storageAccepted, err := baselineProcessStorage(ctx, hostID, storagePayload, syncTime)
	if err != nil {
		return StorageBaselineSyncResult{}, err
	}

	if !locked && nodeSecuritySynced {
		baselineSyncDesiredPolicyAsync(hostID)
	}

	pools := []StoragePoolView{}
	if !opts.SkipPoolReload {
		pools, err = baselineLoadStoredPools(ctx, hostID)
		if err != nil {
			return StorageBaselineSyncResult{}, err
		}
	}

	dbPoolNames := make(map[string]struct{}, len(pools))
	missingPools := []string{}
	for _, p := range pools {
		dbPoolNames[p.Name] = struct{}{}
		if p.Status == "missing" {
			missingPools = append(missingPools, p.Name)
		}
	}

	nodePoolNames := []string{}
	if rawPools, ok := storagePayload["pools"].([]any); ok {
		for _, item := range rawPools {
			if obj, ok := agentStorageIsRecord(item); ok {
				if name, ok := obj["name"].(string); ok && name != "" {
					nodePoolNames = append(nodePoolNames, name)
				}
			}
		}
	}

	untrackedPools := []string{}
	if !opts.SkipPoolReload {
		for _, name := range nodePoolNames {
			if _, exists := dbPoolNames[name]; !exists {
				untrackedPools = append(untrackedPools, name)
			}
		}
	}

	return StorageBaselineSyncResult{
		UsedBaselineEndpoint: usedBaselineEndpoint,
		StorageAccepted:      storageAccepted,
		Locked:               locked,
		NodePoolNames:        nodePoolNames,
		MissingPools:         missingPools,
		UntrackedPools:       untrackedPools,
		Pools:                pools,
		NodeSecuritySynced:   nodeSecuritySynced,
	}, nil
}
