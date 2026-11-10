// Port of the database-facing halves of
// server/src/lib/node-security/report.ts, recovery-task.ts and
// routes/node-security.ts.
//
// Everything here is a security control, so the invariants are explicit:
//
//   - recordReport serialises each node's reports with the two-argument
//     transaction advisory lock (700260, hostId). The lock and every read/write
//     share one transaction; taking it on the bare pool would scope it to a
//     single statement and release it immediately.
//   - a report is only persisted when its revision is not older than the durable
//     one, so a delayed delivery cannot roll a newer lock revision backwards.
//     The two reason values that explicitly describe an unavailable node are the
//     exception, exactly as in the TypeScript.
//   - a node whose security state is recoveryBlocked always reports locked, so
//     recovery cannot be skipped by a stale/duplicated report.
//   - the nodeCliEvent insert uses ON CONFLICT DO NOTHING (Prisma skipDuplicates)
//     because the primary key is (host_id, event_id) and agents retry.
//
// The package must not import server-go/src/lib (lib already imports db). The
// parsed report shape is therefore declared here, as the column contract of
// node_security_states.report.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Node security advisory lock namespaces.
//
// These are a cross-process contract with the Node backend: the same numeric
// namespaces must be used on both sides, otherwise the parallel-run window would
// let Node and Go interleave the same critical section. They are NOT in
// db/advisory-locks.ts (which only lists the money/instance namespaces) but are
// the literal values embedded in the Node source.
const (
	// NodeSecurityReportLockNamespace serialises recordReport per node.
	NodeSecurityReportLockNamespace int32 = 700260
	// NodeRecoveryEnqueueLockNamespace serialises enqueueNodeRecovery per node.
	NodeRecoveryEnqueueLockNamespace int32 = 700261
)

// NodeRecoveryKind mirrors NODE_RECOVERY_KIND in recovery-task.ts.
//
// It must stay equal to lib.OperationTaskKindNodeRecover; lib/node_security_test.go
// asserts that, because db cannot import lib.
const NodeRecoveryKind = "node_recover"

// nodeCliEventRetentionDays mirrors the `30 * 86400000` cleanup window.
const nodeCliEventRetentionDays = 30

// NodeSecurityResumeInstance is one allow-listed pending workload identity stored
// in the report JSON. Only project and name survive parsing: the node's uuid and
// frozen flag are kept in the *recovery* state, never in the persisted report.
type NodeSecurityResumeInstance struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

// NodeSecurityResult is one allow-listed storage verification result.
type NodeSecurityResult struct {
	Target  string `json:"target"`
	Success bool   `json:"success"`
	Detail  string `json:"detail"`
}

// NodeSecurityReport is the allow-listed document stored in
// node_security_states.report.
//
// The field set and order mirror the TypeScript object literal in parseReport:
// { enabled, locked, recoveryPending, startupRecovering, revision, results,
//
//	resumeInstances, reason }.
//
// Nothing else from the agent's payload may ever reach the column.
type NodeSecurityReport struct {
	Enabled           bool                         `json:"enabled"`
	Locked            bool                         `json:"locked"`
	RecoveryPending   bool                         `json:"recoveryPending"`
	StartupRecovering bool                         `json:"startupRecovering"`
	Revision          int64                        `json:"revision"`
	Results           []NodeSecurityResult         `json:"results"`
	ResumeInstances   []NodeSecurityResumeInstance `json:"resumeInstances"`
	Reason            string                       `json:"reason"`
}

// NodeCLIEventInput is one validated CLI event, already allow-listed by
// lib.ParseNodeReport. The uid arrives as a JavaScript Number (0..4294967295)
// and is stored in a BigInt column.
type NodeCLIEventInput struct {
	EventID    string
	Command    string
	UID        int64
	PID        int32
	Critical   bool
	Blocked    bool
	OccurredAt time.Time
}

// ==================== node_security_states ====================

