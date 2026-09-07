package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// Readiness.Timeoutを30msに縮めた終端状態の検査を含むため、直列で実行する。
// 理由はTestManagerReadinessAndRecoveryFailurePathsと同じである。
func TestWaitForSnapshotReportsTerminalAndTimeoutStates(t *testing.T) {
	newFixture := func(t *testing.T, sessionState, slotState string) (*Manager, *state.Store, state.Session) {
		t.Helper()
		f := manualManagerFixture(t, func(s *managerFixtureSetup) {
			s.Config.Readiness.Timeout.Duration = 30 * time.Millisecond
		})
		root, cfg, store, manager := f.Root, f.Config, f.Store, f.Manager
		workspaceRecord := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "repository"}
		workspaceRecord = registerTestWorkspace(t, store, workspaceRecord)
		session := state.Session{ID: "session", WorkspaceID: string(workspaceRecord.ID), SlotID: "slot", State: sessionState, AgentKind: "codex", TokenHash: state.HashToken("token")}
		if _, err := store.CreateSlotSession(context.Background(), slotAtPath(t, manager, string(workspaceRecord.ID), "slot", filepath.Join(cfg.Storage.WorktreeRoot, "slot"), 0, slotState), nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return manager, store, session
	}

	t.Run("missing session", func(t *testing.T) {
		manager, _, _ := newFixture(t, "ACTIVE", "LEASED")
		if _, _, err := manager.waitForSnapshot(context.Background(), "missing"); err == nil {
			t.Fatal("missing session wait succeeded")
		}
	})
	t.Run("expired session", func(t *testing.T) {
		manager, _, session := newFixture(t, "EXPIRED", "ARCHIVED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired wait error=%v", err)
		}
	})
	t.Run("failed slot", func(t *testing.T) {
		manager, _, session := newFixture(t, "RELEASING", "FAILED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "FAILED") {
			t.Fatalf("failed slot wait error=%v", err)
		}
	})
	t.Run("archived without membership", func(t *testing.T) {
		manager, _, session := newFixture(t, "ARCHIVED", "SNAPSHOTTED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); err == nil || !strings.Contains(err.Error(), "no recorded repository") {
			t.Fatalf("membership wait error=%v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		manager, _, session := newFixture(t, "ACTIVE", "LEASED")
		if _, _, err := manager.waitForSnapshot(context.Background(), session.ID); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline wait error=%v", err)
		}
	})
}
