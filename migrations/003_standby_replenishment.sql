CREATE TABLE standby_replenish_successes(
 session_id TEXT PRIMARY KEY,
 workspace_id TEXT NOT NULL,
 recorded_at TEXT NOT NULL
);
CREATE TABLE standby_replenish_exclusions(
 slot_id TEXT PRIMARY KEY,
 workspace_id TEXT NOT NULL,
 generation INTEGER NOT NULL,
 success_session_id TEXT NOT NULL,
 excluded_at TEXT NOT NULL
);
CREATE INDEX standby_replenish_success_workspace_idx ON standby_replenish_successes(workspace_id);
CREATE INDEX standby_replenish_exclusion_workspace_idx ON standby_replenish_exclusions(workspace_id,generation);
CREATE INDEX standby_replenish_exclusion_success_idx ON standby_replenish_exclusions(success_session_id);
