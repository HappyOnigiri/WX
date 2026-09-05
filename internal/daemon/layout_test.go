package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func registerTestRoot(t *testing.T, m *Manager, path string) string {
	// 手組みManagerでもslotを登録できるよう、root rowとin-memory IDをproductionと同じ形で用意する。
	t.Helper()
	id, err := tryRegisterTestRoot(m, path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func tryRegisterTestRoot(m *Manager, path string) (string, error) {
	if m.store == nil {
		return "", nil
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		return "", err
	}
	id, err := m.store.EnsureActiveRoot(context.Background(), path, identity)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.ensureRootStateLocked()
	m.rootIDs[path] = id

	m.roots[path] = true
	m.mu.Unlock()
	return id, nil
}

func testSlot(t *testing.T, m *Manager, workspaceID, slotID string, generation int, slotState string) state.Slot {
	t.Helper()
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return testSlotUnder(t, m, rootPath, rootID, workspaceID, slotID, generation, slotState)
}

func testSlotUnder(t *testing.T, m *Manager, rootPath, rootID, workspaceID, slotID string, generation int, slotState string) state.Slot {
	t.Helper()
	slot := testSlotRowUnder(t, rootPath, rootID, workspaceID, slotID, generation, slotState)
	if err := os.MkdirAll(slot.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(slot.Path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	slot.DirIdentity = identity
	return slot
}

func testSlotRow(t *testing.T, m *Manager, workspaceID, slotID string, generation int, slotState string) state.Slot {
	t.Helper()
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return testSlotRowUnder(t, rootPath, rootID, workspaceID, slotID, generation, slotState)
}

func testSlotRowUnder(t *testing.T, rootPath, rootID, workspaceID, slotID string, generation int, slotState string) state.Slot {
	// directoryを作らないfixtureなので、欠損・通常file・symlinkを個別にstageできる。
	t.Helper()
	relPath, err := slotRelPath(workspaceID, slotID, workspaceID == "")
	if err != nil {
		t.Fatal(err)
	}
	return state.Slot{
		ID: slotID, WorkspaceID: workspaceID, Generation: generation,
		RootID: rootID, RelPath: relPath, Path: filepath.Join(rootPath, relPath),
		State: slotState,
	}
}

func testSlotID(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, name)
}

func TestSlotRelPathGeneratesTheDocumentedLayout(t *testing.T) {
	t.Parallel()
	relPath, err := slotRelPath("wsp001", "slt001", false)
	if err != nil {
		t.Fatal(err)
	}
	if relPath != filepath.Join("wsp001", "slt001") {
		t.Fatalf("bound slot rel path=%q", relPath)
	}
	unbound, err := slotRelPath("", "slt002", true)
	if err != nil {
		t.Fatal(err)
	}
	if unbound != filepath.Join(unboundNamespace, "slt002") {
		t.Fatalf("unbound slot rel path=%q", unbound)
	}
	if _, err := slotRelPath("", "slt003", false); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("empty workspace id error=%v", err)
	}
	for _, slotID := range []string{"", ".", "..", "a/b", `a\b`, "_reserved"} {
		if _, err := slotRelPath("wsp001", slotID, false); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("slot id %q error=%v", slotID, err)
		}
	}
	for _, workspaceID := range []string{"", ".", "..", "a/b", "_unbound", "_recovery"} {
		if _, err := slotRelPath(workspaceID, "slt001", false); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("workspace id %q error=%v", workspaceID, err)
		}
	}
}

func TestValidateLayoutComponentReservesTheUnderscorePrefix(t *testing.T) {
	t.Parallel()
	if err := validateLayoutComponent("repository directory", "WX"); err != nil {
		t.Fatalf("plain name error=%v", err)
	}
	for _, value := range []string{"_unbound", "_recovery", "_anything"} {
		if err := validateLayoutComponent("workspace id", value); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("reserved prefix %q error=%v", value, err)
		}
	}
}

func TestLeasePathDependsOnWorkspaceKind(t *testing.T) {
	t.Parallel()
	slotPath := filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001")
	single := []state.SlotRepository{{RepositoryID: "r1", DirName: "WX"}}
	if got := leasePath(slotPath, "repository", single); got != filepath.Join(slotPath, "WX") {
		t.Fatalf("single-repository lease path=%q", got)
	}
	multi := []state.SlotRepository{{RepositoryID: "r1", DirName: "server"}, {RepositoryID: "r2", DirName: "client"}}
	if got := leasePath(slotPath, "multi_repository", multi); got != slotPath {
		t.Fatalf("multi-repository lease path=%q", got)
	}
	if got := leasePath(slotPath, "", nil); got != slotPath {
		t.Fatalf("unbound lease path=%q", got)
	}
	if got := leasePath(slotPath, "repository", []state.SlotRepository{{RepositoryID: "r1"}}); got != slotPath {
		t.Fatalf("nameless repository lease path=%q", got)
	}
}

