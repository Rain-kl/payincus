package routes

import "testing"

// TestInstancesIncusProvisionerWiring pins the resource-delivery wiring the instance
// create path depends on.
//
// Before lib/incus_instances.go landed these two seams were nil, so every async
// creation failed loudly and the purchase was refunded. They are now backed by
// lib.IncusInstanceProvision / lib.IncusInstanceCleanup; a nil here means the
// instance creation flow silently regressed to "always fail", which is why it is a
// test and not a comment.
func TestInstancesIncusProvisionerWiring(t *testing.T) {
	if InstancesIncusProvisioner == nil {
		t.Fatal("InstancesIncusProvisioner is nil: createInstanceAsync would fail every creation")
	}
	if InstancesIncusCleanup == nil {
		t.Fatal("InstancesIncusCleanup is nil: failed creations would leak instances on the node")
	}
}
