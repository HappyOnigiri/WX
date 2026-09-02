package daemon

import (
	"context"
	"database/sql"
	"errors"
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

func TestReadyMatchesRejectsEveryUnsafeRepresentation(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	type slotCase struct {
		name      string
		rootKind  string
		repoState string
		baseOID   string
		finger    string
		target    string
		want      bool
		wantError bool
	}
	cases := []slotCase{
		{name: "missing root", rootKind: "missing", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint},
		{name: "regular root", rootKind: "file", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint},
		{name: "symlink root", rootKind: "symlink", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint},
		{name: "preparing repository", rootKind: "directory", repoState: "PREPARING", baseOID: resolved[0].OID, finger: fingerprint},
		{name: "wrong base", rootKind: "directory", repoState: "READY", baseOID: "wrong", finger: fingerprint},
		{name: "wrong fingerprint", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: "wrong"},
		{name: "cold repository still present", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "directory"},
		{name: "cold repository absent", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "missing", want: true},
		{name: "ready repository absent", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "missing"},
		{name: "ready repository symlink", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "symlink"},
		{name: "ready repository is not Git", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "directory", wantError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			id := domain.StableID("ready-coverage", test.name)
			root := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", id, "root")
			target := filepath.Join(root, "repository")
			switch test.rootKind {
			case "file":
				if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				real := t.TempDir()
				if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, root); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch test.target {
			case "directory":
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.CreateStandby(ctx,
				state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: root, State: "READY"},
				[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: target, State: test.repoState, RequestedRef: "main", BaseOID: test.baseOID, Fingerprint: test.finger}})
			if err != nil {
				t.Fatal(err)
			}
			valid, err := manager.readyMatches(ctx, state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: root, State: "READY"}, resolved)
			if test.wantError != (err != nil) || valid != test.want {
				t.Fatalf("readyMatches valid=%v err=%v, want valid=%v error=%v", valid, err, test.want, test.wantError)
			}
		})
	}

	id := domain.StableID("ready-coverage", "repository-count")
	root := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", id, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: root, State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	if valid, err := manager.readyMatches(ctx, state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: root, State: "READY"}, resolved); err != nil || valid {
		t.Fatalf("repository count mismatch valid=%v err=%v", valid, err)
	}
}

func TestPrepareSlotFailureAndReplayBoundaries(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	workspaceRecord.Kind = "repository"
	if err := manager.prepareSlot(ctx, "missing", workspaceRecord, resolved, nil); err == nil {
		t.Fatal("missing slot preparation succeeded")
	}
	archived := domain.StableID("prepare-coverage", "archived")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: archived, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "root"), State: "ARCHIVED"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, archived, workspaceRecord, nil, nil); err == nil {
		t.Fatal("archived slot preparation succeeded")
	}

	mismatch := domain.StableID("prepare-coverage", "mismatch")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: mismatch, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "root"), State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, mismatch, workspaceRecord, resolved, nil); err == nil {
		t.Fatal("mismatched repository metadata succeeded")
	}

	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	readyID := domain.StableID("prepare-coverage", "ready-replay")
	readyRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", readyID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: readyID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: readyRoot, State: "PREPARING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: readyRoot, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, readyID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err != nil {
		t.Fatal(err)
	}

	missingRepoID := domain.StableID("prepare-coverage", "missing-repository")
	missingRepoRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", missingRepoID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: missingRepoID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: missingRepoRoot, State: "PREPARING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: missingRepoRoot, State: "PREPARING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	unknownResolved := append([]pool.Resolved(nil), resolved...)
	unknownResolved[0].Repository.ID = "unknown"
	if err := manager.prepareSlot(ctx, missingRepoID, workspaceRecord, unknownResolved, []state.SlotRepository{{RepositoryID: "unknown"}}); err == nil {
		t.Fatal("missing stored repository preparation succeeded")
	}

	failureID := domain.StableID("prepare-coverage", "preparer-failure")
	failureRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", failureID, "root")
	outside := filepath.Join(t.TempDir(), "outside")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: failureID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: failureRoot, State: "PREPARING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: outside, State: "PREPARING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, failureID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err == nil {
		t.Fatal("unsafe worktree preparation succeeded")
	}
	slot, err := store.Slot(ctx, failureID)
	if err != nil || slot.State != "FAILED" {
		t.Fatalf("failed preparation slot=%+v err=%v", slot, err)
	}

	ambiguousID := domain.StableID("prepare-coverage", "ambiguous-command")
	ambiguousRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "coverage", ambiguousID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: ambiguousID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: ambiguousRoot, State: "PREPARING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: ambiguousRoot, State: "PREPARE_RUNNING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Repositories[string(resolved[0].Repository.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"true"}}}
	manager.cfg = cfg
	manager.mu.Unlock()
	if err := manager.prepareSlot(ctx, ambiguousID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err == nil {
		t.Fatal("ambiguous prepare command replay succeeded")
	}
	slot, err = store.Slot(ctx, ambiguousID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("ambiguous preparation slot=%+v err=%v", slot, err)
	}
}

