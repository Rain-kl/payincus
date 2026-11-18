// Port of server/src/services/host-diagnostics.ts.
//
// Splits control-plane connectivity checks (Agent heartbeat freshness, WebSocket
// reverse tunnel RTT ping, fingerprint-pinned Incus mTLS handshake, live node
// security sync, and storage baseline diff) from live node telemetry.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// DiagnosticItemStatus mirrors DiagnosticItemStatus.
type DiagnosticItemStatus string

const (
	DiagnosticStatusPass    DiagnosticItemStatus = "pass"
	DiagnosticStatusWarn    DiagnosticItemStatus = "warn"
	DiagnosticStatusFail    DiagnosticItemStatus = "fail"
	DiagnosticStatusSkipped DiagnosticItemStatus = "skipped"
)

// DiagnosticCheckItem mirrors DiagnosticCheckItem in host-diagnostics.ts.
type DiagnosticCheckItem struct {
	Key       string               `json:"key"`
	Label     string               `json:"label"`
	Status    DiagnosticItemStatus `json:"status"`
	OK        bool                 `json:"ok"`
	LatencyMs *int64               `json:"latencyMs,omitempty"`
	Detail    string               `json:"detail"`
	Meta      map[string]any       `json:"meta,omitempty"`
}

// HostDiagnosticChecks mirrors the `checks` object in HostDiagnosticResult.
type HostDiagnosticChecks struct {
	AgentHeartbeat  DiagnosticCheckItem `json:"agentHeartbeat"`
	ReverseTunnel   DiagnosticCheckItem `json:"reverseTunnel"`
	IncusMtls       DiagnosticCheckItem `json:"incusMtls"`
	NodeSecurity    DiagnosticCheckItem `json:"nodeSecurity"`
	StorageBaseline DiagnosticCheckItem `json:"storageBaseline"`
}

// HostDiagnosticResult mirrors HostDiagnosticResult in host-diagnostics.ts.
type HostDiagnosticResult struct {
	HostID            int32                `json:"hostId"`
	HostName          string               `json:"hostName"`
	CheckedAt         string               `json:"checkedAt"`
	OK                bool                 `json:"ok"`
	OverallStatus     string               `json:"overallStatus"`
	Summary           string               `json:"summary"`
	NeedsBaselineSync bool                 `json:"needsBaselineSync"`
	Checks            HostDiagnosticChecks `json:"checks"`
}

type diagnosticPoolRow struct {
	Name   string
	Driver string
	Status string
	Config map[string]any
}

type diagnosticHostSnapshot struct {
	HostID            int32
	HostName          string
	URL               string
	CertPath          *string
	KeyPath           *string
	TunnelEnabled     bool
	TargetHost        string
	TargetPort        int32
	HasAgent          bool
	AgentStatus       string
	AgentVersion      *string
	AgentLastSeenAt   *time.Time
	AgentLastIP       *string
	AgentLastReport   map[string]any
	ServerFingerprint *string
	SecurityReport    map[string]any
	Pools             []diagnosticPoolRow
}

type diagnosticClient interface {
	Connect(ctx context.Context) error
	Request(ctx context.Context, method, path string) (json.RawMessage, error)
	Close() error
}

type diagnosticIncusClient struct {
	client *lib.IncusClient
}

func (d *diagnosticIncusClient) Connect(ctx context.Context) error {
	_, err := d.client.Connect(ctx)
	return err
}

func (d *diagnosticIncusClient) Request(ctx context.Context, method, path string) (json.RawMessage, error) {
	return d.client.Request(ctx, method, path, nil, 0, nil)
}

func (d *diagnosticIncusClient) Close() error {
	return d.client.Close()
}

var (
	diagnosticNow         = func() time.Time { return time.Now().UTC() }
	diagnosticLoadHost    = loadDiagnosticHostSnapshot
	diagnosticMeasurePing = func(ctx context.Context, hostID int32) lib.TunnelPingResult {
		return lib.TunnelManagerInstance().MeasureTunnelPing(ctx, hostID, 3500*time.Millisecond)
	}
	diagnosticNewClient = func(snap *diagnosticHostSnapshot) diagnosticClient {
		opts := lib.IncusClientOptions{
			URL:           snap.URL,
			CertPath:      snap.CertPath,
			KeyPath:       snap.KeyPath,
			HostID:        int64(snap.HostID),
			TunnelEnabled: snap.TunnelEnabled,
			TargetHost:    snap.TargetHost,
			TargetPort:    int(snap.TargetPort),
		}
		if snap.ServerFingerprint != nil && *snap.ServerFingerprint != "" {
			opts.ServerFingerprint = *snap.ServerFingerprint
		}
		return &diagnosticIncusClient{client: lib.NewIncusClient(opts)}
	}
	diagnosticRecordSuccess = lib.RecordHostSuccess
	diagnosticRecordReport  = lib.RecordReportWithOptions
	diagnosticSyncPolicy    = lib.SyncDesiredSecurityPolicy
	diagnosticFpMismatchRe  = regexp.MustCompile(`(?i)指纹|fingerprint`)
)

