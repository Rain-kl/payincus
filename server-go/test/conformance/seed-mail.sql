-- Mail-module fixture rows for test/conformance/requests-mail.json.
--
-- The conformance harness replays the same request list against the Node and the
-- Go backend on the *same* database and diffs the responses, so the mail cases
-- need rows that already exist before either server starts.
--
-- Apply once, AFTER the parity database exists and its identities are seeded
-- (reset-parity-db.sh wipes everything, so this file has to run again after it):
--
--   PARITY_DATABASE_URL=postgresql://.../payincus_goparity \
--     ./test/conformance/reset-parity-db.sh
--   psql "$PARITY_DATABASE_URL" -f ./test/conformance/seed-mail.sql
--   PARITY_DATABASE_URL=postgresql://.../payincus_goparity \
--     ./test/conformance/parity.sh requests-mail.json
--
-- Every row uses an id >= 900001 so it cannot collide with the auto-increment
-- sequence; the sequences are then advanced past the seeded range, so a route
-- that creates a row later still works. The file is idempotent (ON CONFLICT DO
-- NOTHING), so re-running it never duplicates fixtures.
--
-- Users are resolved by username (admin / parityuser), which is what the seed
-- program creates — not by a hard-coded id.
--
-- The api URLs use the reserved ".invalid" TLD: DNS never resolves it, which makes
-- every upstream call fail deterministically with
-- "Unable to resolve hostname (ENOTFOUND)" on both backends. That is what lets the
-- upstream-failure branches be compared byte-for-byte.

BEGIN;

INSERT INTO mail_sources (id, name, code, api_url, api_key, smartermail_url, enabled, sort_order, created_at, updated_at) VALUES
  (900001, 'Parity Mail US',       'pmus', 'https://cranemail.parity.invalid',      'plain-parity-key-us',  'https://smartermail.parity.invalid',      true,  0, now() - interval '40 days', now() - interval '40 days'),
  (900002, 'Parity Mail Disabled', 'pmoff','https://cranemail-off.parity.invalid',  'plain-parity-key-off', 'https://smartermail-off.parity.invalid',  false, 1, now() - interval '40 days', now() - interval '40 days'),
  (900003, 'Parity Mail Extra',    'pmex', 'https://cranemail-ex.parity.invalid',   'plain-parity-key-ex',  'https://smartermail-ex.parity.invalid',   true,  2, now() - interval '40 days', now() - interval '40 days')
ON CONFLICT (id) DO NOTHING;

INSERT INTO mail_plans (id, source_id, name, description, domain_limit, disk_limit_gb, billing_cycle, price, enabled, sort_order, created_at, updated_at) VALUES
  (900001, 900001, 'Parity Monthly',        NULL,                     3, 10, 'monthly', 12.34,  true,  0, now() - interval '40 days', now() - interval '40 days'),
  (900002, 900001, 'Parity Yearly',         'Yearly parity plan',     5, 50, 'yearly',  120.00, true,  1, now() - interval '40 days', now() - interval '40 days'),
  (900003, 900002, 'Disabled Source Plan',  NULL,                     2, 5,  'monthly', 5.00,   true,  0, now() - interval '40 days', now() - interval '40 days'),
  (900004, 900003, 'Parity Extra Monthly',  NULL,                     2, 20, 'monthly', 8.88,   true,  0, now() - interval '40 days', now() - interval '40 days'),
  (900005, 900001, 'Parity Disabled Plan',  NULL,                     2, 5,  'monthly', 7.77,   false, 2, now() - interval '40 days', now() - interval '40 days')
ON CONFLICT (id) DO NOTHING;

-- 900001: parityuser's active subscription (domainLimit 2 = already full -> QUOTA_EXCEEDED)
-- 900002: parityuser's expired subscription on the disabled source
-- 900003: admin's active subscription (domainLimit 3, one slot free)
INSERT INTO mail_subscriptions (id, user_id, source_id, plan_id, status, domain_limit, disk_limit_gb, expires_at, auto_renew, created_at, updated_at)
SELECT 900001, u.id, 900001, 900001, 'active',  2, 10, now() + interval '60 days', false, now() - interval '30 days', now() - interval '1 day'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO mail_subscriptions (id, user_id, source_id, plan_id, status, domain_limit, disk_limit_gb, expires_at, auto_renew, created_at, updated_at)
SELECT 900002, u.id, 900002, 900003, 'expired', 2, 5,  now() - interval '10 days', true,  now() - interval '60 days', now() - interval '10 days'
  FROM users u WHERE u.username = 'parityuser'
ON CONFLICT (id) DO NOTHING;

INSERT INTO mail_subscriptions (id, user_id, source_id, plan_id, status, domain_limit, disk_limit_gb, expires_at, auto_renew, created_at, updated_at)
SELECT 900003, u.id, 900001, 900001, 'active',  3, 10, now() + interval '90 days', false, now() - interval '25 days', now() - interval '5 days'
  FROM users u WHERE u.username = 'admin'
ON CONFLICT (id) DO NOTHING;

INSERT INTO mail_domains (id, subscription_id, source_id, domain, status, admin_username, admin_password, disk_used_mb, verified_at, created_at, updated_at) VALUES
  (900001, 900001, 900001, 'parity-pending.example.com',        'pending',  'postmaster@parity-pending.example.com',        'plainpwd1', 0,  NULL,                           now() - interval '20 days', now() - interval '20 days'),
  (900002, 900001, 900001, 'parity-verified.example.com',       'verified', 'postmaster@parity-verified.example.com',       'plainpwd2', 12, now() - interval '19 days',     now() - interval '19 days', now() - interval '19 days'),
  (900003, 900003, 900001, 'parity-admin.example.com',          'pending',  'postmaster@parity-admin.example.com',          'plainpwd3', 0,  NULL,                           now() - interval '5 days',  now() - interval '5 days'),
  (900004, 900003, 900001, 'parity-admin-verified.example.com', 'verified', 'postmaster@parity-admin-verified.example.com', 'plainpwd4', 3,  now() - interval '4 days',      now() - interval '4 days',  now() - interval '4 days')
ON CONFLICT (id) DO NOTHING;

INSERT INTO mail_accounts (id, domain_id, email, username, display_name, disk_limit_mb, disk_used_mb, is_admin, created_at, updated_at) VALUES
  (900001, 900002, 'admin@parity-verified.example.com',            'admin',      'Administrator', 2048, 5, true,  now() - interval '19 days', now() - interval '19 days'),
  (900002, 900002, 'alice@parity-verified.example.com',            'alice',      NULL,            512,  0, false, now() - interval '18 days', now() - interval '18 days'),
  (900003, 900004, 'postmaster@parity-admin-verified.example.com', 'postmaster', 'Administrator', 1024, 3, true,  now() - interval '4 days',  now() - interval '4 days')
ON CONFLICT (id) DO NOTHING;

SELECT setval(pg_get_serial_sequence('mail_sources', 'id'), 900010, true);
SELECT setval(pg_get_serial_sequence('mail_plans', 'id'), 900010, true);
SELECT setval(pg_get_serial_sequence('mail_subscriptions', 'id'), 900010, true);
SELECT setval(pg_get_serial_sequence('mail_domains', 'id'), 900010, true);
SELECT setval(pg_get_serial_sequence('mail_accounts', 'id'), 900010, true);

COMMIT;
