package routes

import (
	"context"
	"testing"
)

func TestHostsSeamWiringsNonNil(t *testing.T) {
	if HostsManagedInstanceProvision == nil {
		t.Fatal("HostsManagedInstanceProvision must be wired")
	}
	if HostsInstanceRefund == nil {
		t.Fatal("HostsInstanceRefund must be wired")
	}
	if HostsBatchDeleteExecutor == nil {
		t.Fatal("HostsBatchDeleteExecutor must be wired")
	}
	if HostsMigrateExecutor == nil {
		t.Fatal("HostsMigrateExecutor must be wired")
	}
	if HostsStoragePoolsSnapshot == nil {
		t.Fatal("HostsStoragePoolsSnapshot must be wired")
	}
	if HostsAgentLogStreamSubscribe == nil {
		t.Fatal("HostsAgentLogStreamSubscribe must be wired")
	}
	if HostsSnapshotSyncApplier == nil {
		t.Fatal("HostsSnapshotSyncApplier must be wired")
	}

	// Free (non-paid) instance refund calculation needs no DB connection.
	info, err := HostsInstanceRefund(context.Background(), HostsRefundInput{ID: 1})
	if err != nil || info.IsPaid || info.RefundAmount != 0 {
		t.Fatalf("free instance refund = (%+v, %v), want empty non-error", info, err)
	}
}
