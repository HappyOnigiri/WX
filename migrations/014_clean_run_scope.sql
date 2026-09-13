ALTER TABLE clean_runs ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE clean_runs ADD COLUMN replenish INTEGER NOT NULL DEFAULT 0;
ALTER TABLE clean_runs ADD COLUMN replenish_state TEXT NOT NULL DEFAULT '';
ALTER TABLE clean_runs ADD COLUMN replenish_result TEXT;
