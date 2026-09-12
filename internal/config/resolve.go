package config

import "time"

// WorktreeMode は正規化済み workspace root の個別設定を優先し、未定義なら全体の方針を返す。
func (c Config) WorktreeMode(root string) string {
	if c.V2() {
		if mode := c.WorkspaceFor(root).Worktree; mode != "" {
			return mode
		}
		return c.Worktree.Undefined
	}
	if mode := c.Workspaces[root].Worktree; mode != "" {
		return mode
	}
	return c.Worktree.Undefined
}

// WarmCountForWorkspace は workspace root に対する待機枠数と、個別設定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) WarmCountForWorkspace(root string) (int, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.WarmCount != nil {
			return *w.WarmCount, c.Workspaces[root].WarmCount != nil
		}
		return c.Pool.WarmPerWorkspace, false
	}
	if override := c.Workspaces[root].WarmCount; override != nil {
		return *override, true
	}
	return c.Pool.WarmPerWorkspace, false
}

// ReuseStandbyForWorkspace は古い READY slot を貸出時に更新する実効方針を返す。
func (c Config) ReuseStandbyForWorkspace(root string) (bool, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.ReuseStandby != nil {
			return *w.ReuseStandby, c.Workspaces[root].ReuseStandby != nil
		}
		return c.Worktree.ReuseStandby, false
	}
	if override := c.Workspaces[root].ReuseStandby; override != nil {
		return *override, true
	}
	return c.Worktree.ReuseStandby, false
}

// SubmodulesForWorkspace は準備時に submodule を実体化する実効方針と、個別指定の有無を返す。
func (c Config) SubmodulesForWorkspace(root string) (bool, bool) {
	if c.V2() {
		if w, ok := c.Workspaces[root]; ok {
			if w.RepositoryDefaults.Submodules != nil {
				return *w.RepositoryDefaults.Submodules, true
			}
			// memory 上で v2 Config を直接構築する caller には legacy field を許容する。
			// YAML v2 の検証では明示的に拒否する。
			if w.Submodules != nil {
				return *w.Submodules, true
			}
		}
		if c.RepositoryDefaults.Submodules != nil {
			return *c.RepositoryDefaults.Submodules, false
		}
		return c.Worktree.Submodules, false
	}
	if override := c.Workspaces[root].Submodules; override != nil {
		return *override, true
	}
	return c.Worktree.Submodules, false
}

// AddDirForWorkspace は workspace root に対する agent.add_dir の実効値と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) AddDirForWorkspace(root string) (string, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.Agent.AddDir != "" {
			return w.Agent.AddDir, c.Workspaces[root].Agent.AddDir != ""
		}
		return c.Agent.AddDir, false
	}
	if override := c.Workspaces[root].Agent.AddDir; override != "" {
		return override, true
	}
	return c.Agent.AddDir, false
}

// HotStandbyForWorkspace は workspace root に対する待機枠の保持期間と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) HotStandbyForWorkspace(root string) (time.Duration, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.Retention.HotStandby != nil {
			return w.Retention.HotStandby.Duration, c.Workspaces[root].Retention.HotStandby != nil
		}
		return c.Retention.HotStandby.Duration, false
	}
	if override := c.Workspaces[root].Retention.HotStandby; override != nil {
		return override.Duration, true
	}
	return c.Retention.HotStandby.Duration, false
}

// EndedWorktreeForWorkspace は workspace root に対する終了 worktree の保持期間と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) EndedWorktreeForWorkspace(root string) (time.Duration, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.Retention.EndedWorktree != nil {
			return w.Retention.EndedWorktree.Duration, c.Workspaces[root].Retention.EndedWorktree != nil
		}
		return c.Retention.EndedWorktree.Duration, false
	}
	if override := c.Workspaces[root].Retention.EndedWorktree; override != nil {
		return override.Duration, true
	}
	return c.Retention.EndedWorktree.Duration, false
}

// DiscoveryMaxDepthForWorkspace は workspace root に対する探索の深さ上限と、個別指定の有無を返す。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) DiscoveryMaxDepthForWorkspace(root string) (int, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.Discovery.MaxDepth != nil {
			return *w.Discovery.MaxDepth, c.Workspaces[root].Discovery.MaxDepth != nil
		}
		return c.Discovery.MaxDepth, false
	}
	if override := c.Workspaces[root].Discovery.MaxDepth; override != nil {
		return *override, true
	}
	return c.Discovery.MaxDepth, false
}