func TestRestoreSlotFailureAndReplayBoundaries(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
	workspaceRecord.Kind = "repository"
	if err := manager.restoreSlot(ctx, "missing", workspaceRecord, resolved, nil, nil); err == nil {
		t.Fatal("missing restore slot succeeded")
	}
	readyID := domain.StableID("restore-coverage", "already-ready")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: readyID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "ready"), State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, readyID, workspaceRecord, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	archivedID := domain.StableID("restore-coverage", "archived")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: archivedID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "archived"), State: "ARCHIVED"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, archivedID, workspaceRecord, nil, nil, nil); err == nil {
		t.Fatal("archived restore slot succeeded")
	}

	mismatchID := domain.StableID("restore-coverage", "mismatch")
	if _, err := store.CreateStandby(ctx, state.Slot{ID: mismatchID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(t.TempDir(), "mismatch"), State: "RESTORING"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, mismatchID, workspaceRecord, resolved, nil, nil); err == nil {
		t.Fatal("mismatched restore metadata succeeded")
	}

	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	missingRepoID := domain.StableID("restore-coverage", "missing-repository")
	missingRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore", missingRepoID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: missingRepoID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: missingRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: missingRoot, State: "RESTORING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	unknownResolved := append([]pool.Resolved(nil), resolved...)
	unknownResolved[0].Repository.ID = "unknown"
	if err := manager.restoreSlot(ctx, missingRepoID, workspaceRecord, unknownResolved, []state.SlotRepository{{RepositoryID: "unknown"}}, nil); err == nil {
		t.Fatal("missing stored restore repository succeeded")
	}

	completeID := domain.StableID("restore-coverage", "ready-replay")
	completeRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore", completeID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: completeID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: completeRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: completeRoot, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, completeID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}, nil); err != nil {
		t.Fatal(err)
	}

	ambiguousID := domain.StableID("restore-coverage", "ambiguous-command")
	ambiguousRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore", ambiguousID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: ambiguousID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: ambiguousRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: ambiguousRoot, State: "RESTORE_RUNNING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	cfg := manager.cfg
	cfg.Repositories[string(resolved[0].Repository.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"true"}}}
	manager.cfg = cfg
	manager.mu.Unlock()
	if err := manager.restoreSlot(ctx, ambiguousID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}, nil); err == nil {
		t.Fatal("ambiguous restore command replay succeeded")
	}
	slot, err := store.Slot(ctx, ambiguousID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("ambiguous restore slot=%+v err=%v", slot, err)
	}

	failureID := domain.StableID("restore-coverage", "missing-snapshot")
	failureRoot := filepath.Join(manager.Config().Storage.WorktreeRoot, "restore", failureID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: failureID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: failureRoot, State: "RESTORING"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: failureRoot, State: "RESTORING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, failureID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: failureRoot}}, nil); err == nil {
		t.Fatal("restore without snapshot metadata succeeded")
	}
	slot, err = store.Slot(ctx, failureID)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("failed restore slot=%+v err=%v", slot, err)
	}
}

