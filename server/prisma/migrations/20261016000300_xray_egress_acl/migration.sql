CREATE TABLE "proxy_nodes" (
  "id" SERIAL PRIMARY KEY, "name" TEXT NOT NULL, "protocol" TEXT NOT NULL,
  "server" TEXT NOT NULL, "port" INTEGER NOT NULL,
  "config_encrypted" TEXT NOT NULL, "fingerprint" TEXT NOT NULL UNIQUE,
  "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP, "updated_at" TIMESTAMP(3) NOT NULL
);
CREATE TABLE "host_network_policies" (
  "host_id" INTEGER PRIMARY KEY REFERENCES "hosts"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "revision" INTEGER NOT NULL DEFAULT 1, "egress" JSONB NOT NULL, "firewall" JSONB NOT NULL,
  "applied_revision" INTEGER NOT NULL DEFAULT 0, "status" JSONB NOT NULL DEFAULT '{}',
  "reported_at" TIMESTAMP(3), "updated_at" TIMESTAMP(3) NOT NULL
);
CREATE TABLE "proxy_probe_jobs" (
  "id" TEXT PRIMARY KEY,
  "host_id" INTEGER NOT NULL REFERENCES "hosts"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "node_ids" JSONB NOT NULL, "snapshot_encrypted" TEXT NOT NULL, "probe_url" TEXT NOT NULL,
  "state" TEXT NOT NULL DEFAULT 'pending', "result" JSONB NOT NULL DEFAULT '[]',
  "lease_until" TIMESTAMP(3), "expires_at" TIMESTAMP(3) NOT NULL,
  "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP, "completed_at" TIMESTAMP(3)
);
CREATE INDEX "proxy_probe_jobs_host_id_state_created_at_idx" ON "proxy_probe_jobs"("host_id", "state", "created_at");
