-- Public-API fixture rows for test/conformance/requests-public-api.json.
--
-- The harness replays one request list against the Node and the Go backend on the
-- SAME database and diffs status + JSON body, so reads need rows that already
-- exist before either server starts. parity.sh applies every seed-*.sql file
-- automatically; this file is idempotent and only ever writes its own id range.
--
-- Id ranges (980001+ is this module's; hosts 900001, instance-billing 920001,
-- packages 930001, backups 940001, instance-destroy 950001 are other waves):
--
--   host                 980001
--   payment provider     980001
--   packages/plans       980001/980002, plans 980011..980013
--   instances            980101..980107
--   instance transfer    980201 (PENDING, gives SERVICE_TRANSFER_LOCKED)
--   billing records      980301, 980302
--   recharge records     980401..980403
--   balance logs         980501..980503
--   tickets/messages     980601..980603, messages 980701..980703
--   instance task        980801 (COMPLETED, safe from workers)
--   public_api_tokens    980901..980903
--
-- Credentials: 980901..980903 are deliberately NON-SECRET probe tokens. Their
-- plaintexts are obviously fake, exist only in this throwaway parity database, and
-- are bound to the non-admin `parityuser`. They grant at most `profile:read`
-- (980901), are revoked (980902) or expired (980903), so none of them can be used
-- anywhere else. The fully-scoped token the positive cases use is minted by
-- parity.sh at runtime and never appears in the repository.
--
-- Rows that the request list reads are seeded with fixed timestamps so ordering is
-- stable. Nothing here is mutated by a request case except:
--   * users.avatar_style (PATCH /me with a valid style — idempotent),
--   * balance_adjustment_requests (one POST per backend per run — cleaned below),
--   * one ticket created by POST /tickets (subject prefix — cleaned below),
--   * one inbox notification per backend (cleaned below).
--
-- Users are resolved by username (admin / parityuser), never by a hard-coded id.

BEGIN;

-- ---------------------------------------------------------------------------
-- Reset the state that write cases create, so a second run starts clean.
-- ---------------------------------------------------------------------------
DELETE FROM balance_adjustment_requests
 WHERE source_type = 'public_api'
   AND user_id = (SELECT id FROM users WHERE username = 'parityuser');

DELETE FROM inbox_messages
 WHERE event_type = 'public_api_notification'
   AND content LIKE '%conformance-harness%';

DELETE FROM inbox_messages
 WHERE event_type IN ('ticket_created', 'ticket_replied')
   AND content LIKE '%conformance-public-api%';

DELETE FROM tickets
 WHERE subject LIKE 'conformance-public-api%';

-- ---------------------------------------------------------------------------
-- Host (admin-owned; the URL refuses connections immediately so no Incus path
-- is ever attempted by the read cases).
-- ---------------------------------------------------------------------------
INSERT INTO hosts (
  id, user_id, name, url, location, country_code, architecture, status,
  cpu_used, memory_used, disk_used, created_at, updated_at
)
SELECT 980001, u.id, 'Parity Public API Host', 'https://127.0.0.1:1', 'Parity DC', 'us',
       'x86_64', 'online', 0, 0, 0,
       timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Payment provider (only needed for the recharge_records FK).
