package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestResolveBranchesUsesFetchPolicyAndFallsBackWhenOriginIsUnavailable(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	initGitRepo(t, repoPath)
	cfg := config.Defaults()
	cfg.Worktree.FetchDefaultBranch = true
	m := &Manager{cfg: cfg, git: &gitx.Runner{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := discovery.Workspace{Root: domain.CanonicalPath(repoPath), Repositories: []discovery.Repository{{
		ID: "repo", MainPath: domain.CanonicalPath(repoPath), CommonDir: domain.CanonicalPath(filepath.Join(repoPath, ".git")), RelativePath: ".", DefaultBranch: "main",
	}}}
	resolved, err := m.resolveBranches(context.Background(), w, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].OID == "" {
		t.Fatalf("resolved=%+v, want local fallback", resolved)
	}
	if explicit, _ := m.Config().FetchDefaultBranchForWorkspace(repoPath); !explicit {
		t.Fatal("fetch policy was not enabled")
	}
}

func TestLeaseUpdatesReadyStandbyToFetchedFastForward(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initRemoteRepository)
	f.manager.cfg.Worktree.FetchDefaultBranch = true
	localOID := gitOutput(t, f.repository, "rev-parse", "HEAD")
	remotePath := filepath.Join(f.root, "remote")
	if err := os.WriteFile(filepath.Join(remotePath, "tracked.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remotePath, "commit", "-am", "advance remote")
	gitRun(t, remotePath, "push", "origin", "main")
	remoteOID := gitOutput(t, remotePath, "rev-parse", "HEAD")
	lease, err := f.manager.ResolveAndLease(context.Background(), f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Route != RouteUpdate || lease.Ready {
		t.Fatalf("lease=%+v, want update route", lease)
	}
	f.runPendingJobs(t)
	updated, err := f.store.SlotRepository(context.Background(), lease.SessionID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if updated.BaseOID != remoteOID {
		t.Fatalf("updated BaseOID=%s, want remote %s", updated.BaseOID, remoteOID)
	}
	if got := gitOutput(t, f.repository, "rev-parse", "HEAD"); got != localOID {
		t.Fatalf("source HEAD=%s, want unchanged %s", got, localOID)
	}
}

func TestReconcileUpdatesReadyStandbyToFetchedFastForward(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initRemoteRepository)
	f.manager.cfg.Worktree.FetchDefaultBranch = true
	remotePath := filepath.Join(f.root, "remote")
	if err := os.WriteFile(filepath.Join(remotePath, "tracked.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, remotePath, "commit", "-am", "advance remote")
	gitRun(t, remotePath, "push", "origin", "main")
	remoteOID := gitOutput(t, remotePath, "rev-parse", "HEAD")

	f.manager.reconcileRegistry(context.Background())
	f.runPendingJobs(t)
	ready := f.readyStandby(t)
	updated, err := f.store.SlotRepository(context.Background(), ready.ID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if updated.BaseOID != remoteOID {
		t.Fatalf("reconciled BaseOID=%s, want remote %s", updated.BaseOID, remoteOID)
	}
}

func initRemoteRepository(t *testing.T, path string) {
	t.Helper()
	initGitRepo(t, path)
	parent := filepath.Dir(path)
	origin := filepath.Join(parent, "origin.git")
	gitRun(t, parent, "init", "--bare", origin)
	gitRun(t, path, "remote", "add", "origin", origin)
	gitRun(t, path, "push", "-u", "origin", "main")
	gitRun(t, parent, "clone", "--branch", "main", origin, filepath.Join(parent, "remote"))
	gitRun(t, filepath.Join(parent, "remote"), "config", "user.name", "test")
	gitRun(t, filepath.Join(parent, "remote"), "config", "user.email", "test@example.com")
}
