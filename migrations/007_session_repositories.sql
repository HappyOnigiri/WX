CREATE TABLE session_repositories (
  session_id TEXT NOT NULL REFERENCES sessions(id),
  repository_id TEXT NOT NULL REFERENCES repositories(id),
  relative_path TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  PRIMARY KEY(session_id, repository_id),
  UNIQUE(session_id, relative_path)
);

INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal)
SELECT se.id,sr.repository_id,wr.relative_path,wr.ordinal
FROM sessions se
JOIN slot_repositories sr ON sr.slot_id=se.slot_id
JOIN workspace_repositories wr ON wr.workspace_id=se.workspace_id AND wr.repository_id=sr.repository_id;

CREATE INDEX session_repositories_repository_idx ON session_repositories(repository_id);
