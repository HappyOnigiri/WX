package daemon

import (
	"context"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/pool"
)

// resolveBranches は貸出・待機枠の実体に使う branch/OID を解決する。
// fetch は workspace 単位の解決にだけ許可し、明示 branch と復元系の呼び出し側は
// 従来の pool.ResolveBranches を直接使って fetch を起こさない。
func (m *Manager) resolveBranches(ctx context.Context, w discovery.Workspace, specs []string) ([]pool.Resolved, error) {
	fetchDefault, _ := m.Config().FetchDefaultBranchForWorkspace(string(w.Root))
	if !fetchDefault || len(specs) != 0 {
		return pool.ResolveBranches(ctx, m.git, w, specs)
	}
	return pool.ResolveBranchesWithFetch(ctx, m.git, w, specs, func(warning pool.FetchWarning) {
		if m.log != nil {
			m.log.Warn("default branch fetch skipped remote update; using local base", "workspace_id", w.ID, "repository_id", warning.Repository.ID, "repository", warning.Repository.RelativePath, "operation", warning.Operation, "error", warning.Err)
		}
	})
}
