package config

import "time"

// WorktreeMode は正規化済み workspace root の個別設定を優先し、未定義なら全体の方針を返す。
func (c Config) WorktreeMode(root string) string {
	if mode := c.WorkspaceFor(root).Worktree; mode != "" {
		return mode
	}
	return c.WorkspaceDefaults.Worktree
}

// WarmCountForWorkspace は workspace root に対する待機枠数と、個別設定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) WarmCountForWorkspace(root string) (int, bool) {
	w := c.WorkspaceFor(root)
	if w.WarmCount != nil {
		return *w.WarmCount, c.Workspaces[root].WarmCount != nil
	}
	if c.WorkspaceDefaults.WarmCount == nil {
		return 0, false
	}
	return *c.WorkspaceDefaults.WarmCount, false
}

// ReuseStandbyForWorkspace は古い READY slot を貸出時に更新する実効方針を返す。
func (c Config) ReuseStandbyForWorkspace(root string) (bool, bool) {
	w := c.WorkspaceFor(root)
	if w.ReuseStandby != nil {
		return *w.ReuseStandby, c.Workspaces[root].ReuseStandby != nil
	}
	return false, false
}

// FetchDefaultBranchForWorkspace は branch 未指定の貸出前に origin の既定 branch を
// fetch する実効方針と、workspace 個別指定の有無を返す。
func (c Config) FetchDefaultBranchForWorkspace(root string) (bool, bool) {
	w := c.WorkspaceFor(root)
	if w.FetchDefaultBranch != nil {
		return *w.FetchDefaultBranch, c.Workspaces[root].FetchDefaultBranch != nil
	}
	return false, false
}

// SubmodulesForWorkspace は準備時に submodule を実体化する実効方針と、個別指定の有無を返す。
func (c Config) SubmodulesForWorkspace(root string) (bool, bool) {
	c = withLegacyAdapter(c)
	if w, ok := c.Workspaces[root]; ok && w.RepositoryDefaults.Submodules != nil {
		return *w.RepositoryDefaults.Submodules, true
	}
	if c.RepositoryDefaults.Submodules != nil {
		return *c.RepositoryDefaults.Submodules, false
	}
	return false, false
}

// AddDirForWorkspace は workspace root に対する agent.add_dir の実効値と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) AddDirForWorkspace(root string) (string, bool) {
	w := c.WorkspaceFor(root)
	if w.Agent.AddDir != "" {
		return w.Agent.AddDir, c.Workspaces[root].Agent.AddDir != ""
	}
	return c.WorkspaceDefaults.Agent.AddDir, false
}

// CodexNoDaemonForWorkspace は Codex の共有 daemon を使わない実効方針を返す。
func (c Config) CodexNoDaemonForWorkspace(root string) bool {
	if enabled := c.WorkspaceFor(root).Agent.CodexNoDaemon; enabled != nil {
		return *enabled
	}
	return true
}

// HotStandbyForWorkspace は workspace root に対する待機枠の保持期間と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) HotStandbyForWorkspace(root string) (time.Duration, bool) {
	w := c.WorkspaceFor(root)
	if w.Retention.HotStandby != nil {
		return w.Retention.HotStandby.Duration, c.Workspaces[root].Retention.HotStandby != nil
	}
	return 0, false
}

// EndedWorktreeForWorkspace は workspace root に対する終了 worktree の保持期間と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) EndedWorktreeForWorkspace(root string) (time.Duration, bool) {
	w := c.WorkspaceFor(root)
	if w.Retention.EndedWorktree != nil {
		return w.Retention.EndedWorktree.Duration, c.Workspaces[root].Retention.EndedWorktree != nil
	}
	return 0, false
}

// DiscoveryMaxDepthForWorkspace は workspace root に対する探索の深さ上限と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) DiscoveryMaxDepthForWorkspace(root string) (int, bool) {
	w := c.WorkspaceFor(root)
	if w.Discovery.MaxDepth != nil {
		return *w.Discovery.MaxDepth, c.Workspaces[root].Discovery.MaxDepth != nil
	}
	return 0, false
}

// DiscoveryExcludeForWorkspace は workspace root に対する探索除外名と、個別指定の有無を返す。
// 個別指定は global list への追加ではなく置き換えである。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) DiscoveryExcludeForWorkspace(root string) ([]string, bool) {
	w := c.WorkspaceFor(root)
	if w.Discovery.Exclude != nil {
		return cloneStrings(w.Discovery.Exclude), c.Workspaces[root].Discovery.Exclude != nil
	}
	return nil, false
}

// ReadinessForRepository は repository の readiness 実効値を組で返す。消費側が mode・early_paths・timeout を常に組で使うため、個別に解決させない。
// Progress も mode などと同じく repository override を反映する。
// mainPath は NormalizePaths 済み canonical path であることを呼び出し側の契約とする。
func (c Config) ReadinessForRepository(mainPath string) Readiness {
	c = withLegacyAdapter(c)
	return readinessFromRepository(c, c.RepositoryFor("", ".", mainPath))
}

