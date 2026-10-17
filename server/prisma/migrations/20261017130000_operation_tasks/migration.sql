CREATE TABLE "operation_tasks" (
  "id" SERIAL PRIMARY KEY,
  "host_id" INTEGER NOT NULL,
  "instance_name" TEXT NOT NULL,
  "kind" TEXT NOT NULL,
  "status" TEXT NOT NULL DEFAULT 'running',
  "error" TEXT,
  "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  "finished_at" TIMESTAMP(3)
);
CREATE INDEX "operation_tasks_host_id_status_idx" ON "operation_tasks"("host_id", "status");
CREATE INDEX "operation_tasks_created_at_idx" ON "operation_tasks"("created_at");
CREATE TABLE "operation_task_logs" (
  "id" SERIAL PRIMARY KEY,
  "task_id" INTEGER NOT NULL REFERENCES "operation_tasks"("id") ON DELETE CASCADE,
  "source" TEXT NOT NULL,
  "level" TEXT NOT NULL,
  "message" TEXT NOT NULL,
  "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX "operation_task_logs_task_id_id_idx" ON "operation_task_logs"("task_id", "id");
