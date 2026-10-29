ALTER TABLE "node_security_states" ADD COLUMN "recovery_blocked" BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE "node_security_states" ADD COLUMN "recovery_generation" INTEGER NOT NULL DEFAULT 0;
