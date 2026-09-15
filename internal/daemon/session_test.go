package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestWaitReadyIncludesPrepareDiagnosticMetadata(t *testing.T) {
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
	lease, err := legacyLeaseFixture(m, "codex", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	detailPath := filepath.Join(root, "prepare-failure.log")
	detail := "failure_id: prepare-diagnostic-id\nexit_code: 17\ntimed_out: false\ncanceled: false\nstderr:\n[stderr] unique prepare cause\n"
	if err := os.WriteFile(detailPath, []byte(detail), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotStateWithDetail(context.Background(), lease.SessionID, []string{"UNBOUND"}, "FAILED", "PREPARE_FAILED", detailPath); err != nil {
		t.Fatal(err)
	}
	err = m.WaitReady(context.Background(), lease.SessionID, lease.Token)
	for _, want := range []string{"failure_id=prepare-diagnostic-id", "detail_path=" + detailPath, "exit_code=17", "timed_out=false", "canceled=false"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("readiness error=%v missing %q", err, want)
		}
	}
}

func TestReadPrepareDiagnosticAcceptsGitExitStatus(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "git-failure.log")
	if err := os.WriteFile(path, []byte("exit_status: 128\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := readPrepareDiagnostic(path)
	if !metadata.HasExitCode || metadata.ExitCode != 128 {
		t.Fatalf("prepare diagnostic=%+v, want exit_status 128", metadata)
	}
}

// 失敗 code ごとに、readiness エラーへ載る marker が client の分岐と一致することを確かめる。
// 空文字は marker を付けない code である。
func TestWaitReadyMarksSlotFailuresForTheClient(t *testing.T) {
	t.Parallel()
	for code, wantMarker := range map[string]string{
		"RESTORE_FAILED": RecoveryUnavailableMarker,
		"UPDATE_FAILED":  ColdStartRetryableMarker,
		"PREPARE_FAILED": "",
	} {
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
		for _, marker := range []string{RecoveryUnavailableMarker, ColdStartRetryableMarker} {
			if got := strings.Contains(err.Error(), marker); got != (marker == wantMarker) {
				t.Fatalf("%s readiness error=%v contains %q=%t, want marker %q", code, err, marker, got, wantMarker)
			}
		}
	}
}