func loadDiagnosticHostSnapshot(ctx context.Context, hostID int32) (*diagnosticHostSnapshot, error) {
	host, err := db.HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, nil
	}
	snap := &diagnosticHostSnapshot{
		HostID:        host.ID,
		HostName:      host.Name,
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		TargetHost:    host.TargetHost,
		TargetPort:    host.TargetPort,
		Pools:         []diagnosticPoolRow{},
	}
	if db.DB != nil {
		var agent struct {
			ID         int32      `gorm:"column:id"`
			Status     string     `gorm:"column:status"`
			Version    *string    `gorm:"column:version"`
			LastSeenAt *time.Time `gorm:"column:last_seen_at"`
			LastIP     *string    `gorm:"column:last_heartbeat_ip"`
			LastReport []byte     `gorm:"column:last_report"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT id, status::text AS status, version, last_seen_at, last_heartbeat_ip, last_report
			 FROM host_agents WHERE host_id = ? LIMIT 1`, hostID,
		).Scan(&agent).Error; err == nil && agent.ID != 0 {
			snap.HasAgent = true
			snap.AgentStatus = agent.Status
			snap.AgentVersion = agent.Version
			snap.AgentLastSeenAt = agent.LastSeenAt
			snap.AgentLastIP = agent.LastIP
			if len(agent.LastReport) > 0 {
				var rep map[string]any
				if json.Unmarshal(agent.LastReport, &rep) == nil {
					snap.AgentLastReport = rep
				}
			}
		}

		var sec struct {
			Fingerprint *string `gorm:"column:server_fingerprint"`
			Report      []byte  `gorm:"column:report"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT server_fingerprint, report FROM node_security_states WHERE host_id = ? LIMIT 1`, hostID,
		).Scan(&sec).Error; err == nil {
			snap.ServerFingerprint = sec.Fingerprint
			if len(sec.Report) > 0 {
				var rep map[string]any
				if json.Unmarshal(sec.Report, &rep) == nil {
					snap.SecurityReport = rep
				}
			}
		}

		var poolRows []struct {
			Name   string `gorm:"column:name"`
			Driver string `gorm:"column:driver"`
			Status string `gorm:"column:status"`
			Config []byte `gorm:"column:config"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT name, driver, status, config FROM storage_pools WHERE host_id = ? ORDER BY created_at ASC`, hostID,
		).Scan(&poolRows).Error; err == nil {
			for _, pr := range poolRows {
				var cfg map[string]any
				if len(pr.Config) > 0 {
					_ = json.Unmarshal(pr.Config, &cfg)
				}
				if cfg == nil {
					cfg = map[string]any{}
				}
				snap.Pools = append(snap.Pools, diagnosticPoolRow{
					Name:   pr.Name,
					Driver: pr.Driver,
					Status: pr.Status,
					Config: cfg,
				})
			}
		}
	}
	return snap, nil
}

func diagnosticWithTimeout[T any](ctx context.Context, d time.Duration, message string, fn func(context.Context) (T, error)) (T, error) {
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn(tctx)
		ch <- result{val: v, err: err}
	}()
	select {
	case res := <-ch:
		return res.val, res.err
	case <-tctx.Done():
		var zero T
		if errors.Is(ctx.Err(), context.Canceled) {
			return zero, ctx.Err()
		}
		return zero, errors.New(message)
	}
}

