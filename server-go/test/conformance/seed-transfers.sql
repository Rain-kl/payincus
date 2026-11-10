-- Instance-transfer fixture rows for test/conformance/requests-transfers.json.
--
-- The harness replays the same requests against the Node reference backend and the
-- Go backend on the SAME database and diffs status + JSON body. Every fixture here
-- is read-only for the compared cases: the request list deliberately contains no
-- successful accept/push (those move instance ownership and call Incus) and no
-- successful reject/cancel (those mutate the transfer state and refund money).
-- Those paths are covered by unit tests (TestTransfersAccept*, TestTransfersReject*,
-- TestTransfersCancel*, TestTransfersPush*) with injected seams.
--
-- Id ranges: users 993001-993004, hosts 993001-993002, instances 993101-993110,
-- transfers 993001-993012. Everything is inside the 993001+ block assigned to this
-- module, so nothing collides with the 980xxx (public-api) or 950xxx
-- (instance-destroy) fixtures.
--
-- Idempotent: every INSERT is ON CONFLICT (id) DO NOTHING and no row is rewritten.
-- The two participants that need a login are resolved by username, never by id.

BEGIN;

-- ---------------------------------------------------------------------------
-- Users: a sender, a recipient-shaped target, a third party and a banned target.
-- (parityuser takes part through the NON_ADMIN token.)
-- ---------------------------------------------------------------------------
INSERT INTO users (id, username, password_hash, status, balance, updated_at)
VALUES
  (993001, 'parity-transfer-from', 'x', 'active', 100.00, now() at time zone 'utc'),
  (993002, 'parity-transfer-to', 'x', 'active', 0.00, now() at time zone 'utc'),
  (993003, 'parity-transfer-third', 'x', 'active', 0.00, now() at time zone 'utc'),
  (993004, 'parity-transfer-banned', 'x', 'banned', 0.00, now() at time zone 'utc')
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Hosts: 993001 allows transfers, 993002 has the host-level switch off.
-- ---------------------------------------------------------------------------
INSERT INTO hosts (id, user_id, name, url, transfer_enabled, caddy_enabled, updated_at)
VALUES
  (993001, 993001, 'Parity Transfer Host', 'https://host.transfer.parity.invalid:8443', true, false,
   now() at time zone 'utc'),
  (993002, 993001, 'Parity Transfer Host Disabled', 'https://host.transfer-disabled.parity.invalid:8443',
   false, false, now() at time zone 'utc')
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Instances. The owner alternates between parity-transfer-from and parityuser so
-- the create-route ownership branch can be exercised in both directions.
-- ---------------------------------------------------------------------------
INSERT INTO instances (
  id, incus_id, name, user_id, host_id, image, status, suspend_reason,
  cpu, memory, disk, ipv4, network_mode, display_order, auto_renew, created_at, updated_at
)
SELECT v.id, v.incus_id, v.name, v.owner_id, v.host_id, 'debian/12', v.status::"InstanceStatus", v.suspend_reason,
       1, 512, 5120, v.ipv4, 'nat', 0, true,
       timestamp '2026-03-01 00:00:00.000', timestamp '2026-03-01 00:00:00.000'
  FROM (
    VALUES
      (993101, 'parity-transfer-incus-993101', 'parity-transfer-box',        993001,     993001, 'stopped',   NULL::text,      '203.0.113.201'),
      (993102, 'parity-transfer-incus-993102', 'parity-transfer-box-two',    993001,     993001, 'stopped',   NULL,            '203.0.113.202'),
      (993103, 'parity-transfer-incus-993103', 'parity-transfer-suspended',  993001,     993001, 'suspended', 'expired',       '203.0.113.203'),
      (993104, 'parity-transfer-incus-993104', 'parity-transfer-mine',       (SELECT id FROM users WHERE username = 'parityuser'), 993001, 'stopped', NULL, '203.0.113.204'),
      (993105, 'parity-transfer-incus-993105', 'parity-transfer-running',    (SELECT id FROM users WHERE username = 'parityuser'), 993001, 'running', NULL, '203.0.113.205'),
      (993106, 'parity-transfer-incus-993106', 'parity-transfer-accepted',   (SELECT id FROM users WHERE username = 'parityuser'), 993001, 'stopped', NULL, '203.0.113.206'),
      (993107, 'parity-transfer-incus-993107', 'parity-transfer-detail',     (SELECT id FROM users WHERE username = 'parityuser'), 993001, 'stopped', NULL, '203.0.113.207'),
      (993108, 'parity-transfer-incus-993108', 'parity-transfer-disabled',   993001,     993002, 'stopped',   NULL,            '203.0.113.208'),
      (993109, 'parity-transfer-incus-993109', 'parity-transfer-mine-expired', (SELECT id FROM users WHERE username = 'parityuser'), 993001, 'suspended', 'expired', '203.0.113.209'),
      (993110, 'parity-transfer-incus-993110', 'parity-transfer-mine-disabled', (SELECT id FROM users WHERE username = 'parityuser'), 993002, 'stopped', NULL, '203.0.113.210')
  ) AS v(id, incus_id, name, owner_id, host_id, status, suspend_reason, ipv4)
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Transfers. created_at increases with the id so the list order is total and
-- deterministic. The snapshot is the real TransferSnapshot shape.
-- ---------------------------------------------------------------------------
INSERT INTO instance_transfers (
  id, instance_id, from_user_id, to_user_id, status, snapshot, remark, reject_reason, fee,
  created_at, updated_at, accepted_at, rejected_at, cancelled_at
)
SELECT v.id, v.instance_id, v.from_user_id, v.to_user_id, v.status::"TransferStatus",
       jsonb_build_object(
         'packageId', NULL,
         'packageName', NULL,
         'hostId', 993001,
         'hostName', 'Parity Transfer Host',
         'hostLocation', NULL,
         'hostCountryCode', 'us',
         'originalName', v.instance_name,
         'cpu', 1,
         'memory', 512,
         'disk', 5120,
         'networkMode', 'nat',
         'portMappingsCount', 0,
         'snapshotsCount', 0,
         'backupsCount', 0,
         'ipv4', NULL,
         'ipv6', NULL
       ),
       v.remark, v.reject_reason, v.fee,
       v.created_at, v.created_at, v.accepted_at, v.rejected_at, v.cancelled_at
  FROM (
    VALUES
      -- 993001: pending, sender → parityuser (detail/list/pending-count; NEVER accepted).
      (993001::int, 993101::int, 993001::int,
       (SELECT id FROM users WHERE username = 'parityuser'),
       'pending', 'parity-transfer-box', 'please take it'::text, NULL::text, NULL::numeric,
       timestamp '2026-03-01 10:00:00.000', NULL::timestamp, NULL::timestamp, NULL::timestamp),
      -- 993002: pending, parityuser → third party (sent list / cancel guard).
      (993002, 993104, (SELECT id FROM users WHERE username = 'parityuser'), 993002,
       'pending', 'parity-transfer-mine', NULL, NULL, NULL,
       timestamp '2026-03-01 10:01:00.000', NULL, NULL, NULL),
      -- 993003: accepted, parityuser → third party (accept/cancel/push NOT_PENDING).
      (993003, 993106, (SELECT id FROM users WHERE username = 'parityuser'), 993002,
       'accepted', 'parity-transfer-accepted', NULL, NULL, 5.00,
       timestamp '2026-03-01 10:02:00.000', timestamp '2026-03-01 10:02:30.000', NULL, NULL),
      -- 993004: rejected, sender → parityuser.
      (993004, 993101, 993001, (SELECT id FROM users WHERE username = 'parityuser'),
       'rejected', 'parity-transfer-box', NULL, 'changed my mind', NULL,
       timestamp '2026-03-01 10:03:00.000', NULL, timestamp '2026-03-01 10:03:30.000', NULL),
      -- 993005: pending on a RUNNING instance, parityuser → third party (push guard).
      (993005, 993105, (SELECT id FROM users WHERE username = 'parityuser'), 993002,
       'pending', 'parity-transfer-running', NULL, NULL, NULL,
       timestamp '2026-03-01 10:04:00.000', NULL, NULL, NULL),
      -- 993006: pending, third party → parityuser (detail 200, do not accept).
      (993006, 993106, 993002, (SELECT id FROM users WHERE username = 'parityuser'),
       'pending', 'parity-transfer-accepted', NULL, NULL, NULL,
       timestamp '2026-03-01 10:05:00.000', NULL, NULL, NULL),
      -- 993007: pending, neither party is parityuser (detail 403, reject 403).
      (993007, 993101, 993001, 993002,
       'pending', 'parity-transfer-box', NULL, NULL, NULL,
       timestamp '2026-03-01 10:06:00.000', NULL, NULL, NULL),
      -- 993008: cancelled, parityuser → third party (cancel NOT_PENDING).
      (993008, 993107, (SELECT id FROM users WHERE username = 'parityuser'), 993002,
       'cancelled', 'parity-transfer-detail', NULL, NULL, 2.00,
       timestamp '2026-03-01 10:07:00.000', NULL, NULL, timestamp '2026-03-01 10:07:30.000'),
      -- 993009: pending, sender → parityuser on a second stopped instance.
      (993009, 993102, 993001, (SELECT id FROM users WHERE username = 'parityuser'),
       'pending', 'parity-transfer-box-two', NULL, NULL, NULL,
       timestamp '2026-03-01 10:08:00.000', NULL, NULL, NULL),
      -- 993010: rejected, sender → parityuser (accept NOT_PENDING).
      (993010, 993102, 993001, (SELECT id FROM users WHERE username = 'parityuser'),
       'rejected', 'parity-transfer-box-two', NULL, NULL, NULL,
       timestamp '2026-03-01 10:09:00.000', NULL, timestamp '2026-03-01 10:09:30.000', NULL),
      -- 993011: cancelled, third party → parityuser (reject NOT_PENDING + benign body cases).
      (993011, 993102, 993003, (SELECT id FROM users WHERE username = 'parityuser'),
       'cancelled', 'parity-transfer-box-two', NULL, NULL, NULL,
       timestamp '2026-03-01 10:10:00.000', NULL, NULL, timestamp '2026-03-01 10:10:30.000'),
      -- 993012: accepted, third party → parityuser (reject NOT_PENDING).
      (993012, 993102, 993003, (SELECT id FROM users WHERE username = 'parityuser'),
       'accepted', 'parity-transfer-box-two', NULL, NULL, NULL,
       timestamp '2026-03-01 10:11:00.000', NULL, NULL, NULL)
  ) AS v(id, instance_id, from_user_id, to_user_id, status, instance_name, remark,
         reject_reason, fee, created_at, accepted_at, rejected_at, cancelled_at)
ON CONFLICT (id) DO NOTHING;

COMMIT;
