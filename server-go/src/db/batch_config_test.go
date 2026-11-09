package db

import "testing"

// TestBatchConfigInstanceResourceUpdates pins the tri-state mapping of
// updateInstanceResources: absent fields never reach the SET clause, an explicit
// null monthly traffic limit becomes SQL NULL, and a bigint is passed as int64.
func TestBatchConfigInstanceResourceUpdates(t *testing.T) {
	// The TypeScript's early return: no provided field → no update at all.
	if updates := batchConfigInstanceResourceUpdates(BatchConfigInstanceResourcesUpdate{}); len(updates) != 0 {
		t.Fatalf("empty update produced %v", updates)
	}

	updates := batchConfigInstanceResourceUpdates(BatchConfigInstanceResourcesUpdate{
		CPUProvided:    true,
		CPU:            4,
		DiskProvided:   true,
		Disk:           20,
		MemoryProvided: false,
	})
	if len(updates) != 2 {
		t.Fatalf("updates = %v", updates)
	}
	if updates["cpu"] != int32(4) {
		t.Errorf("cpu = %#v", updates["cpu"])
	}
	if updates["disk"] != int32(20) {
		t.Errorf("disk = %#v", updates["disk"])
	}
	if _, present := updates["memory"]; present {
		t.Errorf("memory must not be set when it was not provided")
	}

	// Explicit null.
	updates = batchConfigInstanceResourceUpdates(BatchConfigInstanceResourcesUpdate{MonthlyTrafficLimitProvided: true})
	value, present := updates["monthly_traffic_limit"]
	if !present || value != nil {
		t.Fatalf("explicit null monthlyTrafficLimit = %#v, %v", value, present)
	}

	// bigint value.
	limit := uint64(1099511627776)
	updates = batchConfigInstanceResourceUpdates(BatchConfigInstanceResourcesUpdate{
		MonthlyTrafficLimitProvided: true,
		MonthlyTrafficLimit:         &limit,
	})
	if got, ok := updates["monthly_traffic_limit"].(int64); !ok || got != int64(limit) {
		t.Fatalf("monthlyTrafficLimit = %#v", updates["monthly_traffic_limit"])
	}
}

// TestBatchConfigInstanceQuotaUpdates pins updateInstanceQuota's mapping,
// including explicit NULLs.
func TestBatchConfigInstanceQuotaUpdates(t *testing.T) {
	if updates := batchConfigInstanceQuotaUpdates(BatchConfigInstanceQuotaUpdate{}); len(updates) != 0 {
		t.Fatalf("empty quota update produced %v", updates)
	}

	portLimit := int32(5)
	updates := batchConfigInstanceQuotaUpdates(BatchConfigInstanceQuotaUpdate{
		PortLimitProvided:     true,
		PortLimit:             &portLimit,
		SnapshotLimitProvided: true, // explicit NULL
	})
	if got, ok := updates["port_limit"].(*int32); !ok || got == nil || *got != 5 {
		t.Fatalf("port_limit = %#v", updates["port_limit"])
	}
	if value, present := updates["snapshot_limit"]; !present || value.(*int32) != nil {
		t.Fatalf("snapshot_limit = %#v, %v", value, present)
	}
	if _, present := updates["backup_limit"]; present {
		t.Errorf("backup_limit must not be set")
	}
}

// TestBatchConfigInstanceSwapUpdates pins updateInstanceSwap's mapping.
func TestBatchConfigInstanceSwapUpdates(t *testing.T) {
	if updates := batchConfigInstanceSwapUpdates(BatchConfigInstanceSwapUpdate{}); len(updates) != 0 {
		t.Fatalf("empty swap update produced %v", updates)
	}

	size := int32(256)
	updates := batchConfigInstanceSwapUpdates(BatchConfigInstanceSwapUpdate{
		SwapEnabledProvided: true,
		SwapEnabled:         true,
		SwapSizeProvided:    true,
		SwapSize:            &size,
	})
	if updates["swap_enabled"] != true {
		t.Errorf("swap_enabled = %#v", updates["swap_enabled"])
	}
	if got, ok := updates["swap_size"].(*int32); !ok || got == nil || *got != 256 {
		t.Errorf("swap_size = %#v", updates["swap_size"])
	}

	// Explicit NULL swap size.
	updates = batchConfigInstanceSwapUpdates(BatchConfigInstanceSwapUpdate{SwapSizeProvided: true})
	if value, present := updates["swap_size"]; !present || value.(*int32) != nil {
		t.Fatalf("null swap_size = %#v, %v", value, present)
	}
}

// TestBatchConfigInstanceAdvancedConfigUpdates pins the column names, including the
// configured_limits_ingress / configured_limits_egress quirk, and the
// absent-vs-null distinction.
func TestBatchConfigInstanceAdvancedConfigUpdates(t *testing.T) {
	read := "10MB"
	priority := int32(5)
	autostart := true

	updates := batchConfigInstanceAdvancedConfigUpdates(BatchConfigInstanceAdvancedConfigUpdate{
		LimitsReadProvided:        true,
		LimitsRead:                &read,
		LimitsReadIopsProvided:    true, // explicit NULL (limitsRead wins)
		LimitsIngressProvided:     true,
		LimitsIngress:             nil, // explicit NULL
		LimitsCPUPriorityProvided: true,
		LimitsCPUPriority:         &priority,
		BootAutostartProvided:     true,
		BootAutostart:             &autostart,
	})

	if got, ok := updates["limits_read"].(*string); !ok || got == nil || *got != "10MB" {
		t.Errorf("limits_read = %#v", updates["limits_read"])
	}
	if value, present := updates["limits_read_iops"]; !present || value.(*int32) != nil {
		t.Errorf("limits_read_iops = %#v, %v", value, present)
	}
	if value, present := updates["configured_limits_ingress"]; !present || value.(*string) != nil {
		t.Errorf("configured_limits_ingress = %#v, %v", value, present)
	}
	if _, present := updates["limits_ingress"]; present {
		t.Errorf("limits_ingress (effective) must never be written by this route")
	}
	if got, ok := updates["limits_cpu_priority"].(*int32); !ok || got == nil || *got != 5 {
		t.Errorf("limits_cpu_priority = %#v", updates["limits_cpu_priority"])
	}
	if got, ok := updates["boot_autostart"].(*bool); !ok || got == nil || !*got {
		t.Errorf("boot_autostart = %#v", updates["boot_autostart"])
	}

	// No provided field → no update.
	if empty := batchConfigInstanceAdvancedConfigUpdates(BatchConfigInstanceAdvancedConfigUpdate{}); len(empty) != 0 {
		t.Fatalf("empty advanced update produced %v", empty)
	}
}

// TestBatchConfigUpdateMissingMessage pins the Prisma P2025 wording that ends up in
// the per-instance failure item.
func TestBatchConfigUpdateMissingMessage(t *testing.T) {
	want := "An operation failed because it depends on one or more records " +
		"that were required but not found. Record to update not found."
	if batchConfigUpdateMissingMessage != want {
		t.Fatalf("message = %q", batchConfigUpdateMissingMessage)
	}
}
