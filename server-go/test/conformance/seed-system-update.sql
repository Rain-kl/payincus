-- system-update fixture rows for test/conformance/requests-system-update.json.
--
-- The harness replays the same requests against the Node reference backend and
-- the Go backend on the SAME database and diffs status + JSON body. Every case in
-- the request list is READ-ONLY or an expected negative: /start and
-- /tasks/:id/rollback are deliberately NOT compared because the Go backend
-- refuses them with 501 while Node would really start an OTA (see
-- routes/system_update.go). Those two seams are covered by unit tests with a
-- recording store.
--
-- Id range: 996001+ (this module's block). No users/hosts are inserted: the rows
-- reference the `admin` identity that the parity seed already creates.
--
-- Idempotent: every INSERT is ON CONFLICT (id) DO NOTHING and no row is rewritten.

BEGIN;

-- ---------------------------------------------------------------------------
-- Three task rows with distinct created_at values so `ORDER BY created_at DESC`
-- is deterministic:
--   996001  success, backup present, no log file
--   996002  failed,  no backup,     error message set
--   996003  rolled_back, backup present, log_path deliberately OUTSIDE the
--           update log directory (the logs route must answer 403)
-- ---------------------------------------------------------------------------
INSERT INTO system_update_tasks (
  id, target_version, from_version, status, started_by_user_id,
  backup_path, log_path, error_message, started_at, finished_at,
  created_at, updated_at
)
SELECT
  v.id,
  v.target_version,
  v.from_version,
  v.status::"SystemUpdateTaskStatus",
  (SELECT id FROM users WHERE username = 'admin'),
  v.backup_path,
  v.log_path,
  v.error_message,
  v.started_at,
  v.finished_at,
  v.created_at,
  v.updated_at
FROM (
  VALUES
    (996001, 'v1.5.8', 'v1.4.0', 'success',
     '/opt/incudal/backups/parity-996001', NULL::text, NULL::text,
     timestamp '2026-09-01 10:00:00.000', timestamp '2026-09-01 10:05:00.000',
     timestamp '2026-09-01 09:59:59.500', timestamp '2026-09-01 10:05:00.000'),
    (996002, 'v1.5.9', 'v1.5.8', 'failed',
     NULL::text, NULL::text, 'simulated updater failure',
     timestamp '2026-09-02 10:00:00.000', timestamp '2026-09-02 10:01:00.000',
     timestamp '2026-09-02 09:59:59.500', timestamp '2026-09-02 10:01:00.000'),
    (996003, 'v1.6.0', 'v1.5.9', 'rolled_back',
     '/opt/incudal/backups/parity-996003', '/tmp/payincus-parity-outside-update.log', NULL::text,
     timestamp '2026-09-03 10:00:00.000', timestamp '2026-09-03 10:10:00.000',
     timestamp '2026-09-03 09:59:59.500', timestamp '2026-09-03 10:10:00.000')
) AS v(id, target_version, from_version, status, backup_path, log_path, error_message,
       started_at, finished_at, created_at, updated_at)
WHERE EXISTS (SELECT 1 FROM users WHERE username = 'admin')
ON CONFLICT (id) DO NOTHING;

COMMIT;
