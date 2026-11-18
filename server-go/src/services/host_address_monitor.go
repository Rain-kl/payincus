// Port of server/src/services/host-address-monitor.ts.
package services

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	hostAddressLockKey    = "host-address-registry"
	hostAddressLockExpire = 60 * time.Second
	hostAddressLockWait   = 10 * time.Second
)

// HostAddressRegistryError mirrors HostAddressRegistryError.
type HostAddressRegistryError struct {
	ErrorCode lib.ErrorCode
	Details   string
}

func (e *HostAddressRegistryError) Error() string {
	if e.Details != "" {
		return e.Details
	}
	return string(e.ErrorCode)
}

type normalizedHostAddress struct {
	Address string
	Kind    models.HostAddressKind
}

// HostAddressSnapshot mirrors HostAddressSnapshot.
type HostAddressSnapshot struct {
	Input    normalizedHostAddress
	Resolved []normalizedHostAddress
	Aliases  []db.HostAddressAliasInput
}

func normalizeHostAddressInput(raw string) normalizedHostAddress {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "[")
	trimmed = strings.TrimSuffix(trimmed, "]")
	if ip := net.ParseIP(trimmed); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return normalizedHostAddress{Address: v4.String(), Kind: models.HostAddressKindIpv4}
		}
		return normalizedHostAddress{Address: ip.String(), Kind: models.HostAddressKindIpv6}
	}
	return normalizedHostAddress{
		Address: strings.TrimSuffix(strings.ToLower(trimmed), "."),
		Kind:    models.HostAddressKindDomain,
	}
}

func extractHostInputAddress(rawURL string) (normalizedHostAddress, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return normalizedHostAddress{}, &HostAddressRegistryError{
			ErrorCode: lib.ErrHostAddressUnresolvable,
			Details:   "宿主机连接地址格式无效: " + rawURL,
		}
	}
	return normalizeHostAddressInput(u.Hostname()), nil
}

var hostAddressLookupIPs = func(ctx context.Context, domain string) ([]string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(resolveCtx, domain)
}

func resolveHostDomainAddresses(ctx context.Context, domain string) ([]normalizedHostAddress, error) {
	records, err := hostAddressLookupIPs(ctx, domain)
	if err != nil {
		return nil, &HostAddressRegistryError{
			ErrorCode: lib.ErrHostAddressUnresolvable,
			Details:   fmt.Sprintf("面板当前无法解析域名 %s (%v)", domain, err),
		}
	}
	seen := map[string]bool{}
	out := make([]normalizedHostAddress, 0, len(records))
	for _, r := range records {
		norm := normalizeHostAddressInput(r)
		if !seen[norm.Address] {
			seen[norm.Address] = true
			out = append(out, norm)
		}
	}
	if len(out) == 0 {
		return nil, &HostAddressRegistryError{
			ErrorCode: lib.ErrHostAddressUnresolvable,
			Details:   "面板当前未解析到域名 " + domain,
		}
	}
	return out, nil
}

func buildHostAddressSnapshot(input normalizedHostAddress, resolved []normalizedHostAddress) *HostAddressSnapshot {
	seen := map[string]bool{}
	var aliases []db.HostAddressAliasInput
	seen[input.Address] = true
	aliases = append(aliases, db.HostAddressAliasInput{
		Address: input.Address, Kind: input.Kind, Source: models.HostAddressSourceInput,
	})
	for _, item := range resolved {
		if !seen[item.Address] {
			seen[item.Address] = true
			aliases = append(aliases, db.HostAddressAliasInput{
				Address: item.Address, Kind: item.Kind, Source: models.HostAddressSourceResolved,
			})
		}
	}
	return &HostAddressSnapshot{Input: input, Resolved: resolved, Aliases: aliases}
}