func diagnosticEvaluateHeartbeat(snap *diagnosticHostSnapshot, now time.Time) DiagnosticCheckItem {
	if !snap.HasAgent {
		return DiagnosticCheckItem{
			Key:    "agentHeartbeat",
			Label:  "Agent 心跳上报",
			Status: DiagnosticStatusFail,
			OK:     false,
			Detail: "尚未绑定或安装节点 Agent",
		}
	}
	intervalSec := 5.0
	if metrics, ok := agentStorageIsRecord(snap.AgentLastReport["metrics"]); ok {
		if val, ok := hostServiceJSONNumber(metrics["heartbeatIntervalSeconds"]); ok && val > 0 {
			intervalSec = val
		}
	}
	staleThreshold := time.Duration(math.Max(intervalSec*3*1000, 35000)) * time.Millisecond
	var (
		age      time.Duration
		hasAge   bool
		lastSeen any
	)
	if snap.AgentLastSeenAt != nil {
		hasAge = true
		age = now.Sub(snap.AgentLastSeenAt.UTC())
		if age < 0 {
			age = 0
		}
		lastSeen = snap.AgentLastSeenAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	fresh := hasAge && age <= staleThreshold && snap.AgentStatus == "online"
	status := DiagnosticStatusFail
	if fresh {
		status = DiagnosticStatusPass
	}

	versionText := "未知"
	var versionMeta any
	if snap.AgentVersion != nil && *snap.AgentVersion != "" {
		versionText = *snap.AgentVersion
		versionMeta = *snap.AgentVersion
	}
	var lastIPMeta any
	if snap.AgentLastIP != nil && *snap.AgentLastIP != "" {
		lastIPMeta = *snap.AgentLastIP
	}
	var ageSeconds any
	if hasAge {
		ageSeconds = int64(math.Round(age.Seconds()))
	}

	detail := "尚未收到过节点心跳"
	if hasAge {
		ageSec := int64(math.Round(age.Seconds()))
		if fresh {
			detail = fmt.Sprintf("在线（%d 秒前心跳，版本 %s）", ageSec, versionText)
		} else {
			detail = fmt.Sprintf("心跳超时（已失联 %d 秒，阈值 %d 秒）", ageSec, int64(math.Round(staleThreshold.Seconds())))
		}
	}

	return DiagnosticCheckItem{
		Key:    "agentHeartbeat",
		Label:  "Agent 心跳上报",
		Status: status,
		OK:     fresh,
		Detail: detail,
		Meta: map[string]any{
			"version":         versionMeta,
			"lastSeenAt":      lastSeen,
			"lastHeartbeatIp": lastIPMeta,
			"ageSeconds":      ageSeconds,
		},
	}
}

// RunHostDiagnostics mirrors runHostDiagnostics in services/host-diagnostics.ts.
func RunHostDiagnostics(ctx context.Context, hostID int32) (HostDiagnosticResult, error) {
	snap, err := diagnosticLoadHost(ctx, hostID)
	if err != nil {
		return HostDiagnosticResult{}, err
	}
	if snap == nil {
		return HostDiagnosticResult{}, errors.New("宿主机不存在")
	}

	now := diagnosticNow()
	agentHeartbeat := diagnosticEvaluateHeartbeat(snap, now)

	var reverseTunnel DiagnosticCheckItem
	if !snap.TunnelEnabled {
		reverseTunnel = DiagnosticCheckItem{
			Key:    "reverseTunnel",
			Label:  "反向 WebSocket 隧道",
			Status: DiagnosticStatusSkipped,
			OK:     true,
			Detail: "当前使用直连 mTLS 模式",
		}
	} else {
		ping := diagnosticMeasurePing(ctx, hostID)
		status := DiagnosticStatusFail
		detail := "反向隧道未连接"
		if ping.Error != nil && *ping.Error != "" {
			detail = *ping.Error
		}
		if ping.OK {
			status = DiagnosticStatusPass
			if ping.LatencyMs != nil {
				detail = fmt.Sprintf("隧道在线 · RTT %d ms", *ping.LatencyMs)
			} else {
				detail = "隧道在线"
			}
		}
		reverseTunnel = DiagnosticCheckItem{
			Key:       "reverseTunnel",
			Label:     "反向 WebSocket 隧道",
			Status:    status,
			OK:        ping.OK,
			LatencyMs: ping.LatencyMs,
			Detail:    detail,
		}
	}

	dbSecurityReport := snap.SecurityReport
	dbPools := make([]diagnosticPoolRow, 0, len(snap.Pools))
	for _, p := range snap.Pools {
		if _, deleting := p.Config["_payincusDeletion"]; deleting {
			continue
		}
		dbPools = append(dbPools, p)
	}

	var (
		incusMtls         DiagnosticCheckItem
		nodeSecurity      DiagnosticCheckItem
		storageBaseline   DiagnosticCheckItem
		needsBaselineSync bool
	)

	if snap.TunnelEnabled && !reverseTunnel.OK {
		cachedLocked := dbSecurityReport != nil && dbSecurityReport["locked"] == true
		cachedEnabled := dbSecurityReport != nil && dbSecurityReport["enabled"] == true
		incusMtls = DiagnosticCheckItem{
			Key:    "incusMtls",
			Label:  "Incus mTLS 控制通道",
			Status: DiagnosticStatusFail,
			OK:     false,
			Detail: "因反向隧道未在线，无法建立 mTLS 通道",
		}
		secStatus := DiagnosticStatusFail
		secDetail := "无法连接节点核验实时安全状态"
		if cachedLocked {
			secStatus = DiagnosticStatusWarn
			secDetail = "节点断开前处于安全锁定状态，无法实时核验"
		}
		nodeSecurity = DiagnosticCheckItem{
			Key:    "nodeSecurity",
			Label:  "安全模式与节点状态",
			Status: secStatus,
			OK:     false,
			Detail: secDetail,
			Meta: map[string]any{
				"cachedLocked":  cachedLocked,
				"cachedEnabled": cachedEnabled,
			},
		}
		missingInCache := make([]string, 0)
		for _, p := range dbPools {
			if p.Status == "missing" {
				missingInCache = append(missingInCache, p.Name)
			}
		}
		storageStatus := DiagnosticStatusFail
		if len(missingInCache) > 0 {
			storageStatus = DiagnosticStatusWarn
		}
		storageBaseline = DiagnosticCheckItem{
			Key:    "storageBaseline",
			Label:  "存储池基线一致性",
			Status: storageStatus,
			OK:     false,
			Detail: "无法连接节点比对实时存储池，当前仅展示缓存状态",
			Meta: map[string]any{
				"cachedPoolCount": len(dbPools),
				"missingInCache":  missingInCache,
			},
		}
	} else {
		client := diagnosticNewClient(snap)
		defer func() { _ = client.Close() }()

		mTLSStart := diagnosticNow()
		_, connectErr := diagnosticWithTimeout(ctx, 5500*time.Millisecond, "mTLS 握手连接节点超时 (5.5s)", func(cctx context.Context) (struct{}, error) {
			return struct{}{}, client.Connect(cctx)
		})
		if connectErr != nil {
			incusMtls, nodeSecurity, storageBaseline = diagnosticBuildConnectFailure(connectErr, mTLSStart)
		} else {
			agentInfoRaw, _ := diagnosticWithTimeout(ctx, 4000*time.Millisecond, "读取 /1.0/agent 超时", func(cctx context.Context) (json.RawMessage, error) {
				return client.Request(cctx, http.MethodGet, "/1.0/agent")
			})
			var agentInfo struct {
				APIExtensions []string `json:"api_extensions"`
			}
			if len(agentInfoRaw) > 0 {
				_ = json.Unmarshal(agentInfoRaw, &agentInfo)
			}
			if agentInfo.APIExtensions == nil {
				agentInfo.APIExtensions = []string{}
			}

			var (
				baselinePayload   map[string]any
				liveSecurityState map[string]any
				liveStoragePools  []map[string]any
				baselineErr       error
			)
			rawBaseline, bErr := diagnosticWithTimeout(ctx, 6000*time.Millisecond, "读取节点基线超时 (6s)", func(cctx context.Context) (json.RawMessage, error) {
				return client.Request(cctx, http.MethodGet, "/1.0/agent/baseline")
			})
			if bErr != nil {
				var apiErr *lib.IncusApiError
				if errors.As(bErr, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) {
					var (
						secRaw   json.RawMessage
						poolsRaw json.RawMessage
						wg       sync.WaitGroup
					)
					wg.Add(2)
					go func() {
						defer wg.Done()
						secRaw, _ = diagnosticWithTimeout(ctx, 4000*time.Millisecond, "读取安全状态超时", func(cctx context.Context) (json.RawMessage, error) {
							return client.Request(cctx, http.MethodGet, "/1.0/agent/security")
						})
					}()
					go func() {
						defer wg.Done()
						poolsRaw, _ = diagnosticWithTimeout(ctx, 4000*time.Millisecond, "读取存储池列表超时", func(cctx context.Context) (json.RawMessage, error) {
							return client.Request(cctx, http.MethodGet, "/1.0/storage-pools?recursion=1")
						})
					}()
					wg.Wait()
					if len(secRaw) > 0 {
						_ = json.Unmarshal(secRaw, &liveSecurityState)
					}
					if len(poolsRaw) > 0 {
						_ = json.Unmarshal(poolsRaw, &liveStoragePools)
					}
				} else {
					baselineErr = bErr
				}
			} else {
				_ = json.Unmarshal(rawBaseline, &baselinePayload)
			}

			if baselineErr != nil {
				incusMtls, nodeSecurity, storageBaseline = diagnosticBuildConnectFailure(baselineErr, mTLSStart)
			} else {
				latencyMs := diagnosticNow().Sub(mTLSStart).Milliseconds()
				if latencyMs < 0 {
					latencyMs = 0
				}
				diagnosticRecordSuccess(hostID, latencyMs)
				fpPinned := snap.ServerFingerprint != nil && *snap.ServerFingerprint != ""
				fpNote := "（尚未核对绑定证书指纹）"
				if fpPinned {
					fpNote = "（已校验证书指纹）"
				}
				incusMtls = DiagnosticCheckItem{
					Key:       "incusMtls",
					Label:     "Incus mTLS 控制通道",
					Status:    DiagnosticStatusPass,
					OK:        true,
					LatencyMs: &latencyMs,
					Detail:    fmt.Sprintf("mTLS 验证通过 · %d ms%s", latencyMs, fpNote),
					Meta: map[string]any{
						"fingerprintPinned": fpPinned,
						"apiExtensions":     agentInfo.APIExtensions,
					},
				}

				if baselinePayload != nil {
					if ns, ok := agentStorageIsRecord(baselinePayload["nodeSecurity"]); ok {
						if st, ok := agentStorageIsRecord(ns["state"]); ok {
							liveSecurityState = st
						}
					}
					if st, ok := agentStorageIsRecord(baselinePayload["storage"]); ok {
						if rawPools, ok := st["pools"].([]any); ok {
							liveStoragePools = make([]map[string]any, 0, len(rawPools))
							for _, item := range rawPools {
								if obj, ok := agentStorageIsRecord(item); ok {
									liveStoragePools = append(liveStoragePools, obj)
								}
							}
						}
					}
				}

				nodeSecurity = diagnosticEvaluateSecurity(ctx, hostID, snap, dbSecurityReport, liveSecurityState)
				storageBaseline, needsBaselineSync = diagnosticEvaluateStorageBaseline(dbPools, liveStoragePools)
			}
		}
	}

	allItems := []DiagnosticCheckItem{agentHeartbeat, reverseTunnel, incusMtls, nodeSecurity, storageBaseline}
	hasFail := false
	hasWarn := false
	for _, item := range allItems {
		if item.Status == DiagnosticStatusFail {
			hasFail = true
		} else if item.Status == DiagnosticStatusWarn {
			hasWarn = true
		}
	}
	ok := !hasFail && !hasWarn
	overallStatus := "healthy"
	summary := "所有连接与基线检查均已通过"
	if hasFail {
		overallStatus = "error"
		summary = "节点链路存在故障项，请查看下方逐项诊断"
	} else if hasWarn {
		overallStatus = "warning"
		summary = "链路正常，但存在需要注意的状态或基线差异"
	}

	return HostDiagnosticResult{
		HostID:            hostID,
		HostName:          snap.HostName,
		CheckedAt:         diagnosticNow().Format("2006-01-02T15:04:05.000Z"),
		OK:                ok,
		OverallStatus:     overallStatus,
		Summary:           summary,
		NeedsBaselineSync: needsBaselineSync,
		Checks: HostDiagnosticChecks{
			AgentHeartbeat:  agentHeartbeat,
			ReverseTunnel:   reverseTunnel,
			IncusMtls:       incusMtls,
			NodeSecurity:    nodeSecurity,
			StorageBaseline: storageBaseline,
		},
	}, nil
}

func diagnosticBuildConnectFailure(err error, mTLSStart time.Time) (DiagnosticCheckItem, DiagnosticCheckItem, DiagnosticCheckItem) {
	detail := lib.NodeErrorDetail(err, "mTLS 连接或状态查询失败", nil)
	fpMismatch := diagnosticFpMismatchRe.MatchString(detail)
	latencyMs := diagnosticNow().Sub(mTLSStart).Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}
	fullDetail := detail
	if fpMismatch {
		fullDetail = detail + "（若节点已重装，请在「运维 -> 核对证书指纹」更新指纹）"
	}
	return DiagnosticCheckItem{
			Key:       "incusMtls",
			Label:     "Incus mTLS 控制通道",
			Status:    DiagnosticStatusFail,
			OK:        false,
			LatencyMs: &latencyMs,
			Detail:    fullDetail,
			Meta:      map[string]any{"fingerprintMismatch": fpMismatch},
		},
		DiagnosticCheckItem{
			Key:    "nodeSecurity",
			Label:  "安全模式与节点状态",
			Status: DiagnosticStatusFail,
			OK:     false,
			Detail: "因 mTLS 通道未通过，无法核验节点实时安全状态",
		},
		DiagnosticCheckItem{
			Key:    "storageBaseline",
			Label:  "存储池基线一致性",
			Status: DiagnosticStatusFail,
			OK:     false,
			Detail: "因 mTLS 通道未通过，无法比对存储池基线",
		}
}

