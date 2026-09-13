ALTER TABLE snapshots ADD COLUMN git_state_oid TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN git_state_recovery_ref TEXT NOT NULL DEFAULT '';
