CREATE TABLE unsaved_submodules(
 slot_id TEXT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
 repository_id TEXT NOT NULL, path TEXT NOT NULL,
 reasons TEXT NOT NULL, detected_at TEXT NOT NULL,
 PRIMARY KEY(slot_id,repository_id,path)
);
CREATE INDEX unsaved_submodule_slot_idx ON unsaved_submodules(slot_id);
