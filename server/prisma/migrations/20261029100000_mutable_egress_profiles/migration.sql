-- New deployment contract: policies are mutable references, never immutable versions.
ALTER TABLE "packages" DROP CONSTRAINT IF EXISTS "packages_egress_profile_version_id_fkey";
ALTER TABLE "instances" DROP CONSTRAINT IF EXISTS "instances_egress_profile_version_id_fkey";
ALTER TABLE "packages" DROP COLUMN "egress_profile_version_id", ADD COLUMN "egress_profile_id" INTEGER REFERENCES "egress_profiles"("id") ON DELETE RESTRICT;
ALTER TABLE "instances" DROP COLUMN "egress_profile_version_id", ADD COLUMN "egress_profile_id" INTEGER REFERENCES "egress_profiles"("id") ON DELETE RESTRICT;
ALTER TABLE "instance_egress_attachments" DROP COLUMN "profile_version_id", ADD COLUMN "profile_id" INTEGER REFERENCES "egress_profiles"("id") ON DELETE RESTRICT;
DROP TABLE "egress_profile_versions";
ALTER TABLE "egress_profiles" ADD COLUMN "egress" JSONB NOT NULL DEFAULT '{}', ADD COLUMN "allowed_host_ids" INTEGER[] NOT NULL DEFAULT '{}', ADD COLUMN "updated_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE "host_network_policies" DROP COLUMN "egress", ADD COLUMN "egress_mode" TEXT NOT NULL DEFAULT 'off', ADD COLUMN "egress_profile_id" INTEGER REFERENCES "egress_profiles"("id") ON DELETE RESTRICT;
ALTER TABLE "packages" ALTER COLUMN "egress_mode" SET DEFAULT 'host';
ALTER TABLE "instances" ALTER COLUMN "egress_mode" SET DEFAULT 'host';

ALTER TABLE "packages" ADD CONSTRAINT "packages_egress_mode_check" CHECK (("egress_mode" IN ('host','direct') AND "egress_profile_id" IS NULL) OR ("egress_mode" = 'profile' AND "egress_profile_id" IS NOT NULL));
ALTER TABLE "instances" ADD CONSTRAINT "instances_egress_mode_check" CHECK (("egress_mode" IN ('host','direct') AND "egress_profile_id" IS NULL) OR ("egress_mode" = 'profile' AND "egress_profile_id" IS NOT NULL));
ALTER TABLE "host_network_policies" ADD CONSTRAINT "host_network_policy_egress_check" CHECK (("egress_mode" IN ('off','direct') AND "egress_profile_id" IS NULL) OR ("egress_mode" = 'profile' AND "egress_profile_id" IS NOT NULL));
ALTER TABLE "instance_egress_attachments" ADD CONSTRAINT "instance_egress_attachment_mode_check" CHECK (("mode" = 'direct' AND "profile_id" IS NULL) OR ("mode" = 'profile' AND "profile_id" IS NOT NULL));
