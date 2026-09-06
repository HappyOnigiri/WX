package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRecoveryUnavailableCoversRestoreFailuresOnly(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]bool{
		"RESTORE_FAILED":                true,
		"RESTORE_FAILED:prepare-id":     true,
		"RESTORE_AMBIGUOUS":             true,
		"SNAPSHOT_INCOMPLETE":           true,
		"SNAPSHOT_UNAVAILABLE":          true,
		"PREPARE_FAILED":                false,
		"PREPARE_FAILED:RESTORE_FAILED": false,
		"JOB_RETRY_EXHAUSTED":           false,
		"WORKTREE_OWNERSHIP_UNCERTAIN":  false,
		"":                              false,
	} {
		if got := recoveryUnavailable(code); got != want {
			t.Errorf("recoveryUnavailable(%q)=%t, want %t", code, got, want)
		}
	}
}

func TestIsRecoveryUnavailableReadsTheMarker(t *testing.T) {
	t.Parallel()
	if IsRecoveryUnavailable(nil) {
		t.Fatal("a missing error must not report an unavailable recovery")
	}
	if IsRecoveryUnavailable(errors.New("workspace readiness failed: state=FAILED failure_id=PREPARE_FAILED")) {
		t.Fatal("a prepare failure must not report an unavailable recovery")
	}
	if !IsRecoveryUnavailable(errors.New("readiness failed " + RecoveryUnavailableMarker + " detail_path=unavailable")) {
		t.Fatal("the marker must be recognized inside a wrapped RPC message")
	}
}

func TestWaitReadyMarksRestoreFailuresAsUnavailableRecovery(t *testing.T) {
	t.Parallel()
	for code, wantMarker := range map[string]bool{"RESTORE_FAILED": true, "PREPARE_FAILED": false} {
		root := t.TempDir()
		store, err := state.Open(filepath.Join(root, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		cfg := config.Defaults()
		cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
		m := testManager(t, cfg, store)
		t.Cleanup(m.Close)
		lease, err := legacyLeaseFixture(m, "codex", os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetSlotState(context.Background(), lease.SessionID, []string{"UNBOUND"}, "QUARANTINED", code); err != nil {
			t.Fatal(err)
		}
		err = m.WaitReady(context.Background(), lease.SessionID, lease.Token)
		if err == nil {
			t.Fatalf("%s slot passed readiness", code)
		}
		if got := strings.Contains(err.Error(), RecoveryUnavailableMarker); got != wantMarker {
			t.Fatalf("%s readiness error=%v marker=%t, want %t", code, err, got, wantMarker)
		}
	}
}
