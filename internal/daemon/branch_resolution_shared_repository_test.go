package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestStandbyKeepsWorkspaceBranchAfterOtherWorkspaceRegistration は、同じ repository を含む
// 別 workspace を後から登録しても、先に登録した workspace の standby が自分の既定 branch を
// materialize し、READY の貸出でもその内容が渡ることを固定する。
// 既定 branch を repository の共有 row に置くと、branch 名だけでなく worktree の中身まで入れ替わる。
// commentlint:allow-long -- 共有 row へ戻したときの実害が worktree の内容であることを残す
func TestStandbyKeepsWorkspaceBranchAfterOtherWorkspaceRegistration(t *testing.T) {
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "hot"
		s.Config.Pool.WarmPerWorkspace = 1
	})
	ctx := context.Background()
	multi := filepath.Join(f.Root, "multi")
	repo := filepath.Join(multi, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		result, err := f.Manager.git.Run(ctx, repo, args...)
		if err != nil {
			t.Fatalf("git %v: %v stderr=%s", args, err, result.Stderr)
		}
		return strings.TrimSpace(result.Stdout)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.com"}} {
		git(args...)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "main"}, {"branch", "release"}, {"checkout", "release"}} {
		git(args...)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "release"}, {"checkout", "main"}} {
		git(args...)
	}
	releaseOID := git("rev-parse", "release")

	// workspace A は repository 自身、workspace B は同じ repository を含む親ディレクトリで、
	// それぞれ別の既定 branch を設定する。A を先に登録してから B を登録する順序が再現条件である。
	f.Config.Workspaces[string(domain.CanonicalPath(repo))] = config.Workspace{Repositories: map[string]config.Repository{".": {DefaultBranch: "release"}}}
	f.Config.Workspaces[string(domain.CanonicalPath(multi))] = config.Workspace{Repositories: map[string]config.Repository{"repo": {DefaultBranch: "main"}}}
	discoverer := discovery.Discoverer{Git: f.Manager.git, Config: f.Config}
	workspaceA, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	workspaceB, err := discoverer.Resolve(ctx, multi)
	if err != nil {
		t.Fatal(err)
	}
	workspaceA = registerTestWorkspace(t, f.Store, workspaceA)
	registerTestWorkspace(t, f.Store, workspaceB)
	storedA, err := f.Store.Workspace(ctx, string(workspaceA.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(storedA.Repositories) != 1 || storedA.Repositories[0].DefaultBranch != "release" {
		t.Fatalf("stored workspace A repositories=%+v, want default branch release", storedA.Repositories)
	}

	// 補充は hot 判定に最終貸出時刻を使うため、Hot Standby を選ばせてから実際に PREPARE を走らせる。
	raw := openTestDatabase(t, f.DatabasePath)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.ensureStandby(ctx, storedA); err != nil {
		t.Fatal(err)
	}
	jobs, err := f.Store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "PREPARE" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job, err := f.Store.ClaimJob(ctx, jobs[0].ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.runRecoveredJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.FinishJob(ctx, job.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := f.Store.ReadySlot(ctx, string(workspaceA.ID))
	if err != nil || !ok {
		t.Fatalf("ready slot=%+v ok=%v err=%v", ready, ok, err)
	}
	repository, err := f.Store.SlotRepository(ctx, ready.ID, string(storedA.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if repository.RequestedRef != "release" || repository.BaseOID != releaseOID {
		t.Fatalf("standby ref=%q oid=%q, want release %s", repository.RequestedRef, repository.BaseOID, releaseOID)
	}
	content, err := os.ReadFile(filepath.Join(repository.WorktreePath, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "release\n" {
		t.Fatalf("standby worktree content=%q, want %q", string(content), "release\n")
	}
	lease, err := f.Manager.leaseWorkspace(ctx, storedA, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != "ready" {
		t.Fatalf("lease route=%q, want ready", lease.Route)
	}
	leasedSlot, err := f.Store.Slot(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	leased, err := f.Store.SlotRepository(ctx, leasedSlot.ID, string(storedA.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(leased.WorktreePath, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "release\n" {
		t.Fatalf("leased worktree content=%q, want %q", string(content), "release\n")
	}
}
