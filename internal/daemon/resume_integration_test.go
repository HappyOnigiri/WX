package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLeaseArchiveAndRestorePreservesGitState(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	store, m := f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("shared\nlocal.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte("local.cfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared", "data"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "local.cfg"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore", ".worktreeinclude", ".worktreelink")
	gitRun(t, repo, "commit", "-m", "worktree metadata")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("worktree branch=%q", got)
	}
	data, err := os.ReadFile(filepath.Join(lease.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base\n" {
		t.Fatalf("main dirty content leaked: %q", data)
	}
	if info, err := os.Lstat(filepath.Join(lease.Path, "shared")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("shared is not a symlink: %v %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(lease.Path, "local.cfg")); err != nil || string(data) != "local\n" {
		t.Fatalf("include copy=%q err=%v", data, err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, lease.Path, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	native, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, native.SessionID, native.Token); err != nil {
		details, detailsErr := store.StatusDiagnostics(ctx)
		t.Fatalf("wait for native resume: %v; diagnostics=%+v diagnostics_error=%v", err, details, detailsErr)
	}
	nativeWorktree := boundWorktreePath(t, store, native.SessionID)
	if status := gitOutput(t, nativeWorktree, "status", "--porcelain"); !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("native restored status:\n%s", status)
	}
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Path == lease.Path {
		t.Fatal("resume reused old physical path")
	}
	if err := waitReady(ctx, m, 30*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	status := gitOutput(t, resumed.Path, "status", "--porcelain")
	if !strings.Contains(status, "MM tracked.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Fatalf("restored status:\n%s", status)
	}
	data, err = os.ReadFile(filepath.Join(resumed.Path, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "working\n" {
		t.Fatalf("working content=%q", data)
	}
	data, err = os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dirty main\n" {
		t.Fatalf("source main changed: %q", data)
	}
	statusResult, err := m.ResumeStatus(ctx, lease.SessionID)
	if err != nil || statusResult["expired"] != false {
		t.Fatalf("snapshot must remain usable: %v %v", statusResult, err)
	}
	fresh, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), true, ResumeOptions{Branches: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, fresh.SessionID, fresh.Token); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(fresh.Path, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("fresh workspace restored snapshot content: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Path, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("snapshot-only file in fresh workspace: %v", err)
	}
}

func TestNativeResumeWaitsForInFlightSnapshot(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Pool.PreparationConcurrency = 2
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	store, m := f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 15*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "in-flight-agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "pending.txt"), []byte("recover me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	native, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 15*time.Second, native.SessionID, native.Token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(boundWorktreePath(t, store, native.SessionID), "pending.txt"))
	if err != nil || string(data) != "recover me\n" {
		t.Fatalf("restored pending file=%q err=%v", data, err)
	}
}

func TestExpiredExplicitResumeRequiresOptInAndUsesCurrentBase(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Retention.RecoverySnapshot.Duration = time.Millisecond
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	store, m := f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "expired-agent-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), true); err == nil {
		t.Fatal("fresh resume accepted an active parent")
	}

	if err := os.WriteFile(filepath.Join(lease.Path, "uncommitted.txt"), []byte("discarded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	time.Sleep(5 * time.Millisecond)
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		return slot.State == "ARCHIVED"
	})
	if _, err := m.GC(ctx, false); err != nil {
		t.Fatal(err)
	}
	status, err := m.ResumeStatus(ctx, lease.SessionID)
	if err != nil || status["expired"] != true {
		t.Fatalf("resume status=%v err=%v", status, err)
	}
	if _, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false); err == nil {
		t.Fatal("expired resume proceeded without confirmation")
	}
	fresh, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, fresh.SessionID, fresh.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Path, "uncommitted.txt")); !os.IsNotExist(err) {
		t.Fatalf("expired local state leaked into fresh workspace: %v", err)
	}
	if got := gitOutput(t, fresh.Path, "rev-parse", "HEAD"); got != gitOutput(t, repo, "rev-parse", "refs/heads/main") {
		t.Fatalf("fresh base=%s main=%s", got, gitOutput(t, repo, "rev-parse", "refs/heads/main"))
	}
	nativeFresh, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), true)
	if err != nil {
		t.Fatal(err)
	}
	nativeWait, nativeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer nativeCancel()
	if err := m.WaitReady(nativeWait, nativeFresh.SessionID, nativeFresh.Token); err != nil {
		t.Fatalf("native --fresh workspace did not become ready: %v", err)
	}
	if got := gitOutput(t, boundWorktreePath(t, store, nativeFresh.SessionID), "rev-parse", "HEAD"); got != gitOutput(t, repo, "rev-parse", "refs/heads/main") {
		t.Fatalf("native fresh base=%s main=%s", got, gitOutput(t, repo, "rev-parse", "refs/heads/main"))
	}
}

// TestResumeLeavesSkipWorktreePathsToTheHook は、post-checkout hook が tracked file を個人版へ置き換えて
// skip-worktree を付ける repository でも、返却と resume が成功して slot を隔離しないことを検証する。
// flag 付き path は snapshot の対象外なので、復元先の内容は hook が置いた個人版のままで flag も残る。
func TestResumeLeavesSkipWorktreePathsToTheHook(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	store, m := f.Store, f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	hook := "#!/bin/sh\nprintf 'personal\\n' > tracked.txt\ngit update-index --skip-worktree tracked.txt\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if listing := gitOutput(t, lease.Path, "ls-files", "-v", "tracked.txt"); listing != "S tracked.txt" {
		t.Fatalf("post-checkout hook did not blind the leased worktree: %q", listing)
	}
	// flag 付き path への編集は契約どおり引き継がれない。ここでは復元先が hook の個人版に戻ることを確かめる。
	if err := os.WriteFile(filepath.Join(lease.Path, "tracked.txt"), []byte("session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := gitOutput(t, lease.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("skip-worktree fixture does not blind git status: %q", status)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	worktree := boundWorktreePath(t, store, resumed.SessionID)
	if data, err := os.ReadFile(filepath.Join(worktree, "tracked.txt")); err != nil || string(data) != "personal\n" {
		t.Fatalf("restored tracked.txt=%q err=%v, want %q", data, err, "personal\n")
	}
	if listing := gitOutput(t, worktree, "ls-files", "-v", "tracked.txt"); listing != "S tracked.txt" {
		t.Fatalf("skip-worktree was not reinstated after resume: %q", listing)
	}
}
