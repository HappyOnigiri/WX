CREATE TABLE submodule_snapshots(
 session_id TEXT NOT NULL, repository_id TEXT NOT NULL,
 path TEXT NOT NULL, name TEXT NOT NULL,
 head_oid TEXT NOT NULL, head_ref TEXT NOT NULL,
 index_tree_oid TEXT NOT NULL, worktree_tree_oid TEXT NOT NULL,
 git_state_oid TEXT NOT NULL DEFAULT '',
 capsule_oid TEXT NOT NULL, capsule_recovery_ref TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(session_id,repository_id,path),
 FOREIGN KEY(session_id,repository_id) REFERENCES snapshots(session_id,repository_id) ON DELETE CASCADE
);
CREATE INDEX submodule_snapshot_repository_idx ON submodule_snapshots(repository_id);