func diagnosticEvaluateSecurity(
	ctx context.Context,
	hostID int32,
	snap *diagnosticHostSnapshot,
	dbSecurityReport map[string]any,
	liveSecurityState map[string]any,
) DiagnosticCheckItem {
	if liveSecurityState != nil {
		if enabled, hasEnabled := liveSecurityState["enabled"].(bool); hasEnabled {
			if locked, hasLocked := liveSecurityState["locked"].(bool); hasLocked {
				if revF, hasRev := hostServiceJSONNumber(liveSecurityState["revision"]); hasRev && revF == math.Trunc(revF) && revF >= 0 {
					_, _ = diagnosticRecordReport(ctx, hostID, map[string]any{"state": liveSecurityState, "events": []any{}}, db.NodeSecurityRecordReportOptions{Authoritative: true})
					var desiredPtr *bool
					if dbSecurityReport != nil {
						if d, ok := dbSecurityReport["_desiredEnabled"].(bool); ok {
							desiredPtr = &d
						}
					}
					policySynced := false
					fpPinned := snap.ServerFingerprint != nil && *snap.ServerFingerprint != ""
					if !locked && desiredPtr != nil && *desiredPtr != enabled && fpPinned {
						policySynced = diagnosticSyncPolicy(ctx, hostID, true)
					}
					effectiveEnabled := enabled
					if policySynced && desiredPtr != nil {
						effectiveEnabled = *desiredPtr
					}
					if locked {
						reason, _ := liveSecurityState["reason"].(string)
						detail := "节点当前处于安全锁定状态（已停止实例），需在「运维 -> 节点安全模式」批准恢复"
						if reason != "" {
							detail += " · 原因: " + reason
						}
						return DiagnosticCheckItem{
							Key:    "nodeSecurity",
							Label:  "安全模式与节点状态",
							Status: DiagnosticStatusWarn,
							OK:     false,
							Detail: detail,
							Meta: map[string]any{
								"locked":   true,
								"enabled":  effectiveEnabled,
								"revision": revF,
								"reason":   reason,
							},
						}
					}
					detail := fmt.Sprintf("节点正常运行 · 安全模式未开启（revision %d）", int64(revF))
					if effectiveEnabled {
						detail = fmt.Sprintf("节点正常运行 · 安全模式已开启（revision %d）", int64(revF))
					}
					return DiagnosticCheckItem{
						Key:    "nodeSecurity",
						Label:  "安全模式与节点状态",
						Status: DiagnosticStatusPass,
						OK:     true,
						Detail: detail,
						Meta: map[string]any{
							"locked":       false,
							"enabled":      effectiveEnabled,
							"revision":     revF,
							"policySynced": policySynced,
						},
					}
				}
			}
		}
	}

	fpPinned := snap.ServerFingerprint != nil && *snap.ServerFingerprint != ""
	lockedInCache := dbSecurityReport != nil && dbSecurityReport["locked"] == true
	status := DiagnosticStatusPass
	if lockedInCache {
		status = DiagnosticStatusWarn
	}
	detail := "尚未核对节点证书指纹，建议在「运维 -> 核对证书指纹」完成绑定"
	if fpPinned {
		detail = "未能获取实时安全状态（可能为旧版节点）"
	}
	return DiagnosticCheckItem{
		Key:    "nodeSecurity",
		Label:  "安全模式与节点状态",
		Status: status,
		OK:     !lockedInCache,
		Detail: detail,
		Meta:   map[string]any{"fingerprintPinned": fpPinned},
	}
}

