// Port of the agent-facing halves of server/src/lib/egress/{sync,realtime}.ts.
//
// Scope note (deliberate, to avoid a second implementation drifting from the
// first): the full egress control plane — requireNetworkControl, controlFailure,
// pushNetworkPolicy/applyNetworkPolicy, networkInstruction and
// sanitizeProbeResults — was already ported into routes/egress.go when the egress
// module landed (egressRequireNetworkControl, egressControlFailure,
// egressPushNetworkPolicy, egressNetworkInstruction, egressSanitizeProbeResults).
// routes/agent.go is in that same package, so the agent route calls those
// functions directly instead of importing a copy. What did NOT exist yet is the
// heartbeat half: sanitising a node-reported network status and persisting it
// under the same guards as recordNetworkStatus(). That lives here.
package lib

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// AgentNetworkStatusSanitizer is the seam that lets this package reuse the
// canonical sanitizeNetworkStatus port living in the routes package (routes
// imports lib, so lib cannot import routes). routes/agent.go installs
// egressSanitizeNetworkStatus during init; a nil sanitizer makes
// AgentRecordNetworkStatus a no-op, matching the TypeScript's bare `return` on
// an invalid report.
var AgentNetworkStatusSanitizer func(report any) (any, error)

// AgentRecordNetworkStatus mirrors recordNetworkStatus(hostId, report,
// expectedRevision?):
//
//   - no policy row, a non-object report, or a stale expectedRevision ⇒ no write;
//   - an unparseable report ⇒ no write (the TypeScript swallows the throw);
//   - appliedRevision outside [policy.appliedRevision, policy.revision] ⇒ no write;
//   - otherwise update applied_revision/status/reported_at for the exact
//     revision that was read (a concurrent bump makes the update a no-op).
//
// The returned bool reports whether the write was attempted, which is what the
// heartbeat needs for its "network status accepted" bookkeeping.
func AgentRecordNetworkStatus(ctx context.Context, hostID int32, report any, expectedRevision *int32) (bool, error) {
	if db.Pool() == nil {
		return false, errors.New("database not initialized")
	}
	policy, err := db.EgressHostNetworkPolicyGet(ctx, hostID)
	if err != nil {
		return false, err
	}
	if policy == nil {
		return false, nil
	}
	if expectedRevision != nil && policy.Revision != *expectedRevision {
		return false, nil
	}
	if report == nil {
		return false, nil
	}
	if _, ok := report.(map[string]any); !ok {
		return false, nil
	}
	if AgentNetworkStatusSanitizer == nil {
		return false, nil
	}
	status, err := AgentNetworkStatusSanitizer(report)
	if err != nil {
		return false, nil
	}
	appliedValue, _ := report.(map[string]any)["appliedRevision"].(float64)
	if appliedValue < 0 || appliedValue > 2147483647 {
		// The revision column is an Int4; the TypeScript would let Prisma throw.
		return false, nil
	}
	applied := int32(appliedValue)
	encoded, err := json.Marshal(status)
	if err != nil {
		return false, nil
	}
	if err := db.EgressRecordNetworkStatus(ctx, hostID, applied, encoded, nil); err != nil {
		return false, err
	}
	return true, nil
}
