package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestEnsureStandbyOnlyChecksOutRecentlyUsedRepositories(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
	})
	store, m := f.Store, f.Manager
	m.git.SetTimeout(10 * time.Second)
	root := f.Root
	hotRepoPath := filepath.Join(root, "hot")
	coldRepoPath := filepath.Join(root, "cold")
	initGitRepo(t, hotRepoPath)
	initGitRepo(t, coldRepoPath)
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "hot", MainPath: discoveryPath(hotRepoPath), CommonDir: discoveryPath(filepath.Join(hotRepoPath, ".git")), RelativePath: "hot", DefaultBranch: "main"},
		{ID: "cold", MainPath: discoveryPath(coldRepoPath), CommonDir: discoveryPath(filepath.Join(coldRepoPath, ".git")), RelativePath: "cold", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, f.DatabasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id=?`, state.FormatTime(time.Now()), "hot"); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("standby jobs=%+v err=%v", jobs, err)
	}
	prepared, err := store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepared.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	hotRepository, err := store.SlotRepository(ctx, ready.ID, "hot")
	if err != nil || hotRepository.State != "READY" {
		t.Fatalf("recently-used repository was not checked out: %+v err=%v", hotRepository, err)
	}
	if _, statErr := os.Stat(hotRepository.WorktreePath); statErr != nil {
		t.Fatalf("recently-used repository worktree is missing: %v", statErr)
	}
	coldRepository, err := store.SlotRepository(ctx, ready.ID, "cold")
	if err != nil || coldRepository.State != "COLD" {
		t.Fatalf("never-leased repository was checked out early: %+v err=%v", coldRepository, err)
	}
	if _, statErr := os.Stat(coldRepository.WorktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("never-leased repository worktree was materialized: err=%v", statErr)
	}
}
