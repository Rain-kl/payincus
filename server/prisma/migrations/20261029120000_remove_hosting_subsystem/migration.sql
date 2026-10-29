-- 彻底移除托管子系统（托管余额 / 提现 / 拉黑 / 专区 + 相关字段与枚举）

-- 1. 删除托管专用表
DROP TABLE IF EXISTS "hosting_balance_logs" CASCADE;
DROP TABLE IF EXISTS "hosting_withdrawals" CASCADE;
DROP TABLE IF EXISTS "hosting_user_blocks" CASCADE;
DROP TABLE IF EXISTS "hosting_zones" CASCADE;

-- 2. 删除托管相关字段
ALTER TABLE "users" DROP COLUMN IF EXISTS "hosting_balance";
ALTER TABLE "users" DROP COLUMN IF EXISTS "has_created_host_before";
ALTER TABLE "vip_level_rules" DROP COLUMN IF EXISTS "min_hosting_income";
ALTER TABLE "vip_level_rules" DROP COLUMN IF EXISTS "min_hosting_instances";

-- 3. 删除托管 VIP 规则（必须在收窄枚举前）
DELETE FROM "vip_level_rules" WHERE "type"::text = 'hosting';

-- 4. 收窄 VipLevelRuleType 枚举（移除 hosting）
ALTER TYPE "VipLevelRuleType" RENAME TO "VipLevelRuleType_old";
CREATE TYPE "VipLevelRuleType" AS ENUM ('user');
ALTER TABLE "vip_level_rules" ALTER COLUMN "type" TYPE "VipLevelRuleType" USING ("type"::text::"VipLevelRuleType");
DROP TYPE "VipLevelRuleType_old";

-- 5. 收窄 BalanceLogType 枚举（移除 hosting_withdraw / hosting_deduction）。
--    历史托管流水仍属面板余额台账，改记为 admin_adjust 以保留台账连续性。
UPDATE "balance_logs" SET "type" = 'admin_adjust' WHERE "type"::text IN ('hosting_withdraw', 'hosting_deduction');
ALTER TYPE "BalanceLogType" RENAME TO "BalanceLogType_old";
CREATE TYPE "BalanceLogType" AS ENUM (
  'recharge',
  'consume',
  'refund',
  'admin_adjust',
  'gift',
  'transfer_fee',
  'transfer_refund',
  'invite_generate',
  'exchange_purchase',
  'exchange_refund',
  'exchange_transfer',
  'gift_card_issue'
);
ALTER TABLE "balance_logs" ALTER COLUMN "type" TYPE "BalanceLogType" USING ("type"::text::"BalanceLogType");
DROP TYPE "BalanceLogType_old";

-- 6. 删除托管系统配置项
DELETE FROM "system_configs" WHERE "key" IN (
  'hosting_feature_enabled',
  'hosting_market_entry_enabled',
  'hosting_notice'
);
