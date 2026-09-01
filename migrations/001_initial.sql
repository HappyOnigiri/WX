CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE workspaces(
 id TEXT PRIMARY KEY, root_path TEXT UNIQUE NOT NULL, kind TEXT NOT NULL,
 generation INTEGER NOT NULL, discovery_state TEXT NOT NULL,
 first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, last_reconciled_at TEXT NOT NULL
);
CREATE TABLE repositories(
 id TEXT PRIMARY KEY, main_worktree_path TEXT NOT NULL, common_git_dir TEXT UNIQUE NOT NULL,
 default_branch TEXT NOT NULL, first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, last_leased_at TEXT
);
CREATE TABLE workspace_repositories(
 workspace_id TEXT NOT NULL REFERENCES workspaces(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 relative_path TEXT NOT NULL, ordinal INTEGER NOT NULL,
 PRIMARY KEY(workspace_id, repository_id), UNIQUE(workspace_id, relative_path)
);
CREATE TABLE slots(
 id TEXT PRIMARY KEY, workspace_id TEXT REFERENCES workspaces(id), generation INTEGER NOT NULL,
 path TEXT UNIQUE NOT NULL, state TEXT NOT NULL, owner_session_id TEXT,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, ready_at TEXT, last_used_at TEXT,
 failure_code TEXT, failure_detail_path TEXT
);
CREATE TABLE slot_repositories(
 slot_id TEXT NOT NULL REFERENCES slots(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 worktree_path TEXT UNIQUE NOT NULL, state TEXT NOT NULL, requested_ref TEXT NOT NULL,
 base_oid TEXT NOT NULL, prepare_fingerprint TEXT NOT NULL, git_worktree_name TEXT,
 PRIMARY KEY(slot_id, repository_id)
);
CREATE TABLE sessions(
 id TEXT PRIMARY KEY, workspace_id TEXT REFERENCES workspaces(id), slot_id TEXT NOT NULL REFERENCES slots(id),
 parent_session_id TEXT REFERENCES sessions(id), state TEXT NOT NULL, agent_kind TEXT NOT NULL,
 agent_session_id TEXT, client_pid INTEGER, session_token_hash BLOB NOT NULL,
 requested_branch_spec TEXT, created_at TEXT NOT NULL, started_at TEXT, released_at TEXT,
 archived_at TEXT, expires_at TEXT, UNIQUE(agent_kind, agent_session_id)
);
CREATE TABLE snapshots(
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 head_oid TEXT NOT NULL, head_recovery_ref TEXT NOT NULL, index_tree_oid TEXT NOT NULL,
 worktree_snapshot_oid TEXT NOT NULL, worktree_recovery_ref TEXT NOT NULL,
 status TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
 UNIQUE(session_id, repository_id)
);
CREATE TABLE jobs(
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, workspace_id TEXT, slot_id TEXT, session_id TEXT, repository_id TEXT,
 state TEXT NOT NULL, attempt INTEGER NOT NULL, not_before TEXT, started_at TEXT, finished_at TEXT,
 lease_owner TEXT, lease_expires_at TEXT, error_code TEXT, error_detail_path TEXT
);
CREATE TABLE events(
 id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, level TEXT NOT NULL, kind TEXT NOT NULL,
 workspace_id TEXT, slot_id TEXT, session_id TEXT, repository_id TEXT, message TEXT NOT NULL
);
CREATE INDEX idx_slots_ready ON slots(workspace_id, generation, state, ready_at);
CREATE INDEX idx_sessions_agent ON sessions(agent_kind, agent_session_id);
CREATE INDEX idx_jobs_state ON jobs(state, not_before);
