package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// orphanRefFixture は、孤児 recovery ref だけを持つ最小構成の Manager と登録済み repository を用意する。
type orphanRefFixture struct {
	manager      *Manager
	store        *state.Store
	workspaceID  string
	repository   string
	repositoryID string
	logs         *bytes.Buffer
}

// saveSnapshot は snapshots の外部キーを満たす session を用意してから archive 済み snapshot を書く。
func (f *orphanRefFixture) saveSnapshot(t *testing.T, snapshot state.Snapshot) {
	t.Helper()
	ctx := context.Background()
	slot := testSlotRow(t, f.manager, f.workspaceID, snapshot.SessionID, 1, "SNAPSHOTTED")
	session := state.Session{ID: snapshot.SessionID, WorkspaceID: f.workspaceID, SlotID: snapshot.SessionID, State: "ARCHIVED", AgentKind: "codex", TokenHash: state.HashToken("token")}
	if _, err := f.store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
}

func newOrphanRefFixture(t *testing.T) *orphanRefFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.WarmPerWorkspace = 0
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := testManager(t, cfg, store)
	t.Cleanup(m.Close)
	m.git.SetTimeout(10 * time.Second)
	logs := &bytes.Buffer{}
	m.log = slog.New(slog.NewTextHandler(logs, nil))
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	if len(w.Repositories) != 1 {
		t.Fatalf("workspace repositories=%d, want 1", len(w.Repositories))
	}
	return &orphanRefFixture{manager: m, store: store, workspaceID: string(w.ID), repository: repository, repositoryID: string(w.Repositories[0].ID), logs: logs}
}

// orphanWarnings は集約された unknown_refs の WARN 行数を数え、バッファを次の周のために空にする。
func (f *orphanRefFixture) orphanWarnings() int {
	count := 0
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "category=unknown_refs") {
			count++
		}
	}
	f.logs.Reset()
	return count
}

func (f *orphanRefFixture) quarantinePaths(t *testing.T) []string {
	t.Helper()
	diagnostics, err := f.store.StatusDiagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(diagnostics.Quarantine))
	for _, item := range diagnostics.Quarantine {
		paths = append(paths, item.Path)
	}
	return paths
}

func TestReconcileWarnsOncePerRepositoryForOrphanRecoveryRefs(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := newOrphanRefFixture(t)
	ctx := context.Background()
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/orphan-a", head)
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/orphan-b", head)

	f.manager.reconcileArtifacts(ctx)
	if got := f.orphanWarnings(); got != 1 {
		t.Fatalf("first reconcile emitted %d aggregated warnings, want 1", got)
	}
	if paths := f.quarantinePaths(t); len(paths) != 2 {
		t.Fatalf("orphan refs were not recorded: %v", paths)
	}

	f.manager.reconcileArtifacts(ctx)
	if got := f.orphanWarnings(); got != 0 {
		t.Fatalf("second reconcile repeated %d warnings, want 0", got)
	}

	// 手で ref を消したら記録も消え、戻せば再び 1 回だけ警告が出る。
	gitRun(t, f.repository, "update-ref", "-d", "refs/wx/recovery/orphan-b", head)
	f.manager.reconcileArtifacts(ctx)
	if got := f.orphanWarnings(); got != 0 {
		t.Fatalf("resolved orphan ref emitted %d warnings, want 0", got)
	}
	if paths := f.quarantinePaths(t); len(paths) != 1 || !strings.HasSuffix(paths[0], ":refs/wx/recovery/orphan-a") {
		t.Fatalf("resolved orphan record was not pruned: %v", paths)
	}
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/orphan-b", head)
	f.manager.reconcileArtifacts(ctx)
	if got := f.orphanWarnings(); got != 1 {
		t.Fatalf("reappearing orphan ref emitted %d warnings, want 1", got)
	}
}