func diagnosticEvaluateStorageBaseline(dbPools []diagnosticPoolRow, liveStoragePools []map[string]any) (DiagnosticCheckItem, bool) {
	if liveStoragePools == nil {
		return DiagnosticCheckItem{
			Key:    "storageBaseline",
			Label:  "存储池基线一致性",
			Status: DiagnosticStatusWarn,
			OK:     false,
			Detail: "未能读取节点实时存储池列表（节点锁定或接口不支持）",
		}, false
	}

	nodeByName := make(map[string]map[string]any, len(liveStoragePools))
	for _, p := range liveStoragePools {
		if name, ok := p["name"].(string); ok && name != "" {
			nodeByName[name] = p
		}
	}
	dbNames := make(map[string]struct{}, len(dbPools))
	for _, p := range dbPools {
		dbNames[p.Name] = struct{}{}
	}

	missingOnNode := []string{}
	mismatched := []string{}
	for _, dbPool := range dbPools {
		if imp, ok := dbPool.Config["_payincusImport"]; ok && agentStorageJSTruthy(imp) {
			continue
		}
		livePool, exists := nodeByName[dbPool.Name]
		if !exists {
			missingOnNode = append(missingOnNode, dbPool.Name)
			continue
		}
		dbSource := agentStorageJSString(dbPool.Config["zfs.pool_name"])
		if dbSource == "" {
			dbSource = agentStorageJSString(dbPool.Config["source"])
		}
		liveCfg, _ := agentStorageIsRecord(livePool["config"])
		liveSource := ""
		if liveCfg != nil {
			liveSource = agentStorageJSString(liveCfg["zfs.pool_name"])
			if liveSource == "" {
				liveSource = agentStorageJSString(liveCfg["source"])
			}
		}
		sourceMatches := dbSource == "" || strings.HasPrefix(dbSource, "/") || dbSource == liveSource
		liveDriver, _ := livePool["driver"].(string)
		if liveDriver != dbPool.Driver || !sourceMatches {
			missingOnNode = append(missingOnNode, dbPool.Name)
			continue
		}
		liveStatus, _ := livePool["status"].(string)
		if liveStatus == "ready" && dbPool.Status != "ready" {
			mismatched = append(mismatched, dbPool.Name)
		} else if liveStatus == "locked" && dbPool.Status != "locked" {
			mismatched = append(mismatched, dbPool.Name)
		}
	}

	untrackedOnNode := []string{}
	for name := range nodeByName {
		if _, exists := dbNames[name]; !exists {
			untrackedOnNode = append(untrackedOnNode, name)
		}
	}

	inSync := len(missingOnNode) == 0 && len(untrackedOnNode) == 0 && len(mismatched) == 0
	issues := make([]string, 0, 3)
	if len(missingOnNode) > 0 {
		issues = append(issues, fmt.Sprintf("控制端有 %d 个缓存池在节点缺失（%s）", len(missingOnNode), strings.Join(missingOnNode, ", ")))
	}
	if len(untrackedOnNode) > 0 {
		issues = append(issues, fmt.Sprintf("节点有 %d 个未同步存储池（%s）", len(untrackedOnNode), strings.Join(untrackedOnNode, ", ")))
	}
	if len(mismatched) > 0 {
		issues = append(issues, fmt.Sprintf("%d 个存储池状态与节点缓存不一致（%s）", len(mismatched), strings.Join(mismatched, ", ")))
	}

	status := DiagnosticStatusWarn
	detail := strings.Join(issues, "；") + "，建议执行基线同步"
	if inSync {
		status = DiagnosticStatusPass
		detail = fmt.Sprintf("基线一致（已同步 %d 个存储池）", len(dbPools))
	}

	return DiagnosticCheckItem{
		Key:    "storageBaseline",
		Label:  "存储池基线一致性",
		Status: status,
		OK:     inSync,
		Detail: detail,
		Meta: map[string]any{
			"inSync":          inSync,
			"dbPoolCount":     len(dbPools),
			"nodePoolCount":   len(nodeByName),
			"missingOnNode":   missingOnNode,
			"untrackedOnNode": untrackedOnNode,
			"mismatched":      mismatched,
		},
	}, !inSync
}
