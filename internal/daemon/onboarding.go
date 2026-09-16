package daemon

import (
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// FirstLeaseRepository は初回セットアップ検査に必要な source と slot の対応である。
type FirstLeaseRepository struct {
	RelativePath string `json:"relative_path"`
	MainPath     string `json:"main_path"`
	DirName      string `json:"dir_name"`
}

func firstLeaseRepositories(first []state.FirstLeaseRepository, w discovery.Workspace, cfg config.Config) []FirstLeaseRepository {
	byRelative := make(map[string]discovery.Repository, len(w.Repositories))
	for _, repository := range w.Repositories {
		byRelative[repository.RelativePath] = repository
	}
	taken := map[string]bool{}
	dirNames := make(map[string]string, len(w.Repositories))
	for _, repository := range w.Repositories {
		dirNames[repository.RelativePath] = workspace.UniqueDirName(workspace.RepositoryDirName(repository, cfg), taken)
	}
	out := make([]FirstLeaseRepository, 0, len(first))
	for _, repository := range first {
		if _, ok := byRelative[repository.RelativePath]; !ok {
			continue
		}
		out = append(out, FirstLeaseRepository{RelativePath: repository.RelativePath, MainPath: repository.MainPath, DirName: dirNames[repository.RelativePath]})
	}
	return out
}
