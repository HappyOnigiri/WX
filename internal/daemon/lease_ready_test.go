package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// leaseTwoRepositoryFixture は repository 2 件の multi_repository workspace を用意する。
// readyMatches の repository ループを 2 件以上で踏むための最小構成で、slot は呼び出し側が作る。
func leaseTwoRepositoryFixture(t *testing.T) (context.Context, *Manager, *state.Store, discovery.Workspace, []pool.Resolved) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	initGitRepo(t, filepath.Join(bundle, "server"))
	initGitRepo(t, filepath.Join(bundle, "client"))
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	runner := &gitx.Runner{Timeout: 30 * time.Second}
	ctx := context.Background()
	workspaceRecord, err := (&discovery.Discoverer{Git: runner, Config: cfg}).Resolve(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaceRecord.Repositories) != 2 {
		t.Fatalf("workspace repositories=%d, want the two repositories under the bundle", len(workspaceRecord.Repositories))
	}
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspaceRecord, _, err = store.UpsertWorkspaceGeneration(ctx, workspaceRecord)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := pool.ResolveBranches(ctx, runner, workspaceRecord, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.git = runner
	manager.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(manager.Close)
	return ctx, manager, store, workspaceRecord, resolved
}

// leaseReadyRepositoryRow は repository 1 件の READY row を組む。
// breakage は検証を落とす仕込みで、"" は一致する READY repository を表す。
func leaseReadyRepositoryRow(t *testing.T, manager *Manager, slot state.Slot, r pool.Resolved, breakage string) state.SlotRepository {
	t.Helper()
	dirName := testDirName(r.Repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(slot.Generation, r.OID, r.Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	row := state.SlotRepository{
		RepositoryID: string(r.Repository.ID), DirName: dirName, State: "READY",
		RequestedRef: "main", BaseOID: r.OID, Fingerprint: fingerprint,
	}
	switch breakage {
	case "base":
		row.BaseOID = "wrong-base"
	case "fingerprint":
		row.Fingerprint = "wrong-fingerprint"
	case "ownership":
		// worktree として実体はあるが Git の所有権 marker が無い状態。ValidateReady が ownership error を返す。
		if err := os.MkdirAll(filepath.Join(slot.Path, dirName), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return row
}

func TestLeaseReadyMatchesEvaluatesRepositoriesInIndexOrder(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved := leaseTwoRepositoryFixture(t)
	cases := []struct {
		name      string
		breakage  [2]string
		want      bool
		wantError bool
	}{
		{name: "both repositories fail ownership", breakage: [2]string{"ownership", "ownership"}, wantError: true},
		{name: "second repository has a stale base", breakage: [2]string{"", "base"}},
		{name: "mismatch at the lower index wins over a later error", breakage: [2]string{"fingerprint", "ownership"}},
		{name: "error at the lower index wins over a later mismatch", breakage: [2]string{"ownership", "fingerprint"}, wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			id := domain.StableID("lease-ready-order", test.name)
			slot := testSlotRow(t, manager, string(workspaceRecord.ID), id, 1, "READY")
			if err := os.MkdirAll(slot.Path, 0o700); err != nil {
				t.Fatal(err)
			}
			rows := []state.SlotRepository{
				leaseReadyRepositoryRow(t, manager, slot, resolved[0], test.breakage[0]),
				leaseReadyRepositoryRow(t, manager, slot, resolved[1], test.breakage[1]),
			}
			if _, err := store.CreateStandby(ctx, slot, rows); err != nil {
				t.Fatal(err)
			}
			// 並列検証の結果は index 昇順で採るので、-count を上げても毎回同じ判定になる。
			matched, err := manager.readyMatches(ctx, slot, resolved)
			if matched != test.want || test.wantError != (err != nil) {
				t.Fatalf("readyMatches matched=%v err=%v, want matched=%v error=%v", matched, err, test.want, test.wantError)
			}
		})
	}
}

func TestLeaseReadyMatchesAcceptsEveryMatchingRepository(t *testing.T) {
	t.Parallel()
	ctx, manager, store, workspaceRecord, resolved := leaseTwoRepositoryFixture(t)
	slot := testSlot(t, manager, string(workspaceRecord.ID), domain.StableID("lease-ready-cold", "all-cold"), 1, "READY")
	rows := make([]state.SlotRepository, 0, len(resolved))
	for _, r := range resolved {
		row := leaseReadyRepositoryRow(t, manager, slot, r, "")
		// COLD は未実体化のまま一致するので、実際の worktree を作らずに全 repository 一致を確かめられる。
		row.State = "COLD"
		rows = append(rows, row)
	}
	if _, err := store.CreateStandby(ctx, slot, rows); err != nil {
		t.Fatal(err)
	}
	matched, err := manager.readyMatches(ctx, slot, resolved)
	if err != nil || !matched {
		t.Fatalf("readyMatches matched=%v err=%v, want every repository to match", matched, err)
	}
}
