package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestMultiRepositoryArchiveDoesNotQuarantineInFlightRecoveryRefs(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "multi")
	service := filepath.Join(workspaceRoot, "service")
	web := filepath.Join(workspaceRoot, "web")
	initGitRepo(t, service)
	initGitRepo(t, web)

	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	cfg.Readiness.Timeout.Duration = 10 * time.Second
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	m.git.SetTimeout(10 * time.Second)
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Repositories) != 2 {
		t.Fatalf("workspace repositories=%d, want 2", len(w.Repositories))
	}
	w = registerTestWorkspace(t, store, w)
	resolved, err := resolveTestBranches(ctx, m, w)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(cfg.Storage.WorktreeRoot, string(w.ID), sessionID)
	if err := os.MkdirAll(slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	repos, err := m.slotRepos(slotPath, w, resolved, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: sessionID, WorkspaceID: string(w.ID), SlotID: sessionID, State: "STARTING", AgentKind: "codex", TokenHash: state.HashToken("token")}
	prepareJob, err := store.CreateSlotSession(ctx, slotAtPath(t, m, string(w.ID), sessionID, slotPath, 1, "PREPARING"), repos, session, "PREPARE")
	if err != nil {
		t.Fatal(err)
	}
	claimedPrepare, err := store.ClaimJob(ctx, prepareJob.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, claimedPrepare); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedPrepare.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	for _, sr := range repos {
		if err := os.WriteFile(filepath.Join(sr.WorktreePath, "session-change.txt"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshotJob, changed, err := store.Release(ctx, sessionID, string(w.ID), sessionID)
	if err != nil || !changed {
		t.Fatalf("release changed=%v job=%+v err=%v", changed, snapshotJob, err)
	}
	claimedSnapshot, err := store.ClaimJob(ctx, snapshotJob.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	released, err := store.SessionByID(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	started, proceed := installRecoveryRefBarrier(t)
	archiveDone := make(chan error, 1)
	go func() { archiveDone <- m.snapshotSession(ctx, released) }()
	barrierDeadline := time.NewTimer(5 * time.Second)
	defer barrierDeadline.Stop()
	barrierTick := time.NewTicker(10 * time.Millisecond)
	defer barrierTick.Stop()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-archiveDone:
			t.Fatalf("archive ended before ref barrier: %v", err)
		case <-barrierTick.C:
		case <-barrierDeadline.C:
			t.Fatal("archive did not reach recovery-ref barrier")
		}
	}

	if diagnostics := m.artifactDiagnostics(ctx); len(diagnostics["unknown_refs"].([]string)) != 0 || len(diagnostics["missing_refs"].([]string)) != 0 {
		t.Fatalf("in-flight archive was diagnosed as an artifact: %v", diagnostics)
	}
	m.reconcileArtifacts(ctx)
	status, err := store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Quarantine) != 0 {
		t.Fatalf("in-flight archive was quarantined: %+v", status.Quarantine)
	}
	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimedSnapshot.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	m.reconcileArtifacts(ctx)
	status, err = store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Quarantine) != 0 {
		t.Fatalf("successful archive was quarantined: %+v", status.Quarantine)
	}
	snapshots, err := store.Snapshots(ctx, sessionID)
	if err != nil || len(snapshots) != 2 {
		t.Fatalf("multi-repository snapshots=%+v err=%v", snapshots, err)
	}

	gitRun(t, service, "update-ref", "refs/wx/recovery/foreign", gitOutput(t, service, "rev-parse", "HEAD"))
	m.reconcileArtifacts(ctx)
	diagnostics := m.artifactDiagnostics(ctx)
	unknown := diagnostics["unknown_refs"].([]string)
	if len(unknown) != 1 || !strings.HasSuffix(unknown[0], ":refs/wx/recovery/foreign") {
		t.Fatalf("genuine unknown ref was not diagnosed: %v", diagnostics)
	}
	status, err = store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Quarantine) != 1 || !strings.HasSuffix(status.Quarantine[0].Path, ":refs/wx/recovery/foreign") {
		t.Fatalf("genuine unknown ref was not quarantined: %+v", status.Quarantine)
	}

	mismatchedSnapshot := snapshots[0]
	var mismatchedRepository discovery.Repository
	for _, repository := range w.Repositories {
		if string(repository.ID) == mismatchedSnapshot.RepositoryID {
			mismatchedRepository = repository
			break
		}
	}
	if mismatchedRepository.ID == "" {
		t.Fatalf("snapshot repository %q was not discovered", mismatchedSnapshot.RepositoryID)
	}
	mismatched := mismatchedSnapshot.HeadRef
	mismatchedOID := mismatchedSnapshot.WorktreeOID
	if mismatchedOID == mismatchedSnapshot.HeadOID {
		t.Fatalf("test fixture did not provide a mismatched object for %s", mismatched)
	}
	gitRun(t, string(mismatchedRepository.MainPath), "update-ref", mismatched, mismatchedOID)
	m.reconcileArtifacts(ctx)
	diagnostics = m.artifactDiagnostics(ctx)
	// 名前ごと未知の ref と、DB が知る名前が別 OID を指す ref は別カテゴリに入る。前者は wx prune の対象、後者は resume の実害である。
	unknown = diagnostics["unknown_refs"].([]string)
	if len(unknown) != 1 || !strings.HasSuffix(unknown[0], ":refs/wx/recovery/foreign") {
		t.Fatalf("unknown ref category changed: %v", diagnostics)
	}
	mismatchedRefs := diagnostics["mismatched_refs"].([]string)
	if len(mismatchedRefs) != 1 || !containsString(mismatchedRefs, mismatchedSnapshot.RepositoryID+":"+mismatched) {
		t.Fatalf("mismatched recovery ref was not diagnosed: %v", diagnostics)
	}
	status, err = store.StatusDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Quarantine) != 2 || !containsString(quarantinePaths(status.Quarantine), mismatchedSnapshot.RepositoryID+":"+mismatched) {
		t.Fatalf("mismatched recovery ref was not quarantined: %+v", status.Quarantine)
	}
}

func quarantinePaths(items []state.QuarantineDiagnostic) []string {
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	return paths
}

func resolveTestBranches(ctx context.Context, m *Manager, w discovery.Workspace) ([]pool.Resolved, error) {
	return pool.ResolveBranches(ctx, m.git, w, nil)
}

func installRecoveryRefBarrier(t *testing.T) (started, proceed string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	started = filepath.Join(bin, "started")
	proceed = filepath.Join(bin, "proceed")
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
case " $* " in
  *" update-ref --create-reflog "*)
    if test ! -e "$WX_RECOVERY_REF_BARRIER_STARTED"; then
      : > "$WX_RECOVERY_REF_BARRIER_STARTED"
      while test ! -e "$WX_RECOVERY_REF_BARRIER_PROCEED"; do sleep 0.01; done
    fi
    ;;
esac
exec "$WX_RECOVERY_REF_BARRIER_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_RECOVERY_REF_BARRIER_STARTED", started)
	t.Setenv("WX_RECOVERY_REF_BARRIER_PROCEED", proceed)
	t.Setenv("WX_RECOVERY_REF_BARRIER_REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return started, proceed
}
