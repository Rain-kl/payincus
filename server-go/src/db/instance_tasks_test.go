package db

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestInstanceTasksClampPagination pins clampInstanceTaskPagination: pageSize is
// clamped to [1, 100] with a default of 20, while page must be a positive integer
// (0, negative and nil all fall back to 1). The clamp is what stops a client from
// asking for the whole table.
func TestInstanceTasksClampPagination(t *testing.T) {
	intPtr := func(value int) *int { return &value }

	cases := []struct {
		name         string
		page         *int
		pageSize     *int
		wantPage     int
		wantPageSize int
	}{
		{"defaults", nil, nil, 1, 20},
		{"page one", intPtr(1), nil, 1, 20},
		{"page three", intPtr(3), nil, 3, 20},
		{"page zero", intPtr(0), nil, 1, 20},
		{"page negative", intPtr(-5), nil, 1, 20},
		{"size zero clamps up", nil, intPtr(0), 1, 1},
		{"size negative clamps up", nil, intPtr(-10), 1, 1},
		{"size one", nil, intPtr(1), 1, 1},
		{"size hundred", nil, intPtr(100), 1, 100},
		{"size above cap", nil, intPtr(101), 1, 100},
		{"size huge", nil, intPtr(10_000), 1, 100},
	}
	for _, tc := range cases {
		page, pageSize := instanceTasksClampPagination(tc.page, tc.pageSize)
		if page != tc.wantPage || pageSize != tc.wantPageSize {
			t.Errorf("%s: got (%d, %d), want (%d, %d)",
				tc.name, page, pageSize, tc.wantPage, tc.wantPageSize)
		}
	}
}

