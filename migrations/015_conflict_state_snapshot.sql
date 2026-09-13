ALTER TABLE snapshots ADD COLUMN conflict_oid TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN conflict_recovery_ref TEXT NOT NULL DEFAULT '';