// PrepareHostAddressSnapshotForWrite mirrors prepareHostAddressSnapshotForWrite.
func PrepareHostAddressSnapshotForWrite(ctx context.Context, rawURL string, excludeHostID *int32) (*HostAddressSnapshot, error) {
	input, err := extractHostInputAddress(rawURL)
	if err != nil {
		return nil, err
	}
	var resolved []normalizedHostAddress
	if input.Kind == models.HostAddressKindDomain {
		resolved, err = resolveHostDomainAddresses(ctx, input.Address)
		if err != nil {
			return nil, err
		}
	}
	snapshot := buildHostAddressSnapshot(input, resolved)
	addrs := make([]string, 0, len(snapshot.Aliases))
	for _, a := range snapshot.Aliases {
		addrs = append(addrs, a.Address)
	}
	conflicts, err := db.GetAliasesByAddresses(ctx, addrs, excludeHostID, nil)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		byAddr := map[string][]string{}
		for _, c := range conflicts {
			label := fmt.Sprintf("%s(#%d)", c.HostName, c.HostID)
			found := false
			for _, ex := range byAddr[c.Address] {
				if ex == label {
					found = true
					break
				}
			}
			if !found {
				byAddr[c.Address] = append(byAddr[c.Address], label)
			}
		}
		var keys []string
		for k := range byAddr {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, addr := range keys {
			parts = append(parts, fmt.Sprintf("%s 已被节点 %s 使用", addr, strings.Join(byAddr[addr], ", ")))
		}
		return nil, &HostAddressRegistryError{
			ErrorCode: lib.ErrHostAddressExists,
			Details:   strings.Join(parts, "；"),
		}
	}
	return snapshot, nil
}

// WithHostAddressRegistryLock acquires the PostgreSQL distributed lock for host address registry updates.
func WithHostAddressRegistryLock[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	expireMs := int(hostAddressLockExpire / time.Millisecond)
	waitMs := int(hostAddressLockWait / time.Millisecond)
	lockRes, err := lib.AcquireLock(ctx, hostAddressLockKey, lib.LockOptions{
		ExpireMs:      &expireMs,
		WaitTimeoutMs: &waitMs,
	})
	if err != nil {
		return zero, err
	}
	if !lockRes.Success || lockRes.OwnerID == "" {
		msg := lockRes.Error
		if msg == "" {
			msg = "Failed to acquire host address registry lock"
		}
		return zero, fmt.Errorf("%s", msg)
	}
	defer lib.ReleaseLock(context.WithoutCancel(ctx), hostAddressLockKey, lockRes.OwnerID)
	return fn()
}

// PersistHostAddressSnapshot mirrors persistHostAddressSnapshot.
func PersistHostAddressSnapshot(
	ctx context.Context,
	hostID int32,
	snapshot *HostAddressSnapshot,
	trigger models.HostAddressCheckTrigger,
	client *gorm.DB,
) ([]db.HostAddressConflictInfo, error) {
	if snapshot == nil {
		return nil, nil
	}
	existing, err := db.GetHostAddressAliasesForHost(ctx, hostID, client)
	if err != nil {
		return nil, err
	}
	if err := db.DeleteStaleHostInputAliases(ctx, hostID, snapshot.Input.Address, client); err != nil {
		return nil, err
	}
	if err := db.UpsertHostInputAlias(ctx, hostID, db.HostAddressAliasInput{
		Address: snapshot.Input.Address, Kind: snapshot.Input.Kind, Source: models.HostAddressSourceInput,
	}, client); err != nil {
		return nil, err
	}
	resolvedInputs := make([]db.HostAddressAliasInput, 0, len(snapshot.Resolved))
	for _, r := range snapshot.Resolved {
		resolvedInputs = append(resolvedInputs, db.HostAddressAliasInput{
			Address: r.Address, Kind: r.Kind, Source: models.HostAddressSourceResolved,
		})
	}
	if err := db.ReplaceHostResolvedAliases(ctx, hostID, resolvedInputs, client); err != nil {
		return nil, err
	}
	resolvedAddrs := make([]string, 0, len(snapshot.Resolved))
	for _, r := range snapshot.Resolved {
		resolvedAddrs = append(resolvedAddrs, r.Address)
	}
	aliasAddrs := make([]string, 0, len(snapshot.Aliases))
	for _, a := range snapshot.Aliases {
		aliasAddrs = append(aliasAddrs, a.Address)
	}
	if err := db.CreateHostAddressResolutionLog(ctx, db.HostAddressResolutionLogInput{
		HostID:            hostID,
		InputAddress:      snapshot.Input.Address,
		InputKind:         snapshot.Input.Kind,
		Trigger:           trigger,
		Status:            models.HostAddressCheckStatusSuccess,
		ResolvedAddresses: resolvedAddrs,
		Details:           map[string]any{"aliasAddresses": aliasAddrs},
	}, client); err != nil {
		return nil, err
	}
	allAddrs := append(existing, aliasAddrs...)
	return db.SyncHostAddressConflicts(ctx, allAddrs, client)
}

// RemoveHostAddressAliasesAndSyncConflicts removes aliases for a tunnel-mode host and resolves conflicts.
func RemoveHostAddressAliasesAndSyncConflicts(ctx context.Context, hostID int32, client *gorm.DB) error {
	removed, err := db.RemoveHostAddressAliases(ctx, hostID, client)
	if err != nil || len(removed) == 0 {
		return err
	}
	_, err = db.SyncHostAddressConflicts(ctx, removed, client)
	return err
}