-- ---------------------------------------------------------------------------
INSERT INTO payment_providers (
  id, name, type, status, config, methods, fee_rate, fee_fixed, min_amount, sort_order,
  created_at, updated_at
)
VALUES (
  980001, 'Parity Public API Provider', 'manual', 'disabled', '{}'::jsonb, '[]'::jsonb,
  0, 0, 1, 0, timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
)
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Packages and plans.
-- ---------------------------------------------------------------------------
INSERT INTO packages (
  id, user_id, name, description, cpu_max, memory_max, disk_max, bandwidth_max,
  network_mode, instance_type, active, port_limit, snapshot_limit, backup_limit,
  site_limit, monthly_traffic_limit, created_at
)
SELECT 980001, u.id, 'Parity Public API Package', 'conformance fixture',
       800, 4096, 60000, 1000, 'nat', 'container', true, 20, 5, 3, 10,
       1099511627776, timestamp '2026-01-02 00:00:00.000'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO packages (
  id, user_id, name, description, cpu_max, memory_max, disk_max, network_mode,
  instance_type, active, port_limit, snapshot_limit, backup_limit, site_limit,
  created_at
)
SELECT 980002, u.id, 'Parity Public API Inactive Package', 'conformance fixture',
       400, 2048, 30000, 'nat', 'container', false, 10, 2, 1, 5,
       timestamp '2026-01-02 00:00:00.000'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO package_plans (
  id, package_id, name, description, cpu, memory, disk, port_limit, snapshot_limit,
  backup_limit, site_limit, traffic_limit, traffic_limit_speed, price, billing_cycle,
  setup_fee, is_active, sort_order, is_sold_out, created_at, updated_at
)
VALUES
  (980011, 980001, 'Parity Public API Plan A', 'active plan', 2, 2048, 20000, 20, 5, 3, 10,
   1099511627776, '100Mbit', 25.50, 1, 0, true, 10, false,
   timestamp '2026-01-02 00:00:00.000', timestamp '2026-01-02 00:00:00.000'),
  (980012, 980001, 'Parity Public API Plan Inactive', 'inactive plan', 1, 1024, 10000, 10, 2, 1, 5,
   549755813888, '50Mbit', 10.00, 1, 0, false, 20, false,
   timestamp '2026-01-02 00:00:00.000', timestamp '2026-01-02 00:00:00.000'),
  (980013, 980001, 'Parity Public API Plan Sold Out', 'active sold-out plan', 4, 4096, 40000, 20, 5, 3, 10,
   2199023255552, '200Mbit', 49.90, 1, 5.00, true, 30, true,
   timestamp '2026-01-02 00:00:00.000', timestamp '2026-01-02 00:00:00.000')
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Instances. 980101..980106 belong to parityuser; 980107 belongs to admin so the
-- ownership guards can be exercised. 980105 is deleted and 980103/980104 are
-- suspended (expired / manual) for the operation guards.
-- ---------------------------------------------------------------------------
INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, ipv6, network_mode, port_limit, snapshot_limit,
  backup_limit, site_limit, monthly_traffic_limit, monthly_traffic_used,
  traffic_status, expires_at, suspended_at, suspend_reason, billing_price,
  billing_cycle, auto_renew, display_order, created_at, updated_at
)
SELECT 980101, 'parity-pub-980101', 'conformance-public-api-running', p.id, 980001,
       980001, 980011, 'debian/12', 'running', 2, 2048, 20000, '203.0.113.101', NULL,
       'nat', 20, 5, 3, 10, 1099511627776, 1073741824, 'NORMAL',
       timestamp '2026-12-01 00:00:00.000', NULL, NULL, 25.50, 1, false, 10,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-01-03 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, network_mode, display_order, created_at, updated_at
)
SELECT 980102, 'parity-pub-980102', 'conformance-public-api-stopped', p.id, 980001,
       980001, 980011, 'debian/12', 'stopped', 2, 2048, 20000, '203.0.113.102', 'nat', 20,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-01-03 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, network_mode, suspended_at, suspend_reason, display_order,
  created_at, updated_at
)
SELECT 980103, 'parity-pub-980103', 'conformance-public-api-suspended-expired', p.id, 980001,
       980001, 980011, 'debian/12', 'suspended', 2, 2048, 20000, '203.0.113.103', 'nat',
       timestamp '2026-02-01 00:00:00.000', 'expired', 30,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-02-01 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, network_mode, suspended_at, suspend_reason, display_order,
  created_at, updated_at
)
SELECT 980104, 'parity-pub-980104', 'conformance-public-api-suspended-manual', p.id, 980001,
       980001, 980011, 'debian/12', 'suspended', 2, 2048, 20000, '203.0.113.104', 'nat',
       timestamp '2026-02-01 00:00:00.000', 'manual', 40,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-02-01 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, network_mode, display_order, created_at, updated_at
)
SELECT 980105, 'parity-pub-980105', 'conformance-public-api-deleted', p.id, 980001,
       980001, 980011, 'debian/12', 'deleted', 2, 2048, 20000, '203.0.113.105', 'nat', 50,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-01-03 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, image, status, cpu, memory, disk, ipv4,
  network_mode, display_order, created_at, updated_at
)
SELECT 980106, 'parity-pub-980106', 'conformance-public-api-free', p.id, 980001,
       'debian/12', 'running', 1, 512, 5000, '203.0.113.106', 'nat', 60,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-01-03 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instances (
  id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
  cpu, memory, disk, ipv4, network_mode, display_order, created_at, updated_at
)
SELECT 980107, 'parity-pub-980107', 'conformance-public-api-other-user', a.id, 980001,
       980001, 980011, 'debian/12', 'running', 2, 2048, 20000, '203.0.113.107', 'nat', 70,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-01-03 00:00:00.000'
  FROM users a WHERE a.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Pending transfer on the stopped instance: starting it must answer
