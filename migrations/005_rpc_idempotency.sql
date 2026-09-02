CREATE TABLE rpc_idempotency (
  idempotency_key TEXT PRIMARY KEY,
  method TEXT NOT NULL,
  params TEXT NOT NULL,
  result BLOB,
  error_code TEXT,
  error_message TEXT,
  completed_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE INDEX rpc_idempotency_expiry_idx ON rpc_idempotency(expires_at);

CREATE TABLE quarantined_artifacts (
  path TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  reason TEXT NOT NULL,
  detected_at TEXT NOT NULL
);
