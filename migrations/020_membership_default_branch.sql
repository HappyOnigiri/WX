-- 既定 branch は workspace ごとの設定であり、repository の identity ではない。
-- repositories の共有 row に置くと、同じ repository を含む別 workspace の登録が
-- 先に登録した workspace の branch を上書きし、standby と貸出が別 workspace の
-- branch を materialize する。membership 側へ移し、repository row は main path と
-- common directory という不変 identity だけを持つ。
ALTER TABLE workspace_repositories ADD COLUMN default_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE session_repositories ADD COLUMN default_branch TEXT NOT NULL DEFAULT '';
UPDATE workspace_repositories SET default_branch =
  COALESCE((SELECT r.default_branch FROM repositories r WHERE r.id=workspace_repositories.repository_id), '');
UPDATE session_repositories SET default_branch =
  COALESCE((SELECT r.default_branch FROM repositories r WHERE r.id=session_repositories.repository_id), '');
ALTER TABLE repositories DROP COLUMN default_branch;
