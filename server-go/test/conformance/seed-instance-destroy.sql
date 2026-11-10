-- Instance-destroy fixture rows for test/conformance/requests-instance-destroy.json.
--
-- The harness replays the same request list against the Node and the Go backend on
-- the *same* database and diffs status + JSON body, so the destroy quotes need real
-- instances, a real package plan and real billing periods (the refund quote is
-- computed from instance_billing_records, not from billing_price alone).
--
-- Id range: 950001+ (host 950001, package 950001, plan 950001, instances 950001+
-- and billing records 950001+), so nothing collides with the 900001 (mail),
-- 920001 (instance-billing), 930001 (packages) or 940001 ranges.
--
-- The file is idempotent (ON CONFLICT DO NOTHING) and never rewrites an existing row.
--
-- What is deliberately NOT here: any case that reaches POST /api/instances/:id/destroy
-- past its guards. Those delete Incus instances and move money, so they are covered
-- by unit tests (TestInstanceDestroy*) instead of the dual run. The POST cases in the
-- request list only exercise the pre-claim branches (invalid id / 404 / 403 /
-- already-deleted), which return before any Incus call or refund.
--
-- Users are resolved by username (admin / parityuser), which is what the seed program
-- creates — never by a hard-coded id.

BEGIN;

-- 950001: admin-owned host (also the "host owner" for every instance below).
INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
                   cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
                   nat_ports_used_count, created_at, updated_at)
SELECT 950001, u.id, 'Parity Destroy Host', 'https://host.destroy.parity.invalid:8443',
       'online', 0, 0, 0, 10000, 1048576, 1000, 'zfs', 'loop',
       0, now() - interval '40 days', now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 950001: admin-owned package that the paid instances belong to.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, created_at)
SELECT 950001, u.id, 'Parity Destroy Package', 'instance-destroy fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Plan prices are CENTS (¥30.00 -> 3000.00), matching the rest of the schema.
INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
                           backup_limit, site_limit, traffic_limit, traffic_limit_speed, price,
                           billing_cycle, is_active, is_sold_out, sort_order, swap_size,
                           created_at, updated_at) VALUES
  (950001, 950001, 'Parity Destroy 1C1G', 1, 1024, 10240, 20, 5, 3, 10,
   1099511627776, '1Mbit', 3000.00, 1, true, false, 0, 0,
   now() - interval '40 days', now() - interval '40 days')
ON CONFLICT (id) DO NOTHING;

-- 950001: paid running instance with a current billing period.
--   refund quote: 30.00 over 30 days, 20 days left -> remainingValue 20.00,
--   maxRefundable 30.00 -> refundable 20.00; destroyCount 1 -> fee 2.00, refund 18.00.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950001, 'parity-destroy-950001', 'parity-destroy-paid', u.id, 950001, 950001, 950001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950002: free instance (no package plan): refund is all zeros, feeRate still follows
-- the user's destroy count, traffic limit is not enforced.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       auto_renew, version, monthly_traffic_used, created_at, updated_at)
SELECT 950002, 'parity-destroy-950002', 'parity-destroy-free', u.id, 950001, 950001, NULL,
       'images:debian/12', 'running', 1, 512, 5120, 'nat',
       false, 0, 0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950003: already deleted (both destroy-info and destroy answer 400).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950003, 'parity-destroy-950003', 'parity-destroy-deleted', u.id, 950001, 950001, 950001,
       'images:debian/12', 'deleted', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950004: still creating.
-- created_at is deliberately in the FUTURE: the Node reference server runs a
-- "stuck creating" sweeper every 2 minutes that flips any status='creating' row
-- older than 30 minutes to 'error' (app.ts createTimeoutCleanup). A fresh/future
-- timestamp keeps this fixture in the creating state for the whole run.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950004, 'parity-destroy-950004', 'parity-destroy-creating', u.id, 950001, 950001, 950001,
       'images:debian/12', 'creating', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() + interval '1 hour', now() - interval '1 hour'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- Repair the one drift the harness itself causes: the Node reference server's
-- 2-minute "stuck creating" sweeper flips status='creating' rows older than 30
-- minutes to 'error'. Re-arm only that exact row/state so the case stays
-- reproducible across runs; every other state is left untouched.
UPDATE instances
   SET status = 'creating',
       created_at = now() + interval '1 hour',
       updated_at = now()
 WHERE id = 950004
   AND incus_id = 'parity-destroy-950004'
   AND status = 'error';

