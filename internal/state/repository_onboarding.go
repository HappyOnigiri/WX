package state

import "context"

// FirstLeaseRepository は、この workspace でまだ一度も貸し出されていない repository の事実である。
// last_leased_at を更新する貸出 transaction より前にだけ正しく読めるため、daemon の貸出処理専用とする。
type FirstLeaseRepository struct {
	RelativePath string
	MainPath     string
}

func (s *Store) FirstLeaseRepositories(ctx context.Context, workspaceID string) ([]FirstLeaseRepository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT wr.relative_path,r.main_worktree_path FROM workspace_repositories wr JOIN repositories r ON r.id=wr.repository_id WHERE wr.workspace_id=? AND r.last_leased_at IS NULL ORDER BY wr.ordinal`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []FirstLeaseRepository
	for rows.Next() {
		var repository FirstLeaseRepository
		if err := rows.Scan(&repository.RelativePath, &repository.MainPath); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}
