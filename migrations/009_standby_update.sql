ALTER TABLE slots ADD COLUMN update_started_at TEXT;
ALTER TABLE slots ADD COLUMN update_completed_at TEXT;
ALTER TABLE slots ADD COLUMN update_copy_mode TEXT;
ALTER TABLE slots ADD COLUMN placement_history_complete INTEGER NOT NULL DEFAULT 0;
ALTER TABLE slot_repositories ADD COLUMN update_requested_ref TEXT;
ALTER TABLE slot_repositories ADD COLUMN update_base_oid TEXT;
ALTER TABLE slot_repositories ADD COLUMN update_fingerprint TEXT;
ALTER TABLE slot_repositories ADD COLUMN update_compatibility_fingerprint TEXT;
ALTER TABLE slot_repositories ADD COLUMN compatibility_fingerprint TEXT NOT NULL DEFAULT '';

CREATE TABLE slot_placements(
 slot_id TEXT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
 repository_id TEXT NOT NULL DEFAULT '', relative_path TEXT NOT NULL,
 kind TEXT NOT NULL, source_path TEXT NOT NULL, content_sha256 TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(slot_id,repository_id,relative_path)
);

CREATE TABLE slot_update_placements(
 slot_id TEXT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
 repository_id TEXT NOT NULL DEFAULT '', relative_path TEXT NOT NULL,
 kind TEXT NOT NULL, source_path TEXT NOT NULL, content_sha256 TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(slot_id,repository_id,relative_path)
);
