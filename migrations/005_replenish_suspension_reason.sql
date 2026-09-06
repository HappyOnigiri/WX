CREATE TABLE replenish_suspensions_v2(
 workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 reason TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', suspended_at TEXT NOT NULL
);
INSERT INTO replenish_suspensions_v2(workspace_id,reason,detail,suspended_at)
 SELECT workspace_id,'CLEAN',run_id,suspended_at FROM replenish_suspensions;
DROP TABLE replenish_suspensions;
ALTER TABLE replenish_suspensions_v2 RENAME TO replenish_suspensions;
DROP TABLE standby_quarantine_resets;
