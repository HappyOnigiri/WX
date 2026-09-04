CREATE TABLE roots(
 id TEXT PRIMARY KEY, path TEXT UNIQUE NOT NULL, identity TEXT NOT NULL,
 active INTEGER NOT NULL, created_at TEXT NOT NULL, retired_at TEXT
);
CREATE TABLE workspaces(
 id TEXT PRIMARY KEY, root_path TEXT UNIQUE NOT NULL, kind TEXT NOT NULL,
 generation INTEGER NOT NULL, discovery_state TEXT NOT NULL,
 first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, last_reconciled_at TEXT NOT NULL
);
CREATE TABLE repositories(
 id TEXT PRIMARY KEY, main_worktree_path TEXT NOT NULL, common_git_dir TEXT UNIQUE NOT NULL,
 default_branch TEXT NOT NULL, remote_name TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL, last_leased_at TEXT
);
CREATE TABLE workspace_repositories(
 workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE, repository_id TEXT NOT NULL REFERENCES repositories(id),
 relative_path TEXT NOT NULL, ordinal INTEGER NOT NULL,
 PRIMARY KEY(workspace_id, repository_id), UNIQUE(workspace_id, relative_path)
);
CREATE TABLE slots(
 id TEXT PRIMARY KEY, workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL, generation INTEGER NOT NULL,
 root_id TEXT NOT NULL REFERENCES roots(id), rel_path TEXT NOT NULL, dir_identity TEXT,
 state TEXT NOT NULL, owner_session_id TEXT,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, ready_at TEXT, last_used_at TEXT,
 failure_code TEXT, failure_detail_path TEXT,
 UNIQUE(root_id, rel_path)
);
CREATE TABLE slot_repositories(
 slot_id TEXT NOT NULL REFERENCES slots(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 dir_name TEXT NOT NULL, dir_identity TEXT, state TEXT NOT NULL, requested_ref TEXT NOT NULL,
 base_oid TEXT NOT NULL, prepare_fingerprint TEXT NOT NULL, git_worktree_name TEXT,
 PRIMARY KEY(slot_id, repository_id), UNIQUE(slot_id, dir_name)
);
CREATE TABLE sessions(
 id TEXT PRIMARY KEY, workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL, slot_id TEXT NOT NULL REFERENCES slots(id),
 parent_session_id TEXT REFERENCES sessions(id), state TEXT NOT NULL, agent_kind TEXT NOT NULL,
 agent_session_id TEXT, client_pid INTEGER, agent_pid INTEGER, session_token_hash BLOB NOT NULL,
 requested_branch_spec TEXT, created_at TEXT NOT NULL, started_at TEXT, released_at TEXT,
 archived_at TEXT, expires_at TEXT, last_heartbeat_at TEXT, pending_agent_session_id TEXT,
 UNIQUE(agent_kind, agent_session_id)
);
CREATE TABLE session_repositories(
 session_id TEXT NOT NULL REFERENCES sessions(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 relative_path TEXT NOT NULL, ordinal INTEGER NOT NULL,
 PRIMARY KEY(session_id, repository_id), UNIQUE(session_id, relative_path)
);
CREATE TABLE snapshots(
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id), repository_id TEXT NOT NULL REFERENCES repositories(id),
 head_oid TEXT NOT NULL, head_recovery_ref TEXT NOT NULL, index_tree_oid TEXT NOT NULL, index_recovery_ref TEXT NOT NULL DEFAULT '',
 worktree_snapshot_oid TEXT NOT NULL, worktree_recovery_ref TEXT NOT NULL,
 status TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
 UNIQUE(session_id, repository_id)
);
CREATE TABLE workspace_snapshots(
 session_id TEXT PRIMARY KEY REFERENCES sessions(id),
 root_id TEXT NOT NULL REFERENCES roots(id), rel_path TEXT NOT NULL, sha256 TEXT NOT NULL,
 status TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
 UNIQUE(root_id, rel_path)
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
CREATE TABLE rpc_idempotency(
 idempotency_key TEXT PRIMARY KEY, method TEXT NOT NULL, params TEXT NOT NULL,
 result BLOB, error_code TEXT, error_message TEXT,
 completed_at TEXT NOT NULL, expires_at TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'COMPLETED'
);
CREATE TABLE quarantined_artifacts(
 path TEXT PRIMARY KEY, kind TEXT NOT NULL, reason TEXT NOT NULL, detected_at TEXT NOT NULL
);
CREATE INDEX idx_slots_ready ON slots(workspace_id, generation, state, ready_at);
CREATE INDEX idx_slots_root ON slots(root_id);
CREATE INDEX idx_sessions_agent ON sessions(agent_kind, agent_session_id);
CREATE INDEX idx_jobs_state ON jobs(state, not_before);
CREATE INDEX session_repositories_repository_idx ON session_repositories(repository_id);
CREATE INDEX workspace_snapshots_expiry_idx ON workspace_snapshots(status, expires_at);
CREATE INDEX rpc_idempotency_expiry_idx ON rpc_idempotency(expires_at);
