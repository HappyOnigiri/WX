package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 補充計画（ENSURE_STANDBY）の失敗は停止行を残さないため、待機枠が足りている間だけ報告を消す。
// 枠が欠けたままなら `wx status` と `wx doctor` の両方へ失敗理由まで出す。
func TestStandbyPlanFailureIsReportedWhileTheWarmSlotsAreMissing(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 1
	cfg.Workspaces = map[string]config.Workspace{repo: {Worktree: "hot"}}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	rootID, err := tryRegisterTestRoot(m, filepath.Join(root, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	database := openTestDatabase(t, filepath.Join(root, "state.db"))
	if _, err := database.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at)
		VALUES('workspace',?,'repository',1,'READY',datetime('now'),datetime('now'),datetime('now'))`, repo); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(ctx, "ENSURE_STANDBY", "workspace", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, job.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJobWithDetail(ctx, job.ID, "test",
		errors.New(`unsafe .worktreeinclude pattern "../escape"`), "JOB_FAILED", "/logs/ensure.log"); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reported, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic)
	if !ok || len(reported) != 1 || reported[0].Reason != state.StandbyReplenishReasonPlanFailure || reported[0].Detail != job.ID {
		t.Fatalf("standby diagnostics=%v", status["standby_replenishment"])
	}
	if !strings.Contains(reported[0].Action, "wx retry-standby") || reported[0].FailedAt == "" {
		t.Fatalf("standby diagnostic=%+v", reported[0])
	}
	finding := doctorProblem(t, m.Doctor(ctx), diag.CheckStandbyReplenishment)
	for _, fragment := range []string{job.ID, "unsafe .worktreeinclude pattern", "/logs/ensure.log"} {
		if !strings.Contains(finding.Cause, fragment) {
			t.Fatalf("doctor standby cause=%q, want %q", finding.Cause, fragment)
		}
	}
	// 枠が埋まれば同じ FAILED 行は残っていても報告しない。計画の失敗は補充を止めないためである。
	reserved, err := store.ReserveStandbyIfNeeded(ctx, state.Slot{
		ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: rootID, RelPath: "workspace/standby",
	}, cfg.Pool.WarmPerWorkspace)
	if err != nil || !reserved {
		t.Fatalf("reserve standby=%v err=%v", reserved, err)
	}
	status, err = m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reported, ok := status["standby_replenishment"].([]state.StandbyReplenishmentDiagnostic); !ok || len(reported) != 0 {
		t.Fatalf("standby diagnostics once the warm slot exists=%v", status["standby_replenishment"])
	}
	for _, finding := range doctorFindings(m.Doctor(ctx), diag.CheckStandbyReplenishment) {
		if finding.Severity != diag.SeverityOK {
			t.Fatalf("doctor standby finding once the warm slot exists=%+v", finding)
		}
	}
}