func TestReconcileKeepsWarningAboutMismatchedRecoveryRefs(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := newOrphanRefFixture(t)
	ctx := context.Background()
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	tree := gitOutput(t, f.repository, "rev-parse", "HEAD^{tree}")
	// head ref だけを期待外の OID へ動かす。worktree ref を欠くと snapshot 自体が隔離され、mismatch の観測にならない。
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/known/head", tree)
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/known/worktree", head)
	f.saveSnapshot(t, state.Snapshot{
		ID: "mismatched", SessionID: "mismatchedsession", RepositoryID: f.repositoryID,
		HeadOID: head, HeadRef: "refs/wx/recovery/known/head", IndexTreeOID: tree,
		WorktreeOID: head, WorktreeRef: "refs/wx/recovery/known/worktree", Status: "ARCHIVED",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	})
	diagnostics := f.manager.artifactDiagnostics(ctx)
	if got := diagnostics["mismatched_refs"].([]string); len(got) != 1 || !strings.HasSuffix(got[0], ":refs/wx/recovery/known/head") {
		t.Fatalf("mismatched ref diagnostics=%v", diagnostics)
	}
	if got := diagnostics["unknown_refs"].([]string); len(got) != 0 {
		t.Fatalf("mismatched ref leaked into unknown_refs: %v", got)
	}
	mismatchWarnings := func() int {
		count := 0
		for _, line := range strings.Split(f.logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "category=mismatched_refs") {
				count++
			}
		}
		f.logs.Reset()
		return count
	}
	f.manager.reconcileArtifacts(ctx)
	if got := mismatchWarnings(); got != 1 {
		t.Fatalf("first reconcile emitted %d mismatch warnings, want 1", got)
	}
	// OID 不一致は resume が誤った内容を復元する実害なので、集約も抑制もせず毎周報告する。
	f.manager.reconcileArtifacts(ctx)
	if got := mismatchWarnings(); got != 1 {
		t.Fatalf("second reconcile emitted %d mismatch warnings, want 1", got)
	}
}

func TestPruneRecoveryRefsDeletesOnlyProvablySafeRefs(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := newOrphanRefFixture(t)
	ctx := context.Background()
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	// reachable は main から到達できる commit を指すので、消しても失われる object はない。
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/reachable", head)
	// exclusive は他のどの ref からも到達できない commit object を持つ。
	exclusive := makeUnreachableCommit(t, f.repository, head)
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/exclusive", exclusive)
	// DB が期待する ref は prune の対象集合に入らない。
	gitRun(t, f.repository, "update-ref", "refs/wx/recovery/expected", head)
	f.saveSnapshot(t, state.Snapshot{
		ID: "expected", SessionID: "expectedsession", RepositoryID: f.repositoryID,
		HeadOID: head, HeadRef: "refs/wx/recovery/expected", IndexTreeOID: "",
		WorktreeOID: head, WorktreeRef: "refs/wx/recovery/expected", Status: "ARCHIVED",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	})
	f.manager.reconcileArtifacts(ctx)

	dry, err := f.manager.PruneRecoveryRefs(ctx, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted != 1 || dry.Kept != 1 || len(dry.Errors) != 0 {
		t.Fatalf("dry run result=%+v", dry)
	}
	if got := gitOutput(t, f.repository, "rev-parse", "refs/wx/recovery/reachable"); got != head {
		t.Fatalf("dry run deleted refs/wx/recovery/reachable")
	}
	if len(f.quarantinePaths(t)) != 3 {
		t.Fatalf("dry run changed quarantine records: %v", f.quarantinePaths(t))
	}

	result, err := f.manager.PruneRecoveryRefs(ctx, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.Kept != 1 || len(result.Errors) != 0 {
		t.Fatalf("prune result=%+v", result)
	}
	if len(result.KeptRefs) != 1 || result.KeptRefs[0].Ref != "refs/wx/recovery/exclusive" || result.KeptRefs[0].UnreachableObjects == 0 {
		t.Fatalf("kept refs=%+v", result.KeptRefs)
	}
	refs := gitOutput(t, f.repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
	if strings.Contains(refs, "refs/wx/recovery/reachable") {
		t.Fatalf("safe orphan ref survived prune: %s", refs)
	}
	if !strings.Contains(refs, "refs/wx/recovery/exclusive") || !strings.Contains(refs, "refs/wx/recovery/expected") {
		t.Fatalf("prune removed a ref it must not touch: %s", refs)
	}
	if paths := f.quarantinePaths(t); len(paths) != 2 {
		t.Fatalf("prune did not forget the deleted ref: %v", paths)
	}

	all, err := f.manager.PruneRecoveryRefs(ctx, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if all.Deleted != 1 || all.Kept != 0 || len(all.Errors) != 0 {
		t.Fatalf("prune --all result=%+v", all)
	}
	refs = gitOutput(t, f.repository, "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
	if strings.Contains(refs, "refs/wx/recovery/exclusive") {
		t.Fatalf("prune --all kept an unsafe ref: %s", refs)
	}
	if !strings.Contains(refs, "refs/wx/recovery/expected") {
		t.Fatalf("prune --all touched a ref the database expects: %s", refs)
	}
}

// makeUnreachableCommit は、どのブランチからも到達できない commit object を作る。
// tree は parent と共有するので、固有なのは commit 自身の 1 object だけになる。
func makeUnreachableCommit(t *testing.T, repository, parent string) string {
	t.Helper()
	tree := gitOutput(t, repository, "rev-parse", parent+"^{tree}")
	return gitOutput(t, repository, "commit-tree", tree, "-p", parent, "-m", "unreachable")
}
