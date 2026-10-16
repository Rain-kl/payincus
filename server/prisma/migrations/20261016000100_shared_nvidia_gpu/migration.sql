CREATE TABLE "host_gpus" (
  "id" SERIAL PRIMARY KEY,
  "host_id" INTEGER NOT NULL REFERENCES "hosts"("id") ON DELETE CASCADE,
  "uuid" TEXT NOT NULL,
  "pci_address" TEXT NOT NULL,
  "name" TEXT NOT NULL,
  "memory_total_mib" INTEGER NOT NULL,
  "metrics" JSONB NOT NULL DEFAULT '{}',
  "available" BOOLEAN NOT NULL DEFAULT false,
  "runtime_ready" BOOLEAN NOT NULL DEFAULT false,
  "sharing_enabled" BOOLEAN NOT NULL DEFAULT false,
  "max_instances" INTEGER NOT NULL DEFAULT 10 CHECK (max_instances BETWEEN 1 AND 1000),
  "last_seen_at" TIMESTAMP(3) NOT NULL,
  UNIQUE ("host_id", "uuid")
);
ALTER TABLE "packages" ADD COLUMN "gpu_shared" BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN "gpu_device_ids" INTEGER[] NOT NULL DEFAULT '{}';
ALTER TABLE "package_plans" ADD COLUMN "gpu_shared" BOOLEAN,
  ADD COLUMN "gpu_device_ids" INTEGER[] NOT NULL DEFAULT '{}';
ALTER TABLE "instances" ADD COLUMN "gpu_device_id" INTEGER REFERENCES "host_gpus"("id") ON DELETE RESTRICT,
  ADD COLUMN "gpu_released_at" TIMESTAMP(3);
CREATE INDEX "instances_gpu_device_id_idx" ON "instances"("gpu_device_id");
