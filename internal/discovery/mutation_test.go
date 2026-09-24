package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

func TestMutationMultiWorkspaceIncludesRepositoryAtMaximumDepth(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	initDiscoveryRepository(t, repository)

	maxDepth := 1
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(t.TempDir(), "worktrees")
	cfg.WorkspaceDefaults.Discovery.MaxDepth = &maxDepth
	cfg.System.Discovery.MaxEntries = 100
	cfg.System.Discovery.Timeout.Duration = time.Second
	discoverer := Discoverer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}

	workspace, err := discoverer.multiWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Repositories) != 1 || workspace.Repositories[0].RelativePath != "repository" {
		t.Fatalf("workspace=%+v, want the repository at exact max_depth", workspace)
	}
}
