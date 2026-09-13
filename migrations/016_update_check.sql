-- 更新確認の結果は利用者の意思ではなく機械内部の状態なので config.yaml ではなくここに置く。
-- 1行しか持たないため id を 1 に固定し、行の有無を呼び出し側が気にしなくてよいよう初期行を入れる。
CREATE TABLE update_checks (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  checked_at TEXT NOT NULL DEFAULT '',
  latest_version TEXT NOT NULL DEFAULT '',
  release_url TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  announced_version TEXT NOT NULL DEFAULT ''
);
INSERT INTO update_checks(id) VALUES (1);