// TestInstanceTasksNormalizeStatuses pins normalizeInstanceTaskStatuses: unknown
// values are dropped, an empty result means "no status filter" (not "match
// nothing"), and a nil slice stays nil.
func TestInstanceTasksNormalizeStatuses(t *testing.T) {
	if got := instanceTasksNormalizeStatuses(nil); got != nil {
		t.Fatalf("nil = %v, want nil", got)
	}
	if got := instanceTasksNormalizeStatuses([]models.InstanceTaskStatus{}); got != nil {
		t.Fatalf("empty = %v, want nil", got)
	}
	if got := instanceTasksNormalizeStatuses([]models.InstanceTaskStatus{"bogus", ""}); got != nil {
		t.Fatalf("unknown-only = %v, want nil", got)
	}

	got := instanceTasksNormalizeStatuses([]models.InstanceTaskStatus{
		models.InstanceTaskStatusPROCESSING, "bogus", models.InstanceTaskStatusPENDING,
	})
	want := []models.InstanceTaskStatus{
		models.InstanceTaskStatusPROCESSING, models.InstanceTaskStatusPENDING,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// All four documented statuses survive.
	all := instanceTasksNormalizeStatuses(instanceTaskStatuses)
	if len(all) != 4 {
		t.Fatalf("all statuses = %v, want 4 entries", all)
	}
}

// TestInstanceTaskConflictError pins the error the routes translate into
// 409 TASK_IN_PROGRESS: the message is the TypeScript class message and the active
// task (when there is one) travels with it.
func TestInstanceTaskConflictError(t *testing.T) {
	plain := &InstanceTaskConflictError{}
	if plain.Error() != "Instance has an active task" {
		t.Fatalf("message = %q", plain.Error())
	}
	if plain.ActiveTask != nil {
		t.Fatal("lock-only conflict must not carry a task")
	}

	task := &InstanceTaskWithDetails{ID: 7, TaskType: models.InstanceTaskTypeStart}
	conflict := &InstanceTaskConflictError{ActiveTask: task}
	var target *InstanceTaskConflictError
	if !errors.As(error(conflict), &target) {
		t.Fatal("errors.As must recover the conflict error")
	}
	if target.ActiveTask == nil || target.ActiveTask.ID != 7 {
		t.Fatalf("active task = %#v", target.ActiveTask)
	}
}

// TestInstanceTaskStatusUpdateAssignments pins `{ status, ...updates }` semantics:
// an absent key is skipped, a present key is written even when it is the zero value
// (empty string / zero id / zero time), and $1 stays the task id.
func TestInstanceTaskStatusUpdateAssignments(t *testing.T) {
	sets, values := instanceTaskStatusUpdateAssignments(models.InstanceTaskStatusPROCESSING, nil)
	if !reflect.DeepEqual(sets, []string{"status = $2"}) {
		t.Fatalf("sets = %v", sets)
	}
	if len(values) != 1 || values[0] != string(models.InstanceTaskStatusPROCESSING) {
		t.Fatalf("values = %v", values)
	}

	emptyProgress := ""
	emptyError := ""
	zeroID := int32(0)
	started := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	sets, values = instanceTaskStatusUpdateAssignments(models.InstanceTaskStatusCOMPLETED, &InstanceTaskStatusUpdates{
		Progress:      &emptyProgress,
		Error:         &emptyError,
		StartedAt:     &started,
		NewInstanceID: &zeroID,
	})
	wantSets := []string{
		"status = $2", "progress = $3", "error = $4", "started_at = $5", "new_instance_id = $6",
	}
	if !reflect.DeepEqual(sets, wantSets) {
		t.Fatalf("sets = %v\nwant %v", sets, wantSets)
	}
	if len(values) != 5 {
		t.Fatalf("values = %v", values)
	}
	// The zero values must come through: an empty progress string clears no data but
	// is still a write, and new_instance_id 0 is the only id the clone path could
	// ever produce before the new row exists.
	if values[1] != "" || values[2] != "" {
		t.Fatalf("empty strings must be preserved: %v", values)
	}
	if value, ok := values[4].(int32); !ok || value != 0 {
		t.Fatalf("new_instance_id = %#v, want 0", values[4])
	}
}

// TestInstanceTaskIDTruthyAndStringTruthy pins the `data.x || null` guards: 0 and ""
// are falsy in JavaScript, so they become NULL instead of being stored.
func TestInstanceTaskIDTruthyAndStringTruthy(t *testing.T) {
	zero := int32(0)
	if instanceTaskIDTruthy(nil) != nil {
		t.Fatal("nil id must stay nil")
	}
	if instanceTaskIDTruthy(&zero) != nil {
		t.Fatal("0 id must become NULL")
	}
	one := int32(1)
	if got := instanceTaskIDTruthy(&one); got == nil || *got != 1 {
		t.Fatalf("id 1 = %v", got)
	}

	empty := ""
	if instanceTaskStringTruthy(&empty) != nil {
		t.Fatal("empty string must become NULL")
	}
	text := "debian/12"
	if got := instanceTaskStringTruthy(&text); got == nil || *got != text {
		t.Fatalf("text = %v", got)
	}
}

// TestInstanceTaskRecordJSONShape pins the Prisma model wire shape: camelCase keys in
// schema order, Dates as ISO strings, and payload always present (null when NULL).
func TestInstanceTaskRecordJSONShape(t *testing.T) {
	record := InstanceTaskWithDetails{Payload: json.RawMessage("null")}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{
		"id", "instanceId", "hostId", "userId", "taskType", "status", "progress",
		"retryCount", "imageAlias", "sshKeyId", "customInitCommandIds", "targetName",
		"targetHostId", "snapshotName", "newInstanceId", "payload", "error",
		"createdAt", "startedAt", "finishedAt",
	}
	order := keyOrder(t, encoded)
	if !reflect.DeepEqual(order, wantKeys) {
		t.Fatalf("key order:\n got %v\nwant %v", order, wantKeys)
	}
	if string(decoded["payload"]) != "null" {
		t.Fatalf("payload = %s, want null", decoded["payload"])
	}
	if string(decoded["createdAt"]) != `""` {
		// Zero time is not a valid instant; the converter always fills it from a row.
		t.Fatalf("createdAt = %s", decoded["createdAt"])
	}
}

// TestInstanceTaskQueueOrdering pins the two orderings the TypeScript uses and that
// the SQL above reproduces:
//
//	conflict lookup (createInstanceTask): createdAt ASC, id ASC — report the OLDEST
//	  active task, because it is the one blocking the instance.
//	user list / active lookup:            createdAt DESC — show the NEWEST first.
func TestInstanceTaskQueueOrdering(t *testing.T) {
	if InstanceOperationLockNamespace != 4106 {
		t.Fatalf("lock namespace = %d, want 4106", InstanceOperationLockNamespace)
	}
	source, err := os.ReadFile("instance_tasks.go")
	if err != nil {
		t.Fatalf("read instance_tasks.go: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, "ORDER BY created_at ASC, id ASC") {
		t.Fatal("the conflict lookup must order by created_at ASC, id ASC")
	}
	if !strings.Contains(text, "ORDER BY created_at DESC") {
		t.Fatal("the list/active lookups must order by created_at DESC")
	}
}
