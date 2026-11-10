-- Backups-module fixture rows for test/conformance/requests-backups.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs status + JSON body, so the backup,
-- restore-task and upload-task reads need real rows before either server starts.
--
-- Every row uses an id >= 940001 so it cannot collide with the auto-increment
-- sequences or the other waves' fixtures (hosts 900001, instance-billing 920001,
-- packages 930001). The file is idempotent (ON CONFLICT DO NOTHING), so re-running
-- never duplicates fixtures.
--
-- Users are resolved by username (admin / parityuser), never by a hard-coded id.
--
-- IMPORTANT: no PENDING / PROCESSING task rows are seeded. The Node backend starts
-- its restore and upload workers with the app, and those workers immediately claim
-- such rows (measured: a PENDING upload was marked FAILED within seconds, a
-- PENDING restore was attempted and failed on the missing certificate). Such rows
-- can therefore never be compared across two backends. Pending-state behaviour
-- (active-task 409, queue length 429, queue position) is covered by the
-- database-backed Go unit tests, which run without workers.
--
-- Only one write case mutates a row that later cases read:
--   PUT /api/instances/940004/backup-policy {"enabled":false}
-- creates (first run) or updates (later runs) the policy row of 940004 and leaves
-- next_run_at untouched, so the following GET is identical on both backends and
-- stable across runs. No backup, restore task or upload task is written by any
-- case.

BEGIN;

-- Drop the PENDING/PROCESSING fixtures an earlier version of this file seeded
-- (they were consumed/mutated by the Node workers). Scoped to the exact ids this
-- file used, so rows created by other waves (or by parity writes) are untouched.
DELETE FROM backup_upload_tasks WHERE id IN (940001, 940003, 940101);
DELETE FROM restore_tasks WHERE id IN (940003, 940101) OR id BETWEEN 940011 AND 940020;

-- 940001: admin-owned host. The URL refuses connections immediately, so any path
-- that does reach Incus fails fast; such paths are intentionally absent from the
-- request list.
INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
                   cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
                   created_at, updated_at)
SELECT 940001, u.id, 'parity-backups-host', 'https://127.0.0.1:1', 'online',
       0, 0, 0, 10000, 1048576, 1000, 'zfs', 'loop',
       now() - interval '40 days', now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 940001: parityuser's instance, backup_limit 2, holding 2 active backups
-- (940001 ready + 940002 creating) so create refuses with "(2/2)".
INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
                       cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
                       created_at, updated_at)
SELECT 940001, 'parity-backups-940001', 'parity-backups-owner', u.id, 940001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 2, 2, 2,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 940002: admin's instance — a parityuser request is a stranger (403).
INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
                       cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
                       created_at, updated_at)
SELECT 940002, 'parity-backups-940002', 'parity-backups-admin', u.id, 940001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 2, 2, 2,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 940003: suspended because of expiry → create backup answers 403
-- INSTANCE_SUSPENDED_EXPIRED before the quota check.
INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
                       cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
                       suspend_reason, suspended_at, created_at, updated_at)
SELECT 940003, 'parity-backups-940003', 'parity-backups-suspended', u.id, 940001,
       'images:debian/12', 'suspended', 1, 1024, 10240, 'nat', 2, 2, 2,
       'expired', now() - interval '2 days', now() - interval '40 days', now() - interval '2 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 940004: backup_limit 0 → "Backup quota not allocated for this instance" and
-- BACKUP_QUOTA_NOT_SET when enabling the policy. It is also the instance whose
-- policy row the single write case creates.
INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
                       cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
                       created_at, updated_at)
SELECT 940004, 'parity-backups-940004', 'parity-backups-noquota', u.id, 940001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat', 0, 0, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- Backups. 940003 is soft-deleted and must never appear in the list; 940005 is in
-- 'error' so export/restore/upload answer 400 BACKUP_NOT_READY.
INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at, expires_at) VALUES
  (940001, 940001, 'parity-backup-ready',    'parity-ready',    'fixture ready',    1024, 'ready',    '2024-01-02 03:04:05.678', NULL),
  (940002, 940001, 'parity-backup-creating', 'parity-creating', NULL,               0,    'creating', '2024-01-03 04:05:06.789', NULL),
  (940003, 940001, 'parity-backup-deleted',  'parity-deleted',  NULL,               0,    'deleted',  '2024-01-04 05:06:07.890', NULL),
  (940004, 940002, 'parity-backup-admin',    'parity-admin',    'admin instance',   2048, 'ready',    '2024-01-02 06:07:08.901', NULL),
  (940005, 940001, 'parity-backup-error',    'parity-error',    NULL,               0,    'error',    '2024-01-05 07:08:09.012', NULL)
ON CONFLICT (id) DO NOTHING;

-- Policy row for 940001 so GET/PUT are deterministic.
INSERT INTO backup_policies (id, instance_id, enabled, interval_minutes, last_run_at, next_run_at,
                             created_at, updated_at)
VALUES (940001, 940001, true, 360,
        '2024-01-05 06:07:08.901', '2024-01-05 12:07:08.901',
        '2024-01-01 00:00:00.000', '2024-01-05 06:07:08.901')
ON CONFLICT (id) DO NOTHING;

-- Restore tasks: one COMPLETED (fixed 60 s duration) and one FAILED. Both are
-- terminal, so the workers leave them alone.
INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status, progress,
                           retry_count, temp_instance_name, original_instance_name,
                           original_incus_id, error, created_at, started_at, finished_at)
SELECT 940001, 940001, 940001, 940001, u.id, 'COMPLETED', NULL, 0, 'parity-restore-temp',
       'parity-backups-owner', 'parity-backups-940001', NULL,
       '2024-01-06 00:00:00.000', '2024-01-06 00:00:10.000', '2024-01-06 00:01:10.000'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status, progress,
                           retry_count, temp_instance_name, original_instance_name,
                           original_incus_id, error, created_at, started_at, finished_at)
SELECT 940002, 940001, 940001, 940001, u.id, 'FAILED', NULL, 0, 'parity-restore-temp',
       'parity-backups-owner', 'parity-backups-940001', 'boom',
       '2024-01-06 00:30:00.000', '2024-01-06 00:30:10.000', '2024-01-06 00:31:10.000'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- parityuser's default remote storage (upload_remote needs a row to resolve).
INSERT INTO storage_configs (id, user_id, name, type, host, port, username, password,
                             base_path, extra, is_default, created_at, updated_at)
SELECT 940001, u.id, 'parity-backups-s3', 'S3', 's3.parity.invalid', 443,
       'parity', 'secret', 'backups', NULL, true,
       now() - interval '40 days', now() - interval '40 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- One COMPLETED upload task (fixed 60 s duration, known remote name and size).
INSERT INTO backup_upload_tasks (id, user_id, instance_id, backup_id, host_id, storage_config_id,
                                 status, progress, retry_count, remote_file_name, file_size,
                                 error, created_at, started_at, finished_at)
SELECT 940002, u.id, 940001, 940001, 940001, 940001, 'COMPLETED', NULL, 0,
       'remote-backup.tar.gz', 123456, NULL,
       '2024-01-06 03:00:00.000', '2024-01-06 03:00:10.000', '2024-01-06 03:01:10.000'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- The sequences are intentionally left alone: fixtures use explicit ids below the
-- other waves' high-water marks, and forcing a setval here would make later
-- auto-increment writes land inside this module's id range.

COMMIT;
