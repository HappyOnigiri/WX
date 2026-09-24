package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

func TestSetupCheckRepositoriesAddsSlotDirectoryNames(t *testing.T) {
	t.Parallel()
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: domain.RepositoryID("repo"), MainPath: "/source/repo", RelativePath: "."}}}
	got := setupCheckRepositories(w, config.Defaults())
	if len(got) != 1 || got[0].DirName == "" {
		t.Fatalf("repositories=%+v", got)
	}
}

func TestResolveSetupOnboardingDoesNotCreateState(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := manualManagerFixture(t, func(*managerFixtureSetup) {})
	repository := filepath.Join(f.Root, "repository")
	initGitRepo(t, repository)
	resolved, err := f.Manager.ResolveSetupOnboarding(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SourceWorkspace != repository || len(resolved.Repositories) != 1 || resolved.Repositories[0].MainPath != repository {
		t.Fatalf("resolved=%+v", resolved)
	}
	slots, err := f.Store.ListSlots(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatalf("preflight created slots: %+v", slots)
	}
}
