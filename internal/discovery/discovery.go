package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

type Repository struct {
	ID            domain.RepositoryID
	MainPath      domain.CanonicalPath
	CommonDir     domain.CanonicalPath
	RelativePath  string
	DefaultBranch string
}
type Workspace struct {
	ID           domain.WorkspaceID
	Root         domain.CanonicalPath
	Kind         string
	Repositories []Repository
}
type Discoverer struct {
	Git    *gitx.Runner
	Config config.Config
}

func (d *Discoverer) Resolve(ctx context.Context, cwd string) (Workspace, error) {
	canonical, err := domain.Canonicalize(cwd)
	if err != nil {
		return Workspace{}, err
	}
	if res, e := d.Git.Run(ctx, string(canonical), "rev-parse", "--show-toplevel"); e == nil {
		return d.repositoryWorkspace(ctx, strings.TrimSpace(res.Stdout))
	}
	return d.multiWorkspace(ctx, string(canonical))
}

func (d *Discoverer) repositoryWorkspace(ctx context.Context, root string) (Workspace, error) {
	repo, err := d.inspectRepo(ctx, root, ".")
	if err != nil {
		return Workspace{}, err
	}
	w := Workspace{Root: repo.MainPath, Kind: "repository", Repositories: []Repository{repo}}
	// A repository workspace must survive a move of its main worktree. The
	// canonical common directory is the Git identity that remains stable while
	// the worktree path changes (for example, for a bare repository with linked
	// worktrees), so do not derive the workspace ID from the display path.
	w.ID = domain.WorkspaceID(domain.StableID("repository-workspace", string(repo.CommonDir)))
	return w, nil
}

// ResolveFromCommonDir rediscovers a repository whose main worktree path may
// have moved. Git keeps the worktree registry rooted at its common directory,
// so asking Git from that directory lets the daemon find the current main path
// without trusting the path stored in SQLite.
func (d *Discoverer) ResolveFromCommonDir(ctx context.Context, commonDir string) (Workspace, error) {
	common, err := domain.Canonicalize(commonDir)
	if err != nil {
		return Workspace{}, err
	}
	res, err := d.Git.Run(ctx, string(common), "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Workspace{}, err
	}
	main := firstWorktreePath(res.Stdout)
	if main == "" {
		return Workspace{}, errors.New("git did not report a main worktree from its common directory")
	}
	workspace, err := d.repositoryWorkspace(ctx, main)
	if err != nil {
		return Workspace{}, err
	}
	if len(workspace.Repositories) != 1 || workspace.Repositories[0].CommonDir != common {
		return Workspace{}, errors.New("Git common directory identity changed during rediscovery")
	}
	return workspace, nil
}

func (d *Discoverer) inspectRepo(ctx context.Context, root, relative string) (Repository, error) {
	res, err := d.Git.Run(ctx, root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Repository{}, err
	}
	main := firstWorktreePath(res.Stdout)
	if main == "" {
		return Repository{}, errors.New("git did not report a main worktree")
	}
	mainPath, err := domain.Canonicalize(main)
	if err != nil {
		return Repository{}, err
	}
	commonRes, err := d.Git.Run(ctx, string(mainPath), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repository{}, err
	}
	common, err := domain.Canonicalize(strings.TrimSpace(commonRes.Stdout))
	if err != nil {
		return Repository{}, err
	}
	branch := "main"
	if override, ok := d.Config.Repositories[string(mainPath)]; ok && override.DefaultBranch != "" {
		branch = override.DefaultBranch
	}
	return Repository{ID: domain.RepositoryID(domain.StableID(string(common))), MainPath: mainPath, CommonDir: common, RelativePath: filepath.Clean(relative), DefaultBranch: branch}, nil
}

func firstWorktreePath(output string) string {
	for _, field := range strings.Split(output, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			return strings.TrimPrefix(field, "worktree ")
		}
	}
	return ""
}

func (d *Discoverer) multiWorkspace(ctx context.Context, root string) (Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Config.Discovery.Timeout.Duration)
	defer cancel()
	exclude := map[string]bool{".git": true}
	for _, v := range d.Config.Discovery.Exclude {
		exclude[v] = true
	}
	wtRoot, _ := config.ExpandHome(d.Config.Storage.WorktreeRoot)
	entries := 0
	var repos []Repository
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > d.Config.Discovery.MaxEntries {
			return fmt.Errorf("discovery exceeded max_entries=%d", d.Config.Discovery.MaxEntries)
		}
		rel, _ := filepath.Rel(root, path)
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(filepath.Separator)) + 1
		}
		if depth > d.Config.Discovery.MaxDepth && e.IsDir() {
			return filepath.SkipDir
		}
		if e.Type()&os.ModeSymlink != 0 && e.IsDir() {
			return filepath.SkipDir
		}
		if e.IsDir() && (exclude[e.Name()] || path == wtRoot) {
			return filepath.SkipDir
		}
		if !e.IsDir() {
			return nil
		}
		gitPath := filepath.Join(path, ".git")
		if _, err := os.Lstat(gitPath); err == nil {
			repo, err := d.inspectRepo(ctx, path, rel)
			if err != nil {
				return err
			}
			repos = append(repos, repo)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	if len(repos) == 0 {
		return Workspace{}, fmt.Errorf("no Git repositories found below %s", root)
	}
	canonical, err := domain.Canonicalize(root)
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: domain.WorkspaceID(domain.StableID(string(canonical))), Root: canonical, Kind: "multi_repository", Repositories: repos}, nil
}

func ReadPatterns(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		v := strings.TrimSpace(s.Text())
		if v != "" && !strings.HasPrefix(v, "#") {
			out = append(out, v)
		}
	}
	return out, s.Err()
}

var _ = time.Second
