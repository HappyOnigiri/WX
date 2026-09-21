package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// TestWorkspaceDefaultBranchSurvivesOtherWorkspaceRegistration は、同じ repository を含む
// 別 workspace を後から登録しても、先に登録した workspace の既定 branch が変わらないことを固定する。
// 既定 branch を repositories の共有 row に置くと、後の登録が先の workspace の branch を奪い、
// standby と貸出が別 workspace の branch を materialize する。
// commentlint:allow-long -- 共有 row へ戻したときに起きる実害を回帰テストの意図として残す
func TestWorkspaceDefaultBranchSurvivesOtherWorkspaceRegistration(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	repoPath := domain.CanonicalPath(filepath.Join(root, "repo"))
	commonDir := domain.CanonicalPath(filepath.Join(root, "repo", ".git"))
	repository := func(relative, branch string) discovery.Repository {
		return discovery.Repository{ID: "repository", MainPath: repoPath, CommonDir: commonDir, RelativePath: relative, DefaultBranch: branch}
	}
	single := discovery.Workspace{ID: "single", Root: repoPath, Kind: "repository", Repositories: []discovery.Repository{repository(".", "release")}}
	multi := discovery.Workspace{ID: "multi", Root: domain.CanonicalPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{repository("repo", "main")}}

	registeredSingle, _, err := store.UpsertWorkspaceGeneration(ctx, single)
	if err != nil {
		t.Fatal(err)
	}
	registeredMulti, _, err := store.UpsertWorkspaceGeneration(ctx, multi)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Workspace(ctx, string(registeredSingle.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Repositories) != 1 {
		t.Fatalf("repositories=%+v", stored.Repositories)
	}
	if got := stored.Repositories[0].DefaultBranch; got != "release" {
		t.Fatalf("default branch=%q, want release", got)
	}
	// 逆順の登録でも、後から登録した側が先に登録した側を上書きしないことを確かめる。
	if _, _, err := store.UpsertWorkspaceGeneration(ctx, single); err != nil {
		t.Fatal(err)
	}
	storedMulti, err := store.Workspace(ctx, string(registeredMulti.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(storedMulti.Repositories) != 1 || storedMulti.Repositories[0].DefaultBranch != "main" {
		t.Fatalf("multi repositories=%+v, want default branch main", storedMulti.Repositories)
	}
}
