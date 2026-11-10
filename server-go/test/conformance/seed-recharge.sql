-- Recharge fixture for test/conformance/requests-recharge.json.
--
-- Ids live in the 970001+ range so they cannot collide with the auto-increment
-- sequences, the hosts fixture (900001) or the other module fixtures (91xxxx-95xxxx).
-- The file is idempotent: ON CONFLICT DO NOTHING keeps a previous parity run's state
-- and never duplicates rows.
--
-- SECURITY: every credential below is a randomly invented, obviously fake value
-- ("parity-fake-*", "FAKE_*"). No real gateway merchant id, MD5 key or RSA key is
-- present. Provider configs are stored as plain JSON objects (the legacy shape the
-- backend supports) so no encryption key is needed to apply the fixture.
--
-- State written by the request list: none of the cases may credit a balance
-- (successful entry is deliberately excluded from parity). The single repay case
-- (PARITY-RECHARGE-MANUAL-1) rewrites the same payment selection values it already
-- has, and the valid-signature amount-mismatch callback is rejected before any
-- balance write.

BEGIN;

-- 970001: yipay V1 (MD5). Used for callback signature cases (no network calls).
INSERT INTO payment_providers (id, name, type, status, config, methods, fee_rate, fee_fixed,
                               min_amount, max_amount, sort_order, created_at, updated_at)