// GetHostForNodeSecurity loads the columns the node client needs.
//
// The TypeScript does `prisma.host.findUnique({ where: { id } })` and then reads
// url/certPath/keyPath/tunnelEnabled/targetHost/targetPort, so only those are
// selected here.
func GetHostForNodeSecurity(ctx context.Context, hostID int32) (*models.Host, error) {
	var host models.Host
	err := DB.WithContext(ctx).
		Select("id", "url", "cert_path", "key_path", "tunnel_enabled", "target_host", "target_port").
		Where("id = ?", hostID).
		Take(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// NodeSecurityHostExists mirrors `prisma.host.findUnique({select:{id:true}})`.
func NodeSecurityHostExists(ctx context.Context, hostID int32) (bool, error) {
	var id int32
	err := DB.WithContext(ctx).Raw("SELECT id FROM hosts WHERE id = $1", hostID).Scan(&id).Error
	if err != nil {
		return false, err
	}
	return id != 0, nil
}

// GetNodeSecurityState mirrors `prisma.nodeSecurityState.findUnique({where:{hostId}})`.
func GetNodeSecurityState(ctx context.Context, hostID int32) (*models.NodeSecurityState, error) {
	var state models.NodeSecurityState
	err := DB.WithContext(ctx).Where("host_id = ?", hostID).Take(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// UpsertNodeSecurityFingerprint mirrors the identity route's
// `nodeSecurityState.upsert({create:{hostId, report:{}, serverFingerprint}, update:{serverFingerprint}})`.
//
// The report column is required by the schema, so the insert seeds it with `{}`
// exactly like the TypeScript.
func UpsertNodeSecurityFingerprint(ctx context.Context, hostID int32, fingerprint string) error {
	return DB.WithContext(ctx).Exec(
		`INSERT INTO node_security_states (host_id, report, server_fingerprint)
		 VALUES ($1, '{}'::jsonb, $2)
		 ON CONFLICT (host_id) DO UPDATE SET server_fingerprint = EXCLUDED.server_fingerprint`,
		hostID, fingerprint,
	).Error
}

// GetNodeSecurityRecoveryGeneration mirrors
// `findUnique({where:{hostId}, select:{recoveryGeneration:true}})`:
// a missing row yields nil, nil.
func GetNodeSecurityRecoveryGeneration(ctx context.Context, hostID int32) (*int32, error) {
	row := DB.WithContext(ctx).Raw(
		"SELECT recovery_generation FROM node_security_states WHERE host_id = ?", hostID,
	).Row()

	var generation int32
	switch err := row.Scan(&generation); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &generation, nil
}

// ClearNodeSecurityRecoveryBlocked mirrors
// `updateMany({where:{hostId, recoveryGeneration}, data:{recoveryBlocked:false}})`.
// The route treats any count other than 1 as "state changed during recovery".
func ClearNodeSecurityRecoveryBlocked(ctx context.Context, hostID, generation int32) (int64, error) {
	result := DB.WithContext(ctx).Exec(
		"UPDATE node_security_states SET recovery_blocked = false WHERE host_id = $1 AND recovery_generation = $2",
		hostID, generation,
	)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ==================== node_storage_keys ====================

// ListNodeStorageKeys mirrors the GET projection:
// select {id, dataset, fingerprint, version, createdAt}.
func ListNodeStorageKeys(ctx context.Context, hostID int32) ([]models.NodeStorageKey, error) {
	var keys []models.NodeStorageKey
	err := DB.WithContext(ctx).
		Select("id", "host_id", "dataset", "fingerprint", "version", "created_at").
		Where("host_id = ?", hostID).
		Order("id").
		Find(&keys).Error
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// GetNodeStorageKey mirrors `findUnique({where:{hostId_dataset:{hostId,dataset}}})`.
func GetNodeStorageKey(ctx context.Context, hostID int32, dataset string) (*models.NodeStorageKey, error) {
	var record models.NodeStorageKey
	err := DB.WithContext(ctx).
		Where("host_id = ? AND dataset = ?", hostID, dataset).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// CreateNodeStorageKey mirrors
// `create({data:{hostId, dataset, fingerprint, ciphertext}, select:{id,dataset,fingerprint,version}})`.
func CreateNodeStorageKey(ctx context.Context, hostID int32, dataset, fingerprint, ciphertext string) (*models.NodeStorageKey, error) {
	var record models.NodeStorageKey
	err := DB.WithContext(ctx).Raw(
		`INSERT INTO node_storage_keys (host_id, dataset, fingerprint, ciphertext)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, dataset, fingerprint, version`,
		hostID, dataset, fingerprint, ciphertext,
	).Scan(&record).Error
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// CreateNodeSecurityLogStrict mirrors `prisma.log.create(...)` in the
// reveal/unlock path, which is the one place an audit failure must abort the
// request ("审计记录未能保存，未提供密钥"). db.CreateLog is deliberately not used:
// it swallows errors.
func CreateNodeSecurityLogStrict(ctx context.Context, userID int64, module, action, content, result string) error {
	return DB.WithContext(ctx).Exec(
		`INSERT INTO logs (user_id, instance_id, module, action, content, result)
		 VALUES ($1, NULL, $2, $3, $4, $5)`,
		userID, module, action, content, result,
	).Error
}

// ==================== node_cli_events ====================

// ListNodeCliEvents mirrors
// `findMany({where:{hostId, command:{in: [...]}}, orderBy:{receivedAt:'desc'}, take:100})`.
//
// `IN ?` is GORM's slice expansion (one placeholder per allowed command); the
// allow-list is fixed at 49 entries so the statement stays small.
func ListNodeCliEvents(ctx context.Context, hostID int32, commands []string, take int) ([]models.NodeCliEvent, error) {
	var events []models.NodeCliEvent
	err := DB.WithContext(ctx).
		Where("host_id = ? AND command IN ?", hostID, commands).
		Order("received_at DESC").
		Limit(take).
		Find(&events).Error
	if err != nil {
		return nil, err
	}
	return events, nil
}

// ==================== report transaction ====================

// RecordNodeSecurityReport mirrors recordReport's transaction body.
//
// The returned count is Prisma's `inserted.count`: how many CLI events were
// actually written (duplicates are skipped). The report is mutated in place when
// the node is recovery-blocked, mirroring `state = { ...state, locked: true,
// reason: 'startup_verification' }`.
func RecordNodeSecurityReport(ctx context.Context, hostID int32, report NodeSecurityReport, events []NodeCLIEventInput) (int64, error) {
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Serialize this node's reports. Two-argument form: (namespace, key).
	if err := AdvisoryTransactionLock(ctx, tx, NodeSecurityReportLockNamespace, hostID); err != nil {
		return 0, err
	}

	var (
		recoveryBlocked bool
		previousReport  []byte
	)
	err = tx.QueryRow(ctx,
		"SELECT recovery_blocked, report FROM node_security_states WHERE host_id = $1", hostID,
	).Scan(&recoveryBlocked, &previousReport)
	hasPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	// A node awaiting recovery must never be reported as unlocked, no matter what
	// the (possibly delayed) agent payload says.
	report = NodeSecurityApplyRecoveryBlocked(report, hasPrevious && recoveryBlocked)

	previousRevision := 0.0
	if hasPrevious {
		previousRevision = NodeReportRevisionOf(previousReport)
	}

	// Preserve a newer durable lock revision against delayed delivery. The two
	// "unavailable" reasons always persist so the panel can see the node is down.
	if NodeSecurityShouldPersistReport(report.Revision, report.Reason, previousRevision) {
		encoded, err := json.Marshal(report)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO node_security_states (host_id, report, reported_at)
			 VALUES ($1, $2::jsonb, NOW())
			 ON CONFLICT (host_id) DO UPDATE
			   SET report = EXCLUDED.report, reported_at = NOW()`,
			hostID, string(encoded),
		); err != nil {
			return 0, err
		}
	}

	// 30-day retention for CLI events.
	if _, err := tx.Exec(ctx,
		"DELETE FROM node_cli_events WHERE host_id = $1 AND received_at < $2",
		hostID, time.Now().UTC().Add(-nodeCliEventRetentionDays*24*time.Hour),
	); err != nil {
		return 0, err
	}

	var inserted int64
	if len(events) > 0 {
		batch := &pgx.Batch{}
		const insertEvent = `INSERT INTO node_cli_events
			(host_id, event_id, command, uid, pid, critical, blocked, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`
		for _, event := range events {
			batch.Queue(insertEvent, hostID, event.EventID, event.Command, event.UID,
				event.PID, event.Critical, event.Blocked, event.OccurredAt)
		}
		results := tx.SendBatch(ctx, batch)
		for range events {
			tag, execErr := results.Exec()
			if execErr != nil {
				_ = results.Close()
				return 0, execErr
			}
			inserted += tag.RowsAffected()
		}
		if err := results.Close(); err != nil {
			return inserted, err
		}

		if inserted > 0 {
			blocked := false
			for _, event := range events {
				if event.Blocked {
					blocked = true
					break
				}
			}
			if blocked {
				if _, err := tx.Exec(ctx,
					`INSERT INTO logs (user_id, instance_id, module, action, content, result)
					 VALUES (NULL, NULL, 'security', 'node.cli_alarm', $1, 'warning')`,
					fmt.Sprintf("节点 #%d 上报关键命令告警，请检查节点锁定与存储状态", hostID),
				); err != nil {
					return inserted, err
				}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

// NodeSecurityApplyRecoveryBlocked mirrors
//
//	if (previous?.recoveryBlocked) state = { ...state, locked: true, reason: 'startup_verification' }
//
// A node that is still awaiting recovery can never be persisted as unlocked, so a
// stale or replayed "unlocked" report cannot make the panel believe the node is
// safe.
func NodeSecurityApplyRecoveryBlocked(report NodeSecurityReport, recoveryBlocked bool) NodeSecurityReport {
	if !recoveryBlocked {
		return report
	}
	report.Locked = true
	report.Reason = "startup_verification"
	return report
}

// NodeSecurityShouldPersistReport mirrors
//
//	Number(state.revision) >= previousRevision || state.reason === 'state_unavailable'
//	  || state.reason === 'audit_unavailable'
//
// A NaN previousRevision (an unparsable stored revision) makes every comparison
// false, exactly like JavaScript, so only an explicit "unavailable" report lands.
func NodeSecurityShouldPersistReport(revision int64, reason string, previousRevision float64) bool {
	if float64(revision) >= previousRevision {
		return true
	}
	return reason == "state_unavailable" || reason == "audit_unavailable"
}

// NodeReportRevisionOf mirrors
//
//	const previousRevision = previous?.report && typeof previous.report === 'object'
//	  && !Array.isArray(previous.report) ? Number(previous.report.revision ?? 0) : 0
//
// It returns NaN when the stored revision is not numeric, because JavaScript's
// `Number(undefined-like)` is NaN and every `>= NaN` comparison is false.
func NodeReportRevisionOf(raw []byte) float64 {
	if len(raw) == 0 {
		return 0
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return math.NaN()
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return 0
	}
	value, present := object["revision"]
	if !present || value == nil {
		// `report.revision ?? 0`
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		return jsNumberFromString(typed)
	case bool:
		if typed {
			return 1
		}
		return 0
	default:
		return math.NaN()
	}
}

// jsNumberFromString mirrors JavaScript's Number(string).
//
// Only the shapes that can appear in a stored revision are needed: the column is
// always written by parseReport as a JSON number, so this defends the comparison
// against a hand-edited or legacy row rather than a live code path.
func jsNumberFromString(raw string) float64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return math.NaN()
	}
	return value
}

// ==================== operation_tasks ====================

// EnqueueNodeRecoveryTask mirrors the transactional half of enqueueNodeRecovery.
//
// `created` reports whether this call inserted the task; only then may the caller
// schedule the recovery action. The insert must be durable before any node
// request is made, because a failed insert must never unlock a node.
func EnqueueNodeRecoveryTask(ctx context.Context, hostID int32) (taskID int32, created bool, err error) {
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := AdvisoryTransactionLock(ctx, tx, NodeRecoveryEnqueueLockNamespace, hostID); err != nil {
		return 0, false, err
	}

	var existingID int32
	err = tx.QueryRow(ctx,
		`SELECT id FROM operation_tasks
		 WHERE host_id = $1 AND kind = $2 AND status = 'running'
		 ORDER BY id DESC LIMIT 1`,
		hostID, NodeRecoveryKind,
	).Scan(&existingID)
	if err == nil {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return 0, false, commitErr
		}
		return existingID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}

	var createdID int32
	if err := tx.QueryRow(ctx,
		`INSERT INTO operation_tasks (host_id, instance_name, kind, status)
		 VALUES ($1, $2, $3, 'running')
		 RETURNING id`,
		hostID, fmt.Sprintf("节点 #%d", hostID), NodeRecoveryKind,
	).Scan(&createdID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return createdID, true, nil
}

// NodeRecoveryTaskView is the projection latestNodeRecovery selects.
type NodeRecoveryTaskView struct {
	ID         int32      `gorm:"column:id"`
	Status     string     `gorm:"column:status"`
	Error      *string    `gorm:"column:error"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

// LatestNodeRecoveryTask mirrors
// `operationTask.findFirst({where:{hostId, kind}, orderBy:{id:'desc'},
//
//	select:{id,status,error,createdAt,finishedAt}})`.
func LatestNodeRecoveryTask(ctx context.Context, hostID int32) (*NodeRecoveryTaskView, error) {
	var task NodeRecoveryTaskView
	// The explicit table is required: GORM derives a name from the struct
	// ("node_recovery_task_views") when none is given, and that relation does not
	// exist. The source row is operation_tasks, like Prisma's operationTask model.
	err := DB.WithContext(ctx).
		Table("operation_tasks").
		Select("id", "status", "error", "created_at", "finished_at").
		Where("host_id = ? AND kind = ?", hostID, NodeRecoveryKind).
		Order("id DESC").
		Limit(1).
		Take(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// NodeRecoveryInstance is the instance row the recovery allow-list consults.
type NodeRecoveryInstance struct {
	Status    string     `gorm:"column:status"`
	ExpiresAt *time.Time `gorm:"column:expires_at"`
}

// FindInstanceForNodeRecovery mirrors
// `prisma.instance.findFirst({where:{hostId, incusId}, select:{status,expiresAt}})`.
func FindInstanceForNodeRecovery(ctx context.Context, hostID int32, incusID string) (*NodeRecoveryInstance, error) {
	var instance NodeRecoveryInstance
	err := DB.WithContext(ctx).
		Select("status", "expires_at").
		Where("host_id = ? AND incus_id = ?", hostID, incusID).
		Take(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &instance, nil
}

// NodeSecurityStoredReport implements the GET route's offline fallback:
//
//	state: reported?.report && typeof reported.report === 'object'
//	  && 'locked' in reported.report ? reported.report : null
//
// A stored `{}` (written by the identity enrollment) has no `locked` key and
// therefore reads as null, not as an empty object.
func NodeSecurityStoredReport(state *models.NodeSecurityState) json.RawMessage {
	if state == nil || len(state.Report) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(state.Report, &decoded); err != nil {
		return nil
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	if _, hasLocked := object["locked"]; !hasLocked {
		return nil
	}
	return json.RawMessage(state.Report)
}