-- SERVICE_TRANSFER_LOCKED. PENDING transfers are user-driven (no worker claims
-- them), so this row stays put for the whole run.
-- ---------------------------------------------------------------------------
INSERT INTO instance_transfers (
  id, instance_id, from_user_id, to_user_id, status, snapshot, created_at, updated_at
)
SELECT 980201, 980102, p.id, a.id, 'pending', '{}'::jsonb,
       timestamp '2026-02-02 00:00:00.000', timestamp '2026-02-02 00:00:00.000'
  FROM users p, users a
 WHERE p.username = 'parityuser' AND a.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Billing records (one renew with a plan/package, one refund without a remark).
-- ---------------------------------------------------------------------------
INSERT INTO instance_billing_records (
  id, instance_id, user_id, type, amount, months, period_start, period_end, remark, created_at
)
SELECT 980301, 980101, p.id, 'renew', 25.50, 1,
       timestamp '2026-01-03 00:00:00.000', timestamp '2026-02-03 00:00:00.000',
       '续费 1 个月', timestamp '2026-01-04 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (
  id, instance_id, user_id, type, amount, months, period_start, period_end, remark, created_at
)
SELECT 980302, 980101, p.id, 'refund', 10.00, 1,
       timestamp '2026-01-05 00:00:00.000', timestamp '2026-02-05 00:00:00.000',
       NULL, timestamp '2026-01-05 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Recharge records: completed, pending and refunded, covering the status mapping.
-- ---------------------------------------------------------------------------
INSERT INTO recharge_records (
  id, user_id, provider_id, order_no, trade_no, amount, actual_amount, fee,
  payment_method, status, created_at, updated_at, expired_at, completed_at
)
SELECT 980401, p.id, 980001, 'PARITY-PUB-980401', '20260104000000000000000000000001',
       100.00, 100.00, 0.00, 'manual', 'completed',
       timestamp '2026-01-04 00:00:00.000', timestamp '2026-01-04 00:00:00.000',
       timestamp '2026-01-05 00:00:00.000', timestamp '2026-01-04 01:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO recharge_records (
  id, user_id, provider_id, order_no, amount, actual_amount, fee, payment_method,
  status, created_at, updated_at, expired_at
)
SELECT 980402, p.id, 980001, 'PARITY-PUB-980402', 50.00, 50.00, 0.00, 'manual', 'pending',
       timestamp '2026-01-05 00:00:00.000', timestamp '2026-01-05 00:00:00.000',
       timestamp '2026-01-06 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO recharge_records (
  id, user_id, provider_id, order_no, trade_no, amount, actual_amount, fee,
  payment_method, status, fail_reason, created_at, updated_at, completed_at
)
SELECT 980403, p.id, 980001, 'PARITY-PUB-980403', '1234567890123',
       30.00, 30.00, 0.00, 'manual', 'refunded', NULL,
       timestamp '2026-01-06 00:00:00.000', timestamp '2026-01-06 00:00:00.000',
       timestamp '2026-01-06 01:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Balance logs: recharge, consume (with instance) and a lottery gift.
