// Traffic-collection seam for the workers.
//
// Stop, restart, rebuild and restore all take a "last live reading" of the
// instance counters before the instance goes down, because the periodic collector
// would otherwise lose the final window. In Node this is
// services/instance-traffic-collector.ts#collectTrafficForRunningInstance; the Go
// collector lives in the services package, which the schedulers wave is still
// building. Importing that package from the worker would couple the two waves'
// build health, so the call goes through this registration hook instead.
//
// The default is intentionally NOT a silent success: it reports failure with a
// reason, and every caller logs the warning and continues — exactly what happens
// when the real collector cannot read the counters. A missing registration is
// therefore visible in the logs and never inflates usage.
package workers

import (
	"context"
	"log"
	"sync"
)

// TrafficCollectorFunc mirrors CollectInstanceTrafficResult's success/error pair.
type TrafficCollectorFunc func(ctx context.Context, instanceID int32) (success bool, errMsg string)

var (
	trafficCollectorMu sync.RWMutex
	trafficCollector   TrafficCollectorFunc = func(ctx context.Context, instanceID int32) (bool, string) {
		return false, "流量采集器未注册（等待 services 包接线）"
	}
)

// SetTrafficCollector installs the live counter reader. The integration owner
// passes an adapter around services.CollectTrafficForRunningInstance.
func SetTrafficCollector(fn TrafficCollectorFunc) {
	trafficCollectorMu.Lock()
	defer trafficCollectorMu.Unlock()
	if fn == nil {
		trafficCollector = func(ctx context.Context, instanceID int32) (bool, string) {
			return false, "流量采集器未注册（等待 services 包接线）"
		}
		return
	}
	trafficCollector = fn
}

// collectTrafficForRunningInstance never fails a task: a lost sample is logged and
// the operation proceeds, matching the TypeScript's try/catch-free warn path.
func collectTrafficForRunningInstance(ctx context.Context, instanceID int32) (bool, string) {
	trafficCollectorMu.RLock()
	fn := trafficCollector
	trafficCollectorMu.RUnlock()
	success, errMsg := fn(ctx, instanceID)
	if !success && errMsg != "" {
		log.Printf("[workers] 即时采集流量失败（instance=%d）: %s", instanceID, errMsg)
	}
	return success, errMsg
}
