// Port of server/src/lib/instance-status-observation.ts.
//
// A single-backend observation window: a VM reboot can briefly report Error, so
// the status scheduler only accepts an "error" observation after the same instance
// has kept reporting error for 30 seconds. State is process-local BY DESIGN
// (WAVE_D_SPEC §0.1): a second backend replica would not share the window, which is
// one more reason the private backend must stay a single process.
//
// The TypeScript keeps a Map<number, {first,last}> and relies on two Map
// properties that a Go map does not have:
//
//   - iteration order is insertion order, so the 15-minute expiry sweep walks
//     oldest-first;
//   - `this.errors.keys().next().value` is the *oldest inserted* key, which is the
//     one evicted when the 10_000-entry cap is hit.
//
// Both are reproduced with an explicit insertion-order list alongside the map,
// because evicting a different key would change which instances get their window
// reset once the cap is reached.
package lib

import (
	"container/list"
	"sync"
	"time"
)

const (
	// InstanceStatusObservationErrorWindowMs is how long an instance must keep
	// reporting "error" before the observation is applied (30 seconds).
	InstanceStatusObservationErrorWindowMs int64 = 30_000
	// InstanceStatusObservationExpiryMs is when a remembered error observation is
	// forgotten entirely (15 minutes).
	InstanceStatusObservationExpiryMs int64 = 15 * 60_000
	// InstanceStatusObservationMaxEntries bounds memory even if instances vanish.
	InstanceStatusObservationMaxEntries = 10_000
)

// instanceStatusObservation is the per-instance window.
type instanceStatusObservation struct {
	first int64
	last  int64
}

// InstanceStatusObservations mirrors the TypeScript class.
type InstanceStatusObservations struct {
	mu     sync.Mutex
	errors map[int32]instanceStatusObservation
	// order holds the live keys in insertion order (front = oldest), matching
	// JavaScript Map iteration order.
	order *list.List
}

// NewInstanceStatusObservations mirrors `new InstanceStatusObservations()`.
func NewInstanceStatusObservations() *InstanceStatusObservations {
	return &InstanceStatusObservations{
		errors: map[int32]instanceStatusObservation{},
		order:  list.New(),
	}
}

// InstanceStatusObservationsShared mirrors the module-level
// `export const instanceStatusObservations`.
var InstanceStatusObservationsShared = NewInstanceStatusObservations()

// instanceStatusObservationKnownStatus mirrors the `['running','stopped','error']`
// whitelist used for both `current` and `observed`.
func instanceStatusObservationKnownStatus(status string) bool {
	switch status {
	case "running", "stopped", "error":
		return true
	default:
		return false
	}
}

// ShouldApply mirrors shouldApply(id, current, observed, now).
//
// Returning false means "do not trust this observation (yet)". Returning true
// means the caller may write the observed status through.
func (o *InstanceStatusObservations) ShouldApply(id int32, current, observed string, now int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !instanceStatusObservationKnownStatus(current) || !instanceStatusObservationKnownStatus(observed) {
		o.deleteLocked(id)
		return false
	}
	// Only a transition into "error" needs debouncing; everything else (including
	// error → error) applies immediately and clears the window.
	if observed != "error" || current == "error" {
		o.deleteLocked(id)
		return true
	}

	// Expire old observations and bound memory even if instances disappear.
	for element := o.order.Front(); element != nil; {
		next := element.Next()
		key := element.Value.(int32)
		if now-o.errors[key].last > InstanceStatusObservationExpiryMs {
			o.deleteLocked(key)
		}
		element = next
	}

	previous, ok := o.errors[id]
	if !ok {
		if len(o.errors) >= InstanceStatusObservationMaxEntries {
			if oldest := o.order.Front(); oldest != nil {
				o.deleteLocked(oldest.Value.(int32))
			}
		}
		o.errors[id] = instanceStatusObservation{first: now, last: now}
		o.order.PushBack(id)
		return false
	}

	previous.last = now
	o.errors[id] = previous
	return now-previous.first >= InstanceStatusObservationErrorWindowMs
}

// Forget mirrors forget(id).
func (o *InstanceStatusObservations) Forget(id int32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleteLocked(id)
}

// Len reports the number of remembered windows. It has no TypeScript counterpart;
// it exists so tests can assert the memory cap without reaching into the map.
func (o *InstanceStatusObservations) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.errors)
}

func (o *InstanceStatusObservations) deleteLocked(id int32) {
	if _, ok := o.errors[id]; !ok {
		return
	}
	delete(o.errors, id)
	for element := o.order.Front(); element != nil; element = element.Next() {
		if element.Value.(int32) == id {
			o.order.Remove(element)
			return
		}
	}
}

// InstanceStatusObservationsNow mirrors the `now = Date.now()` default argument.
func InstanceStatusObservationsNow() int64 {
	return time.Now().UTC().UnixMilli()
}