-- ---------------------------------------------------------------------------
INSERT INTO balance_logs (
  id, user_id, type, amount, balance_before, balance_after, order_id, instance_id,
  remark, created_at
)
SELECT 980501, p.id, 'recharge', 100.00, 0.00, 100.00, 'PARITY-PUB-980401', NULL,
       'conformance recharge', timestamp '2026-01-04 01:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO balance_logs (
  id, user_id, type, amount, balance_before, balance_after, order_id, instance_id,
  remark, created_at
)
SELECT 980502, p.id, 'consume', 25.50, 100.00, 74.50, NULL, 980101,
       'conformance consume', timestamp '2026-01-04 02:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO balance_logs (
  id, user_id, type, amount, balance_before, balance_after, order_id, instance_id,
  remark, created_at
)
SELECT 980503, p.id, 'gift', 5.00, 74.50, 79.50, NULL, NULL,
       '抽奖中奖 conformance', timestamp '2026-01-04 03:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Tickets: 980601 closed, 980602 open with the newest message, 980603 resolved.
-- The statuses are reset on every run because the status/reply cases expect the
-- same state each time.
-- ---------------------------------------------------------------------------
INSERT INTO tickets (
  id, user_id, host_id, instance_id, subject, category, priority, status,
  created_at, updated_at, resolved_at, closed_at
)
SELECT 980601, p.id, 980001, 980101, 'conformance-public-api-closed-ticket', 'billing',
       'high', 'closed', timestamp '2026-01-07 00:00:00.000', timestamp '2026-01-07 03:00:00.000',
       timestamp '2026-01-07 03:00:00.000', timestamp '2026-01-07 03:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  status = 'closed', resolved_at = timestamp '2026-01-07 03:00:00.000',
  closed_at = timestamp '2026-01-07 03:00:00.000',
  updated_at = timestamp '2026-01-07 03:00:00.000';

INSERT INTO tickets (
  id, user_id, host_id, instance_id, subject, category, priority, status,
  created_at, updated_at
)
SELECT 980602, p.id, 980001, 980101, 'conformance-public-api-open-ticket', 'technical',
       'normal', 'open', timestamp '2026-01-08 00:00:00.000', timestamp '2026-01-08 02:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  status = 'open', resolved_at = NULL, closed_at = NULL,
  updated_at = timestamp '2026-01-08 02:00:00.000';

INSERT INTO tickets (
  id, user_id, host_id, instance_id, subject, category, priority, status,
  created_at, updated_at, resolved_at
)
SELECT 980603, p.id, 980001, NULL, 'conformance-public-api-resolved-ticket', 'general',
       'low', 'resolved', timestamp '2026-01-09 00:00:00.000', timestamp '2026-01-09 01:00:00.000',
       timestamp '2026-01-09 01:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  status = 'resolved', resolved_at = timestamp '2026-01-09 01:00:00.000', closed_at = NULL;

INSERT INTO ticket_messages (id, ticket_id, sender_id, content, is_from_owner, created_at)
SELECT 980701, 980601, p.id, 'conformance closed ticket message', false,
       timestamp '2026-01-07 00:10:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_messages (id, ticket_id, sender_id, content, is_from_owner, created_at)
SELECT 980702, 980602, p.id, 'conformance first message', false,
       timestamp '2026-01-08 00:10:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_messages (id, ticket_id, sender_id, content, is_from_owner, created_at)
SELECT 980703, 980602, a.id, 'conformance staff reply', true,
       timestamp '2026-01-08 02:00:00.000'
  FROM users a WHERE a.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO ticket_messages (id, ticket_id, sender_id, content, is_from_owner, created_at)
SELECT 980704, 980603, p.id, 'conformance resolved ticket message', false,
       timestamp '2026-01-09 00:10:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- A COMPLETED operation task: readable through GET, not cancellable (409).
-- COMPLETED rows are never claimed by the task worker.
-- ---------------------------------------------------------------------------
INSERT INTO instance_tasks (
  id, instance_id, host_id, user_id, task_type, status, progress, created_at,
  started_at, finished_at
)
SELECT 980801, 980101, 980001, p.id, 'restart', 'COMPLETED', '100',
       now() at time zone 'utc' - interval '2 minutes',
       now() at time zone 'utc' - interval '2 minutes' + interval '1 second',
       now() at time zone 'utc' - interval '1 minute'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  status = 'COMPLETED', progress = '100',
  started_at = now() at time zone 'utc' - interval '2 minutes' + interval '1 second',
  finished_at = now() at time zone 'utc' - interval '1 minute';

