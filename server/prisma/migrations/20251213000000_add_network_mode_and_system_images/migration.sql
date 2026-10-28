-- 修改 NetworkMode 枚举：添加新值并重命名
-- 旧值: nat, ipv6, dual
-- 新值: nat, nat_ipv6, ipv4, ipv4_ipv6
-- 注意: PostgreSQL 不允许在同一事务中添加新枚举值后立即使用，所以使用替换枚举类型的方法

-- 1. 创建新的枚举类型
CREATE TYPE "NetworkMode_new" AS ENUM ('nat', 'nat_ipv6', 'ipv4', 'ipv4_ipv6');

-- 2. 修改 packages 表：先删除默认值，转换类型，再恢复默认值
ALTER TABLE "packages" ALTER COLUMN "network_mode" DROP DEFAULT;
ALTER TABLE "packages" ALTER COLUMN "network_mode" TYPE TEXT;
UPDATE "packages" SET "network_mode" = 'nat_ipv6' WHERE "network_mode" = 'ipv6';
UPDATE "packages" SET "network_mode" = 'ipv4_ipv6' WHERE "network_mode" = 'dual';
ALTER TABLE "packages" ALTER COLUMN "network_mode" TYPE "NetworkMode_new" USING "network_mode"::"NetworkMode_new";
ALTER TABLE "packages" ALTER COLUMN "network_mode" SET DEFAULT 'nat'::"NetworkMode_new";

-- 3. 修改 instances 表：先删除默认值，转换类型，再恢复默认值
ALTER TABLE "instances" ALTER COLUMN "network_mode" DROP DEFAULT;
ALTER TABLE "instances" ALTER COLUMN "network_mode" TYPE TEXT;
UPDATE "instances" SET "network_mode" = 'nat_ipv6' WHERE "network_mode" = 'ipv6';
UPDATE "instances" SET "network_mode" = 'ipv4_ipv6' WHERE "network_mode" = 'dual';
ALTER TABLE "instances" ALTER COLUMN "network_mode" TYPE "NetworkMode_new" USING "network_mode"::"NetworkMode_new";
ALTER TABLE "instances" ALTER COLUMN "network_mode" SET DEFAULT 'nat'::"NetworkMode_new";

-- 4. 删除旧枚举类型，重命名新类型
DROP TYPE "NetworkMode";
ALTER TYPE "NetworkMode_new" RENAME TO "NetworkMode";

-- CreateTable
CREATE TABLE "system_images" (
    "id" SERIAL NOT NULL,
    "name" TEXT NOT NULL,
    "remote_alias" TEXT NOT NULL,
    "os_type" TEXT NOT NULL DEFAULT 'Linux',
    "architecture" TEXT NOT NULL DEFAULT 'x86_64',
    "icon" TEXT NOT NULL,
    "sort_order" INTEGER NOT NULL DEFAULT 0,
    "hidden" BOOLEAN NOT NULL DEFAULT false,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_at" TIMESTAMP(3) NOT NULL,

    CONSTRAINT "system_images_pkey" PRIMARY KEY ("id")
);

-- CreateIndex
CREATE UNIQUE INDEX "system_images_remote_alias_key" ON "system_images"("remote_alias");

-- 不再预置默认镜像，由管理员在镜像管理中按需添加
