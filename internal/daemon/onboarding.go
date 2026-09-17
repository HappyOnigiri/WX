package daemon

import (
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// SetupCheckRepository は初回セットアップ検査に必要な source と slot の対応である。
type SetupCheckRepository struct {
	RelativePath string `json:"relative_path"`
	MainPath     string `json:"main_path"`
	DirName      string `json:"dir_name"`
}

func setupCheckRepositories(w discovery.Workspace, cfg config.Config) []SetupCheckRepository {
	taken := map[string]bool{}
	dirNames := make(map[string]string, len(w.Repositories))
	for _, repository := range w.Repositories {
		dirNames[repository.RelativePath] = workspace.UniqueDirName(workspace.RepositoryDirName(repository, cfg), taken)
	}
	out := make([]SetupCheckRepository, 0, len(w.Repositories))
	for _, repository := range w.Repositories {
		out = append(out, SetupCheckRepository{RelativePath: repository.RelativePath, MainPath: string(repository.MainPath), DirName: dirNames[repository.RelativePath]})
	}
	return out
}
