-- Fixture rows for test/conformance/requests-admin-billing.json.
--
-- The harness replays the same request list against the Node reference and the Go
-- port on the SAME database and diffs status + body, so every read this group
-- exercises needs real rows: an admin-owned host/package/plans, paid and free
-- instances, billing/balance/recharge history, an AFF code + binding, and a
-- pre-built financial reconciliation run for a fixed business day.
--
-- Ids start at 991001 (the docs' 9A0001+ segment) so they cannot collide with the
-- auto-increment sequences or the other fixtures (900001 hosts, 910001/920001
-- instances, 960001 backups, 970001 recharge, 950001 instance-destroy).
-- ON CONFLICT DO NOTHING keeps the file idempotent: re-running a parity pass never
-- duplicates a row and never rewrites one.
--
-- SECURITY: every credential/identifier below is an obviously fake invention
-- ("PARITY_FAKE_*", "parity-...invalid"). No real gateway credential is present.
--
-- State written by the request list: none. Every write case in the group is either
-- rejected before the write (validation / 404 / 403) or a no-op ("price 未变更").
-- POST /api/admin/billing/reconciliation/run is the single exception: it upserts a
-- run for 2021-03-05 (compare only the status code with skipBody), which is
-- idempotent because the run is unique per date.
--
-- Business day used by the reconciliation reads/exports: 2021-03-04 in Asia/Shanghai
-- = [2021-03-03T16:00Z, 2021-03-04T16:00Z).

BEGIN;

-- 991001: admin-owned, online host that can host containers.
INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
                   cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
                   instance_type, created_at, updated_at)
SELECT 991001, u.id, 'Parity AdminBilling Host', 'https://host.adminbilling.parity.invalid:8443',
       'online', 0, 0, 0, 10000, 1048576, 1000, 'zfs', 'loop',
       'container', '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 991001: the package every fixture instance belongs to (admin-owned, active).
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, created_at)
SELECT 991001, u.id, 'Parity AdminBilling Package', 'admin-billing fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776, true,
       '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 991002: a second package so "new plan must belong to the same package" is real.
INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
                      network_mode, instance_type, port_limit, snapshot_limit, backup_limit,
                      site_limit, monthly_traffic_limit, active, created_at)
SELECT 991002, u.id, 'Parity AdminBilling Other Package', 'admin-billing fixture',
       800, 4096, 60000, 'nat', 'container', 20, 5, 3, 10, 1099511627776, true,
       '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Plan prices are CENTS (¥30.00 -> 3000.00).
INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
                           backup_limit, site_limit, traffic_limit, traffic_limit_speed, price,
                           billing_cycle, is_active, is_sold_out, sort_order, swap_size,
                           created_at, updated_at)