func TestMaintenanceLoopHandlesReloadAndTimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(home, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Discovery.ReconcileInterval.Duration = 10 * time.Millisecond
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.reloads = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		manager.maintainLifecycle()
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return !manager.lastBackup.IsZero()
	})
	manager.reloads <- struct{}{}
	started := manager.started
	waitUntil(t, 2*time.Second, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.lastReload.After(started)
	})
	manager.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance loop did not stop")
	}
}

func TestRegistryOrphanAndClosedStoreReconciliation(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	workspaceRecord, err := (&discovery.Discoverer{Git: runner, Config: cfg}).Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, cfg, store)
	manager.git = runner

	resolved, err := pool.ResolveBranches(ctx, runner, workspaceRecord, nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, cfg)
	if err != nil {
		t.Fatal(err)
	}
	readyID := domain.StableID("registry-coverage", "invalid-ready")
	readyPath := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(workspaceRecord.ID), "slots", readyID, "root")
	if _, err := store.CreateStandby(ctx,
		state.Slot{ID: readyID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: readyPath, State: "READY"},
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: readyPath, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	diagnostics := manager.registrationDiagnostics(ctx)
	if diagnostics["checked"] != 1 || len(diagnostics["invalid"].([]map[string]string)) != 1 {
		t.Fatalf("registration diagnostics=%v", diagnostics)
	}
	manager.reconcileRegistry(ctx)
	slot, err := store.Slot(ctx, readyID)
	if err != nil || slot.State != "STALE" {
		t.Fatalf("reconciled READY slot=%+v err=%v", slot, err)
	}

	missingWorkspace := discovery.Workspace{ID: "missing-workspace", Root: domain.CanonicalPath(filepath.Join(root, "missing")), Kind: "repository"}
	if err := store.UpsertWorkspace(ctx, missingWorkspace); err != nil {
		t.Fatal(err)
	}
	manager.reconcileRegistry(ctx)
	diagnostics = manager.registrationDiagnostics(ctx)
	if len(diagnostics["invalid"].([]map[string]string)) == 0 {
		t.Fatalf("missing workspace diagnostics=%v", diagnostics)
	}

	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	old := state.FormatTime(time.Now().Add(-time.Hour))
	for _, candidate := range []struct {
		id  string
		pid int
	}{
		{id: domain.StableID("orphan", "dead"), pid: 99999999},
		{id: domain.StableID("orphan", "alive"), pid: os.Getpid()},
	} {
		path := filepath.Join(cfg.Storage.WorktreeRoot, "orphan", candidate.id, "root")
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: candidate.id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: path, State: "LEASED"}, nil,
			state.Session{ID: candidate.id, WorkspaceID: string(workspaceRecord.ID), SlotID: candidate.id, State: "ACTIVE", AgentKind: "coverage", ClientPID: candidate.pid, TokenHash: state.HashToken(candidate.id)}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `UPDATE sessions SET created_at=? WHERE id=?`, old, candidate.id); err != nil {
			t.Fatal(err)
		}
	}
	manager.reconcileOrphans(ctx)
	dead, err := store.SessionByID(ctx, domain.StableID("orphan", "dead"))
	if err != nil || dead.State != "RELEASING" {
		t.Fatalf("dead orphan=%+v err=%v", dead, err)
	}
	alive, err := store.SessionByID(ctx, domain.StableID("orphan", "alive"))
	if err != nil || alive.State != "ACTIVE" {
		t.Fatalf("live session=%+v err=%v", alive, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := manager.artifactDiagnostics(ctx); len(got["errors"].([]string)) == 0 {
		t.Fatalf("closed-store artifacts=%v", got)
	}
	manager.reconcileRegistry(ctx)
	manager.reconcileOrphans(ctx)
	manager.maybeBackup(ctx)
	if manager.backupError == "" {
		t.Fatal("closed store backup error was not recorded")
	}
	if got := manager.registrationDiagnostics(ctx); got["error"] == nil {
		t.Fatalf("closed-store registration diagnostics=%v", got)
	}
	manager.Close()
}

func TestManagerLateStageFaultBoundaries(t *testing.T) {
	t.Run("finish preparation", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, resolved, databasePath := managerCoverageFixture(t)
		workspaceRecord.Kind = "repository"
		fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
		if err != nil {
			t.Fatal(err)
		}
		id := domain.StableID("late-fault", "finish-preparation")
		root := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", id, "root")
		if _, err := store.CreateStandby(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: root, State: "PREPARING"},
			[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), WorktreePath: root, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_finish_preparation BEFORE UPDATE ON slots BEGIN SELECT RAISE(FAIL, 'injected finish failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := manager.prepareSlot(ctx, id, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err == nil {
			t.Fatal("injected finish preparation failure succeeded")
		}
	})

	t.Run("drain retired root", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		id := domain.StableID("late-fault", "drain-root")
		path := filepath.Join(manager.Config().Storage.WorktreeRoot, "workspaces", string(workspaceRecord.ID), "slots", id, "root")
		if _, err := store.CreateStandby(ctx, state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: path, State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_root_drain BEFORE UPDATE ON slots BEGIN SELECT RAISE(FAIL, 'injected drain failure'); END`); err != nil {
			t.Fatal(err)
		}
		cfg := manager.Config()
		cfg.Storage.WorktreeRoot = filepath.Join(home, "new-worktrees")
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		if err := manager.reloadConfig(false); err == nil {
			t.Fatal("injected root drain failure succeeded")
		}
	})

	t.Run("invalid release timestamp", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		id := domain.StableID("late-fault", "release-time")
		path := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", id, "root")
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: path, State: "DRAINING"}, nil,
			state.Session{ID: id, WorkspaceID: string(workspaceRecord.ID), SlotID: id, State: "RELEASING", AgentKind: "coverage", TokenHash: state.HashToken(id)}, ""); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `UPDATE sessions SET released_at='not-a-time' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		session, err := store.SessionByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.snapshotSession(ctx, session); err == nil {
			t.Fatal("invalid release timestamp was snapshotted")
		}
	})

	t.Run("begin snapshot storage failure", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		id := domain.StableID("late-fault", "begin-snapshot")
		path := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", id, "root")
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: path, State: "DRAINING"}, nil,
			state.Session{ID: id, WorkspaceID: string(workspaceRecord.ID), SlotID: id, State: "RELEASING", AgentKind: "coverage", TokenHash: state.HashToken(id)}, ""); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_begin_snapshot BEFORE UPDATE OF state ON sessions WHEN NEW.state='SNAPSHOTTING' BEGIN SELECT RAISE(ABORT,'injected snapshot failure'); END`); err != nil {
			t.Fatal(err)
		}
		session, err := store.SessionByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.snapshotSession(ctx, session); err == nil {
			t.Fatal("injected snapshot persistence failure succeeded")
		}
		storedSession, err := store.SessionByID(ctx, id)
		if err != nil || storedSession.State != "RELEASING" {
			t.Fatalf("failed snapshot advanced session: session=%+v err=%v", storedSession, err)
		}
		slot, err := store.Slot(ctx, id)
		if err != nil || slot.State != "DRAINING" {
			t.Fatalf("failed snapshot advanced slot: slot=%+v err=%v", slot, err)
		}
	})

	t.Run("artifact recovery refs", func(t *testing.T) {
		ctx, manager, _, _, _, databasePath := managerCoverageFixture(t)
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `DROP TABLE snapshots`); err != nil {
			t.Fatal(err)
		}
		diagnostics := manager.artifactDiagnostics(ctx)
		if len(diagnostics["errors"].([]string)) == 0 {
			t.Fatalf("artifact diagnostics=%v", diagnostics)
		}
	})

	t.Run("orphan release", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		id := domain.StableID("late-fault", "orphan-release")
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(manager.Config().Storage.WorktreeRoot, id), State: "LEASED"}, nil,
			state.Session{ID: id, WorkspaceID: string(workspaceRecord.ID), SlotID: id, State: "ACTIVE", AgentKind: "coverage", ClientPID: 99999999, TokenHash: state.HashToken(id)}, ""); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		if _, err := raw.ExecContext(ctx, `UPDATE sessions SET created_at=? WHERE id=?`, state.FormatTime(time.Now().Add(-time.Hour)), id); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_orphan_release BEFORE UPDATE ON sessions BEGIN SELECT RAISE(FAIL, 'injected orphan failure'); END`); err != nil {
			t.Fatal(err)
		}
		manager.reconcileOrphans(ctx)
		session, err := store.SessionByID(ctx, id)
		if err != nil || session.State != "ACTIVE" {
			t.Fatalf("orphan after injected failure=%+v err=%v", session, err)
		}
	})

	t.Run("GC scheduling", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		manager.mu.Lock()
		manager.cfg.Retention.EndedWorktree.Duration = -time.Second
		manager.cfg.Retention.HotStandby.Duration = time.Nanosecond
		manager.mu.Unlock()
		archivedID := domain.StableID("late-fault", "gc-archived")
		archivedPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", archivedID, "root")
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: archivedID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: archivedPath, State: "SNAPSHOTTED"}, nil,
			state.Session{ID: archivedID, WorkspaceID: string(workspaceRecord.ID), SlotID: archivedID, State: "ARCHIVED", AgentKind: "coverage", TokenHash: state.HashToken(archivedID)}, ""); err != nil {
			t.Fatal(err)
		}
		standbyID := domain.StableID("late-fault", "gc-standby")
		standbyPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", standbyID, "root")
		if _, err := store.CreateStandby(ctx, state.Slot{ID: standbyID, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: standbyPath, State: "READY"}, nil); err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		old := state.FormatTime(time.Now().Add(-time.Hour))
		if _, err := raw.ExecContext(ctx, `UPDATE sessions SET archived_at=? WHERE id=?`, old, archivedID); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `UPDATE slots SET ready_at=? WHERE id=?`, old, standbyID); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_gc_schedule BEFORE UPDATE ON slots BEGIN SELECT RAISE(FAIL, 'injected GC failure'); END`); err != nil {
			t.Fatal(err)
		}
		if count, err := manager.GC(ctx, false); err != nil || count != 0 {
			t.Fatalf("GC count=%d err=%v", count, err)
		}
	})
}

func TestRecoveredJobAndRestoreParentFailures(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	for _, job := range []state.Job{
		{Kind: "PREPARE", WorkspaceID: "missing", SlotID: "missing"},
		{Kind: "ENSURE_STANDBY", WorkspaceID: "missing"},
		{Kind: "SNAPSHOT", SessionID: "missing"},
		{Kind: "RESTORE", SessionID: "missing"},
		{Kind: "REMOVE", SlotID: "missing"},
		{Kind: "REMOVE_REPOSITORY", SlotID: "missing", RepositoryID: "missing"},
		{Kind: "UNKNOWN"},
	} {
		if err := manager.runRecoveredJob(ctx, job); err == nil {
			t.Errorf("runRecoveredJob(%s) succeeded", job.Kind)
		}
	}

	createSession := func(id, stateName, parent string) {
		t.Helper()
		if _, err := store.CreateSlotSession(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspaceRecord.ID), Generation: 1, Path: filepath.Join(manager.Config().Storage.WorktreeRoot, "restore-parent", id), State: stateName}, nil,
			state.Session{ID: id, WorkspaceID: string(workspaceRecord.ID), SlotID: id, ParentSessionID: parent, State: stateName, AgentKind: "coverage", TokenHash: state.HashToken(id)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	noParent := domain.StableID("restore-parent", "none")
	createSession(noParent, "RESTORING", "")
	if err := manager.resumeRestoreJob(ctx, noParent); err == nil {
		t.Fatal("restore without parent succeeded")
	}
	releasingParent := domain.StableID("restore-parent", "releasing-parent")
	createSession(releasingParent, "RELEASING", "")
	releasingChild := domain.StableID("restore-parent", "releasing-child")
	createSession(releasingChild, "RESTORING", releasingParent)
	var pending dependencyPendingError
	if err := manager.resumeRestoreJob(ctx, releasingChild); !errors.As(err, &pending) {
		t.Fatalf("releasing parent error=%v", err)
	}

	archivedParent := domain.StableID("restore-parent", "archived-parent")
	createSession(archivedParent, "ARCHIVED", "")
	archivedChild := domain.StableID("restore-parent", "archived-child")
	createSession(archivedChild, "RESTORING", archivedParent)
	if err := manager.resumeRestoreJob(ctx, archivedChild); err == nil {
		t.Fatal("restore with incomplete archived snapshot succeeded")
	}
	slot, err := store.Slot(ctx, archivedChild)
	if err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("incomplete restore child=%+v err=%v", slot, err)
	}
}

func TestStatusHotStandbyUsesRepositoryLastLease(t *testing.T) {
	ctx, manager, _, _, _, databasePath := managerCoverageFixture(t)
	manager.mu.Lock()
	manager.cfg.Retention.HotStandby.Duration = time.Hour
	manager.mu.Unlock()
	raw := openManagerCoverageDB(t, databasePath)
	oldLease := time.Now().Add(-2 * time.Hour)
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(oldLease)); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	details := status["repository_details"].([]state.RepositoryDiagnostic)
	if len(details) != 1 || details[0].Hot || details[0].StandbyExpiresAt != state.FormatTime(oldLease.Add(time.Hour)) {
		t.Fatalf("expired hot standby=%+v", details)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	details = status["repository_details"].([]state.RepositoryDiagnostic)
	if len(details) != 1 || !details[0].Hot {
		t.Fatalf("recently leased repository was not hot: %+v", details)
	}
}

func TestManagerConfigurationAndStoreFailureBranches(t *testing.T) {
	t.Run("resolve and lease", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
		if _, err := manager.ResolveAndLease(ctx, filepath.Join(t.TempDir(), "missing"), nil, "coverage", 0); err == nil {
			t.Fatal("missing workspace resolved")
		}
		manager.mu.Lock()
		cfg := manager.cfg
		cfg.Repositories[string(workspaceRecord.Repositories[0].MainPath)] = config.Repository{DefaultBranch: "missing"}
		manager.cfg = cfg
		manager.mu.Unlock()
		if _, err := manager.ResolveAndLease(ctx, string(workspaceRecord.Root), nil, "coverage", 0); err == nil {
			t.Fatal("missing base branch lease succeeded")
		}
		manager.mu.Lock()
		cfg.Repositories = map[string]config.Repository{}
		manager.cfg = cfg
		manager.mu.Unlock()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ResolveAndLease(ctx, string(workspaceRecord.Root), nil, "coverage", 0); err == nil {
			t.Fatal("lease with closed store succeeded")
		}
	})

	t.Run("allocation roots", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t)
		manager.mu.Lock()
		cfg := manager.cfg
		cfg.Storage.WorktreeRoot = "$UNSUPPORTED/root"
		manager.cfg = cfg
		manager.mu.Unlock()
		if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "coverage", 0, "STARTING", ""); err == nil {
			t.Fatal("allocation with unsupported root succeeded")
		}
		if _, err := manager.AllocateResumeSlot(ctx, "coverage", 0); err == nil {
			t.Fatal("resume allocation with unsupported root succeeded")
		}
		blocked := filepath.Join(t.TempDir(), "blocked")
		if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		manager.mu.Lock()
		cfg.Storage.WorktreeRoot = filepath.Join(blocked, "child")
		manager.cfg = cfg
		manager.mu.Unlock()
		if _, err := manager.allocate(ctx, workspaceRecord, resolved, 1, "coverage", 0, "STARTING", ""); err == nil {
			t.Fatal("allocation below a regular file succeeded")
		}
		manager.mu.Lock()
		cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "valid")
		manager.cfg = cfg
		manager.mu.Unlock()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AllocateResumeSlot(ctx, "coverage", 0); err == nil {
			t.Fatal("resume allocation with closed store succeeded")
		}
	})

	t.Run("standby", func(t *testing.T) {
		ctx, manager, _, workspaceRecord, _, _ := managerCoverageFixture(t)
		manager.mu.Lock()
		cfg := manager.cfg
		cfg.Pool.WarmPerWorkspace = 1
		manager.cfg = cfg
		manager.mu.Unlock()
		missingBase := workspaceRecord
		missingBase.Repositories = append([]discovery.Repository(nil), workspaceRecord.Repositories...)
		missingBase.Repositories[0].DefaultBranch = "missing"
		if err := manager.ensureStandby(ctx, missingBase); err == nil {
			t.Fatal("standby with missing base succeeded")
		}
		unknown := workspaceRecord
		unknown.ID = "unknown-workspace"
		if err := manager.ensureStandby(ctx, unknown); err == nil {
			t.Fatal("standby for unregistered workspace succeeded")
		}
		manager.mu.Lock()
		cfg.Storage.WorktreeRoot = "$UNSUPPORTED/root"
		manager.cfg = cfg
		manager.mu.Unlock()
		if err := manager.ensureStandby(ctx, workspaceRecord); err == nil {
			t.Fatal("standby with unsupported root succeeded")
		}
	})

	t.Run("closed store diagnostics", func(t *testing.T) {
		ctx, manager, store, _, _, _ := managerCoverageFixture(t)
		manager.Close()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Status(ctx); err == nil {
			t.Fatal("status with closed store succeeded")
		}
		if err := manager.Heartbeat(ctx, "missing", "token"); err == nil {
			t.Fatal("heartbeat with closed store succeeded")
		}
		if _, err := manager.Sessions(ctx, false); err == nil {
			t.Fatal("sessions with closed store succeeded")
		}
		for name, operation := range map[string]func() error{
			"wait ready":     func() error { return manager.WaitReady(ctx, "missing", "token") },
			"bind agent":     func() error { return manager.BindAgentSession(ctx, "missing", "token", "agent") },
			"validate fresh": func() error { return manager.ValidateFreshResume(ctx, "missing", "token", "agent") },
			"bind restore":   func() error { return manager.BindAndRestoreResume(ctx, "missing", "token", "agent") },
			"release":        func() error { return manager.Release(ctx, "missing", "token", "coverage") },
			"snapshot":       func() error { return manager.snapshotSession(ctx, state.Session{SlotID: "missing"}) },
			"forget":         func() error { return manager.Forget(ctx, t.TempDir()) },
			"registration report": func() error {
				_, ok := manager.registrationDiagnostics(ctx)["error"]
				if !ok {
					return errors.New("missing database error")
				}
				return nil
			},
		} {
			if err := operation(); err == nil && name != "registration report" {
				t.Errorf("%s with closed store succeeded", name)
			} else if err != nil && name == "registration report" {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := manager.ResumeStatus(ctx, "missing"); err == nil {
			t.Fatal("resume status with closed store succeeded")
		}
		if _, err := manager.Resume(ctx, "missing", "coverage", 0, false); err == nil {
			t.Fatal("resume with closed store succeeded")
		}
		if _, err := manager.GC(ctx, false); err == nil {
			t.Fatal("GC with closed store succeeded")
		}
		doctor := manager.Doctor(ctx)
		checks := doctor["checks"].(map[string]any)
		if checks["sqlite"] == "ok" {
			t.Fatal("doctor reported closed store as healthy")
		}
	})
}

func openManagerCoverageDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func managerCoverageFixture(t *testing.T) (context.Context, *Manager, *state.Store, discovery.Workspace, []pool.Resolved, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	workspaceRecord, err := (&discovery.Discoverer{Git: runner, Config: cfg}).Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRecord.Kind = "multi_repository"
	workspaceRecord.Repositories[0].RelativePath = "repository"
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertWorkspace(ctx, workspaceRecord); err != nil {
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
	return ctx, manager, store, workspaceRecord, resolved, databasePath
}
