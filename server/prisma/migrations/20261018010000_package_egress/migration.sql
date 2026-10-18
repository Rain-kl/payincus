CREATE TABLE "egress_profiles" (
 "id" SERIAL PRIMARY KEY, "name" TEXT NOT NULL, "active" BOOLEAN NOT NULL DEFAULT true,
 "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE "egress_profile_versions" (
 "id" SERIAL PRIMARY KEY, "profile_id" INTEGER NOT NULL REFERENCES "egress_profiles"("id") ON DELETE RESTRICT,
 "version" INTEGER NOT NULL, "proxy_node_id" INTEGER NOT NULL REFERENCES "proxy_nodes"("id") ON DELETE RESTRICT,
 "allowed_host_ids" INTEGER[] NOT NULL, "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
 UNIQUE ("profile_id", "version")
);
CREATE INDEX "egress_profile_versions_proxy_node_id_idx" ON "egress_profile_versions"("proxy_node_id");
ALTER TABLE "packages" ADD COLUMN "egress_mode" TEXT NOT NULL DEFAULT 'legacy', ADD COLUMN "egress_profile_version_id" INTEGER REFERENCES "egress_profile_versions"("id") ON DELETE RESTRICT;
ALTER TABLE "instances" ADD COLUMN "egress_mode" TEXT NOT NULL DEFAULT 'legacy', ADD COLUMN "egress_profile_version_id" INTEGER REFERENCES "egress_profile_versions"("id") ON DELETE RESTRICT;
ALTER TABLE "packages" ADD CONSTRAINT "packages_egress_mode_check" CHECK (("egress_mode" IN ('legacy','direct') AND "egress_profile_version_id" IS NULL) OR ("egress_mode" = 'profile' AND "egress_profile_version_id" IS NOT NULL));
ALTER TABLE "instances" ADD CONSTRAINT "instances_egress_mode_check" CHECK (("egress_mode" IN ('legacy','direct') AND "egress_profile_version_id" IS NULL) OR ("egress_mode" = 'profile' AND "egress_profile_version_id" IS NOT NULL));
CREATE TABLE "instance_egress_attachments" (
 "id" SERIAL PRIMARY KEY, "instance_id" INTEGER NOT NULL REFERENCES "instances"("id") ON DELETE CASCADE,
 "host_id" INTEGER NOT NULL, "incus_name" TEXT NOT NULL, "mode" TEXT NOT NULL,
 "profile_version_id" INTEGER, "generation" INTEGER NOT NULL DEFAULT 1, "nics" JSONB NOT NULL,
 UNIQUE ("host_id", "incus_name")
);
CREATE INDEX "instance_egress_attachments_instance_id_idx" ON "instance_egress_attachments"("instance_id");