func readinessFromRepository(c Config, repository Repository) Readiness {
	out := Readiness{
		Mode:       c.RepositoryDefaults.Readiness.Mode,
		EarlyPaths: cloneStrings(c.RepositoryDefaults.Readiness.EarlyPaths),
		Progress:   derefBool(c.RepositoryDefaults.Readiness.Progress),
		Timeout:    derefDuration(c.RepositoryDefaults.Readiness.Timeout),
	}
	if repository.Readiness.Mode != "" {
		out.Mode = repository.Readiness.Mode
	}
	if repository.Readiness.EarlyPaths != nil {
		out.EarlyPaths = cloneStrings(repository.Readiness.EarlyPaths)
	}
	if repository.Readiness.Timeout != nil {
		out.Timeout = *repository.Readiness.Timeout
	}
	if repository.Readiness.Progress != nil {
		out.Progress = *repository.Readiness.Progress
	}
	return out
}

// ReadinessForWorkspaceRepository は workspace membership を含む文脈で readiness を
// 解決する。multi-repository workspace ではこの API を優先して使う。
func (c Config) ReadinessForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) Readiness {
	c = withLegacyAdapter(c)
	return readinessFromRepository(c, c.RepositoryFor(workspaceRoot, relativePath, mainPath))
}

func (c Config) CopyModeForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) string {
	c = withLegacyAdapter(c)
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if c.prepareOverride.CopyMode != "" {
		return c.prepareOverride.CopyMode
	}
	if o.Storage.CopyMode != "" {
		return o.Storage.CopyMode
	}
	return c.RepositoryDefaults.Storage.CopyMode
}

func (c Config) COWMinSizeKiBForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) int {
	c = withLegacyAdapter(c)
	if c.prepareOverride.COWMinSizeKiB != nil {
		return *c.prepareOverride.COWMinSizeKiB
	}
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if o.COWMinSizeKiB != nil {
		return *o.COWMinSizeKiB
	}
	if c.RepositoryDefaults.COWMinSizeKiB == nil {
		return 0
	}
	return *c.RepositoryDefaults.COWMinSizeKiB
}

func (c Config) DefaultAgentRulesForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) bool {
	c = withLegacyAdapter(c)
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if o.Includes.DefaultAgentRules != nil {
		return *o.Includes.DefaultAgentRules
	}
	return c.RepositoryDefaults.Includes.DefaultAgentRules != nil && *c.RepositoryDefaults.Includes.DefaultAgentRules
}

// HotStandbyOverrides は workspace root ごとの明示的な待機枠保持期間をコピーして返す。
func (c Config) HotStandbyOverrides() map[string]time.Duration {
	return durationOverrides(c.Workspaces, func(w Workspace) *Duration { return w.Retention.HotStandby })
}

// EndedWorktreeOverrides は workspace root ごとの明示的な終了 worktree 保持期間をコピーして返す。
func (c Config) EndedWorktreeOverrides() map[string]time.Duration {
	return durationOverrides(c.Workspaces, func(w Workspace) *Duration { return w.Retention.EndedWorktree })
}

func durationOverrides(workspaces map[string]Workspace, pick func(Workspace) *Duration) map[string]time.Duration {
	out := make(map[string]time.Duration)
	for root, workspace := range workspaces {
		if d := pick(workspace); d != nil {
			out[root] = d.Duration
		}
	}
	return out
}

// ShortestHotStandbyRetention は global と全 workspace 個別指定のうち最短の待機枠保持期間を返す。
// SQL の走査量を絞る floor に使う。保持期間が短いほど期限切れの範囲は広いので、
// 最短で引いた集合はどの workspace の判定結果も含む。正確な per-root 判定は Go 側で行う。
func (c Config) ShortestHotStandbyRetention() time.Duration {
	c = withLegacyAdapter(c)
	base := derefDuration(c.WorkspaceDefaults.Retention.HotStandby).Duration
	return minDuration(base, c.HotStandbyOverrides())
}

// ShortestEndedWorktreeRetention は global と全 workspace 個別指定のうち最短の終了 worktree 保持期間を返す。
func (c Config) ShortestEndedWorktreeRetention() time.Duration {
	c = withLegacyAdapter(c)
	base := derefDuration(c.WorkspaceDefaults.Retention.EndedWorktree).Duration
	return minDuration(base, c.EndedWorktreeOverrides())
}

// MaxReadinessTimeout は global と全 repository 個別指定のうち最長の readiness timeout を返す。
// handler の上限を global だけで決めると、個別指定が長い repository の待機が handler 側で黙って切られる。
func (c Config) MaxReadinessTimeout() time.Duration {
	c = withLegacyAdapter(c)
	longest := derefDuration(c.RepositoryDefaults.Readiness.Timeout).Duration
	for _, w := range c.Workspaces {
		if w.RepositoryDefaults.Readiness.Timeout != nil && w.RepositoryDefaults.Readiness.Timeout.Duration > longest {
			longest = w.RepositoryDefaults.Readiness.Timeout.Duration
		}
		for _, r := range w.Repositories {
			if r.Readiness.Timeout != nil && r.Readiness.Timeout.Duration > longest {
				longest = r.Readiness.Timeout.Duration
			}
		}
	}
	if c.present == nil {
		for _, r := range c.Repositories {
			if r.Readiness.Timeout != nil && r.Readiness.Timeout.Duration > longest {
				longest = r.Readiness.Timeout.Duration
			}
		}
	}
	return longest
}

func minDuration(base time.Duration, overrides map[string]time.Duration) time.Duration {
	shortest := base
	for _, d := range overrides {
		if d < shortest {
			shortest = d
		}
	}
	return shortest
}
