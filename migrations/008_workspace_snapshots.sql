CREATE TABLE workspace_snapshots (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id),
  archive_path TEXT UNIQUE NOT NULL,
  sha256 TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE INDEX workspace_snapshots_expiry_idx ON workspace_snapshots(status, expires_at);
