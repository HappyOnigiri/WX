package config

import (
	"slices"
	"testing"
	"time"
)

func durationPointer(d time.Duration) *Duration { return &Duration{d} }

// workspace 個別指定はそれぞれ独立に効き、指定の無い項目は global のままになる。
func TestWorkspaceResolversPreferOverrides(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	depth := 2
	cfg.Workspaces["/tuned"] = Workspace{
		Agent:     WorkspaceAgent{AddDir: AgentAddDirWorktree},
		Retention: WorkspaceRetention{HotStandby: durationPointer(0), EndedWorktree: durationPointer(30 * time.Minute)},
		Discovery: WorkspaceDiscovery{MaxDepth: &depth, Exclude: []string{"build"}},
	}
	cfg.Workspaces["/plain"] = Workspace{WarmCount: new(int)}

	if mode, overridden := cfg.AddDirForWorkspace("/tuned"); mode != AgentAddDirWorktree || !overridden {
		t.Fatalf("add_dir=%s overridden=%v", mode, overridden)
	}
	if mode, overridden := cfg.AddDirForWorkspace("/plain"); mode != cfg.Agent.AddDir || overridden {
		t.Fatalf("add_dir=%s overridden=%v, want the global value", mode, overridden)
	}
	// 明示的な 0 は「即時回収」であって未指定ではない。
	if hot, overridden := cfg.HotStandbyForWorkspace("/tuned"); hot != 0 || !overridden {
		t.Fatalf("hot_standby=%s overridden=%v, want an explicit zero", hot, overridden)
	}
	if hot, overridden := cfg.HotStandbyForWorkspace("/plain"); hot != cfg.Retention.HotStandby.Duration || overridden {
		t.Fatalf("hot_standby=%s overridden=%v, want the global value", hot, overridden)
	}
	if ended, overridden := cfg.EndedWorktreeForWorkspace("/tuned"); ended != 30*time.Minute || !overridden {
		t.Fatalf("ended_worktree=%s overridden=%v", ended, overridden)
	}
	if got, overridden := cfg.DiscoveryMaxDepthForWorkspace("/tuned"); got != 2 || !overridden {
		t.Fatalf("max_depth=%d overridden=%v", got, overridden)
	}
	if got, overridden := cfg.DiscoveryExcludeForWorkspace("/tuned"); !overridden || !slices.Equal(got, []string{"build"}) {
		t.Fatalf("exclude=%v overridden=%v, want the override to replace the global list", got, overridden)
	}
	if got, overridden := cfg.DiscoveryExcludeForWorkspace("/plain"); overridden || !slices.Contains(got, "node_modules") {
		t.Fatalf("exclude=%v overridden=%v, want the global list", got, overridden)
	}
}

// GC の SQL へ渡す floor は最短の保持期間から作る。最長で絞ると、短い個別指定の slot が問い合わせから落ちる。
func TestRetentionFloorsUseTheShortestRetention(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Workspaces["/short"] = Workspace{Retention: WorkspaceRetention{HotStandby: durationPointer(time.Minute), EndedWorktree: durationPointer(time.Minute)}}
	cfg.Workspaces["/long"] = Workspace{Retention: WorkspaceRetention{HotStandby: durationPointer(1000 * time.Hour), EndedWorktree: durationPointer(1000 * time.Hour)}}
	if got := cfg.ShortestHotStandbyRetention(); got != time.Minute {
		t.Fatalf("hot standby floor=%s, want the shortest retention", got)
	}
	if got := cfg.ShortestEndedWorktreeRetention(); got != time.Minute {
		t.Fatalf("ended worktree floor=%s, want the shortest retention", got)
	}
	hot := cfg.HotStandbyOverrides()
	ended := cfg.EndedWorktreeOverrides()
	if len(hot) != 2 || hot["/short"] != time.Minute || len(ended) != 2 {
		t.Fatalf("hot=%v ended=%v, want both overrides copied", hot, ended)
	}
}

// handler の上限は最長の readiness timeout で決める。global だけで決めると長い個別指定が黙って切られる。
func TestMaxReadinessTimeoutCoversRepositoryOverrides(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Repositories["/slow"] = Repository{Readiness: RepositoryReadiness{Timeout: durationPointer(90 * time.Minute)}}
	if got := cfg.MaxReadinessTimeout(); got != 90*time.Minute {
		t.Fatalf("max readiness timeout=%s, want the longest repository override", got)
	}
}
