// Port of server/src/services/traffic-notifier.ts — scoped to the surface that
// routes/traffic.ts and db/traffic.ts actually consume.
//
// Checked against both call sites before writing this file:
//
//   - routes/traffic.ts imports exactly one symbol from this module, `formatBytes`.
//     traffic-notifier.ts only re-exports it (`export { formatBytes }`) from
//     services/traffic-utils.ts, whose port lives in lib/traffic_utils.go.
//   - db/traffic.ts imports nothing from it.
//
// Everything else in the module — sendTrafficWarningNotification,
// sendTrafficThrottledNotification, the Telegram/Discord transports and the
// retry/lease policy — is consumed exclusively by services/traffic-scheduler.ts,
// which is a Phase 7 worker. Duplicating it here now would fork the notifier from
// the scheduler that owns its retry semantics, so it is deliberately left to that
// wave. The import path and the exported helper keep the module mapping intact.
package services

import "github.com/rain-kl/payincus/server-go/src/lib"

// FormatBytes mirrors `export { formatBytes }` in traffic-notifier.ts (re-exported
// from services/traffic-utils.ts).
func FormatBytes(bytes int64) string {
	return lib.FormatBytes(bytes)
}
