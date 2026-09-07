package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestOrphanReconciliationWaitsForRegisteredAgentProcess(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	cfg, store, m := f.Config, f.Store, f.Manager
	ctx := context.Background()
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "slot")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, "", "agent", slotPath, 0, "LEASED"), nil, state.Session{ID: "agent", SlotID: "agent", State: "ACTIVE", AgentKind: "codex", ClientPID: 99999999, TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	agent := exec.Command("sleep", "30")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if agent.Process != nil {
			_ = agent.Process.Kill()
		}
		_ = agent.Wait()
	})
	if err := m.RegisterAgentProcess(ctx, "agent", "token", agent.Process.Pid); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET last_heartbeat_at=? WHERE id='agent'`, state.FormatTime(time.Now().Add(-time.Minute))); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	m.reconcileOrphans(ctx)
	session, err := store.SessionByID(ctx, "agent")
	if err != nil || session.State != "ACTIVE" {
		t.Fatalf("live agent was released: session=%+v err=%v", session, err)
	}
	var pending dependencyPendingError
	if err := m.snapshotSession(ctx, session); !errors.As(err, &pending) {
		t.Fatalf("snapshot did not wait for live agent: %v", err)
	}
	if err := m.removeSlotJob(ctx, state.Job{SessionID: "agent"}); !errors.As(err, &pending) {
		t.Fatalf("removal did not wait for live agent: %v", err)
	}
	if err := m.removeSlotJob(ctx, state.Job{SessionID: "missing"}); err == nil {
		t.Fatal("removal with a missing session identity succeeded")
	}
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := agent.Wait(); err == nil {
		t.Fatal("killed agent exited successfully")
	}
	agent.Process = nil
	m.reconcileOrphans(ctx)
	session, err = store.SessionByID(ctx, "agent")
	if err != nil || session.State != "RELEASING" {
		t.Fatalf("dead agent was not released: session=%+v err=%v", session, err)
	}
}

// 隔離 slot を owner に持つ session の返却は候補から消えるまで一度で収束する。
// 修正前は slot を DRAINING へ進められず、10 分ごとに同じ解放失敗を記録し続けていた。
func TestOrphanReconcileConvergesForQuarantinedSlotOwner(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	cfg, store, m := f.Config, f.Store, f.Manager
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, "quarantined")
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, "", "quarantined", slotPath, 0, "PREPARING"), nil,
		state.Session{ID: "quarantined", SlotID: "quarantined", State: "ACTIVE", AgentKind: "codex", ClientPID: 99999999, TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotState(ctx, "quarantined", []string{"PREPARING"}, "QUARANTINED", "JOB_RETRY_EXHAUSTED"); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE sessions SET last_heartbeat_at=? WHERE id='quarantined'`, state.FormatTime(time.Now().Add(-time.Minute))); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	m.reconcileOrphans(ctx)
	m.reconcileOrphans(ctx)
	session, err := store.SessionByID(ctx, "quarantined")
	if err != nil || session.State != "EXPIRED" {
		t.Fatalf("session owning a quarantined slot=%+v err=%v", session, err)
	}
	slot, err := store.Slot(ctx, "quarantined")
	if err != nil || slot.State != "QUARANTINED" || slot.OwnerSessionID != "" {
		t.Fatalf("quarantined slot after release=%+v err=%v", slot, err)
	}
	if strings.Contains(logs.String(), "orphan release failed") {
		t.Fatalf("orphan release kept failing: %s", logs.String())
	}
	// 復旧 snapshot を作らない終端なので、成功として黙って閉じずに一度だけ記録する。
	if got := strings.Count(logs.String(), "session expired without a recovery snapshot"); got != 1 {
		t.Fatalf("snapshotless termination records=%d, want one: %s", got, logs.String())
	}
}
