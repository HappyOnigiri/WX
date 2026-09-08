-- エージェント起動以外への worktree 貸出を session の列で表す。
-- 新しい state を作ると state IN (...) を持つ全 SQL を一斉に触ることになり、
-- 返却が保存経路に乗らなくなるため、貸出の性質だけを列で区別する。
ALTER TABLE sessions ADD COLUMN lease_kind TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE sessions ADD COLUMN lease_expires_at TEXT;
ALTER TABLE sessions ADD COLUMN lease_owner_session_id TEXT REFERENCES sessions(id);
CREATE INDEX IF NOT EXISTS idx_sessions_lease_expiry ON sessions(lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_lease_owner ON sessions(lease_owner_session_id);
