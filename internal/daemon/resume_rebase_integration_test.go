package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// OSS 検証と同じ経路を固定する。`rebase -i` の edit 停止を正常返却して resume したとき、
// 復元先で `git rebase --continue` が完走し、rebase 後の全 commit が揃うことを確認する。
func TestResumeContinuesStoppedInteractiveRebase(t *testing.T) {
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
	commitTrackedFile(t, repo, "second.txt", "2\n")
	commitTrackedFile(t, repo, "third.txt", "3\n")
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	stopRebaseAtEdit(t, lease.Path)
	stopped := gitOutput(t, lease.Path, "rev-parse", "HEAD")
	original := gitOutput(t, lease.Path, "rev-parse", "ORIG_HEAD")
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	snapshots, err := store.Snapshots(ctx, lease.SessionID)
	if err != nil || snapshots[0].GitStateOID == "" {
		t.Fatalf("snapshot did not record the stopped rebase: %+v err=%v", snapshots, err)
	}
	continued := resumeStoppedOperation(t, ctx, m, lease.SessionID)
	if got := gitOutput(t, continued, "rev-parse", "HEAD"); got != stopped {
		t.Fatalf("restored HEAD=%s want the stopped commit %s", got, stopped)
	}
	runRebase(t, continued, "--continue")
	if _, err := os.Lstat(filepath.Join(continued, ".git")); err != nil {
		t.Fatal(err)
	}
	log := gitOutput(t, continued, "log", "--format=%s", "-3")
	if log != "third.txt\nsecond.txt\ninitial" {
		t.Fatalf("rebase did not replay every commit:\n%s", log)
	}
	if status := gitOutput(t, continued, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("continued rebase left changes:\n%s", status)
	}
	// --abort も同じ停止地点から動く。orig-head へ戻れなければ、利用者は続行も中止もできない状態に置かれる。
	aborted := resumeStoppedOperation(t, ctx, m, lease.SessionID)
	runRebase(t, aborted, "--abort")
	if got := gitOutput(t, aborted, "rev-parse", "HEAD"); got != original {
		t.Fatalf("aborted HEAD=%s want %s", got, original)
	}
}

