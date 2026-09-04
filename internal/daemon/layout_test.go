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

// registerTestRoot registers path as a root generation the way New() does.
// slots.root_id is a NOT NULL foreign key into roots, so a hand-built Manager
// cannot insert a slot until its worktree root has a durable row and the
// in-memory registry knows the ID.
func registerTestRoot(t *testing.T, m *Manager, path string) string {
	t.Helper()
	id, err := tryRegisterTestRoot(m, path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// tryRegisterTestRoot is the non-fatal form. New() logs and carries on when
// its configured root is unavailable, so a Manager built for that case must
// not fail the test during construction.
func tryRegisterTestRoot(m *Manager, path string) (string, error) {
	if m.store == nil {
		// Some tests build a Manager with no store at all, to exercise
		// descriptor bookkeeping that never reaches SQLite.
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
	m.activeRootID = id
	m.roots[path] = true
	m.mu.Unlock()
	return id, nil
}

// testSlot builds a slot row located under the manager's active root the way
// allocate does, and creates the slot directory. It returns the row so the
// caller can pass it to CreateStandby/CreateSlotSession and read Path for
// filesystem setup.
func testSlot(t *testing.T, m *Manager, workspaceID, slotID string, generation int, slotState string) state.Slot {
	t.Helper()
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return testSlotUnder(t, m, rootPath, rootID, workspaceID, slotID, generation, slotState)
}

// testSlotUnder is testSlot for a specific root generation, used by the tests
// that keep two roots alive at once.
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

// testSlotRow builds the slot row without creating its directory, for tests
// that stage the slot directory themselves (absent, a regular file, or a
// symlink). DirIdentity is left empty because there is no inode to record.
func testSlotRow(t *testing.T, m *Manager, workspaceID, slotID string, generation int, slotState string) state.Slot {
	t.Helper()
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return testSlotRowUnder(t, rootPath, rootID, workspaceID, slotID, generation, slotState)
}

func testSlotRowUnder(t *testing.T, rootPath, rootID, workspaceID, slotID string, generation int, slotState string) state.Slot {
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

// testSlotID returns a layout-safe slot identifier derived from name. Slot IDs
// are only required to be one safe path component, so a readable label keeps
// failures diagnosable where a random short ID would not.
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
	// An unbound slot has no workspace yet, so the workspace component is not
	// consulted at all rather than being validated as empty.
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
	if err := validateLayoutComponent("repository directory", "WX"); err != nil {
		t.Fatalf("plain name error=%v", err)
	}
	// _unbound and _recovery are wx's own namespaces; the orphan scan reads
	// every other top-level entry as a workspace ID, so nothing else may take
	// that prefix.
	for _, value := range []string{"_unbound", "_recovery", "_anything"} {
		if err := validateLayoutComponent("workspace id", value); !errors.Is(err, state.ErrOwnership) {
			t.Errorf("reserved prefix %q error=%v", value, err)
		}
	}
}

func TestLeasePathDependsOnWorkspaceKind(t *testing.T) {
	slotPath := filepath.Join(string(filepath.Separator)+"wx", "wsp001", "slt001")
	single := []state.SlotRepository{{RepositoryID: "r1", DirName: "WX"}}
	if got := leasePath(slotPath, "repository", single); got != filepath.Join(slotPath, "WX") {
		t.Fatalf("single-repository lease path=%q", got)
	}
	multi := []state.SlotRepository{{RepositoryID: "r1", DirName: "server"}, {RepositoryID: "r2", DirName: "client"}}
	if got := leasePath(slotPath, "multi_repository", multi); got != slotPath {
		t.Fatalf("multi-repository lease path=%q", got)
	}
	// An _unbound slot has no repositories yet, so it leases the slot
	// directory and the worktree appears one level below the agent's CWD once
	// the workspace is bound.
	if got := leasePath(slotPath, "", nil); got != slotPath {
		t.Fatalf("unbound lease path=%q", got)
	}
	// A repository row without a recorded directory name cannot name a
	// worktree, so the slot directory is used rather than a path ending in "/".
	if got := leasePath(slotPath, "repository", []state.SlotRepository{{RepositoryID: "r1"}}); got != slotPath {
		t.Fatalf("nameless repository lease path=%q", got)
	}
}

func TestWorkspaceRecoveryExclusionsUseSlotDirectoryNames(t *testing.T) {
	cfg := config.Defaults()
	cfg.Workspaces["/src/bundle"] = config.Workspace{Link: []string{"shared"}}
	w := discoveryWorkspaceForExclusions()
	// The source-relative path is deliberately different from the slot
	// directory name: the bundle root is the slot directory, so an exclusion
	// naming "group/server" would neither protect nor prune the real worktree.
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
	// A repository with no recorded directory name contributes nothing rather
	// than an empty exclusion that would match the bundle root itself.
	if got := workspaceRecoveryExclusions(w, []state.SlotRepository{{RepositoryID: "repo-1"}}, config.Defaults()); len(got) != 0 {
		t.Fatalf("nameless repository exclusions=%v", got)
	}
}

func TestNewSlotIDProducesShortIdentifiers(t *testing.T) {
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

// testDirName is the directory name slotRepos would choose for repo inside a
// slot. A hand-written fingerprint has to agree with it, because Fingerprint
// hashes the chosen name.
func testDirName(repo discovery.Repository, cfg config.Config) string {
	return workspace.RepositoryDirName(repo, cfg)
}

// slotAtPath describes a slot located at an explicit absolute path inside a
// registered root. slots.rel_path is plain text, so a test may still place a
// slot wherever it needs one; this only converts the absolute path into the
// root generation plus root-relative pair SQLite now records. The directory
// itself is left to the caller.
func slotAtPath(t *testing.T, m *Manager, workspaceID, slotID, slotPath string, generation int, slotState string) state.Slot {
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

// existingDirIdentity records the slot directory's inode the way allocation
// does, so a hand-built row carries the same evidence a real one would. The
// tests that stage an absent slot, a regular file, or a symlink get an empty
// identity, matching the row a slot with no directory of its own would have.
func existingDirIdentity(tb testing.TB, slotPath string) string {
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

// storeSlotAt is slotAtPath for the tests that have no Manager yet (or a
// benchmark, which has no *testing.T at all). It registers rootPath as a root
// generation directly on the store and derives the root-relative location
// from slotPath.
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

// registerTestWorkspace upserts w and returns it with the durable ID the
// store assigned. Workspace identity belongs to the store, not the caller, so
// a test cannot pin an ID by writing one into the literal.
func registerTestWorkspace(tb testing.TB, store *state.Store, w discovery.Workspace) discovery.Workspace {
	tb.Helper()
	registered, _, err := store.UpsertWorkspaceGeneration(context.Background(), w)
	if err != nil {
		tb.Fatal(err)
	}
	return registered
}

// escapingDirNameFor returns a slot_repositories.dir_name that resolves
// outside root once joined to slotPath. dir_name is plain text, so this is
// the only remaining way a test can express a recorded worktree wx does not
// own; every name wx itself writes is a single component. The traversal depth
// is derived from the slot's own depth, because filepath.Join cleans ".."
// away and a fixed count would stay inside the root for a deeper slot.
func escapingDirNameFor(tb testing.TB, root, slotPath string) string {
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

// boundWorktreePath returns the repository worktree inside a slot. An
// _unbound lease hands out the slot directory itself, because the repository
// name is unknowable before the agent session reveals its workspace, so for a
// single-repository workspace the worktree appears one level below the
// agent's initial CWD once the SessionStart hook binds it.
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
