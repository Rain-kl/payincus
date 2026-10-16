CREATE TABLE "instance_gpu_allocations" (
  "instance_id" INTEGER NOT NULL REFERENCES "instances"("id") ON DELETE CASCADE,
  "gpu_device_id" INTEGER NOT NULL REFERENCES "host_gpus"("id") ON DELETE RESTRICT,
  PRIMARY KEY ("instance_id", "gpu_device_id")
);
CREATE INDEX "instance_gpu_allocations_gpu_device_id_idx" ON "instance_gpu_allocations"("gpu_device_id");

-- Preserve existing single-card instances exactly; package edits never add cards
-- to containers that have already been delivered.
INSERT INTO "instance_gpu_allocations" ("instance_id", "gpu_device_id")
SELECT "id", "gpu_device_id" FROM "instances" WHERE "gpu_device_id" IS NOT NULL;