VALUES
  (991001, 991001, 'Parity AB 1C1G', 1, 1024, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit',
   3000.00, 1, true, false, 0, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00'),
  (991002, 991001, 'Parity AB 2C2G', 2, 2048, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit',
   6000.00, 1, true, false, 1, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00'),
  (991003, 991001, 'Parity AB Inactive', 4, 4096, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit',
   12000.00, 1, false, false, 2, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00'),
  (991004, 991001, 'Parity AB Sold Out', 4, 4096, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit',
   12000.00, 1, true, true, 3, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00'),
  (991005, 991002, 'Parity AB Other Package Plan', 2, 2048, 10240, 20, 5, 3, 10,
   1099511627776, '1Mbit', 9000.00, 1, true, false, 0, 0,
   '2021-01-01 00:00:00', '2021-01-01 00:00:00'),
  (991006, 991001, 'Parity AB Cheaper', 1, 512, 10240, 20, 5, 3, 10, 1099511627776, '1Mbit',
   1000.00, 1, true, false, 4, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00')
ON CONFLICT (id) DO NOTHING;

-- 991001: parityuser's paid instance (the refund / price-change / extend target).
-- version = 3 so a stale expectedVersion is a deterministic 409.
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 991001, 'parity-adminbilling-991001', 'parity-adminbilling-paid', u.id, 991001, 991001, 991001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '30 days', false, 3,
       '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991002: parityuser's FREE instance (no plan, no price, no expiry).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       auto_renew, version, created_at, updated_at)
SELECT 991002, 'parity-adminbilling-991002', 'parity-adminbilling-free', u.id, 991001, 991001, NULL,
       'images:debian/12', 'running', 1, 512, 5120, 'nat',
       false, 0, '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991003: paid instance that already carries an AFF binding (duplicate-apply case).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 991003, 'parity-adminbilling-991003', 'parity-adminbilling-aff', u.id, 991001, 991001, 991001,
       'images:debian/12', 'stopped', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '20 days', false, 0,
       '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991004: deleted paid instance (status filters + "实例已删除" branches).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 991004, 'parity-adminbilling-991004', 'parity-adminbilling-deleted', u.id, 991001, 991001, 991001,
       'images:debian/12', 'deleted', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '10 days', false, 0,
       '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991005: paid instance owned by the admin account (a different user row in the list).
INSERT INTO instances (id, incus_id, name, user_id, host_id, package_id, package_plan_id,
                       image, status, cpu, memory, disk, network_mode,
                       billing_price, billing_cycle, expires_at, auto_renew, version,
                       created_at, updated_at)
SELECT 991005, 'parity-adminbilling-991005', 'parity-adminbilling-admin', u.id, 991001, 991001, 991001,
       'images:debian/12', 'running', 1, 1024, 10240, 'nat',
       30.00, 1, now() + interval '3 days', false, 0,
       '2021-01-01 00:00:00', '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- Purchase history: a paid period that still has unused value, so the refund quote
-- and the price-change credit are both non-trivial.
INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 991001, 991001, u.id, 'newPurchase', 30.00, 1,
       '2026-01-01 00:00:00', '2026-12-31 00:00:00', 'parity admin-billing purchase',
       '2026-01-01 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO instance_billing_records (id, instance_id, user_id, type, amount, months,
                                      period_start, period_end, remark, created_at)
SELECT 991002, 991001, u.id, 'renew', 30.00, 1,
       '2026-01-01 00:00:00', '2026-12-31 00:00:00', 'parity admin-billing renew',
       '2026-01-02 00:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991001: AFF code owned by the admin (discount 10%), used by the apply-aff cases.
INSERT INTO aff_codes (id, code, user_id, discount_rate, commission_rate, enabled,
                       used_count, total_earnings, created_at)
SELECT 991001, 'PARITYABCODE', u.id, 0.10, 0.05, true, 0, 0, '2021-01-01 00:00:00'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 991001: the binding on instance 991003 (so a second apply is rejected).
INSERT INTO aff_bindings (id, instance_id, aff_code_id, created_at)
VALUES (991001, 991003, 991001, '2021-01-01 00:00:00')
ON CONFLICT (id) DO NOTHING;

-- 991001: manual payment provider used by the recharge list / export fixtures.
INSERT INTO payment_providers (id, name, type, status, config, methods, fee_rate, fee_fixed,
                               min_amount, max_amount, sort_order, created_at, updated_at)
VALUES (991001, 'Parity AdminBilling Manual', 'manual', 'active',
        '{"instructions":"请联系客服完成转账"}'::jsonb, '[]'::jsonb, 0, 0, 1, NULL, 90,
        '2021-01-01 00:00:00', '2021-01-01 00:00:00')
ON CONFLICT (id) DO NOTHING;

-- 991001: a completed recharge inside the 2021-03-04 business day (orders CSV).
INSERT INTO recharge_records (id, user_id, provider_id, order_no, trade_no, amount, actual_amount,
                              fee, payment_method, status, created_at, updated_at, completed_at)
SELECT 991001, u.id, 991001, 'PARITY-ADMINBILLING-1', 'PARITYTRADE123456789', 30.00, 30.00,
       0.50, 'alipay', 'completed', '2021-03-03 20:00:00', '2021-03-04 02:00:00',
       '2021-03-04 02:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991001: a balance log inside the same day (balance_logs CSV + records dialog).
INSERT INTO balance_logs (id, user_id, type, amount, balance_before, balance_after,
                          order_id, instance_id, remark, created_at)
SELECT 991001, u.id, 'consume', -30.00, 130.00, 100.00, 'PARITY-ORDER-123456789', 991001,
       'parity admin-billing balance log', '2021-03-04 03:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

-- 991001: an approved adjustment inside the same day (adjustments CSV).
INSERT INTO balance_adjustment_requests (id, user_id, requested_by_user_id, reviewed_by_user_id,
                                         amount, request_type, status, source_type, source_id,
                                         order_no, reason, review_remark, balance_log_id,
                                         created_at, updated_at, reviewed_at)
SELECT 991001, u.id, a.id, a.id, 5.00, 'manual_adjust', 'approved', 'order', 991001,
       'PARITY-ADJ-ORDER-NO-123456789', 'parity admin-billing adjustment', 'ok', NULL,
       '2021-03-04 04:00:00', '2021-03-04 04:00:00', '2021-03-04 04:00:00'
  FROM users u, users a WHERE u.username = 'parityuser' AND a.username = 'admin'
ON CONFLICT (id) DO NOTHING;

-- 991001: a pre-built reconciliation run for 2021-03-04 so the read endpoints and
-- the export can be compared without running anything. Timestamps are pinned, so
-- the JSON is byte-stable across the two backends.
INSERT INTO financial_reconciliation_runs (id, date, status, summary, created_by_user_id,
                                           updated_by_user_id, created_at, updated_at)
SELECT 991001, '2021-03-04'::date, 'discrepancy',
       '{"timezone":"Asia/Shanghai","recharge":{"count":1,"amount":30,"fee":0.5},"balanceLogs":{"count":1,"amount":-30},"instanceBilling":{"count":0,"amount":0},"approvedAdjustments":{"count":1,"amount":5},"discrepancies":{"total":2,"byType":{"recharge_missing_balance_log":1,"orphan_balance_log":1}}}'::jsonb,
       a.id, a.id, '2021-03-04 09:00:00', '2021-03-04 10:00:00'
  FROM users a WHERE a.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO financial_reconciliation_items (id, run_id, item_key, item_type, status, source_type,
                                           source_id, user_id, amount, title, detail, note,
                                           handled_by_user_id, handled_at, created_at, updated_at)
SELECT 991001, 991001, 'recharge:991001:missing-balance-log', 'recharge_missing_balance_log',
       'discrepancy', 'recharge', 991001, u.id, 30.00,
       '成功充值未找到入账流水：PARITY-ADMINBILLING-1',
       '{"orderNo":"PARITY-ADMINBILLING-1","completedAt":"2021-03-04T02:00:00.000Z","callbackAt":null,"createdAt":"2021-03-03T20:00:00.000Z"}'::jsonb,
       NULL, NULL, NULL, '2021-03-04 09:00:00', '2021-03-04 09:00:00'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO financial_reconciliation_items (id, run_id, item_key, item_type, status, source_type,
                                           source_id, user_id, amount, title, detail, note,
                                           handled_by_user_id, handled_at, created_at, updated_at)
SELECT 991002, 991001, 'balance-log:991001:missing-source', 'orphan_balance_log',
       'discrepancy', 'balance_log', 991001, u.id, 12.34,
       '余额流水缺少业务来源：#991001',
       '{"type":"consume","remark":"parity orphan","createdAt":"2021-03-04T05:00:00.000Z"}'::jsonb,
       NULL, NULL, NULL, '2021-03-04 09:00:01', '2021-03-04 09:00:01'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

COMMIT;