-- ---------------------------------------------------------------------------
-- Non-secret probe tokens (see the header note). Plaintexts are obviously fake
-- and are NOT usable outside this throwaway database.
--   scope   : only profile:read                    → 403 on balance/orders/...
--   revoked : all scopes, revoked_at in the past   → 401 TOKEN_REVOKED
--   expired : all scopes, expires_at in the past   → 401 TOKEN_EXPIRED
-- ---------------------------------------------------------------------------
INSERT INTO public_api_tokens (
  id, user_id, name, token_prefix, token_hash, scopes, revoked_at, expires_at,
  created_at, updated_at
)
SELECT 980901, p.id, 'conformance-scope-probe', 'pat_conformance_',
       '6db7d33420960736b5e127f8f3469a6a46f6cd24b7d7904a1c7a18c66133dc97',
       '["profile:read"]'::jsonb, NULL, NULL,
       timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  token_prefix = 'pat_conformance_',
  token_hash = '6db7d33420960736b5e127f8f3469a6a46f6cd24b7d7904a1c7a18c66133dc97',
  scopes = '["profile:read"]'::jsonb, revoked_at = NULL, expires_at = NULL,
  updated_at = timestamp '2026-01-01 00:00:00.000';

INSERT INTO public_api_tokens (
  id, user_id, name, token_prefix, token_hash, scopes, revoked_at, expires_at,
  created_at, updated_at
)
SELECT 980902, p.id, 'conformance-revoked-probe', 'pat_conformance_',
       '9a9a7a49490a565dbd0da6da60e7df5687ce8c6149307f3ce7769d5c1bb0f4d2',
       '["profile:read","profile:write","balance:read","balance:write","billing:read","products:read","services:read","services:operate","services:billing","orders:read","tickets:read","tickets:write","notifications:read","notifications:send"]'::jsonb,
       timestamp '2026-01-01 00:00:00.000', NULL,
       timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  token_prefix = 'pat_conformance_',
  token_hash = '9a9a7a49490a565dbd0da6da60e7df5687ce8c6149307f3ce7769d5c1bb0f4d2',
  scopes = '["profile:read","profile:write","balance:read","balance:write","billing:read","products:read","services:read","services:operate","services:billing","orders:read","tickets:read","tickets:write","notifications:read","notifications:send"]'::jsonb,
  revoked_at = timestamp '2026-01-01 00:00:00.000', expires_at = NULL,
  updated_at = timestamp '2026-01-01 00:00:00.000';

INSERT INTO public_api_tokens (
  id, user_id, name, token_prefix, token_hash, scopes, revoked_at, expires_at,
  created_at, updated_at
)
SELECT 980903, p.id, 'conformance-expired-probe', 'pat_conformance_',
       '70b7114d7c6191dd70a99530e50d5afb1cc93e82d19ea905777020a4ab53ff1d',
       '["profile:read","profile:write","balance:read","balance:write","billing:read","products:read","services:read","services:operate","services:billing","orders:read","tickets:read","tickets:write","notifications:read","notifications:send"]'::jsonb,
       NULL, timestamp '2026-01-01 00:00:00.000',
       timestamp '2026-01-01 00:00:00.000', timestamp '2026-01-01 00:00:00.000'
  FROM users p WHERE p.username = 'parityuser'
ON CONFLICT (id) DO UPDATE SET
  token_prefix = 'pat_conformance_',
  token_hash = '70b7114d7c6191dd70a99530e50d5afb1cc93e82d19ea905777020a4ab53ff1d',
  scopes = '["profile:read","profile:write","balance:read","balance:write","billing:read","products:read","services:read","services:operate","services:billing","orders:read","tickets:read","tickets:write","notifications:read","notifications:send"]'::jsonb,
  revoked_at = NULL, expires_at = timestamp '2026-01-01 00:00:00.000',
  updated_at = timestamp '2026-01-01 00:00:00.000';

COMMIT;
