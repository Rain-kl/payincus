CREATE TABLE "node_security_states" (
 "server_fingerprint" TEXT,
 "host_id" INTEGER PRIMARY KEY REFERENCES "hosts"("id") ON DELETE CASCADE,
 "report" JSONB NOT NULL,
 "reported_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE "node_storage_keys" (
 "id" SERIAL PRIMARY KEY,
 "host_id" INTEGER NOT NULL REFERENCES "hosts"("id") ON DELETE CASCADE,
 "dataset" TEXT NOT NULL,
 "fingerprint" TEXT NOT NULL,
 "ciphertext" TEXT NOT NULL,
 "version" INTEGER NOT NULL DEFAULT 1,
 "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
 UNIQUE("host_id","dataset")
);
CREATE TABLE "node_cli_events" (
 "host_id" INTEGER NOT NULL REFERENCES "hosts"("id") ON DELETE CASCADE,
 "event_id" TEXT NOT NULL,
 "command" TEXT NOT NULL,
 "uid" BIGINT NOT NULL,
 "pid" INTEGER NOT NULL,
 "critical" BOOLEAN NOT NULL,
 "blocked" BOOLEAN NOT NULL,
 "occurred_at" TIMESTAMP(3) NOT NULL,
 "received_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY("host_id","event_id")
);
CREATE INDEX "node_cli_events_host_id_received_at_idx" ON "node_cli_events"("host_id","received_at");
