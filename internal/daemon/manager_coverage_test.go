package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
		// A COLD repository is unmaterialized when its directory is absent
		// or empty; a cold lease creates the empty directory so the client
		// can open it as its CWD before preparation runs. Only content means
		// the repository was actually checked out.
		{name: "cold repository has a checkout", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "populated"},
		{name: "cold repository directory is empty", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "directory", want: true},
		{name: "cold repository absent", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "missing", want: true},
		{name: "cold repository is a symlink", rootKind: "directory", repoState: "COLD", baseOID: resolved[0].OID, finger: fingerprint, target: "symlink"},
		{name: "ready repository absent", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "missing"},
		{name: "ready repository symlink", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "symlink"},
		{name: "ready repository is not Git", rootKind: "directory", repoState: "READY", baseOID: resolved[0].OID, finger: fingerprint, target: "directory", wantError: true},
	}
	dirName := testDirName(resolved[0].Repository, manager.Config())
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			id := domain.StableID("ready-coverage", test.name)
			slot := testSlotRow(t, manager, string(workspaceRecord.ID), id, 1, "READY")
			// Every repository worktree now sits one level below its slot
			// directory, for a single-repository workspace as much as for a
			// bundle, so "root" and "target" stage two different levels.
			target := filepath.Join(slot.Path, dirName)
			switch test.rootKind {
			case "file":
				if err := os.MkdirAll(filepath.Dir(slot.Path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(slot.Path, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				real := t.TempDir()
				if err := os.MkdirAll(filepath.Dir(slot.Path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, slot.Path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.MkdirAll(slot.Path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			switch test.target {
			case "directory":
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "populated":
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "tracked"), []byte("checked out\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.CreateStandby(ctx, slot,
				[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: test.repoState, RequestedRef: "main", BaseOID: test.baseOID, Fingerprint: test.finger}})
			if err != nil {
				t.Fatal(err)
			}
			valid, err := manager.readyMatches(ctx, slot, resolved)
			if test.wantError != (err != nil) || valid != test.want {
				t.Fatalf("readyMatches valid=%v err=%v, want valid=%v error=%v", valid, err, test.want, test.wantError)
			}
		})
	}

	countSlot := testSlot(t, manager, string(workspaceRecord.ID), domain.StableID("ready-coverage", "repository-count"), 1, "READY")
	if _, err := store.CreateStandby(ctx, countSlot, nil); err != nil {
		t.Fatal(err)
	}
	if valid, err := manager.readyMatches(ctx, countSlot, resolved); err != nil || valid {
		t.Fatalf("repository count mismatch valid=%v err=%v", valid, err)
	}
}

func TestPrepareSlotFailureAndReplayBoundaries(t *testing.T) {
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	resolved[0].Repository.RelativePath = "."
	if err := manager.prepareSlot(ctx, "missing", workspaceRecord, resolved, nil); err == nil {
		t.Fatal("missing slot preparation succeeded")
	}
	archived := domain.StableID("prepare-coverage", "archived")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), archived, 1, "ARCHIVED"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, archived, workspaceRecord, nil, nil); err == nil {
		t.Fatal("archived slot preparation succeeded")
	}

	mismatch := domain.StableID("prepare-coverage", "mismatch")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), mismatch, 1, "PREPARING"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, mismatch, workspaceRecord, resolved, nil); err == nil {
		t.Fatal("mismatched repository metadata succeeded")
	}

	dirName := testDirName(resolved[0].Repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	readyID := domain.StableID("prepare-coverage", "ready-replay")
	readySlot := testSlot(t, manager, string(workspaceRecord.ID), readyID, 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, readySlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, readyID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned READY replay error=%v", err)
	}
	if slot, err := store.Slot(ctx, readyID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned READY replay slot=%+v err=%v", slot, err)
	}

	validID := domain.StableID("prepare-coverage", "ready-replay-owned")
	validSlot := testSlot(t, manager, string(workspaceRecord.ID), validID, 1, "PREPARING")
	validWorktree := filepath.Join(validSlot.Path, dirName)
	if _, err := store.CreateStandby(ctx, validSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := manager.holdRootForPath(validSlot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.newPreparer(manager.Config(), validSlot).Prepare(ctx, resolved[0].Repository, validWorktree, resolved[0].OID, validID); err != nil {
		releaseRoot()
		t.Fatalf("create worktree for valid READY replay: %v", err)
	}
	releaseRoot()
	if err := store.SetSlotRepositoryState(ctx, validID, string(resolved[0].Repository.ID), []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareSlot(ctx, validID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}); err != nil {
		t.Fatalf("owned READY replay: %v", err)
	}
	if slot, err := store.Slot(ctx, validID); err != nil || slot.State != "READY" {
		t.Fatalf("owned READY replay slot=%+v err=%v", slot, err)
	}

	missingRepoID := domain.StableID("prepare-coverage", "missing-repository")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), missingRepoID, 1, "PREPARING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	unknownResolved := append([]pool.Resolved(nil), resolved...)
	unknownResolved[0].Repository.ID = "unknown"
	if err := manager.prepareSlot(ctx, missingRepoID, workspaceRecord, unknownResolved, []state.SlotRepository{{RepositoryID: "unknown"}}); err == nil {
		t.Fatal("missing stored repository preparation succeeded")
	}

	failureID := domain.StableID("prepare-coverage", "preparer-failure")
	// A regular file where the repository worktree must go makes the
	// checkout itself fail, which is an ordinary preparation failure rather
	// than an ownership one. A worktree path outside the root is no longer
	// expressible: dir_name is one component below the slot.
	failureSlot := testSlot(t, manager, string(workspaceRecord.ID), failureID, 1, "PREPARING")
	if err := os.WriteFile(filepath.Join(failureSlot.Path, dirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateStandby(ctx, failureSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), ambiguousID, 1, "PREPARING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "PREPARE_RUNNING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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
	ctx, manager, store, workspaceRecord, resolved, _ := managerCoverageFixture(t, "repository")
	resolved[0].Repository.RelativePath = "."
	if err := manager.restoreSlot(ctx, "missing", workspaceRecord, resolved, nil, nil); err == nil {
		t.Fatal("missing restore slot succeeded")
	}
	readyID := domain.StableID("restore-coverage", "already-ready")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), readyID, 1, "READY"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, readyID, workspaceRecord, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	archivedID := domain.StableID("restore-coverage", "archived")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), archivedID, 1, "ARCHIVED"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, archivedID, workspaceRecord, nil, nil, nil); err == nil {
		t.Fatal("archived restore slot succeeded")
	}

	mismatchID := domain.StableID("restore-coverage", "mismatch")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), mismatchID, 1, "RESTORING"), nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, mismatchID, workspaceRecord, resolved, nil, nil); err == nil {
		t.Fatal("mismatched restore metadata succeeded")
	}

	dirName := testDirName(resolved[0].Repository, manager.Config())
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
	if err != nil {
		t.Fatal(err)
	}
	missingRepoID := domain.StableID("restore-coverage", "missing-repository")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), missingRepoID, 1, "RESTORING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "RESTORING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	unknownResolved := append([]pool.Resolved(nil), resolved...)
	unknownResolved[0].Repository.ID = "unknown"
	if err := manager.restoreSlot(ctx, missingRepoID, workspaceRecord, unknownResolved, []state.SlotRepository{{RepositoryID: "unknown"}}, nil); err == nil {
		t.Fatal("missing stored restore repository succeeded")
	}

	completeID := domain.StableID("restore-coverage", "ready-replay")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), completeID, 1, "RESTORING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, completeID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unowned READY restore replay error=%v", err)
	}
	if slot, err := store.Slot(ctx, completeID); err != nil || slot.State != "QUARANTINED" {
		t.Fatalf("unowned READY restore replay slot=%+v err=%v", slot, err)
	}

	validID := domain.StableID("restore-coverage", "ready-replay-owned")
	validSlot := testSlot(t, manager, string(workspaceRecord.ID), validID, 1, "RESTORING")
	validWorktree := filepath.Join(validSlot.Path, dirName)
	if _, err := store.CreateStandby(ctx, validSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "RESTORING", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	releaseRoot, err := manager.holdRootForPath(validSlot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.newPreparer(manager.Config(), validSlot).PrepareForRestore(ctx, resolved[0].Repository, validWorktree, resolved[0].OID, validID); err != nil {
		releaseRoot()
		t.Fatalf("create worktree for valid READY restore replay: %v", err)
	}
	releaseRoot()
	if err := store.SetSlotRepositoryState(ctx, validID, string(resolved[0].Repository.ID), []string{"RESTORING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, validID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID)}}, nil); err != nil {
		t.Fatalf("owned READY restore replay: %v", err)
	}
	if slot, err := store.Slot(ctx, validID); err != nil || slot.State != "READY" {
		t.Fatalf("owned READY restore replay slot=%+v err=%v", slot, err)
	}

	ambiguousID := domain.StableID("restore-coverage", "ambiguous-command")
	if _, err := store.CreateStandby(ctx, testSlot(t, manager, string(workspaceRecord.ID), ambiguousID, 1, "RESTORING"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "RESTORE_RUNNING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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
	failureSlot := testSlot(t, manager, string(workspaceRecord.ID), failureID, 1, "RESTORING")
	if _, err := store.CreateStandby(ctx, failureSlot,
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "RESTORING", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreSlot(ctx, failureID, workspaceRecord, resolved, []state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, WorktreePath: filepath.Join(failureSlot.Path, dirName)}}, nil); err == nil {
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
	workspaceRecord = registerTestWorkspace(t, store, workspaceRecord)
	manager := testManager(t, cfg, store)
	manager.git = runner

	resolved, err := pool.ResolveBranches(ctx, runner, workspaceRecord, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirName := testDirName(resolved[0].Repository, cfg)
	fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, cfg)
	if err != nil {
		t.Fatal(err)
	}
	readyID := domain.StableID("registry-coverage", "invalid-ready")
	readyPath := filepath.Join(cfg.Storage.WorktreeRoot, string(workspaceRecord.ID), readyID)
	if _, err := store.CreateStandby(ctx,
		slotAtPath(t, manager, string(workspaceRecord.ID), readyID, readyPath, 1, "READY"),
		[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "READY", RequestedRef: "main", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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
	missingWorkspace = registerTestWorkspace(t, store, missingWorkspace)
	_ = missingWorkspace
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
			slotAtPath(t, manager, string(workspaceRecord.ID), candidate.id, path, 1, "LEASED"), nil,
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
		dirName := testDirName(resolved[0].Repository, manager.Config())
		fingerprint, err := workspace.Fingerprint(1, resolved[0].OID, resolved[0].Repository, manager.Config())
		if err != nil {
			t.Fatal(err)
		}
		id := domain.StableID("late-fault", "finish-preparation")
		root := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", id, "root")
		if _, err := store.CreateStandby(ctx,
			slotAtPath(t, manager, string(workspaceRecord.ID), id, root, 1, "PREPARING"),
			[]state.SlotRepository{{RepositoryID: string(resolved[0].Repository.ID), DirName: dirName, State: "READY", BaseOID: resolved[0].OID, Fingerprint: fingerprint}}); err != nil {
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

	t.Run("retired root keeps its slots", func(t *testing.T) {
		// Changing storage.worktree_root no longer drains the previous root:
		// its roots row stays registered with active=0 so existing slots keep
		// resolving, and only new slots go to the new root. The removed
		// DrainRoot used to mark them STALE, which is what this subtest used
		// to inject a failure into.
		ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		id := domain.StableID("late-fault", "retired-root")
		retained := testSlot(t, manager, string(workspaceRecord.ID), id, 1, "READY")
		if _, err := store.CreateStandby(ctx, retained, nil); err != nil {
			t.Fatal(err)
		}
		cfg := manager.Config()
		cfg.Storage.WorktreeRoot = filepath.Join(home, "new-worktrees")
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		if err := manager.reloadConfig(false); err != nil {
			t.Fatalf("reload after root change: %v", err)
		}
		slot, err := store.Slot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if slot.State != "READY" || slot.RootID != retained.RootID || slot.Path != retained.Path {
			t.Fatalf("slot on the retired root=%+v, want READY at %s", slot, retained.Path)
		}
		if _, activeRootID, err := manager.activeRoot(); err != nil || activeRootID == retained.RootID {
			t.Fatalf("active root id=%q err=%v, want a new generation for %s", activeRootID, err, cfg.Storage.WorktreeRoot)
		}
	})

	t.Run("invalid release timestamp", func(t *testing.T) {
		ctx, manager, store, workspaceRecord, _, databasePath := managerCoverageFixture(t)
		id := domain.StableID("late-fault", "release-time")
		path := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", id, "root")
		if _, err := store.CreateSlotSession(ctx,
			slotAtPath(t, manager, string(workspaceRecord.ID), id, path, 1, "DRAINING"), nil,
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
			slotAtPath(t, manager, string(workspaceRecord.ID), id, path, 1, "DRAINING"), nil,
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
			slotAtPath(t, manager, string(workspaceRecord.ID), id, filepath.Join(manager.Config().Storage.WorktreeRoot, id), 1, "LEASED"), nil,
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
			slotAtPath(t, manager, string(workspaceRecord.ID), archivedID, archivedPath, 1, "SNAPSHOTTED"), nil,
			state.Session{ID: archivedID, WorkspaceID: string(workspaceRecord.ID), SlotID: archivedID, State: "ARCHIVED", AgentKind: "coverage", TokenHash: state.HashToken(archivedID)}, ""); err != nil {
			t.Fatal(err)
		}
		standbyID := domain.StableID("late-fault", "gc-standby")
		standbyPath := filepath.Join(manager.Config().Storage.WorktreeRoot, "fault", standbyID, "root")
		if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), standbyID, standbyPath, 1, "READY"), nil); err != nil {
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
			slotAtPath(t, manager, string(workspaceRecord.ID), id, filepath.Join(manager.Config().Storage.WorktreeRoot, "restore-parent", id), 1, stateName), nil,
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

func TestCleanupSchedulingUsesPinnedRootOwnership(t *testing.T) {
	ctx, manager, store, workspaceRecord, _, _ := managerCoverageFixture(t)
	root := manager.Config().Storage.WorktreeRoot
	repositoryID := string(workspaceRecord.Repositories[0].ID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	newStandby := func(name string, repositories []state.SlotRepository) state.Slot {
		t.Helper()
		id := domain.StableID("cleanup-scheduling", name)
		path := filepath.Join(root, "cleanup", id, "root")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateStandby(ctx, slotAtPath(t, manager, string(workspaceRecord.ID), id, path, 1, "READY"), repositories); err != nil {
			t.Fatal(err)
		}
		return slotAtPath(t, manager, string(workspaceRecord.ID), id, path, 1, "READY")
	}

	standby := newStandby("standby", nil)
	if count := manager.scheduleStandbyRemovals(ctx, []state.StandbyGCCandidate{{SlotID: standby.ID, WorkspaceID: standby.WorkspaceID, Path: standby.Path, State: standby.State}}); count != 1 {
		t.Fatalf("standby removal count=%d, want 1", count)
	}
	if job := <-manager.jobs; job.id == "" {
		t.Fatal("standby removal did not enqueue a durable job")
	}
	if stored, err := store.Slot(ctx, standby.ID); err != nil || stored.State != "REMOVING" {
		t.Fatalf("standby slot after scheduling=%+v err=%v", stored, err)
	}
	if count := manager.scheduleEndedWorktreeRemovals(ctx, []state.GCCandidate{{SlotID: standby.ID, Path: standby.Path}}); count != 0 {
		t.Fatalf("already-removing worktree count=%d, want 0", count)
	}

	cold := newStandby("cold", []state.SlotRepository{{RepositoryID: repositoryID, DirName: "repository", State: "READY", BaseOID: "head"}})
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "cleanup", "cold", "repository")), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := state.ColdRepositoryCandidate{SlotID: cold.ID, WorkspaceID: cold.WorkspaceID, RepositoryID: repositoryID, WorktreePath: filepath.Join(cold.Path, "repository")}
	if count := manager.scheduleColdRepositoryRemovals(ctx, []state.ColdRepositoryCandidate{candidate}, map[string]bool{}); count != 1 {
		t.Fatalf("cold repository removal count=%d, want 1", count)
	}
	if job := <-manager.jobs; job.id == "" {
		t.Fatal("cold repository removal did not enqueue a durable job")
	}
	if count := manager.scheduleColdRepositoryRemovals(ctx, []state.ColdRepositoryCandidate{candidate}, map[string]bool{}); count != 0 {
		t.Fatalf("already-retiring repository count=%d, want 0", count)
	}
	if count := manager.scheduleColdRepositoryRemovals(ctx, []state.ColdRepositoryCandidate{candidate}, map[string]bool{cold.ID: true}); count != 0 {
		t.Fatalf("whole-slot cold removal count=%d, want 0", count)
	}

	unsafe := newStandby("unsafe", nil)
	outside := filepath.Join(t.TempDir(), "outside")
	if count := manager.scheduleEndedWorktreeRemovals(ctx, []state.GCCandidate{{SlotID: unsafe.ID, Path: outside, SessionID: ""}}); count != 0 {
		t.Fatalf("outside worktree removal count=%d, want 0", count)
	}
	if stored, err := store.Slot(ctx, unsafe.ID); err != nil || stored.State != "QUARANTINED" {
		t.Fatalf("outside worktree slot after quarantine=%+v err=%v", stored, err)
	}
	manager.quarantineCleanupFailure(unsafe.ID, errors.New("ordinary cleanup failure"))
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
			"validate fresh": func() error { return manager.PrepareFreshResume(ctx, "missing", "token", "agent", "", nil) },
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

func TestPrepareFreshResumePropagatesFailureCodes(t *testing.T) {
	t.Run("refuses a still-live prior mapping", func(t *testing.T) {
		root := t.TempDir()
		repoPath := filepath.Join(root, "repo")
		initGitRepo(t, repoPath)
		cfg := config.Defaults()
		cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
		cfg.Pool.WarmPerWorkspace = 0
		store, err := state.Open(filepath.Join(root, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		manager := testManager(t, cfg, store)
		t.Cleanup(manager.Close)
		ctx := context.Background()
		prior, err := manager.AllocateResumeSlot(ctx, "codex", os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BindAgentSession(ctx, prior.SessionID, "shared-agent"); err != nil {
			t.Fatal(err)
		}
		current, err := manager.AllocateResumeSlot(ctx, "codex", os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.PrepareFreshResume(ctx, current.SessionID, current.Token, "shared-agent", repoPath, nil); err == nil || !strings.Contains(err.Error(), "not EXPIRED") {
			t.Fatalf("fresh resume against a live prior mapping error=%v", err)
		}
		currentSession, err := store.SessionByID(ctx, current.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if slot, err := store.Slot(ctx, currentSession.SlotID); err != nil || slot.State != "FAILED" {
			t.Fatalf("refused fresh resume slot=%+v err=%v", slot, err)
		}
	})

	t.Run("propagates a bind storage failure", func(t *testing.T) {
		root := t.TempDir()
		repoPath := filepath.Join(root, "repo")
		initGitRepo(t, repoPath)
		cfg := config.Defaults()
		cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
		cfg.Pool.WarmPerWorkspace = 0
		databasePath := filepath.Join(root, "state.db")
		store, err := state.Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		manager := testManager(t, cfg, store)
		t.Cleanup(manager.Close)
		ctx := context.Background()
		current, err := manager.AllocateResumeSlot(ctx, "codex", os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		raw := openManagerCoverageDB(t, databasePath)
		// SQLite trigger WHEN clauses cannot bind parameters, so the already-
		// known session id is interpolated directly into the trigger body.
		if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_fresh_bind BEFORE UPDATE ON sessions WHEN OLD.id='`+current.SessionID+`' BEGIN SELECT RAISE(ABORT,'injected bind failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := manager.PrepareFreshResume(ctx, current.SessionID, current.Token, "solo-agent", repoPath, nil); err == nil {
			t.Fatal("fresh resume succeeded despite an injected bind storage failure")
		}
		currentSession, err := store.SessionByID(ctx, current.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if slot, err := store.Slot(ctx, currentSession.SlotID); err != nil || slot.State != "FAILED" {
			t.Fatalf("failed fresh resume slot=%+v err=%v", slot, err)
		}
	})
}

// managerCoverageFixture registers one workspace and returns a Manager for
// it. kind defaults to "multi_repository"; pass "repository" for the tests
// that need a single-repository workspace. It is a parameter rather than
// something a caller re-upserts afterwards because workspace identity is now
// the store's: a second upsert at the same root_path with a different kind
// gets a fresh ID and collides on workspaces.root_path.
func managerCoverageFixture(t *testing.T, kind ...string) (context.Context, *Manager, *state.Store, discovery.Workspace, []pool.Resolved, string) {
	t.Helper()
	workspaceKind := "multi_repository"
	if len(kind) > 0 {
		workspaceKind = kind[0]
	}
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
	workspaceRecord.Kind = workspaceKind
	if workspaceKind == "multi_repository" {
		workspaceRecord.Repositories[0].RelativePath = "repository"
	} else {
		// A single-repository workspace is rooted at its own main worktree,
		// which is what validateWorkspaceRepositoryAssociation proves.
		workspaceRecord.Root = workspaceRecord.Repositories[0].MainPath
		workspaceRecord.Repositories[0].RelativePath = "."
	}
	databasePath := filepath.Join(root, "state.db")
	store, err := state.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// The store, not discovery, owns workspace identity: the returned
	// workspace carries the durable ID that names slot directories.
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
	return ctx, manager, store, workspaceRecord, resolved, databasePath
}
