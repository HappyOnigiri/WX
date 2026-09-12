package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 未登録のworkspaceでもcwdの解決結果は返し、登録後はslot pathとsessionが同じIDの下に並ぶことを検査する。
func TestWorkspaceScopeReportsRegistrationSlotPathsAndSessions(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()

	unregistered, err := m.WorkspaceScope(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if unregistered.Registered {
		t.Fatalf("unobserved workspace reported as registered: %+v", unregistered)
	}
	if unregistered.Kind != "repository" || filepath.Clean(unregistered.Root) != filepath.Clean(repository) {
		t.Fatalf("unregistered scope kind=%q root=%q", unregistered.Kind, unregistered.Root)
	}
	if len(unregistered.SlotPaths) != 0 || len(unregistered.Sessions) != 0 {
		t.Fatalf("unregistered scope carried state: %+v", unregistered)
	}

	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	slot := testSlot(t, m, string(w.ID), "scope-slot", 1, "LEASED")
	session := state.Session{ID: "scope-session", WorkspaceID: string(w.ID), SlotID: "scope-slot", State: "ACTIVE", AgentKind: "codex", TokenHash: state.HashToken("scope-session")}
	if _, err := store.CreateSlotSession(ctx, slot, nil, session, ""); err != nil {
		t.Fatal(err)
	}

	registered, err := m.WorkspaceScope(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !registered.Registered || registered.WorkspaceID != string(w.ID) {
		t.Fatalf("registered scope=%+v, want workspace %s", registered, w.ID)
	}
	if len(registered.SlotPaths) != 1 || filepath.Clean(registered.SlotPaths[0]) != filepath.Clean(slot.Path) {
		t.Fatalf("scope slot paths=%v, want %s", registered.SlotPaths, slot.Path)
	}
	if len(registered.Sessions) != 1 {
		t.Fatalf("scope sessions=%+v, want exactly one", registered.Sessions)
	}
	if got := registered.Sessions[0]; got.ID != "scope-session" || got.Agent != "codex" || got.State != "ACTIVE" {
		t.Fatalf("scope session=%+v", got)
	}
}

// cwdがどのrepositoryにも属さないときは、空のscopeを返さずエラーで止まる。
func TestWorkspaceScopeFailsOutsideAnyRepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if scope, err := m.WorkspaceScope(context.Background(), outside); err == nil {
		t.Fatalf("scope outside every repository succeeded: %+v", scope)
	}
}

// 登録済みmulti-repository rootのscopeは配下のwalkなしで返し、rootの子repositoryは従来のrepository規則を保つ。
func TestWorkspaceScopeResolvesRegisteredMultiRootWithoutWalking(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	multiRoot := filepath.Join(root, "multi")
	member := filepath.Join(multiRoot, "member")
	initGitRepo(t, member)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	// walkは1 entryも許さない。高速経路が使われなければ探索はmax_entriesで失敗する。
	cfg.Discovery.MaxEntries = 1
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	registered := registerTestWorkspace(t, store, multiWorkspaceFixture(t, multiRoot, member))

	// 無関係なdirectoryをいくら足してもwalkが起きないため、scope取得のコストは変わらない。
	for i := range 20 {
		if err := os.MkdirAll(filepath.Join(multiRoot, fmt.Sprintf("noise-%d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := m.WorkspaceScope(ctx, multiRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Registered || scope.WorkspaceID != string(registered.ID) || scope.Kind != "multi_repository" {
		t.Fatalf("multi root scope=%+v, want registered workspace %s", scope, registered.ID)
	}
	if scope.Root != string(canonicalTestPath(t, multiRoot)) {
		t.Fatalf("multi root scope root=%q, want %q", scope.Root, multiRoot)
	}

	// 子repositoryは所属規則を変えず、multi workspaceへprefixで結合しない。
	child, err := m.WorkspaceScope(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if child.Registered || child.Kind != "repository" || child.Root != string(canonicalTestPath(t, member)) {
		t.Fatalf("member repository scope=%+v, want an unregistered repository workspace at %s", child, member)
	}

	// 未登録のmulti rootは通常の探索へ戻るため、max_entriesの制限がそのまま観測できる。
	sibling := filepath.Join(root, "unregistered")
	if err := os.MkdirAll(filepath.Join(sibling, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if scope, err := m.WorkspaceScope(ctx, sibling); err == nil {
		t.Fatalf("unregistered multi root scope=%+v, want the discovery walk to run", scope)
	}
}

// 畳んだslotの記録は実体がなくても同じworkspaceへ解決し、実体が別repositoryへ置き換わったときは記録へ結合しない。
func TestWorkspaceScopeUsesSlotRecordsUnlessTheEntryBecameAnotherRepository(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	discovered, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	registered := registerTestWorkspace(t, store, discovered)
	slot := testSlot(t, m, string(registered.ID), "scope-slot", 1, "ARCHIVED")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}

	// 実体を失ったworktreeの下のcwdでも、記録から同じworkspaceへ戻る。
	retired := filepath.Join(slot.Path, "repo", "internal")
	scope, err := m.WorkspaceScope(ctx, retired)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Registered || scope.WorkspaceID != string(registered.ID) {
		t.Fatalf("retired slot path scope=%+v, want registered workspace %s", scope, registered.ID)
	}

	// slot pathが別repositoryへ置き換わったら、記録ではなく現在のGit identityで解決する。
	foreign := filepath.Join(slot.Path, "foreign")
	initGitRepo(t, foreign)
	replaced, err := m.WorkspaceScope(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Registered || replaced.WorkspaceID == string(registered.ID) {
		t.Fatalf("replaced slot entry scope=%+v, want the current Git identity", replaced)
	}
	if replaced.Root != string(canonicalTestPath(t, foreign)) {
		t.Fatalf("replaced slot entry root=%q, want %q", replaced.Root, foreign)
	}
}

// main worktreeが移動しても同じworkspace IDを保ち、rootはDBの記録ではなくGitの現在値を返す。
func TestWorkspaceScopeFollowsRelocatedMainWorktree(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	common := filepath.Join(root, "common.git")
	oldMain := filepath.Join(root, "old-main")
	newMain := filepath.Join(root, "new-main")
	initGitRepo(t, source)
	gitRun(t, root, "clone", "--bare", source, common)
	gitRun(t, common, "worktree", "add", oldMain, "main")

	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	m.git.SetTimeout(10 * time.Second)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	discovered, err := discoverer.Resolve(ctx, oldMain)
	if err != nil {
		t.Fatal(err)
	}
	registered := registerTestWorkspace(t, store, discovered)
	gitRun(t, common, "worktree", "move", oldMain, newMain)

	scope, err := m.WorkspaceScope(ctx, newMain)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Registered || scope.WorkspaceID != string(registered.ID) {
		t.Fatalf("relocated main scope=%+v, want registered workspace %s", scope, registered.ID)
	}
	if scope.Root != string(canonicalTestPath(t, newMain)) {
		t.Fatalf("relocated main scope root=%q, want %q", scope.Root, newMain)
	}
	if roots, err := store.WorkspaceRoots(ctx); err != nil || len(roots) != 1 || roots[0] != string(discovered.Root) {
		t.Fatalf("registered roots=%v err=%v, scope resolution must not update the registry", roots, err)
	}
}

func multiWorkspaceFixture(t *testing.T, root, member string) discovery.Workspace {
	t.Helper()
	common := canonicalTestPath(t, filepath.Join(member, ".git"))
	return discovery.Workspace{
		Root: canonicalTestPath(t, root), Kind: "multi_repository",
		Repositories: []discovery.Repository{{
			ID:        domain.RepositoryID(domain.StableID(string(common))),
			MainPath:  canonicalTestPath(t, member),
			CommonDir: common, RelativePath: "member", DefaultBranch: "main",
		}},
	}
}

func canonicalTestPath(t *testing.T, path string) domain.CanonicalPath {
	t.Helper()
	canonical, err := domain.Canonicalize(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
