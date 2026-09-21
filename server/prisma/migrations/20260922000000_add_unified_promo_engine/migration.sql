-- 统一优惠码引擎（Unified Promo Code Engine）模型与枚举
-- CreateEnum
CREATE TYPE "PromoCodeType" AS ENUM ('AFF_USER', 'ADMIN_PROMO');

-- CreateEnum
CREATE TYPE "PromoDiscountType" AS ENUM ('PERCENTAGE', 'FIXED_AMOUNT');

-- CreateEnum
CREATE TYPE "PromoDurationType" AS ENUM ('ONCE', 'REPEATING', 'FOREVER');

-- CreateTable
CREATE TABLE IF NOT EXISTS "promo_codes" (
    "id" SERIAL NOT NULL,
    "code" VARCHAR(64) NOT NULL,
    "name" VARCHAR(128),
    "type" "PromoCodeType" NOT NULL DEFAULT 'ADMIN_PROMO',
    "user_id" INTEGER,
    "admin_id" INTEGER,
    "is_global" BOOLEAN NOT NULL DEFAULT false,
    "discount_type" "PromoDiscountType" NOT NULL DEFAULT 'PERCENTAGE',
    "discount_value" DECIMAL(10,2) NOT NULL,
    "commission_rate" DECIMAL(5,4) NOT NULL DEFAULT 0,
    "duration_type" "PromoDurationType" NOT NULL DEFAULT 'FOREVER',
    "duration_cycles" INTEGER,
    "max_total_uses" INTEGER,
    "used_total_count" INTEGER NOT NULL DEFAULT 0,
    "max_uses_per_user" INTEGER DEFAULT 1,
    "starts_at" TIMESTAMP(3),
    "expires_at" TIMESTAMP(3),
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "total_discount_amount" DECIMAL(10,2) NOT NULL DEFAULT 0,
    "total_earnings" DECIMAL(10,2) NOT NULL DEFAULT 0,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_at" TIMESTAMP(3) NOT NULL,

    CONSTRAINT "promo_codes_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE IF NOT EXISTS "promo_code_scopes" (
    "id" SERIAL NOT NULL,
    "promo_code_id" INTEGER NOT NULL,
    "package_id" INTEGER,
    "package_plan_id" INTEGER,

    CONSTRAINT "promo_code_scopes_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE IF NOT EXISTS "instance_promo_bindings" (
    "id" SERIAL NOT NULL,
    "instance_id" INTEGER NOT NULL,
    "promo_code_id" INTEGER NOT NULL,
    "duration_type" "PromoDurationType" NOT NULL,
    "total_cycles" INTEGER,
    "used_cycles" INTEGER NOT NULL DEFAULT 0,
    "remaining_cycles" INTEGER,
    "bound_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "last_redeemed_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "instance_promo_bindings_pkey" PRIMARY KEY ("id")
);

-- CreateTable
CREATE TABLE IF NOT EXISTS "promo_redemption_logs" (
    "id" SERIAL NOT NULL,
    "promo_code_id" INTEGER NOT NULL,
    "instance_id" INTEGER NOT NULL,
    "user_id" INTEGER NOT NULL,
    "action_type" VARCHAR(32) NOT NULL,
    "cycle_index" INTEGER NOT NULL,
    "months" INTEGER NOT NULL DEFAULT 1,
    "original_price" DECIMAL(10,2) NOT NULL,
    "discount_amount" DECIMAL(10,2) NOT NULL,
    "final_price" DECIMAL(10,2) NOT NULL,
    "commission_amount" DECIMAL(10,2) NOT NULL DEFAULT 0,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT "promo_redemption_logs_pkey" PRIMARY KEY ("id")
);

-- CreateIndex
CREATE UNIQUE INDEX IF NOT EXISTS "promo_codes_code_key" ON "promo_codes"("code");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_codes_type_enabled_idx" ON "promo_codes"("type", "enabled");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_codes_user_id_idx" ON "promo_codes"("user_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_code_scopes_promo_code_id_idx" ON "promo_code_scopes"("promo_code_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_code_scopes_package_id_idx" ON "promo_code_scopes"("package_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_code_scopes_package_plan_id_idx" ON "promo_code_scopes"("package_plan_id");

-- CreateIndex
CREATE UNIQUE INDEX IF NOT EXISTS "instance_promo_bindings_instance_id_key" ON "instance_promo_bindings"("instance_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "instance_promo_bindings_promo_code_id_idx" ON "instance_promo_bindings"("promo_code_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_redemption_logs_promo_code_id_idx" ON "promo_redemption_logs"("promo_code_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_redemption_logs_instance_id_idx" ON "promo_redemption_logs"("instance_id");

-- CreateIndex
CREATE INDEX IF NOT EXISTS "promo_redemption_logs_user_id_idx" ON "promo_redemption_logs"("user_id");

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_codes_user_id_fkey'
  ) THEN
    ALTER TABLE "promo_codes" ADD CONSTRAINT "promo_codes_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "users"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_code_scopes_promo_code_id_fkey'
  ) THEN
    ALTER TABLE "promo_code_scopes" ADD CONSTRAINT "promo_code_scopes_promo_code_id_fkey" FOREIGN KEY ("promo_code_id") REFERENCES "promo_codes"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_code_scopes_package_id_fkey'
  ) THEN
    ALTER TABLE "promo_code_scopes" ADD CONSTRAINT "promo_code_scopes_package_id_fkey" FOREIGN KEY ("package_id") REFERENCES "packages"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_code_scopes_package_plan_id_fkey'
  ) THEN
    ALTER TABLE "promo_code_scopes" ADD CONSTRAINT "promo_code_scopes_package_plan_id_fkey" FOREIGN KEY ("package_plan_id") REFERENCES "package_plans"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'instance_promo_bindings_instance_id_fkey'
  ) THEN
    ALTER TABLE "instance_promo_bindings" ADD CONSTRAINT "instance_promo_bindings_instance_id_fkey" FOREIGN KEY ("instance_id") REFERENCES "instances"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'instance_promo_bindings_promo_code_id_fkey'
  ) THEN
    ALTER TABLE "instance_promo_bindings" ADD CONSTRAINT "instance_promo_bindings_promo_code_id_fkey" FOREIGN KEY ("promo_code_id") REFERENCES "promo_codes"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_redemption_logs_promo_code_id_fkey'
  ) THEN
    ALTER TABLE "promo_redemption_logs" ADD CONSTRAINT "promo_redemption_logs_promo_code_id_fkey" FOREIGN KEY ("promo_code_id") REFERENCES "promo_codes"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_redemption_logs_instance_id_fkey'
  ) THEN
    ALTER TABLE "promo_redemption_logs" ADD CONSTRAINT "promo_redemption_logs_instance_id_fkey" FOREIGN KEY ("instance_id") REFERENCES "instances"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;

-- AddForeignKey
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'promo_redemption_logs_user_id_fkey'
  ) THEN
    ALTER TABLE "promo_redemption_logs" ADD CONSTRAINT "promo_redemption_logs_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "users"("id") ON DELETE CASCADE ON UPDATE CASCADE;
  END IF;
END $$;
