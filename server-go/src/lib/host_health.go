// Port of server/src/lib/incus/host-health.ts.
//
// A single in-process health cache shared by the status scheduler, the Incus
// connection pool and the admin node-security routes. Node keeps the counters in a
// module-level Map; Go does the same behind a mutex.
//
// AGENTS.md / WAVE_D_SPEC §0.1: this state is process-local BY DESIGN. A second
// backend replica would not see these counters, which is one more reason the
// private backend must stay a single process. Do not move it to PostgreSQL.
package lib

import (
	"sync"
	"time"
)

// Host health thresholds, mirroring the TypeScript constants.
const (
	// HostErrorThreshold is the consecutive-error count that marks a host unhealthy.
	HostErrorThreshold = 3
	// HostRecoveryCooldownMillis is how long a host stays unhealthy before a retry
	// is allowed (milliseconds).
	HostRecoveryCooldownMillis int64 = 5 * 60 * 1000
)

// HostHealth mirrors the TypeScript interface. Timestamps are Unix milliseconds,
// matching Date.now().
type HostHealth struct {
	LastResponseTime int64 // most recent response time in ms
	ErrorCount       int   // consecutive errors
	LastCheck        int64 // last check timestamp (ms)
	LastSuccess      int64 // last success timestamp (ms)
}

var (
	hostHealthMu    sync.Mutex
	hostHealthCache = map[int32]*HostHealth{}
)

// hostHealthNow is Date.now(). It is a variable so tests can move the clock
// without touching the cache internals.
var hostHealthNow = func() int64 { return time.Now().UTC().UnixMilli() }

// IsHostHealthy mirrors isHostHealthy(hostId).
//
// A host with no recorded health is healthy (optimistic first use). After the
// cooldown a still-failing host is retried once, and that retry resets the error
// counter — the TypeScript mutates the cached entry, so this does too.
func IsHostHealthy(hostID int32) bool {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	health, ok := hostHealthCache[hostID]
	if !ok {
		return true
	}
	if health.ErrorCount >= HostErrorThreshold {
		if hostHealthNow()-health.LastCheck > HostRecoveryCooldownMillis {
			health.ErrorCount = 0
			return true
		}
		return false
	}
	return true
}

// RecordHostSuccess mirrors recordHostSuccess(hostId, responseTimeMs).
func RecordHostSuccess(hostID int32, responseTimeMs int64) {
	now := hostHealthNow()

	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	health, ok := hostHealthCache[hostID]
	if !ok {
		health = &HostHealth{LastCheck: now, LastSuccess: now}
	}
	health.LastResponseTime = responseTimeMs
	health.ErrorCount = 0
	health.LastCheck = now
	health.LastSuccess = now
	hostHealthCache[hostID] = health
}

// RecordHostError mirrors recordHostError(hostId): increments the consecutive
// error counter but keeps the last success timestamp.
func RecordHostError(hostID int32) {
	now := hostHealthNow()

	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	health, ok := hostHealthCache[hostID]
	if !ok {
		health = &HostHealth{}
	}
	health.ErrorCount++
	health.LastCheck = now
	hostHealthCache[hostID] = health
}

// GetHostHealth mirrors getHostHealth(hostId).
//
// It returns a copy: the TypeScript hands back the live object, but no caller in
// the backend mutates it, and a copy cannot be corrupted by one.
func GetHostHealth(hostID int32) *HostHealth {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	health, ok := hostHealthCache[hostID]
	if !ok {
		return nil
	}
	snapshot := *health
	return &snapshot
}

// GetAllHostHealth mirrors getAllHostHealth(): a snapshot of every entry.
func GetAllHostHealth() map[int32]HostHealth {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	all := make(map[int32]HostHealth, len(hostHealthCache))
	for hostID, health := range hostHealthCache {
		all[hostID] = *health
	}
	return all
}

// GetUnhealthyHosts mirrors getUnhealthyHosts().
func GetUnhealthyHosts() []int32 {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	unhealthy := make([]int32, 0)
	for hostID, health := range hostHealthCache {
		if health.ErrorCount >= HostErrorThreshold {
			unhealthy = append(unhealthy, hostID)
		}
	}
	return unhealthy
}

// ClearHostHealth mirrors clearHostHealth(hostId): called when a host is deleted.
func ClearHostHealth(hostID int32) {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()
	delete(hostHealthCache, hostID)
}

// HostHealthSummary mirrors the getHealthSummary() return value.
type HostHealthSummary struct {
	TotalHosts      int   `json:"totalHosts"`
	HealthyHosts    int   `json:"healthyHosts"`
	UnhealthyHosts  int   `json:"unhealthyHosts"`
	AvgResponseTime int64 `json:"avgResponseTime"`
}

// GetHealthSummary mirrors getHealthSummary().
//
// avgResponseTime is Math.round(total / count), i.e. half away from zero for the
// positive values involved.
func GetHealthSummary() HostHealthSummary {
	hostHealthMu.Lock()
	defer hostHealthMu.Unlock()

	healthy := 0
	unhealthy := 0
	var totalResponseTime int64
	responseCount := 0

	for _, health := range hostHealthCache {
		if health.ErrorCount >= HostErrorThreshold {
			unhealthy++
		} else {
			healthy++
		}
		if health.LastResponseTime > 0 {
			totalResponseTime += health.LastResponseTime
			responseCount++
		}
	}

	average := int64(0)
	if responseCount > 0 {
		// Math.round(x) for a positive x is floor(x + 0.5).
		average = (totalResponseTime*2 + int64(responseCount)) / (int64(responseCount) * 2)
	}

	return HostHealthSummary{
		TotalHosts:      len(hostHealthCache),
		HealthyHosts:    healthy,
		UnhealthyHosts:  unhealthy,
		AvgResponseTime: average,
	}
}
