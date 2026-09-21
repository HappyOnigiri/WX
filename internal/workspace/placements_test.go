package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSortedPlacementsUsesRepositoryThenPath(t *testing.T) {
	t.Parallel()
	placements := sortedPlacements(map[string]state.Placement{
		"b":   {RepositoryID: "b", RelativePath: "z"},
		"a-z": {RepositoryID: "a", RelativePath: "z"},
		"a-y": {RepositoryID: "a", RelativePath: "y"},
	})
	if len(placements) != 3 {
		t.Fatalf("placements=%+v", placements)
	}
	want := []struct {
		repository string
		path       string
	}{
		{repository: "a", path: "y"},
		{repository: "a", path: "z"},
		{repository: "b", path: "z"},
	}
	for index, expected := range want {
		if placements[index].RepositoryID != expected.repository || placements[index].RelativePath != expected.path {
			t.Fatalf("placements[%d]=%+v want repository=%q path=%q", index, placements[index], expected.repository, expected.path)
		}
	}
}

// TestTrackedPathsAtKeepsTreeBoundaries は ls-tree の末尾 NUL を空の path として登録せず、
// Git の tree 読み取り失敗も空の tree へ読み替えないことを確認する。
func TestTrackedPathsAtKeepsTreeBoundaries(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.Mkdir(filepath.Join(repository, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "nested", "file.txt"), []byte("nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "tracked paths")
	oid := gitOutput(t, repository, "rev-parse", "HEAD")

	preparer := Preparer{Git: &gitx.Runner{Timeout: 10 * time.Second}}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	tracked, err := preparer.trackedPathsAt(context.Background(), repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked) != 2 || !tracked["tracked.txt"] || !tracked["nested/file.txt"] {
		t.Fatalf("tracked paths=%v, want the two tree entries", tracked)
	}
	if tracked[""] || tracked["."] {
		t.Fatalf("tracked paths=%v, trailing NUL must not create an empty path", tracked)
	}

	if _, err := preparer.trackedPathsAt(context.Background(), repo, "missing-revision"); err == nil {
		t.Fatal("missing tree revision was accepted as an empty tree")
	}
}

// TestRootPlacementsListsCopiesAndLinks は workspace root の配置計画が、copy を file 単位に開き
// link を1件として返すことを確認する。standby の UPDATE はこの計画を旧配置履歴と比較する。
func TestRootPlacementsListsCopiesAndLinks(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	for path, content := range map[string]string{"AGENTS.md": "root rules", "configs/app.yml": "config", "shared/file": "linked"} {
		full := filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	placements, err := RootPlacements(source, RootRulesFromConfig(config.Workspace{Copy: []string{"configs"}, Link: []string{"shared"}}))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, placement := range placements {
		if placement.RepositoryID != "" {
			t.Fatalf("workspace root placement carries a repository: %+v", placement)
		}
		kinds[placement.RelativePath] = placement.Kind
	}
	if kinds["AGENTS.md"] != "copy" || kinds["configs/app.yml"] != "copy" || kinds["shared"] != "link" {
		t.Fatalf("placements=%+v", placements)
	}
	if _, ok := kinds["configs"]; ok {
		t.Fatalf("copied directory recorded as a placement: %+v", placements)
	}
}

// TestRepositoryPlacementsExpandLinkGlobs は、standby の UPDATE が比べる desired 側の link 計画が
// 展開後の match で作られ、match の増減がそのまま計画の増減になることを確認する。
// 展開順は safeGlob が各階層で整列するため決定的で、履歴との比較が回ごとに揺れない。
func TestRepositoryPlacementsExpandLinkGlobs(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	worktreeRoot := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("local-*\n.worktreelink\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("local-*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "ignore local rules")
	oid := gitOutput(t, repository, "rev-parse", "HEAD")
	for _, name := range []string{"local-b", "local-a"} {
		if err := os.Mkdir(filepath.Join(repository, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: 10 * time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}

	linkPaths := func() []string {
		t.Helper()
		placements, err := preparer.RepositoryPlacements(context.Background(), repo, oid)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, placement := range placements {
			if placement.Kind == "link" {
				out = append(out, placement.RelativePath)
			}
		}
		return out
	}
	if got := linkPaths(); !slices.Equal(got, []string{"local-a", "local-b"}) {
		t.Fatalf("link placements=%v want the expanded matches in order", got)
	}
	if err := os.Mkdir(filepath.Join(repository, "local-c"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := linkPaths(); !slices.Equal(got, []string{"local-a", "local-b", "local-c"}) {
		t.Fatalf("link placements=%v want a new match added", got)
	}
	if err := os.RemoveAll(filepath.Join(repository, "local-b")); err != nil {
		t.Fatal(err)
	}
	if got := linkPaths(); !slices.Equal(got, []string{"local-a", "local-c"}) {
		t.Fatalf("link placements=%v want the removed match dropped", got)
	}
}
