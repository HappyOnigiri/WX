CREATE TABLE standby_quarantine_resets(
 workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL, reset_at TEXT NOT NULL
);
CREATE INDEX standby_quarantine_reset_generation_idx ON standby_quarantine_resets(workspace_id,generation);
