package state

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestFirstLeaseRepositoriesReturnsOnlyUnleasedMembers(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	w := discovery.Workspace{
		ID: "workspace", Root: "/workspace", Kind: "multi_repository",
		Repositories: []discovery.Repository{
			{ID: domain.RepositoryID("a"), MainPath: "/workspace/a", CommonDir: "/git/a", RelativePath: "a"},
			{ID: domain.RepositoryID("b"), MainPath: "/workspace/b", CommonDir: "/git/b", RelativePath: "b"},
		},
	}
	w, _, err = store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE repositories SET last_leased_at=? WHERE id='a'`, now()); err != nil {
		t.Fatal(err)
	}
	got, err := store.FirstLeaseRepositories(ctx, string(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RelativePath != "b" || got[0].MainPath != "/workspace/b" {
		t.Fatalf("repositories=%+v", got)
	}
}
