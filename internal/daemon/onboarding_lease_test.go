package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAndLeaseReportsFirstRepositoryOnlyOnce(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "cold"
		s.Config.Pool.WarmPerWorkspace = 0
	})
	repository := filepath.Join(f.Root, "repository")
	initGitRepo(t, repository)
	ctx := context.Background()
	first, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.FirstLeaseRepositories) != 1 || first.FirstLeaseRepositories[0].MainPath != repository {
		t.Fatalf("first lease repositories=%+v, want %s", first.FirstLeaseRepositories, repository)
	}
	second, err := f.Manager.ResolveAndLease(ctx, repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.FirstLeaseRepositories) != 0 {
		t.Fatalf("second lease repositories=%+v, want none", second.FirstLeaseRepositories)
	}
}
