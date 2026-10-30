-- 实例创建排队：新增 create 任务类型与重放参数快照。
-- 节点不可用或创建出错时任务回到 PENDING 重试，最多 5 次（worker 侧控制）。

-- 1. 扩展任务类型枚举（不可在事务块内使用新值，故单独执行）
ALTER TYPE "InstanceTaskType" ADD VALUE IF NOT EXISTS 'create';

-- 2. 创建任务重放所需的参数快照
ALTER TABLE "instance_tasks" ADD COLUMN IF NOT EXISTS "payload" JSONB;

-- 3. 队列按节点 + 类型 + 状态查询，心跳与 worker 都会用到
CREATE INDEX IF NOT EXISTS "instance_tasks_host_id_task_type_status_idx"
  ON "instance_tasks" ("host_id", "task_type", "status");
