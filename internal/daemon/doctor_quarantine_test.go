package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 隔離した slot は自動では解けないため、doctor が問題として 1 件ずつ報告する。
func TestQuarantinedSlotFindingsReportTheQuarantinedSlot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	ctx := context.Background()
	if findings := m.quarantinedSlotFindings(ctx); len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
		t.Fatalf("findings=%+v, want a single OK finding before any quarantine", findings)
	}
	lease, err := legacyLeaseFixture(m, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	detailPath := filepath.Join(root, "update-failure.log")
	if err := store.SetSlotStateWithDetail(ctx, lease.SessionID, []string{"UNBOUND"}, "QUARANTINED", "UPDATE_FAILED", detailPath); err != nil {
		t.Fatal(err)
	}
	findings := m.quarantinedSlotFindings(ctx)
	if len(findings) != 1 || findings[0].Severity != diag.SeverityProblem {
		t.Fatalf("findings=%+v, want a single problem finding", findings)
	}
	finding := findings[0]
	if finding.Check != diag.CheckQuarantinedSlots {
		t.Fatalf("check=%q, want %q", finding.Check, diag.CheckQuarantinedSlots)
	}
	for _, want := range []string{"UPDATE_FAILED", detailPath} {
		if !strings.Contains(finding.Cause, want) {
			t.Fatalf("cause=%q, want it to keep %q", finding.Cause, want)
		}
	}
	if !strings.Contains(finding.Action, "wx clear") {
		t.Fatalf("action=%q, want it to name the command that releases the capacity", finding.Action)
	}
	if !strings.Contains(strings.Join(finding.Details, "\n"), lease.SessionID) {
		t.Fatalf("details=%v, want the slot ID", finding.Details)
	}
}

// 失敗 code も詳細ログも残っていない隔離では、無いことを原因に明示する。
func TestQuarantinedSlotFindingReportsMissingFailureRecords(t *testing.T) {
	t.Parallel()
	finding := quarantinedSlotFinding(state.QuarantinedSlot{SlotID: "slot-1", Path: "/root/slot-1"})
	if finding.Target != "/root/slot-1" {
		t.Fatalf("target=%q, want the slot path", finding.Target)
	}
	for _, want := range []string{"without a recorded failure code", "no command output was recorded"} {
		if !strings.Contains(finding.Cause, want) {
			t.Fatalf("cause=%q, want it to say %q", finding.Cause, want)
		}
	}
}
