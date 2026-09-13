package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
)

// 保存できない作業を子に残したまま返した slot は、保持期限を過ぎても GC が消さない。
// 出口は利用者が明示する `wx clear --discard` だけで、そこでは従来どおり削除する。
func TestEndedWorktreeWithUnsavedSubmoduleWorkSurvivesGC(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		s.Config.Readiness.Timeout.Duration = 60 * time.Second
	})
	store, m := f.Store, f.Manager
	repository := filepath.Join(f.Root, "repo")
	initGitRepoWithSubmodule(t, repository)
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 60*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatalf("wait for the cold start: %v", err)
	}
	// 未解消 index の子は capsule へ保存できない。返却後に実体が消えると復元経路が無くなる作業である。
	conflictInSubmodule(t, lease.Path)
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	protected, err := store.ProtectedSlots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(protected) != 1 || len(protected[0].Submodules) != 1 || protected[0].Submodules[0].Path != daemonSubmodulePath {
		t.Fatalf("protected slots=%+v, want the submodule that holds the untracked file", protected)
	}
	slotID := protected[0].SlotID
	result, err := m.GC(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pending == 0 || !strings.Contains(gcReasonFor(result.Reasons, "worktree "+slotID), "--discard") {
		t.Fatalf("gc result=%+v, want the protected worktree reported as pending", result)
	}
	if _, err := os.Stat(lease.Path); err != nil {
		t.Fatalf("gc removed a worktree that holds unsaved submodule work: %v", err)
	}
	if slot, err := store.Slot(ctx, slotID); err != nil || slot.State != "SNAPSHOTTED" {
		t.Fatalf("slot=%+v err=%v, want it left as SNAPSHOTTED", slot, err)
	}
	if !hasFinding(m.Doctor(ctx).Findings, "submodule work that its recovery snapshot does not contain", diag.SeverityProblem) {
		t.Fatal("wx doctor did not report the protected slot")
	}
	// --discard 無しの clear は理由つきで残す。保存できない作業を黙って消さないためである。
	dry, err := m.Clean(ctx, CleanRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	target := targetByID(replyTargets(t, dry), slotID)
	if target.State != cleanTargetSkipped || !strings.Contains(target.Reason, "--discard") {
		t.Fatalf("clear target without --discard=%+v", target)
	}
	if _, err := m.Clean(ctx, CleanRequest{Discard: true}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		slot, _ := store.Slot(ctx, slotID)
		_, statErr := os.Stat(lease.Path)
		return slot.State == "ARCHIVED" && os.IsNotExist(statErr)
	})
	remaining, err := store.ProtectedSlots(ctx)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("protected slots after the explicit deletion=%+v err=%v", remaining, err)
	}
}

func gcReasonFor(reasons []GCReason, target string) string {
	for _, reason := range reasons {
		if reason.Target == target {
			return reason.Reason
		}
	}
	return ""
}

// 子の未追跡 file・未 push commit は親と同じ snapshot・resume 契約で戻り、slot は保護されない。
func TestResumeRestoresSubmoduleWork(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
		s.Config.Readiness.Timeout.Duration = 60 * time.Second
	})
	store, m := f.Store, f.Manager
	repository := filepath.Join(f.Root, "repo")
	initGitRepoWithSubmodule(t, repository)
	ctx := context.Background()
	lease, err := m.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 60*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatalf("wait for the cold start: %v", err)
	}
	child := filepath.Join(lease.Path, daemonSubmodulePath)
	writeSubmoduleFile(t, child, "child work\n")
	gitRun(t, child, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-am", "child work")
	if err := os.WriteFile(filepath.Join(child, "scratch.txt"), []byte("note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantHead := gitOutput(t, child, "rev-parse", "HEAD")
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	protected, err := store.ProtectedSlots(ctx)
	if err != nil || len(protected) != 0 {
		t.Fatalf("protected slots=%+v err=%v, want none once the submodule is saved", protected, err)
	}
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 60*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatalf("wait for the resume: %v", err)
	}
	restored := filepath.Join(resumed.Path, daemonSubmodulePath)
	if got := gitOutput(t, restored, "rev-parse", "HEAD"); got != wantHead {
		t.Fatalf("restored submodule HEAD=%s, want %s", got, wantHead)
	}
	if data, err := os.ReadFile(filepath.Join(restored, "tracked.txt")); err != nil || string(data) != "child work\n" {
		t.Fatalf("restored submodule content=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(restored, "scratch.txt")); err != nil || string(data) != "note\n" {
		t.Fatalf("restored submodule untracked file=%q err=%v", data, err)
	}
}
