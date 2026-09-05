package archive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func orphanRefRepository(t *testing.T) (*Manager, discovery.Repository, string) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	mustMkdir(t, repository)
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	gitCommand(t, repository, "commit", "--allow-empty", "-m", "initial")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	repo := discovery.Repository{
		ID:        "repository",
		MainPath:  domain.CanonicalPath(repository),
		CommonDir: domain.CanonicalPath(filepath.Join(repository, ".git")),
	}
	return &Manager{Git: &gitx.Runner{Timeout: 10 * time.Second}}, repo, head
}

func TestClassifyOrphanRefsSeparatesRefsThatKeepObjectsAlive(t *testing.T) {
	t.Parallel()
	manager, repo, head := orphanRefRepository(t)
	ctx := context.Background()
	exclusive := gitCommand(t, string(repo.MainPath), "commit-tree", head+"^{tree}", "-p", head, "-m", "unreachable")
	gitCommand(t, string(repo.MainPath), "update-ref", "refs/wx/recovery/reachable", head)
	gitCommand(t, string(repo.MainPath), "update-ref", "refs/wx/recovery/exclusive", exclusive)

	safe, unsafe, err := manager.ClassifyOrphanRefs(ctx, repo, []string{"refs/wx/recovery/reachable", "refs/wx/recovery/exclusive", "refs/wx/recovery/absent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(safe) != 1 || safe[0].Ref != "refs/wx/recovery/reachable" || safe[0].OID != head {
		t.Fatalf("safe=%+v", safe)
	}
	if len(unsafe) != 1 || unsafe[0].Ref != "refs/wx/recovery/exclusive" || unsafe[0].UnreachableObjects != 1 {
		t.Fatalf("unsafe=%+v", unsafe)
	}
}

func TestDeleteOrphanRefsRefusesRefsThatMovedAfterClassification(t *testing.T) {
	t.Parallel()
	manager, repo, head := orphanRefRepository(t)
	ctx := context.Background()
	moved := gitCommand(t, string(repo.MainPath), "commit-tree", head+"^{tree}", "-p", head, "-m", "moved")
	gitCommand(t, string(repo.MainPath), "update-ref", "refs/wx/recovery/moved", moved)

	err := manager.DeleteOrphanRefs(ctx, repo, []OrphanRef{{Ref: "refs/wx/recovery/moved", OID: head}})
	if err == nil {
		t.Fatal("stale expected OID was accepted")
	}
	if got := gitCommand(t, string(repo.MainPath), "rev-parse", "refs/wx/recovery/moved"); got != moved {
		t.Fatalf("refused deletion still changed the ref: %s", got)
	}
}

func TestDeleteOrphanRefsRemovesEveryRefInOneBatch(t *testing.T) {
	t.Parallel()
	manager, repo, head := orphanRefRepository(t)
	ctx := context.Background()
	gitCommand(t, string(repo.MainPath), "update-ref", "refs/wx/recovery/a", head)
	gitCommand(t, string(repo.MainPath), "update-ref", "refs/wx/recovery/b", head)

	refs := []OrphanRef{{Ref: "refs/wx/recovery/a", OID: head}, {Ref: "refs/wx/recovery/b", OID: head}}
	if err := manager.DeleteOrphanRefs(ctx, repo, refs); err != nil {
		t.Fatal(err)
	}
	if listed := gitCommand(t, string(repo.MainPath), "for-each-ref", "--format=%(refname)", "refs/wx/recovery"); strings.TrimSpace(listed) != "" {
		t.Fatalf("refs survived deletion: %q", listed)
	}
	if err := manager.DeleteOrphanRefs(ctx, repo, nil); err != nil {
		t.Fatalf("empty deletion failed: %v", err)
	}
}
