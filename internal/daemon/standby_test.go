package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 貸出が進行中の workspace には hot の待機枠を作る。
// 起動直後のreconcileが最初の貸出へ割り込むと、last_leased_at がまだ無いためCOLDの待機枠ができ、
// その枠を貸した session が warm にならないためである。
func TestStandbyTreatsWorkspaceWithInFlightLeaseAsHot(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	for _, tc := range []struct {
		name          string
		leaseInFlight bool
		wantCold      bool
	}{
		{name: "in-flight lease", leaseInFlight: true, wantCold: false},
		{name: "idle workspace", leaseInFlight: false, wantCold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initGitRepo(t, repo)
			cfg := config.Defaults()
			cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
			cfg.Worktree.Undefined = "hot"
			cfg.Pool.WarmPerWorkspace = 1
			cfg.Retention.HotStandby.Duration = time.Hour
			cfg.Discovery.ReconcileInterval.Duration = time.Hour
			store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			m := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer m.Close()
			ctx := context.Background()
			w, err := (&discovery.Discoverer{Git: m.git, Config: cfg}).Resolve(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			w, err = store.CanonicalWorkspace(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			w, _, err = store.UpsertWorkspaceGeneration(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			if tc.leaseInFlight {
				defer m.beginWorkspaceLease(string(w.ID))()
			}
			if err := m.ensureStandby(ctx, w); err != nil {
				t.Fatal(err)
			}
			// hotの待機枠は準備が進むため状態が動く。cold かどうかだけを見る。
			repos := standbyRepositories(t, ctx, store, string(w.ID))
			if len(repos) != 1 || (repos[0].State == "COLD") != tc.wantCold {
				t.Fatalf("standby repositories=%+v want cold=%v", repos, tc.wantCold)
			}
		})
	}
}

// standbyRepositories は workspace に1つだけ作られた待機枠の repository 行を返す。
func standbyRepositories(t *testing.T, ctx context.Context, store *state.Store, workspaceID string) []state.SlotRepository {
	t.Helper()
	artifacts, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []state.SlotRepository
	for _, artifact := range artifacts {
		slot, slotErr := store.Slot(ctx, artifact.ID)
		if slotErr != nil || slot.WorkspaceID != workspaceID {
			continue
		}
		repos, reposErr := store.SlotRepositories(ctx, artifact.ID)
		if reposErr != nil {
			t.Fatal(reposErr)
		}
		out = append(out, repos...)
	}
	return out
}