// 未解消 index を伴う rebase 停止も snapshot・resume で stage と制御ファイルを保ち、
// 復元先で解消して rebase を続行できることを固定する。
func TestConflictedRebaseStopResumesAndContinues(t *testing.T) {
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
	commitTrackedFile(t, repo, "tracked.txt", "theirs\n")
	gitRun(t, repo, "checkout", "-q", "-b", "side", "HEAD~1")
	commitTrackedFile(t, repo, "tracked.txt", "ours\n")
	gitRun(t, repo, "checkout", "-q", "main")
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, []string{"side"}, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if output, err := runGit(lease.Path, nil, "rebase", "main"); err == nil {
		t.Fatalf("rebase was expected to stop on a conflict:\n%s", output)
	}
	wantStages := gitOutput(t, lease.Path, "ls-files", "--stage")
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	snapshots, err := store.Snapshots(ctx, lease.SessionID)
	if err != nil || len(snapshots) != 1 || snapshots[0].ConflictOID == "" || snapshots[0].GitStateOID == "" {
		t.Fatalf("conflicted stop did not preserve conflict state: %+v err=%v", snapshots, err)
	}
	resumed := resumeStoppedOperation(t, ctx, m, lease.SessionID)
	if got := gitOutput(t, resumed, "ls-files", "--stage"); got != wantStages {
		t.Fatalf("restored index stages=%q want %q", got, wantStages)
	}
	if status := gitOutput(t, resumed, "status", "--porcelain=v1"); status == "" {
		t.Fatal("restored conflicted rebase lost its conflict status")
	}
	if err := os.WriteFile(filepath.Join(resumed, "tracked.txt"), []byte("resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, resumed, "add", "tracked.txt")
	runRebase(t, resumed, "--continue")
	if status := gitOutput(t, resumed, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("continued conflicted rebase left changes: %s", status)
	}
}

// merge の未解消 index も同じ artifact で保存し、復元後に merge commit と abort の両方を可能にする。
func TestConflictedMergeResumesAndCommits(t *testing.T) {
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
	gitRun(t, repo, "checkout", "-q", "-b", "side")
	commitTrackedFile(t, repo, "tracked.txt", "ours\n")
	gitRun(t, repo, "checkout", "-q", "main")
	commitTrackedFile(t, repo, "tracked.txt", "theirs\n")
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repo, []string{"side"}, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	mergeHead := gitOutput(t, lease.Path, "rev-parse", "HEAD")
	if output, err := runGit(lease.Path, nil, "merge", "main"); err == nil {
		t.Fatalf("merge was expected to stop on a conflict:\n%s", output)
	}
	wantStages := gitOutput(t, lease.Path, "ls-files", "--stage")
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool { snaps, _ := store.Snapshots(ctx, lease.SessionID); return len(snaps) == 1 })
	snapshots, err := store.Snapshots(ctx, lease.SessionID)
	if err != nil || len(snapshots) != 1 || snapshots[0].ConflictOID == "" {
		t.Fatalf("conflicted merge was not snapshotted: %+v err=%v", snapshots, err)
	}
	resumed := resumeStoppedOperation(t, ctx, m, lease.SessionID)
	if got := gitOutput(t, resumed, "ls-files", "--stage"); got != wantStages {
		t.Fatalf("restored merge stages=%q want %q", got, wantStages)
	}
	if got := gitOutput(t, resumed, "rev-parse", "HEAD"); got != mergeHead {
		t.Fatalf("restored merge HEAD=%s want %s", got, mergeHead)
	}
	if err := os.WriteFile(filepath.Join(resumed, "tracked.txt"), []byte("resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, resumed, "add", "tracked.txt")
	gitRun(t, resumed, "commit", "-m", "merge resolution")
	parents := strings.Fields(gitOutput(t, resumed, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("resolved merge commit parents=%v", parents)
	}
	if status := gitOutput(t, resumed, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("resolved merge left changes: %s", status)
	}
	aborted := resumeStoppedOperation(t, ctx, m, lease.SessionID)
	gitRun(t, aborted, "merge", "--abort")
	if got := gitOutput(t, aborted, "rev-parse", "HEAD"); got != mergeHead {
		t.Fatalf("aborted merge HEAD=%s want %s", got, mergeHead)
	}
}

// resumeStoppedOperation は session を復元し、READY になった worktree の path を返す。
func resumeStoppedOperation(t *testing.T, ctx context.Context, m *Manager, sessionID string) string {
	t.Helper()
	resumed, err := m.Resume(ctx, sessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Release(context.Background(), resumed.SessionID, resumed.Token, "test"); err != nil {
			t.Errorf("release resumed worktree: %v", err)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			session, err := m.store.SessionByID(context.Background(), resumed.SessionID)
			if err == nil && session.State == "ARCHIVED" {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("resumed worktree was not archived: state=%q error=%v", session.State, err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	if err := waitReady(ctx, m, 30*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	return resumed.Path
}

func runRebase(t *testing.T, dir string, args ...string) {
	t.Helper()
	output, err := runGit(dir, []string{"GIT_EDITOR=true"}, append([]string{"rebase"}, args...)...)
	if err != nil {
		t.Fatalf("git rebase %v: %v\n%s", args, err, output)
	}
}

func runGit(dir string, env []string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func commitTrackedFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-m", name)
}

// stopRebaseAtEdit は先頭の pick を edit に書き換えた `rebase -i HEAD~2` を走らせ、1 件目の replay 直後で停止させる。
func stopRebaseAtEdit(t *testing.T, dir string) {
	t.Helper()
	editor := filepath.Join(t.TempDir(), "sequence-editor")
	script := "#!/bin/sh\nawk 'NR==1{sub(/^pick/,\"edit\")}1' \"$1\" > \"$1.wx\" && mv \"$1.wx\" \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := runGit(dir, []string{"GIT_SEQUENCE_EDITOR=" + editor}, "rebase", "-i", "HEAD~2"); err != nil {
		t.Fatalf("start interactive rebase: %v\n%s", err, output)
	}
}
