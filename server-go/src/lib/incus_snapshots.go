// Port of server/src/lib/incus/incus-snapshots.ts (79 lines).
//
// Five of the six helpers were already ported inside lib/incus_instances.go while
// the snapshots module needed them, because they share the same 30-minute
// operation budget and the same error mapping:
//
//	ListSnapshots   ↔ listSnapshots    (GET  /1.0/instances/{name}/snapshots)
//	GetSnapshot     ↔ getSnapshot      (GET  /1.0/instances/{name}/snapshots/{snap})
//	CreateSnapshot  ↔ createSnapshot   (POST /1.0/instances/{name}/snapshots {name})
//	DeleteSnapshot  ↔ deleteSnapshot   (DELETE …/snapshots/{snap})
//	RestoreSnapshot ↔ restoreSnapshot  (PUT  /1.0/instances/{name} {restore: snap})
//
// This file owns the sixth (renameSnapshot), which the snapshots routes did not
// need, so the whole TypeScript module now has a Go counterpart. It intentionally
// does not re-declare the other five: two copies of the same request would drift.
package lib

import (
	"context"
	"encoding/json"
	"net/http"
)

// RenameSnapshot mirrors renameSnapshot(client, instanceName, snapshotName,
// newName): POST {name} to the snapshot path.
func RenameSnapshot(ctx context.Context, client *IncusClient, instanceName, snapshotName, newName string) (json.RawMessage, error) {
	body := map[string]any{"name": newName}
	return client.Request(ctx, http.MethodPost,
		"/1.0/instances/"+instanceName+"/snapshots/"+snapshotName, body,
		incusInstanceOperationTimeout, body)
}
