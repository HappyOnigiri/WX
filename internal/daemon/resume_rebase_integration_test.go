package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	continued := resumeStoppedRebase(t, ctx, m, lease.SessionID)
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
	aborted := resumeStoppedRebase(t, ctx, m, lease.SessionID)
	runRebase(t, aborted, "--abort")
	if got := gitOutput(t, aborted, "rev-parse", "HEAD"); got != original {
		t.Fatalf("aborted HEAD=%s want %s", got, original)
	}
}

// 未解消 index を伴う停止は対象外である。従来どおり snapshot が失敗して slot が隔離され、
// 復元できるかのように見せないことを回帰として固定する。
func TestConflictedRebaseStopStillQuarantines(t *testing.T) {
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
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		artifacts, err := store.SlotArtifacts(ctx)
		if err != nil {
			return false
		}
		for _, artifact := range artifacts {
			if artifact.State == "QUARANTINED" {
				return true
			}
		}
		return false
	})
	if snaps, err := store.Snapshots(ctx, lease.SessionID); err != nil || len(snaps) != 0 {
		t.Fatalf("conflicted stop must not produce a snapshot: %+v err=%v", snaps, err)
	}
}

// resumeStoppedRebase は session を復元し、READY になった worktree の path を返す。
func resumeStoppedRebase(t *testing.T, ctx context.Context, m *Manager, sessionID string) string {
	t.Helper()
	resumed, err := m.Resume(ctx, sessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
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