// DiscoveryExcludeForWorkspace は workspace root に対する探索除外名と、個別指定の有無を返す。
// 個別指定は global list への追加ではなく置き換えである。
// Workspaces は NormalizePaths 済みであることを呼び出し側の契約とする。
func (c Config) DiscoveryExcludeForWorkspace(root string) ([]string, bool) {
	if c.V2() {
		w := c.WorkspaceFor(root)
		if w.Discovery.Exclude != nil {
			return cloneStrings(w.Discovery.Exclude), c.Workspaces[root].Discovery.Exclude != nil
		}
		return c.Discovery.Exclude, false
	}
	if override := c.Workspaces[root].Discovery.Exclude; override != nil {
		return override, true
	}
	return c.Discovery.Exclude, false
}

// ReadinessForRepository は repository の readiness 実効値を組で返す。消費側が mode・early_paths・timeout を常に組で使うため、個別に解決させない。
// Progress も mode などと同じく repository override を反映する。
// mainPath は NormalizePaths 済み canonical path であることを呼び出し側の契約とする。
func (c Config) ReadinessForRepository(mainPath string) Readiness {
	if c.V2() {
		resolved := c.RepositoryFor("", ".", mainPath)
		return readinessFromRepository(c, resolved)
	}
	out := c.Readiness
	override, ok := c.Repositories[mainPath]
	if !ok {
		return out
	}
	if override.Readiness.Mode != "" {
		out.Mode = override.Readiness.Mode
	}
	if override.Readiness.EarlyPaths != nil {
		out.EarlyPaths = override.Readiness.EarlyPaths
	}
	if override.Readiness.Timeout != nil {
		out.Timeout = *override.Readiness.Timeout
	}
	if override.Readiness.Progress != nil {
		out.Progress = *override.Readiness.Progress
	}
	return out
}

func readinessFromRepository(c Config, repository Repository) Readiness {
	out := c.Readiness
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
	return readinessFromRepository(c, c.RepositoryFor(workspaceRoot, relativePath, mainPath))
}

func (c Config) CopyModeForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) string {
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if c.prepareOverride.CopyMode != "" {
		return c.prepareOverride.CopyMode
	}
	if o.Storage.CopyMode != "" {
		return o.Storage.CopyMode
	}
	return c.Storage.CopyMode
}

func (c Config) COWMinSizeKiBForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) int {
	if c.prepareOverride.COWMinSizeKiB != nil {
		return *c.prepareOverride.COWMinSizeKiB
	}
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if o.COWMinSizeKiB != nil {
		return *o.COWMinSizeKiB
	}
	return c.Storage.COWMinSizeKiB
}

func (c Config) DefaultAgentRulesForWorkspaceRepository(workspaceRoot, relativePath, mainPath string) bool {
	o := c.RepositoryFor(workspaceRoot, relativePath, mainPath)
	if o.Includes.DefaultAgentRules != nil {
		return *o.Includes.DefaultAgentRules
	}
	return c.Includes.DefaultAgentRules
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
	return minDuration(c.Retention.HotStandby.Duration, c.HotStandbyOverrides())
}

// ShortestEndedWorktreeRetention は global と全 workspace 個別指定のうち最短の終了 worktree 保持期間を返す。
func (c Config) ShortestEndedWorktreeRetention() time.Duration {
	return minDuration(c.Retention.EndedWorktree.Duration, c.EndedWorktreeOverrides())
}

// MaxReadinessTimeout は global と全 repository 個別指定のうち最長の readiness timeout を返す。
// handler の上限を global だけで決めると、個別指定が長い repository の待機が handler 側で黙って切られる。
func (c Config) MaxReadinessTimeout() time.Duration {
	longest := c.Readiness.Timeout.Duration
	if c.V2() {
		if c.RepositoryDefaults.Readiness.Timeout != nil && c.RepositoryDefaults.Readiness.Timeout.Duration > longest {
			longest = c.RepositoryDefaults.Readiness.Timeout.Duration
		}
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
		return longest
	}
	for _, override := range c.Repositories {
		if override.Readiness.Timeout != nil && override.Readiness.Timeout.Duration > longest {
			longest = override.Readiness.Timeout.Duration
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
