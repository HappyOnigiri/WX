CREATE TABLE clean_runs(
 id TEXT PRIMARY KEY, mode TEXT NOT NULL, state TEXT NOT NULL,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, finished_at TEXT
);
CREATE TABLE clean_targets(
 run_id TEXT NOT NULL REFERENCES clean_runs(id) ON DELETE CASCADE,
 slot_id TEXT NOT NULL, workspace_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
 path TEXT NOT NULL, state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
 terminate_deadline TEXT, updated_at TEXT NOT NULL,
 PRIMARY KEY(run_id, slot_id)
);
CREATE TABLE session_termination_requests(
 session_id TEXT PRIMARY KEY REFERENCES sessions(id),
 request_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES clean_runs(id) ON DELETE CASCADE,
 requested_at TEXT NOT NULL, deadline TEXT NOT NULL, state TEXT NOT NULL
);
CREATE TABLE replenish_suspensions(
 workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL, suspended_at TEXT NOT NULL
);
CREATE INDEX idx_clean_runs_state ON clean_runs(state);
CREATE INDEX idx_clean_targets_state ON clean_targets(run_id, state);