func recordHostAddressResolutionFailure(
	ctx context.Context,
	hostID int32,
	rawURL string,
	trigger models.HostAddressCheckTrigger,
	regErr *HostAddressRegistryError,
	client *gorm.DB,
) ([]db.HostAddressConflictInfo, error) {
	existing, err := db.GetHostAddressAliasesForHost(ctx, hostID, client)
	if err != nil {
		return nil, err
	}
	input, parseErr := extractHostInputAddress(rawURL)
	if parseErr != nil {
		return nil, parseErr
	}
	if err := db.DeleteStaleHostInputAliases(ctx, hostID, input.Address, client); err != nil {
		return nil, err
	}
	if err := db.UpsertHostInputAlias(ctx, hostID, db.HostAddressAliasInput{
		Address: input.Address, Kind: input.Kind, Source: models.HostAddressSourceInput,
	}, client); err != nil {
		return nil, err
	}
	errStr := regErr.Error()
	if err := db.CreateHostAddressResolutionLog(ctx, db.HostAddressResolutionLogInput{
		HostID:            hostID,
		InputAddress:      input.Address,
		InputKind:         input.Kind,
		Trigger:           trigger,
		Status:            models.HostAddressCheckStatusFailed,
		ResolvedAddresses: []string{},
		Error:             &errStr,
		Details:           map[string]any{"errorCode": string(regErr.ErrorCode)},
	}, client); err != nil {
		return nil, err
	}
	return db.SyncHostAddressConflicts(ctx, append(existing, input.Address), client)
}

func notifyAdminsAboutHostConflicts(ctx context.Context, conflicts []db.HostAddressConflictInfo) {
	if len(conflicts) == 0 {
		return
	}
	adminIDs, err := db.GetAllAdminUserIds(ctx)
	if err != nil || len(adminIDs) == 0 {
		return
	}
	lines := make([]string, 0, len(conflicts))
	conflictData := make([]map[string]any, 0, len(conflicts))
	for _, c := range conflicts {
		lines = append(lines, fmt.Sprintf("地址 %s 在节点 %s(#%d) 与 %s(#%d) 之间发生冲突",
			c.Address, c.HostAName, c.HostAID, c.HostBName, c.HostBID))
		conflictData = append(conflictData, map[string]any{
			"id":      c.ID,
			"address": c.Address,
			"hostAId": c.HostAID,
			"hostBId": c.HostBID,
		})
	}
	content := strings.Join(lines, "\n")
	_, _ = db.CreateBulkMessages(ctx, db.CreateBulkInboxMessagesData{
		UserIDs:   adminIDs,
		EventType: "host_address_conflict",
		Title:     "检测到节点连接地址冲突",
		Content:   content,
		Data:      map[string]any{"conflicts": conflictData},
	})
	db.CreateLog(ctx, nil, db.LogModuleHost, "host.address_conflict", content, "warning", db.CreateLogOptions{})
}

