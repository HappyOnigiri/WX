package daemon

import (
	"context"

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

// SetupOnboarding は貸出前に初回検査の要否を決めるための読み取り専用の workspace 情報である。
type SetupOnboarding struct {
	SourceWorkspace string                 `json:"source_workspace"`
	Repositories    []SetupCheckRepository `json:"repositories"`
}

// ResolveSetupOnboarding は slot や state を作らず、貸出と同じ discovery で workspace を解決する。
func (m *Manager) ResolveSetupOnboarding(ctx context.Context, cwd string) (SetupOnboarding, error) {
	cfg := m.Config()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return SetupOnboarding{}, err
	}
	return SetupOnboarding{SourceWorkspace: string(w.Root), Repositories: setupCheckRepositories(w, cfg)}, nil
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
