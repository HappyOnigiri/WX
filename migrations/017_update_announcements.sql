-- 案内済みの版は上書きではなく履歴で持つ。単一の値だと、リリースの非公開と再公開で
-- 最新が A→B→A と動いたときに A の案内権がもう一度成立し、同じ版が二度案内される。
CREATE TABLE update_announcements (
  version TEXT PRIMARY KEY,
  announced_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO update_announcements(version, announced_at)
  SELECT announced_version, checked_at FROM update_checks WHERE announced_version <> '';
ALTER TABLE update_checks DROP COLUMN announced_version;
