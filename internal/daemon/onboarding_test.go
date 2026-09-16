package daemon

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestFirstLeaseRepositoriesAddsSlotDirectoryNames(t *testing.T) {
	t.Parallel()
	w := discovery.Workspace{Repositories: []discovery.Repository{{ID: domain.RepositoryID("repo"), MainPath: "/source/repo", RelativePath: "."}}}
	got := firstLeaseRepositories([]state.FirstLeaseRepository{{RelativePath: ".", MainPath: "/source/repo"}}, w, config.Defaults())
	if len(got) != 1 || got[0].DirName == "" {
		t.Fatalf("repositories=%+v", got)
	}
}