-- 950005: manually suspended -> destroy is refused.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, suspend_reason, suspended_at,
                       auto_renew, version, monthly_traffic_used, created_at, updated_at)
SELECT 950005, 'parity-destroy-950005', 'parity-destroy-suspended', u.id, 950001, 950001, 950001,
       'images:debian/12', 'suspended', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', 'manual', now() - interval '2 days',
       false, 0, 0, now() - interval '10 days', now() - interval '2 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950006: auto-suspended because it expired -> the destroy exemption applies, and the
-- (expired) refund quote is all zeros.
-- suspended_at stays inside the 3-day auto-delete grace window (billing-scheduler
-- SUSPEND_AFTER_DELETE_DAYS) so the expiry-delete job never removes the fixture.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, suspend_reason, suspended_at,
                       auto_renew, version, monthly_traffic_used, created_at, updated_at)
SELECT 950006, 'parity-destroy-950006', 'parity-destroy-expired', u.id, 950001, 950001, 950001,
       'images:debian/12', 'suspended', 1, 1024, 10240, 'nat',
       30.00, 1, now() - interval '1 hour', 'expired', now() - interval '1 hour',
       false, 0, 0, now() - interval '40 days', now() - interval '1 hour'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950007: exactly 5 GiB used this cycle -> destroy is refused with the traffic reason.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950007, 'parity-destroy-950007', 'parity-destroy-traffic', u.id, 950001, 950001, 950001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       5368709120, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950008: error state -> feeWaiverEligible, and ?feeWaiver=error waives the fee.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950008, 'parity-destroy-950008', 'parity-destroy-error', u.id, 950001, 950001, 950001,
       'images:debian/12', 'error', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 950009: admin-owned instance -> a parityuser destroy-info request is 403.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950009, 'parity-destroy-950009', 'parity-destroy-admin', u.id, 950001, 950001, 950001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 950010: paid running instance with NO billing records -> getMaxRefundable returns 0,
-- so the refund is capped at 0 while remainingValue still comes from the legacy
-- billing_price fallback. Pins the maxRefundable clamp.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       monthly_traffic_used, created_at, updated_at)
SELECT 950010, 'parity-destroy-950010', 'parity-destroy-no-records', u.id, 950001, 950001, 950001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       0, now() - interval '10 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- Positive billing periods (newPurchase). The refund quote sums the unused ratio of
-- every period that has not ended yet: 30.00 over 30 days with 20 days left = 20.00.
INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 950001, 950001, u.id, 'newPurchase', 30.00, 1,
       now() - interval '10 days', now() + interval '20 days',
       'parity-destroy new purchase', now() - interval '10 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 950002, 950008, u.id, 'newPurchase', 30.00, 1,
       now() - interval '10 days', now() + interval '20 days',
       'parity-destroy new purchase', now() - interval '10 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 950003, 950007, u.id, 'newPurchase', 30.00, 1,
       now() - interval '10 days', now() + interval '20 days',
       'parity-destroy new purchase', now() - interval '10 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 950004, 950009, u.id, 'newPurchase', 30.00, 1,
       now() - interval '10 days', now() + interval '20 days',
       'parity-destroy new purchase', now() - interval '10 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- One previous destroy for parityuser: exercises the 10% fee tier
-- (destroyCount > 0 -> isFirstTime false, feeRate 0.10). It points at the free
-- instance 950002 because user_destroy_records is unique per instance.
INSERT INTO user_destroy_records (id, user_id, host_id, instance_id, instance_name,
                                  destroyed_at, refund_amount, fee_amount, is_first_time)
SELECT 950001, u.id, 950001, 950002, 'parity-destroy-free',
       now() - interval '20 days', 0.00, 0.00, true
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

SELECT setval(pg_get_serial_sequence('hosts', 'id'), 950010, true);
SELECT setval(pg_get_serial_sequence('packages', 'id'), 950010, true);
SELECT setval(pg_get_serial_sequence('package_plans', 'id'), 950010, true);
SELECT setval(pg_get_serial_sequence('instances', 'id'), 950010, true);
SELECT setval(pg_get_serial_sequence('instance_billing_records', 'id'), 950010, true);
SELECT setval(pg_get_serial_sequence('user_destroy_records', 'id'), 950010, true);

COMMIT;
