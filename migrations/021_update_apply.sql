-- 自動適用の claim は案内権（update_announcements）とは別に持つ。案内権を daemon が消費すると、
-- 対話起動の案内（internal/cli/update_notice.go）がその版について出なくなる。
-- 親は install.sh の成否を観測しないため、失敗の再試行は attempts と attempted_at の時間で切る。
CREATE TABLE update_applies (
  version TEXT PRIMARY KEY,
  attempts INTEGER NOT NULL DEFAULT 0,
  attempted_at TEXT NOT NULL DEFAULT ''
);