func syncExistingHostAddressState(
	ctx context.Context,
	hostID int32,
	hostName, rawURL string,
	trigger models.HostAddressCheckTrigger,
) bool {
	if db.DB == nil {
		return false
	}
	host, err := db.HostsGetByID(ctx, hostID)
	if err == nil && host != nil && host.TunnelEnabled {
		_, _ = WithHostAddressRegistryLock(ctx, func() (struct{}, error) {
			return struct{}{}, db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return RemoveHostAddressAliasesAndSyncConflicts(ctx, hostID, tx)
			})
		})
		return true
	}

	input, parseErr := extractHostInputAddress(rawURL)
	if parseErr != nil {
		if regErr, ok := parseErr.(*HostAddressRegistryError); ok {
			msg := fmt.Sprintf("[HostAddress] %s 解析失败: %s(#%d) -> %s; %s", trigger, hostName, hostID, rawURL, regErr.Error())
			log.Printf("%s", msg)
			db.CreateLog(ctx, nil, db.LogModuleHost, "host.address_resolve", msg, "warning", db.CreateLogOptions{})
		}
		return false
	}

	var resolved []normalizedHostAddress
	var resolveErr error
	if input.Kind == models.HostAddressKindDomain {
		resolved, resolveErr = resolveHostDomainAddresses(ctx, input.Address)
	}
	if resolveErr != nil {
		regErr, ok := resolveErr.(*HostAddressRegistryError)
		if !ok {
			log.Printf("[HostAddress] %s failed for %s(#%d): %v", trigger, hostName, hostID, resolveErr)
			return false
		}
		msg := fmt.Sprintf("[HostAddress] %s 解析失败: %s(#%d) -> %s; %s", trigger, hostName, hostID, input.Address, regErr.Error())
		log.Printf("%s", msg)
		db.CreateLog(ctx, nil, db.LogModuleHost, "host.address_resolve", msg, "warning", db.CreateLogOptions{})

		created, failErr := WithHostAddressRegistryLock(ctx, func() ([]db.HostAddressConflictInfo, error) {
			var out []db.HostAddressConflictInfo
			txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				c, err := recordHostAddressResolutionFailure(ctx, hostID, rawURL, trigger, regErr, tx)
				out = c
				return err
			})
			return out, txErr
		})
		if failErr != nil {
			log.Printf("[HostAddress] Failed to persist %s error for %s(#%d): %v", trigger, hostName, hostID, failErr)
		} else {
			notifyAdminsAboutHostConflicts(ctx, created)
		}
		return false
	}

	snapshot := buildHostAddressSnapshot(input, resolved)
	created, err := WithHostAddressRegistryLock(ctx, func() ([]db.HostAddressConflictInfo, error) {
		var out []db.HostAddressConflictInfo
		txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			c, err := PersistHostAddressSnapshot(ctx, hostID, snapshot, trigger, tx)
			out = c
			return err
		})
		return out, txErr
	})
	if err != nil {
		log.Printf("[HostAddress] %s failed for %s(#%d): %v", trigger, hostName, hostID, err)
		return false
	}
	notifyAdminsAboutHostConflicts(ctx, created)
	return true
}

// RunHostAddressBackfillJob mirrors runHostAddressBackfillJob().
func RunHostAddressBackfillJob(ctx context.Context) {
	if ctx.Err() != nil || db.DB == nil {
		return
	}
	if _, err := WithHostAddressRegistryLock(ctx, func() ([]db.HostAddressConflictInfo, error) {
		var out []db.HostAddressConflictInfo
		txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			c, err := db.CleanupTunnelHostAddressAliases(ctx, tx)
			out = c
			return err
		})
		return out, txErr
	}); err != nil {
		log.Printf("[HostAddress] Failed to clean up tunnel host address aliases: %v", err)
	}

	hosts, err := db.GetHostsForAddressBackfill(ctx)
	if err != nil {
		log.Printf("[HostAddress] Initial backfill query failed: %v", err)
		return
	}
	log.Printf("[HostAddress] Starting backfill for %d host(s)", len(hosts))
	successCount, failedCount := 0, 0
	for _, h := range hosts {
		if ctx.Err() != nil {
			return
		}
		if syncExistingHostAddressState(ctx, h.ID, h.Name, h.URL, models.HostAddressCheckTriggerBackfill) {
			successCount++
		} else {
			failedCount++
		}
	}
	log.Printf("[HostAddress] Backfill completed: success=%d, failed=%d", successCount, failedCount)
}

// RunHostAddressPollJob mirrors runHostAddressPollJob().
func RunHostAddressPollJob(ctx context.Context) {
	if ctx.Err() != nil || db.DB == nil {
		return
	}
	hosts, err := db.GetHostsWithDomainInputAlias(ctx)
	if err != nil {
		log.Printf("[HostAddress] Poll query failed: %v", err)
		return
	}
	log.Printf("[HostAddress] Starting poll for %d domain host(s)", len(hosts))
	successCount, failedCount := 0, 0
	for _, h := range hosts {
		if ctx.Err() != nil {
			return
		}
		if syncExistingHostAddressState(ctx, h.ID, h.Name, h.URL, models.HostAddressCheckTriggerPoll) {
			successCount++
		} else {
			failedCount++
		}
	}
	log.Printf("[HostAddress] Poll completed: success=%d, failed=%d", successCount, failedCount)
}

// HostAddressMonitorNextRun computes the next */30 * * * * boundary in Asia/Shanghai.
func HostAddressMonitorNextRun(now time.Time) time.Time {
	loc := trafficLocation()
	local := now.In(loc)
	minute := local.Minute()
	nextMin := 0
	hourOffset := 0
	if minute < 30 {
		nextMin = 30
	} else {
		nextMin = 0
		hourOffset = 1
	}
	target := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), nextMin, 0, 0, loc)
	if hourOffset > 0 {
		target = target.Add(time.Hour)
	}
	return target.UTC()
}

var hostAddressMonitorRunning atomic.Bool
