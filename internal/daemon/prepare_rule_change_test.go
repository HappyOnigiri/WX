package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// standbyPrepareJobFixture は待機用 slot を1つ作り、その PREPARE job を実行直前まで用意する。
func standbyPrepareJobFixture(t *testing.T, cfg config.Config, root, repository string) (*state.Store, *Manager, discovery.Workspace, state.Job) {
	t.Helper()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 30 * time.Second}
	t.Cleanup(m.Close)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, filepath.Join(root, "state.db"))
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	return store, m, w, jobs[0]
}

// TestPreparationKeepsPlacementHistoryWhenRulesChangeMidJob は、配置の後・配置履歴の記録の前に
// include/link の規則が変わっても PREPARE が成功し、記録が配置した実体を表すことを確認する。
// 規則の書き換えは prepare command で起こす。この区間が配置と記録の間にあたる。
func TestPreparationKeepsPlacementHistoryWhenRulesChangeMidJob(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	for name, content := range map[string]string{
		".gitignore":       "notes.txt\n.worktreeinclude\n.worktreelink\n",
		".worktreeinclude": "notes.txt\n",
		"notes.txt":        "placed by include\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repository, "add", ".gitignore")
	gitRun(t, repository, "commit", "-m", "ignore local rules")
	// prepare command は配置の後に走るので、ここで規則を copy から link へ入れ替えると
	// 記録の直前だけ規則が変わった状態を作れる。
	swap := "printf 'notes.txt\\n' > " + filepath.Join(repository, ".worktreelink") +
		" && : > " + filepath.Join(repository, ".worktreeinclude")
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: []string{"sh", "-c", swap}}}}
	store, m, w, job := standbyPrepareJobFixture(t, cfg, root, repository)
	ctx := context.Background()
	claimed, err := store.ClaimJob(ctx, job.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatalf("prepare failed after the rules changed mid job: %v", err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	slot, err := store.Slot(ctx, job.SlotID)
	if err != nil || slot.State != "READY" {
		t.Fatalf("slot=%+v err=%v", slot, err)
	}
	placements, err := store.Placements(ctx, job.SlotID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []state.Placement
	for _, placement := range placements {
		if placement.RelativePath == "notes.txt" {
			recorded = append(recorded, placement)
		}
	}
	if len(recorded) != 1 || recorded[0].Kind != "copy" {
		t.Fatalf("notes.txt placements=%+v, want a single copy", recorded)
	}
	repositories, err := store.SlotRepositories(ctx, job.SlotID)
	if err != nil || len(repositories) != 1 {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	info, err := os.Lstat(filepath.Join(repositories[0].WorktreePath, "notes.txt"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("notes.txt in the worktree=%v err=%v", info, err)
	}
	suspended, err := store.ReplenishSuspended(ctx, string(w.ID))
	if err != nil || suspended {
		t.Fatalf("replenishment suspended=%t err=%v", suspended, err)
	}
}

// TestInterruptedStandbyPreparationIsRecycled は、開始済みの二段階準備が中断した待機枠を
// 隔離せず STALE へ移し、補充を止めないことを確認する。
func TestInterruptedStandbyPreparationIsRecycled(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	store, m, w, job := standbyPrepareJobFixture(t, cfg, root, repository)
	ctx := context.Background()
	if err := store.BeginStagedPreparation(ctx, job.SlotID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimed); err != nil {
		t.Fatalf("interrupted standby preparation was not recycled: %v", err)
	}
	slot, err := store.Slot(ctx, job.SlotID)
	if err != nil || slot.State != "STALE" || slot.FailureCode != "PREPARE_INTERRUPTED" {
		t.Fatalf("slot=%+v err=%v", slot, err)
	}
	suspended, err := store.ReplenishSuspended(ctx, string(w.ID))
	if err != nil || suspended {
		t.Fatalf("replenishment suspended=%t err=%v", suspended, err)
	}
}
