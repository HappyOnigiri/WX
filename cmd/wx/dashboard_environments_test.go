package main

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

// TestMarkDashboardRegistrationsKeepsConfiguredWorkspacesOnly は環境一覧の対象を
// 設定ファイルに書かれた workspace だけに保つことを固定する。貸出のたびに登録される
// daemon 側の workspace を並べると、一時ディレクトリや実体の消えた登録まで設定対象として現れる。
func TestMarkDashboardRegistrationsKeepsConfiguredWorkspacesOnly(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultsV2()
	cfg.Workspaces = map[string]config.Workspace{
		"/configured/multi":  {Worktree: "hot"},
		"/configured/absent": {Worktree: "cold"},
	}
	payload := map[string]any{"workspace_details": []any{
		map[string]any{"root": "/configured/multi", "repositories": []any{
			map[string]any{"relative_path": "app"},
			map[string]any{"relative_path": "infra"},
		}},
		map[string]any{"root": "/leased/scratch"},
	}}
	markDashboardRegistrations(&cfg, payload)
	if len(cfg.Workspaces) != 2 {
		t.Fatalf("workspaces=%v, want only the configured roots", cfg.Workspaces)
	}
	if _, registered := cfg.Workspaces["/leased/scratch"]; registered {
		t.Fatal("a workspace without configuration was added to the environment list")
	}
	multi := cfg.Workspaces["/configured/multi"]
	if !multi.Discovered {
		t.Fatal("the configured workspace was not marked as discovered")
	}
	// membership は個別設定を書くまで設定ファイルに現れないため、設定済み workspace の配下だけは daemon の一覧から補う。
	if len(multi.Repositories) != 2 || !multi.Repositories["app"].Discovered || !multi.Repositories["infra"].Discovered {
		t.Fatalf("repositories=%+v, want both memberships marked as discovered", multi.Repositories)
	}
	if absent := cfg.Workspaces["/configured/absent"]; absent.Discovered || absent.Repositories != nil {
		t.Fatalf("workspace=%+v, want an unregistered configuration left untouched", absent)
	}
}
