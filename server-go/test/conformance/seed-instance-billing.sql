-- Instance-billing fixture rows for test/conformance/requests-instance-billing.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs status + JSON body, so the billing
-- reads need real instances (the parity database starts with none).
--
-- Every row uses an id >= 920001 (host 920001) so it cannot collide with the
-- auto-increment sequences or the hosts fixture's 900001 range. The file is
-- idempotent (ON CONFLICT DO NOTHING), so re-running never duplicates fixtures.
--
-- State written by the single write case in the request list:
--   PATCH /api/instances/920004/auto-renew {"autoRenew":true}
-- flips instances.auto_renew on 920004 from false to true. Restore it with
--   UPDATE instances SET auto_renew = false WHERE id = 920004;
-- (or reset the parity database). No balance, instance billing record or plan row
-- is modified by any case.
--
-- Users are resolved by username (admin / parityuser), which is what the seed
-- program creates — never by a hard-coded id.

BEGIN;

-- 920001: admin-owned host. "admin" is therefore the host owner of every
-- instance below, which is what exercises the host-owner read branch.
INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
                   cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
                   created_at, updated_at)
SELECT 920001, u.id, 'Parity Billing Host', 'https://host.billing.parity.invalid:8443',
       'online', 0, 0, 0, 10000, 1048576, 1000, 'zfs', 'loop',
       now() - interval '40 days', now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 920001: the package the paid instances belong to (admin-owned).
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, created_at)
SELECT 920001, u.id, 'Parity Billing Package', 'instance-billing fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- A second package so "new plan must belong to the same package" can be tested.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, created_at)
SELECT 920002, u.id, 'Parity Billing Other Package', 'instance-billing fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776,
       now() - interval '40 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Plan prices are CENTS (¥30.00 -> 3000.00).
INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
                           backup_limit, site_limit, traffic_limit, traffic_limit_speed, price,
                           billing_cycle, is_active, is_sold_out, sort_order, swap_size,
                           created_at, updated_at) VALUES
  (920001, 920001, 'Parity 1C1G', 1, 1024, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 3000.00, 1, true,  false, 0, 0, now() - interval '40 days', now() - interval '40 days'),
  (920002, 920001, 'Parity 2C2G', 2, 2048, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 6000.00, 1, true,  false, 1, 0, now() - interval '40 days', now() - interval '40 days'),
  (920003, 920001, 'Parity Inactive', 4, 4096, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 12000.00, 1, false, false, 2, 0, now() - interval '40 days', now() - interval '40 days'),
  (920004, 920001, 'Parity Sold Out', 4, 4096, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 12000.00, 1, true,  true,  3, 0, now() - interval '40 days', now() - interval '40 days'),
  (920005, 920002, 'Parity Other Package Plan', 2, 2048, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 9000.00, 1, true, false, 0, 0, now() - interval '40 days', now() - interval '40 days'),
  (920006, 920001, 'Parity Downgrade', 1, 512, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit', 1000.00, 1, true, false, 4, 0, now() - interval '40 days', now() - interval '40 days')
ON CONFLICT (id) DO NOTHING;

-- 920001: parityuser's paid instance (owner read + admin host-owner read).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920001, 'parity-billing-920001', 'parity-billing-paid', u.id, 920001, 920001, 920001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920002: parityuser's free instance (no package plan, no expiry, no price).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       auto_renew, version, created_at, updated_at)
SELECT 920002, 'parity-billing-920002', 'parity-billing-free', u.id, 920001, 920001, NULL,
       'images:debian/12', 'running', 1, 512, 5120, 'nat',
       false, 0, now() - interval '40 days', now() - interval '2 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920003: admin's paid instance on admin's host (a parityuser request is a stranger).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920003, 'parity-billing-920003', 'parity-billing-admin', u.id, 920001, 920001, 920001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 920004: dedicated target of the single reversible write case (auto-renew toggle).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920004, 'parity-billing-920004', 'parity-billing-toggle', u.id, 920001, 920001, 920001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920005: paid instance whose remaining time is under the 15-day change-plan floor.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920005, 'parity-billing-920005', 'parity-billing-short', u.id, 920001, 920001, 920001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '5 days', false, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920006: paid instance that is still creating (change-plan status branch).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920006, 'parity-billing-920006', 'parity-billing-creating', u.id, 920001, 920001, 920001,
       'images:debian/12', 'creating', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '1 day', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920007: deleted paid instance (renew -> INSTANCE_DELETED).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 920007, 'parity-billing-920007', 'parity-billing-deleted', u.id, 920001, 920001, 920001,
       'images:debian/12', 'deleted', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 0,
       now() - interval '40 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 920001's billing history: one expired purchase, one current period, one refund.
-- Amounts are positive for charges and refunds are stored positive too (the Node
-- refund path writes a positive amount of type 'refund').
INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 920001, 920001, u.id, 'newPurchase', 30.00, 1,
       now() - interval '71 days', now() - interval '40 days',
       'parity new purchase', now() - interval '71 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 920002, 920001, u.id, 'renew', 30.00, 1,
       now() - interval '1 day', now() + interval '30 days',
       'parity renew', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 920003, 920001, u.id, 'refund', 5.00, 0,
       now() - interval '2 days', now() - interval '2 days',
       'parity refund', now() - interval '2 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

SELECT setval(pg_get_serial_sequence('hosts', 'id'), 920010, true);
SELECT setval(pg_get_serial_sequence('packages', 'id'), 920010, true);
SELECT setval(pg_get_serial_sequence('package_plans', 'id'), 920010, true);
SELECT setval(pg_get_serial_sequence('instances', 'id'), 920010, true);
SELECT setval(pg_get_serial_sequence('instance_billing_records', 'id'), 920010, true);

COMMIT;