func TestWorkspaceRecoveryExclusionsUseSlotDirectoryNames(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Workspaces["/src/bundle"] = config.Workspace{Link: []string{"shared"}}
	w := discoveryWorkspaceForExclusions()
	repos := []state.SlotRepository{{RepositoryID: "repo-1", DirName: "server"}}
	got := workspaceRecoveryExclusions(w, repos, cfg)
	want := map[string]bool{"server": true, ".wx-owner-repo-1": true, "shared": true}
	if len(got) != len(want) {
		t.Fatalf("exclusions=%v want keys %v", got, want)
	}
	for _, value := range got {
		if !want[value] {
			t.Fatalf("exclusions=%v contains unexpected %q", got, value)
		}
	}
	if containsString(got, w.Repositories[0].RelativePath) {
		t.Fatalf("exclusions=%v still use the source-relative repository path", got)
	}
	if got := workspaceRecoveryExclusions(w, []state.SlotRepository{{RepositoryID: "repo-1"}}, config.Defaults()); len(got) != 0 {
		t.Fatalf("nameless repository exclusions=%v", got)
	}
}

func TestNewSlotIDProducesShortIdentifiers(t *testing.T) {
	t.Parallel()
	id, err := newSlotID()
	if err != nil {
		t.Fatal(err)
	}
	if !domain.ValidShortID(id) {
		t.Fatalf("slot id=%q is not a short identifier", id)
	}
	if err := validateLayoutComponent("slot id", id); err != nil {
		t.Fatalf("slot id=%q is not a usable layout component: %v", id, err)
	}
}

func discoveryWorkspaceForExclusions() discovery.Workspace {
	return discovery.Workspace{
		ID: "wsp001", Root: "/src/bundle", Kind: "multi_repository",
		Repositories: []discovery.Repository{{ID: "repo-1", RelativePath: filepath.Join("group", "server")}},
	}
}

func testDirName(repo discovery.Repository, cfg config.Config) string {
	return workspace.RepositoryDirName(repo, cfg)
}

func slotAtPath(t *testing.T, m *Manager, workspaceID, slotID, slotPath string, generation int, slotState string) state.Slot {
	// 明示pathをSQLiteが持つroot IDとroot相対pathへ変換し、実directoryの準備は呼び出し側に任せる。
	t.Helper()
	rootPath, rootID, err := m.rootIDForPath(slotPath)
	if err != nil {
		t.Fatalf("slot path %s is outside every registered root generation: %v", slotPath, err)
	}
	relPath, ok := relativeWithinRoot(rootPath, slotPath)
	if !ok {
		t.Fatalf("slot path %s is not relative to root %s", slotPath, rootPath)
	}
	return state.Slot{
		ID: slotID, WorkspaceID: workspaceID, Generation: generation,
		RootID: rootID, RelPath: relPath, Path: filepath.Clean(slotPath),
		DirIdentity: existingDirIdentity(t, slotPath), State: slotState,
	}
}

func recordTestWorktreeIdentity(t *testing.T, store *state.Store, slotID, repositoryID, worktreePath string) {
	t.Helper()
	info, err := os.Lstat(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSlotRepositoryIdentity(context.Background(), slotID, repositoryID, identity); err != nil {
		t.Fatal(err)
	}
}

func existingDirIdentity(tb testing.TB, slotPath string) string {
	// 実在するphysical directoryだけがallocation後のslotと同じinode証拠を持つ。
	tb.Helper()
	info, err := os.Lstat(slotPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		tb.Fatal(err)
	}
	return identity
}

func storeSlotAt(tb testing.TB, store *state.Store, rootPath, workspaceID, slotID, slotPath string, generation int, slotState string) state.Slot {
	tb.Helper()
	rootPath = filepath.Clean(rootPath)
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		tb.Fatal(err)
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		tb.Fatal(err)
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		tb.Fatal(err)
	}
	rootID, err := store.EnsureActiveRoot(context.Background(), rootPath, identity)
	if err != nil {
		tb.Fatal(err)
	}
	relPath, ok := relativeWithinRoot(rootPath, slotPath)
	if !ok {
		tb.Fatalf("slot path %s is not relative to root %s", slotPath, rootPath)
	}
	return state.Slot{
		ID: slotID, WorkspaceID: workspaceID, Generation: generation,
		RootID: rootID, RelPath: relPath, Path: filepath.Clean(slotPath),
		DirIdentity: existingDirIdentity(tb, slotPath), State: slotState,
	}
}

func registerTestWorkspace(tb testing.TB, store *state.Store, w discovery.Workspace) discovery.Workspace {
	tb.Helper()
	registered, _, err := store.UpsertWorkspaceGeneration(context.Background(), w)
	if err != nil {
		tb.Fatal(err)
	}
	return registered
}

func escapingDirNameFor(tb testing.TB, root, slotPath string) string {
	// forged rowがroot外を指す場合を再現するため、slotの深さに合わせて".."を積む。
	tb.Helper()
	relative, ok := relativeWithinRoot(root, slotPath)
	if !ok {
		tb.Fatalf("slot path %s is not inside root %s", slotPath, root)
	}
	parts := make([]string, 0, 4)
	for range strings.Split(relative, string(filepath.Separator)) {
		parts = append(parts, "..")
	}
	return filepath.Join(append(parts, "..", "outside-repository")...)
}

func boundWorktreePath(t *testing.T, store *state.Store, slotID string) string {
	t.Helper()
	repos, err := store.SlotRepositories(context.Background(), slotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("slot %s has %d repositories, want exactly one", slotID, len(repos))
	}
	return repos[0].WorktreePath
}