VALUES
  (970001, 'Parity Epay V1', 'yipay', 'active',
   '{"version":"v1","apiurl":"https://epay.parity.invalid","pid":"PARITY_FAKE_PID","key":"parity-fake-md5-key","methodFees":{"alipay":{"feeRate":0.03,"feeFixed":0.5},"wxpay":0.04}}'::jsonb,
   '["alipay","wxpay"]'::jsonb, 0.03, 0.5, 1, 1000, 0,
   '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
  -- 970002: yipay V2 (RSA). queryOrder is a local "unsupported" answer, which is
  -- what the verify endpoint exercises. The dummy keys are only checked for
  -- non-emptiness by buildEpayConfig.
  (970002, 'Parity Epay V2', 'yipay', 'active',
   '{"version":"v2","apiurl":"https://epay.parity.invalid","pid":"PARITY_FAKE_PID","platform_public_key":"FAKE_PLATFORM_PUBLIC_KEY_PARITY","merchant_private_key":"FAKE_MERCHANT_PRIVATE_KEY_PARITY"}'::jsonb,
   '["alipay"]'::jsonb, 0, 0, 1, NULL, 1,
   '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
  -- 970003: manual provider (no gateway, no signature key).
  (970003, 'Parity Manual', 'manual', 'active',
   '{"instructions":"请转账至测试账户后联系客服"}'::jsonb,
   '[]'::jsonb, 0, 0, 1, NULL, 2,
   '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
  -- 970004: heleket. The default callback IP whitelist (31.133.220.8) rejects the
  -- conformance client, which is the deterministic 403 case.
  (970004, 'Parity Heleket', 'heleket', 'active',
   '{"apiurl":"https://api.heleket.invalid","merchant_uuid":"parity-fake-uuid","api_key":"parity-fake-api-key","currency":"CNY","lifetime":3600}'::jsonb,
   '[]'::jsonb, 0, 0, 1, NULL, 3,
   '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
  -- 970005: disabled provider: must not appear in the user list but must appear in
  -- the admin list.
  (970005, 'Parity Disabled', 'manual', 'disabled',
   '{"instructions":"disabled provider"}'::jsonb,
   '[]'::jsonb, 0, 0, 1, NULL, 4,
   '2026-01-01 00:00:00', '2026-01-01 00:00:00')
ON CONFLICT (id) DO NOTHING;

-- Orders. created_at values are distinct and far in the past, so page ordering is
-- deterministic (ORDER BY created_at DESC) no matter what other rows exist.
INSERT INTO recharge_records (id, user_id, provider_id, order_no, trade_no, amount, actual_amount, fee,
                              payment_method, status, payment_details, fail_reason,
                              created_at, updated_at, expired_at, completed_at)
SELECT v.id, u.id, v.provider_id, v.order_no, v.trade_no, v.amount::numeric, v.actual_amount::numeric,
       v.fee::numeric, v.payment_method, v.status::"RechargeStatus", v.payment_details::jsonb, v.fail_reason,
       v.created_at::timestamp, v.updated_at::timestamp, v.expired_at::timestamp, v.completed_at::timestamp
  FROM (VALUES
    (970101, 'parityuser', 970002, 'PARITY-RECHARGE-PENDING-1', NULL, '10.00', '10.00', '0', 'alipay', 'pending',
     '{"kind":"yipay","recharge":{"amount":10,"payableAmount":10,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     NULL, '2026-01-02 00:00:00', '2026-01-02 00:00:00', '2027-01-01 00:00:00', NULL),
    (970102, 'parityuser', 970002, 'PARITY-RECHARGE-COMPLETED-1', 'T-PARITY-COMPLETED-1', '20.00', '19.40', '0.60', 'alipay', 'completed',
     '{"kind":"yipay","recharge":{"amount":20,"payableAmount":19.40,"fee":0.60,"feeRate":0.03,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     NULL, '2026-01-03 00:00:00', '2026-01-03 00:00:00', '2026-01-04 00:00:00', '2026-11-01 00:00:00'),
    (970103, 'admin', 970002, 'PARITY-RECHARGE-ADMIN-1', NULL, '15.00', '15.00', '0', 'alipay', 'pending',
     '{"kind":"yipay","recharge":{"amount":15,"payableAmount":15,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     NULL, '2026-01-04 00:00:00', '2026-01-04 00:00:00', '2027-01-01 00:00:00', NULL),
    (970104, 'parityuser', 970002, 'PARITY-RECHARGE-CANCELLED-1', NULL, '8.00', '8.00', '0', 'alipay', 'cancelled',
     '{"kind":"yipay","recharge":{"amount":8,"payableAmount":8,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     NULL, '2026-01-05 00:00:00', '2026-01-05 00:00:00', '2026-01-06 00:00:00', NULL),
    (970105, 'parityuser', 970002, 'PARITY-RECHARGE-FAILED-1', NULL, '9.00', '9.00', '0', 'alipay', 'failed',
     '{"kind":"yipay","recharge":{"amount":9,"payableAmount":9,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     '测试失败', '2026-01-06 00:00:00', '2026-01-06 00:00:00', '2026-01-07 00:00:00', NULL),
    (970106, 'parityuser', 970002, 'PARITY-RECHARGE-COMPLETED-2', 'T-PARITY-COMPLETED-2', '5.00', '5.00', '0', 'alipay', 'completed',
     '{"kind":"yipay","recharge":{"amount":5,"payableAmount":5,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"surcharge","paymentMethod":"alipay"}}',
     NULL, '2026-01-07 00:00:00', '2026-01-07 00:00:00', '2026-01-08 00:00:00', '2026-11-02 00:00:00'),
    (970107, 'parityuser', 970003, 'PARITY-RECHARGE-MANUAL-1', NULL, '10.00', '10.00', '0', NULL, 'pending',
     '{"kind":"manual","manual":{"instructions":"请转账至测试账户后联系客服"},"recharge":{"amount":10,"payableAmount":10,"fee":0,"feeRate":0,"feeFixed":0,"feeMode":"deduct","paymentMethod":null}}',
     NULL, '2026-01-08 00:00:00', '2026-01-08 00:00:00', '2027-01-01 00:00:00', NULL)
  ) AS v(id, username, provider_id, order_no, trade_no, amount, actual_amount, fee, payment_method, status,
         payment_details, fail_reason, created_at, updated_at, expired_at, completed_at)
  JOIN users u ON u.username = v.username
ON CONFLICT (id) DO NOTHING;

COMMIT;
