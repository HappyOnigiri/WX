package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// Readiness.Timeoutを20msに縮めた状態で終端エラーの即時返却を確かめるため、直列で実行する。
// 並列実行の負荷では待機が先に切れ、context.DeadlineExceededに化ける。
func TestManagerReadinessAndRecoveryFailurePaths(t *testing.T) {
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Readiness.Timeout.Duration = 20 * time.Millisecond
	})
	root, store, m := f.Root, f.Store, f.Manager
	ctx := context.Background()

	lease, err := legacyLeaseFixture(m, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, "wrong"); err == nil {
		t.Fatal("wrong token passed readiness")
	}
	timed, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	if err := m.WaitReady(timed, lease.SessionID, lease.Token); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("unbound readiness error=%v", err)
	}
	cancel()

	if err := store.SetSlotState(ctx, lease.SessionID, []string{"UNBOUND"}, "QUARANTINED", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, lease.SessionID); err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatalf("quarantined archive wait error=%v", err)
	}
	if err := m.WaitReady(ctx, lease.SessionID, lease.Token); err == nil ||
		!strings.Contains(err.Error(), "failure_id=test") ||
		!strings.Contains(err.Error(), "wx status") ||
		!strings.Contains(err.Error(), "wx doctor") {
		t.Fatalf("quarantined slot readiness error missing failure id or diagnostic guidance: %v", err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, "wrong", "agent"); err == nil {
		t.Fatal("wrong token bound agent session")
	}
	if err := m.runRecoveredJob(ctx, state.Job{Kind: "UNKNOWN"}); err == nil {
		t.Fatal("unknown persistent job kind succeeded")
	}
	if err := m.removeSlotJob(ctx, state.Job{SlotID: lease.SessionID}); err == nil {
		t.Fatal("quarantined slot was removed")
	}
	if err := m.removeColdRepositoryJob(ctx, state.Job{SlotID: lease.SessionID, RepositoryID: "missing"}); err == nil {
		t.Fatal("missing repository retirement succeeded")
	}
	if _, err := m.ResumeStatus(ctx, "missing"); err == nil {
		t.Fatal("missing resume status succeeded")
	}
	if _, err := m.Resume(ctx, "missing", "codex", os.Getpid(), false); err == nil {
		t.Fatal("missing explicit resume succeeded")
	}
	if _, _, err := m.waitForSnapshot(ctx, "missing"); err == nil {
		t.Fatal("missing snapshot wait succeeded")
	}
	w := registerTestWorkspace(t, store, discovery.Workspace{Root: discoveryPath(root), Kind: "repository"})
	pending := state.Session{ID: "pending", WorkspaceID: string(w.ID), SlotID: "pending", State: "RELEASING", AgentKind: "codex", TokenHash: state.HashToken("pending")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, string(w.ID), "pending", 1, "DRAINING"), nil, pending, ""); err != nil {
		t.Fatal(err)
	}
	cancelled, cancelWaiting := context.WithCancel(ctx)
	cancelWaiting()
	if _, err := m.Resume(cancelled, "pending", "codex", os.Getpid(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pending resume error=%v", err)
	}
	expired := state.Session{ID: "expired", SlotID: "expired", State: "EXPIRED", AgentKind: "codex", TokenHash: state.HashToken("expired")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "expired", 0, "SNAPSHOTTED"), nil, expired, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, "expired"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired archive wait error=%v", err)
	}
	waiting := state.Session{ID: "waiting", SlotID: "waiting", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("waiting")}
	if _, err := store.CreateSlotSession(ctx, testSlotRow(t, m, "", "waiting", 0, "LEASED"), nil, waiting, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.waitForSnapshot(ctx, "waiting"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("archive wait timeout error=%v", err)
	}

	missing := state.Slot{ID: "missing", Path: filepath.Join(root, "does-not-exist")}
	if ok, err := m.readyMatches(ctx, missing, nil); err != nil || ok {
		t.Fatalf("missing READY root ok=%v err=%v", ok, err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "file", Path: file}, nil); err != nil || ok {
		t.Fatalf("file READY root ok=%v err=%v", ok, err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.readyMatches(ctx, state.Slot{ID: "link", Path: link}, nil); err != nil || ok {
		t.Fatalf("symlink READY root ok=%v err=%v", ok, err)
	}
}
